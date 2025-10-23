package version

import "testing"

func TestStringReflectsBuildVersion(t *testing.T) {
	original := String()
	t.Cleanup(func() { version = original })

	version = "1.2.3-test"
	if got := String(); got != "1.2.3-test" {
		t.Fatalf("expected version 1.2.3-test, got %s", got)
	}
}
