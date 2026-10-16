package api

import (
	"net/http"

	"github.com/simuuh/hopper-downloader/internal/settings"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settings.Get())
}

// handleUpdateSettings applies a partial settings.Update. See
// settings.Manager.Apply: an invalid field rejects the whole request,
// nothing is applied.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req settings.Update
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := s.settings.Apply(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
