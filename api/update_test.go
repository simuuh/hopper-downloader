package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simuuh/hopper-downloader/internal/update"
)

type fakeUpdates struct {
	st     update.Status
	err    error
	checks int
}

func (f *fakeUpdates) Status() update.Status { return f.st }
func (f *fakeUpdates) Check(context.Context) (update.Status, error) {
	f.checks++
	return f.st, f.err
}

func TestUpdateStatus_ReturnsCachedWithoutChecking(t *testing.T) {
	fu := &fakeUpdates{st: update.Status{Current: "v0.9.0", Latest: "v0.10.0", Available: true}}
	srv, _ := newTestServer(t, func(c *Config) { c.Updates = fu })
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/update", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got update.Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.Latest != "v0.10.0" {
		t.Fatalf("body = %+v", got)
	}
	if fu.checks != 0 {
		t.Fatalf("GET /update contacted the release source %d times", fu.checks)
	}
}

func TestUpdateStatus_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, func(c *Config) { c.Updates = &fakeUpdates{} })
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/update", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestUpdateCheck_RunsCheck(t *testing.T) {
	fu := &fakeUpdates{st: update.Status{Current: "v0.9.0", Latest: "v0.9.0"}}
	srv, _ := newTestServer(t, func(c *Config) { c.Updates = fu })
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/update/check", nil, token))
	if w.Code != http.StatusOK || fu.checks != 1 {
		t.Fatalf("status = %d, checks = %d", w.Code, fu.checks)
	}
}

func TestUpdateCheck_FailureIs502(t *testing.T) {
	fu := &fakeUpdates{err: errors.New("update: latest release: HTTP 403")}
	srv, _ := newTestServer(t, func(c *Config) { c.Updates = fu })
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/update/check", nil, token))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestUpdate_NotConfigured(t *testing.T) {
	srv, _ := newTestServer(t, func(c *Config) { c.Version = "v0.9.0" })
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/update", nil, token))
	if w.Code != http.StatusOK || w.Body.String() != "{\"current\":\"v0.9.0\",\"available\":false}\n" {
		t.Fatalf("GET /update = %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/update/check", nil, token))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /update/check = %d, want 503", w.Code)
	}
}
