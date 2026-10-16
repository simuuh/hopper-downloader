package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
)

// debridFake claims every example.com URL; fetchErr (if set) is returned
// from FetchDownloadURL, otherwise directURL.
type debridFake struct {
	fakeHoster
	fetchErr  error
	directURL string

	mu      sync.Mutex
	fetches int
}

func (d *debridFake) Resolve(string, *plugins.Account) (*plugins.FileInfo, error) {
	return &plugins.FileInfo{Name: "file.bin", Available: true}, nil
}

func (d *debridFake) FetchDownloadURL(string, *plugins.Account) (string, error) {
	d.mu.Lock()
	d.fetches++
	d.mu.Unlock()
	if d.fetchErr != nil {
		return "", d.fetchErr
	}
	return d.directURL, nil
}

func (d *debridFake) fetchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fetches
}

func startUnsupportedHostManager(t *testing.T, accounts AccountLookup, hosters ...plugins.Hoster) *Manager {
	t.Helper()
	eng := downloader.New(downloader.Config{BaseDir: t.TempDir(), RetryMax: 1, RetryBackoffBase: 5 * time.Millisecond})
	mgr, err := New(newTestStore(t), plugins.NewRegistry(hosters...), eng, accounts, nil, nil, nil, scheduler.New(), discardLogger(), Config{PollInterval: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr.Start(ctx)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = mgr.Shutdown(sctx)
	})
	return mgr
}

func TestRun_UnsupportedHostFallsThroughToNextPluginWithoutCooldown(t *testing.T) {
	srv := staticFileServer(t, []byte("payload"))
	second := &debridFake{fakeHoster: fakeHoster{name: "debrid-b", matchHosts: []string{"files.example.com"}}, directURL: srv.URL + "/file.bin"}
	first := &debridFake{fakeHoster: fakeHoster{name: "debrid-a", matchHosts: []string{"files.example.com"}}, fetchErr: plugins.ErrUnsupportedHost}
	accounts := &rotatingAccounts{ids: []string{"acc"}, failed: map[string]bool{}}

	// Registered last = preferred by default, so debrid-a is tried first.
	mgr := startUnsupportedHostManager(t, accounts, second, first)
	it, err := mgr.AddLink("https://files.example.com/f/1", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForState(t, mgr, it.ID, StateDone, 3*time.Second)

	if first.fetchCount() != 1 || second.fetchCount() != 1 {
		t.Errorf("fetches: first=%d second=%d, want 1/1", first.fetchCount(), second.fetchCount())
	}
	if done.HosterKey != "debrid-b" {
		t.Errorf("HosterKey = %q, want debrid-b", done.HosterKey)
	}
	if len(accounts.reported) != 0 {
		t.Errorf("accounts reported %v, want none (ErrUnsupportedHost must not cool down the account)", accounts.reported)
	}
}

func TestRun_UnsupportedHostWithNoOtherPluginIsTransient(t *testing.T) {
	only := &debridFake{fakeHoster: fakeHoster{name: "debrid-a", matchHosts: []string{"files.example.com"}}, fetchErr: plugins.ErrUnsupportedHost}
	accounts := &rotatingAccounts{ids: []string{"acc"}, failed: map[string]bool{}}

	eng := downloader.New(downloader.Config{BaseDir: t.TempDir()})
	mgr, err := New(newTestStore(t), plugins.NewRegistry(only), eng, accounts, nil, nil, nil, scheduler.New(), discardLogger(), Config{PollInterval: 15 * time.Millisecond, AutoRetry: 2, AutoRetryDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink("https://files.example.com/f/1", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := mgr.Get(it.ID); got.RetryAfter != nil {
			if got.State != StateQueued || got.Attempts != 1 {
				t.Fatalf("item = %+v, want queued for an automatic retry", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := mgr.Get(it.ID)
	t.Fatalf("no automatic retry scheduled: %+v", got)
}
