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
	// Collect all .md files from the embedded prompts/ directory.
	entries, err := promptTemplatesFS.ReadDir("prompts")
	if err != nil {
		t.Fatalf("read embedded prompts dir: %v", err)
	}

	fileKeys := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".md")
		fileKeys[key] = struct{}{}
	}

	// Check: every template key has a corresponding file.
	for key := range defaultPromptTemplates {
		if _, ok := fileKeys[key]; !ok {
			t.Errorf("template key %q has no matching prompts/%s.md file", key, key)
		}
	}

	// Check: every file has a corresponding template key.
	for key := range fileKeys {
		if _, ok := defaultPromptTemplates[key]; !ok {
			t.Errorf("prompts/%s.md exists but has no entry in defaultPromptTemplates", key)
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

func TestPromptEventTypesSyncWithDescriptions(t *testing.T) {
	descriptions := PromptEventDescriptions()
	typeSet := make(map[string]struct{}, len(constants.PromptEventTypes))
	for _, et := range constants.PromptEventTypes {
		typeSet[et] = struct{}{}
	}

	for _, et := range constants.PromptEventTypes {
		if _, ok := descriptions[et]; !ok {
			t.Errorf("PromptEventTypes entry %q has no entry in promptEventDescriptions", et)
		}
	}
	for key := range descriptions {
		if _, ok := typeSet[key]; !ok {
			t.Errorf("promptEventDescriptions key %q missing from PromptEventTypes slice", key)
		}
	}
	if len(constants.PromptEventTypes) != len(descriptions) {
		t.Errorf("count mismatch: PromptEventTypes=%d vs promptEventDescriptions=%d",
			len(constants.PromptEventTypes), len(descriptions))
	}
}

func TestDefaultPromptTemplatesIncludesJournalCompaction(t *testing.T) {
	templates := DefaultPromptTemplates()
	content, ok := templates[constants.PromptEventJournalCompaction]
	if !ok {
		t.Fatal("expected journal_compaction key in DefaultPromptTemplates")
	}
	if !strings.Contains(content, "journal") {
		t.Error("journal_compaction template should mention journal")
	}
	if !strings.Contains(content, "{{.transcript}}") {
		t.Error("journal_compaction template should use {{.transcript}} placeholder")
	}
}

func TestDefaultPromptTemplatesIncludesConversationCompaction(t *testing.T) {
	templates := DefaultPromptTemplates()
	content, ok := templates[constants.PromptEventConversationCompaction]
	if !ok {
		t.Fatal("expected conversation_compaction key in DefaultPromptTemplates")
	}
	if !strings.Contains(content, "conversation") {
		t.Error("conversation_compaction template should mention conversation")
	}
	if !strings.Contains(content, "{{.transcript}}") {
		t.Error("conversation_compaction template should use {{.transcript}} placeholder")
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

