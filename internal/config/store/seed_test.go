package store

import (
	"strings"
	"testing"
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
	content, ok := templates["session_slug"]
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

func TestDefaultPromptTemplatesIncludesMemoryFlush(t *testing.T) {
	templates := DefaultPromptTemplates()
	content, ok := templates["memory_flush"]
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
