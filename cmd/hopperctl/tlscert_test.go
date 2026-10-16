package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type expiredTestCert struct {
	cert    tls.Certificate
	certPEM []byte
	keyPEM  []byte
}

// certTemplateExpiredForTest builds a self-signed ECDSA cert/key pair
// whose NotAfter is already in the past, for exercising
// loadValidCert/ensureCert's expiry-rejection path without waiting ~10
// years for a real one to expire.
func certTemplateExpiredForTest(t *testing.T) expiredTestCert {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              time.Now().Add(-24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return expiredTestCert{cert: cert, certPEM: certPEM, keyPEM: keyPEM}
}

func TestGenerateCert_CoversLocalhostAnd127001(t *testing.T) {
	cert, certPEM, keyPEM, err := generateCert()
	if err != nil {
		t.Fatalf("generateCert: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("expected non-empty PEM output")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}

	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("PublicKey type = %T, want *ecdsa.PublicKey", leaf.PublicKey)
	}

	if got := leaf.VerifyHostname("localhost"); got != nil {
		t.Errorf("VerifyHostname(localhost) = %v, want nil", got)
	}
	if got := leaf.VerifyHostname("127.0.0.1"); got != nil {
		t.Errorf("VerifyHostname(127.0.0.1) = %v, want nil", got)
	}

	wantValidity := 10 * 365 * 24 * time.Hour
	gotValidity := leaf.NotAfter.Sub(leaf.NotBefore)
	// NotBefore is backdated by an hour for clock-skew tolerance (see
	// generateCert), so the total span is validity + that hour, not
	// exactly validity.
	if diff := gotValidity - wantValidity - time.Hour; diff < -time.Minute || diff > time.Minute {
		t.Errorf("validity span = %v, want ~%v", gotValidity, wantValidity+time.Hour)
	}
	if time.Now().After(leaf.NotAfter) {
		t.Error("generated certificate is already expired")
	}
}

func TestEnsureCert_GeneratesOnFirstCall(t *testing.T) {
	dir := t.TempDir()

	cert, err := ensureCert(dir)
	if err != nil {
		t.Fatalf("ensureCert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected a non-empty certificate chain")
	}

	if _, err := os.Stat(filepath.Join(dir, certFileName)); err != nil {
		t.Errorf("cert file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Errorf("key file not written: %v", err)
	}
}

func TestEnsureCert_ReusesExistingValidCert(t *testing.T) {
	dir := t.TempDir()

	first, err := ensureCert(dir)
	if err != nil {
		t.Fatalf("ensureCert (first): %v", err)
	}
	second, err := ensureCert(dir)
	if err != nil {
		t.Fatalf("ensureCert (second): %v", err)
	}

	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Error("expected the second call to reuse the persisted certificate, got a different one")
	}
}

func TestEnsureCert_RegeneratesWhenExpired(t *testing.T) {
	dir := t.TempDir()

	// Build an already-expired cert/key pair directly (bypassing
	// generateCert's fixed ~10y validity) and write it where ensureCert
	// expects to find one.
	expired := certTemplateExpiredForTest(t)
	if err := os.WriteFile(filepath.Join(dir, certFileName), expired.certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFileName), expired.keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	cert, err := ensureCert(dir)
	if err != nil {
		t.Fatalf("ensureCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse resulting certificate: %v", err)
	}
	if time.Now().After(leaf.NotAfter) {
		t.Error("ensureCert returned an expired certificate instead of regenerating")
	}
	if string(cert.Certificate[0]) == string(expired.cert.Certificate[0]) {
		t.Error("expected a freshly generated certificate, got the same expired one back")
	}
}

func TestLoadValidCert_RejectsExpired(t *testing.T) {
	dir := t.TempDir()
	expired := certTemplateExpiredForTest(t)
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, expired.certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, expired.keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadValidCert(certPath, keyPath); err == nil {
		t.Error("expected an error for an expired certificate")
	}
}

func TestLoadValidCert_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadValidCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")); err == nil {
		t.Error("expected an error for missing cert/key files")
	}
}
