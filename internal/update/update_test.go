package update

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// releaseServer answers like GitHub's latest-release endpoint and counts
// requests.
func releaseServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") != "Hopper" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.10.0", "v1.9.0", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.3.0", false},
		{"v1.2.0", "1.1.0", true},
		{"v1.2.0", "v1.2.0-rc1", true},
		{"v1.2.0", "v1.2.1-rc1", false},
		{"v1.2.0", "v1.1.0+build.5", true},
		{"v1.2.0", "dev", false},
		{"v1.2.0", "", false},
		{"garbage", "v1.0.0", false},
	}
	for _, tc := range cases {
		if got := newer(tc.latest, tc.current); got != tc.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestCheck_NewerRelease(t *testing.T) {
	srv, _ := releaseServer(t, http.StatusOK, `{"tag_name":"v0.10.0","html_url":"https://example.invalid/elsewhere"}`)
	c := New("v0.9.0", srv.URL, nil, discardLogger())

	st, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.Latest != "v0.10.0" || st.Current != "v0.9.0" || st.CheckedAt == nil {
		t.Fatalf("status = %+v", st)
	}
	if st.ReleaseURL != releasePage+"v0.10.0" {
		t.Fatalf("release URL = %q, want the project's own release page", st.ReleaseURL)
	}
	if got := c.Status(); !got.Available {
		t.Fatalf("cached status = %+v", got)
	}
}

func TestCheck_UpToDate(t *testing.T) {
	srv, _ := releaseServer(t, http.StatusOK, `{"tag_name":"v0.9.0"}`)
	st, err := New("v0.9.0", srv.URL, nil, discardLogger()).Check(context.Background())
	if err != nil || st.Available || st.Latest != "v0.9.0" {
		t.Fatalf("status = %+v, err = %v", st, err)
	}
}

func TestCheck_FailureKeepsLastResult(t *testing.T) {
	ok, _ := releaseServer(t, http.StatusOK, `{"tag_name":"v0.10.0"}`)
	c := New("v0.9.0", ok.URL, nil, discardLogger())
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}

	limited, _ := releaseServer(t, http.StatusForbidden, `{"message":"rate limited"}`)
	c.url = limited.URL
	c.lastAttempt = time.Time{}
	st, err := c.Check(context.Background())
	if err == nil || st.Error == "" {
		t.Fatalf("want an error, got status %+v", st)
	}
	if !st.Available || st.Latest != "v0.10.0" {
		t.Fatalf("last successful result lost: %+v", st)
	}
}

func TestCheck_RejectsUnexpectedTag(t *testing.T) {
	srv, _ := releaseServer(t, http.StatusOK, `{"tag_name":"v1.0.0/../../evil"}`)
	st, err := New("v0.9.0", srv.URL, nil, discardLogger()).Check(context.Background())
	if err == nil || st.Latest != "" || st.ReleaseURL != "" {
		t.Fatalf("status = %+v, err = %v", st, err)
	}
}

func TestCheck_Throttled(t *testing.T) {
	srv, hits := releaseServer(t, http.StatusOK, `{"tag_name":"v0.10.0"}`)
	c := New("v0.9.0", srv.URL, nil, discardLogger())
	for range 3 {
		if _, err := c.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("requests = %d, want 1 within minCheckInterval", n)
	}
}

func TestCheckIfDue(t *testing.T) {
	srv, hits := releaseServer(t, http.StatusOK, `{"tag_name":"v0.10.0"}`)
	c := New("v0.9.0", srv.URL, nil, discardLogger())

	c.CheckIfDue(context.Background(), time.Hour)
	if hits.Load() != 1 {
		t.Fatalf("first CheckIfDue made %d requests, want 1", hits.Load())
	}

	c.lastAttempt = time.Time{} // isolate from the throttle
	c.CheckIfDue(context.Background(), time.Hour)
	if hits.Load() != 1 {
		t.Fatalf("CheckIfDue within maxAge made a request")
	}

	old := time.Now().Add(-2 * time.Hour)
	c.st.CheckedAt = &old
	c.CheckIfDue(context.Background(), time.Hour)
	if hits.Load() != 2 {
		t.Fatalf("CheckIfDue after maxAge made %d requests, want 2", hits.Load())
	}
}
