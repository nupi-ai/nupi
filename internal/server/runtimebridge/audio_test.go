package runtimebridge

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/session"
)

func TestAudioIngressProvider(t *testing.T) {
	if AudioIngressProvider(nil) != nil {
		t.Fatalf("expected nil provider for nil service")
	}

	bus := eventbus.New()
	svc := ingress.New(bus)
	provider := AudioIngressProvider(svc)
	if provider == nil {
		t.Fatalf("expected provider instance")
	}

	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}

	stream, err := provider.OpenStream("session", "primary", format, map[string]string{"origin": "test"})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}

	if err := stream.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	stream, err = provider.OpenStream("session", "primary", format, nil)
	if err != nil {
		t.Fatalf("OpenStream second attempt returned error: %v", err)
	}
	defer stream.Close()

	if _, err := provider.OpenStream("session", "primary", format, nil); !errors.Is(err, server.ErrAudioStreamExists) {
		t.Fatalf("expected ErrAudioStreamExists, got %v", err)
	}
}

func TestAudioEgressController(t *testing.T) {
	if AudioEgressController(nil) != nil {
		t.Fatalf("expected nil controller for nil service")
	}

	bus := eventbus.New()
	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 44100,
		Channels:   2,
		BitDepth:   16,
	}

	speakCh := make(chan egress.SpeakRequest, 1)
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	factory := egress.FactoryFunc(func(ctx context.Context, params egress.SessionParams) (egress.Synthesizer, error) {
		return &stubSynth{requests: speakCh}, nil
	})

	svc := egress.New(
		bus,
		egress.WithFactory(factory),
		egress.WithLogger(logger),
		egress.WithAudioFormat(format),
		egress.WithStreamID("tts.custom"),
	)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("tts service start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	controller := AudioEgressController(svc)
	if controller.DefaultStreamID() != "tts.custom" {
		t.Fatalf("unexpected stream id: %s", controller.DefaultStreamID())
	}
	if controller.PlaybackFormat() != format {
		t.Fatalf("unexpected playback format: %+v", controller.PlaybackFormat())
	}

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Speak, "", eventbus.ConversationSpeakEvent{
		SessionID: "sess",
		PromptID:  "prompt",
		Text:      "hello",
	})

	select {
	case req := <-speakCh:
		if req.SessionID != "sess" {
			t.Fatalf("unexpected session id in synth request: %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for synthesizer invocation")
	}

	controller.Interrupt("sess", "", "barge", map[string]string{"reason": "test"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(logBuf.String(), "barge-in interrupt session=sess") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected interrupt log, got %q", logBuf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommandExecutorRejectsStoppedSession(t *testing.T) {
	// Skip if PTY is not available (sandboxed environments).
	p := pty.New()
	err := p.Start(pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "no such file or directory") {
			t.Skipf("PTY not available: %v", err)
		}
	}
	if p != nil {
		p.Stop(100 * time.Millisecond)
	}

	// Redirect session manager home to a temp dir.
	t.Setenv("HOME", t.TempDir())

	mgr := session.NewManager()

	// Create a session that exits immediately.
	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "exit 0"},
		Rows:    24,
		Cols:    80,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Wait for the session to reach StatusStopped.
	deadline := time.Now().Add(3 * time.Second)
	for sess.CurrentStatus() != session.StatusStopped {
		if time.Now().After(deadline) {
			t.Fatalf("session did not stop within deadline (status: %s)", sess.CurrentStatus())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Build the executor adapter and attempt a command.
	executor := CommandExecutor(mgr)
	defer executor.Stop()
	err = executor.QueueCommand(sess.ID, "echo hello", eventbus.ContentOrigin("test"))
	if err == nil {
		t.Fatal("expected QueueCommand to reject command on stopped session")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("expected error to mention 'stopped', got: %v", err)
	}
}

func TestOriginPriority(t *testing.T) {
	tests := []struct {
		origin   eventbus.ContentOrigin
		expected int
	}{
		{eventbus.OriginUser, 0},
		{eventbus.OriginTool, 1},
		{eventbus.OriginAI, 2},
		{eventbus.OriginSystem, 3},
		{eventbus.ContentOrigin("unknown"), 2}, // defaults to AI priority
	}

	for _, tt := range tests {
		got := originPriority(tt.origin)
		if got != tt.expected {
			t.Errorf("originPriority(%q) = %d, want %d", tt.origin, got, tt.expected)
		}
	}

	// Verify ordering: user < tool < ai < system
	if originPriority(eventbus.OriginUser) >= originPriority(eventbus.OriginTool) {
		t.Error("user should have higher priority (lower value) than tool")
	}
	if originPriority(eventbus.OriginTool) >= originPriority(eventbus.OriginAI) {
		t.Error("tool should have higher priority (lower value) than ai")
	}
	if originPriority(eventbus.OriginAI) >= originPriority(eventbus.OriginSystem) {
		t.Error("ai should have higher priority (lower value) than system")
	}
}

func TestCommandExecutorPriorityOrdering(t *testing.T) {
	// Verify priority queue ordering: user > tool > ai > system,
	// with FIFO within same priority.
	type entry struct {
		cmd    string
		origin eventbus.ContentOrigin
	}
	entries := []entry{
		{"system-cmd", eventbus.OriginSystem},
		{"ai-cmd-1", eventbus.OriginAI},
		{"user-cmd", eventbus.OriginUser},
		{"ai-cmd-2", eventbus.OriginAI},
		{"tool-cmd", eventbus.OriginTool},
	}

	pq := make(commandPriorityQueue, 0, len(entries))
	heap.Init(&pq)
	for i, e := range entries {
		heap.Push(&pq, commandEntry{
			sessionID: "test",
			command:   e.cmd,
			origin:    e.origin,
			priority:  originPriority(e.origin),
			seq:       uint64(i + 1),
		})
	}

	expected := []string{"user-cmd", "tool-cmd", "ai-cmd-1", "ai-cmd-2", "system-cmd"}
	for i, cmd := range expected {
		if pq.Len() == 0 {
			t.Fatalf("expected command %q at position %d, queue is empty", cmd, i)
		}
		got := heap.Pop(&pq).(commandEntry).command
		if got != cmd {
			t.Errorf("position %d: expected %q, got %q", i, cmd, got)
		}
	}
}

func TestCommandExecutorFIFOWithinSamePriority(t *testing.T) {
	// Verify FIFO ordering is preserved for entries at the same priority.
	cmds := []string{"first", "second", "third"}
	pq := make(commandPriorityQueue, 0, len(cmds))
	heap.Init(&pq)
	for i, cmd := range cmds {
		heap.Push(&pq, commandEntry{
			sessionID: "test",
			command:   cmd,
			origin:    eventbus.OriginAI,
			priority:  originPriority(eventbus.OriginAI),
			seq:       uint64(i + 1),
		})
	}

	for i := range cmds {
		got := heap.Pop(&pq).(commandEntry).command
		if got != cmds[i] {
			cmd := cmds[i]
			t.Errorf("position %d: expected %q, got %q (FIFO violated)", i, cmd, got)
		}
	}
}

func TestCommandExecutorStopGraceful(t *testing.T) {
	a := &commandExecutorAdapter{
		logger: log.New(io.Discard, "", 0),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}

	// Start drain goroutine.
	drainDone := make(chan struct{})
	go func() {
		a.drain()
		close(drainDone)
	}()

	// Stop should cause drain to exit.
	a.Stop()

	select {
	case <-drainDone:
		// OK - drain exited
	case <-time.After(2 * time.Second):
		t.Fatal("drain goroutine did not exit after Stop()")
	}

	// Double Stop should not panic.
	a.Stop()
}

func TestCommandExecutorRejectsAfterStop(t *testing.T) {
	a := &commandExecutorAdapter{
		logger: log.New(io.Discard, "", 0),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go a.drain()
	a.Stop()

	err := a.QueueCommand("any-session", "echo hello", eventbus.OriginAI)
	if err == nil {
		t.Fatal("expected QueueCommand to return error after Stop()")
	}
	if !strings.Contains(err.Error(), "executor is stopped") {
		t.Fatalf("expected 'executor is stopped' error, got: %v", err)
	}
}

// syncBuffer wraps bytes.Buffer with a mutex for concurrent-safe reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *syncBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

func TestCommandExecutorOriginLogging(t *testing.T) {
	// Skip if PTY is not available.
	p := pty.New()
	err := p.Start(pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "sleep 5"}})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "no such file or directory") {
			t.Skipf("PTY not available: %v", err)
		}
		t.Fatalf("PTY start: %v", err)
	}
	defer p.Stop(100 * time.Millisecond)

	t.Setenv("HOME", t.TempDir())
	mgr := session.NewManager()

	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 5"},
		Rows:    24,
		Cols:    80,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	var logBuf syncBuffer
	logger := log.New(&logBuf, "", 0)

	executor := &commandExecutorAdapter{
		manager: mgr,
		logger:  logger,
		notify:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go executor.drain()
	defer executor.Stop()

	if err := executor.QueueCommand(sess.ID, "echo hello", eventbus.OriginAI); err != nil {
		t.Fatalf("QueueCommand: %v", err)
	}

	// Wait for the command to be drained and logged.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(logBuf.String(), "origin=ai") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected log to contain origin=ai, got: %q", logBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !strings.Contains(logBuf.String(), "echo hello") {
		t.Errorf("expected log to contain command text, got: %q", logBuf.String())
	}
}

type stubSynth struct {
	requests chan<- egress.SpeakRequest
}

func (s *stubSynth) Speak(ctx context.Context, req egress.SpeakRequest) ([]egress.SynthesisChunk, error) {
	select {
	case s.requests <- req:
	default:
	}
	return []egress.SynthesisChunk{{
		Data:   []byte("audio"),
		Final:  true,
		Format: nil,
	}}, nil
}

func (s *stubSynth) Close(ctx context.Context) ([]egress.SynthesisChunk, error) {
	return nil, nil
}
