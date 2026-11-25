// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package amd64

import (
	"errors"
	"fmt"
	"runtime/goos"
	"time"

	"github.com/usbarmory/tamago/amd64/lapic"
)

// IRQ_PREEMPT represents the interrupt vector of preemption (see
// [CPU.EnablePreemption]). The LAPIC timer of each processor raises it
// every [PreemptPeriod], and the runtime raises it on the processor of a
// goroutine that it wants to stop.
const IRQ_PREEMPT = 252 // priority 15

// PreemptPeriod is the period of the LAPIC timer of each processor while
// preemption is on. The runtime preempts a goroutine that has run for 10 ms,
// so a goroutine runs for at most two periods without a check.
const PreemptPeriod = 10 * time.Millisecond

// preemptStackUse is the number of bytes below the stack pointer of the
// interrupted code that IRQ_PREEMPT uses: the frame of the interrupt with
// its alignment (48 bytes), and then the larger of the three registers
// that preemptGate saves and the four that the handler of the runtime
// saves before it switches stacks (32 bytes).
const preemptStackUse = 80

// The LVT timer register of the LAPIC for IRQ_PREEMPT, in TSC deadline
// mode and in one-shot mode, the divide configuration register for a
// divisor of 1, and the MSRs of the timer registers in x2APIC mode, for
// preemptGate.
const (
	preemptDeadlineTimer = lapic.TIMER_MODE_TSC_DEADLINE<<lapic.TIMER_MODE | IRQ_PREEMPT
	preemptCountTimer    = lapic.TIMER_MODE_ONE_SHOT<<lapic.TIMER_MODE | IRQ_PREEMPT
	timerDivideBy1       = 0b1011

	x2apicTimer     = lapic.MSR_X2APIC + lapic.LAPIC_LVT_TIMER>>4
	x2apicTimerInit = lapic.MSR_X2APIC + lapic.LAPIC_LVT_TIMER_INIT>>4
	x2apicTimerDiv  = lapic.MSR_X2APIC + lapic.LAPIC_LVT_TIMER_DIV>>4
)

// preemptCalibration is the time over which EnablePreemption measures the
// rate of the LAPIC timer when the LAPIC has no TSC deadline mode.
const preemptCalibration = time.Millisecond

var (
	// preemptTicks holds PreemptPeriod in ticks of the timer that
	// preemptGate sets: of the TSC in TSC deadline mode, else of the
	// LAPIC timer. It is not zero while preemption is on.
	preemptTicks uint64

	// preemptDeadline tells preemptGate to use the TSC deadline mode of
	// the LAPIC timer, else its one-shot mode.
	preemptDeadline bool
)

// defined in preempt.s
func preemptGateAddress() uintptr
func preemptRaise(apicid uint64)

// EnablePreemption turns on the preemption of goroutines, as on Linux: a
// goroutine that has run for 10 ms stops at its next function call, and the
// runtime moves a goroutine that makes no call to a safe point (see
// goos.StartPreemption). Long computations then need no runtime.Gosched,
// and a loop without calls no longer holds its processor or stops the
// garbage collector.
//
// The LAPIC timer of each processor raises [IRQ_PREEMPT] every
// [PreemptPeriod], from the first IRQ_PREEMPT on that processor. The
// runtime raises the first one on the calling processor at once, and on
// the processor of every later M when the M starts. So EnablePreemption
// must be called before the APs run the program (see [CPU.UseAPs] and
// [CPU.InitSMP]), after [CPU.InitLAPIC] and [CPU.EnableExceptions], and
// before the IDT is write protected.
//
// The timer runs in TSC deadline mode where the LAPIC has it, as on every
// current server processor. Otherwise, as in QEMU without KVM, it runs in
// one-shot mode, at the rate that EnablePreemption measures against the
// TSC on the calling processor.
//
// Preemption takes the LAPIC timer of every processor: while it is on,
// [CPU.SetAlarm] sets no alarm, and a halted processor wakes at the next
// IRQ_PREEMPT at the latest.
func (cpu *CPU) EnablePreemption() error {
	switch {
	case preemptTicks != 0:
		return errors.New("preemption is on already")
	case cpu.init > 0:
		return errors.New("the APs run the program already")
	case idtAddr == 0:
		return errors.New("the IDT is not loaded (see EnableExceptions)")
	case cpu.freq <= 1 || cpu.TimerMultiplier <= 0:
		return errors.New("the rate of the TSC is unknown")
	case goos.StackSystem < preemptStackUse:
		return errors.New("goos.StackSystem is too small for interrupts on the stacks of goroutines")
	}

	if cpu.features.TSCDeadline {
		preemptDeadline = true
		preemptTicks = uint64(float64(PreemptPeriod) / cpu.TimerMultiplier)
	} else {
		ticks, err := cpu.lapicTimerTicks(PreemptPeriod)

		if err != nil {
			return err
		}

		preemptTicks = ticks
	}

	setGates(IRQ_PREEMPT, IRQ_PREEMPT, preemptGateAddress()-IRQ_PREEMPT*callSize)

	goos.ProcID = cpu.ID
	goos.StartPreemption(preemptRaise)

	return nil
}

// lapicTimerTicks measures the rate of the LAPIC timer of the calling
// processor against the TSC, with a divisor of 1, and returns the ticks of
// the timer in d.
func (cpu *CPU) lapicTimerTicks(d time.Duration) (uint64, error) {
	l := cpu.LAPIC

	// one-shot mode with the interrupt masked
	l.Write(lapic.LAPIC_LVT_TIMER, 1<<16|preemptCountTimer)
	l.Write(lapic.LAPIC_LVT_TIMER_DIV, timerDivideBy1)
	l.Write(lapic.LAPIC_LVT_TIMER_INIT, 0xffffffff)

	start := cpu.GetTime()

	for cpu.GetTime()-start < int64(preemptCalibration) {
	}

	elapsed := 0xffffffff - uint64(l.Read(lapic.LAPIC_LVT_TIMER_CURRENT))
	took := cpu.GetTime() - start
	l.Write(lapic.LAPIC_LVT_TIMER_INIT, 0)

	ticks := elapsed * uint64(d) / uint64(took)

	if ticks == 0 || ticks > 0xffffffff {
		return 0, fmt.Errorf("the LAPIC timer counts %d ticks in %v", ticks, d)
	}

	return ticks, nil
}
