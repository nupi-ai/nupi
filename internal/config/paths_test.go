package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetNupiHome(t *testing.T) {
	os.Setenv("NUPI_HOME", "/tmp/should-be-ignored")
	defer os.Unsetenv("NUPI_HOME")

	home := GetNupiHome()

	userHome, _ := os.UserHomeDir()
	expected := filepath.Join(userHome, ".nupi")

	if home != expected {
		t.Errorf("GetNupiHome() = %s; want %s (NUPI_HOME should be ignored)", home, expected)
	}
}

func TestGetInstancePaths(t *testing.T) {
	paths := GetInstancePaths("")

	if !strings.Contains(paths.Config, "instances/default/config.yaml") {
		t.Errorf("Config path incorrect: %s", paths.Config)
	}
	if !strings.Contains(paths.Socket, "instances/default/nupi.sock") {
		t.Errorf("Socket path incorrect: %s", paths.Socket)
	}
	if !strings.Contains(paths.Lock, "instances/default/daemon.lock") {
		t.Errorf("Lock path incorrect: %s", paths.Lock)
	}
	if !strings.Contains(paths.Home, "instances/default") {
		t.Errorf("Home path incorrect: %s", paths.Home)
	}
	if !strings.Contains(paths.BinDir, ".nupi/bin") {
		t.Errorf("BinDir path incorrect: %s", paths.BinDir)
	}
	if !strings.Contains(paths.RunnerRoot, "bin/adapter-runner") {
		t.Errorf("RunnerRoot path incorrect: %s", paths.RunnerRoot)
	}
}

func TestGetInstancePathsAlwaysUsesDefault(t *testing.T) {
	paths1 := GetInstancePaths("")
	paths2 := GetInstancePaths("default")
	paths3 := GetInstancePaths("custom")

	if paths1.Config != paths2.Config {
		t.Error("Empty string and 'default' should give same paths")
	}

	if paths1.Config != paths3.Config {
		t.Log("Custom instance names will be supported in future versions")
	}
}

func TestExpandPath(t *testing.T) {
	tests := []struct {
		input    string
		contains string
	}{
		{"~/test", "/test"},
		{"~", ""},
		{"/absolute/path", "/absolute/path"},
		{"", ""},
	}

	for _, tt := range tests {
		result := ExpandPath(tt.input)
		if tt.input == "~" {
			home, _ := os.UserHomeDir()
			if result != home {
				t.Errorf("ExpandPath(%q) = %q; want home directory", tt.input, result)
			}
		} else if tt.input != "" && !strings.Contains(result, tt.contains) {
			t.Errorf("ExpandPath(%q) = %q; should contain %q", tt.input, result, tt.contains)
		}
	}
}
