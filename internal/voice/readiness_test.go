package voice

import (
	"context"
	"testing"

	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/testutil"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

func TestVoiceReadinessAllAdapters(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	// Activate all three voice adapters.
	for _, tc := range []struct {
		slot, adapterID string
	}{
		{slots.STT, adapters.MockSTTAdapterID},
		{slots.TTS, adapters.MockTTSAdapterID},
		{slots.VAD, adapters.MockVADAdapterID},
	} {
		if err := store.SetActiveAdapter(ctx, tc.slot, tc.adapterID, nil); err != nil {
			t.Fatalf("activate %s: %v", tc.slot, err)
		}
	}

	report, err := CheckReadiness(ctx, store)
	if err != nil {
		t.Fatalf("check readiness: %v", err)
	}

	if !report.AllAvailable() {
		t.Errorf("expected all slots available, got: %+v", report.Slots)
	}

	for _, slot := range VoiceSlots() {
		s := report.SlotByName(slot)
		if s == nil {
			t.Errorf("missing slot %s in report", slot)
			continue
		}
		if !s.Available {
			t.Errorf("slot %s should be available", slot)
		}
		if s.AdapterID == "" {
			t.Errorf("slot %s should have adapter ID", slot)
		}
	}
}

func TestVoiceReadinessMissingAdapter(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	// Activate only STT and TTS (no VAD).
	if err := store.SetActiveAdapter(ctx, slots.STT, adapters.MockSTTAdapterID, nil); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, slots.TTS, adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	report, err := CheckReadiness(ctx, store)
	if err != nil {
		t.Fatalf("check readiness: %v", err)
	}

	if report.AllAvailable() {
		t.Errorf("expected NOT all available (VAD missing), got: %+v", report.Slots)
	}

	sttStatus := report.SlotByName(slots.STT)
	if sttStatus == nil || !sttStatus.Available {
		t.Error("STT should be available")
	}

	ttsStatus := report.SlotByName(slots.TTS)
	if ttsStatus == nil || !ttsStatus.Available {
		t.Error("TTS should be available")
	}

	vadStatus := report.SlotByName(slots.VAD)
	if vadStatus == nil {
		t.Fatal("VAD slot missing from report")
	}
	if vadStatus.Available {
		t.Error("VAD should NOT be available")
	}
	if vadStatus.AdapterID != "" {
		t.Errorf("VAD adapter ID should be empty, got %q", vadStatus.AdapterID)
	}
}
