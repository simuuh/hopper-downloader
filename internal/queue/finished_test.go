package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/notify"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/directhttp"
)

type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordingNotifier) PackageFinished(ev notify.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingNotifier) snapshot() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

// waitEvent waits until a first event arrived, then a little longer to
// catch unwanted duplicates.
func (r *recordingNotifier) waitEvent(t *testing.T) []notify.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(r.snapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	return r.snapshot()
}

func newNotifyTestManager(t *testing.T, rn *recordingNotifier, fe *fakeExtractor, fs *fakeExtractSettings) *Manager {
	t.Helper()
	baseDir := t.TempDir()
	reg := plugins.NewRegistry(directhttp.New())
	eng := downloader.New(downloader.Config{BaseDir: baseDir, RetryMax: 1, RetryBackoffBase: 5 * time.Millisecond})
	var ex Extractor
	var es ExtractSettings
	if fe != nil {
		ex, es = fe, fs
	}
	mgr, err := New(newTestStore(t), reg, eng, nil, nil, ex, es, scheduler.New(), discardLogger(), Config{
		ParallelFiles: 2,
		PollInterval:  15 * time.Millisecond,
		Notifier:      rn,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr.Start(ctx)
	return mgr
}

func TestCheckFinished_NotifiesOnceWhenWholePackageDone(t *testing.T) {
	srv := staticFileServer(t, []byte("0123456789"))
	rn := &recordingNotifier{}
	mgr := newNotifyTestManager(t, rn, nil, nil)

	pkg, _, err := mgr.AddPackage("Release", []string{srv.URL + "/a.bin", srv.URL + "/b.bin", srv.URL + "/c.bin"}, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}

	events := rn.waitEvent(t)
	if len(events) != 1 {
		t.Fatalf("got %d notifications, want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.PackageID != pkg.ID || ev.Package != "Release" || ev.Status != notify.StatusCompleted {
		t.Errorf("event = %+v", ev)
	}
	if ev.Items != 3 || ev.Done != 3 || ev.Failed != 0 || ev.Bytes != 30 {
		t.Errorf("counts = items %d done %d failed %d bytes %d", ev.Items, ev.Done, ev.Failed, ev.Bytes)
	}
	if ev.Path != "Release" {
		t.Errorf("path = %q, want Release", ev.Path)
	}
}

func TestCheckFinished_PartialWhenAnItemFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.bin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "4")
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("data"))
		}
	}))
	t.Cleanup(srv.Close)
	rn := &recordingNotifier{}
	mgr := newNotifyTestManager(t, rn, nil, nil)

	if _, _, err := mgr.AddPackage("Mixed", []string{srv.URL + "/ok.bin", srv.URL + "/missing.bin"}, AddOptions{}); err != nil {
		t.Fatal(err)
	}

	events := rn.waitEvent(t)
	if len(events) != 1 {
		t.Fatalf("got %d notifications, want 1: %+v", len(events), events)
	}
	if ev := events[0]; ev.Status != notify.StatusPartial || ev.Done != 1 || ev.Failed != 1 {
		t.Errorf("event = %+v, want partial 1 done / 1 failed", ev)
	}
}

func TestCheckFinished_WaitsForExtraction(t *testing.T) {
	srv := staticFileServer(t, []byte("rar content"))
	rn := &recordingNotifier{}
	fe := &fakeExtractor{}
	mgr := newNotifyTestManager(t, rn, fe, &fakeExtractSettings{enabled: true})

	_, _, err := mgr.AddPackage("Movie", []string{srv.URL + "/1", srv.URL + "/2"}, AddOptions{
		Filenames: []string{"movie.part1.rar", "movie.part2.rar"},
	})
	if err != nil {
		t.Fatal(err)
	}

	events := rn.waitEvent(t)
	if len(events) != 1 {
		t.Fatalf("got %d notifications, want exactly 1 (after extraction): %+v", len(events), events)
	}
	if fe.callCount() != 1 {
		t.Fatalf("extractor called %d times, want 1", fe.callCount())
	}
	if ev := events[0]; ev.Extract != string(ExtractStateExtracted) || ev.Status != notify.StatusCompleted {
		t.Errorf("event = %+v, want completed + extracted", ev)
	}
}

func TestCheckFinished_SingleLinkWithoutPackage(t *testing.T) {
	srv := staticFileServer(t, []byte("x"))
	rn := &recordingNotifier{}
	mgr := newNotifyTestManager(t, rn, nil, nil)

	if _, err := mgr.AddLink(srv.URL+"/single.iso", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	events := rn.waitEvent(t)
	if len(events) != 1 {
		t.Fatalf("got %d notifications, want 1", len(events))
	}
	if ev := events[0]; ev.PackageID != "" || ev.Package != "single.iso" || ev.Items != 1 {
		t.Errorf("event = %+v", ev)
	}
}
