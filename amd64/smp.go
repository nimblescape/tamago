// AMD64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package amd64

import (
	"runtime"
	"runtime/goos"
	"time"
	"unsafe"

	"github.com/usbarmory/tamago/amd64/lapic"
	"github.com/usbarmory/tamago/internal/reg"
)

// The start code and the start data of the APs lie in the pages from
// 0x4000 to 0x6fff, below 1 MiB, because an AP starts in 16-bit Real
// Mode.
const (
	// ·apinit 16-bit relocation address
	apinitAddress = 0x4000
	// ·apstart pointer address
	apstartAddress = 0x5000

	// AP Global Descriptor Table (GDT) 16-bit address
	gdtAddress = 0x6000
	// AP GDT Descriptor (GDTR) 16-bit address
	gdtrAddress = 0x6018

	// AP task address (see task), its first word counts the APs that are
	// ready before the first task
	taskAddress = 0x6020

	// CR3 value address, 32 bits
	cr3Address = 0x6038

	// slotAddress counts the APs that started. Each AP takes the next
	// slot: it writes its APIC ID into the table at apicTableAddress
	// and uses the start stack of the slot.
	slotAddress = 0x6040

	// stacksAddress holds the address of the start stacks of the slots
	stacksAddress = 0x6048

	// patAddress holds the page attributes (PAT) of the BSP, which each
	// AP applies
	patAddress = 0x6050

	// The far return to Long Mode in ·apinit pushes the same values on
	// every AP, so all APs share the stack that ends here.
	sharedStackAddress = 0x6100

	// APIC ID table, one 32-bit word per slot, which holds noAPIC until
	// the AP of the slot is ready
	apicTableAddress = 0x6100
	noAPIC           = 0xffffffff

	// maxAPs is the number of slots, one for each APIC ID up to
	// maxAPICID. An AP without a slot stays halted.
	maxAPs = 255

	// startStackSize is the size of the start stack of a slot, which
	// holds a return address, the frame of an interrupt that the AP
	// takes while it waits for its task (less than 100 bytes for an
	// event vector) and the frame of the NMI of [CPU.Task].
	startStackShift = 8
	startStackSize  = 1 << startStackShift

	// apSettle is the time without a new AP after which
	// [CPU.StartAPs] considers all APs started, apLimit the
	// longest time that it waits for them and that [CPU.Task] waits
	// until an AP takes its task.
	apSettle = 100 * time.Millisecond
	apLimit  = 1 * time.Second

	// nmiInterval is the time after which [CPU.Task] sends the NMI
	// again while the AP has not taken its task.
	nmiInterval = time.Millisecond
)

// defined in smp.s
func apinit_reloc(init uintptr, start uintptr)

// apTLS holds the thread-local storage (TLS) block of each slot. The AP of
// the slot points its FS base at the block, and the runtime keeps the
// current goroutine (g) there, as it keeps the g of the BSP in m0.tls. An M
// never leaves its AP, so the block of the slot serves its M.
var apTLS [maxAPs]uint64

// task represents a CPU task
type task struct {
	sp uint64 // stack pointer
	gp uint64 // G
	pc uint64 // fn
}

const (
	taskSP = taskAddress + uint64(unsafe.Offsetof(task{}.sp))
	taskGP = taskAddress + uint64(unsafe.Offsetof(task{}.gp))
	taskPC = taskAddress + uint64(unsafe.Offsetof(task{}.pc))
)

// Task schedules a goroutine on a previously initialized Application Processor
// (see [CPU.InitSMP]).
//
// On `GOOS=tamago` Go scheduler M's are never dropped, therefore the function
// is invoked only once per AP (i.e. GOMAXPROCS-1).
//
// The runtime may call Task on several processors at once, and all APs
// share one task structure. So Task holds a lock and returns only when
// the AP has taken the task.
func (cpu *CPU) Task(sp, _, gp, fn unsafe.Pointer) {
	if sp == nil || gp == nil {
		return
	}

	for !cpu.taskLock.CompareAndSwap(false, true) {
	}
	defer cpu.taskLock.Store(false)

	if cpu.init+1 >= runtime.GOMAXPROCS(-1) {
		panic("Task exceeds available resources")
	}

	// the APIC IDs of the APs are 1 to n-1, unless StartAPs found them
	apicid := cpu.init + 1

	if cpu.aps != nil {
		apicid = cpu.aps[cpu.init]
	}

	cpu.init += 1

	// the AP reads pc first, therefore pc is written last
	reg.Write64(taskSP, uint64(uintptr(sp)))
	reg.Write64(taskGP, uint64(uintptr(gp)))
	reg.Write64(taskPC, uint64(uintptr(fn)))

	// Signal the task through NMI until the AP clears pc: an NMI that
	// arrives just before the AP halts does not wake it.
	var sent int64

	for start := cpu.GetTime(); reg.Read64(taskPC) != 0; {
		now := cpu.GetTime()

		if now-start > int64(apLimit) {
			panic("AP does not take its task")
		}

		if sent == 0 || now-sent >= int64(nmiInterval) {
			cpu.LAPIC.IPI(apicid, 0, lapic.ICR_DLV_NMI)
			sent = now
		}
	}
}

// ID returns the processor identifier.
func (cpu *CPU) ID() uint64 {
	return uint64(cpu.LAPIC.ID())
}

// GOMAXPROCS sets the maximum number of CPUs that can be executing
// simultaneously and returns the previous setting, it must be used instead of
// [runtime.GOMAXPROCS] when using this package.
//
// The function checks if n-1 APs are ready (within 1s) at their idle state, if
// the condition fails the current state is not changed.
//
// The function is not required when using [CPU.InitSMP], which invokes it, as
// it is exported for applications that initialize APs by other means.
func (cpu *CPU) GOMAXPROCS(n int) int {
	if cpu.init > 0 {
		return runtime.GOMAXPROCS(n)
	}

	// wait for all APs to reach ·apstart idle state
	if reg.WaitFor(apLimit, taskAddress, 0, 0xffffffff, uint32(n-1)) {
		goos.ProcID = cpu.ID
		goos.Task = cpu.Task
		goos.Wake = cpu.Wake
	} else {
		n = 1
	}

	return runtime.GOMAXPROCS(n)
}

// InitSMP enables Secure Multiprocessor (SMP) operation by initializing
// available Application Processors.
//
// A positive argument caps the total (BSP+APs) number of cores, a negative
// argument initializes all available APs, an agument of 0 or 1 disables SMP.
//
// After initialization [runtime.NumCPU] or [runtime.GOMAXPROCS] can be used to
// verify SMP use by the runtime.
func (cpu *CPU) InitSMP(n int) {
	var i int

	if n == 0 || n == 1 {
		goos.Task = nil
		runtime.GOMAXPROCS(1)
		return
	}

	cpu.prepareAPs()

	for i = 1; i < NumCPU(); i++ {
		if i == n {
			break
		}

		// AMD64 Architecture Programmer’s Manual
		// Volume 2 - 15.27.8 Secure Multiprocessor Initialization
		//
		// AP Startup Sequence:
		// The vector provides the upper 8 bits of a 20-bit physical address.
		vector := apinitAddress >> 12

		cpu.LAPIC.IPI(i, vector, 1<<lapic.ICR_INIT|lapic.ICR_DLV_INIT)
		time.Sleep(10 * time.Millisecond)

		cpu.LAPIC.IPI(i, vector, 1<<lapic.ICR_INIT|lapic.ICR_DLV_SIPI)
	}

	cpu.GOMAXPROCS(i)
}

// StartAPs starts every Application Processor (AP) of a bare metal system
// whose APIC IDs need not be consecutive and which has no firmware table
// that names them. It sends the INIT IPI and twice the Startup IPI to
// all processors but the BSP. Each AP writes its APIC ID into a table
// when it is ready, and StartAPs returns the APIC IDs of the ready APs in
// the order of their arrival. The runtime uses them after [CPU.UseAPs].
//
// The APs use the LAPIC in the mode of the BSP (see [CPU.InitLAPIC]), and
// StartAPs returns an error and starts no AP when the LAPIC of the BSP is
// not in that mode. The package supports APIC IDs up to 254, so an AP with
// a larger APIC ID stays halted.
//
// A ready AP takes interrupts while it waits for the runtime to start a
// thread on it, so that a device may send interrupts to any AP that
// StartAPs returns. The IDT, with its event vectors (see
// [CPU.EnableEvents]), must be complete before the call.
//
// The caller maps the pages from 0x4000 to 0x6fff before the call. The
// page at 0x4000 holds the 16-bit start code of the APs and must be
// writable and executable during the call. The caller may unmap it and
// the page at 0x5000 after the call. The page at 0x6000 must stay mapped
// and writable while the runtime starts tasks on the APs.
func (cpu *CPU) StartAPs() ([]int, error) {
	if err := checkLAPIC(cpu.LAPIC.X2APIC); err != nil {
		return nil, err
	}

	cpu.prepareAPs()

	// AMD64 Architecture Programmer’s Manual
	// Volume 2 - 15.27.8 Secure Multiprocessor Initialization
	vector := apinitAddress >> 12

	cpu.LAPIC.IPI(0, vector, 1<<lapic.ICR_INIT|lapic.ICR_DLV_INIT|lapic.ICR_DST_REST)
	time.Sleep(10 * time.Millisecond)

	// The second Startup IPI starts an AP that missed the first one. An
	// AP that has started ignores it.
	for range 2 {
		cpu.LAPIC.IPI(0, vector, 1<<lapic.ICR_INIT|lapic.ICR_DLV_SIPI|lapic.ICR_DST_REST)
		time.Sleep(200 * time.Microsecond)
	}

	// wait until no new AP is ready for apSettle
	ready := uint32(0)
	last := time.Now()

	for start := last; time.Since(start) < apLimit && time.Since(last) < apSettle; {
		if r := reg.Read(taskAddress); r != ready {
			ready = r
			last = time.Now()
		}

		runtime.Gosched()
	}

	// An AP writes its APIC ID into its slot when it is ready, so a
	// slot without an APIC ID belongs to an AP that did not get ready.
	var aps []int

	for i := range min(reg.Read(slotAddress), maxAPs) {
		if id := reg.Read(apicTableAddress + 4*i); id != noAPIC {
			aps = append(aps, int(id))
		}
	}

	return aps, nil
}

// UseAPs lets the runtime run on the BSP and on the APs with the APIC IDs
// aps, which [CPU.StartAPs] returned. It must be called once. Each AP
// drops its address translations when it takes its first task, so the
// caller may change the mappings of the start code before the call.
func (cpu *CPU) UseAPs(aps []int) {
	if len(aps) == 0 {
		return
	}

	cpu.aps = aps
	goos.ProcID = cpu.ID
	goos.Task = cpu.Task
	goos.Wake = cpu.Wake

	runtime.GOMAXPROCS(len(aps) + 1)
}

// msrPAT is the MSR of the page attributes.
const msrPAT = 0x277

// prepareAPs writes the start code of the APs and its data into the pages
// from 0x4000 to 0x6fff.
func (cpu *CPU) prepareAPs() {
	// copy ·apinit to a 16-bit address reachable in real mode
	// copy ·apstart pointer to avoid RIP/EIP-relative addressing
	apinit_reloc(apinitAddress, apstartAddress)

	// create AP Global Descriptor Table (GDT), with the accessed bit set
	// as in ·gdt (see amd64.s)
	reg.Write64(gdtAddress+0x00, 0x0000000000000000) // null descriptor
	reg.Write64(gdtAddress+0x08, 0x00af9b000000ffff) // code descriptor (x/r, 64-bit, accessed)
	reg.Write64(gdtAddress+0x10, 0x00cf93000000ffff) // data descriptor (r/w, flat, accessed)

	// create AP GDT Descriptor (GDTR)
	reg.Write16(gdtrAddress+0x00, 3*8-1)      // GTD Limit
	reg.Write32(gdtrAddress+0x02, gdtAddress) // GDT Base Address

	// reset SMP semaphore/task
	reg.Write64(taskSP, 0)
	reg.Write64(taskGP, 0)
	reg.Write64(taskPC, 0)

	// read fresh CR3 as a cpuinit override might have overridden ours,
	// the APs load it in 16-bit mode
	cr3 := read_cr3()

	if cr3 >= 1<<32 {
		panic("page tables above 4 GiB")
	}

	reg.Write(cr3Address, uint32(cr3))
	reg.Write64(patAddress, cpu.MSR(msrPAT))

	// the slots and their start stacks
	if cpu.apStacks == nil {
		cpu.apStacks = make([]byte, maxAPs*startStackSize)
	}

	for i := range uint32(maxAPs) {
		reg.Write(apicTableAddress+4*i, noAPIC)
	}

	reg.Write(slotAddress, 0)
	reg.Write64(stacksAddress, uint64(uintptr(unsafe.Pointer(&cpu.apStacks[0]))))
}

// Wake issues a wake interrupt (see [IRQ_WAKEUP]) to the target processor.
func (cpu *CPU) Wake(procid uint64) {
	cpu.LAPIC.IPI(int(procid), IRQ_WAKEUP, lapic.ICR_DLV_IRQ)
}
