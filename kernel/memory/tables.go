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
	"sync/atomic"
	"unsafe"
)

// TableEntries is the number of entries of a page table that fills one frame
// with entries of 8 bytes, as the page tables of amd64, of arm64 with pages
// of 4 KiB and of riscv64 do.
const TableEntries = FrameSize / 8

// maxEarly is the number of chunks that Tables records before Reach, so that
// Reach can make them reachable.
const maxEarly = 8

// Tables gives page tables the frames of one owner from one zone. It takes
// the frames in chunks of contiguous frames and gives out one table at a
// time. The kernel reaches each table at its physical address: a kernel
// without a map of all memory makes each chunk reachable 1:1 with Reach.
// It is the table memory of the page tables of an architecture (for
// example amd64/paging.TableMemory).
//
// Like Frames, it never allocates from the Go heap and never blocks, so the
// memory provider of the runtime may call it with locks of the runtime
// held.
type Tables struct {
	frames *Frames
	owner  *Owner
	zone   Zone
	chunk  uint64 // frames per chunk
	reach  atomic.Pointer[func(phys, size uint64) error]

	lock      spinLock
	free      uint64 // the first free table, linked through entry 0, or 0
	next, end uint64 // the part of the newest chunk that was never used
	used      uint64 // tables in use
	early     [maxEarly]uint64
	nEarly    int
	lost      bool // a chunk was taken before Reach and not recorded
}

// NewTables returns table memory from the frames f of zone z, held by o,
// which takes chunk contiguous frames at once, aligned to their size.
// chunk is a power of two.
func NewTables(f *Frames, o *Owner, z Zone, chunk uint64) *Tables {
	if f == nil || o == nil || chunk == 0 || chunk&(chunk-1) != 0 {
		panic("memory: invalid table memory")
	}

	return &Tables{frames: f, owner: o, zone: z, chunk: chunk}
}

// Reach makes the tables reachable: Tables calls reach for every chunk
// that it takes from now on, before it gives out a table of it, and Reach
// calls it for the chunks taken before. reach maps a chunk 1:1 and must not
// need a table itself. The kernel calls Reach before it uses the address
// space into which reach maps.
func (t *Tables) Reach(reach func(phys, size uint64) error) error {
	t.lock.lock()
	early, n, lost := t.early, t.nEarly, t.lost
	t.reach.Store(&reach)
	t.lock.unlock()

	if lost {
		return errors.New("memory: more chunks of tables than recorded before Reach")
	}

	for _, c := range early[:n] {
		if err := reach(c, t.chunk*FrameSize); err != nil {
			return err
		}
	}

	return nil
}

// Alloc returns the address of a new table filled with zeros.
func (t *Tables) Alloc() (uint64, error) {
	for {
		t.lock.lock()
		addr, ok := t.takeLocked()
		t.lock.unlock()

		if ok {
			// A free table is zero, except for the link.
			t.Table(addr)[0] = 0
			return addr, nil
		}

		if err := t.grow(); err != nil {
			return 0, err
		}
	}
}

// takeLocked takes a free table or a table of the newest chunk that was
// never used.
//
//go:nosplit
func (t *Tables) takeLocked() (addr uint64, ok bool) {
	switch {
	case t.free != 0:
		addr = t.free
		t.free = t.Table(addr)[0]
	case t.next < t.end:
		addr = t.next
		t.next += FrameSize
	default:
		return 0, false
	}

	t.used++

	return addr, true
}

// grow takes a new chunk, makes it reachable and clears it outside the
// lock, so that reach may run code with preemption points. Of two cores
// that grow at once, both chunks stay: the second goes to the free list.
func (t *Tables) grow() error {
	size := t.chunk * FrameSize
	chunk, err := t.frames.AllocRun(t.owner, t.zone, t.chunk, size)

	if err != nil {
		return err
	}

	for reached, cleared := false, false; ; {
		if reach := t.reach.Load(); reach != nil && !reached {
			if err := (*reach)(chunk, size); err != nil {
				t.frames.Free(t.owner, chunk, t.chunk)
				return err
			}

			reached = true
		}

		// Before Reach, the kernel reaches all memory.
		if !cleared {
			for addr := chunk; addr < chunk+size; addr += FrameSize {
				clear(t.Table(addr)[:])
			}

			cleared = true
		}

		t.lock.lock()
		added := t.addLocked(chunk, size, reached)
		t.lock.unlock()

		if added {
			return nil
		}
	}
}

// addLocked adds the chunk of size bytes at chunk to the tables. A chunk
// that is not reached is recorded for Reach. It reports false, and adds
// nothing, when Reach came in between: the caller must reach the chunk
// first.
//
//go:nosplit
func (t *Tables) addLocked(chunk, size uint64, reached bool) bool {
	if !reached {
		switch {
		case t.reach.Load() != nil:
			return false
		case t.nEarly < maxEarly:
			t.early[t.nEarly] = chunk
			t.nEarly++
		default:
			t.lost = true
		}
	}

	if t.next == t.end {
		t.next, t.end = chunk, chunk+size
		return true
	}

	for addr := chunk; addr < chunk+size; addr += FrameSize {
		t.Table(addr)[0] = t.free
		t.free = addr
	}

	return true
}

// Free takes back a table that Alloc gave and that no entry points to. It
// clears the table and keeps it for the next Alloc: the frames of tables
// stay with their owner.
func (t *Tables) Free(addr uint64) {
	clear(t.Table(addr)[:])

	t.lock.lock()
	t.freeLocked(addr)
	t.lock.unlock()
}

//go:nosplit
func (t *Tables) freeLocked(addr uint64) {
	t.Table(addr)[0] = t.free
	t.free = addr
	t.used--
}

// Used returns the number of tables in use.
func (t *Tables) Used() uint64 {
	t.lock.lock()
	defer t.lock.unlock()

	return t.used
}

// Table returns the entries of the table at addr. The address is a physical
// address, which the kernel maps 1:1, not a pointer that the Go runtime
// gave, so the pointer checks of the race detector do not apply.
//
//go:nocheckptr
//go:nosplit
func (t *Tables) Table(addr uint64) *[TableEntries]uint64 {
	return (*[TableEntries]uint64)(unsafe.Pointer(uintptr(addr)))
}
