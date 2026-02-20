package store

import (
	"strings"
	"testing"
)

// Valid 64-character hexadecimal strings for SHA-256 hash test values.
var (
	testHash1 = strings.Repeat("a1", 32)
	testHash2 = strings.Repeat("b2", 32)
	testHash3 = strings.Repeat("c3", 32)
	testHash4 = strings.Repeat("d4", 32)
)

// testStoreWithInstanceName returns a shallow copy of s with a different
// instanceName. Copies all fields so the clone stays complete if new fields
// are added to Store.
func testStoreWithInstanceName(s *Store, name string) *Store {
	clone := *s
	clone.instanceName = name
	return &clone
}

func TestSetPluginChecksums_RoundTrip(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "checksum-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	checksums := map[string]string{
		"main.js":     testHash1,
		"plugin.yaml": testHash2,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, checksums); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get checksums by ID: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 checksums, got %d", len(got))
	}

	byPath := make(map[string]string, len(got))
	for _, pc := range got {
		byPath[pc.FilePath] = pc.SHA256
		if pc.PluginID != pluginID {
			t.Fatalf("expected plugin ID %d, got %d for %s", pluginID, pc.PluginID, pc.FilePath)
		}
		if pc.CreatedAt == "" {
			t.Fatalf("expected non-empty created_at for %s", pc.FilePath)
		}
	}

	if byPath["main.js"] != testHash1 {
		t.Fatalf("expected main.js hash %s, got %q", testHash1, byPath["main.js"])
	}
	if byPath["plugin.yaml"] != testHash2 {
		t.Fatalf("expected plugin.yaml hash %s, got %q", testHash2, byPath["plugin.yaml"])
	}
}

func TestGetPluginChecksumsByPlugin_JoinPath(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "test.checksums", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "join-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	checksums := map[string]string{
		"lib/index.js": testHash1,
		"README.md":    testHash2,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, checksums); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	got, err := store.GetPluginChecksumsByPlugin(ctx, "test.checksums", "join-plugin")
	if err != nil {
		t.Fatalf("get checksums by plugin: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 checksums, got %d", len(got))
	}

	byPath := make(map[string]string, len(got))
	for _, pc := range got {
		byPath[pc.FilePath] = pc.SHA256
		if pc.PluginID != pluginID {
			t.Fatalf("expected plugin ID %d, got %d for %s", pluginID, pc.PluginID, pc.FilePath)
		}
	}

	if byPath["lib/index.js"] != testHash1 {
		t.Fatalf("expected lib/index.js hash %s, got %q", testHash1, byPath["lib/index.js"])
	}
	if byPath["README.md"] != testHash2 {
		t.Fatalf("expected README.md hash %s, got %q", testHash2, byPath["README.md"])
	}
}

func TestSetPluginChecksums_Overwrite(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "overwrite-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// First set
	first := map[string]string{
		"old-file.js": testHash1,
		"shared.js":   testHash2,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, first); err != nil {
		t.Fatalf("first set: %v", err)
	}

	// Second set replaces entirely
	second := map[string]string{
		"new-file.js": testHash3,
		"shared.js":   testHash4,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, second); err != nil {
		t.Fatalf("second set: %v", err)
	}

	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get checksums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 checksums after overwrite, got %d", len(got))
	}

	byPath := make(map[string]string, len(got))
	for _, pc := range got {
		byPath[pc.FilePath] = pc.SHA256
	}

	if _, ok := byPath["old-file.js"]; ok {
		t.Fatalf("old-file.js should have been removed by overwrite")
	}
	if byPath["new-file.js"] != testHash3 {
		t.Fatalf("expected new-file.js hash %s, got %q", testHash3, byPath["new-file.js"])
	}
	if byPath["shared.js"] != testHash4 {
		t.Fatalf("expected shared.js hash %s, got %q", testHash4, byPath["shared.js"])
	}
}

func TestGetPluginChecksumsByID_Empty(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	got, err := store.GetPluginChecksumsByID(ctx, 99999)
	if err != nil {
		t.Fatalf("get checksums for nonexistent plugin: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %d items", len(got))
	}
}

func TestDeleteInstalledPlugin_CascadesChecksums(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "cascade-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	checksums := map[string]string{
		"file1.js": testHash1,
		"file2.js": testHash2,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, checksums); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	// Verify checksums exist
	before, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get checksums before delete: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected 2 checksums before delete, got %d", len(before))
	}

	// Delete the plugin — cascade should remove checksums
	if err := store.DeleteInstalledPlugin(ctx, "ai.nupi", "cascade-plugin"); err != nil {
		t.Fatalf("delete plugin: %v", err)
	}

	after, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get checksums after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("expected 0 checksums after cascade delete, got %d", len(after))
	}
}

func TestSetPluginChecksums_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	err := store.SetPluginChecksums(ctx, 1, map[string]string{"f.js": testHash1})
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

func TestDeletePluginChecksumsByID_ReadOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openReadOnlyTestStore(t)

	err := store.DeletePluginChecksumsByID(ctx, 1)
	if err == nil {
		t.Fatalf("expected error in read-only mode")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only error, got: %v", err)
	}
}

func TestDeletePluginChecksumsByID_Success(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "del-checksums-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	checksums := map[string]string{
		"a.js": testHash1,
		"b.js": testHash2,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, checksums); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	if err := store.DeletePluginChecksumsByID(ctx, pluginID); err != nil {
		t.Fatalf("delete checksums: %v", err)
	}

	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 checksums after delete, got %d", len(got))
	}
}

func TestSetPluginChecksums_EmptyMap(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "empty-map-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Set some checksums first
	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"f.js": testHash1}); err != nil {
		t.Fatalf("initial set: %v", err)
	}

	// Now set with empty map — should clear all checksums
	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{}); err != nil {
		t.Fatalf("set empty: %v", err)
	}

	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get after empty set: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 checksums after empty set, got %d", len(got))
	}
}

func TestGetPluginChecksumsByPlugin_WrongNamespace(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "ns-checksum-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"f.js": testHash1}); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	// Wrong namespace should return empty
	got, err := store.GetPluginChecksumsByPlugin(ctx, "others", "ns-checksum-plugin")
	if err != nil {
		t.Fatalf("get with wrong namespace: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty for wrong namespace, got %d", len(got))
	}
}

func TestGetPluginChecksumsByPlugin_EmptyInputs(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	t.Run("empty_namespace", func(t *testing.T) {
		t.Parallel()
		_, err := store.GetPluginChecksumsByPlugin(ctx, "", "some-slug")
		if err == nil {
			t.Fatalf("expected error for empty namespace")
		}
		if !strings.Contains(err.Error(), "namespace and slug required") {
			t.Fatalf("expected 'namespace and slug required' error, got: %v", err)
		}
	})

	t.Run("empty_slug", func(t *testing.T) {
		t.Parallel()
		_, err := store.GetPluginChecksumsByPlugin(ctx, "ai.nupi", "")
		if err == nil {
			t.Fatalf("expected error for empty slug")
		}
		if !strings.Contains(err.Error(), "namespace and slug required") {
			t.Fatalf("expected 'namespace and slug required' error, got: %v", err)
		}
	})
}

func TestSetPluginChecksums_InvalidPluginID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	t.Run("zero", func(t *testing.T) {
		t.Parallel()
		err := store.SetPluginChecksums(ctx, 0, map[string]string{"f.js": testHash1})
		if err == nil {
			t.Fatalf("expected error for zero plugin ID")
		}
		if !strings.Contains(err.Error(), "invalid plugin ID") {
			t.Fatalf("expected 'invalid plugin ID' error, got: %v", err)
		}
	})

	t.Run("negative", func(t *testing.T) {
		t.Parallel()
		err := store.SetPluginChecksums(ctx, -1, map[string]string{"f.js": testHash1})
		if err == nil {
			t.Fatalf("expected error for negative plugin ID")
		}
		if !strings.Contains(err.Error(), "invalid plugin ID") {
			t.Fatalf("expected 'invalid plugin ID' error, got: %v", err)
		}
	})
}

func TestSetPluginChecksums_NonexistentPlugin(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	err := store.SetPluginChecksums(ctx, 99999, map[string]string{"f.js": testHash1})
	if err == nil {
		t.Fatalf("expected error for nonexistent plugin ID")
	}
	if !strings.Contains(err.Error(), "plugin ID 99999 not found") {
		t.Fatalf("expected 'plugin ID 99999 not found' error, got: %v", err)
	}
}

func TestSetPluginChecksums_NonexistentPluginEmptyMap(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	// Empty map on nonexistent plugin should still fail (plugin doesn't exist)
	err := store.SetPluginChecksums(ctx, 99999, map[string]string{})
	if err == nil {
		t.Fatalf("expected error for nonexistent plugin ID with empty map")
	}
	if !strings.Contains(err.Error(), "plugin ID 99999 not found") {
		t.Fatalf("expected 'plugin ID 99999 not found' error, got: %v", err)
	}
}

func TestSetPluginChecksums_NilMap(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "nil-map-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Set some checksums first
	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"f.js": testHash1}); err != nil {
		t.Fatalf("initial set: %v", err)
	}

	// Nil map should clear all checksums (same behavior as empty map)
	if err := store.SetPluginChecksums(ctx, pluginID, nil); err != nil {
		t.Fatalf("set nil map: %v", err)
	}

	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get after nil set: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 checksums after nil map set, got %d", len(got))
	}
}

func TestGetPluginChecksumsByID_InvalidPluginID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	t.Run("zero", func(t *testing.T) {
		t.Parallel()
		_, err := store.GetPluginChecksumsByID(ctx, 0)
		if err == nil {
			t.Fatalf("expected error for zero plugin ID")
		}
		if !strings.Contains(err.Error(), "invalid plugin ID") {
			t.Fatalf("expected 'invalid plugin ID' error, got: %v", err)
		}
	})

	t.Run("negative", func(t *testing.T) {
		t.Parallel()
		_, err := store.GetPluginChecksumsByID(ctx, -1)
		if err == nil {
			t.Fatalf("expected error for negative plugin ID")
		}
		if !strings.Contains(err.Error(), "invalid plugin ID") {
			t.Fatalf("expected 'invalid plugin ID' error, got: %v", err)
		}
	})
}

func TestSetPluginChecksums_EmptyPathOrHash(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "empty-val-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	t.Run("empty_path", func(t *testing.T) {
		t.Parallel()
		err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"": testHash1})
		if err == nil {
			t.Fatalf("expected error for empty file path")
		}
		if !strings.Contains(err.Error(), "empty file path or hash") {
			t.Fatalf("expected 'empty file path or hash' error, got: %v", err)
		}
		if !strings.Contains(err.Error(), `""->`) {
			t.Fatalf("expected error to identify the entry with empty path, got: %v", err)
		}
	})

	t.Run("empty_hash", func(t *testing.T) {
		t.Parallel()
		err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"file.js": ""})
		if err == nil {
			t.Fatalf("expected error for empty hash")
		}
		if !strings.Contains(err.Error(), "empty file path or hash") {
			t.Fatalf("expected 'empty file path or hash' error, got: %v", err)
		}
		if !strings.Contains(err.Error(), `"file.js"->""`) {
			t.Fatalf("expected error to identify the entry, got: %v", err)
		}
	})
}

func TestSetPluginChecksums_PrevalidatesBeforeDelete(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "preval-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Set valid checksums first
	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"keep.js": testHash1}); err != nil {
		t.Fatalf("initial set: %v", err)
	}

	// Attempt set with invalid entry — should fail WITHOUT deleting existing checksums
	err = store.SetPluginChecksums(ctx, pluginID, map[string]string{"valid.js": testHash2, "": testHash3})
	if err == nil {
		t.Fatalf("expected error for empty path")
	}

	// Verify original checksums are preserved (pre-validation prevented DELETE)
	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get after failed set: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 checksum preserved, got %d", len(got))
	}
	if got[0].FilePath != "keep.js" {
		t.Fatalf("expected keep.js preserved, got %q", got[0].FilePath)
	}
}

func TestGetPluginChecksumsByPlugin_WhitespaceInputs(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	t.Run("whitespace_namespace", func(t *testing.T) {
		t.Parallel()
		_, err := store.GetPluginChecksumsByPlugin(ctx, "  ", "some-slug")
		if err == nil {
			t.Fatalf("expected error for whitespace namespace")
		}
		if !strings.Contains(err.Error(), "namespace and slug required") {
			t.Fatalf("expected 'namespace and slug required' error, got: %v", err)
		}
	})

	t.Run("whitespace_slug", func(t *testing.T) {
		t.Parallel()
		_, err := store.GetPluginChecksumsByPlugin(ctx, "ai.nupi", "  ")
		if err == nil {
			t.Fatalf("expected error for whitespace slug")
		}
		if !strings.Contains(err.Error(), "namespace and slug required") {
			t.Fatalf("expected 'namespace and slug required' error, got: %v", err)
		}
	})
}

func TestGetPluginChecksumsByPlugin_TrimmedWhitespaceMatch(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "test.trim", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "trim-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"f.js": testHash1}); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	// Namespace and slug with leading/trailing whitespace should be trimmed and match
	got, err := store.GetPluginChecksumsByPlugin(ctx, "  test.trim  ", "  trim-plugin  ")
	if err != nil {
		t.Fatalf("get with padded inputs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 checksum, got %d", len(got))
	}
	if got[0].FilePath != "f.js" {
		t.Fatalf("expected f.js, got %q", got[0].FilePath)
	}
}

func TestGetPluginChecksumsByPlugin_InstanceScoping(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "test.scope", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "scope-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"f.js": testHash1}); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	// Create a second store view with a different instance name sharing the same DB.
	otherInstance := testStoreWithInstanceName(store, "other-instance")

	// Query from other instance — should return empty (instance scoping via JOIN)
	got, err := otherInstance.GetPluginChecksumsByPlugin(ctx, "test.scope", "scope-plugin")
	if err != nil {
		t.Fatalf("get from other instance: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 checksums from other instance, got %d", len(got))
	}

	// Verify original instance still returns the checksum
	got, err = store.GetPluginChecksumsByPlugin(ctx, "test.scope", "scope-plugin")
	if err != nil {
		t.Fatalf("get from original instance: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 checksum from original instance, got %d", len(got))
	}
}

func TestGetPluginChecksumsByID_OrderByFilePath(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "order-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	checksums := map[string]string{
		"z-last.js":  testHash1,
		"a-first.js": testHash2,
		"m-mid.js":   testHash3,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, checksums); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get checksums: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 checksums, got %d", len(got))
	}

	// Verify alphabetical order by file_path
	if got[0].FilePath != "a-first.js" {
		t.Fatalf("expected first file a-first.js, got %q", got[0].FilePath)
	}
	if got[1].FilePath != "m-mid.js" {
		t.Fatalf("expected second file m-mid.js, got %q", got[1].FilePath)
	}
	if got[2].FilePath != "z-last.js" {
		t.Fatalf("expected third file z-last.js, got %q", got[2].FilePath)
	}
}

func TestGetPluginChecksumsByPlugin_NoChecksums(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "test.nochk", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	if _, err := store.InsertInstalledPlugin(ctx, m.ID, "no-checksums-plugin", ""); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Plugin exists but has no checksums — should return empty slice, not error
	got, err := store.GetPluginChecksumsByPlugin(ctx, "test.nochk", "no-checksums-plugin")
	if err != nil {
		t.Fatalf("expected no error for plugin with no checksums, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %d items", len(got))
	}
}

func TestDeletePluginChecksumsByID_InvalidPluginID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	t.Run("zero", func(t *testing.T) {
		t.Parallel()
		err := store.DeletePluginChecksumsByID(ctx, 0)
		if err == nil {
			t.Fatalf("expected error for zero plugin ID")
		}
		if !strings.Contains(err.Error(), "invalid plugin ID") {
			t.Fatalf("expected 'invalid plugin ID' error, got: %v", err)
		}
	})

	t.Run("negative", func(t *testing.T) {
		t.Parallel()
		err := store.DeletePluginChecksumsByID(ctx, -1)
		if err == nil {
			t.Fatalf("expected error for negative plugin ID")
		}
		if !strings.Contains(err.Error(), "invalid plugin ID") {
			t.Fatalf("expected 'invalid plugin ID' error, got: %v", err)
		}
	})
}

func TestDeletePluginChecksumsByID_NonexistentPlugin(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	// Valid ID but no checksums exist — should return NotFoundError
	err := store.DeletePluginChecksumsByID(ctx, 99999)
	if err == nil {
		t.Fatalf("expected error for nonexistent plugin checksums")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got: %v", err)
	}
}

func TestGetPluginChecksumsByPlugin_OrderByFilePath(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.AddMarketplace(ctx, "test.order", "https://example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "order-join-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	checksums := map[string]string{
		"z-last.js":  testHash1,
		"a-first.js": testHash2,
		"m-mid.js":   testHash3,
	}
	if err := store.SetPluginChecksums(ctx, pluginID, checksums); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	got, err := store.GetPluginChecksumsByPlugin(ctx, "test.order", "order-join-plugin")
	if err != nil {
		t.Fatalf("get checksums: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 checksums, got %d", len(got))
	}

	// Verify alphabetical order by file_path (same as TestGetPluginChecksumsByID_OrderByFilePath)
	if got[0].FilePath != "a-first.js" {
		t.Fatalf("expected first file a-first.js, got %q", got[0].FilePath)
	}
	if got[1].FilePath != "m-mid.js" {
		t.Fatalf("expected second file m-mid.js, got %q", got[1].FilePath)
	}
	if got[2].FilePath != "z-last.js" {
		t.Fatalf("expected third file z-last.js, got %q", got[2].FilePath)
	}
}

func TestDeletePluginChecksumsByID_ExistingPluginNoChecksums(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "no-chk-del-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Plugin exists but has no checksums — delete returns NotFoundError
	// (consistent with DeleteInstalledPlugin pattern: 0 rows affected = not found)
	err = store.DeletePluginChecksumsByID(ctx, pluginID)
	if err == nil {
		t.Fatalf("expected error when deleting checksums for plugin with none set")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got: %v", err)
	}
}

func TestSetPluginChecksums_WhitespaceOnlyPath(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "ws-path-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Whitespace-only path should be rejected (same as empty path)
	err = store.SetPluginChecksums(ctx, pluginID, map[string]string{"  ": testHash1})
	if err == nil {
		t.Fatalf("expected error for whitespace-only file path")
	}
	if !strings.Contains(err.Error(), "empty file path or hash") {
		t.Fatalf("expected 'empty file path or hash' error, got: %v", err)
	}
}

func TestSetPluginChecksums_InvalidHashLength(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "bad-hash-len-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Hash that is too short (valid hex but not 64 chars)
	err = store.SetPluginChecksums(ctx, pluginID, map[string]string{"file.js": "abc123"})
	if err == nil {
		t.Fatalf("expected error for short hash")
	}
	if !strings.Contains(err.Error(), "invalid SHA-256 hash") {
		t.Fatalf("expected 'invalid SHA-256 hash' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "length 6, want 64") {
		t.Fatalf("expected length details in error, got: %v", err)
	}
}

func TestSetPluginChecksums_InvalidHashHex(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "bad-hash-hex-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// 64 chars but not valid hex (contains 'x', 'y', 'z')
	badHash := strings.Repeat("xyz1", 16) // 64 chars, invalid hex
	err = store.SetPluginChecksums(ctx, pluginID, map[string]string{"file.js": badHash})
	if err == nil {
		t.Fatalf("expected error for non-hex hash")
	}
	if !strings.Contains(err.Error(), "not valid hexadecimal") {
		t.Fatalf("expected 'not valid hexadecimal' error, got: %v", err)
	}
}

func TestSetPluginChecksums_NormalizesHashToLowercase(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	m, err := store.GetMarketplaceByNamespace(ctx, "ai.nupi")
	if err != nil {
		t.Fatalf("get marketplace: %v", err)
	}

	pluginID, err := store.InsertInstalledPlugin(ctx, m.ID, "case-norm-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}

	// Store hash with uppercase hex — should be normalized to lowercase
	uppercaseHash := strings.Repeat("A1", 32)
	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{"file.js": uppercaseHash}); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	got, err := store.GetPluginChecksumsByID(ctx, pluginID)
	if err != nil {
		t.Fatalf("get checksums: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 checksum, got %d", len(got))
	}
	if got[0].SHA256 != strings.ToLower(uppercaseHash) {
		t.Fatalf("expected lowercase hash %q, got %q", strings.ToLower(uppercaseHash), got[0].SHA256)
	}
}
