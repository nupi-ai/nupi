package tlswarn

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
)

// TestLogInsecureOnce must NOT use t.Parallel() because it mutates global
// state (sync.Once and log output).
func TestLogInsecureOnce(t *testing.T) {
	// Reset the package-level Once so this test is self-contained.
	once = sync.Once{}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	// Call multiple times — only one warning should be emitted.
	LogInsecure()
	LogInsecure()
	LogInsecure()

	output := buf.String()
	count := strings.Count(output, "[TLS] WARNING:")
	if count != 1 {
		t.Fatalf("expected exactly 1 warning, got %d; output:\n%s", count, output)
	}

	if !strings.Contains(output, "certificate and hostname verification is disabled") {
		t.Fatalf("warning missing expected text; output:\n%s", output)
	}
}
