package awareness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewRollingLog(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatalf("NewRollingLog() error = %v", err)
		}
		if rl.compactionThreshold != DefaultCompactionThreshold {
			t.Errorf("compactionThreshold = %d, want %d", rl.compactionThreshold, DefaultCompactionThreshold)
		}
		if rl.summaryBudget != DefaultSummaryBudget {
			t.Errorf("summaryBudget = %d, want %d", rl.summaryBudget, DefaultSummaryBudget)
		}
		if rl.RawTailSize() != 0 {
			t.Errorf("RawTailSize() = %d, want 0", rl.RawTailSize())
		}
		if rl.SummariesSize() != 0 {
			t.Errorf("SummariesSize() = %d, want 0", rl.SummariesSize())
		}
	})

	t.Run("custom_options", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		rl, err := NewRollingLog(basePath,
			WithCompactionThreshold(5000),
			WithSummaryBudget(3000),
		)
		if err != nil {
			t.Fatalf("NewRollingLog() error = %v", err)
		}
		if rl.compactionThreshold != 5000 {
			t.Errorf("compactionThreshold = %d, want 5000", rl.compactionThreshold)
		}
		if rl.summaryBudget != 3000 {
			t.Errorf("summaryBudget = %d, want 3000", rl.summaryBudget)
		}
	})

	t.Run("invalid_options_ignored", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		rl, err := NewRollingLog(basePath,
			WithCompactionThreshold(-1),
			WithSummaryBudget(0),
		)
		if err != nil {
			t.Fatalf("NewRollingLog() error = %v", err)
		}
		if rl.compactionThreshold != DefaultCompactionThreshold {
			t.Errorf("compactionThreshold = %d, want default %d", rl.compactionThreshold, DefaultCompactionThreshold)
		}
		if rl.summaryBudget != DefaultSummaryBudget {
			t.Errorf("summaryBudget = %d, want default %d", rl.summaryBudget, DefaultSummaryBudget)
		}
	})

	t.Run("rejects_empty_path", func(t *testing.T) {
		t.Parallel()
		_, err := NewRollingLog("")
		if err == nil {
			t.Fatal("NewRollingLog should reject empty basePath")
		}
	})

	t.Run("rejects_relative_path", func(t *testing.T) {
		t.Parallel()
		_, err := NewRollingLog("relative/path.md")
		if err == nil {
			t.Fatal("NewRollingLog should reject relative basePath")
		}
	})
}

func TestAppendRaw(t *testing.T) {
	t.Parallel()

	t.Run("single_append", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendRaw("hello world"); err != nil {
			t.Fatalf("AppendRaw() error = %v", err)
		}
		if rl.RawTailSize() != len("hello world") {
			t.Errorf("RawTailSize() = %d, want %d", rl.RawTailSize(), len("hello world"))
		}
	})

	t.Run("multiple_appends", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		texts := []string{"line1", "line2", "line3"}
		expectedSize := 0
		for _, text := range texts {
			if err := rl.AppendRaw(text); err != nil {
				t.Fatalf("AppendRaw(%q) error = %v", text, err)
			}
			expectedSize += len(text)
		}
		if rl.RawTailSize() != expectedSize {
			t.Errorf("RawTailSize() = %d, want %d", rl.RawTailSize(), expectedSize)
		}
	})

	t.Run("persists_to_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")
		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendRaw("persisted text"); err != nil {
			t.Fatal(err)
		}

		data, err := os.ReadFile(basePath)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if !strings.Contains(string(data), "persisted text") {
			t.Errorf("file should contain 'persisted text', got:\n%s", data)
		}
	})

	t.Run("allows_double_newlines", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendRaw("hello\n\nworld"); err != nil {
			t.Fatalf("AppendRaw should accept double newlines, got error: %v", err)
		}
		if rl.RawTailSize() != len("hello\n\nworld") {
			t.Errorf("RawTailSize = %d, want %d", rl.RawTailSize(), len("hello\n\nworld"))
		}
	})

	t.Run("rejects_triple_newlines", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		err = rl.AppendRaw("hello\n\n\nworld")
		if err == nil {
			t.Fatal("AppendRaw should reject text containing triple newlines")
		}
		if rl.RawTailSize() != 0 {
			t.Errorf("RawTailSize should be 0 after rejected append, got %d", rl.RawTailSize())
		}
	})

	t.Run("trims_surrounding_newlines", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendRaw("\ntext with newlines\n"); err != nil {
			t.Fatal(err)
		}
		if rl.RawTailSize() != len("text with newlines") {
			t.Errorf("RawTailSize = %d, want %d (after trim)", rl.RawTailSize(), len("text with newlines"))
		}
	})

	t.Run("empty_after_trim_is_noop", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendRaw("\n\n"); err != nil {
			t.Fatalf("AppendRaw on whitespace-only should be noop, got error: %v", err)
		}
		if rl.RawTailSize() != 0 {
			t.Errorf("RawTailSize should be 0 after noop append, got %d", rl.RawTailSize())
		}
	})

	t.Run("rejects_xml_tag_delimiters", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		tags := []string{
			"text with <nupi:rolling-log:summaries> embedded",
			"text with </nupi:rolling-log:summaries> embedded",
			"text with <nupi:rolling-log:raw> embedded",
			"text with </nupi:rolling-log:raw> embedded",
		}
		for _, tag := range tags {
			err := rl.AppendRaw(tag)
			if err == nil {
				t.Errorf("AppendRaw should reject content with XML tag: %q", tag)
			}
		}
		if rl.RawTailSize() != 0 {
			t.Errorf("RawTailSize should be 0 after all rejections, got %d", rl.RawTailSize())
		}
	})
}

func TestAppendSummary(t *testing.T) {
	t.Parallel()

	t.Run("single_summary", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		summary := "### [summary] 14:30 – 14:45\nAI analyzed codebase."
		if err := rl.AppendSummary(summary); err != nil {
			t.Fatalf("AppendSummary() error = %v", err)
		}
		if rl.SummariesSize() != len(summary) {
			t.Errorf("SummariesSize() = %d, want %d", rl.SummariesSize(), len(summary))
		}
	})

	t.Run("multiple_summaries", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		summaries := []string{
			"### [summary] 14:30 – 14:45\nFirst summary.",
			"### [summary] 14:45 – 15:00\nSecond summary.",
		}
		expectedSize := 0
		for _, s := range summaries {
			if err := rl.AppendSummary(s); err != nil {
				t.Fatal(err)
			}
			expectedSize += len(s)
		}
		if rl.SummariesSize() != expectedSize {
			t.Errorf("SummariesSize() = %d, want %d", rl.SummariesSize(), expectedSize)
		}
	})

	t.Run("rejects_missing_header_prefix", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		err = rl.AppendSummary("No header prefix here.")
		if err == nil {
			t.Fatal("AppendSummary should reject summaries without ### prefix")
		}
		if rl.SummariesSize() != 0 {
			t.Errorf("SummariesSize should be 0 after rejected append, got %d", rl.SummariesSize())
		}
	})

	t.Run("rejects_internal_double_newline_header", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		// \n\n### inside a summary would cause it to split on reload.
		err = rl.AppendSummary("### Main header\nContent\n\n### Sub-heading\nMore content")
		if err == nil {
			t.Fatal("AppendSummary should reject summaries containing \\n\\n### ")
		}
		if rl.SummariesSize() != 0 {
			t.Errorf("SummariesSize should be 0 after rejected append, got %d", rl.SummariesSize())
		}
	})

	t.Run("rejects_xml_tag_delimiters", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		tags := []string{
			"### Summary with <nupi:rolling-log:summaries> tag",
			"### Summary with </nupi:rolling-log:summaries> tag",
			"### Summary with <nupi:rolling-log:raw> tag",
			"### Summary with </nupi:rolling-log:raw> tag",
		}
		for _, tag := range tags {
			err := rl.AppendSummary(tag)
			if err == nil {
				t.Errorf("AppendSummary should reject content with XML tag: %q", tag)
			}
		}
		if rl.SummariesSize() != 0 {
			t.Errorf("SummariesSize should be 0 after all rejections, got %d", rl.SummariesSize())
		}
	})
}

func TestShouldCompact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		threshold int
		rawSize   int
		want      bool
	}{
		{"below_threshold", 100, 50, false},
		{"at_threshold", 100, 100, false},
		{"above_threshold", 100, 101, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
				WithCompactionThreshold(tc.threshold),
			)
			if err != nil {
				t.Fatal(err)
			}

			// Add raw text to reach desired size.
			text := strings.Repeat("x", tc.rawSize)
			if err := rl.AppendRaw(text); err != nil {
				t.Fatal(err)
			}

			if got := rl.ShouldCompact(); got != tc.want {
				t.Errorf("ShouldCompact() = %v, want %v (rawSize=%d, threshold=%d)",
					got, tc.want, tc.rawSize, tc.threshold)
			}
		})
	}
}

func TestOlderHalf(t *testing.T) {
	t.Parallel()

	t.Run("splits_correctly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		// Add 4 entries.
		for i := 0; i < 4; i++ {
			if err := rl.AppendRaw(fmt.Sprintf("entry-%d", i)); err != nil {
				t.Fatal(err)
			}
		}

		older, err := rl.OlderHalf()
		if err != nil {
			t.Fatalf("OlderHalf() error = %v", err)
		}

		// Should contain entries 0 and 1.
		if !strings.Contains(older, "entry-0") || !strings.Contains(older, "entry-1") {
			t.Errorf("OlderHalf() should contain entry-0 and entry-1, got: %q", older)
		}
		if strings.Contains(older, "entry-2") || strings.Contains(older, "entry-3") {
			t.Errorf("OlderHalf() should NOT contain entry-2 or entry-3, got: %q", older)
		}

		// Remaining raw tail should have entries 2 and 3.
		_, raw, _ := rl.GetContext()
		if !strings.Contains(raw, "entry-2") || !strings.Contains(raw, "entry-3") {
			t.Errorf("remaining raw tail should contain entry-2 and entry-3, got: %q", raw)
		}
		if strings.Contains(raw, "entry-0") || strings.Contains(raw, "entry-1") {
			t.Errorf("remaining raw tail should NOT contain entry-0 or entry-1, got: %q", raw)
		}
	})

	t.Run("single_entry", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendRaw("only-entry"); err != nil {
			t.Fatal(err)
		}

		older, err := rl.OlderHalf()
		if err != nil {
			t.Fatal(err)
		}
		if older != "only-entry" {
			t.Errorf("OlderHalf() = %q, want 'only-entry'", older)
		}
		if rl.RawTailSize() != 0 {
			t.Errorf("RawTailSize() = %d after OlderHalf on single entry, want 0", rl.RawTailSize())
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		older, err := rl.OlderHalf()
		if err != nil {
			t.Fatal(err)
		}
		if older != "" {
			t.Errorf("OlderHalf() on empty = %q, want empty", older)
		}
	})
}

func TestShouldArchive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		budget int
		size   int
		want   bool
	}{
		{"below_budget", 100, 50, false},
		{"at_budget", 100, 100, false},
		{"above_budget", 100, 101, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
				WithSummaryBudget(tc.budget),
			)
			if err != nil {
				t.Fatal(err)
			}

			summary := "### " + strings.Repeat("s", tc.size-4) // -4 for "### " prefix
			if err := rl.AppendSummary(summary); err != nil {
				t.Fatal(err)
			}

			if got := rl.ShouldArchive(); got != tc.want {
				t.Errorf("ShouldArchive() = %v, want %v (size=%d, budget=%d)",
					got, tc.want, len(summary), tc.budget)
			}
		})
	}
}

func TestOlderSummaries(t *testing.T) {
	t.Parallel()

	t.Run("returns_oldest_exceeding_budget", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(50),
		)
		if err != nil {
			t.Fatal(err)
		}

		// Add 3 summaries of 30 chars each (total 90, budget 50).
		for i := 0; i < 3; i++ {
			s := fmt.Sprintf("### Summary %d %s", i, strings.Repeat("x", 18))
			if err := rl.AppendSummary(s); err != nil {
				t.Fatal(err)
			}
		}

		older := rl.OlderSummaries()

		// Should identify summaries to archive.
		if len(older) == 0 {
			t.Fatal("OlderSummaries() returned empty, expected at least one")
		}

		// OlderSummaries is read-only — size should not change yet.
		originalSize := rl.SummariesSize()
		if originalSize <= 50 {
			t.Fatalf("SummariesSize() = %d before commit, expected > 50", originalSize)
		}

		// CommitArchival should remove them.
		if err := rl.CommitArchival(len(older)); err != nil {
			t.Fatalf("CommitArchival() error = %v", err)
		}

		// Remaining should be within budget.
		if rl.SummariesSize() > 50 {
			t.Errorf("remaining SummariesSize() = %d, should be <= 50 after CommitArchival", rl.SummariesSize())
		}
	})

	t.Run("no_removal_when_under_budget", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(1000),
		)
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendSummary("### short"); err != nil {
			t.Fatal(err)
		}

		older := rl.OlderSummaries()
		if len(older) != 0 {
			t.Errorf("OlderSummaries() should return nil when under budget, got %d items", len(older))
		}
	})
}

func TestArchive(t *testing.T) {
	t.Parallel()

	t.Run("creates_dated_archive_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		archivePath := filepath.Join(dir, "archives")

		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		summaries := []string{
			"### [summary] 14:30\nFirst summary.",
			"### [summary] 14:45\nSecond summary.",
		}

		if err := rl.Archive(archivePath, summaries); err != nil {
			t.Fatalf("Archive() error = %v", err)
		}

		date := time.Now().UTC().Format("2006-01-02")
		archiveFile := filepath.Join(archivePath, date+".md")
		data, err := os.ReadFile(archiveFile)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}

		content := string(data)
		if !strings.Contains(content, "First summary.") {
			t.Error("archive should contain first summary")
		}
		if !strings.Contains(content, "Second summary.") {
			t.Error("archive should contain second summary")
		}
	})

	t.Run("appends_to_existing_archive", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		archivePath := filepath.Join(dir, "archives")

		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		// First archive.
		if err := rl.Archive(archivePath, []string{"First batch."}); err != nil {
			t.Fatal(err)
		}

		// Second archive — should append.
		if err := rl.Archive(archivePath, []string{"Second batch."}); err != nil {
			t.Fatal(err)
		}

		date := time.Now().UTC().Format("2006-01-02")
		data, err := os.ReadFile(filepath.Join(archivePath, date+".md"))
		if err != nil {
			t.Fatal(err)
		}

		content := string(data)
		if !strings.Contains(content, "First batch.") || !strings.Contains(content, "Second batch.") {
			t.Errorf("archive should contain both batches, got:\n%s", content)
		}
	})

	t.Run("empty_summaries_noop", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		archivePath := filepath.Join(dir, "archives")

		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.Archive(archivePath, nil); err != nil {
			t.Fatalf("Archive(nil) should be noop, got error = %v", err)
		}

		// Archive dir should not be created.
		if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
			t.Error("archive directory should not be created for empty summaries")
		}
	})

	t.Run("rejects_empty_path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}
		err = rl.Archive("", []string{"### Summary"})
		if err == nil {
			t.Fatal("Archive should reject empty archivePath")
		}
	})

	t.Run("rejects_relative_path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}
		err = rl.Archive("relative/archives", []string{"### Summary"})
		if err == nil {
			t.Fatal("Archive should reject relative archivePath")
		}
	})
}

func TestGetContext(t *testing.T) {
	t.Parallel()

	t.Run("returns_summaries_and_raw", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendSummary("### Summary 1\nContent one."); err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendRaw("raw line 1"); err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendRaw("raw line 2"); err != nil {
			t.Fatal(err)
		}

		summaries, raw, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(summaries, "Summary 1") {
			t.Errorf("summaries should contain 'Summary 1', got: %q", summaries)
		}
		if !strings.Contains(raw, "raw line 1") || !strings.Contains(raw, "raw line 2") {
			t.Errorf("raw should contain both lines, got: %q", raw)
		}
	})

	t.Run("empty_rolling_log", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		summaries, raw, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if summaries != "" {
			t.Errorf("summaries should be empty, got: %q", summaries)
		}
		if raw != "" {
			t.Errorf("raw should be empty, got: %q", raw)
		}
	})
}

func TestWriteLog(t *testing.T) {
	t.Parallel()

	t.Run("creates_dated_log_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logsPath := filepath.Join(dir, "logs")
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.WriteLog(logsPath, "log entry 1"); err != nil {
			t.Fatalf("WriteLog() error = %v", err)
		}

		date := time.Now().UTC().Format("2006-01-02")
		data, err := os.ReadFile(filepath.Join(logsPath, date+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "log entry 1") {
			t.Errorf("log file should contain 'log entry 1', got: %s", data)
		}
	})

	t.Run("appends_to_existing_log", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logsPath := filepath.Join(dir, "logs")
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.WriteLog(logsPath, "entry 1"); err != nil {
			t.Fatal(err)
		}
		if err := rl.WriteLog(logsPath, "entry 2"); err != nil {
			t.Fatal(err)
		}

		date := time.Now().UTC().Format("2006-01-02")
		data, err := os.ReadFile(filepath.Join(logsPath, date+".md"))
		if err != nil {
			t.Fatal(err)
		}

		content := string(data)
		if !strings.Contains(content, "entry 1") || !strings.Contains(content, "entry 2") {
			t.Errorf("log file should contain both entries, got: %s", content)
		}
	})

	t.Run("append_only_no_modify", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logsPath := filepath.Join(dir, "logs")
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		// Pre-create the log file with some content.
		date := time.Now().UTC().Format("2006-01-02")
		logFile := filepath.Join(logsPath, date+".md")
		if err := os.MkdirAll(logsPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logFile, []byte("existing content\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := rl.WriteLog(logsPath, "new entry"); err != nil {
			t.Fatal(err)
		}

		data, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, "existing content") {
			t.Error("WriteLog should not modify existing content")
		}
		if !strings.Contains(content, "new entry") {
			t.Error("WriteLog should append new content")
		}
	})

	t.Run("rejects_empty_path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}
		err = rl.WriteLog("", "some text")
		if err == nil {
			t.Fatal("WriteLog should reject empty logsPath")
		}
	})

	t.Run("rejects_relative_path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}
		err = rl.WriteLog("relative/logs", "some text")
		if err == nil {
			t.Fatal("WriteLog should reject relative logsPath")
		}
	})

	t.Run("skips_empty_text", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logsPath := filepath.Join(dir, "logs")
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		// Empty and whitespace-only text should be silently skipped.
		if err := rl.WriteLog(logsPath, ""); err != nil {
			t.Fatalf("WriteLog('') should be noop, got error: %v", err)
		}
		if err := rl.WriteLog(logsPath, "   \n  "); err != nil {
			t.Fatalf("WriteLog(whitespace) should be noop, got error: %v", err)
		}

		// Logs dir should not be created for empty writes.
		if _, err := os.Stat(logsPath); !os.IsNotExist(err) {
			t.Error("logs directory should not be created for empty text writes")
		}
	})
}

func TestLoadFromFile(t *testing.T) {
	t.Parallel()

	t.Run("loads_persisted_state", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		// Create and populate a RollingLog.
		rl1, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := rl1.AppendSummary("### [summary] 14:30\nSummary content."); err != nil {
			t.Fatal(err)
		}
		if err := rl1.AppendRaw("raw line 1"); err != nil {
			t.Fatal(err)
		}
		if err := rl1.AppendRaw("raw line 2"); err != nil {
			t.Fatal(err)
		}

		// Create a new RollingLog from the same file — should load state.
		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		summaries, raw, err := rl2.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(summaries, "Summary content.") {
			t.Errorf("loaded summaries should contain 'Summary content.', got: %q", summaries)
		}
		if !strings.Contains(raw, "raw line 1") || !strings.Contains(raw, "raw line 2") {
			t.Errorf("loaded raw should contain both lines, got: %q", raw)
		}
	})

	t.Run("handles_empty_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		if err := os.WriteFile(basePath, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}
		if rl.RawTailSize() != 0 || rl.SummariesSize() != 0 {
			t.Error("empty file should result in empty state")
		}
	})

	t.Run("handles_missing_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "nonexistent.md")

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatalf("NewRollingLog() should not error on missing file, got: %v", err)
		}
		if rl.RawTailSize() != 0 || rl.SummariesSize() != 0 {
			t.Error("missing file should result in empty state")
		}
	})
}

func TestRawEntryRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("multi_line_entry_survives_reload", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		multiLine := "line1\nline2\nline3"
		if err := rl.AppendRaw(multiLine); err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendRaw("single-line"); err != nil {
			t.Fatal(err)
		}

		origSize := rl.RawTailSize()

		// Reload from file.
		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		_, raw, _ := rl2.GetContext()
		if !strings.Contains(raw, "line1\nline2\nline3") {
			t.Errorf("multi-line raw entry should survive round-trip, got: %q", raw)
		}
		if !strings.Contains(raw, "single-line") {
			t.Errorf("single-line entry should survive, got: %q", raw)
		}

		if rl2.RawTailSize() != origSize {
			t.Errorf("RawTailSize mismatch after reload: got %d, want %d", rl2.RawTailSize(), origSize)
		}
	})

	t.Run("single_line_entries_round_trip", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		entries := []string{"entry-0", "entry-1", "entry-2"}
		for _, e := range entries {
			if err := rl.AppendRaw(e); err != nil {
				t.Fatal(err)
			}
		}

		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		_, raw, _ := rl2.GetContext()
		for _, e := range entries {
			if !strings.Contains(raw, e) {
				t.Errorf("entry %q should survive round-trip, got: %q", e, raw)
			}
		}
		if rl2.RawTailSize() != rl.RawTailSize() {
			t.Errorf("RawTailSize mismatch: %d vs %d", rl2.RawTailSize(), rl.RawTailSize())
		}
	})

	t.Run("double_newline_entry_survives_reload", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		entry := "line1\n\nline3"
		if err := rl.AppendRaw(entry); err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendRaw("other-entry"); err != nil {
			t.Fatal(err)
		}

		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		_, raw, _ := rl2.GetContext()
		if !strings.Contains(raw, "line1\n\nline3") {
			t.Errorf("double-newline entry should survive round-trip, got: %q", raw)
		}
		if !strings.Contains(raw, "other-entry") {
			t.Errorf("second entry should survive, got: %q", raw)
		}
		if rl2.RawTailSize() != rl.RawTailSize() {
			t.Errorf("RawTailSize mismatch: %d vs %d", rl2.RawTailSize(), rl.RawTailSize())
		}
	})

	t.Run("preserves_leading_spaces", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		entry := "  indented line\n    more indented"
		if err := rl.AppendRaw(entry); err != nil {
			t.Fatal(err)
		}

		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		_, raw, _ := rl2.GetContext()
		if !strings.Contains(raw, "  indented line") {
			t.Errorf("leading spaces should be preserved, got: %q", raw)
		}
		if !strings.Contains(raw, "    more indented") {
			t.Errorf("nested indentation should be preserved, got: %q", raw)
		}
	})
}

func TestSummaryRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("trailing_newlines_normalized", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")
		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		// Trailing newlines are trimmed before storage, so size is stable on reload.
		if err := rl.AppendSummary("### [summary] 14:30\nBody content.\n\n"); err != nil {
			t.Fatal(err)
		}

		sizeBefore := rl.SummariesSize()

		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		sizeAfter := rl2.SummariesSize()
		if sizeBefore != sizeAfter {
			t.Errorf("SummariesSize mismatch after reload: before=%d, after=%d (byte count drift)", sizeBefore, sizeAfter)
		}

		summaries, _, err := rl2.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(summaries, "Body content.") {
			t.Errorf("summary content should be preserved, got: %q", summaries)
		}
	})

	t.Run("leading_newlines_normalized", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")
		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendSummary("\n\n### [summary] 14:30\nBody."); err != nil {
			t.Fatal(err)
		}

		sizeBefore := rl.SummariesSize()
		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}
		if rl2.SummariesSize() != sizeBefore {
			t.Errorf("SummariesSize drift: %d -> %d", sizeBefore, rl2.SummariesSize())
		}
	})

	t.Run("empty_summary_is_noop", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendSummary(""); err != nil {
			t.Fatalf("AppendSummary('') should be noop, got error: %v", err)
		}
		if err := rl.AppendSummary("\n\n"); err != nil {
			t.Fatalf("AppendSummary(newlines) should be noop, got error: %v", err)
		}
		if rl.SummariesSize() != 0 {
			t.Errorf("SummariesSize should be 0 after noop appends, got %d", rl.SummariesSize())
		}
	})
}

func TestParagraphAlignedTruncation(t *testing.T) {
	t.Parallel()

	t.Run("within_budget_returns_all", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(1000),
		)
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendSummary("### Summary 1\nShort content."); err != nil {
			t.Fatal(err)
		}

		summaries, _, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(summaries, "Short content.") {
			t.Errorf("within budget should return all content, got: %q", summaries)
		}
	})

	t.Run("truncates_at_header_boundary", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Budget set large enough that the \n\n### boundary falls within the
		// discard region (before cutPoint), so the algorithm finds it.
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(45),
		)
		if err != nil {
			t.Fatal(err)
		}

		// Summary 1: 49 chars. Summary 2: 43 chars. Combined: 49 + 2 (\n\n) + 43 = 94.
		// cutPoint = 94 - 45 = 49. searchRegion[:49] contains the full first summary
		// which has no \n\n, but combined[49] is the \n\n separator.
		// The \n\n### boundary is at index 49, so searchRegion[:49] doesn't include it.
		// Use 3 summaries to ensure a boundary falls in the discard region.
		if err := rl.AppendSummary("### [summary] 13:30\nVery old."); err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendSummary("### [summary] 14:00\nOld content."); err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendSummary("### [summary] 14:30\nNewer content to keep."); err != nil {
			t.Fatal(err)
		}

		summaries, _, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}

		// Should start at a ### boundary (the algorithm found one in the discard region).
		if !strings.HasPrefix(summaries, "###") {
			t.Errorf("truncated summaries should start at a ### boundary, got: %q", summaries)
		}

		// Should contain the newer content.
		if !strings.Contains(summaries, "Newer content to keep.") {
			t.Errorf("summaries should contain newer content, got: %q", summaries)
		}

		// Should be at least budget size.
		if len(summaries) < 45 {
			t.Errorf("result length %d should be >= budget 45", len(summaries))
		}
	})

	t.Run("single_paragraph", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(10),
		)
		if err != nil {
			t.Fatal(err)
		}

		longSummary := "### [summary] 14:00\n" + strings.Repeat("x", 100)
		if err := rl.AppendSummary(longSummary); err != nil {
			t.Fatal(err)
		}

		summaries, _, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}

		// With single paragraph, should return something (minimum budget chars from end).
		if summaries == "" {
			t.Error("single paragraph should still return content")
		}
	})

	t.Run("multiple_headers", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(80),
		)
		if err != nil {
			t.Fatal(err)
		}

		summaries := []string{
			"### [summary] 14:00\nFirst content block.",
			"### [summary] 14:15\nSecond content block.",
			"### [summary] 14:30\nThird content block.",
			"### [summary] 14:45\nFourth content block.",
		}
		for _, s := range summaries {
			if err := rl.AppendSummary(s); err != nil {
				t.Fatal(err)
			}
		}

		result, _, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}

		// Result should be at least summaryBudget chars.
		if len(result) < 80 {
			t.Errorf("result length %d should be at least summaryBudget (80), got: %q", len(result), result)
		}

		// Should start at a ### header.
		if !strings.HasPrefix(result, "###") {
			t.Errorf("should start at header boundary, got: %q", result[:min(50, len(result))])
		}
	})

	t.Run("minimum_budget_compliance", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		budget := 50
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(budget),
		)
		if err != nil {
			t.Fatal(err)
		}

		// Create content larger than budget.
		for i := 0; i < 5; i++ {
			if err := rl.AppendSummary(fmt.Sprintf("### Block %d\n%s", i, strings.Repeat("a", 30))); err != nil {
				t.Fatal(err)
			}
		}

		result, _, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}

		if len(result) < budget {
			t.Errorf("result length %d should be >= budget %d", len(result), budget)
		}
	})

	t.Run("exact_budget_boundary_returns_all", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Create a summary whose length we know exactly.
		summary := "### Block\n" + strings.Repeat("b", 20)
		budget := len(summary) // budget == content length exactly

		rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
			WithSummaryBudget(budget),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendSummary(summary); err != nil {
			t.Fatal(err)
		}

		result, _, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if result != summary {
			t.Errorf("exact budget should return all content unchanged, got: %q", result)
		}
	})
}

func TestCommitArchival(t *testing.T) {
	t.Parallel()

	t.Run("removes_first_n_summaries", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 4; i++ {
			if err := rl.AppendSummary(fmt.Sprintf("### Summary %d", i)); err != nil {
				t.Fatal(err)
			}
		}

		if err := rl.CommitArchival(2); err != nil {
			t.Fatalf("CommitArchival() error = %v", err)
		}

		summaries, _, err := rl.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(summaries, "Summary 0") || strings.Contains(summaries, "Summary 1") {
			t.Errorf("committed summaries should be removed, got: %q", summaries)
		}
		if !strings.Contains(summaries, "Summary 2") || !strings.Contains(summaries, "Summary 3") {
			t.Errorf("remaining summaries should be kept, got: %q", summaries)
		}
	})

	t.Run("noop_for_zero_or_negative", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendSummary("### Keep me"); err != nil {
			t.Fatal(err)
		}

		if err := rl.CommitArchival(0); err != nil {
			t.Fatalf("CommitArchival(0) error = %v", err)
		}
		if err := rl.CommitArchival(-1); err != nil {
			t.Fatalf("CommitArchival(-1) error = %v", err)
		}

		if rl.SummariesSize() == 0 {
			t.Error("summaries should not be removed for n <= 0")
		}
	})

	t.Run("persists_to_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")
		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 3; i++ {
			if err := rl.AppendSummary(fmt.Sprintf("### Summary %d", i)); err != nil {
				t.Fatal(err)
			}
		}

		if err := rl.CommitArchival(2); err != nil {
			t.Fatal(err)
		}

		// Reload from file and verify.
		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}
		summaries, _, err := rl2.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(summaries, "Summary 0") {
			t.Errorf("reloaded file should not contain committed summaries, got: %q", summaries)
		}
		if !strings.Contains(summaries, "Summary 2") {
			t.Errorf("reloaded file should contain remaining summary, got: %q", summaries)
		}
	})

	t.Run("error_when_n_exceeds_count", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		if err := rl.AppendSummary("### Only one"); err != nil {
			t.Fatal(err)
		}

		err = rl.CommitArchival(5)
		if err == nil {
			t.Fatal("CommitArchival(5) should error when only 1 summary exists")
		}
	})
}

func TestParseRobustness(t *testing.T) {
	t.Parallel()

	t.Run("summary_with_internal_single_newline_header", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")
		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}

		// Summary with internal ### sub-header (single newline, not double).
		summary := "### Main header\nBody text\n### Sub-header\nMore text"
		if err := rl.AppendSummary(summary); err != nil {
			t.Fatal(err)
		}

		// Reload and verify the summary is preserved as one entry.
		rl2, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}
		summaries, _, err := rl2.GetContext()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(summaries, "Sub-header") {
			t.Errorf("internal ### should be preserved, got: %q", summaries)
		}
		if !strings.Contains(summaries, "Main header") {
			t.Errorf("main header should be preserved, got: %q", summaries)
		}
	})

	t.Run("rejects_non_header_summary", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rl, err := NewRollingLog(filepath.Join(dir, "test.md"))
		if err != nil {
			t.Fatal(err)
		}

		// Summaries without ### prefix are rejected to prevent merge on reload.
		err = rl.AppendSummary("Plain text without header.")
		if err == nil {
			t.Fatal("AppendSummary should reject summaries without ### prefix")
		}
		if rl.SummariesSize() != 0 {
			t.Errorf("SummariesSize should be 0 after rejected append, got %d", rl.SummariesSize())
		}
	})

	t.Run("malformed_file_no_panic", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		basePath := filepath.Join(dir, "test.md")

		// Write a malformed file (no XML section tags).
		if err := os.WriteFile(basePath, []byte("random content\nno headers"), 0o600); err != nil {
			t.Fatal(err)
		}

		rl, err := NewRollingLog(basePath)
		if err != nil {
			t.Fatal(err)
		}
		if rl.RawTailSize() != 0 || rl.SummariesSize() != 0 {
			t.Error("malformed file should result in empty state")
		}
	})
}

func TestArchiveWorkflowAfterReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	basePath := filepath.Join(dir, "test.md")
	archivePath := filepath.Join(dir, "archives")

	// Create and populate a RollingLog with summaries exceeding budget.
	rl, err := NewRollingLog(basePath, WithSummaryBudget(60))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		s := fmt.Sprintf("### Summary %d\n%s", i, strings.Repeat("a", 20))
		if err := rl.AppendSummary(s); err != nil {
			t.Fatal(err)
		}
	}

	// Reload from file — this is the critical path where count/size must be stable.
	rl2, err := NewRollingLog(basePath, WithSummaryBudget(60))
	if err != nil {
		t.Fatal(err)
	}

	// Full archive workflow on reloaded instance.
	older := rl2.OlderSummaries()
	if len(older) == 0 {
		t.Fatal("expected OlderSummaries to return entries after reload")
	}
	if err := rl2.Archive(archivePath, older); err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if err := rl2.CommitArchival(len(older)); err != nil {
		t.Fatalf("CommitArchival() error = %v (count drift after reload?)", err)
	}

	// Verify remaining summaries are within budget.
	if rl2.SummariesSize() > 60 {
		t.Errorf("remaining SummariesSize() = %d, should be <= 60 after archival", rl2.SummariesSize())
	}

	// Verify archive file exists with content.
	date := time.Now().UTC().Format("2006-01-02")
	data, err := os.ReadFile(filepath.Join(archivePath, date+".md"))
	if err != nil {
		t.Fatalf("archive file not created: %v", err)
	}
	if !strings.Contains(string(data), "Summary 0") {
		t.Error("archive should contain oldest summary")
	}

	// Verify state survives another reload.
	rl3, err := NewRollingLog(basePath, WithSummaryBudget(60))
	if err != nil {
		t.Fatal(err)
	}
	if rl3.SummariesSize() != rl2.SummariesSize() {
		t.Errorf("SummariesSize drift after re-reload: %d vs %d", rl3.SummariesSize(), rl2.SummariesSize())
	}
}

func TestParagraphAlignedHardCutoff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Budget = 40. Single long summary with no paragraph boundaries in the
	// discard region. The function should hard-cut at budget, not return everything.
	summary := "### x" + strings.Repeat("a", 100)
	budget := 40

	rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
		WithSummaryBudget(budget),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := rl.AppendSummary(summary); err != nil {
		t.Fatal(err)
	}

	result, _, err := rl.GetContext()
	if err != nil {
		t.Fatal(err)
	}

	// Result should be exactly budget bytes (hard cutoff, no boundary found).
	if len(result) != budget {
		t.Errorf("hard cutoff should return exactly budget=%d bytes, got %d", budget, len(result))
	}
	// Result should be the TAIL of the combined string.
	if !strings.HasSuffix(summary, result) {
		t.Errorf("result should be the tail of the summary, got: %q", result)
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
		WithCompactionThreshold(100000),
	)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	const goroutines = 20

	// Concurrent AppendRaw.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = rl.AppendRaw(fmt.Sprintf("raw-%d", n))
		}(i)
	}

	// Concurrent AppendSummary.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = rl.AppendSummary(fmt.Sprintf("### Summary %d\nContent.", n))
		}(i)
	}

	// Concurrent reads.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = rl.GetContext()
			_ = rl.RawTailSize()
			_ = rl.SummariesSize()
			_ = rl.ShouldCompact()
			_ = rl.ShouldArchive()
		}()
	}

	wg.Wait()

	// Verify final state is consistent.
	size := rl.RawTailSize()
	if size == 0 {
		t.Error("RawTailSize() should be > 0 after concurrent appends")
	}
	sSize := rl.SummariesSize()
	if sSize == 0 {
		t.Error("SummariesSize() should be > 0 after concurrent appends")
	}

	// Verify exact entry counts via internal state (same-package access).
	// Direct slice length is reliable regardless of entry content.
	rl.mu.RLock()
	rawCount := len(rl.rawTail)
	summaryCount := len(rl.summaries)
	rl.mu.RUnlock()
	if rawCount != goroutines {
		t.Errorf("expected %d raw entries, got %d", goroutines, rawCount)
	}
	if summaryCount != goroutines {
		t.Errorf("expected %d summaries, got %d", goroutines, summaryCount)
	}
}

func TestConcurrentMutations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rl, err := NewRollingLog(filepath.Join(dir, "test.md"),
		WithCompactionThreshold(100000),
		WithSummaryBudget(100000),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-populate so OlderHalf and CommitArchival have work.
	for i := 0; i < 50; i++ {
		if err := rl.AppendRaw(fmt.Sprintf("raw-%d", i)); err != nil {
			t.Fatal(err)
		}
		if err := rl.AppendSummary(fmt.Sprintf("### Summary %d\nContent.", i)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	archivePath := filepath.Join(dir, "archives")

	// Concurrent OlderHalf calls.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rl.OlderHalf()
		}()
	}

	// Concurrent Archive calls to the same path.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = rl.Archive(archivePath, []string{fmt.Sprintf("### Archive %d", n)})
		}(i)
	}

	// Concurrent CommitArchival calls.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rl.CommitArchival(1)
		}()
	}

	// Concurrent reads interleaved with mutations.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = rl.GetContext()
			_ = rl.ShouldCompact()
			_ = rl.ShouldArchive()
		}()
	}

	wg.Wait()

	// Verify state is consistent (no panics, no corruption).
	_, _, err = rl.GetContext()
	if err != nil {
		t.Fatalf("GetContext after concurrent mutations: %v", err)
	}
}
