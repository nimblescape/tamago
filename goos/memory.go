// Custom GOOS support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package goos

// Memory backs the memory states of the Go runtime (see runtime/mem.go):
// address space that is reserved, and memory that is in use.
//
// The runtime calls it with its locks held, on the system stack and on any
// processor. So a provider never allocates from the Go heap, never blocks
// and never uses the scheduler. Its addresses lie outside the break region
// of the runtime (see [Bloc] and [BlocMax]), and all sizes and addresses
// are multiples of the page size of 4096 bytes.
//
// The type is an alias of an interface literal, so that the runtime
// declares the same type without importing this package.
type Memory = interface {
	// Reserve takes size bytes of address space at hint, or elsewhere
	// when hint is 0 or taken, aligned to align, a power of two. It
	// returns 0 when no address space is left. The range has no memory
	// yet: an access faults.
	Reserve(hint, size, align uintptr) uintptr

	// Use backs the pages of the range that have no memory with memory
	// filled with zeros. Pages that have memory keep it. No memory left
	// is fatal, as on every system.
	Use(addr, size uintptr)

	// Unuse takes the memory of the range back and leaves the range
	// reserved. No data of the range stays in memory.
	Unuse(addr, size uintptr)

	// Release gives the range back completely: its memory and its
	// address space.
	Release(addr, size uintptr)
}

// SetMemory installs m as the memory provider of the runtime, once. From
// then on, the memory of the runtime grows beyond the break region and goes
// back: new requests go to m, and the break region keeps working as
// before.
//
// The runtime of the TamaGo distribution provides this function.
func SetMemory(m Memory)
