package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// statusServer answers HEAD with the payload's size and every GET with
// status until okAfter GETs have been answered, then with the whole
// payload (a 200, fine for a single chunk starting at byte 0).
func statusServer(t *testing.T, payload []byte, status, okAfter int, header http.Header) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		if r.Method == http.MethodHead {
			// Range support on HEAD keeps the probe from spending a GET.
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		if int(gets.Add(1)) <= okAfter {
			for k, v := range header {
				w.Header()[k] = v
			}
			w.Header().Del("Content-Length")
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// A 404 (or 403, 410) for a link won't change by asking again: one
// request, no in-download retries (N6).
func TestDownload_ClientErrorIsNotRetried(t *testing.T) {
	srv, gets := statusServer(t, testPayload(1024), http.StatusNotFound, 100, nil)
	e := New(Config{BaseDir: t.TempDir(), RetryMax: 3, RetryBackoffBase: time.Millisecond})

	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 1}); err == nil {
		t.Fatal("Download succeeded on a 404")
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("%d GET requests for a 404, want 1", n)
	}
}

// 503 with Retry-After waits as long as the server asks before the next
// attempt, then succeeds (N6).
func TestDownload_ServiceUnavailableHonoursRetryAfter(t *testing.T) {
	payload := testPayload(1024)
	srv, gets := statusServer(t, payload, http.StatusServiceUnavailable, 1, http.Header{"Retry-After": {"1"}})
	e := New(Config{BaseDir: t.TempDir(), RetryMax: 3, RetryBackoffBase: time.Millisecond})

	start := time.Now()
	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 1}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Fatalf("retried after %s, want the server's Retry-After of 1s", waited)
	}
	if n := gets.Load(); n != 2 {
		t.Fatalf("%d GET requests, want 2", n)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"garbage", 0},
		{"30", 30 * time.Second},
		{"999999999999", maxRetryAfterWait},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{now.Add(time.Hour).Format(http.TimeFormat), maxRetryAfterWait},
	}
	for _, c := range cases {
		if got := retryAfter(c.in, now); got != c.want {
			t.Errorf("retryAfter(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
