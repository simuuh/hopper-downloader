package onefichier

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"golang.org/x/time/rate"
)

// Every API call goes through the limiter: a limiter that admits
// nothing keeps the request from ever reaching the server.
func TestDo_WaitsForRateLimiter(t *testing.T) {
	var hits atomic.Int32
	handler := func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"filename": "f.bin", "size": 1})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})
	p.limiter = rate.NewLimiter(0, 0)

	if _, err := p.Resolve(context.Background(), "https://1fichier.com/?placeholder", nil); err == nil {
		t.Fatal("request went through a closed limiter")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server hit %d times", n)
	}
}
