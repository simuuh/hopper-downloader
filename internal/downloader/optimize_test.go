package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownload_HTMLErrorPageIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<html><body>Link expired</body></html>")
	}))
	t.Cleanup(srv.Close)

	base := t.TempDir()
	e := New(Config{BaseDir: base, RetryMax: 3, RetryBackoffBase: time.Millisecond})
	_, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "movie.part1.rar"})
	if err == nil || !strings.Contains(err.Error(), "HTML page") {
		t.Fatalf("err = %v, want HTML-page error", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "movie.part1.rar")); statErr == nil {
		t.Fatal("HTML error page was saved as the archive")
	}
}

func TestDownload_HTMLFileIsStillAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, "<html></html>")
	}))
	t.Cleanup(srv.Close)
	e := New(Config{BaseDir: t.TempDir()})
	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "page.html"}); err != nil {
		t.Fatalf("Download of a real .html file: %v", err)
	}
}

// HEAD without Accept-Ranges, but GET honors Range: should still be
// downloaded in parallel chunks.
func TestDownload_RangeSupportDetectedWhenHEADOmitsAcceptRanges(t *testing.T) {
	payload := testPayload(12 << 20) // 12 MiB -> 3 chunks at minChunkSize
	inner := rangeServer(t, payload, true)
	var rangedGETs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			return // no Accept-Ranges
		}
		if r.Header.Get("Range") != "" {
			rangedGETs.Add(1)
		}
		req, _ := http.NewRequest(r.Method, inner.URL, nil)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			if k != "Accept-Ranges" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 64<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			_, _ = w.Write(buf[:n])
			if rerr != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	e := New(Config{BaseDir: t.TempDir(), DefaultParallelChunks: 4})
	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "big.bin"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(res.Path)
	if sha(got) != sha(payload) {
		t.Fatal("content mismatch")
	}
	// 1 probe + 3 chunk requests.
	if n := rangedGETs.Load(); n < 4 {
		t.Fatalf("ranged GETs = %d, want parallel chunks after GET-probe fallback", n)
	}
}

func TestDownload_SmallFileUsesSingleDefaultChunk(t *testing.T) {
	payload := testPayload(100 * 1024)
	var gets atomic.Int32
	inner := rangeServer(t, payload, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		http.Redirect(w, r, inner.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	e := New(Config{BaseDir: t.TempDir(), DefaultParallelChunks: 8})
	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "small.bin"}); err != nil {
		t.Fatal(err)
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("GETs = %d, want a single chunk for a 100 KiB file", n)
	}
}

func TestDownload_TruncatedExistingFileIsRedownloaded(t *testing.T) {
	payload := testPayload(64 * 1024)
	srv := rangeServer(t, payload, true)
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "f.bin"), payload[:1000], 0o600); err != nil {
		t.Fatal(err)
	}
	e := New(Config{BaseDir: base})
	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "f.bin", ExpectedSize: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("truncated leftover was skipped as complete")
	}
	got, _ := os.ReadFile(res.Path)
	if sha(got) != sha(payload) {
		t.Fatal("content mismatch")
	}
}

// A bandwidth-limit change in the Web UI must apply to transfers that are
// already running, not only to the next chunk/request: the limiter used
// to be read once per request, so lifting a low limit left a big
// download crawling at the old rate until it finished.
func TestDownload_LiveBandwidthChangeAppliesToRunningTransfer(t *testing.T) {
	payload := testPayload(2 << 20) // 2 MiB
	srv := rangeServer(t, payload, true)
	e := New(Config{BaseDir: t.TempDir(), GlobalBandwidthLimit: 64 << 10}) // 64 KiB/s -> ~32s

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := e.Download(ctx, Request{URL: srv.URL, RelPath: "f.bin", ParallelChunks: 1})
		done <- err
	}()
	// On failure, stop the transfer before TempDir cleanup (Windows can't
	// delete a file that is still open).
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	time.Sleep(300 * time.Millisecond)
	e.SetGlobalBandwidthLimit(0) // unlimited

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("took %v after lifting the limit", el)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("download still throttled by the old limiter after SetGlobalBandwidthLimit(0)")
	}
}

func TestReadSize_CappedByActiveLimiterBurst(t *testing.T) {
	if got := readSize(readBufSize); got != readBufSize {
		t.Errorf("unthrottled = %d, want full buffer", got)
	}
	low := newByteLimiter(10_000) // burst floors at minLimitedRead
	high := newByteLimiter(50 << 20)
	if got := readSize(readBufSize, nil, low, high); got != minLimitedRead {
		t.Errorf("throttled = %d, want %d (smallest burst)", got, minLimitedRead)
	}
	if got := readSize(readBufSize, high); got != readBufSize {
		t.Errorf("high limit = %d, want full buffer", got)
	}
}
