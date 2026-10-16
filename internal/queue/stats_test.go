package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/directhttp"
)

// fakeStatsRecorder collects RecordDownload/RecordError calls so tests can
// assert queue.Manager calls into StatsRecorder at the right moments with
// the right arguments.
type fakeStatsRecorder struct {
	mu        sync.Mutex
	downloads []recordedDownload
	errors    []string
}

type recordedDownload struct {
	hosterKey string
	bytes     int64
	speed     int64
}

func (f *fakeStatsRecorder) RecordDownload(hosterKey string, bytes int64, speed int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, recordedDownload{hosterKey, bytes, speed})
}

func (f *fakeStatsRecorder) RecordError(hosterKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, hosterKey)
}

func (f *fakeStatsRecorder) snapshot() ([]recordedDownload, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedDownload(nil), f.downloads...), append([]string(nil), f.errors...)
}

func newStatsTestManager(t *testing.T, stats StatsRecorder, cfg Config) *Manager {
	t.Helper()
	baseDir := t.TempDir()
	store := newTestStore(t)
	reg := plugins.NewRegistry(directhttp.New())
	eng := downloader.New(downloader.Config{BaseDir: baseDir, RetryMax: 1, RetryBackoffBase: 5 * time.Millisecond})
	mgr, err := New(store, reg, eng, nil, stats, nil, nil, scheduler.New(), discardLogger(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func TestManager_CompletedDownload_RecordsStats(t *testing.T) {
	content := []byte("hello stats dashboard")
	srv := staticFileServer(t, content)
	stats := &fakeStatsRecorder{}
	mgr := newStatsTestManager(t, stats, Config{ParallelFiles: 2, PollInterval: 30 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	waitForState(t, mgr, it.ID, StateDone, 2*time.Second)

	downloads, errs := stats.snapshot()
	if len(downloads) != 1 {
		t.Fatalf("RecordDownload calls = %d, want 1", len(downloads))
	}
	if downloads[0].hosterKey != "directhttp" {
		t.Errorf("hosterKey = %q, want %q", downloads[0].hosterKey, "directhttp")
	}
	if downloads[0].bytes != int64(len(content)) {
		t.Errorf("bytes = %d, want %d", downloads[0].bytes, len(content))
	}
	if len(errs) != 0 {
		t.Errorf("RecordError calls = %d, want 0", len(errs))
	}
}

func TestManager_FailedDownload_RecordsStatsError(t *testing.T) {
	stats := &fakeStatsRecorder{}
	mgr := newStatsTestManager(t, stats, Config{ParallelFiles: 1, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// Port 0 host is never dialable, so Resolve (a HEAD request) fails.
	it, err := mgr.AddLink("http://127.0.0.1:0/nope", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	waitForState(t, mgr, it.ID, StateError, 2*time.Second)

	downloads, errs := stats.snapshot()
	if len(downloads) != 0 {
		t.Errorf("RecordDownload calls = %d, want 0", len(downloads))
	}
	if len(errs) != 1 {
		t.Fatalf("RecordError calls = %d, want 1", len(errs))
	}
}

func TestManager_NilStatsRecorder_DoesNotPanic(t *testing.T) {
	content := []byte("no stats wired up")
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	waitForState(t, mgr, it.ID, StateDone, 2*time.Second)
}
