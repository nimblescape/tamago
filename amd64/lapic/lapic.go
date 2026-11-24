// Intel Advanced Programmable Interrupt Controller (APIC) driver
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package lapic implements a driver for the Intel Local (LAPIC) Advanced
// Programmable Interrupt Controllers adopting the following reference
// specifications:
//   - Intel® 64 and IA-32 Architectures Software Developer’s Manual - Volume 3A - Chapter 10
//   - Intel® 64 and IA-32 Architectures Software Developer’s Manual - Volume 3A - 11.12 Extended XAPIC (x2APIC)
//
// The driver supports both modes of the LAPIC: the xAPIC mode, in which the
// registers are memory at the base address, and the x2APIC mode, in which
// the registers are Model Specific Registers (MSRs) and the APIC IDs have 32
// bits. The firmware chooses the mode (see [Mode]).
//
// This package is only meant to be used with `GOOS=tamago` as
// supported by the TamaGo framework for bare metal Go, see
// https://github.com/usbarmory/tamago.
package lapic

import (
	"github.com/usbarmory/tamago/bits"
	"github.com/usbarmory/tamago/internal/reg"
)

// LAPIC registers
const (
	LAPIC_ID = 0x20
	ID       = 24

	LAPIC_VER   = 0x30
	VER_ENTRIES = 16

	LAPIC_EOI = 0xb0

	LAPIC_SVR  = 0xf0
	SVR_ENABLE = 8

	LAPIC_ICRL = 0x300
	LAPIC_ICRH = 0x310

	ICR_DST      = 18
	ICR_DST_SELF = 0b01 << ICR_DST
	ICR_DST_ALL  = 0b10 << ICR_DST
	ICR_DST_REST = 0b11 << ICR_DST

	ICR_INIT       = 14
	ICR_DLV_STATUS = 12
	ICR_DLV        = 8

	ICR_DLV_SIPI = 0b110 << ICR_DLV
	ICR_DLV_INIT = 0b101 << ICR_DLV
	ICR_DLV_NMI  = 0b100 << ICR_DLV
	ICR_DLV_SMI  = 0b010 << ICR_DLV
	ICR_DLV_LOW  = 0b001 << ICR_DLV
	ICR_DLV_IRQ  = 0b000 << ICR_DLV

	LAPIC_LVT_TIMER = 0x320
	TIMER_MODE      = 17
	TIMER_IRQ       = 0

	TIMER_MODE_ONE_SHOT     = 0b00
	TIMER_MODE_PERIODIC     = 0b01
	TIMER_MODE_TSC_DEADLINE = 0b10

	LAPIC_LVT_TIMER_INIT    = 0x380
	LAPIC_LVT_TIMER_CURRENT = 0x390
	LAPIC_LVT_TIMER_DIV     = 0x3e0
)

// The MSR of the base address and the mode of the LAPIC, and the MSRs of the
// registers in x2APIC mode: the register at offset off of the xAPIC mode is
// the MSR MSR_X2APIC + off>>4.
const (
	MSR_APIC_BASE    = 0x1b
	APIC_BASE_ENABLE = 11
	APIC_BASE_X2APIC = 10
	APIC_BASE_ADDR   = 0x000ffffffffff000

	MSR_X2APIC = 0x800
)

// Mode reads the mode of the LAPIC of the calling processor from its base
// address MSR: whether the LAPIC is enabled, whether it is in x2APIC mode
// and, in xAPIC mode, the base address of its registers.
func Mode() (enabled bool, x2apic bool, base uint64) {
	msr := reg.ReadMSR(MSR_APIC_BASE)
	return msr&(1<<APIC_BASE_ENABLE) != 0, msr&(1<<APIC_BASE_X2APIC) != 0, msr & APIC_BASE_ADDR
}

// LAPIC represents a Local APIC instance.
type LAPIC struct {
	// Base register
	Base uint32
	// X2APIC selects the x2APIC mode: the registers are MSRs and the
	// APIC IDs have 32 bits. Without it, the registers are memory at
	// Base (xAPIC mode).
	X2APIC bool
}

// Read returns the register at offset off of the xAPIC mode, such as
// LAPIC_SVR, in the mode of the LAPIC.
func (io *LAPIC) Read(off uint32) uint32 {
	if io.X2APIC {
		return uint32(reg.ReadMSR(MSR_X2APIC + uint64(off>>4)))
	}

	return reg.Read(io.Base + off)
}

// Write writes the register at offset off of the xAPIC mode, such as
// LAPIC_SVR, in the mode of the LAPIC.
func (io *LAPIC) Write(off uint32, val uint32) {
	if io.X2APIC {
		reg.WriteMSR(MSR_X2APIC+uint64(off>>4), uint64(val))
		return
	}

	// no reg.Set as we do not emulate atomic MMIO ops under SEV-SNP
	reg.Write(io.Base+off, val)
}

// ID returns the APIC ID of the calling processor: 8 bits in xAPIC mode,
// 32 bits in x2APIC mode.
func (io *LAPIC) ID() uint32 {
	if io.X2APIC {
		return io.Read(LAPIC_ID)
	}

	return io.Read(LAPIC_ID) >> ID & 0xff
}

// Version returns the LAPIC version register.
func (io *LAPIC) Version() uint32 {
	return io.Read(LAPIC_VER)
}

// Entries returns the size of the LAPIC local vector table.
func (io *LAPIC) Entries() int {
	maxIndex := io.Read(LAPIC_VER) >> VER_ENTRIES & 0xff
	return int(maxIndex) + 1
}

// Enable enables the Local APIC.
func (io *LAPIC) Enable() {
	svr := io.Read(LAPIC_SVR)
	bits.Set(&svr, SVR_ENABLE)
	io.Write(LAPIC_SVR, svr)
}

// Disable disables the Local APIC.
func (io *LAPIC) Disable() {
	svr := io.Read(LAPIC_SVR)
	bits.Clear(&svr, SVR_ENABLE)
	io.Write(LAPIC_SVR, svr)
}

// ClearInterrupt signals the end of an interrupt handling routine.
func (io *LAPIC) ClearInterrupt() {
	io.Write(LAPIC_EOI, 0)
}

// IPI sends an Inter-Processor Interrupt (IPI). In x2APIC mode, the
// interrupt command register is one 64-bit MSR with the 32-bit APIC ID of
// the destination in its upper half, and it has no delivery status. The
// write waits for the earlier stores, so that the destination sees them
// when it takes the interrupt, as in xAPIC mode.
func (io *LAPIC) IPI(apicid int, id int, flags int) {
	low := uint32(flags&0xffffff00) | uint32(id&0xff)

	if io.X2APIC {
		reg.WriteMSRFenced(MSR_X2APIC+LAPIC_ICRL>>4, uint64(uint32(apicid))<<32|uint64(low))
		return
	}

	icrh := reg.Read(io.Base + LAPIC_ICRH)
	bits.SetN(&icrh, ID, 0xff, uint32(apicid))

	// no reg.Set as we do not emulate atomic MMIO ops under SEV-SNP
	reg.Write(io.Base+LAPIC_ICRH, icrh)
	reg.Write(io.Base+LAPIC_ICRL, low)
	reg.Wait(io.Base+LAPIC_ICRL, ICR_DLV_STATUS, 1, 0)
}

// SetTimer configures the LAPIC LVT Timer with the argument vector and mode.
func (io *LAPIC) SetTimer(id int, mode int) {
	var val uint32

	bits.SetN(&val, TIMER_IRQ, 0xff, uint32(id))
	bits.SetN(&val, TIMER_MODE, 0b11, uint32(mode))

	io.Write(LAPIC_LVT_TIMER, val)
}

// MSI message (Intel® 64 and IA-32 Architectures Software Developer’s Manual
// Volume 3A - 11.11 Message Signalled Interrupts)
const (
	MSI_ADDRESS = 0xfee00000
	MSI_DEST    = 12
)

// MSI returns the message address and data of an MSI or MSI-X interrupt with
// the argument vector to the processor with the argument APIC ID: fixed
// delivery, edge triggered, physical destination mode. The address holds 8
// bits of the APIC ID, in x2APIC mode too, so the processor needs an APIC
// ID below 255.
func MSI(apicid int, vector int) (addr uint64, data uint32) {
	return MSI_ADDRESS | uint64(apicid&0xff)<<MSI_DEST, uint32(vector & 0xff)
}
