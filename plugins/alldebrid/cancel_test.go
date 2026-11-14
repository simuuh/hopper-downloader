package alldebrid

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

// Polling a "delayed" link can take minutes; cancelling the caller's
// context (pause, cancel, shutdown) must end it right away (M13).
func TestFetchDownloadURL_DelayedPollingStopsOnCancel(t *testing.T) {
	p := fakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v4/link/unlock":  reply(`{"status":"success","data":{"delayed":42}}`),
		"/v4/link/delayed": reply(`{"status":"success","data":{"status":1,"time_left":600}}`),
	})
	p.pollInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	_, err := p.FetchDownloadURL(ctx, "https://rapidgator.net/file/placeholder", &plugins.Account{Password: testKey})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("FetchDownloadURL returned %s after cancel", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
