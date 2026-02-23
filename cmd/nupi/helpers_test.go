package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/spf13/cobra"
)

func TestGetTrimmedFlag(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "name")
	if err := cmd.Flags().Set("name", "  demo  "); err != nil {
		t.Fatalf("failed to set flag: %v", err)
	}

	got := getTrimmedFlag(cmd, "name")
	if got != "demo" {
		t.Fatalf("getTrimmedFlag() = %q, want %q", got, "demo")
	}
}

func TestGetRequiredFlag(t *testing.T) {
	t.Run("returns value when set", func(t *testing.T) {
		cmd := &cobra.Command{}
		cmd.Flags().String("id", "", "id")
		if err := cmd.Flags().Set("id", "  adapter.local  "); err != nil {
			t.Fatalf("failed to set flag: %v", err)
		}

		out := &OutputFormatter{}
		got, err := getRequiredFlag(cmd, "id", out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "adapter.local" {
			t.Fatalf("getRequiredFlag() = %q, want %q", got, "adapter.local")
		}
	})

	t.Run("returns formatted error when empty", func(t *testing.T) {
		cmd := &cobra.Command{}
		cmd.Flags().String("id", "", "id")
		out := &OutputFormatter{}

		got, err := getRequiredFlag(cmd, "id", out)
		if got != "" {
			t.Fatalf("expected empty value, got %q", got)
		}
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "--id is required") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

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

func TestOutputFormatterRender(t *testing.T) {
	t.Run("json mode uses payload", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		called := false
		f := &OutputFormatter{jsonMode: true}
		err := f.Render(CommandResult{
			Data: map[string]any{"key": "value"},
			HumanReadable: func() error {
				called = true
				return nil
			},
		})

		w.Close()
		os.Stdout = oldStdout

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Fatalf("human-readable callback should not be called in JSON mode")
		}

		var buf bytes.Buffer
		io.Copy(&buf, r)
		output := strings.TrimSpace(buf.String())
		var parsed map[string]any
		if jsonErr := json.Unmarshal([]byte(output), &parsed); jsonErr != nil {
			t.Fatalf("expected valid JSON, got %q: %v", output, jsonErr)
		}
		if parsed["key"] != "value" {
			t.Fatalf("expected key=value in JSON output, got %+v", parsed)
		}
	})

	t.Run("text mode uses callback", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		called := false
		f := &OutputFormatter{jsonMode: false}
		err := f.Render(CommandResult{
			Data: map[string]any{"key": "value"},
			HumanReadable: func() error {
				called = true
				fmt.Println("plain output")
				return nil
			},
		})

		w.Close()
		os.Stdout = oldStdout

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Fatalf("human-readable callback should be called in text mode")
		}

		var buf bytes.Buffer
		io.Copy(&buf, r)
		if got := strings.TrimSpace(buf.String()); got != "plain output" {
			t.Fatalf("unexpected text output: %q", got)
		}
	})
}

func TestOutputFormatterPrintText(t *testing.T) {
	t.Run("json mode skips callback", func(t *testing.T) {
		called := false
		f := &OutputFormatter{jsonMode: true}
		f.PrintText(func() {
			called = true
		})
		if called {
			t.Fatalf("callback should not run in JSON mode")
		}
	})

	t.Run("text mode runs callback", func(t *testing.T) {
		called := false
		f := &OutputFormatter{jsonMode: false}
		f.PrintText(func() {
			called = true
		})
		if !called {
			t.Fatalf("callback should run in text mode")
		}
	})
}

func TestFinalizeClientResult(t *testing.T) {
	t.Run("maps typed call error to formatter output", func(t *testing.T) {
		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		f := &OutputFormatter{jsonMode: true}
		retErr := finalizeClientResult(f, nil, clientCallFailed("rpc failed", io.EOF))

		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		io.Copy(&buf, r)
		output := strings.TrimSpace(buf.String())

		if retErr == nil {
			t.Fatal("expected non-nil error")
		}
		if !strings.Contains(retErr.Error(), "rpc failed") {
			t.Fatalf("expected returned error to include message, got %q", retErr.Error())
		}
		if !strings.Contains(output, "rpc failed") {
			t.Fatalf("expected stderr output to include message, got %q", output)
		}
	})

	t.Run("renders command result", func(t *testing.T) {
		called := false
		f := &OutputFormatter{jsonMode: false}
		err := finalizeClientResult(f, CommandResult{
			Data: map[string]any{"ignored": true},
			HumanReadable: func() error {
				called = true
				return nil
			},
		}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Fatal("expected human-readable callback to run")
		}
	})

	t.Run("renders success result", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		f := &OutputFormatter{jsonMode: false}
		err := finalizeClientResult(f, clientSuccess("done", map[string]interface{}{"id": "x"}), nil)

		w.Close()
		os.Stdout = oldStdout

		var buf bytes.Buffer
		io.Copy(&buf, r)
		output := strings.TrimSpace(buf.String())

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if output != "done" {
			t.Fatalf("unexpected stdout output: %q", output)
		}
	})
}

func TestOutputFormatterError_WritesToErrW(t *testing.T) {
	t.Run("json error lands in errW buffer", func(t *testing.T) {
		var errBuf bytes.Buffer
		f := &OutputFormatter{jsonMode: true, errW: &errBuf}
		retErr := f.Error("connection failed", io.EOF)

		if retErr == nil {
			t.Fatal("expected non-nil error")
		}

		output := strings.TrimSpace(errBuf.String())
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(output), &parsed); err != nil {
			t.Fatalf("expected valid JSON in errW buffer, got %q: %v", output, err)
		}
		if parsed["error"] != "connection failed" {
			t.Errorf("expected error='connection failed', got %v", parsed["error"])
		}
		if _, ok := parsed["details"]; !ok {
			t.Errorf("expected 'details' field in JSON error output")
		}
	})

	t.Run("text error lands in errW buffer", func(t *testing.T) {
		var errBuf bytes.Buffer
		f := &OutputFormatter{jsonMode: false, errW: &errBuf}
		retErr := f.Error("not found", fmt.Errorf("missing resource"))

		if retErr == nil {
			t.Fatal("expected non-nil error")
		}

		output := errBuf.String()
		if !strings.Contains(output, "not found") {
			t.Errorf("expected errW to contain 'not found', got %q", output)
		}
		if !strings.Contains(output, "missing resource") {
			t.Errorf("expected errW to contain 'missing resource', got %q", output)
		}
	})

	t.Run("nil errW falls back to os.Stderr", func(t *testing.T) {
		// Verify that a directly constructed formatter without errW doesn't panic.
		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		f := &OutputFormatter{jsonMode: false}
		_ = f.Error("fallback test", nil)

		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		io.Copy(&buf, r)
		if !strings.Contains(buf.String(), "fallback test") {
			t.Errorf("expected os.Stderr fallback to contain message, got %q", buf.String())
		}
	})
}

func TestOutputFormatterSuccess_WritesToW(t *testing.T) {
	var outBuf bytes.Buffer
	f := &OutputFormatter{jsonMode: true, w: &outBuf}
	if err := f.Success("adapter bound", map[string]interface{}{"slot": "stt"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := strings.TrimSpace(outBuf.String())
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("expected valid JSON in w buffer, got %q: %v", output, err)
	}
	if parsed["message"] != "adapter bound" {
		t.Errorf("expected message='adapter bound', got %v", parsed["message"])
	}
	if parsed["slot"] != "stt" {
		t.Errorf("expected slot='stt', got %v", parsed["slot"])
	}
}

func TestOutputFormatterWriter(t *testing.T) {
	t.Run("returns w when set", func(t *testing.T) {
		var buf bytes.Buffer
		f := &OutputFormatter{w: &buf}
		w := f.Writer()
		fmt.Fprintln(w, "test")
		if !strings.Contains(buf.String(), "test") {
			t.Fatalf("expected output in w buffer, got %q", buf.String())
		}
	})

	t.Run("falls back to os.Stdout when nil", func(t *testing.T) {
		f := &OutputFormatter{}
		w := f.Writer()
		if w != os.Stdout {
			t.Fatalf("expected os.Stdout fallback, got %T", w)
		}
	})
}

func TestOutputFormatterErrWriter(t *testing.T) {
	t.Run("returns errW when set", func(t *testing.T) {
		var buf bytes.Buffer
		f := &OutputFormatter{errW: &buf}
		w := f.ErrWriter()
		fmt.Fprintln(w, "test")
		if !strings.Contains(buf.String(), "test") {
			t.Fatalf("expected output in errW buffer, got %q", buf.String())
		}
	})

	t.Run("falls back to os.Stderr when nil", func(t *testing.T) {
		f := &OutputFormatter{}
		w := f.ErrWriter()
		if w != os.Stderr {
			t.Fatalf("expected os.Stderr fallback, got %T", w)
		}
	})
}

func TestPrintMissingReferenceAdapters_WritesToWriter(t *testing.T) {
	t.Parallel()

	t.Run("writes warning and help to provided writer", func(t *testing.T) {
		var buf bytes.Buffer
		printMissingReferenceAdapters(&buf, []string{"stt-adapter", "tts-adapter"}, true)
		output := buf.String()
		if !strings.Contains(output, "stt-adapter") {
			t.Fatalf("expected adapter name in output, got %q", output)
		}
		if !strings.Contains(output, "tts-adapter") {
			t.Fatalf("expected adapter name in output, got %q", output)
		}
		if !strings.Contains(output, "Install the recommended") {
			t.Fatalf("expected help text in output, got %q", output)
		}
	})

	t.Run("no output for empty list", func(t *testing.T) {
		var buf bytes.Buffer
		printMissingReferenceAdapters(&buf, nil, true)
		if buf.Len() != 0 {
			t.Fatalf("expected no output for empty list, got %q", buf.String())
		}
	})

	t.Run("omits help when showHelp is false", func(t *testing.T) {
		var buf bytes.Buffer
		printMissingReferenceAdapters(&buf, []string{"stt-adapter"}, false)
		output := buf.String()
		if !strings.Contains(output, "stt-adapter") {
			t.Fatalf("expected adapter name in output, got %q", output)
		}
		if strings.Contains(output, "Install the recommended") {
			t.Fatalf("expected no help text when showHelp=false, got %q", output)
		}
	})
}

func TestPrintPluginsTable_WritesToWriter(t *testing.T) {
	t.Parallel()

	t.Run("renders table to provided writer", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []pluginRow{
			{Type: "adapter", Namespace: "nupi", Slug: "stt-whisper", Name: "Whisper STT", Slot: "stt", Status: "bound", Health: "ready"},
			{Type: "adapter", Namespace: "nupi", Slug: "tts-piper", Name: "Piper TTS", Slot: "tts", Status: "unbound", Health: "-"},
		}
		printPluginsTable(&buf, rows)
		output := buf.String()
		if !strings.Contains(output, "TYPE") || !strings.Contains(output, "NAMESPACE/SLUG") {
			t.Fatalf("expected table headers in output, got %q", output)
		}
		if !strings.Contains(output, "nupi/stt-whisper") {
			t.Fatalf("expected adapter entry in output, got %q", output)
		}
		if !strings.Contains(output, "nupi/tts-piper") {
			t.Fatalf("expected second adapter entry in output, got %q", output)
		}
	})

	t.Run("empty rows produces header only", func(t *testing.T) {
		var buf bytes.Buffer
		printPluginsTable(&buf, nil)
		output := buf.String()
		if !strings.Contains(output, "TYPE") {
			t.Fatalf("expected header even for empty rows, got %q", output)
		}
	})
}

func TestPrintAdapterTable_WritesToWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	id := "whisper-stt"
	entries := []apihttp.AdapterEntry{
		{Slot: "stt", AdapterID: &id, Status: "bound", UpdatedAt: "2026-02-23"},
	}
	printAdapterTable(&buf, entries)
	output := buf.String()
	if !strings.Contains(output, "SLOT") || !strings.Contains(output, "ADAPTER") {
		t.Fatalf("expected table headers in output, got %q", output)
	}
	if !strings.Contains(output, "whisper-stt") {
		t.Fatalf("expected adapter entry in output, got %q", output)
	}
}

func TestPrintAdapterSummary_WritesToWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	id := "whisper"
	entry := apihttp.AdapterEntry{Slot: "stt", AdapterID: &id, Status: "started"}
	printAdapterSummary(&buf, "Started", entry)
	output := buf.String()
	if !strings.Contains(output, "Started") {
		t.Fatalf("expected action in output, got %q", output)
	}
	if !strings.Contains(output, "stt") {
		t.Fatalf("expected slot in output, got %q", output)
	}
}

func TestPrintAdapterRuntimeMessages_WritesToWriter(t *testing.T) {
	t.Parallel()

	msg := "adapter unhealthy"
	var buf bytes.Buffer
	id1 := "whisper"
	id2 := "piper"
	entries := []apihttp.AdapterEntry{
		{Slot: "stt", AdapterID: &id1, Runtime: &apihttp.AdapterRuntime{Message: msg}},
		{Slot: "tts", AdapterID: &id2},
	}
	printAdapterRuntimeMessages(&buf, entries)
	output := buf.String()
	if !strings.Contains(output, msg) {
		t.Fatalf("expected runtime message in output, got %q", output)
	}
	// Entry without runtime message should not appear.
	if strings.Contains(output, "tts") {
		t.Fatalf("expected no output for entry without runtime message, got %q", output)
	}
}
