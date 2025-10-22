# Nupi Build System
# =================

# Variables
BINARY_DIR := bin
APP_DIR := clients/desktop
TAURI_BINARY_DIR := $(APP_DIR)/src-tauri/binaries
RUNNER_VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)

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

# Colors for output
RED := \033[0;31m
GREEN := \033[0;32m
YELLOW := \033[1;33m
NC := \033[0m # No Color

.PHONY: all clean cli daemon adapter-runner app dev test help install

# Default target
all: cli daemon adapter-runner app
	@echo "$(GREEN)✓ Build complete!$(NC)"

# Build CLI binary
cli:
	@echo "$(YELLOW)Building nupi CLI...$(NC)"
	@mkdir -p $(BINARY_DIR)
	@go build -o $(BINARY_DIR)/nupi ./cmd/nupi
	@echo "$(GREEN)✓ CLI built: $(BINARY_DIR)/nupi$(NC)"

# Build daemon binary
daemon:
	@echo "$(YELLOW)Building nupid daemon...$(NC)"
	@mkdir -p $(BINARY_DIR)
	@go build -o $(BINARY_DIR)/nupid ./cmd/nupid
	@echo "$(GREEN)✓ Daemon built: $(BINARY_DIR)/nupid$(NC)"

# Build adapter-runner binary
adapter-runner:
	@echo "$(YELLOW)Building adapter-runner...$(NC)"
	@mkdir -p $(BINARY_DIR)
	@go build -o $(BINARY_DIR)/adapter-runner ./cmd/adapter-runner
	@echo "$(GREEN)✓ Adapter-runner built: $(BINARY_DIR)/adapter-runner$(NC)"

# Build desktop app
app: cli daemon
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

# Install locally to ~/.nupi/bin (no sudo required)
install: cli daemon adapter-runner
	@echo "$(YELLOW)Installing Nupi to ~/.nupi/bin/...$(NC)"
	@mkdir -p $(HOME)/.nupi/bin
	@cp $(BINARY_DIR)/nupi $(HOME)/.nupi/bin/nupi
	@cp $(BINARY_DIR)/nupid $(HOME)/.nupi/bin/nupid
	@chmod +x $(HOME)/.nupi/bin/nupi
	@chmod +x $(HOME)/.nupi/bin/nupid
	@runner_root="$(HOME)/.nupi/bin/adapter-runner"; \
	version="$(RUNNER_VERSION)"; \
	rm -f "$$runner_root"; \
	mkdir -p "$$runner_root/versions/$$version"; \
	cp $(BINARY_DIR)/adapter-runner "$$runner_root/versions/$$version/adapter-runner"; \
	chmod +x "$$runner_root/versions/$$version/adapter-runner"; \
	rm -rf "$$runner_root/current"; \
	mkdir -p "$$runner_root/current"; \
	ln -sf "../versions/$$version/adapter-runner" "$$runner_root/current/adapter-runner"; \
	echo "$$version" > "$$runner_root/current/VERSION"; \
	ln -sf "$$runner_root/current/adapter-runner" $(HOME)/.nupi/bin/adapter-runner
	@echo "$(GREEN)✓ Nupi installed to ~/.nupi/bin/$(NC)"
	@echo ""
	@echo "$(YELLOW)Add to your PATH:$(NC)"
	@echo '  export PATH="$$HOME/.nupi/bin:$$PATH"'
	@echo ""
	@echo "$(YELLOW)Add to your shell config (~/.bashrc or ~/.zshrc):$(NC)"
	@echo '  echo '\''export PATH="$$HOME/.nupi/bin:$$PATH"'\'' >> ~/.bashrc'

# Clean build artifacts
clean:
	@echo "$(YELLOW)Cleaning build artifacts...$(NC)"
	@rm -rf $(BINARY_DIR)
	@rm -rf $(TAURI_BINARY_DIR)
	@rm -rf $(APP_DIR)/dist
	@rm -rf $(APP_DIR)/src-tauri/target
	@echo "$(GREEN)✓ Clean complete$(NC)"

# Help message
help:
	@echo "$(YELLOW)Nupi Build System$(NC)"
	@echo ""
	@echo "Available targets:"
	@echo "  $(GREEN)make$(NC)         - Build everything (CLI, daemon, and desktop app)"
	@echo "  $(GREEN)make cli$(NC)     - Build only the CLI binary"
	@echo "  $(GREEN)make daemon$(NC)  - Build only the daemon binary"
	@echo "  $(GREEN)make adapter-runner$(NC) - Build only the adapter-runner binary"
	@echo "  $(GREEN)make app$(NC)     - Build the desktop application"
	@echo "  $(GREEN)make dev$(NC)     - Run in development mode"
	@echo "  $(GREEN)make test$(NC)    - Run all tests"
	@echo "  $(GREEN)make install$(NC) - Install to ~/.nupi/bin/ (no sudo)"
	@echo "  $(GREEN)make clean$(NC)   - Remove all build artifacts"
	@echo "  $(GREEN)make help$(NC)    - Show this help message"
	@echo ""
	@echo "Build outputs:"
	@echo "  CLI:     $(BINARY_DIR)/nupi"
	@echo "  Daemon:  $(BINARY_DIR)/nupid"
	@echo "  Runner:  $(BINARY_DIR)/adapter-runner"
	@echo "  App:     $(APP_DIR)/src-tauri/target/release/bundle/"
	@echo ""
	@echo "Installation:"
	@echo "  $(YELLOW)make install$(NC)      - Manual install to ~/.nupi/bin/"
	@echo "  $(YELLOW)curl install.sh$(NC)   - Automated install (recommended)"
	@echo "  $(YELLOW)Tauri Desktop$(NC)     - Auto-installs on first run"
