package awareness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewService(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if svc.instanceDir != dir {
		t.Errorf("instanceDir = %q, want %q", svc.instanceDir, dir)
	}
	if svc.awarenessDir != filepath.Join(dir, "awareness") {
		t.Errorf("awarenessDir = %q, want %q", svc.awarenessDir, filepath.Join(dir, "awareness"))
	}
}

func TestStartCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	expected := []string{
		filepath.Join(dir, "awareness"),
		filepath.Join(dir, "awareness", "memory"),
		filepath.Join(dir, "awareness", "memory", "daily"),
		filepath.Join(dir, "awareness", "memory", "topics"),
		filepath.Join(dir, "awareness", "memory", "projects"),
	}

	for _, d := range expected {
		info, err := os.Stat(d)
		if err != nil {
			t.Errorf("directory %s not created: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}
}

func TestStartWithEmptyAwarenessDir(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	// No core memory files → empty string
	if got := svc.CoreMemory(); got != "" {
		t.Errorf("CoreMemory() = %q, want empty string", got)
	}
}

func TestShutdownWithTimeout(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := svc.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}
}

func TestShutdownWithoutStart(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// Shutdown before Start should not panic or error
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() without Start error = %v", err)
	}
}
