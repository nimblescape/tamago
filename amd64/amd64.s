// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

#include "go_asm.h"
#include "textflag.h"

// Global Descriptor Table
//
// The accessed bit is set in each descriptor. A processor that loads a
// segment whose descriptor has the bit clear writes the bit into the table,
// also when it loads CS for an interrupt. The table lies in read-only data,
// which a program may map read-only: the write would then fault during the
// delivery of every interrupt, and the processor would reset (triple
// fault). The APs load the GDT of the BSP: this table, or the GDT of a
// cpuinit override, whose register load_idt saves in ·gdtptr.
DATA	·gdt+0x00(SB)/8, $0x0000000000000000	// null descriptor
DATA	·gdt+0x08(SB)/8, $0x00209b0000000000	// code descriptor (x/r, accessed)
DATA	·gdt+0x10(SB)/8, $0x0000930000000000	// data descriptor (r/w, accessed)
GLOBL	·gdt(SB),RODATA,$(3*8)

// Global Descriptor Table Register
DATA	·gdtptr+0x00(SB)/2, $(3*8-1)		// GDT Limit
DATA	·gdtptr+0x02(SB)/8, $·gdt(SB)		// GDT Base Address
GLOBL	·gdtptr(SB),RODATA,$(2+8)

// Interrupt Descriptor Table
GLOBL	·idt(SB),RODATA,$(const_vectors*16)

// Interrupt Descriptor Table Register
DATA	·idtptr+0x00(SB)/2, $(const_vectors*16-1)	// IDT Limit
DATA	·idtptr+0x02(SB)/8, $·idt(SB)			// IDT Base Address
GLOBL	·idtptr(SB),RODATA,$(2+8)

// func Fault()
TEXT ·Fault(SB),$0
	CLI

	// invalidate IDT
	MOVQ	$·idtptr(SB), AX
	MOVQ	$0, (AX)
	LIDT	(AX)

	// triple-fault
	CALL	$0
halt:
	HLT
	JMP halt

// func exit(int32)
TEXT ·exit(SB),$0-4
	CLI
halt:
	HLT
	JMP halt

// func halt()
TEXT ·halt(SB),$0
	HLT
	RET
