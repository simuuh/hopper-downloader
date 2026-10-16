package onefichier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

func freeDownloadHandler(t *testing.T, wantToken string, body map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, wantToken)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

func TestFetchFreeDownloadURL_Success(t *testing.T) {
	p := newTestPlugin(t, freeDownloadHandler(t, "test-key", map[string]any{
		"status": "OK",
		"url":    "https://1fichier.com/free-dl/abc",
	}), &fakeTokens{token: "test-key"})

	got, err := p.FetchFreeDownloadURL(context.Background(), "https://1fichier.com/?abc123", nil)
	if err != nil {
		t.Fatalf("FetchFreeDownloadURL: %v", err)
	}
	if got != "https://1fichier.com/free-dl/abc" {
		t.Errorf("got %q", got)
	}
}

func TestFetchFreeDownloadURL_DelayReturnsErrRetryAfter(t *testing.T) {
	p := newTestPlugin(t, freeDownloadHandler(t, "test-key", map[string]any{
		"status": "KO",
		"delay":  30,
	}), &fakeTokens{token: "test-key"})

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://1fichier.com/?abc123", nil)
	var retryErr plugins.ErrRetryAfter
	if !errors.As(err, &retryErr) {
		t.Fatalf("err = %v, want plugins.ErrRetryAfter", err)
	}
	if retryErr.Seconds != 30 {
		t.Errorf("Seconds = %d, want 30", retryErr.Seconds)
	}
}

func TestFetchFreeDownloadURL_MessageSurfacesAsPlainError(t *testing.T) {
	// No captcha-image field is specified for 1fichier's free flow — a
	// message must never be turned into a plugins.ErrCaptchaRequired with
	// a fabricated ImageURL (see freeDownloadTokenResponse's doc comment).
	p := newTestPlugin(t, freeDownloadHandler(t, "test-key", map[string]any{
		"status":  "KO",
		"message": "please solve the captcha",
	}), &fakeTokens{token: "test-key"})

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://1fichier.com/?abc123", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var captchaErr plugins.ErrCaptchaRequired
	if errors.As(err, &captchaErr) {
		t.Errorf("err = %v, should not be ErrCaptchaRequired (no image URL field exists to populate it with)", err)
	}
}

func TestFetchFreeDownloadURL_NoTokenConfigured(t *testing.T) {
	p := newTestPlugin(t, http.NotFoundHandler(), &fakeTokens{})

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://1fichier.com/?abc123", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestPluginSatisfiesFreeDownloader(t *testing.T) {
	var _ plugins.FreeDownloader = (*Plugin)(nil)
}
