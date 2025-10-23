package termresize

import (
	"reflect"
	"testing"
	"time"
)

type stubMode struct {
	name     string
	decision ResizeDecision
	err      error
	calls    int
}

func (m *stubMode) Name() string {
	return m.name
}

func (m *stubMode) Handle(ctx *Context) (ResizeDecision, error) {
	m.calls++
	return m.decision, m.err
}

func TestManagerModeRegistration(t *testing.T) {
	defaultMode := &stubMode{name: "default"}
	mgr, err := NewManager(defaultMode)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := mgr.RegisterMode(&stubMode{name: "extra"}); err != nil {
		t.Fatalf("RegisterMode: %v", err)
	}

	if err := mgr.RegisterMode(&stubMode{name: "extra"}); err == nil {
		t.Fatalf("expected duplicate registration to fail")
	}

	modes := mgr.AvailableModes()
	if !reflect.DeepEqual(modes, []string{"default", "extra"}) {
		t.Fatalf("unexpected modes: %+v", modes)
	}
}

func TestManagerSetSessionMode(t *testing.T) {
	defaultMode := &stubMode{name: "default"}
	custom := &stubMode{name: "custom", decision: ResizeDecision{SessionID: "s-custom"}}
	mgr, err := NewManager(defaultMode, custom)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := mgr.SetSessionMode("session", "custom"); err != nil {
		t.Fatalf("SetSessionMode: %v", err)
	}

	decision, err := mgr.Handle(ResizeRequest{
		SessionID: "session",
		Source:    SourceHost,
		Size:      ViewportSize{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if decision.SessionID != "s-custom" {
		t.Fatalf("expected custom decision, got %+v", decision)
	}
	if custom.calls != 1 {
		t.Fatalf("expected custom mode to be invoked once, got %d", custom.calls)
	}
}

func TestHostLockModeHostResize(t *testing.T) {
	mode := NewHostLockMode()
	mgr, err := NewManager(mode)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	size := ViewportSize{Cols: 100, Rows: 40}
	decision, err := mgr.HandleHostResize("session", size, map[string]any{"source": "host"})
	if err != nil {
		t.Fatalf("HandleHostResize: %v", err)
	}

	if decision.PTYSize == nil || *decision.PTYSize != size {
		t.Fatalf("expected PTY size %+v, got %+v", size, decision.PTYSize)
	}
	if len(decision.Broadcast) != 1 || decision.Broadcast[0].Reason != "host_lock" {
		t.Fatalf("unexpected broadcast: %+v", decision.Broadcast)
	}
	if decision.State.ActiveController == nil || decision.State.ActiveController.Source != SourceHost {
		t.Fatalf("expected host controller, got %+v", decision.State.ActiveController)
	}

	state := mgr.Snapshot("session")
	if state == nil || state.Host == nil || state.Host.Size != size {
		t.Fatalf("expected session snapshot to contain host size, got %+v", state)
	}
}

func TestHostLockModeClientResize(t *testing.T) {
	mode := NewHostLockMode()
	mgr, err := NewManager(mode)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	decision, err := mgr.HandleClientResize("session", "client", ViewportSize{Cols: 120, Rows: 60}, nil)
	if err != nil {
		t.Fatalf("HandleClientResize: %v", err)
	}

	if decision.PTYSize != nil {
		t.Fatalf("expected no PTY change, got %+v", decision.PTYSize)
	}
	if len(decision.Broadcast) != 0 {
		t.Fatalf("expected no broadcast for client resize, got %+v", decision.Broadcast)
	}
	if len(decision.Notes) == 0 {
		t.Fatalf("expected notes indicating ignored resize")
	}
}

func TestSessionStateClone(t *testing.T) {
	state := newSessionState("sess")
	state.Host = &ViewportSnapshot{
		Size:     ViewportSize{Cols: 80, Rows: 24},
		Source:   SourceHost,
		Metadata: map[string]any{"host": true},
	}
	state.Clients["client"] = &ViewportSnapshot{
		Size:     ViewportSize{Cols: 100, Rows: 30},
		Source:   SourceClient,
		ClientID: "client",
		Metadata: map[string]any{"client": true},
	}
	state.Metadata["foo"] = "bar"
	state.ActiveController = &ControllerRef{Source: SourceClient, ClientID: "client"}
	state.registerClient("client", map[string]any{"meta": "data"}, time.Unix(100, 0))

	clone := state.Clone()
	if clone == nil {
		t.Fatalf("Clone returned nil")
	}

	clone.Metadata["foo"] = "mutated"
	clone.Clients["client"].Metadata["client"] = "mutated"

	if state.Metadata["foo"] != "bar" {
		t.Fatalf("expected original metadata untouched, got %v", state.Metadata["foo"])
	}
	if state.Clients["client"].Metadata["client"] != true {
		t.Fatalf("expected original client metadata untouched, got %+v", state.Clients["client"].Metadata)
	}
}

func TestRegisterAndUnregisterClient(t *testing.T) {
	mgr, err := NewManager(NewHostLockMode())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.RegisterClient("session", "client", map[string]any{"role": "viewer"})
	state := mgr.Snapshot("session")
	if state == nil || state.Connected["client"] == nil {
		t.Fatalf("expected client registration, got %+v", state)
	}
	mgr.UnregisterClient("session", "client")
	state = mgr.Snapshot("session")
	if state == nil {
		t.Fatalf("expected snapshot after unregister")
	}
	if _, exists := state.Connected["client"]; exists {
		t.Fatalf("expected client removed, got %+v", state.Connected)
	}
}
