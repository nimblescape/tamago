// AMD64 page tables
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package paging

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

var errFull = errors.New("test: no table memory")

// memory gives tables from a map, and fails after limit tables when limit
// is not 0.
type memory struct {
	mu     sync.Mutex
	tables map[uint64]*[entries]uint64
	next   uint64
	limit  int
	allocs int
	frees  int
}

func newMemory() *memory {
	return &memory{tables: map[uint64]*[entries]uint64{}, next: 0x10_0000}
}

func (m *memory) Alloc() (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.limit != 0 && m.allocs-m.frees >= m.limit {
		return 0, errFull
	}

	addr := m.next
	m.next += TableSize
	m.tables[addr] = new([entries]uint64)
	m.allocs++

	return addr, nil
}

func (m *memory) Free(addr uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.tables, addr)
	m.frees++
}

func (m *memory) Table(addr uint64) *[entries]uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tables[addr]

	if !ok {
		panic("test: no table at this address")
	}

	return t
}

func newSpace(t testing.TB, giant bool) (*Space, *memory) {
	t.Helper()

	m := newMemory()
	s, err := New(m, giant)

	if err != nil {
		t.Fatal(err)
	}

	return s, m
}

// expect checks what virt maps to.
func expect(t *testing.T, s *Space, virt, phys uint64, f Flags, size uint64) {
	t.Helper()

	p, gf, gsize, ok := s.Lookup(virt)

	if !ok || p != phys || gf != f || gsize != size {
		t.Errorf("%#x: got %#x, %#x, page %#x, %v; want %#x, %#x, page %#x", virt, p, uint64(gf), gsize, ok, phys, uint64(f), size)
	}
}

// expectNone checks that virt is not mapped.
func expectNone(t *testing.T, s *Space, virt uint64) {
	t.Helper()

	if p, _, _, ok := s.Lookup(virt); ok {
		t.Errorf("%#x: mapped to %#x, want nothing", virt, p)
	}
}

func TestMapPageSizes(t *testing.T) {
	s, _ := newSpace(t, true)

	// 1 GiB pages where virt and phys are aligned to 1 GiB, 2 MiB and
	// 4 KiB pages at the ends.
	virt := uint64(512*gib - 4*kib)
	phys := uint64(64*gib - 4*kib)

	if err := s.Map(virt, phys, gib+2*mib+8*kib, Memory); err != nil {
		t.Fatal(err)
	}

	expect(t, s, virt, phys, Memory, 4*kib)
	expect(t, s, 512*gib+123, 64*gib+123, Memory, gib)
	expect(t, s, 513*gib+mib, 65*gib+mib, Memory, 2*mib)
	expect(t, s, 513*gib+2*mib, 65*gib+2*mib, Memory, 4*kib)
	expectNone(t, s, 513*gib+2*mib+4*kib)
	expectNone(t, s, virt-4*kib)

	// Without pages of 1 GiB, and with phys not aligned like virt.
	s2, _ := newSpace(t, false)

	if err := s2.Map(gib, 4*kib, 4*mib, Device); err != nil {
		t.Fatal(err)
	}

	expect(t, s2, gib+3*mib, 3*mib+4*kib, Device, 4*kib)
}

func TestMapRefuses(t *testing.T) {
	s, _ := newSpace(t, true)

	if err := s.Map(gib, 0x8000_0000, 2*mib, Memory); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name             string
		virt, phys, size uint64
		f                Flags
		want             error
	}{
		{"empty", 0, 0, 0, Memory, ErrRange},
		{"not whole pages", 4*kib + 1, 0, 4 * kib, Memory, ErrRange},
		{"beyond MaxAddress", MaxAddress - 4*kib, 0, 8 * kib, Memory, ErrRange},
		{"physical not aligned", 0x1000, 0x1001, 4 * kib, Memory, ErrAlign},
		{"no Present", 0x1000, 0, 4 * kib, Writable, ErrFlags},
		{"unknown flag", 0x1000, 0, 4 * kib, Memory | 1<<9, ErrFlags},
		{"overlap", gib + 2*mib - 4*kib, 0, 8 * kib, Memory, ErrMapped},
	} {
		if err := s.Map(c.virt, c.phys, c.size, c.f); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func TestMapRollback(t *testing.T) {
	s, m := newSpace(t, false)
	m.limit = 3 // the root, a PDPT and a PD: no page table for 4 KiB pages

	err := s.Map(gib, 0, 2*mib+4*kib, Memory)

	if !errors.Is(err, errFull) {
		t.Fatalf("got %v, want the error of the table memory", err)
	}

	expectNone(t, s, gib)
	m.limit = 0

	if err := s.Map(gib, 0, 2*mib+4*kib, Memory); err != nil {
		t.Errorf("after the rollback: %v", err)
	}
}

func TestUnmapReclaim(t *testing.T) {
	s, _ := newSpace(t, false)

	if err := s.Map(gib, 8*gib, 2*mib+8*kib, Memory); err != nil {
		t.Fatal(err)
	}

	if err := s.Reclaim(gib, 4*kib, func(_, _ uint64) {}); !errors.Is(err, ErrNotUnmapped) {
		t.Errorf("Reclaim before Unmap: got %v", err)
	}

	// Unmap one 4 KiB page inside the 2 MiB page: it splits.
	if err := s.Unmap(gib+mib, 4*kib); err != nil {
		t.Fatal(err)
	}

	expectNone(t, s, gib+mib)
	expect(t, s, gib+mib+4*kib, 8*gib+mib+4*kib, Memory, 4*kib)
	expect(t, s, gib, 8*gib, Memory, 4*kib)

	if err := s.Map(gib+mib, 0, 4*kib, Memory); !errors.Is(err, ErrMapped) {
		t.Errorf("Map of a page that waits for Reclaim: got %v", err)
	}

	if err := s.Unmap(gib+mib, 4*kib); !errors.Is(err, ErrNotMapped) {
		t.Errorf("second Unmap: got %v", err)
	}

	var freed []uint64

	if err := s.Reclaim(gib+mib, 4*kib, func(phys, size uint64) {
		freed = append(freed, phys, size)
	}); err != nil {
		t.Fatal(err)
	}

	if len(freed) != 2 || freed[0] != 8*gib+mib || freed[1] != 4*kib {
		t.Errorf("freed %#x, want one page at %#x", freed, uint64(8*gib+mib))
	}

	if err := s.Map(gib+mib, 3*gib, 4*kib, Memory); err != nil {
		t.Errorf("Map after Reclaim: %v", err)
	}

	expect(t, s, gib+mib, 3*gib, Memory, 4*kib)
}

func TestReclaimLargePage(t *testing.T) {
	s, _ := newSpace(t, false)

	if err := s.Map(2*gib, 2*gib, 4*mib, Memory); err != nil {
		t.Fatal(err)
	}

	if err := s.Unmap(2*gib, 4*mib); err != nil {
		t.Fatal(err)
	}

	if err := s.Reclaim(2*gib, 2*mib-4*kib, func(_, _ uint64) {}); !errors.Is(err, ErrNotUnmapped) {
		t.Errorf("Reclaim of a part of a large page: got %v", err)
	}

	var sizes []uint64

	if err := s.Reclaim(2*gib, 4*mib, func(_, size uint64) { sizes = append(sizes, size) }); err != nil {
		t.Fatal(err)
	}

	if len(sizes) != 2 || sizes[0] != 2*mib || sizes[1] != 2*mib {
		t.Errorf("reclaimed pages %v, want two of 2 MiB", sizes)
	}
}

func TestProtect(t *testing.T) {
	s, _ := newSpace(t, false)

	if err := s.Map(gib, gib, 2*mib, Memory); err != nil {
		t.Fatal(err)
	}

	readOnly := Present | NoExec

	if err := s.Protect(gib+8*kib, 4*kib, readOnly); err != nil {
		t.Fatal(err)
	}

	expect(t, s, gib+8*kib, gib+8*kib, readOnly, 4*kib)
	expect(t, s, gib+12*kib, gib+12*kib, Memory, 4*kib)

	if err := s.Protect(gib+2*mib, 4*kib, readOnly); !errors.Is(err, ErrNotMapped) {
		t.Errorf("Protect of an unmapped page: got %v", err)
	}
}

func TestConcurrentMap(t *testing.T) {
	const workers, pages = 8, 256

	s, m := newSpace(t, false)

	var wg sync.WaitGroup

	for w := range uint64(workers) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// All workers map pages that share their tables.
			for i := range uint64(pages) {
				virt := gib + (i*workers+w)*PageSize

				if err := s.Map(virt, 4*gib+virt, PageSize, Memory); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}

	wg.Wait()

	for i := uint64(0); i < workers*pages; i++ {
		virt := gib + i*PageSize

		if p, _, _, ok := s.Lookup(virt); !ok || p != 4*gib+virt {
			t.Fatalf("%#x: got %#x, %v", virt, p, ok)
		}
	}

	// root, PDPT, PD and four page tables, plus the tables of the losers
	// that went back.
	if live := m.allocs - m.frees; live != 7 {
		t.Errorf("%d tables in use, want 7", live)
	}
}

func TestChangeAllOrNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(s *Space) error
		after  func(t *testing.T, s *Space)
	}{
		{"Unmap", func(s *Space) error { return s.Unmap(gib+mib, 2*mib) }, func(t *testing.T, s *Space) {
			expectNone(t, s, gib+mib)
			expectNone(t, s, gib+3*mib-4*kib)
		}},
		{"Protect", func(s *Space) error { return s.Protect(gib+mib, 2*mib, Present) }, func(t *testing.T, s *Space) {
			expect(t, s, gib+mib, 2*gib+mib, Present, 4*kib)
			expect(t, s, gib+3*mib-4*kib, 2*gib+3*mib-4*kib, Present, 4*kib)
		}},
	} {
		s, m := newSpace(t, false)

		// Two pages of 2 MiB; the range splits both, but the memory
		// holds one table more only.
		if err := s.Map(gib, 2*gib, 4*mib, Memory); err != nil {
			t.Fatal(err)
		}

		m.limit = m.allocs - m.frees + 1

		if err := c.change(s); !errors.Is(err, errFull) {
			t.Fatalf("%s: got %v, want the error of the table memory", c.name, err)
		}

		expect(t, s, gib+mib, 2*gib+mib, Memory, 4*kib)
		expect(t, s, gib+3*mib-4*kib, 2*gib+3*mib-4*kib, Memory, 2*mib)
		m.limit = 0

		if err := c.change(s); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		c.after(t, s)
		expect(t, s, gib, 2*gib, Memory, 4*kib)
		expect(t, s, gib+3*mib, 2*gib+3*mib, Memory, 4*kib)
	}
}

func TestTableRefusesPage(t *testing.T) {
	s, m := newSpace(t, false)

	if err := s.Map(gib, 0, 2*mib, Memory); err != nil {
		t.Fatal(err)
	}

	pdpt := m.Table(m.Table(s.Root())[0] & addrMask)
	pd := m.Table(pdpt[1] & addrMask)

	if _, err := s.table(2, &pd[0]); !errors.Is(err, ErrMapped) {
		t.Errorf("table of a page entry: got %v, want %v", err, ErrMapped)
	}
}

func TestUndoKeepsOtherPages(t *testing.T) {
	s, _ := newSpace(t, false)

	if err := s.Map(gib+4*kib, 5*gib, 4*kib, Memory); err != nil {
		t.Fatal(err)
	}

	// A Map that skipped the check of used, as one that races with the
	// owner of the page would, fails on that page and removes only its
	// own.
	off := uint64(7*gib) - gib

	if err := s.mapRange(0, s.root, 0, gib, gib+8*kib, off, uint64(Memory)); !errors.Is(err, ErrMapped) {
		t.Fatalf("got %v, want %v", err, ErrMapped)
	}

	s.undo(0, s.root, 0, gib, gib+8*kib, off, uint64(Memory))

	expectNone(t, s, gib)
	expect(t, s, gib+4*kib, 5*gib, Memory, 4*kib)
}
