package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestAdapterTypeForSlot(t *testing.T) {
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

func TestOutputFormatterError(t *testing.T) {
	t.Run("json mode with error", func(t *testing.T) {
		// Capture stderr
		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		f := &OutputFormatter{jsonMode: true}
		retErr := f.Error("connection failed", io.EOF)

		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		io.Copy(&buf, r)

		// Verify returned error
		if retErr == nil {
			t.Fatal("expected non-nil error")
		}
		if !strings.Contains(retErr.Error(), "connection failed") {
			t.Errorf("returned error should contain message, got %q", retErr.Error())
		}

		// Verify JSON output on stderr
		output := strings.TrimSpace(buf.String())
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(output), &parsed); err != nil {
			t.Fatalf("expected valid JSON on stderr, got %q: %v", output, err)
		}

		// Must have "error" field
		if _, ok := parsed["error"]; !ok {
			t.Errorf("JSON output missing 'error' field: %s", output)
		}
		// Must NOT have "success" field
		if _, ok := parsed["success"]; ok {
			t.Errorf("JSON output should not have 'success' field: %s", output)
		}
		// Must have "details" field
		if _, ok := parsed["details"]; !ok {
			t.Errorf("JSON output missing 'details' field: %s", output)
		}
	})

	t.Run("json mode without underlying error", func(t *testing.T) {
		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		f := &OutputFormatter{jsonMode: true}
		retErr := f.Error("not found", nil)

		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		io.Copy(&buf, r)

		if retErr == nil {
			t.Fatal("expected non-nil error")
		}

		output := strings.TrimSpace(buf.String())
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(output), &parsed); err != nil {
			t.Fatalf("expected valid JSON on stderr, got %q: %v", output, err)
		}

		if _, ok := parsed["error"]; !ok {
			t.Errorf("JSON output missing 'error' field: %s", output)
		}
		// No "details" when err is nil
		if _, ok := parsed["details"]; ok {
			t.Errorf("JSON output should not have 'details' when err is nil: %s", output)
		}
		if _, ok := parsed["success"]; ok {
			t.Errorf("JSON output should not have 'success' field: %s", output)
		}
	})
}

func TestOutputFormatterSuccess(t *testing.T) {
	t.Run("json mode", func(t *testing.T) {
		// Capture stdout
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		f := &OutputFormatter{jsonMode: true}
		err := f.Success("adapter bound", map[string]interface{}{
			"slot": "stt",
		})

		w.Close()
		os.Stdout = oldStdout

		var buf bytes.Buffer
		io.Copy(&buf, r)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		output := strings.TrimSpace(buf.String())
		var parsed map[string]interface{}
		if jsonErr := json.Unmarshal([]byte(output), &parsed); jsonErr != nil {
			t.Fatalf("expected valid JSON on stdout, got %q: %v", output, jsonErr)
		}

		// Must have "message" field
		if msg, ok := parsed["message"]; !ok || msg != "adapter bound" {
			t.Errorf("expected message='adapter bound', got %v", parsed["message"])
		}
		// Must have extra data
		if slot, ok := parsed["slot"]; !ok || slot != "stt" {
			t.Errorf("expected slot='stt', got %v", parsed["slot"])
		}
		// Must NOT have "success" field
		if _, ok := parsed["success"]; ok {
			t.Errorf("JSON output should not have 'success' field: %s", output)
		}
	})
}
