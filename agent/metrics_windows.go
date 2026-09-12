//go:build windows

//
//  File:      metrics_windows.go
//  Created:   2026-09-11
//  Updated:   2026-09-11
//  Developer: Kennt Kim / Calida Lab
//  Overview:  Windows CPU, memory, and machine identity readers for the fleet agent.
//  Notes:     Uses kernel32 system times and memory status plus registry identity.
//             CPU usage is a 200ms delta. Windows APIs are called without CGO.
//
package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const machineKind = "windows"

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes          = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx    = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetLogicalDriveStringsW = kernel32.NewProc("GetLogicalDriveStringsW")
	procGetDriveTypeW           = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceExW     = kernel32.NewProc("GetDiskFreeSpaceExW")
)

const driveFixed = 3 // DRIVE_FIXED — a non-removable local disk (excludes removable/network/CD-ROM)

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// MARK: - CPU

func readCPU() CPU {
	c := CPU{Cores: runtime.NumCPU(), LoadAvg1: loadAvg1()}
	i1, t1, ok1 := systemTimes()
	time.Sleep(200 * time.Millisecond)
	i2, t2, ok2 := systemTimes()
	if totalDelta := t2 - t1; ok1 && ok2 && totalDelta > 0 {
		busyDelta := totalDelta - (i2 - i1)
		c.UsagePercent = round1(100 * float64(busyDelta) / float64(totalDelta))
	}
	return c
}

func systemTimes() (idle, total uint64, ok bool) {
	var idleTime, kernelTime, userTime windows.Filetime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleTime)),
		uintptr(unsafe.Pointer(&kernelTime)),
		uintptr(unsafe.Pointer(&userTime)),
	)
	if r == 0 {
		return 0, 0, false
	}
	idle = uint64(idleTime.HighDateTime)<<32 | uint64(idleTime.LowDateTime)
	kernel := uint64(kernelTime.HighDateTime)<<32 | uint64(kernelTime.LowDateTime)
	user := uint64(userTime.HighDateTime)<<32 | uint64(userTime.LowDateTime)
	return idle, kernel + user, true
}

func loadAvg1() float64 {
	// Windows has no load average. A measured zero differs from a missing measurement,
	// so keep the field's neutral zero rather than inventing a value.
	return 0
}

// MARK: - Memory

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readMemory() Memory {
	var s memoryStatusEx
	s.Length = uint32(unsafe.Sizeof(s))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&s)))
	if r == 0 {
		return Memory{}
	}
	m := Memory{TotalBytes: int64(s.TotalPhys), AvailableBytes: int64(s.AvailPhys)}
	m.UsedBytes = m.TotalBytes - m.AvailableBytes
	return m
}

// MARK: - Disks

// readDisks enumerates logical drives, keeps only fixed disks (GetDriveTypeW == DRIVE_FIXED), and
// reads each one's capacity. Free uses the caller-available bytes (GetDiskFreeSpaceExW's
// lpFreeBytesAvailableToCaller), the Windows analogue of statfs Bavail, so quota-restricted space
// isn't counted as free. FSType is left unset (no cheap CGO-free call); it's omitempty on the wire.
func readDisks() []Disk {
	buf := make([]uint16, 256)
	n, _, _ := procGetLogicalDriveStringsW.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if n == 0 {
		return []Disk{} // never nil — a nil slice marshals to `null` and breaks the viewer (#33)
	}
	disks := []Disk{}
	for _, root := range splitDriveStrings(buf[:n]) {
		rootPtr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if t, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(rootPtr))); t != driveFixed {
			continue
		}
		var freeToCaller, total, totalFree uint64
		r, _, _ := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(rootPtr)),
			uintptr(unsafe.Pointer(&freeToCaller)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)),
		)
		if r == 0 {
			continue
		}
		disks = append(disks, Disk{
			Mount:      strings.TrimSuffix(root, `\`), // "C:\" -> "C:"
			TotalBytes: int64(total),
			FreeBytes:  int64(freeToCaller),
		})
	}
	return topDisks(disks)
}

// splitDriveStrings decodes the NUL-separated, double-NUL-terminated buffer GetLogicalDriveStringsW
// fills ("C:\\\x00D:\\\x00\x00") into root-path strings.
func splitDriveStrings(buf []uint16) []string {
	var roots []string
	start := 0
	for i, c := range buf {
		if c == 0 {
			if i > start {
				roots = append(roots, windows.UTF16ToString(buf[start:i]))
			}
			start = i + 1
		}
	}
	return roots
}

// MARK: - small helpers

func machineID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if id, _, err := k.GetStringValue("MachineGuid"); err == nil {
			if id = strings.TrimSpace(id); id != "" {
				return id
			}
		}
	}
	h, _ := os.Hostname()
	return h
}

func osPrettyName() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return runtime.GOOS
	}
	defer k.Close()
	product, _, err := k.GetStringValue("ProductName")
	product = strings.TrimSpace(product)
	if err != nil || product == "" {
		return runtime.GOOS
	}
	buildString, _, err := k.GetStringValue("CurrentBuild")
	if err == nil {
		if build, err := strconv.Atoi(strings.TrimSpace(buildString)); err == nil && build >= 22000 && strings.Contains(product, "Windows 10") {
			// Microsoft froze ProductName at Windows 10 on Windows 11 hosts. Build 22000
			// is the Windows 11 threshold, keeping os honest without shelling out to WMI.
			product = strings.ReplaceAll(product, "Windows 10", "Windows 11")
		}
	}
	return product
}
