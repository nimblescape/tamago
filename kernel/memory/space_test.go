// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import (
	"slices"
	"testing"
)

const window = 512 * gib

func newSpace(t testing.TB, pages uint64) *Space {
	t.Helper()

	s, err := NewSpace(Range{Start: window, End: window + pages*FrameSize})

	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestNewSpaceRefuses(t *testing.T) {
	for _, w := range []Range{
		{Start: window + 1, End: window + gib},
		{Start: window, End: window + gib + 1},
		{Start: window, End: window},
	} {
		if _, err := NewSpace(w); err == nil {
			t.Errorf("window %#x to %#x: no error", w.Start, w.End)
		}
	}
}

func TestReserve(t *testing.T) {
	s := newSpace(t, 64)

	// The first range comes from the start, the second at its hint.
	a, ok := s.Reserve(0, 4*FrameSize, FrameSize)

	if !ok || a != window {
		t.Fatalf("first: got %#x, %v; want %#x", a, ok, uint64(window))
	}

	b, ok := s.Reserve(window+32*FrameSize, 8*FrameSize, FrameSize)

	if !ok || b != window+32*FrameSize {
		t.Fatalf("at the hint: got %#x, %v", b, ok)
	}

	// A taken hint falls back to the first fitting range, aligned.
	c, ok := s.Reserve(window+33*FrameSize, 2*FrameSize, 8*FrameSize)

	if !ok || c != window+8*FrameSize {
		t.Errorf("hint taken: got %#x, %v; want %#x", c, ok, uint64(window+8*FrameSize))
	}

	if _, ok := s.Reserve(0, 64*FrameSize, FrameSize); ok {
		t.Error("larger than the free space: reserved")
	}

	if free, _ := s.Free(); free != 50*FrameSize {
		t.Errorf("free %d bytes, want %d", free, 50*FrameSize)
	}

	mustPanic(t, "size 0", func() { s.Reserve(0, 0, FrameSize) })
	mustPanic(t, "size not frames", func() { s.Reserve(0, 100, FrameSize) })
	mustPanic(t, "alignment not a power of two", func() { s.Reserve(0, FrameSize, 3*FrameSize) })
}

func TestRelease(t *testing.T) {
	s := newSpace(t, 16)

	var r []uint64

	for range 4 {
		addr, ok := s.Reserve(0, 4*FrameSize, FrameSize)

		if !ok {
			t.Fatal("reserve failed")
		}

		r = append(r, addr)
	}

	// Release the second and the fourth, then the third, which joins
	// both into one extent.
	s.Release(r[1], 4*FrameSize)
	s.Release(r[3], 4*FrameSize)

	if s.n != 2 {
		t.Errorf("%d free extents, want 2", s.n)
	}

	s.Release(r[2], 4*FrameSize)

	if free, largest := s.Free(); s.n != 1 || free != 12*FrameSize || largest != 12*FrameSize {
		t.Errorf("%d extents, %d free, largest %d; want one extent of %d", s.n, free, largest, 12*FrameSize)
	}

	mustPanic(t, "free range", func() { s.Release(r[2], FrameSize) })
	mustPanic(t, "partly free", func() { s.Release(r[0], 5*FrameSize) })
	mustPanic(t, "outside", func() { s.Release(window+16*FrameSize, FrameSize) })
	mustPanic(t, "not frames", func() { s.Release(r[0]+1, FrameSize) })

	s.Release(r[0], 4*FrameSize)

	if free, _ := s.Free(); s.n != 1 || free != 16*FrameSize {
		t.Errorf("%d extents and %d free after all releases", s.n, free)
	}
}

func TestExtentsFull(t *testing.T) {
	s := newSpace(t, 2*maxExtents+2)

	for range 2*maxExtents + 2 {
		if _, ok := s.Reserve(0, FrameSize, FrameSize); !ok {
			t.Fatal("reserve failed")
		}
	}

	// Every second page free: maxExtents extents.
	for i := range uint64(maxExtents) {
		s.Release(window+2*i*FrameSize, FrameSize)
	}

	if s.n != maxExtents {
		t.Fatalf("%d extents, want %d", s.n, maxExtents)
	}

	mustPanic(t, "one extent too many", func() { s.Release(window+(2*maxExtents+1)*FrameSize, FrameSize) })

	// A hint inside a free extent of one page still works: the extent goes.
	if addr, ok := s.Reserve(window+2*FrameSize, FrameSize, FrameSize); !ok || addr != window+2*FrameSize {
		t.Errorf("hint with a full array: got %#x, %v", addr, ok)
	}
}

// FuzzSpace runs reservations and releases against a model with one entry
// per page, and checks after each step that the free extents describe
// exactly the free pages.
func FuzzSpace(f *testing.F) {
	f.Add([]byte{0, 3, 1, 1, 9, 0, 2, 0, 1, 0, 40, 7})

	f.Fuzz(func(t *testing.T, ops []byte) {
		const pages = 96

		s := newSpace(t, pages)
		reserved := make([]bool, pages)

		type piece struct{ addr, size uint64 }

		var pieces []piece

		for ; len(ops) >= 3; ops = ops[3:] {
			op, x, y := ops[0], ops[1], ops[2]

			switch op % 2 {
			case 0:
				size := (uint64(x)%9 + 1) * FrameSize
				align := uint64(FrameSize) << (y % 4)
				hint := uint64(0)

				if op&2 != 0 {
					hint = window + uint64(y)%pages*FrameSize
				}

				addr, ok := s.Reserve(hint, size, align)

				if !ok {
					if fitsPages(reserved, size, align) {
						t.Fatalf("no reservation of %d bytes aligned to %#x, but it fits", size, align)
					}

					continue
				}

				if addr%align != 0 && addr != hint {
					t.Fatalf("address %#x not aligned to %#x", addr, align)
				}

				for p := (addr - window) / FrameSize; p < (addr-window+size)/FrameSize; p++ {
					if p >= pages || reserved[p] {
						t.Fatalf("page %d reserved twice or outside", p)
					}

					reserved[p] = true
				}

				pieces = append(pieces, piece{addr, size})
			case 1:
				if len(pieces) == 0 {
					continue
				}

				i := int(x) % len(pieces)
				p := pieces[i]
				pieces = slices.Delete(pieces, i, i+1)
				s.Release(p.addr, p.size)

				for k := (p.addr - window) / FrameSize; k < (p.addr-window+p.size)/FrameSize; k++ {
					reserved[k] = false
				}
			}

			checkSpace(t, s, reserved)
		}
	})
}

// checkSpace compares the free extents with the model.
func checkSpace(t *testing.T, s *Space, reserved []bool) {
	t.Helper()

	var want []Range

	for p, r := range reserved {
		addr := window + uint64(p)*FrameSize

		switch {
		case r:
		case len(want) > 0 && want[len(want)-1].End == addr:
			want[len(want)-1].End += FrameSize
		default:
			want = append(want, Range{addr, addr + FrameSize})
		}
	}

	if !slices.Equal(s.free[:s.n], want) {
		t.Fatalf("free extents %v, the model says %v", s.free[:s.n], want)
	}
}

// fitsPages reports whether size bytes aligned to align are free in the
// model.
func fitsPages(reserved []bool, size, align uint64) bool {
	n := size / FrameSize

	for first := uint64(0); first+n <= uint64(len(reserved)); first++ {
		if (window+first*FrameSize)%align != 0 {
			continue
		}

		if !slices.Contains(reserved[first:first+n], true) {
			return true
		}
	}

	return false
}
