package debridlink

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"error":"badToken","error_description":"The authentication token is invalid or expired."}`))
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

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const hostsList = `{"success":true,"value":[
 {"name":"rapidgator","type":"host","status":1,"isFree":false,"domains":["rapidgator.net","rg.to"]},
 {"name":"katfile","type":"host","status":0,"isFree":false,"domains":["katfile.com"]},
 {"name":"youtube","type":"stream","status":1,"isFree":true,"domains":["youtube.com"]}]}`

func TestMatch(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){"/downloader/hosts": reply(200, hostsList)})
	for url, want := range map[string]bool{
		"https://rapidgator.net/file/abc": true,
		"https://rg.to/file/abc":          true,
		"https://katfile.com/abc":         false, // offline
		"https://youtube.com/watch?v=1":   false, // not a file host
	} {
		if got := p.Match(url); got != want {
			t.Errorf("Match(%s) = %v, want %v", url, got, want)
		}
	}
	if New(fakeTokens{}, nil).Match("https://rapidgator.net/file/abc") {
		t.Error("matched without an account")
	}
}

func TestResolveThenFetch_ShareOneAddCall(t *testing.T) {
	var adds atomic.Int32
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/downloader/add": func(w http.ResponseWriter, r *http.Request) {
			adds.Add(1)
			var body struct{ URL string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL != "https://rapidgator.net/file/abc" {
				t.Errorf("add body: %+v %v", body, err)
			}
			_, _ = w.Write([]byte(`{"success":true,"value":{"id":"x","name":"movie.part1.rar","url":"https://rapidgator.net/file/abc","downloadUrl":"https://dl6.debrid.example/dl/x","expired":false,"host":"rapidgator","size":125002}}`))
		},
	})
	acc := &plugins.Account{Password: testKey}
	info, err := p.Resolve("https://rapidgator.net/file/abc", acc)
	if err != nil || info.Name != "movie.part1.rar" || info.Size != 125002 {
		t.Fatalf("Resolve = %+v, %v", info, err)
	}
	got, err := p.FetchDownloadURL("https://rapidgator.net/file/abc", acc)
	if err != nil || got != "https://dl6.debrid.example/dl/x" {
		t.Fatalf("FetchDownloadURL = %q, %v", got, err)
	}
	if adds.Load() != 1 {
		t.Errorf("downloader/add called %d times, want 1", adds.Load())
	}
}

func TestFetch_FolderAndProcessing(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/downloader/add": func(w http.ResponseWriter, r *http.Request) {
			var body struct{ URL string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body.URL {
			case "https://host.example/one":
				_, _ = w.Write([]byte(`{"success":true,"value":[{"name":"a","downloadUrl":"https://dl.example/a","size":1}]}`))
			case "https://host.example/folder":
				_, _ = w.Write([]byte(`{"success":true,"value":[{"name":"a"},{"name":"b"}]}`))
			default:
				_, _ = w.Write([]byte(`{"success":true,"value":{"name":"c","isProcessing":true}}`))
			}
		},
	})
	acc := &plugins.Account{Password: testKey}
	if got, err := p.FetchDownloadURL("https://host.example/one", acc); err != nil || got != "https://dl.example/a" {
		t.Errorf("single-element array: %q, %v", got, err)
	}
	if _, err := p.FetchDownloadURL("https://host.example/folder", acc); err == nil {
		t.Error("folder with two files accepted")
	}
	if _, err := p.FetchDownloadURL("https://host.example/processing", acc); !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("processing: err = %v, want ErrUnavailable", err)
	}
}

func TestErrorMapping(t *testing.T) {
	for code, want := range map[string]error{
		"notFreeHost":      plugins.ErrAccountRequired,
		"maxData":          plugins.ErrAccountRequired,
		"hostNotValid":     plugins.ErrUnsupportedHost,
		"maintenanceHost":  plugins.ErrUnsupportedHost,
		"maxDataHost":      plugins.ErrUnsupportedHost,
		"fileNotFound":     plugins.ErrNotFound,
		"fileNotAvailable": plugins.ErrUnavailable,
		"floodDetected":    plugins.ErrRateLimited,
	} {
		p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/downloader/add": reply(400, `{"success":false,"error":"`+code+`"}`),
		})
		if _, err := p.FetchDownloadURL("https://host.example/x", &plugins.Account{Password: testKey}); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", code, err, want)
		}
	}
	p := fakeAPI(t, nil)
	if _, err := p.FetchDownloadURL("https://host.example/x", &plugins.Account{Password: "wrong"}); !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("bad token: err = %v", err)
	}
}

func TestValidateAccount(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/account/infos":    reply(200, `{"success":true,"value":{"username":"Amy","accountType":1,"premiumLeft":3628800}}`),
		"/downloader/hosts": reply(200, hostsList),
	})
	info, err := p.ValidateAccount(&plugins.Account{Password: testKey})
	if err != nil || !info.Valid || info.ExpiresAt == nil {
		t.Fatalf("ValidateAccount = %+v, %v", info, err)
	}
	if d := time.Until(*info.ExpiresAt); d < 41*24*time.Hour || d > 43*24*time.Hour {
		t.Errorf("expiry %v from now, want ~42 days", d)
	}
	if !p.Match("https://rapidgator.net/file/abc") {
		t.Error("host list not warmed")
	}
	info, err = p.ValidateAccount(&plugins.Account{Password: "wrong"})
	if err != nil || info.Valid {
		t.Errorf("bad key: %+v, %v", info, err)
	}
}

func TestValidateAccount_FreeAndLifetime(t *testing.T) {
	for body, label := range map[string]string{
		`{"success":true,"value":{"accountType":0,"premiumLeft":0}}`: "Free account (no premium)",
		`{"success":true,"value":{"accountType":2,"premiumLeft":0}}`: "Lifetime premium",
	} {
		p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/account/infos":    reply(200, body),
			"/downloader/hosts": reply(200, hostsList),
		})
		info, err := p.ValidateAccount(&plugins.Account{Password: testKey})
		if err != nil || !info.Valid || info.ExpiresAt != nil || info.Label != label {
			t.Errorf("%s: %+v, %v", label, info, err)
		}
	}
}
