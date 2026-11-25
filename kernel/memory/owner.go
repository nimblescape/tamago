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
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// ErrEnded means that the owner has ended and takes no new piece.
var ErrEnded = errors.New("memory: the owner has ended")

// Piece is something that an owner holds from a manager of the kernel,
// besides its frames: a DMA buffer, the registers of a device, a region of
// kernel memory, and later an interrupt vector, a device function or an
// IOMMU domain. The manager that gives out a piece records it with Hold and
// takes it back in Release.
type Piece interface {
	// String describes the piece for the diagnostics, for example
	// "DMA 0x7f000000-0x7f004000".
	String() string
	// Release gives the piece back to its manager, which clears it. A
	// second call does nothing and returns nil.
	Release() error
}

// Owner holds the pieces that the managers of a kernel give out: the Go
// heap, a driver, the page tables, the tables of the IOMMU, and later a
// process or a guest. It counts its frames and may have a limit of frames.
// It records every other piece, so that End gives everything back.
//
// An owner may have children (NewChild), such as the drivers of the
// kernel or the owners of a guest. The frames of a child count also for
// its parent, so the limit of the parent holds for the parent and all its
// children together, and End of the parent ends its children.
type Owner struct {
	name    string
	limited bool
	limit   uint64 // frames
	frames  atomic.Uint64
	parent  *Owner // set once, when the owner is made

	// lock guards the fields below. The memory provider of the runtime
	// never takes it: it changes only the count of frames.
	lock   sync.Mutex
	pieces []Piece
	domain AddressSpace
	ended  bool
	// manager is the manager that lists the owner, or nil.
	manager *Manager
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

// NewChild returns a new owner with the name name, which may hold at most
// limit bytes, or any amount when limit is 0, and whose frames count also
// for o. The child is a piece of o and is listed by the manager of o.
func (o *Owner) NewChild(name string, limit uint64) (*Owner, error) {
	c := NewOwner(name, limit)
	c.parent = o

	if err := o.Hold(c); err != nil {
		return nil, err
	}

	o.lock.Lock()
	m := o.manager
	o.lock.Unlock()

	if m != nil {
		m.adopt(c)
	}

	return c, nil
}

// Parent returns the parent of the owner, or nil.
func (o *Owner) Parent() *Owner {
	return o.parent
}

// Release ends the owner as a piece of its parent (see End).
func (o *Owner) Release() error {
	return o.End()
}

// Name returns the name of the owner.
func (o *Owner) Name() string {
	return o.name
}

// Bytes returns the number of bytes of frames that the owner and its
// children hold.
func (o *Owner) Bytes() uint64 {
	return o.frames.Load() * FrameSize
}

// Limit returns the most bytes that the owner may hold, or 0 when it has
// no limit.
func (o *Owner) Limit() uint64 {
	return o.limit * FrameSize
}

// String describes the owner and what it holds for the diagnostics.
func (o *Owner) String() string {
	limit := "no limit"

	if o.limited {
		limit = fmt.Sprintf("limit %d KiB", o.Limit()>>10)
	}

	o.lock.Lock()
	n := len(o.pieces)
	o.lock.Unlock()

	return fmt.Sprintf("%q holds %d KiB of frames (%s) and %d other pieces", o.name, o.Bytes()>>10, limit, n)
}

// SetDomain gives the owner the DMA address space of its device (an IOMMU
// domain). Every DMA buffer of the owner is then also mapped there, at the
// address that the processor uses. It is set once, before the first DMA
// buffer.
func (o *Owner) SetDomain(d AddressSpace) error {
	o.lock.Lock()
	defer o.lock.Unlock()

	switch {
	case o.ended:
		return ErrEnded
	case o.domain != nil:
		return errors.New("memory: the owner has a domain already")
	case slices.ContainsFunc(o.pieces, isBuffer):
		return errors.New("memory: the owner holds DMA buffers already")
	}

	o.domain = d

	return nil
}

// Domain returns the DMA address space of the owner, or nil.
func (o *Owner) Domain() AddressSpace {
	o.lock.Lock()
	defer o.lock.Unlock()

	return o.domain
}

// Hold records that the owner holds p. It returns ErrEnded, and records
// nothing, when the owner has ended: the caller then releases p itself.
func (o *Owner) Hold(p Piece) error {
	o.lock.Lock()
	defer o.lock.Unlock()

	if o.ended {
		return ErrEnded
	}

	o.pieces = append(o.pieces, p)

	return nil
}

// Drop forgets p, which its manager took back. A piece that the owner does
// not hold is ignored.
func (o *Owner) Drop(p Piece) {
	o.lock.Lock()
	defer o.lock.Unlock()

	o.pieces = remove(o.pieces, p)
}

// Pieces returns the pieces that the owner holds, the oldest first.
func (o *Owner) Pieces() []Piece {
	o.lock.Lock()
	defer o.lock.Unlock()

	return append([]Piece(nil), o.pieces...)
}

// End ends the owner: it releases every piece, the newest first, and takes
// no new piece. It returns the errors of the releases, and an error when
// the owner still holds frames that are not part of a piece.
func (o *Owner) End() error {
	o.lock.Lock()

	if o.ended {
		o.lock.Unlock()
		return nil
	}

	o.ended = true
	pieces := o.pieces
	o.pieces = nil
	m := o.manager
	o.lock.Unlock()

	var errs []error

	for i := len(pieces) - 1; i >= 0; i-- {
		p := pieces[i]

		if err := p.Release(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}

	if n := o.frames.Load(); n != 0 {
		errs = append(errs, fmt.Errorf("memory: owner %q still holds %d frames after its end", o.name, n))
	}

	if o.parent != nil {
		o.parent.Drop(o)
	}

	if m != nil {
		m.forget(o)
	}

	return errors.Join(errs...)
}

// Ended reports whether the owner has ended.
func (o *Owner) Ended() bool {
	o.lock.Lock()
	defer o.lock.Unlock()

	return o.ended
}

// take counts n more frames for the owner and its parents and reports
// true, unless one of them would then hold more than its limit.
//
//go:nosplit
func (o *Owner) take(n uint64) bool {
	for p := o; p != nil; p = p.parent {
		if !p.takeOwn(n) {
			for q := o; q != p; q = q.parent {
				q.giveOwn(n)
			}

			return false
		}
	}

	return true
}

// give counts n frames fewer for the owner and its parents.
//
//go:nosplit
func (o *Owner) give(n uint64) {
	for p := o; p != nil; p = p.parent {
		p.giveOwn(n)
	}
}

// takeOwn counts n more frames for the owner alone, unless it would then
// hold more than its limit.
//
//go:nosplit
func (o *Owner) takeOwn(n uint64) bool {
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

// giveOwn counts n frames fewer for the owner alone.
//
//go:nosplit
func (o *Owner) giveOwn(n uint64) {
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

// remove removes the first element of s that equals x, and keeps the order
// of the others. It clears the element at the end, so that s holds no
// pointer to x.
func remove[E comparable](s []E, x E) []E {
	i := slices.Index(s, x)

	if i < 0 {
		return s
	}

	var zero E

	copy(s[i:], s[i+1:])
	s[len(s)-1] = zero

	return s[:len(s)-1]
}
