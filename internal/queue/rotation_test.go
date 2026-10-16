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

// rotatingAccounts hands out the first account not reported as failed,
// like internal/account.Manager's cooldown does.
type rotatingAccounts struct {
	mu       sync.Mutex
	ids      []string
	failed   map[string]bool
	reported []string
}

func (r *rotatingAccounts) AccountFor(string) (*plugins.Account, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.ids {
		if !r.failed[id] {
			return &plugins.Account{ID: id, Username: id}, true
		}
	}
	return &plugins.Account{ID: r.ids[0], Username: r.ids[0]}, true
}

func (r *rotatingAccounts) ReportFailure(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed[id] = true
	r.reported = append(r.reported, id)
	return true
}

// pickyHoster rejects every account except okID with ErrAccountRequired.
type pickyHoster struct {
	fakeHoster
	okID       string
	directURL  string
	mu         sync.Mutex
	fetchedFor []string
}

func (p *pickyHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	return &plugins.FileInfo{Name: "file.bin", Available: true}, nil
}

func (p *pickyHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	p.mu.Lock()
	p.fetchedFor = append(p.fetchedFor, acc.ID)
	p.mu.Unlock()
	if acc.ID != p.okID {
		return "", plugins.ErrAccountRequired
	}
	return p.directURL, nil
}

func TestRun_RotatesToNextAccountWhenHosterRejectsOne(t *testing.T) {
	srv := staticFileServer(t, []byte("payload"))
	hoster := &pickyHoster{
		fakeHoster: fakeHoster{name: "rapidgator", matchHosts: []string{"rapidgator.net"}},
		okID:       "acc-2",
		directURL:  srv.URL + "/file.bin",
	}
	accounts := &rotatingAccounts{ids: []string{"acc-1", "acc-2"}, failed: map[string]bool{}}

	eng := downloader.New(downloader.Config{BaseDir: t.TempDir(), RetryMax: 1, RetryBackoffBase: 5 * time.Millisecond})
	mgr, err := New(newTestStore(t), plugins.NewRegistry(hoster), eng, accounts, nil, nil, nil, scheduler.New(), discardLogger(), Config{PollInterval: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = mgr.Shutdown(sctx)
	})

	it, err := mgr.AddLink("https://rapidgator.net/file/abc/file.bin.html", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, it.ID, StateDone, 3*time.Second)

	if len(accounts.reported) != 1 || accounts.reported[0] != "acc-1" {
		t.Errorf("reported = %v, want [acc-1]", accounts.reported)
	}
	hoster.mu.Lock()
	defer hoster.mu.Unlock()
	if len(hoster.fetchedFor) != 2 || hoster.fetchedFor[1] != "acc-2" {
		t.Errorf("FetchDownloadURL called for %v, want [acc-1 acc-2]", hoster.fetchedFor)
	}
}

func TestRun_NoRotationLoopWhenEveryAccountIsRejected(t *testing.T) {
	hoster := &pickyHoster{
		fakeHoster: fakeHoster{name: "rapidgator", matchHosts: []string{"rapidgator.net"}},
		okID:       "none",
	}
	accounts := &rotatingAccounts{ids: []string{"acc-1", "acc-2"}, failed: map[string]bool{}}

	eng := downloader.New(downloader.Config{BaseDir: t.TempDir()})
	mgr, err := New(newTestStore(t), plugins.NewRegistry(hoster), eng, accounts, nil, nil, nil, scheduler.New(), discardLogger(), Config{PollInterval: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.AddLink("https://rapidgator.net/file/abc/file.bin.html", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, it.ID, StateError, 3*time.Second)

	hoster.mu.Lock()
	defer hoster.mu.Unlock()
	if len(hoster.fetchedFor) != 2 {
		t.Errorf("FetchDownloadURL called %d times (%v), want each account exactly once", len(hoster.fetchedFor), hoster.fetchedFor)
	}
}
