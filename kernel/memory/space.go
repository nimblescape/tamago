// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

// Attr are the access rights and the memory type of a mapping. They are the
// same for every architecture and every kind of address space: each
// implementation of AddressSpace turns them into the bits of its table
// format.
type Attr uint8

const (
	// Read allows reads.
	Read Attr = 1 << iota
	// Write allows writes.
	Write
	// Execute allows the processor to run code from the memory. An
	// address space of a device never allows it.
	Execute
	// Device marks the registers of a device: uncached, and every access
	// reaches the device in program order.
	Device

	// Data are the attributes of ordinary data: read and write, cached,
	// not executable.
	Data = Read | Write
	// Registers are the attributes of the registers of a device: read
	// and write, uncached, not executable.
	Registers = Read | Write | Device
)

// AddressSpace maps physical memory into one address space. A kernel has
// address spaces of three kinds, which share this contract and differ in
// their table format and in their flush:
//
//   - the address space of the kernel, in the page tables of the
//     processor, which a TLB shootdown flushes on every core;
//   - the nested address space of a guest, in the same format, which the
//     ASID of the guest flushes;
//   - the DMA address space of an IOMMU domain, in the format of the
//     IOMMU, which an IOMMU command flushes.
//
// An unmap goes in three steps, so that no frame is reused while a core or
// a device may still reach it through a translation cache: Unmap, Flush,
// Reclaim. Map needs no flush: no translation cache holds a page that was
// not present.
//
// The memory provider of the Go runtime calls an AddressSpace with locks
// of the runtime held, and also when the runtime allows no write barrier.
// So an implementation never allocates, never blocks and never writes a
// pointer outside the stack (see Provider).
type AddressSpace interface {
	// Map maps size bytes from virt on to the memory from phys on with
	// the attributes attr. No page of the range is mapped, and attr has
	// Read. An implementation refuses attributes that its format cannot
	// hold.
	Map(virt, phys, size uint64, attr Attr) error
	// Unmap makes every page of the range unreachable and keeps its
	// physical address for Reclaim. Every page of the range is mapped.
	Unmap(virt, size uint64) error
	// Flush returns when no core and no device can reach a page that an
	// earlier Unmap made unreachable.
	Flush()
	// Reclaim removes the pages of the range that Unmap made
	// unreachable, after a Flush, and calls free with the physical
	// address and the size of each of them. Pages that are not mapped at
	// all are skipped.
	Reclaim(virt, size uint64, free func(phys, size uint64)) error
	// Lookup returns the physical address and the attributes of the page
	// at virt. It reports false when the page is not mapped, also while
	// it waits for Reclaim.
	Lookup(virt uint64) (phys uint64, attr Attr, ok bool)
}
