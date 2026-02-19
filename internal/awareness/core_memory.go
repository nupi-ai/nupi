package awareness

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxCoreMemoryChars is the hard cap for total core memory content (NFR29).
const maxCoreMemoryChars = 15000

// coreMemoryFiles lists the core memory files loaded in order.
// Each entry is relative to the awareness directory.
var coreMemoryFiles = []struct {
	name     string // Display name used as section header
	filename string // File path relative to awarenessDir
}{
	{"SOUL", "SOUL.md"},
	{"IDENTITY", "IDENTITY.md"},
	{"USER", "USER.md"},
	{"GLOBAL", "GLOBAL.md"},
}

// loadCoreMemory reads all core memory files and combines them into a single string.
// If activeProjectSlug is non-empty, PROJECT.md from the project directory is included.
// Missing files are logged as warnings but do not cause errors.
func (s *Service) loadCoreMemory(activeProjectSlug string) {
	var sections []string

	for _, f := range coreMemoryFiles {
		path := filepath.Join(s.awarenessDir, f.filename)
		content, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("[Awareness] WARNING: core memory file missing: %s", path)
			} else {
				log.Printf("[Awareness] ERROR: reading core memory file %s: %v", path, err)
			}
			continue
		}

		text := strings.TrimSpace(string(content))
		if text == "" {
			continue
		}

		sections = append(sections, fmt.Sprintf("## %s\n%s", f.name, text))
	}

	// Load project-specific core memory if a project is active.
	// Validate slug to prevent path traversal — reject if it contains path separators or "..".
	if activeProjectSlug != "" && !strings.Contains(activeProjectSlug, "/") && !strings.Contains(activeProjectSlug, "\\") && !strings.Contains(activeProjectSlug, "..") {
		projectPath := filepath.Join(s.awarenessDir, "memory", "projects", activeProjectSlug, "PROJECT.md")
		content, err := os.ReadFile(projectPath)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("[Awareness] WARNING: project core memory file missing: %s", projectPath)
			} else {
				log.Printf("[Awareness] ERROR: reading project core memory %s: %v", projectPath, err)
			}
		} else {
			text := strings.TrimSpace(string(content))
			if text != "" {
				sections = append(sections, fmt.Sprintf("## PROJECT\n%s", text))
			}
		}
	}

	combined := strings.Join(sections, "\n\n")

	// Apply character cap (NFR29) — truncate at rune boundary to avoid breaking UTF-8.
	if utf8.RuneCountInString(combined) > maxCoreMemoryChars {
		runes := []rune(combined)
		combined = string(runes[:maxCoreMemoryChars])
		log.Printf("[Awareness] WARNING: core memory truncated to %d chars (NFR29 cap)", maxCoreMemoryChars)
	}

	s.mu.Lock()
	s.coreMemory = combined
	s.mu.Unlock()
}
