package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// truncatingServer advertises the full size and range support, but the
// first ranged GET it serves ends cleanly (chunked encoding, no
// Content-Length) after only half the requested bytes — the "hoster
// dropped the connection" case that used to be finalized as a complete,
// silently truncated file.
func truncatingServer(t *testing.T, payload []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			return
		}
		var start, end int
		end = len(payload) - 1
		if n, _ := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); n == 0 {
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		part := payload[start : end+1]
		if gets.Add(1) == 1 {
			part = part[:len(part)/2]
		}
		_, _ = w.Write(part)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

func TestDownload_EarlyCleanEOFIsRetriedNotFinalized(t *testing.T) {
	payload := testPayload(64 * 1024)
	srv, gets := truncatingServer(t, payload)
	base := t.TempDir()

	e := New(Config{BaseDir: base, RetryMax: 2, RetryBackoffBase: time.Millisecond})
	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 1})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if sha(got) != sha(payload) {
		t.Fatalf("file corrupt/truncated: %d bytes, want %d", len(got), len(payload))
	}
	if gets.Load() < 2 {
		t.Fatalf("expected a resume request after the early EOF, got %d GETs", gets.Load())
	}
}

func TestDownload_ServerIgnoringRangeFailsInsteadOfCorrupting(t *testing.T) {
	payload := testPayload(256 * 1024)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes") // lies
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		if r.Method == http.MethodHead {
			return
		}
		gets.Add(1)
		_, _ = w.Write(payload) // always 200, whole file
	}))
	t.Cleanup(srv.Close)

	e := New(Config{BaseDir: t.TempDir(), RetryMax: 5, RetryBackoffBase: 50 * time.Millisecond})
	_, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 4})
	if err == nil || !strings.Contains(err.Error(), "ignored range") {
		t.Fatalf("err = %v, want ignored-range error", err)
	}
	// 4 chunks, no retries for a permanent failure.
	if n := gets.Load(); n > 4 {
		t.Fatalf("GETs = %d, permanent error was retried", n)
	}
}
