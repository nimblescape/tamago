// Custom GOOS support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package goos

// Interrupt events are provided by the runtime of the TamaGo distribution,
// unlike the other functions of this package, which the application
// defines.
//
// An event connects one interrupt source with one goroutine: an interrupt
// handler raises the event, and the goroutine that waits for it becomes
// runnable on any processor.

// WaitEvent blocks the calling goroutine until event n (0 to 255) is
// raised, and clears it. An event raised while no goroutine waits stays
// pending, and several raises before a wait count once. At most one
// goroutine waits for an event at a time.
func WaitEvent(n uint32)

// RaiseEvent raises event n. It does not allocate, lock or use the
// runtime, so that interrupt handlers can call it.
//
//go:nosplit
func RaiseEvent(n uint32)
