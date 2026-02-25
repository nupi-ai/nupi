package store

import (
	"strings"
	"testing"

	"github.com/nupi-ai/nupi/internal/constants"
)

func TestDefaultPromptTemplatesNotEmpty(t *testing.T) {
	templates := DefaultPromptTemplates()
	if len(templates) == 0 {
		t.Fatal("expected non-empty default templates map")
	}
	for name, content := range templates {
		if content == "" {
			t.Fatalf("default template %q is empty", name)
		}
	}
}

func TestEmbeddedPromptFilesMatchTemplateKeys(t *testing.T) {
	// Collect all .txt files from the embedded prompts/ directory.
	entries, err := promptTemplatesFS.ReadDir("prompts")
	if err != nil {
		t.Fatalf("read embedded prompts dir: %v", err)
	}

	fileKeys := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".txt")
		fileKeys[key] = struct{}{}
	}

	// Check: every template key has a corresponding file.
	for key := range defaultPromptTemplates {
		if _, ok := fileKeys[key]; !ok {
			t.Errorf("template key %q has no matching prompts/%s.txt file", key, key)
		}
	}

	// Check: every file has a corresponding template key.
	for key := range fileKeys {
		if _, ok := defaultPromptTemplates[key]; !ok {
			t.Errorf("prompts/%s.txt exists but has no entry in defaultPromptTemplates", key)
		}
	}

	if len(fileKeys) != len(defaultPromptTemplates) {
		t.Errorf("mismatch: %d prompt files vs %d template keys", len(fileKeys), len(defaultPromptTemplates))
	}
}

func TestPromptEventDescriptionsSyncWithTemplates(t *testing.T) {
	templates := DefaultPromptTemplates()
	descriptions := PromptEventDescriptions()

	for key := range templates {
		if _, ok := descriptions[key]; !ok {
			t.Errorf("template key %q has no entry in promptEventDescriptions", key)
		}
	}
	for key := range descriptions {
		if _, ok := templates[key]; !ok {
			t.Errorf("description key %q has no entry in defaultPromptTemplates", key)
		}
	}
}

func TestDefaultPromptTemplatesIncludesSessionSlug(t *testing.T) {
	templates := DefaultPromptTemplates()
	content, ok := templates[constants.PromptEventSessionSlug]
	if !ok {
		t.Fatal("expected session_slug key in DefaultPromptTemplates")
	}
	if !strings.Contains(content, "NO_REPLY") {
		t.Error("session_slug template should mention NO_REPLY instruction")
	}
	if !strings.Contains(content, "SLUG:") {
		t.Error("session_slug template should mention SLUG: format")
	}
	if !strings.Contains(content, "{{.history}}") {
		t.Error("session_slug template should use {{.history}} placeholder")
	}
}

func TestDefaultPromptTemplatesIncludesOnboarding(t *testing.T) {
	templates := DefaultPromptTemplates()
	content, ok := templates[constants.PromptEventOnboarding]
	if !ok {
		t.Fatal("expected onboarding key in DefaultPromptTemplates")
	}
	if !strings.Contains(content, "onboarding_complete") {
		t.Error("onboarding template should mention onboarding_complete tool")
	}
	if !strings.Contains(content, "core_memory_update") {
		t.Error("onboarding template should mention core_memory_update tool")
	}
}

func TestDefaultPromptTemplatesIncludesHeartbeat(t *testing.T) {
	templates := DefaultPromptTemplates()
	content, ok := templates[constants.PromptEventHeartbeat]
	if !ok {
		t.Fatal("expected heartbeat key in DefaultPromptTemplates")
	}
	if !strings.Contains(content, "heartbeat") {
		t.Fatal("heartbeat template should mention heartbeat")
	}
	if !strings.Contains(content, "{{.transcript}}") {
		t.Fatal("heartbeat template should use {{.transcript}} placeholder")
	}
	if !strings.Contains(content, "heartbeat_name") {
		t.Fatal("heartbeat template should reference heartbeat_name metadata")
	}
}

func TestDefaultPromptTemplatesIncludesMemoryFlush(t *testing.T) {
	templates := DefaultPromptTemplates()
	content, ok := templates[constants.PromptEventMemoryFlush]
	if !ok {
		t.Fatal("expected memory_flush key in DefaultPromptTemplates")
	}
	if !strings.Contains(content, "NO_REPLY") {
		t.Error("memory_flush template should mention NO_REPLY instruction")
	}
	if !strings.Contains(content, "{{.history}}") {
		t.Error("memory_flush template should use {{.history}} placeholder")
	}
}
