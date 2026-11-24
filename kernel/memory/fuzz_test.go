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
	"slices"
	"testing"
)

// FuzzFrames runs a sequence of allocations and frees against a model that
// knows the owner of every frame. After each step it checks that no frame
// is given out twice, that every frame lies in its zone and alignment,
// that the counts of the owners and the zones match the model, that limits
// hold, and that an allocation fails only when no fitting run exists.
func FuzzFrames(f *testing.F) {
	f.Add([]byte{0, 0, 7, 1, 1, 0, 3, 2, 9, 2, 0, 1, 0, 1, 31})
	f.Add([]byte{4, 2, 3, 4, 2, 3, 4, 2, 3, 5, 0, 0})

	f.Fuzz(func(t *testing.T, ops []byte) {
		ranges := []Range{
			{Start: 4*gib - 3*FrameSize, End: 4*gib + 70*FrameSize},
			{Start: 6 * gib, End: 6*gib + 9*FrameSize},
		}

		frames := newFrames(t, ranges...)
		owners := []*Owner{
			NewOwner("any", 0),
			NewOwner("twenty", 20*FrameSize),
			NewOwner("five", 5*FrameSize),
		}

		type run struct {
			addr, n uint64
			owner   int
		}

		var runs []run
		model := map[uint64]int{} // frame address to owner index

		for ; len(ops) >= 3; ops = ops[3:] {
			op, x, y := ops[0], ops[1], ops[2]
			o := int(x) % len(owners)

			switch op % 3 {
			case 0, 1:
				z := Zone(op / 3 % 2)
				n := uint64(y%8) + 1
				align := uint64(FrameSize) << (y / 8 % 4)

				addr, err := frames.AllocRun(owners[o], z, n, align)

				switch {
				case errors.Is(err, ErrLimit):
					held := uint64(0)

					for _, owner := range model {
						if owner == o {
							held++
						}
					}

					if !owners[o].limited || held+n <= owners[o].limit {
						t.Fatalf("ErrLimit for %d frames of an owner with %d of %d", n, held, owners[o].limit)
					}

					continue
				case errors.Is(err, ErrNoMemory):
					if fits(frames, model, z, n, align) {
						t.Fatalf("ErrNoMemory, but %d frames aligned to %#x fit in zone %v", n, align, z)
					}

					continue
				case err != nil:
					t.Fatal(err)
				}

				if addr%align != 0 {
					t.Fatalf("address %#x not aligned to %#x", addr, align)
				}

				if (z == Low) != (addr+n*FrameSize <= lowEnd) {
					t.Fatalf("run %#x of %d frames not in zone %v", addr, n, z)
				}

				for k := range n {
					frame := addr + k*FrameSize

					if !slices.ContainsFunc(ranges, func(r Range) bool { return r.Start <= frame && frame+FrameSize <= r.End }) {
						t.Fatalf("frame %#x outside the ranges", frame)
					}

					if owner, taken := model[frame]; taken {
						t.Fatalf("frame %#x given to owner %d and to owner %d", frame, owner, o)
					}

					model[frame] = o
				}

				runs = append(runs, run{addr, n, o})
			case 2:
				if len(runs) == 0 {
					continue
				}

				i := int(x) % len(runs)
				r := runs[i]
				k := uint64(y)%r.n + 1

				frames.Free(owners[r.owner], r.addr, k)

				for j := range k {
					delete(model, r.addr+j*FrameSize)
				}

				if k == r.n {
					runs = slices.Delete(runs, i, i+1)
				} else {
					runs[i] = run{r.addr + k*FrameSize, r.n - k, r.owner}
				}
			}

			check(t, frames, owners, model)
		}

		for _, r := range runs {
			frames.Free(owners[r.owner], r.addr, r.n)
		}

		check(t, frames, owners, map[uint64]int{})
	})
}

// check compares the counts of the owners and the zones with the model.
func check(t *testing.T, frames *Frames, owners []*Owner, model map[uint64]int) {
	t.Helper()

	held := make([]uint64, len(owners))
	used := map[Zone]uint64{}

	for frame, owner := range model {
		held[owner] += FrameSize

		if frame < lowEnd {
			used[Low] += FrameSize
		} else {
			used[High] += FrameSize
		}
	}

	for i, o := range owners {
		if o.Bytes() != held[i] {
			t.Fatalf("owner %s holds %d bytes, the model says %d", o.Name(), o.Bytes(), held[i])
		}

		if o.limited && o.Bytes() > o.Limit() {
			t.Fatalf("owner %s holds %d bytes over its limit %d", o.Name(), o.Bytes(), o.Limit())
		}
	}

	for _, z := range []Zone{Low, High} {
		if total, free := frames.Usage(z); total-free != used[z] {
			t.Fatalf("zone %v has %d bytes in use, the model says %d", z, total-free, used[z])
		}
	}
}

// fits reports whether n free frames aligned to align lie in one area of
// zone z, by the model.
func fits(frames *Frames, model map[uint64]int, z Zone, n, align uint64) bool {
	for _, a := range frames.areas {
		if a.zone != z {
			continue
		}

		for first := alignUp(a.start, align); first+n*FrameSize <= a.start+a.frames*FrameSize; first += align {
			free := true

			for k := range n {
				if _, taken := model[first+k*FrameSize]; taken {
					free = false
					break
				}
			}

			if free {
				return true
			}
		}
	}

	return false
}
