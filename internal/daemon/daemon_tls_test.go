package daemon

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/server"
)

func TestValidateTLSAssets(t *testing.T) {
	t.Run("no tls configured", func(t *testing.T) {
		if err := validateTLSAssets(server.TransportSnapshot{}); err != nil {
			t.Fatalf("expected nil error for empty TLS config, got %v", err)
		}
	})

	t.Run("missing key path", func(t *testing.T) {
		snap := server.TransportSnapshot{TLSCertPath: "/tmp/cert.pem"}
		if err := validateTLSAssets(snap); err == nil {
			t.Fatalf("expected error for missing key path")
		}
	})

	t.Run("valid pair", func(t *testing.T) {
		dir := t.TempDir()
		certPath, keyPath := writeTestCertificatePair(t, dir)

		snap := server.TransportSnapshot{
			TLSCertPath: certPath,
			TLSKeyPath:  keyPath,
		}

		if err := validateTLSAssets(snap); err != nil {
			t.Fatalf("expected valid TLS assets, got %v", err)
		}

		if err := os.WriteFile(certPath, []byte("corrupted"), 0o600); err != nil {
			t.Fatalf("failed to corrupt certificate: %v", err)
		}
		if err := validateTLSAssets(snap); err == nil {
			t.Fatalf("expected error for corrupted certificate")
		}
	})
}

func writeTestCertificatePair(t *testing.T, dir string) (string, string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("failed to write certificate file: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}

	return certPath, keyPath
}
