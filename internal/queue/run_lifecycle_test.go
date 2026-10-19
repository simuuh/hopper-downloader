package queue

import (
	"context"
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
