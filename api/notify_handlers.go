package api

import (
	"context"
	"net/http"
	"time"

	"github.com/simuuh/hopper-downloader/internal/notify"
)

// Notifier is the subset of *notify.Notifier the API needs. May be nil
// (Config.Notifier unset), which reports notifications as not configured.
type Notifier interface {
	Enabled() bool
	Send(ctx context.Context, ev notify.Event) error
}

// handleNotifyStatus reports whether any notification target is configured
// (the targets themselves — URLs, tokens — are never returned).
func (s *Server) handleNotifyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": s.cfg.Notifier != nil && s.cfg.Notifier.Enabled()})
}

// handleNotifyTest sends a sample "package finished" notification to every
// configured target synchronously and reports the outcome, so a user can
// verify a webhook/ntfy setup without waiting for a real download.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Notifier == nil || !s.cfg.Notifier.Enabled() {
		writeError(w, http.StatusBadRequest, "no notification target configured (notify.webhook_url / notify.ntfy_url in hopper.yaml)")
		return
	}
	ev := notify.Event{
		Event:      notify.EventPackageFinished,
		Package:    "Hopper test notification",
		Status:     notify.StatusCompleted,
		Items:      1,
		Done:       1,
		FinishedAt: time.Now(),
	}
	if err := s.cfg.Notifier.Send(r.Context(), ev); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
