// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package memory manages the physical memory of a TamaGo kernel: the
// frames of the memory list, the owners that hold them, and the limit of
// each owner.
//
// The package has no architecture code and maps nothing. The page tables
// of the processor and of the IOMMU map the frames that it gives out.
// Every free frame is zero: the kernel clears all free memory at its
// start, and an owner clears each frame through its own mapping before it
// gives the frame back.
//
// The memory provider of the Go runtime calls this package with locks of
// the runtime held, on any core. So the methods that give out and take
// back frames never allocate from the Go heap and never block: they take
// a spin lock, the code that holds it has no preemption point, and the
// bitmaps lie in storage that the caller gives.
package memory

import "errors"

// FrameSize is the size of a frame in bytes.
const FrameSize = 4096

// lowEnd is the end of the memory that devices with 32-bit DMA addresses
// reach, and that a core reaches in 32-bit mode.
const lowEnd = 1 << 32

// Range is the physical memory from Start to End, End excluded.
type Range struct {
	Start uint64
	End   uint64
}

// Zone selects the part of the physical memory that frames come from.
type Zone int

const (
	// Low is the memory below 4 GiB outside the DMA region, such as the
	// page tables that a core loads while it starts in 32-bit mode.
	Low Zone = iota
	// High is the memory from 4 GiB on outside the DMA region, such as
	// the memory of the Go runtime.
	High
	// DMA is the fixed DMA region (see Manager.StartDMA). It holds only
	// the DMA parts of devices and keeps its place and size; on amd64 it
	// lies below 4 GiB, for devices with 32-bit DMA addresses.
	DMA
)

func (z Zone) String() string {
	switch z {
	case Low:
		return "below 4 GiB"
	case High:
		return "from 4 GiB"
	case DMA:
		return "of the DMA region"
	default:
		return "unknown zone"
	}
}

var (
	// ErrNoMemory means that the zone has no free frames in the number
	// and the alignment asked for.
	ErrNoMemory = errors.New("memory: no free frames")
	// ErrLimit means that the owner would hold more than its limit.
	ErrLimit = errors.New("memory: limit of the owner reached")
)
