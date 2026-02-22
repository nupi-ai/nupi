package version

import (
	"fmt"
	"regexp"
	"strings"
)

var version = "dev"

// String returns the build version for the current binary.
func String() string {
	return version
}

// ForTesting overrides the version string and returns a cleanup function
// that restores the original value. Must not be called concurrently.
func ForTesting(v string) func() {
	original := version
	version = v
	return func() { version = original }
}

// gitDescribeSuffix matches the trailing "-N-gHASH" added by git describe
// (e.g., "0.3.0-5-gabcdef" → strip "-5-gabcdef").
var gitDescribeSuffix = regexp.MustCompile(`-\d+-g[0-9a-f]+$`)

// normalizeVersion strips the "v" prefix and any git-describe suffix so that
// versions like "v0.3.0-5-gabcdef" and "0.3.0" compare as equal.
func normalizeVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	return gitDescribeSuffix.ReplaceAllString(v, "")
}

// FormatVersion returns a display-friendly version string. For normal versions
// it ensures a "v" prefix (e.g. "0.3.0" → "v0.3.0"). Special values like
// "dev" and empty strings are returned as-is.
func FormatVersion(v string) string {
	if v == "" || v == "dev" {
		return v
	}
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// CheckVersionMismatch compares the local build version with the daemon's
// reported version.  It returns a human-readable warning string when the
// versions differ, or an empty string when they match or when either side
// reports "dev" (development builds are expected to be inconsistent).
func CheckVersionMismatch(daemonVersion string) string {
	if daemonVersion == "" || version == "" {
		return ""
	}
	client := version
	if client == "dev" || daemonVersion == "dev" {
		return ""
	}
	// 0.0.0 is the CARGO_SEMVER fallback when no git tags exist (see Makefile).
	if client == "0.0.0" || daemonVersion == "0.0.0" {
		return ""
	}
	clientNorm := normalizeVersion(client)
	daemonNorm := normalizeVersion(daemonVersion)
	if clientNorm == daemonNorm {
		return ""
	}
	return fmt.Sprintf(
		"WARNING: nupi %s connected to nupid %s — version mismatch, please restart the daemon or reinstall",
		FormatVersion(client), FormatVersion(daemonVersion),
	)
}
