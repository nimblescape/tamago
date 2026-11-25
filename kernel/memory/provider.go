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
	"unsafe"
)

// Mapper maps frames into an address space for data: read and write, no
// execute. The driver of each architecture implements it on its page
// tables.
type Mapper interface {
	// Map maps size bytes from virt on to the frames from phys on. No
	// page of the range is mapped.
	Map(virt, phys, size uint64) error
	// Unmap makes every page of the range unreachable and keeps its
	// frame for Reclaim. Every page of the range is mapped.
	Unmap(virt, size uint64) error
	// Reclaim removes the pages of the range that Unmap unmapped, after
	// the TLB of every core was flushed, and calls free for the frames of
	// each page. Pages that are not mapped at all are skipped.
	Reclaim(virt, size uint64, free func(phys, size uint64)) error
	// Mapped reports whether the page at virt is mapped.
	Mapped(virt uint64) bool
}

// ProviderConfig describes the memory that a Provider gives out.
type ProviderConfig struct {
	// Window is the address range of the memory.
	Window Range
	// Frames, Owner and Zone are the frames of the memory, the owner
	// that pays for them and the zone that they come from.
	Frames *Frames
	Owner  *Owner
	Zone   Zone
	// Mapper maps the frames into the window.
	Mapper Mapper
	// Flush flushes the TLB of every core, and returns when no core
	// can reach an unmapped page any more.
	Flush func()
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
// methods, and the Mapper and Flush that they call, never allocate and
// never write a pointer outside the stack, which needs a write barrier.
// TestNoWriteBarrier checks this for this package and for package
// amd64/paging. A lack of frames is fatal, as on every system.
type Provider struct {
	space  *Space
	frames *Frames
	owner  *Owner
	zone   Zone
	mapper Mapper
	flush  func()

	// free gives a reclaimed frame back. It is made once, so that
	// Unuse passes it to the Mapper without an allocation.
	free func(phys, size uint64)
	// clear fills memory of the window with zeros.
	clear func(virt, size uint64)
}

// NewProvider returns a provider for the memory that cfg describes.
func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if cfg.Frames == nil || cfg.Owner == nil || cfg.Mapper == nil || cfg.Flush == nil {
		return nil, errors.New("memory: incomplete provider configuration")
	}

	space, err := NewSpace(cfg.Window)

	if err != nil {
		return nil, err
	}

	p := &Provider{
		space:  space,
		frames: cfg.Frames,
		owner:  cfg.Owner,
		zone:   cfg.Zone,
		mapper: cfg.Mapper,
		flush:  cfg.Flush,
		clear:  clearMemory,
	}

	p.free = func(phys, size uint64) {
		p.frames.Free(p.owner, phys, size/FrameSize)
	}

	return p, nil
}

// Reserve takes size bytes of the window at hint, or elsewhere when hint
// is 0 or taken, aligned to align. It returns 0 when the window has no
// such range left.
func (p *Provider) Reserve(hint, size, align uintptr) uintptr {
	addr, ok := p.space.Reserve(uint64(hint), uint64(size), uint64(align))

	if !ok {
		return 0
	}

	return uintptr(addr)
}

// Use gives each page of the range that has no frame a frame filled with
// zeros. Pages that have a frame keep it.
func (p *Provider) Use(addr, size uintptr) {
	for virt := uint64(addr); virt < uint64(addr+size); virt += FrameSize {
		if p.mapper.Mapped(virt) {
			continue
		}

		phys, err := p.frames.Alloc(p.owner, p.zone)

		if err != nil {
			fatal("memory: no frame for the memory of ", p.owner.Name(), ": ", err)
		}

		if err := p.mapper.Map(virt, phys, FrameSize); err != nil {
			fatal("memory: map of a page for ", p.owner.Name(), ": ", err)
		}

		p.clear(virt, FrameSize)
	}
}

// Unuse takes the frames of the range back and keeps the range reserved.
// Each page is cleared before it is unmapped.
func (p *Provider) Unuse(addr, size uintptr) {
	start, end := uint64(addr), uint64(addr+size)
	unmapped := false

	for virt := start; virt < end; {
		if !p.mapper.Mapped(virt) {
			virt += FrameSize
			continue
		}

		run := virt

		for virt < end && p.mapper.Mapped(virt) {
			virt += FrameSize
		}

		p.clear(run, virt-run)

		if err := p.mapper.Unmap(run, virt-run); err != nil {
			fatal("memory: unmap of pages of ", p.owner.Name(), ": ", err)
		}

		unmapped = true
	}

	if !unmapped {
		return
	}

	p.flush()

	if err := p.mapper.Reclaim(start, end-start, p.free); err != nil {
		fatal("memory: reclaim of pages of ", p.owner.Name(), ": ", err)
	}
}

// Release gives the range back completely: its frames and its addresses.
func (p *Provider) Release(addr, size uintptr) {
	p.Unuse(addr, size)
	p.space.Release(uint64(addr), uint64(size))
}

// Free returns the free bytes of the window and the size of its largest
// free range.
func (p *Provider) Free() (free, largest uint64) {
	return p.space.Free()
}

// clearMemory fills mapped memory with zeros.
func clearMemory(virt, size uint64) {
	clear(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(virt))), size))
}

// fatal stops the program: the runtime called the provider with its locks
// held, so the error cannot go back to it. It prints without an allocation
// before it panics.
func fatal(msg, name, sep string, err error) {
	print(msg, name, sep, err.Error(), "\n")
	panic("memory: provider failed")
}
