package clicknload

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/queue"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/directhttp"
)

// realQueue is a started queue.Manager with the directhttp plugin and a
// file server that counts every request it receives — a request there
// means a download (or its probe) was started.
type realQueue struct {
	mgr     *queue.Manager
	baseURL string
	hits    *atomic.Int32
}

func newRealQueue(t *testing.T) *realQueue {
	t.Helper()
	var hits atomic.Int32
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "4")
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("data"))
		}
	}))
	t.Cleanup(files.Close)

	db, err := bbolt.Open(filepath.Join(t.TempDir(), "queue.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := queue.OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	eng := downloader.New(downloader.Config{BaseDir: t.TempDir(), RetryMax: 1, RetryBackoffBase: time.Millisecond})
	mgr, err := queue.New(store, plugins.NewRegistry(directhttp.New()), eng, nil, nil, nil, nil,
		scheduler.New(), discardLogger(), queue.Config{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = mgr.Shutdown(sctx)
	})
	return &realQueue{mgr: mgr, baseURL: files.URL, hits: &hits}
}

// assertAllPausedNoDownload waits long enough for the dispatcher (10 ms
// poll) to have picked up a runnable item, then checks that every queued
// item is paused and the file server never saw a request.
func (q *realQueue) assertAllPausedNoDownload(t *testing.T, wantItems int) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	items := q.mgr.List()
	if len(items) != wantItems {
		t.Fatalf("queued %d items, want %d", len(items), wantItems)
	}
	for _, it := range items {
		if it.State != queue.StatePaused {
			t.Errorf("item %s state = %s, want %s", it.URL, it.State, queue.StatePaused)
		}
	}
	if n := q.hits.Load(); n != 0 {
		t.Errorf("file server received %d requests, want 0 (no download may start)", n)
	}
}

func postCNL(t *testing.T, h http.Handler, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "127.0.0.1:40000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A page on any website can POST to the loopback listener (a CORS simple
// request needs no preflight), so Click'n'Load must never start a
// download by itself — even with auto_start_imports on.
func TestHandleAdd_ForeignOrigin_QueuedPausedNoDownload(t *testing.T) {
	q := newRealQueue(t)
	s := New(q.mgr, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	w := postCNL(t, s.Handler(), "/flash/add",
		url.Values{"urls": {q.baseURL + "/file.bin"}},
		map[string]string{"Origin": "https://attacker.example", "Referer": "https://attacker.example/page"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	q.assertAllPausedNoDownload(t, 1)
}

func TestHandleAdd_NoOrigin_AutoStartOn_QueuedPausedNoDownload(t *testing.T) {
	q := newRealQueue(t)
	s := New(q.mgr, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	w := postCNL(t, s.Handler(), "/flash/add",
		url.Values{"urls": {q.baseURL + "/a.bin\r\n" + q.baseURL + "/b.bin"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	q.assertAllPausedNoDownload(t, 2)
}

func TestHandleAddCrypted2_QueuedPausedNoDownload(t *testing.T) {
	q := newRealQueue(t)
	s := New(q.mgr, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	key := []byte("0123456789abcdef")
	crypted := encryptCNL2(t, []byte(q.baseURL+"/a.bin\r\n"+q.baseURL+"/b.bin"), key)
	w := postCNL(t, s.Handler(), "/flash/addcrypted2", url.Values{
		"jk":      {"function f(){ return '" + hex.EncodeToString(key) + "'; }"},
		"crypted": {crypted},
		"source":  {"https://links.example.org/folder"},
	}, map[string]string{"Origin": "https://links.example.org"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	q.assertAllPausedNoDownload(t, 2)
}

// A zoned IPv6 RemoteAddr doesn't parse as an IP and must fail closed —
// rejected, not panicking, and nothing queued — even when the bare
// address would be trusted.
func TestTrust_ZonedIPv6_RejectedWithoutPanic(t *testing.T) {
	_, v4, _ := net.ParseCIDR("127.0.0.1/32")
	_, v6, _ := net.ParseCIDR("::1/128")
	_, ll, _ := net.ParseCIDR("fe80::/10")
	cfg := Config{Host: "127.0.0.1", TrustedNets: []*net.IPNet{v4, v6, ll}}

	for _, addr := range []string{"[::1%lo0]:40000", "[fe80::1%eth0]:40000", "[fe80::1%25eth0]:40000", "::1%lo"} {
		t.Run(addr, func(t *testing.T) {
			sink := &fakeSink{}
			s := New(sink, fakeAutoStart{}, cfg, discardLogger())
			form := url.Values{"urls": {"http://example.com/file.bin"}}
			req := httptest.NewRequest(http.MethodPost, "/flash/add", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = addr
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
			if len(sink.links)+len(sink.pkgURLs) != 0 {
				t.Error("links were queued for an unparseable client address")
			}
		})
	}
}

func TestHandleAddCrypted2_MalformedCrypted_RejectedWithoutPanic(t *testing.T) {
	key := []byte("0123456789abcdef")
	jk := "function f(){ return '" + hex.EncodeToString(key) + "'; }"
	cases := map[string]string{
		"empty":             "",
		"not base64":        "%%%not-base64%%%",
		"not block aligned": base64.StdEncoding.EncodeToString(make([]byte, 15)),
		"one block garbage": base64.StdEncoding.EncodeToString([]byte("0123456789ABCDEF")),
		"truncated base64":  "QUJD",
	}
	for name, crypted := range cases {
		t.Run(name, func(t *testing.T) {
			sink := &fakeSink{}
			s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
			w := postCNL(t, s.Handler(), "/flash/addcrypted2", url.Values{"jk": {jk}, "crypted": {crypted}}, nil)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if len(sink.links)+len(sink.pkgURLs) != 0 {
				t.Error("links were queued from a malformed payload")
			}
		})
	}
}

func TestHandleAddCrypted2_OversizedJK_RejectedWithoutPanic(t *testing.T) {
	crypted := base64.StdEncoding.EncodeToString(make([]byte, 16))
	cases := map[string]string{
		// Longer than the 1 MiB body cap: ParseForm fails.
		"body over limit": "function f(){ return '" + strings.Repeat("a", maxBodyBytes) + "'; }",
		// Fits the body cap, but the "key" is far longer than AES-128.
		"long hex key": "function f(){ return '" + strings.Repeat("ab", 64<<10) + "'; }",
		// Large and without the literal-return form at all.
		"no return": strings.Repeat("x", 512<<10),
	}
	for name, jk := range cases {
		t.Run(name, func(t *testing.T) {
			sink := &fakeSink{}
			s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
			w := postCNL(t, s.Handler(), "/flash/addcrypted2", url.Values{"jk": {jk}, "crypted": {crypted}}, nil)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if len(sink.links)+len(sink.pkgURLs) != 0 {
				t.Error("links were queued despite an unusable jk")
			}
		})
	}
}
