// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import "fmt"

// maxExtents is the number of free extents that a Window records. A
// Release that needs one more panics. The Go runtime reserves its heap in
// growing, adjacent ranges and gives little back, so its window has few
// free extents.
const maxExtents = 1024

// Window gives out the address ranges of a window, such as the window of the
// Go memory in the address space of the kernel. It holds no frames and no
// page tables: the memory provider maps frames into the ranges.
//
// A fixed array of free extents keeps Window free of allocations, so that
// the memory provider of the Go runtime may call it with locks of the
// runtime held.
type Window struct {
	lock   spinLock
	window Range
	free   [maxExtents]Range // free extents, in address order, none empty, none adjacent
	n      int               // number of free extents
}

// NewWindow returns the address ranges of window, all free. The start and
// the end of window are multiples of FrameSize.
func NewWindow(window Range) (*Window, error) {
	if window.Start%FrameSize != 0 || window.End%FrameSize != 0 || window.Start >= window.End {
		return nil, fmt.Errorf("memory: invalid window %#x to %#x", window.Start, window.End)
	}

	s := &Window{window: window, n: 1}
	s.free[0] = window

	return s, nil
}

// Reserve takes size bytes and returns their address: at hint, when the
// range from hint on is free and hint is a multiple of align; else the
// first free range whose address is a multiple of align. size and align
// are multiples of FrameSize, align is a power of two, and hint 0 means no
// hint. Reserve reports false when no range fits.
func (s *Window) Reserve(hint, size, align uint64) (uint64, bool) {
	if size == 0 || size%FrameSize != 0 || align < FrameSize || align&(align-1) != 0 {
		panic("memory: invalid size or alignment of a reservation")
	}

	return s.reserveLocked(hint, size, align)
}

//go:nosplit
func (s *Window) reserveLocked(hint, size, align uint64) (addr uint64, ok bool) {
	s.lock.lock()

	if hint != 0 && hint%align == 0 {
		for i := 0; i < s.n; i++ {
			if e := s.free[i]; e.Start <= hint && hint < e.End && size <= e.End-hint {
				addr, ok = hint, s.take(i, hint, size)
				break
			}
		}
	}

	for i := 0; !ok && i < s.n; i++ {
		e := s.free[i]
		start := alignUp(e.Start, align)

		if start >= e.Start && start < e.End && size <= e.End-start {
			addr, ok = start, s.take(i, start, size)
		}
	}

	s.lock.unlock()

	return addr, ok
}

// take removes the range of size bytes from addr on from the free extent
// i, which holds it. It reports false, and changes nothing, when the rest
// of the extent needs one more extent than the array has.
//
//go:nosplit
func (s *Window) take(i int, addr, size uint64) bool {
	e := s.free[i]
	before := Range{e.Start, addr}
	after := Range{addr + size, e.End}

	switch {
	case before.Start == before.End && after.Start == after.End:
		s.remove(i)
	case before.Start == before.End:
		s.free[i] = after
	case after.Start == after.End:
		s.free[i] = before
	default:
		if s.n == maxExtents {
			return false
		}

		s.free[i] = before
		s.insert(i+1, after)
	}

	return true
}

// Release gives the size bytes from addr on back. A range that is not
// reserved, in whole or in part, is a fault of the caller: Release panics
// and changes nothing.
func (s *Window) Release(addr, size uint64) {
	if size == 0 {
		return
	}

	if addr%FrameSize != 0 || size%FrameSize != 0 {
		panic("memory: release of a range that is not made of frames")
	}

	if msg := s.releaseLocked(addr, size); msg != "" {
		panic(msg)
	}
}

//go:nosplit
func (s *Window) releaseLocked(addr, size uint64) (msg string) {
	end := addr + size

	s.lock.lock()

	// i is the first free extent after the range.
	i := 0

	for i < s.n && s.free[i].Start < end {
		i++
	}

	prev := i > 0 && s.free[i-1].End == addr
	next := i < s.n && s.free[i].Start == end

	switch {
	case addr < s.window.Start || end > s.window.End || end < addr:
		msg = "memory: release of a range outside the window"
	case i > 0 && s.free[i-1].End > addr:
		msg = "memory: release of a range that is free"
	case prev && next:
		s.free[i-1].End = s.free[i].End
		s.remove(i)
	case prev:
		s.free[i-1].End = end
	case next:
		s.free[i].Start = addr
	case s.n == maxExtents:
		msg = "memory: too many free extents in the window"
	default:
		s.insert(i, Range{addr, end})
	}

	s.lock.unlock()

	return msg
}

// Free returns the free bytes of the window and the size of its largest
// free extent.
func (s *Window) Free() (free, largest uint64) {
	return s.freeLocked()
}

//go:nosplit
func (s *Window) freeLocked() (free, largest uint64) {
	s.lock.lock()

	for i := 0; i < s.n; i++ {
		size := s.free[i].End - s.free[i].Start
		free += size
		largest = max(largest, size)
	}

	s.lock.unlock()

	return free, largest
}

// remove deletes the free extent i.
//
//go:nosplit
func (s *Window) remove(i int) {
	for k := i; k < s.n-1; k++ {
		s.free[k] = s.free[k+1]
	}

	s.n--
}

// insert puts e at index i of the free extents.
//
//go:nosplit
func (s *Window) insert(i int, e Range) {
	for k := s.n; k > i; k-- {
		s.free[k] = s.free[k-1]
	}

	s.free[i] = e
	s.n++
}
