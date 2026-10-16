package rapidgator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

// freeDownloadFakeAPI is a minimal server for FetchFreeDownloadURL tests:
// a working /user/login (so callWithToken's session-token exchange
// succeeds) plus a scripted, flat (non-enveloped — see
// freeDownloadResponse's doc comment) POST /file/download response.
type freeDownloadFakeAPI struct {
	t           *testing.T
	issuedToken string
	response    map[string]any
}

func (f *freeDownloadFakeAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/login":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"response": map[string]any{"token": f.issuedToken},
				"status":   http.StatusOK,
				"details":  "",
			})
		case "/file/download":
			if r.Method != http.MethodPost {
				f.t.Errorf("free file/download: method = %s, want POST", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				f.t.Fatalf("parse form: %v", err)
			}
			if got := r.FormValue("token"); got != f.issuedToken {
				f.t.Errorf("free file/download: token = %q, want %q", got, f.issuedToken)
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(f.response)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newFreeDownloadTestPlugin(t *testing.T, response map[string]any) *Plugin {
	t.Helper()
	api := &freeDownloadFakeAPI{t: t, issuedToken: "session-token-abc", response: response}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)

	p := New(&fakeTokens{username: "user@example.com", password: "correct-password"}, nil)
	p.baseURL = srv.URL
	return p
}

func TestFetchFreeDownloadURL_Success(t *testing.T) {
	p := newFreeDownloadTestPlugin(t, map[string]any{"url": "https://dl.rapidgator.net/free/abc.mkv"})

	got, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/abcdef123456/movie.rar.html", nil)
	if err != nil {
		t.Fatalf("FetchFreeDownloadURL: %v", err)
	}
	if got != "https://dl.rapidgator.net/free/abc.mkv" {
		t.Errorf("got %q", got)
	}
}

func TestFetchFreeDownloadURL_WaitTimeReturnsErrRetryAfter(t *testing.T) {
	p := newFreeDownloadTestPlugin(t, map[string]any{"wait_time": 45})

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/abcdef123456/movie.rar.html", nil)
	var retryErr plugins.ErrRetryAfter
	if !errors.As(err, &retryErr) {
		t.Fatalf("err = %v, want plugins.ErrRetryAfter", err)
	}
	if retryErr.Seconds != 45 {
		t.Errorf("Seconds = %d, want 45", retryErr.Seconds)
	}
}

func TestFetchFreeDownloadURL_CaptchaReturnsErrCaptchaRequired(t *testing.T) {
	p := newFreeDownloadTestPlugin(t, map[string]any{"captcha": map[string]any{"image_url": "https://rapidgator.net/captcha.png"}})

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/abcdef123456/movie.rar.html", nil)
	var captchaErr plugins.ErrCaptchaRequired
	if !errors.As(err, &captchaErr) {
		t.Fatalf("err = %v, want plugins.ErrCaptchaRequired", err)
	}
	if captchaErr.ImageURL != "https://rapidgator.net/captcha.png" {
		t.Errorf("ImageURL = %q, want the captcha image URL", captchaErr.ImageURL)
	}
}

func TestFetchFreeDownloadURL_ErrorFieldSurfacesAsError(t *testing.T) {
	p := newFreeDownloadTestPlugin(t, map[string]any{"error": "daily limit reached"})

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/abcdef123456/movie.rar.html", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var retryErr plugins.ErrRetryAfter
	var captchaErr plugins.ErrCaptchaRequired
	if errors.As(err, &retryErr) || errors.As(err, &captchaErr) {
		t.Errorf("err = %v, should be a plain error, not ErrRetryAfter/ErrCaptchaRequired", err)
	}
}

func TestFetchFreeDownloadURL_NoAccountConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil) // no username/password
	_, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/abcdef123456/movie.rar.html", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestPluginSatisfiesFreeDownloader(t *testing.T) {
	var _ plugins.FreeDownloader = (*Plugin)(nil)
}
