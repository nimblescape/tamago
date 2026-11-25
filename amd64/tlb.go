// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package amd64

import (
	"sync/atomic"

	"github.com/usbarmory/tamago/amd64/lapic"
)

// IRQ_TLB represents the interrupt vector of a TLB shootdown (see
// [CPU.FlushTLB]). Its handler drops the address translations of the
// processor that takes it.
const IRQ_TLB = 253 // priority 15

// tlbLimit is the time in nanoseconds in which every other processor must
// answer a TLB shootdown. A processor takes the interrupt as soon as it
// runs with interrupts enabled, so only a processor that hangs with
// interrupts disabled misses it.
const tlbLimit = 1e9

// icrSendPending is the delivery status of the interrupt command register
// in xAPIC mode: the LAPIC has not yet accepted the last IPI.
const icrSendPending = 1 << lapic.ICR_DLV_STATUS

var (
	// tlbGeneration counts the TLB shootdowns.
	tlbGeneration uint64

	// tlbDone holds, by APIC ID, the generation that each processor
	// read before it last dropped its translations in the handler of
	// IRQ_TLB.
	tlbDone [maxAPICID + 1]uint64
)

// defined in tlb.s
func tlbBroadcast() (apicid uint32)

// FlushTLB drops the address translations of every processor that runs the
// program, and returns when no processor can use a translation that changed
// before the call. To take a page away, a caller removes its mapping, calls
// FlushTLB, and only then reuses the memory of the page.
//
// The calling processor drops its own translations, and the others drop
// theirs in the handler of [IRQ_TLB]. A reload of CR3 drops no global
// translation, so the changed pages must not be global: the page tables of
// package paging hold no global pages.
//
// FlushTLB never allocates and never yields the processor, so the runtime
// may call it with its locks held, on the system stack. It must be called
// with interrupts enabled, after [CPU.EnableExceptions]: two processors
// that call it at once then answer the shootdown of each other. A
// processor that does not answer within one second stops the program.
func (cpu *CPU) FlushTLB() {
	// The changes of the caller are visible before the new generation,
	// as the processor keeps the order of stores and the addition is a
	// locked instruction.
	gen := atomic.AddUint64(&tlbGeneration, 1)
	self := int(tlbBroadcast())

	var start int64

	for _, id := range cpu.cores {
		if id == self {
			continue
		}

		for atomic.LoadUint64(&tlbDone[id]) < gen {
			now := cpu.GetTime()

			switch {
			case start == 0:
				start = now
			case now-start > tlbLimit:
				print("TLB shootdown: no answer from the processor with APIC ID ", id, "\n")
				panic("TLB shootdown failed")
			}
		}
	}
}

// setCores records the APIC IDs of the processors that run the program, for
// the TLB shootdowns: the calling processor and the APs with the APIC IDs
// aps. Each of them takes interrupts once it is ready, before the runtime
// gives it a task.
func (cpu *CPU) setCores(aps []int) {
	bsp := int(cpu.LAPIC.ID())
	cores := []int{bsp}

	for _, id := range aps {
		if id != bsp {
			cores = append(cores, id)
		}
	}

	cpu.cores = cores
}
