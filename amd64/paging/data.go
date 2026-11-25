// AMD64 page tables
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package paging

// Data maps ordinary data into a Space: each page writable and not
// executable (see [Memory]). It has the methods with which a memory provider
// maps the pages that it backs (see kernel/memory.Mapper): Map, Unmap,
// Reclaim and Mapped. Every processor that uses the Space must have
// EFER.NXE set: without it, the NX bit of an entry is reserved, and an access
// to the page faults.
type Data struct {
	*Space
}

// Map maps size bytes from virt on to the memory from phys on, with the
// attributes [Memory].
func (d Data) Map(virt, phys, size uint64) error {
	return d.Space.Map(virt, phys, size, Memory)
}

// Mapped reports whether the page at virt is mapped. A page that Unmap
// unmapped and that waits for Reclaim is not mapped.
func (d Data) Mapped(virt uint64) bool {
	_, _, _, ok := d.Lookup(virt)
	return ok
}
