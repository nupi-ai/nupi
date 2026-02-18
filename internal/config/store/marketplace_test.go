package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, context.Background()
}

func openReadOnlyTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	// First open read-write to create schema + seed data.
	rw, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open rw store for setup: %v", err)
	}
	rw.Close()

	ro, err := Open(Options{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	return ro, context.Background()
}

// ---------------------------------------------------------------------------
// ListMarketplaces
// ---------------------------------------------------------------------------

func TestListMarketplaces_SeededBuiltins(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	mps, err := store.ListMarketplaces(ctx)
	if err != nil {
		t.Fatalf("list marketplaces: %v", err)
	}
	if len(mps) != 2 {
		t.Fatalf("expected 2 seeded marketplaces, got %d", len(mps))
	}

	// Ordered by namespace: ai.nupi, others
	if mps[0].Namespace != "ai.nupi" {
		t.Fatalf("expected first namespace ai.nupi, got %q", mps[0].Namespace)
	}
	if mps[1].Namespace != "others" {
		t.Fatalf("expected second namespace others, got %q", mps[1].Namespace)
	}

	// Verify builtin flags
	if !mps[0].IsBuiltin {
		t.Fatalf("expected ai.nupi to be builtin")
	}
	if !mps[1].IsBuiltin {
		t.Fatalf("expected others to be builtin")
	}

	// Verify instance name
	if mps[0].InstanceName != store.InstanceName() {
		t.Fatalf("expected instance %q, got %q", store.InstanceName(), mps[0].InstanceName)
	}

	// Verify ai.nupi URL
	if mps[0].URL != BuiltinMarketplaceURL {
		t.Fatalf("expected ai.nupi URL %q, got %q", BuiltinMarketplaceURL, mps[0].URL)
	}
	if mps[1].URL != "" {
		t.Fatalf("expected others URL to be empty, got %q", mps[1].URL)
	}
}

func TestListMarketplaces_IncludesUserAdded(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if _, err := store.AddMarketplace(ctx, "custom.registry", "https://example.com/index.yaml"); err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	mps, err := store.ListMarketplaces(ctx)
	if err != nil {
		t.Fatalf("list marketplaces: %v", err)
	}
	if len(mps) != 3 {
		t.Fatalf("expected 3 marketplaces, got %d", len(mps))
	}

	// Verify ordering: ai.nupi, custom.registry, others
	expected := []string{"ai.nupi", "custom.registry", "others"}
	for i, ns := range expected {
		if mps[i].Namespace != ns {
			t.Fatalf("position %d: expected %q, got %q", i, ns, mps[i].Namespace)
		}
	}

	// User-added must not be builtin
	if mps[1].IsBuiltin {
		t.Fatalf("expected custom.registry to not be builtin")
	}
}

// ---------------------------------------------------------------------------
// GetMarketplaceByNamespace
// ---------------------------------------------------------------------------

func TestGetMarketplaceByNamespace_Builtin(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}
	if m.Namespace != "ai.nupi" {
		t.Fatalf("expected namespace ai.nupi, got %q", m.Namespace)
	}
	if !m.IsBuiltin {
		t.Fatalf("expected builtin=true")
	}
	if m.URL != BuiltinMarketplaceURL {
		t.Fatalf("expected URL %q, got %q", BuiltinMarketplaceURL, m.URL)
	}
	if m.ID == 0 {
		t.Fatalf("expected non-zero ID")
	}
	if m.CreatedAt == "" {
		t.Fatalf("expected non-empty created_at")
	}
}

func TestGetMarketplaceByNamespace_NotFound(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetMarketplaceByNamespace(ctx, "nonexistent")
	if err == nil {
		t.Fatalf("expected error for nonexistent namespace")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestGetMarketplaceByNamespace_EmptyNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetMarketplaceByNamespace(ctx, "")
	if err == nil {
		t.Fatalf("expected error for empty namespace")
	}
	if !strings.Contains(err.Error(), "namespace required") {
		t.Fatalf("expected 'namespace required' error, got: %v", err)
	}
}

func TestGetMarketplaceByNamespace_WhitespaceOnlyNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetMarketplaceByNamespace(ctx, "   ")
	if err == nil {
		t.Fatalf("expected error for whitespace-only namespace")
	}
	if !strings.Contains(err.Error(), "namespace required") {
		t.Fatalf("expected 'namespace required' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetMarketplaceByID
// ---------------------------------------------------------------------------

func TestGetMarketplaceByID_Builtin(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	// First get the ID via namespace
	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get by namespace: %v", err)
	}

	byID, err := store.GetMarketplaceByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get by ID: %v", err)
	}
	if byID.Namespace != "ai.nupi" {
		t.Fatalf("expected namespace ai.nupi, got %q", byID.Namespace)
	}
	if byID.ID != m.ID {
		t.Fatalf("expected ID %d, got %d", m.ID, byID.ID)
	}
}

func TestGetMarketplaceByID_NotFound(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetMarketplaceByID(ctx, 99999)
	if err == nil {
		t.Fatalf("expected error for nonexistent ID")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestGetMarketplaceByID_ZeroID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetMarketplaceByID(ctx, 0)
	if err == nil {
		t.Fatalf("expected error for zero ID")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestGetMarketplaceByID_NegativeID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetMarketplaceByID(ctx, -1)
	if err == nil {
		t.Fatalf("expected error for negative ID")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// AddMarketplace
// ---------------------------------------------------------------------------

func TestAddMarketplace_Success(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "my.registry", "https://example.com/index.yaml")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}
	if m.Namespace != "my.registry" {
		t.Fatalf("expected namespace my.registry, got %q", m.Namespace)
	}
	if m.URL != "https://example.com/index.yaml" {
		t.Fatalf("expected URL, got %q", m.URL)
	}
	if m.ID == 0 {
		t.Fatalf("expected non-zero ID")
	}
	if m.InstanceName != store.InstanceName() {
		t.Fatalf("expected instance %q, got %q", store.InstanceName(), m.InstanceName)
	}

	// Verify it can be retrieved
	got, err := store.GetMarketplaceByNamespace(ctx, "my.registry")
	if err != nil {
		t.Fatalf("get added marketplace: %v", err)
	}
	if got.ID != m.ID {
		t.Fatalf("expected ID %d, got %d", m.ID, got.ID)
	}
	if got.IsBuiltin {
		t.Fatalf("user-added marketplace should not be builtin")
	}
}

func TestAddMarketplace_EmptyURL(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "local.only", "")
	if err != nil {
		t.Fatalf("add marketplace with empty URL: %v", err)
	}
	if m.URL != "" {
		t.Fatalf("expected empty URL, got %q", m.URL)
	}
}

func TestAddMarketplace_DuplicateNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if _, err := store.AddMarketplace(ctx, "dup.ns", "https://one.example.com"); err != nil {
		t.Fatalf("first add: %v", err)
	}

	_, err := store.AddMarketplace(ctx, "dup.ns", "https://two.example.com")
	if err == nil {
		t.Fatalf("expected error for duplicate namespace")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got: %v", err)
	}
}

func TestAddMarketplace_DuplicateBuiltinNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.AddMarketplace(ctx, "ai.nupi", "https://evil.com")
	if err == nil {
		t.Fatalf("expected error for duplicate builtin namespace")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got: %v", err)
	}
}

func TestAddMarketplace_EmptyNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.AddMarketplace(ctx, "", "https://example.com")
	if err == nil {
		t.Fatalf("expected error for empty namespace")
	}
	if !strings.Contains(err.Error(), "namespace required") {
		t.Fatalf("expected 'namespace required' error, got: %v", err)
	}
}

func TestAddMarketplace_WhitespaceNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.AddMarketplace(ctx, "   ", "https://example.com")
	if err == nil {
		t.Fatalf("expected error for whitespace-only namespace")
	}
	if !strings.Contains(err.Error(), "namespace required") {
		t.Fatalf("expected 'namespace required' error, got: %v", err)
	}
}

func TestAddMarketplace_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	_, err := store.AddMarketplace(ctx, "new.mp", "https://example.com")
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RemoveMarketplace
// ---------------------------------------------------------------------------

func TestRemoveMarketplace_UserAdded(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if _, err := store.AddMarketplace(ctx, "removable", "https://example.com"); err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	if err := store.RemoveMarketplace(ctx, "removable"); err != nil {
		t.Fatalf("remove marketplace: %v", err)
	}

	// Verify it's gone
	_, err := store.GetMarketplaceByNamespace(ctx, "removable")
	if err == nil {
		t.Fatalf("expected not found after removal")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestRemoveMarketplace_BuiltinProtected(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.RemoveMarketplace(ctx, "ai.nupi")
	if err == nil {
		t.Fatalf("expected error removing builtin marketplace")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("expected 'built-in' error, got: %v", err)
	}

	// Also check the others builtin
	err = store.RemoveMarketplace(ctx, "others")
	if err == nil {
		t.Fatalf("expected error removing builtin marketplace 'others'")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("expected 'built-in' error, got: %v", err)
	}
}

func TestRemoveMarketplace_NotFound(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.RemoveMarketplace(ctx, "nonexistent")
	if err == nil {
		t.Fatalf("expected error for nonexistent namespace")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestRemoveMarketplace_BlockedByInstalledPlugins(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "has.plugins", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	// Install a plugin into this marketplace
	if err := store.InsertInstalledPlugin(ctx, m.ID, "my-plugin", "https://example.com/plugin.tar.gz"); err != nil {
		t.Fatalf("install plugin: %v", err)
	}

	err = store.RemoveMarketplace(ctx, "has.plugins")
	if err == nil {
		t.Fatalf("expected error removing marketplace with installed plugins")
	}
	if !strings.Contains(err.Error(), "plugins still installed") {
		t.Fatalf("expected 'plugins still installed' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "has.plugins/my-plugin") {
		t.Fatalf("expected plugin slug in error message, got: %v", err)
	}
}

func TestRemoveMarketplace_BlockedByMultiplePlugins(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "multi.plugins", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	// Install two plugins
	if err := store.InsertInstalledPlugin(ctx, m.ID, "plugin-a", ""); err != nil {
		t.Fatalf("install plugin-a: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, m.ID, "plugin-b", ""); err != nil {
		t.Fatalf("install plugin-b: %v", err)
	}

	err = store.RemoveMarketplace(ctx, "multi.plugins")
	if err == nil {
		t.Fatalf("expected error removing marketplace with installed plugins")
	}
	if !strings.Contains(err.Error(), "2 plugins still installed") {
		t.Fatalf("expected '2 plugins still installed' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "multi.plugins/plugin-a") {
		t.Fatalf("expected plugin-a in error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "multi.plugins/plugin-b") {
		t.Fatalf("expected plugin-b in error message, got: %v", err)
	}
}

func TestRemoveMarketplace_SucceedsAfterPluginUninstall(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "eventually.removable", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "blocker", ""); err != nil {
		t.Fatalf("install plugin: %v", err)
	}

	// Should fail first
	if err := store.RemoveMarketplace(ctx, "eventually.removable"); err == nil {
		t.Fatalf("expected error with plugin installed")
	}

	// Remove the plugin
	if err := store.DeleteInstalledPlugin(ctx, "eventually.removable", "blocker"); err != nil {
		t.Fatalf("delete plugin: %v", err)
	}

	// Now removal should succeed
	if err := store.RemoveMarketplace(ctx, "eventually.removable"); err != nil {
		t.Fatalf("remove marketplace after uninstall: %v", err)
	}

	_, err = store.GetMarketplaceByNamespace(ctx, "eventually.removable")
	if !IsNotFound(err) {
		t.Fatalf("expected marketplace to be gone, err: %v", err)
	}
}

func TestRemoveMarketplace_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	// read-only check happens first in RemoveMarketplace, before the builtin check
	err := store.RemoveMarketplace(ctx, "ai.nupi")
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// UpdateMarketplaceCache
// ---------------------------------------------------------------------------

func TestUpdateMarketplaceCache_Success(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	cachedYAML := "plugins:\n  - name: test-plugin\n    version: 1.0.0"
	timestamp := "2025-01-15T10:30:00Z"

	if err := store.UpdateMarketplaceCache(ctx, "ai.nupi", cachedYAML, timestamp); err != nil {
		t.Fatalf("update cache: %v", err)
	}

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}
	if m.CachedIndex != cachedYAML {
		t.Fatalf("expected cached_index %q, got %q", cachedYAML, m.CachedIndex)
	}
	if m.LastRefreshed != timestamp {
		t.Fatalf("expected last_refreshed %q, got %q", timestamp, m.LastRefreshed)
	}
}

func TestUpdateMarketplaceCache_OverwriteExisting(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	// First update
	if err := store.UpdateMarketplaceCache(ctx, "ai.nupi", "old-data", "2025-01-01T00:00:00Z"); err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Overwrite
	if err := store.UpdateMarketplaceCache(ctx, "ai.nupi", "new-data", "2025-01-02T00:00:00Z"); err != nil {
		t.Fatalf("second update: %v", err)
	}

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}
	if m.CachedIndex != "new-data" {
		t.Fatalf("expected cached_index 'new-data', got %q", m.CachedIndex)
	}
	if m.LastRefreshed != "2025-01-02T00:00:00Z" {
		t.Fatalf("expected last_refreshed '2025-01-02T00:00:00Z', got %q", m.LastRefreshed)
	}
}

func TestUpdateMarketplaceCache_NotFound(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.UpdateMarketplaceCache(ctx, "nonexistent", "data", "now")
	if err == nil {
		t.Fatalf("expected error for nonexistent marketplace")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestUpdateMarketplaceCache_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	err := store.UpdateMarketplaceCache(ctx, "ai.nupi", "data", "now")
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

func TestUpdateMarketplaceCache_EmptyValues(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	// Set some data first
	if err := store.UpdateMarketplaceCache(ctx, "ai.nupi", "some-data", "2025-01-01T00:00:00Z"); err != nil {
		t.Fatalf("initial update: %v", err)
	}

	// Overwrite with empty strings
	if err := store.UpdateMarketplaceCache(ctx, "ai.nupi", "", ""); err != nil {
		t.Fatalf("update with empty values: %v", err)
	}

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}
	if m.CachedIndex != "" {
		t.Fatalf("expected empty cached_index, got %q", m.CachedIndex)
	}
	if m.LastRefreshed != "" {
		t.Fatalf("expected empty last_refreshed, got %q", m.LastRefreshed)
	}
}

// ---------------------------------------------------------------------------
// InsertInstalledPlugin
// ---------------------------------------------------------------------------

func TestInsertInstalledPlugin_Success(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "test-plugin", "https://example.com/plugin.tar.gz"); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Verify via get
	p, err := store.GetInstalledPlugin(ctx, "ai.nupi", "test-plugin")
	if err != nil {
		t.Fatalf("get installed plugin: %v", err)
	}
	if p.Slug != "test-plugin" {
		t.Fatalf("expected slug test-plugin, got %q", p.Slug)
	}
	if p.SourceURL != "https://example.com/plugin.tar.gz" {
		t.Fatalf("expected source URL, got %q", p.SourceURL)
	}
	if p.Namespace != "ai.nupi" {
		t.Fatalf("expected namespace ai.nupi, got %q", p.Namespace)
	}
	if p.MarketplaceID != m.ID {
		t.Fatalf("expected marketplace ID %d, got %d", m.ID, p.MarketplaceID)
	}
	if p.Enabled {
		t.Fatalf("expected plugin to be disabled by default")
	}
	if p.InstalledAt == "" {
		t.Fatalf("expected non-empty installed_at")
	}
}

func TestInsertInstalledPlugin_EmptySourceURL(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "local-plugin", ""); err != nil {
		t.Fatalf("insert plugin with empty URL: %v", err)
	}

	p, err := store.GetInstalledPlugin(ctx, "ai.nupi", "local-plugin")
	if err != nil {
		t.Fatalf("get installed plugin: %v", err)
	}
	if p.SourceURL != "" {
		t.Fatalf("expected empty source URL, got %q", p.SourceURL)
	}
}

func TestInsertInstalledPlugin_DuplicateSlug(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "dup-plugin", ""); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	err = store.InsertInstalledPlugin(ctx, m.ID, "dup-plugin", "")
	if err == nil {
		t.Fatalf("expected error for duplicate slug")
	}
	if !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("expected 'already installed' error, got: %v", err)
	}
}

func TestInsertInstalledPlugin_EmptySlug(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	err = store.InsertInstalledPlugin(ctx, m.ID, "", "")
	if err == nil {
		t.Fatalf("expected error for empty slug")
	}
	if !strings.Contains(err.Error(), "slug is required") {
		t.Fatalf("expected 'slug is required' error, got: %v", err)
	}
}

func TestInsertInstalledPlugin_WhitespaceSlug(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	err = store.InsertInstalledPlugin(ctx, m.ID, "   ", "")
	if err == nil {
		t.Fatalf("expected error for whitespace-only slug")
	}
	if !strings.Contains(err.Error(), "slug is required") {
		t.Fatalf("expected 'slug is required' error, got: %v", err)
	}
}

func TestInsertInstalledPlugin_InvalidMarketplaceID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.InsertInstalledPlugin(ctx, 0, "plugin", "")
	if err == nil {
		t.Fatalf("expected error for zero marketplace ID")
	}
	if !strings.Contains(err.Error(), "invalid marketplace ID") {
		t.Fatalf("expected 'invalid marketplace ID' error, got: %v", err)
	}

	err = store.InsertInstalledPlugin(ctx, -1, "plugin", "")
	if err == nil {
		t.Fatalf("expected error for negative marketplace ID")
	}
	if !strings.Contains(err.Error(), "invalid marketplace ID") {
		t.Fatalf("expected 'invalid marketplace ID' error, got: %v", err)
	}
}

func TestInsertInstalledPlugin_NonexistentMarketplaceID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.InsertInstalledPlugin(ctx, 99999, "plugin", "")
	if err == nil {
		t.Fatalf("expected error for nonexistent marketplace ID")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestInsertInstalledPlugin_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	err := store.InsertInstalledPlugin(ctx, 1, "plugin", "")
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListInstalledPlugins
// ---------------------------------------------------------------------------

func TestListInstalledPlugins_Empty(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	plugins, err := store.ListInstalledPlugins(ctx)
	if err != nil {
		t.Fatalf("list installed plugins: %v", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("expected 0 plugins, got %d", len(plugins))
	}
}

func TestListInstalledPlugins_MultipleMarketplaces(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	nupi, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get ai.nupi: %v", err)
	}
	others, err := store.GetMarketplaceByNamespace(ctx, "others")
	if err != nil {
		t.Fatalf("get others: %v", err)
	}

	// Install plugins across marketplaces
	if err := store.InsertInstalledPlugin(ctx, nupi.ID, "nupi-plugin", "https://example.com/nupi.tar.gz"); err != nil {
		t.Fatalf("insert nupi plugin: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, others.ID, "other-plugin", ""); err != nil {
		t.Fatalf("insert other plugin: %v", err)
	}

	plugins, err := store.ListInstalledPlugins(ctx)
	if err != nil {
		t.Fatalf("list installed plugins: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}

	// Ordered by namespace then slug: ai.nupi/nupi-plugin, others/other-plugin
	if plugins[0].Namespace != "ai.nupi" || plugins[0].Slug != "nupi-plugin" {
		t.Fatalf("expected first plugin ai.nupi/nupi-plugin, got %s/%s", plugins[0].Namespace, plugins[0].Slug)
	}
	if plugins[1].Namespace != "others" || plugins[1].Slug != "other-plugin" {
		t.Fatalf("expected second plugin others/other-plugin, got %s/%s", plugins[1].Namespace, plugins[1].Slug)
	}
}

func TestListInstalledPlugins_OrderedByNamespaceThenSlug(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	nupi, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get ai.nupi: %v", err)
	}

	// Insert in reverse alphabetical order
	if err := store.InsertInstalledPlugin(ctx, nupi.ID, "zebra", ""); err != nil {
		t.Fatalf("insert zebra: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, nupi.ID, "alpha", ""); err != nil {
		t.Fatalf("insert alpha: %v", err)
	}

	plugins, err := store.ListInstalledPlugins(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}
	if plugins[0].Slug != "alpha" {
		t.Fatalf("expected first slug alpha, got %q", plugins[0].Slug)
	}
	if plugins[1].Slug != "zebra" {
		t.Fatalf("expected second slug zebra, got %q", plugins[1].Slug)
	}
}

// ---------------------------------------------------------------------------
// GetInstalledPlugin
// ---------------------------------------------------------------------------

func TestGetInstalledPlugin_Success(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "fetch-plugin", "https://example.com/fetch.tar.gz"); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	p, err := store.GetInstalledPlugin(ctx, "ai.nupi", "fetch-plugin")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	if p.Slug != "fetch-plugin" {
		t.Fatalf("expected slug fetch-plugin, got %q", p.Slug)
	}
	if p.Namespace != "ai.nupi" {
		t.Fatalf("expected namespace ai.nupi, got %q", p.Namespace)
	}
	if p.SourceURL != "https://example.com/fetch.tar.gz" {
		t.Fatalf("expected source URL, got %q", p.SourceURL)
	}
}

func TestGetInstalledPlugin_NotFound(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetInstalledPlugin(ctx, "ai.nupi", "nonexistent")
	if err == nil {
		t.Fatalf("expected error for nonexistent plugin")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestGetInstalledPlugin_WrongNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "ns-plugin", ""); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Try to get it from the wrong namespace
	_, err = store.GetInstalledPlugin(ctx, "others", "ns-plugin")
	if err == nil {
		t.Fatalf("expected error for wrong namespace")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestGetInstalledPlugin_EmptyNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetInstalledPlugin(ctx, "", "some-slug")
	if err == nil {
		t.Fatalf("expected error for empty namespace")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestGetInstalledPlugin_EmptySlug(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	_, err := store.GetInstalledPlugin(ctx, "ai.nupi", "")
	if err == nil {
		t.Fatalf("expected error for empty slug")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// DeleteInstalledPlugin
// ---------------------------------------------------------------------------

func TestDeleteInstalledPlugin_Success(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "del-plugin", ""); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	if err := store.DeleteInstalledPlugin(ctx, "ai.nupi", "del-plugin"); err != nil {
		t.Fatalf("delete plugin: %v", err)
	}

	// Verify it's gone
	_, err = store.GetInstalledPlugin(ctx, "ai.nupi", "del-plugin")
	if !IsNotFound(err) {
		t.Fatalf("expected plugin to be gone after delete, err: %v", err)
	}
}

func TestDeleteInstalledPlugin_NotFound(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.DeleteInstalledPlugin(ctx, "ai.nupi", "nonexistent")
	if err == nil {
		t.Fatalf("expected error for nonexistent plugin")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestDeleteInstalledPlugin_EmptyNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.DeleteInstalledPlugin(ctx, "", "some-slug")
	if err == nil {
		t.Fatalf("expected error for empty namespace")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestDeleteInstalledPlugin_EmptySlug(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.DeleteInstalledPlugin(ctx, "ai.nupi", "")
	if err == nil {
		t.Fatalf("expected error for empty slug")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestDeleteInstalledPlugin_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	// Empty inputs are checked before read-only, so use valid-looking inputs
	err := store.DeleteInstalledPlugin(ctx, "ai.nupi", "some-plugin")
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SetPluginEnabled
// ---------------------------------------------------------------------------

func TestSetPluginEnabled_Toggle(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "toggle-plugin", ""); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Starts disabled
	p, err := store.GetInstalledPlugin(ctx, "ai.nupi", "toggle-plugin")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	if p.Enabled {
		t.Fatalf("expected plugin to start disabled")
	}

	// Enable
	if err := store.SetPluginEnabled(ctx, "ai.nupi", "toggle-plugin", true); err != nil {
		t.Fatalf("enable plugin: %v", err)
	}
	p, err = store.GetInstalledPlugin(ctx, "ai.nupi", "toggle-plugin")
	if err != nil {
		t.Fatalf("get plugin after enable: %v", err)
	}
	if !p.Enabled {
		t.Fatalf("expected plugin to be enabled")
	}

	// Disable again
	if err := store.SetPluginEnabled(ctx, "ai.nupi", "toggle-plugin", false); err != nil {
		t.Fatalf("disable plugin: %v", err)
	}
	p, err = store.GetInstalledPlugin(ctx, "ai.nupi", "toggle-plugin")
	if err != nil {
		t.Fatalf("get plugin after disable: %v", err)
	}
	if p.Enabled {
		t.Fatalf("expected plugin to be disabled")
	}
}

func TestSetPluginEnabled_NotFound(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.SetPluginEnabled(ctx, "ai.nupi", "nonexistent", true)
	if err == nil {
		t.Fatalf("expected error for nonexistent plugin")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestSetPluginEnabled_EmptyNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.SetPluginEnabled(ctx, "", "slug", true)
	if err == nil {
		t.Fatalf("expected error for empty namespace")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestSetPluginEnabled_EmptySlug(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.SetPluginEnabled(ctx, "ai.nupi", "", true)
	if err == nil {
		t.Fatalf("expected error for empty slug")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestSetPluginEnabled_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	err := store.SetPluginEnabled(ctx, "ai.nupi", "some-plugin", true)
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

func TestSetPluginEnabled_IdempotentEnable(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "idem-plugin", ""); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Enable twice should not error
	if err := store.SetPluginEnabled(ctx, "ai.nupi", "idem-plugin", true); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	if err := store.SetPluginEnabled(ctx, "ai.nupi", "idem-plugin", true); err != nil {
		t.Fatalf("second enable: %v", err)
	}

	p, err := store.GetInstalledPlugin(ctx, "ai.nupi", "idem-plugin")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	if !p.Enabled {
		t.Fatalf("expected plugin to remain enabled")
	}
}

// ---------------------------------------------------------------------------
// CountInstalledPluginsByMarketplace
// ---------------------------------------------------------------------------

func TestCountInstalledPluginsByMarketplace_Zero(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	count, err := store.CountInstalledPluginsByMarketplace(ctx, m.ID)
	if err != nil {
		t.Fatalf("count plugins: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 plugins, got %d", count)
	}
}

func TestCountInstalledPluginsByMarketplace_WithPlugins(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "count-a", ""); err != nil {
		t.Fatalf("insert plugin-a: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, m.ID, "count-b", ""); err != nil {
		t.Fatalf("insert plugin-b: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, m.ID, "count-c", ""); err != nil {
		t.Fatalf("insert plugin-c: %v", err)
	}

	count, err := store.CountInstalledPluginsByMarketplace(ctx, m.ID)
	if err != nil {
		t.Fatalf("count plugins: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 plugins, got %d", count)
	}
}

func TestCountInstalledPluginsByMarketplace_IsolatedPerMarketplace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	nupi, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get ai.nupi: %v", err)
	}
	others, err := store.GetMarketplaceByNamespace(ctx, "others")
	if err != nil {
		t.Fatalf("get others: %v", err)
	}

	// Install 2 in ai.nupi, 1 in others
	if err := store.InsertInstalledPlugin(ctx, nupi.ID, "p1", ""); err != nil {
		t.Fatalf("insert p1: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, nupi.ID, "p2", ""); err != nil {
		t.Fatalf("insert p2: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, others.ID, "p3", ""); err != nil {
		t.Fatalf("insert p3: %v", err)
	}

	nupiCount, err := store.CountInstalledPluginsByMarketplace(ctx, nupi.ID)
	if err != nil {
		t.Fatalf("count nupi: %v", err)
	}
	if nupiCount != 2 {
		t.Fatalf("expected 2 for ai.nupi, got %d", nupiCount)
	}

	othersCount, err := store.CountInstalledPluginsByMarketplace(ctx, others.ID)
	if err != nil {
		t.Fatalf("count others: %v", err)
	}
	if othersCount != 1 {
		t.Fatalf("expected 1 for others, got %d", othersCount)
	}
}

func TestCountInstalledPluginsByMarketplace_NonexistentID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	count, err := store.CountInstalledPluginsByMarketplace(ctx, 99999)
	if err != nil {
		t.Fatalf("count plugins: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 for nonexistent marketplace, got %d", count)
	}
}

func TestCountInstalledPluginsByMarketplace_DecreasesAfterDelete(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	if err := store.InsertInstalledPlugin(ctx, m.ID, "will-delete", ""); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, m.ID, "will-keep", ""); err != nil {
		t.Fatalf("insert: %v", err)
	}

	count, _ := store.CountInstalledPluginsByMarketplace(ctx, m.ID)
	if count != 2 {
		t.Fatalf("expected 2 before delete, got %d", count)
	}

	if err := store.DeleteInstalledPlugin(ctx, "ai.nupi", "will-delete"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	count, err = store.CountInstalledPluginsByMarketplace(ctx, m.ID)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 after delete, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// Cross-cutting: same slug different marketplaces
// ---------------------------------------------------------------------------

func TestSameSlugDifferentMarketplaces(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	nupi, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get ai.nupi: %v", err)
	}
	others, err := store.GetMarketplaceByNamespace(ctx, "others")
	if err != nil {
		t.Fatalf("get others: %v", err)
	}

	// Same slug in different marketplaces should be allowed
	if err := store.InsertInstalledPlugin(ctx, nupi.ID, "shared-slug", "https://nupi.example.com"); err != nil {
		t.Fatalf("insert into ai.nupi: %v", err)
	}
	if err := store.InsertInstalledPlugin(ctx, others.ID, "shared-slug", "https://other.example.com"); err != nil {
		t.Fatalf("insert into others: %v", err)
	}

	// Both should be retrievable independently
	p1, err := store.GetInstalledPlugin(ctx, "ai.nupi", "shared-slug")
	if err != nil {
		t.Fatalf("get from ai.nupi: %v", err)
	}
	if p1.SourceURL != "https://nupi.example.com" {
		t.Fatalf("expected nupi source URL, got %q", p1.SourceURL)
	}

	p2, err := store.GetInstalledPlugin(ctx, "others", "shared-slug")
	if err != nil {
		t.Fatalf("get from others: %v", err)
	}
	if p2.SourceURL != "https://other.example.com" {
		t.Fatalf("expected other source URL, got %q", p2.SourceURL)
	}

	// Deleting from one should not affect the other
	if err := store.DeleteInstalledPlugin(ctx, "ai.nupi", "shared-slug"); err != nil {
		t.Fatalf("delete from ai.nupi: %v", err)
	}

	_, err = store.GetInstalledPlugin(ctx, "ai.nupi", "shared-slug")
	if !IsNotFound(err) {
		t.Fatalf("expected ai.nupi plugin to be gone, err: %v", err)
	}

	p2Again, err := store.GetInstalledPlugin(ctx, "others", "shared-slug")
	if err != nil {
		t.Fatalf("others plugin should still exist: %v", err)
	}
	if p2Again.Slug != "shared-slug" {
		t.Fatalf("expected slug shared-slug, got %q", p2Again.Slug)
	}
}

// ---------------------------------------------------------------------------
// Persistence across store reopen
// ---------------------------------------------------------------------------

func TestMarketplaceAndPluginPersistAcrossReopen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	// Open, add marketplace and plugin, close
	store1, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	added, err := store1.AddMarketplace(ctx, "persist.test", "https://persist.example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}
	if err := store1.InsertInstalledPlugin(ctx, added.ID, "persist-plugin", "https://persist.example.com/p.tar.gz"); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}
	if err := store1.SetPluginEnabled(ctx, "persist.test", "persist-plugin", true); err != nil {
		t.Fatalf("enable plugin: %v", err)
	}
	if err := store1.UpdateMarketplaceCache(ctx, "persist.test", "cached-yaml", "2025-06-01T00:00:00Z"); err != nil {
		t.Fatalf("update cache: %v", err)
	}
	store1.Close()

	// Reopen and verify
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { store2.Close() })

	m, err := store2.GetMarketplaceByNamespace(ctx, "persist.test")
	if err != nil {
		t.Fatalf("get marketplace after reopen: %v", err)
	}
	if m.URL != "https://persist.example.com" {
		t.Fatalf("expected URL to persist, got %q", m.URL)
	}
	if m.CachedIndex != "cached-yaml" {
		t.Fatalf("expected cached_index to persist, got %q", m.CachedIndex)
	}
	if m.LastRefreshed != "2025-06-01T00:00:00Z" {
		t.Fatalf("expected last_refreshed to persist, got %q", m.LastRefreshed)
	}

	p, err := store2.GetInstalledPlugin(ctx, "persist.test", "persist-plugin")
	if err != nil {
		t.Fatalf("get plugin after reopen: %v", err)
	}
	if !p.Enabled {
		t.Fatalf("expected plugin to remain enabled after reopen")
	}
	if p.SourceURL != "https://persist.example.com/p.tar.gz" {
		t.Fatalf("expected source URL to persist, got %q", p.SourceURL)
	}
}

// ---------------------------------------------------------------------------
// AddMarketplace with trimmed namespace
// ---------------------------------------------------------------------------

func TestAddMarketplace_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "  trimmed.ns  ", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}
	if m.Namespace != "trimmed.ns" {
		t.Fatalf("expected trimmed namespace, got %q", m.Namespace)
	}

	// Retrievable by trimmed name
	got, err := store.GetMarketplaceByNamespace(ctx, "trimmed.ns")
	if err != nil {
		t.Fatalf("get by trimmed namespace: %v", err)
	}
	if got.ID != m.ID {
		t.Fatalf("expected ID %d, got %d", m.ID, got.ID)
	}
}
