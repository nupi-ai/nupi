package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	defaultAudioFormat     = "pcm_s16le"
	defaultVADThreshold    = float32(0.5)
	updateAudioSettingsSQL = `
INSERT INTO audio_settings (
    instance_name,
    profile_name,
    capture_device,
    playback_device,
    preferred_format,
    vad_threshold,
    metadata,
    updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT(instance_name, profile_name) DO UPDATE SET
    capture_device = excluded.capture_device,
    playback_device = excluded.playback_device,
    preferred_format = excluded.preferred_format,
    vad_threshold = excluded.vad_threshold,
    metadata = excluded.metadata,
    updated_at = STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now')
`
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
        SELECT capture_device, playback_device, preferred_format, vad_threshold, metadata, updated_at
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
	if s.readOnly {
		return fmt.Errorf("config: save audio settings: store opened read-only")
	}

	captureDevice := strings.TrimSpace(settings.CaptureDevice)
	playbackDevice := strings.TrimSpace(settings.PlaybackDevice)
	preferredFormat := strings.TrimSpace(settings.PreferredFormat)
	if preferredFormat == "" {
		preferredFormat = defaultAudioFormat
	}

	var metadata interface{}
	if settings.Metadata.Valid {
		meta := strings.TrimSpace(settings.Metadata.String)
		if meta != "" {
			metadata = meta
		}
	}

	vad := settings.VADThreshold
	if vad < 0 || vad > 1 || (vad != vad) { // NaN check via self compare
		vad = defaultVADThreshold
	}

	_, err := s.db.ExecContext(
		ctx,
		updateAudioSettingsSQL,
		s.instanceName,
		s.profileName,
		captureDevice,
		playbackDevice,
		preferredFormat,
		float64(vad),
		metadata,
	)
	if err != nil {
		return fmt.Errorf("config: save audio settings: %w", err)
	}
	return nil
}
