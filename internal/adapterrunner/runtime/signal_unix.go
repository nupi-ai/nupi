//go:build !windows

package runtime

import (
	"os"
	"syscall"
)

func syscallSIGTERM() os.Signal {
	return syscall.SIGTERM
}
