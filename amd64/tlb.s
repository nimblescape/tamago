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

// func tlbBroadcast() (apicid uint32)
//
// tlbBroadcast drops the address translations of the calling processor,
// sends IRQ_TLB to all other processors and returns the APIC ID of the
// calling processor. It runs with interrupts disabled, so that it does all
// three on the same processor.
TEXT ·tlbBroadcast(SB),NOSPLIT,$0-4
	PUSHFQ
	CLI

	// drop the translations of this processor
	MOVQ	CR3, AX
	MOVQ	AX, CR3

	// the stores before the call are visible to the other processors
	// before they take the interrupt (see reg.WriteMSRFenced)
	MFENCE
	LFENCE

	// in xAPIC mode, wait until the LAPIC accepted the last IPI of this
	// processor, without yielding the processor as LAPIC.IPI does
	MOVBLZX	·x2apic(SB), CX
	TESTL	CX, CX
	JNZ	send
	MOVL	$(const_LAPIC_ICRL), CX
wait:
	MOVL	(CX), AX
	TESTL	$(const_icrSendPending), AX
	JNZ	wait
send:
	MOVL	$(const_ICR_DST_REST|const_IRQ_TLB), AX
	LAPIC_WRITE_AX(const_LAPIC_ICRL, const_x2apicICR)

	LAPIC_ID_AX
	MOVL	AX, BX

	POPFQ
	MOVL	BX, apicid+0(FP)
	RET

// tlbInterrupt is the handler of IRQ_TLB: it drops the address translations
// of this processor and reports the generation of the newest TLB shootdown
// (see CPU.FlushTLB).
TEXT ·tlbInterrupt(SB),NOSPLIT|NOFRAME,$0
	// save caller registers
	PUSHQ	AX
	PUSHQ	BX
	PUSHQ	CX
	PUSHQ	DX

	// Read the generation before the flush: the write of CR3 serializes
	// the processor, so the flush comes after every change that the
	// generation covers.
	MOVQ	·tlbGeneration(SB), BX

	// drop the translations of this processor
	MOVQ	CR3, AX
	MOVQ	AX, CR3

	// report the generation in the slot of this processor, whose APIC ID
	// is at most maxAPICID (see CPU.InitLAPIC and CPU.StartAPs)
	LAPIC_ID_AX
	LEAQ	·tlbDone(SB), CX
	MOVQ	BX, (CX)(AX*8)

	// clear interrupt
	LAPIC_SEND_EOI

	// restore caller registers
	POPQ	DX
	POPQ	CX
	POPQ	BX
	POPQ	AX

	// return to caller
	ADDQ	$8, SP
	IRETQ
