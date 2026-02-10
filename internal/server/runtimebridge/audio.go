package runtimebridge

import (
	"errors"
	"fmt"

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

// CommandExecutor wraps session.Manager for the intentrouter.CommandExecutor interface.
func CommandExecutor(manager *session.Manager) intentrouter.CommandExecutor {
	if manager == nil {
		return nil
	}
	return &commandExecutorAdapter{manager: manager}
}

type commandExecutorAdapter struct {
	manager *session.Manager
}

func (a *commandExecutorAdapter) QueueCommand(sessionID string, command string, origin eventbus.ContentOrigin) error {
	// Validate session state before executing
	sess, err := a.manager.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("command rejected: %w", err)
	}
	if status := sess.CurrentStatus(); status == session.StatusStopped {
		return fmt.Errorf("command rejected: session %s is stopped", sessionID)
	}

	data := []byte(command + "\n")
	return a.manager.WriteToSession(sessionID, data)
}

