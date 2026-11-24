// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

#define MSR_EFER    0xc0000080
#define MSR_FS_BASE 0xc0000100
#define MSR_PAT     0x277

#define PML4T 0x9000	// Page Map Level 4 Table       (512GB entries)
#define PDPT  0xa000	// Page Directory Pointer Table   (1GB entries)
#define PDT   0xb000	// Page Directory Table           (2MB entries)
#define PT    0xc000	// Page Table                     (4kB entries)

// These legacy prefixes are used in 16-bit Real Mode to ensure valid Go
// assembly interpretation.

#define CSADDR BYTE $0x2e	//          CS segment override prefix
#define DATA32 BYTE $0x66	// 32-bit operand size override prefix
#define ADDR32 BYTE $0x67	// 32-bit address size override prefix

// The LAPIC macros access the LAPIC of the processor in the mode of the
// package (see CPU.InitLAPIC and ·x2apic): in xAPIC mode the memory at
// the address LAPIC_x, in x2APIC mode the MSR x2apicX. They need
// go_asm.h, change CX and DX, and count their jumps in instructions, as a
// macro holds no label.

// LAPIC_ID_AX sets AX to the APIC ID of the processor.
#define LAPIC_ID_AX \
	MOVBLZX	·x2apic(SB), AX; \
	TESTL	AX, AX; \
	JNZ	5(PC); \
	MOVL	$(const_LAPIC_ID), AX; \
	MOVL	(AX), AX; \
	SHRL	$(const_ID), AX; \
	JMP	3(PC); \
	MOVL	$(const_x2apicID), CX; \
	RDMSR

// LAPIC_WRITE_AX writes AX to the register at the address addr in xAPIC
// mode, or to the MSR msr in x2APIC mode.
#define LAPIC_WRITE_AX(addr, msr) \
	MOVBLZX	·x2apic(SB), CX; \
	TESTL	CX, CX; \
	JNZ	4(PC); \
	MOVL	$(addr), CX; \
	MOVL	AX, (CX); \
	JMP	4(PC); \
	MOVL	$(msr), CX; \
	XORL	DX, DX; \
	WRMSR

// LAPIC_SEND_EOI ends the interrupt in service. It changes AX.
#define LAPIC_SEND_EOI \
	XORL	AX, AX; \
	LAPIC_WRITE_AX(const_LAPIC_EOI, const_x2apicEOI)
