package realdebrid

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

// newValidateTestPlugin builds a Plugin whose test server answers GET
// /user with (status, body) and GET /hosts/status with a benign, always-
// empty 200 response — ValidateAccount's own WarmHostsCache call hits
// that endpoint too on a successful validation, and tests here only
// care about the /user response.
func newValidateTestPlugin(t *testing.T, tokens TokenSource, status int, body map[string]any) *Plugin {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method for /user: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/hosts/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]map[string]any{})
	})
	return newTestPluginWithTokens(t, mux, tokens)
}

func TestValidateAccount_Success(t *testing.T) {
	p := newValidateTestPlugin(t, &fakeTokens{token: "test-token"}, http.StatusOK, map[string]any{
		"type":       "premium",
		"expiration": "2026-12-31T00:00:00.000Z",
	})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_token": "test-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true")
	}
	if info.ExpiresAt == nil || info.ExpiresAt.Format("2006-01-02") != "2026-12-31" {
		t.Errorf("ExpiresAt = %v, want 2026-12-31", info.ExpiresAt)
	}
	if info.Label != "Premium until 2026-12-31" {
		t.Errorf("Label = %q, want %q", info.Label, "Premium until 2026-12-31")
	}
}

func TestValidateAccount_InvalidToken(t *testing.T) {
	p := newValidateTestPlugin(t, &fakeTokens{token: "bad-token"}, http.StatusUnauthorized, map[string]any{})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_token": "bad-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount returned an error, want a (Valid: false) result: %v", err)
	}
	if info.Valid {
		t.Error("expected Valid = false")
	}
	if info.Label != "Invalid API token" {
		t.Errorf("Label = %q, want %q", info.Label, "Invalid API token")
	}
}

func TestValidateAccount_NoTokenConfigured(t *testing.T) {
	p := newTestPlugin(t, http.NotFoundHandler(), "")

	_, err := p.ValidateAccount(nil)
	if err != plugins.ErrAccountRequired {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestValidateAccount_FallsBackToPassword(t *testing.T) {
	// Web UI "Add Account" only collects username/password: no Extra at
	// all. ValidateAccount must still work with the token stored as
	// Password (see plugins.TokenFromAccount).
	p := newValidateTestPlugin(t, &fakeTokens{password: "webui-token"}, http.StatusOK, map[string]any{"type": "premium"})

	info, err := p.ValidateAccount(&plugins.Account{Password: "webui-token"})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true using the password-fallback token")
	}
}

func TestValidateAccount_WarmsHostsCache(t *testing.T) {
	var hostsCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "premium"})
	})
	mux.HandleFunc("/hosts/status", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hostsCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]map[string]any{
			"rapidgator.net": {"supported": 1, "status": "up"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := New(&fakeTokens{token: "test-token"}, nil)
	p.baseURL = srv.URL

	if atomic.LoadInt32(&hostsCalls) != 0 {
		t.Fatal("setup: /hosts/status should not have been called yet")
	}

	if _, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_token": "test-token"}}); err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if got := atomic.LoadInt32(&hostsCalls); got != 1 {
		t.Fatalf("/hosts/status called %d times after ValidateAccount, want 1 (cache pre-warmed)", got)
	}

	// Match should now succeed without triggering another /hosts/status
	// fetch (still within the cache TTL just populated by the warm).
	if !p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match to succeed against the pre-warmed cache")
	}
	if got := atomic.LoadInt32(&hostsCalls); got != 1 {
		t.Errorf("/hosts/status called %d times, want still 1 (Match reused the warmed cache)", got)
	}
}

// TestValidateAccount_FreeAccountReportsNoExpiry covers RD's documented
// /user response for a free account: {"type": "free"}, no "expiration"
// field at all.
func TestValidateAccount_FreeAccountReportsNoExpiry(t *testing.T) {
	p := newValidateTestPlugin(t, &fakeTokens{token: "test-token"}, http.StatusOK, map[string]any{"type": "free"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_token": "test-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true for a free account")
	}
	if info.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil for a free account", info.ExpiresAt)
	}
	if info.Label != "Free account (no expiry)" {
		t.Errorf("Label = %q, want %q", info.Label, "Free account (no expiry)")
	}
}

// TestValidateAccount_FreeAccountIgnoresBogusExpiration is the actual bug
// this fix targets: a free account reporting type=="free" alongside some
// stray/placeholder "expiration" value (not confirmed to happen, but not
// documented as impossible either) must still be reported as having no
// expiry — a free account genuinely has none, regardless of what that
// field happens to contain.
func TestValidateAccount_FreeAccountIgnoresBogusExpiration(t *testing.T) {
	p := newValidateTestPlugin(t, &fakeTokens{token: "test-token"}, http.StatusOK, map[string]any{
		"type":       "free",
		"expiration": "1970-01-01T00:00:00.000Z",
	})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_token": "test-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if info.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil — a free account's expiration field must be ignored", info.ExpiresAt)
	}
	if info.Label != "Free account (no expiry)" {
		t.Errorf("Label = %q, want %q", info.Label, "Free account (no expiry)")
	}
}

// TestValidateAccount_InvalidTokenClearsHostsCache guards the bug behind
// queue.Manager's plugin-fallback fix: without ClearHostsCache, a token
// that goes bad *after* the host cache was already warmed (the realistic
// case — an account that worked fine and only later expired/got revoked)
// would leave Match() matching against the pre-revocation snapshot for up
// to hostsCacheTTL, since Match's own stale-cache fallback exists to
// tolerate a transient refresh failure and can't otherwise tell that apart
// from a confirmed-permanent one.
func TestValidateAccount_InvalidTokenClearsHostsCache(t *testing.T) {
	var status atomic.Int32
	status.Store(int32(http.StatusOK))

	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if s := int(status.Load()); s != http.StatusOK {
			w.WriteHeader(s)
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "premium"})
	})
	mux.HandleFunc("/hosts/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if s := int(status.Load()); s != http.StatusOK {
			w.WriteHeader(s)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]map[string]any{
			"rapidgator.net": {"supported": 1, "status": "up"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := New(&fakeTokens{token: "test-token"}, nil)
	p.baseURL = srv.URL
	acc := &plugins.Account{Extra: map[string]string{"api_token": "test-token"}}

	// Starts out valid: ValidateAccount warms the cache, Match succeeds.
	if _, err := p.ValidateAccount(acc); err != nil {
		t.Fatalf("ValidateAccount (valid): %v", err)
	}
	if !p.Match("https://rapidgator.net/file/123") {
		t.Fatal("setup: expected Match to succeed against the warmed cache")
	}

	// Token is now revoked/expired: both endpoints start returning 401.
	status.Store(int32(http.StatusUnauthorized))
	info, err := p.ValidateAccount(acc)
	if err != nil {
		t.Fatalf("ValidateAccount (invalid): %v", err)
	}
	if info.Valid {
		t.Fatal("expected Valid = false")
	}

	if p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match to return false once the token is confirmed invalid, not fall back to the stale pre-revocation cache")
	}
}

func TestMatch_UsesPasswordFallbackToken(t *testing.T) {
	// The exact bug this guards: an RD account added through the Web UI
	// has no Extra at all, only Password. Before the fix, currentToken
	// returned "" for such an account and Match failed silently.
	handler := hostsStatusHandler(t, nil, map[string]map[string]any{
		"rapidgator.net": {"supported": 1, "status": "up"},
	})
	p := newTestPluginWithTokens(t, handler, &fakeTokens{password: "test-token"})

	if !p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match true using a token stored only as Password")
	}
}

func TestPluginSatisfiesAccountValidator(t *testing.T) {
	var _ plugins.AccountValidator = (*Plugin)(nil)
}
