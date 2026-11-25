// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import "unsafe"

// TableEntries is the number of entries of a page table that fills one frame
// with entries of 8 bytes, as the page tables of amd64, of arm64 with pages
// of 4 KiB and of riscv64 do.
const TableEntries = FrameSize / 8

// Tables gives page tables the frames of one owner from one zone. The kernel
// reaches each table at its physical address, so the zone must lie in
// memory that the kernel maps 1:1. It is the table memory of the page tables
// of an architecture (for example amd64/paging.TableMemory).
//
// Like [Frames], it never allocates from the Go heap and never blocks, so
// the memory provider of the runtime may call it with locks of the runtime
// held.
type Tables struct {
	frames *Frames
	owner  *Owner
	zone   Zone
}

// NewTables returns table memory from the frames f of zone z, held by o.
func NewTables(f *Frames, o *Owner, z Zone) *Tables {
	if f == nil || o == nil {
		panic("memory: table memory without frames or owner")
	}

	return &Tables{frames: f, owner: o, zone: z}
}

// Alloc returns the address of a new table filled with zeros.
func (t *Tables) Alloc() (uint64, error) {
	addr, err := t.frames.Alloc(t.owner, t.zone)

	if err != nil {
		return 0, err
	}

	// A free frame is zero, and the table is cleared once more so that
	// no stale entry can map anything.
	clear(t.Table(addr)[:])

	return addr, nil
}

// Free takes back a table that Alloc gave and that no entry points to. It
// clears the table, as every free frame is zero.
func (t *Tables) Free(addr uint64) {
	clear(t.Table(addr)[:])
	t.frames.Free(t.owner, addr, 1)
}

// Table returns the entries of the table at addr. The address is a physical
// address, which the kernel maps 1:1, not a pointer that the Go runtime
// gave, so the pointer checks of the race detector do not apply.
//
//go:nocheckptr
func (t *Tables) Table(addr uint64) *[TableEntries]uint64 {
	return (*[TableEntries]uint64)(unsafe.Pointer(uintptr(addr)))
}
