package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
)

const audioSettingsSnapshotColumns = "capture_device, playback_device, preferred_format, vad_threshold, IFNULL(metadata, ''), IFNULL(updated_at, '')"
const maxUpdatedAtExpr = "IFNULL(MAX(updated_at), '')"

// ChangeSnapshot captures update markers for configuration tables.
type ChangeSnapshot struct {
	Settings         string
	AudioSettings    string
	Adapters         string
	AdapterBindings  string
	AdapterEndpoints string
}

// ChangeEvent describes modified configuration groups since the last snapshot.
type ChangeEvent struct {
	SettingsChanged         bool
	AudioSettingsChanged    bool
	AdaptersChanged         bool
	AdapterBindingsChanged  bool
	AdapterEndpointsChanged bool
	Snapshot                ChangeSnapshot
}

// Changed returns true when at least one tracked group changed.
func (e ChangeEvent) Changed() bool {
	return e.SettingsChanged || e.AudioSettingsChanged || e.AdaptersChanged || e.AdapterBindingsChanged || e.AdapterEndpointsChanged
}

// Watch polls the configuration store for changes and emits events on the returned channel.
// The caller must cancel ctx to terminate the watcher. The provided interval is clamped to
// a minimum of 500ms to avoid excessive polling.
func (s *Store) Watch(ctx context.Context, interval time.Duration) (<-chan ChangeEvent, error) {
	if s == nil {
		return nil, sql.ErrConnDone
	}

	if interval <= 0 {
		interval = constants.Duration1Second
	}
	if interval < constants.Duration500Milliseconds {
		interval = constants.Duration500Milliseconds
	}

	out := make(chan ChangeEvent, 1)

	initial, err := s.snapshot(ctx)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(out)

		last := initial
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, err := s.snapshot(ctx)
				if err != nil {
					continue
				}

				ev := diffSnapshots(last, next)
				if ev.Changed() {
					out <- ev
					last = next
				}
			}
		}
	}()

	return out, nil
}

func (s *Store) snapshot(ctx context.Context) (ChangeSnapshot, error) {
	var snap ChangeSnapshot
	if err := s.db.QueryRowContext(ctx, `
        SELECT `+maxUpdatedAtExpr+`
        FROM settings
        WHERE instance_name = ? AND profile_name = ?
    `, s.instanceName, s.profileName).Scan(&snap.Settings); err != nil {
		return ChangeSnapshot{}, err
	}

	var (
		captureDevice   string
		playbackDevice  string
		preferredFormat string
		vad             float64
		metadata        string
		audioUpdated    string
	)

	err := s.db.QueryRowContext(ctx, `
        SELECT `+audioSettingsSnapshotColumns+`
        FROM audio_settings
        WHERE instance_name = ? AND profile_name = ?
    `, s.instanceName, s.profileName).Scan(&captureDevice, &playbackDevice, &preferredFormat, &vad, &metadata, &audioUpdated)
	switch {
	case err == sql.ErrNoRows:
		snap.AudioSettings = ""
	case err != nil:
		return ChangeSnapshot{}, err
	default:
		snap.AudioSettings = fmt.Sprintf("%s|%s|%s|%.6f|%s|%s", captureDevice, playbackDevice, preferredFormat, vad, metadata, audioUpdated)
	}

	if err := s.db.QueryRowContext(ctx, `
        SELECT `+maxUpdatedAtExpr+`
        FROM adapters
    `).Scan(&snap.Adapters); err != nil {
		return ChangeSnapshot{}, err
	}

	if err := s.db.QueryRowContext(ctx, `
        SELECT `+maxUpdatedAtExpr+`
        FROM adapter_bindings
        WHERE instance_name = ? AND profile_name = ?
    `, s.instanceName, s.profileName).Scan(&snap.AdapterBindings); err != nil {
		return ChangeSnapshot{}, err
	}

	if err := s.db.QueryRowContext(ctx, `
        SELECT `+maxUpdatedAtExpr+`
        FROM adapter_endpoints
    `).Scan(&snap.AdapterEndpoints); err != nil {
		return ChangeSnapshot{}, err
	}

	return snap, nil
}

func diffSnapshots(prev, curr ChangeSnapshot) ChangeEvent {
	return ChangeEvent{
		SettingsChanged:         curr.Settings != prev.Settings,
		AudioSettingsChanged:    curr.AudioSettings != prev.AudioSettings,
		AdaptersChanged:         curr.Adapters != prev.Adapters,
		AdapterBindingsChanged:  curr.AdapterBindings != prev.AdapterBindings,
		AdapterEndpointsChanged: curr.AdapterEndpoints != prev.AdapterEndpoints,
		Snapshot:                curr,
	}
}
