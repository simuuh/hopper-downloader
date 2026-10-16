package downloader

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// testPayload returns a deterministic, non-repeating byte sequence so
// truncation/corruption bugs in chunk math are easy to detect.
func testPayload(size int) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i*31 + 7)
	}
	return buf
}

func rangeServer(t *testing.T, payload []byte, acceptRanges bool) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptRanges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		rangeHdr := r.Header.Get("Range")
		if !acceptRanges || rangeHdr == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(payload)
			return
		}
		var start, end int
		end = len(payload) - 1
		n, _ := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end)
		if n == 0 {
			_, _ = fmt.Sscanf(rangeHdr, "bytes=%d-", &start)
		}
		if start >= len(payload) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= len(payload) {
			end = len(payload) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(payload[start : end+1])
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func sha(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func TestDownload_Basic_MultiChunk(t *testing.T) {
	payload := testPayload(500 * 1024)
	srv := rangeServer(t, payload, true)
	base := t.TempDir()

	e := New(Config{BaseDir: base, RetryMax: 2})
	res, err := e.Download(context.Background(), Request{
		URL:            srv.URL,
		RelPath:        "file.bin",
		ParallelChunks: 4,
	})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}

	got, err := os.ReadFile(filepath.Join(base, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha(got) != sha(payload) {
		t.Fatal("downloaded content does not match source payload")
	}
	if _, err := os.Stat(filepath.Join(base, "file.bin.part.json")); !os.IsNotExist(err) {
		t.Error("expected resume state file to be removed after completion")
	}
	if _, err := os.Stat(filepath.Join(base, "file.bin.part")); !os.IsNotExist(err) {
		t.Error("expected temp .part file to be removed after completion")
	}
}

func TestDownload_NoRangeSupport_SingleStream(t *testing.T) {
	payload := testPayload(200 * 1024)
	srv := rangeServer(t, payload, false)
	base := t.TempDir()

	e := New(Config{BaseDir: base})
	res, err := e.Download(context.Background(), Request{
		URL:            srv.URL,
		RelPath:        "file.bin",
		ParallelChunks: 4, // should be forced down to 1
	})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
	got, _ := os.ReadFile(filepath.Join(base, "file.bin"))
	if sha(got) != sha(payload) {
		t.Fatal("downloaded content does not match source payload")
	}
}

func TestDownload_DestinationAlreadyExists(t *testing.T) {
	base := t.TempDir()
	existing := []byte("already here")
	if err := os.WriteFile(filepath.Join(base, "file.bin"), existing, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := rangeServer(t, testPayload(1024), true)

	e := New(Config{BaseDir: base})
	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "file.bin"})
	if err != nil {
		t.Fatalf("Download should skip an already-complete destination, not fail: %v", err)
	}
	if !res.Skipped {
		t.Error("Result.Skipped = false, want true")
	}
	if res.BytesWritten != int64(len(existing)) {
		t.Errorf("BytesWritten = %d, want %d (the existing file's real size)", res.BytesWritten, len(existing))
	}
	got, _ := os.ReadFile(filepath.Join(base, "file.bin"))
	if string(got) != string(existing) {
		t.Error("existing file content was overwritten, want it left untouched")
	}
}

func TestDownload_RejectsPathEscape(t *testing.T) {
	base := t.TempDir()
	srv := rangeServer(t, testPayload(1024), true)
	e := New(Config{BaseDir: base})
	_, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "../escape.bin"})
	if err == nil {
		t.Fatal("expected path escape error, got nil")
	}
}

// hijackDropServer serves Range requests normally except that the first
// dropAfterCalls requests to path "/drop" are truncated mid-body by
// hijacking and closing the connection, simulating a network interruption
// partway through a chunk transfer.
func hijackDropServer(t *testing.T, payload []byte, dropAfterCalls int32) *httptest.Server {
	t.Helper()
	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		var start, end int
		end = len(payload) - 1
		n, _ := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if n == 0 {
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		}
		if end >= len(payload) {
			end = len(payload) - 1
		}
		chunk := payload[start : end+1]

		if atomic.AddInt32(&calls, 1) <= dropAfterCalls && len(chunk) > 4096 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter does not support hijacking")
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			half := chunk[:len(chunk)/4]
			resp := fmt.Sprintf("HTTP/1.1 206 Partial Content\r\nContent-Range: bytes %d-%d/%d\r\nContent-Length: %d\r\n\r\n",
				start, end, len(payload), len(chunk))
			_, _ = buf.WriteString(resp)
			_, _ = buf.Write(half)
			_ = buf.Flush()
			_ = conn.Close()
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(chunk)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestDownload_RetriesAfterMidStreamDrop(t *testing.T) {
	payload := testPayload(300 * 1024)
	const parallelChunks = 3
	// Drop exactly the first wave of concurrent requests (one per chunk);
	// by the time each dropped chunk retries, the call count has already
	// passed this threshold so the retry succeeds.
	srv := hijackDropServer(t, payload, parallelChunks)
	base := t.TempDir()

	e := New(Config{BaseDir: base, RetryMax: 5, RetryBackoffBase: 10 * time.Millisecond})
	res, err := e.Download(context.Background(), Request{
		URL:            srv.URL,
		RelPath:        "file.bin",
		ParallelChunks: parallelChunks,
	})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
	got, _ := os.ReadFile(filepath.Join(base, "file.bin"))
	if sha(got) != sha(payload) {
		t.Fatal("downloaded content does not match source payload after retries")
	}
}

// slowRangeServer behaves like rangeServer but drip-feeds the response in
// small delayed writes, so a test can reliably cancel a download mid-flight
// via a short context timeout instead of racing the transfer.
func slowRangeServer(t *testing.T, payload []byte, perWriteDelay time.Duration) *httptest.Server {
	t.Helper()
	const writeSize = 4096
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		start, end := 0, len(payload)-1
		if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
			n, _ := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end)
			if n == 0 {
				_, _ = fmt.Sscanf(rangeHdr, "bytes=%d-", &start)
			}
			if end >= len(payload) {
				end = len(payload) - 1
			}
		}
		chunk := payload[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodHead {
			return
		}
		flusher, _ := w.(http.Flusher)
		for len(chunk) > 0 {
			n := writeSize
			if n > len(chunk) {
				n = len(chunk)
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			chunk = chunk[n:]
			time.Sleep(perWriteDelay)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestDownload_ResumeAcrossEngineRestart(t *testing.T) {
	payload := testPayload(400 * 1024)
	srv := slowRangeServer(t, payload, 5*time.Millisecond) // ~500ms full transfer
	base := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	e1 := New(Config{BaseDir: base, RetryMax: 0})
	_, err := e1.Download(ctx, Request{
		URL:            srv.URL,
		RelPath:        "file.bin",
		ParallelChunks: 4,
	})
	if err == nil {
		t.Fatal("expected first Download to be cancelled, got nil error")
	}

	if _, statErr := os.Stat(filepath.Join(base, "file.bin.part")); statErr != nil {
		t.Fatalf("expected partial .part file to remain after cancellation: %v", statErr)
	}

	e2 := New(Config{BaseDir: base, RetryMax: 2})
	res, err := e2.Download(context.Background(), Request{
		URL:            srv.URL,
		RelPath:        "file.bin",
		ParallelChunks: 4,
	})
	if err != nil {
		t.Fatalf("resumed Download failed: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
	got, _ := os.ReadFile(filepath.Join(base, "file.bin"))
	if sha(got) != sha(payload) {
		t.Fatal("resumed download content does not match source payload")
	}
}

func TestDownload_GlobalBandwidthLimit_StillCorrect(t *testing.T) {
	payload := testPayload(64 * 1024)
	srv := rangeServer(t, payload, true)
	base := t.TempDir()

	e := New(Config{BaseDir: base, GlobalBandwidthLimit: 256 * 1024})
	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "file.bin", ParallelChunks: 2})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
}

func TestDownload_HosterRateGate_DelaysSecondDownload(t *testing.T) {
	payload := testPayload(4 * 1024)
	srv := rangeServer(t, payload, true)
	base := t.TempDir()

	e := New(Config{
		BaseDir:                  base,
		HosterMinRequestInterval: map[string]time.Duration{"testhoster": 300 * time.Millisecond},
	})

	start := time.Now()
	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "a.bin", HosterKey: "testhoster"}); err != nil {
		t.Fatalf("first download: %v", err)
	}
	firstElapsed := time.Since(start)

	start2 := time.Now()
	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "b.bin", HosterKey: "testhoster"}); err != nil {
		t.Fatalf("second download: %v", err)
	}
	secondElapsed := time.Since(start2)

	if secondElapsed < 250*time.Millisecond {
		t.Errorf("expected second download to be gated by ~300ms hoster interval, took %v (first took %v)", secondElapsed, firstElapsed)
	}
}
