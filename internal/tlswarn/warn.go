// Package tlswarn provides a process-wide one-shot warning for insecure TLS usage.
package tlswarn

import (
	"log"
	"sync"
)

var once sync.Once

// LogInsecure emits a single warning to stderr (via log.Print) the first time
// it is called. Subsequent calls are no-ops. This prevents log spam when
// multiple packages detect insecure TLS configuration in the same process.
func LogInsecure() {
	once.Do(func() {
		log.Print("[TLS] WARNING: TLS certificate and hostname verification is disabled. Do NOT use in production.")
	})
}
