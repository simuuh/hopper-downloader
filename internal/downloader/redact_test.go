package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRedactURL(t *testing.T) {
	cases := map[string]string{ //nolint:gosec // G101: placeholder tokens, the point of a redaction test
		"https://cdn.example/d/SECRETTOKEN/file.part1.rar?sig=abc": "https://cdn.example/…/file.part1.rar",
		"https://user:pw@cdn.example/get?token=abc":                "https://cdn.example/…/get",
		"https://cdn.example/":                                     "https://cdn.example/…",
		"not a url":                                                "<url>",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A failed request must not put the direct link's token into the error,
// which ends up in the item, the Web UI and the logs (M19).
func TestDownload_ErrorDoesNotLeakDirectLinkToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drop the connection without an answer.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)

	e := New(Config{BaseDir: t.TempDir(), RetryMax: 0, RetryBackoffBase: time.Millisecond})
	link := srv.URL + "/d/SECRETPATHTOKEN/f.bin?token=SECRETQUERYTOKEN"
	_, err := e.Download(context.Background(), Request{URL: link, RelPath: "f.bin", ExpectedSize: 1024, ParallelChunks: 1})
	if err == nil {
		t.Fatal("Download succeeded against a server that drops every connection")
	}
	if msg := err.Error(); strings.Contains(msg, "SECRET") {
		t.Fatalf("error leaks the link's token: %s", msg)
	}
}

// Without an expected size the probe's failure is the error; it must be
// redacted too.
func TestDownload_ProbeErrorDoesNotLeakDirectLinkToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)

	e := New(Config{BaseDir: t.TempDir(), RetryMax: 0, RetryBackoffBase: time.Millisecond})
	link := srv.URL + "/d/SECRETPATHTOKEN/f.bin?token=SECRETQUERYTOKEN"
	_, err := e.Download(context.Background(), Request{URL: link, RelPath: "f.bin", ParallelChunks: 1})
	if err == nil {
		t.Fatal("Download succeeded against a server that drops every connection")
	}
	if msg := err.Error(); strings.Contains(msg, "SECRET") {
		t.Fatalf("error leaks the link's token: %s", msg)
	}
}
