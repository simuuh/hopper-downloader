package main

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProxyServer_LocalProbesAnswerWithoutTarget(t *testing.T) {
	p := newProxyServer("", discardLogger())
	srv := httptest.NewServer(p.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/flash/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "JDownloader") {
		t.Errorf("/flash/ body = %q, want it to mention JDownloader", body)
	}

	resp2, err := http.Get(srv.URL + "/jdcheck.js")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	body2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body2), "jdownloader=true") {
		t.Errorf("/jdcheck.js body = %q", body2)
	}
}

func TestProxyServer_ForwardsToTarget(t *testing.T) {
	var gotPath, gotBody, gotContentType string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
	}))
	defer target.Close()

	p := newProxyServer(target.URL, discardLogger())
	srv := httptest.NewServer(p.handler())
	defer srv.Close()

	form := url.Values{"urls": {"http://example.com/file"}}
	resp, err := http.Post(srv.URL+"/flash/add", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(body) != "success" {
		t.Errorf("body = %q", body)
	}
	if gotPath != "/flash/add" {
		t.Errorf("target saw path %q", gotPath)
	}
	if gotBody != form.Encode() {
		t.Errorf("target saw body %q, want %q", gotBody, form.Encode())
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("target saw Content-Type %q", gotContentType)
	}
}

func TestProxyServer_UnreachableTargetReturns503(t *testing.T) {
	// A closed listener address: nothing answers here.
	ln := httptest.NewServer(http.NotFoundHandler())
	deadURL := ln.URL
	ln.Close()

	p := newProxyServer(deadURL, discardLogger())
	srv := httptest.NewServer(p.handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/flash/add", "application/x-www-form-urlencoded", strings.NewReader("urls=x"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestProxyServer_NoTargetConfiguredReturns503(t *testing.T) {
	p := newProxyServer("", discardLogger())
	srv := httptest.NewServer(p.handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/flash/add", "application/x-www-form-urlencoded", strings.NewReader("urls=x"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func checkCORSHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := h.Get("Access-Control-Allow-Methods"); got != "POST, GET, OPTIONS" {
		t.Errorf("Access-Control-Allow-Methods = %q, want %q", got, "POST, GET, OPTIONS")
	}
	if got := h.Get("Access-Control-Allow-Headers"); got != "Content-Type, *" {
		t.Errorf("Access-Control-Allow-Headers = %q, want %q", got, "Content-Type, *")
	}
}

// TestProxyServer_CORSHeadersOnEveryResponse covers the actual bug: a
// hoster site's Click'n'Load button (e.g. hide.cx, served over HTTPS)
// POSTs directly from its own page's origin to this http://127.0.0.1:9666
// listener — cross-origin as far as browser CORS enforcement is
// concerned, regardless of this server only being reachable from
// 127.0.0.1 in the first place. Without these headers, the browser
// refuses to let the button's own JS read the response at all ("No
// 'Access-Control-Allow-Origin' header"), breaking the button even
// though the actual POST/queue action already succeeded server-side.
func TestProxyServer_CORSHeadersOnEveryResponse(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("success"))
	}))
	defer target.Close()

	p := newProxyServer(target.URL, discardLogger())
	srv := httptest.NewServer(p.handler())
	defer srv.Close()

	getResp, err := http.Get(srv.URL + "/flash/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = getResp.Body.Close() }()
	checkCORSHeaders(t, getResp.Header)

	postResp, err := http.Post(srv.URL+"/flash/add", "application/x-www-form-urlencoded", strings.NewReader("urls=x"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = postResp.Body.Close() }()
	checkCORSHeaders(t, postResp.Header)
}

// TestProxyServer_OPTIONSPreflight covers the preflight the browser sends
// ahead of hide.cx's actual POST — the button's request never reaches
// the real POST at all if this doesn't answer 200 with CORS headers and
// no body.
func TestProxyServer_OPTIONSPreflight(t *testing.T) {
	p := newProxyServer("http://unused.example", discardLogger())
	srv := httptest.NewServer(p.handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/flash/add", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	checkCORSHeaders(t, resp.Header)
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

// TestRunProxy_ServesPlainHTTPAlongsideHTTPS covers the actual reported
// bug: filecrypt.cc loads jdcheck.js via a plain
// <script src="http://127.0.0.1:9666/jdcheck.js"> tag, which a browser
// does not scheme-upgrade the way it does an HTTPS page's own fetch/XHR
// POST — so a TLS-only listener on the same port doesn't just refuse
// that request, Go's net/http actively answers it with a raw "400 Bad
// Request: Client sent an HTTP request to an HTTPS server" before any
// handler runs. Both plain HTTP and HTTPS must work on the exact same
// address, since real Click'n'Load buttons are hardcoded to
// 127.0.0.1:9666 with no way to configure a second port.
func TestRunProxy_ServesPlainHTTPAlongsideHTTPS(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("success"))
	}))
	defer target.Close()

	addr := freeLocalAddr(t)
	certDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runProxy(ctx, addr, target.URL, certDir, discardLogger())
	}()

	plainClient := &http.Client{Timeout: 2 * time.Second}
	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = plainClient.Get("http://" + addr + "/jdcheck.js") //nolint:noctx // test helper, bounded by the deadline loop
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET http://%s/jdcheck.js: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (plain HTTP request must not hit the TLS-only 400)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "jdownloader=true") {
		t.Errorf("body = %q, want it to mention jdownloader=true", body)
	}

	// GET /flash/ (the other CnL probe endpoint) must work identically on
	// the plain-HTTP leg — same dualProtocolListener routing /jdcheck.js
	// already proved, exercised here for a second endpoint since it's the
	// one filecrypt.cc/hide.cx button scripts actually detect a running
	// client with, not just jdcheck.js.
	flashResp, err := plainClient.Get("http://" + addr + "/flash/") //nolint:noctx // short-lived test call
	if err != nil {
		t.Fatalf("GET http://%s/flash/: %v", addr, err)
	}
	defer func() { _ = flashResp.Body.Close() }()
	if flashResp.StatusCode != http.StatusOK {
		t.Fatalf("/flash/ status = %d, want 200", flashResp.StatusCode)
	}
	flashBody, _ := io.ReadAll(flashResp.Body)
	if string(flashBody) != "JDownloader - Hopper" {
		t.Errorf("/flash/ body = %q, want %q", flashBody, "JDownloader - Hopper")
	}

	// The same address must still serve HTTPS correctly too.
	tlsClient := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // G402: test client deliberately trusts the throwaway self-signed cert under test
	}
	httpsResp, err := tlsClient.Get("https://" + addr + "/jdcheck.js") //nolint:noctx // short-lived test call
	if err != nil {
		t.Fatalf("GET https://%s/jdcheck.js: %v", addr, err)
	}
	defer func() { _ = httpsResp.Body.Close() }()
	if httpsResp.TLS == nil {
		t.Fatal("expected a TLS connection, got a plain one (response.TLS is nil)")
	}
	if httpsResp.StatusCode != http.StatusOK {
		t.Fatalf("https status = %d, want 200", httpsResp.StatusCode)
	}

	httpsFlashResp, err := tlsClient.Get("https://" + addr + "/flash/") //nolint:noctx // short-lived test call
	if err != nil {
		t.Fatalf("GET https://%s/flash/: %v", addr, err)
	}
	defer func() { _ = httpsFlashResp.Body.Close() }()
	if httpsFlashResp.StatusCode != http.StatusOK {
		t.Fatalf("https /flash/ status = %d, want 200", httpsFlashResp.StatusCode)
	}
	httpsFlashBody, _ := io.ReadAll(httpsFlashResp.Body)
	if string(httpsFlashBody) != "JDownloader - Hopper" {
		t.Errorf("https /flash/ body = %q, want %q", httpsFlashBody, "JDownloader - Hopper")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("runProxy returned an error after shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runProxy did not shut down within 2s of context cancellation")
	}
}

func TestJoinURL(t *testing.T) {
	cases := map[string]string{
		"http://host:9666":  "http://host:9666/flash/",
		"http://host:9666/": "http://host:9666/flash/",
	}
	for base, want := range cases {
		if got := joinURL(base, "/flash/"); got != want {
			t.Errorf("joinURL(%q, /flash/) = %q, want %q", base, got, want)
		}
	}
}

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestRunProxy_ServesHTTPS is the end-to-end check for the actual fix:
// an HTTPS hoster site's Click'n'Load button can't POST to a plain
// http://127.0.0.1:9666 target at all (browsers block it as mixed
// content) — this proxy has to actually terminate TLS, not just accept
// TLS-shaped configuration that goes unused.
func TestRunProxy_ServesHTTPS(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("success"))
	}))
	defer target.Close()

	addr := freeLocalAddr(t)
	certDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runProxy(ctx, addr, target.URL, certDir, discardLogger())
	}()

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // G402: test client deliberately trusts the throwaway self-signed cert under test
	}

	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = client.Get("https://" + addr + "/flash/") //nolint:noctx // test helper, bounded by the deadline loop
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET https://%s/flash/: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.TLS == nil {
		t.Fatal("expected a TLS connection, got a plain one (response.TLS is nil)")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "JDownloader") {
		t.Errorf("body = %q, want it to mention JDownloader", body)
	}

	if _, err := os.Stat(filepath.Join(certDir, certFileName)); err != nil {
		t.Errorf("expected ensureCert to have written a cert file: %v", err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("runProxy returned an error after shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runProxy did not shut down within 2s of context cancellation")
	}
}
