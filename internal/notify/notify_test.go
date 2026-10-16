package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testEvent() Event {
	return Event{
		Event:      EventPackageFinished,
		PackageID:  "p1",
		Package:    "Movie (2026)",
		Status:     StatusCompleted,
		Items:      3,
		Done:       3,
		Bytes:      3 << 30,
		Extract:    "extracted",
		Path:       "Movie (2026)",
		FinishedAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}
}

func TestSend_WebhookReceivesJSONEvent(t *testing.T) {
	var got Event
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
	}))
	defer srv.Close()

	n := New(Config{WebhookURL: srv.URL}, nil, slog.New(slog.DiscardHandler))
	if err := n.Send(context.Background(), testEvent()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if got != testEvent() {
		t.Errorf("webhook got %+v, want %+v", got, testEvent())
	}
}

func TestSend_NtfyHeadersAndBody(t *testing.T) {
	var title, tags, prio, auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dec := new(mime.WordDecoder)
		title, _ = dec.DecodeHeader(r.Header.Get("Title"))
		tags = r.Header.Get("Tags")
		prio = r.Header.Get("Priority")
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()

	n := New(Config{NtfyURL: srv.URL + "/hopper", NtfyToken: "tk_123"}, nil, slog.New(slog.DiscardHandler))
	ev := testEvent()
	ev.Package = "Füße"
	ev.Status = StatusPartial
	ev.Done, ev.Failed = 2, 1
	if err := n.Send(context.Background(), ev); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if title != "Finished with errors: Füße" {
		t.Errorf("Title = %q", title)
	}
	if tags != "hopper,partial" || prio != "high" || auth != "Bearer tk_123" {
		t.Errorf("headers: tags=%q prio=%q auth=%q", tags, prio, auth)
	}
	if !strings.HasPrefix(body, "2/3 files downloaded, 1 failed (3.0 GiB), extracted") {
		t.Errorf("body = %q", body)
	}
}

func TestSend_ReportsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	n := New(Config{WebhookURL: srv.URL, NtfyURL: srv.URL}, nil, slog.New(slog.DiscardHandler))
	err := n.Send(context.Background(), testEvent())
	if err == nil || !strings.Contains(err.Error(), "webhook: HTTP 403") || !strings.Contains(err.Error(), "ntfy: HTTP 403") {
		t.Fatalf("err = %v, want both targets reported", err)
	}
}

func TestSend_NotConfigured(t *testing.T) {
	n := New(Config{}, nil, slog.New(slog.DiscardHandler))
	if n.Enabled() {
		t.Fatal("Enabled() = true with no targets")
	}
	if err := n.Send(context.Background(), testEvent()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	n.PackageFinished(testEvent()) // must be a silent no-op
	n.Wait()
}

func TestPackageFinished_DeliversInBackground(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
	}))
	defer srv.Close()

	n := New(Config{WebhookURL: srv.URL}, nil, slog.New(slog.DiscardHandler))
	n.PackageFinished(testEvent())
	n.Wait()
	select {
	case <-hit:
	default:
		t.Fatal("webhook was not called")
	}
}
