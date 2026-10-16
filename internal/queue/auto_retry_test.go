package queue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyServer fails every GET with 503 until failGETs have been served,
// then serves content normally.
func flakyServer(t *testing.T, content []byte, failGETs int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		if r.Method == http.MethodHead {
			return
		}
		if gets.Add(1) <= failGETs {
			w.Header().Del("Content-Length")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

func TestAutoRetry_TransientDownloadFailureRecovers(t *testing.T) {
	content := []byte("eventually fine")
	// newTestManager's engine uses RetryMax 1 -> 2 GETs (incl. the ranged
	// probe fallback) per attempt fail; 6 failures need a couple of
	// automatic item-level retries.
	srv, _ := flakyServer(t, content, 6)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond, AutoRetry: 5, AutoRetryDelay: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/f.txt", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForState(t, mgr, it.ID, StateDone, 5*time.Second)
	if done.Attempts != 0 {
		t.Errorf("Attempts = %d after success, want reset to 0", done.Attempts)
	}
}

func TestAutoRetry_GivesUpAfterBudget(t *testing.T) {
	srv, _ := flakyServer(t, []byte("x"), 1<<30)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond, AutoRetry: 2, AutoRetryDelay: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink(srv.URL+"/f.txt", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForState(t, mgr, it.ID, StateError, 5*time.Second)
	if failed.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", failed.Attempts)
	}

	// A manual retry gets a fresh budget.
	if err := mgr.Retry(it.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.Get(it.ID)
	if got.Attempts != 0 || got.RetryAfter != nil {
		t.Errorf("after manual Retry: Attempts=%d RetryAfter=%v, want reset", got.Attempts, got.RetryAfter)
	}
}

func TestAutoRetry_DisabledFailsImmediately(t *testing.T) {
	srv, gets := flakyServer(t, []byte("x"), 1<<30)
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, _ := mgr.AddLink(srv.URL+"/f.txt", AddOptions{})
	failed := waitForState(t, mgr, it.ID, StateError, 5*time.Second)
	if strings.Contains(failed.Error, "auto-retry") {
		t.Errorf("Error = %q, auto-retry should be off", failed.Error)
	}
	n := gets.Load()
	time.Sleep(100 * time.Millisecond)
	if gets.Load() != n {
		t.Error("item kept retrying with AutoRetry disabled")
	}
}
