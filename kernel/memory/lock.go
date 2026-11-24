// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import "sync/atomic"

// spinLock is a lock that never parks the goroutine, so that code which
// holds a lock of the Go runtime may take it.
//
// The code that holds a spinLock must have no preemption point: on TamaGo
// a goroutine stops only in the stack check at the entry of a function,
// so that code calls only functions without a stack check (nosplit) and
// intrinsics. A goroutine that held the lock and stopped could keep every
// other core waiting, also a core that stops the world for the garbage
// collector.
type spinLock struct {
	held atomic.Bool
}

//go:nosplit
func (l *spinLock) lock() {
	for !l.held.CompareAndSwap(false, true) {
	}
}

//go:nosplit
func (l *spinLock) unlock() {
	l.held.Store(false)
}
