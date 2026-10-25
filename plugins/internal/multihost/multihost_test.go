package multihost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostsSupports(t *testing.T) {
	h := Hosts{}
	h.Add("Rapidgator.net", " rg.to ", "")
	for host, want := range map[string]bool{
		"rapidgator.net":     true,
		"www.rapidgator.net": true,
		"rg.to":              true,
		"notrapidgator.net":  false,
		"net":                false,
		"1fichier.com":       false,
	} {
		if got := h.Supports(host); got != want {
			t.Errorf("Supports(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestHostnameOf(t *testing.T) {
	if got := HostnameOf("https://WWW.Example.com/x"); got != "www.example.com" {
		t.Errorf("got %q", got)
	}
	if got := HostnameOf("ftp://example.com/x"); got != "" {
		t.Errorf("non-http: got %q", got)
	}
}

func TestHostCache_StaleOnErrorAndClear(t *testing.T) {
	var fail atomic.Bool
	calls := 0
	c := &HostCache{TTL: time.Nanosecond, Fetch: func(string) (Hosts, error) {
		calls++
		if fail.Load() {
			return nil, errors.New("down")
		}
		h := Hosts{}
		h.Add("a.example")
		return h, nil
	}}
	if h, err := c.Snapshot("t"); err != nil || !h.Supports("a.example") {
		t.Fatalf("first snapshot: %v %v", h, err)
	}
	fail.Store(true)
	time.Sleep(time.Millisecond)
	h, err := c.Snapshot("t")
	if err == nil || !h.Supports("a.example") {
		t.Fatalf("failed refresh should return stale hosts with the error: %v %v", h, err)
	}
	c.Clear()
	if h, _ := c.Snapshot("t"); h != nil {
		t.Fatalf("after Clear and a failed refresh: %v, want nil", h)
	}
	if calls != 3 {
		t.Errorf("fetch calls = %d, want 3", calls)
	}
}

func TestClientDo_Retries429(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient("test", 1000, 10, 5*time.Second)
	c.RetryDelay = time.Millisecond
	resp, err := c.Do(context.Background(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || n.Load() != 3 {
		t.Errorf("status %d after %d calls", resp.StatusCode, n.Load())
	}
}

func TestResultCache(t *testing.T) {
	c := &ResultCache[string]{TTL: 50 * time.Millisecond}
	c.Put("k", "v")
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatalf("Get = %q %v", v, ok)
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Error("expired entry still returned")
	}
}

// With the service's API down, Match runs for every added link: a failed
// refresh is remembered for FailureBackoff instead of being retried per
// call (M12), but not for another token.
func TestHostCache_RemembersFailure(t *testing.T) {
	var calls atomic.Int32
	c := &HostCache{TTL: time.Hour, Fetch: func(string) (Hosts, error) {
		calls.Add(1)
		return nil, errors.New("api down")
	}}
	for i := 0; i < 5; i++ {
		if _, err := c.Snapshot("token-a"); err == nil {
			t.Fatal("Snapshot succeeded with a failing fetch")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d fetches for 5 calls within the backoff, want 1", n)
	}
	_, _ = c.Snapshot("token-b")
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d fetches after switching token, want 2", n)
	}
}

// Concurrent callers share one refresh (M12).
func TestHostCache_OneRefreshAtATime(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	c := &HostCache{TTL: time.Hour, Fetch: func(string) (Hosts, error) {
		calls.Add(1)
		<-release
		return Hosts{"host.example": true}, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Snapshot("token")
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d fetches for 10 concurrent calls, want 1", n)
	}
}
