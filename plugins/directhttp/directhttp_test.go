package directhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

func TestMatch(t *testing.T) {
	p := New()
	cases := map[string]bool{
		"http://example.com/file":  true,
		"https://example.com/file": true,
		"ftp://example.com/file":   false,
		"not-a-url":                false,
		"":                         false,
	}
	for url, want := range cases {
		if got := p.Match(url); got != want {
			t.Errorf("Match(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestName(t *testing.T) {
	if New().Name() != "directhttp" {
		t.Errorf("Name() = %q, want %q", New().Name(), "directhttp")
	}
}

func TestResolve_UsesContentDispositionAndLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="report.pdf"`)
		w.Header().Set("Content-Length", "12345")
		if r.Method == http.MethodHead {
			return
		}
		t.Errorf("Resolve should only send HEAD, got %s", r.Method)
	}))
	t.Cleanup(srv.Close)

	info, err := New().Resolve(srv.URL+"/download", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != "report.pdf" {
		t.Errorf("Name = %q, want report.pdf", info.Name)
	}
	if info.Size != 12345 {
		t.Errorf("Size = %d, want 12345", info.Size)
	}
	if !info.Available {
		t.Error("expected Available = true")
	}
}

func TestResolve_FallsBackToURLPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
	}))
	t.Cleanup(srv.Close)

	info, err := New().Resolve(srv.URL+"/path/to/my%20file.zip", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Name != "my file.zip" {
		t.Errorf("Name = %q, want %q", info.Name, "my file.zip")
	}
}

func TestResolve_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := New().Resolve(srv.URL, nil)
	if err != plugins.ErrNotFound {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestResolve_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	_, err := New().Resolve(srv.URL, nil)
	if err != plugins.ErrRateLimited {
		t.Errorf("err = %v, want plugins.ErrRateLimited", err)
	}
}

func TestResolve_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := New().Resolve(srv.URL, nil)
	if err == nil {
		t.Fatal("expected an error for HTTP 500")
	}
}

func TestFetchDownloadURL_ReturnsInputUnchanged(t *testing.T) {
	p := New()
	url := "https://example.com/file.zip"
	got, err := p.FetchDownloadURL(url, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != url {
		t.Errorf("got %q, want %q", got, url)
	}
}

func TestFilenameFor(t *testing.T) {
	cases := []struct {
		url, cd, want string
	}{
		{"http://example.com/a/b/file.zip", "", "file.zip"},
		{"http://example.com/", "", "download"},
		{"http://example.com/a/b/file.zip", `attachment; filename="other.zip"`, "other.zip"},
		{"http://example.com/a%20b.txt", "", "a b.txt"},
		// A hoster info-page URL (e.g. rapidgator.net's
		// .../<hash>/<filename>.html) falling through to directhttp with
		// no Content-Disposition header — the general fix alongside
		// hidecx's own fallback, see plugins.FilenameFromURL.
		{"https://rapidgator.net/file/0123456789abcdef0123/movie.part1.rar.html", "", "movie.part1.rar"},
	}
	for _, c := range cases {
		if got := filenameFor(c.url, c.cd); got != c.want {
			t.Errorf("filenameFor(%q, %q) = %q, want %q", c.url, c.cd, got, c.want)
		}
	}
}

func TestContentLength(t *testing.T) {
	resp := &http.Response{ContentLength: -1, Header: http.Header{}}
	resp.Header.Set("Content-Length", "42")
	if got := contentLength(resp); got != 42 {
		t.Errorf("contentLength = %d, want 42", got)
	}

	resp2 := &http.Response{ContentLength: 99, Header: http.Header{}}
	if got := contentLength(resp2); got != 99 {
		t.Errorf("contentLength = %d, want 99", got)
	}

	resp3 := &http.Response{ContentLength: -1, Header: http.Header{}}
	if got := contentLength(resp3); got != -1 {
		t.Errorf("contentLength = %d, want -1", got)
	}
}

func TestResolve_TooManyRedirects(t *testing.T) {
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	target = srv.URL + "/loop"

	_, err := New().Resolve(srv.URL+"/loop", nil)
	if err == nil {
		t.Fatal("expected an error for a redirect loop")
	}
}
