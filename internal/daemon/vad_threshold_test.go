package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/vad"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

func TestLoadVADLatencyThreshold_NilStore(t *testing.T) {
	d, ok := loadVADLatencyThreshold(nil)
	if ok || d != 0 {
		t.Fatalf("expected (0, false), got (%v, %v)", d, ok)
	}
}

func TestLoadVADLatencyThreshold_NoMetadata(t *testing.T) {
	store := openTestStore(t)
	d, ok := loadVADLatencyThreshold(store)
	if ok || d != 0 {
		t.Fatalf("expected (0, false) for default settings, got (%v, %v)", d, ok)
	}
}

func TestLoadVADLatencyThreshold_Float64Value(t *testing.T) {
	store := openTestStore(t)
	saveAudioMetadata(t, store, `{"vad_latency_p99_threshold_ms": 50}`)

	d, ok := loadVADLatencyThreshold(store)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if d != 50*time.Millisecond {
		t.Fatalf("expected 50ms, got %v", d)
	}
}

func TestLoadVADLatencyThreshold_StringValue(t *testing.T) {
	store := openTestStore(t)
	saveAudioMetadata(t, store, `{"vad_latency_p99_threshold_ms": "200"}`)

	d, ok := loadVADLatencyThreshold(store)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if d != 200*time.Millisecond {
		t.Fatalf("expected 200ms, got %v", d)
	}
}

func TestLoadVADLatencyThreshold_ZeroValue(t *testing.T) {
	store := openTestStore(t)
	saveAudioMetadata(t, store, `{"vad_latency_p99_threshold_ms": 0}`)

	_, ok := loadVADLatencyThreshold(store)
	if ok {
		t.Fatal("expected ok=false for zero threshold")
	}
}

func TestLoadVADLatencyThreshold_NegativeValue(t *testing.T) {
	store := openTestStore(t)
	saveAudioMetadata(t, store, `{"vad_latency_p99_threshold_ms": -10}`)

	_, ok := loadVADLatencyThreshold(store)
	if ok {
		t.Fatal("expected ok=false for negative threshold")
	}
}

func TestLoadVADLatencyThreshold_MissingKey(t *testing.T) {
	store := openTestStore(t)
	saveAudioMetadata(t, store, `{"other_key": 100}`)

	_, ok := loadVADLatencyThreshold(store)
	if ok {
		t.Fatal("expected ok=false when key is missing")
	}
}

func TestLoadVADLatencyThreshold_InvalidJSON(t *testing.T) {
	store := openTestStore(t)
	saveAudioMetadata(t, store, `not-json`)

	_, ok := loadVADLatencyThreshold(store)
	if ok {
		t.Fatal("expected ok=false for invalid JSON")
	}
}

func openTestStore(t *testing.T) *configstore.Store {
	t.Helper()
	s, err := configstore.Open(configstore.Options{DBPath: filepath.Join(t.TempDir(), "config.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLoadVADLatencyThreshold_BooleanValue(t *testing.T) {
	store := openTestStore(t)
	saveAudioMetadata(t, store, `{"vad_latency_p99_threshold_ms": true}`)

	_, ok := loadVADLatencyThreshold(store)
	if ok {
		t.Fatal("expected ok=false for unsupported boolean type")
	}
}

func TestMapVADLatencySnapshot(t *testing.T) {
	input := vad.LatencyHistogramSnapshot{
		Bounds: []time.Duration{
			time.Millisecond,
			5 * time.Millisecond,
			10 * time.Millisecond,
		},
		Counts: []uint64{3, 7, 2},
		Sum:    42500 * time.Microsecond, // 0.0425 seconds
		Count:  12,
	}

	result := mapVADLatencySnapshot(input)

	if len(result.Bounds) != 3 {
		t.Fatalf("expected 3 bounds, got %d", len(result.Bounds))
	}
	if result.Bounds[0] != 0.001 {
		t.Fatalf("expected bounds[0]=0.001, got %f", result.Bounds[0])
	}
	if result.Bounds[1] != 0.005 {
		t.Fatalf("expected bounds[1]=0.005, got %f", result.Bounds[1])
	}
	if result.Bounds[2] != 0.01 {
		t.Fatalf("expected bounds[2]=0.01, got %f", result.Bounds[2])
	}
	if result.Count != 12 {
		t.Fatalf("expected count=12, got %d", result.Count)
	}
	if result.Counts[0] != 3 || result.Counts[1] != 7 || result.Counts[2] != 2 {
		t.Fatalf("unexpected counts: %v", result.Counts)
	}
	expectedSum := 0.0425
	if diff := result.Sum - expectedSum; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("expected sum≈%f, got %f", expectedSum, result.Sum)
	}
}

func TestMapVADLatencySnapshotEmpty(t *testing.T) {
	input := vad.LatencyHistogramSnapshot{}
	result := mapVADLatencySnapshot(input)

	if len(result.Bounds) != 0 || len(result.Counts) != 0 || result.Count != 0 || result.Sum != 0 {
		t.Fatalf("expected zero snapshot, got %+v", result)
	}
}

func saveAudioMetadata(t *testing.T, store *configstore.Store, metadata string) {
	t.Helper()
	ctx := context.Background()
	settings, err := store.LoadAudioSettings(ctx)
	if err != nil {
		t.Fatalf("load audio settings: %v", err)
	}
	settings.Metadata = sql.NullString{String: metadata, Valid: true}
	if err := store.SaveAudioSettings(ctx, settings); err != nil {
		t.Fatalf("save audio settings: %v", err)
	}
}
