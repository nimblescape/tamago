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
	"testing"
	"unsafe"
)

// hostTables returns table memory on frames frames of the memory of the
// test, filled with ones, and that memory, which the caller keeps alive.
func hostTables(t *testing.T, frames uint64) (*Tables, *Owner, Range, []byte) {
	t.Helper()

	buf := make([]byte, (frames+1)*FrameSize)

	for i := range buf {
		buf[i] = 0xff
	}

	start := alignUp(uint64(uintptr(unsafe.Pointer(&buf[0]))), FrameSize)
	r := Range{Start: start, End: start + frames*FrameSize}
	zone := High

	if r.End <= lowEnd {
		zone = Low
	}

	o := NewOwner("page tables", 0)

	return NewTables(newFrames(t, r), o, zone), o, r, buf
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

	for addr := range seen {
		tables.Free(addr)

		for i, e := range tables.Table(addr) {
			if e != 0 {
				t.Fatalf("entry %d of a freed table is %#x", i, e)
			}
		}
	}

	if o.Bytes() != 0 {
		t.Fatalf("owner holds %d bytes after the frees", o.Bytes())
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

	NewTables(nil, NewOwner("page tables", 0), Low)
}
