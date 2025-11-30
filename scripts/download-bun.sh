#!/bin/bash
# download-bun.sh - Downloads Bun runtime for specified platform with checksum verification
#
# Usage: ./scripts/download-bun.sh <platform> <version> <output-dir>
#   platform: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64, windows-amd64
#   version:  Bun version (e.g., 1.1.34)
#   output-dir: Directory to place the bun binary (default: bin/)
#
# Example:
#   ./scripts/download-bun.sh darwin-arm64 1.1.34 dist/darwin-arm64/bin

set -e

PLATFORM="${1:-}"
VERSION="${2:-}"
OUTPUT_DIR="${3:-bin}"

if [ -z "$PLATFORM" ] || [ -z "$VERSION" ]; then
    echo "Usage: $0 <platform> <version> [output-dir]"
    echo ""
    echo "Platforms: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64, windows-amd64"
    echo "Example: $0 darwin-arm64 1.1.34"
    exit 1
fi

# SHA256 checksums for Bun 1.1.34 (from official SHASUMS256.txt)
# Update these when updating RuntimeVersion in internal/jsrunner/provider.go
declare -A CHECKSUMS_1_1_34=(
    ["bun-darwin-aarch64.zip"]="ba7167e1b7b1ba97e3b4503a371ce2fef0b5b2dcee05760fe633dfc176d90e28"
    ["bun-darwin-x64.zip"]="564b30a1fb40ba4c16a06b6e07361f9fa3b0421493f11fd052fa9c1b3013ac5a"
    ["bun-linux-aarch64.zip"]="04862513246ec9476f8a9b025441d3391949a009c7fabbf5a20bf5d09507c8e0"
    ["bun-linux-x64.zip"]="4bc000ff5096c5348767ad04d993505f210039a95880273a76d7bd0af0fc2f1f"
    ["bun-windows-x64.zip"]="22d740bd1a04779399a3d3081052a3f7879ea6c8d5f3e55972ce2c0e73802b37"
)

# Map our platform names to Bun's archive names
case "$PLATFORM" in
    darwin-arm64)
        BUN_ARCHIVE="bun-darwin-aarch64.zip"
        BUN_BINARY="bun"
        ;;
    darwin-amd64)
        BUN_ARCHIVE="bun-darwin-x64.zip"
        BUN_BINARY="bun"
        ;;
    linux-amd64)
        BUN_ARCHIVE="bun-linux-x64.zip"
        BUN_BINARY="bun"
        ;;
    linux-arm64)
        BUN_ARCHIVE="bun-linux-aarch64.zip"
        BUN_BINARY="bun"
        ;;
    windows-amd64)
        BUN_ARCHIVE="bun-windows-x64.zip"
        BUN_BINARY="bun.exe"
        ;;
    *)
        echo "Error: Unknown platform '$PLATFORM'"
        echo "Supported: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64, windows-amd64"
        exit 1
        ;;
esac

# Get expected checksum for this version
get_expected_checksum() {
    local version=$1
    local archive=$2

    case "$version" in
        1.1.34)
            echo "${CHECKSUMS_1_1_34[$archive]}"
            ;;
        *)
            echo ""
            ;;
    esac
}

EXPECTED_CHECKSUM=$(get_expected_checksum "$VERSION" "$BUN_ARCHIVE")

if [ -z "$EXPECTED_CHECKSUM" ]; then
    echo "Warning: No embedded checksum for Bun ${VERSION}/${BUN_ARCHIVE}"
    echo "         Downloading checksum from GitHub release..."
    # Fallback: download SHASUMS256.txt from release
    EXPECTED_CHECKSUM=$(curl -fsSL "https://github.com/oven-sh/bun/releases/download/bun-v${VERSION}/SHASUMS256.txt" 2>/dev/null | grep "$BUN_ARCHIVE" | awk '{print $1}')
    if [ -z "$EXPECTED_CHECKSUM" ]; then
        echo "Error: Could not obtain checksum for Bun ${VERSION}/${BUN_ARCHIVE}"
        exit 1
    fi
fi

BUN_URL="https://github.com/oven-sh/bun/releases/download/bun-v${VERSION}/${BUN_ARCHIVE}"
TMP_DIR=$(mktemp -d)

echo "Downloading Bun ${VERSION} for ${PLATFORM}..."
echo "  URL: ${BUN_URL}"
echo "  Output: ${OUTPUT_DIR}/${BUN_BINARY}"
echo "  Expected SHA256: ${EXPECTED_CHECKSUM}"

# Create output directory
mkdir -p "$OUTPUT_DIR"

# Download archive
curl -fsSL "$BUN_URL" -o "${TMP_DIR}/bun.zip"

# Verify checksum
echo "Verifying checksum..."
if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_CHECKSUM=$(sha256sum "${TMP_DIR}/bun.zip" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL_CHECKSUM=$(shasum -a 256 "${TMP_DIR}/bun.zip" | awk '{print $1}')
else
    echo "Error: Neither sha256sum nor shasum available for checksum verification"
    rm -rf "$TMP_DIR"
    exit 1
fi

if [ "$ACTUAL_CHECKSUM" != "$EXPECTED_CHECKSUM" ]; then
    echo "Error: Checksum verification failed!"
    echo "  Expected: ${EXPECTED_CHECKSUM}"
    echo "  Actual:   ${ACTUAL_CHECKSUM}"
    rm -rf "$TMP_DIR"
    exit 1
fi
echo "Checksum verified: OK"

# Extract binary
# Bun archives contain a directory like bun-darwin-aarch64/bun
unzip -o -j "${TMP_DIR}/bun.zip" "*/${BUN_BINARY}" -d "$OUTPUT_DIR"

# Make executable (no-op on Windows but doesn't hurt)
chmod +x "${OUTPUT_DIR}/${BUN_BINARY}" 2>/dev/null || true

# Cleanup
rm -rf "$TMP_DIR"

echo "Done: ${OUTPUT_DIR}/${BUN_BINARY}"
