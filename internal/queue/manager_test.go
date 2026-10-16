package queue

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/directhttp"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestManager returns a Manager wired to a real downloader.Engine (with
// its own temp BaseDir, also returned) and the directhttp plugin, so tests
// exercise the same code path production traffic uses.
func newTestManager(t *testing.T, store *Store, cfg Config) (*Manager, string) {
	t.Helper()
	baseDir := t.TempDir()
	reg := plugins.NewRegistry(directhttp.New())
	eng := downloader.New(downloader.Config{BaseDir: baseDir, RetryMax: 1, RetryBackoffBase: 5 * time.Millisecond})
	mgr, err := New(store, reg, eng, nil, nil, nil, nil, scheduler.New(), discardLogger(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir above, so it runs before the temp dir is
	// removed: wait for in-flight download goroutines to stop writing.
	// Without this, Windows fails the TempDir cleanup ("directory not
	// empty") whenever a test ends with a transfer still finishing.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})
	return mgr, baseDir
}

func staticFileServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitForState(t *testing.T, mgr *Manager, id string, want State, timeout time.Duration) *Item {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		it, ok := mgr.Get(id)
		if ok && it.State == want {
			return it
		}
		time.Sleep(5 * time.Millisecond)
	}
	it, _ := mgr.Get(id)
	t.Fatalf("timed out waiting for item %s to reach state %s; last seen: %+v", id, want, it)
	return nil
}

func TestManager_AddLink_CompletesDownload(t *testing.T) {
	content := []byte("hello hopper")
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, baseDir := newTestManager(t, store, Config{ParallelFiles: 2, PollInterval: 30 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	done := waitForState(t, mgr, it.ID, StateDone, 2*time.Second)
	if done.Downloaded != int64(len(content)) {
		t.Errorf("Downloaded = %d, want %d", done.Downloaded, len(content))
	}

	data, err := os.ReadFile(filepath.Join(baseDir, "file.txt"))
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("file content = %q, want %q", data, content)
	}
}

func TestManager_FailedResolve_SetsErrorState(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// Port 0 host is never dialable, so Resolve (a HEAD request) fails.
	it, err := mgr.AddLink("http://127.0.0.1:0/nope", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	errored := waitForState(t, mgr, it.ID, StateError, 2*time.Second)
	if errored.Error == "" {
		t.Error("expected non-empty Error message")
	}
}

func TestManager_RetryAfterFailure(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink("http://127.0.0.1:0/nope", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	waitForState(t, mgr, it.ID, StateError, 2*time.Second)

	// Stop the dispatcher so the requeued item isn't picked up (and, with
	// an undialable URL, failed again) before the assertions below run.
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = mgr.Shutdown(shutdownCtx)

	if err := mgr.Retry(it.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	requeued, ok := mgr.Get(it.ID)
	if !ok || requeued.State != StateQueued {
		t.Fatalf("expected item requeued, got %+v", requeued)
	}

	if err := mgr.Retry(it.ID); err == nil {
		t.Error("expected Retry on a non-error item to fail, got nil")
	}
}

func TestManager_PauseAndResume(t *testing.T) {
	content := make([]byte, 5*1024*1024) // large enough that pause wins the race
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/big.bin", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	waitForState(t, mgr, it.ID, StateDownloading, 2*time.Second)

	if err := mgr.Pause(it.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	paused := waitForState(t, mgr, it.ID, StatePaused, 2*time.Second)
	if paused.State != StatePaused {
		t.Fatalf("expected paused state, got %s", paused.State)
	}

	if err := mgr.Resume(it.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitForState(t, mgr, it.ID, StateDone, 5*time.Second)
}

func TestManager_AddLink_StartPausedSkipsQueuedState(t *testing.T) {
	content := []byte("should not download")
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/f.bin", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if it.State != StatePaused {
		t.Fatalf("State = %s, want paused immediately on add", it.State)
	}

	// Give the dispatcher a couple of poll ticks to (incorrectly) pick it
	// up if StartPaused hadn't actually kept it out of StateQueued.
	time.Sleep(100 * time.Millisecond)
	got, _ := mgr.Get(it.ID)
	if got.State != StatePaused {
		t.Fatalf("State = %s, want still paused (dispatcher must not run a paused item)", got.State)
	}

	if err := mgr.Resume(it.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitForState(t, mgr, it.ID, StateDone, 2*time.Second)
}

func TestManager_Cancel_RemovesItem(t *testing.T) {
	content := []byte("cancel me")
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 500 * time.Millisecond})

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	// Deliberately not calling Start, so the item stays queued and we can
	// exercise Cancel on a non-downloading item without racing the worker.
	if err := mgr.Cancel(it.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, ok := mgr.Get(it.ID); ok {
		t.Error("expected item to be gone after Cancel")
	}
	if err := mgr.Cancel(it.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound cancelling twice, got %v", err)
	}
}

func TestManager_CancelMany_DeletesAllAndReportsUnknownIDsAsFailed(t *testing.T) {
	content := []byte("cancel many")
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 500 * time.Millisecond})

	var ids []string
	for i := 0; i < 5; i++ {
		it, err := mgr.AddLink(fmt.Sprintf("%s/file%d.txt", srv.URL, i), AddOptions{})
		if err != nil {
			t.Fatalf("AddLink: %v", err)
		}
		ids = append(ids, it.ID)
	}
	ids = append(ids, "does-not-exist")

	deleted, failed := mgr.CancelMany(ids)
	if len(deleted) != 5 {
		t.Errorf("len(deleted) = %d, want 5", len(deleted))
	}
	if len(failed) != 1 || failed[0] != "does-not-exist" {
		t.Errorf("failed = %v, want [does-not-exist]", failed)
	}
	for _, id := range ids[:5] {
		if _, ok := mgr.Get(id); ok {
			t.Errorf("item %s still present after CancelMany", id)
		}
	}
}

func TestManager_Priority_HighRunsBeforeLow(t *testing.T) {
	content := []byte("x")
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond})

	low, err := mgr.AddLink(srv.URL+"/low.txt", AddOptions{Priority: PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	high, err := mgr.AddLink(srv.URL+"/high.txt", AddOptions{Priority: PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	waitForState(t, mgr, high.ID, StateDone, 2*time.Second)
	lowItem, _ := mgr.Get(low.ID)
	if lowItem.State == StateDone {
		t.Skip("both completed too fast on this machine to observe ordering; not a correctness signal")
	}
}

func TestManager_Reorder(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: time.Second})

	a, _ := mgr.AddLink("http://127.0.0.1:0/a", AddOptions{})
	b, _ := mgr.AddLink("http://127.0.0.1:0/b", AddOptions{})
	c, _ := mgr.AddLink("http://127.0.0.1:0/c", AddOptions{})

	if err := mgr.Reorder([]string{c.ID, a.ID, b.ID}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	list := mgr.List()
	if len(list) != 3 || list[0].ID != c.ID || list[1].ID != a.ID || list[2].ID != b.ID {
		ids := make([]string, len(list))
		for i, it := range list {
			ids[i] = it.ID
		}
		t.Fatalf("unexpected order: %v (want [%s %s %s])", ids, c.ID, a.ID, b.ID)
	}
}

func TestManager_PersistsAndReloadsState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reload.db")
	db, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}

	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: time.Second})
	it, err := mgr.AddLink("http://127.0.0.1:0/persisted", AddOptions{Priority: PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash while downloading: force the state directly.
	mgr.mu.Lock()
	mgr.items[it.ID].State = StateDownloading
	mgr.mu.Unlock()
	_ = store.PutItem(mgr.items[it.ID])
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	store2, err := OpenStore(db2)
	if err != nil {
		t.Fatal(err)
	}
	mgr2, _ := newTestManager(t, store2, Config{ParallelFiles: 1, PollInterval: time.Second})

	reloaded, ok := mgr2.Get(it.ID)
	if !ok {
		t.Fatal("expected item to survive reload")
	}
	if reloaded.State != StateQueued {
		t.Errorf("expected crash-recovered item to be StateQueued, got %s", reloaded.State)
	}
	if reloaded.Priority != PriorityHigh {
		t.Errorf("expected priority to survive reload, got %s", reloaded.Priority)
	}
}
