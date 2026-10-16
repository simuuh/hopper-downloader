package rapidgator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

// fakeTokens is a minimal TokenSource for tests.
type fakeTokens struct {
	username string
	password string
}

func (f *fakeTokens) AccountFor(hosterKey string) (*plugins.Account, bool) {
	if hosterKey != pluginName || f.username == "" || f.password == "" {
		return nil, false
	}
	return &plugins.Account{Username: f.username, Password: f.password}, true
}

func newTestPlugin(t *testing.T, handler http.Handler, username, password string) *Plugin {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p := New(&fakeTokens{username: username, password: password}, nil)
	p.baseURL = srv.URL
	return p
}

// fakeAPI implements just enough of Rapidgator's v2 shape to exercise
// the plugin: a fixed valid login token, one file keyed by file_id, and
// a user profile. currentToken (if set) is what /file/info,
// /file/download, and /user/info require; a mismatched token responds
// with the documented "Session not exist" 401.
type fakeAPI struct {
	t            *testing.T
	validUser    string
	validPass    string
	issuedToken  string
	loginCalls   int
	file         fileObject
	fileExists   bool
	user         userObject
	rejectToken  bool // simulate an expired/invalidated token on the next authenticated call
	rejectedOnce bool
}

func (f *fakeAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/login":
			f.handleLogin(w, r)
		case "/file/info":
			f.handleFileInfo(w, r)
		case "/file/download":
			f.handleFileDownload(w, r)
		case "/user/info":
			f.handleUserInfo(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *fakeAPI) handleLogin(w http.ResponseWriter, r *http.Request) {
	f.loginCalls++
	if r.Method != http.MethodPost {
		f.t.Errorf("login: method = %s, want POST", r.Method)
	}
	if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		f.t.Errorf("login: Content-Type = %q, want form-encoded", got)
	}
	// Credentials must travel in the body, not the query string.
	if r.URL.RawQuery != "" {
		f.t.Errorf("login: credentials leaked into query string: %q", r.URL.RawQuery)
	}
	if err := r.ParseForm(); err != nil {
		f.t.Fatalf("login: parse form: %v", err)
	}
	login, password := r.PostFormValue("login"), r.PostFormValue("password")
	if login != f.validUser || password != f.validPass {
		writeEnvelope(w, nil, http.StatusUnauthorized, "Login or password is wrong")
		return
	}
	writeEnvelope(w, loginResponse{Token: f.issuedToken}, http.StatusOK, "")
}

func (f *fakeAPI) checkToken(w http.ResponseWriter, r *http.Request) bool {
	token := r.URL.Query().Get("token")
	if f.rejectToken && !f.rejectedOnce {
		f.rejectedOnce = true
		writeEnvelope(w, nil, http.StatusUnauthorized, "Session not exist")
		return false
	}
	if token != f.issuedToken {
		writeEnvelope(w, nil, http.StatusUnauthorized, "Session not exist")
		return false
	}
	return true
}

func (f *fakeAPI) handleFileInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.t.Errorf("file/info: method = %s, want GET", r.Method)
	}
	if !f.checkToken(w, r) {
		return
	}
	fileID := r.URL.Query().Get("file_id")
	if !f.fileExists || fileID != f.file.FileID {
		writeEnvelope(w, nil, http.StatusOK, "File not found")
		return
	}
	writeEnvelope(w, f.file, http.StatusOK, "")
}

func (f *fakeAPI) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.t.Errorf("file/download: method = %s, want GET", r.Method)
	}
	if !f.checkToken(w, r) {
		return
	}
	fileID := r.URL.Query().Get("file_id")
	if !f.fileExists || fileID != f.file.FileID {
		writeEnvelope(w, nil, http.StatusOK, "File not found")
		return
	}
	writeEnvelope(w, downloadObject{DownloadURL: "https://dl.rapidgator.net/direct/" + fileID}, http.StatusOK, "")
}

func (f *fakeAPI) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.t.Errorf("user/info: method = %s, want GET", r.Method)
	}
	if !f.checkToken(w, r) {
		return
	}
	writeEnvelope(w, f.user, http.StatusOK, "")
}

func writeEnvelope(w http.ResponseWriter, response any, status int, details string) {
	if response == nil {
		response = struct{}{}
	}
	env := map[string]any{"response": response, "status": status, "details": details}
	_ = json.NewEncoder(w).Encode(env)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t:           t,
		validUser:   "user@example.com",
		validPass:   "correct-password",
		issuedToken: "session-token-abc",
		file:        fileObject{FileID: "abcdef123456", Name: "movie.rar", Size: 123456789},
		fileExists:  true,
		user: userObject{
			Email:     "user@example.com",
			IsPremium: true,
			Traffic: struct {
				Total int64 `json:"total"`
				Left  int64 `json:"left"`
			}{Total: 1 << 40, Left: 1 << 30},
		},
	}
}

func TestName(t *testing.T) {
	if New(&fakeTokens{}, nil).Name() != "rapidgator" {
		t.Errorf("Name() = %q, want rapidgator", New(&fakeTokens{}, nil).Name())
	}
}

func TestDomains(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	got := p.Domains()
	want := []string{"rapidgator.net", "rg.to"}
	if len(got) != len(want) {
		t.Fatalf("Domains() = %v, want %v", got, want)
	}
	for i, d := range want {
		if got[i] != d {
			t.Errorf("Domains()[%d] = %q, want %q", i, got[i], d)
		}
	}
}

func TestCredentialed_IsTrue(t *testing.T) {
	if !New(&fakeTokens{}, nil).Credentialed() {
		t.Error("Credentialed() = false, want true")
	}
}

func TestDisplayName(t *testing.T) {
	if got := New(&fakeTokens{}, nil).DisplayName(); got != "Rapidgator" {
		t.Errorf("DisplayName() = %q", got)
	}
}

func TestMatch(t *testing.T) {
	p := New(&fakeTokens{username: "u", password: "p"}, nil)
	cases := map[string]bool{
		"https://rapidgator.net/file/123/x.html": true,
		"https://www.rapidgator.net/file/123":    true,
		"https://rg.to/file/abc123/x.html":       true,
		"https://www.rg.to/file/abc":             true,
		"https://example.com/file/123":           false,
		"not-a-url":                              false,
	}
	for url, want := range cases {
		if got := p.Match(url); got != want {
			t.Errorf("Match(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestMatch_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil) // no username/password
	if p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match to return false with no account configured")
	}
}

func TestFileIDFromURL(t *testing.T) {
	cases := map[string]string{
		"https://rapidgator.net/file/abc123/movie.part1.rar.html": "abc123",
		"https://rg.to/file/xyz789/movie.zip":                     "xyz789",
		"https://rapidgator.net/file/abc123":                      "abc123",
	}
	for url, want := range cases {
		got, err := fileIDFromURL(url)
		if err != nil {
			t.Errorf("fileIDFromURL(%q): %v", url, err)
			continue
		}
		if got != want {
			t.Errorf("fileIDFromURL(%q) = %q, want %q", url, got, want)
		}
	}

	if _, err := fileIDFromURL("https://rapidgator.net/other/path"); err == nil {
		t.Error("expected an error for a URL with no /file/<id> segment")
	}
}

func TestResolve_Success(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	info, err := p.Resolve("https://rapidgator.net/file/"+api.file.FileID+"/movie.rar.html", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != api.file.Name || info.Size != api.file.Size || !info.Available {
		t.Errorf("Resolve() = %+v, want name=%q size=%d available=true", info, api.file.Name, api.file.Size)
	}
	if api.loginCalls != 1 {
		t.Errorf("loginCalls = %d, want exactly 1 (token should be cached)", api.loginCalls)
	}
}

func TestResolve_FileNotFound(t *testing.T) {
	api := newFakeAPI(t)
	api.fileExists = false
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	_, err := p.Resolve("https://rapidgator.net/file/"+api.file.FileID+"/x.html", nil)
	if !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestResolve_WrongCredentials(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), "wrong@example.com", "wrong-password")

	_, err := p.Resolve("https://rapidgator.net/file/"+api.file.FileID+"/x.html", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestResolve_RetriesOnceAfterExpiredToken(t *testing.T) {
	api := newFakeAPI(t)
	api.rejectToken = true // the first authenticated call after login reports the token invalid
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	info, err := p.Resolve("https://rapidgator.net/file/"+api.file.FileID+"/x.html", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != api.file.Name {
		t.Errorf("Resolve() = %+v after retry, want name=%q", info, api.file.Name)
	}
	if api.loginCalls != 2 {
		t.Errorf("loginCalls = %d, want 2 (initial + re-login after expired token)", api.loginCalls)
	}
}

func TestResolve_UsesAccountParamOverConfiguredOne(t *testing.T) {
	api := newFakeAPI(t)
	api.validUser, api.validPass = "override@example.com", "override-password"
	// Configured account (via TokenSource) is deliberately wrong; the
	// explicit acc parameter must be what's actually used.
	p := newTestPlugin(t, api.handler(), "wrong@example.com", "wrong-password")

	acc := &plugins.Account{Username: "override@example.com", Password: "override-password"}
	info, err := p.Resolve("https://rapidgator.net/file/"+api.file.FileID+"/x.html", acc)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != api.file.Name {
		t.Errorf("Resolve() = %+v, want name=%q", info, api.file.Name)
	}
}

func TestFetchDownloadURL_Success(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	got, err := p.FetchDownloadURL("https://rapidgator.net/file/"+api.file.FileID+"/x.html", nil)
	if err != nil {
		t.Fatalf("FetchDownloadURL: %v", err)
	}
	want := "https://dl.rapidgator.net/direct/" + api.file.FileID
	if got != want {
		t.Errorf("FetchDownloadURL() = %q, want %q", got, want)
	}
}

func TestFetchDownloadURL_NotFound(t *testing.T) {
	api := newFakeAPI(t)
	api.fileExists = false
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	_, err := p.FetchDownloadURL("https://rapidgator.net/file/"+api.file.FileID+"/x.html", nil)
	if !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestValidateAccount_ValidPremium(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	info, err := p.ValidateAccount(&plugins.Account{Username: api.validUser, Password: api.validPass})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("Valid = false, want true")
	}
	if info.Label != "Premium" {
		t.Errorf("Label = %q, want Premium", info.Label)
	}
	if info.TrafficLeft == nil || *info.TrafficLeft != api.user.Traffic.Left {
		t.Errorf("TrafficLeft = %v, want %d", info.TrafficLeft, api.user.Traffic.Left)
	}
}

func TestValidateAccount_NotPremium(t *testing.T) {
	api := newFakeAPI(t)
	api.user.IsPremium = false
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	info, err := p.ValidateAccount(&plugins.Account{Username: api.validUser, Password: api.validPass})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if info.Label != "Free (not premium)" {
		t.Errorf("Label = %q, want %q", info.Label, "Free (not premium)")
	}
}

func TestValidateAccount_InvalidCredentials(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	info, err := p.ValidateAccount(&plugins.Account{Username: "wrong@example.com", Password: "wrong"})
	if err != nil {
		t.Fatalf("ValidateAccount returned an error instead of Valid:false: %v", err)
	}
	if info.Valid {
		t.Error("Valid = true, want false for wrong credentials")
	}
}

func TestValidateAccount_PremiumEndTimeUnixSeconds(t *testing.T) {
	api := newFakeAPI(t)
	expiry := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
	api.user.PremiumEndTimeRaw = json.RawMessage(fmt.Sprintf("%d", expiry.Unix()))
	p := newTestPlugin(t, api.handler(), api.validUser, api.validPass)

	info, err := p.ValidateAccount(&plugins.Account{Username: api.validUser, Password: api.validPass})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if info.ExpiresAt == nil || !info.ExpiresAt.Equal(expiry) {
		t.Errorf("ExpiresAt = %v, want %v", info.ExpiresAt, expiry)
	}
}

func TestParsePremiumEndTime(t *testing.T) {
	cases := []struct {
		raw     string
		wantNil bool
	}{
		{raw: `1799999999`, wantNil: false},
		{raw: `"2027-03-15"`, wantNil: false},
		{raw: `"2027-03-15 12:00:00"`, wantNil: false},
		{raw: `null`, wantNil: true},
		{raw: `""`, wantNil: true},
		{raw: `"not-a-date"`, wantNil: true},
		{raw: ``, wantNil: true},
	}
	for _, c := range cases {
		got := parsePremiumEndTime(json.RawMessage(c.raw))
		if (got == nil) != c.wantNil {
			t.Errorf("parsePremiumEndTime(%q) = %v, wantNil %v", c.raw, got, c.wantNil)
		}
	}
}

// TestDecodeEnvelope_RealHTTP401IsAccountRequired covers a real
// transport-level 401 (no JSON envelope body at all) — distinct from the
// documented "200 status with a 'Login or password is wrong'/'Session
// not exist' details string" envelope-level case mapAPIError already
// handles. Without this, decodeEnvelope would instead try (and fail) to
// JSON-decode an empty/non-JSON body and return an opaque "decode API
// response" error, which callWithToken's cached-token-expired retry
// logic has no way to recognize as "re-login and retry".
func TestDecodeEnvelope_RealHTTP401IsAccountRequired(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody}
	_, err := decodeEnvelope[fileObject](resp)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

// TestSanitizeTransportErr_RedactsQueryString covers a real secret-leak
// fix: every authenticated GET here carries the session token as a
// ?token=... query parameter (see the package doc comment on why), so a
// transport-level failure (DNS, timeout, connection refused — all before
// the server ever sees the request) has Go's net/http wrap the full
// request URL, token included, into the error text. That text ends up in
// queue.Item.Error (persisted, shown in the Web UI, and logged) with no
// later chance to redact it, so it must be stripped here instead.
func TestSanitizeTransportErr_RedactsQueryString(t *testing.T) {
	orig := &url.Error{Op: "Get", URL: "https://rapidgator.net/api/v2/file/info?file_id=abc&token=super-secret-token", Err: fmt.Errorf("dial tcp: connection refused")}
	got := sanitizeTransportErr(orig)

	if strings.Contains(got.Error(), "super-secret-token") {
		t.Errorf("sanitized error still contains the token: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "REDACTED") {
		t.Errorf("expected sanitized error to contain REDACTED, got %q", got.Error())
	}
}

// TestSanitizeTransportErr_NonURLErrorPassedThrough covers the case
// sanitizeTransportErr can't do anything about: an error that isn't a
// *url.Error at all (e.g. context.DeadlineExceeded surfacing directly)
// is returned unchanged rather than dropped or mangled.
func TestSanitizeTransportErr_NonURLErrorPassedThrough(t *testing.T) {
	orig := fmt.Errorf("some other error")
	got := sanitizeTransportErr(orig)
	if got != orig {
		t.Errorf("got %v, want the original error unchanged", got)
	}
}

func TestMapAPIError(t *testing.T) {
	cases := []struct {
		status  int
		details string
		want    error
	}{
		{status: http.StatusOK, details: "", want: nil},
		{status: http.StatusTooManyRequests, details: "", want: plugins.ErrRateLimited},
		{status: http.StatusUnauthorized, details: "Session not exist", want: plugins.ErrAccountRequired},
		{status: http.StatusOK, details: "Login or password is wrong", want: plugins.ErrAccountRequired},
		{status: http.StatusOK, details: "File not found", want: plugins.ErrNotFound},
		{status: http.StatusServiceUnavailable, details: "", want: plugins.ErrUnavailable},
	}
	for _, c := range cases {
		got := mapAPIError(c.status, c.details)
		if c.want == nil {
			if got != nil {
				t.Errorf("mapAPIError(%d, %q) = %v, want nil", c.status, c.details, got)
			}
			continue
		}
		if !errors.Is(got, c.want) {
			t.Errorf("mapAPIError(%d, %q) = %v, want %v", c.status, c.details, got, c.want)
		}
	}
}
