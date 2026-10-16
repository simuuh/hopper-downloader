package clicknload

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/simuuh/hopper-downloader/internal/queue"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSink records calls instead of touching a real queue.Manager.
type fakeSink struct {
	links   []string
	pkgName string
	pkgURLs []string
}

func (f *fakeSink) AddLinks(links []queue.LinkInput, name string, opts queue.AddOptions) (*queue.AddResult, error) {
	urls := make([]string, len(links))
	items := make([]*queue.Item, len(links))
	for i, l := range links {
		urls[i] = l.URL
		items[i] = &queue.Item{ID: "item", URL: l.URL, Password: opts.Password}
	}
	if len(urls) == 1 && name == "" {
		f.links = append(f.links, urls...)
	} else {
		f.pkgName = name
		f.pkgURLs = urls
	}
	return &queue.AddResult{Items: items}, nil
}

// fakeAutoStart always reports true, matching the pre-existing (pre-
// auto_start_imports) test expectation that AddLink/AddPackage calls
// aren't paused.
type fakeAutoStart struct{}

func (fakeAutoStart) AutoStartImports() bool { return true }

func localhostOnlyConfig() Config {
	_, cidr, _ := net.ParseCIDR("127.0.0.1/32")
	return Config{Host: "127.0.0.1", Port: 0, TrustedNets: []*net.IPNet{cidr}}
}

func TestHandleCheck_ContainsJDownloaderAndHopper(t *testing.T) {
	s := New(&fakeSink{}, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/flash/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "JDownloader") {
		t.Errorf("expected response to contain %q for CNL button detection, got %q", "JDownloader", body)
	}
	if !strings.Contains(body, "Hopper") {
		t.Errorf("expected response to self-identify as Hopper, got %q", body)
	}
}

func TestHandleCheckJS(t *testing.T) {
	s := New(&fakeSink{}, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/jdcheck.js", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "jdownloader=true") {
		t.Errorf("expected jdcheck.js response to contain jdownloader=true, got %q", w.Body.String())
	}
}

// TestTrustedClient_IPv4MappedIPv6Accepted covers the fix for a real gap:
// a dual-stack listener can see an IPv4 client's RemoteAddr in its
// IPv4-mapped IPv6 form (::ffff:127.0.0.1) rather than plain IPv4 —
// net.IPNet.Contains on an IPv4 CIDR (the common trusted_cidrs case)
// doesn't match that form unless it's unwrapped first via To4.
func TestTrustedClient_IPv4MappedIPv6Accepted(t *testing.T) {
	s := New(&fakeSink{}, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/flash/", nil)
	req.RemoteAddr = "[::ffff:127.0.0.1]:54321"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for an IPv4-mapped IPv6 client inside trusted_cidrs", w.Code)
	}
}

func TestUntrustedClient_Rejected(t *testing.T) {
	s := New(&fakeSink{}, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/flash/", nil)
	req.RemoteAddr = "203.0.113.5:54321" // not in 127.0.0.1/32
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func checkCORSHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := h.Get("Access-Control-Allow-Methods"); got != "POST, GET, OPTIONS" {
		t.Errorf("Access-Control-Allow-Methods = %q, want %q", got, "POST, GET, OPTIONS")
	}
	if got := h.Get("Access-Control-Allow-Headers"); got != "Content-Type, *" {
		t.Errorf("Access-Control-Allow-Headers = %q, want %q", got, "Content-Type, *")
	}
}

// TestCORSHeaders_OnTrustedResponse covers the actual bug: hide.cx's
// Click'n'Load button POSTs directly from its HTTPS page origin to this
// listener, which the browser treats as cross-origin regardless of
// trusted_cidrs/loopback binding — those govern which sockets can reach
// this listener at all, not what a browser is willing to let the
// calling page's JS read back from the response.
func TestCORSHeaders_OnTrustedResponse(t *testing.T) {
	s := New(&fakeSink{}, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/flash/", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	checkCORSHeaders(t, w.Header())
}

// TestCORSHeaders_OnUntrustedRejection covers the "ALL responses" part
// specifically: CORS headers must still be present on trustMiddleware's
// own 403, since corsMiddleware wraps outside it, not the other way
// around.
func TestCORSHeaders_OnUntrustedRejection(t *testing.T) {
	s := New(&fakeSink{}, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/flash/", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	checkCORSHeaders(t, w.Header())
}

// TestCORSHeaders_OPTIONSBypassesTrustCheck: a preflight OPTIONS from an
// untrusted IP must still succeed — it carries no CnL payload and
// triggers no queue action by itself, so gating it the same as a real
// GET/POST would just break the preflight negotiation for legitimate
// callers without adding any real protection (the actual POST that
// follows a successful preflight is still fully gated by
// trustMiddleware, unconditionally).
func TestCORSHeaders_OPTIONSBypassesTrustCheck(t *testing.T) {
	s := New(&fakeSink{}, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())
	req := httptest.NewRequest(http.MethodOptions, "/flash/add", nil)
	req.RemoteAddr = "203.0.113.5:54321" // not in 127.0.0.1/32
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	checkCORSHeaders(t, w.Header())
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
}

func TestHandleAdd_PlainSingleURL(t *testing.T) {
	sink := &fakeSink{}
	s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	form := url.Values{"urls": {"http://example.com/file1"}, "passwords": {"secret"}}
	req := httptest.NewRequest(http.MethodPost, "/flash/add", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "success" {
		t.Errorf("body = %q, want %q", w.Body.String(), "success")
	}
	if len(sink.links) != 1 || sink.links[0] != "http://example.com/file1" {
		t.Errorf("unexpected links added: %v", sink.links)
	}
}

// TestHandleAdd_SkipsInvalidURLsButQueuesTheRest covers the fix for a
// real gap: previously, whether an invalid URL among several valid ones
// was silently processed or dropped depended entirely on queue.Manager's
// own internals, with no log trail explaining what happened to it. Now
// it's filtered out before ever reaching the sink, and the rest still
// queue normally.
func TestHandleAdd_SkipsInvalidURLsButQueuesTheRest(t *testing.T) {
	sink := &fakeSink{}
	s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	urls := strings.Join([]string{
		"http://example.com/file1",
		"not-a-valid-url",
		"http://example.com/file2",
	}, "\r\n")
	form := url.Values{"urls": {urls}}
	req := httptest.NewRequest(http.MethodPost, "/flash/add", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(sink.pkgURLs) != 2 {
		t.Fatalf("expected 2 valid URLs queued, got %v", sink.pkgURLs)
	}
	for _, want := range []string{"http://example.com/file1", "http://example.com/file2"} {
		found := false
		for _, got := range sink.pkgURLs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q among queued URLs, got %v", want, sink.pkgURLs)
		}
	}
}

// TestHandleAdd_AllURLsInvalid_Rejected covers the other end: when
// nothing in the submission is a usable URL, nothing should reach the
// sink at all, and the caller gets a 400 rather than an opaque 500 from
// deeper inside queue.Manager.
func TestHandleAdd_AllURLsInvalid_Rejected(t *testing.T) {
	sink := &fakeSink{}
	s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	form := url.Values{"urls": {"not-a-url\r\nftp://also-not-supported"}}
	req := httptest.NewRequest(http.MethodPost, "/flash/add", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(sink.links)+len(sink.pkgURLs) != 0 {
		t.Error("expected nothing queued when every submitted URL is invalid")
	}
}

func TestHandleAdd_PlainMultiURL_CreatesPackage(t *testing.T) {
	sink := &fakeSink{}
	s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	urls := "http://example.com/file1\r\nhttp://example.com/file2"
	form := url.Values{"urls": {urls}, "source": {"http://referrer.example.com/page"}}
	req := httptest.NewRequest(http.MethodPost, "/flash/add", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(sink.pkgURLs) != 2 {
		t.Fatalf("expected 2 URLs in package, got %v", sink.pkgURLs)
	}
	if sink.pkgName != "referrer.example.com" {
		t.Errorf("pkgName = %q, want host derived from source", sink.pkgName)
	}
}

// encryptCNL2 is the inverse of decryptCNL2, used to build test fixtures
// the same way a real hoster-site CNL2 button script would.
func encryptCNL2(t *testing.T, plaintext []byte, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func TestHandleAddCrypted2_DecryptsAndQueues(t *testing.T) {
	sink := &fakeSink{}
	s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	key := []byte("0123456789abcdef") // 16 bytes
	hexKey := hex.EncodeToString(key)
	plaintext := "http://example.com/a\r\nhttp://example.com/b"
	crypted := encryptCNL2(t, []byte(plaintext), key)

	form := url.Values{
		"jk":        {"function f(){ return '" + hexKey + "'; }"},
		"crypted":   {crypted},
		"passwords": {"pw1"},
		"source":    {"http://hoster.example.com/x"},
	}
	req := httptest.NewRequest(http.MethodPost, "/flash/addcrypted2", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(sink.pkgURLs) != 2 || sink.pkgURLs[0] != "http://example.com/a" || sink.pkgURLs[1] != "http://example.com/b" {
		t.Fatalf("unexpected decrypted URLs: %v", sink.pkgURLs)
	}
}

func TestHandleAddCrypted2_BadKey_Rejected(t *testing.T) {
	sink := &fakeSink{}
	s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	form := url.Values{
		"jk":      {"function f(){ return 'not-hex!!'; }"},
		"crypted": {"AAAA"},
	}
	req := httptest.NewRequest(http.MethodPost, "/flash/addcrypted2", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(sink.links)+len(sink.pkgURLs) != 0 {
		t.Error("expected no links queued on decrypt failure")
	}
}

// TestHandleAdd_GenericSource_DerivesNameFromFilenames covers the actual
// bug: real CNL submissions (link protectors' button scripts,
// etc.) very often send an empty or generic "source", and the old
// fallback ("Click'n'Load", literally) made an ugly, uninformative
// folder name for every such package. A common multi-part-RAR naming
// pattern across the URLs should all reduce to the same base name.
func TestHandleAdd_GenericSource_DerivesNameFromFilenames(t *testing.T) {
	sink := &fakeSink{}
	s := New(sink, fakeAutoStart{}, localhostOnlyConfig(), discardLogger())

	urls := strings.Join([]string{
		"https://rapidgator.net/file/abc/movie.part1.rar.html",
		"https://rapidgator.net/file/def/movie.part2.rar.html",
	}, "\r\n")
	form := url.Values{"urls": {urls}} // no "source" at all
	req := httptest.NewRequest(http.MethodPost, "/flash/add", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if sink.pkgName != "movie" {
		t.Errorf("pkgName = %q, want %q (derived from the shared filename base)", sink.pkgName, "movie")
	}
}

func TestPackageName(t *testing.T) {
	cases := []struct {
		name   string
		source string
		urls   []string
		want   string
	}{
		{
			name:   "meaningful source kept unchanged",
			source: "http://links.example.org/some/page",
			urls:   []string{"http://host/movie.part1.rar.html", "http://host/movie.part2.rar.html"},
			want:   "links.example.org",
		},
		{
			name: "empty source falls back to filenames",
			urls: []string{"http://host/movie.part1.rar.html", "http://host/movie.part2.rar.html"},
			want: "movie",
		},
		{
			name:   "literal Click'n'Load host treated as generic",
			source: "http://click'n'load/x", // not a real hostname, but exercises the map lookup path
			urls:   []string{"http://host/report.pdf"},
			want:   "report",
		},
		{
			name: "no common multi-part suffix falls back to shared prefix",
			urls: []string{"http://host/report-part-a.bin", "http://host/report-part-b.bin"},
			want: "report-part",
		},
		{
			name: "no shared prefix at all falls back to first filename",
			urls: []string{"http://host/alpha.bin", "http://host/zeta.bin"},
			want: "alpha",
		},
		{
			name: "single url",
			urls: []string{"http://host/standalone-file.zip"},
			want: "standalone-file",
		},
		{
			name: "no usable filename in any url falls back to the literal default",
			urls: []string{"http://host/", "http://host"},
			want: "Click'n'Load",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := packageName(c.source, c.urls); got != c.want {
				t.Errorf("packageName(%q, %v) = %q, want %q", c.source, c.urls, got, c.want)
			}
		})
	}
}

func TestCommonPrefix(t *testing.T) {
	cases := []struct {
		strs []string
		want string
	}{
		{[]string{"movie.part1.rar", "movie.part2.rar"}, "movie"},
		// Shared prefix is "ab" — packageNameFromURLs' own >= 3 floor is
		// what decides whether that's usable, not commonPrefix itself,
		// which just reports the longest shared prefix it found.
		{[]string{"abc", "abd"}, "ab"},
		{[]string{"onlyone"}, "onlyone"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := commonPrefix(c.strs); got != c.want {
			t.Errorf("commonPrefix(%v) = %q, want %q", c.strs, got, c.want)
		}
	}
}

func TestFilenameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://rapidgator.net/file/abc/movie.part1.rar.html": "movie.part1.rar",
		"http://host/plain.zip":                                "plain.zip",
		"http://host/a%20b.zip":                                "a b.zip",
		"http://host/":                                         "",
		"not a url\x7f":                                        "",
	}
	for in, want := range cases {
		if got := filenameFromURL(in); got != want {
			t.Errorf("filenameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList("a\r\nb\r\n\r\nc\n  \n")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
