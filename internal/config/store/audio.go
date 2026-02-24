package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

const (
	defaultAudioFormat   = "pcm_s16le"
	defaultVADThreshold  = float32(0.5)
	audioSettingsColumns = "capture_device, playback_device, preferred_format, vad_threshold, metadata, updated_at"
)

var updateAudioSettingsSQL = buildUpsertSQL(
	"audio_settings",
	[]string{
		"instance_name",
		"profile_name",
		"capture_device",
		"playback_device",
		"preferred_format",
		"vad_threshold",
		"metadata",
	},
	[]string{"instance_name", "profile_name"},
	[]string{
		"capture_device",
		"playback_device",
		"preferred_format",
		"vad_threshold",
		"metadata",
	},
	upsertOptions{
		InsertUpdatedAt: true,
		UpdateUpdatedAt: true,
		UpdatedAtExpr:   "STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now')",
	},
)

// AudioSettings captures per-profile audio configuration values.
type AudioSettings struct {
	CaptureDevice   string
	PlaybackDevice  string
	PreferredFormat string
	VADThreshold    float32
	Metadata        sql.NullString
	UpdatedAt       string
}

func defaultAudioSettings() AudioSettings {
	return AudioSettings{
		CaptureDevice:   "",
		PlaybackDevice:  "",
		PreferredFormat: defaultAudioFormat,
		VADThreshold:    defaultVADThreshold,
		Metadata:        sql.NullString{},
		UpdatedAt:       "",
	}
}

func normalizeAudioSettings(settings AudioSettings) (AudioSettings, bool) {
	normalized := settings
	changed := false

	if trimmed := strings.TrimSpace(settings.CaptureDevice); trimmed != settings.CaptureDevice {
		normalized.CaptureDevice = trimmed
		changed = true
	}

	if trimmed := strings.TrimSpace(settings.PlaybackDevice); trimmed != settings.PlaybackDevice {
		normalized.PlaybackDevice = trimmed
		changed = true
	}

	if trimmed := strings.TrimSpace(settings.PreferredFormat); trimmed == "" {
		if settings.PreferredFormat != defaultAudioFormat {
			normalized.PreferredFormat = defaultAudioFormat
			changed = true
		}
	} else if trimmed != settings.PreferredFormat {
		normalized.PreferredFormat = trimmed
		changed = true
	}

	vad := settings.VADThreshold
	if math.IsNaN(float64(vad)) || vad < 0 || vad > 1 {
		normalized.VADThreshold = defaultVADThreshold
		changed = true
	}

	if settings.Metadata.Valid {
		trimmed := strings.TrimSpace(settings.Metadata.String)
		if trimmed == "" {
			normalized.Metadata = sql.NullString{}
			changed = true
		} else if trimmed != settings.Metadata.String {
			normalized.Metadata = sql.NullString{String: trimmed, Valid: true}
			changed = true
		}
	} else {
		// Metadata already empty; keep as-is (no change to 'changed' flag).
		normalized.Metadata = sql.NullString{}
	}

	return normalized, changed
}

// LoadAudioSettings returns audio preferences for the active profile.
func (s *Store) LoadAudioSettings(ctx context.Context) (AudioSettings, error) {
	if s == nil || s.db == nil {
		return AudioSettings{}, sql.ErrConnDone
	}

	var (
		settings AudioSettings
		vadFloat float64
	)

	err := s.db.QueryRowContext(ctx, `
        SELECT `+audioSettingsColumns+`
        FROM audio_settings
        WHERE instance_name = ? AND profile_name = ?
    `, s.instanceName, s.profileName).Scan(
		&settings.CaptureDevice,
		&settings.PlaybackDevice,
		&settings.PreferredFormat,
		&vadFloat,
		&settings.Metadata,
		&settings.UpdatedAt,
	)
	switch {
	case err == sql.ErrNoRows:
		return defaultAudioSettings(), nil
	case err != nil:
		return AudioSettings{}, fmt.Errorf("config: load audio settings: %w", err)
	default:
		settings.VADThreshold = float32(vadFloat)
		return settings, nil
	}
}

// SaveAudioSettings upserts audio preferences for the active profile.
func (s *Store) SaveAudioSettings(ctx context.Context, settings AudioSettings) error {
	if s == nil || s.db == nil {
		return sql.ErrConnDone
	}
	if err := s.ensureWritable("save audio settings"); err != nil {
		return err
	}

	normalized, _ := normalizeAudioSettings(settings)

	var metadata interface{}
	if normalized.Metadata.Valid {
		metadata = normalized.Metadata.String
	}

	_, err := s.db.ExecContext(
		ctx,
		updateAudioSettingsSQL,
		s.instanceName,
		s.profileName,
		normalized.CaptureDevice,
		normalized.PlaybackDevice,
		normalized.PreferredFormat,
		float64(normalized.VADThreshold),
		metadata,
	)
	if err != nil {
		return fmt.Errorf("config: save audio settings: %w", err)
	}
	return nil
}
