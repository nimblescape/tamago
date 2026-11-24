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

// The entries of eventTable are CALL instructions of callSize bytes, so the
// return address of an entry tells handleEvent the vector. The division by
// callSize (5) is a multiplication by 52429 and a shift by 18 bits, which is
// exact for the offsets of the table.
#define EVENT CALL ·handleEvent(SB)
#define EVENT8 EVENT; EVENT; EVENT; EVENT; EVENT; EVENT; EVENT; EVENT

// func eventTableAddress() uintptr
TEXT ·eventTableAddress(SB),NOSPLIT,$0-8
	MOVQ	$·eventTable(SB), AX
	MOVQ	AX, ret+0(FP)
	RET

// handleEvent raises the event of its vector and ends the interrupt.
TEXT ·handleEvent(SB),NOSPLIT|NOFRAME,$0
	// save caller registers
	PUSHQ	AX
	PUSHQ	BX
	PUSHQ	CX
	PUSHQ	DX

	// AMD64 Architecture Programmer’s Manual
	// Volume 2 - 8.9.3 Interrupt Stack Frame

	// the vector from the return address of the entry
	MOVQ	32(SP), AX
	MOVQ	$·eventTable(SB), BX
	SUBQ	BX, AX
	IMULQ	$52429, AX
	SHRQ	$18, AX
	ADDQ	$(const_EventFirst-1), AX

	SUBQ	$8, SP
	MOVL	AX, (SP)
	CALL	runtime∕goos·RaiseEvent(SB)
	ADDQ	$8, SP

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

// eventTable holds one entry for each event vector, from EventFirst to
// EventLast (32 to 223).
TEXT ·eventTable(SB),NOSPLIT|NOFRAME,$0
	EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8 //  32 to  95
	EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8 //  96 to 159
	EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8; EVENT8 // 160 to 223
