package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	certFileName = "hopperctl-cert.pem"
	keyFileName  = "hopperctl-key.pem"
	certValidity = 10 * 365 * 24 * time.Hour // ~10 years
)

// certDir returns the directory hopperctl's self-signed TLS certificate
// is stored in — the same directory as hopperctl.yaml (see
// ctlConfigPath), so there's exactly one "where does hopperctl keep its
// state" location to document, not two.
func certDir() (string, error) {
	path, err := ctlConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// ensureCert loads a persisted self-signed TLS certificate from dir
// (certFileName/keyFileName), generating and persisting a fresh one if
// it's missing or expired.
//
// Why the proxy needs TLS at all: an HTTPS hoster site's Click'n'Load
// button (link protectors such as filecrypt.cc or hide.cx) POSTs directly from
// its own page origin to http://127.0.0.1:9666 — a browser refuses that
// outright as mixed content once the button's own page is HTTPS
// (essentially all of them are), regardless of CORS headers or this
// listener being loopback-only. Serving HTTPS here fixes it generically
// for every current and future HTTPS hoster site, not just one.
func ensureCert(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	if cert, err := loadValidCert(certPath, keyPath); err == nil {
		return cert, nil
	}

	cert, certPEM, keyPEM, err := generateCert()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate TLS certificate: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("create %s: %w", dir, err)
	}
	// The certificate's public half isn't sensitive, but 0o600 costs
	// nothing extra and keeps both files under the same permissive-to-
	// nobody-else policy as hopperctl.yaml/the session file.
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write %s: %w", keyPath, err)
	}
	return cert, nil
}

// loadValidCert loads an existing cert/key pair from disk and rejects it
// (as an error, so ensureCert regenerates) if it has expired. A
// self-signed cert has no CA to renew it automatically — without this
// check, an expired one would just keep being reused forever, silently
// failing every TLS handshake instead of being replaced.
func loadValidCert(certPath, keyPath string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse existing certificate: %w", err)
	}
	if time.Now().After(leaf.NotAfter) {
		return tls.Certificate{}, fmt.Errorf("certificate expired at %s", leaf.NotAfter)
	}
	return cert, nil
}

// generateCert creates a new self-signed certificate covering exactly
// the two hosts a hoster site's Click'n'Load button ever addresses this
// proxy as — "localhost" and 127.0.0.1 — valid for ~10 years. ECDSA
// P-256 rather than RSA: smaller and faster to generate and handshake,
// and nothing here needs RSA's wider legacy-client compatibility — this
// certificate is never presented to anything but a local, modern
// browser.
func generateCert() (cert tls.Certificate, certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             now.Add(-time.Hour), // tolerate minor clock skew
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	return cert, certPEM, keyPEM, nil
}
