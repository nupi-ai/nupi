//go:build !windows

package procutil

import (
	"os"
	"syscall"
)

// GracefulTerminate sends SIGTERM to the process for graceful shutdown.
func GracefulTerminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// TerminateByPID sends SIGTERM to the process identified by pid.
func TerminateByPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// IsProcessAlive checks whether a process with the given pid is still running.
func IsProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
