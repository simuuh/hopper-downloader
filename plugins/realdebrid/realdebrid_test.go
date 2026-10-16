package realdebrid

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

// fakeTokens is a minimal TokenSource for tests. password, when set
// without token, exercises the Web-UI-added-account path: no Extra at
// all, just a Password (see plugins.TokenFromAccount's doc comment).
type fakeTokens struct {
	token    string
	password string
}

func (f *fakeTokens) AccountFor(hosterKey string) (*plugins.Account, bool) {
	if hosterKey != pluginName || (f.token == "" && f.password == "") {
		return nil, false
	}
	acc := &plugins.Account{Password: f.password}
	if f.token != "" {
		acc.Extra = map[string]string{"api_token": f.token}
	}
	return acc, true
}

func newTestPlugin(t *testing.T, handler http.Handler, token string) *Plugin {
	t.Helper()
	return newTestPluginWithTokens(t, handler, &fakeTokens{token: token})
}

func newTestPluginWithTokens(t *testing.T, handler http.Handler, tokens TokenSource) *Plugin {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p := New(tokens, nil)
	p.baseURL = srv.URL
	p.retryBaseDelay = time.Millisecond
	return p
}

func hostsStatusHandler(t *testing.T, calls *int32, hosts map[string]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if r.URL.Path != "/hosts/status" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hosts)
	}
}

func TestName(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.Name() != "realdebrid" {
		t.Errorf("Name() = %q, want realdebrid", p.Name())
	}
}

func TestMatch_NoAccountConfigured(t *testing.T) {
	var calls int32
	handler := hostsStatusHandler(t, &calls, map[string]map[string]any{
		"rapidgator.net": {"supported": 1, "status": "up"},
	})
	p := newTestPlugin(t, handler, "") // no token configured

	if p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match to return false with no account configured")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("expected no /hosts/status call when no account is configured")
	}
}

func TestMatch_SupportedHost(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"rapidgator.net": {"supported": 1, "status": "up"},
		"1fichier.com":   {"supported": 1, "status": "up"},
	})
	p := newTestPlugin(t, handler, "test-token")

	if !p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match true for a supported host")
	}
	if !p.Match("https://www.1fichier.com/?abc") {
		t.Error("expected Match true for a supported host with a www. subdomain")
	}
}

// TestMatch_RgToMirrorsRapidgatorSupport covers hide.cx's Click'n'Load
// output: hoster sites (and hide.cx) sometimes send rg.to, Rapidgator's
// own URL shortener (rg.to/file/<hash> -> rapidgator.net/file/...), but
// /hosts/status never lists "rg.to" as its own entry, only
// "rapidgator.net" — fetchHostsStatus mirrors that support onto rg.to so
// Match (and therefore FetchDownloadURL/Resolve, which just forward
// whatever URL matched straight to RD's own API) recognizes it too.
func TestMatch_RgToMirrorsRapidgatorSupport(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"rapidgator.net": {"supported": 1, "status": "up"},
	})
	p := newTestPlugin(t, handler, "test-token")

	if !p.Match("https://rg.to/file/abc123") {
		t.Error("expected Match true for rg.to when rapidgator.net is supported")
	}
	if !p.Match("https://www.rg.to/file/abc123") {
		t.Error("expected Match true for a rg.to www. subdomain")
	}
}

func TestMatch_RgToNotAddedWhenRapidgatorUnsupported(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"rapidgator.net": {"supported": 0, "status": "down"},
	})
	p := newTestPlugin(t, handler, "test-token")

	if p.Match("https://rg.to/file/abc123") {
		t.Error("expected Match false for rg.to when rapidgator.net itself is unsupported")
	}
}

func TestMatch_RgToNotAddedWhenRapidgatorAbsent(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"1fichier.com": {"supported": 1, "status": "up"},
	})
	p := newTestPlugin(t, handler, "test-token")

	if p.Match("https://rg.to/file/abc123") {
		t.Error("expected Match false for rg.to when rapidgator.net isn't in /hosts/status at all")
	}
}

// TestMatch_RgToExplicitEntryNotOverwritten guards fetchHostsStatus's
// defensive check: if Real-Debrid's API ever does start listing rg.to
// explicitly, that real entry must win over the synthetic mirror.
func TestMatch_RgToExplicitEntryNotOverwritten(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"rapidgator.net": {"supported": 1, "status": "up"},
		"rg.to":          {"supported": 0, "status": "down"},
	})
	p := newTestPlugin(t, handler, "test-token")

	if p.Match("https://rg.to/file/abc123") {
		t.Error("expected Match false: rg.to's own explicit (unsupported) entry should not be overwritten by the rapidgator.net mirror")
	}
}

func TestMatch_UnsupportedOrUnknownHost(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"rapidgator.net": {"supported": 0, "status": "down"},
	})
	p := newTestPlugin(t, handler, "test-token")

	if p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match false for a host marked unsupported")
	}
	if p.Match("https://example.com/file") {
		t.Error("expected Match false for a host not in /hosts/status at all")
	}
}

func TestMatch_InvalidURL(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{})
	p := newTestPlugin(t, handler, "test-token")

	if p.Match("not a url at all") {
		t.Error("expected Match false for an unparseable URL")
	}
	if p.Match("ftp://example.com/file") {
		t.Error("expected Match false for a non-http(s) scheme")
	}
}

func TestMatch_CachesHostsStatus(t *testing.T) {
	var calls int32
	handler := hostsStatusHandler(t, &calls, map[string]map[string]any{
		"rapidgator.net": {"supported": 1, "status": "up"},
	})
	p := newTestPlugin(t, handler, "test-token")

	for i := 0; i < 5; i++ {
		p.Match("https://rapidgator.net/file")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("/hosts/status called %d times, want 1 (cached within TTL)", got)
	}
}

func TestMatch_StaleCacheUsedWhenRefreshFails(t *testing.T) {
	var fail int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]map[string]any{
			"rapidgator.net": {"supported": 1, "status": "up"},
		})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), "test-token")

	if !p.Match("https://rapidgator.net/file") {
		t.Fatal("expected initial Match to succeed and populate the cache")
	}

	// Force the cache to look stale, then make the server start failing.
	p.mu.Lock()
	p.hostsFetchedAt = time.Now().Add(-2 * hostsCacheTTL)
	p.mu.Unlock()
	atomic.StoreInt32(&fail, 1)

	if !p.Match("https://rapidgator.net/file") {
		t.Error("expected Match to fall back to the stale cache when refresh fails")
	}
}

func TestDomains_ReflectsCache(t *testing.T) {
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"rapidgator.net": {"supported": 1, "status": "up"},
		"1fichier.com":   {"supported": 1, "status": "up"},
	})
	p := newTestPlugin(t, handler, "test-token")

	if len(p.Domains()) != 0 {
		t.Error("expected empty Domains before any Match call")
	}
	p.Match("https://rapidgator.net/file")
	domains := p.Domains()
	// 2, matching the fixture exactly: rg.to's alias to rapidgator.net is
	// now applied at Match-time (see hostSupported), not mirrored into
	// the cached hosts map fetchHostsStatus populates, so Domains() (which
	// reads that map directly) reports only what /hosts/status actually
	// returned — see TestMatch_RgToMirrorsRapidgatorSupport for the
	// Match-level behavior this used to guarantee via the map instead.
	if len(domains) != 2 {
		t.Errorf("Domains() = %v, want 2 entries (1fichier.com, rapidgator.net)", domains)
	}
}

func unrestrictHandler(t *testing.T, checkResp, linkResp map[string]any, checkStatus, linkStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want Bearer test-token", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.FormValue("link") == "" {
			t.Error("expected non-empty link form value")
		}

		switch r.URL.Path {
		case "/unrestrict/check":
			if r.Method != http.MethodPost {
				t.Errorf("unrestrict/check method = %s, want POST", r.Method)
			}
			w.WriteHeader(checkStatus)
			_ = json.NewEncoder(w).Encode(checkResp)
		case "/unrestrict/link":
			if r.Method != http.MethodPost {
				t.Errorf("unrestrict/link method = %s, want POST", r.Method)
			}
			w.WriteHeader(linkStatus)
			_ = json.NewEncoder(w).Encode(linkResp)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}
}

func TestResolve_Success(t *testing.T) {
	handler := unrestrictHandler(t, map[string]any{
		"filename":  "movie.mkv",
		"filesize":  123456,
		"supported": 1,
	}, nil, http.StatusOK, 0)
	p := newTestPlugin(t, handler, "test-token")

	info, err := p.Resolve("https://rapidgator.net/file/123", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != "movie.mkv" || info.Size != 123456 || !info.Available {
		t.Errorf("unexpected FileInfo: %+v", info)
	}
}

func TestResolve_UsesAccountFromArgumentOverTokenSource(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer arg-token" {
			t.Errorf("Authorization = %q, want Bearer arg-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"filename": "f", "filesize": 1, "supported": 1})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), "source-token")

	_, err := p.Resolve("https://rapidgator.net/f", &plugins.Account{Extra: map[string]string{"api_token": "arg-token"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestResolve_NoAccountConfigured(t *testing.T) {
	p := newTestPlugin(t, http.NotFoundHandler(), "")
	_, err := p.Resolve("https://rapidgator.net/f", nil)
	if err != plugins.ErrAccountRequired {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestResolve_Unavailable(t *testing.T) {
	handler := unrestrictHandler(t, map[string]any{}, nil, http.StatusServiceUnavailable, 0)
	p := newTestPlugin(t, handler, "test-token")

	_, err := p.Resolve("https://rapidgator.net/f", nil)
	if err != plugins.ErrUnavailable {
		t.Errorf("err = %v, want plugins.ErrUnavailable", err)
	}
}

func TestResolve_BadToken(t *testing.T) {
	handler := unrestrictHandler(t, map[string]any{}, nil, http.StatusUnauthorized, 0)
	p := newTestPlugin(t, handler, "test-token")

	_, err := p.Resolve("https://rapidgator.net/f", nil)
	if err != plugins.ErrAccountRequired {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestFetchDownloadURL_Success(t *testing.T) {
	handler := unrestrictHandler(t, nil, map[string]any{
		"download": "https://xyz.real-debrid.com/d/abc/movie.mkv",
	}, 0, http.StatusOK)
	p := newTestPlugin(t, handler, "test-token")

	got, err := p.FetchDownloadURL("https://rapidgator.net/file/123", nil)
	if err != nil {
		t.Fatalf("FetchDownloadURL: %v", err)
	}
	if got != "https://xyz.real-debrid.com/d/abc/movie.mkv" {
		t.Errorf("got %q", got)
	}
}

func TestFetchDownloadURL_EmptyDownloadIsUnavailable(t *testing.T) {
	handler := unrestrictHandler(t, nil, map[string]any{"download": ""}, 0, http.StatusOK)
	p := newTestPlugin(t, handler, "test-token")

	_, err := p.FetchDownloadURL("https://rapidgator.net/f", nil)
	if err == nil {
		t.Fatal("expected an error for an empty download URL")
	}
}

func TestDo_RetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"filename": "f", "filesize": 1, "supported": 1})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), "test-token")

	info, err := p.Resolve("https://rapidgator.net/f", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != "f" {
		t.Errorf("unexpected result after retry: %+v", info)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server called %d times, want 3 (2 x 429 + 1 success)", got)
	}
}

func TestDo_ExhaustsRetriesReturnsRateLimited(t *testing.T) {
	var calls int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), "test-token")

	_, err := p.Resolve("https://rapidgator.net/f", nil)
	if err != plugins.ErrRateLimited {
		t.Errorf("err = %v, want plugins.ErrRateLimited", err)
	}
	if got := atomic.LoadInt32(&calls); got != max429Retries+1 {
		t.Errorf("server called %d times, want %d", got, max429Retries+1)
	}
}

func TestHostnameOf(t *testing.T) {
	cases := map[string]string{
		"https://Rapidgator.NET/file": "rapidgator.net",
		"http://example.com":          "example.com",
		"ftp://example.com":           "",
		"not a url":                   "",
		"":                            "",
	}
	for in, want := range cases {
		if got := hostnameOf(in); got != want {
			t.Errorf("hostnameOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPluginSatisfiesHosterInterface(t *testing.T) {
	var _ plugins.Hoster = (*Plugin)(nil)
}
