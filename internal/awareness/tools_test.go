package awareness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupToolsService creates a Service with directories and indexer for tool tests.
func setupToolsService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { svc.Shutdown(ctx) })
	return svc, ctx
}

// findSpec returns the ToolSpec with the given name or fails the test.
func findSpec(t *testing.T, specs []ToolSpec, name string) ToolSpec {
	t.Helper()
	for _, s := range specs {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("tool spec %q not found", name)
	return ToolSpec{}
}

// writeMemoryFile writes a file under awareness/memory/ for test setup.
func writeMemoryFile(t *testing.T, svc *Service, relPath, content string) {
	t.Helper()
	fullPath := filepath.Join(svc.awarenessDir, "memory", relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMemorySearchHandler(t *testing.T) {
	svc, ctx := setupToolsService(t)

	writeMemoryFile(t, svc, "daily/2026-02-20.md", "## Test\n\nUnique searchable content about awareness tools.")

	// Sync the indexer so the file is indexed.
	if err := svc.indexer.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	searchSpec := findSpec(t, svc.ToolSpecs(), "memory_search")

	result, err := searchSpec.Handler(ctx, json.RawMessage(`{"query": "searchable content", "scope": "all"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var entries []map[string]interface{}
	if err := json.Unmarshal(result, &entries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("Expected at least 1 search result")
	}

	found := false
	for _, entry := range entries {
		if path, ok := entry["path"].(string); ok && strings.Contains(path, "2026-02-20") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Expected result containing '2026-02-20', got %v", entries)
	}
}

func TestMemorySearchHandlerEmptyQuery(t *testing.T) {
	svc, ctx := setupToolsService(t)

	searchSpec := findSpec(t, svc.ToolSpecs(), "memory_search")

	_, err := searchSpec.Handler(ctx, json.RawMessage(`{"query": ""}`))
	if err == nil {
		t.Fatal("Expected error for empty query")
	}

	if err.Error() != "query is required" {
		t.Fatalf("Expected 'query is required' error, got %q", err.Error())
	}
}

func TestMemorySearchHandlerDefaultScope(t *testing.T) {
	svc, ctx := setupToolsService(t)

	writeMemoryFile(t, svc, "daily/2026-02-20.md", "## Test\n\nDefault scope unique searchable content.")
	if err := svc.indexer.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	searchSpec := findSpec(t, svc.ToolSpecs(), "memory_search")

	// Call without scope — should default to "all" and return the indexed file.
	result, err := searchSpec.Handler(ctx, json.RawMessage(`{"query": "unique searchable content"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var entries []map[string]interface{}
	if err := json.Unmarshal(result, &entries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("Expected at least 1 result with default scope='all', got 0")
	}

	found := false
	for _, entry := range entries {
		if path, ok := entry["path"].(string); ok && strings.Contains(path, "2026-02-20") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Expected result containing '2026-02-20', got %v", entries)
	}
}

func TestMemorySearchHandlerInvalidScope(t *testing.T) {
	svc, ctx := setupToolsService(t)

	searchSpec := findSpec(t, svc.ToolSpecs(), "memory_search")

	_, err := searchSpec.Handler(ctx, json.RawMessage(`{"query": "test", "scope": "invalid"}`))
	if err == nil {
		t.Fatal("Expected error for invalid scope")
	}

	if !strings.Contains(err.Error(), "scope must be") {
		t.Fatalf("Expected scope validation error, got %q", err.Error())
	}
}

func TestMemorySearchHandlerDateNormalization(t *testing.T) {
	svc, ctx := setupToolsService(t)

	today := time.Now().UTC().Format("2006-01-02")
	writeMemoryFile(t, svc, "daily/"+today+".md", "## Test\n\nDate filter test content for normalization.")
	if err := svc.indexer.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	searchSpec := findSpec(t, svc.ToolSpecs(), "memory_search")

	// Search with date_to=today (bare YYYY-MM-DD). Without normalization, the
	// mtime (RFC3339Nano) would be excluded by raw string comparison.
	args := fmt.Sprintf(`{"query": "normalization", "scope": "all", "date_to": "%s"}`, today)
	result, err := searchSpec.Handler(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var entries []map[string]interface{}
	if err := json.Unmarshal(result, &entries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(entries) == 0 {
		t.Fatalf("Expected results with date_to=%q to include same-day entries, got 0", today)
	}
}

func TestNormalizeDate(t *testing.T) {
	tests := []struct {
		input string
		isEnd bool
		want  string
	}{
		{"", false, ""},
		{"", true, ""},
		{"2026-02-20", false, "2026-02-20T00:00:00Z"},
		{"2026-02-20", true, "2026-02-20T23:59:59.999999999Z"},
		{"2026-02-20T15:04:05Z", false, "2026-02-20T15:04:05Z"},
		{"2026-02-20T15:04:05Z", true, "2026-02-20T15:04:05Z"},
		{"invalid", false, "invalid"},
	}

	for _, tt := range tests {
		got := normalizeDate(tt.input, tt.isEnd)
		if got != tt.want {
			t.Errorf("normalizeDate(%q, %v) = %q, want %q", tt.input, tt.isEnd, got, tt.want)
		}
	}
}

func TestMemoryGetHandler(t *testing.T) {
	svc, ctx := setupToolsService(t)

	expected := "# Test File\n\nThis is test content."
	writeMemoryFile(t, svc, "daily/2026-02-20.md", expected)

	getSpec := findSpec(t, svc.ToolSpecs(), "memory_get")

	result, err := getSpec.Handler(ctx, json.RawMessage(`{"path": "daily/2026-02-20.md"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["content"] != expected {
		t.Fatalf("Expected content %q, got %q", expected, parsed["content"])
	}
	if parsed["path"] != "daily/2026-02-20.md" {
		t.Fatalf("Expected path 'daily/2026-02-20.md', got %q", parsed["path"])
	}
}

func TestMemoryGetHandlerPathTraversal(t *testing.T) {
	svc, ctx := setupToolsService(t)

	getSpec := findSpec(t, svc.ToolSpecs(), "memory_get")

	_, err := getSpec.Handler(ctx, json.RawMessage(`{"path": "../../etc/passwd"}`))
	if err == nil {
		t.Fatal("Expected error for path traversal attempt")
	}
	if !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("Expected 'path traversal' error, got %q", err.Error())
	}
}

func TestMemoryGetHandlerSymlinkBypass(t *testing.T) {
	svc, ctx := setupToolsService(t)

	// Create a file outside the memory directory.
	secretDir := filepath.Join(svc.instanceDir, "secret")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "creds.txt"), []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create a symlink inside memory/ pointing to the secret directory.
	memoryDir := filepath.Join(svc.awarenessDir, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretDir, filepath.Join(memoryDir, "escape")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	getSpec := findSpec(t, svc.ToolSpecs(), "memory_get")

	_, err := getSpec.Handler(ctx, json.RawMessage(`{"path": "escape/creds.txt"}`))
	if err == nil {
		t.Fatal("Expected error for symlink bypass attempt, got success")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatal("Symlink bypass: error message should not contain secret content")
	}
}

func TestMemoryGetHandlerFileNotFound(t *testing.T) {
	svc, ctx := setupToolsService(t)

	getSpec := findSpec(t, svc.ToolSpecs(), "memory_get")

	_, err := getSpec.Handler(ctx, json.RawMessage(`{"path": "daily/nonexistent.md"}`))
	if err == nil {
		t.Fatal("Expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("Expected 'file not found' error, got %q", err.Error())
	}
}

func TestMemoryWriteHandlerDaily(t *testing.T) {
	svc, ctx := setupToolsService(t)

	writeSpec := findSpec(t, svc.ToolSpecs(), "memory_write")

	result, err := writeSpec.Handler(ctx, json.RawMessage(`{"content": "test daily entry", "type": "daily"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}

	// Verify the returned path matches expected relative path.
	date := time.Now().UTC().Format("2006-01-02")
	expectedPath := filepath.Join("daily", date+".md")
	if parsed["path"] != expectedPath {
		t.Fatalf("Expected path %q, got %q", expectedPath, parsed["path"])
	}

	// Verify file was created.
	dailyFile := filepath.Join(svc.awarenessDir, "memory", "daily", date+".md")
	data, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("Daily file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "test daily entry") {
		t.Fatalf("Expected daily file to contain 'test daily entry', got %q", content)
	}
	if !strings.Contains(content, "UTC") {
		t.Fatalf("Expected timestamp header with UTC, got %q", content)
	}
}

func TestMemoryWriteHandlerTopic(t *testing.T) {
	svc, ctx := setupToolsService(t)

	writeSpec := findSpec(t, svc.ToolSpecs(), "memory_write")

	result, err := writeSpec.Handler(ctx, json.RawMessage(`{"content": "topic content", "type": "topic", "topic_name": "decisions"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}

	expectedPath := filepath.Join("topics", "decisions.md")
	if parsed["path"] != expectedPath {
		t.Fatalf("Expected path %q, got %q", expectedPath, parsed["path"])
	}

	topicFile := filepath.Join(svc.awarenessDir, "memory", "topics", "decisions.md")
	data, err := os.ReadFile(topicFile)
	if err != nil {
		t.Fatalf("Topic file not created: %v", err)
	}

	if string(data) != "topic content" {
		t.Fatalf("Expected 'topic content', got %q", string(data))
	}
}

func TestMemoryWriteHandlerAppendExisting(t *testing.T) {
	svc, ctx := setupToolsService(t)

	// Write initial daily content.
	date := time.Now().UTC().Format("2006-01-02")
	dailyDir := filepath.Join(svc.awarenessDir, "memory", "daily")
	initialContent := "# Daily Log " + date + "\n\n## Initial Entry\n\nInitial content."
	if err := os.WriteFile(filepath.Join(dailyDir, date+".md"), []byte(initialContent), 0o600); err != nil {
		t.Fatal(err)
	}

	writeSpec := findSpec(t, svc.ToolSpecs(), "memory_write")

	_, err := writeSpec.Handler(ctx, json.RawMessage(`{"content": "appended content", "type": "daily"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dailyDir, date+".md"))
	if err != nil {
		t.Fatalf("Read file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "Initial content.") {
		t.Fatal("Expected initial content to be preserved")
	}
	if !strings.Contains(content, "appended content") {
		t.Fatal("Expected appended content")
	}
}

func TestMemoryWriteHandlerProjectScoped(t *testing.T) {
	svc, ctx := setupToolsService(t)

	writeSpec := findSpec(t, svc.ToolSpecs(), "memory_write")

	result, err := writeSpec.Handler(ctx, json.RawMessage(`{"content": "project note", "type": "daily", "project_slug": "nupi"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}

	// Verify the returned path matches project-scoped relative path.
	date := time.Now().UTC().Format("2006-01-02")
	expectedPath := filepath.Join("projects", "nupi", "daily", date+".md")
	if parsed["path"] != expectedPath {
		t.Fatalf("Expected path %q, got %q", expectedPath, parsed["path"])
	}

	// Verify file was created in project-scoped path.
	projectFile := filepath.Join(svc.awarenessDir, "memory", "projects", "nupi", "daily", date+".md")
	data, err := os.ReadFile(projectFile)
	if err != nil {
		t.Fatalf("Project-scoped daily file not created: %v", err)
	}

	if !strings.Contains(string(data), "project note") {
		t.Fatalf("Expected 'project note' in file, got %q", string(data))
	}
}

func TestMemoryWriteHandlerMissingTopicName(t *testing.T) {
	svc, ctx := setupToolsService(t)

	writeSpec := findSpec(t, svc.ToolSpecs(), "memory_write")

	_, err := writeSpec.Handler(ctx, json.RawMessage(`{"content": "test", "type": "topic"}`))
	if err == nil {
		t.Fatal("Expected error for missing topic_name")
	}

	if !strings.Contains(err.Error(), "topic_name is required") {
		t.Fatalf("Expected 'topic_name is required' error, got %q", err.Error())
	}
}

func TestCoreMemoryUpdateHandlerReplace(t *testing.T) {
	svc, ctx := setupToolsService(t)

	// Create initial SOUL.md.
	if err := os.WriteFile(filepath.Join(svc.awarenessDir, "SOUL.md"), []byte("old soul"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.loadCoreMemory("")

	coreSpec := findSpec(t, svc.ToolSpecs(), "core_memory_update")

	result, err := coreSpec.Handler(ctx, json.RawMessage(`{"file": "SOUL.md", "content": "new soul content", "mode": "replace"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}

	// Verify file content replaced.
	data, err := os.ReadFile(filepath.Join(svc.awarenessDir, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new soul content" {
		t.Fatalf("Expected 'new soul content', got %q", string(data))
	}

	// Verify CoreMemory() returns updated content.
	cm := svc.CoreMemory()
	if !strings.Contains(cm, "new soul content") {
		t.Fatalf("CoreMemory() should contain 'new soul content', got %q", cm)
	}
}

func TestCoreMemoryUpdateHandlerAppend(t *testing.T) {
	svc, ctx := setupToolsService(t)

	// Create initial USER.md.
	if err := os.WriteFile(filepath.Join(svc.awarenessDir, "USER.md"), []byte("initial user info"), 0o600); err != nil {
		t.Fatal(err)
	}

	coreSpec := findSpec(t, svc.ToolSpecs(), "core_memory_update")

	result, err := coreSpec.Handler(ctx, json.RawMessage(`{"file": "USER.md", "content": "appended info", "mode": "append"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}

	data, err := os.ReadFile(filepath.Join(svc.awarenessDir, "USER.md"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if !strings.Contains(content, "initial user info") {
		t.Fatal("Expected initial content preserved")
	}
	if !strings.Contains(content, "appended info") {
		t.Fatal("Expected appended content")
	}
}

func TestCoreMemoryUpdateHandlerInvalidFile(t *testing.T) {
	svc, ctx := setupToolsService(t)

	coreSpec := findSpec(t, svc.ToolSpecs(), "core_memory_update")

	_, err := coreSpec.Handler(ctx, json.RawMessage(`{"file": "EVIL.md", "content": "hack"}`))
	if err == nil {
		t.Fatal("Expected error for invalid core memory file")
	}

	if !strings.Contains(err.Error(), "invalid core memory file") {
		t.Fatalf("Expected 'invalid core memory file' error, got %q", err.Error())
	}
}

func TestCoreMemoryUpdateHandlerDefaultAppend(t *testing.T) {
	svc, ctx := setupToolsService(t)

	// Create initial GLOBAL.md.
	if err := os.WriteFile(filepath.Join(svc.awarenessDir, "GLOBAL.md"), []byte("global rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	coreSpec := findSpec(t, svc.ToolSpecs(), "core_memory_update")

	// No "mode" field — should default to "append".
	result, err := coreSpec.Handler(ctx, json.RawMessage(`{"file": "GLOBAL.md", "content": "new rule"}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}

	data, err := os.ReadFile(filepath.Join(svc.awarenessDir, "GLOBAL.md"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if !strings.Contains(content, "global rules") {
		t.Fatal("Expected initial content preserved with default append")
	}
	if !strings.Contains(content, "new rule") {
		t.Fatal("Expected new content appended")
	}
}

func TestToolSpecs(t *testing.T) {
	svc, _ := setupToolsService(t)

	specs := svc.ToolSpecs()

	if len(specs) != 5 {
		t.Fatalf("Expected 5 tool specs, got %d", len(specs))
	}

	expectedNames := map[string]bool{
		"memory_search":        false,
		"memory_get":           false,
		"memory_write":         false,
		"core_memory_update":   false,
		"onboarding_complete":  false,
	}

	for _, spec := range specs {
		if spec.Name == "" {
			t.Fatal("ToolSpec has empty Name")
		}
		if spec.Description == "" {
			t.Fatalf("ToolSpec %q has empty Description", spec.Name)
		}
		if spec.ParametersJSON == "" {
			t.Fatalf("ToolSpec %q has empty ParametersJSON", spec.Name)
		}
		if spec.Handler == nil {
			t.Fatalf("ToolSpec %q has nil Handler", spec.Name)
		}

		if _, exists := expectedNames[spec.Name]; !exists {
			t.Fatalf("Unexpected tool spec: %q", spec.Name)
		}
		expectedNames[spec.Name] = true
	}

	for name, found := range expectedNames {
		if !found {
			t.Fatalf("Missing expected tool spec: %q", name)
		}
	}
}

func TestValidatePathComponent(t *testing.T) {
	tests := []struct {
		name    string
		label   string
		wantErr string
	}{
		{"valid-name", "slug", ""},
		{"my_project", "slug", ""},
		{"has space", "slug", ""},
		{"../escape", "slug", "invalid slug"},
		{"a/b", "slug", "invalid slug"},
		{"a\\b", "slug", "invalid slug"},
		{" leading", "slug", "leading or trailing whitespace"},
		{"trailing ", "slug", "leading or trailing whitespace"},
		{"has\x00null", "topic", "control characters"},
		{"has\ttab", "topic", "control characters"},
		{"has\nnewline", "topic", "control characters"},
	}

	for _, tt := range tests {
		err := validatePathComponent(tt.name, tt.label)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("validatePathComponent(%q, %q) unexpected error: %v", tt.name, tt.label, err)
			}
		} else {
			if err == nil {
				t.Errorf("validatePathComponent(%q, %q) expected error containing %q, got nil", tt.name, tt.label, tt.wantErr)
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validatePathComponent(%q, %q) error %q does not contain %q", tt.name, tt.label, err.Error(), tt.wantErr)
			}
		}
	}
}

func TestToolSpecsParametersJSONValid(t *testing.T) {
	svc, _ := setupToolsService(t)

	specs := svc.ToolSpecs()

	for _, spec := range specs {
		var schema map[string]interface{}
		if err := json.Unmarshal([]byte(spec.ParametersJSON), &schema); err != nil {
			t.Fatalf("ToolSpec %q has invalid JSON in ParametersJSON: %v", spec.Name, err)
		}

		if schema["type"] != "object" {
			t.Fatalf("ToolSpec %q: expected type 'object', got %v", spec.Name, schema["type"])
		}

		if _, ok := schema["properties"]; !ok {
			t.Fatalf("ToolSpec %q: missing 'properties' in schema", spec.Name)
		}
	}
}

func TestOnboardingCompleteToolSpec(t *testing.T) {
	svc, _ := setupToolsService(t)

	spec := findSpec(t, svc.ToolSpecs(), "onboarding_complete")

	if spec.Description == "" {
		t.Fatal("onboarding_complete has empty Description")
	}
	if spec.Handler == nil {
		t.Fatal("onboarding_complete has nil Handler")
	}
}

func TestOnboardingCompleteToolHandler(t *testing.T) {
	svc, ctx := setupToolsService(t)

	// Confirm BOOTSTRAP.md exists (created by scaffoldCoreFiles in Start).
	if !svc.isOnboarding() {
		t.Fatal("Expected isOnboarding() = true after Start")
	}

	spec := findSpec(t, svc.ToolSpecs(), "onboarding_complete")

	result, err := spec.Handler(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}
	if parsed["message"] != "Onboarding complete. BOOTSTRAP.md removed." {
		t.Fatalf("Expected removal message, got %q", parsed["message"])
	}

	// BOOTSTRAP.md must be gone.
	if svc.isOnboarding() {
		t.Fatal("Expected isOnboarding() = false after handler call")
	}
}

func TestOnboardingCompleteToolHandlerConcurrentComplete(t *testing.T) {
	svc, ctx := setupToolsService(t)

	if !svc.isOnboarding() {
		t.Fatal("Expected isOnboarding() = true after Start")
	}

	spec := findSpec(t, svc.ToolSpecs(), "onboarding_complete")

	// Simulate a race: complete onboarding from another goroutine while
	// the tool handler is about to execute. CompleteOnboarding is idempotent
	// so both calls must succeed without error.
	removed, err := svc.CompleteOnboarding()
	if err != nil {
		t.Fatalf("direct CompleteOnboarding: %v", err)
	}
	if !removed {
		t.Fatal("first CompleteOnboarding should report removed=true")
	}

	// Now the handler should see already-complete state.
	result, err := spec.Handler(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Handler error after concurrent completion: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}
	if parsed["message"] != "Onboarding was already complete." {
		t.Fatalf("Expected already-complete message, got %q", parsed["message"])
	}
}

func TestOnboardingCompleteToolHandlerAlreadyComplete(t *testing.T) {
	svc, ctx := setupToolsService(t)

	// Remove BOOTSTRAP.md to simulate already-completed onboarding.
	os.Remove(filepath.Join(svc.awarenessDir, "BOOTSTRAP.md"))

	if svc.isOnboarding() {
		t.Fatal("Expected isOnboarding() = false after removing BOOTSTRAP.md")
	}

	spec := findSpec(t, svc.ToolSpecs(), "onboarding_complete")

	result, err := spec.Handler(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got %v", parsed)
	}
	if parsed["message"] != "Onboarding was already complete." {
		t.Fatalf("Expected already-complete message, got %q", parsed["message"])
	}
}
