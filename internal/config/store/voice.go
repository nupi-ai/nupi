package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/voice/slots"
)

// VoiceIssue describes a configuration problem preventing voice features from working.
type VoiceIssue struct {
	Slot    string
	Code    string
	Message string
}

// VoiceReadiness summarises whether voice capture and playback features can be used.
type VoiceReadiness struct {
	CaptureEnabled  bool
	PlaybackEnabled bool
	Issues          []VoiceIssue
}

const (
	voiceIssueSlotMissing     = "slot_missing"
	voiceIssueAdapterMissing  = "adapter_missing"
	voiceIssueAdapterUnbound  = "adapter_unassigned"
	voiceIssueAdapterInactive = "adapter_inactive"
)

// VoiceReadiness inspects adapter bindings required for voice features.
func (s *Store) VoiceReadiness(ctx context.Context) (VoiceReadiness, error) {
	if s == nil || s.db == nil {
		return VoiceReadiness{}, sql.ErrConnDone
	}

	var (
		issues []VoiceIssue
	)

	sttIssues, err := s.voiceSlotIssues(ctx, slots.STT)
	if err != nil {
		return VoiceReadiness{}, err
	}
	issues = append(issues, sttIssues...)

	ttsIssues, err := s.voiceSlotIssues(ctx, slots.TTS)
	if err != nil {
		return VoiceReadiness{}, err
	}
	issues = append(issues, ttsIssues...)

	readiness := VoiceReadiness{
		CaptureEnabled:  len(sttIssues) == 0,
		PlaybackEnabled: len(ttsIssues) == 0,
		Issues:          issues,
	}
	return readiness, nil
}

func (s *Store) voiceSlotIssues(ctx context.Context, slot string) ([]VoiceIssue, error) {
	binding, err := s.AdapterBinding(ctx, slot)
	if err != nil {
		var issues []VoiceIssue
		switch {
		case errorsAsNotFound(err):
			issues = append(issues, VoiceIssue{
				Slot:    slot,
				Code:    voiceIssueSlotMissing,
				Message: fmt.Sprintf("adapter slot %s is missing from configuration", slot),
			})
			return issues, nil
		default:
			return nil, err
		}
	}

	var issues []VoiceIssue

	adapterID := ""
	if binding.AdapterID != nil {
		adapterID = strings.TrimSpace(*binding.AdapterID)
	}

	if adapterID == "" {
		issues = append(issues, VoiceIssue{
			Slot:    slot,
			Code:    voiceIssueAdapterUnbound,
			Message: fmt.Sprintf("adapter slot %s has no adapter assigned", slot),
		})
		return issues, nil
	}

	exists, err := s.AdapterExists(ctx, adapterID)
	if err != nil {
		return nil, fmt.Errorf("config: adapter exist check for %s: %w", adapterID, err)
	}
	if !exists {
		issues = append(issues, VoiceIssue{
			Slot:    slot,
			Code:    voiceIssueAdapterMissing,
			Message: fmt.Sprintf("adapter %s referenced by slot %s is not installed", adapterID, slot),
		})
	}

	status := strings.TrimSpace(binding.Status)
	if status != "" && status != BindingStatusActive {
		issues = append(issues, VoiceIssue{
			Slot:    slot,
			Code:    voiceIssueAdapterInactive,
			Message: fmt.Sprintf("adapter slot %s is %s (expected active)", slot, status),
		})
	}

	return issues, nil
}

func errorsAsNotFound(err error) bool {
	var notFoundErr NotFoundError
	return errors.As(err, &notFoundErr) || errors.Is(err, sql.ErrNoRows)
}
