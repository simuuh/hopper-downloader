package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// randomPayload returns n pseudo-random bytes from a fixed seed. Unlike
// testPayload (period 256), a block written at the wrong offset never
// matches by accident, so offset and mixing bugs show up in the hash.
func randomPayload(n int, seed uint32) []byte {
	b := make([]byte, n)
	x := seed
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// origin is a test file server with optional Range support, validators
// (ETag / Last-Modified, honoring If-Range) and a per-4 KiB write delay
// so a download can be paused mid-transfer. Payload and validators can
// be swapped to simulate the remote file changing.
type origin struct {
	mu      sync.Mutex
	payload []byte
	etag    string
	lastMod string

	ranges bool
	delay  time.Duration

	bytesOut  atomic.Int64 // body bytes sent by GETs
	gets      atomic.Int32
	ifRanges  atomic.Int32 // GETs carrying If-Range
	fullGets  atomic.Int32 // GETs answered 200 with the whole file
	rangeGets atomic.Int32 // GETs answered 206

	srv *httptest.Server
}

func newOrigin(t *testing.T, payload []byte, ranges bool, delay time.Duration) *origin {
	t.Helper()
	o := &origin{payload: payload, ranges: ranges, delay: delay}
	o.srv = httptest.NewServer(o)
	t.Cleanup(o.srv.Close)
	return o
}

func (o *origin) resetCounters() {
	o.bytesOut.Store(0)
	o.gets.Store(0)
	o.ifRanges.Store(0)
	o.fullGets.Store(0)
	o.rangeGets.Store(0)
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	payload, etag, lastMod := o.payload, o.etag, o.lastMod
	o.mu.Unlock()

	h := w.Header()
	if etag != "" {
		h.Set("ETag", etag)
	}
	if lastMod != "" {
		h.Set("Last-Modified", lastMod)
	}
	if o.ranges {
		h.Set("Accept-Ranges", "bytes")
	}

	start, end := 0, len(payload)-1
	ranged := false
	if rh := r.Header.Get("Range"); o.ranges && rh != "" {
		ranged = true
		if n, _ := fmt.Sscanf(rh, "bytes=%d-%d", &start, &end); n == 0 {
			_, _ = fmt.Sscanf(rh, "bytes=%d-", &start)
		}
		if ir := r.Header.Get("If-Range"); ir != "" {
			if r.Method == http.MethodGet {
				o.ifRanges.Add(1)
			}
			if ir != etag && ir != lastMod {
				ranged, start, end = false, 0, len(payload)-1 // changed: whole new file
			}
		}
	}
	if ranged {
		if start >= len(payload) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= len(payload) {
			end = len(payload) - 1
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
	}
	body := payload[start : end+1]
	h.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if ranged {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	o.gets.Add(1)
	if ranged {
		o.rangeGets.Add(1)
	} else {
		o.fullGets.Add(1)
	}
	flusher, _ := w.(http.Flusher)
	for len(body) > 0 {
		n := min(4096, len(body))
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		o.bytesOut.Add(int64(n))
		if flusher != nil {
			flusher.Flush()
		}
		body = body[n:]
		if o.delay > 0 {
			time.Sleep(o.delay)
		}
	}
}

// pauseDownload starts a download of url into base/f.bin and cancels it
// after d, like a user pausing it. It fails the test if the transfer
// finished before the pause or left no resume data behind.
func pauseDownload(t *testing.T, base, url string, chunks int, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, err := New(Config{BaseDir: base, RetryMax: 0}).Download(ctx, Request{URL: url, RelPath: "f.bin", ParallelChunks: chunks})
	if err == nil {
		t.Fatal("download finished before the pause; slow the origin down")
	}
	dest := filepath.Join(base, "f.bin")
	for _, p := range []string{tempPath(dest), statePath(dest)} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("no resume data after pause: %v", statErr)
		}
	}
}

func resumeDownload(t *testing.T, base, url string, chunks int) *Result {
	t.Helper()
	res, err := New(Config{BaseDir: base, RetryMax: 2, RetryBackoffBase: time.Millisecond}).
		Download(context.Background(), Request{URL: url, RelPath: "f.bin", ParallelChunks: chunks})
	if err != nil {
		t.Fatalf("resumed download: %v", err)
	}
	return res
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if len(got) != len(want) || sha(got) != sha(want) {
		t.Fatalf("file differs from the source: %d bytes, want %d (sha match: %v)", len(got), len(want), sha(got) == sha(want))
	}
}

// --- M16: stale .part data must never survive into a fresh download ---

func TestDownload_FreshStartTruncatesStalePart_M16(t *testing.T) {
	payload := randomPayload(1<<20, 1)
	stale := randomPayload(3<<20, 2)

	chunkedNoLength := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				return // no Content-Length, no ranges: size unknown
			}
			w.(http.Flusher).Flush() // forces chunked encoding
			_, _ = w.Write(payload)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	cases := []struct {
		name  string
		url   func() string
		state string // contents of .part.json, "" = none
	}{
		{"state for another URL", func() string { return rangeServer(t, payload, true).URL },
			`{"url":"http://old.invalid/f","size":3145728,"resumable":true,"num_chunks":1,"chunks":[{"start":0,"end":3145727,"downloaded":10}]}`},
		{"part without state", func() string { return rangeServer(t, payload, true).URL }, ""},
		{"unknown size", chunkedNoLength, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dest := filepath.Join(base, "f.bin")
			if err := os.WriteFile(tempPath(dest), stale, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.state != "" {
				if err := os.WriteFile(statePath(dest), []byte(tc.state), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			res, err := New(Config{BaseDir: base}).Download(context.Background(), Request{URL: tc.url(), RelPath: "f.bin"})
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if res.Resumed {
				t.Error("Resumed = true for a download that had to start over")
			}
			assertFileEquals(t, dest, payload)
		})
	}
}

// Normal case: pausing and resuming against an unchanged server keeps
// the bytes already on disk and yields a bit-exact file.
func TestDownload_PauseResume_Resumable_BitExact(t *testing.T) {
	payload := randomPayload(512<<10, 3)
	o := newOrigin(t, payload, true, 4*time.Millisecond)
	base := t.TempDir()

	pauseDownload(t, base, o.srv.URL, 4, 80*time.Millisecond)
	o.resetCounters()
	res := resumeDownload(t, base, o.srv.URL, 4)

	if !res.Resumed {
		t.Error("Resumed = false, want the paused transfer to continue")
	}
	if sent := o.bytesOut.Load(); sent >= int64(len(payload)) {
		t.Errorf("resume transferred %d bytes, want fewer than the whole file (%d)", sent, len(payload))
	}
	assertFileEquals(t, res.Path, payload)
}
