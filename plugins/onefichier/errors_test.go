package onefichier

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

func TestKOErr(t *testing.T) {
	cases := []struct {
		msg  string
		want error
	}{
		{"File not found or deleted", plugins.ErrNotFound},
		{"Resource does not exist", plugins.ErrNotFound},
		{"File removed by owner", plugins.ErrNotFound},
		{"Flood detected, try later", plugins.ErrRateLimited},
		{"Too many requests", plugins.ErrRateLimited},
		{"Bad API key", plugins.ErrUnavailable},
		{"Only 1 location allowed at a time", plugins.ErrUnavailable},
		{"", plugins.ErrUnavailable},
	}
	for _, tc := range cases {
		if got := koErr(tc.msg); !errors.Is(got, tc.want) {
			t.Errorf("koErr(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// A KO that isn't clearly "file gone" must not fail the item for good.
func TestResolve_KOWithoutNotFoundIsTransient(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "KO", "message": "Flood detected"})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	_, err := p.Resolve("https://1fichier.com/?placeholder", nil)
	if errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("flood KO mapped to ErrNotFound: %v", err)
	}
	if !errors.Is(err, plugins.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestResolve_KONotFoundStaysPermanent(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "KO", "message": "File not found or deleted"})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	if _, err := p.Resolve("https://1fichier.com/?placeholder", nil); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
