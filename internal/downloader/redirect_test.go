package downloader

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{URL: u}
}

func TestCheckRedirect(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		to      string
		wantErr error
	}{
		{"https to https", "https://a.example/f", "https://b.example/f", nil},
		{"http to https", "http://a.example/f", "https://a.example/f", nil},
		{"http to http", "http://a.example/f", "http://b.example/f", nil},
		{"https to http", "https://a.example/f?token=x", "http://b.example/f", errRedirectDowngrade},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRedirect(mustReq(t, tc.to), []*http.Request{mustReq(t, tc.from)})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("checkRedirect = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckRedirect_Limit(t *testing.T) {
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = mustReq(t, "https://a.example/f")
	}
	if err := checkRedirect(mustReq(t, "https://a.example/g"), via); err == nil {
		t.Fatal("redirect past the limit allowed")
	}
}

// End to end through a real TLS server: the plain-http target must
// never be contacted.
func TestCheckRedirect_BlocksDowngradeOverTheWire(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/file.bin", http.StatusFound)
	}))
	defer secure.Close()

	client := secure.Client()
	client.CheckRedirect = checkRedirect
	resp, err := client.Get(secure.URL + "/file.bin?token=secret")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("downgrade redirect followed")
	}
	if !errors.Is(err, errRedirectDowngrade) {
		t.Fatalf("err = %v, want errRedirectDowngrade", err)
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("plain-http server contacted %d times", n)
	}
}

func TestNew_DefaultClientUsesRedirectPolicy(t *testing.T) {
	e := New(Config{BaseDir: t.TempDir()})
	if e.client.CheckRedirect == nil {
		t.Fatal("default client has no redirect policy")
	}
}
