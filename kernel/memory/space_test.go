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
	"sync"
)

// page is a page of a testSpace.
type page struct {
	phys uint64
	attr Attr
}

// testSpace is an AddressSpace that keeps its pages in maps. A page that
// Unmap unmapped waits in unmapped until Reclaim, which refuses to take it
// before a Flush.
type testSpace struct {
	mu       sync.Mutex
	mapped   map[uint64]page
	unmapped map[uint64]page
	stale    bool // an Unmap came after the last Flush
	flushes  int
	fail     error // the error of the next Map, when not nil
	maps     int   // the number of calls of Map
	failAt   int   // the call of Map that fails, or 0
}

func newTestSpace() *testSpace {
	return &testSpace{mapped: map[uint64]page{}, unmapped: map[uint64]page{}}
}

func (s *testSpace) Map(virt, phys, size uint64, attr Attr) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.fail; err != nil {
		s.fail = nil
		return err
	}

	if s.maps++; s.maps == s.failAt {
		return errors.New("test: map fails")
	}

	if attr&Read == 0 || size == 0 || virt%FrameSize != 0 || phys%FrameSize != 0 || size%FrameSize != 0 {
		return errors.New("test: invalid map")
	}

	for off := uint64(0); off < size; off += FrameSize {
		_, mapped := s.mapped[virt+off]
		_, waiting := s.unmapped[virt+off]

		if mapped || waiting {
			return errors.New("test: page in use")
		}
	}

	for off := uint64(0); off < size; off += FrameSize {
		s.mapped[virt+off] = page{phys + off, attr}
	}

	return nil
}

func (s *testSpace) Unmap(virt, size uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for off := uint64(0); off < size; off += FrameSize {
		if _, ok := s.mapped[virt+off]; !ok {
			return errors.New("test: unmap of a page that is not mapped")
		}
	}

	for off := uint64(0); off < size; off += FrameSize {
		s.unmapped[virt+off] = s.mapped[virt+off]
		delete(s.mapped, virt+off)
	}

	s.stale = true

	return nil
}

func (s *testSpace) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stale = false
	s.flushes++
}

func (s *testSpace) Reclaim(virt, size uint64, free func(phys, size uint64)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stale {
		return errors.New("test: reclaim before a flush")
	}

	for off := uint64(0); off < size; off += FrameSize {
		if _, ok := s.mapped[virt+off]; ok {
			return errors.New("test: reclaim of a mapped page")
		}
	}

	for off := uint64(0); off < size; off += FrameSize {
		if p, ok := s.unmapped[virt+off]; ok {
			delete(s.unmapped, virt+off)
			free(p.phys, FrameSize)
		}
	}

	return nil
}

func (s *testSpace) Lookup(virt uint64) (uint64, Attr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.mapped[virt&^(FrameSize-1)]

	return p.phys + virt%FrameSize, p.attr, ok
}

// failAfter makes the Map of s fail after n more maps that succeed.
func (s *testSpace) failAfter(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failAt = s.maps + n + 1
}

// pages returns the number of mapped pages and of pages that wait for
// Reclaim.
func (s *testSpace) pages() (mapped, waiting int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.mapped), len(s.unmapped)
}
