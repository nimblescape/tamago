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
	"unsafe"
)

// hostAlign is the alignment of the memory of hostMemory, enough for the
// chunks of tables and the DMA buffers of the tests.
const hostAlign = 16 * FrameSize

// hostMemory returns a range of frames frames of the memory of the test,
// filled with ones, and that memory, which the caller keeps alive.
func hostMemory(frames uint64) (Range, []byte) {
	buf := make([]byte, frames*FrameSize+hostAlign)

	for i := range buf {
		buf[i] = 0xff
	}

	start := alignUp(uint64(uintptr(unsafe.Pointer(&buf[0]))), hostAlign)

	return Range{Start: start, End: start + frames*FrameSize}, buf
}

// zoneOf returns the zone of the range r.
func zoneOf(r Range) Zone {
	if r.End <= lowEnd {
		return Low
	}

	return High
}

// hostTables returns table memory on frames frames of the memory of the
// test, in chunks of one frame, and that memory, which the caller keeps
// alive.
func hostTables(t *testing.T, frames uint64) (*Tables, *Owner, Range, []byte) {
	t.Helper()

	r, buf := hostMemory(frames)
	o := NewOwner("page tables", 0)

	return NewTables(newFrames(t, r), o, zoneOf(r), 1), o, r, buf
}

func TestTables(t *testing.T) {
	tables, o, r, buf := hostTables(t, 4)
	defer runtime.KeepAlive(buf)

	seen := map[uint64]bool{}

	for range 4 {
		addr, err := tables.Alloc()

		switch {
		case err != nil:
			t.Fatal(err)
		case addr%FrameSize != 0 || addr < r.Start || addr >= r.End:
			t.Fatalf("table at %#x outside the frames %#x-%#x", addr, r.Start, r.End)
		case seen[addr]:
			t.Fatalf("table at %#x given twice", addr)
		}

		seen[addr] = true

		for i, e := range tables.Table(addr) {
			if e != 0 {
				t.Fatalf("entry %d of a new table is %#x", i, e)
			}
		}

		// a used table holds entries
		tables.Table(addr)[addr/FrameSize%TableEntries] = 1
	}

	if _, err := tables.Alloc(); err != ErrNoMemory {
		t.Fatalf("Alloc without free frames: %v", err)
	}

	// A freed table holds only the link of the free tables, in entry 0.
	for addr := range seen {
		tables.Free(addr)

		for i, e := range tables.Table(addr)[1:] {
			if e != 0 {
				t.Fatalf("entry %d of a freed table is %#x", i+1, e)
			}
		}
	}

	// The frames of the tables stay with their owner.
	if o.Bytes() != 4*FrameSize || tables.Used() != 0 {
		t.Fatalf("after the frees: owner holds %d bytes, %d tables used", o.Bytes(), tables.Used())
	}

	for range 4 {
		addr, err := tables.Alloc()

		if err != nil || !seen[addr] {
			t.Fatalf("Alloc after the frees: got %#x, %v; want a freed table", addr, err)
		}

		for i, e := range tables.Table(addr) {
			if e != 0 {
				t.Fatalf("entry %d of a reused table is %#x", i, e)
			}
		}
	}
}

func TestTablesNoAllocation(t *testing.T) {
	tables, _, _, buf := hostTables(t, 1)
	defer runtime.KeepAlive(buf)

	allocs := testing.AllocsPerRun(100, func() {
		addr, err := tables.Alloc()

		if err != nil {
			panic(err)
		}

		tables.Free(addr)
	})

	if allocs != 0 {
		t.Fatalf("%v allocations for one table", allocs)
	}
}

func TestNewTablesRefuses(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTables without frames did not panic")
		}
	}()

	NewTables(nil, NewOwner("page tables", 0), Low, 1)
}

func TestNewTablesRefusesChunk(t *testing.T) {
	mustPanic(t, "chunk of 3 frames", func() { NewTables(newFrames(t), NewOwner("page tables", 0), Low, 3) })
}

func TestTablesChunks(t *testing.T) {
	r, buf := hostMemory(8)
	defer runtime.KeepAlive(buf)

	o := NewOwner("page tables", 0)
	tables := NewTables(newFrames(t, r), o, zoneOf(r), 4)

	// The first chunk, taken before Reach, is recorded.
	first, err := tables.Alloc()

	if err != nil {
		t.Fatal(err)
	}

	if o.Bytes() != 4*FrameSize {
		t.Fatalf("owner holds %d bytes after one table, want a chunk of 4 frames", o.Bytes())
	}

	var reached []Range

	if err := tables.Reach(func(phys, size uint64) error {
		reached = append(reached, Range{phys, phys + size})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if len(reached) != 1 || reached[0].Start != first&^(4*FrameSize-1) || reached[0].End-reached[0].Start != 4*FrameSize {
		t.Fatalf("Reach reached %v, want the chunk of %#x", reached, first)
	}

	// Three more tables come from the first chunk, the fifth from a
	// second chunk, which Tables reaches before it gives it out.
	for range 4 {
		addr, err := tables.Alloc()

		if err != nil {
			t.Fatal(err)
		}

		for i, e := range tables.Table(addr) {
			if e != 0 {
				t.Fatalf("entry %d of a new table is %#x", i, e)
			}
		}
	}

	if len(reached) != 2 || o.Bytes() != 8*FrameSize || tables.Used() != 5 {
		t.Fatalf("after 5 tables: reached %v, owner %d bytes, %d used", reached, o.Bytes(), tables.Used())
	}

	if _, err := tables.Alloc(); err != nil {
		t.Fatal(err)
	}

	// A freed table serves again, and then the frames run out.
	tables.Free(first)

	if again, err := tables.Alloc(); err != nil || again != first {
		t.Fatalf("Alloc after Free: got %#x, %v; want %#x", again, err, first)
	}

	for range 2 {
		if _, err := tables.Alloc(); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := tables.Alloc(); err != ErrNoMemory {
		t.Fatalf("Alloc without free frames: %v", err)
	}
}

func TestTablesReachFails(t *testing.T) {
	r, buf := hostMemory(4)
	defer runtime.KeepAlive(buf)

	o := NewOwner("page tables", 0)
	tables := NewTables(newFrames(t, r), o, zoneOf(r), 2)
	errReach := errors.New("test: no reach")

	if err := tables.Reach(func(phys, size uint64) error { return errReach }); err != nil {
		t.Fatal(err)
	}

	if _, err := tables.Alloc(); err != errReach {
		t.Fatalf("Alloc: got %v, want the error of reach", err)
	}

	if o.Bytes() != 0 {
		t.Fatalf("owner holds %d bytes after a failed reach", o.Bytes())
	}
}

func TestTablesTooManyEarly(t *testing.T) {
	r, buf := hostMemory(maxEarly + 1)
	defer runtime.KeepAlive(buf)

	tables := NewTables(newFrames(t, r), NewOwner("page tables", 0), zoneOf(r), 1)

	for range maxEarly + 1 {
		if _, err := tables.Alloc(); err != nil {
			t.Fatal(err)
		}
	}

	if err := tables.Reach(func(phys, size uint64) error { return nil }); err == nil {
		t.Fatal("Reach after too many early chunks: no error")
	}
}
