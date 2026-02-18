package marketplace

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// openTestStore creates a fresh SQLite-backed config store in a temp directory.
// The store is automatically closed when the test finishes.
// NOTE: t.Setenv("HOME") prevents tests using this helper from calling t.Parallel().
// This is acceptable because the store operations are fast and parallelism isn't needed.
func openTestStore(t *testing.T) *configstore.Store {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	dbPath := filepath.Join(tmpDir, "config.db")
	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// newTestClient creates a marketplace Client backed by a fresh store.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	return NewClient(openTestStore(t))
}

// testIndexYAML returns a valid index.yaml string with the given namespace and
// a single plugin.
func testIndexYAML(namespace, slug, category string) string {
	return fmt.Sprintf(`apiVersion: marketplace.nupi.ai/v1
namespace: %s
name: Test Marketplace
plugins:
  - slug: %s
    name: Test Plugin
    category: %s
    description: A test plugin
    latest:
      archives:
        any:
          url: https://example.com/test.tar.gz
          sha256: abc123def456
`, namespace, slug, category)
}

// testIndexYAMLMultiPlugin returns an index with multiple plugins.
func testIndexYAMLMultiPlugin(namespace string) string {
	return fmt.Sprintf(`apiVersion: marketplace.nupi.ai/v1
namespace: %s
name: Multi Plugin Marketplace
plugins:
  - slug: alpha-stt
    name: Alpha STT
    category: stt
    description: A speech-to-text plugin
    latest:
      archives:
        any:
          url: https://example.com/alpha.tar.gz
          sha256: aaa111
  - slug: beta-tts
    name: Beta TTS
    category: tts
    description: A text-to-speech plugin
    latest:
      archives:
        any:
          url: https://example.com/beta.tar.gz
          sha256: bbb222
  - slug: gamma-stt
    name: Gamma STT
    category: stt
    description: Another STT plugin
    latest:
      archives:
        any:
          url: https://example.com/gamma.tar.gz
          sha256: ccc333
`, namespace)
}

// serveIndex creates an httptest server that serves the given YAML on every request.
func serveIndex(t *testing.T, yaml string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(yaml))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveStatusCode creates an httptest server that always responds with the given status code.
func serveStatusCode(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedCache directly writes a cached index into the store for a given namespace.
func seedCache(t *testing.T, store *configstore.Store, namespace, indexYAML, lastRefreshed string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpdateMarketplaceCache(ctx, namespace, indexYAML, lastRefreshed); err != nil {
		t.Fatalf("seed cache for %q: %v", namespace, err)
	}
}

// ---------------------------------------------------------------------------
// NewClient
// ---------------------------------------------------------------------------

func TestNewClient(t *testing.T) {
	c := newTestClient(t)
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.cacheTTL != DefaultCacheTTL {
		t.Errorf("default cacheTTL = %v, want %v", c.cacheTTL, DefaultCacheTTL)
	}
	if c.http == nil {
		t.Fatal("http client is nil")
	}
}

func TestWithCacheTTL(t *testing.T) {
	c := newTestClient(t)
	custom := 5 * time.Minute
	returned := c.WithCacheTTL(custom)
	if returned != c {
		t.Fatal("WithCacheTTL should return the same Client pointer")
	}
	if c.cacheTTL != custom {
		t.Errorf("cacheTTL = %v, want %v", c.cacheTTL, custom)
	}
}

// ---------------------------------------------------------------------------
// Add
// ---------------------------------------------------------------------------

func TestAdd_Valid(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "test-marketplace"
	yaml := testIndexYAML(ns, "my-plugin", "stt")
	srv := serveIndex(t, yaml)

	m, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("Add returned nil marketplace")
	}
	if m.Namespace != ns {
		t.Errorf("Namespace = %q, want %q", m.Namespace, ns)
	}
	if m.URL != srv.URL {
		t.Errorf("URL = %q, want %q", m.URL, srv.URL)
	}
	if m.IsBuiltin {
		t.Error("expected IsBuiltin = false for user-added marketplace")
	}

	// Verify the cache was populated
	got, err := store.GetMarketplaceByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("GetMarketplaceByNamespace: %v", err)
	}
	if got.CachedIndex == "" {
		t.Error("expected CachedIndex to be populated after Add")
	}
	if got.LastRefreshed == "" {
		t.Error("expected LastRefreshed to be populated after Add")
	}
}

func TestAdd_EmptyURL(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	_, err := c.Add(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "URL is required") {
		t.Errorf("error = %q, want it to mention URL is required", err.Error())
	}
}

func TestAdd_WhitespaceOnlyURL(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	_, err := c.Add(ctx, "   ")
	if err == nil {
		t.Fatal("expected error for whitespace-only URL")
	}
	if !strings.Contains(err.Error(), "URL is required") {
		t.Errorf("error = %q, want it to mention URL is required", err.Error())
	}
}

func TestAdd_InvalidURLScheme(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	_, err := c.Add(ctx, "ftp://example.com/index.yaml")
	if err == nil {
		t.Fatal("expected error for ftp:// URL")
	}
	if !strings.Contains(err.Error(), "invalid marketplace URL") {
		t.Errorf("error = %q, want it to mention invalid marketplace URL", err.Error())
	}
}

func TestAdd_InvalidURLNoScheme(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	_, err := c.Add(ctx, "example.com/index.yaml")
	if err == nil {
		t.Fatal("expected error for URL without scheme")
	}
	if !strings.Contains(err.Error(), "invalid marketplace URL") {
		t.Errorf("error = %q, want it to mention invalid marketplace URL", err.Error())
	}
}

func TestAdd_DuplicateNamespace(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "dupe-ns"
	yaml := testIndexYAML(ns, "plugin-a", "stt")
	srv := serveIndex(t, yaml)

	// First add should succeed
	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("first Add: unexpected error: %v", err)
	}

	// Second add with same namespace should fail
	_, err = c.Add(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for duplicate namespace")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to mention already exists", err.Error())
	}
}

func TestAdd_ServerReturnsNon200(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	srv := serveStatusCode(t, http.StatusInternalServerError)

	_, err := c.Add(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want it to mention HTTP 500", err.Error())
	}
}

func TestAdd_ServerReturnsInvalidYAML(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	srv := serveIndex(t, "{{{not yaml")

	_, err := c.Add(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for invalid YAML response")
	}
}

func TestAdd_ServerReturnsMissingNamespace(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	yaml := `apiVersion: marketplace.nupi.ai/v1
plugins: []
`
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for index missing namespace")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error = %q, want it to mention namespace", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Remove
// ---------------------------------------------------------------------------

func TestRemove_UserAdded(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	// Add a marketplace first
	ns := "removable"
	yaml := testIndexYAML(ns, "plugin-x", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Remove it
	if err := c.Remove(ctx, ns); err != nil {
		t.Fatalf("Remove: unexpected error: %v", err)
	}

	// Verify it is gone
	_, err = store.GetMarketplaceByNamespace(ctx, ns)
	if err == nil {
		t.Fatal("expected not-found error after removal")
	}
}

func TestRemove_BuiltinFails(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	err := c.Remove(ctx, "ai.nupi")
	if err == nil {
		t.Fatal("expected error when removing builtin marketplace")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Errorf("error = %q, want it to mention built-in", err.Error())
	}
}

func TestRemove_BuiltinOthersFails(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	err := c.Remove(ctx, "others")
	if err == nil {
		t.Fatal("expected error when removing builtin 'others' marketplace")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Errorf("error = %q, want it to mention built-in", err.Error())
	}
}

func TestRemove_NotFound(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	err := c.Remove(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for removing nonexistent marketplace")
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestList_BuiltinMarketplaces(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	marketplaces, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// The store auto-seeds "ai.nupi" and "others"
	if len(marketplaces) < 2 {
		t.Fatalf("expected at least 2 builtin marketplaces, got %d", len(marketplaces))
	}

	namespaces := make(map[string]bool)
	for _, m := range marketplaces {
		namespaces[m.Namespace] = true
	}
	if !namespaces["ai.nupi"] {
		t.Error("missing builtin marketplace 'ai.nupi'")
	}
	if !namespaces["others"] {
		t.Error("missing builtin marketplace 'others'")
	}
}

func TestList_IncludesUserAdded(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "user-added"
	yaml := testIndexYAML(ns, "plugin-z", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	marketplaces, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	found := false
	for _, m := range marketplaces {
		if m.Namespace == ns {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("user-added marketplace %q not found in List output", ns)
	}
}

// ---------------------------------------------------------------------------
// Refresh
// ---------------------------------------------------------------------------

func TestRefresh_AllValid(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	// Serve a valid index for the builtin ai.nupi marketplace as well, so
	// Refresh does not fail on it. The test server returns whichever
	// namespace the request is for -- we use a single server that serves
	// the ai.nupi index for the builtin URL and the user-added one.
	aiNupiYAML := testIndexYAML("ai.nupi", "builtin-plug", "stt")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(aiNupiYAML))
	}))
	t.Cleanup(srv.Close)

	// Point the builtin ai.nupi marketplace at our test server so Refresh
	// can succeed without network access.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE marketplaces SET url = ? WHERE namespace = 'ai.nupi'`, srv.URL); err != nil {
		t.Fatalf("override ai.nupi URL: %v", err)
	}

	// Add a user marketplace with a working endpoint
	ns := "refreshable"
	userYAML := testIndexYAML(ns, "plug", "stt")
	userSrv := serveIndex(t, userYAML)

	_, err := c.Add(ctx, userSrv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Wipe the cache to ensure refresh re-populates it
	if err := store.UpdateMarketplaceCache(ctx, ns, "", ""); err != nil {
		t.Fatalf("clear cache: %v", err)
	}

	// Refresh all
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: unexpected error: %v", err)
	}

	// Verify the cache was restored
	m, err := store.GetMarketplaceByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("GetMarketplaceByNamespace: %v", err)
	}
	if m.CachedIndex == "" {
		t.Error("expected CachedIndex to be populated after Refresh")
	}
}

func TestRefresh_SkipsEmptyURL(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	// The "others" marketplace has no URL; Refresh should skip it without error.
	if err := c.Refresh(ctx); err != nil {
		// The builtin "ai.nupi" marketplace points to a real URL that may fail
		// in tests. We only care that "others" didn't cause an issue.
		// If the error is about "ai.nupi" that's expected in test environments.
		if !strings.Contains(err.Error(), "ai.nupi") {
			t.Fatalf("Refresh: unexpected error not related to ai.nupi: %v", err)
		}
	}
}

func TestRefresh_ReportsErrors(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	// Add a marketplace with a failing endpoint
	ns := "failing"
	yaml := testIndexYAML(ns, "plug", "stt")

	// Serve once for Add, then close the server to cause refresh failure
	addSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(yaml))
	}))

	_, err := c.Add(ctx, addSrv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Close the server to make refresh fail
	addSrv.Close()

	err = c.Refresh(ctx)
	if err == nil {
		t.Fatal("expected error from Refresh when endpoint is down")
	}
	if !strings.Contains(err.Error(), ns) {
		t.Errorf("error = %q, want it to mention namespace %q", err.Error(), ns)
	}
}

// ---------------------------------------------------------------------------
// RefreshOne
// ---------------------------------------------------------------------------

func TestRefreshOne_Success(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "single-refresh"
	yaml := testIndexYAML(ns, "plug", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Clear cache, then refresh just this one
	if err := store.UpdateMarketplaceCache(ctx, ns, "", ""); err != nil {
		t.Fatalf("clear cache: %v", err)
	}

	if err := c.RefreshOne(ctx, ns); err != nil {
		t.Fatalf("RefreshOne: unexpected error: %v", err)
	}

	m, err := store.GetMarketplaceByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("GetMarketplaceByNamespace: %v", err)
	}
	if m.CachedIndex == "" {
		t.Error("expected CachedIndex to be populated after RefreshOne")
	}
}

func TestRefreshOne_NotFound(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	err := c.RefreshOne(ctx, "nonexistent-ns")
	if err == nil {
		t.Fatal("expected error for nonexistent namespace")
	}
}

func TestRefreshOne_EmptyURLNoOp(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	// "others" has no URL, so RefreshOne should be a no-op
	if err := c.RefreshOne(ctx, "others"); err != nil {
		t.Fatalf("RefreshOne on empty-URL marketplace: unexpected error: %v", err)
	}
}

func TestRefreshOne_304NotModified(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "notmod-ns"
	yaml := testIndexYAML(ns, "plug", "stt")

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Header.Get("If-Modified-Since") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(yaml))
	}))
	t.Cleanup(srv.Close)

	// Add marketplace (first request, serves 200)
	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Get cached index before refresh
	m1, err := store.GetMarketplaceByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}
	cachedBefore := m1.CachedIndex

	// RefreshOne should send If-Modified-Since and get 304
	if err := c.RefreshOne(ctx, ns); err != nil {
		t.Fatalf("RefreshOne: unexpected error: %v", err)
	}

	// Cache should remain unchanged
	m2, err := store.GetMarketplaceByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("get marketplace after refresh: %v", err)
	}
	if m2.CachedIndex != cachedBefore {
		t.Error("expected cache to remain unchanged after 304 response")
	}

	if requestCount < 2 {
		t.Errorf("expected at least 2 requests (add + refresh), got %d", requestCount)
	}
}

func TestRefreshOne_NamespaceMismatch(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	// Add a marketplace with namespace "orig-ns"
	origYAML := testIndexYAML("orig-ns", "plug", "stt")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// On refresh, return an index with a different namespace
		changedYAML := testIndexYAML("different-ns", "plug", "stt")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(changedYAML))
	}))
	t.Cleanup(srv.Close)

	// Manually add the marketplace to the store so it has the URL
	_, err := store.AddMarketplace(ctx, "orig-ns", srv.URL)
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}

	// Seed with original valid index
	seedCache(t, store, "orig-ns", origYAML, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339))

	err = c.RefreshOne(ctx, "orig-ns")
	if err == nil {
		t.Fatal("expected error for namespace mismatch")
	}
	if !strings.Contains(err.Error(), "namespace mismatch") {
		t.Errorf("error = %q, want it to mention namespace mismatch", err.Error())
	}
}

// ---------------------------------------------------------------------------
// GetIndex
// ---------------------------------------------------------------------------

func TestGetIndex_FreshCache(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "cached-ns"
	yaml := testIndexYAML(ns, "my-plug", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Cache should be fresh (just added)
	idx, err := c.GetIndex(ctx, ns)
	if err != nil {
		t.Fatalf("GetIndex: %v", err)
	}
	if idx.Namespace != ns {
		t.Errorf("Namespace = %q, want %q", idx.Namespace, ns)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("len(Plugins) = %d, want 1", len(idx.Plugins))
	}
	if idx.Plugins[0].Slug != "my-plug" {
		t.Errorf("Plugins[0].Slug = %q, want %q", idx.Plugins[0].Slug, "my-plug")
	}
}

func TestGetIndex_StaleCache_RefreshSucceeds(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Minute)
	ctx := context.Background()

	ns := "stale-ns"
	yaml := testIndexYAML(ns, "stale-plug", "stt")
	srv := serveIndex(t, yaml)

	// Manually add marketplace and seed stale cache
	_, err := store.AddMarketplace(ctx, ns, srv.URL)
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	staleTime := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	seedCache(t, store, ns, yaml, staleTime)

	// GetIndex should detect stale cache, refresh, and return new index
	idx, err := c.GetIndex(ctx, ns)
	if err != nil {
		t.Fatalf("GetIndex: %v", err)
	}
	if idx.Namespace != ns {
		t.Errorf("Namespace = %q, want %q", idx.Namespace, ns)
	}
}

func TestGetIndex_StaleCache_RefreshFails_UsesStaleCacheAsFallback(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Minute)
	ctx := context.Background()

	ns := "stale-fallback"
	yaml := testIndexYAML(ns, "fallback-plug", "stt")

	// Create a server just for the Add, then shut it down
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(yaml))
	}))

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Make the cache stale
	staleTime := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	seedCache(t, store, ns, yaml, staleTime)

	// Close server to make refresh fail
	srv.Close()

	// GetIndex should fall back to stale cache
	idx, err := c.GetIndex(ctx, ns)
	if err != nil {
		t.Fatalf("GetIndex should fall back to stale cache, got error: %v", err)
	}
	if idx.Namespace != ns {
		t.Errorf("Namespace = %q, want %q", idx.Namespace, ns)
	}
	if idx.Plugins[0].Slug != "fallback-plug" {
		t.Errorf("Plugins[0].Slug = %q, want %q", idx.Plugins[0].Slug, "fallback-plug")
	}
}

func TestGetIndex_NoCache_FetchesFromURL(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "nocache-ns"
	yaml := testIndexYAML(ns, "fresh-plug", "stt")
	srv := serveIndex(t, yaml)

	// Add marketplace manually (without using c.Add which would cache)
	_, err := store.AddMarketplace(ctx, ns, srv.URL)
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}

	// GetIndex with no cache should fetch from URL
	idx, err := c.GetIndex(ctx, ns)
	if err != nil {
		t.Fatalf("GetIndex: %v", err)
	}
	if idx.Namespace != ns {
		t.Errorf("Namespace = %q, want %q", idx.Namespace, ns)
	}
}

func TestGetIndex_NoCacheNoURL(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	// "others" has no URL and no cached index
	_, err := c.GetIndex(ctx, "others")
	if err == nil {
		t.Fatal("expected error for marketplace with no URL and no cache")
	}
	if !strings.Contains(err.Error(), "no index") || !strings.Contains(err.Error(), "others") {
		t.Errorf("error = %q, want it to mention 'no index' and 'others'", err.Error())
	}
}

func TestGetIndex_NotFound(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	_, err := c.GetIndex(ctx, "nonexistent-ns")
	if err == nil {
		t.Fatal("expected error for nonexistent namespace")
	}
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func TestSearch_AllPlugins(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-ns"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Empty query = return all
	results, err := c.Search(ctx, "", "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Should find all 3 plugins from our marketplace
	found := 0
	for _, r := range results {
		if r.Namespace == ns {
			found++
		}
	}
	if found != 3 {
		t.Errorf("expected 3 plugins from %q, got %d", ns, found)
	}
}

func TestSearch_ByQuery(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-query"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Search by term that matches name "Alpha STT"
	results, err := c.Search(ctx, "alpha", "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Plugin.Slug == "alpha-stt" {
			found = true
		}
	}
	if !found {
		t.Error("expected to find alpha-stt plugin when searching for 'alpha'")
	}

	// Ensure non-matching plugins were filtered out
	for _, r := range results {
		if r.Namespace == ns && r.Plugin.Slug == "beta-tts" {
			t.Error("beta-tts should not match query 'alpha'")
		}
	}
}

func TestSearch_ByQueryMatchesDescription(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-desc"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// "speech-to-text" appears in the description of alpha-stt
	results, err := c.Search(ctx, "speech-to-text", "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Plugin.Slug == "alpha-stt" {
			found = true
		}
	}
	if !found {
		t.Error("expected description-based match for 'speech-to-text'")
	}
}

func TestSearch_ByQueryCaseInsensitive(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-case"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	results, err := c.Search(ctx, "GAMMA", "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Plugin.Slug == "gamma-stt" {
			found = true
		}
	}
	if !found {
		t.Error("expected case-insensitive match for 'GAMMA'")
	}
}

func TestSearch_ByCategoryFilter(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-cat"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Filter by category=stt
	results, err := c.Search(ctx, "", "stt", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, r := range results {
		if r.Namespace == ns && r.Plugin.Category != "stt" {
			t.Errorf("found plugin %q with category %q, expected only stt", r.Plugin.Slug, r.Plugin.Category)
		}
	}

	sttCount := 0
	for _, r := range results {
		if r.Namespace == ns {
			sttCount++
		}
	}
	if sttCount != 2 {
		t.Errorf("expected 2 stt plugins from %q, got %d", ns, sttCount)
	}
}

func TestSearch_ByCategoryCaseInsensitive(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-cat-ci"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Category in index is lowercase "stt"; search with uppercase "STT"
	results, err := c.Search(ctx, "", "STT", ns)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	sttCount := 0
	for _, r := range results {
		if r.Namespace == ns {
			sttCount++
			if !strings.EqualFold(r.Plugin.Category, "stt") {
				t.Errorf("found plugin %q with category %q, expected stt", r.Plugin.Slug, r.Plugin.Category)
			}
		}
	}
	if sttCount != 2 {
		t.Errorf("expected 2 stt plugins (case-insensitive), got %d", sttCount)
	}
}

func TestSearch_ByNamespaceFilter(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-nsfilter"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Filter by specific namespace
	results, err := c.Search(ctx, "", "", ns)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, r := range results {
		if r.Namespace != ns {
			t.Errorf("found result from namespace %q, expected only %q", r.Namespace, ns)
		}
	}
	if len(results) != 3 {
		t.Errorf("expected 3 plugins from namespace %q, got %d", ns, len(results))
	}
}

func TestSearch_QueryAndCategoryCombined(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-combo"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Search for "stt" in name/slug/desc, and filter to category "stt"
	// This should match alpha-stt and gamma-stt but not beta-tts
	results, err := c.Search(ctx, "stt", "stt", ns)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results for query=stt + category=stt, got %d", len(results))
	}
	for _, r := range results {
		if r.Plugin.Category != "stt" {
			t.Errorf("unexpected category %q for plugin %q", r.Plugin.Category, r.Plugin.Slug)
		}
	}
}

func TestSearch_NoMatches(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-nomatch"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	results, err := c.Search(ctx, "zzz-nonexistent-query", "", ns)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearch_MatchesBySlug(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "search-slug"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// "beta-tts" should match by slug
	results, err := c.Search(ctx, "beta-tts", "", ns)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result for slug match 'beta-tts', got %d", len(results))
	}
	if results[0].Plugin.Slug != "beta-tts" {
		t.Errorf("expected slug 'beta-tts', got %q", results[0].Plugin.Slug)
	}
}

// ---------------------------------------------------------------------------
// FindPlugin
// ---------------------------------------------------------------------------

func TestFindPlugin_Found(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "findme"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	plugin, err := c.FindPlugin(ctx, ns, "beta-tts")
	if err != nil {
		t.Fatalf("FindPlugin: %v", err)
	}
	if plugin.Slug != "beta-tts" {
		t.Errorf("Slug = %q, want %q", plugin.Slug, "beta-tts")
	}
	if plugin.Name != "Beta TTS" {
		t.Errorf("Name = %q, want %q", plugin.Name, "Beta TTS")
	}
	if plugin.Category != "tts" {
		t.Errorf("Category = %q, want %q", plugin.Category, "tts")
	}
}

func TestFindPlugin_NotFound(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "findme-nf"
	yaml := testIndexYAMLMultiPlugin(ns)
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err = c.FindPlugin(ctx, ns, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention 'not found'", err.Error())
	}
}

func TestFindPlugin_NamespaceNotFound(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	_, err := c.FindPlugin(ctx, "nonexistent-ns", "some-slug")
	if err == nil {
		t.Fatal("expected error for nonexistent namespace")
	}
}

func TestFindPlugin_ReturnsArchiveInfo(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "archive-info"
	yaml := testIndexYAML(ns, "with-archive", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	plugin, err := c.FindPlugin(ctx, ns, "with-archive")
	if err != nil {
		t.Fatalf("FindPlugin: %v", err)
	}

	archive, ok := plugin.Latest.Archives["any"]
	if !ok {
		t.Fatal("expected 'any' archive entry")
	}
	if archive.URL != "https://example.com/test.tar.gz" {
		t.Errorf("archive URL = %q, want %q", archive.URL, "https://example.com/test.tar.gz")
	}
	if archive.SHA256 != "abc123def456" {
		t.Errorf("archive SHA256 = %q, want %q", archive.SHA256, "abc123def456")
	}
}

// ---------------------------------------------------------------------------
// isCacheFresh
// ---------------------------------------------------------------------------

func TestIsCacheFresh_Fresh(t *testing.T) {
	c := NewClient(nil).WithCacheTTL(1 * time.Hour)
	ts := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	if !c.isCacheFresh(ts) {
		t.Error("expected cache from 30 minutes ago to be fresh with 1h TTL")
	}
}

func TestIsCacheFresh_Stale(t *testing.T) {
	c := NewClient(nil).WithCacheTTL(1 * time.Hour)
	ts := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if c.isCacheFresh(ts) {
		t.Error("expected cache from 2 hours ago to be stale with 1h TTL")
	}
}

func TestIsCacheFresh_Empty(t *testing.T) {
	c := NewClient(nil).WithCacheTTL(1 * time.Hour)
	if c.isCacheFresh("") {
		t.Error("expected empty lastRefreshed to be stale")
	}
}

func TestIsCacheFresh_InvalidFormat(t *testing.T) {
	c := NewClient(nil).WithCacheTTL(1 * time.Hour)
	if c.isCacheFresh("not-a-date") {
		t.Error("expected invalid date format to be stale")
	}
}

func TestIsCacheFresh_Pre2020Rejected(t *testing.T) {
	c := NewClient(nil).WithCacheTTL(100 * 365 * 24 * time.Hour) // Very long TTL
	ts := "2019-12-31T23:59:59Z"
	if c.isCacheFresh(ts) {
		t.Error("expected pre-2020 timestamp to be rejected as stale")
	}
}

func TestIsCacheFresh_Year2020Accepted(t *testing.T) {
	c := NewClient(nil).WithCacheTTL(100 * 365 * 24 * time.Hour) // Very long TTL
	ts := "2020-01-01T00:00:00Z"
	if !c.isCacheFresh(ts) {
		t.Error("expected year 2020 timestamp to be accepted with long TTL")
	}
}

func TestIsCacheFresh_ExactBoundary(t *testing.T) {
	ttl := 10 * time.Minute
	c := NewClient(nil).WithCacheTTL(ttl)

	// Just under the TTL
	justUnder := time.Now().Add(-ttl + 5*time.Second).UTC().Format(time.RFC3339)
	if !c.isCacheFresh(justUnder) {
		t.Error("expected timestamp just under TTL to be fresh")
	}

	// Just over the TTL
	justOver := time.Now().Add(-ttl - 5*time.Second).UTC().Format(time.RFC3339)
	if c.isCacheFresh(justOver) {
		t.Error("expected timestamp just over TTL to be stale")
	}
}

func TestIsCacheFresh_ZeroTTL(t *testing.T) {
	c := NewClient(nil).WithCacheTTL(0)
	ts := time.Now().UTC().Format(time.RFC3339)
	// With zero TTL, time.Since(t) will be >= 0 which is not < 0
	if c.isCacheFresh(ts) {
		t.Error("expected zero TTL to always be stale")
	}
}

// ---------------------------------------------------------------------------
// Integration: Add then Search round-trip
// ---------------------------------------------------------------------------

func TestAddThenSearch_RoundTrip(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "roundtrip"
	yaml := testIndexYAML(ns, "round-plug", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	results, err := c.Search(ctx, "round-plug", "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Namespace == ns && r.Plugin.Slug == "round-plug" {
			found = true
		}
	}
	if !found {
		t.Error("expected to find round-plug via Search after Add")
	}
}

func TestAddThenFindPlugin_RoundTrip(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "roundtrip-find"
	yaml := testIndexYAML(ns, "findable", "tts")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	plugin, err := c.FindPlugin(ctx, ns, "findable")
	if err != nil {
		t.Fatalf("FindPlugin: %v", err)
	}
	if plugin.Slug != "findable" {
		t.Errorf("Slug = %q, want %q", plugin.Slug, "findable")
	}
	if plugin.Category != "tts" {
		t.Errorf("Category = %q, want %q", plugin.Category, "tts")
	}
}

// ---------------------------------------------------------------------------
// Integration: Add, Remove, then verify Search is empty
// ---------------------------------------------------------------------------

func TestAddRemoveSearch_RoundTrip(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "transient"
	yaml := testIndexYAML(ns, "temp-plug", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := c.Remove(ctx, ns); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	results, err := c.Search(ctx, "temp-plug", "", ns)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results after removal, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// HTTP behavior: User-Agent header
// ---------------------------------------------------------------------------

func TestFetchSendsUserAgent(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "ua-test"
	yaml := testIndexYAML(ns, "ua-plug", "stt")

	var receivedUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(yaml))
	}))
	t.Cleanup(srv.Close)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !strings.Contains(receivedUA, "nupi-marketplace-client") {
		t.Errorf("User-Agent = %q, want it to contain 'nupi-marketplace-client'", receivedUA)
	}
}

// ---------------------------------------------------------------------------
// HTTP behavior: various error status codes
// ---------------------------------------------------------------------------

func TestAdd_HTTP404(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	srv := serveStatusCode(t, http.StatusNotFound)
	_, err := c.Add(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error = %q, want it to mention HTTP 404", err.Error())
	}
}

func TestAdd_HTTP403(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	srv := serveStatusCode(t, http.StatusForbidden)
	_, err := c.Add(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("error = %q, want it to mention HTTP 403", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Context cancellation
// ---------------------------------------------------------------------------

func TestAdd_CancelledContext(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	ns := "cancelled"
	yaml := testIndexYAML(ns, "plug", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Search with no cached indices (should not panic)
// ---------------------------------------------------------------------------

func TestSearch_NoCachedIndices(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	// Builtin marketplaces exist but have no cached indices.
	// Search should return empty results, not panic or error.
	// Note: ai.nupi has a URL so Search may try to refresh it and fail,
	// but it should still return gracefully.
	results, err := c.Search(ctx, "anything", "", "others")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results from 'others' with no cache, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// SearchResult namespace is populated correctly
// ---------------------------------------------------------------------------

func TestSearchResult_NamespacePopulated(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store).WithCacheTTL(1 * time.Hour)
	ctx := context.Background()

	ns := "ns-check"
	yaml := testIndexYAML(ns, "ns-plug", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	results, err := c.Search(ctx, "", "", ns)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, r := range results {
		if r.Namespace == "" {
			t.Error("SearchResult.Namespace should not be empty")
		}
		if r.Namespace != ns {
			t.Errorf("SearchResult.Namespace = %q, want %q", r.Namespace, ns)
		}
	}
}

// ---------------------------------------------------------------------------
// Refresh updates the last_refreshed timestamp
// ---------------------------------------------------------------------------

func TestRefreshOne_UpdatesTimestamp(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "ts-update"
	yaml := testIndexYAML(ns, "ts-plug", "stt")
	srv := serveIndex(t, yaml)

	_, err := c.Add(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	m1, err := store.GetMarketplaceByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	// Clear the Last-Modified check by clearing last_refreshed
	if err := store.UpdateMarketplaceCache(ctx, ns, m1.CachedIndex, ""); err != nil {
		t.Fatalf("clear last_refreshed: %v", err)
	}

	if err := c.RefreshOne(ctx, ns); err != nil {
		t.Fatalf("RefreshOne: %v", err)
	}

	m2, err := store.GetMarketplaceByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("get marketplace after refresh: %v", err)
	}

	if m2.LastRefreshed == "" {
		t.Error("expected LastRefreshed to be set after RefreshOne")
	}

	// Parse and verify it's a valid RFC3339 timestamp
	_, err = time.Parse(time.RFC3339, m2.LastRefreshed)
	if err != nil {
		t.Errorf("LastRefreshed %q is not valid RFC3339: %v", m2.LastRefreshed, err)
	}
}

// ---------------------------------------------------------------------------
// Add with URL that has trailing whitespace
// ---------------------------------------------------------------------------

func TestAdd_URLTrimmed(t *testing.T) {
	store := openTestStore(t)
	c := NewClient(store)
	ctx := context.Background()

	ns := "trim-url"
	yaml := testIndexYAML(ns, "trim-plug", "stt")
	srv := serveIndex(t, yaml)

	// Add with trailing whitespace
	m, err := c.Add(ctx, "  "+srv.URL+"  ")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if m.URL != srv.URL {
		t.Errorf("URL = %q, want trimmed %q", m.URL, srv.URL)
	}
}

// ---------------------------------------------------------------------------
// GetIndex with builtin ai.nupi (has URL but no cache) does not panic
// ---------------------------------------------------------------------------

func TestGetIndex_BuiltinAiNupi_NoPanic(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	// ai.nupi has a URL pointing to GitHub. In test environments this will
	// likely fail to fetch. The important thing is it doesn't panic.
	_, err := c.GetIndex(ctx, "ai.nupi")
	// We expect an error (network failure) but no panic.
	if err == nil {
		// It's OK if it succeeds (e.g., test environment has internet).
		return
	}
	// Any error is fine as long as we get here without panic.
}
