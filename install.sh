#!/usr/bin/env bash
# Nupi Installation Script
# Usage: curl -fsSL https://nupi.sh/install.sh | bash

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
INSTALL_DIR="$HOME/.nupi/bin"
REPO="nupi-ai/nupi"
NUPI_HOME="$HOME/.nupi"

# Detect OS and architecture
OS="$(uname -s)"
ARCH="$(uname -m)"

echo -e "${YELLOW}Installing Nupi...${NC}"
echo ""

# Determine platform
case "$OS" in
    Darwin)
        PLATFORM="darwin"
        ;;
    Linux)
        PLATFORM="linux"
        ;;
    *)
        echo -e "${RED}Error: Unsupported operating system: $OS${NC}"
        exit 1
        ;;
esac

case "$ARCH" in
    x86_64)
        ARCH_NAME="amd64"
        ;;
    arm64|aarch64)
        ARCH_NAME="arm64"
        ;;
    *)
        echo -e "${RED}Error: Unsupported architecture: $ARCH${NC}"
        exit 1
        ;;
esac

TARGET="${PLATFORM}_${ARCH_NAME}"
echo -e "Platform: ${GREEN}$TARGET${NC}"

# Get latest release
echo -e "${YELLOW}Fetching latest release...${NC}"
RELEASE_URL="https://api.github.com/repos/$REPO/releases/latest"
RELEASE_JSON=$(curl -s "$RELEASE_URL")
DOWNLOAD_URL=$(echo "$RELEASE_JSON" | grep -Eo "https://[^"]+nupi_${TARGET}\\.tar\\.gz" | head -n1)
RUNNER_URL=$(echo "$RELEASE_JSON" | grep -Eo "https://[^"]+adapter-runner_${TARGET}\\.tar\\.gz" | head -n1)
TAG_NAME=$(echo "$RELEASE_JSON" | grep -m1 '"tag_name":' | cut -d '"' -f 4)

if [ -z "$DOWNLOAD_URL" ]; then
    echo -e "${RED}Error: Could not find release for $TARGET${NC}"
    exit 1
fi

echo -e "Download URL: ${GREEN}$DOWNLOAD_URL${NC}"

# Create temporary directory
TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

# Download and extract
echo -e "${YELLOW}Downloading Nupi...${NC}"
cd "$TMP_DIR"
curl -L -o nupi.tar.gz "$DOWNLOAD_URL"

echo -e "${YELLOW}Extracting...${NC}"
tar -xzf nupi.tar.gz

# Create installation directories
echo -e "${YELLOW}Creating installation directories...${NC}"
mkdir -p "$INSTALL_DIR"
mkdir -p "$NUPI_HOME/instances/default"

# Install binaries (no sudo needed - user's home directory)
echo -e "${YELLOW}Installing binaries to $INSTALL_DIR...${NC}"
cp nupi "$INSTALL_DIR/nupi"
cp nupid "$INSTALL_DIR/nupid"
chmod +x "$INSTALL_DIR/nupi"
chmod +x "$INSTALL_DIR/nupid"

install_adapter_runner() {
    local version="$1"
    local url="$2"

    if [ -z "$version" ]; then
        version="unknown"
    fi

    if [ -z "$url" ]; then
        echo -e "${YELLOW}Skipping adapter-runner installation (asset not found).${NC}"
        return
    fi

    echo -e "${YELLOW}Downloading adapter-runner...${NC}"
    curl -L -o adapter-runner.tar.gz "$url"

    mkdir -p adapter-runner
    tar -xzf adapter-runner.tar.gz -C adapter-runner

    local runner_bin
    runner_bin=$(find adapter-runner -type f -name 'adapter-runner*' | head -n1)
    if [ -z "$runner_bin" ]; then
        echo -e "${YELLOW}Warning: adapter-runner archive does not contain a binary.${NC}"
        return
    fi

    local runner_root="$NUPI_HOME/bin/adapter-runner"
    local version_dir="$runner_root/versions/$version"

    mkdir -p "$runner_root"
    mkdir -p "$version_dir"
    rm -rf "$runner_root/current"
    mkdir -p "$runner_root/current"

    cp "$runner_bin" "$version_dir/adapter-runner"
    chmod +x "$version_dir/adapter-runner"

    rm -f "$INSTALL_DIR/adapter-runner"
    cp "$version_dir/adapter-runner" "$runner_root/current/adapter-runner"
    echo "$version" > "$runner_root/current/VERSION"

    ln -sf "$runner_root/current/adapter-runner" "$INSTALL_DIR/adapter-runner"

    echo -e "${GREEN}✓ adapter-runner ${version} installed${NC}"
}

install_adapter_runner "$TAG_NAME" "$RUNNER_URL"

echo ""
echo -e "${GREEN}✓ Nupi installed successfully!${NC}"
echo ""
echo -e "Installed binaries:"
echo -e "  • ${GREEN}nupi${NC}           - CLI client"
echo -e "  • ${GREEN}nupid${NC}          - Background daemon"
echo -e "  • ${GREEN}adapter-runner${NC} - Adapter host for plugins"
echo ""
echo -e "Installation directory: ${GREEN}$INSTALL_DIR${NC}"
echo ""

# Add to PATH automatically
SHELL_RC=""
if [ -n "$BASH_VERSION" ]; then
    SHELL_RC="$HOME/.bashrc"
elif [ -n "$ZSH_VERSION" ]; then
    SHELL_RC="$HOME/.zshrc"
fi

if [ -n "$SHELL_RC" ] && [ -f "$SHELL_RC" ]; then
    if ! grep -q '.nupi/bin' "$SHELL_RC"; then
        echo -e "${YELLOW}Adding ~/.nupi/bin to PATH in $SHELL_RC${NC}"
        echo '' >> "$SHELL_RC"
        echo '# Nupi' >> "$SHELL_RC"
        echo 'export PATH="$HOME/.nupi/bin:$PATH"' >> "$SHELL_RC"
        echo -e "${GREEN}✓ Added to PATH${NC}"
        echo -e "${YELLOW}Run: source $SHELL_RC${NC} or restart your terminal"
    else
        echo -e "${GREEN}✓ PATH already configured${NC}"
    fi
else
    echo -e "${YELLOW}Add to your shell configuration:${NC}"
    echo -e "  export PATH=\"\$HOME/.nupi/bin:\$PATH\""
fi

echo ""
echo -e "Get started:"
echo -e "  ${YELLOW}nupi run${NC}           # Start your default shell"
echo -e "  ${YELLOW}nupi run claude${NC}    # Run Claude Code"
echo -e "  ${YELLOW}nupi list${NC}          # List active sessions"
echo -e "  ${YELLOW}nupi attach <id>${NC}   # Attach to a session"
echo ""
echo -e "The daemon will start automatically when needed."
