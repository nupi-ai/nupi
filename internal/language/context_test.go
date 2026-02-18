package language

import (
	"context"
	"testing"
)

func TestWithLanguageAndFromContext(t *testing.T) {
	lang := Language{ISO1: "pl", BCP47: "pl-PL", EnglishName: "Polish", NativeName: "Polski"}
	ctx := WithLanguage(context.Background(), lang)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext returned not found")
	}
	if got != lang {
		t.Errorf("got %+v, want %+v", got, lang)
	}
}

func TestFromContextEmpty(t *testing.T) {
	_, ok := FromContext(context.Background())
	if ok {
		t.Error("FromContext should return false for empty context")
	}
}

func TestFromContextNil(t *testing.T) {
	_, ok := FromContext(nil)
	if ok {
		t.Error("FromContext should return false for nil context")
	}
}

func TestMergeToMetadata(t *testing.T) {
	lang := Language{ISO1: "de", BCP47: "de-DE", EnglishName: "German", NativeName: "Deutsch"}

	m := MergeToMetadata(lang, nil)
	if m[MetaISO1] != "de" {
		t.Errorf("iso1: got %q, want %q", m[MetaISO1], "de")
	}
	if m[MetaBCP47] != "de-DE" {
		t.Errorf("bcp47: got %q, want %q", m[MetaBCP47], "de-DE")
	}
	if m[MetaEnglish] != "German" {
		t.Errorf("english: got %q, want %q", m[MetaEnglish], "German")
	}
	if m[MetaNative] != "Deutsch" {
		t.Errorf("native: got %q, want %q", m[MetaNative], "Deutsch")
	}
}

func TestMergeToMetadataExistingMap(t *testing.T) {
	lang := Language{ISO1: "fr", BCP47: "fr-FR", EnglishName: "French", NativeName: "Français"}
	existing := map[string]string{"foo": "bar"}

	m := MergeToMetadata(lang, existing)
	if m["foo"] != "bar" {
		t.Error("existing key was lost")
	}
	if m[MetaISO1] != "fr" {
		t.Errorf("iso1: got %q, want %q", m[MetaISO1], "fr")
	}
}

func TestMergeContextLanguage(t *testing.T) {
	lang := Language{ISO1: "ja", BCP47: "ja-JP", EnglishName: "Japanese", NativeName: "日本語"}
	ctx := WithLanguage(context.Background(), lang)

	m := MergeContextLanguage(ctx, nil)
	if m[MetaISO1] != "ja" {
		t.Errorf("iso1: got %q, want %q", m[MetaISO1], "ja")
	}
}

func TestMergeContextLanguageNoLanguage(t *testing.T) {
	m := map[string]string{"existing": "value"}
	result := MergeContextLanguage(context.Background(), m)
	if result["existing"] != "value" {
		t.Error("existing value was lost")
	}
	if _, has := result[MetaISO1]; has {
		t.Error("should not have language keys when no language in context")
	}
}

func TestMetadataKeys(t *testing.T) {
	keys := MetadataKeys()
	expected := []string{MetaISO1, MetaBCP47, MetaEnglish, MetaNative}
	if len(keys) != len(expected) {
		t.Fatalf("expected %d keys, got %d", len(expected), len(keys))
	}
	for i, want := range expected {
		if keys[i] != want {
			t.Errorf("keys[%d]: got %q, want %q", i, keys[i], want)
		}
	}
}
