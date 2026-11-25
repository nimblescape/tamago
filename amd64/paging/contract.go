// AMD64 page tables
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package paging

import "github.com/usbarmory/tamago/kernel/memory"

// AddressSpace is a Space in the form of the general contract of the memory
// manager (see kernel/memory.AddressSpace): the address space of the
// kernel, or the nested address space of a guest, which has the same
// format. It turns the attributes of the contract into the bits of an
// entry. Flush runs the flush of its creator: a TLB shootdown for the
// address space of the kernel, a flush of the ASID for a guest.
//
// Every processor that uses the Space must have EFER.NXE set: without it,
// the NX bit of an entry is reserved, and an access to the page faults.
//
// Like Space, it never allocates from the Go heap and never writes a
// pointer outside the stack, so the memory provider of the runtime may
// call it.
type AddressSpace struct {
	space *Space
	flush func()
}

// NewAddressSpace returns s in the form of the contract, with flush as its
// Flush.
func NewAddressSpace(s *Space, flush func()) *AddressSpace {
	if s == nil || flush == nil {
		panic("paging: invalid address space")
	}

	return &AddressSpace{space: s, flush: flush}
}

// Space returns the page tables of the address space.
func (a *AddressSpace) Space() *Space {
	return a.space
}

// Map maps size bytes from virt on to the memory from phys on with the
// attributes attr (see Space.Map).
func (a *AddressSpace) Map(virt, phys, size uint64, attr memory.Attr) error {
	f, err := FlagsOf(attr)

	if err != nil {
		return err
	}

	return a.space.Map(virt, phys, size, f)
}

// Unmap makes every page of the range unreachable and keeps its address
// for Reclaim (see Space.Unmap).
func (a *AddressSpace) Unmap(virt, size uint64) error {
	return a.space.Unmap(virt, size)
}

// Flush returns when no processor can reach a page that an earlier Unmap
// made unreachable.
func (a *AddressSpace) Flush() {
	a.flush()
}

// Reclaim removes the pages of the range that Unmap made unreachable and
// reports each of them to free (see Space.Reclaim).
func (a *AddressSpace) Reclaim(virt, size uint64, free func(phys, size uint64)) error {
	return a.space.Reclaim(virt, size, free)
}

// Lookup returns the physical address and the attributes of the page at
// virt. It reports false when the page is not mapped, also while it waits
// for Reclaim.
func (a *AddressSpace) Lookup(virt uint64) (phys uint64, attr memory.Attr, ok bool) {
	phys, f, _, ok := a.space.Lookup(virt)

	if !ok {
		return 0, 0, false
	}

	return phys, AttrOf(f), true
}

// FlagsOf returns the bits of an entry for the attributes attr. It refuses
// attributes without memory.Read, and unknown attributes, with ErrFlags.
func FlagsOf(attr memory.Attr) (Flags, error) {
	const known = memory.Read | memory.Write | memory.Execute | memory.Device

	if attr&memory.Read == 0 || attr&^known != 0 {
		return 0, ErrFlags
	}

	f := Present

	if attr&memory.Write != 0 {
		f |= Writable
	}

	if attr&memory.Execute == 0 {
		f |= NoExec
	}

	if attr&memory.Device != 0 {
		f |= WriteThrough | CacheDisable
	}

	return f, nil
}

// AttrOf returns the attributes of the bits f of a present entry.
func AttrOf(f Flags) memory.Attr {
	attr := memory.Read

	if f&Writable != 0 {
		attr |= memory.Write
	}

	if f&NoExec == 0 {
		attr |= memory.Execute
	}

	if f&CacheDisable != 0 {
		attr |= memory.Device
	}

	return attr
}
