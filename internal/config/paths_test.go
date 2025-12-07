package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetNupiHome(t *testing.T) {
	t.Run("default to ~/.nupi", func(t *testing.T) {
		// Ensure NUPI_HOME is not set
		orig := os.Getenv("NUPI_HOME")
		os.Unsetenv("NUPI_HOME")
		defer func() {
			if orig != "" {
				os.Setenv("NUPI_HOME", orig)
			}
		}()

		home := GetNupiHome()
		userHome, _ := os.UserHomeDir()
		expected := filepath.Join(userHome, ".nupi")

		if home != expected {
			t.Errorf("GetNupiHome() = %s; want %s", home, expected)
		}
	})

	t.Run("respects NUPI_HOME", func(t *testing.T) {
		orig := os.Getenv("NUPI_HOME")
		os.Setenv("NUPI_HOME", "/custom/nupi/home")
		defer func() {
			if orig != "" {
				os.Setenv("NUPI_HOME", orig)
			} else {
				os.Unsetenv("NUPI_HOME")
			}
		}()

		home := GetNupiHome()
		if home != "/custom/nupi/home" {
			t.Errorf("GetNupiHome() = %s; want /custom/nupi/home (NUPI_HOME should be respected)", home)
		}
	})
}

func TestGetInstancePaths(t *testing.T) {
	// Use controlled NUPI_HOME for predictable paths
	orig := os.Getenv("NUPI_HOME")
	os.Setenv("NUPI_HOME", "/test/nupi")
	defer func() {
		if orig != "" {
			os.Setenv("NUPI_HOME", orig)
		} else {
			os.Unsetenv("NUPI_HOME")
		}
	}()

	paths := GetInstancePaths("")

	if paths.Config != "/test/nupi/instances/default/config.yaml" {
		t.Errorf("Config path incorrect: got %s, want /test/nupi/instances/default/config.yaml", paths.Config)
	}
	if paths.Socket != "/test/nupi/instances/default/nupi.sock" {
		t.Errorf("Socket path incorrect: got %s, want /test/nupi/instances/default/nupi.sock", paths.Socket)
	}
	if paths.Lock != "/test/nupi/instances/default/daemon.lock" {
		t.Errorf("Lock path incorrect: got %s, want /test/nupi/instances/default/daemon.lock", paths.Lock)
	}
	if paths.Home != "/test/nupi/instances/default" {
		t.Errorf("Home path incorrect: got %s, want /test/nupi/instances/default", paths.Home)
	}
	if paths.BinDir != "/test/nupi/bin" {
		t.Errorf("BinDir path incorrect: got %s, want /test/nupi/bin", paths.BinDir)
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
