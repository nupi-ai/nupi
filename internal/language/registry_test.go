package language

import (
	"testing"
)

func TestRegistryEntryCount(t *testing.T) {
	langs := All()
	if len(langs) < 180 {
		t.Errorf("expected at least 180 languages, got %d", len(langs))
	}
}

func TestLookupKnownLanguage(t *testing.T) {
	tests := []struct {
		iso1    string
		bcp47   string
		english string
		native  string
	}{
		{"pl", "pl-PL", "Polish", "Polski"},
		{"en", "en-US", "English", "English"},
		{"de", "de-DE", "German", "Deutsch"},
		{"ja", "ja-JP", "Japanese", "日本語"},
		{"ar", "ar-SA", "Arabic", "العربية"},
		{"zh", "zh-CN", "Chinese", "中文"},
	}

	for _, tt := range tests {
		t.Run(tt.iso1, func(t *testing.T) {
			lang, ok := Lookup(tt.iso1)
			if !ok {
				t.Fatalf("Lookup(%q) returned not found", tt.iso1)
			}
			if lang.ISO1 != tt.iso1 {
				t.Errorf("ISO1: got %q, want %q", lang.ISO1, tt.iso1)
			}
			if lang.BCP47 != tt.bcp47 {
				t.Errorf("BCP47: got %q, want %q", lang.BCP47, tt.bcp47)
			}
			if lang.EnglishName != tt.english {
				t.Errorf("EnglishName: got %q, want %q", lang.EnglishName, tt.english)
			}
			if lang.NativeName != tt.native {
				t.Errorf("NativeName: got %q, want %q", lang.NativeName, tt.native)
			}
		})
	}
}

func TestLookupInvalidCode(t *testing.T) {
	codes := []string{"xx", "zz", "", "123", "polish"}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			_, ok := Lookup(code)
			if ok {
				t.Errorf("Lookup(%q) should return not found", code)
			}
		})
	}
}

func TestAllEntriesHaveNonEmptyFields(t *testing.T) {
	for _, lang := range All() {
		if lang.ISO1 == "" {
			t.Error("found entry with empty ISO1")
		}
		if lang.BCP47 == "" {
			t.Errorf("language %q has empty BCP47", lang.ISO1)
		}
		if lang.EnglishName == "" {
			t.Errorf("language %q has empty EnglishName", lang.ISO1)
		}
		if lang.NativeName == "" {
			t.Errorf("language %q has empty NativeName", lang.ISO1)
		}
	}
}

func TestAllEntriesHaveUniqueISO1(t *testing.T) {
	seen := make(map[string]bool)
	for _, lang := range All() {
		if seen[lang.ISO1] {
			t.Errorf("duplicate ISO1 code: %q", lang.ISO1)
		}
		seen[lang.ISO1] = true
	}
}

func TestAllReturnsDefensiveCopy(t *testing.T) {
	a := All()
	b := All()
	if len(a) == 0 {
		t.Fatal("All() returned empty slice")
	}
	a[0] = Language{ISO1: "MODIFIED"}
	if b[0].ISO1 == "MODIFIED" {
		t.Error("All() does not return a defensive copy")
	}
}

func TestMapIndexConsistency(t *testing.T) {
	// Verify that byISO1 map has the same count as All().
	if len(byISO1) != len(all) {
		t.Errorf("byISO1 map size (%d) != all slice size (%d)", len(byISO1), len(all))
	}
}

func TestLookupCaseInsensitive(t *testing.T) {
	cases := []string{"PL", "Pl", "EN", "En", "DE", "De"}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			lang, ok := Lookup(code)
			if !ok {
				t.Fatalf("Lookup(%q) should match case-insensitively", code)
			}
			if lang.ISO1 == "" {
				t.Errorf("Lookup(%q) returned empty ISO1", code)
			}
		})
	}
}
