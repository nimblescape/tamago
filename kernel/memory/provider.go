// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import (
	"errors"
)

// ProviderConfig describes the memory that a Provider gives out.
type ProviderConfig struct {
	// Window is the address range of the memory.
	Window Range
	// Frames, Owner and Zone are the frames of the memory, the owner
	// that pays for them and the zone that they come from.
	Frames *Frames
	Owner  *Owner
	Zone   Zone
	// Space is the address space of the window, into which the provider
	// maps the frames as Data.
	Space AddressSpace
}

// Provider backs the memory of the Go runtime: it implements the methods of
// runtime/goos.Memory with frames that it maps into a window, so that the
// memory of the runtime grows and goes back.
//
// A page gets its frame when the runtime uses it, filled with zeros, and
// gives it back when the runtime no longer uses it, after Provider cleared
// it through its mapping and every core forgot the mapping. So no data of
// the runtime stays in a free frame.
//
// The runtime calls a Provider with its locks held, and also at times at
// which it allows no write barrier, such as while the garbage collector
// takes the pointers out of the buffer of the write barriers. So its
// methods, and the AddressSpace that they call, never allocate and never
// write a pointer outside the stack, which needs a write barrier.
// TestNoWriteBarrier checks this for this package and for package
// amd64/paging. A lack of frames is fatal, as on every system.
type Provider struct {
	window *Window
	frames *Frames
	owner  *Owner
	zone   Zone
	space  AddressSpace

	// clear fills memory of the window with zeros.
	clear func(virt, size uint64)
}

// NewProvider returns a provider for the memory that cfg describes.
func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if cfg.Frames == nil || cfg.Owner == nil || cfg.Space == nil {
		return nil, errors.New("memory: incomplete provider configuration")
	}

	window, err := NewWindow(cfg.Window)

	if err != nil {
		return nil, err
	}

	p := &Provider{
		window: window,
		frames: cfg.Frames,
		owner:  cfg.Owner,
		zone:   cfg.Zone,
		space:  cfg.Space,
		clear:  clearMemory,
	}

	return p, nil
}

// Reserve takes size bytes of the window at hint, or elsewhere when hint
// is 0 or taken, aligned to align. It returns 0 when the window has no
// such range left.
func (p *Provider) Reserve(hint, size, align uintptr) uintptr {
	addr, ok := p.window.Reserve(uint64(hint), uint64(size), uint64(align))

	if !ok {
		return 0
	}

	return uintptr(addr)
}

// Use gives each page of the range that has no frame a frame filled with
// zeros. Pages that have a frame keep it. The frames of a run of such pages
// come from a few calls of Frames.AllocUpTo, and each page gets its own
// Map, so that the window holds only pages of the smallest size, which
// Unuse can give back one at a time.
func (p *Provider) Use(addr, size uintptr) {
	end := uint64(addr + size)

	for virt := uint64(addr); virt < end; {
		if p.mapped(virt) {
			virt += FrameSize
			continue
		}

		run := virt + FrameSize

		for run < end && !p.mapped(run) {
			run += FrameSize
		}

		for virt < run {
			phys, n, err := p.frames.AllocUpTo(p.owner, p.zone, (run-virt)/FrameSize)

			if err != nil {
				fatal("memory: no frame for the memory of ", p.owner.Name(), ": ", err)
			}

			for off := uint64(0); off < n*FrameSize; off += FrameSize {
				if err := p.space.Map(virt+off, phys+off, FrameSize, Data); err != nil {
					fatal("memory: map of a page for ", p.owner.Name(), ": ", err)
				}
			}

			p.clear(virt, n*FrameSize)
			virt += n * FrameSize
		}
	}
}

// unuseRuns is the number of runs of contiguous frames that Unuse collects
// before each flush.
const unuseRuns = 32

// Unuse takes the frames of the range back and keeps the range reserved.
// Each page is cleared before it is unmapped. Unuse notes the frames of the
// pages that it unmaps as runs of contiguous frames, and frees each run
// with one call of Frames.Free after the flush and the reclaim. When the
// frames of the range form more than unuseRuns runs, it does this in
// several parts, each with its own flush.
func (p *Provider) Unuse(addr, size uintptr) {
	start, end := uint64(addr), uint64(addr+size)

	for start < end {
		var runs [unuseRuns]Range

		n, next := p.unmapRuns(start, end, &runs)

		if n > 0 {
			p.space.Flush()

			if err := p.space.Reclaim(start, next-start, noFree); err != nil {
				fatal("memory: reclaim of pages of ", p.owner.Name(), ": ", err)
			}

			for _, r := range runs[:n] {
				p.frames.Free(p.owner, r.Start, (r.End-r.Start)/FrameSize)
			}
		}

		start = next
	}
}

// unmapRuns clears and unmaps the mapped pages from start on, until end or
// until their frames would form more than unuseRuns runs. It writes the
// runs of frames into runs and returns their number and the end of the part
// that it handled.
func (p *Provider) unmapRuns(start, end uint64, runs *[unuseRuns]Range) (n int, next uint64) {
	// run is the first page of the run of mapped pages that ends at
	// virt.
	run := start
	virt := start

	for ; virt < end; virt += FrameSize {
		phys, _, ok := p.space.Lookup(virt)

		if ok && n > 0 && runs[n-1].End == phys {
			runs[n-1].End += FrameSize
			continue
		}

		if ok && n < unuseRuns {
			runs[n] = Range{phys, phys + FrameSize}
			n++
			continue
		}

		p.unmap(run, virt)
		run = virt + FrameSize

		if ok {
			// runs is full, so the part ends before this page.
			return n, virt
		}
	}

	p.unmap(run, virt)

	return n, virt
}

// unmap clears and unmaps the pages from start to end, which are all
// mapped. An empty range is no error.
func (p *Provider) unmap(start, end uint64) {
	if start >= end {
		return
	}

	p.clear(start, end-start)

	if err := p.space.Unmap(start, end-start); err != nil {
		fatal("memory: unmap of pages of ", p.owner.Name(), ": ", err)
	}
}

// noFree is the free function of Reclaim in Unuse, which frees the frames
// itself.
func noFree(phys, size uint64) {}

// mapped reports whether the page at virt is mapped.
func (p *Provider) mapped(virt uint64) bool {
	_, _, ok := p.space.Lookup(virt)
	return ok
}

// Release gives the range back completely: its frames and its addresses.
func (p *Provider) Release(addr, size uintptr) {
	p.Unuse(addr, size)
	p.window.Release(uint64(addr), uint64(size))
}

// Free returns the free bytes of the window and the size of its largest
// free range.
func (p *Provider) Free() (free, largest uint64) {
	return p.window.Free()
}

// clearMemory fills mapped memory with zeros.
func clearMemory(virt, size uint64) {
	clear(viewAt(virt, size).b)
}

// fatal stops the program: the runtime called the provider with its locks
// held, so the error cannot go back to it. It prints without an allocation
// before it panics.
func fatal(msg, name, sep string, err error) {
	print(msg, name, sep, err.Error(), "\n")
	panic("memory: provider failed")
}
