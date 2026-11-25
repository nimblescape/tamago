// AMD64 page tables
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package paging

import (
	"testing"

	"github.com/usbarmory/tamago/kernel/memory"
)

// poolBase is the address of the first table of a pool.
const poolBase = 0x10_0000

// pool gives tables from a fixed array, without an allocation.
type pool struct {
	tables [16][entries]uint64
	next   int
}

func (p *pool) Alloc() (uint64, error) {
	if p.next == len(p.tables) {
		return 0, errFull
	}

	p.next++

	return poolBase + uint64(p.next-1)*TableSize, nil
}

func (p *pool) Free(addr uint64) {}

func (p *pool) Table(addr uint64) *[entries]uint64 {
	return &p.tables[(addr-poolBase)/TableSize]
}

func TestAddressSpace(t *testing.T) {
	s, _ := newSpace(t, true)
	flushes := 0
	a := NewAddressSpace(s, func() { flushes++ })
	virt := uint64(512 * gib)

	if _, _, ok := a.Lookup(virt); ok {
		t.Fatal("page mapped before Map")
	}

	if err := a.Map(virt, 4*gib, 2*PageSize, memory.Data); err != nil {
		t.Fatal(err)
	}

	expect(t, s, virt+PageSize, 4*gib+PageSize, Memory, PageSize)

	if phys, attr, ok := a.Lookup(virt + PageSize + 8); !ok || phys != 4*gib+PageSize+8 || attr != memory.Data {
		t.Fatalf("Lookup: got %#x, %#x, %v", phys, attr, ok)
	}

	if _, _, ok := a.Lookup(virt + 2*PageSize); ok {
		t.Fatal("Lookup shows a page after the range")
	}

	if err := a.Unmap(virt, 2*PageSize); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := a.Lookup(virt); ok {
		t.Fatal("page mapped after Unmap")
	}

	a.Flush()

	if flushes != 1 {
		t.Fatalf("%d flushes, want 1", flushes)
	}

	var freed uint64

	if err := a.Reclaim(virt, 2*PageSize, func(phys, size uint64) { freed += size }); err != nil {
		t.Fatal(err)
	}

	if freed != 2*PageSize {
		t.Fatalf("Reclaim freed %d bytes, want %d", freed, 2*PageSize)
	}

	if err := a.Map(virt, 4*gib, PageSize, memory.Write); err != ErrFlags {
		t.Fatalf("Map without Read: got %v, want ErrFlags", err)
	}
}

func TestAttributes(t *testing.T) {
	for _, c := range []struct {
		attr  memory.Attr
		flags Flags
	}{
		{memory.Data, Memory},
		{memory.Registers, Device},
		{memory.Read, Present | NoExec},
		{memory.Read | memory.Execute, Present},
		{memory.Read | memory.Write | memory.Execute, Present | Writable},
	} {
		f, err := FlagsOf(c.attr)

		if err != nil || f != c.flags {
			t.Errorf("FlagsOf(%#x): got %#x, %v; want %#x", c.attr, f, err, c.flags)
		}

		if attr := AttrOf(f); attr != c.attr {
			t.Errorf("AttrOf(%#x): got %#x, want %#x", f, attr, c.attr)
		}
	}

	for _, attr := range []memory.Attr{0, memory.Write, memory.Device, memory.Read | 1<<7} {
		if _, err := FlagsOf(attr); err != ErrFlags {
			t.Errorf("FlagsOf(%#x): got %v, want ErrFlags", attr, err)
		}
	}
}

// TestAddressSpaceNoAllocation checks that a memory provider, which runs
// with the locks of the Go runtime held, can map and unmap pages without
// an allocation from the Go heap.
func TestAddressSpaceNoAllocation(t *testing.T) {
	s, err := New(&pool{}, true)

	if err != nil {
		t.Fatal(err)
	}

	a := NewAddressSpace(s, func() {})
	virt := uint64(512 * gib)
	free := func(phys, size uint64) {}

	allocs := testing.AllocsPerRun(100, func() {
		if _, _, ok := a.Lookup(virt); ok {
			panic("page mapped")
		}

		if err := a.Map(virt, 4*gib, PageSize, memory.Data); err != nil {
			panic(err)
		}

		if err := a.Unmap(virt, PageSize); err != nil {
			panic(err)
		}

		a.Flush()

		if err := a.Reclaim(virt, PageSize, free); err != nil {
			panic(err)
		}
	})

	if allocs != 0 {
		t.Fatalf("%v allocations for a map and an unmap of one page", allocs)
	}
}

func TestNewAddressSpaceRefuses(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewAddressSpace without a flush did not panic")
		}
	}()

	s, _ := newSpace(t, false)
	NewAddressSpace(s, nil)
}
