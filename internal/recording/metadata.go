package recording

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Metadata represents metadata for a single recording.
type Metadata struct {
	SessionID     string    `json:"session_id"`
	Filename      string    `json:"filename"`
	Command       string    `json:"command"`
	Args          []string  `json:"args,omitempty"`
	WorkDir       string    `json:"work_dir,omitempty"`
	StartTime     time.Time `json:"start_time"`
	Duration      float64   `json:"duration"` // in seconds
	Rows          int       `json:"rows"`
	Cols          int       `json:"cols"`
	Title         string    `json:"title"`
	Tool          string    `json:"tool,omitempty"` // Detected AI tool
	RecordingPath string    `json:"recording_path"` // Absolute path to .cast file
}

// Store manages recording metadata.
type Store struct {
	recordingsDir string
	metadataFile  string
}

// NewStore creates a new metadata store.
func NewStore() (*Store, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	recordingsDir := filepath.Join(homeDir, ".nupi", "recordings")
	metadataFile := filepath.Join(recordingsDir, "metadata.json")

	if err := os.MkdirAll(recordingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create recordings directory: %w", err)
	}

	return &Store{
		recordingsDir: recordingsDir,
		metadataFile:  metadataFile,
	}, nil
}

// SaveMetadata saves metadata for a recording.
func (s *Store) SaveMetadata(meta Metadata) error {
	metadata, err := s.LoadAll()
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to load existing metadata: %w", err)
	}

	found := false
	for i, m := range metadata {
		if m.SessionID == meta.SessionID {
			metadata[i] = meta
			found = true
			break
		}
	}
	if !found {
		metadata = append(metadata, meta)
	}

	sort.Slice(metadata, func(i, j int) bool {
		return metadata[i].StartTime.After(metadata[j].StartTime)
	})

	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(s.metadataFile, data, 0o644); err != nil {
		return fmt.Errorf("failed to write metadata file: %w", err)
	}

	return nil
}

// LoadAll loads all recording metadata.
func (s *Store) LoadAll() ([]Metadata, error) {
	data, err := os.ReadFile(s.metadataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return []Metadata{}, nil
		}
		return nil, fmt.Errorf("failed to read metadata file: %w", err)
	}

	var metadata []Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	return metadata, nil
}

// GetBySessionID returns metadata for a specific session.
func (s *Store) GetBySessionID(sessionID string) (*Metadata, error) {
	metadata, err := s.LoadAll()
	if err != nil {
		return nil, err
	}

	for _, m := range metadata {
		if m.SessionID == sessionID {
			return &m, nil
		}
	}

	return nil, fmt.Errorf("metadata not found for session %s", sessionID)
}

// Delete removes metadata for a session.
func (s *Store) Delete(sessionID string) error {
	metadata, err := s.LoadAll()
	if err != nil {
		return err
	}

	filtered := make([]Metadata, 0, len(metadata))
	for _, m := range metadata {
		if m.SessionID != sessionID {
			filtered = append(filtered, m)
		}
	}

	data, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	return os.WriteFile(s.metadataFile, data, 0o644)
}

// ScanRecordings scans the recordings directory and returns all .cast files.
func (s *Store) ScanRecordings() ([]string, error) {
	entries, err := os.ReadDir(s.recordingsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read recordings directory: %w", err)
	}

	recordings := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".cast") {
			recordings = append(recordings, filepath.Join(s.recordingsDir, entry.Name()))
		}
	}

	return recordings, nil
}

// GetRecordingsDir returns the recordings directory path.
func (s *Store) GetRecordingsDir() string {
	return s.recordingsDir
}
