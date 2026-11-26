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
	"sync"
	"testing"
)

const (
	kib = 1 << 10
	mib = 1 << 20
	gib = 1 << 30
)

// newFrames returns the frames of ranges with storage of their own.
func newFrames(t testing.TB, ranges ...Range) *Frames {
	t.Helper()

	return newDMAFrames(t, Range{}, ranges...)
}

// newDMAFrames returns the frames of ranges with the DMA region dma and
// storage of their own.
func newDMAFrames(t testing.TB, dma Range, ranges ...Range) *Frames {
	t.Helper()

	n, err := Words(ranges, dma)

	if err != nil {
		t.Fatal(err)
	}

	f, err := New(ranges, dma, make([]uint64, n))

	if err != nil {
		t.Fatal(err)
	}

	return f
}

// mustPanic fails the test if fn returns without a panic.
func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()

	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", what)
		}
	}()

	fn()
}

func TestSplit(t *testing.T) {
	areas, err := split([]Range{
		{Start: 4*gib - 8*kib + 1, End: 4*gib + 12*kib + 5}, // across 4 GiB, not aligned
		{Start: 1 * mib, End: 1*mib + 4*kib},                // one frame, given after a higher range
		{Start: 2 * mib, End: 2*mib + 100},                  // less than a frame
	}, Range{})

	if err != nil {
		t.Fatal(err)
	}

	want := []area{
		{start: 1 * mib, frames: 1, zone: Low},
		{start: 4*gib - 4*kib, frames: 1, zone: Low},
		{start: 4 * gib, frames: 3, zone: High},
	}

	if len(areas) != len(want) {
		t.Fatalf("got %d areas, want %d: %+v", len(areas), len(want), areas)
	}

	for i, a := range areas {
		if a.start != want[i].start || a.frames != want[i].frames || a.zone != want[i].zone {
			t.Errorf("area %d: got start %#x, %d frames, zone %v; want start %#x, %d frames, zone %v",
				i, a.start, a.frames, a.zone, want[i].start, want[i].frames, want[i].zone)
		}
	}
}

func TestSplitRefuses(t *testing.T) {
	for _, ranges := range [][]Range{
		{{Start: 2 * mib, End: 1 * mib}},
		{{Start: 1 * mib, End: 3 * mib}, {Start: 2 * mib, End: 4 * mib}},
	} {
		if _, err := Words(ranges, Range{}); err == nil {
			t.Errorf("%+v: no error", ranges)
		}
	}
}

func TestNewStorage(t *testing.T) {
	ranges := []Range{{Start: 0, End: 65 * FrameSize}}

	if _, err := New(ranges, Range{}, make([]uint64, 1)); err == nil {
		t.Error("storage too small: no error")
	}

	storage := []uint64{^uint64(0), ^uint64(0)}
	f, err := New(ranges, Range{}, storage)

	if err != nil {
		t.Fatal(err)
	}

	if total, free := f.Usage(Low); total != 65*FrameSize || free != total {
		t.Errorf("got %d bytes, %d free; want %d, all free", total, free, 65*FrameSize)
	}

	if storage[0] != 0 || storage[1] != ^uint64(1) {
		t.Errorf("storage not prepared: %#x %#x", storage[0], storage[1])
	}
}

func TestAllocZones(t *testing.T) {
	f := newFrames(t, Range{Start: 1 * mib, End: 1*mib + 2*FrameSize}, Range{Start: 8 * gib, End: 8*gib + FrameSize})
	o := NewOwner("test", 0)

	high, err := f.Alloc(o, High)

	if err != nil || high != 8*gib {
		t.Fatalf("High: got %#x, %v; want %#x", high, err, uint64(8*gib))
	}

	if _, err := f.Alloc(o, High); !errors.Is(err, ErrNoMemory) {
		t.Errorf("High after its last frame: got %v, want ErrNoMemory", err)
	}

	for range 2 {
		if addr, err := f.Alloc(o, Low); err != nil || addr >= lowEnd {
			t.Fatalf("Low: got %#x, %v", addr, err)
		}
	}

	if got := o.Bytes(); got != 3*FrameSize {
		t.Errorf("owner holds %d bytes, want %d", got, 3*FrameSize)
	}

	f.Free(o, high, 1)

	if got := o.Bytes(); got != 2*FrameSize {
		t.Errorf("owner holds %d bytes after free, want %d", got, 2*FrameSize)
	}

	if _, free := f.Usage(High); free != FrameSize {
		t.Errorf("High has %d free bytes after free, want %d", free, FrameSize)
	}
}

func TestLimit(t *testing.T) {
	f := newFrames(t, Range{Start: 4 * gib, End: 4*gib + 8*FrameSize})
	o := NewOwner("limited", 3*FrameSize+100)

	if o.Limit() != 3*FrameSize {
		t.Errorf("limit %d, want %d", o.Limit(), 3*FrameSize)
	}

	if _, err := f.AllocRun(o, High, 4, FrameSize); !errors.Is(err, ErrLimit) {
		t.Errorf("4 frames: got %v, want ErrLimit", err)
	}

	addr, err := f.AllocRun(o, High, 3, FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Alloc(o, High); !errors.Is(err, ErrLimit) {
		t.Errorf("frame over the limit: got %v, want ErrLimit", err)
	}

	f.Free(o, addr, 3)

	if o.Bytes() != 0 {
		t.Errorf("owner holds %d bytes, want 0", o.Bytes())
	}

	// A failed allocation gives the count back.
	small := NewOwner("small", 0)

	if _, err := f.AllocRun(small, High, 9, FrameSize); !errors.Is(err, ErrNoMemory) {
		t.Errorf("9 of 8 frames: got %v, want ErrNoMemory", err)
	}

	if small.Bytes() != 0 {
		t.Errorf("owner holds %d bytes after a failed allocation, want 0", small.Bytes())
	}
}

func TestAllocRun(t *testing.T) {
	f := newFrames(t, Range{Start: 4*gib + FrameSize, End: 4*gib + 64*FrameSize})
	o := NewOwner("test", 0)

	// Fill frames 2 and 5 of the area, so that a run of 4 fits only
	// after them.
	a := &f.areas[0]
	a.mark(2, 1, true)
	a.mark(5, 1, true)
	a.free -= 2

	run, err := f.AllocRun(o, High, 4, FrameSize)

	if err != nil || run != 4*gib+7*FrameSize {
		t.Errorf("run of 4: got %#x, %v; want %#x", run, err, uint64(4*gib+7*FrameSize))
	}

	aligned, err := f.AllocRun(o, High, 2, 16*FrameSize)

	if err != nil || aligned != 4*gib+16*FrameSize {
		t.Errorf("aligned run: got %#x, %v; want %#x", aligned, err, uint64(4*gib+16*FrameSize))
	}

	if _, err := f.AllocRun(o, High, 64, FrameSize); !errors.Is(err, ErrNoMemory) {
		t.Errorf("run larger than the area: got %v, want ErrNoMemory", err)
	}

	mustPanic(t, "no frames", func() { f.AllocRun(o, High, 0, FrameSize) })
	mustPanic(t, "small alignment", func() { f.AllocRun(o, High, 1, 512) })
	mustPanic(t, "alignment not a power of two", func() { f.AllocRun(o, High, 1, 3*FrameSize) })
}

func TestFreeRefuses(t *testing.T) {
	f := newFrames(t, Range{Start: 4 * gib, End: 4*gib + 4*FrameSize}, Range{Start: 4*gib + 4*FrameSize, End: 4*gib + 8*FrameSize})
	o := NewOwner("test", 0)

	run, err := f.AllocRun(o, High, 4, FrameSize)

	if err != nil {
		t.Fatal(err)
	}

	mustPanic(t, "not the start of a frame", func() { f.Free(o, run+1, 1) })
	mustPanic(t, "outside the memory list", func() { f.Free(o, 2*gib, 1) })
	mustPanic(t, "across two areas", func() { f.Free(o, run+3*FrameSize, 2) })
	mustPanic(t, "more than the owner holds", func() { f.Free(NewOwner("other", 0), run, 1) })

	f.Free(o, run, 4)

	mustPanic(t, "free frames", func() { f.Free(o, run, 1) })

	if _, free := f.Usage(High); free != 8*FrameSize {
		t.Errorf("free bytes %d, want %d", free, 8*FrameSize)
	}
}

func TestFindOneWraps(t *testing.T) {
	f := newFrames(t, Range{Start: 4 * gib, End: 4*gib + 130*FrameSize})
	o := NewOwner("test", 0)

	var addrs []uint64

	for range 130 {
		addr, err := f.Alloc(o, High)

		if err != nil {
			t.Fatal(err)
		}

		addrs = append(addrs, addr)
	}

	f.Free(o, addrs[3], 1)

	if addr, err := f.Alloc(o, High); err != nil || addr != addrs[3] {
		t.Errorf("after the end: got %#x, %v; want %#x", addr, err, addrs[3])
	}
}

func TestAllocUpTo(t *testing.T) {
	const base = 4 * gib

	// 200 frames: the run crosses words, and the area ends inside a word.
	f := newFrames(t, Range{Start: base, End: base + 200*FrameSize})
	o := NewOwner("test", 150*FrameSize)

	addr, got, err := f.AllocUpTo(o, High, 70)

	if err != nil || addr != base || got != 70 {
		t.Fatalf("first: got %#x, %d, %v; want %#x, 70", addr, got, err, uint64(base))
	}

	// A used frame in the middle ends the run before it.
	if _, err := f.Alloc(o, High); err != nil {
		t.Fatal(err)
	}

	f.Free(o, base+10*FrameSize, 20)

	addr, got, err = f.AllocUpTo(o, High, 50)

	if err != nil || addr != base+71*FrameSize || got != 50 {
		t.Errorf("after the used frame: got %#x, %d, %v; want frame 71, 50", addr, got, err)
	}

	// The owner pays only for the frames that it got.
	if want := uint64(70 - 20 + 1 + 50); o.Bytes() != want*FrameSize {
		t.Errorf("owner holds %d frames, want %d", o.Bytes()/FrameSize, want)
	}

	// The limit counts the whole request.
	if _, _, err := f.AllocUpTo(o, High, 50); err != ErrLimit {
		t.Errorf("over the limit: got %v, want ErrLimit", err)
	}

	// The search goes on after the last run, to the end of the area.
	addr, got, err = f.AllocUpTo(o, High, 28)

	if err != nil || addr != base+121*FrameSize || got != 28 {
		t.Errorf("at the end: got %#x, %d, %v; want frame 121, 28", addr, got, err)
	}

	o2 := NewOwner("test 2", 0)
	addr, got, err = f.AllocUpTo(o2, High, 100)

	if err != nil || addr != base+149*FrameSize || got != 51 {
		t.Errorf("up to the end of the area: got %#x, %d, %v; want frame 149, 51", addr, got, err)
	}

	// Then the search wraps to the freed frames.
	addr, got, err = f.AllocUpTo(o2, High, 100)

	if err != nil || addr != base+10*FrameSize || got != 20 {
		t.Errorf("after the wrap: got %#x, %d, %v; want frame 10, 20", addr, got, err)
	}

	if _, _, err := f.AllocUpTo(o2, High, 1); err != ErrNoMemory {
		t.Errorf("no frame left: got %v, want ErrNoMemory", err)
	}

	if o2.Bytes() != 71*FrameSize {
		t.Errorf("owner 2 holds %d frames, want 71", o2.Bytes()/FrameSize)
	}

	mustPanic(t, "AllocUpTo of 0 frames", func() { f.AllocUpTo(o, High, 0) })
}

func TestFreeRun(t *testing.T) {
	a := area{frames: 130, used: make([]uint64, 3)}
	all := ^uint64(0)
	a.used[2] = all << 2 // the bits after the last frame

	for _, c := range []struct{ i, n, want uint64 }{
		{0, 1, 1},
		{0, 64, 64},
		{0, 200, 130},
		{63, 3, 3},
		{100, 100, 30},
		{129, 5, 1},
	} {
		if got := a.freeRun(c.i, c.n); got != c.want {
			t.Errorf("freeRun(%d, %d) = %d, want %d", c.i, c.n, got, c.want)
		}
	}

	a.mark(60, 10, true)

	if !a.allUsed(60, 10) || a.allUsed(59, 2) || a.allUsed(69, 2) {
		t.Error("mark across a word: wrong bits")
	}

	if got := a.freeRun(0, 100); got != 60 {
		t.Errorf("freeRun before a used frame = %d, want 60", got)
	}

	if got := a.freeRun(70, 100); got != 60 {
		t.Errorf("freeRun after the used frames = %d, want 60", got)
	}

	a.mark(60, 10, false)

	if a.used[0] != 0 || a.used[1] != 0 {
		t.Error("mark as free across a word: bits stay set")
	}
}

func TestConcurrent(t *testing.T) {
	const workers, rounds = 8, 2000

	f := newFrames(t, Range{Start: 4 * gib, End: 4*gib + 64*mib})
	owners := make([]*Owner, workers)

	var held sync.Map
	var wg sync.WaitGroup

	for w := range workers {
		owners[w] = NewOwner("worker", 0)
		wg.Add(1)

		go func(o *Owner) {
			defer wg.Done()

			var mine []uint64

			for i := range rounds {
				if i%3 == 2 && len(mine) > 0 {
					addr := mine[len(mine)-1]
					mine = mine[:len(mine)-1]
					held.Delete(addr)
					f.Free(o, addr, 1)
					continue
				}

				addr, err := f.Alloc(o, High)

				if err != nil {
					t.Error(err)
					return
				}

				if _, dup := held.LoadOrStore(addr, o); dup {
					t.Errorf("frame %#x given out twice", addr)
				}

				mine = append(mine, addr)
			}
		}(owners[w])
	}

	wg.Wait()

	var sum uint64

	for _, o := range owners {
		sum += o.Bytes()
	}

	total, free := f.Usage(High)

	if total-free != sum {
		t.Errorf("frames in use %d bytes, owners hold %d", total-free, sum)
	}
}

func TestDMARegion(t *testing.T) {
	dma := Range{Start: 3 * gib, End: 3*gib + 4*FrameSize}
	areas, err := split([]Range{{Start: 3*gib - 2*FrameSize, End: 4*gib + FrameSize}}, dma)

	if err != nil {
		t.Fatal(err)
	}

	want := []area{
		{start: 3*gib - 2*FrameSize, frames: 2, zone: Low},
		{start: 3 * gib, frames: 4, zone: DMA},
		{start: 3*gib + 4*FrameSize, frames: (gib - 4*FrameSize) / FrameSize, zone: Low},
		{start: 4 * gib, frames: 1, zone: High},
	}

	if len(areas) != len(want) {
		t.Fatalf("%d areas, want %d: %+v", len(areas), len(want), areas)
	}

	for i, a := range areas {
		if a.start != want[i].start || a.frames != want[i].frames || a.zone != want[i].zone {
			t.Errorf("area %d: %#x, %d frames, %v; want %#x, %d frames, %v", i, a.start, a.frames, a.zone, want[i].start, want[i].frames, want[i].zone)
		}
	}

	f := newDMAFrames(t, dma, Range{Start: 3 * gib, End: 3*gib + 8*FrameSize})
	o := NewOwner("driver", 0)

	for range 4 {
		if addr, err := f.Alloc(o, DMA); err != nil || addr < dma.Start || addr >= dma.End {
			t.Fatalf("DMA frame: got %#x, %v; want one in the DMA region", addr, err)
		}
	}

	if _, err := f.Alloc(o, DMA); err != ErrNoMemory {
		t.Errorf("fifth DMA frame: got %v, want %v", err, ErrNoMemory)
	}

	if addr, err := f.Alloc(o, Low); err != nil || addr < dma.End {
		t.Errorf("Low frame: got %#x, %v; want one after the DMA region", addr, err)
	}

	for _, bad := range []Range{
		{Start: 3*gib + 7*FrameSize, End: 3*gib + 9*FrameSize}, // not all free memory
		{Start: 3*gib + 1, End: 3*gib + FrameSize},             // not whole frames
	} {
		if _, err := Words([]Range{{Start: 3 * gib, End: 3*gib + 8*FrameSize}}, bad); err == nil {
			t.Errorf("DMA region %#x-%#x: no error", bad.Start, bad.End)
		}
	}
}
