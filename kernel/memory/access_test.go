// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import (
	"runtime"
	"sync/atomic"
	"testing"
)

// expectPanic checks that f panics.
func expectPanic(t *testing.T, name string, f func()) {
	t.Helper()

	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", name)
		}
	}()

	f()
}

func TestHeapView(t *testing.T) {
	for _, n := range []int{0, 1, 7, 8, 9, 4096} {
		v := HeapView(n)

		if v.Size() != n || len(v.Bytes()) != n || cap(v.Bytes()) != n {
			t.Errorf("HeapView(%d): size %d, len %d, cap %d", n, v.Size(), len(v.Bytes()), cap(v.Bytes()))
		}

		if n > 0 && v.Addr()%8 != 0 {
			t.Errorf("HeapView(%d) at %#x, want 8-byte alignment", n, v.Addr())
		}

		if !zero(v.Bytes()) {
			t.Errorf("HeapView(%d) not zeroed", n)
		}
	}

	if v := HeapView(0); v.Addr() != 0 {
		t.Errorf("empty view at %#x, want 0", v.Addr())
	}

	expectPanic(t, "HeapView(-1)", func() { HeapView(-1) })
}

func TestViewAt(t *testing.T) {
	heap := HeapView(2 * FrameSize)
	defer runtime.KeepAlive(heap)

	v := viewAt(heap.Addr()+FrameSize, FrameSize)

	if v.Addr() != heap.Addr()+FrameSize || v.Size() != FrameSize || cap(v.Bytes()) != FrameSize {
		t.Fatalf("view at %#x of %d bytes, capacity %d", v.Addr(), v.Size(), cap(v.Bytes()))
	}

	v.Bytes()[0] = 0xa5
	v.Bytes()[FrameSize-1] = 0x5a

	if heap.Bytes()[FrameSize] != 0xa5 || heap.Bytes()[2*FrameSize-1] != 0x5a {
		t.Error("the view does not show the memory at its address")
	}

	part := v.Slice(16, 32)

	if part.Addr() != v.Addr()+16 || part.Size() != 32 || cap(part.Bytes()) != 32 {
		t.Errorf("slice at %#x of %d bytes, capacity %d", part.Addr(), part.Size(), cap(part.Bytes()))
	}

	expectPanic(t, "Slice past the end", func() { v.Slice(FrameSize-8, 16) })
	expectPanic(t, "Slice at a negative offset", func() { v.Slice(-1, 4) })
}

func TestViewWords(t *testing.T) {
	v := HeapView(64)

	v.Store32(4, 0xdeadbeef)
	v.Store64(8, 0x0123456789abcdef)

	if got := v.Load32(4); got != 0xdeadbeef {
		t.Errorf("Load32 = %#x", got)
	}

	if got := v.Load64(8); got != 0x0123456789abcdef {
		t.Errorf("Load64 = %#x", got)
	}

	// Little endian, as Bytes shows it.
	if b := v.Bytes(); b[4] != 0xef || b[8] != 0xef || b[15] != 0x01 {
		t.Errorf("bytes % x", b[:16])
	}

	for _, c := range []struct {
		name string
		f    func()
	}{
		{"Load32 unaligned", func() { v.Load32(2) }},
		{"Store32 unaligned", func() { v.Store32(1, 0) }},
		{"Load64 unaligned", func() { v.Load64(4) }},
		{"Store64 unaligned", func() { v.Store64(12, 0) }},
		{"Load32 past the end", func() { v.Load32(64) }},
		{"Load32 across the end", func() { v.Load32(62) }},
		{"Load64 across the end", func() { v.Load64(60) }},
		{"Load32 negative", func() { v.Load32(-4) }},
		{"Load64 at the largest offset", func() { v.Load64(int(^uint(0) >> 1)) }},
		{"Load32 of an empty view", func() { View{}.Load32(0) }},
	} {
		expectPanic(t, c.name, c.f)
	}
}

func TestRegisterView(t *testing.T) {
	heap := HeapView(64)
	defer runtime.KeepAlive(heap)

	r := RegisterView{addr: uintptr(heap.Addr()) + 16, size: 16}

	r.Write32(0, 0x11223344)
	r.Write64(8, 0x5566778899aabbcc)

	if got := r.Read32(0); got != 0x11223344 {
		t.Errorf("Read32 = %#x", got)
	}

	if got := r.Read64(8); got != 0x5566778899aabbcc {
		t.Errorf("Read64 = %#x", got)
	}

	if heap.Load32(16) != 0x11223344 || heap.Load64(24) != 0x5566778899aabbcc {
		t.Error("the registers do not lie at their address")
	}

	for _, c := range []struct {
		name string
		f    func()
	}{
		{"Read32 unaligned", func() { r.Read32(2) }},
		{"Write64 unaligned", func() { r.Write64(4, 0) }},
		{"Read32 past the end", func() { r.Read32(16) }},
		{"Read64 across the end", func() { r.Read64(12) }},
		{"Read32 negative", func() { r.Read32(-4) }},
		{"Read32 of an empty range", func() { RegisterView{}.Read32(0) }},
		{"Read32 of a range of 2 bytes", func() { RegisterView{addr: r.addr, size: 2}.Read32(0) }},
	} {
		expectPanic(t, c.name, c.f)
	}
}

func TestGrantViews(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	g, err := tm.Give(tm.NewOwner("driver", 0), nicMemory(2*FrameSize))

	if err != nil {
		t.Fatal(err)
	}

	r := g.RegisterView(0)

	if r.addr != device+0x10 || r.Size() != FrameSize {
		t.Errorf("registers at %#x, %d bytes; want %#x, %d bytes", r.addr, r.Size(), device+0x10, FrameSize)
	}

	v, dev := g.DMA()

	if dev == 0 || v.Addr() != dev || v.Size() != 2*FrameSize || !zero(v.Bytes()) {
		t.Errorf("DMA view at %#x of %d bytes for the device at %#x", v.Addr(), v.Size(), dev)
	}

	v.Store32(0, 1)

	if v.Load32(0) != 1 {
		t.Error("the DMA view does not keep a word")
	}

	if err := g.Release(); err != nil {
		t.Fatal(err)
	}

	empty, err := tm.Give(tm.NewOwner("no DMA", 0), DeviceMemory{Name: "no DMA", Registers: []Range{{Start: device + 4*FrameSize, End: device + 5*FrameSize}}})

	if err != nil {
		t.Fatal(err)
	}

	if v, dev := empty.DMA(); v.Size() != 0 || dev != 0 {
		t.Errorf("device without DMA: view of %d bytes, device address %#x", v.Size(), dev)
	}
}

func TestRegionView(t *testing.T) {
	tm := newTestManager(t)
	defer runtime.KeepAlive(tm.bufs)

	r, err := tm.Alloc(tm.NewOwner("region", 0), 100)

	if err != nil {
		t.Fatal(err)
	}

	v := r.View()

	if v.Size() != FrameSize || !zero(v.Bytes()) {
		t.Errorf("region view of %d bytes", v.Size())
	}

	if err := r.Release(); err != nil {
		t.Fatal(err)
	}
}

// FuzzViewWords checks that a word access either stays inside the view
// and is aligned, or panics, for every offset and every view.
func FuzzViewWords(f *testing.F) {
	f.Add(uint16(64), uint16(0), uint16(64), int64(0))
	f.Add(uint16(64), uint16(4), uint16(8), int64(4))
	f.Add(uint16(16), uint16(3), uint16(9), int64(-1))

	heap := HeapView(1 << 16)

	f.Fuzz(func(t *testing.T, size, start, n uint16, off int64) {
		if int(start)+int(n) > int(size) {
			return
		}

		v := heap.Slice(0, int(size)).Slice(int(start), int(n))

		for _, width := range []int64{4, 8} {
			inside := off >= 0 && off+width <= int64(n) && (v.Addr()+uint64(off))%uint64(width) == 0

			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()

				if width == 4 {
					v.Store32(int(off), 0x01020304)
					_ = v.Load32(int(off))
				} else {
					v.Store64(int(off), 0x0102030405060708)
					_ = v.Load64(int(off))
				}

				return false
			}()

			if panicked == inside {
				t.Fatalf("view of %d bytes at offset %d: %d-byte word at %d, inside %v, panic %v", n, start, width, off, inside, panicked)
			}
		}
	})
}

var sink uint32

func BenchmarkLoad32(b *testing.B) {
	words := make([]uint32, 1024)
	v := HeapView(4096)

	b.Run("atomic", func(b *testing.B) {
		var x uint32

		for i := range b.N {
			x += atomic.LoadUint32(&words[i&1023])
		}

		sink = x
	})

	b.Run("view", func(b *testing.B) {
		var x uint32

		for i := range b.N {
			x += v.Load32((i & 1023) * 4)
		}

		sink = x
	})
}

func TestViewOf(t *testing.T) {
	b := make([]byte, 16, 32)
	v := ViewOf(b)

	if v.Size() != 16 || cap(v.Bytes()) != 16 || &v.Bytes()[0] != &b[0] {
		t.Errorf("view of %d bytes, capacity %d", v.Size(), cap(v.Bytes()))
	}

	if ViewOf(nil).Size() != 0 {
		t.Error("view of nil is not empty")
	}
}
