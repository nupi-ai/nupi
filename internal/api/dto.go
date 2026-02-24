package api

import (
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/nupi-ai/nupi/internal/session"
)

// SessionDTO is the data transfer object for Session API responses
// It exposes only the fields that should be visible to API clients
type SessionDTO struct {
	ID           string    `json:"id"`
	Command      string    `json:"command"`
	Args         []string  `json:"args,omitempty"`
	StartTime    time.Time `json:"start_time"`
	Status       string    `json:"status"`
	PID          int       `json:"pid"`
	ExitCode     int       `json:"exit_code,omitempty"`
	WorkDir      string    `json:"work_dir,omitempty"`
	Tool         string    `json:"tool,omitempty"`           // Detected AI tool
	ToolIcon     string    `json:"tool_icon,omitempty"`      // Icon filename for detected tool
	ToolIconData string    `json:"tool_icon_data,omitempty"` // Base64 encoded icon data
	Mode         string    `json:"mode,omitempty"`
}

// ToDTO converts a Session to its DTO representation
func ToDTO(s *session.Session) SessionDTO {
	dto := SessionDTO{
		ID:        s.ID,
		Command:   s.Command,
		Args:      s.Args,
		WorkDir:   s.WorkDir,
		StartTime: s.StartTime,
		Status:    string(s.CurrentStatus()),
	}

	// Add PTY-specific fields if available
	if s.PTY != nil {
		dto.PID = s.PTY.GetPID()

		// Add exit code if process has exited
		if exitCode, err := s.PTY.GetExitCode(); err == nil && exitCode != -1 {
			dto.ExitCode = exitCode
		}
	}

	// Add detected tool if available
	if tool := s.GetDetectedTool(); tool != "" {
		dto.Tool = tool
		// Also add icon if available
		if icon := s.GetDetectedIcon(); icon != "" {
			dto.ToolIcon = icon
			// Load and encode icon to base64
			dto.ToolIconData = loadIconAsBase64(icon)
		}
	}

	return dto
}

// ToDTOList converts a slice of Sessions to DTOs
func ToDTOList(sessions []*session.Session) []SessionDTO {
	dtos := make([]SessionDTO, len(sessions))
	for i, s := range sessions {
		dtos[i] = ToDTO(s)
	}
	return dtos
}

// loadIconAsBase64 reads an icon file and returns its base64 encoded content
func loadIconAsBase64(filename string) string {
	homeDir, _ := os.UserHomeDir()
	iconPath := filepath.Join(homeDir, ".nupi", "instances", "default", "plugins", "icons", filename)

	data, err := os.ReadFile(iconPath)
	if err != nil {
		log.Printf("[DTO] Failed to read icon %s: %v", filename, err)
		return ""
	}

	return base64.StdEncoding.EncodeToString(data)
}
