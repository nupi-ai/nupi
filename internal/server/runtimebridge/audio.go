package runtimebridge

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/intentrouter"
	"github.com/nupi-ai/nupi/internal/plugins"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
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

// PluginWarningsProvider wraps plugins.Service for use by API handlers.
func PluginWarningsProvider(service *plugins.Service) server.PluginWarningsProvider {
	if service == nil {
		return nil
	}
	return &pluginWarningsAdapter{service: service}
}

type pluginWarningsAdapter struct {
	service *plugins.Service
}

func (a *pluginWarningsAdapter) GetDiscoveryWarnings() []server.PluginDiscoveryWarning {
	warnings := a.service.GetDiscoveryWarnings()
	if len(warnings) == 0 {
		return nil
	}

	result := make([]server.PluginDiscoveryWarning, len(warnings))
	for i, w := range warnings {
		result[i] = server.PluginDiscoveryWarning{
			Dir:   w.Dir,
			Error: formatError(w.Err),
		}
	}
	return result
}

func formatError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// PluginWarningsProviderFromManifest creates a provider directly from manifest warnings (for testing).
func PluginWarningsProviderFromManifest(warnings []manifest.DiscoveryWarning) server.PluginWarningsProvider {
	result := make([]server.PluginDiscoveryWarning, len(warnings))
	for i, w := range warnings {
		result[i] = server.PluginDiscoveryWarning{
			Dir:   w.Dir,
			Error: formatError(w.Err),
		}
	}
	return &staticWarningsProvider{warnings: result}
}

type staticWarningsProvider struct {
	warnings []server.PluginDiscoveryWarning
}

func (p *staticWarningsProvider) GetDiscoveryWarnings() []server.PluginDiscoveryWarning {
	return p.warnings
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
		notify:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go a.drain()
	return a
}

type commandExecutorAdapter struct {
	manager *session.Manager
	logger  *log.Logger

	mu    sync.Mutex
	queue []commandEntry
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
	a.seq++
	a.queue = append(a.queue, commandEntry{
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

		a.drainBatch()
	}
}

// drainBatch takes a snapshot of the current queue, sorts it once by priority
// (stable FIFO within same priority), and executes each entry. Checks the done
// channel between items so Stop() is respected promptly.
func (a *commandExecutorAdapter) drainBatch() {
	a.mu.Lock()
	if len(a.queue) == 0 {
		a.mu.Unlock()
		return
	}
	batch := a.queue
	a.queue = nil
	a.mu.Unlock()

	// Sort once for the entire batch.
	sortByPriority(batch)

	for i, entry := range batch {
		// Check for shutdown between items.
		select {
		case <-a.done:
			a.logger.Printf("[CommandExecutor] shutdown: dropping %d remaining commands", len(batch)-i)
			return
		default:
		}

		a.logger.Printf("[CommandExecutor] executing command origin=%s session=%s cmd=%q",
			entry.origin, entry.sessionID, entry.command)

		data := []byte(entry.command + "\n")
		if err := a.manager.WriteToSession(entry.sessionID, data); err != nil {
			a.logger.Printf("[CommandExecutor] write failed session=%s: %v", entry.sessionID, err)
		}
	}
}

// sortByPriority sorts entries by priority ascending, then by seq ascending
// for FIFO within the same priority level.
func sortByPriority(entries []commandEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].priority != entries[j].priority {
			return entries[i].priority < entries[j].priority
		}
		return entries[i].seq < entries[j].seq
	})
}

