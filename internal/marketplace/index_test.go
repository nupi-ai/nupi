package marketplace

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// validIndexYAML returns a minimal valid marketplace index YAML string.
func validIndexYAML() string {
	return `
apiVersion: marketplace.nupi.ai/v1
namespace: acme
name: ACME Plugins
plugins:
  - slug: hello-world
    category: tools
    latest:
      archives:
        any:
          url: https://example.com/hello.tar.gz
          sha256: abc123
`
}

// mustParseIndex is a test helper that parses YAML and fails on error.
func mustParseIndex(t *testing.T, yaml string) *Index {
	t.Helper()
	idx, err := ParseIndex([]byte(yaml))
	if err != nil {
		t.Fatalf("mustParseIndex: unexpected error: %v", err)
	}
	return idx
}

// ---------------------------------------------------------------------------
// ParseIndex — valid YAML
// ---------------------------------------------------------------------------

func TestParseIndex_Valid(t *testing.T) {
	idx := mustParseIndex(t, validIndexYAML())

	if idx.APIVersion != "marketplace.nupi.ai/v1" {
		t.Errorf("APIVersion = %q, want %q", idx.APIVersion, "marketplace.nupi.ai/v1")
	}
	if idx.Namespace != "acme" {
		t.Errorf("Namespace = %q, want %q", idx.Namespace, "acme")
	}
	if idx.Name != "ACME Plugins" {
		t.Errorf("Name = %q, want %q", idx.Name, "ACME Plugins")
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("len(Plugins) = %d, want 1", len(idx.Plugins))
	}
	if idx.Plugins[0].Slug != "hello-world" {
		t.Errorf("Plugins[0].Slug = %q, want %q", idx.Plugins[0].Slug, "hello-world")
	}
}

func TestParseIndex_InvalidYAML(t *testing.T) {
	_, err := ParseIndex([]byte(`{{{not yaml`))
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parse marketplace index") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "parse marketplace index")
	}
}

// ---------------------------------------------------------------------------
// ParseIndex — missing required fields
// ---------------------------------------------------------------------------

func TestParseIndex_MissingAPIVersion(t *testing.T) {
	y := `
namespace: acme
plugins:
  - slug: foo
    category: tools
    latest:
      archives:
        any:
          url: https://example.com/foo.tar.gz
          sha256: abc
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for missing apiVersion")
	}
	if !strings.Contains(err.Error(), "apiVersion") {
		t.Errorf("error = %q, want it to mention apiVersion", err.Error())
	}
}

func TestParseIndex_UnsupportedAPIVersion(t *testing.T) {
	y := `
apiVersion: something.else/v1
namespace: acme
plugins: []
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for unsupported apiVersion")
	}
	if !strings.Contains(err.Error(), "unsupported apiVersion") {
		t.Errorf("error = %q, want it to mention unsupported apiVersion", err.Error())
	}
}

func TestParseIndex_MissingNamespace(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
plugins: []
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for missing namespace")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error = %q, want it to mention namespace", err.Error())
	}
}

func TestParseIndex_MissingSlug(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: acme
plugins:
  - category: tools
    latest:
      archives:
        any:
          url: https://example.com/x.tar.gz
          sha256: abc
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for missing slug")
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("error = %q, want it to mention slug", err.Error())
	}
}

func TestParseIndex_MissingCategory(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: acme
plugins:
  - slug: foo
    latest:
      archives:
        any:
          url: https://example.com/x.tar.gz
          sha256: abc
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for missing category")
	}
	if !strings.Contains(err.Error(), "category") {
		t.Errorf("error = %q, want it to mention category", err.Error())
	}
}

func TestParseIndex_MissingArchives(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: acme
plugins:
  - slug: foo
    category: tools
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for missing archives")
	}
	if !strings.Contains(err.Error(), "archives") {
		t.Errorf("error = %q, want it to mention archives", err.Error())
	}
}

// ---------------------------------------------------------------------------
// ParseIndex — invalid namespace characters / length
// ---------------------------------------------------------------------------

func TestParseIndex_InvalidNamespaceChars(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: "no spaces allowed"
plugins: []
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for invalid namespace chars")
	}
	if !strings.Contains(err.Error(), "invalid namespace") {
		t.Errorf("error = %q, want it to mention invalid namespace", err.Error())
	}
}

func TestParseIndex_NamespaceStartsWithDash(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: "-bad"
plugins: []
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for namespace starting with dash")
	}
	if !strings.Contains(err.Error(), "invalid namespace") {
		t.Errorf("error = %q, want it to mention invalid namespace", err.Error())
	}
}

func TestParseIndex_NamespaceTooLong(t *testing.T) {
	longNS := strings.Repeat("a", 129)
	y := "apiVersion: marketplace.nupi.ai/v1\nnamespace: " + longNS + "\nplugins: []\n"
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for namespace exceeding 128 chars")
	}
	if !strings.Contains(err.Error(), "invalid namespace") {
		t.Errorf("error = %q, want it to mention invalid namespace", err.Error())
	}
}

func TestParseIndex_NamespaceExactly128(t *testing.T) {
	ns := strings.Repeat("a", 128)
	y := "apiVersion: marketplace.nupi.ai/v1\nnamespace: " + ns + "\nplugins: []\n"
	idx, err := ParseIndex([]byte(y))
	if err != nil {
		t.Fatalf("expected no error for 128-char namespace, got: %v", err)
	}
	if idx.Namespace != ns {
		t.Errorf("Namespace length = %d, want 128", len(idx.Namespace))
	}
}

// ---------------------------------------------------------------------------
// validateIndex — edge cases
// ---------------------------------------------------------------------------

func TestValidateIndex_WhitespaceOnlyNamespace(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: "   "
plugins: []
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for whitespace-only namespace")
	}
}

func TestValidateIndex_InvalidSlugChars(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: acme
plugins:
  - slug: "has space"
    category: tools
    latest:
      archives:
        any:
          url: https://example.com/a.tar.gz
          sha256: abc
`
	_, err := ParseIndex([]byte(y))
	if err == nil {
		t.Fatal("expected error for invalid slug chars")
	}
	if !strings.Contains(err.Error(), "invalid slug") {
		t.Errorf("error = %q, want it to mention invalid slug", err.Error())
	}
}

func TestValidateIndex_NilArchiveEntry(t *testing.T) {
	// Construct an index directly to inject a nil archive pointer.
	idx := &Index{
		APIVersion: "marketplace.nupi.ai/v1",
		Namespace:  "acme",
		Plugins: []IndexPlugin{
			{
				Slug:     "foo",
				Category: "tools",
				Latest: LatestEntry{
					Archives: map[string]*Archive{
						"linux-amd64": nil,
					},
				},
			},
		},
	}
	err := validateIndex(idx)
	if err == nil {
		t.Fatal("expected error for nil archive entry")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error = %q, want it to mention nil", err.Error())
	}
}

func TestValidateIndex_ArchiveMissingURL(t *testing.T) {
	idx := &Index{
		APIVersion: "marketplace.nupi.ai/v1",
		Namespace:  "acme",
		Plugins: []IndexPlugin{
			{
				Slug:     "foo",
				Category: "tools",
				Latest: LatestEntry{
					Archives: map[string]*Archive{
						"any": {URL: "", SHA256: "abc123"},
					},
				},
			},
		},
	}
	err := validateIndex(idx)
	if err == nil {
		t.Fatal("expected error for archive missing url")
	}
	if !strings.Contains(err.Error(), "missing url") {
		t.Errorf("error = %q, want it to mention missing url", err.Error())
	}
}

func TestValidateIndex_ArchiveInvalidURLScheme(t *testing.T) {
	idx := &Index{
		APIVersion: "marketplace.nupi.ai/v1",
		Namespace:  "acme",
		Plugins: []IndexPlugin{
			{
				Slug:     "foo",
				Category: "tools",
				Latest: LatestEntry{
					Archives: map[string]*Archive{
						"any": {URL: "ftp://evil.com/payload.tar.gz", SHA256: "abc123"},
					},
				},
			},
		},
	}
	err := validateIndex(idx)
	if err == nil {
		t.Fatal("expected error for archive with ftp:// URL scheme")
	}
	if !strings.Contains(err.Error(), "invalid url") {
		t.Errorf("error = %q, want it to mention invalid url", err.Error())
	}
}

func TestValidateIndex_ArchiveURLMissingHost(t *testing.T) {
	idx := &Index{
		APIVersion: "marketplace.nupi.ai/v1",
		Namespace:  "acme",
		Plugins: []IndexPlugin{
			{
				Slug:     "foo",
				Category: "tools",
				Latest: LatestEntry{
					Archives: map[string]*Archive{
						"any": {URL: "https://", SHA256: "abc123"},
					},
				},
			},
		},
	}
	err := validateIndex(idx)
	if err == nil {
		t.Fatal("expected error for archive URL with missing host")
	}
	if !strings.Contains(err.Error(), "invalid url") {
		t.Errorf("error = %q, want it to mention invalid url", err.Error())
	}
}

func TestValidateIndex_ArchiveMissingSHA256(t *testing.T) {
	idx := &Index{
		APIVersion: "marketplace.nupi.ai/v1",
		Namespace:  "acme",
		Plugins: []IndexPlugin{
			{
				Slug:     "foo",
				Category: "tools",
				Latest: LatestEntry{
					Archives: map[string]*Archive{
						"any": {URL: "https://example.com/a.tar.gz", SHA256: ""},
					},
				},
			},
		},
	}
	err := validateIndex(idx)
	if err == nil {
		t.Fatal("expected error for archive missing sha256")
	}
	if !strings.Contains(err.Error(), "missing sha256") {
		t.Errorf("error = %q, want it to mention missing sha256", err.Error())
	}
}

func TestValidateIndex_NoPlugins(t *testing.T) {
	y := `
apiVersion: marketplace.nupi.ai/v1
namespace: acme
plugins: []
`
	idx, err := ParseIndex([]byte(y))
	if err != nil {
		t.Fatalf("expected no error for empty plugins list, got: %v", err)
	}
	if len(idx.Plugins) != 0 {
		t.Errorf("len(Plugins) = %d, want 0", len(idx.Plugins))
	}
}

// ---------------------------------------------------------------------------
// IndexPlugin.DisplayName
// ---------------------------------------------------------------------------

func TestDisplayName_WithName(t *testing.T) {
	p := &IndexPlugin{Slug: "my-plugin", Name: "My Plugin"}
	if got := p.DisplayName(); got != "My Plugin" {
		t.Errorf("DisplayName() = %q, want %q", got, "My Plugin")
	}
}

func TestDisplayName_FallbackToSlug(t *testing.T) {
	p := &IndexPlugin{Slug: "my-plugin"}
	if got := p.DisplayName(); got != "my-plugin" {
		t.Errorf("DisplayName() = %q, want %q", got, "my-plugin")
	}
}

func TestDisplayName_EmptyNameFallback(t *testing.T) {
	p := &IndexPlugin{Slug: "fallback-slug", Name: ""}
	if got := p.DisplayName(); got != "fallback-slug" {
		t.Errorf("DisplayName() = %q, want %q", got, "fallback-slug")
	}
}

// ---------------------------------------------------------------------------
// IndexPlugin.ArchiveForPlatform
// ---------------------------------------------------------------------------

func TestArchiveForPlatform_ExactMatch(t *testing.T) {
	p := &IndexPlugin{
		Slug: "test",
		Latest: LatestEntry{
			Archives: map[string]*Archive{
				"darwin-arm64": {URL: "https://example.com/darwin-arm64.tar.gz", SHA256: "aaa"},
				"linux-amd64":  {URL: "https://example.com/linux-amd64.tar.gz", SHA256: "bbb"},
				"any":          {URL: "https://example.com/any.tar.gz", SHA256: "ccc"},
			},
		},
	}

	a, key, err := p.ArchiveForPlatform("darwin", "arm64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "darwin-arm64" {
		t.Errorf("key = %q, want %q", key, "darwin-arm64")
	}
	if a.URL != "https://example.com/darwin-arm64.tar.gz" {
		t.Errorf("URL = %q, want darwin-arm64 archive URL", a.URL)
	}
}

func TestArchiveForPlatform_AnyFallback(t *testing.T) {
	p := &IndexPlugin{
		Slug: "test",
		Latest: LatestEntry{
			Archives: map[string]*Archive{
				"linux-amd64": {URL: "https://example.com/linux-amd64.tar.gz", SHA256: "bbb"},
				"any":         {URL: "https://example.com/any.tar.gz", SHA256: "ccc"},
			},
		},
	}

	a, key, err := p.ArchiveForPlatform("darwin", "arm64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "any" {
		t.Errorf("key = %q, want %q", key, "any")
	}
	if a.URL != "https://example.com/any.tar.gz" {
		t.Errorf("URL = %q, want any archive URL", a.URL)
	}
}

func TestArchiveForPlatform_NoMatch(t *testing.T) {
	p := &IndexPlugin{
		Slug: "test",
		Latest: LatestEntry{
			Archives: map[string]*Archive{
				"linux-amd64": {URL: "https://example.com/linux-amd64.tar.gz", SHA256: "bbb"},
			},
		},
	}

	_, _, err := p.ArchiveForPlatform("windows", "amd64")
	if err == nil {
		t.Fatal("expected error when no platform matches")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "not available")
	}
	if !strings.Contains(err.Error(), "windows-amd64") {
		t.Errorf("error = %q, want it to contain the requested platform", err.Error())
	}
}

// validateHTTPURL tests moved to internal/validate/validate_test.go
