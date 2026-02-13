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
