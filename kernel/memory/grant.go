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
	"strings"
	"sync/atomic"
)

// DeviceMemory describes the memory of a device that the kernel gives to an
// owner (Give). The kernel builds it from its own enumeration of the
// devices, such as a PCI scan, and never from the owner: an owner cannot
// name registers itself.
type DeviceMemory struct {
	// Name names the device for the diagnostics.
	Name string
	// Registers are the physical ranges of the registers of the device,
	// such as its BARs. Give extends each range to whole pages.
	Registers []Range
	// DMA is the size of the DMA part of the device in bytes, which its
	// driver states, or 0 for a device without DMA.
	DMA uint64
}

// Grant is a device that an owner holds (Give): its registers and its DMA
// part. The processor of the owner reaches both in the address space of
// the owner (see Owner.SetSpace), and the device reaches its part at the
// physical address, also in the IOMMU domain of the owner (see
// Owner.SetDomain). The grant is one piece of the owner.
type Grant struct {
	m      *Manager
	owner  *Owner
	name   string
	space  AddressSpace // the address space of the processor of the owner
	window *Window      // the window of space, nil for the 1:1 maps of the kernel
	domain AddressSpace // the IOMMU domain of the owner, or nil

	regs  []Range  // the registers, in whole pages
	offs  []uint64 // the offset of each range of the DeviceMemory in its pages
	views []uint64 // the address of each range of regs for the processor
	held  bool     // the manager records regs as given

	part     Range  // the DMA part, empty without one
	partView uint64 // the address of the part for the processor
	parts    *Owner // counts the frames of the parts of the DMA region
	mapped   int    // the last step of givePart that is done (stepTaken and on)

	released atomic.Bool
}

// The steps of Give for the part, in their order. Release undoes the steps
// that are done, the last first.
const (
	stepTaken  = iota + 1 // the frames are taken from the DMA region
	stepKernel            // the part is mapped 1:1 in the kernel and cleared
	stepDomain            // the part is mapped in the domain
	stepView              // the part is mapped for the processor of the owner
)

// Give gives the device d to o and returns the grant. It maps the
// registers of d for the processor of o as device memory (Registers), and
// takes the DMA part of d from the DMA region (StartDMA): mapped 1:1 in
// the address space of the kernel, which clears it, at its physical
// address in the domain of o, and for the processor of o.
//
// Give refuses registers that lie in memory or in a window, and registers
// that the manager gave to another grant. An owner with its own address
// space (a process) gets a device with DMA only when it has a domain, so
// that the IOMMU confines the device to its part.
//
// The hardware enforces the grant: the page tables of the owner hold only
// what it was given, and the domain of the device holds only its part. The
// platform enables the bus mastering of the device only after Give, and
// disables it before the grant goes back.
func (m *Manager) Give(o *Owner, d DeviceMemory) (*Grant, error) {
	space, window, domain := o.view()

	g := &Grant{m: m, owner: o, name: d.Name, space: m.space, domain: domain}

	if space != nil {
		g.space, g.window = space, window
	}

	if d.DMA > 0 && space != nil && domain == nil {
		return nil, fmt.Errorf("memory: %s: an owner with its own address space gets DMA only with a domain", d.Name)
	}

	if err := m.checkRegisters(g, d.Registers); err != nil {
		return nil, err
	}

	if err := m.hold(g.regs); err != nil {
		return nil, fmt.Errorf("memory: %s: %w", d.Name, err)
	}

	g.held = true

	if err := g.give(d.DMA); err != nil {
		return nil, errors.Join(fmt.Errorf("memory: %s: %w", d.Name, err), g.Release())
	}

	if err := o.Hold(g); err != nil {
		return nil, errors.Join(err, g.Release())
	}

	return g, nil
}

// checkRegisters records the registers of the device in g, in whole pages,
// and refuses a range that is empty, wraps, lies in memory or in a window,
// or overlaps another range of the device.
func (m *Manager) checkRegisters(g *Grant, regs []Range) error {
	for _, r := range regs {
		start, end := r.Start&^(FrameSize-1), alignUp(r.End, FrameSize)

		if r.End <= r.Start || end < start {
			return fmt.Errorf("memory: %s: invalid register range %#x-%#x", g.name, r.Start, r.End)
		}

		for _, mem := range m.memory {
			if start < mem.End && mem.Start < end {
				return fmt.Errorf("memory: %s: the registers %#x-%#x overlap the memory %#x-%#x", g.name, start, end, mem.Start, mem.End)
			}
		}

		if w, ok := m.inWindow(start, end); ok {
			return fmt.Errorf("memory: %s: the registers %#x-%#x overlap the window %#x-%#x", g.name, start, end, w.Start, w.End)
		}

		for _, other := range g.regs {
			if start < other.End && other.Start < end {
				return fmt.Errorf("memory: %s: the registers %#x-%#x overlap the registers %#x-%#x of the same device", g.name, start, end, other.Start, other.End)
			}
		}

		g.regs = append(g.regs, Range{Start: start, End: end})
		g.offs = append(g.offs, r.Start-start)
	}

	return nil
}

// give takes the DMA part of size bytes and maps it and the registers.
func (g *Grant) give(size uint64) error {
	if size > 0 {
		if err := g.givePart(size); err != nil {
			return err
		}
	}

	for _, r := range g.regs {
		virt, err := g.mapView(r.Start, r.End-r.Start, Registers)

		if err != nil {
			return fmt.Errorf("map of the registers %#x-%#x: %w", r.Start, r.End, err)
		}

		g.views = append(g.views, virt)
	}

	return nil
}

// givePart takes the DMA part of size bytes, step by step (see stepTaken).
func (g *Grant) givePart(size uint64) error {
	dma := g.m.dma.Load()

	if dma == nil {
		return errors.New("no DMA region")
	}

	n := frameCount(size)
	addr, err := dma.frames.AllocRun(dma.parts, DMA, n, FrameSize)

	if err != nil {
		return fmt.Errorf("DMA part of %d bytes: %w", size, err)
	}

	g.part, g.parts, g.mapped = Range{Start: addr, End: addr + n*FrameSize}, dma.parts, stepTaken
	p := g.part

	if err := g.m.space.Map(p.Start, p.Start, p.End-p.Start, Data); err != nil {
		return fmt.Errorf("map of the DMA part: %w", err)
	}

	// The device and the owner see the part only after the clearing.
	clear(bytesAt(p.Start, p.End-p.Start))
	g.mapped = stepKernel

	if g.domain != nil {
		if err := g.domain.Map(p.Start, p.Start, p.End-p.Start, Data); err != nil {
			return fmt.Errorf("map of the DMA part in the domain: %w", err)
		}
	}

	g.mapped = stepDomain

	if g.window == nil {
		g.partView, g.mapped = p.Start, stepView
		return nil
	}

	virt, err := g.mapView(p.Start, p.End-p.Start, Data)

	if err != nil {
		return fmt.Errorf("map of the DMA part for the owner: %w", err)
	}

	g.partView, g.mapped = virt, stepView

	return nil
}

// mapView maps size bytes from phys on for the processor of the owner: 1:1
// in the address space of the kernel, or at an address of the window of
// the owner.
func (g *Grant) mapView(phys, size uint64, attr Attr) (uint64, error) {
	if g.window == nil {
		return phys, g.space.Map(phys, phys, size, attr)
	}

	virt, ok := g.window.Reserve(0, size, FrameSize)

	if !ok {
		return 0, fmt.Errorf("no %d bytes left in the window of the owner", size)
	}

	if err := g.space.Map(virt, phys, size, attr); err != nil {
		g.window.Release(virt, size)
		return 0, err
	}

	return virt, nil
}

// unmapView removes a range that mapView mapped.
func (g *Grant) unmapView(virt, size uint64) error {
	if err := unmap(g.space, virt, size, keep); err != nil {
		return err
	}

	if g.window != nil {
		g.window.Release(virt, size)
	}

	return nil
}

// Register returns the address for the processor of the owner of the first
// byte of the register range i of the DeviceMemory.
func (g *Grant) Register(i int) uint64 {
	return g.views[i] + g.offs[i]
}

// Part returns the DMA part: its address for the device, its address for
// the processor of the owner, and its size in bytes, a multiple of
// FrameSize. The size is 0 for a device without DMA.
func (g *Grant) Part() (dev, virt, size uint64) {
	return g.part.Start, g.partView, g.part.End - g.part.Start
}

// Bytes returns the memory of the DMA part for the kernel, which maps it
// 1:1, or nil for a device without DMA. It is outside the Go heap: the
// garbage collector never scans or moves it.
func (g *Grant) Bytes() []byte {
	if g.part.End == g.part.Start {
		return nil
	}

	return bytesAt(g.part.Start, g.part.End-g.part.Start)
}

func (g *Grant) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "device %s:", g.name)

	for _, r := range g.regs {
		fmt.Fprintf(&b, " registers %#x-%#x", r.Start, r.End)
	}

	if p := g.part; p.End != p.Start {
		fmt.Fprintf(&b, " DMA part %#x-%#x (%d KiB)", p.Start, p.End, (p.End-p.Start)>>10)
	}

	return b.String()
}

// Release gives the device back. The device must no longer use its part:
// the platform has disabled its bus mastering. The part leaves the domain
// first, so that the device cannot write after the clearing, then it
// leaves the address space of the owner, is cleared, leaves the address
// space of the kernel, and only then its frames go back to the DMA region.
// The registers leave the address space of the owner last.
func (g *Grant) Release() error {
	if g.released.Swap(true) {
		return nil
	}

	defer g.owner.Drop(g)

	if err := g.releasePart(); err != nil {
		return err
	}

	for i := len(g.views) - 1; i >= 0; i-- {
		r := g.regs[i]

		if err := g.unmapView(g.views[i], r.End-r.Start); err != nil {
			return fmt.Errorf("memory: %s: unmap of the registers %#x-%#x: %w", g.name, r.Start, r.End, err)
		}
	}

	if g.held {
		g.m.drop(g.regs)
	}

	return nil
}

// releasePart undoes the steps of givePart that are done, the last first.
func (g *Grant) releasePart() error {
	p := g.part
	size := p.End - p.Start

	if g.mapped >= stepView && g.window != nil {
		if err := g.unmapView(g.partView, size); err != nil {
			return fmt.Errorf("memory: %s: unmap of the DMA part for the owner: %w", g.name, err)
		}
	}

	if g.mapped >= stepDomain && g.domain != nil {
		if err := unmap(g.domain, p.Start, size, keep); err != nil {
			return fmt.Errorf("memory: %s: unmap of the DMA part from the domain: %w", g.name, err)
		}
	}

	if g.mapped >= stepKernel {
		clear(bytesAt(p.Start, size))

		if err := unmap(g.m.space, p.Start, size, keep); err != nil {
			return fmt.Errorf("memory: %s: unmap of the DMA part: %w", g.name, err)
		}
	}

	if g.mapped >= stepTaken {
		g.m.dma.Load().frames.Free(g.parts, p.Start, size/FrameSize)
	}

	return nil
}

// hold records the register ranges regs as given, unless one of them
// overlaps a range that the manager gave before.
func (m *Manager) hold(regs []Range) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	for _, r := range regs {
		for _, other := range m.given {
			if r.Start < other.End && other.Start < r.End {
				return fmt.Errorf("the registers %#x-%#x overlap the registers %#x-%#x of another device", r.Start, r.End, other.Start, other.End)
			}
		}
	}

	m.given = append(m.given, regs...)

	return nil
}

// drop forgets the register ranges regs, which hold recorded.
func (m *Manager) drop(regs []Range) {
	m.lock.Lock()
	defer m.lock.Unlock()

	for _, r := range regs {
		m.given = remove(m.given, r)
	}
}
