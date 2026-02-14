package main

import "testing"

func TestAdapterTypeForSlot(t *testing.T) {
	t.Helper()
	cases := map[string]string{
		"stt":      "stt",
		"ai":       "ai",
		"tts":      "tts",
		"vad":      "vad",
		" TUNNEL ": "tunnel",
		"":         "",
		"custom":   "custom",
		".invalid": "",
	}
	for input, want := range cases {
		if got := adapterTypeForSlot(input); got != want {
			t.Fatalf("adapterTypeForSlot(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFilterAdaptersForSlot(t *testing.T) {
	adapters := []adapterInfo{
		{ID: "adapter.ai", Type: "ai"},
		{ID: "adapter.stt", Type: "stt"},
		{ID: "adapter.tts", Type: "tts"},
	}

	filtered := filterAdaptersForSlot("stt", adapters)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 adapter for stt, got %d", len(filtered))
	}
	if filtered[0].ID != "adapter.stt" {
		t.Fatalf("unexpected filtered adapters: %+v", filtered)
	}

	if got := filterAdaptersForSlot("vad", adapters); len(got) != 0 {
		t.Fatalf("expected empty slice for vad, got %d adapters: %+v", len(got), got)
	}

	if got := filterAdaptersForSlot("", adapters); got != nil {
		t.Fatalf("expected nil for empty slot, got %+v", got)
	}
}

func TestResolveAdapterChoice(t *testing.T) {
	ordered := []adapterInfo{
		{ID: "adapter.stt", Name: "Mock STT", Type: "stt"},
	}
	all := append([]adapterInfo{}, ordered...)
	all = append(all, adapterInfo{ID: "adapter.ai", Name: "Mock AI", Type: "ai"})

	if id, ok := resolveAdapterChoice("1", ordered, all); !ok || id != "adapter.stt" {
		t.Fatalf("expected selection of adapter.stt, got %q (ok=%v)", id, ok)
	}

	if id, ok := resolveAdapterChoice("adapter.ai", ordered, all); !ok || id != "adapter.ai" {
		t.Fatalf("expected selection by id adapter.ai, got %q (ok=%v)", id, ok)
	}

	if _, ok := resolveAdapterChoice("3", ordered, all); ok {
		t.Fatalf("expected invalid index to fail")
	}

	if _, ok := resolveAdapterChoice("unknown", ordered, all); ok {
		t.Fatalf("expected unknown adapter to fail")
	}

	if id, ok := resolveAdapterChoice("Mock AI", ordered, all); !ok || id != "adapter.ai" {
		t.Fatalf("expected selection by name Mock AI, got %q (ok=%v)", id, ok)
	}
}
