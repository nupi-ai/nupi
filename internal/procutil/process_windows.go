//go:build windows

package procutil

import (
	"fmt"
	"os"
	"syscall"
)

const processQueryLimitedInformation = 0x1000

// GracefulTerminate terminates the process. On Windows, Process.Signal only
// supports os.Kill, so we use that directly (TerminateProcess).
func GracefulTerminate(p *os.Process) error {
	return p.Kill()
}

// TerminateByPID terminates the process identified by pid.
func TerminateByPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()
	return p.Kill()
}

// IsProcessAlive checks whether a process with the given pid is still running
// by attempting to open a handle with PROCESS_QUERY_LIMITED_INFORMATION.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	syscall.CloseHandle(h)
	return true
}
