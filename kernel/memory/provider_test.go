// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import "testing"

// testProvider returns a provider with a window of 64 pages over 16 frames
// above 4 GiB, and records what it clears.
func testProvider(t *testing.T, limit uint64) (*Provider, *testSpace, *Owner, *[]Range) {
	t.Helper()

	ranges := []Range{{Start: 8 * gib, End: 8*gib + 16*FrameSize}}
	words, err := Words(ranges, Range{})

	if err != nil {
		t.Fatal(err)
	}

	frames, err := New(ranges, Range{}, make([]uint64, words))

	if err != nil {
		t.Fatal(err)
	}

	m := newTestSpace()
	owner := NewOwner("Go heap", limit)

	p, err := NewProvider(ProviderConfig{
		Window: Range{Start: window, End: window + 64*FrameSize},
		Frames: frames,
		Owner:  owner,
		Zone:   High,
		Space:  m,
	})

	if err != nil {
		t.Fatal(err)
	}

	var cleared []Range

	p.clear = func(virt, size uint64) {
		cleared = append(cleared, Range{virt, virt + size})
	}

	return p, m, owner, &cleared
}

func TestProviderUse(t *testing.T) {
	p, m, owner, cleared := testProvider(t, 0)

	addr := p.Reserve(0, 8*FrameSize, FrameSize)

	if addr != window {
		t.Fatalf("Reserve: got %#x, want %#x", addr, uint64(window))
	}

	if len(m.mapped) != 0 || owner.Bytes() != 0 {
		t.Fatal("Reserve mapped pages")
	}

	p.Use(addr+FrameSize, 2*FrameSize)
	p.Use(addr, 4*FrameSize) // two pages already have frames

	if len(m.mapped) != 4 || owner.Bytes() != 4*FrameSize {
		t.Errorf("%d pages mapped, owner has %d bytes; want 4 pages", len(m.mapped), owner.Bytes())
	}

	for virt, pg := range m.mapped {
		if pg.attr != Data {
			t.Errorf("page %#x mapped with %#x, want Data", virt, pg.attr)
		}
	}

	if len(*cleared) != 4 {
		t.Errorf("%d pages cleared, want 4", len(*cleared))
	}

	if m.flushes != 0 {
		t.Error("Use flushed the TLB")
	}
}

func TestProviderUnuse(t *testing.T) {
	p, m, owner, cleared := testProvider(t, 0)

	addr := p.Reserve(0, 8*FrameSize, FrameSize)

	// Pages 0, 1 and 4 have frames; the range of Unuse holds two runs.
	p.Use(addr, 2*FrameSize)
	p.Use(addr+4*FrameSize, FrameSize)
	*cleared = nil

	p.Unuse(addr, 8*FrameSize)

	if len(m.mapped) != 0 || len(m.unmapped) != 0 || owner.Bytes() != 0 {
		t.Errorf("after Unuse: %d mapped, %d waiting, owner %d bytes", len(m.mapped), len(m.unmapped), owner.Bytes())
	}

	a := uint64(addr)
	want := []Range{{a, a + 2*FrameSize}, {a + 4*FrameSize, a + 5*FrameSize}}

	if len(*cleared) != 2 || (*cleared)[0] != want[0] || (*cleared)[1] != want[1] {
		t.Errorf("cleared %v, want %v", *cleared, want)
	}

	if m.flushes != 1 {
		t.Errorf("%d flushes, want 1", m.flushes)
	}

	// Without frames, Unuse neither clears nor flushes.
	p.Unuse(addr, 8*FrameSize)

	if m.flushes != 1 {
		t.Errorf("Unuse of a range without frames flushed")
	}

	// The range stays reserved, and the frames serve again.
	if again := p.Reserve(addr, FrameSize, FrameSize); again == addr {
		t.Error("Unuse released the range")
	}

	p.Use(addr, 8*FrameSize)

	if owner.Bytes() != 8*FrameSize {
		t.Errorf("after the second Use: owner %d bytes", owner.Bytes())
	}
}

func TestProviderRelease(t *testing.T) {
	p, _, owner, _ := testProvider(t, 0)

	addr := p.Reserve(0, 16*FrameSize, FrameSize)
	p.Use(addr, 16*FrameSize)
	p.Release(addr, 16*FrameSize)

	if free, _ := p.Free(); free != 64*FrameSize || owner.Bytes() != 0 {
		t.Errorf("after Release: %d bytes free, owner %d bytes", free, owner.Bytes())
	}
}

func TestProviderFatal(t *testing.T) {
	p, _, _, _ := testProvider(t, 2*FrameSize)

	addr := p.Reserve(0, 4*FrameSize, FrameSize)

	mustPanic(t, "use beyond the limit", func() { p.Use(addr, 4*FrameSize) })
}

func TestNewProviderRefuses(t *testing.T) {
	if _, err := NewProvider(ProviderConfig{Window: Range{Start: window, End: window + gib}}); err == nil {
		t.Error("incomplete configuration: no error")
	}
}

func TestProviderNoAllocation(t *testing.T) {
	p, m, _, _ := testProvider(t, 0)

	// The test mapper allocates in its maps; count only the provider.
	m.mapped = make(map[uint64]page, 64)
	m.unmapped = make(map[uint64]page, 64)
	p.clear = func(_, _ uint64) {}

	addr := p.Reserve(0, 8*FrameSize, FrameSize)

	if n := testing.AllocsPerRun(10, func() {
		p.Use(addr, 8*FrameSize)
		p.Unuse(addr, 8*FrameSize)
	}); n != 0 {
		t.Errorf("Use and Unuse allocate %v times", n)
	}
}
