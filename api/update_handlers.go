package api

import (
	"context"
	"net/http"

	"github.com/simuuh/hopper-downloader/internal/update"
)

// UpdateChecker is the subset of *update.Checker the API needs. May be
// nil (Config.Updates unset), which reports the running version with no
// check result and refuses manual checks.
type UpdateChecker interface {
	Status() update.Status
	Check(ctx context.Context) (update.Status, error)
}

// handleUpdateStatus returns the cached result of the last update check.
// It never contacts GitHub itself.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		writeJSON(w, http.StatusOK, update.Status{Current: s.cfg.Version})
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Updates.Status())
}

// handleUpdateCheck checks for a newer release now, on the user's
// explicit request — independent of the update_check setting, which only
// governs the automatic daily check.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		writeError(w, http.StatusServiceUnavailable, "update check not available")
		return
	}
	st, err := s.cfg.Updates.Check(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}
