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
	"runtime"
	"testing"
)

// process is the start of the window of a process in the tests, an
// address that no memory of the test uses.
const process = 64 << 40

// nicMemory returns the memory of a test device with two pages of
// registers from device+0x10 on and a DMA part of dma bytes.
func nicMemory(dma uint64) DeviceMemory {
	return DeviceMemory{Name: "nic", Registers: []Range{{Start: device + 0x10, End: device + 0x10 + FrameSize}}, DMA: dma}
}

// expectNotMapped checks that no page of the range from virt on, size
// bytes, is mapped in s.
func expectNotMapped(t *testing.T, s *testSpace, virt, size uint64) {
	t.Helper()

	for v := virt; v < virt+size; v += FrameSize {
		if _, _, ok := s.Lookup(v); ok {
			t.Errorf("%#x still mapped", v)
		}
	}
}

func TestGive(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("driver", 0)
	g, err := tm.Give(o, nicMemory(3*FrameSize-10))

	if err != nil {
		t.Fatal(err)
	}

	if g.Register(0) != device+0x10 {
		t.Errorf("registers at %#x, want 1:1 at %#x", g.Register(0), device+0x10)
	}

	for _, virt := range []uint64{device, device + FrameSize} {
		if phys, attr, ok := tm.space.Lookup(virt); !ok || phys != virt || attr != Registers {
			t.Errorf("%#x: got %#x, %#x, %v; want 1:1 Registers", virt, phys, attr, ok)
		}
	}

	dev, virt, size := g.Part()
	region := tm.DMAFrames()

	if size != 3*FrameSize || dev != virt || g.dmaSize() != int(size) {
		t.Fatalf("part %#x for the device, %#x for the processor, %d bytes", dev, virt, size)
	}

	if total, free := region.Usage(DMA); total-free != size {
		t.Errorf("the DMA region gave %d bytes, want the part", total-free)
	}

	for v := dev; v < dev+size; v += FrameSize {
		if phys, attr, ok := tm.space.Lookup(v); !ok || phys != v || attr != Data {
			t.Errorf("%#x: got %#x, %#x, %v; want 1:1 Data", v, phys, attr, ok)
		}
	}

	if !zero(g.dmaBytes()) {
		t.Error("new DMA part not cleared")
	}

	// The DMA memory is a kind of its own: no limit counts it.
	if o.Bytes() != 0 || len(o.Pieces()) != 1 || o.Pieces()[0] != g {
		t.Fatalf("%v: pieces %v", o, o.Pieces())
	}

	if _, err := tm.Give(tm.NewOwner("other driver", 0), DeviceMemory{Name: "other", Registers: []Range{{Start: device + FrameSize, End: device + 2*FrameSize}}}); err == nil {
		t.Error("registers of another device: no error")
	}

	data := g.dmaBytes()
	data[0], data[len(data)-1] = 0xab, 0xcd

	if err := g.Release(); err != nil {
		t.Fatal(err)
	}

	if !zero(data) {
		t.Error("released DMA part not cleared")
	}

	tm.expectEmpty(t, o)

	if _, free := region.Usage(DMA); free != 8*FrameSize {
		t.Errorf("after Release: %d bytes of the DMA region free, want all", free)
	}

	if err := g.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}

	// The registers are free for another grant again.
	g, err = tm.Give(o, nicMemory(0))

	if err != nil {
		t.Fatal(err)
	}

	if _, _, size := g.Part(); size != 0 || g.dmaBytes() != nil {
		t.Errorf("device without DMA has a part of %d bytes", size)
	}

	if err := g.Release(); err != nil {
		t.Fatal(err)
	}

	tm.expectEmpty(t, o)
}

func TestGiveRefuses(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("driver", 0)

	for _, c := range []struct {
		name string
		regs []Range
	}{
		{"memory", []Range{{Start: tm.memory.Start + FrameSize, End: tm.memory.Start + FrameSize + 1}}},
		{"window", []Range{{Start: tm.window.End - 1, End: tm.window.End + 1}}},
		{"no bytes", []Range{{Start: device, End: device}}},
		{"wrap", []Range{{Start: ^uint64(0) - FrameSize, End: ^uint64(0)}}},
		{"overlap", []Range{{Start: device, End: device + FrameSize}, {Start: device + 0x800, End: device + 0x900}}},
	} {
		if _, err := tm.Give(o, DeviceMemory{Name: c.name, Registers: c.regs}); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}

	// A failed step gives back what the steps before it took.
	if _, err := tm.Give(o, nicMemory(9*FrameSize)); !errors.Is(err, ErrNoMemory) {
		t.Errorf("part larger than the DMA region: got %v, want %v", err, ErrNoMemory)
	}

	tm.space.failAfter(1)

	if _, err := tm.Give(o, nicMemory(FrameSize)); err == nil {
		t.Error("failed map of the registers: no error")
	}

	tm.expectEmpty(t, o)

	if _, free := tm.DMAFrames().Usage(DMA); free != 8*FrameSize {
		t.Errorf("%d bytes of the DMA region free, want all", free)
	}

	if g, err := tm.Give(o, nicMemory(FrameSize)); err != nil {
		t.Errorf("Give after the refusals: %v", err)
	} else if err := g.Release(); err != nil {
		t.Error(err)
	}
}

func TestGiveDomain(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("driver", 0)
	domain := newTestSpace()

	if err := o.SetDomain(domain); err != nil {
		t.Fatal(err)
	}

	if err := o.SetDomain(newTestSpace()); err == nil {
		t.Error("second domain: no error")
	}

	g, err := tm.Give(o, nicMemory(FrameSize))

	if err != nil {
		t.Fatal(err)
	}

	dev, _, _ := g.Part()

	if phys, attr, ok := domain.Lookup(dev); !ok || phys != dev || attr != Data {
		t.Errorf("domain: got %#x, %#x, %v; want the part 1:1 as Data", phys, attr, ok)
	}

	if mapped, _ := domain.pages(); mapped != 1 {
		t.Errorf("domain: %d pages mapped, want only the part", mapped)
	}

	if err := g.Release(); err != nil {
		t.Fatal(err)
	}

	if mapped, waiting := domain.pages(); mapped != 0 || waiting != 0 || domain.flushes != 1 {
		t.Errorf("domain after Release: %d mapped, %d waiting, %d flushes", mapped, waiting, domain.flushes)
	}

	tm.expectEmpty(t, o)

	// A failed map in the domain gives everything back.
	domain.fail = errors.New("test: domain full")

	if _, err := tm.Give(o, nicMemory(FrameSize)); err == nil {
		t.Fatal("Give with a failed map in the domain: no error")
	}

	tm.expectEmpty(t, o)

	// The domain must come before the first device.
	other := tm.NewOwner("other driver", 0)

	if _, err := tm.Give(other, DeviceMemory{Name: "other", DMA: FrameSize}); err != nil {
		t.Fatal(err)
	}

	if err := other.SetDomain(newTestSpace()); err == nil {
		t.Error("domain after a device: no error")
	}
}

func TestGiveOwnSpace(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("process", 0)
	ps := newTestSpace()
	win := Range{Start: process, End: process + 16*FrameSize}

	if err := o.SetSpace(ps, win); err != nil {
		t.Fatal(err)
	}

	if err := o.SetSpace(newTestSpace(), win); err == nil {
		t.Error("second address space: no error")
	}

	// The IOMMU must confine the device of a process.
	if _, err := tm.Give(o, nicMemory(FrameSize)); err == nil {
		t.Fatal("DMA for a process without a domain: no error")
	}

	domain := newTestSpace()

	if err := o.SetDomain(domain); err != nil {
		t.Fatal(err)
	}

	g, err := tm.Give(o, nicMemory(FrameSize))

	if err != nil {
		t.Fatal(err)
	}

	// The registers lie only in the window of the process.
	reg := g.Register(0)

	if reg < win.Start || reg >= win.End || reg%FrameSize != 0x10 {
		t.Errorf("registers at %#x, want an address in the window with the offset 0x10", reg)
	}

	if phys, attr, ok := ps.Lookup(reg); !ok || phys != device+0x10 || attr != Registers {
		t.Errorf("process: got %#x, %#x, %v; want the registers", phys, attr, ok)
	}

	if _, _, ok := tm.space.Lookup(device); ok {
		t.Error("the registers of the process are mapped in the kernel")
	}

	// The part lies in the window of the process, in its domain, and
	// 1:1 in the kernel, which clears it.
	dev, virt, size := g.Part()

	if virt < win.Start || virt+size > win.End {
		t.Errorf("part at %#x for the process, want an address in the window", virt)
	}

	for _, c := range []struct {
		name string
		s    *testSpace
		virt uint64
	}{{"process", ps, virt}, {"domain", domain, dev}, {"kernel", tm.space, dev}} {
		if phys, attr, ok := c.s.Lookup(c.virt); !ok || phys != dev || attr != Data {
			t.Errorf("%s: got %#x, %#x, %v; want the part as Data", c.name, phys, attr, ok)
		}
	}

	if err := o.SetSpace(newTestSpace(), win); err == nil {
		t.Error("address space after a device: no error")
	}

	if err := o.End(); err != nil {
		t.Fatal(err)
	}

	for _, s := range []*testSpace{ps, domain} {
		if mapped, waiting := s.pages(); mapped != 0 || waiting != 0 {
			t.Errorf("after End: %d pages mapped, %d waiting", mapped, waiting)
		}
	}

	tm.expectEmpty(t, o)

	if free, _ := o.window.Free(); free != win.End-win.Start {
		t.Errorf("window of the process: %d bytes free, want all", free)
	}
}

func TestGiveWithoutDMARegion(t *testing.T) {
	tm := newBareManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("processor", 0)

	if _, err := tm.Give(o, nicMemory(FrameSize)); err == nil {
		t.Error("DMA part without a DMA region: no error")
	}

	tm.expectEmpty(t, o)

	// The registers of a device without DMA need no region.
	g, err := tm.Give(o, nicMemory(0))

	if err != nil {
		t.Fatal(err)
	}

	if err := g.Release(); err != nil {
		t.Fatal(err)
	}

	tm.expectEmpty(t, o)
}

func TestGiveDriverOfProcess(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	proc := tm.NewOwner("process", 0)
	ps := newTestSpace()
	win := Range{Start: process, End: process + 16*FrameSize}

	if err := proc.SetSpace(ps, win); err != nil {
		t.Fatal(err)
	}

	if err := proc.SetDomain(newTestSpace()); err != nil {
		t.Fatal(err)
	}

	driver, err := proc.NewChild("driver", 0)

	if err != nil {
		t.Fatal(err)
	}

	// The driver runs in the address space of its process, but the
	// domain of the process is not the domain of the driver.
	if _, err := tm.Give(driver, nicMemory(FrameSize)); err == nil {
		t.Fatal("DMA for a driver in a process without its own domain: no error")
	}

	domain := newTestSpace()

	if err := driver.SetDomain(domain); err != nil {
		t.Fatal(err)
	}

	g, err := tm.Give(driver, nicMemory(FrameSize))

	if err != nil {
		t.Fatal(err)
	}

	dev, virt, _ := g.Part()

	if phys, _, ok := ps.Lookup(virt); !ok || phys != dev || virt < win.Start || virt >= win.End {
		t.Errorf("part at %#x: got %#x, %v; want the part in the window of the process", virt, phys, ok)
	}

	if phys, _, ok := domain.Lookup(dev); !ok || phys != dev {
		t.Errorf("domain of the driver: got %#x, %v; want the part", phys, ok)
	}

	// The end of the process ends its driver and gives back its device.
	if err := proc.End(); err != nil {
		t.Fatal(err)
	}

	if !driver.Ended() {
		t.Error("the driver did not end with its process")
	}

	for _, s := range []*testSpace{ps, domain} {
		if mapped, waiting := s.pages(); mapped != 0 || waiting != 0 {
			t.Errorf("after End: %d pages mapped, %d waiting", mapped, waiting)
		}
	}

	tm.expectEmpty(t, driver)
}

// dmaBytes returns the DMA part of g for the kernel, or nil without one.
func (g *Grant) dmaBytes() []byte {
	v, _ := g.DMA()
	return v.Bytes()
}

// dmaSize returns the size of the DMA part of g for the kernel.
func (g *Grant) dmaSize() int {
	v, _ := g.DMA()
	return v.Size()
}
