package premiumize

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

const testKey = "test-api-key"

type fakeTokens struct{ key string }

func (f fakeTokens) AccountFor(string) (*plugins.Account, bool) {
	if f.key == "" {
		return nil, false
	}
	return &plugins.Account{Password: f.key}, true
}

func fakeAPI(t *testing.T, handlers map[string]func(w http.ResponseWriter, r *http.Request)) *Plugin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			_, _ = w.Write([]byte(`{"status":"error","message":"Not logged in","code":"authentication_failed"}`))
			return
		}
		h, ok := handlers[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	p := New(fakeTokens{key: testKey}, nil)
	p.baseURL = srv.URL
	p.client.RetryDelay = time.Millisecond
	return p
}

func reply(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

const servicesList = `{"status":"success","cache":["rapidgator.net"],"directdl":["rapidgator.net","1fichier.com"],
 "queue":["katfile.com"],"fairusefactor":{"rapidgator.net":1},
 "aliases":{"rapidgator.net":["rg.to","rapidgator.net","rapidgator.host"],"1fichier.com":["1fichier.com","megadl.fr"],"katfile.com":["katfile.com"]},
 "regexpatterns":{}}`

func TestMatch(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){"/services/list": reply(servicesList)})
	for url, want := range map[string]bool{
		"https://rapidgator.net/file/abc": true,
		"https://rg.to/file/abc":          true,
		"https://megadl.fr/?abc":          true,
		"https://katfile.com/abc":         false, // queue only, no directdl
		"https://other.example.com/x":     false,
	} {
		if got := p.Match(url); got != want {
			t.Errorf("Match(%s) = %v, want %v", url, got, want)
		}
	}
	if New(fakeTokens{}, nil).Match("https://rapidgator.net/file/abc") {
		t.Error("matched without an account")
	}
}

func TestResolveThenFetch_ShareOneDirectDLCall(t *testing.T) {
	var calls atomic.Int32
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/transfer/directdl": func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != http.MethodPost || r.FormValue("src") != "https://rapidgator.net/file/abc" {
				t.Errorf("directdl request: %s src=%q", r.Method, r.FormValue("src"))
			}
			_, _ = w.Write([]byte(`{"status":"success","content":[{"path":"Release/movie.part1.rar","size":2048,"link":"https://dl.premiumize.example/f"}]}`))
		},
	})
	acc := &plugins.Account{Password: testKey}
	info, err := p.Resolve("https://rapidgator.net/file/abc", acc)
	if err != nil || info.Name != "movie.part1.rar" || info.Size != 2048 {
		t.Fatalf("Resolve = %+v, %v", info, err)
	}
	got, err := p.FetchDownloadURL("https://rapidgator.net/file/abc", acc)
	if err != nil || got != "https://dl.premiumize.example/f" || calls.Load() != 1 {
		t.Fatalf("FetchDownloadURL = %q, %v (%d calls)", got, err, calls.Load())
	}
}

func TestDirectDL_FolderRejected(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/transfer/directdl": reply(`{"status":"success","content":[{"path":"a","link":"x"},{"path":"b","link":"y"}]}`),
	})
	if _, err := p.FetchDownloadURL("https://host.example/folder", &plugins.Account{Password: testKey}); err == nil {
		t.Error("folder with two files accepted")
	}
}

func TestErrorMapping(t *testing.T) {
	for code, want := range map[string]error{
		"account_limit_reached":  plugins.ErrAccountRequired,
		"service_unsupported":    plugins.ErrUnsupportedHost,
		"service_down":           plugins.ErrUnsupportedHost,
		"service_limit_reached":  plugins.ErrUnsupportedHost,
		"not_found":              plugins.ErrNotFound,
		"rate_limit_reached":     plugins.ErrRateLimited,
		"link_generation_failed": plugins.ErrUnavailable,
	} {
		p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/transfer/directdl": reply(`{"status":"error","message":"m","code":"` + code + `"}`),
		})
		if _, err := p.FetchDownloadURL("https://host.example/x", &plugins.Account{Password: testKey}); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", code, err, want)
		}
	}
	p := fakeAPI(t, nil)
	if _, err := p.FetchDownloadURL("https://host.example/x", &plugins.Account{Password: "wrong"}); !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("bad key: err = %v", err)
	}
}

func TestValidateAccount(t *testing.T) {
	until := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/account/info":  reply(`{"status":"success","customer_id":"1","premium_until":` + strconv.FormatInt(until.Unix(), 10) + `,"limit_used":0.25,"booster_points":0}`),
		"/services/list": reply(servicesList),
	})
	info, err := p.ValidateAccount(&plugins.Account{Password: testKey})
	if err != nil || !info.Valid || info.ExpiresAt == nil || !info.ExpiresAt.Equal(until) || info.TrafficLeft != nil {
		t.Fatalf("ValidateAccount = %+v, %v", info, err)
	}
	if info.Label != "Premium until 2027-03-01, fair use 25% used" {
		t.Errorf("Label = %q", info.Label)
	}
	if !p.Match("https://rapidgator.net/file/abc") {
		t.Error("host list not warmed")
	}
	info, err = p.ValidateAccount(&plugins.Account{Password: "wrong"})
	if err != nil || info.Valid {
		t.Errorf("bad key: %+v, %v", info, err)
	}
}

func TestValidateAccount_FreeAndFairUseExhausted(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/account/info":  reply(`{"status":"success","premium_until":null,"limit_used":0}`),
		"/services/list": reply(servicesList),
	})
	if info, err := p.ValidateAccount(&plugins.Account{Password: testKey}); err != nil || info.Label != "Free account (no premium)" || info.ExpiresAt != nil {
		t.Errorf("free: %+v, %v", info, err)
	}

	p = fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/account/info":  reply(`{"status":"success","premium_until":4102444800,"limit_used":1}`),
		"/services/list": reply(servicesList),
	})
	info, err := p.ValidateAccount(&plugins.Account{Password: testKey})
	if err != nil || info.TrafficLeft == nil || *info.TrafficLeft != 0 {
		t.Errorf("fair use exhausted: %+v, %v — want TrafficLeft 0", info, err)
	}
}
