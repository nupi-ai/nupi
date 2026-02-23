package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// isSQLiteUniqueViolation checks whether a database error is a UNIQUE constraint failure.
// Both major Go SQLite drivers (mattn/go-sqlite3 and modernc.org/sqlite) produce this
// substring. Neither exposes a typed error for constraint violations.
func isSQLiteUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Marketplace represents a registered marketplace source.
type Marketplace struct {
	ID            int64
	InstanceName  string
	Namespace     string
	URL           string
	IsBuiltin     bool
	CachedIndex   string
	LastRefreshed string
	CreatedAt     string
}

// InstalledPlugin tracks a plugin installed from a marketplace or other source.
// Metadata (name, version, category, etc.) lives in plugin.yaml on disk.
type InstalledPlugin struct {
	ID            int64
	MarketplaceID int64
	Slug          string
	SourceURL     string
	InstalledAt   string
	Enabled       bool
}

// InstalledPluginWithNamespace extends InstalledPlugin with the marketplace namespace.
type InstalledPluginWithNamespace struct {
	InstalledPlugin
	Namespace string
}

// ListMarketplaces returns all registered marketplaces for the current instance.
func (s *Store) ListMarketplaces(ctx context.Context) ([]Marketplace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, instance_name, namespace, url, is_builtin, cached_index, last_refreshed, created_at
		FROM marketplaces
		WHERE instance_name = ?
		ORDER BY namespace
	`, s.instanceName)
	if err != nil {
		return nil, fmt.Errorf("config: list marketplaces: %w", err)
	}
	return scanList(rows, scanMarketplace, "config: scan marketplace", "config: iterate marketplaces")
}

// GetMarketplaceByNamespace retrieves a marketplace by its namespace.
func (s *Store) GetMarketplaceByNamespace(ctx context.Context, namespace string) (Marketplace, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return Marketplace{}, fmt.Errorf("config: get marketplace: namespace required")
	}

	m, err := scanMarketplace(s.db.QueryRowContext(ctx, `
		SELECT id, instance_name, namespace, url, is_builtin, cached_index, last_refreshed, created_at
		FROM marketplaces
		WHERE instance_name = ? AND namespace = ?
	`, s.instanceName, namespace))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Marketplace{}, NotFoundError{Entity: "marketplace", Key: namespace}
		}
		return Marketplace{}, fmt.Errorf("config: get marketplace %q: %w", namespace, err)
	}
	return m, nil
}

// GetMarketplaceByID retrieves a marketplace by its database ID.
func (s *Store) GetMarketplaceByID(ctx context.Context, id int64) (Marketplace, error) {
	m, err := scanMarketplace(s.db.QueryRowContext(ctx, `
		SELECT id, instance_name, namespace, url, is_builtin, cached_index, last_refreshed, created_at
		FROM marketplaces
		WHERE id = ? AND instance_name = ?
	`, id, s.instanceName))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Marketplace{}, NotFoundError{Entity: "marketplace", Key: fmt.Sprintf("id=%d", id)}
		}
		return Marketplace{}, fmt.Errorf("config: get marketplace id=%d: %w", id, err)
	}
	return m, nil
}

// AddMarketplace registers a new marketplace source. The namespace must be unique.
func (s *Store) AddMarketplace(ctx context.Context, namespace, url string) (Marketplace, error) {
	if err := s.ensureWritable("add marketplace"); err != nil {
		return Marketplace{}, err
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return Marketplace{}, fmt.Errorf("config: add marketplace: namespace required")
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO marketplaces (instance_name, namespace, url, is_builtin, created_at)
		VALUES (?, ?, ?, 0, CURRENT_TIMESTAMP)
	`, s.instanceName, namespace, url)
	if err != nil {
		if isSQLiteUniqueViolation(err) {
			return Marketplace{}, fmt.Errorf("marketplace with namespace %q already exists", namespace)
		}
		return Marketplace{}, fmt.Errorf("config: add marketplace: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Marketplace{}, fmt.Errorf("config: get last insert id: %w", err)
	}
	return Marketplace{
		ID:           id,
		InstanceName: s.instanceName,
		Namespace:    namespace,
		URL:          url,
	}, nil
}

// RemoveMarketplace removes a non-builtin marketplace. Returns error if plugins are still installed.
func (s *Store) RemoveMarketplace(ctx context.Context, namespace string) error {
	if err := s.ensureWritable("remove marketplace"); err != nil {
		return err
	}

	m, err := s.GetMarketplaceByNamespace(ctx, namespace)
	if err != nil {
		return err
	}
	if m.IsBuiltin {
		return fmt.Errorf("cannot remove built-in marketplace %q", namespace)
	}

	// Check for installed plugins (single query instead of COUNT + SELECT)
	rows, err := s.db.QueryContext(ctx, `
		SELECT slug FROM installed_plugins WHERE marketplace_id = ?
	`, m.ID)
	if err != nil {
		return fmt.Errorf("config: check installed plugins: %w", err)
	}
	slugsRaw, err := scanList(rows, scanString, "config: scan plugin slug", "config: iterate plugin slugs")
	if err != nil {
		return err
	}

	slugs := make([]string, 0, len(slugsRaw))
	for _, slug := range slugsRaw {
		slugs = append(slugs, namespace+"/"+slug)
	}
	if len(slugs) > 0 {
		return fmt.Errorf("cannot remove marketplace %q: %d plugins still installed. Uninstall them first: %s",
			namespace, len(slugs), strings.Join(slugs, ", "))
	}

	_, err = s.db.ExecContext(ctx, `DELETE FROM marketplaces WHERE id = ?`, m.ID)
	if err != nil {
		return fmt.Errorf("config: remove marketplace: %w", err)
	}
	return nil
}

// UpdateMarketplaceCache stores the fetched index YAML and refresh timestamp.
func (s *Store) UpdateMarketplaceCache(ctx context.Context, namespace, cachedIndex, lastRefreshed string) error {
	if err := s.ensureWritable("update marketplace cache"); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE marketplaces SET cached_index = ?, last_refreshed = ?
		WHERE instance_name = ? AND namespace = ?
	`, cachedIndex, lastRefreshed, s.instanceName, namespace)
	if err != nil {
		return fmt.Errorf("config: update marketplace cache: %w", err)
	}
	n, raErr := result.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("config: update marketplace cache: rows affected: %w", raErr)
	}
	if n == 0 {
		return NotFoundError{Entity: "marketplace", Key: namespace}
	}
	return nil
}

// --- Installed Plugins ---

// ListInstalledPlugins returns all installed plugins for the current instance.
func (s *Store) ListInstalledPlugins(ctx context.Context) ([]InstalledPluginWithNamespace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ip.id, ip.marketplace_id, ip.slug, ip.source_url, ip.installed_at, ip.enabled, m.namespace
		FROM installed_plugins ip
		JOIN marketplaces m ON ip.marketplace_id = m.id
		WHERE m.instance_name = ?
		ORDER BY m.namespace, ip.slug
	`, s.instanceName)
	if err != nil {
		return nil, fmt.Errorf("config: list installed plugins: %w", err)
	}
	return scanList(rows, scanInstalledPluginWithNamespace, "config: scan installed plugin", "config: iterate installed plugins")
}

// GetInstalledPlugin retrieves an installed plugin by namespace and slug.
func (s *Store) GetInstalledPlugin(ctx context.Context, namespace, slug string) (InstalledPluginWithNamespace, error) {
	if namespace == "" || slug == "" {
		return InstalledPluginWithNamespace{}, NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
	}
	p, err := scanInstalledPluginWithNamespace(s.db.QueryRowContext(ctx, `
		SELECT ip.id, ip.marketplace_id, ip.slug, ip.source_url, ip.installed_at, ip.enabled, m.namespace
		FROM installed_plugins ip
		JOIN marketplaces m ON ip.marketplace_id = m.id
		WHERE m.instance_name = ? AND m.namespace = ? AND ip.slug = ?
	`, s.instanceName, namespace, slug))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InstalledPluginWithNamespace{}, NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
		}
		return InstalledPluginWithNamespace{}, fmt.Errorf("config: get installed plugin %s/%s: %w", namespace, slug, err)
	}
	return p, nil
}

// InsertInstalledPlugin records a newly installed plugin and returns its ID.
func (s *Store) InsertInstalledPlugin(ctx context.Context, marketplaceID int64, slug, sourceURL string) (int64, error) {
	if err := s.ensureWritable("insert installed plugin"); err != nil {
		return 0, err
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return 0, fmt.Errorf("config: slug is required")
	}
	if marketplaceID <= 0 {
		return 0, fmt.Errorf("config: invalid marketplace ID")
	}

	// Verify the marketplace belongs to this instance
	var instanceOwner string
	err := s.db.QueryRowContext(ctx, `SELECT instance_name FROM marketplaces WHERE id = ?`, marketplaceID).Scan(&instanceOwner)
	if err != nil {
		return 0, fmt.Errorf("config: marketplace ID %d not found", marketplaceID)
	}
	if instanceOwner != s.instanceName {
		return 0, fmt.Errorf("config: marketplace ID %d does not belong to instance %q", marketplaceID, s.instanceName)
	}

	var src *string
	if sourceURL != "" {
		src = &sourceURL
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO installed_plugins (marketplace_id, slug, source_url, installed_at, enabled)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP, 0)
	`, marketplaceID, slug, src)
	if err != nil {
		if isSQLiteUniqueViolation(err) {
			return 0, fmt.Errorf("plugin %q already installed in this marketplace", slug)
		}
		return 0, fmt.Errorf("config: insert installed plugin: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("config: insert installed plugin: last insert id: %w", err)
	}
	return id, nil
}

// DeleteInstalledPlugin removes the tracking record for an installed plugin.
func (s *Store) DeleteInstalledPlugin(ctx context.Context, namespace, slug string) error {
	if namespace == "" || slug == "" {
		return NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
	}
	if err := s.ensureWritable("delete installed plugin"); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM installed_plugins
		WHERE marketplace_id IN (
			SELECT id FROM marketplaces WHERE instance_name = ? AND namespace = ?
		) AND slug = ?
	`, s.instanceName, namespace, slug)
	if err != nil {
		return fmt.Errorf("config: delete installed plugin: %w", err)
	}
	n, raErr := result.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("config: delete installed plugin: rows affected: %w", raErr)
	}
	if n == 0 {
		return NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
	}
	return nil
}

// SetPluginEnabled updates the enabled flag for an installed plugin.
func (s *Store) SetPluginEnabled(ctx context.Context, namespace, slug string, enabled bool) error {
	if namespace == "" || slug == "" {
		return NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
	}
	if err := s.ensureWritable("set plugin enabled"); err != nil {
		return err
	}
	val := 0
	if enabled {
		val = 1
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE installed_plugins SET enabled = ?
		WHERE marketplace_id IN (
			SELECT id FROM marketplaces WHERE instance_name = ? AND namespace = ?
		) AND slug = ?
	`, val, s.instanceName, namespace, slug)
	if err != nil {
		return fmt.Errorf("config: set plugin enabled: %w", err)
	}
	n, raErr := result.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("config: set plugin enabled: rows affected: %w", raErr)
	}
	if n == 0 {
		return NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
	}
	return nil
}

// CountInstalledPluginsByMarketplace returns the number of installed plugins for a marketplace.
func (s *Store) CountInstalledPluginsByMarketplace(ctx context.Context, marketplaceID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM installed_plugins ip
		JOIN marketplaces m ON ip.marketplace_id = m.id
		WHERE ip.marketplace_id = ? AND m.instance_name = ?
	`, marketplaceID, s.instanceName).Scan(&count)
	return count, err
}
