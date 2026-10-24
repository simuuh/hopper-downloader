package api

import (
	"context"
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
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSignedCert writes a throwaway certificate for 127.0.0.1 and
// its key to dir and returns their paths.
func writeSelfSignedCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hopper test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// With tls_cert_file/tls_key_file set the listener speaks HTTPS (M5);
// the settings used to be accepted and silently ignored.
func TestListenAndServe_ServesHTTPSWithCertificate(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t, t.TempDir())
	port := freePort(t)
	srv, _ := newTestServer(t, func(c *Config) {
		c.Port = port
		c.TLSCertFile, c.TLSKeyFile = certFile, keyFile
	})
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		<-errc
	})

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // G402: self-signed test certificate
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/healthz", port)
	var resp *http.Response
	var err error
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if resp, err = client.Get(url); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil {
		t.Fatalf("status %d, TLS %v; want 200 over TLS", resp.StatusCode, resp.TLS != nil)
	}
}
