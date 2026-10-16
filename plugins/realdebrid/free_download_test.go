package realdebrid

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

func TestFetchFreeDownloadURL_Success(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/unrestrict/link" {
			t.Errorf("path = %q, want /unrestrict/link", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"download": "https://xyz.real-debrid.com/d/abc/movie.mkv"})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), "test-token")

	got, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/123", nil)
	if err != nil {
		t.Fatalf("FetchFreeDownloadURL: %v", err)
	}
	if got != "https://xyz.real-debrid.com/d/abc/movie.mkv" {
		t.Errorf("got %q", got)
	}
}

func TestFetchFreeDownloadURL_TrafficExhaustedReturnsAccountRequired(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "traffic_exhausted", "error_code": rdErrorCodeTrafficExhausted})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), "test-token")

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/123", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired (no free fallback for RD once traffic is exhausted)", err)
	}
}

func TestFetchFreeDownloadURL_NoTokenConfigured(t *testing.T) {
	p := newTestPlugin(t, http.NotFoundHandler(), "")

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/123", nil)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestFetchFreeDownloadURL_OtherErrorCodeSurfacesAsGenericError(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "hoster_unavailable", "error_code": 34})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), "test-token")

	_, err := p.FetchFreeDownloadURL(context.Background(), "https://rapidgator.net/file/123", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, plugins.ErrAccountRequired) {
		t.Errorf("err = %v, should not be ErrAccountRequired for an unrelated error_code", err)
	}
}

func TestPluginSatisfiesFreeDownloader(t *testing.T) {
	var _ plugins.FreeDownloader = (*Plugin)(nil)
}
