// Package stats tracks Hopper's download activity for the Web UI's
// statistics dashboard. Individual download events are never stored —
// only pre-aggregated daily buckets (bbolt) and an all-time running
// total (bbolt), plus in-memory session totals that intentionally reset
// on every restart. See Tracker.
package stats

import "time"

// DailyStats aggregates one calendar day's activity (server-local time),
// keyed by Date ("2006-01-02") in the "stats_daily" bbolt bucket.
type DailyStats struct {
	Date       string           `json:"date"`
	BytesTotal int64            `json:"bytes_total"`
	FilesTotal int64            `json:"files_total"`
	Errors     int64            `json:"errors"`
	ByHoster   map[string]int64 `json:"by_hoster"`
}

// AllTimeStats is the single running total since Hopper was first set up,
// stored under the "stats_alltime" bbolt bucket's "v1" key.
type AllTimeStats struct {
	BytesTotal  int64            `json:"bytes_total"`
	FilesTotal  int64            `json:"files_total"`
	Errors      int64            `json:"errors"`
	PeakSpeed   int64            `json:"peak_speed"`
	LargestFile int64            `json:"largest_file"`
	ByHoster    map[string]int64 `json:"by_hoster"`
}

// SessionStats covers activity since this hopperd process started.
// In-memory only — losing it on restart is correct behavior, not a bug:
// it answers "what has this run of hopperd done", not a persisted metric.
type SessionStats struct {
	StartedAt  time.Time        `json:"started_at"`
	BytesTotal int64            `json:"bytes_total"`
	FilesTotal int64            `json:"files_total"`
	Errors     int64            `json:"errors"`
	PeakSpeed  int64            `json:"peak_speed"`
	ByHoster   map[string]int64 `json:"by_hoster"`
}

// AggregateStats sums Days worth of DailyStats ending today (inclusive),
// e.g. the last 7/30/365 days for the API's week/month/year fields.
// Daily carries each individual day in the range, oldest first — a day
// with no recorded activity still appears with its Date set and every
// other field zero, so a caller charting the range (the Web UI's daily
// bar chart) doesn't need to fill gaps itself.
type AggregateStats struct {
	Days       int              `json:"days"`
	BytesTotal int64            `json:"bytes_total"`
	FilesTotal int64            `json:"files_total"`
	Errors     int64            `json:"errors"`
	ByHoster   map[string]int64 `json:"by_hoster"`
	Daily      []DailyStats     `json:"daily"`
}

// dateLayout is the bbolt key format for a DailyStats bucket entry — also
// DailyStats.Date's own format, so a day's own record round-trips through
// dateFor(date's own Date).
const dateLayout = "2006-01-02"
