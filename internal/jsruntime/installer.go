package jsruntime

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// BunVersion is the version of Bun to download.
const BunVersion = "1.1.34"

// BunBaseURL is the base URL for Bun releases.
const BunBaseURL = "https://github.com/oven-sh/bun/releases/download"

// BunChecksums contains SHA256 checksums for Bun releases.
// These are verified against downloaded archives before extraction.
// Checksums from: https://github.com/oven-sh/bun/releases/tag/bun-v1.1.34
var BunChecksums = map[string]string{
	"bun-darwin-aarch64.zip": "36b9deee120868184e65fc1d1a5e6e02be0c55c589515b4e59c4b8c7ca1fadb9",
	"bun-darwin-x64.zip":     "ea99dfc57e1c1a3d2d858f2a4a02ea280ddcd81b574f2c7e5fc0c5ed4bd6d53d",
	"bun-linux-aarch64.zip":  "b8ed9a916f43ed6b18943eb5c0bacf66c9f6f6a5fbe23a58f51ff91d18adecbd",
	"bun-linux-x64.zip":      "19958f35e2a57e17de60aab0dd14af09b636cdba9bee0f46c3f052b1d1a3f59c",
}

// EnsureBun ensures Bun is available, downloading it if necessary.
// Returns the path to the bun binary.
// If download is required but fails (e.g., offline), returns an error.
func EnsureBun(ctx context.Context) (string, error) {
	// 1. Check if already resolved via environment or PATH
	if path := resolveJSRuntime(); path != "bun" {
		// We have a specific path (not just "bun" which means search PATH)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// 2. Check if bun is in PATH
	if path, err := resolveBunFromPath(); err == nil {
		return path, nil
	}

	// 3. Check if we have a bundled bun in NUPI_HOME
	bunPath := getBundledBunPath()
	if _, err := os.Stat(bunPath); err == nil {
		return bunPath, nil
	}

	// 4. Download Bun (with timeout)
	log.Printf("[jsruntime] Bun not found, downloading v%s for %s/%s...", BunVersion, runtime.GOOS, runtime.GOARCH)

	// Use a reasonable timeout for download (5 minutes)
	downloadCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := downloadBun(downloadCtx, bunPath); err != nil {
		return "", fmt.Errorf("download bun: %w", err)
	}

	log.Printf("[jsruntime] Bun v%s installed to %s", BunVersion, bunPath)
	return bunPath, nil
}

// EnsureBunGraceful is like EnsureBun but returns ("", nil) instead of error
// when download fails due to network issues. This allows graceful degradation
// in offline scenarios where plugins are not critical.
func EnsureBunGraceful(ctx context.Context) (string, error) {
	// 1. Check if already resolved via environment or PATH
	if path := resolveJSRuntime(); path != "bun" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// 2. Check if bun is in PATH
	if path, err := resolveBunFromPath(); err == nil {
		return path, nil
	}

	// 3. Check if we have a bundled bun in NUPI_HOME
	bunPath := getBundledBunPath()
	if _, err := os.Stat(bunPath); err == nil {
		return bunPath, nil
	}

	// 4. Try to download, but don't fail on network errors
	log.Printf("[jsruntime] Bun not found, attempting download v%s for %s/%s...", BunVersion, runtime.GOOS, runtime.GOARCH)

	downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := downloadBun(downloadCtx, bunPath); err != nil {
		log.Printf("[jsruntime] WARNING: Bun download failed: %v (JS plugins will not work)", err)
		return "", nil // Return empty path, no error - graceful degradation
	}

	log.Printf("[jsruntime] Bun v%s installed to %s", BunVersion, bunPath)
	return bunPath, nil
}

// getBundledBunPath returns the path where we store the downloaded Bun binary.
func getBundledBunPath() string {
	// Use NUPI_HOME if set, otherwise default to ~/.nupi
	home := os.Getenv("NUPI_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			userHome = "."
		}
		home = filepath.Join(userHome, ".nupi")
	}
	return filepath.Join(home, "bin", "bun")
}

// resolveBunFromPath looks for bun in PATH.
func resolveBunFromPath() (string, error) {
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		path := filepath.Join(dir, "bun")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("bun not found in PATH")
}

// downloadBun downloads and installs Bun to the specified path.
func downloadBun(ctx context.Context, destPath string) error {
	url, format, checksum, err := getBunDownloadInfo()
	if err != nil {
		return err
	}

	// Create destination directory
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Download the archive
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Download to temp file and verify checksum
	tmpFile, err := os.CreateTemp("", "bun-*.zip")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmpFile, hash), resp.Body)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("download to temp: %w", err)
	}
	tmpFile.Close()

	// Verify checksum
	actualChecksum := hex.EncodeToString(hash.Sum(nil))
	if checksum != "" && actualChecksum != checksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", checksum, actualChecksum)
	}

	// Reopen for extraction
	tmpFile, err = os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("reopen temp file: %w", err)
	}
	defer tmpFile.Close()

	// Extract based on format
	switch format {
	case "zip":
		return extractBunFromZip(tmpFile, written, destPath)
	case "tgz":
		return extractBunFromTarGz(tmpFile, destPath)
	default:
		return fmt.Errorf("unknown archive format: %s", format)
	}
}

// getBunDownloadInfo returns the download URL, format, and checksum for the current platform.
func getBunDownloadInfo() (url string, format string, checksum string, err error) {
	var platform, arch string

	switch runtime.GOOS {
	case "darwin":
		platform = "darwin"
		format = "zip"
	case "linux":
		platform = "linux"
		format = "zip"
	default:
		return "", "", "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}

	switch runtime.GOARCH {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "aarch64"
	default:
		return "", "", "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}

	// Example: https://github.com/oven-sh/bun/releases/download/bun-v1.1.34/bun-darwin-aarch64.zip
	filename := fmt.Sprintf("bun-%s-%s.%s", platform, arch, format)
	url = fmt.Sprintf("%s/bun-v%s/%s", BunBaseURL, BunVersion, filename)
	checksum = BunChecksums[filename]

	return url, format, checksum, nil
}

// extractBunFromZip extracts the bun binary from a zip archive.
func extractBunFromZip(f *os.File, size int64, destPath string) error {
	// Open as zip
	zipReader, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}

	// Find the bun binary (it's usually in a subdirectory like bun-darwin-aarch64/bun)
	for _, zf := range zipReader.File {
		if strings.HasSuffix(zf.Name, "/bun") || zf.Name == "bun" {
			rc, err := zf.Open()
			if err != nil {
				return fmt.Errorf("open bun in zip: %w", err)
			}
			defer rc.Close()

			outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return fmt.Errorf("create bun: %w", err)
			}
			defer outFile.Close()

			if _, err := io.Copy(outFile, rc); err != nil {
				return fmt.Errorf("write bun: %w", err)
			}

			return nil
		}
	}

	return fmt.Errorf("bun binary not found in archive")
}

// extractBunFromTarGz extracts the bun binary from a tar.gz archive.
func extractBunFromTarGz(r io.Reader, destPath string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gzr.Close()

	// Read tar entries
	// This is a simplified implementation - for a real tar we'd need archive/tar
	// But Bun releases are zip files, so this is just for completeness
	return fmt.Errorf("tar.gz format not implemented - Bun releases use zip")
}
