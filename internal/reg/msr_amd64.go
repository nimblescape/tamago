// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package reg

// defined in msr_amd64.s
func ReadMSR(addr uint64) (val uint64)
func WriteMSR(addr uint64, val uint64)

// WriteMSRFenced writes the MSR at addr after all earlier loads and stores
// of the processor are complete. A write to an MSR of the x2APIC, such as
// its interrupt command register, does not wait for them by itself (Intel®
// 64 and IA-32 Architectures Software Developer’s Manual - Volume 3A -
// 11.12.3 MSR Access in x2APIC Mode).
//
// defined in msr_amd64.s
func WriteMSRFenced(addr uint64, val uint64)
