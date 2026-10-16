package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

// freeResult scripts one FetchFreeDownloadURL response.
type freeResult struct {
	url string
	err error
}

// freeFallbackHoster simulates a plugin whose premium FetchDownloadURL
// always fails with plugins.ErrAccountRequired (e.g. a configured but
// non-premium/free-tier account) while also implementing
// plugins.FreeDownloader. freeResults is consumed in order, one per
// FetchFreeDownloadURL call — the last entry repeats for any call beyond
// the scripted list — so a test can script "captcha required, then
// success after the solution is submitted" as two entries.
type freeFallbackHoster struct {
	name string

	mu          sync.Mutex
	freeCalls   int
	lastAcc     *plugins.Account
	freeResults []freeResult
}

func (h *freeFallbackHoster) Name() string      { return h.name }
func (h *freeFallbackHoster) Domains() []string { return nil }
func (h *freeFallbackHoster) Match(string) bool { return true }
func (h *freeFallbackHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	return &plugins.FileInfo{Name: "f", Available: true}, nil
}
func (h *freeFallbackHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	return "", plugins.ErrAccountRequired
}
func (h *freeFallbackHoster) FetchFreeDownloadURL(ctx context.Context, url string, acc *plugins.Account) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx := h.freeCalls
	h.freeCalls++
	h.lastAcc = acc
	if idx >= len(h.freeResults) {
		idx = len(h.freeResults) - 1
	}
	r := h.freeResults[idx]
	return r.url, r.err
}

func (h *freeFallbackHoster) lastAccount() *plugins.Account {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastAcc
}

// waitForCondition polls until check reports true or timeout elapses,
// used here for the RetryAfter/CaptchaURL fields, which — unlike a
// terminal State waitForState already covers — are set on an item that
// deliberately stays StateQueued.
func waitForCondition(t *testing.T, mgr *Manager, id string, timeout time.Duration, check func(*Item) bool) *Item {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		it, ok := mgr.Get(id)
		if ok && check(it) {
			return it
		}
		time.Sleep(5 * time.Millisecond)
	}
	it, _ := mgr.Get(id)
	t.Fatalf("timed out waiting for condition on item %s; last seen: %+v", id, it)
	return nil
}

// TestRun_FreeFallback_SucceedsWhenPremiumFailsAndNoOtherPlugin covers the
// success path of the free-download fallback: the plugin's normal
// FetchDownloadURL fails with ErrAccountRequired (e.g. the configured
// account is free-tier, not premium), no other plugin can pick up the
// URL, and the plugin's own FreeDownloader succeeds outright.
func TestRun_FreeFallback_SucceedsWhenPremiumFailsAndNoOtherPlugin(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)

	h := &freeFallbackHoster{name: "onefichier", freeResults: []freeResult{{url: srv.URL + "/file.txt"}}}
	reg := plugins.NewRegistry(h)
	accounts := &fakeAccountLookup{}
	accounts.SetAccount("onefichier")
	mgr := newResolveTestManager(t, reg, accounts)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	mgr.dispatch(context.Background())
	final := waitForState(t, mgr, it.ID, StateDone, 2*time.Second)
	if final.HosterKey != "onefichier" {
		t.Errorf("HosterKey = %q, want onefichier", final.HosterKey)
	}
}

// TestRun_FreeFallback_ErrRetryAfterSchedulesRetryNotFailure covers the
// wait-and-retry outcome: the item must stay StateQueued (not StateError)
// with RetryAfter set to a future time, and no Error message — a hoster
// asking the client to wait is not a failure.
func TestRun_FreeFallback_ErrRetryAfterSchedulesRetryNotFailure(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)

	h := &freeFallbackHoster{name: "onefichier", freeResults: []freeResult{{err: plugins.ErrRetryAfter{Seconds: 3600}}}}
	reg := plugins.NewRegistry(h)
	accounts := &fakeAccountLookup{}
	accounts.SetAccount("onefichier")
	mgr := newResolveTestManager(t, reg, accounts)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	mgr.dispatch(context.Background())
	got := waitForCondition(t, mgr, it.ID, 2*time.Second, func(it *Item) bool { return it.RetryAfter != nil })

	if got.State != StateQueued {
		t.Errorf("State = %q, want queued (a scheduled retry is not a failure)", got.State)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}
	if !got.RetryAfter.After(time.Now()) {
		t.Errorf("RetryAfter = %v, want a future time roughly 3600s out", got.RetryAfter)
	}
}

// TestRun_FreeFallback_ErrCaptchaRequiredPausesForSolution covers the
// captcha outcome: CaptchaURL is set to the challenge image, the item
// stays StateQueued (not runnable again until SolveCaptcha clears it).
func TestRun_FreeFallback_ErrCaptchaRequiredPausesForSolution(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)

	const imageURL = "https://example.com/captcha.png"
	h := &freeFallbackHoster{name: "onefichier", freeResults: []freeResult{{err: plugins.ErrCaptchaRequired{ImageURL: imageURL}}}}
	reg := plugins.NewRegistry(h)
	accounts := &fakeAccountLookup{}
	accounts.SetAccount("onefichier")
	mgr := newResolveTestManager(t, reg, accounts)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	mgr.dispatch(context.Background())
	got := waitForCondition(t, mgr, it.ID, 2*time.Second, func(it *Item) bool { return it.CaptchaURL != "" })

	if got.CaptchaURL != imageURL {
		t.Errorf("CaptchaURL = %q, want %q", got.CaptchaURL, imageURL)
	}
	if got.State != StateQueued {
		t.Errorf("State = %q, want queued", got.State)
	}
	if got.runnable(time.Now()) {
		t.Error("expected item to not be runnable while a captcha is pending")
	}
}

// TestSolveCaptcha_RetriesAndPassesSolutionToPlugin covers the full
// round trip: a submitted solution clears CaptchaURL, makes the item
// runnable again, and is handed to the plugin's next FetchFreeDownloadURL
// call via acc.Extra["captcha_solution"] (see withCaptchaSolution).
func TestSolveCaptcha_RetriesAndPassesSolutionToPlugin(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)

	h := &freeFallbackHoster{
		name: "onefichier",
		freeResults: []freeResult{
			{err: plugins.ErrCaptchaRequired{ImageURL: "https://example.com/captcha.png"}},
			{url: srv.URL + "/file.txt"},
		},
	}
	reg := plugins.NewRegistry(h)
	accounts := &fakeAccountLookup{}
	accounts.SetAccount("onefichier")
	mgr := newResolveTestManager(t, reg, accounts)

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	mgr.dispatch(context.Background())
	waitForCondition(t, mgr, it.ID, 2*time.Second, func(it *Item) bool { return it.CaptchaURL != "" })

	if err := mgr.SolveCaptcha(it.ID, "the-answer"); err != nil {
		t.Fatalf("SolveCaptcha: %v", err)
	}
	// These tests drive dispatch manually (no Start()), so SolveCaptcha's
	// own wakeAsync has nothing consuming it — re-trigger dispatch
	// explicitly, same as every other manually-driven test in this
	// package.
	mgr.dispatch(context.Background())

	final := waitForState(t, mgr, it.ID, StateDone, 2*time.Second)
	if final.CaptchaURL != "" {
		t.Errorf("CaptchaURL = %q, want cleared after success", final.CaptchaURL)
	}

	lastAcc := h.lastAccount()
	if lastAcc == nil || lastAcc.Extra["captcha_solution"] != "the-answer" {
		t.Errorf("plugin's second FetchFreeDownloadURL call got acc = %+v, want captcha_solution=the-answer", lastAcc)
	}
}

// TestSolveCaptcha_NoPendingCaptchaReturnsError covers the guard: solving
// a captcha for an item that has none pending must not silently succeed.
func TestSolveCaptcha_NoPendingCaptchaReturnsError(t *testing.T) {
	content := []byte("hello")
	srv := staticFileServer(t, content)
	reg := plugins.NewRegistry(&passthroughHoster{name: "fallback-plugin"})
	mgr := newResolveTestManager(t, reg, &fakeAccountLookup{})

	it, err := mgr.AddLink(srv.URL+"/file.txt", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	if err := mgr.SolveCaptcha(it.ID, "whatever"); err != ErrNoCaptchaPending {
		t.Errorf("SolveCaptcha err = %v, want ErrNoCaptchaPending", err)
	}
}
