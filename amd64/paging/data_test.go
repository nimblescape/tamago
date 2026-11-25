// AMD64 page tables
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package paging

import "testing"

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

func TestData(t *testing.T) {
	s, _ := newSpace(t, true)
	d := Data{s}
	virt := uint64(512 * gib)

	if d.Mapped(virt) {
		t.Fatal("page mapped before Map")
	}

	if err := d.Map(virt, 4*gib, 2*PageSize); err != nil {
		t.Fatal(err)
	}

	expect(t, s, virt+PageSize, 4*gib+PageSize, Memory, PageSize)

	if !d.Mapped(virt) || !d.Mapped(virt+PageSize) || d.Mapped(virt+2*PageSize) {
		t.Fatal("Mapped does not show the two pages of Map")
	}

	if err := d.Unmap(virt, 2*PageSize); err != nil {
		t.Fatal(err)
	}

	if d.Mapped(virt) {
		t.Fatal("page mapped after Unmap")
	}

	var freed uint64

	if err := d.Reclaim(virt, 2*PageSize, func(phys, size uint64) { freed += size }); err != nil {
		t.Fatal(err)
	}

	if freed != 2*PageSize {
		t.Fatalf("Reclaim freed %d bytes, want %d", freed, 2*PageSize)
	}
}

// TestDataNoAllocation checks that a memory provider, which runs with the
// locks of the Go runtime held, can map and unmap pages without an
// allocation from the Go heap.
func TestDataNoAllocation(t *testing.T) {
	s, err := New(&pool{}, true)

	if err != nil {
		t.Fatal(err)
	}

	d := Data{s}
	virt := uint64(512 * gib)
	free := func(phys, size uint64) {}

	allocs := testing.AllocsPerRun(100, func() {
		if d.Mapped(virt) {
			panic("page mapped")
		}

		if err := d.Map(virt, 4*gib, PageSize); err != nil {
			panic(err)
		}

		if err := d.Unmap(virt, PageSize); err != nil {
			panic(err)
		}

		if err := d.Reclaim(virt, PageSize, free); err != nil {
			panic(err)
		}
	})

	if allocs != 0 {
		t.Fatalf("%v allocations for a map and an unmap of one page", allocs)
	}
}
