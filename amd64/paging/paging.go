// AMD64 page tables
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package paging builds and changes the 4-level page tables of amd64: one
// address space, which maps virtual addresses to physical memory.
//
// An address space never frees a table while it lives, so that two cores
// may change it at once without a lock: a new table enters with a
// compare-and-swap, and every other change touches only the entries of the
// range of its caller. The owners of the ranges make sure that no two
// callers change the same range at once. Even so, a page entry is never
// taken for a table, and a failed Map removes only the pages that it wrote.
// The tables of a range stay after the range is gone; they take at most
// 1/512 of the memory that the range mapped.
//
// An unmap goes in three steps, so that no frame is reused while a core may
// still reach it through its translation cache (TLB):
//
//  1. Unmap clears the present bit of each page and keeps its address.
//  2. The caller flushes the TLB of every core.
//  3. Reclaim clears the entries and reports the frames, which the caller
//     may then free. Pages that were never mapped do not disturb it.
//
// Map refuses a range in which any page is still waiting for Reclaim.
//
// AMD64 Architecture Programmer's Manual, volume 2, section 5.3 (long-mode
// page translation) and section 5.4 (page-translation-table entry fields).
//
// The package has no assembly code, so that its tests run on the
// development host. Loading CR3 and flushing the TLB belong to the
// processor driver.
package paging

import (
	"errors"
	"sync/atomic"
)

// Flags are the attributes of a mapping.
type Flags uint64

// The attributes of a mapping.
const (
	Present      Flags = 1 << 0
	Writable     Flags = 1 << 1
	WriteThrough Flags = 1 << 3
	CacheDisable Flags = 1 << 4
	NoExec       Flags = 1 << 63

	// Memory are the attributes of ordinary data: writable, not
	// executable.
	Memory = Present | Writable | NoExec

	// Device are the attributes of device memory: writable, not cached,
	// not executable.
	Device = Present | Writable | WriteThrough | CacheDisable | NoExec
)

const (
	// PageSize is the size of the smallest page.
	PageSize = 1 << 12
	// TableSize is the size of one table in bytes.
	TableSize = entries * 8
	// MaxAddress is the end of the lower half of the canonical
	// addresses, which an address space maps.
	MaxAddress = 1 << 47

	entries = 512
	// large marks an entry of a page directory or a page directory
	// pointer table as a page, not as a pointer to a table.
	large = 1 << 7
	// addrMask holds the address bits of an entry.
	addrMask = 0x000f_ffff_ffff_f000
	// flagMask holds the attribute bits that this package writes.
	flagMask = uint64(Present | Writable | WriteThrough | CacheDisable | NoExec)
	// pointer are the attributes of an entry that points to a table. The
	// entries of the pages decide the access.
	pointer = uint64(Present | Writable)
)

// spans are the sizes that one entry covers at each level, from the page
// map level 4 (index 0) to the page table (index 3).
var spans = [4]uint64{1 << 39, 1 << 30, 1 << 21, 1 << 12}

var (
	// ErrRange means that a range is empty, not made of whole pages, or
	// beyond MaxAddress.
	ErrRange = errors.New("paging: invalid range")
	// ErrFlags means that the attributes are unknown or lack Present.
	ErrFlags = errors.New("paging: invalid attributes")
	// ErrMapped means that a page of the range is mapped, or waits for
	// Reclaim.
	ErrMapped = errors.New("paging: page in use")
	// ErrNotMapped means that a page of the range is not mapped.
	ErrNotMapped = errors.New("paging: page not mapped")
	// ErrNotUnmapped means that a page of the range is still mapped, or
	// that a large page lies only in part in the range, so Reclaim
	// cannot take it.
	ErrNotUnmapped = errors.New("paging: page not unmapped")
	// ErrAlign means that the physical address does not fit the page
	// size that the range needs.
	ErrAlign = errors.New("paging: physical address not aligned")
)

// TableMemory gives the tables their memory.
type TableMemory interface {
	// Alloc returns the address of a new table of TableSize bytes,
	// aligned to TableSize and filled with zeros.
	Alloc() (uint64, error)
	// Free takes back a table that Alloc gave and that no entry points
	// to.
	Free(addr uint64)
	// Table returns the entries of the table at addr.
	Table(addr uint64) *[entries]uint64
}

// Space is one address space.
type Space struct {
	mem   TableMemory
	root  uint64
	giant bool
}

// New returns an empty address space: nothing is mapped. giant allows
// pages of 1 GiB, which the processor offers when CPUID 0x80000001 sets EDX
// bit 26.
func New(mem TableMemory, giant bool) (*Space, error) {
	root, err := mem.Alloc()

	if err != nil {
		return nil, err
	}

	return &Space{mem: mem, root: root, giant: giant}, nil
}

// Root returns the address of the page map level 4, the value for CR3.
func (s *Space) Root() uint64 {
	return s.root
}

// Map maps size bytes from virt on to the physical memory from phys on
// with the attributes f. No page of the range may be mapped or wait for
// Reclaim. It uses the largest pages that the alignment of virt and phys
// allows. When the memory for a table runs out, Map removes what it mapped
// and returns the error of TableMemory.
func (s *Space) Map(virt, phys, size uint64, f Flags) error {
	if err := checkRange(virt, size); err != nil {
		return err
	}

	if phys%PageSize != 0 || phys+size < phys {
		return ErrAlign
	}

	if uint64(f)&^flagMask != 0 || f&Present == 0 {
		return ErrFlags
	}

	if s.used(0, s.root, 0, virt, virt+size) {
		return ErrMapped
	}

	if err := s.mapRange(0, s.root, 0, virt, virt+size, phys-virt, uint64(f)); err != nil {
		s.undo(0, s.root, 0, virt, virt+size, phys-virt, uint64(f))
		return err
	}

	return nil
}

// Protect sets the attributes of the mapped range of size bytes from virt
// on to f. A large page that the range covers only in part becomes a
// table of smaller pages first. When the memory for a table runs out,
// Protect changes no attributes and returns the error of TableMemory. The
// caller flushes the TLB of every core after it, also after an error.
func (s *Space) Protect(virt, size uint64, f Flags) error {
	if err := checkRange(virt, size); err != nil {
		return err
	}

	if uint64(f)&^flagMask != 0 || f&Present == 0 {
		return ErrFlags
	}

	if !s.mapped(0, s.root, 0, virt, virt+size) {
		return ErrNotMapped
	}

	if err := s.splitEdges(virt, virt+size); err != nil {
		return err
	}

	s.change(0, s.root, 0, virt, virt+size, func(e uint64) uint64 {
		return e&^flagMask | uint64(f)
	})

	return nil
}

// Unmap clears the present bit of each page of the mapped range of size
// bytes from virt on, and keeps its address for Reclaim. A large page that
// the range covers only in part becomes a table of smaller pages first.
// When the memory for a table runs out, Unmap unmaps nothing and returns
// the error of TableMemory. The caller flushes the TLB of every core after
// it, also after an error, and then calls Reclaim.
func (s *Space) Unmap(virt, size uint64) error {
	if err := checkRange(virt, size); err != nil {
		return err
	}

	if !s.mapped(0, s.root, 0, virt, virt+size) {
		return ErrNotMapped
	}

	if err := s.splitEdges(virt, virt+size); err != nil {
		return err
	}

	s.change(0, s.root, 0, virt, virt+size, func(e uint64) uint64 {
		return e &^ uint64(Present)
	})

	return nil
}

// Reclaim clears the entries of the pages of the range of size bytes from
// virt on, which Unmap unmapped and whose TLB entries the caller flushed on
// every core, and calls free with the physical address and the size of
// each of these pages. Pages of the range that are not mapped at all are
// skipped. free must not change the address space.
func (s *Space) Reclaim(virt, size uint64, free func(phys, size uint64)) error {
	if err := checkRange(virt, size); err != nil {
		return err
	}

	if !s.unmapped(0, s.root, 0, virt, virt+size) {
		return ErrNotUnmapped
	}

	s.reclaim(0, s.root, 0, virt, virt+size, free)

	return nil
}

// Lookup returns the physical address that virt maps to, the attributes
// and the size of its page. It reports false when virt is not mapped.
func (s *Space) Lookup(virt uint64) (phys uint64, f Flags, size uint64, ok bool) {
	if virt >= MaxAddress {
		return 0, 0, 0, false
	}

	table := s.mem.Table(s.root)

	for level := range 4 {
		e := atomic.LoadUint64(&table[(virt/spans[level])%entries])

		switch {
		case e&uint64(Present) == 0:
			return 0, 0, 0, false
		case isLeaf(level, e):
			span := spans[level]
			return e&addrMask&^(span-1) + virt%span, Flags(e & flagMask), span, true
		}

		table = s.mem.Table(e & addrMask)
	}

	return 0, 0, 0, false
}

// checkRange checks that a range is not empty, is made of whole pages and
// ends at MaxAddress at most.
func checkRange(virt, size uint64) error {
	if size == 0 || virt%PageSize != 0 || size%PageSize != 0 || virt >= MaxAddress || size > MaxAddress-virt {
		return ErrRange
	}

	return nil
}

// isLeaf reports whether the entry e of the level maps a page: always in
// the page table, and in the page directory and the page directory pointer
// table when it has the large bit. A pointer to a table never loses its
// present bit, so an entry without it is a page that waits for Reclaim.
func isLeaf(level int, e uint64) bool {
	return level == 3 || (level > 0 && e&large != 0) || (e&uint64(Present) == 0 && e != 0)
}

// leafAllowed reports whether an entry of the level can map a page: in the
// page directory pointer table only with pages of 1 GiB, never in the page
// map level 4.
func (s *Space) leafAllowed(level int) bool {
	return level >= 2 || (level == 1 && s.giant)
}

// leaf returns the entry of a page at phys on the level with the
// attributes f.
func leaf(level int, phys, f uint64) uint64 {
	e := phys | f

	if level < 3 {
		e |= large
	}

	return e
}

// walk calls fn for each entry of the table at addr of the level, which
// covers the addresses from base on, that overlaps the range from start to
// end, with the part of the range that the entry covers. It stops when fn
// returns false, and reports whether it went through.
func (s *Space) walk(level int, addr, base, start, end uint64, fn func(e *uint64, eStart, lo, hi uint64) bool) bool {
	span := spans[level]
	table := s.mem.Table(addr)

	for i := (start - base) / span; i < entries; i++ {
		eStart := base + i*span

		if eStart >= end {
			break
		}

		if !fn(&table[i], eStart, max(start, eStart), min(end, eStart+span)) {
			return false
		}
	}

	return true
}

// used reports whether any page of the range from start to end is mapped
// or waits for Reclaim.
func (s *Space) used(level int, addr, base, start, end uint64) bool {
	return !s.walk(level, addr, base, start, end, func(p *uint64, eStart, lo, hi uint64) bool {
		e := atomic.LoadUint64(p)

		switch {
		case e == 0:
			return true
		case isLeaf(level, e):
			return false
		default:
			return !s.used(level+1, e&addrMask, eStart, lo, hi)
		}
	})
}

// mapped reports whether every page of the range from start to end is
// mapped.
func (s *Space) mapped(level int, addr, base, start, end uint64) bool {
	return s.walk(level, addr, base, start, end, func(p *uint64, eStart, lo, hi uint64) bool {
		e := atomic.LoadUint64(p)

		switch {
		case e&uint64(Present) == 0:
			return false
		case isLeaf(level, e):
			return true
		default:
			return s.mapped(level+1, e&addrMask, eStart, lo, hi)
		}
	})
}

// unmapped reports whether no page of the range from start to end is
// mapped, and no large page that waits for Reclaim lies only partly in the
// range.
func (s *Space) unmapped(level int, addr, base, start, end uint64) bool {
	return s.walk(level, addr, base, start, end, func(p *uint64, eStart, lo, hi uint64) bool {
		e := atomic.LoadUint64(p)

		switch {
		case e == 0:
			return true
		case isLeaf(level, e):
			return e&uint64(Present) == 0 && lo == eStart && hi == eStart+spans[level]
		default:
			return s.unmapped(level+1, e&addrMask, eStart, lo, hi)
		}
	})
}

// mapRange maps the range from start to end, which is free, to the
// physical memory at offset off from it.
//
// mapRange goes through the entries itself and does not use walk: a
// closure would write the error through a pointer, with a write barrier,
// and the Go runtime maps its memory through Map (kernel/memory) also at
// times at which it allows no write barrier.
func (s *Space) mapRange(level int, addr, base, start, end, off, f uint64) error {
	span := spans[level]
	table := s.mem.Table(addr)

	for i := (start - base) / span; i < entries && base+i*span < end; i++ {
		p := &table[i]
		eStart := base + i*span
		lo, hi := max(start, eStart), min(end, eStart+span)

		if lo == eStart && hi == eStart+span && s.leafAllowed(level) && (lo+off)%span == 0 {
			if !atomic.CompareAndSwapUint64(p, 0, leaf(level, lo+off, f)) {
				return ErrMapped
			}

			continue
		}

		sub, err := s.table(level, p)

		if err != nil {
			return err
		}

		if err := s.mapRange(level+1, sub, eStart, lo, hi, off, f); err != nil {
			return err
		}
	}

	return nil
}

// table returns the table that the entry at p of the level points to, and
// makes one when the entry is empty. Of two cores that make one at once,
// one wins and the other frees its table. An entry that maps a page is
// never taken for a table: table returns ErrMapped.
func (s *Space) table(level int, p *uint64) (uint64, error) {
	e := atomic.LoadUint64(p)

	if e == 0 {
		sub, err := s.mem.Alloc()

		if err != nil {
			return 0, err
		}

		if !atomic.CompareAndSwapUint64(p, 0, sub|pointer) {
			s.mem.Free(sub)
		}

		e = atomic.LoadUint64(p)
	}

	if isLeaf(level, e) {
		return 0, ErrMapped
	}

	return e & addrMask, nil
}

// splitEdges splits the large pages that hold start or end without
// starting there, so that every page from start to end lies whole in the
// range. The pages of a split map the same memory as before.
func (s *Space) splitEdges(start, end uint64) error {
	if err := s.splitAt(start); err != nil {
		return err
	}

	return s.splitAt(end)
}

// splitAt splits the large pages that hold addr without starting there.
// An addr at MaxAddress is a multiple of every span and needs no split.
func (s *Space) splitAt(addr uint64) error {
	table := s.root

	// The pages of the page table (level 3) are the smallest: a page
	// always starts at addr.
	for level := range 3 {
		if addr%spans[level] == 0 {
			return nil
		}

		p := &s.mem.Table(table)[(addr/spans[level])%entries]

		switch e := atomic.LoadUint64(p); {
		case e == 0:
			return nil
		case isLeaf(level, e):
			sub, err := s.split(level, p, e)

			if err != nil {
				return err
			}

			table = sub
		default:
			table = e & addrMask
		}
	}

	return nil
}

// split replaces the large page e at p of the level by a table of the next
// level, whose pages map the same memory with the same attributes, and
// returns the address of the table. When another core changed the entry
// first, split takes the table that it made, or splits the new page.
func (s *Space) split(level int, p *uint64, e uint64) (uint64, error) {
	sub, err := s.mem.Alloc()

	if err != nil {
		return 0, err
	}

	for {
		table := s.mem.Table(sub)
		next := spans[level+1]

		for j := range uint64(entries) {
			table[j] = leaf(level+1, e&addrMask+j*next, e&flagMask)
		}

		if atomic.CompareAndSwapUint64(p, e, sub|pointer) {
			return sub, nil
		}

		if e = atomic.LoadUint64(p); !isLeaf(level, e) {
			s.mem.Free(sub)
			return e & addrMask, nil
		}
	}
}

// change applies fn to the entry of each page of the range from start to
// end. Every page lies whole in the range (splitEdges). The processor may
// set the accessed and dirty bits of an entry at the same time, so the
// entry changes with a compare-and-swap.
func (s *Space) change(level int, addr, base, start, end uint64, fn func(uint64) uint64) {
	s.walk(level, addr, base, start, end, func(p *uint64, eStart, lo, hi uint64) bool {
		e := atomic.LoadUint64(p)

		if !isLeaf(level, e) {
			s.change(level+1, e&addrMask, eStart, lo, hi, fn)
			return true
		}

		for !atomic.CompareAndSwapUint64(p, e, fn(e)) {
			e = atomic.LoadUint64(p)
		}

		return true
	})
}

// undo removes the pages that mapRange wrote for the range from start to
// end at offset off with the attributes f, and no other page. Map calls it
// when it fails.
func (s *Space) undo(level int, addr, base, start, end, off, f uint64) {
	s.walk(level, addr, base, start, end, func(p *uint64, eStart, lo, hi uint64) bool {
		switch e := atomic.LoadUint64(p); {
		case e == 0:
		case isLeaf(level, e):
			if lo == eStart && hi == eStart+spans[level] {
				atomic.CompareAndSwapUint64(p, leaf(level, lo+off, f), 0)
			}
		default:
			s.undo(level+1, e&addrMask, eStart, lo, hi, off, f)
		}

		return true
	})
}

// reclaim clears the entries of the pages of the range from start to end
// and reports each page to free.
func (s *Space) reclaim(level int, addr, base, start, end uint64, free func(phys, size uint64)) {
	s.walk(level, addr, base, start, end, func(p *uint64, eStart, lo, hi uint64) bool {
		switch e := atomic.LoadUint64(p); {
		case e == 0:
		case isLeaf(level, e):
			atomic.StoreUint64(p, 0)
			free(e&addrMask&^(spans[level]-1), spans[level])
		default:
			s.reclaim(level+1, e&addrMask, eStart, lo, hi, free)
		}

		return true
	})
}
