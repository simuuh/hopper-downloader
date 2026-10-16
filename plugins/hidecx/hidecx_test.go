package hidecx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

// fakeHoster is a minimal plugins.Hoster for exercising delegation
// without needing a real hoster implementation, matching the fakeHoster
// pattern used in internal/queue/resolve_test.go.
type fakeHoster struct {
	name        string
	info        *plugins.FileInfo
	resolveErr  error
	downloadURL string
	downloadErr error

	gotResolveURL string
	gotResolveAcc *plugins.Account
	gotFetchURL   string
	gotFetchAcc   *plugins.Account
}

func (f *fakeHoster) Name() string      { return f.name }
func (f *fakeHoster) Domains() []string { return nil }
func (f *fakeHoster) Match(string) bool { return true }
func (f *fakeHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	f.gotResolveURL, f.gotResolveAcc = url, acc
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.info, nil
}
func (f *fakeHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	f.gotFetchURL, f.gotFetchAcc = url, acc
	if f.downloadErr != nil {
		return "", f.downloadErr
	}
	return f.downloadURL, nil
}

// newTestPlugin builds a Plugin pointed at srv instead of the real
// api.hide.cx, the same baseURL-override pattern plugins/onefichier
// uses.
func newTestPlugin(srv *httptest.Server, resolve Resolver) *Plugin {
	p := New(resolve, nil)
	if srv != nil {
		p.apiBase = srv.URL
	}
	return p
}

func TestMatch(t *testing.T) {
	p := New(nil, nil)
	cases := map[string]bool{
		"https://hide.cx/container/11111111-2222-4333-8444-555555555555": true,
		"https://hide.cx/11111111-2222-4333-8444-555555555555":           true,
		"http://hide.cx/container/abc":                                   true,
		"https://www.hide.cx/container/x":                                true,
		"https://HIDE.CX/container/x":                                    true,
		"https://rapidgator.net/file/x":                                  false,
		"https://notthide.cx/container/x":                                false,
		"not-a-url":                                                      false,
		"":                                                               false,
	}
	for url, want := range cases {
		if got := p.Match(url); got != want {
			t.Errorf("Match(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestName(t *testing.T) {
	if New(nil, nil).Name() != "hidecx" {
		t.Errorf("Name() = %q, want hidecx", New(nil, nil).Name())
	}
}

func TestCredentialed_IsFalse(t *testing.T) {
	if New(nil, nil).Credentialed() {
		t.Error("Credentialed() = true, want false (hide.cx is public, no account)")
	}
}

func TestContainerUUID(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	cases := map[string]string{
		"https://hide.cx/container/" + uuid:            uuid,
		"https://hide.cx/" + uuid:                      uuid,
		"https://hide.cx/container/not-a-uuid":         "",
		"https://hide.cx/not-a-uuid":                   "",
		"https://hide.cx/container/" + uuid + "/extra": "",
		"https://hide.cx/":                             "",
	}
	for url, want := range cases {
		got, err := containerUUID(url)
		if want == "" {
			if err == nil {
				t.Errorf("containerUUID(%q) = %q, nil, want an error", url, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("containerUUID(%q) = %q, %v, want %q, nil", url, got, err, want)
		}
	}
}

func TestDelegateFor_StillHideCx(t *testing.T) {
	p := New(func(string) (plugins.Hoster, *plugins.Account, bool) {
		t.Fatal("resolve should not be called when the final URL is still hide.cx")
		return nil, nil, false
	}, nil)

	_, _, err := p.delegateFor("https://hide.cx/container/still-here")
	if err == nil || !strings.Contains(err.Error(), "could not resolve hide.cx link") {
		t.Errorf("err = %v, want it to mention %q", err, "could not resolve hide.cx link")
	}
}

func TestDelegateFor_NoPluginMatches(t *testing.T) {
	p := New(func(string) (plugins.Hoster, *plugins.Account, bool) {
		return nil, nil, false
	}, nil)

	_, _, err := p.delegateFor("https://totally-unknown-host.example/x")
	if err == nil || !strings.Contains(err.Error(), "could not resolve hide.cx link") {
		t.Errorf("err = %v, want it to mention %q", err, "could not resolve hide.cx link")
	}
}

func TestDelegateFor_ResolvedBackToSelf(t *testing.T) {
	self := &fakeHoster{name: "hidecx"}
	p := New(func(string) (plugins.Hoster, *plugins.Account, bool) {
		return self, nil, true
	}, nil)

	_, _, err := p.delegateFor("https://example.com/x")
	if err == nil || !strings.Contains(err.Error(), "could not resolve hide.cx link") {
		t.Errorf("err = %v, want it to mention %q", err, "could not resolve hide.cx link")
	}
}

func TestDelegateFor_DelegatesToMatchedHosterAndAccount(t *testing.T) {
	rd := &fakeHoster{name: "realdebrid"}
	acc := &plugins.Account{Username: "u"}
	p := New(func(url string) (plugins.Hoster, *plugins.Account, bool) {
		if url != "https://rapidgator.net/file/x" {
			t.Errorf("resolver got url %q", url)
		}
		return rd, acc, true
	}, nil)

	h, gotAcc, err := p.delegateFor("https://rapidgator.net/file/x")
	if err != nil {
		t.Fatalf("delegateFor: %v", err)
	}
	if h != rd {
		t.Errorf("hoster = %v, want the fake realdebrid plugin", h)
	}
	if gotAcc != acc {
		t.Errorf("account not passed through from resolver")
	}
}

func TestPickLink_PrefersPrimaryHoster(t *testing.T) {
	c := &containerResponse{
		PrimaryHoster: "rapidgator.net",
		Links: []containerLink{
			{ID: "1", Host: "nitroflare.com", LinkStatus: "online"},
			{ID: "2", Host: "rapidgator.net", LinkStatus: "online"},
		},
	}
	link, err := pickLink(c)
	if err != nil {
		t.Fatalf("pickLink: %v", err)
	}
	if link.ID != "2" {
		t.Errorf("picked link %q, want the primary_hoster match (id=2)", link.ID)
	}
}

func TestPickLink_FallsBackToFirstOnline(t *testing.T) {
	c := &containerResponse{
		PrimaryHoster: "rapidgator.net",
		Links: []containerLink{
			{ID: "1", Host: "nitroflare.com", LinkStatus: "offline"},
			{ID: "2", Host: "nitroflare.com", LinkStatus: "online"},
			{ID: "3", Host: "rapidgator.net", LinkStatus: "offline"},
		},
	}
	link, err := pickLink(c)
	if err != nil {
		t.Fatalf("pickLink: %v", err)
	}
	if link.ID != "2" {
		t.Errorf("picked link %q, want the first online link (id=2)", link.ID)
	}
}

func TestPickLink_NoOnlineLinks(t *testing.T) {
	c := &containerResponse{
		Links: []containerLink{{ID: "1", LinkStatus: "offline"}},
	}
	_, err := pickLink(c)
	if !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("err = %v, want plugins.ErrUnavailable", err)
	}
}

// containerHandlerUUID is the fixed container UUID every test in this
// file uses — the real value doesn't matter (hide.cx assigns these),
// only that requests and responses agree on the same one, so it's
// hardcoded here rather than threaded through as a parameter.
const containerHandlerUUID = "11111111-2222-4333-8444-555555555555"

// containerHandler serves a fake /containers/<uuid> and
// /containers/<uuid>/links/<id> pair, checking that hidecx sends the
// headers the real API requires.
func containerHandler(t *testing.T, resp containerResponse, linkURLs map[string]string) http.HandlerFunc {
	t.Helper()
	uuid := containerHandlerUUID
	return func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header = %q", got)
		}
		if got := r.Header.Get("Origin"); got != "https://hide.cx" {
			t.Errorf("Origin header = %q", got)
		}
		if got := r.Header.Get("Referer"); got != "https://hide.cx/" {
			t.Errorf("Referer header = %q", got)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent header missing")
		}

		switch r.URL.Path {
		case "/containers/" + uuid:
			_ = json.NewEncoder(w).Encode(resp)
		default:
			for id, u := range linkURLs {
				if r.URL.Path == "/containers/"+uuid+"/links/"+id {
					_ = json.NewEncoder(w).Encode(linkResolveResponse{URL: u})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"container not found or invalid"}`))
		}
	}
}

func TestResolve_FetchesContainerResolvesLinkAndDelegates(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	info := &plugins.FileInfo{Name: "movie.mkv", Size: 42, Available: true}
	target := &fakeHoster{name: "rapidgator", info: info}

	srv := httptest.NewServer(containerHandler(t, containerResponse{
		AccessStatus:  "online",
		PrimaryHoster: "rapidgator.net",
		Links: []containerLink{
			{ID: "link-1", Host: "rapidgator.net", LinkStatus: "online"},
		},
	}, map[string]string{
		"link-1": "https://rapidgator.net/file/abc/movie.rar.html",
	}))
	t.Cleanup(srv.Close)

	var gotFinalURL string
	p := newTestPlugin(srv, func(url string) (plugins.Hoster, *plugins.Account, bool) {
		gotFinalURL = url
		return target, nil, true
	})

	got, err := p.Resolve("https://hide.cx/container/"+uuid, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != info {
		t.Errorf("Resolve returned %v, want the fake hoster's FileInfo", got)
	}
	want := "https://rapidgator.net/file/abc/movie.rar.html"
	if gotFinalURL != want {
		t.Errorf("resolver saw url %q, want %q", gotFinalURL, want)
	}
	if target.gotResolveURL != want {
		t.Errorf("delegate Resolve got url %q", target.gotResolveURL)
	}
}

func TestFetchDownloadURL_UsesDirectHosterURLFieldWhenPresent(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	target := &fakeHoster{name: "rapidgator", downloadURL: "https://cdn.example/direct-link"}
	directURL := "https://rapidgator.net/file/already-known.html"

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path != "/containers/"+uuid {
			t.Errorf("unexpected request to %s (hoster_url was already present, no link resolve call needed)", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(containerResponse{
			AccessStatus:  "online",
			PrimaryHoster: "rapidgator.net",
			Links: []containerLink{
				{ID: "link-1", Host: "rapidgator.net", LinkStatus: "online", HosterURL: &directURL},
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, func(url string) (plugins.Hoster, *plugins.Account, bool) {
		return target, nil, true
	})

	got, err := p.FetchDownloadURL("https://hide.cx/container/"+uuid, nil)
	if err != nil {
		t.Fatalf("FetchDownloadURL: %v", err)
	}
	if got != "https://cdn.example/direct-link" {
		t.Errorf("got %q", got)
	}
	if target.gotFetchURL != directURL {
		t.Errorf("delegate FetchDownloadURL got url %q, want %q", target.gotFetchURL, directURL)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 request (no link-resolve call), got %d", callCount)
	}
}

func TestFetchContainer_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"container not found or invalid"}`))
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	_, err := p.fetchContainer("00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestFetchContainer_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	_, err := p.fetchContainer("uuid")
	if !errors.Is(err, plugins.ErrRateLimited) {
		t.Errorf("err = %v, want plugins.ErrRateLimited", err)
	}
}

func TestFetchContainer_NotOnlineAccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(containerResponse{
			AccessStatus: "offline",
			Links:        []containerLink{{ID: "1", LinkStatus: "online"}},
		})
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	_, err := p.fetchContainer("uuid")
	if !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("err = %v, want plugins.ErrUnavailable", err)
	}
}

func TestFetchContainer_NoLinks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(containerResponse{AccessStatus: "online"})
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	_, err := p.fetchContainer("uuid")
	if !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("err = %v, want plugins.ErrUnavailable", err)
	}
}

func TestFetchContainer_ParsesRealSampleShape(t *testing.T) {
	// Shape confirmed against a live https://api.hide.cx/containers/<uuid>
	// response (see docs/ARCHITECTURE.md) — trimmed to the fields this plugin reads,
	// plus a couple of neighboring fields to confirm unknown fields don't
	// break decoding.
	const sample = `{
		"id":"11111111-2222-4333-8444-555555555555",
		"name":"Example.Release.2024-rapidgator",
		"access_status":"online",
		"primary_hoster":"rapidgator.net",
		"password":null,
		"links":[
			{"id":"3d00a38c-da68-4404-8462-1825a7e3b25a","host":"rapidgator.net","link_type":"mirror_1","link_status":"online","link_size":1572864000,"file":"x.part1.rar.html","name":"x.part1.rar","hoster_url":null},
			{"id":"5513be81-276f-4dde-abee-4e241fe66306","host":"rapidgator.net","link_type":"mirror_1","link_status":"online","link_size":1524337665,"file":"x.part6.rar.html","name":"x.part6.rar","hoster_url":null}
		],
		"canonical_url":"https://hide.cx/container/11111111-2222-4333-8444-555555555555"
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sample))
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	c, err := p.fetchContainer("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("fetchContainer: %v", err)
	}
	if c.AccessStatus != "online" || c.PrimaryHoster != "rapidgator.net" {
		t.Errorf("container = %+v", c)
	}
	if len(c.Links) != 2 {
		t.Fatalf("Links = %d, want 2", len(c.Links))
	}
	if c.Links[0].HosterURL != nil {
		t.Errorf("Links[0].HosterURL = %v, want nil (matches real API behavior)", c.Links[0].HosterURL)
	}
}

// realSixPartContainer reproduces the exact shape of a live
// GET https://api.hide.cx/containers/11111111-2222-4333-8444-555555555555
// response (see docs/ARCHITECTURE.md): 6 links for one multi-part RAR, returned
// out of numeric order (1, 6, 3, 4, 2, 5) — exactly as the real API does.
func realSixPartContainer() containerResponse {
	return containerResponse{
		AccessStatus:  "online",
		PrimaryHoster: "rapidgator.net",
		Links: []containerLink{
			{ID: "3d00a38c", Host: "rapidgator.net", Name: "52e0dabfb9ede5f9.part1.rar", LinkStatus: "online", LinkSize: 1572864000},
			{ID: "5513be81", Host: "rapidgator.net", Name: "52e0dabfb9ede5f9.part6.rar", LinkStatus: "online", LinkSize: 1524337665},
			{ID: "5ff14a67", Host: "rapidgator.net", Name: "52e0dabfb9ede5f9.part3.rar", LinkStatus: "online", LinkSize: 1572864000},
			{ID: "6559614f", Host: "rapidgator.net", Name: "52e0dabfb9ede5f9.part4.rar", LinkStatus: "online", LinkSize: 1572864000},
			{ID: "915275a1", Host: "rapidgator.net", Name: "52e0dabfb9ede5f9.part2.rar", LinkStatus: "online", LinkSize: 1572864000},
			{ID: "c0147db7", Host: "rapidgator.net", Name: "52e0dabfb9ede5f9.part5.rar", LinkStatus: "online", LinkSize: 1572864000},
		},
	}
}

func TestExpand_ReturnsAllOnlineLinksSortedNumerically(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	// Keyed by link ID, matching realSixPartContainer's IDs — the fake
	// per-link resolve endpoint just echoes back a URL containing the
	// same part number, so asserting on links[i].URL doubles as an
	// end-to-end check that each link was resolved via its own ID, not
	// mixed up during sorting.
	linkURLs := map[string]string{
		"3d00a38c": "https://rapidgator.net/file/x/part1.rar.html",
		"5513be81": "https://rapidgator.net/file/x/part6.rar.html",
		"5ff14a67": "https://rapidgator.net/file/x/part3.rar.html",
		"6559614f": "https://rapidgator.net/file/x/part4.rar.html",
		"915275a1": "https://rapidgator.net/file/x/part2.rar.html",
		"c0147db7": "https://rapidgator.net/file/x/part5.rar.html",
	}
	srv := httptest.NewServer(containerHandler(t, realSixPartContainer(), linkURLs))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	links, err := p.Expand("https://hide.cx/container/" + uuid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(links) != 6 {
		t.Fatalf("len(links) = %d, want 6", len(links))
	}
	for i, l := range links {
		wantPart := i + 1
		wantNameSuffix := fmt.Sprintf("part%d.rar", wantPart)
		wantURLSuffix := fmt.Sprintf("part%d.rar.html", wantPart)
		if !strings.HasSuffix(l.Name, wantNameSuffix) {
			t.Errorf("links[%d].Name = %q, want the part%d link (numeric order)", i, l.Name, wantPart)
		}
		if !strings.HasSuffix(l.URL, wantURLSuffix) {
			t.Errorf("links[%d].URL = %q, want it to resolve to the matching part%d link", i, l.URL, wantPart)
		}
	}
}

func TestExpand_UsesContainerPasswordForEveryLink(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	pw := "s3cr3t"
	c := containerResponse{
		AccessStatus: "online",
		Password:     &pw,
		Links: []containerLink{
			{ID: "l1", Name: "x.part1.rar", LinkStatus: "online"},
			{ID: "l2", Name: "x.part2.rar", LinkStatus: "online"},
		},
	}
	srv := httptest.NewServer(containerHandler(t, c, map[string]string{
		"l1": "https://rapidgator.net/file/1.html",
		"l2": "https://rapidgator.net/file/2.html",
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	links, err := p.Expand("https://hide.cx/container/" + uuid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for _, l := range links {
		if l.Password != pw {
			t.Errorf("link %q Password = %q, want %q", l.Name, l.Password, pw)
		}
	}
}

func TestExpand_NullPasswordBecomesEmptyString(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	c := containerResponse{
		AccessStatus: "online",
		Password:     nil,
		Links:        []containerLink{{ID: "l1", Name: "x.rar", LinkStatus: "online"}},
	}
	srv := httptest.NewServer(containerHandler(t, c, map[string]string{"l1": "https://rapidgator.net/file/1.html"}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	links, err := p.Expand("https://hide.cx/container/" + uuid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if links[0].Password != "" {
		t.Errorf("Password = %q, want empty for a null container password", links[0].Password)
	}
}

// TestExpand_EmptyNameFallsBackToURLDerivedFilename is the regression
// test for the bug where a hidecx-expanded item ended up with a queue
// item filename that was actually the rapidgator.net page URL's own
// path (hash and all), not a real filename: if the container link's
// "name" field ever comes back empty, Expand must not just pass that
// empty string through — queue.Manager.addExpandedItems trusts
// ExpandedLink.Name as the item's Filename verbatim, and an empty one
// falls all the way through to whatever plugin ends up resolving the
// URL (directhttp, if no hoster account is configured), which showed
// the raw URL instead.
func TestExpand_EmptyNameFallsBackToURLDerivedFilename(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	c := containerResponse{
		AccessStatus: "online",
		Links:        []containerLink{{ID: "l1", Name: "", LinkStatus: "online"}},
	}
	hosterURL := "https://rapidgator.net/file/0123456789abcdef0123/00000000-0000-4000-8000-000000000001.part1.rar.html"
	srv := httptest.NewServer(containerHandler(t, c, map[string]string{"l1": hosterURL}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	links, err := p.Expand("https://hide.cx/container/" + uuid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := "00000000-0000-4000-8000-000000000001.part1.rar"
	if len(links) != 1 || links[0].Name != want {
		t.Errorf("Name = %q, want %q (derived from the URL, .html stripped)", links[0].Name, want)
	}
}

func TestExpand_SkipsOfflineLinks(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	c := containerResponse{
		AccessStatus: "online",
		Links: []containerLink{
			{ID: "l1", Name: "x.part1.rar", LinkStatus: "online"},
			{ID: "l2", Name: "x.part2.rar", LinkStatus: "offline"},
		},
	}
	srv := httptest.NewServer(containerHandler(t, c, map[string]string{"l1": "https://rapidgator.net/file/1.html"}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	links, err := p.Expand("https://hide.cx/container/" + uuid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(links) != 1 || links[0].Name != "x.part1.rar" {
		t.Errorf("links = %+v, want only the online part1 link", links)
	}
}

func TestExpand_NoOnlineLinksReturnsUnavailable(t *testing.T) {
	const uuid = "11111111-2222-4333-8444-555555555555"
	c := containerResponse{
		AccessStatus: "online",
		Links:        []containerLink{{ID: "l1", Name: "x.rar", LinkStatus: "offline"}},
	}
	srv := httptest.NewServer(containerHandler(t, c, nil))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	_, err := p.Expand("https://hide.cx/container/" + uuid)
	if !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("err = %v, want plugins.ErrUnavailable", err)
	}
}

func TestExpand_ContainerFetchErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(srv, nil)
	_, err := p.Expand("https://hide.cx/container/11111111-2222-4333-8444-555555555555")
	if !errors.Is(err, plugins.ErrNotFound) {
		t.Errorf("err = %v, want plugins.ErrNotFound", err)
	}
}

func TestExpand_InvalidURLReturnsError(t *testing.T) {
	p := New(nil, nil)
	if _, err := p.Expand("https://hide.cx/"); err == nil {
		t.Error("expected an error for a URL with no extractable UUID")
	}
}

func TestSortLinksByPart_NumericNotLexicographic(t *testing.T) {
	links := []plugins.ExpandedLink{
		{Name: "x.part10.rar"},
		{Name: "x.part2.rar"},
		{Name: "x.part1.rar"},
	}
	sortLinksByPart(links)
	want := []string{"x.part1.rar", "x.part2.rar", "x.part10.rar"}
	for i, w := range want {
		if links[i].Name != w {
			t.Errorf("links[%d].Name = %q, want %q (numeric order, part10 after part2)", i, links[i].Name, w)
		}
	}
}

func TestSortLinksByPart_FallsBackToNameForNonPartPattern(t *testing.T) {
	links := []plugins.ExpandedLink{
		{Name: "zeta.rar"},
		{Name: "alpha.rar"},
	}
	sortLinksByPart(links)
	if links[0].Name != "alpha.rar" || links[1].Name != "zeta.rar" {
		t.Errorf("links = %+v, want alphabetical fallback order", links)
	}
}
