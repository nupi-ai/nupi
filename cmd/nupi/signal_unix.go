//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyAttachSignals registers signals relevant for the attach command.
// On Unix this includes SIGINT, SIGTERM, and SIGWINCH (terminal resize).
func notifyAttachSignals(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
}

// isResizeSignal reports whether sig is a terminal resize signal (SIGWINCH).
func isResizeSignal(sig os.Signal) bool {
	return sig == syscall.SIGWINCH
}
