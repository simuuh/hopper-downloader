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

	startsMu sync.Mutex
	starts   []int // first byte of every ranged GET

	srv *httptest.Server
}

func (o *origin) rangeStarts() []int {
	o.startsMu.Lock()
	defer o.startsMu.Unlock()
	return append([]int(nil), o.starts...)
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
		o.startsMu.Lock()
		o.starts = append(o.starts, start)
		o.startsMu.Unlock()
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

// --- H5: a 416 is missing data, never "chunk complete" ---

// Every range not starting at byte 0 is answered 416, as a server does
// once the file behind the URL shrank or the link stopped serving it.
func TestDownload_416IsAnErrorNotCompletion_H5(t *testing.T) {
	payload := randomPayload(16<<20, 4)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		rh := r.Header.Get("Range")
		if r.Method == http.MethodHead || rh == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(payload)
			}
			return
		}
		gets.Add(1)
		var s, e int
		_, _ = fmt.Sscanf(rh, "bytes=%d-%d", &s, &e)
		if s > 0 {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[s : e+1])
	}))
	t.Cleanup(srv.Close)
	base := t.TempDir()

	e := New(Config{BaseDir: base, RetryMax: 3, RetryBackoffBase: 20 * time.Millisecond})
	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 4})
	if err == nil {
		t.Fatalf("Download succeeded (%d bytes written), want an error for the 416 chunks", res.BytesWritten)
	}
	if _, statErr := os.Stat(filepath.Join(base, "f.bin")); statErr == nil {
		t.Fatal("an incomplete file was finalized")
	}
	// 4 chunks, one request each: a 416 won't change on retry.
	if n := gets.Load(); n > 4 {
		t.Errorf("ranged GETs = %d, the 416 was retried", n)
	}
}

// Normal case: a chunk that is already complete in the resume state is
// skipped, not requested again, and the result is bit-exact.
func TestDownload_ResumeSkipsCompletedChunk_BitExact(t *testing.T) {
	payload := randomPayload(1<<20, 5)
	o := newOrigin(t, payload, true, 0)
	base := t.TempDir()
	dest := filepath.Join(base, "f.bin")

	st := newState(o.srv.URL, int64(len(payload)), true, 4, statePath(dest))
	done := st.Chunks[1]
	part := make([]byte, len(payload))
	copy(part[done.Start:done.End+1], payload[done.Start:done.End+1])
	st.Chunks[1].Downloaded = done.End - done.Start + 1
	if err := os.WriteFile(tempPath(dest), part, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	res := resumeDownload(t, base, o.srv.URL, 4)
	if !res.Resumed {
		t.Error("Resumed = false, want the saved state to be used")
	}
	for _, s := range o.rangeStarts() {
		if int64(s) >= done.Start && int64(s) <= done.End {
			t.Errorf("completed chunk was requested again (range start %d)", s)
		}
	}
	assertFileEquals(t, dest, payload)
}

// --- H4: resume only what can really be resumed ---

// A state saved by an interrupted non-resumable transfer (no Range
// support) must not be resumed: the server sends the file from byte 0
// again, which used to be written at the saved offset.
func TestDownload_NonResumableStateIsNotResumed_H4(t *testing.T) {
	payload := randomPayload(8<<20, 6)
	srv := rangeServer(t, payload, false)
	base := t.TempDir()
	dest := filepath.Join(base, "f.bin")

	st := newState(srv.URL, int64(len(payload)), false, 1, statePath(dest))
	st.Chunks[0].Downloaded = 1 << 20
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempPath(dest), payload[:1<<20], 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := New(Config{BaseDir: base}).Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Resumed {
		t.Error("Resumed = true for a server without Range support")
	}
	assertFileEquals(t, dest, payload)
}

// The same through a real pause: the restart must yield the exact file.
func TestDownload_PauseResume_NoRangeSupport_RestartsBitExact_H4(t *testing.T) {
	payload := randomPayload(512<<10, 7)
	o := newOrigin(t, payload, false, 4*time.Millisecond)
	base := t.TempDir()

	pauseDownload(t, base, o.srv.URL, 1, 80*time.Millisecond)
	res := resumeDownload(t, base, o.srv.URL, 1)
	if res.Resumed {
		t.Error("Resumed = true for a server without Range support")
	}
	assertFileEquals(t, res.Path, payload)
}

// A resumable state whose chunk layout doesn't describe the file
// (corrupt or hand-edited .part.json) is discarded instead of trusted.
func TestDownload_InvalidChunkLayoutStartsFresh_H4(t *testing.T) {
	const size = 1 << 20
	const half = size / 2
	payload := randomPayload(size, 8)
	cases := map[string]string{
		"progress beyond chunk": fmt.Sprintf(`[{"start":0,"end":%d,"downloaded":%d},{"start":%d,"end":%d,"downloaded":0}]`, half-1, half+100, half, size-1),
		"gap between chunks":    fmt.Sprintf(`[{"start":0,"end":%d,"downloaded":0},{"start":%d,"end":%d,"downloaded":0}]`, half-1, half+10, size-1),
		"overlapping chunks":    fmt.Sprintf(`[{"start":0,"end":%d,"downloaded":0},{"start":%d,"end":%d,"downloaded":0}]`, half+10, half, size-1),
		"short of the end":      fmt.Sprintf(`[{"start":0,"end":%d,"downloaded":0}]`, size-100),
	}
	for name, chunks := range cases {
		t.Run(name, func(t *testing.T) {
			srv := rangeServer(t, payload, true)
			base := t.TempDir()
			dest := filepath.Join(base, "f.bin")
			state := fmt.Sprintf(`{"url":%q,"size":%d,"resumable":true,"num_chunks":2,"chunks":%s}`, srv.URL, size, chunks)
			if err := os.WriteFile(statePath(dest), []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tempPath(dest), make([]byte, size), 0o600); err != nil {
				t.Fatal(err)
			}

			res, err := New(Config{BaseDir: base}).Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 2})
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if res.Resumed {
				t.Error("Resumed = true with an invalid chunk layout")
			}
			assertFileEquals(t, dest, payload)
		})
	}
}

// Normal case: a valid saved state is used and only the missing bytes
// are fetched.
func TestDownload_ValidStateResumesOnlyMissingBytes(t *testing.T) {
	payload := randomPayload(1<<20, 9)
	o := newOrigin(t, payload, true, 0)
	base := t.TempDir()
	dest := filepath.Join(base, "f.bin")

	st := newState(o.srv.URL, int64(len(payload)), true, 4, statePath(dest))
	part := make([]byte, len(payload))
	var have int64
	for i := range st.Chunks {
		c := &st.Chunks[i]
		c.Downloaded = (c.End - c.Start + 1) / 3
		copy(part[c.Start:c.Start+c.Downloaded], payload[c.Start:c.Start+c.Downloaded])
		have += c.Downloaded
	}
	if err := os.WriteFile(tempPath(dest), part, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	res := resumeDownload(t, base, o.srv.URL, 4)
	if !res.Resumed {
		t.Error("Resumed = false for a valid saved state")
	}
	if sent, want := o.bytesOut.Load(), int64(len(payload))-have; sent != want {
		t.Errorf("resume transferred %d bytes, want exactly the missing %d", sent, want)
	}
	assertFileEquals(t, dest, payload)
}
