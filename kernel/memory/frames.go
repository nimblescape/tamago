// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import (
	"cmp"
	"fmt"
	"math/bits"
	"slices"
)

// Frames gives out the frames of the memory list. A bitmap per area marks
// the frames in use, so that no frame is ever given out twice.
type Frames struct {
	lock  spinLock
	areas []area // in the order of their addresses
}

// area is a range of frames in one zone, with its bitmap.
type area struct {
	start  uint64   // address of the first frame
	frames uint64   // number of frames
	zone   Zone     // zone of all frames of the area
	used   []uint64 // bit i%64 of word i/64 set: frame i is in use
	free   uint64   // number of free frames
	next   uint64   // frame where the search for a single frame starts
}

// Words returns the number of 64-bit words that New needs as storage for
// the bitmaps of ranges.
func Words(ranges []Range) (int, error) {
	areas, err := split(ranges)

	if err != nil {
		return 0, err
	}

	n := 0

	for _, a := range areas {
		n += words(a.frames)
	}

	return n, nil
}

// New returns the frames of ranges, which must not overlap. It takes whole
// frames only, so it rounds the start of each range up and its end down to
// a multiple of FrameSize, and it splits a range at 4 GiB, so that each
// area lies in one zone.
//
// storage holds the bitmaps. It must have Words(ranges) words, lie outside
// ranges and stay with the frames for their whole life. New clears it, so
// that every frame starts free.
func New(ranges []Range, storage []uint64) (*Frames, error) {
	areas, err := split(ranges)

	if err != nil {
		return nil, err
	}

	need := 0

	for _, a := range areas {
		need += words(a.frames)
	}

	if len(storage) != need {
		return nil, fmt.Errorf("memory: the storage has %d words, the ranges need %d", len(storage), need)
	}

	clear(storage)

	for i := range areas {
		a := &areas[i]
		n := words(a.frames)
		a.used, storage = storage[:n:n], storage[n:]
		a.free = a.frames

		// The bits after the last frame stay set, so that no search
		// gives them out.
		if rest := a.frames % 64; rest != 0 {
			a.used[n-1] = ^uint64(0) << rest
		}
	}

	return &Frames{areas: areas}, nil
}

// split returns the areas of ranges, without bitmaps.
func split(ranges []Range) ([]area, error) {
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, func(a, b Range) int {
		return cmp.Compare(a.Start, b.Start)
	})

	var areas []area

	for i, r := range sorted {
		if r.End < r.Start {
			return nil, fmt.Errorf("memory: the range %#x to %#x ends before it starts", r.Start, r.End)
		}

		if i > 0 && r.Start < sorted[i-1].End {
			return nil, fmt.Errorf("memory: the ranges overlap at %#x", r.Start)
		}

		start := alignUp(r.Start, FrameSize)
		end := r.End &^ (FrameSize - 1)

		for _, part := range [...]Range{
			{start, min(end, lowEnd)},
			{max(start, lowEnd), end},
		} {
			if part.Start >= part.End {
				continue
			}

			zone := High

			if part.End <= lowEnd {
				zone = Low
			}

			areas = append(areas, area{
				start:  part.Start,
				frames: (part.End - part.Start) / FrameSize,
				zone:   zone,
			})
		}
	}

	return areas, nil
}

// Alloc gives one frame of zone z to o and returns its address.
func (f *Frames) Alloc(o *Owner, z Zone) (uint64, error) {
	return f.AllocRun(o, z, 1, FrameSize)
}

// AllocRun gives n contiguous frames of zone z to o and returns the address
// of the first. The address is a multiple of align, which is a power of
// two and at least FrameSize.
func (f *Frames) AllocRun(o *Owner, z Zone, n, align uint64) (uint64, error) {
	if n == 0 || align < FrameSize || align&(align-1) != 0 {
		panic("memory: invalid number of frames or alignment")
	}

	if !o.take(n) {
		return 0, ErrLimit
	}

	addr, ok := f.allocLocked(z, n, align)

	if !ok {
		o.give(n)
		return 0, ErrNoMemory
	}

	return addr, nil
}

// allocLocked marks the first fitting run of n free frames of zone z as in
// use and returns the address of its first frame.
//
//go:nosplit
func (f *Frames) allocLocked(z Zone, n, align uint64) (addr uint64, ok bool) {
	f.lock.lock()

	for i := range f.areas {
		a := &f.areas[i]

		if a.zone != z || a.free < n {
			continue
		}

		var first uint64

		if n == 1 && align == FrameSize {
			first, ok = a.findOne()
		} else {
			first, ok = a.findRun(n, align)
		}

		if !ok {
			continue
		}

		a.mark(first, n, true)
		a.free -= n
		a.next = first + n
		addr = a.start + first*FrameSize
		break
	}

	f.lock.unlock()

	return addr, ok
}

// Free takes the n frames from addr on back from o, which has cleared
// them. A frame that is free or that lies outside the memory list is a
// fault of the caller: Free panics and changes nothing.
func (f *Frames) Free(o *Owner, addr, n uint64) {
	if n == 0 {
		return
	}

	if addr%FrameSize != 0 {
		panic("memory: free of an address that is not the start of a frame")
	}

	// give panics before any change when o holds fewer than n frames.
	o.give(n)

	if !f.freeLocked(addr, n) {
		o.frames.Add(n)
		panic("memory: free of frames that are not in use")
	}
}

// freeLocked marks the n frames from addr on as free, if they lie in one
// area and are all in use.
//
//go:nosplit
func (f *Frames) freeLocked(addr, n uint64) (ok bool) {
	f.lock.lock()

	for i := range f.areas {
		a := &f.areas[i]

		if addr < a.start || addr >= a.start+a.frames*FrameSize {
			continue
		}

		first := (addr - a.start) / FrameSize

		if n <= a.frames-first && a.allUsed(first, n) {
			a.mark(first, n, false)
			a.free += n
			ok = true
		}

		break
	}

	f.lock.unlock()

	return ok
}

// Usage returns the bytes of zone z in total and the free bytes of it.
func (f *Frames) Usage(z Zone) (total, free uint64) {
	total, free = f.usageLocked(z)
	return total * FrameSize, free * FrameSize
}

//go:nosplit
func (f *Frames) usageLocked(z Zone) (total, free uint64) {
	f.lock.lock()

	for i := range f.areas {
		if a := &f.areas[i]; a.zone == z {
			total += a.frames
			free += a.free
		}
	}

	f.lock.unlock()

	return total, free
}

// findOne returns the index of a free frame. It searches from the word of
// a.next on, and then from the start of the area.
//
//go:nosplit
func (a *area) findOne() (uint64, bool) {
	n := uint64(len(a.used))
	w := a.next / 64

	for k := range n {
		i := (w + k) % n

		if free := ^a.used[i]; free != 0 {
			return i*64 + uint64(bits.TrailingZeros64(free)), true
		}
	}

	return 0, false
}

// findRun returns the index of the first run of n free frames whose
// address is a multiple of align. After a used frame inside a candidate
// run, the search goes on after that frame.
//
//go:nosplit
func (a *area) findRun(n, align uint64) (uint64, bool) {
	i := a.aligned(0, align)

	for i <= a.frames && n <= a.frames-i {
		last, used := a.lastUsed(i, n)

		if !used {
			return i, true
		}

		i = a.aligned(last+1, align)
	}

	return 0, false
}

// aligned returns the index of the first frame at or after frame i whose
// address is a multiple of align.
//
//go:nosplit
func (a *area) aligned(i, align uint64) uint64 {
	return (alignUp(a.start+i*FrameSize, align) - a.start) / FrameSize
}

// lastUsed returns the index of the last frame in use among the n frames
// from frame i on.
//
//go:nosplit
func (a *area) lastUsed(i, n uint64) (uint64, bool) {
	for k := i + n; k > i; k-- {
		if a.used[(k-1)/64]&(1<<((k-1)%64)) != 0 {
			return k - 1, true
		}
	}

	return 0, false
}

// allUsed reports whether the n frames from frame i on are all in use.
//
//go:nosplit
func (a *area) allUsed(i, n uint64) bool {
	for k := i; k < i+n; k++ {
		if a.used[k/64]&(1<<(k%64)) == 0 {
			return false
		}
	}

	return true
}

// mark sets the n frames from frame i on as in use or as free.
//
//go:nosplit
func (a *area) mark(i, n uint64, used bool) {
	for k := i; k < i+n; k++ {
		if used {
			a.used[k/64] |= 1 << (k % 64)
		} else {
			a.used[k/64] &^= 1 << (k % 64)
		}
	}
}

// words returns the number of 64-bit words of a bitmap of n frames.
func words(n uint64) int {
	return int((n + 63) / 64)
}

// alignUp returns the first multiple of align, a power of two, at or after
// v.
//
//go:nosplit
func alignUp(v, align uint64) uint64 {
	return (v + align - 1) &^ (align - 1)
}
