package alldebrid

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// fakeAPI serves the AllDebrid endpoints from handlers keyed by path.
func fakeAPI(t *testing.T, handlers map[string]func(w http.ResponseWriter, r *http.Request)) *Plugin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			_, _ = w.Write([]byte(`{"status":"error","error":{"code":"AUTH_BAD_APIKEY","message":"The auth apikey is invalid"}}`))
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
	p.pollInterval = time.Millisecond
	p.client.RetryDelay = time.Millisecond
	return p
}

func reply(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

const userHosts = `{"status":"success","data":{"hosts":{
 "rapidgator":{"name":"rapidgator","type":"premium","domains":["rapidgator.net","rg.to"],"status":true},
 "katfile":{"name":"katfile","type":"premium","domains":["katfile.com"],"status":false},
 "1fichier":{"name":"1fichier","type":"premium","domains":["1fichier.com"],"quota":0,"quotaMax":50000,"quotaType":"traffic"},
 "turbobit":{"name":"turbobit","type":"premium","domains":["turbobit.net"]}}}}`

func TestMatch_UsesAccountHostListAndSkipsDownOrExhaustedHosts(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){"/v4.1/user/hosts": reply(userHosts)})
	for url, want := range map[string]bool{
		"https://rapidgator.net/file/abc":  true,
		"https://www.rg.to/file/abc":       true,
		"https://turbobit.net/x.html":      true,
		"https://katfile.com/abc":          false, // status false
		"https://1fichier.com/?abc":        false, // quota used up
		"https://unknown.example.com/file": false,
	} {
		if got := p.Match(url); got != want {
			t.Errorf("Match(%s) = %v, want %v", url, got, want)
		}
	}
}

func TestMatch_NoAccountNoMatch(t *testing.T) {
	p := New(fakeTokens{}, nil)
	if p.Match("https://rapidgator.net/file/abc") {
		t.Error("matched without an account")
	}
}

func TestResolve(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v4/link/infos": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			if strings.Contains(r.PostForm.Get("link[]"), "dead") {
				_, _ = w.Write([]byte(`{"status":"success","data":{"infos":[{"link":"x","error":{"code":"LINK_DOWN","message":"This link is not available"}}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"success","data":{"infos":[{"link":"x","filename":"movie.part1.rar","size":1048576,"host":"rapidgator"}]}}`))
		},
	})
	info, err := p.Resolve("https://rapidgator.net/file/abc", &plugins.Account{Password: testKey})
	if err != nil || info.Name != "movie.part1.rar" || info.Size != 1048576 || !info.Available {
		t.Fatalf("Resolve = %+v, %v", info, err)
	}
	if _, err := p.Resolve("https://rapidgator.net/file/dead", &plugins.Account{Password: testKey}); !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("dead link: err = %v, want ErrNotFound", err)
	}
}

func TestFetchDownloadURL_Direct(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v4/link/unlock": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.FormValue("link") != "https://rapidgator.net/file/abc" {
				t.Errorf("unlock request: %s link=%q", r.Method, r.FormValue("link"))
			}
			_, _ = w.Write([]byte(`{"status":"success","data":{"link":"https://dl.example.com/f","filename":"f","filesize":1}}`))
		},
	})
	got, err := p.FetchDownloadURL("https://rapidgator.net/file/abc", &plugins.Account{Password: testKey})
	if err != nil || got != "https://dl.example.com/f" {
		t.Fatalf("FetchDownloadURL = %q, %v", got, err)
	}
}

func TestFetchDownloadURL_DelayedIsPolled(t *testing.T) {
	var polls atomic.Int32
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v4/link/unlock": reply(`{"status":"success","data":{"delayed":42}}`),
		"/v4/link/delayed": func(w http.ResponseWriter, r *http.Request) {
			if r.FormValue("id") != "42" {
				t.Errorf("delayed id = %q", r.FormValue("id"))
			}
			if polls.Add(1) < 3 {
				_, _ = w.Write([]byte(`{"status":"success","data":{"status":1,"time_left":10}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"success","data":{"status":2,"time_left":0,"link":"https://dl.example.com/ready"}}`))
		},
	})
	got, err := p.FetchDownloadURL("https://rapidgator.net/file/abc", &plugins.Account{Password: testKey})
	if err != nil || got != "https://dl.example.com/ready" || polls.Load() != 3 {
		t.Fatalf("FetchDownloadURL = %q, %v after %d polls", got, err, polls.Load())
	}
}

func TestFetchDownloadURL_ErrorMapping(t *testing.T) {
	for code, want := range map[string]error{
		"MUST_BE_PREMIUM":            plugins.ErrAccountRequired,
		"FREE_TRIAL_LIMIT_REACHED":   plugins.ErrAccountRequired,
		"LINK_HOST_NOT_SUPPORTED":    plugins.ErrUnsupportedHost,
		"LINK_HOST_LIMIT_REACHED":    plugins.ErrUnsupportedHost,
		"NO_SERVER":                  plugins.ErrUnsupportedHost,
		"LINK_DOWN":                  plugins.ErrNotFound,
		"LINK_TEMPORARY_UNAVAILABLE": plugins.ErrUnavailable,
	} {
		p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/v4/link/unlock": reply(`{"status":"error","error":{"code":"` + code + `","message":"m"}}`),
		})
		if _, err := p.FetchDownloadURL("https://rapidgator.net/file/abc", &plugins.Account{Password: testKey}); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", code, err, want)
		}
	}
}

func TestFetchDownloadURL_BadKeyIsAccountRequired(t *testing.T) {
	p := fakeAPI(t, nil)
	_, err := p.FetchDownloadURL("https://rapidgator.net/file/abc", &plugins.Account{Password: "wrong"})
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Fatalf("err = %v, want ErrAccountRequired", err)
	}
}

func TestValidateAccount(t *testing.T) {
	until := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v4/user":         reply(`{"status":"success","data":{"user":{"username":"u","isPremium":true,"isTrial":false,"premiumUntil":` + strconv.FormatInt(until.Unix(), 10) + `}}}`),
		"/v4.1/user/hosts": reply(userHosts),
	})
	info, err := p.ValidateAccount(&plugins.Account{Password: testKey})
	if err != nil || !info.Valid || info.ExpiresAt == nil || !info.ExpiresAt.Equal(until) || info.Label != "Premium until 2027-01-31" {
		t.Fatalf("ValidateAccount = %+v, %v", info, err)
	}
	if !p.Match("https://rapidgator.net/file/abc") {
		t.Error("host list not warmed by a successful validation")
	}

	info, err = p.ValidateAccount(&plugins.Account{Password: "wrong"})
	if err != nil || info.Valid {
		t.Fatalf("bad key: %+v, %v — want Valid=false", info, err)
	}
}

func TestValidateAccount_Free(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v4/user":         reply(`{"status":"success","data":{"user":{"isPremium":false,"isTrial":false,"premiumUntil":0}}}`),
		"/v4.1/user/hosts": reply(userHosts),
	})
	info, err := p.ValidateAccount(&plugins.Account{Password: testKey})
	if err != nil || !info.Valid || info.ExpiresAt != nil || info.Label != "Free account (no premium)" {
		t.Fatalf("ValidateAccount = %+v, %v", info, err)
	}
}

func TestInterfaces(t *testing.T) {
	var p any = New(nil, nil)
	if _, ok := p.(plugins.AccountValidator); !ok {
		t.Error("not an AccountValidator")
	}
	if _, ok := p.(plugins.PremiumLinker); !ok {
		t.Error("not a PremiumLinker")
	}
	if c, ok := p.(plugins.Credentialed); !ok || !c.Credentialed() {
		t.Error("not Credentialed")
	}
}
