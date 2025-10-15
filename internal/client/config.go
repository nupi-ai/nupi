package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/nupi-ai/nupi/internal/bootstrap"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// TLSExplicitOptions drives TLS configuration when the client is configured
// explicitly via URL (environment variables, bootstrap file, etc).
type TLSExplicitOptions struct {
	Insecure   bool
	CACertPath string
	ServerName string
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

// DetermineHost returns the host to use for HTTP/gRPC connections based on binding.
func DetermineHost(binding string, tlsEnabled bool) string {
	switch strings.TrimSpace(strings.ToLower(binding)) {
	case "", "loopback":
		if tlsEnabled {
			return "localhost"
		}
		return "127.0.0.1"
	default:
		if tlsEnabled {
			return "localhost"
		}
		return "127.0.0.1"
	}
}

// PrepareTLSConfig builds a tls.Config using the transport configuration.
func PrepareTLSConfig(cfg configstore.TransportConfig, host string, enabled bool) (*tls.Config, error) {
	if strings.TrimSpace(os.Getenv("NUPI_TLS_INSECURE")) == "1" {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}

	if !enabled {
		return nil, nil
	}

	certPath := strings.TrimSpace(cfg.TLSCertPath)
	if certPath == "" {
		return nil, fmt.Errorf("client: TLS certificate path not configured")
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("client: read TLS certificate: %w", err)
	}

	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if ok := roots.AppendCertsFromPEM(certPEM); !ok {
		return nil, fmt.Errorf("client: failed to append TLS certificate")
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
	}, nil
}

// TLSConfigForExplicit mirrors TLS configuration when base URL is provided via env/bootstrap.
func TLSConfigForExplicit(u *url.URL, opts *TLSExplicitOptions) (*tls.Config, error) {
	if strings.EqualFold(u.Scheme, "https") == false {
		return nil, nil
	}

	if opts == nil {
		return nil, nil
	}

	if opts.Insecure {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}

	var roots *x509.CertPool
	if opts.CACertPath != "" {
		data, err := os.ReadFile(opts.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("client: read TLS CA cert: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("client: parse TLS CA cert: %s", opts.CACertPath)
		}
	}

	cfg := &tls.Config{}
	if roots != nil {
		cfg.RootCAs = roots
	}
	if opts.ServerName != "" {
		cfg.ServerName = opts.ServerName
	}

	return cfg, nil
}
