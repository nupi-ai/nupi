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
	defer rows.Close()

	var result []Marketplace
	for rows.Next() {
		var m Marketplace
		var cachedIndex, lastRefreshed sql.NullString
		if err := rows.Scan(&m.ID, &m.InstanceName, &m.Namespace, &m.URL, &m.IsBuiltin, &cachedIndex, &lastRefreshed, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("config: scan marketplace: %w", err)
		}
		m.CachedIndex = cachedIndex.String
		m.LastRefreshed = lastRefreshed.String
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetMarketplaceByNamespace retrieves a marketplace by its namespace.
func (s *Store) GetMarketplaceByNamespace(ctx context.Context, namespace string) (Marketplace, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return Marketplace{}, fmt.Errorf("config: get marketplace: namespace required")
	}

	var m Marketplace
	var cachedIndex, lastRefreshed sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, instance_name, namespace, url, is_builtin, cached_index, last_refreshed, created_at
		FROM marketplaces
		WHERE instance_name = ? AND namespace = ?
	`, s.instanceName, namespace).Scan(&m.ID, &m.InstanceName, &m.Namespace, &m.URL, &m.IsBuiltin, &cachedIndex, &lastRefreshed, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Marketplace{}, NotFoundError{Entity: "marketplace", Key: namespace}
		}
		return Marketplace{}, fmt.Errorf("config: get marketplace %q: %w", namespace, err)
	}
	m.CachedIndex = cachedIndex.String
	m.LastRefreshed = lastRefreshed.String
	return m, nil
}

// GetMarketplaceByID retrieves a marketplace by its database ID.
func (s *Store) GetMarketplaceByID(ctx context.Context, id int64) (Marketplace, error) {
	var m Marketplace
	var cachedIndex, lastRefreshed sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, instance_name, namespace, url, is_builtin, cached_index, last_refreshed, created_at
		FROM marketplaces
		WHERE id = ? AND instance_name = ?
	`, id, s.instanceName).Scan(&m.ID, &m.InstanceName, &m.Namespace, &m.URL, &m.IsBuiltin, &cachedIndex, &lastRefreshed, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Marketplace{}, NotFoundError{Entity: "marketplace", Key: fmt.Sprintf("id=%d", id)}
		}
		return Marketplace{}, fmt.Errorf("config: get marketplace id=%d: %w", id, err)
	}
	m.CachedIndex = cachedIndex.String
	m.LastRefreshed = lastRefreshed.String
	return m, nil
}

// AddMarketplace registers a new marketplace source. The namespace must be unique.
func (s *Store) AddMarketplace(ctx context.Context, namespace, url string) (Marketplace, error) {
	if s.readOnly {
		return Marketplace{}, fmt.Errorf("config: add marketplace: store opened read-only")
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
	if s.readOnly {
		return fmt.Errorf("config: remove marketplace: store opened read-only")
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
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			rows.Close()
			return fmt.Errorf("config: scan plugin slug: %w", err)
		}
		slugs = append(slugs, namespace+"/"+slug)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("config: iterate plugin slugs: %w", err)
	}
	rows.Close()
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
	if s.readOnly {
		return fmt.Errorf("config: update marketplace cache: store opened read-only")
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
	defer rows.Close()

	var result []InstalledPluginWithNamespace
	for rows.Next() {
		var p InstalledPluginWithNamespace
		var sourceURL sql.NullString
		if err := rows.Scan(&p.ID, &p.MarketplaceID, &p.Slug, &sourceURL, &p.InstalledAt, &p.Enabled, &p.Namespace); err != nil {
			return nil, fmt.Errorf("config: scan installed plugin: %w", err)
		}
		p.SourceURL = sourceURL.String
		result = append(result, p)
	}
	return result, rows.Err()
}

// GetInstalledPlugin retrieves an installed plugin by namespace and slug.
func (s *Store) GetInstalledPlugin(ctx context.Context, namespace, slug string) (InstalledPluginWithNamespace, error) {
	if namespace == "" || slug == "" {
		return InstalledPluginWithNamespace{}, NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
	}
	var p InstalledPluginWithNamespace
	var sourceURL sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT ip.id, ip.marketplace_id, ip.slug, ip.source_url, ip.installed_at, ip.enabled, m.namespace
		FROM installed_plugins ip
		JOIN marketplaces m ON ip.marketplace_id = m.id
		WHERE m.instance_name = ? AND m.namespace = ? AND ip.slug = ?
	`, s.instanceName, namespace, slug).Scan(&p.ID, &p.MarketplaceID, &p.Slug, &sourceURL, &p.InstalledAt, &p.Enabled, &p.Namespace)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InstalledPluginWithNamespace{}, NotFoundError{Entity: "installed plugin", Key: namespace + "/" + slug}
		}
		return InstalledPluginWithNamespace{}, fmt.Errorf("config: get installed plugin %s/%s: %w", namespace, slug, err)
	}
	p.SourceURL = sourceURL.String
	return p, nil
}

// InsertInstalledPlugin records a newly installed plugin and returns its ID.
func (s *Store) InsertInstalledPlugin(ctx context.Context, marketplaceID int64, slug, sourceURL string) (int64, error) {
	if s.readOnly {
		return 0, fmt.Errorf("config: insert installed plugin: store opened read-only")
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
	if s.readOnly {
		return fmt.Errorf("config: delete installed plugin: store opened read-only")
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
	if s.readOnly {
		return fmt.Errorf("config: set plugin enabled: store opened read-only")
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
