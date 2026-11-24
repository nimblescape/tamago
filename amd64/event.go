// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package amd64

import (
	"runtime/goos"

	"github.com/usbarmory/tamago/amd64/lapic"
)

// Event vectors connect each interrupt source with its own goroutine. The
// handler of an event vector raises the runtime event with the number of
// the vector (see [goos.RaiseEvent]) and ends the interrupt at once, on the
// processor that takes it. The goroutine that waits for the vector (see
// [CPU.WaitEvent]) then becomes runnable on any processor. Unlike
// [CPU.ServiceInterrupts], no goroutine relays the interrupts and no
// processor handles the interrupts of the others: a device chooses the
// processor of each interrupt in its message (see lapic.MSI).
const (
	EventFirst = 32
	EventLast  = IRQ_MAX
)

// defined in event.s
func eventTableAddress() uintptr

// EnableEvents makes the vectors from [EventFirst] to [EventLast] event
// vectors and enables the LAPIC of the BSP. It must be called after
// [CPU.EnableExceptions], whose gates of these vectors it changes, and
// before the IDT is write protected. Then [CPU.ServiceInterrupts] gets no
// interrupt of these vectors.
//
// Spurious interrupts take the vector of [IRQ_WAKEUP], whose handler only
// records a wake-up, on the BSP and on the APs.
func (cpu *CPU) EnableEvents() {
	setGates(EventFirst, EventLast, eventTableAddress()-EventFirst*callSize)

	cpu.LAPIC.Write(lapic.LAPIC_SVR, 1<<SVR_ENABLE|IRQ_WAKEUP)
}

// WaitEvent blocks the calling goroutine until an interrupt with the event
// vector arrives on any processor, and returns at once when one has
// arrived since the last WaitEvent of the vector. Several interrupts before
// a WaitEvent count once. At most one goroutine waits for a vector at a
// time.
func (cpu *CPU) WaitEvent(vector int) {
	if vector < EventFirst || vector > EventLast {
		panic("WaitEvent: not an event vector")
	}

	goos.WaitEvent(uint32(vector))
}
