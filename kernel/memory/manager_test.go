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
	"slices"
	"testing"
)

// device is the address of device registers in the tests, outside the
// memory of the test.
const device = 0xfee0_0000

// testManager is a Manager over 16 frames of the memory of the test, with a
// window of 16 pages in other memory of the test, so that the clearing of
// buffers and regions writes to real memory.
type testManager struct {
	*Manager
	space  *testSpace
	memory Range
	window Range
	bufs   [][]byte
}

func newTestManager(t *testing.T, windows ...Range) *testManager {
	t.Helper()

	mem, membuf := hostMemory(16)
	win, winbuf := hostMemory(16)
	s := newTestSpace()

	m, err := NewManager(Config{
		Frames:  newFrames(t, mem),
		Space:   s,
		Window:  win,
		Memory:  []Range{mem},
		Windows: windows,
	})

	if err != nil {
		t.Fatal(err)
	}

	return &testManager{Manager: m, space: s, memory: mem, window: win, bufs: [][]byte{membuf, winbuf}}
}

// expectEmpty checks that nothing is mapped, that o holds nothing and that
// the frames and the window are free.
func (tm *testManager) expectEmpty(t *testing.T, o *Owner) {
	t.Helper()

	if mapped, waiting := tm.space.pages(); mapped != 0 || waiting != 0 {
		t.Errorf("%d pages mapped and %d waiting, want none", mapped, waiting)
	}

	if o.Bytes() != 0 || len(o.Pieces()) != 0 {
		t.Errorf("%v", o)
	}

	if free, _ := tm.Manager.window.Free(); free != tm.window.End-tm.window.Start {
		t.Errorf("window: %d bytes free, want all", free)
	}
}

// zero reports whether b holds only zeros.
func zero(b []byte) bool {
	return !slices.ContainsFunc(b, func(c byte) bool { return c != 0 })
}

func TestNewManagerRefuses(t *testing.T) {
	if _, err := NewManager(Config{Window: Range{Start: window, End: window + gib}}); err == nil {
		t.Error("incomplete configuration: no error")
	}

	if _, err := NewManager(Config{Frames: newFrames(t), Space: newTestSpace()}); err == nil {
		t.Error("empty window: no error")
	}
}

func TestDMA(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("driver", 0)
	b, err := tm.DMA(o, zoneOf(tm.memory), 3*FrameSize-10, 2*FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	if b.Size() != 3*FrameSize || b.Addr()%(2*FrameSize) != 0 || len(b.Bytes()) != 3*FrameSize {
		t.Fatalf("buffer %v: size %d", b, b.Size())
	}

	if !zero(b.Bytes()) {
		t.Error("new DMA buffer not cleared")
	}

	for virt := b.Addr(); virt < b.Addr()+b.Size(); virt += FrameSize {
		if phys, attr, ok := tm.space.Lookup(virt); !ok || phys != virt || attr != Data {
			t.Errorf("%#x: got %#x, %#x, %v; want 1:1 Data", virt, phys, attr, ok)
		}
	}

	if o.Bytes() != 3*FrameSize || len(o.Pieces()) != 1 || o.Pieces()[0] != b {
		t.Fatalf("%v: pieces %v", o, o.Pieces())
	}

	data := b.Bytes()
	data[0], data[len(data)-1] = 0xab, 0xcd

	if err := b.Release(); err != nil {
		t.Fatal(err)
	}

	if !zero(data) {
		t.Error("released DMA buffer not cleared")
	}

	tm.expectEmpty(t, o)

	if err := b.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}

	for _, c := range []struct{ size, align uint64 }{{0, FrameSize}, {FrameSize, 3 * FrameSize}} {
		if _, err := tm.DMA(o, zoneOf(tm.memory), c.size, c.align); err == nil {
			t.Errorf("DMA of %d bytes aligned to %d: no error", c.size, c.align)
		}
	}
}

func TestDMADomain(t *testing.T) {
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

	b, err := tm.DMA(o, zoneOf(tm.memory), FrameSize, FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	if phys, attr, ok := domain.Lookup(b.Addr()); !ok || phys != b.Addr() || attr != Data {
		t.Errorf("domain: got %#x, %#x, %v; want 1:1 Data", phys, attr, ok)
	}

	if err := b.Release(); err != nil {
		t.Fatal(err)
	}

	if mapped, waiting := domain.pages(); mapped != 0 || waiting != 0 || domain.flushes != 1 {
		t.Errorf("domain after Release: %d mapped, %d waiting, %d flushes", mapped, waiting, domain.flushes)
	}

	tm.expectEmpty(t, o)

	// A failed map in the domain gives everything back.
	domain.fail = errors.New("test: domain full")

	if _, err := tm.DMA(o, zoneOf(tm.memory), FrameSize, FrameSize); err == nil {
		t.Fatal("DMA with a failed map in the domain: no error")
	}

	tm.expectEmpty(t, o)

	// The domain must come before the first buffer.
	other := tm.NewOwner("other driver", 0)

	if _, err := tm.DMA(other, zoneOf(tm.memory), FrameSize, FrameSize); err != nil {
		t.Fatal(err)
	}

	if err := other.SetDomain(newTestSpace()); err == nil {
		t.Error("domain after a DMA buffer: no error")
	}
}

func TestDMARefusesWindow(t *testing.T) {
	// The window of the Go memory lies on all memory of the manager.
	tm := newTestManager(t, Range{Start: 0, End: 1 << 47})
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("driver", 0)

	if _, err := tm.DMA(o, zoneOf(tm.memory), FrameSize, FrameSize); err == nil {
		t.Fatal("DMA buffer in a window: no error")
	}

	tm.expectEmpty(t, o)
}

func TestMapDevice(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("driver", 0)
	mp, err := tm.MapDevice(o, device+0x10, FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	if mp.Addr() != device || mp.Size() != 2*FrameSize {
		t.Fatalf("%v: want the two pages from %#x", mp, device)
	}

	if phys, attr, ok := tm.space.Lookup(device + FrameSize); !ok || phys != device+FrameSize || attr != Registers {
		t.Errorf("got %#x, %#x, %v; want 1:1 Registers", phys, attr, ok)
	}

	if _, err := tm.MapDevice(tm.NewOwner("other driver", 0), device, FrameSize); err == nil {
		t.Error("device mapped twice: no error")
	}

	for _, c := range []struct {
		name       string
		phys, size uint64
	}{
		{"memory", tm.memory.Start + FrameSize, 1},
		{"window", tm.window.End - 1, 2},
		{"no bytes", device, 0},
		{"wrap", ^uint64(0) - FrameSize, 2 * FrameSize},
	} {
		if _, err := tm.MapDevice(o, c.phys, c.size); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}

	if err := mp.Release(); err != nil {
		t.Fatal(err)
	}

	tm.expectEmpty(t, o)
}

func TestAlloc(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("kernel", 0)
	r, err := tm.Alloc(o, 2*FrameSize+1)

	if err != nil {
		t.Fatal(err)
	}

	if r.Size() != 3*FrameSize || r.Addr() < tm.window.Start+FrameSize || r.Addr()+r.Size() > tm.window.End-FrameSize {
		t.Fatalf("%v: want 3 pages between guard pages in the window %#x-%#x", r, tm.window.Start, tm.window.End)
	}

	if !zero(r.Bytes()) {
		t.Error("new region not cleared")
	}

	for _, guard := range []uint64{r.Addr() - FrameSize, r.Addr() + r.Size()} {
		if _, _, ok := tm.space.Lookup(guard); ok {
			t.Errorf("guard page %#x mapped", guard)
		}
	}

	for virt := r.Addr(); virt < r.Addr()+r.Size(); virt += FrameSize {
		if phys, attr, ok := tm.space.Lookup(virt); !ok || attr != Data || phys < tm.memory.Start || phys >= tm.memory.End {
			t.Errorf("%#x: got %#x, %#x, %v; want Data on a frame", virt, phys, attr, ok)
		}
	}

	// The guard pages stay out of other regions.
	r2, err := tm.Alloc(o, FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	if r2.Addr() < r.Addr()+r.Size()+FrameSize && r.Addr() < r2.Addr()+r2.Size()+FrameSize {
		t.Errorf("%v and %v share a guard page", r, r2)
	}

	data := r.Bytes()
	data[0] = 0xab

	if err := r.Release(); err != nil {
		t.Fatal(err)
	}

	if err := r2.Release(); err != nil {
		t.Fatal(err)
	}

	if data[0] != 0 {
		t.Error("released region not cleared")
	}

	tm.expectEmpty(t, o)

	if _, err := tm.Alloc(o, 0); err == nil {
		t.Error("region of 0 bytes: no error")
	}

	if _, err := tm.Alloc(o, 15*FrameSize); err == nil {
		t.Error("region larger than the window: no error")
	}

	// A failed map gives back the pages that were mapped.
	tm.space.failAfter(2)

	if _, err := tm.Alloc(o, 4*FrameSize); err == nil {
		t.Fatal("region with a failed map: no error")
	}

	tm.expectEmpty(t, o)
}

func TestOwnerEnd(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	o := tm.NewOwner("driver", 0)

	if _, err := tm.DMA(o, zoneOf(tm.memory), FrameSize, FrameSize); err != nil {
		t.Fatal(err)
	}

	if _, err := tm.MapDevice(o, device, FrameSize); err != nil {
		t.Fatal(err)
	}

	if _, err := tm.Alloc(o, FrameSize); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(tm.Owners(), o) || len(o.Pieces()) != 3 {
		t.Fatalf("%v: pieces %v", o, o.Pieces())
	}

	if err := o.End(); err != nil {
		t.Fatal(err)
	}

	tm.expectEmpty(t, o)

	if !o.Ended() || slices.Contains(tm.Owners(), o) {
		t.Error("the owner did not end")
	}

	if err := o.End(); err != nil {
		t.Errorf("second End: %v", err)
	}

	// An ended owner takes no new piece.
	if _, err := tm.DMA(o, zoneOf(tm.memory), FrameSize, FrameSize); !errors.Is(err, ErrEnded) {
		t.Errorf("DMA after End: got %v, want %v", err, ErrEnded)
	}

	if _, err := tm.MapDevice(o, device, FrameSize); !errors.Is(err, ErrEnded) {
		t.Errorf("MapDevice after End: got %v, want %v", err, ErrEnded)
	}

	if _, err := tm.Alloc(o, FrameSize); !errors.Is(err, ErrEnded) {
		t.Errorf("Alloc after End: got %v, want %v", err, ErrEnded)
	}

	if err := o.SetDomain(newTestSpace()); !errors.Is(err, ErrEnded) {
		t.Errorf("SetDomain after End: got %v, want %v", err, ErrEnded)
	}

	tm.expectEmpty(t, o)
}

// piece records the order of its release in a list.
type piece struct {
	name     string
	released *[]string
	err      error
}

func (p *piece) String() string {
	return p.name
}

func (p *piece) Release() error {
	*p.released = append(*p.released, p.name)
	return p.err
}

func TestOwnerPieces(t *testing.T) {
	var released []string

	o := NewOwner("process", 0)
	a := &piece{name: "a", released: &released}
	b := &piece{name: "b", released: &released, err: errors.New("busy")}
	c := &piece{name: "c", released: &released}

	for _, p := range []Piece{a, b, c} {
		if err := o.Hold(p); err != nil {
			t.Fatal(err)
		}
	}

	o.Drop(c)
	o.Drop(c)

	if got := o.Pieces(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("pieces %v, want [a b]", got)
	}

	// The frames that the owner holds outside a piece are an error too.
	if !o.take(1) {
		t.Fatal("take of one frame failed")
	}

	err := o.End()

	if err == nil || !slices.Equal(released, []string{"b", "a"}) {
		t.Fatalf("End: %v, released %v; want an error and the order b, a", err, released)
	}

	if err := o.Hold(c); !errors.Is(err, ErrEnded) {
		t.Errorf("Hold after End: got %v, want %v", err, ErrEnded)
	}

	if o.String() == "" || o.Name() != "process" || o.Limit() != 0 {
		t.Errorf("owner %v", o)
	}
}
