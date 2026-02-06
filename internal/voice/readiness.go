package voice

import (
	"context"
	"fmt"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

// voiceSlots are the adapter slots required for full voice pipeline operation.
var voiceSlots = []string{slots.STT, slots.TTS, slots.VAD}

// VoiceSlots returns the adapter slots required for full voice pipeline operation.
func VoiceSlots() []string {
	out := make([]string, len(voiceSlots))
	copy(out, voiceSlots)
	return out
}

// SlotStatus reports the availability of a single voice adapter slot.
type SlotStatus struct {
	Slot      string
	Available bool
	AdapterID string
}

// ReadinessReport summarizes voice pipeline adapter availability.
type ReadinessReport struct {
	Slots []SlotStatus
}

// AllAvailable returns true if every voice slot has an active adapter.
func (r ReadinessReport) AllAvailable() bool {
	for _, s := range r.Slots {
		if !s.Available {
			return false
		}
	}
	return len(r.Slots) > 0
}

// SlotByName returns the status for a specific slot, or nil if not found.
func (r ReadinessReport) SlotByName(name string) *SlotStatus {
	for i := range r.Slots {
		if r.Slots[i].Slot == name {
			return &r.Slots[i]
		}
	}
	return nil
}

// CheckReadiness queries the config store for active voice adapter bindings
// and reports per-slot availability.
func CheckReadiness(ctx context.Context, store *configstore.Store) (ReadinessReport, error) {
	if store == nil {
		return ReadinessReport{}, fmt.Errorf("voice readiness: store is nil")
	}
	bindings, err := store.ListAdapterBindings(ctx)
	if err != nil {
		return ReadinessReport{}, fmt.Errorf("voice readiness: %w", err)
	}

	active := make(map[string]string) // slot → adapterID
	for _, b := range bindings {
		if b.Status == configstore.BindingStatusActive && b.AdapterID != nil && *b.AdapterID != "" {
			active[b.Slot] = *b.AdapterID
		}
	}

	var report ReadinessReport
	for _, slot := range voiceSlots {
		s := SlotStatus{Slot: slot}
		if adapterID, ok := active[slot]; ok {
			s.Available = true
			s.AdapterID = adapterID
		}
		report.Slots = append(report.Slots, s)
	}

	return report, nil
}
