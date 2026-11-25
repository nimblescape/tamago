// Custom GOOS support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package goos

// Preemption is provided by the runtime of the TamaGo distribution, as
// events are.
//
// With preemption, a goroutine that has run for 10 ms stops at its next
// function call, and the runtime moves a goroutine that makes no call to a
// safe point, as on Linux. The platform raises one interrupt for it, the
// preemption interrupt: on each processor at least every 10 ms, and when
// the runtime asks for it.

// StartPreemption turns preemption on. raise raises the preemption
// interrupt on the processor with the identifier procid (see [ProcID]),
// and must not allocate, lock or yield the processor. From the first
// preemption interrupt on a processor, the platform raises it there at
// least every 10 ms, and the gate of the interrupt jumps to
// [PreemptInterrupt].
//
// StartPreemption must be called on the only M of the program, after
// [ProcID] is set. The runtime raises the first preemption interrupt on
// the calling processor at once, and on the processor of every later M
// when the M starts.
func StartPreemption(raise func(procid uint64))

// PreemptInterrupt is the handler of the preemption interrupt. It is not a
// function to call: the gate of the interrupt jumps to it after the end of
// the interrupt, with interrupts disabled and the frame of the interrupt
// on the stack. It uses at most 32 bytes of the stack of the interrupted
// code beyond that frame (see [StackSystem]).
func PreemptInterrupt()
