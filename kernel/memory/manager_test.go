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

// testManager is a Manager over 16 frames of the memory of the test, with
// a window of 16 pages in other memory of the test, so that the clearing
// of buffers and regions writes to real memory.
type testManager struct {
	*Manager
	space  *testSpace
	memory Range
	window Range
	bufs   [][]byte
}

// newBareManager returns a testManager without a DMA region.
func newBareManager(t *testing.T, windows ...Range) *testManager {
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

// newTestManager returns a testManager whose DMA region is 8 of its
// frames. The owner of the region is not an owner of the manager, so that
// the tests see only their own owners.
func newTestManager(t *testing.T) *testManager {
	t.Helper()

	tm := newBareManager(t)

	if _, err := tm.StartDMA(NewOwner("DMA region", 0), zoneOf(tm.memory), 8*FrameSize, FrameSize); err != nil {
		t.Fatal(err)
	}

	return tm
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

func TestStartDMA(t *testing.T) {
	tm := newBareManager(t)
	defer runtime.KeepAlive(tm.bufs)

	driver := tm.NewOwner("driver", 0)

	if _, err := tm.Give(driver, DeviceMemory{Name: "nic", DMA: FrameSize}); err == nil {
		t.Error("DMA part before StartDMA: no error")
	}

	region := NewOwner("DMA region", 0)
	z := zoneOf(tm.memory)

	for _, c := range []struct{ size, align uint64 }{{0, FrameSize}, {FrameSize + 1, FrameSize}, {FrameSize, 3 * FrameSize}, {17 * FrameSize, FrameSize}} {
		if _, err := tm.StartDMA(region, z, c.size, c.align); err == nil {
			t.Errorf("DMA region of %d bytes aligned to %d: no error", c.size, c.align)
		}
	}

	if region.Bytes() != 0 || tm.DMAFrames() != nil {
		t.Fatalf("after refused regions: %v, DMA frames %v", region, tm.DMAFrames())
	}

	r, err := tm.StartDMA(region, z, 4*FrameSize, 4*FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	if r.End-r.Start != 4*FrameSize || r.Start%(4*FrameSize) != 0 || r.Start < tm.memory.Start || r.End > tm.memory.End || region.Bytes() != 4*FrameSize {
		t.Fatalf("region %#x-%#x, %v", r.Start, r.End, region)
	}

	if total, free := tm.DMAFrames().Usage(DMA); total != 4*FrameSize || free != 4*FrameSize {
		t.Errorf("DMA frames: %d bytes, %d free; want 4 frames free", total, free)
	}

	if _, err := tm.StartDMA(region, z, FrameSize, FrameSize); err == nil {
		t.Error("second DMA region: no error")
	}

	// The parts come only from the region, and it does not grow.
	part := DeviceMemory{Name: "nic", DMA: FrameSize}

	for range 4 {
		g, err := tm.Give(driver, part)

		if err != nil {
			t.Fatal(err)
		}

		if dev, _, _ := g.Part(); dev < r.Start || dev >= r.End {
			t.Errorf("part %v outside the region", g)
		}
	}

	if _, err := tm.Give(driver, part); !errors.Is(err, ErrNoMemory) {
		t.Errorf("part in a full region: got %v, want %v", err, ErrNoMemory)
	}

	if err := driver.End(); err != nil {
		t.Fatal(err)
	}

	if _, free := tm.DMAFrames().Usage(DMA); free != 4*FrameSize || region.Bytes() != 4*FrameSize {
		t.Errorf("after End: %d bytes free, %v", free, region)
	}
}

func TestStartDMARefusesWindow(t *testing.T) {
	// The window of the Go memory lies on all memory of the manager.
	tm := newBareManager(t, Range{Start: 0, End: 1 << 47})
	defer runtime.KeepAlive(tm.bufs)

	region := NewOwner("DMA region", 0)

	if _, err := tm.StartDMA(region, zoneOf(tm.memory), FrameSize, FrameSize); err == nil {
		t.Fatal("DMA region in a window: no error")
	}

	if region.Bytes() != 0 || tm.DMAFrames() != nil {
		t.Errorf("after the refusal: %v, DMA frames %v", region, tm.DMAFrames())
	}
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
	nic := DeviceMemory{Name: "nic", Registers: []Range{{Start: device, End: device + FrameSize}}, DMA: FrameSize}

	if _, err := tm.Give(o, nic); err != nil {
		t.Fatal(err)
	}

	if _, err := tm.Alloc(o, FrameSize); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(tm.Owners(), o) || len(o.Pieces()) != 2 {
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
	if _, err := tm.Give(o, nic); !errors.Is(err, ErrEnded) {
		t.Errorf("Give after End: got %v, want %v", err, ErrEnded)
	}

	if _, err := tm.Alloc(o, FrameSize); !errors.Is(err, ErrEnded) {
		t.Errorf("Alloc after End: got %v, want %v", err, ErrEnded)
	}

	if err := o.SetDomain(newTestSpace()); !errors.Is(err, ErrEnded) {
		t.Errorf("SetDomain after End: got %v, want %v", err, ErrEnded)
	}

	if err := o.SetSpace(newTestSpace(), Range{Start: process, End: process + gib}); !errors.Is(err, ErrEnded) {
		t.Errorf("SetSpace after End: got %v, want %v", err, ErrEnded)
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

func TestOwnerChildren(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	kernel := tm.NewOwner("kernel", 4*FrameSize)
	driver, err := kernel.NewChild("driver", 0)

	if err != nil {
		t.Fatal(err)
	}

	heap, err := kernel.NewChild("Go heap", 2*FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	if driver.Parent() != kernel || !slices.Contains(tm.Owners(), driver) || !slices.Contains(kernel.Pieces(), Piece(driver)) {
		t.Fatal("the child is not a piece of its parent or not listed by the manager")
	}

	// The children share the limit of the parent.
	if _, err := tm.Alloc(driver, 3*FrameSize); err != nil {
		t.Fatal(err)
	}

	if kernel.Bytes() != 3*FrameSize || driver.Bytes() != 3*FrameSize {
		t.Fatalf("kernel %d bytes, driver %d bytes; want 3 frames each", kernel.Bytes(), driver.Bytes())
	}

	frame, err := tm.Frames().Alloc(heap, zoneOf(tm.memory))

	if err != nil {
		t.Fatal(err)
	}

	// The fifth frame passes the limit of the heap, not the limit of
	// the kernel.
	if _, err := tm.Frames().Alloc(heap, zoneOf(tm.memory)); !errors.Is(err, ErrLimit) {
		t.Fatalf("a fifth frame for the kernel: got %v, want %v", err, ErrLimit)
	}

	if heap.Bytes() != FrameSize || driver.Bytes() != 3*FrameSize || kernel.Bytes() != 4*FrameSize {
		t.Fatalf("after a refused frame: heap %d, driver %d, kernel %d bytes", heap.Bytes(), driver.Bytes(), kernel.Bytes())
	}

	tm.Frames().Free(heap, frame, 1)

	// The end of the parent ends the children.
	if err := kernel.End(); err != nil {
		t.Fatal(err)
	}

	if !driver.Ended() || !heap.Ended() || kernel.Bytes() != 0 || len(tm.Owners()) != 0 {
		t.Fatalf("after End: driver ended %v, heap ended %v, kernel %d bytes, owners %v", driver.Ended(), heap.Ended(), kernel.Bytes(), tm.Owners())
	}

	if _, err := kernel.NewChild("late", 0); !errors.Is(err, ErrEnded) {
		t.Errorf("NewChild after End: got %v, want %v", err, ErrEnded)
	}

	// A child that ends leaves its parent.
	parent := tm.NewOwner("guest", 0)
	child, _ := parent.NewChild("vCPU", 0)

	if err := child.End(); err != nil || len(parent.Pieces()) != 0 {
		t.Errorf("End of the child: %v, parent pieces %v", err, parent.Pieces())
	}
}
