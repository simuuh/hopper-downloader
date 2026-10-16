package queue

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
)

// fakeHoster is a minimal plugins.Hoster for exercising matchHoster in
// isolation, without needing a real hoster implementation.
type fakeHoster struct {
	name       string
	matchHosts []string
}

func (f *fakeHoster) Name() string      { return f.name }
func (f *fakeHoster) Domains() []string { return f.matchHosts }
func (f *fakeHoster) Match(url string) bool {
	for _, h := range f.matchHosts {
		if strings.Contains(url, h) {
			return true
		}
	}
	return false
}
func (f *fakeHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	return nil, nil
}
func (f *fakeHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	return "", nil
}

// fakeAccountLookup reports an account as configured for exactly the
// hoster keys added via SetAccount. Safe for concurrent use, since
// Manager.run calls AccountFor from a worker goroutine.
type fakeAccountLookup struct {
	mu          sync.Mutex
	withAccount map[string]bool
}

func (f *fakeAccountLookup) AccountFor(hosterKey string) (*plugins.Account, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.withAccount[hosterKey] {
		return &plugins.Account{}, true
	}
	return nil, false
}

func (f *fakeAccountLookup) SetAccount(hosterKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.withAccount == nil {
		f.withAccount = make(map[string]bool)
	}
	f.withAccount[hosterKey] = true
}

// newResolveTestManager builds a Manager whose only exercised surface in
// these tests is matchHoster: registry and accounts are the interesting
// bits, the store/engine/scheduler are real-but-unused plumbing New()
// requires.
func newResolveTestManager(t *testing.T, reg *plugins.Registry, accounts AccountLookup) *Manager {
	t.Helper()
	store := newTestStore(t)
	eng := downloader.New(downloader.Config{BaseDir: t.TempDir()})
	mgr, err := New(store, reg, eng, accounts, nil, nil, nil, scheduler.New(), discardLogger(), Config{PollInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func TestMatchHoster_PrefersAccountHavingPluginRegardlessOfRegistrationOrder(t *testing.T) {
	// accountless is registered last, so plain Registry.Match (pure
	// reverse-registration-order) would prefer it over withAccount.
	// matchHoster must override that in favor of the account-having
	// plugin despite it being registered first.
	accountless := &fakeHoster{name: "rapidgator", matchHosts: []string{"rapidgator.net"}}
	withAccount := &fakeHoster{name: "realdebrid", matchHosts: []string{"rapidgator.net"}}

	reg := plugins.NewRegistry(withAccount, accountless) // accountless registered last = normally higher priority
	accounts := &fakeAccountLookup{withAccount: map[string]bool{"realdebrid": true}}
	mgr := newResolveTestManager(t, reg, accounts)

	h, ok := mgr.matchHoster("https://rapidgator.net/file/123")
	if !ok {
		t.Fatal("expected a match")
	}
	if h.Name() != "realdebrid" {
		t.Errorf("matched %q, want realdebrid (the account-having plugin) despite being registered first", h.Name())
	}
}

func TestMatchHoster_FallsBackWhenNoPluginHasAccount(t *testing.T) {
	rapidgatorLike := &fakeHoster{name: "rapidgator", matchHosts: []string{"rapidgator.net"}}
	reg := plugins.NewRegistry(rapidgatorLike)
	accounts := &fakeAccountLookup{withAccount: map[string]bool{}}
	mgr := newResolveTestManager(t, reg, accounts)

	h, ok := mgr.matchHoster("https://rapidgator.net/file/123")
	if !ok {
		t.Fatal("expected pass-2 fallback to still find a match")
	}
	if h.Name() != "rapidgator" {
		t.Errorf("matched %q, want rapidgator", h.Name())
	}
}

func TestMatchHoster_NilAccountsSkipsToPassTwo(t *testing.T) {
	rapidgatorLike := &fakeHoster{name: "rapidgator", matchHosts: []string{"rapidgator.net"}}
	reg := plugins.NewRegistry(rapidgatorLike)
	mgr := newResolveTestManager(t, reg, nil)

	h, ok := mgr.matchHoster("https://rapidgator.net/file/123")
	if !ok || h.Name() != "rapidgator" {
		t.Errorf("expected pass-2 match with nil accounts, got %v, %v", h, ok)
	}
}

func TestMatchHoster_NoMatch(t *testing.T) {
	rapidgatorLike := &fakeHoster{name: "rapidgator", matchHosts: []string{"rapidgator.net"}}
	reg := plugins.NewRegistry(rapidgatorLike)
	mgr := newResolveTestManager(t, reg, &fakeAccountLookup{})

	_, ok := mgr.matchHoster("https://example.com/file")
	if ok {
		t.Error("expected no match for an unsupported domain")
	}
}

func TestMatchHoster_AccountForWrongPluginDoesNotWin(t *testing.T) {
	// An account exists, but not for a plugin that actually matches this
	// URL — pass 1 must not pick it just because *some* account exists.
	a := &fakeHoster{name: "pluginA", matchHosts: []string{"a.example.com"}}
	b := &fakeHoster{name: "pluginB", matchHosts: []string{"b.example.com"}}
	reg := plugins.NewRegistry(a, b)
	accounts := &fakeAccountLookup{withAccount: map[string]bool{"pluginA": true}}
	mgr := newResolveTestManager(t, reg, accounts)

	h, ok := mgr.matchHoster("https://b.example.com/file")
	if !ok {
		t.Fatal("expected pass-2 fallback match")
	}
	if h.Name() != "pluginB" {
		t.Errorf("matched %q, want pluginB", h.Name())
	}
}

// passthroughHoster matches any URL and hands it straight to the
// downloader unchanged (like directhttp, but without the real HEAD
// request), so a full download can complete deterministically in a test
// without depending on which of several same-domain fake plugins is
// "supposed" to win — that's exactly what these tests are checking.
type passthroughHoster struct{ name string }

func (h *passthroughHoster) Name() string      { return h.name }
func (h *passthroughHoster) Domains() []string { return nil }
func (h *passthroughHoster) Match(string) bool { return true }
func (h *passthroughHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	return &plugins.FileInfo{Name: "f", Available: true}, nil
}
func (h *passthroughHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	return url, nil
}

// TestRun_ReResolvesHosterKeyWhenAccountAddedAfterQueueing exercises the
// bug found alongside the two-pass resolution work: run() used to look
// up the download account by the item's stored HosterKey (set once, at
// add time) while using a freshly re-matched Hoster to actually perform
// the download — so if an account was added after a link was queued but
// before it ran, the download would use the *new* plugin with the *old*
// (wrong, or missing) account. Dispatch is driven manually here (no
// Start()) so the account can be added at a precise, race-free point
// between queueing and running.
func TestRun_ReResolvesHosterKeyWhenAccountAddedAfterQueueing(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)

	// acctPlugin registered *first* so it only wins via pass-1 (account
	// match); fallback registered last so it wins the pass-2 tiebreak
	// while no account exists yet.
	acctPlugin := &passthroughHoster{name: "acct-plugin"}
	fallback := &passthroughHoster{name: "fallback-plugin"}
	reg := plugins.NewRegistry(acctPlugin, fallback)
	accounts := &fakeAccountLookup{}
	mgr := newResolveTestManager(t, reg, accounts)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if it.HosterKey != "fallback-plugin" {
		t.Fatalf("setup: expected initial match to be fallback-plugin (no account yet, pass-2 tiebreak), got %q", it.HosterKey)
	}

	// Now an account shows up for acct-plugin, after the item was
	// already queued but before it's run.
	accounts.SetAccount("acct-plugin")

	ctx := context.Background()
	mgr.dispatch(ctx)
	final := waitForState(t, mgr, it.ID, StateDone, 2*time.Second)

	if final.HosterKey != "acct-plugin" {
		t.Errorf("HosterKey = %q, want acct-plugin", final.HosterKey)
	}
}

// accountRequiredHoster simulates a plugin whose account still looks
// configured — so it wins matchHoster's pass 1 just like the real
// realdebrid plugin does as long as an api_token is present, regardless of
// whether that token still actually works — but whose Resolve/
// FetchDownloadURL calls fail with plugins.ErrAccountRequired once
// actually invoked, e.g. an expired or revoked token.
type accountRequiredHoster struct{ name string }

func (h *accountRequiredHoster) Name() string      { return h.name }
func (h *accountRequiredHoster) Domains() []string { return nil }
func (h *accountRequiredHoster) Match(string) bool { return true }
func (h *accountRequiredHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	return nil, plugins.ErrAccountRequired
}
func (h *accountRequiredHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	return "", plugins.ErrAccountRequired
}

// TestRun_FallsBackToAnotherPluginWhenResolveReturnsAccountRequired covers
// an expired Real-Debrid account: an item
// queued with hoster_key="realdebrid" kept failing forever with "hoster
// account required" on every retry, instead of automatically falling back
// to a native plugin (e.g. rapidgator) that still has a working account.
// The root cause isn't that run() bypasses matchHoster on retry — it
// already re-resolves the hoster fresh on every call (see
// TestRun_ReResolvesHosterKeyWhenAccountAddedAfterQueueing above) — it's
// that matchHoster picks a plugin based on whether an account is
// *configured*, which doesn't change just because that account's token
// has since expired, so a plain re-match deterministically picks the
// exact same broken plugin again. run() now reacts to the actual
// Resolve/FetchDownloadURL failure by excluding that plugin and
// re-matching (matchHosterExcluding) within the same attempt.
func TestRun_FallsBackToAnotherPluginWhenResolveReturnsAccountRequired(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)

	// broken is registered first so it only wins via pass-1 (it has a
	// configured account); fallback has no account and only matches via
	// pass-2 — exactly mirroring a configured-but-expired Real-Debrid
	// account versus a native Rapidgator plugin with its own account
	// wouldn't even be needed here, since pass-2 alone proves the
	// exclusion is doing the work, not an account-preference accident.
	broken := &accountRequiredHoster{name: "realdebrid"}
	fallback := &passthroughHoster{name: "rapidgator"}
	reg := plugins.NewRegistry(broken, fallback)
	accounts := &fakeAccountLookup{}
	accounts.SetAccount("realdebrid")
	mgr := newResolveTestManager(t, reg, accounts)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if it.HosterKey != "realdebrid" {
		t.Fatalf("setup: expected initial match to be realdebrid (pass-1, has the configured account), got %q", it.HosterKey)
	}

	ctx := context.Background()
	mgr.dispatch(ctx)
	final := waitForState(t, mgr, it.ID, StateDone, 2*time.Second)

	if final.HosterKey != "rapidgator" {
		t.Errorf("HosterKey = %q, want rapidgator (fallback after realdebrid's ErrAccountRequired)", final.HosterKey)
	}
}

// TestRun_FailsWithClearMessageWhenNoFallbackPluginAvailable covers the
// other half of the same fix: when every plugin that can handle the URL
// has already been tried and rejected with plugins.ErrAccountRequired,
// run() must give up with a clear, specific message rather than the
// generic "resolve: plugins: hoster account required" it used to.
func TestRun_FailsWithClearMessageWhenNoFallbackPluginAvailable(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)

	broken := &accountRequiredHoster{name: "realdebrid"}
	reg := plugins.NewRegistry(broken)
	accounts := &fakeAccountLookup{}
	accounts.SetAccount("realdebrid")
	mgr := newResolveTestManager(t, reg, accounts)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	ctx := context.Background()
	mgr.dispatch(ctx)
	final := waitForState(t, mgr, it.ID, StateError, 2*time.Second)

	const want = "no plugin with a configured account can handle this URL"
	if final.Error != want {
		t.Errorf("Error = %q, want %q", final.Error, want)
	}
}
