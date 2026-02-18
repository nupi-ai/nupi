package marketplace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/validate"
)

const (
	// DefaultCacheTTL is the default time-to-live for cached marketplace indices.
	DefaultCacheTTL = 1 * time.Hour

	maxIndexSize = 10 * 1024 * 1024 // 10 MB
)

// Client provides operations for interacting with marketplace sources.
type Client struct {
	store    *configstore.Store
	cacheTTL time.Duration
	http     *http.Client
}

// NewClient creates a new marketplace client.
func NewClient(store *configstore.Store) *Client {
	return &Client{
		store:    store,
		cacheTTL: DefaultCacheTTL,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("too many redirects")
				}
				// Block redirects to non-HTTP(S) schemes (SSRF prevention)
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return fmt.Errorf("redirect to disallowed scheme: %s", req.URL.Scheme)
				}
				return nil
			},
		},
	}
}

// WithCacheTTL sets a custom cache TTL.
func (c *Client) WithCacheTTL(ttl time.Duration) *Client {
	c.cacheTTL = ttl
	return c
}

// Add registers a new marketplace by fetching and validating its index.yaml.
func (c *Client) Add(ctx context.Context, rawURL string) (*configstore.Marketplace, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("marketplace URL is required")
	}

	if err := validate.HTTPURL(rawURL); err != nil {
		return nil, fmt.Errorf("invalid marketplace URL: %w", err)
	}

	url := rawURL

	// Fetch and parse the index to extract namespace
	data, err := c.doFetch(ctx, url, "")
	if err != nil {
		return nil, fmt.Errorf("fetch marketplace index: %w", err)
	}

	idx, err := ParseIndex(data)
	if err != nil {
		return nil, err
	}

	// Add to store
	m, err := c.store.AddMarketplace(ctx, idx.Namespace, url)
	if err != nil {
		return nil, err
	}

	// Cache the fetched index
	now := time.Now().UTC().Format(time.RFC3339)
	if err := c.store.UpdateMarketplaceCache(ctx, idx.Namespace, string(data), now); err != nil {
		log.Printf("[Marketplace] WARNING: failed to cache index for %s: %v", idx.Namespace, err)
	}

	return &m, nil
}

// Remove deletes a marketplace (delegates constraint checking to store).
func (c *Client) Remove(ctx context.Context, namespace string) error {
	return c.store.RemoveMarketplace(ctx, namespace)
}

// List returns all registered marketplaces.
func (c *Client) List(ctx context.Context) ([]configstore.Marketplace, error) {
	return c.store.ListMarketplaces(ctx)
}

// Refresh forces a re-fetch of all marketplace indices, ignoring cache TTL.
func (c *Client) Refresh(ctx context.Context) error {
	marketplaces, err := c.store.ListMarketplaces(ctx)
	if err != nil {
		return err
	}

	var errs []string
	for _, m := range marketplaces {
		if m.URL == "" {
			continue // "others" namespace has no remote URL
		}
		if err := c.refreshOne(ctx, m); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", m.Namespace, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("refresh errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// RefreshOne refreshes a single marketplace index.
func (c *Client) RefreshOne(ctx context.Context, namespace string) error {
	m, err := c.store.GetMarketplaceByNamespace(ctx, namespace)
	if err != nil {
		return err
	}
	if m.URL == "" {
		return nil
	}
	return c.refreshOne(ctx, m)
}

func (c *Client) refreshOne(ctx context.Context, m configstore.Marketplace) error {
	data, err := c.doFetch(ctx, m.URL, m.LastRefreshed)
	if errors.Is(err, errNotModified) {
		return nil
	}
	if err != nil {
		return err
	}

	// Validate the fetched index
	idx, err := ParseIndex(data)
	if err != nil {
		return fmt.Errorf("invalid index from %s: %w", m.URL, err)
	}
	if idx.Namespace != m.Namespace {
		return fmt.Errorf("namespace mismatch: marketplace registered as %q but index declares %q", m.Namespace, idx.Namespace)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	return c.store.UpdateMarketplaceCache(ctx, m.Namespace, string(data), now)
}

// GetIndex returns the parsed index for a marketplace, using cache if fresh.
func (c *Client) GetIndex(ctx context.Context, namespace string) (*Index, error) {
	m, err := c.store.GetMarketplaceByNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if m.URL == "" && m.CachedIndex == "" {
		return nil, fmt.Errorf("marketplace %q has no index", namespace)
	}

	// Check cache freshness
	if m.CachedIndex != "" {
		if c.isCacheFresh(m.LastRefreshed) {
			return ParseIndex([]byte(m.CachedIndex))
		}
		// Cache stale, try to refresh
		if m.URL != "" {
			if err := c.refreshOne(ctx, m); err != nil {
				log.Printf("[Marketplace] WARNING: refresh failed for %s, using stale cache: %v", namespace, err)
				return ParseIndex([]byte(m.CachedIndex))
			}
			// Re-read after refresh
			m, err = c.store.GetMarketplaceByNamespace(ctx, namespace)
			if err != nil {
				return nil, err
			}
			if m.CachedIndex != "" {
				return ParseIndex([]byte(m.CachedIndex))
			}
		}
		// No URL to refresh from, use stale cache
		return ParseIndex([]byte(m.CachedIndex))
	}

	// No cache, must fetch
	if m.URL == "" {
		return nil, fmt.Errorf("marketplace %q has no URL and no cached index", namespace)
	}
	if err := c.refreshOne(ctx, m); err != nil {
		return nil, err
	}
	m, err = c.store.GetMarketplaceByNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return ParseIndex([]byte(m.CachedIndex))
}

// Search searches across all marketplaces for plugins matching the query.
func (c *Client) Search(ctx context.Context, query string, category string, namespace string) ([]SearchResult, error) {
	marketplaces, err := c.store.ListMarketplaces(ctx)
	if err != nil {
		return nil, err
	}

	queryLower := strings.ToLower(strings.TrimSpace(query))
	var results []SearchResult
	var skipped []string

	for _, m := range marketplaces {
		if namespace != "" && m.Namespace != namespace {
			continue
		}
		if m.CachedIndex == "" {
			// Try refreshing if has URL, with a per-marketplace timeout to prevent
			// slow/unreachable sources from blocking the entire search.
			if m.URL != "" {
				refreshCtx, refreshCancel := context.WithTimeout(ctx, 10*time.Second)
				if err := c.refreshOne(refreshCtx, m); err != nil {
					log.Printf("[Marketplace] WARNING: refresh %s during search failed: %v", m.Namespace, err)
				}
				refreshCancel()
				updated, err := c.store.GetMarketplaceByNamespace(ctx, m.Namespace)
				if err != nil {
					log.Printf("[Marketplace] WARNING: re-read %s after refresh failed: %v", m.Namespace, err)
					skipped = append(skipped, m.Namespace)
					continue
				}
				m = updated
			}
			if m.CachedIndex == "" {
				skipped = append(skipped, m.Namespace)
				continue
			}
		}

		idx, err := ParseIndex([]byte(m.CachedIndex))
		if err != nil {
			log.Printf("[Marketplace] WARNING: skipping %s: invalid cached index: %v", m.Namespace, err)
			skipped = append(skipped, m.Namespace)
			continue
		}

		for _, p := range idx.Plugins {
			if category != "" && !strings.EqualFold(p.Category, category) {
				continue
			}
			if queryLower != "" {
				nameLower := strings.ToLower(p.DisplayName())
				descLower := strings.ToLower(p.Description)
				slugLower := strings.ToLower(p.Slug)
				if !strings.Contains(nameLower, queryLower) &&
					!strings.Contains(descLower, queryLower) &&
					!strings.Contains(slugLower, queryLower) {
					continue
				}
			}
			results = append(results, SearchResult{
				Namespace: m.Namespace,
				Plugin:    p,
			})
		}
	}

	if len(skipped) > 0 {
		log.Printf("[Marketplace] WARNING: search skipped %d marketplace(s): %s", len(skipped), strings.Join(skipped, ", "))
	}

	return results, nil
}

// FindPlugin looks up a specific plugin by namespace/slug in marketplace indices.
func (c *Client) FindPlugin(ctx context.Context, namespace, slug string) (*IndexPlugin, error) {
	idx, err := c.GetIndex(ctx, namespace)
	if err != nil {
		return nil, err
	}

	for i := range idx.Plugins {
		if idx.Plugins[i].Slug == slug {
			return &idx.Plugins[i], nil
		}
	}
	return nil, fmt.Errorf("plugin %q not found in marketplace %q", slug, namespace)
}

// SearchResult pairs a plugin with its marketplace namespace.
type SearchResult struct {
	Namespace string
	Plugin    IndexPlugin
}

// errNotModified is returned by doFetch when the server responds with 304 Not Modified.
var errNotModified = errors.New("server returned 304 Not Modified")

// doFetch performs an HTTP GET request, optionally setting If-Modified-Since.
// Returns errNotModified when the server responds with 304.
func (c *Client) doFetch(ctx context.Context, url string, ifModifiedSince string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "nupi-marketplace-client/1.0")

	if ifModifiedSince != "" {
		t, err := time.Parse(time.RFC3339, ifModifiedSince)
		if err == nil {
			req.Header.Set("If-Modified-Since", t.UTC().Format(http.TimeFormat))
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, errNotModified
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexSize+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(data)) > maxIndexSize {
		return nil, fmt.Errorf("marketplace index exceeds maximum size (%d bytes)", maxIndexSize)
	}
	return data, nil
}

func (c *Client) isCacheFresh(lastRefreshed string) bool {
	if lastRefreshed == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastRefreshed)
	if err != nil {
		return false
	}
	// Reject timestamps outside a reasonable range. 2020 is before the
	// marketplace feature existed; year+1 catches far-future clock skew.
	if t.Year() < 2020 || t.Year() > time.Now().Year()+1 {
		return false
	}
	return time.Since(t) < c.cacheTTL
}

