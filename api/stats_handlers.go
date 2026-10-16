package api

import (
	"net/http"

	"github.com/simuuh/hopper-downloader/internal/stats"
)

// statsAPIResponse is GET /api/v1/stats's response shape: every period the
// Web UI's stats dashboard shows, in one call — no query params needed.
type statsAPIResponse struct {
	Session stats.SessionStats   `json:"session"`
	Today   stats.DailyStats     `json:"today"`
	Week    stats.AggregateStats `json:"week"`
	Month   stats.AggregateStats `json:"month"`
	Year    stats.AggregateStats `json:"year"`
	AllTime stats.AllTimeStats   `json:"alltime"`
}

const (
	daysInWeek  = 7
	daysInMonth = 30
	daysInYear  = 365
)

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, statsAPIResponse{
		Session: s.stats.Session(),
		Today:   s.stats.Today(),
		Week:    s.stats.Period(daysInWeek),
		Month:   s.stats.Period(daysInMonth),
		Year:    s.stats.Period(daysInYear),
		AllTime: s.stats.AllTime(),
	})
}
