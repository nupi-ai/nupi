# Nupi Build System
# =================

# Variables
BINARY_DIR := bin
APP_DIR := clients/desktop
TAURI_BINARY_DIR := $(APP_DIR)/src-tauri/binaries
VERSION := $(shell (git describe --tags --always 2>/dev/null || echo dev) | sed 's/^v//')
# Extract valid semver from VERSION for Cargo.toml (Cargo requires MAJOR.MINOR.PATCH format).
# VERSION already has v prefix stripped, so just extract the MAJOR.MINOR.PATCH part.
CARGO_SEMVER := $(shell echo "$(VERSION)" | grep -oE '^[0-9]+\.[0-9]+\.[0-9]+')
ifeq ($(CARGO_SEMVER),)
CARGO_SEMVER := 0.0.0
endif
GO_LDFLAGS := -ldflags "-X github.com/nupi-ai/nupi/internal/version.version=$(VERSION)"
# BUN_VERSION is extracted from jsrunner package (single source of truth)
BUN_VERSION := $(shell grep 'RuntimeVersion.*=' internal/jsrunner/provider.go | sed 's/.*"\(.*\)".*/\1/')

# Platform detection
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

# Set target triple for current platform
ifeq ($(UNAME_S),Darwin)
	ifeq ($(UNAME_M),arm64)
		TARGET_TRIPLE := aarch64-apple-darwin
	else
		TARGET_TRIPLE := x86_64-apple-darwin
	endif
else ifeq ($(UNAME_S),Linux)
	ifeq ($(UNAME_M),x86_64)
		TARGET_TRIPLE := x86_64-unknown-linux-gnu
	else
		TARGET_TRIPLE := $(UNAME_M)-unknown-linux-gnu
	endif
else ifeq ($(UNAME_S),Windows_NT)
	TARGET_TRIPLE := x86_64-pc-windows-msvc
endif

# Bun download URLs based on platform
ifeq ($(UNAME_S),Darwin)
	ifeq ($(UNAME_M),arm64)
		BUN_ARCHIVE := bun-darwin-aarch64.zip
	else
		BUN_ARCHIVE := bun-darwin-x64.zip
	endif
else ifeq ($(UNAME_S),Linux)
	ifeq ($(UNAME_M),x86_64)
		BUN_ARCHIVE := bun-linux-x64.zip
	else ifeq ($(UNAME_M),aarch64)
		BUN_ARCHIVE := bun-linux-aarch64.zip
	endif
endif
BUN_URL := https://github.com/oven-sh/bun/releases/download/bun-v$(BUN_VERSION)/$(BUN_ARCHIVE)

# Colors for output
RED := \033[0;31m
GREEN := \033[0;32m
YELLOW := \033[1;33m
NC := \033[0m # No Color

# Release platforms
PLATFORMS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 windows-amd64
DIST_DIR := dist
RELEASE_DIR := releases

.PHONY: all clean cli daemon app dev test help install download-bun release release-all sync-version $(addprefix release-,$(PLATFORMS))

# Default target
all: cli daemon app
	@echo "$(GREEN)✓ Build complete!$(NC)"

# Build CLI binary
cli:
	@echo "$(YELLOW)Building nupi CLI...$(NC)"
	@mkdir -p $(BINARY_DIR)
	@go build $(GO_LDFLAGS) -o $(BINARY_DIR)/nupi ./cmd/nupi
	@echo "$(GREEN)✓ CLI built: $(BINARY_DIR)/nupi$(NC)"

# Build daemon binary
daemon:
	@echo "$(YELLOW)Building nupid daemon...$(NC)"
	@mkdir -p $(BINARY_DIR)
	@go build $(GO_LDFLAGS) -o $(BINARY_DIR)/nupid ./cmd/nupid
	@echo "$(GREEN)✓ Daemon built: $(BINARY_DIR)/nupid$(NC)"

# Download Bun runtime for JS plugin execution
download-bun:
	@echo "$(YELLOW)Downloading Bun $(BUN_VERSION)...$(NC)"
	@mkdir -p $(BINARY_DIR)
	@if [ -z "$(BUN_ARCHIVE)" ]; then \
		echo "$(RED)✗ Unsupported platform for Bun download$(NC)"; \
		exit 1; \
	fi
	@curl -fsSL "$(BUN_URL)" -o /tmp/bun.zip
	@unzip -o -j /tmp/bun.zip -d $(BINARY_DIR) "*/bun"
	@rm /tmp/bun.zip
	@chmod +x $(BINARY_DIR)/bun
	@echo "$(GREEN)✓ Bun $(BUN_VERSION) installed to $(BINARY_DIR)/bun$(NC)"

# Sync VERSION into desktop config files before build
sync-version:
	@command -v jq >/dev/null 2>&1 || { echo "$(RED)✗ 'jq' is required for sync-version but not found. Install via: brew install jq$(NC)"; exit 1; }
	@echo "$(VERSION)" | grep -qE '^([0-9]+\.[0-9]+\.[0-9]+(-[0-9a-zA-Z._-]+)*|[0-9a-f]+|dev)$$' || { echo "$(RED)✗ VERSION '$(VERSION)' is not semver, commit hash, or 'dev'$(NC)"; exit 1; }
	@echo "$(YELLOW)Syncing version $(CARGO_SEMVER) into desktop config files...$(NC)"
	@jq '.version = "$(CARGO_SEMVER)"' $(APP_DIR)/src-tauri/tauri.conf.json > tmp.$$$$.json && mv tmp.$$$$.json $(APP_DIR)/src-tauri/tauri.conf.json
	@jq '.version = "$(CARGO_SEMVER)"' $(APP_DIR)/package.json > tmp.$$$$.json && mv tmp.$$$$.json $(APP_DIR)/package.json
	@sed -i.bak 's/^version = ".*"/version = "$(CARGO_SEMVER)"/' $(APP_DIR)/src-tauri/Cargo.toml && rm -f $(APP_DIR)/src-tauri/Cargo.toml.bak
	@echo "$(GREEN)✓ Version synced to $(CARGO_SEMVER)$(NC)"

# Build desktop app
app: cli daemon sync-version
	@echo "$(YELLOW)Building Tauri desktop app...$(NC)"
	@# Copy Go binaries to Tauri sidecar directory with correct naming
	@mkdir -p $(TAURI_BINARY_DIR)
	@cp $(BINARY_DIR)/nupi $(TAURI_BINARY_DIR)/nupi-$(TARGET_TRIPLE)
	@cp $(BINARY_DIR)/nupid $(TAURI_BINARY_DIR)/nupid-$(TARGET_TRIPLE)
	@echo "$(GREEN)✓ Binaries copied to Tauri$(NC)"
	@# Install npm dependencies if needed
	@cd $(APP_DIR) && npm install --silent
	@# Build Tauri app
	@cd $(APP_DIR) && npm run tauri build || (echo "$(RED)✗ Build failed$(NC)" && exit 1)
	@echo "$(GREEN)✓ Desktop app built$(NC)"

# Development mode
dev: cli daemon
	@echo "$(YELLOW)Starting development mode...$(NC)"
	@# Copy binaries for development
	@mkdir -p $(TAURI_BINARY_DIR)
	@cp $(BINARY_DIR)/nupi $(TAURI_BINARY_DIR)/nupi-$(TARGET_TRIPLE)
	@cp $(BINARY_DIR)/nupid $(TAURI_BINARY_DIR)/nupid-$(TARGET_TRIPLE)
	@# Start Tauri dev server
	cd $(APP_DIR) && npm run tauri dev

# Run tests
test:
	@echo "$(YELLOW)Running tests...$(NC)"
	@go test -v ./...
	@echo "$(GREEN)✓ Tests complete$(NC)"

# Install locally to ~/.nupi/ (no sudo required)
# Note: host.js is now embedded in the daemon binary, no external files needed
# Note: adapter-runner has been removed - adapters are now launched directly
install: cli daemon
	@echo "$(YELLOW)Installing Nupi to ~/.nupi/...$(NC)"
	@mkdir -p $(HOME)/.nupi/bin
	@cp $(BINARY_DIR)/nupi $(HOME)/.nupi/bin/nupi
	@cp $(BINARY_DIR)/nupid $(HOME)/.nupi/bin/nupid
	@chmod +x $(HOME)/.nupi/bin/nupi
	@chmod +x $(HOME)/.nupi/bin/nupid
	@# Copy Bun - required for JS adapter plugins
	@if [ -f "$(BINARY_DIR)/bun" ]; then \
		cp $(BINARY_DIR)/bun $(HOME)/.nupi/bin/bun && \
		chmod +x $(HOME)/.nupi/bin/bun && \
		echo "$(GREEN)✓ Bun runtime installed$(NC)"; \
	else \
		echo "$(RED)✗ Bun runtime not found in $(BINARY_DIR)/$(NC)"; \
		echo "$(RED)  JS adapter plugins will not work without the bundled Bun runtime.$(NC)"; \
		echo "$(YELLOW)  Run 'make download-bun' first, then 'make install' again.$(NC)"; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ Nupi installed to ~/.nupi/$(NC)"
	@echo ""
	@echo "$(YELLOW)Add to your PATH:$(NC)"
	@echo '  export PATH="$$HOME/.nupi/bin:$$PATH"'
	@echo '  export NUPI_HOME="$$HOME/.nupi"'
	@echo ""
	@echo "$(YELLOW)Add to your shell config (~/.bashrc or ~/.zshrc):$(NC)"
	@echo '  echo '\''export PATH="$$HOME/.nupi/bin:$$PATH"'\'' >> ~/.bashrc'
	@echo '  echo '\''export NUPI_HOME="$$HOME/.nupi"'\'' >> ~/.bashrc'

# Clean build artifacts
clean:
	@echo "$(YELLOW)Cleaning build artifacts...$(NC)"
	@rm -rf $(BINARY_DIR)
	@rm -rf $(TAURI_BINARY_DIR)
	@rm -rf $(APP_DIR)/dist
	@rm -rf $(APP_DIR)/src-tauri/target
	@rm -rf $(DIST_DIR)
	@rm -rf $(RELEASE_DIR)
	@echo "$(GREEN)✓ Clean complete$(NC)"

# =============================================================================
# Release targets - Build packages with bundled Bun for each platform
# =============================================================================

# Build all release packages
release-all: $(addprefix release-,$(PLATFORMS))
	@echo "$(GREEN)✓ All release packages built in $(RELEASE_DIR)/$(NC)"

# Release package for specific platform
# Usage: make release-darwin-arm64
release-darwin-arm64:
	@$(MAKE) _release GOOS=darwin GOARCH=arm64 PLATFORM=darwin-arm64

release-darwin-amd64:
	@$(MAKE) _release GOOS=darwin GOARCH=amd64 PLATFORM=darwin-amd64

release-linux-amd64:
	@$(MAKE) _release GOOS=linux GOARCH=amd64 PLATFORM=linux-amd64

release-linux-arm64:
	@$(MAKE) _release GOOS=linux GOARCH=arm64 PLATFORM=linux-arm64

release-windows-amd64:
	@$(MAKE) _release GOOS=windows GOARCH=amd64 PLATFORM=windows-amd64 EXE=.exe

# Internal release target (called with GOOS, GOARCH, PLATFORM, EXE set)
_release:
	@echo "$(YELLOW)Building release for $(PLATFORM)...$(NC)"
	@mkdir -p $(DIST_DIR)/$(PLATFORM)/bin
	@# Build binaries for target platform
	@echo "  Building nupi..."
	@GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_LDFLAGS) -o $(DIST_DIR)/$(PLATFORM)/bin/nupi$(EXE) ./cmd/nupi
	@echo "  Building nupid..."
	@GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_LDFLAGS) -o $(DIST_DIR)/$(PLATFORM)/bin/nupid$(EXE) ./cmd/nupid
	@# Download Bun for target platform
	@echo "  Downloading Bun $(BUN_VERSION)..."
	@./scripts/download-bun.sh $(PLATFORM) $(BUN_VERSION) $(DIST_DIR)/$(PLATFORM)/bin
	@# Create release archive
	@mkdir -p $(RELEASE_DIR)
	@echo "  Creating archive..."
	@if [ "$(GOOS)" = "windows" ]; then \
		cd $(DIST_DIR)/$(PLATFORM) && zip -r ../../$(RELEASE_DIR)/nupi-$(VERSION)-$(PLATFORM).zip bin/; \
	else \
		tar -czvf $(RELEASE_DIR)/nupi-$(VERSION)-$(PLATFORM).tar.gz -C $(DIST_DIR)/$(PLATFORM) bin/; \
	fi
	@echo "$(GREEN)✓ Release package: $(RELEASE_DIR)/nupi-$(VERSION)-$(PLATFORM).tar.gz$(NC)"

# Quick release for current platform only
release:
	@if [ "$(UNAME_S)" = "Darwin" ] && [ "$(UNAME_M)" = "arm64" ]; then \
		$(MAKE) release-darwin-arm64; \
	elif [ "$(UNAME_S)" = "Darwin" ]; then \
		$(MAKE) release-darwin-amd64; \
	elif [ "$(UNAME_S)" = "Linux" ] && [ "$(UNAME_M)" = "x86_64" ]; then \
		$(MAKE) release-linux-amd64; \
	elif [ "$(UNAME_S)" = "Linux" ]; then \
		$(MAKE) release-linux-arm64; \
	else \
		$(MAKE) release-windows-amd64; \
	fi

# Help message
help:
	@echo "$(YELLOW)Nupi Build System$(NC)"
	@echo ""
	@echo "Available targets:"
	@echo "  $(GREEN)make$(NC)              - Build everything (CLI, daemon, and desktop app)"
	@echo "  $(GREEN)make cli$(NC)          - Build only the CLI binary"
	@echo "  $(GREEN)make daemon$(NC)       - Build only the daemon binary"
	@echo "  $(GREEN)make download-bun$(NC) - Download Bun $(BUN_VERSION) for current platform"
	@echo "  $(GREEN)make app$(NC)          - Build the desktop application"
	@echo "  $(GREEN)make dev$(NC)          - Run in development mode"
	@echo "  $(GREEN)make test$(NC)         - Run all tests"
	@echo "  $(GREEN)make install$(NC)      - Install to ~/.nupi/ (no sudo)"
	@echo "  $(GREEN)make clean$(NC)        - Remove all build artifacts"
	@echo "  $(GREEN)make help$(NC)         - Show this help message"
	@echo ""
	@echo "$(YELLOW)Release targets (with bundled Bun):$(NC)"
	@echo "  $(GREEN)make release$(NC)      - Build release package for current platform"
	@echo "  $(GREEN)make release-all$(NC)  - Build release packages for all platforms"
	@echo "  $(GREEN)make release-darwin-arm64$(NC)  - macOS Apple Silicon"
	@echo "  $(GREEN)make release-darwin-amd64$(NC)  - macOS Intel"
	@echo "  $(GREEN)make release-linux-amd64$(NC)   - Linux x86_64"
	@echo "  $(GREEN)make release-linux-arm64$(NC)   - Linux ARM64"
	@echo "  $(GREEN)make release-windows-amd64$(NC) - Windows x64"
	@echo ""
	@echo "Build outputs:"
	@echo "  CLI:       $(BINARY_DIR)/nupi"
	@echo "  Daemon:    $(BINARY_DIR)/nupid"
	@echo "  Bun:       $(BINARY_DIR)/bun (after download-bun)"
	@echo "  App:       $(APP_DIR)/src-tauri/target/release/bundle/"
	@echo "  Releases:  $(RELEASE_DIR)/nupi-$(VERSION)-<platform>.tar.gz"
	@echo ""
	@echo "Note: Release packages include bundled Bun $(BUN_VERSION) for offline installs."
	@echo ""
	@echo "Installation:"
	@echo "  $(YELLOW)make download-bun install$(NC) - Install with bundled Bun runtime (required)"
	@echo "  $(YELLOW)make release && tar -xzf ...$(NC) - From release package"
	@echo ""
	@echo "Note: Bun runtime is REQUIRED for JS adapter plugins. Run 'make download-bun' before 'make install'."
