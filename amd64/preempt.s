// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

#include "amd64.h"
#include "go_asm.h"
#include "textflag.h"

#define MSR_TSC_DEADLINE 0x000006e0

// func preemptGateAddress() uintptr
TEXT ·preemptGateAddress(SB),NOSPLIT,$0-8
	MOVQ	$·preemptGate(SB), AX
	MOVQ	AX, ret+0(FP)
	RET

// preemptGate is the gate of IRQ_PREEMPT. It ends the interrupt, sets the
// LAPIC timer of this processor to raise IRQ_PREEMPT again after
// PreemptPeriod, and jumps to the handler of the runtime with the frame of
// the interrupt unchanged (see goos.PreemptInterrupt).
TEXT ·preemptGate(SB),NOSPLIT|NOFRAME,$0
	// save caller registers
	PUSHQ	AX
	PUSHQ	CX
	PUSHQ	DX

	// clear interrupt
	LAPIC_SEND_EOI

	// The timer registers first: the first IRQ_PREEMPT on a processor
	// comes from the runtime, not from the timer.
	CMPB	·preemptDeadline(SB), $0
	JEQ	count

	// TSC deadline mode; in xAPIC mode, the fence orders the write of
	// the register before the write of the deadline
	MOVL	$(const_preemptDeadlineTimer), AX
	LAPIC_WRITE_AX(const_LAPIC_TIMER, const_x2apicTimer)
	MFENCE

	// deadline = TSC + preemptTicks
	RDTSC
	SHLQ	$32, DX
	ORQ	DX, AX
	ADDQ	·preemptTicks(SB), AX
	MOVQ	AX, DX
	SHRQ	$32, DX
	MOVL	$MSR_TSC_DEADLINE, CX
	WRMSR
	JMP	armed

count:
	// one-shot mode, divisor 1, preemptTicks ticks of the LAPIC timer
	MOVL	$(const_preemptCountTimer), AX
	LAPIC_WRITE_AX(const_LAPIC_TIMER, const_x2apicTimer)
	MOVL	$(const_timerDivideBy1), AX
	LAPIC_WRITE_AX(const_LAPIC_TIMER_DIV, const_x2apicTimerDiv)
	MOVQ	·preemptTicks(SB), AX
	LAPIC_WRITE_AX(const_LAPIC_TIMER_INIT, const_x2apicTimerInit)

armed:
	// restore caller registers
	POPQ	DX
	POPQ	CX
	POPQ	AX

	JMP	runtime∕goos·PreemptInterrupt(SB)

// func preemptRaise(apicid uint64)
//
// preemptRaise sends IRQ_PREEMPT to the processor with the APIC ID apicid.
// It never allocates, locks or yields the processor, as the runtime calls
// it with its locks held (see goos.StartPreemption).
TEXT ·preemptRaise(SB),NOSPLIT,$0-8
	MOVQ	apicid+0(FP), BX

	MOVBLZX	·x2apic(SB), CX
	TESTL	CX, CX
	JZ	xapic

	// In x2APIC mode, one write of the interrupt command register, with
	// the APIC ID in its upper half. The stores before the call, such as
	// the preemption request, are visible to the destination before it
	// takes the interrupt (see reg.WriteMSRFenced).
	MFENCE
	LFENCE
	MOVL	$(const_IRQ_PREEMPT), AX
	MOVQ	BX, DX
	MOVL	$(const_x2apicICR), CX
	WRMSR
	RET

xapic:
	// In xAPIC mode, the destination register and then the command
	// register, with interrupts disabled, so that no other IPI of this
	// processor comes between them, after the LAPIC accepted the last
	// IPI of this processor.
	PUSHFQ
	CLI
	MOVL	$(const_LAPIC_ICRL), CX
wait:
	MOVL	(CX), AX
	TESTL	$(const_icrSendPending), AX
	JNZ	wait
	SHLL	$24, BX
	MOVL	$(const_LAPIC_ICRH), CX
	MOVL	BX, (CX)
	MOVL	$(const_IRQ_PREEMPT), AX
	MOVL	$(const_LAPIC_ICRL), CX
	MOVL	AX, (CX)
	POPFQ
	RET
