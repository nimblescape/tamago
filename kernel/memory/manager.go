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
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Config describes the memory of a Manager.
type Config struct {
	// Frames are the free frames of the memory list.
	Frames *Frames
	// Space is the address space of the kernel.
	Space AddressSpace
	// Window is a range of virtual addresses of Space for the regions of
	// kernel memory (Alloc). No other mapping lies in it.
	Window Range
	// Memory are the physical ranges of memory. No device registers lie
	// there, so Give refuses them as registers.
	Memory []Range
	// Windows are other ranges of virtual addresses of Space, such as
	// the window of the Go memory (see Provider). The DMA region and the
	// registers of the devices of the kernel are mapped 1:1, at their
	// physical addresses, so none of them may lie in a window or in
	// Window.
	Windows []Range
	// Owners are owners that exist before the Manager, such as the owner
	// of the page tables of Space.
	Owners []*Owner
}

// Manager is the memory manager of a kernel. It gives its owners the
// memory of the kernel in two kinds of pieces:
//
//   - devices (Give): the registers of a device, mapped as device memory,
//     and its DMA part, a fixed piece of the DMA region (StartDMA), both
//     for the processor of the owner, and the part also in the IOMMU
//     domain of the owner. A driver in the kernel and a process get their
//     devices through the same call;
//   - regions of kernel memory between guard pages, at virtual addresses
//     of the window of the kernel (Alloc).
//
// Each piece is cleared before its frames go back, and when an owner ends,
// all its pieces go back. The memory of the Go runtime is not a piece: a
// Provider maps it page by page. Nothing else is mapped, so the kernel has
// no map of all physical memory.
type Manager struct {
	frames  *Frames
	space   AddressSpace
	window  *Window
	memory  []Range
	windows []Range

	lock   sync.Mutex
	owners []*Owner
	given  []Range // the registers of the devices that Give gave

	dmaLock sync.Mutex                // serializes StartDMA
	dma     atomic.Pointer[dmaRegion] // the DMA region, nil before StartDMA
}

// dmaRegion is the DMA region of a Manager.
type dmaRegion struct {
	frames *Frames // the frames of the region, all in zone DMA
	parts  *Owner  // counts the frames of the DMA parts, which no limit counts
}

// NewManager returns the memory manager of cfg.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Frames == nil || cfg.Space == nil {
		return nil, errors.New("memory: incomplete manager configuration")
	}

	window, err := NewWindow(cfg.Window)

	if err != nil {
		return nil, err
	}

	m := &Manager{
		frames:  cfg.Frames,
		space:   cfg.Space,
		window:  window,
		memory:  slices.Clone(cfg.Memory),
		windows: append(slices.Clone(cfg.Windows), cfg.Window),
	}

	for _, o := range cfg.Owners {
		m.adopt(o)
	}

	return m, nil
}

// Frames returns the frames of the manager.
func (m *Manager) Frames() *Frames {
	return m.frames
}

// StartDMA sets the fixed DMA region of the manager, once: a run of size
// bytes from zone z of the frames, aligned to align bytes, which o holds
// for the life of the kernel. The region neither grows nor shrinks, and
// from then on Give takes the DMA parts of the devices from it. The DMA
// memory is a kind of its own: no limit of an owner counts a part. The
// platform calls StartDMA when it knows the need of its devices, and it
// chooses the zone that its devices reach.
func (m *Manager) StartDMA(o *Owner, z Zone, size, align uint64) (Range, error) {
	if size == 0 || size%FrameSize != 0 || align&(align-1) != 0 {
		return Range{}, fmt.Errorf("memory: invalid DMA region of %d bytes with alignment %d", size, align)
	}

	m.dmaLock.Lock()
	defer m.dmaLock.Unlock()

	if m.dma.Load() != nil {
		return Range{}, errors.New("memory: the DMA region is set already")
	}

	if o == nil {
		return Range{}, errors.New("memory: the DMA region needs an owner")
	}

	n := size / FrameSize
	addr, err := m.frames.AllocRun(o, z, n, max(align, FrameSize))

	if err != nil {
		return Range{}, fmt.Errorf("memory: DMA region of %d bytes: %w", size, err)
	}

	r := Range{Start: addr, End: addr + size}

	if w, ok := m.inWindow(r.Start, r.End); ok {
		m.frames.Free(o, addr, n)
		return Range{}, fmt.Errorf("memory: the DMA region %#x-%#x lies in the window %#x-%#x", r.Start, r.End, w.Start, w.End)
	}

	words, err := Words([]Range{r}, r)

	if err != nil {
		m.frames.Free(o, addr, n)
		return Range{}, err
	}

	f, err := New([]Range{r}, r, make([]uint64, words))

	if err != nil {
		m.frames.Free(o, addr, n)
		return Range{}, err
	}

	m.dma.Store(&dmaRegion{frames: f, parts: NewOwner("DMA parts", 0)})

	return r, nil
}

// DMAFrames returns the frames of the DMA region, or nil before StartDMA.
func (m *Manager) DMAFrames() *Frames {
	if d := m.dma.Load(); d != nil {
		return d.frames
	}

	return nil
}

// Space returns the address space of the kernel.
func (m *Manager) Space() AddressSpace {
	return m.space
}

// NewOwner returns a new owner of the manager (see NewOwner).
func (m *Manager) NewOwner(name string, limit uint64) *Owner {
	o := NewOwner(name, limit)
	m.adopt(o)

	return o
}

// Owners returns the owners that have not ended, in the order of their
// start, the children with the others (see Owner.Parent).
func (m *Manager) Owners() []*Owner {
	m.lock.Lock()
	defer m.lock.Unlock()

	return append([]*Owner(nil), m.owners...)
}

func (m *Manager) adopt(o *Owner) {
	o.lock.Lock()
	o.manager = m
	o.lock.Unlock()

	m.lock.Lock()
	m.owners = append(m.owners, o)
	m.lock.Unlock()
}

func (m *Manager) forget(o *Owner) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.owners = remove(m.owners, o)
}

// inWindow returns the window that the range from start to end overlaps.
func (m *Manager) inWindow(start, end uint64) (Range, bool) {
	for _, w := range m.windows {
		if start < w.End && w.Start < end {
			return w, true
		}
	}

	return Range{}, false
}

// unmap removes the pages of the range from s and calls free for the
// physical memory of each page, after no core and no device can reach
// it.
func unmap(s AddressSpace, virt, size uint64, free func(phys, size uint64)) error {
	if err := s.Unmap(virt, size); err != nil {
		return err
	}

	s.Flush()

	return s.Reclaim(virt, size, free)
}

// keep is the free function of unmap for memory that stays where it is.
func keep(phys, size uint64) {}

// Region is kernel memory of an owner at virtual addresses of the window of
// the kernel. A guard page that is never mapped lies before and after it,
// so that an access beyond either end faults, as a stack overflow does.
type Region struct {
	m          *Manager
	owner      *Owner
	addr, size uint64
	mapped     uint64 // the bytes from addr on that have frames
	released   atomic.Bool
}

// Alloc gives o a region of size bytes of kernel memory, extended to whole
// pages and filled with zeros. Its frames come from zone High, or from
// zone Low when High has none.
func (m *Manager) Alloc(o *Owner, size uint64) (*Region, error) {
	if size == 0 {
		return nil, errors.New("memory: region of 0 bytes")
	}

	n := frameCount(size)
	virt, ok := m.window.Reserve(0, (n+2)*FrameSize, FrameSize)

	if !ok {
		return nil, fmt.Errorf("memory: no %d bytes left in the window of the kernel", (n+2)*FrameSize)
	}

	r := &Region{m: m, owner: o, addr: virt + FrameSize, size: n * FrameSize}

	for range n {
		phys, err := m.frames.Alloc(o, High)

		if errors.Is(err, ErrNoMemory) {
			phys, err = m.frames.Alloc(o, Low)
		}

		if err == nil {
			if err = m.space.Map(r.addr+r.mapped, phys, FrameSize, Data); err != nil {
				m.frames.Free(o, phys, 1)
			}
		}

		if err != nil {
			return nil, errors.Join(fmt.Errorf("memory: region of kernel memory: %w", err), r.Release())
		}

		r.mapped += FrameSize
	}

	clear(r.Bytes())

	if err := o.Hold(r); err != nil {
		return nil, errors.Join(err, r.Release())
	}

	return r, nil
}

// Addr returns the first address of the region.
func (r *Region) Addr() uint64 {
	return r.addr
}

// Size returns the size of the region in bytes, a multiple of FrameSize.
func (r *Region) Size() uint64 {
	return r.size
}

// Bytes returns the memory of the region. It is outside the Go heap.
func (r *Region) Bytes() []byte {
	return bytesAt(r.addr, r.size)
}

func (r *Region) String() string {
	return fmt.Sprintf("kernel memory %#x-%#x", r.addr, r.addr+r.size)
}

// Release clears the region, unmaps it, frees its frames and gives its
// addresses and guard pages back to the window.
func (r *Region) Release() error {
	if r.released.Swap(true) {
		return nil
	}

	defer r.owner.Drop(r)

	if r.mapped != 0 {
		clear(bytesAt(r.addr, r.mapped))

		free := func(phys, size uint64) {
			r.m.frames.Free(r.owner, phys, size/FrameSize)
		}

		if err := unmap(r.m.space, r.addr, r.mapped, free); err != nil {
			return fmt.Errorf("memory: unmap of the region %#x-%#x: %w", r.addr, r.addr+r.mapped, err)
		}
	}

	r.m.window.Release(r.addr-FrameSize, r.size+2*FrameSize)

	return nil
}

// frameCount returns the number of frames that hold size bytes.
func frameCount(size uint64) uint64 {
	return alignUp(size, FrameSize) / FrameSize
}

// bytesAt returns the mapped memory of size bytes at addr. The address is
// not a pointer of the Go runtime, so the pointer checks of the race
// detector do not apply.
//
//go:nocheckptr
func bytesAt(addr, size uint64) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(addr))), size)
}
