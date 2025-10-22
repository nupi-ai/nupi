package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultGracePeriod = 15 * time.Second
)

// Config captures the inputs required to launch a module process.
type Config struct {
	Slot           string
	Adapter        string
	Command        string
	Args           []string
	ModuleConfig   string
	ModuleHome     string
	ModuleDataDir  string
	ManifestPath   string
	Transport      string
	Endpoint       string
	ListenAddrEnv  string
	ListenAddr     string
	GracePeriod    time.Duration
	RawEnvironment []string
}

// LoadConfigFromEnv reads adapter-runner configuration from environment variables.
func LoadConfigFromEnv() (Config, error) {
	env := os.Environ()
	get := func(key string) string {
		return strings.TrimSpace(os.Getenv(key))
	}

	cfg := Config{
		Slot:           get("NUPI_MODULE_SLOT"),
		Adapter:        get("NUPI_MODULE_ADAPTER"),
		Command:        get("NUPI_MODULE_COMMAND"),
		ModuleConfig:   get("NUPI_MODULE_CONFIG"),
		ModuleHome:     get("NUPI_MODULE_HOME"),
		ModuleDataDir:  get("NUPI_MODULE_DATA_DIR"),
		ManifestPath:   get("NUPI_MODULE_MANIFEST_PATH"),
		Transport:      get("NUPI_MODULE_TRANSPORT"),
		Endpoint:       get("NUPI_MODULE_ENDPOINT"),
		ListenAddrEnv:  get("NUPI_ADAPTER_LISTEN_ADDR_ENV"),
		ListenAddr:     get("NUPI_ADAPTER_LISTEN_ADDR"),
		GracePeriod:    defaultGracePeriod,
		RawEnvironment: env,
	}

	if graceRaw := get("NUPI_MODULE_SHUTDOWN_GRACE"); graceRaw != "" {
		if duration, err := time.ParseDuration(graceRaw); err == nil && duration > 0 {
			cfg.GracePeriod = duration
		}
	}

	// Fallback for legacy naming.
	argsJSON := get("NUPI_MODULE_ARGS")
	if argsJSON == "" {
		argsJSON = get("NUPI_MODULE_ARGS_JSON")
	}
	if argsJSON != "" {
		var args []string
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return Config{}, fmt.Errorf("adapter-runner: parse NUPI_MODULE_ARGS: %w", err)
		}
		cfg.Args = args
	}

	if cfg.ListenAddrEnv == "" {
		cfg.ListenAddrEnv = "NUPI_ADAPTER_LISTEN_ADDR"
	}

	if err := cfg.validateAndResolve(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c *Config) validateAndResolve() error {
	if strings.TrimSpace(c.Command) == "" {
		return errors.New("adapter-runner: NUPI_MODULE_COMMAND is required")
	}

	// Resolve relative command path.
	if !filepath.IsAbs(c.Command) {
		base := strings.TrimSpace(c.ModuleHome)
		if base == "" {
			base, _ = os.Getwd()
		}
		c.Command = filepath.Clean(filepath.Join(base, c.Command))
	}

	absCommand, err := filepath.Abs(c.Command)
	if err != nil {
		return fmt.Errorf("adapter-runner: resolve command path: %w", err)
	}
	c.Command = absCommand

	// Ensure module home is absolute if provided.
	if c.ModuleHome != "" {
		if !filepath.IsAbs(c.ModuleHome) {
			homeAbs, err := filepath.Abs(c.ModuleHome)
			if err != nil {
				return fmt.Errorf("adapter-runner: resolve module home: %w", err)
			}
			c.ModuleHome = homeAbs
		}
	}

	// Resolve data dir relative to module home if necessary.
	if c.ModuleDataDir != "" && !filepath.IsAbs(c.ModuleDataDir) {
		base := c.ModuleHome
		if base == "" {
			base, _ = os.Getwd()
		}
		dataAbs, err := filepath.Abs(filepath.Join(base, c.ModuleDataDir))
		if err != nil {
			return fmt.Errorf("adapter-runner: resolve module data dir: %w", err)
		}
		c.ModuleDataDir = dataAbs
	}

	return nil
}

// DefaultStopSignal returns the recommended termination signal for the platform.
func DefaultStopSignal() os.Signal {
	if runtime.GOOS == "windows" {
		return os.Interrupt
	}
	return syscallSIGTERM()
}
