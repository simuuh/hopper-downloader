package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
)

// hangingHoster's first Resolve blocks until release is closed (like a
// hoster API that doesn't answer); later calls return at once.
type hangingHoster struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (h *hangingHoster) unblock() { h.once.Do(func() { close(h.release) }) }

func newHangingHoster() *hangingHoster {
	return &hangingHoster{entered: make(chan struct{}), release: make(chan struct{})}
}

func (h *hangingHoster) Name() string      { return "hanging" }
func (h *hangingHoster) Domains() []string { return nil }
func (h *hangingHoster) Match(string) bool { return true }
func (h *hangingHoster) Resolve(string, *plugins.Account) (*plugins.FileInfo, error) {
	if h.calls.Add(1) == 1 {
		close(h.entered)
		<-h.release
	}
	return &plugins.FileInfo{Name: "f.bin", Available: true}, nil
}
func (h *hangingHoster) FetchDownloadURL(url string, _ *plugins.Account) (string, error) {
	return url, nil
}

func newLifecycleManager(t *testing.T, h plugins.Hoster, cfg Config) *Manager {
	t.Helper()
	baseDir := t.TempDir()
	eng := downloader.New(downloader.Config{BaseDir: baseDir, RetryMax: 1, RetryBackoffBase: 5 * time.Millisecond})
	mgr, err := New(newTestStore(t), plugins.NewRegistry(h), eng, nil, nil, nil, nil, scheduler.New(), discardLogger(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})
	return mgr
}

// A run stuck in a plugin call ignores its cancelled context. Pausing and
// resuming the item meanwhile must not start a second run of it: two runs
// would write the same file, and the stuck one, once it returns, used to
// unregister the new one, which then could no longer be paused or
// cancelled (H2).
func TestRun_ResumeWhileOldRunHangs_StartsNoSecondRun(t *testing.T) {
	srv := staticFileServer(t, []byte("payload"))
	h := newHangingHoster()
	mgr := newLifecycleManager(t, h, Config{ParallelFiles: 2, PollInterval: 10 * time.Millisecond})
	t.Cleanup(h.unblock) // runs before the manager's Shutdown

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/f.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first run never reached Resolve")
	}

	if err := mgr.Pause(it.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Resume(it.ID); err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond) // many poll intervals
	if n := h.calls.Load(); n != 1 {
		t.Fatalf("Resolve called %d times while the first run still hangs, want 1 (a second run started)", n)
	}

	h.unblock()
	waitForState(t, mgr, it.ID, StateDone, 3*time.Second)
}

// hangingDownloadServer answers HEAD normally and holds every GET open
// until the client goes away, signalling gotGET when one arrives.
func hangingDownloadServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	gotGET := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		if r.Method == http.MethodHead {
			return
		}
		gotGET <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, gotGET
}

// On SIGTERM hopperd's signal context is cancelled before
// Manager.Shutdown runs, so in-flight downloads end with "context
// canceled" before any intent is recorded. That is a shutdown, not a
// failure: the item must go back to the queue as it was, without using
// up an automatic retry or waiting out a retry delay (H3).
func TestShutdown_CancelledRunContextIsNotAFailure(t *testing.T) {
	srv, gotGET := hangingDownloadServer(t)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond, AutoRetry: 3, AutoRetryDelay: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	it, err := mgr.AddLink(srv.URL+"/f.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotGET:
	case <-time.After(3 * time.Second):
		t.Fatal("download never started")
	}

	cancel() // the signal context, before Shutdown
	got := waitForState(t, mgr, it.ID, StateQueued, 3*time.Second)
	if got.Attempts != 0 || got.RetryAfter != nil || got.Error != "" {
		t.Fatalf("after shutdown: Attempts=%d RetryAfter=%v Error=%q, want an untouched queued item", got.Attempts, got.RetryAfter, got.Error)
	}
}

// panickingHoster stands in for a plugin with a bug, e.g. an index out of
// range while parsing an unexpected API response.
type panickingHoster struct{}

func (panickingHoster) Name() string      { return "panicking" }
func (panickingHoster) Domains() []string { return nil }
func (panickingHoster) Match(string) bool { return true }
func (panickingHoster) Resolve(string, *plugins.Account) (*plugins.FileInfo, error) {
	panic("unexpected response shape")
}
func (panickingHoster) FetchDownloadURL(url string, _ *plugins.Account) (string, error) {
	return url, nil
}

// A panic in one download's plugin call fails that item; it must not take
// the whole daemon down with it (N8).
func TestRun_PluginPanicFailsOnlyThatItem(t *testing.T) {
	mgr := newLifecycleManager(t, panickingHoster{}, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink("https://host.example/file.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitForState(t, mgr, it.ID, StateError, 3*time.Second)
	if !strings.Contains(got.Error, "internal error") {
		t.Fatalf("Error = %q, want it to name an internal error", got.Error)
	}
	if mgr.activeFiles.Load() != 0 {
		t.Fatalf("activeFiles = %d after the run ended, want its slot freed", mgr.activeFiles.Load())
	}
}
