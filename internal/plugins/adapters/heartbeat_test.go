package adapters

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/napdial"
)

// newHeartbeatTestService creates a Service wired for heartbeat testing.
// It mocks readiness and sets up a Manager with the given bindings.
// Does NOT call Ensure or Start — the caller should call svc.Start().
func newHeartbeatTestService(t *testing.T, bindings []Binding, heartbeatInterval time.Duration) (*Service, *eventbus.Bus) {
	t.Helper()

	// Mock readiness so remote adapters don't try to actually connect.
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{}
	for _, b := range bindings {
		store.mu.Lock()
		store.bindings = append(store.bindings, hbFakeBinding(b))
		store.mu.Unlock()
		store.setAdapter(hbFakeAdapter(b.AdapterID))
		store.setEndpoint(hbFakeEndpoint(b))
	}

	launcher := NewMockLauncher()
	bus := eventbus.New()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		Bus:       bus,
		PluginDir: t.TempDir(),
	})

	// store=nil: heartbeat tests don't need config watching (startWatcher is skipped).
	svc := NewService(manager, nil, bus,
		WithEnsureInterval(0),
		WithHeartbeatInterval(heartbeatInterval),
	)

	return svc, bus
}

func hbFakeBinding(b Binding) configstore.AdapterBinding {
	return configstore.AdapterBinding{
		Slot:      string(b.Slot),
		Status:    "active",
		AdapterID: strPtr(b.AdapterID),
	}
}

func hbFakeAdapter(id string) configstore.Adapter {
	return configstore.Adapter{ID: id}
}

func hbFakeEndpoint(b Binding) configstore.AdapterEndpoint {
	transport := b.Runtime[RuntimeExtraTransport]
	addr := b.Runtime[RuntimeExtraAddress]
	// No default for transport — tests must set it explicitly to avoid masking
	// missing-field bugs. Address defaults to a placeholder for convenience
	// (the mock prober never dials it).
	if addr == "" {
		addr = "127.0.0.1:9999"
	}
	ep := configstore.AdapterEndpoint{
		AdapterID: b.AdapterID,
		Transport: transport,
		Address:   addr,
	}
	if transport == "process" {
		ep.Command = "./mock-adapter"
	}
	return ep
}

// NOTE: Heartbeat integration tests (TestHeartbeat_*) intentionally do NOT use
// t.Parallel(). They share the global heartbeatProbeFn via SetHeartbeatProber —
// running in parallel would cause logical interference between test overrides.
// The probeAdapter dispatch and buildTLSConfig unit tests below DO use t.Parallel()
// because they call the function directly without the global override.

// 5.1: Test heartbeat fires at configured interval.
// Uses a channel to wait for exactly N probes instead of sleep-and-count,
// making the test deterministic and CI-stable.
func TestHeartbeat_FiresAtInterval(t *testing.T) {
	probeCh := make(chan struct{}, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return nil
	}))

	svc, _ := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for exactly 4 probes: 1 immediate + 3 ticker ticks at 100ms.
	// Each with a generous per-probe timeout to avoid CI flakiness.
	for i := 0; i < 4; i++ {
		select {
		case <-probeCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for probe %d of 4", i+1)
		}
	}

	cancel()
	svc.wg.Wait()
}

// 5.2: Test heartbeat probes all running adapters.
// Uses channel-based approach: waits until both adapters are probed at least once.
func TestHeartbeat_ProbesAllRunning(t *testing.T) {
	var mu sync.Mutex
	probed := make(map[string]bool)
	allProbed := make(chan struct{}, 1)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, b Binding) error {
		mu.Lock()
		probed[b.AdapterID] = true
		if probed["adapter.ai"] && probed["adapter.stt"] {
			select {
			case allProbed <- struct{}{}:
			default:
			}
		}
		mu.Unlock()
		return nil
	}))

	svc, _ := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
		{Slot: SlotSTT, AdapterID: "adapter.stt", Runtime: map[string]string{
			RuntimeExtraTransport: "http",
			RuntimeExtraAddress:   "127.0.0.1:9002",
		}},
	}, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-allProbed:
	case <-time.After(3 * time.Second):
		mu.Lock()
		t.Fatalf("timeout: probed adapters: %v", probed)
		mu.Unlock()
	}

	cancel()
	svc.wg.Wait()
}

// 5.3: Test probe failure → AdapterHealthDegraded event published.
func TestHeartbeat_ProbeFailure_DegradedEvent(t *testing.T) {
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		return errors.New("connection refused")
	}))

	svc, bus := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai.flaky", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}, 100*time.Millisecond)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	evt := hbWaitForStatus(t, sub, eventbus.AdapterHealthDegraded, 3*time.Second)
	if evt.AdapterID != "adapter.ai.flaky" {
		t.Errorf("expected AdapterID adapter.ai.flaky, got %q", evt.AdapterID)
	}
	if evt.Slot != string(SlotAI) {
		t.Errorf("expected Slot %s, got %q", SlotAI, evt.Slot)
	}
	if !strings.Contains(evt.Message, "connection refused") {
		t.Errorf("expected failure message, got %q", evt.Message)
	}

	cancel()
	svc.wg.Wait()
}

// 5.4: Test probe success → no event published (steady state).
// Uses channel-based wait for probe calls to ensure heartbeat actually ran,
// then verifies no status events were published.
func TestHeartbeat_ProbeSuccess_NoEvent(t *testing.T) {
	probeCh := make(chan struct{}, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return nil // Always healthy.
	}))

	svc, bus := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}, 100*time.Millisecond)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain initial "ready" event from reconcile.
	hbDrainEvents(t, sub, 500*time.Millisecond)

	// Wait for at least 3 probe calls (deterministic proof heartbeat ran).
	for i := 0; i < 3; i++ {
		select {
		case <-probeCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for probe %d of 3", i+1)
		}
	}

	cancel()
	svc.wg.Wait()

	// Drain any late events with a reasonable window — longer than 50ms to catch
	// events that may still be in the event bus pipeline after cancellation.
	hbDrainEvents(t, sub, 200*time.Millisecond)

	// No heartbeat events should have been published (steady-state healthy = no spam).
	select {
	case env := <-sub.C():
		t.Errorf("expected no events for steady-state healthy, got status=%s msg=%q", env.Payload.Status, env.Payload.Message)
	default:
		// Good — no events.
	}
}

// 5.5: Test probe failure then recovery → AdapterHealthReady event.
func TestHeartbeat_FailureThenRecovery(t *testing.T) {
	var shouldFail atomic.Bool
	shouldFail.Store(true)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		if shouldFail.Load() {
			return errors.New("timeout")
		}
		return nil
	}))

	svc, bus := newHeartbeatTestService(t, []Binding{
		{Slot: SlotSTT, AdapterID: "adapter.stt.recover", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9002",
		}},
	}, 100*time.Millisecond)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for degraded event (may get initial ready first, skip it).
	hbWaitForStatus(t, sub, eventbus.AdapterHealthDegraded, 3*time.Second)

	// Recover.
	shouldFail.Store(false)

	// Wait for recovery event.
	hbWaitForStatus(t, sub, eventbus.AdapterHealthReady, 3*time.Second)

	cancel()
	svc.wg.Wait()
}

// 5.6: Test heartbeat skips builtin adapters.
// Uses a second non-builtin adapter as a probe witness: once the grpc adapter
// has been probed 3 times, we know the heartbeat ran multiple cycles and can
// definitively assert the builtin adapter was never probed.
func TestHeartbeat_SkipsBuiltin(t *testing.T) {
	probeCh := make(chan string, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, b Binding) error {
		select {
		case probeCh <- b.Runtime[RuntimeExtraTransport]:
		default:
		}
		return nil
	}))

	svc, _ := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: MockAIAdapterID, Runtime: map[string]string{
			RuntimeExtraTransport: "builtin",
		}},
		{Slot: SlotSTT, AdapterID: "adapter.stt.witness", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9002",
		}},
	}, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for 3 probes of the witness adapter (proves heartbeat ran).
	for i := 0; i < 3; i++ {
		select {
		case transport := <-probeCh:
			if transport == "builtin" {
				t.Fatal("builtin adapter was probed — expected skip")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for probe %d of 3", i+1)
		}
	}

	cancel()
	svc.wg.Wait()
}

// 5.7: Test heartbeat respects context cancellation.
// Uses channel-based approach: wait for at least 1 probe deterministically,
// then cancel and verify no more probes fire.
func TestHeartbeat_ContextCancellation(t *testing.T) {
	probeCh := make(chan struct{}, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return nil
	}))

	svc, _ := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for at least 1 probe deterministically (immediate probe on Start).
	select {
	case <-probeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first probe")
	}

	cancel()
	svc.wg.Wait()

	// Drain any probes that completed before wg.Wait returned.
drain:
	for {
		select {
		case <-probeCh:
		default:
			break drain
		}
	}

	// After wg.Wait(), the heartbeat goroutine has exited — no new probes
	// can fire. Verify the channel is empty (all probes drained above).
	select {
	case <-probeCh:
		t.Error("probe fired after context cancellation and wg.Wait")
	default:
		// Good — no probes after cancel.
	}
}

// 5.8: Test WithHeartbeatInterval(0) disables heartbeat.
func TestHeartbeat_DisabledWithZeroInterval(t *testing.T) {
	var probeCount atomic.Int32
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		probeCount.Add(1)
		return nil
	}))

	svc, _ := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}, 0) // Disabled.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// With interval=0, startHeartbeat is never called — no goroutine exists
	// that could fire probes. Cancel and verify immediately.
	cancel()
	svc.wg.Wait()

	if probeCount.Load() > 0 {
		t.Errorf("expected no probes with interval=0, got %d", probeCount.Load())
	}
}

// 5.9: Test deduplication — consecutive failures don't emit duplicate events.
// Uses channel-based approach: counts probe invocations to deterministically
// prove multiple heartbeat cycles ran before checking for duplicate events.
func TestHeartbeat_Deduplication(t *testing.T) {
	probeCh := make(chan struct{}, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return errors.New("persistent failure")
	}))

	svc, bus := newHeartbeatTestService(t, []Binding{
		{Slot: SlotTTS, AdapterID: "adapter.tts.broken", Runtime: map[string]string{
			RuntimeExtraTransport: "http",
			RuntimeExtraAddress:   "127.0.0.1:9003",
		}},
	}, 100*time.Millisecond)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for initial degraded event (may get ready from reconcile first).
	hbWaitForStatus(t, sub, eventbus.AdapterHealthDegraded, 3*time.Second)

	// Deterministically wait for 4 more probe cycles — proves heartbeat ran
	// multiple cycles after the initial degraded transition.
	for i := 0; i < 4; i++ {
		select {
		case <-probeCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for probe cycle %d of 4", i+1)
		}
	}

	cancel()
	svc.wg.Wait()

	// Count any additional degraded events — should be zero (deduplicated).
	extraDegraded := 0
drainDedup:
	for {
		select {
		case env := <-sub.C():
			if env.Payload.Status == eventbus.AdapterHealthDegraded {
				extraDegraded++
			}
		default:
			break drainDedup
		}
	}
	if extraDegraded > 0 {
		t.Errorf("expected 0 duplicate degraded events, got %d", extraDegraded)
	}
}

// Test immediate first heartbeat probe fires without waiting for ticker.
// Uses channel-based wait instead of sleep for deterministic CI behavior.
func TestHeartbeat_ImmediateFirstProbe(t *testing.T) {
	probeCh := make(chan struct{}, 10)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return nil
	}))

	// Use a very long interval so the ticker never fires during the test.
	svc, _ := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for exactly 1 immediate probe — deterministic, no sleep.
	select {
	case <-probeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for immediate probe")
	}

	cancel()
	svc.wg.Wait()

	// Verify no additional probes fired (ticker at 10s should not have fired).
	select {
	case <-probeCh:
		t.Error("expected exactly 1 probe (immediate, no ticker at 10s interval)")
	default:
		// Good — only the immediate probe.
	}
}

// Test probeAdapter returns clear error for empty/missing transport.
func TestProbeAdapter_EmptyTransportReturnsError(t *testing.T) {
	t.Parallel()
	err := probeAdapter(context.Background(), Binding{
		AdapterID: "test-adapter",
		Runtime:   map[string]string{},
	})
	if err == nil {
		t.Error("expected error for empty transport")
	}
	if !strings.Contains(err.Error(), "no transport configured") {
		t.Errorf("expected 'no transport configured' error, got: %v", err)
	}
}

// --- probeAdapter transport dispatch tests (M3 fix) ---

// TestProbeAdapter_NilRuntimeReturnsError verifies nil Runtime map is handled (Go returns zero values).
func TestProbeAdapter_NilRuntimeReturnsError(t *testing.T) {
	t.Parallel()
	err := probeAdapter(context.Background(), Binding{
		AdapterID: "test-adapter",
		Runtime:   nil,
	})
	if err == nil {
		t.Error("expected error for nil runtime map")
	}
	if !strings.Contains(err.Error(), "no transport configured") {
		t.Errorf("expected 'no transport configured' error, got: %v", err)
	}
}

// TestProbeAdapter_BuiltinReturnsNil verifies the builtin transport returns nil (always healthy).
func TestProbeAdapter_BuiltinReturnsNil(t *testing.T) {
	t.Parallel()
	err := probeAdapter(context.Background(), Binding{
		Runtime: map[string]string{RuntimeExtraTransport: "builtin"},
	})
	if err != nil {
		t.Errorf("expected nil for builtin, got: %v", err)
	}
}

// TestProbeAdapter_UnknownTransportReturnsError verifies unknown transports return an error.
func TestProbeAdapter_UnknownTransportReturnsError(t *testing.T) {
	t.Parallel()
	err := probeAdapter(context.Background(), Binding{
		AdapterID: "test-adapter",
		Runtime:   map[string]string{RuntimeExtraTransport: "ftp"},
	})
	if err == nil {
		t.Error("expected error for unknown transport")
	}
	if !strings.Contains(err.Error(), "unsupported transport") {
		t.Errorf("expected unsupported transport error, got: %v", err)
	}
}

// TestProbeAdapter_GRPCDispatch verifies gRPC transport dispatches to grpcHealthDial/grpcHealthProbe.
func TestProbeAdapter_GRPCDispatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := probeAdapter(ctx, Binding{
		AdapterID: "test-grpc",
		Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:1",
		},
	})
	if err == nil {
		t.Error("expected error for unreachable gRPC endpoint")
	}
}

// TestProbeAdapter_HTTPDispatch verifies HTTP transport dispatches to httpHealthSetup/httpHealthProbe.
func TestProbeAdapter_HTTPDispatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := probeAdapter(ctx, Binding{
		AdapterID: "test-http",
		Runtime: map[string]string{
			RuntimeExtraTransport: "http",
			RuntimeExtraAddress:   "127.0.0.1:1",
		},
	})
	if err == nil {
		t.Error("expected error for unreachable HTTP endpoint")
	}
}

// TestProbeAdapter_ProcessDispatch verifies process transport dispatches to TCP dial.
func TestProbeAdapter_ProcessDispatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := probeAdapter(ctx, Binding{
		AdapterID: "test-process",
		Runtime: map[string]string{
			RuntimeExtraTransport: "process",
			RuntimeExtraAddress:   "127.0.0.1:1",
		},
	})
	if err == nil {
		t.Error("expected error for unreachable TCP endpoint")
	}
}

// TestProbeAdapter_ProcessInvalidAddress verifies process transport with malformed address
// returns a clear "invalid address" error (M1 fix: consistent with gRPC/HTTP validation).
func TestProbeAdapter_ProcessInvalidAddress(t *testing.T) {
	t.Parallel()
	err := probeAdapter(context.Background(), Binding{
		AdapterID: "test-process",
		Runtime: map[string]string{
			RuntimeExtraTransport: "process",
			RuntimeExtraAddress:   "missing-port",
		},
	})
	if err == nil {
		t.Error("expected error for malformed address")
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Errorf("expected 'invalid address' error, got: %v", err)
	}
}

// --- buildTLSConfig tests ---

func TestBuildTLSConfig_NoTLS(t *testing.T) {
	t.Parallel()
	cfg, err := buildTLSConfig(Binding{
		Runtime: map[string]string{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Error("expected nil TLS config for empty binding")
	}
}

func TestBuildTLSConfig_WithCertPaths(t *testing.T) {
	t.Parallel()
	cfg, err := buildTLSConfig(Binding{
		Runtime: map[string]string{
			RuntimeExtraTLSCertPath:   "/path/cert.pem",
			RuntimeExtraTLSKeyPath:    "/path/key.pem",
			RuntimeExtraTLSCACertPath: "/path/ca.pem",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if cfg.CertPath != "/path/cert.pem" {
		t.Errorf("expected CertPath /path/cert.pem, got %q", cfg.CertPath)
	}
	if cfg.KeyPath != "/path/key.pem" {
		t.Errorf("expected KeyPath /path/key.pem, got %q", cfg.KeyPath)
	}
	if cfg.CACertPath != "/path/ca.pem" {
		t.Errorf("expected CACertPath /path/ca.pem, got %q", cfg.CACertPath)
	}
	if cfg.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=false")
	}
}

func TestBuildTLSConfig_InsecureOnly(t *testing.T) {
	t.Parallel()
	cfg, err := buildTLSConfig(Binding{
		Runtime: map[string]string{
			RuntimeExtraTLSInsecure: "true",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil TLS config for insecure")
	}
	if !cfg.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=true")
	}
}

// L2 fix: Verify partial TLS config (cert without key) returns clear error.
func TestBuildTLSConfig_PartialCertReturnsError(t *testing.T) {
	t.Parallel()
	_, err := buildTLSConfig(Binding{
		AdapterID: "test-adapter",
		Runtime: map[string]string{
			RuntimeExtraTLSCertPath: "/path/cert.pem",
			// KeyPath intentionally missing.
		},
	})
	if err == nil {
		t.Fatal("expected error for cert without key")
	}
	if !strings.Contains(err.Error(), "incomplete TLS client cert") {
		t.Errorf("expected 'incomplete TLS client cert' error, got: %v", err)
	}
}

// L2 fix: Verify partial TLS config (key without cert) returns clear error.
func TestBuildTLSConfig_PartialKeyReturnsError(t *testing.T) {
	t.Parallel()
	_, err := buildTLSConfig(Binding{
		AdapterID: "test-adapter",
		Runtime: map[string]string{
			RuntimeExtraTLSKeyPath: "/path/key.pem",
			// CertPath intentionally missing.
		},
	})
	if err == nil {
		t.Fatal("expected error for key without cert")
	}
	if !strings.Contains(err.Error(), "incomplete TLS client cert") {
		t.Errorf("expected 'incomplete TLS client cert' error, got: %v", err)
	}
}

// M1 (review #10): Verify strings.EqualFold handles mixed-case insecure flag.
func TestBuildTLSConfig_InsecureCaseInsensitive(t *testing.T) {
	t.Parallel()
	for _, val := range []string{"TRUE", "True", "tRuE"} {
		cfg, err := buildTLSConfig(Binding{
			Runtime: map[string]string{
				RuntimeExtraTLSInsecure: val,
			},
		})
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", val, err)
		}
		if cfg == nil || !cfg.InsecureSkipVerify {
			t.Errorf("expected InsecureSkipVerify=true for %q", val)
		}
	}
}

// M1 fix: Test heartbeat state is cleared when adapter stops and restarts,
// so no false "recovered" event fires for the new instance.
// Uses channel-based probe counting instead of sleep for deterministic CI behavior.
func TestHeartbeat_ClearStateOnAdapterRestart(t *testing.T) {
	var shouldFail atomic.Bool
	shouldFail.Store(true)
	probeCh := make(chan struct{}, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		if shouldFail.Load() {
			return errors.New("down")
		}
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return nil
	}))

	bindings := []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai.restart", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}
	svc, bus := newHeartbeatTestService(t, bindings, 100*time.Millisecond)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for degraded event.
	hbWaitForStatus(t, sub, eventbus.AdapterHealthDegraded, 3*time.Second)

	// Simulate adapter stop by clearing heartbeat state (as updateState does).
	svc.clearHeartbeatState(SlotAI)

	// Now make probes succeed — adapter "restarted" healthy.
	shouldFail.Store(false)

	// Wait for 3 successful probe cycles (deterministic proof heartbeat ran
	// after state clear). probeCh only sends on success, so 3 receives =
	// 3 completed heartbeat cycles with the adapter healthy.
	for i := 0; i < 3; i++ {
		select {
		case <-probeCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for successful probe %d of 3", i+1)
		}
	}

	cancel()
	svc.wg.Wait()

	// Drain remaining events — none should be AdapterHealthReady from heartbeat
	// (the reconcile "ready" events are from updateState, not heartbeat).
	for {
		select {
		case env := <-sub.C():
			if env.Payload.Status == eventbus.AdapterHealthReady &&
				strings.Contains(env.Payload.Message, "heartbeat probe recovered") {
				t.Error("unexpected heartbeat recovery event after state clear — should be fresh start")
			}
		default:
			return
		}
	}
}

// M3 fix: Test mixed success/failure — only the failing adapter gets degraded.
// Uses channel-based probe counting instead of sleep for deterministic CI behavior.
func TestHeartbeat_MixedResults(t *testing.T) {
	probeCh := make(chan struct{}, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, b Binding) error {
		if b.AdapterID == "adapter.ai.failing" {
			return errors.New("unreachable")
		}
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return nil // adapter.stt.healthy succeeds.
	}))

	svc, bus := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai.failing", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
		{Slot: SlotSTT, AdapterID: "adapter.stt.healthy", Runtime: map[string]string{
			RuntimeExtraTransport: "http",
			RuntimeExtraAddress:   "127.0.0.1:9002",
		}},
	}, 100*time.Millisecond)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for degraded event — should be for the failing adapter only.
	evt := hbWaitForStatus(t, sub, eventbus.AdapterHealthDegraded, 3*time.Second)
	if evt.AdapterID != "adapter.ai.failing" {
		t.Errorf("expected degraded for adapter.ai.failing, got %q", evt.AdapterID)
	}

	// Wait for 3 successful probes of the healthy adapter (deterministic proof
	// that multiple heartbeat cycles ran after the initial degraded event).
	for i := 0; i < 3; i++ {
		select {
		case <-probeCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for healthy probe %d of 3", i+1)
		}
	}

	cancel()
	svc.wg.Wait()

	// Verify no degraded events for the healthy adapter.
	for {
		select {
		case env := <-sub.C():
			if env.Payload.Status == eventbus.AdapterHealthDegraded &&
				env.Payload.AdapterID == "adapter.stt.healthy" {
				t.Error("healthy adapter should not get degraded event")
			}
		default:
			return
		}
	}
}

// Test that heartbeat does not publish spurious events during context cancellation.
// Uses channel-based probe-started signal instead of sleep for deterministic CI behavior.
func TestHeartbeat_NoSpuriousEventsOnShutdown(t *testing.T) {
	probeStarted := make(chan struct{}, 10)
	// Probe takes a while — simulates an in-flight probe during shutdown.
	t.Cleanup(SetHeartbeatProber(func(ctx context.Context, _ Binding) error {
		select {
		case probeStarted <- struct{}{}:
		default:
		}
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}))

	svc, bus := newHeartbeatTestService(t, []Binding{
		{Slot: SlotAI, AdapterID: "adapter.ai", Runtime: map[string]string{
			RuntimeExtraTransport: "grpc",
			RuntimeExtraAddress:   "127.0.0.1:9001",
		}},
	}, 100*time.Millisecond)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for probe to actually start (deterministic, no sleep).
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for probe to start")
	}
	cancel()
	svc.wg.Wait()

	// Drain events — none should be degraded from heartbeat (shutdown noise).
	for {
		select {
		case env := <-sub.C():
			if env.Payload.Status == eventbus.AdapterHealthDegraded &&
				strings.Contains(env.Payload.Message, "heartbeat probe failed") {
				t.Error("spurious degraded event published during shutdown")
			}
		default:
			return
		}
	}
}

// L4 fix: Test heartbeat with no running adapters — should do nothing silently.
func TestHeartbeat_EmptyRunning(t *testing.T) {
	var probeCount atomic.Int32
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		probeCount.Add(1)
		return nil
	}))

	svc, _ := newHeartbeatTestService(t, []Binding{}, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Sleep to allow multiple heartbeat cycles to run (at 100ms interval,
	// ~3 cycles in 300ms). This is a negative assertion — proving no probes
	// fire when there are no running adapters. Channel-based approaches
	// cannot prove absence of events without a wait window.
	time.Sleep(300 * time.Millisecond)
	cancel()
	svc.wg.Wait()

	if probeCount.Load() > 0 {
		t.Errorf("expected no probes with no running adapters, got %d", probeCount.Load())
	}
}

// M5 fix: Test that negative WithHeartbeatInterval is ignored (default preserved).
func TestHeartbeat_NegativeIntervalIgnored(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil, eventbus.New(), WithHeartbeatInterval(-5*time.Second))
	if svc.heartbeatInterval != defaultHeartbeatInterval {
		t.Errorf("expected default %v for negative interval, got %v", defaultHeartbeatInterval, svc.heartbeatInterval)
	}
}

// M3 fix: Integration test for the full reconcile → updateState → clearHeartbeatState
// → heartbeat purge path. Unlike TestHeartbeat_ClearStateOnAdapterRestart (which calls
// clearHeartbeatState directly), this test verifies the actual production path where
// a config change triggers reconcile → fingerprint mismatch → stop + clearHeartbeatState.
// Uses channel-based probe counting instead of sleep for deterministic CI behavior.
func TestHeartbeat_ReconcileClearsStateIntegration(t *testing.T) {
	var shouldFail atomic.Bool
	shouldFail.Store(true)
	probeCh := make(chan struct{}, 100)
	t.Cleanup(SetHeartbeatProber(func(_ context.Context, _ Binding) error {
		if shouldFail.Load() {
			return errors.New("down")
		}
		select {
		case probeCh <- struct{}{}:
		default:
		}
		return nil
	}))
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{Slot: string(SlotAI), Status: "active", AdapterID: strPtr("adapter.ai.test")},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai.test"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai.test",
		Transport: "grpc",
		Address:   "127.0.0.1:9001",
	})

	launcher := NewMockLauncher()
	bus := eventbus.New()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		Bus:       bus,
		PluginDir: t.TempDir(),
	})

	svc := NewService(manager, nil, bus,
		WithEnsureInterval(0),
		WithHeartbeatInterval(100*time.Millisecond),
	)

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status, eventbus.WithSubscriptionBuffer(50))
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for heartbeat to detect failure.
	hbWaitForStatus(t, sub, eventbus.AdapterHealthDegraded, 3*time.Second)

	// Make probes succeed from now on.
	shouldFail.Store(false)

	// Simulate config change: update binding config → fingerprint changes.
	// reconcile → updateState detects mismatch → emits stopped + clearHeartbeatState.
	store.mu.Lock()
	store.bindings[0].Config = `{"version":2}`
	store.mu.Unlock()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Wait for 3 successful probe cycles (deterministic proof heartbeat ran
	// after reconcile cleared state). probeCh only sends on success.
	for i := 0; i < 3; i++ {
		select {
		case <-probeCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for successful probe %d of 3 after reconcile", i+1)
		}
	}

	cancel()
	svc.wg.Wait()

	// Drain events — verify no "heartbeat probe recovered" events.
	// The reconcile cleared heartbeat state, so the adapter starts with a
	// clean slate (assumed healthy). No transition = no recovery event.
	for {
		select {
		case env := <-sub.C():
			if env.Payload.Status == eventbus.AdapterHealthReady &&
				strings.Contains(env.Payload.Message, "heartbeat probe recovered") {
				t.Error("unexpected heartbeat recovery event — reconcile should have cleared heartbeat state")
			}
		default:
			return
		}
	}
}

// --- Test helpers ---

// hbDrainEvents drains all events until no events arrive for the given duration.
func hbDrainEvents(t *testing.T, sub *eventbus.TypedSubscription[eventbus.AdapterStatusEvent], wait time.Duration) {
	t.Helper()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-sub.C():
			timer.Reset(wait)
		case <-timer.C:
			return
		}
	}
}

func hbWaitForStatus(t *testing.T, sub *eventbus.TypedSubscription[eventbus.AdapterStatusEvent], want eventbus.AdapterHealth, timeout time.Duration) eventbus.AdapterStatusEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case env := <-sub.C():
			if env.Payload.Status == want {
				return env.Payload
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for status %s", want)
			return eventbus.AdapterStatusEvent{} // unreachable
		}
	}
}
