// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package amd64 provides support for AMD/Intel 64-bit architecture specific
// operations.
//
// The following architectures/cores are supported/tested:
//   - AMD64 (multiprocessor)
//
// This package is only meant to be used with `GOOS=tamago GOARCH=amd64` as
// supported by the TamaGo framework for bare metal Go, see
// https://github.com/usbarmory/tamago.
package amd64

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"runtime/goos"
	"sync/atomic"
	_ "unsafe"

	"github.com/usbarmory/tamago/amd64/lapic"
	"github.com/usbarmory/tamago/internal/reg"
)

// Peripheral registers
const (
	// Keyboard controller port
	KBD_PORT = 0x64
	// Intel Local Advanced Programmable Interrupt Controller
	LAPIC_BASE = 0xfee00000
)

// aliased constants
const (
	LAPIC_ID   = LAPIC_BASE + lapic.LAPIC_ID
	LAPIC_EOI  = LAPIC_BASE + lapic.LAPIC_EOI
	LAPIC_SVR  = LAPIC_BASE + lapic.LAPIC_SVR
	LAPIC_ICRL = LAPIC_BASE + lapic.LAPIC_ICRL

	ID         = lapic.ID
	SVR_ENABLE = lapic.SVR_ENABLE

	ICR_DST      = lapic.ICR_DST
	ICR_DST_REST = lapic.ICR_DST_REST

	LAPIC_TIMER      = LAPIC_BASE + lapic.LAPIC_LVT_TIMER
	LAPIC_TIMER_INIT = LAPIC_BASE + lapic.LAPIC_LVT_TIMER_INIT
	LAPIC_TIMER_CUR  = LAPIC_BASE + lapic.LAPIC_LVT_TIMER_CURRENT
	LAPIC_TIMER_DIV  = LAPIC_BASE + lapic.LAPIC_LVT_TIMER_DIV

	msrAPICBase    = lapic.MSR_APIC_BASE
	apicBaseEnable = lapic.APIC_BASE_ENABLE
	apicBaseX2APIC = lapic.APIC_BASE_X2APIC
)

// The MSRs of the LAPIC registers that the assembly code uses in x2APIC
// mode (see the LAPIC macros in amd64.h).
const (
	x2apicID  = lapic.MSR_X2APIC + lapic.LAPIC_ID>>4
	x2apicEOI = lapic.MSR_X2APIC + lapic.LAPIC_EOI>>4
	x2apicSVR = lapic.MSR_X2APIC + lapic.LAPIC_SVR>>4
	x2apicICR = lapic.MSR_X2APIC + lapic.LAPIC_ICRL>>4
)

// maxAPICID is the largest APIC ID that the package supports, in both modes
// of the LAPIC: the IRQ_WAKEUP flags (irqWake) and the messages of MSI-X
// interrupts (see lapic.MSI) hold 8-bit APIC IDs, and 255 is the broadcast
// ID of the xAPIC.
const maxAPICID = 254

// x2apic tells the assembly code that the LAPIC is in x2APIC mode (see
// [CPU.InitLAPIC] and the LAPIC macros in amd64.h).
var x2apic bool

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint64 = 0x100000 // 1 MB

// CPU represents the Bootstrap Processor (BSP) instance.
type CPU struct {
	// Timer multiplier
	TimerMultiplier float64
	// Timer offset in nanoseconds
	TimerOffset int64

	// LAPIC represents the Local APIC instance
	LAPIC *lapic.LAPIC

	// ISR represents the interrupt service routine invoked by
	// [CPU.ServiceInterrupts]
	ISR func(int)

	// init represents the last initialized CPU index
	init int
	// aps holds the APIC IDs of the APs that StartAPs found, in the
	// order in which Task starts them, or nil
	aps []int
	// cores holds the APIC IDs of the processors that run the program,
	// this processor first, for the TLB shootdowns (see FlushTLB)
	cores []int
	// apStacks holds the start stacks of the APs (see prepareAPs)
	apStacks []byte
	// taskLock serializes Task
	taskLock atomic.Bool
	// features
	features Features
	// core frequency in Hz
	freq uint64

	// page table allocation state (see ConfigureMMU)
	mmu mmu
}

// defined in amd64.s
func exit(int32)
func halt()

// Fault generates a triple fault.
func Fault()

// DefaultIdleGovernor is the default CPU idle time management function
func (cpu *CPU) DefaultIdleGovernor(pollUntil int64) {
	// we have nothing to do forever
	if pollUntil == math.MaxInt64 {
		cpu.WaitInterrupt()
	}
}

// Init performs initialization of an AMD64 bootstrap processor (BSP) instance
// (see [CPU.InitSMP] for AP initialization).
func (cpu *CPU) Init() {
	goos.Exit = exit
	goos.Idle = cpu.DefaultIdleGovernor

	goos.ProcID = func() uint64 {
		return 0
	}

	// Local APIC
	cpu.LAPIC = &lapic.LAPIC{
		Base: LAPIC_BASE,
	}

	cpu.initFeatures()
	cpu.initTimers()

	// reset SMP semaphore/task
	reg.Write64(taskAddress+0x00, 0)
	reg.Write64(taskAddress+0x08, 0)
	reg.Write64(taskAddress+0x10, 0)
}

// InitLAPIC reads the mode in which the firmware has enabled the LAPIC of the
// BSP and accesses the LAPIC in that mode from then on: in xAPIC mode
// through the memory at [LAPIC_BASE], in x2APIC mode through MSRs.
// Firmware may choose the x2APIC mode also on a system with fewer than 256
// processors, and a processor leaves the x2APIC mode only by disabling its
// LAPIC. So a program on bare metal calls InitLAPIC after [CPU.Init] and
// before it uses interrupts. Without InitLAPIC, the package uses the xAPIC
// mode, as in a virtual machine, where the read of the mode may trap.
//
// InitLAPIC returns an error and keeps the xAPIC mode when the LAPIC is
// disabled, when it is in xAPIC mode at another address than LAPIC_BASE, or
// when the APIC ID of the BSP is above 254.
func (cpu *CPU) InitLAPIC() error {
	_, x2, _ := lapic.Mode()

	if err := checkLAPIC(x2); err != nil {
		return err
	}

	l := &lapic.LAPIC{Base: LAPIC_BASE, X2APIC: x2}

	if id := l.ID(); id > maxAPICID {
		return fmt.Errorf("the APIC ID %d of the BSP is above %d", id, maxAPICID)
	}

	cpu.LAPIC = l
	x2apic = x2

	return nil
}

// checkLAPIC returns an error unless the LAPIC of the calling processor is
// enabled in x2APIC mode when x2 is true, else in xAPIC mode at
// [LAPIC_BASE].
func checkLAPIC(x2 bool) error {
	enabled, mode, base := lapic.Mode()

	switch {
	case !enabled:
		return errors.New("the LAPIC is disabled")
	case mode && !x2:
		return errors.New("the LAPIC is in x2APIC mode, and the package uses the xAPIC mode (see CPU.InitLAPIC)")
	case !mode && x2:
		return errors.New("the LAPIC is in xAPIC mode, and the package uses the x2APIC mode")
	case !mode && base != LAPIC_BASE:
		return fmt.Errorf("the LAPIC is at %#x, not at %#x", base, LAPIC_BASE)
	}

	return nil
}

// Name returns the CPU identifier.
func (cpu *CPU) Name() string {
	return runtime.CPU()
}

// Reset resets the CPU pin via 8042 keyboard controller pulse.
func (cpu *CPU) Reset() {
	reg.Out8(KBD_PORT, 0xfe)
}
