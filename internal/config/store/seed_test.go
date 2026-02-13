package store

import "testing"

func TestDefaultPromptTemplatesNotEmpty(t *testing.T) {
	templates := DefaultPromptTemplates()
	if len(templates) != 4 {
		t.Fatalf("expected 4 default templates, got %d", len(templates))
	}
	for name, content := range templates {
		if content == "" {
			t.Fatalf("default template %q is empty", name)
		}
	}
}
