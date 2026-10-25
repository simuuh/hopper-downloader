package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A server that sends part of the body and then goes silent without
// closing the connection must not hang the download forever: the stalled
// transfer is aborted and resumed from the bytes already written.
func TestDownload_StalledTransferIsRetriedNotHungForever(t *testing.T) {
	payload := testPayload(64 * 1024)
	var gets atomic.Int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			return
		}
		var start int
		end := len(payload) - 1
		if n, _ := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); n == 0 {
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		part := payload[start : end+1]
		if gets.Add(1) == 1 {
			_, _ = w.Write(part[:len(part)/2])
			w.(http.Flusher).Flush()
			select { // go silent, keep the connection open
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		_, _ = w.Write(part)
	}))
	t.Cleanup(srv.Close)

	e := New(Config{BaseDir: t.TempDir(), RetryMax: 2, RetryBackoffBase: time.Millisecond, StallTimeout: 300 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := e.Download(ctx, Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 1})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if sha(got) != sha(payload) {
		t.Fatal("content mismatch after stall + resume")
	}
}

// Time spent waiting for the bandwidth limiter is not a stall: a
// download throttled well below what the server delivers finishes
// without a single retry (M18).
func TestDownload_LimiterWaitIsNotAStall(t *testing.T) {
	// Far more than socket buffers hold, at a limit that makes every read
	// wait longer than the stall timeout: about three seconds in total.
	payload := testPayload(8 << 20)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		gets.Add(1)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	e := New(Config{BaseDir: t.TempDir(), RetryMax: 0, StallTimeout: 300 * time.Millisecond, GlobalBandwidthLimit: 2 << 20})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := e.Download(ctx, Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 1}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("%d GET requests, want 1 (no stall retries)", n)
	}
}
