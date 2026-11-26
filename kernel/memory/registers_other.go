// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

//go:build !amd64

package memory

// The register accesses of the other architectures are not implemented
// yet. They need barrier instructions around each access, which the
// register file of each architecture adds. Views of RAM work on every
// architecture.

const noRegisters = "memory: register access is not implemented on this architecture"

func read32(uintptr) uint32 { panic(noRegisters) }

func write32(uintptr, uint32) { panic(noRegisters) }

func read64(uintptr) uint64 { panic(noRegisters) }

func write64(uintptr, uint64) { panic(noRegisters) }
