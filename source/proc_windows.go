//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	createNewProcessGroup          = 0x00000200
	detachedProcess                = 0x00000008
	createNoWindow                 = 0x08000000
	processTerminate               = 0x0001
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

var (
	kernel32Proc           = syscall.NewLazyDLL("kernel32.dll")
	openProcessProc        = kernel32Proc.NewProc("OpenProcess")
	getExitCodeProcessProc = kernel32Proc.NewProc("GetExitCodeProcess")
	terminateProcessProc   = kernel32Proc.NewProc("TerminateProcess")
	closeHandleProc        = kernel32Proc.NewProc("CloseHandle")
)

func configureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
		HideWindow:    true,
	}
}

// processAlive uses the Win32 process API directly. In particular, it does not
// spawn tasklist.exe, which would create a transient conhost window when the
// detached relay polls the Discord PID.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	h, _, _ := openProcessProc.Call(
		processQueryLimitedInformation,
		0,
		uintptr(uint32(pid)),
	)
	if h == 0 {
		return false
	}
	defer closeHandleProc.Call(h)

	var exitCode uint32
	ok, _, _ := getExitCodeProcessProc.Call(
		h,
		uintptr(unsafe.Pointer(&exitCode)),
	)
	return ok != 0 && exitCode == stillActive
}

func terminateProcess(pid int) {
	if pid <= 0 {
		return
	}

	h, _, _ := openProcessProc.Call(
		processTerminate,
		0,
		uintptr(uint32(pid)),
	)
	if h == 0 {
		return
	}
	defer closeHandleProc.Call(h)
	terminateProcessProc.Call(h, 1)
}

func runHiddenCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
		HideWindow:    true,
	}
	return cmd.Run()
}
