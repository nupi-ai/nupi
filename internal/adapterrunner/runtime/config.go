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

// Config captures the inputs required to launch an adapter process.
type Config struct {
	Slot           string
	Adapter        string
	Command        string
	Args           []string
	AdapterConfig  string
	AdapterHome    string
	AdapterDataDir string
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
		Slot:           get("NUPI_ADAPTER_SLOT"),
		Adapter:        get("NUPI_ADAPTER_ID"),
		Command:        get("NUPI_ADAPTER_COMMAND"),
		AdapterConfig:  get("NUPI_ADAPTER_CONFIG"),
		AdapterHome:    get("NUPI_ADAPTER_HOME"),
		AdapterDataDir: get("NUPI_ADAPTER_DATA_DIR"),
		ManifestPath:   get("NUPI_ADAPTER_MANIFEST_PATH"),
		Transport:      get("NUPI_ADAPTER_TRANSPORT"),
		Endpoint:       get("NUPI_ADAPTER_ENDPOINT"),
		ListenAddrEnv:  get("NUPI_ADAPTER_LISTEN_ADDR_ENV"),
		ListenAddr:     get("NUPI_ADAPTER_LISTEN_ADDR"),
		GracePeriod:    defaultGracePeriod,
		RawEnvironment: env,
	}

	if graceRaw := get("NUPI_ADAPTER_SHUTDOWN_GRACE"); graceRaw != "" {
		if duration, err := time.ParseDuration(graceRaw); err == nil && duration > 0 {
			cfg.GracePeriod = duration
		}
	}

	argsJSON := get("NUPI_ADAPTER_ARGS")
	if argsJSON == "" {
		argsJSON = get("NUPI_ADAPTER_ARGS_JSON")
	}
	if argsJSON != "" {
		var args []string
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return Config{}, fmt.Errorf("adapter-runner: parse NUPI_ADAPTER_ARGS: %w", err)
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
		return errors.New("adapter-runner: NUPI_ADAPTER_COMMAND is required")
	}

	// Resolve relative command path.
	if !filepath.IsAbs(c.Command) {
		base := strings.TrimSpace(c.AdapterHome)
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

	// Ensure adapter home is absolute if provided.
	if c.AdapterHome != "" {
		if !filepath.IsAbs(c.AdapterHome) {
			homeAbs, err := filepath.Abs(c.AdapterHome)
			if err != nil {
				return fmt.Errorf("adapter-runner: resolve adapter home: %w", err)
			}
			c.AdapterHome = homeAbs
		}
	}

	// Resolve data dir relative to adapter home if necessary.
	if c.AdapterDataDir != "" && !filepath.IsAbs(c.AdapterDataDir) {
		base := c.AdapterHome
		if base == "" {
			base, _ = os.Getwd()
		}
		dataAbs, err := filepath.Abs(filepath.Join(base, c.AdapterDataDir))
		if err != nil {
			return fmt.Errorf("adapter-runner: resolve adapter data dir: %w", err)
		}
		c.AdapterDataDir = dataAbs
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
