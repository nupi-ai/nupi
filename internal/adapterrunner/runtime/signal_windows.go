//go:build windows

package runtime

import "os"

func syscallSIGTERM() os.Signal {
	return os.Interrupt
}
