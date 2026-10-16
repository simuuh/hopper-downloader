package katfile

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

// fakeTokens is a minimal TokenSource for tests.
type fakeTokens struct {
	key1, key2 string
	username   string
	password   string
}

func (f *fakeTokens) AccountFor(hosterKey string) (*plugins.Account, bool) {
	if hosterKey != pluginName {
		return nil, false
	}
	if f.key1 != "" && f.key2 != "" {
		return &plugins.Account{Extra: map[string]string{"key1": f.key1, "key2": f.key2}}, true
	}
	if f.username != "" && f.password != "" {
		return &plugins.Account{Username: f.username, Password: f.password}, true
	}
	return nil, false
}

func newTestPlugin(t *testing.T, handler http.Handler, tokens TokenSource) *Plugin {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := New(tokens, nil)
	p.baseURL = srv.URL
	return p
}

func writeSuccess(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":      data,
		"_status":   "success",
		"_datetime": "2026-08-23 16:29:11",
	})
}

func writeError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"response":  message,
		"_status":   "error",
		"_datetime": "2026-08-23 16:29:11",
	})
}

// fakeAPI implements just enough of katfile's v2 shape to exercise the
// plugin: a working /authorize (fixed valid key1/key2 -> fixed token),
// /file/download and /file/info gated on that token, and configurable
// /account/package + /account/info for ValidateAccount tests.
type fakeAPI struct {
	t              *testing.T
	validKey1      string
	validKey2      string
	issuedToken    string
	issuedAccount  string
	rejectToken    bool // simulate an expired/invalidated token on the next call
	rejectedOnce   bool
	fileExists     bool
	fileID         string
	shortURL       string
	filename       string
	fileSize       string
	downloadURL    string
	pkgLabel       string
	pkgLevelType   string
	paidExpiryDate string
}

func (f *fakeAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			f.t.Fatalf("ParseForm: %v", err)
		}
		switch r.URL.Path {
		case "/authorize":
			f.handleAuthorize(w, r)
		case "/file/download":
			f.handleFileDownload(w, r)
		case "/file/info":
			f.handleFileInfo(w, r)
		case "/account/package":
			f.handleAccountPackage(w, r)
		case "/account/info":
			f.handleAccountInfo(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *fakeAPI) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("authorize: method = %s, want POST", r.Method)
	}
	key1, key2 := r.PostFormValue("key1"), r.PostFormValue("key2")
	if key1 != f.validKey1 || key2 != f.validKey2 {
		writeError(w, "Could not authenticate user. The username and password may be invalid or your account may be locked from too many failed logins..")
		return
	}
	writeSuccess(w, map[string]any{"access_token": f.issuedToken, "account_id": f.issuedAccount})
}

func (f *fakeAPI) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if f.rejectToken && !f.rejectedOnce {
		f.rejectedOnce = true
		writeError(w, "Could not validate access_token and account_id, please reauthenticate or try again.")
		return false
	}
	if r.PostFormValue("access_token") != f.issuedToken || r.PostFormValue("account_id") != f.issuedAccount {
		writeError(w, "Could not validate access_token and account_id, please reauthenticate or try again.")
		return false
	}
	return true
}

func (f *fakeAPI) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	if got := r.PostFormValue("short_url"); got != f.shortURL || !f.fileExists {
		writeError(w, "Could not find file based on file_id.")
		return
	}
	writeSuccess(w, map[string]any{"file_id": f.fileID, "filename": f.filename, "download_url": f.downloadURL})
}

func (f *fakeAPI) handleFileInfo(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	if got := r.PostFormValue("file_id"); got != f.fileID || !f.fileExists {
		writeError(w, "Could not find file based on file_id.")
		return
	}
	writeSuccess(w, map[string]any{"id": f.fileID, "filename": f.filename, "shortUrl": f.shortURL, "fileSize": f.fileSize})
}

func (f *fakeAPI) handleAccountPackage(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	writeSuccess(w, map[string]any{"label": f.pkgLabel, "level_type": f.pkgLevelType})
}

func (f *fakeAPI) handleAccountInfo(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	writeSuccess(w, map[string]any{"id": f.issuedAccount, "username": "user", "paidExpiryDate": f.paidExpiryDate})
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t:             t,
		validKey1:     "key1value",
		validKey2:     "key2value",
		issuedToken:   "access-token-abc",
		issuedAccount: "12345",
		fileExists:    true,
		fileID:        "1253",
		shortURL:      "k7l8m9n0p1q2",
		filename:      "example.part2.rar",
		fileSize:      "1073741824",
		downloadURL:   "https://katfile.cc/k7l8m9n0p1q2?download_token=abc123",
		pkgLabel:      "Premium Account",
		pkgLevelType:  "paid user",
	}
}

func TestName(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.Name() != "katfile" {
		t.Errorf("Name() = %q, want katfile", p.Name())
	}
}

func TestDomains(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	got := p.Domains()
	if len(got) != 2 || got[0] != "katfile.cc" || got[1] != "katfile.biz" {
		t.Errorf("Domains() = %v, want [katfile.cc katfile.biz]", got)
	}
}

func TestCredentialed_IsTrue(t *testing.T) {
	if !New(&fakeTokens{}, nil).Credentialed() {
		t.Error("expected Credentialed() = true")
	}
}

func TestDisplayName(t *testing.T) {
	if New(&fakeTokens{}, nil).DisplayName() != "Katfile" {
		t.Error("expected DisplayName() = Katfile")
	}
}

func TestMatch(t *testing.T) {
	p := New(&fakeTokens{key1: "a", key2: "b"}, nil)
	cases := map[string]bool{
		"https://katfile.cc/k7l8m9n0p1q2/movie.rar":  true,
		"https://katfile.biz/k7l8m9n0p1q2/movie.rar": true,
		"https://www.katfile.cc/abc":                 true,
		"https://example.com/k7l8m9n0p1q2":           false,
		"not-a-url":                                  false,
	}
	for u, want := range cases {
		if got := p.Match(u); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestMatch_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.Match("https://katfile.cc/k7l8m9n0p1q2") {
		t.Error("expected Match false with no account configured")
	}
}

func TestShortURLFromURL(t *testing.T) {
	cases := map[string]string{
		"https://katfile.cc/k7l8m9n0p1q2/example.part2.rar": "k7l8m9n0p1q2",
		"https://katfile.biz/abc123":                        "abc123",
	}
	for u, want := range cases {
		got, err := shortURLFromURL(u)
		if err != nil {
			t.Errorf("shortURLFromURL(%q): %v", u, err)
			continue
		}
		if got != want {
			t.Errorf("shortURLFromURL(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestResolve_Success(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: api.validKey1, key2: api.validKey2})

	info, err := p.Resolve("https://katfile.cc/k7l8m9n0p1q2/example.part2.rar", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != api.filename {
		t.Errorf("Name = %q, want %q", info.Name, api.filename)
	}
	if info.Size != 1073741824 {
		t.Errorf("Size = %d, want 1073741824", info.Size)
	}
	if !info.Available {
		t.Error("expected Available = true")
	}
}

func TestResolve_NotFound(t *testing.T) {
	api := newFakeAPI(t)
	api.fileExists = false
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: api.validKey1, key2: api.validKey2})

	_, err := p.Resolve("https://katfile.cc/k7l8m9n0p1q2/x.rar", nil)
	if !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestResolve_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	_, err := p.Resolve("https://katfile.cc/x/y", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestResolve_BadKeys(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: "wrong", key2: "wrong"})

	_, err := p.Resolve("https://katfile.cc/k7l8m9n0p1q2/x.rar", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestFetchDownloadURL_Success(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: api.validKey1, key2: api.validKey2})

	got, err := p.FetchDownloadURL("https://katfile.cc/k7l8m9n0p1q2/x.rar", nil)
	if err != nil {
		t.Fatalf("FetchDownloadURL: %v", err)
	}
	if got != api.downloadURL {
		t.Errorf("got %q, want %q", got, api.downloadURL)
	}
}

func TestFetchDownloadURL_NotFound(t *testing.T) {
	api := newFakeAPI(t)
	api.fileExists = false
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: api.validKey1, key2: api.validKey2})

	_, err := p.FetchDownloadURL("https://katfile.cc/k7l8m9n0p1q2/x.rar", nil)
	if !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestFetchDownloadURL_RetriesOnceAfterInvalidatedToken(t *testing.T) {
	api := newFakeAPI(t)
	api.rejectToken = true
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: api.validKey1, key2: api.validKey2})

	// Prime the cache with a token the fake API will reject exactly once.
	got, err := p.FetchDownloadURL("https://katfile.cc/k7l8m9n0p1q2/x.rar", nil)
	if err != nil {
		t.Fatalf("FetchDownloadURL: %v", err)
	}
	if got != api.downloadURL {
		t.Errorf("got %q, want %q", got, api.downloadURL)
	}
}

func TestFetchDownloadURL_CredentialsFallBackToUsernamePassword(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), &fakeTokens{username: api.validKey1, password: api.validKey2})

	got, err := p.FetchDownloadURL("https://katfile.cc/k7l8m9n0p1q2/x.rar", nil)
	if err != nil {
		t.Fatalf("FetchDownloadURL: %v", err)
	}
	if got != api.downloadURL {
		t.Errorf("got %q, want %q", got, api.downloadURL)
	}
}

func TestValidateAccount_Premium(t *testing.T) {
	api := newFakeAPI(t)
	api.paidExpiryDate = "2026-12-31 18:00:00"
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: api.validKey1, key2: api.validKey2})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"key1": api.validKey1, "key2": api.validKey2}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true")
	}
	if info.Label != "Premium Account" {
		t.Errorf("Label = %q, want %q", info.Label, "Premium Account")
	}
	if info.ExpiresAt == nil || info.ExpiresAt.Format("2006-01-02") != "2026-12-31" {
		t.Errorf("ExpiresAt = %v, want 2026-12-31", info.ExpiresAt)
	}
}

func TestValidateAccount_FreeUser(t *testing.T) {
	api := newFakeAPI(t)
	api.pkgLevelType = "free user"
	api.pkgLabel = "Free"
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: api.validKey1, key2: api.validKey2})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"key1": api.validKey1, "key2": api.validKey2}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if info.Label != "Free account" {
		t.Errorf("Label = %q, want %q", info.Label, "Free account")
	}
}

func TestValidateAccount_InvalidKeys(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), &fakeTokens{key1: "wrong", key2: "wrong"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"key1": "wrong", "key2": "wrong"}})
	if err != nil {
		t.Fatalf("ValidateAccount returned an error, want a (Valid: false) result: %v", err)
	}
	if info.Valid {
		t.Error("expected Valid = false")
	}
}

func TestValidateAccount_NoKeysConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	_, err := p.ValidateAccount(nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestParseKatfileTime(t *testing.T) {
	if parseKatfileTime("") != nil {
		t.Error("expected nil for empty string")
	}
	got := parseKatfileTime("2026-12-31 18:00:00")
	if got == nil || !got.Equal(time.Date(2026, 12, 31, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
	if parseKatfileTime("not-a-date") != nil {
		t.Error("expected nil for an unparseable string")
	}
}

func TestPluginSatisfiesInterfaces(t *testing.T) {
	var _ plugins.Hoster = (*Plugin)(nil)
	var _ plugins.Credentialed = (*Plugin)(nil)
	var _ plugins.DisplayNamer = (*Plugin)(nil)
	var _ plugins.AccountValidator = (*Plugin)(nil)
}
