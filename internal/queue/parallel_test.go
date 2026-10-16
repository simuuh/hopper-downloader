package queue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// slowServer blocks each GET (but not HEAD, so resolve/probe don't hang)
// until release is closed, letting a test hold several downloads
// "in flight" (StateDownloading) simultaneously to exercise the
// parallelFiles admission cap under real concurrent dispatch triggers.
func slowServer(t *testing.T, release <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		if r.Method == http.MethodHead {
			return
		}
		<-release
		_, _ = w.Write([]byte("data"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDispatch_ParallelFilesCapHoldsUnderConcurrentTriggers exercises the
// tryAdmit CAS loop added when the fixed-capacity semaphore channel was
// replaced with a live-adjustable atomic counter (see
// Manager.SetParallelFiles): dispatch is invoked concurrently from two
// different goroutines in Start (the poll ticker and the wake handler),
// so a naive load-then-increment admission check could let both admit
// past the limit. This queues many items at once — via concurrent
// AddLink calls, each of which also calls wakeAsync — specifically to
// make that race window as wide as possible.
func TestDispatch_ParallelFilesCapHoldsUnderConcurrentTriggers(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 2, PollInterval: 5 * time.Millisecond})

	release := make(chan struct{})
	defer close(release)
	srv := slowServer(t, release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := mgr.AddLink(fmt.Sprintf("%s/file%d", srv.URL, i), AddOptions{}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	maxDownloading := 0
	for time.Now().Before(deadline) {
		downloading := 0
		for _, it := range mgr.List() {
			if it.State == StateDownloading {
				downloading++
			}
		}
		if downloading > maxDownloading {
			maxDownloading = downloading
		}
		if maxDownloading > 2 {
			break // fail fast, no need to wait out the full deadline
		}
		time.Sleep(2 * time.Millisecond)
	}

	if maxDownloading > 2 {
		t.Fatalf("observed %d items concurrently downloading, want <= 2 (ParallelFiles)", maxDownloading)
	}
	if maxDownloading == 0 {
		t.Fatal("observed 0 items downloading; test didn't actually exercise dispatch")
	}
}

// TestSetParallelFiles_RaisesLimitLive confirms a live increase is
// picked up without a restart: start below the eventual limit, raise it
// mid-flight, and confirm more items start downloading concurrently.
func TestSetParallelFiles_RaisesLimitLive(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 5 * time.Millisecond})

	release := make(chan struct{})
	defer close(release)
	srv := slowServer(t, release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	for i := 0; i < 4; i++ {
		if _, err := mgr.AddLink(fmt.Sprintf("%s/file%d", srv.URL, i), AddOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	countDownloading := func() int {
		n := 0
		for _, it := range mgr.List() {
			if it.State == StateDownloading {
				n++
			}
		}
		return n
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countDownloading() < 1 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := countDownloading(); got != 1 {
		t.Fatalf("setup: downloading = %d, want 1 (ParallelFiles=1)", got)
	}

	mgr.SetParallelFiles(3)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countDownloading() < 3 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := countDownloading(); got != 3 {
		t.Fatalf("after SetParallelFiles(3), downloading = %d, want 3", got)
	}
}
