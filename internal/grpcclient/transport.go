package grpcclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/tlswarn"
)

// TLSExplicitOptions drives TLS configuration when the client is configured
// explicitly via URL (environment variables, bootstrap file, etc).
type TLSExplicitOptions struct {
	Insecure   bool
	CACertPath string
	ServerName string
}

// LoadTransportSettings loads transport config and API tokens from the config store.
func LoadTransportSettings() (configstore.TransportConfig, []string, error) {
	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
		ReadOnly:     true,
	})
	if err != nil {
		return configstore.TransportConfig{}, nil, fmt.Errorf("grpcclient: open config store: %w", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), constants.GRPCClientConfigLoadTimeout)
	defer cancel()

	cfg, err := store.GetTransportConfig(ctx)
	if err != nil {
		return configstore.TransportConfig{}, nil, fmt.Errorf("grpcclient: load transport config: %w", err)
	}

	security, err := store.LoadSecuritySettings(ctx, "auth.http_tokens")
	if err != nil {
		return cfg, nil, fmt.Errorf("grpcclient: load security settings: %w", err)
	}

	tokens := parseTokens(security["auth.http_tokens"])
	sort.Strings(tokens) // deterministic order — matches Rust client behaviour
	return cfg, tokens, nil
}

// ResolveExplicitOptions merges environment variables and bootstrap data.
func ResolveExplicitOptions(boot *bootstrap.Config) (string, TLSExplicitOptions) {
	token := strings.TrimSpace(os.Getenv("NUPI_API_TOKEN"))

	opts := TLSExplicitOptions{
		Insecure:   strings.TrimSpace(os.Getenv("NUPI_TLS_INSECURE")) == "1",
		CACertPath: strings.TrimSpace(os.Getenv("NUPI_TLS_CA_CERT")),
		ServerName: strings.TrimSpace(os.Getenv("NUPI_TLS_SERVER_NAME")),
	}

	if boot != nil {
		if token == "" {
			token = strings.TrimSpace(boot.APIToken)
		}
		if boot.TLS != nil {
			if !opts.Insecure && boot.TLS.Insecure {
				opts.Insecure = true
			}
			if opts.CACertPath == "" {
				opts.CACertPath = strings.TrimSpace(boot.TLS.CACertPath)
			}
			if opts.ServerName == "" {
				opts.ServerName = strings.TrimSpace(boot.TLS.ServerName)
			}
		}
	}

	return token, opts
}

// DetermineHost returns the host to use for gRPC connections. Local clients
// always connect to loopback; TLS requires "localhost" for certificate
// hostname validation.
func DetermineHost(tlsEnabled bool) string {
	if tlsEnabled {
		return "localhost"
	}
	return "127.0.0.1"
}

// PrepareTLSConfig builds a tls.Config using the transport configuration.
func PrepareTLSConfig(cfg configstore.TransportConfig, host string, enabled bool) (*tls.Config, error) {
	if !enabled {
		return nil, nil
	}

	if strings.TrimSpace(os.Getenv("NUPI_TLS_INSECURE")) == "1" {
		tlswarn.LogInsecure()
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil //nolint:gosec // dev-only env override
	}

	certPath := strings.TrimSpace(cfg.TLSCertPath)
	if certPath == "" {
		return nil, fmt.Errorf("grpcclient: TLS certificate path not configured")
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: read TLS certificate: %w", err)
	}

	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if ok := roots.AppendCertsFromPEM(certPEM); !ok {
		return nil, fmt.Errorf("grpcclient: failed to append TLS certificate")
	}

	serverName := strings.TrimSpace(os.Getenv("NUPI_TLS_SERVER_NAME"))
	if serverName == "" {
		if net.ParseIP(host) != nil {
			serverName = "localhost"
		} else {
			serverName = host
		}
	}

	return &tls.Config{
		RootCAs:    roots,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// TLSConfigForExplicit mirrors TLS configuration when base URL is provided via env/bootstrap.
func TLSConfigForExplicit(u *url.URL, opts *TLSExplicitOptions) (*tls.Config, error) {
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, nil
	}

	if opts == nil {
		return nil, nil
	}

	if opts.Insecure {
		tlswarn.LogInsecure()
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil //nolint:gosec // user explicitly requested insecure
	}

	var roots *x509.CertPool
	if opts.CACertPath != "" {
		data, err := os.ReadFile(opts.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("grpcclient: read TLS CA cert: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("grpcclient: parse TLS CA cert: %s", opts.CACertPath)
		}
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if roots != nil {
		cfg.RootCAs = roots
	}
	if opts.ServerName != "" {
		cfg.ServerName = opts.ServerName
	}

	return cfg, nil
}

func parseTokens(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var simple []string
	if err := json.Unmarshal([]byte(raw), &simple); err == nil {
		return sanitizeTokenList(simple)
	}

	var structured []map[string]any
	if err := json.Unmarshal([]byte(raw), &structured); err == nil {
		collected := make([]string, 0, len(structured))
		for _, entry := range structured {
			if token, ok := entry["token"].(string); ok {
				token = strings.TrimSpace(token)
				if token != "" {
					collected = append(collected, token)
				}
			}
		}
		return sanitizeTokenList(collected)
	}
	return nil
}

func sanitizeTokenList(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}
