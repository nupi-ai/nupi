package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

type serviceTracker struct {
	name          string
	startErr      error
	shutdownErr   error
	errCh         chan error
	mu            sync.Mutex
	startCount    int
	shutdownCount int
}

func (tr *serviceTracker) factory(recordStarts, recordStops *[]string, recordMu *sync.Mutex) ServiceFactory {
	return func(ctx context.Context) (Service, error) {
		return &trackedService{
			tracker:      tr,
			recordStarts: recordStarts,
			recordStops:  recordStops,
			recordMu:     recordMu,
		}, nil
	}
}

type trackedService struct {
	tracker      *serviceTracker
	recordStarts *[]string
	recordStops  *[]string
	recordMu     *sync.Mutex
}

func (s *trackedService) Start(ctx context.Context) error {
	s.tracker.mu.Lock()
	s.tracker.startCount++
	s.tracker.mu.Unlock()

	if s.recordStarts != nil && s.recordMu != nil {
		s.recordMu.Lock()
		*s.recordStarts = append(*s.recordStarts, s.tracker.name)
		s.recordMu.Unlock()
	}
	return s.tracker.startErr
}

func (s *trackedService) Shutdown(ctx context.Context) error {
	s.tracker.mu.Lock()
	s.tracker.shutdownCount++
	s.tracker.mu.Unlock()

	if s.recordStops != nil && s.recordMu != nil {
		s.recordMu.Lock()
		*s.recordStops = append(*s.recordStops, s.tracker.name)
		s.recordMu.Unlock()
	}
	return s.tracker.shutdownErr
}

func (s *trackedService) Errors() <-chan error {
	return s.tracker.errCh
}

func TestServiceHostStartStopOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	host := NewServiceHost()

	var mu sync.Mutex
	var starts, stops []string

	alpha := &serviceTracker{name: "alpha"}
	beta := &serviceTracker{name: "beta"}

	if err := host.Register("alpha", alpha.factory(&starts, &stops, &mu)); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if err := host.Register("beta", beta.factory(&starts, &stops, &mu)); err != nil {
		t.Fatalf("register beta: %v", err)
	}

	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("start host: %v", err)
	}

	if want := []string{"alpha", "beta"}; !slicesEqual(starts, want) {
		t.Fatalf("start order mismatch, want %v got %v", want, starts)
	}

	if err := host.Stop(context.Background()); err != nil {
		t.Fatalf("stop host: %v", err)
	}

	if want := []string{"beta", "alpha"}; !slicesEqual(stops, want) {
		t.Fatalf("stop order mismatch, want %v got %v", want, stops)
	}
}

func TestServiceHostRegisterGuards(t *testing.T) {
	host := NewServiceHost()
	tracker := &serviceTracker{name: "svc"}

	if err := host.Register("svc", tracker.factory(nil, nil, nil)); err != nil {
		t.Fatalf("register svc: %v", err)
	}

	if err := host.Register("svc", tracker.factory(nil, nil, nil)); err == nil {
		t.Fatalf("expected duplicate registration error")
	}

	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("start host: %v", err)
	}
	defer host.Stop(context.Background())

	if err := host.Register("late", tracker.factory(nil, nil, nil)); err == nil {
		t.Fatalf("expected registration after start to fail")
	}
}

func TestServiceHostRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	host := NewServiceHost()
	tracker := &serviceTracker{name: "restartable"}

	if err := host.Register("restartable", tracker.factory(nil, nil, nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("start host: %v", err)
	}
	defer host.Stop(context.Background())

	if tracker.startCount != 1 {
		t.Fatalf("expected start count 1, got %d", tracker.startCount)
	}

	if err := host.Restart(context.Background(), "restartable"); err != nil {
		t.Fatalf("restart: %v", err)
	}

	if tracker.startCount != 2 {
		t.Fatalf("expected start count 2 after restart, got %d", tracker.startCount)
	}
	if tracker.shutdownCount != 1 {
		t.Fatalf("expected shutdown count 1 after restart, got %d", tracker.shutdownCount)
	}
}

func TestServiceHostStartRollbackOnFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	host := NewServiceHost()

	alpha := &serviceTracker{name: "alpha"}
	beta := &serviceTracker{name: "beta", startErr: errors.New("boom")}

	if err := host.Register("alpha", alpha.factory(nil, nil, nil)); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if err := host.Register("beta", beta.factory(nil, nil, nil)); err != nil {
		t.Fatalf("register beta: %v", err)
	}

	if err := host.Start(context.Background()); err == nil {
		t.Fatalf("expected start error")
	}

	if alpha.shutdownCount != 1 {
		t.Fatalf("expected alpha shutdown during rollback, got %d", alpha.shutdownCount)
	}
	if beta.startCount != 1 {
		t.Fatalf("expected beta start attempt, got %d", beta.startCount)
	}
}

func TestServiceHostPropagatesServiceErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	host := NewServiceHost()
	tracker := &serviceTracker{name: "observable", errCh: make(chan error, 1)}

	if err := host.Register("observable", tracker.factory(nil, nil, nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("start host: %v", err)
	}
	defer host.Stop(context.Background())

	wantErr := errors.New("service failure")
	tracker.errCh <- wantErr

	select {
	case err := <-host.Errors():
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("unexpected error propagated: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for propagated error")
	}
}

func TestServiceHostWatchConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()

	store, err := configstore.Open(configstore.Options{
		DBPath: filepath.Join(tmp, "config.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	host := NewServiceHost()
	if err := host.Register("noop", (&serviceTracker{name: "noop"}).factory(nil, nil, nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("start host: %v", err)
	}
	defer host.Stop(context.Background())

	events := make(chan configstore.ChangeEvent, 1)

	cancel, err := host.WatchConfig(context.Background(), store, 20*time.Millisecond, func(ev configstore.ChangeEvent) {
		events <- ev
	})
	if err != nil {
		t.Fatalf("watch config: %v", err)
	}
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(context.Background(), time.Second)
	defer cancelCtx()
	if err := store.SaveSettings(ctx, map[string]string{"feature.flag": "on"}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	select {
	case ev := <-events:
		if !ev.SettingsChanged {
			t.Fatalf("expected SettingsChanged=true, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for change event")
	}
}

func TestServiceHostWatchConfigRequiresStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()

	store, err := configstore.Open(configstore.Options{
		DBPath: filepath.Join(tmp, "config.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	host := NewServiceHost()
	if _, err := host.WatchConfig(context.Background(), store, time.Second, nil); err == nil {
		t.Fatalf("expected error when watching before start")
	}
}

func TestLifecycleShutdown(t *testing.T) {
	lc := NewLifecycle()
	select {
	case <-lc.Done():
		t.Fatalf("unexpected done before shutdown")
	default:
	}

	lc.Shutdown()

	select {
	case <-lc.Done():
	default:
		t.Fatalf("expected done channel closed")
	}

	// Second shutdown should be a no-op without panic.
	lc.Shutdown()
}

func TestPIDFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "nupi.pid")

	if err := WritePIDFile(pidPath, 1234); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	info, err := os.Stat(pidPath)
	if err != nil {
		t.Fatalf("stat pid: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 perms, got %o", perm)
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	if string(data) != "1234" {
		t.Fatalf("expected pid 1234, got %s", string(data))
	}

	RemovePIDFile(pidPath)
	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected pid file removed, got err=%v", err)
	}
}

func slicesEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
