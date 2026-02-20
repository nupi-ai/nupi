// Package integrity provides pure filesystem verification of plugin files
// against expected checksums. It is deliberately decoupled from the database
// store so that verification logic is testable without any database setup.
package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileChecksum represents a single file's expected checksum.
type FileChecksum struct {
	Path   string // forward-slash relative path (e.g. "main.js", "lib/util.js")
	SHA256 string // lowercase hex-encoded SHA-256 hash (64 characters)
}

// Result holds the outcome of a plugin integrity verification.
type Result struct {
	Verified    bool     // true if all expected files match their checksums
	NoChecksums bool     // true if no checksums were provided (manual install)
	Mismatches  []string // descriptive entries for each failure
}

// VerifyPlugin compares files in pluginDir against expected checksums.
// It returns a Result indicating whether all expected files are present and
// unmodified. Extra files in pluginDir that are not in expected (runtime
// artifacts) are silently ignored.
func VerifyPlugin(pluginDir string, expected []FileChecksum) Result {
	if len(expected) == 0 {
		return Result{NoChecksums: true}
	}

	// Clean once so every child-path comparison uses the same base.
	cleanDir := filepath.Clean(pluginDir)

	// Resolve symlinks in the plugin directory itself so that containment
	// checks below compare physical paths, not lexical ones.
	realDir, err := filepath.EvalSymlinks(cleanDir)
	if err != nil {
		realDir = cleanDir // best-effort fallback; per-file errors will surface below
	}

	// Sort a copy for deterministic output order.
	sorted := make([]FileChecksum, len(expected))
	copy(sorted, expected)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var mismatches []string

	for _, fc := range sorted {
		filePath := filepath.Join(cleanDir, filepath.FromSlash(fc.Path))

		// Reject paths that escape the plugin directory (e.g. "../../../etc/passwd").
		// filepath.IsLocal handles "..", "../foo", absolute paths, and Windows
		// reserved names (NUL, CON) without false-positives on "..manifest".
		rel, err := filepath.Rel(cleanDir, filePath)
		if err != nil || !filepath.IsLocal(rel) {
			mismatches = append(mismatches, fmt.Sprintf("error: %s: path escapes plugin directory", fc.Path))
			continue
		}

		// Resolve all symlinks (including intermediate directory components)
		// to get the physical path. This catches cases like pluginDir/lib being
		// a symlink to an external directory.
		realPath, err := filepath.EvalSymlinks(filePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				mismatches = append(mismatches, fmt.Sprintf("missing: %s", fc.Path))
				continue
			}
			mismatches = append(mismatches, fmt.Sprintf("error: %s: %v", fc.Path, err))
			continue
		}

		// Verify the resolved physical path is still within the plugin directory.
		realRel, err := filepath.Rel(realDir, realPath)
		if err != nil || !filepath.IsLocal(realRel) {
			mismatches = append(mismatches, fmt.Sprintf("error: %s: path resolves outside plugin directory", fc.Path))
			continue
		}

		// Open the resolved path and use Fstat (on the fd) to verify it is a
		// regular file. This closes the TOCTOU gap that existed in the previous
		// Lstat-then-Open approach — the file we type-check is the file we hash.
		f, err := os.Open(realPath)
		if err != nil {
			mismatches = append(mismatches, fmt.Sprintf("error: %s: %v", fc.Path, err))
			continue
		}

		info, err := f.Stat()
		if err != nil {
			f.Close()
			mismatches = append(mismatches, fmt.Sprintf("error: %s: %v", fc.Path, err))
			continue
		}
		if !info.Mode().IsRegular() {
			f.Close()
			mismatches = append(mismatches, fmt.Sprintf("error: %s: not a regular file", fc.Path))
			continue
		}

		h := sha256.New()
		if _, copyErr := io.Copy(h, f); copyErr != nil {
			f.Close()
			mismatches = append(mismatches, fmt.Sprintf("error: %s: %v", fc.Path, copyErr))
			continue
		}
		f.Close()

		got := hex.EncodeToString(h.Sum(nil))
		want := strings.ToLower(fc.SHA256)
		if got != want {
			wantPrefix := want // want comes from external input, may be < 8 chars
			if len(wantPrefix) > 8 {
				wantPrefix = wantPrefix[:8]
			}
			gotPrefix := got[:8] // always 64 chars from hex.EncodeToString
			mismatches = append(mismatches, fmt.Sprintf(
				"modified: %s (expected: %s..., got: %s...)", fc.Path, wantPrefix, gotPrefix))
		}
	}

	if len(mismatches) > 0 {
		return Result{Verified: false, Mismatches: mismatches}
	}
	return Result{Verified: true}
}
