//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func configureDetached(cmd *exec.Cmd) {
	const CREATE_NEW_PROCESS_GROUP = 0x00000200
	const DETACHED_PROCESS = 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS,
		HideWindow:    true,
	}
}

func processAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(out))
	return s != "" && !strings.HasPrefix(s, "INFO:") && strings.Contains(s, strconv.Itoa(pid))
}

func terminateProcess(pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F", "/T").Run()
}
