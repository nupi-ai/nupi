package awareness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// normalizeDate converts a bare YYYY-MM-DD date string to an RFC3339 timestamp
// suitable for comparison against RFC3339Nano mtime values stored in the index.
// For upper bounds (isEnd=true) it appends T23:59:59.999999999Z to include the
// entire day. For lower bounds it appends T00:00:00Z. Already RFC3339 values
// and empty strings are returned unchanged.
func normalizeDate(s string, isEnd bool) string {
	if s == "" {
		return s
	}
	// If it already contains 'T', assume it's RFC3339 (or close enough).
	if strings.Contains(s, "T") {
		return s
	}
	// Validate YYYY-MM-DD format.
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return s // pass through; search layer will handle gracefully
	}
	if isEnd {
		return s + "T23:59:59.999999999Z"
	}
	return s + "T00:00:00Z"
}

// ToolSpec is a plain data transfer type describing a tool exposed by the awareness
// service. It avoids importing intentrouter types to maintain service boundary isolation
// (Boundary 4). The daemon bridges ToolSpec → intentrouter.ToolHandler via an adapter.
type ToolSpec struct {
	Name           string
	Description    string
	ParametersJSON string
	Handler        func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// ToolSpecs returns all awareness tool specifications with handler closures.
// Must be called after NewService; handlers are safe to register before Start()
// but must only be invoked after Start() (indexer must be open).
func (s *Service) ToolSpecs() []ToolSpec {
	specs := []ToolSpec{
		memorySearchSpec(s),
		memoryGetSpec(s),
		memoryWriteSpec(s),
		coreMemoryUpdateSpec(s),
		onboardingCompleteSpec(s),
	}
	if s.heartbeatStore != nil {
		specs = append(specs, heartbeatAddSpec(s), heartbeatRemoveSpec(s), heartbeatListSpec(s))
	}
	return specs
}

// maxGetFileBytes is the maximum content size returned by GetFile.
const maxGetFileBytes = 50 * 1024 // 50 KB

// --- memory_search ---

type memorySearchArgs struct {
	Query       string `json:"query"`
	Scope       string `json:"scope"`
	ProjectSlug string `json:"project_slug"`
	MaxResults  int    `json:"max_results"`
	DateFrom    string `json:"date_from"`
	DateTo      string `json:"date_to"`
}

func memorySearchSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:        "memory_search",
		Description: "Search long-term memory. Scope: 'project' searches current project memory, 'global' searches non-project memory, 'all' searches everything. Returns ranked results with snippets.",
		ParametersJSON: `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Search query"},
    "scope": {"type": "string", "enum": ["project", "global", "all"], "default": "all", "description": "Search scope"},
    "project_slug": {"type": "string", "description": "Project slug (required when scope='project')"},
    "max_results": {"type": "integer", "default": 5, "description": "Maximum results to return"},
    "date_from": {"type": "string", "description": "Start date filter (YYYY-MM-DD)"},
    "date_to": {"type": "string", "description": "End date filter (YYYY-MM-DD)"}
  },
  "required": ["query"]
}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var a memorySearchArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}

			if strings.TrimSpace(a.Query) == "" {
				return nil, fmt.Errorf("query is required")
			}

			scope := a.Scope
			if scope == "" {
				scope = "all"
			}
			if scope != "project" && scope != "global" && scope != "all" {
				return nil, fmt.Errorf("scope must be 'project', 'global', or 'all'")
			}

			maxResults := a.MaxResults
			if maxResults <= 0 {
				maxResults = 5
			}

			dateFrom := normalizeDate(a.DateFrom, false)
			dateTo := normalizeDate(a.DateTo, true)

			opts := SearchOptions{
				Query:       a.Query,
				Scope:       scope,
				ProjectSlug: a.ProjectSlug,
				MaxResults:  maxResults,
				DateFrom:    dateFrom,
				DateTo:      dateTo,
			}

			results, err := s.SearchHybrid(ctx, a.Query, opts)
			if err != nil {
				// Graceful degradation (NFR33): fall back to FTS-only search.
				log.Printf("[Awareness] memory_search hybrid failed, falling back to FTS: %v", err)
				results, err = s.Search(ctx, opts)
				if err != nil {
					return nil, fmt.Errorf("search failed: %w", err)
				}
			}

			type resultEntry struct {
				Path     string  `json:"path"`
				Snippet  string  `json:"snippet"`
				Score    float64 `json:"score"`
				FileType string  `json:"file_type"`
			}

			entries := make([]resultEntry, 0, len(results))
			for _, r := range results {
				entries = append(entries, resultEntry{
					Path:     r.Path,
					Snippet:  r.Snippet,
					Score:    r.Score,
					FileType: r.FileType,
				})
			}

			return json.Marshal(entries)
		},
	}
}

// --- memory_get ---

type memoryGetArgs struct {
	Path string `json:"path"`
}

func memoryGetSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:        "memory_get",
		Description: "Retrieve a specific memory file by path. Use memory_search first to find relevant files, then memory_get to read full content.",
		ParametersJSON: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path relative to memory directory (e.g., 'daily/2026-02-20.md', 'projects/nupi/topics/auth.md')"}
  },
  "required": ["path"]
}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var a memoryGetArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}

			content, err := s.GetFile(a.Path)
			if err != nil {
				return nil, err
			}

			return json.Marshal(map[string]string{
				"content": content,
				"path":    a.Path,
			})
		},
	}
}

// GetFile reads a file from the awareness/memory/ directory.
// The path must be relative and must not contain path traversal sequences.
// Content exceeding maxGetFileBytes is truncated at a UTF-8-safe boundary.
func (s *Service) GetFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}

	// Path traversal validation.
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	if strings.Contains(path, "..") {
		return "", fmt.Errorf("path traversal is not allowed")
	}

	fullPath := filepath.Join(s.awarenessDir, "memory", filepath.FromSlash(path))

	// Verify the resolved path is within the memory directory.
	// EvalSymlinks resolves symlinks to prevent bypass of the directory check.
	memoryDir := filepath.Join(s.awarenessDir, "memory")
	absMemory, err := filepath.Abs(memoryDir)
	if err != nil {
		return "", fmt.Errorf("internal error: %w", err)
	}
	// Resolve symlinks in the memory dir itself (may not exist yet, ignore error).
	if resolved, err := filepath.EvalSymlinks(absMemory); err == nil {
		absMemory = resolved
	}
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("file not found: %s", path)
		}
		return "", fmt.Errorf("invalid path: %w", err)
	}
	if !strings.HasPrefix(realPath, absMemory+string(filepath.Separator)) && realPath != absMemory {
		return "", fmt.Errorf("path is outside memory directory")
	}

	data, err := os.ReadFile(realPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("file not found: %s", path)
		}
		return "", fmt.Errorf("read file: %w", err)
	}

	// UTF-8-safe truncation — early return avoids allocating the full string.
	if len(data) > maxGetFileBytes {
		truncated := string(data[:maxGetFileBytes])
		for len(truncated) > 0 && !utf8.ValidString(truncated) {
			truncated = truncated[:len(truncated)-1]
		}
		return truncated + "\n[truncated]", nil
	}

	return string(data), nil
}

// --- memory_write ---

type memoryWriteArgs struct {
	Content     string `json:"content"`
	Type        string `json:"type"`
	TopicName   string `json:"topic_name"`
	ProjectSlug string `json:"project_slug"`
}

// WriteOptions configures a WriteMemory call.
type WriteOptions struct {
	Content     string
	Type        string // "daily" or "topic"
	TopicName   string
	ProjectSlug string
}

func memoryWriteSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:        "memory_write",
		Description: "Write or append content to memory. Use type 'daily' for date-based logs, 'topic' for named topic files. Content is appended to existing files.",
		ParametersJSON: `{
  "type": "object",
  "properties": {
    "content": {"type": "string", "description": "Content to write (markdown)"},
    "type": {"type": "string", "enum": ["daily", "topic"], "description": "Target type: 'daily' for today's log, 'topic' for named topic"},
    "topic_name": {"type": "string", "description": "Topic filename (required when type='topic', e.g., 'project-decisions')"},
    "project_slug": {"type": "string", "description": "Project slug (empty for global memory)"}
  },
  "required": ["content", "type"]
}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var a memoryWriteArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}

			if strings.TrimSpace(a.Content) == "" {
				return nil, fmt.Errorf("content is required")
			}

			if a.Type != "daily" && a.Type != "topic" {
				return nil, fmt.Errorf("type must be 'daily' or 'topic'")
			}

			if a.Type == "topic" && strings.TrimSpace(a.TopicName) == "" {
				return nil, fmt.Errorf("topic_name is required when type is 'topic'")
			}

			opts := WriteOptions{
				Content:     a.Content,
				Type:        a.Type,
				TopicName:   a.TopicName,
				ProjectSlug: a.ProjectSlug,
			}

			writtenPath, err := s.WriteMemory(ctx, opts)
			if err != nil {
				return nil, fmt.Errorf("write failed: %w", err)
			}

			return json.Marshal(map[string]string{
				"status": "ok",
				"path":   writtenPath,
			})
		},
	}
}

// validatePathComponent checks that a name used as a filesystem path component
// (project_slug, topic_name) is safe: no traversal sequences, no path separators,
// no control characters, and no leading/trailing whitespace.
func validatePathComponent(name, label string) error {
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid %s", label)
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("invalid %s: leading or trailing whitespace", label)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid %s: control characters not allowed", label)
		}
	}
	return nil
}

// WriteMemory writes or appends content to a daily log or topic file.
// Returns the relative path (from memory/) of the written file.
func (s *Service) WriteMemory(ctx context.Context, opts WriteOptions) (string, error) {
	if strings.TrimSpace(opts.Content) == "" {
		return "", fmt.Errorf("content is required")
	}

	if opts.Type != "daily" && opts.Type != "topic" {
		return "", fmt.Errorf("type must be 'daily' or 'topic'")
	}

	if opts.Type == "topic" && strings.TrimSpace(opts.TopicName) == "" {
		return "", fmt.Errorf("topic_name is required when type is 'topic'")
	}

	// Validate slug for path traversal and unsafe characters.
	if opts.ProjectSlug != "" {
		if err := validatePathComponent(opts.ProjectSlug, "project_slug"); err != nil {
			return "", err
		}
	}

	if opts.Type == "topic" {
		if err := validatePathComponent(opts.TopicName, "topic_name"); err != nil {
			return "", err
		}
	}

	s.memoryWriteMu.Lock()
	defer s.memoryWriteMu.Unlock()

	memoryDir := filepath.Join(s.awarenessDir, "memory")
	var relPath string
	var fullPath string

	now := time.Now().UTC()

	switch opts.Type {
	case "daily":
		dir := memoryDir
		relDir := ""
		if opts.ProjectSlug != "" {
			dir = filepath.Join(memoryDir, "projects", opts.ProjectSlug, "daily")
			relDir = filepath.Join("projects", opts.ProjectSlug, "daily")
		} else {
			dir = filepath.Join(memoryDir, "daily")
			relDir = "daily"
		}

		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create daily dir: %w", err)
		}

		date := now.Format("2006-01-02")
		filename := date + ".md"
		fullPath = filepath.Join(dir, filename)
		relPath = filepath.Join(relDir, filename)

		// Build content with timestamp header.
		section := fmt.Sprintf("\n\n## %s\n\n%s", now.Format("15:04 UTC"), opts.Content)

		existing, err := os.ReadFile(fullPath)
		var data []byte
		if err == nil {
			data = append(existing, []byte(section)...)
		} else if errors.Is(err, os.ErrNotExist) {
			data = []byte(fmt.Sprintf("# Daily Log %s%s", date, section))
		} else {
			return "", fmt.Errorf("read existing daily file: %w", err)
		}

		// Atomic write.
		tmpPath := fullPath + ".tmp"
		if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("write temp file: %w", err)
		}
		if err := os.Rename(tmpPath, fullPath); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("rename temp to daily file: %w", err)
		}

	case "topic":
		dir := memoryDir
		relDir := ""
		if opts.ProjectSlug != "" {
			dir = filepath.Join(memoryDir, "projects", opts.ProjectSlug, "topics")
			relDir = filepath.Join("projects", opts.ProjectSlug, "topics")
		} else {
			dir = filepath.Join(memoryDir, "topics")
			relDir = "topics"
		}

		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create topics dir: %w", err)
		}

		filename := opts.TopicName + ".md"
		fullPath = filepath.Join(dir, filename)
		relPath = filepath.Join(relDir, filename)

		existing, err := os.ReadFile(fullPath)
		var data []byte
		if err == nil {
			data = append(existing, []byte("\n\n"+opts.Content)...)
		} else if errors.Is(err, os.ErrNotExist) {
			data = []byte(opts.Content)
		} else {
			return "", fmt.Errorf("read existing topic file: %w", err)
		}

		// Atomic write.
		tmpPath := fullPath + ".tmp"
		if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("write temp file: %w", err)
		}
		if err := os.Rename(tmpPath, fullPath); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("rename temp to topic file: %w", err)
		}

	}

	log.Printf("[Awareness] Memory written to %s", relPath)

	// Sync the updated file to the index.
	if s.indexer != nil {
		if err := s.indexer.Sync(ctx); err != nil {
			log.Printf("[Awareness] WARNING: index sync after memory write: %v", err)
		}
	}

	return relPath, nil
}

// --- core_memory_update ---

type coreMemoryUpdateArgs struct {
	File    string `json:"file"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

// allowedCoreMemoryFiles lists the core memory files that can be updated.
var allowedCoreMemoryFiles = map[string]bool{
	"SOUL.md":     true,
	"IDENTITY.md": true,
	"USER.md":     true,
	"GLOBAL.md":   true,
}

func coreMemoryUpdateSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:        "core_memory_update",
		Description: "Update a core memory file. SOUL.md defines personality, IDENTITY.md defines name and style, USER.md stores user profile, GLOBAL.md stores cross-project rules. Changes take effect immediately in the next AI interaction.",
		ParametersJSON: `{
  "type": "object",
  "properties": {
    "file": {"type": "string", "enum": ["SOUL.md", "IDENTITY.md", "USER.md", "GLOBAL.md"], "description": "Core memory file to update"},
    "content": {"type": "string", "description": "New content (markdown)"},
    "mode": {"type": "string", "enum": ["replace", "append"], "default": "append", "description": "Write mode: 'replace' overwrites entire file, 'append' adds to end"}
  },
  "required": ["file", "content"]
}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var a coreMemoryUpdateArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}

			mode := a.Mode
			if mode == "" {
				mode = "append"
			}

			if err := s.UpdateCoreMemory(ctx, a.File, a.Content, mode); err != nil {
				return nil, err
			}

			return json.Marshal(map[string]string{
				"status": "ok",
				"file":   a.File,
			})
		},
	}
}

// UpdateCoreMemory updates a core memory file and reloads the in-memory cache.
// fileName must be one of: SOUL.md, IDENTITY.md, USER.md, GLOBAL.md.
// mode must be "replace" or "append".
func (s *Service) UpdateCoreMemory(ctx context.Context, fileName, content, mode string) error {
	if !allowedCoreMemoryFiles[fileName] {
		return fmt.Errorf("invalid core memory file: %s (allowed: SOUL.md, IDENTITY.md, USER.md, GLOBAL.md)", fileName)
	}

	if mode != "replace" && mode != "append" {
		return fmt.Errorf("invalid mode: %s (allowed: replace, append)", mode)
	}

	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("content is required")
	}

	// Serialize read-modify-write for append mode. Uses a dedicated mutex
	// (not s.mu) to avoid deadlock with loadCoreMemory which also acquires s.mu.
	s.coreWriteMu.Lock()
	defer s.coreWriteMu.Unlock()

	filePath := filepath.Join(s.awarenessDir, fileName)

	var data []byte

	switch mode {
	case "replace":
		data = []byte(content)
	case "append":
		existing, err := os.ReadFile(filePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read existing file: %w", err)
		}
		if len(existing) > 0 {
			data = append(existing, []byte("\n\n"+content)...)
		} else {
			data = []byte(content)
		}
	}

	// Atomic write via tmp + rename.
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp to core memory file: %w", err)
	}

	log.Printf("[Awareness] Core memory updated: %s (mode=%s)", fileName, mode)

	// Reload the in-memory core memory cache so the next AI prompt gets updated content.
	// loadCoreMemory handles its own locking on s.mu.
	s.loadCoreMemory("")

	return nil
}

// --- onboarding_complete ---

func onboardingCompleteSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:           "onboarding_complete",
		Description:    "Mark onboarding as complete. Call this after you've gathered the user's preferences and updated IDENTITY.md, USER.md, and SOUL.md.",
		ParametersJSON: `{"type":"object","properties":{},"additionalProperties":false}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			removed, err := s.CompleteOnboarding()
			if err != nil {
				return nil, fmt.Errorf("onboarding_complete: %w", err)
			}
			if !removed {
				return json.Marshal(map[string]string{
					"status":  "ok",
					"message": "Onboarding was already complete.",
				})
			}
			return json.Marshal(map[string]string{
				"status":  "ok",
				"message": "Onboarding complete. BOOTSTRAP.md removed.",
			})
		},
	}
}
