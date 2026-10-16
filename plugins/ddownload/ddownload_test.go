package ddownload

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

// fakeTokens is a minimal TokenSource for tests. password, when set
// without token, exercises the Web-UI-added-account path (see
// plugins.TokenFromAccount's doc comment).
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
		acc.Extra = map[string]string{"api_key": f.token}
	}
	return acc, true
}

func newTestPlugin(t *testing.T, handler http.Handler, tokens TokenSource) *Plugin {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := New(tokens, nil)
	p.baseURL = srv.URL
	return p
}

func TestName(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.Name() != "ddownload" {
		t.Errorf("Name() = %q, want ddownload", p.Name())
	}
}

func TestDomains(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if got := p.Domains(); len(got) != 1 || got[0] != "ddownload.com" {
		t.Errorf("Domains() = %v, want [ddownload.com]", got)
	}
}

func TestCredentialed_IsTrue(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if !p.Credentialed() {
		t.Error("expected Credentialed() = true")
	}
}

func TestDisplayName(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.DisplayName() != "DDownload" {
		t.Errorf("DisplayName() = %q, want DDownload", p.DisplayName())
	}
}

func TestMatch(t *testing.T) {
	p := New(&fakeTokens{token: "test-key"}, nil)
	cases := map[string]bool{
		"https://ddownload.com/a1b2c3d4e5f6/example.part1.rar": true,
		"https://www.ddownload.com/abc123":                     true,
		"https://example.com/a1b2c3d4e5f6":                     false,
		"not-a-url":                                            false,
	}
	for u, want := range cases {
		if got := p.Match(u); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestMatch_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.Match("https://ddownload.com/a1b2c3d4e5f6") {
		t.Error("expected Match to return false with no account configured")
	}
}

func TestFileCodeFromURL(t *testing.T) {
	cases := map[string]string{
		"https://ddownload.com/a1b2c3d4e5f6/example.part1.rar": "a1b2c3d4e5f6",
		"https://ddownload.com/abc123":                         "abc123",
	}
	for u, want := range cases {
		got, err := fileCodeFromURL(u)
		if err != nil {
			t.Errorf("fileCodeFromURL(%q): %v", u, err)
			continue
		}
		if got != want {
			t.Errorf("fileCodeFromURL(%q) = %q, want %q", u, got, want)
		}
	}
}

func fileInfoHandler(t *testing.T, status int, msg string, result []map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/info" {
			t.Errorf("path = %q, want /file/info", r.URL.Path)
		}
		if got := r.URL.Query().Get("key"); got != "test-key" {
			t.Errorf("key = %q, want test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"msg":         msg,
			"server_time": "2026-08-23 18:00:00",
			"status":      status,
			"result":      result,
		})
	}
}

func TestResolve_Success(t *testing.T) {
	p := newTestPlugin(t, fileInfoHandler(t, 200, "OK", []map[string]any{
		{"status": 200, "filecode": "a1b2c3d4e5f6", "name": "movie.rar", "download": "5", "size": "123456789", "uploaded": "2026-01-01 00:00:00"},
	}), &fakeTokens{token: "test-key"})

	info, err := p.Resolve("https://ddownload.com/a1b2c3d4e5f6/movie.rar", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != "movie.rar" || info.Size != 123456789 || !info.Available {
		t.Errorf("info = %+v", info)
	}
}

func TestResolve_NotFound(t *testing.T) {
	p := newTestPlugin(t, fileInfoHandler(t, 200, "OK", []map[string]any{
		{"status": 404, "filecode": "gone"},
	}), &fakeTokens{token: "test-key"})

	_, err := p.Resolve("https://ddownload.com/gone/x", nil)
	if !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestResolve_Unavailable(t *testing.T) {
	p := newTestPlugin(t, fileInfoHandler(t, 200, "OK", []map[string]any{
		{"status": 451, "filecode": "blocked"},
	}), &fakeTokens{token: "test-key"})

	_, err := p.Resolve("https://ddownload.com/blocked/x", nil)
	if !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("err = %v, want plugins.ErrUnavailable", err)
	}
}

func TestResolve_InvalidKey(t *testing.T) {
	p := newTestPlugin(t, fileInfoHandler(t, 400, "Invalid key", nil), &fakeTokens{token: "test-key"})

	_, err := p.Resolve("https://ddownload.com/x/y", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestResolve_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	_, err := p.Resolve("https://ddownload.com/x/y", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

// TestFetchDownloadURL_NoWorkingMechanism guards the deliberate stub:
// FetchDownloadURL must never silently "succeed" with a fabricated URL —
// it always reports a clear, explicit failure (see its doc comment for
// the live investigation behind this).
func TestFetchDownloadURL_NoWorkingMechanism(t *testing.T) {
	p := New(&fakeTokens{token: "test-key"}, nil)
	_, err := p.FetchDownloadURL("https://ddownload.com/a1b2c3d4e5f6/movie.rar", nil)
	if err == nil {
		t.Fatal("expected an error — FetchDownloadURL has no working implementation")
	}
	if !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("err = %v, want it to wrap plugins.ErrUnavailable", err)
	}
}

func TestFetchDownloadURL_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	_, err := p.FetchDownloadURL("https://ddownload.com/x/y", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func accountInfoHandler(t *testing.T, wantKey string, status int, msg string, result map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/info" {
			t.Errorf("path = %q, want /account/info", r.URL.Path)
		}
		if got := r.URL.Query().Get("key"); got != wantKey {
			t.Errorf("key = %q, want %q", got, wantKey)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"msg":         msg,
			"server_time": "2026-08-23 18:00:00",
			"status":      status,
			"result":      result,
		})
	}
}

func TestValidateAccount_Success(t *testing.T) {
	p := newTestPlugin(t, accountInfoHandler(t, "test-key", 200, "OK", map[string]any{
		"storage_left":         "inf",
		"premium_traffic_left": 102400,
		"email":                "user@example.com",
		"premium_expire":       "2026-12-31 18:00:00",
		"balance":              "0",
		"traffic_used":         "0",
		"traffic_left":         "0",
		"storage_used":         "0",
	}), &fakeTokens{token: "test-key"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_key": "test-key"}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true")
	}
	if info.TrafficLeft == nil || *info.TrafficLeft != 102400 {
		t.Errorf("TrafficLeft = %v, want 102400", info.TrafficLeft)
	}
	if info.ExpiresAt == nil || info.ExpiresAt.Format("2006-01-02") != "2026-12-31" {
		t.Errorf("ExpiresAt = %v, want 2026-12-31", info.ExpiresAt)
	}
	if info.Label != "Premium until 2026-12-31" {
		t.Errorf("Label = %q, want %q", info.Label, "Premium until 2026-12-31")
	}
}

func TestValidateAccount_TrafficExhausted(t *testing.T) {
	p := newTestPlugin(t, accountInfoHandler(t, "test-key", 200, "OK", map[string]any{
		"premium_traffic_left": 0,
	}), &fakeTokens{token: "test-key"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_key": "test-key"}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if info.Label != "Traffic exhausted" {
		t.Errorf("Label = %q, want %q", info.Label, "Traffic exhausted")
	}
	if info.TrafficLeft == nil || *info.TrafficLeft != 0 {
		t.Errorf("TrafficLeft = %v, want 0", info.TrafficLeft)
	}
}

func TestValidateAccount_InvalidKey(t *testing.T) {
	p := newTestPlugin(t, accountInfoHandler(t, "test-key", 400, "Invalid key", nil), &fakeTokens{token: "test-key"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_key": "test-key"}})
	if err != nil {
		t.Fatalf("ValidateAccount returned an error, want a (Valid: false) result: %v", err)
	}
	if info.Valid {
		t.Error("expected Valid = false")
	}
}

func TestValidateAccount_NoKeyConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	_, err := p.ValidateAccount(nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestValidateAccount_FallsBackToPassword(t *testing.T) {
	p := newTestPlugin(t, accountInfoHandler(t, "webui-key", 200, "OK", map[string]any{}), &fakeTokens{password: "webui-key"})

	info, err := p.ValidateAccount(&plugins.Account{Password: "webui-key"})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true using the password-fallback key")
	}
}

func TestParseDDownloadTime(t *testing.T) {
	if parseDDownloadTime("") != nil {
		t.Error("expected nil for empty string")
	}
	got := parseDDownloadTime("2018-01-22 18:00:00")
	if got == nil || !got.Equal(time.Date(2018, 1, 22, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
	got = parseDDownloadTime("2026-12-31T00:00:00Z")
	if got == nil {
		t.Error("expected RFC3339 fallback to parse")
	}
	if parseDDownloadTime("not-a-date") != nil {
		t.Error("expected nil for an unparseable string")
	}
}

func TestPluginSatisfiesInterfaces(t *testing.T) {
	var _ plugins.Hoster = (*Plugin)(nil)
	var _ plugins.Credentialed = (*Plugin)(nil)
	var _ plugins.DisplayNamer = (*Plugin)(nil)
	var _ plugins.AccountValidator = (*Plugin)(nil)
}
