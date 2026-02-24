package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/voice/slots"
)

// VoiceReadiness summarises whether voice capture and playback features can be used.
type VoiceReadiness struct {
	CaptureEnabled  bool
	PlaybackEnabled bool
}

// VoiceReadiness inspects adapter bindings required for voice features.
func (s *Store) VoiceReadiness(ctx context.Context) (VoiceReadiness, error) {
	if s == nil || s.db == nil {
		return VoiceReadiness{}, sql.ErrConnDone
	}

	sttReady, err := s.voiceSlotReady(ctx, slots.STT)
	if err != nil {
		return VoiceReadiness{}, err
	}
	ttsReady, err := s.voiceSlotReady(ctx, slots.TTS)
	if err != nil {
		return VoiceReadiness{}, err
	}
	readiness := VoiceReadiness{
		CaptureEnabled:  sttReady,
		PlaybackEnabled: ttsReady,
	}
	return readiness, nil
}

func (s *Store) voiceSlotReady(ctx context.Context, slot string) (bool, error) {
	binding, err := s.AdapterBinding(ctx, slot)
	if err != nil {
		switch {
		case errorsAsNotFound(err):
			return false, nil
		default:
			return false, err
		}
	}

	adapterID := ""
	if binding.AdapterID != nil {
		adapterID = strings.TrimSpace(*binding.AdapterID)
	}

	if adapterID == "" {
		return false, nil
	}

	exists, err := s.AdapterExists(ctx, adapterID)
	if err != nil {
		return false, fmt.Errorf("config: adapter exist check for %s: %w", adapterID, err)
	}
	if !exists {
		return false, nil
	}

	status := strings.TrimSpace(binding.Status)
	if status != "" && status != BindingStatusActive {
		return false, nil
	}

	return true, nil
}

func errorsAsNotFound(err error) bool {
	var notFoundErr NotFoundError
	return errors.As(err, &notFoundErr) || errors.Is(err, sql.ErrNoRows)
}
