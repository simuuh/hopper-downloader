package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simuuh/hopper-downloader/internal/notify"
)

type fakeNotifier struct {
	enabled bool
	err     error
	sent    []notify.Event
}

func (f *fakeNotifier) Enabled() bool { return f.enabled }
func (f *fakeNotifier) Send(_ context.Context, ev notify.Event) error {
	f.sent = append(f.sent, ev)
	return f.err
}

func TestNotifyTest_SendsSampleEvent(t *testing.T) {
	fn := &fakeNotifier{enabled: true}
	srv, _ := newTestServer(t, func(c *Config) { c.Notifier = fn })
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/notify/test", nil, token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if len(fn.sent) != 1 || fn.sent[0].Event != notify.EventPackageFinished {
		t.Fatalf("sent = %+v", fn.sent)
	}
}

func TestNotifyTest_NotConfiguredIs400(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/notify/test", nil, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/notify", nil, token))
	if w.Code != http.StatusOK || w.Body.String() != "{\"enabled\":false}\n" {
		t.Fatalf("GET /notify = %d %q", w.Code, w.Body.String())
	}
}

func TestNotifyTest_DeliveryFailureIs502(t *testing.T) {
	fn := &fakeNotifier{enabled: true, err: errors.New("ntfy: HTTP 403")}
	srv, _ := newTestServer(t, func(c *Config) { c.Notifier = fn })
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/notify/test", nil, token))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}
