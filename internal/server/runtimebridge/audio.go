package runtimebridge

import (
	"container/heap"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/intentrouter"
	"github.com/nupi-ai/nupi/internal/plugins"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/session"
)

// AudioIngressProvider wraps ingress.Service into the API-facing AudioCaptureProvider interface.
func AudioIngressProvider(service *ingress.Service) server.AudioCaptureProvider {
	if service == nil {
		return nil
	}
	return &audioIngressAdapter{service: service}
}

type audioIngressAdapter struct {
	service *ingress.Service
}

func (a *audioIngressAdapter) OpenStream(sessionID, streamID string, format eventbus.AudioFormat, metadata map[string]string) (server.AudioCaptureStream, error) {
	stream, err := a.service.OpenStream(sessionID, streamID, format, metadata)
	if err != nil {
		if errors.Is(err, ingress.ErrStreamExists) {
			return nil, server.ErrAudioStreamExists
		}
		return nil, err
	}
	return &audioIngressStream{stream: stream}, nil
}

type audioIngressStream struct {
	stream *ingress.Stream
}

func (s *audioIngressStream) Write(p []byte) error {
	return s.stream.Write(p)
}

func (s *audioIngressStream) Close() error {
	return s.stream.Close()
}

// AudioEgressController wraps egress.Service for use by API handlers.
func AudioEgressController(service *egress.Service) server.AudioPlaybackController {
	if service == nil {
		return nil
	}
	return &audioEgressAdapter{service: service}
}

type audioEgressAdapter struct {
	service *egress.Service
}

func (a *audioEgressAdapter) DefaultStreamID() string {
	return a.service.DefaultStreamID()
}

func (a *audioEgressAdapter) PlaybackFormat() eventbus.AudioFormat {
	return a.service.PlaybackFormat()
}

func (a *audioEgressAdapter) Interrupt(sessionID, streamID, reason string, metadata map[string]string) {
	a.service.Interrupt(sessionID, streamID, reason, metadata)
}

// PluginReloaderProvider wraps plugins.Service for use as a hot-reload trigger.
func PluginReloaderProvider(service *plugins.Service) server.PluginReloader {
	if service == nil {
		return nil
	}
	return service // plugins.Service already implements Reload() error
}

// SessionProvider wraps session.Manager for the intentrouter.SessionProvider interface.
func SessionProvider(manager *session.Manager) intentrouter.SessionProvider {
	if manager == nil {
		return nil
	}
	return &sessionProviderAdapter{manager: manager}
}

type sessionProviderAdapter struct {
	manager *session.Manager
}

func (a *sessionProviderAdapter) ListSessionInfos() []intentrouter.SessionInfo {
	sessions := a.manager.ListSessions()
	infos := make([]intentrouter.SessionInfo, len(sessions))
	for i, s := range sessions {
		infos[i] = intentrouter.SessionInfo{
			ID:        s.ID,
			Command:   s.Command,
			Args:      s.Args,
			WorkDir:   s.WorkDir,
			Tool:      s.GetDetectedTool(),
			Status:    string(s.CurrentStatus()),
			StartTime: s.StartTime,
		}
	}
	return infos
}

func (a *sessionProviderAdapter) GetSessionInfo(sessionID string) (intentrouter.SessionInfo, bool) {
	s, err := a.manager.GetSession(sessionID)
	if err != nil {
		return intentrouter.SessionInfo{}, false
	}
	return intentrouter.SessionInfo{
		ID:        s.ID,
		Command:   s.Command,
		Args:      s.Args,
		WorkDir:   s.WorkDir,
		Tool:      s.GetDetectedTool(),
		Status:    string(s.CurrentStatus()),
		StartTime: s.StartTime,
	}, true
}

func (a *sessionProviderAdapter) ValidateSession(sessionID string) error {
	_, err := a.manager.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return nil
}

// commandEntry holds a single queued command with its priority metadata.
type commandEntry struct {
	sessionID string
	command   string
	origin    eventbus.ContentOrigin
	priority  int
	seq       uint64 // monotonic sequence for stable FIFO within same priority
}

// originPriority maps a ContentOrigin to a numeric priority (lower = higher priority).
func originPriority(origin eventbus.ContentOrigin) int {
	switch origin {
	case eventbus.OriginUser:
		return 0 // highest
	case eventbus.OriginTool:
		return 1
	case eventbus.OriginAI:
		return 2
	case eventbus.OriginSystem:
		return 3 // lowest
	default:
		return 2 // treat unknown as AI
	}
}

// CommandExecutor wraps session.Manager for the intentrouter.CommandExecutor interface.
// It starts a background drain goroutine that executes commands in priority order.
func CommandExecutor(manager *session.Manager) intentrouter.CommandExecutor {
	if manager == nil {
		return nil
	}
	a := &commandExecutorAdapter{
		manager: manager,
		logger:  log.Default(),
		queue:   make(commandPriorityQueue, 0),
		notify:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	heap.Init(&a.queue)
	go a.drain()
	return a
}

type commandExecutorAdapter struct {
	manager *session.Manager
	logger  *log.Logger

	mu    sync.Mutex
	queue commandPriorityQueue
	seq   uint64

	notify   chan struct{} // signals new entry available
	done     chan struct{} // closed on Stop()
	stopOnce sync.Once
}

func (a *commandExecutorAdapter) QueueCommand(sessionID string, command string, origin eventbus.ContentOrigin) error {
	// Reject commands after executor has been stopped.
	select {
	case <-a.done:
		return fmt.Errorf("command rejected: executor is stopped")
	default:
	}

	// Validate session state before enqueuing.
	sess, err := a.manager.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("command rejected: %w", err)
	}
	if status := sess.CurrentStatus(); status == session.StatusStopped {
		return fmt.Errorf("command rejected: session %s is stopped", sessionID)
	}

	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return fmt.Errorf("command rejected: executor is stopped")
	default:
	}
	a.seq++
	heap.Push(&a.queue, commandEntry{
		sessionID: sessionID,
		command:   command,
		origin:    origin,
		priority:  originPriority(origin),
		seq:       a.seq,
	})
	a.mu.Unlock()

	// Non-blocking signal to the drain goroutine.
	select {
	case a.notify <- struct{}{}:
	default:
	}

	return nil
}

// Stop signals the drain goroutine to exit. Safe for concurrent callers.
func (a *commandExecutorAdapter) Stop() {
	a.stopOnce.Do(func() {
		close(a.done)
	})
}

// drain runs in a goroutine, picking the highest-priority entry from the queue
// and executing it via WriteToSession.
func (a *commandExecutorAdapter) drain() {
	for {
		select {
		case <-a.done:
			return
		case <-a.notify:
		}

		a.drainQueue()
	}
}

// drainQueue pops commands from the priority queue and executes them.
// It checks done between items and clears pending commands on shutdown.
func (a *commandExecutorAdapter) drainQueue() {
	for {
		select {
		case <-a.done:
			dropped := a.clearPending()
			a.logger.Printf("[CommandExecutor] shutdown: dropping %d remaining commands", dropped)
			return
		default:
		}

		entry, ok := a.popNext()
		if !ok {
			return
		}

		a.logger.Printf("[CommandExecutor] executing command origin=%s session=%s cmd=%q",
			entry.origin, entry.sessionID, entry.command)

		data := []byte(entry.command + "\n")
		if err := a.manager.WriteToSession(entry.sessionID, data); err != nil {
			a.logger.Printf("[CommandExecutor] write failed session=%s: %v", entry.sessionID, err)
		}
	}
}

func (a *commandExecutorAdapter) popNext() (commandEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queue) == 0 {
		return commandEntry{}, false
	}
	return heap.Pop(&a.queue).(commandEntry), true
}

func (a *commandExecutorAdapter) clearPending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	dropped := len(a.queue)
	a.queue = a.queue[:0]
	return dropped
}

type commandPriorityQueue []commandEntry

func (pq commandPriorityQueue) Len() int { return len(pq) }

func (pq commandPriorityQueue) Less(i, j int) bool {
	if pq[i].priority != pq[j].priority {
		return pq[i].priority < pq[j].priority
	}
	return pq[i].seq < pq[j].seq
}

func (pq commandPriorityQueue) Swap(i, j int) { pq[i], pq[j] = pq[j], pq[i] }

func (pq *commandPriorityQueue) Push(x any) {
	*pq = append(*pq, x.(commandEntry))
}

func (pq *commandPriorityQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[:n-1]
	return item
}
