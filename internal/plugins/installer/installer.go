package installer

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/marketplace"
	manifestpkg "github.com/nupi-ai/nupi/internal/plugins/manifest"
	"github.com/nupi-ai/nupi/internal/validate"
)

const (
	maxArchiveSize      = 500 * 1024 * 1024 // 500 MB per file
	maxTotalExtractSize = 2 * 1024 * 1024 * 1024 // 2 GB cumulative extraction limit
	maxFileCount        = 10000
)

// Installer handles downloading, validating, and installing plugins.
type Installer struct {
	store     *configstore.Store
	pluginDir string
	http      *http.Client
}

// NewInstaller creates a new plugin installer.
func NewInstaller(store *configstore.Store, pluginDir string) *Installer {
	return &Installer{
		store:     store,
		pluginDir: pluginDir,
		http: &http.Client{
			Timeout: 5 * time.Minute,
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

// InstallResult holds the outcome of an installation.
type InstallResult struct {
	Namespace string
	Slug      string
	Version   string
	Category  string
	Dir       string
}

// InstallFromMarketplace installs a plugin from a marketplace by namespace/slug.
func (inst *Installer) InstallFromMarketplace(ctx context.Context, mClient *marketplace.Client, namespace, slug string) (*InstallResult, error) {
	if !isValidNamespace(namespace) {
		return nil, fmt.Errorf("invalid namespace: %q", namespace)
	}
	if !isValidSlug(slug) {
		return nil, fmt.Errorf("invalid slug: %q", slug)
	}

	// Look up the plugin in the marketplace index
	plugin, err := mClient.FindPlugin(ctx, namespace, slug)
	if err != nil {
		return nil, fmt.Errorf("find plugin %s/%s: %w", namespace, slug, err)
	}

	// Select the right archive for this platform
	archive, _, err := plugin.ArchiveForPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}

	// Check if already installed
	_, err = inst.store.GetInstalledPlugin(ctx, namespace, slug)
	if err == nil {
		return nil, fmt.Errorf("plugin %s/%s is already installed", namespace, slug)
	}

	// Download archive to temp
	tmpFile, err := inst.downloadToTemp(ctx, archive.URL)
	if err != nil {
		return nil, fmt.Errorf("download plugin archive: %w", err)
	}
	defer os.Remove(tmpFile)

	// Verify SHA-256
	if err := verifySHA256(tmpFile, archive.SHA256); err != nil {
		return nil, fmt.Errorf("integrity check failed: %w", err)
	}

	// Extract and validate
	return inst.installFromArchive(ctx, tmpFile, namespace, slug, archive.URL)
}

// InstallFromURL installs a plugin from a direct URL. Namespace = "others".
// Note: No SHA-256 verification is performed because direct URLs do not
// provide an expected hash. Use InstallFromMarketplace for verified installs.
func (inst *Installer) InstallFromURL(ctx context.Context, url string) (*InstallResult, error) {
	tmpFile, err := inst.downloadToTemp(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("download plugin archive: %w", err)
	}
	defer os.Remove(tmpFile)

	log.Printf("[Installer] WARNING: installing from URL without integrity verification (no expected SHA-256)")
	return inst.installFromArchive(ctx, tmpFile, "others", "", url)
}

// InstallFromPath installs a plugin from a local file or directory. Namespace = "others".
func (inst *Installer) InstallFromPath(ctx context.Context, path string) (*InstallResult, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat path: %w", err)
	}

	if info.IsDir() {
		return inst.installFromDir(ctx, absPath, "others", "", "")
	}
	return inst.installFromArchive(ctx, absPath, "others", "", absPath)
}

func (inst *Installer) installFromArchive(ctx context.Context, archivePath, namespace, expectedSlug, sourceURL string) (*InstallResult, error) {
	// Extract to temp directory
	tmpDir, err := os.MkdirTemp("", "nupi-plugin-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractArchive(ctx, archivePath, tmpDir); err != nil {
		return nil, fmt.Errorf("extract archive: %w", err)
	}

	return inst.installFromDir(ctx, tmpDir, namespace, expectedSlug, sourceURL)
}

func (inst *Installer) installFromDir(ctx context.Context, dir, namespace, expectedSlug, sourceURL string) (*InstallResult, error) {
	// Find plugin.yaml - may be at root or in a single subdirectory
	manifestDir := dir
	mf, err := manifestpkg.LoadFromDir(dir)
	if err != nil {
		// Check if there's a single subdirectory containing the manifest
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil, fmt.Errorf("read extracted directory: %w", readErr)
		}
		found := false
		for _, e := range entries {
			if e.IsDir() {
				subDir := filepath.Join(dir, e.Name())
				mf, err = manifestpkg.LoadFromDir(subDir)
				if err == nil {
					manifestDir = subDir
					found = true
					break
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("no valid plugin.yaml found in archive")
		}
	}

	// Validate required fields
	if mf.Metadata.Slug == "" {
		return nil, fmt.Errorf("plugin.yaml missing required field: metadata.slug")
	}

	category := ""
	switch mf.Type {
	case manifestpkg.PluginTypeAdapter:
		if mf.Adapter != nil {
			category = mf.Adapter.Slot
		}
	case manifestpkg.PluginTypeToolHandler:
		category = "tool-handler"
	case manifestpkg.PluginTypePipelineCleaner:
		category = "cleaner"
	}
	if category == "" {
		return nil, fmt.Errorf("plugin.yaml missing category (no slot for adapter, or unrecognized type)")
	}

	slug := mf.Metadata.Slug

	if !isValidSlug(slug) {
		return nil, fmt.Errorf("plugin.yaml contains invalid slug: %q (must match [a-zA-Z0-9][a-zA-Z0-9._-]*)", slug)
	}

	// Verify slug matches marketplace expectation
	if expectedSlug != "" && slug != expectedSlug {
		return nil, fmt.Errorf("slug mismatch: marketplace says %q but plugin.yaml says %q", expectedSlug, slug)
	}

	if !isValidNamespace(namespace) {
		return nil, fmt.Errorf("invalid namespace: %q", namespace)
	}

	// Check for conflicts on disk
	destDir := filepath.Join(inst.pluginDir, namespace, slug)
	if _, err := os.Stat(destDir); err == nil {
		return nil, fmt.Errorf("plugin %s/%s is already installed (directory exists: %s)", namespace, slug, destDir)
	}

	// Check in database
	_, err = inst.store.GetInstalledPlugin(ctx, namespace, slug)
	if err == nil {
		return nil, fmt.Errorf("plugin %s/%s is already installed", namespace, slug)
	}

	// Get marketplace ID for the namespace
	mp, err := inst.store.GetMarketplaceByNamespace(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("marketplace %q not found: %w", namespace, err)
	}

	// Move to final destination
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return nil, fmt.Errorf("create plugin directory: %w", err)
	}

	if err := copyDir(manifestDir, destDir); err != nil {
		os.RemoveAll(destDir)
		return nil, fmt.Errorf("install plugin files: %w", err)
	}

	// Record in database (enabled = false)
	if err := inst.store.InsertInstalledPlugin(ctx, mp.ID, slug, sourceURL); err != nil {
		os.RemoveAll(destDir)
		return nil, fmt.Errorf("record installation: %w", err)
	}

	return &InstallResult{
		Namespace: namespace,
		Slug:      slug,
		Version:   mf.Metadata.Version,
		Category:  category,
		Dir:       destDir,
	}, nil
}

// Uninstall removes an installed plugin from disk and database.
func (inst *Installer) Uninstall(ctx context.Context, namespace, slug string) error {
	// Check if it exists
	p, err := inst.store.GetInstalledPlugin(ctx, namespace, slug)
	if err != nil {
		if configstore.IsNotFound(err) {
			return fmt.Errorf("plugin %s/%s not found", namespace, slug)
		}
		return fmt.Errorf("look up plugin %s/%s: %w", namespace, slug, err)
	}

	// If enabled, disable first
	if p.Enabled {
		if err := inst.store.SetPluginEnabled(ctx, namespace, slug, false); err != nil {
			return fmt.Errorf("disable plugin before uninstall: %w", err)
		}
	}

	// Remove from disk first
	destDir := filepath.Join(inst.pluginDir, namespace, slug)
	if err := os.RemoveAll(destDir); err != nil {
		return fmt.Errorf("remove plugin files: %w", err)
	}

	// Remove from database
	if err := inst.store.DeleteInstalledPlugin(ctx, namespace, slug); err != nil {
		return fmt.Errorf("remove from database: %w", err)
	}

	// Clean up empty namespace directory
	nsDir := filepath.Join(inst.pluginDir, namespace)
	entries, err := os.ReadDir(nsDir)
	if err == nil && len(entries) == 0 {
		os.Remove(nsDir)
	}

	return nil
}

// --- Helper functions ---

func (inst *Installer) downloadToTemp(ctx context.Context, rawURL string) (string, error) {
	if err := validate.HTTPURL(rawURL); err != nil {
		return "", err
	}
	if err := validate.RejectPrivateURL(rawURL); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "nupi-plugin-installer/1.0")

	resp, err := inst.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}

	tmpFile, err := os.CreateTemp("", "nupi-plugin-*.download")
	if err != nil {
		return "", err
	}

	// Ensure temp file is cleaned up on any error path.
	success := false
	name := tmpFile.Name()
	defer func() {
		if !success {
			tmpFile.Close()
			if rmErr := os.Remove(name); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("[Installer] WARNING: failed to remove temp file %s: %v", name, rmErr)
			}
		}
	}()

	lr := io.LimitReader(resp.Body, maxArchiveSize+1) // read one extra byte to detect truncation
	n, err := io.Copy(tmpFile, lr)
	if err != nil {
		return "", err
	}
	if n > maxArchiveSize {
		return "", fmt.Errorf("archive exceeds maximum size (%d bytes)", maxArchiveSize)
	}

	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("finalize download: %w", err)
	}
	success = true
	return name, nil
}

// errNoChecksum is returned when verifySHA256 is called with an empty expected hash.
var errNoChecksum = errors.New("no SHA-256 checksum provided")

func verifySHA256(path, expected string) error {
	expected = strings.TrimSpace(strings.ToLower(expected))
	if expected == "" {
		return errNoChecksum
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

func extractArchive(ctx context.Context, archivePath, destDir string) error {
	// Detect format by file extension first, then fall back to magic bytes
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(ctx, archivePath, destDir)
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(ctx, archivePath, destDir)
	default:
		// Extension not recognized (e.g., temp file from download).
		// Detect format by reading magic bytes.
		format := detectArchiveFormat(archivePath)
		switch format {
		case "zip":
			return extractZip(ctx, archivePath, destDir)
		case "gzip":
			return extractTarGz(ctx, archivePath, destDir)
		default:
			// Unknown magic, try zip then tar.gz as last resort
			if err := extractZip(ctx, archivePath, destDir); err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return fmt.Errorf("extraction cancelled: %w", ctx.Err())
			}
			entries, _ := os.ReadDir(destDir)
			for _, e := range entries {
				os.RemoveAll(filepath.Join(destDir, e.Name()))
			}
			return extractTarGz(ctx, archivePath, destDir)
		}
	}
}

// detectArchiveFormat reads the first few bytes to identify the archive type.
// Returns "zip", "gzip", or "" if unknown.
func detectArchiveFormat(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	header := make([]byte, 4)
	n, err := f.Read(header)
	if err != nil || n < 2 {
		return ""
	}

	// ZIP: starts with PK (0x50 0x4B)
	if n >= 2 && header[0] == 0x50 && header[1] == 0x4B {
		return "zip"
	}
	// GZIP: starts with 0x1F 0x8B
	if n >= 2 && header[0] == 0x1F && header[1] == 0x8B {
		return "gzip"
	}
	return ""
}

func extractZip(ctx context.Context, archivePath, destDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	count := 0
	var totalSize int64

	for _, f := range r.File {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("extraction cancelled: %w", err)
		}
		count++
		if count > maxFileCount {
			return fmt.Errorf("archive contains too many files (max %d)", maxFileCount)
		}

		// Reject symlinks
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive contains symlink (not allowed): %s", f.Name)
		}

		target := filepath.Join(destDir, f.Name)
		// Prevent zip slip
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid path in archive: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if mkErr := os.MkdirAll(target, 0o755); mkErr != nil {
				return fmt.Errorf("create directory %s: %w", f.Name, mkErr)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode()&0o777|0o600)
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		written, copyErr := io.Copy(outFile, io.LimitReader(rc, maxArchiveSize+1))
		rc.Close()
		closeErr := outFile.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return fmt.Errorf("close extracted file %s: %w", f.Name, closeErr)
		}
		if written > maxArchiveSize {
			return fmt.Errorf("file %s exceeds maximum size (%d bytes)", f.Name, maxArchiveSize)
		}

		totalSize += written
		if totalSize > maxTotalExtractSize {
			return fmt.Errorf("archive exceeds total extraction limit (%d bytes)", maxTotalExtractSize)
		}
	}
	return nil
}

func extractTarGz(ctx context.Context, archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	count := 0
	var totalSize int64

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("extraction cancelled: %w", err)
		}
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		count++
		if count > maxFileCount {
			return fmt.Errorf("archive contains too many files (max %d)", maxFileCount)
		}

		// Reject symlinks and hard links
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			return fmt.Errorf("archive contains link entry (not allowed): %s", header.Name)
		}

		target := filepath.Join(destDir, header.Name)
		// Prevent path traversal
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid path in archive: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if mkErr := os.MkdirAll(target, 0o755); mkErr != nil {
				return fmt.Errorf("create directory %s: %w", header.Name, mkErr)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			outFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(header.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			written, copyErr := io.Copy(outFile, io.LimitReader(tr, maxArchiveSize+1))
			closeErr := outFile.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return fmt.Errorf("close extracted file %s: %w", header.Name, closeErr)
			}
			if written > maxArchiveSize {
				return fmt.Errorf("file %s exceeds maximum size (%d bytes)", header.Name, maxArchiveSize)
			}

			totalSize += written
			if totalSize > maxTotalExtractSize {
				return fmt.Errorf("archive exceeds total extraction limit (%d bytes)", maxTotalExtractSize)
			}
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip symlinks — do not follow them (security: prevents directory traversal)
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		// Only copy regular files
		if !info.Mode().IsRegular() {
			return nil
		}

		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		dstFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode()&0o777|0o600)
		if err != nil {
			return err
		}

		_, err = io.Copy(dstFile, srcFile)
		if closeErr := dstFile.Close(); err == nil {
			err = closeErr
		}
		return err
	})
}

func isValidSlug(s string) bool {
	return validate.Ident(s)
}

func isValidNamespace(s string) bool {
	return validate.Ident(s)
}
