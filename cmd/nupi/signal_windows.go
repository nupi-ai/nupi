//go:build windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyAttachSignals registers signals relevant for the attach command.
// On Windows there is no SIGWINCH equivalent; only SIGINT and SIGTERM are registered.
func notifyAttachSignals(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
}

// isResizeSignal reports whether sig is a terminal resize signal.
// Windows does not have SIGWINCH, so this always returns false.
func isResizeSignal(_ os.Signal) bool {
	return false
}
