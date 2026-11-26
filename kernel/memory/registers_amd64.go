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

// The register accesses of amd64. RegisterView.at gives each function an
// address that lies in a register range of a grant and is aligned to the
// width of the access. The kernel maps that range uncached, without
// execute, while the grant exists (Grant.RegisterView).
//
// Safe because of these conditions, and because on amd64 an atomic load
// is one MOV that the compiler neither removes nor merges, and an atomic
// store is one XCHG. XCHG is a locked instruction, so every earlier write
// of the processor to memory reaches the memory before the device sees
// the write to the register. Uncached memory keeps the program order of
// the accesses. Checked by TestRegisters and TestGrantViews.

//go:nocheckptr
func read32(addr uintptr) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(nil), addr)))
}

//go:nocheckptr
func write32(addr uintptr, x uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Add(unsafe.Pointer(nil), addr)), x)
}

//go:nocheckptr
func read64(addr uintptr) uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(nil), addr)))
}

//go:nocheckptr
func write64(addr uintptr, x uint64) {
	atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(nil), addr)), x)
}
