package api

import "net/http"

// handleGetConfig returns the pre-redacted configuration snapshot built
// at Server construction time (see Config.ConfigView). No secret ever
// flows through this path.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.ConfigView)
}
