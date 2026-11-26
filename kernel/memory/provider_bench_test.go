// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// arraySpace is an AddressSpace for benchmarks: one entry per page of a
// window in an array, changed with atomic operations as page tables are, so
// that it adds no lock of its own.
type arraySpace struct {
	base  uint64
	pages []atomic.Uint64 // phys | 1 when mapped, phys when unmapped
}

func (s *arraySpace) entry(virt uint64) *atomic.Uint64 {
	return &s.pages[(virt-s.base)/FrameSize]
}

func (s *arraySpace) Map(virt, phys, size uint64, attr Attr) error {
	for off := uint64(0); off < size; off += FrameSize {
		if !s.entry(virt+off).CompareAndSwap(0, (phys+off)|1) {
			return errors.New("bench: page in use")
		}
	}

	return nil
}

func (s *arraySpace) Unmap(virt, size uint64) error {
	for off := uint64(0); off < size; off += FrameSize {
		e := s.entry(virt + off)
		e.Store(e.Load() &^ 1)
	}

	return nil
}

func (s *arraySpace) Flush() {}

func (s *arraySpace) Reclaim(virt, size uint64, free func(phys, size uint64)) error {
	for off := uint64(0); off < size; off += FrameSize {
		e := s.entry(virt + off)

		if v := e.Load(); v != 0 && v&1 == 0 {
			e.Store(0)
			free(v, FrameSize)
		}
	}

	return nil
}

func (s *arraySpace) Lookup(virt uint64) (uint64, Attr, bool) {
	v := s.entry(virt).Load()
	return v &^ 1, Data, v&1 != 0
}

// BenchmarkProviderParallel lets several goroutines take and give back
// ranges of the Go memory at once, as the heap and the scavenger of the
// runtime do, and reports the slowest Use.
func BenchmarkProviderParallel(b *testing.B) {
	const rangeSize = 16 << 20

	workers := runtime.GOMAXPROCS(0)

	frameRange := Range{Start: 8 * gib, End: 8*gib + 4*gib}
	words, err := Words([]Range{frameRange}, Range{})

	if err != nil {
		b.Fatal(err)
	}

	frames, err := New([]Range{frameRange}, Range{}, make([]uint64, words))

	if err != nil {
		b.Fatal(err)
	}

	size := uint64(workers) * rangeSize
	space := &arraySpace{base: window, pages: make([]atomic.Uint64, size/FrameSize)}
	p, err := NewProvider(ProviderConfig{
		Window: Range{Start: window, End: window + size},
		Frames: frames,
		Owner:  NewOwner("Go heap", 0),
		Zone:   High,
		Space:  space,
	})

	if err != nil {
		b.Fatal(err)
	}

	// The clear writes real memory of the same size, so that its time
	// counts.
	scratch := make([][]byte, workers)

	for i := range scratch {
		scratch[i] = make([]byte, rangeSize)
	}

	p.clear = func(virt, size uint64) {
		i := (virt - window) / rangeSize
		off := (virt - window) % rangeSize
		clear(scratch[i][off : off+size])
	}

	addrs := make([]uintptr, workers)

	for i := range addrs {
		addrs[i] = p.Reserve(0, rangeSize, FrameSize)
	}

	var slowest atomic.Int64

	b.SetBytes(int64(workers) * rangeSize)
	b.ResetTimer()

	for range b.N {
		var wg sync.WaitGroup

		for _, addr := range addrs {
			wg.Go(func() {
				start := time.Now()
				p.Use(addr, rangeSize)

				if d := int64(time.Since(start)); d > slowest.Load() {
					slowest.Store(d)
				}

				p.Unuse(addr, rangeSize)
			})
		}

		wg.Wait()
	}

	b.ReportMetric(float64(slowest.Load())/1e6, "ms/slowest-use")
	b.ReportMetric(float64(runtime.GOMAXPROCS(0)), "procs")
}
