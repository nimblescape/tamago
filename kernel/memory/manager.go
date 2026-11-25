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
	// there, so MapDevice refuses them.
	Memory []Range
	// Windows are other ranges of virtual addresses of Space, such as
	// the window of the Go memory (see Provider). DMA buffers and device
	// registers are mapped 1:1, at their physical addresses, so none of
	// them may lie in a window or in Window.
	Windows []Range
	// Owners are owners that exist before the Manager, such as the owner
	// of the page tables of Space.
	Owners []*Owner
}

// Manager is the memory manager of a kernel. It gives its owners the
// memory of the kernel in three kinds of pieces:
//
//   - DMA buffers: contiguous frames, mapped 1:1 in the address space of
//     the kernel and in the IOMMU domain of the owner (DMA);
//   - the registers of a device, mapped 1:1 as device memory (MapDevice);
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

// Buffer is DMA memory of an owner: contiguous frames, mapped 1:1 in the
// address space of the kernel and, when the owner has a domain, at the same
// address in its domain, so that the processor and the device use the same
// address.
type Buffer struct {
	m        *Manager
	owner    *Owner
	domain   AddressSpace
	addr     uint64
	size     uint64
	released atomic.Bool
}

// DMA gives o a DMA buffer of size bytes from the DMA region (zone DMA),
// aligned to align bytes, filled with zeros.
func (m *Manager) DMA(o *Owner, size, align uint64) (*Buffer, error) {
	if size == 0 || align&(align-1) != 0 {
		return nil, fmt.Errorf("memory: invalid DMA buffer of %d bytes with alignment %d", size, align)
	}

	n := frameCount(size)
	addr, err := m.frames.AllocRun(o, DMA, n, max(align, FrameSize))

	if err != nil {
		return nil, err
	}

	if w, ok := m.inWindow(addr, addr+n*FrameSize); ok {
		m.frames.Free(o, addr, n)
		return nil, fmt.Errorf("memory: the frames %#x-%#x of the DMA buffer lie in the window %#x-%#x", addr, addr+n*FrameSize, w.Start, w.End)
	}

	b := &Buffer{m: m, owner: o, domain: o.Domain(), addr: addr, size: n * FrameSize}

	if err := m.space.Map(addr, addr, b.size, Data); err != nil {
		m.frames.Free(o, addr, n)
		return nil, fmt.Errorf("memory: map of a DMA buffer: %w", err)
	}

	// The device sees the buffer only after the clearing.
	clear(b.Bytes())

	if b.domain != nil {
		if err := b.domain.Map(addr, addr, b.size, Data); err != nil {
			b.domain = nil
			return nil, errors.Join(fmt.Errorf("memory: map of a DMA buffer in the domain: %w", err), b.Release())
		}
	}

	if err := o.Hold(b); err != nil {
		return nil, errors.Join(err, b.Release())
	}

	return b, nil
}

func isBuffer(p Piece) bool {
	_, ok := p.(*Buffer)
	return ok
}

// Addr returns the address of the buffer for the processor and the device.
func (b *Buffer) Addr() uint64 {
	return b.addr
}

// Size returns the size of the buffer in bytes, a multiple of FrameSize.
func (b *Buffer) Size() uint64 {
	return b.size
}

// Bytes returns the memory of the buffer for the processor. It is outside
// the Go heap: the garbage collector never scans or moves it.
func (b *Buffer) Bytes() []byte {
	return bytesAt(b.addr, b.size)
}

func (b *Buffer) String() string {
	return fmt.Sprintf("DMA %#x-%#x", b.addr, b.addr+b.size)
}

// Release gives the buffer back. The device must no longer use it. The
// buffer leaves the domain first, so that the device cannot write after
// the clearing, then it is cleared and leaves the address space of the
// kernel, and only then its frames are free.
func (b *Buffer) Release() error {
	if b.released.Swap(true) {
		return nil
	}

	defer b.owner.Drop(b)

	if b.domain != nil {
		if err := unmap(b.domain, b.addr, b.size, keep); err != nil {
			return fmt.Errorf("memory: unmap of a DMA buffer from the domain: %w", err)
		}
	}

	clear(b.Bytes())

	return b.m.release(b.owner, b.addr, b.size)
}

// release unmaps the 1:1 range of frames of o from the address space of the
// kernel and frees the frames, which are cleared.
func (m *Manager) release(o *Owner, addr, size uint64) error {
	if err := unmap(m.space, addr, size, keep); err != nil {
		return fmt.Errorf("memory: unmap of %#x-%#x: %w", addr, addr+size, err)
	}

	m.frames.Free(o, addr, size/FrameSize)

	return nil
}

// Mapping is a range of device registers of an owner, mapped 1:1 with the
// attributes Registers.
type Mapping struct {
	m          *Manager
	owner      *Owner
	addr, size uint64
	released   atomic.Bool
}

// MapDevice maps the device registers from phys on, size bytes, extended to
// whole pages, for o. It refuses memory, a window, and a range that another
// owner has mapped.
func (m *Manager) MapDevice(o *Owner, phys, size uint64) (*Mapping, error) {
	start, end := phys&^(FrameSize-1), alignUp(phys+size, FrameSize)

	if size == 0 || end < start {
		return nil, fmt.Errorf("memory: invalid device range %#x with %d bytes", phys, size)
	}

	for _, r := range m.memory {
		if start < r.End && r.Start < end {
			return nil, fmt.Errorf("memory: the device range %#x-%#x overlaps the memory %#x-%#x", start, end, r.Start, r.End)
		}
	}

	if w, ok := m.inWindow(start, end); ok {
		return nil, fmt.Errorf("memory: the device range %#x-%#x overlaps the window %#x-%#x", start, end, w.Start, w.End)
	}

	if err := m.space.Map(start, start, end-start, Registers); err != nil {
		return nil, fmt.Errorf("memory: map of the device range %#x-%#x: %w", start, end, err)
	}

	mp := &Mapping{m: m, owner: o, addr: start, size: end - start}

	if err := o.Hold(mp); err != nil {
		return nil, errors.Join(err, mp.Release())
	}

	return mp, nil
}

// Addr returns the first address of the mapping, at the start of a page.
func (mp *Mapping) Addr() uint64 {
	return mp.addr
}

// Size returns the size of the mapping in bytes, a multiple of FrameSize.
func (mp *Mapping) Size() uint64 {
	return mp.size
}

func (mp *Mapping) String() string {
	return fmt.Sprintf("registers %#x-%#x", mp.addr, mp.addr+mp.size)
}

// Release unmaps the registers.
func (mp *Mapping) Release() error {
	if mp.released.Swap(true) {
		return nil
	}

	defer mp.owner.Drop(mp)

	if err := unmap(mp.m.space, mp.addr, mp.size, keep); err != nil {
		return fmt.Errorf("memory: unmap of the device range %#x-%#x: %w", mp.addr, mp.addr+mp.size, err)
	}

	return nil
}

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
