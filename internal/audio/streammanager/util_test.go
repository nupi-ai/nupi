package streammanager

import (
	"testing"
)

func TestStreamKeyRoundTrip(t *testing.T) {
	key := StreamKey("sess-1", "mic")
	if key != "sess-1::mic" {
		t.Fatalf("unexpected key: %q", key)
	}
	sid, strid := SplitStreamKey(key)
	if sid != "sess-1" || strid != "mic" {
		t.Fatalf("unexpected split: sid=%q strid=%q", sid, strid)
	}
}

func TestSplitStreamKeyNoSep(t *testing.T) {
	sid, strid := SplitStreamKey("nodelimiter")
	if sid != "nodelimiter" || strid != "" {
		t.Fatalf("unexpected split: sid=%q strid=%q", sid, strid)
	}
}

func TestValidateRetryConfig(t *testing.T) {
	cfg := RetryConfig{Initial: 500, Max: 100}
	ValidateRetryConfig(&cfg)
	if cfg.Max != 500 {
		t.Fatalf("expected Max to be clamped to Initial, got %v", cfg.Max)
	}
}

func TestValidateRetryConfigZeroInitial(t *testing.T) {
	cfg := RetryConfig{Initial: 0, Max: 0}
	ValidateRetryConfig(&cfg)
	dflt := DefaultRetryConfig()
	if cfg.Initial != dflt.Initial {
		t.Fatalf("expected Initial to be set to default %v, got %v", dflt.Initial, cfg.Initial)
	}
	if cfg.Max < cfg.Initial {
		t.Fatalf("expected Max >= Initial, got Max=%v Initial=%v", cfg.Max, cfg.Initial)
	}
}
