// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import "sync/atomic"

// Owner holds frames: the Go heap, the DMA memory of a device, the page
// tables, the tables of the IOMMU, and later a process or a guest. It
// counts its frames and may have a limit.
type Owner struct {
	name    string
	limited bool
	limit   uint64 // frames
	frames  atomic.Uint64
}

// NewOwner returns an owner with the name name. It may hold at most limit
// bytes, rounded down to whole frames, or any amount when limit is 0.
func NewOwner(name string, limit uint64) *Owner {
	return &Owner{
		name:    name,
		limited: limit != 0,
		limit:   limit / FrameSize,
	}
}

// Name returns the name of the owner.
func (o *Owner) Name() string {
	return o.name
}

// Bytes returns the number of bytes that the owner holds.
func (o *Owner) Bytes() uint64 {
	return o.frames.Load() * FrameSize
}

// Limit returns the most bytes that the owner may hold, or 0 when it has
// no limit.
func (o *Owner) Limit() uint64 {
	return o.limit * FrameSize
}

// take counts n more frames for the owner and reports true, unless the
// owner would then hold more than its limit.
//
//go:nosplit
func (o *Owner) take(n uint64) bool {
	for {
		held := o.frames.Load()

		if o.limited && n > o.limit-min(held, o.limit) {
			return false
		}

		if o.frames.CompareAndSwap(held, held+n) {
			return true
		}
	}
}

// give counts n frames fewer for the owner.
//
//go:nosplit
func (o *Owner) give(n uint64) {
	for {
		held := o.frames.Load()

		if n > held {
			panic("memory: an owner gives back more frames than it holds")
		}

		if o.frames.CompareAndSwap(held, held-n) {
			return
		}
	}
}
