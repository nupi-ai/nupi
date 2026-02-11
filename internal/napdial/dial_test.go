package napdial

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCert generates a self-signed cert and writes it to dir.
func writeTestCert(t *testing.T, dir string, isCA bool) (string, string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	if isCA {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func TestDialOptionsInsecure(t *testing.T) {
	opts, err := DialOptions(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("expected 1 option (insecure creds), got %d", len(opts))
	}
}

func TestDialOptionsTLS(t *testing.T) {
	dir := t.TempDir()
	caPath, _ := writeTestCert(t, dir, true)

	cfg := &TLSConfig{CACertPath: caPath}
	opts, err := DialOptions(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("expected 1 option (TLS creds), got %d", len(opts))
	}
}

func TestDialOptionsTLSWithClientCert(t *testing.T) {
	dir := t.TempDir()
	caPath, _ := writeTestCert(t, dir, true)

	clientDir := filepath.Join(dir, "client")
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	clientCert, clientKey := writeTestCert(t, clientDir, false)

	cfg := &TLSConfig{
		CertPath:   clientCert,
		KeyPath:    clientKey,
		CACertPath: caPath,
	}
	opts, err := DialOptions(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("expected 1 option, got %d", len(opts))
	}
}

func TestDialOptionsTLSInvalidCA(t *testing.T) {
	dir := t.TempDir()
	badCA := filepath.Join(dir, "bad-ca.pem")
	if err := os.WriteFile(badCA, []byte("not a cert"), 0o600); err != nil {
		t.Fatalf("write bad CA: %v", err)
	}

	cfg := &TLSConfig{CACertPath: badCA}
	_, err := DialOptions(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected error for invalid CA cert, got nil")
	}
}

func TestDialOptionsTLSPartialMTLS(t *testing.T) {
	cfg := &TLSConfig{CertPath: "/some/cert.pem"}
	_, err := DialOptions(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected error for CertPath without KeyPath, got nil")
	}

	cfg2 := &TLSConfig{KeyPath: "/some/key.pem"}
	_, err = DialOptions(context.Background(), cfg2, nil)
	if err == nil {
		t.Fatal("expected error for KeyPath without CertPath, got nil")
	}
}

func TestDialOptionsTLSMissingCA(t *testing.T) {
	cfg := &TLSConfig{CACertPath: "/nonexistent/ca.pem"}
	_, err := DialOptions(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected error for missing CA file, got nil")
	}
}

func TestDialOptionsKeepalive(t *testing.T) {
	qos := DefaultQoS()
	opts, err := DialOptions(context.Background(), nil, qos)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("expected 2 options (insecure + keepalive), got %d", len(opts))
	}
}

func TestDialOptionsContextDialer(t *testing.T) {
	customDialer := func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, nil
	}
	ctx := ContextWithDialer(context.Background(), customDialer)
	opts, err := DialOptions(ctx, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("expected 2 options (insecure + dialer), got %d", len(opts))
	}
}

func TestTLSConfigFromFieldsNil(t *testing.T) {
	cfg := TLSConfigFromFields("", "", "", false)
	if cfg != nil {
		t.Fatal("expected nil for all-empty fields")
	}
}

func TestTLSConfigFromFieldsNonNil(t *testing.T) {
	cfg := TLSConfigFromFields("/cert", "/key", "/ca", false)
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.CertPath != "/cert" {
		t.Fatalf("expected cert path /cert, got %s", cfg.CertPath)
	}
}

func TestTLSConfigFromFieldsInsecureOnly(t *testing.T) {
	cfg := TLSConfigFromFields("", "", "", true)
	if cfg == nil {
		t.Fatal("expected non-nil config for insecure=true")
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=true")
	}
}

func TestBuildTLSConfig(t *testing.T) {
	dir := t.TempDir()
	caPath, _ := writeTestCert(t, dir, true)

	cfg := &TLSConfig{
		CACertPath:         caPath,
		ServerName:         "test-server",
		InsecureSkipVerify: true,
	}

	tlsCfg, err := cfg.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.ServerName != "test-server" {
		t.Fatalf("expected ServerName test-server, got %s", tlsCfg.ServerName)
	}
	if !tlsCfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=true")
	}
	if tlsCfg.RootCAs == nil {
		t.Fatal("expected RootCAs to be populated")
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("expected MinVersion TLS 1.2 (%d), got %d", tls.VersionTLS12, tlsCfg.MinVersion)
	}
}

func TestBuildTLSConfigMissingCA(t *testing.T) {
	cfg := &TLSConfig{CACertPath: "/nonexistent/ca.pem"}
	_, err := cfg.buildTLSConfig()
	if err == nil {
		t.Fatal("expected error for missing CA path")
	}
}

func TestDefaultQoS(t *testing.T) {
	qos := DefaultQoS()
	if qos.KeepaliveTime != 30*time.Second {
		t.Fatalf("expected 30s keepalive time, got %v", qos.KeepaliveTime)
	}
	if qos.KeepaliveTimeout != 10*time.Second {
		t.Fatalf("expected 10s keepalive timeout, got %v", qos.KeepaliveTimeout)
	}
}
