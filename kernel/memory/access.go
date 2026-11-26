// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import (
	"sync/atomic"
	"unsafe"
)

// This file and the register file of each architecture hold every
// conversion from an address to memory of the memory manager, and so of
// every program that uses it: the views of RAM (View), the page tables
// (Tables.Table) and the device registers (RegisterView). All other code
// reaches memory outside the Go heap only through these types, so the Go
// compiler checks its accesses. Each conversion states the condition that
// makes it safe, who assures the condition, and the tests that check it.

// View is RAM outside the Go heap that an owner holds: the DMA part of a
// grant (Grant.DMA) or a region of kernel memory (Region.View). Every
// access checks its offset against the size of the view and panics
// outside it. When the owner gives the memory back, the memory leaves the
// address space, so an access through an old view faults.
//
// The zero View is empty.
type View struct {
	b []byte
}

// viewAt returns the view of size bytes at the address addr.
//
// Safe because the caller holds the range for an owner and keeps it
// mapped, readable and writable, for the processor while the view exists:
// Grant and Region map the range before they make a view, and unmap it
// only when the owner gives it back. The range lies outside the Go heap,
// so the garbage collector ignores the slice. Checked by TestViewAt,
// TestGrantViews and FuzzViewWords.
//
//go:nocheckptr
//go:nosplit
func viewAt(addr, size uint64) View {
	p := (*byte)(unsafe.Add(unsafe.Pointer(nil), uintptr(addr)))
	return View{b: unsafe.Slice(p, size)}
}

// HeapView returns a view of n bytes of new, zeroed Go memory, aligned to
// 8 bytes, for tests and for programs without a memory manager, such as
// the mock ports of drivers. The view holds the memory, so the garbage
// collector keeps it while the view is in use.
//
// Safe because the slice points into one allocation of at least n bytes,
// which the slice keeps alive. Checked by TestHeapView.
func HeapView(n int) View {
	if n < 0 {
		panic("memory: negative size of a view")
	}

	words := make([]uint64, (n+7)/8)
	p := (*byte)(unsafe.Pointer(unsafe.SliceData(words)))

	return View{b: unsafe.Slice(p, n)}
}

// ViewOf returns a view of the Go slice b, with the bounds of b. It adds
// no access, because b is already memory that the program may use. It is
// for the boards that take their DMA memory from the TamaGo package dma,
// until they have a memory manager.
func ViewOf(b []byte) View {
	return View{b: b[:len(b):len(b)]}
}

// Size returns the size of the view in bytes.
func (v View) Size() int {
	return len(v.b)
}

// Bytes returns the memory of the view as a slice, for plain accesses at
// the speed of a slice. Its capacity is its length, so an append never
// writes past the view.
func (v View) Bytes() []byte {
	return v.b
}

// Slice returns the n bytes of the view from off on.
func (v View) Slice(off, n int) View {
	return View{b: v.b[off : off+n : off+n]}
}

// Addr returns the address of the first byte of the view for the
// processor, or 0 for an empty view.
//
// Safe because it only reads the address of the slice. Checked by
// TestViewAt.
func (v View) Addr() uint64 {
	if len(v.b) == 0 {
		return 0
	}

	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(v.b))))
}

// Load32 reads the aligned 32-bit word at off with one atomic read, for a
// word that a device or another hardware thread writes, such as the valid
// bit of a completion entry.
func (v View) Load32(off int) uint32 {
	return atomic.LoadUint32(v.word32(off))
}

// Store32 writes the aligned 32-bit word at off with one atomic write.
func (v View) Store32(off int, x uint32) {
	atomic.StoreUint32(v.word32(off), x)
}

// Load64 reads the aligned 64-bit word at off with one atomic read.
func (v View) Load64(off int) uint64 {
	return atomic.LoadUint64(v.word64(off))
}

// Store64 writes the aligned 64-bit word at off with one atomic write.
func (v View) Store64(off int, x uint64) {
	atomic.StoreUint64(v.word64(off), x)
}

// word32 returns the 32-bit word at off of the view.
//
// Safe because the slice expression panics unless the 4 bytes lie in the
// view, and the address test panics unless the word is aligned, as the
// atomic instructions need. Checked by TestViewWords and FuzzViewWords.
func (v View) word32(off int) *uint32 {
	p := unsafe.Pointer(unsafe.SliceData(v.b[off : off+4]))

	if uintptr(p)%4 != 0 {
		panic("memory: unaligned 32-bit word in a view")
	}

	return (*uint32)(p)
}

// word64 returns the 64-bit word at off of the view.
//
// Safe for the same reasons as word32. Checked by TestViewWords and
// FuzzViewWords.
func (v View) word64(off int) *uint64 {
	p := unsafe.Pointer(unsafe.SliceData(v.b[off : off+8]))

	if uintptr(p)%8 != 0 {
		panic("memory: unaligned 64-bit word in a view")
	}

	return (*uint64)(p)
}

// Table returns the entries of the page table at addr.
//
// Safe because addr is a table that t gave out (Alloc) and has not taken
// back, and the kernel maps the tables 1:1, readable and writable, while
// they are in use. The tables lie outside the Go heap. Checked by the
// tests of Tables.
//
//go:nocheckptr
//go:nosplit
func (t *Tables) Table(addr uint64) *[TableEntries]uint64 {
	return (*[TableEntries]uint64)(unsafe.Add(unsafe.Pointer(nil), uintptr(addr)))
}

// RegisterView is device registers that an owner holds: a register range
// of its grant (Grant.RegisterView). Every access checks that the register
// lies in the range and is aligned to its width, and then is one access
// of exactly that width, in program order (see the register file of the
// architecture).
//
// The zero RegisterView is empty.
type RegisterView struct {
	addr uintptr
	size uintptr
}

// Size returns the size of the register range in bytes.
func (r RegisterView) Size() int {
	return int(r.size)
}

// Read32 reads the 32-bit register at off.
func (r RegisterView) Read32(off int) uint32 {
	return read32(r.at(off, 4))
}

// Write32 writes x to the 32-bit register at off. Every earlier write of
// the processor to memory reaches the memory before the device sees this
// write.
func (r RegisterView) Write32(off int, x uint32) {
	write32(r.at(off, 4), x)
}

// Read64 reads the 64-bit register at off.
func (r RegisterView) Read64(off int) uint64 {
	return read64(r.at(off, 8))
}

// Write64 writes x to the 64-bit register at off, ordered as Write32.
func (r RegisterView) Write64(off int, x uint64) {
	write64(r.at(off, 8), x)
}

// at returns the address of the register of n bytes at off. It panics
// unless the register lies in the range and is aligned to n bytes.
func (r RegisterView) at(off int, n uintptr) uintptr {
	if off < 0 || uintptr(off) > r.size || r.size-uintptr(off) < n {
		panic("memory: register outside its range")
	}

	addr := r.addr + uintptr(off)

	if addr%n != 0 {
		panic("memory: unaligned register")
	}

	return addr
}
