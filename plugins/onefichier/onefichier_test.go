package onefichier

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

func checkAuth(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+want {
		t.Errorf("Authorization = %q, want Bearer %s", got, want)
	}
}

func TestName(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.Name() != "onefichier" {
		t.Errorf("Name() = %q, want onefichier", p.Name())
	}
}

func TestMatch_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	if p.Match("https://1fichier.com/?abc123") {
		t.Error("expected Match false with no account configured")
	}
}

func TestMatch_SupportedHost(t *testing.T) {
	p := New(&fakeTokens{token: "tok"}, nil)
	if !p.Match("https://1fichier.com/?abc123") {
		t.Error("expected Match true for 1fichier.com")
	}
	if !p.Match("https://www.1fichier.com/?abc123") {
		t.Error("expected Match true for a www. subdomain")
	}
}

func TestMatch_UnrelatedHost(t *testing.T) {
	p := New(&fakeTokens{token: "tok"}, nil)
	if p.Match("https://rapidgator.net/file/123") {
		t.Error("expected Match false for an unrelated host")
	}
}

func TestMatch_UsesPasswordFallbackToken(t *testing.T) {
	p := New(&fakeTokens{password: "webui-token"}, nil)
	if !p.Match("https://1fichier.com/?abc123") {
		t.Error("expected Match true using a token stored only as Password")
	}
}

func TestResolve_Success(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/info.cgi" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r, "test-token")
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.URL == "" {
			t.Error("expected non-empty url in request body")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"filename": "movie.mkv",
			"size":     123456,
		})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	info, err := p.Resolve("https://1fichier.com/?abc123", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != "movie.mkv" || info.Size != 123456 || !info.Available {
		t.Errorf("unexpected FileInfo: %+v", info)
	}
}

func TestResolve_NotFound(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "KO",
			"message": "File not found or deleted",
		})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	_, err := p.Resolve("https://1fichier.com/?abc123", nil)
	if err == nil {
		t.Fatal("expected an error for a KO status response")
	}
}

func TestResolve_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	_, err := p.Resolve("https://1fichier.com/?abc123", nil)
	if err != plugins.ErrAccountRequired {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestResolve_UsesAccountFromArgumentOverTokenSource(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "arg-token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"filename": "f", "size": 1})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "source-token"})

	_, err := p.Resolve("https://1fichier.com/?x", &plugins.Account{Extra: map[string]string{"api_key": "arg-token"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestFetchDownloadURL_Success(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download/get_token.cgi" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r, "test-token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "OK",
			"url":    "https://download.1fichier.com/abc/movie.mkv",
		})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	got, err := p.FetchDownloadURL("https://1fichier.com/?abc123", nil)
	if err != nil {
		t.Fatalf("FetchDownloadURL: %v", err)
	}
	if got != "https://download.1fichier.com/abc/movie.mkv" {
		t.Errorf("got %q", got)
	}
}

func TestFetchDownloadURL_KOStatus(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "KO",
			"message": "Only 1 location allowed at a time",
		})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	_, err := p.FetchDownloadURL("https://1fichier.com/?abc123", nil)
	if err == nil {
		t.Fatal("expected an error for a KO status response")
	}
}

func TestFetchDownloadURL_BadToken(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	_, err := p.FetchDownloadURL("https://1fichier.com/?abc123", nil)
	if err != plugins.ErrAccountRequired {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestHostnameOf(t *testing.T) {
	cases := map[string]string{
		"https://1Fichier.COM/?abc": "1fichier.com",
		"http://example.com":        "example.com",
		"ftp://example.com":         "",
		"not a url":                 "",
		"":                          "",
	}
	for in, want := range cases {
		if got := hostnameOf(in); got != want {
			t.Errorf("hostnameOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPluginSatisfiesInterfaces(t *testing.T) {
	var _ plugins.Hoster = (*Plugin)(nil)
	var _ plugins.Credentialed = (*Plugin)(nil)
	var _ plugins.DisplayNamer = (*Plugin)(nil)
	var _ plugins.AccountValidator = (*Plugin)(nil)
}
