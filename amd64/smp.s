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

// NOTE: this offset needs adjustment in case of any changes to ·apinit
#define doneOffset 0x86
#define doneMarker 0xcccccccccccccccc

// func apinit_reloc(init uintptr, start uintptr)
TEXT ·apinit_reloc(SB),$0-16
	MOVQ	$·apinit<>(SB), SI
	MOVQ	init+0(FP), DI

	// end of function marker
	MOVQ	$doneMarker, BX
copy_8:
	MOVQ	(SI), AX
	ADDQ	$8, SI

	CMPQ	AX, BX
	JE	done

	MOVQ	AX, (DI)
	ADDQ	$8, DI

	JMP	copy_8
done:
	MOVQ	start+8(FP), DI
	MOVQ	$·apstart<>(SB), SI
	MOVQ	SI, (DI)

	RET

// All APs may run ·apinit and ·apstart at the same time (see
// CPU.StartAPs). Therefore they write no shared memory that
// another AP reads during its start, and each AP takes its own start
// stack before it calls a function.
TEXT ·apinit<>(SB),NOSPLIT|NOFRAME,$0
	// disable interrupts
	CLI

	// This function runs in 16-Bit Real Mode, for this reason Go Assembly
	// must be treated differently.
	//
	// The DATA32 and ADDR32 macros (see amd64.h) are used to ensure
	// correct interpretation of 32-bit operands and/or addresses.
	//
	// The function is copied by `apinit_reloc` to a 16-bit address to be
	// called, for this reason RIP/EIP-relative addressing must be avoided.

	// apply fresh CR3 as a cpuinit override might have overridden ours
	DATA32
	MOVL	$(const_cr3Address), AX
	DATA32; ADDR32
	MOVL	(AX), BX

	// apply BSP paging setup (see init.s)
	MOVL	BX, CR3

	// apply the page attributes (PAT) of the BSP, while the caches and
	// paging are still off
	DATA32
	MOVL	$(const_patAddress), BX
	DATA32; ADDR32
	MOVL	(BX), AX
	DATA32; ADDR32
	MOVL	4(BX), DX
	DATA32
	MOVL	$MSR_PAT, CX
	WRMSR

	// The far return to Long Mode below pushes the same values on every
	// AP, so all APs share this stack.
	DATA32
	MOVL	$(const_sharedStackAddress), SP

	// adjust data segment
	BYTE	$0x8c; BYTE	$0xc8		// mov %cs,%ax
	BYTE	$0x8e; BYTE	$0xd8		// mov %eax,%ds

	// transition from 16-bit Real Mode to 64-bit Long Mode

	MOVL	CR4, AX
	DATA32
	ORL	$(1<<7 | 1<<5), AX		// set CR4.(PGE|PAE)
	MOVL	AX, CR4

	DATA32
	MOVL	$MSR_EFER, CX
	RDMSR
	DATA32
	ORL	$(1<<8|1<<11), AX		// set MSR_EFER.(LME|NXE)
	WRMSR

	// INIT leaves CR0.CD and CR0.NW set: the caches are off, and the
	// cache of the AP does not stay coherent with the other processors.
	// So the whole register is written: caches on, and write protection
	// in ring 0 (WP) and native FPU errors (NE) as on the BSP.
	DATA32
	MOVL	$(1<<31 | 1<<16 | 1<<5 | 1<<4 | 1<<1 | 1<<0), AX // CR0.(PG|WP|NE|ET|MP|PE)
	MOVL	AX, CR0

	// set Global Descriptor Table
	DATA32
	MOVL	$(const_gdtrAddress), AX
	DATA32
	SUBL	$(const_apinitAddress), AX	// convert linear address to CS offset
	DATA32; ADDR32; CSADDR
	LGDT	(AX)

	// segment selector for GDT entry 2
	DATA32
	MOVL	$0x10, AX

	DATA32; BYTE	$0x8e; BYTE	$0xc0	// mov %eax,%es
	DATA32; BYTE	$0x8e; BYTE	$0xd0	// mov %eax,%ss
	DATA32; BYTE	$0x8e; BYTE	$0xd8	// mov %eax,%ds
	DATA32; BYTE	$0x8e; BYTE	$0xe0	// mov %eax,%fs
	DATA32; BYTE	$0x8e; BYTE	$0xe8	// mov %eax,%gs

	// set far return target (avoiding RIP/EIP relative addressing)
	DATA32
	MOVL	$(const_apinitAddress+doneOffset), AX

	// jump to target in Long Mode
	PUSHQ	$0x08
	PUSHQ	AX
	RETFQ
done:
	// 64-bit Long Mode

	// jump to ·apstart (avoiding RIP/EIP relative addressing), which
	// never returns
	MOVQ	$(const_apstartAddress), AX
	JMP	(AX)
marker:
	WORD	$doneMarker

TEXT ·apstart<>(SB),NOSPLIT|NOFRAME,$0
	// take the next slot: a start stack and an entry of the APIC ID table
	MOVQ	$(const_slotAddress), BX
	MOVL	$1, R13
	LOCK
	XADDL	R13, 0(BX)
	CMPL	R13, $(const_maxAPs)
	JCC	park

	// use the start stack of the slot, which ends at
	// stacks + (slot+1)*startStackSize
	MOVQ	$(const_stacksAddress), BX
	MOVQ	0(BX), SP
	MOVL	R13, CX
	INCL	CX
	SHLQ	$(const_startStackShift), CX
	ADDQ	CX, SP

	// enable SSE
	CALL	sse_enable(SB)

	// enable extended processor state management to support SIMD
	CALL	xsave_enable(SB)

	// apply BSP GDT
	MOVQ	$·gdtptr(SB), AX
	LGDT	(AX)

	// apply BSP IDT
	MOVQ	$·idtptr(SB), AX
	LIDT	(AX)

	// Use the LAPIC in the mode of the BSP (see CPU.InitLAPIC). An INIT
	// keeps the mode, so the firmware has usually set it already. The
	// LAPIC enters the x2APIC mode only from the xAPIC mode, and leaves
	// it only when it is disabled, so an AP in x2APIC mode stays halted
	// when the BSP uses the xAPIC mode.
	MOVL	$(const_msrAPICBase), CX
	RDMSR
	MOVB	·x2apic(SB), BX
	CMPB	BX, $0
	JNE	x2apic
	TESTL	$(1<<const_apicBaseX2APIC), AX
	JNZ	park
	JMP	ready
x2apic:
	TESTL	$(1<<const_apicBaseX2APIC), AX
	JNZ	ready
	ORL	$(1<<const_apicBaseEnable), AX
	WRMSR
	ORL	$(1<<const_apicBaseX2APIC), AX
	WRMSR
ready:
	// write the APIC ID into the slot of the table, which tells that
	// this AP is ready, unless the package does not support the APIC ID
	LAPIC_ID_AX
	CMPL	AX, $(const_maxAPICID)
	JHI	park
	MOVQ	$(const_apicTableAddress), BX
	MOVL	AX, (BX)(R13*4)

	// enable LAPIC, so that the AP takes interrupts while it waits for
	// its task: the event vectors of the devices may name it. Spurious
	// interrupts take the vector of IRQ_WAKEUP, whose handler only
	// records a wake-up, as on the BSP (see CPU.EnableEvents).
	MOVL	$(1<<const_SVR_ENABLE | const_IRQ_WAKEUP), AX
	LAPIC_WRITE_AX(const_LAPIC_SVR, const_x2apicSVR)

	// use taskAddress as counting semaphore for SMP enabling
	MOVQ	$(const_taskAddress), BX
	MOVL	$1, AX
	LOCK
	XADDL	AX, 0(BX)
wait:
	// wait for the NMI from CPU.Task, and serve interrupts on the start
	// stack meanwhile: the instruction after STI runs before the first
	// interrupt, so no interrupt passes HLT unseen
	STI
	HLT
	CLI

	// Take only the task that names this AP: an interrupt may wake any
	// waiting AP. The APIC ID comes first: CPU.Task writes it before the
	// task and pc last, and it writes no new task before the AP clears
	// pc. So when the APIC ID matches, a pc that is not 0 belongs to the
	// task of this AP.
	LAPIC_ID_AX
	MOVQ	$(const_taskAPICAddress), BX
	CMPL	AX, (BX)
	JNE	wait

	MOVQ	$(const_taskAddress), AX
	MOVQ	task_pc(AX), R12
	CMPQ	R12, $0
	JE	wait

	MOVQ	task_sp(AX), SP
	MOVQ	task_mp(AX), R13

	// clear task, pc last, as CPU.Task waits for it
	MOVQ	$0, task_sp(AX)
	MOVQ	$0, task_mp(AX)
	MOVQ	$0, task_pc(AX)

	// drop the address translations of the start code, which the caller
	// of CPU.StartAPs may have unmapped
	MOVQ	CR3, BX
	MOVQ	BX, CR3

	// call task target with the M in R13 (see CPU.Task)
	STI
	CALL	R12

	// go back to idle state in case we return
	JMP wait

park:
	// an AP without a slot stays halted
	CLI
	HLT
	JMP	park
