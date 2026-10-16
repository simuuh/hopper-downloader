// Package queue owns download queue state: packages, items, priorities,
// and the worker pool that drives items through the downloader engine. It
// is the single writer of queue state; the REST API and WebSocket layers
// only read from it and call its methods.
package queue

import (
	"fmt"
	"time"
)

// State is the lifecycle state of a queue Item.
type State string

const (
	StateQueued      State = "queued"
	StateDownloading State = "downloading"
	StatePaused      State = "paused"
	StateDone        State = "done"
	StateError       State = "error"
)

// ExtractState reflects internal/extract's lifecycle for an item's
// archive. Set on every sibling item in an archive group at once (see
// Manager.setExtractState) since extraction operates on the whole group
// as a unit — a multi-part RAR's parts all share one outcome — not on a
// single file in isolation.
type ExtractState string

const (
	ExtractStateExtracting    ExtractState = "extracting"
	ExtractStateExtracted     ExtractState = "extracted"
	ExtractStateExtractFailed ExtractState = "extract_failed"
)

// Priority orders items within the queue; higher values run first. The
// zero value is PriorityNormal on purpose: every add path that doesn't
// set a priority (imports, Click'n'Load, container expansion) must get
// "normal", not silently "low". Only the names are persisted (JSON), so
// the numeric values are free to choose.
type Priority int

const (
	PriorityLow    Priority = -1
	PriorityNormal Priority = 0
	PriorityHigh   Priority = 1
)

// String implements fmt.Stringer.
func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	default:
		return "normal"
	}
}

// MarshalJSON encodes Priority as its lowercase name.
func (p Priority) MarshalJSON() ([]byte, error) {
	return []byte(`"` + p.String() + `"`), nil
}

// UnmarshalJSON decodes Priority from its lowercase name.
func (p *Priority) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	parsed, err := ParsePriority(s)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// ParsePriority parses "low", "normal", or "high" (case-insensitive).
func ParsePriority(s string) (Priority, error) {
	switch s {
	case "low":
		return PriorityLow, nil
	case "normal", "":
		return PriorityNormal, nil
	case "high":
		return PriorityHigh, nil
	default:
		return PriorityNormal, fmt.Errorf("queue: invalid priority %q", s)
	}
}

// Package groups related Items (e.g. a multi-part archive).
type Package struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Password string `json:"password,omitempty"`
	// Path is an optional "save to" override, relative to the engine's
	// downloads root, e.g. "movies/2026". Empty means "use Name" (the
	// existing default behavior) — see Manager.SetPackagePath and
	// PATCH /api/v1/packages/{id}.
	Path      string    `json:"path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Item is a single downloadable link and its queue/transfer state.
type Item struct {
	ID         string   `json:"id"`
	PackageID  string   `json:"package_id"`
	URL        string   `json:"url"`
	Filename   string   `json:"filename"`
	Password   string   `json:"password,omitempty"`
	HosterKey  string   `json:"hoster_key"`
	Priority   Priority `json:"priority"`
	State      State    `json:"state"`
	Position   int64    `json:"position"`
	Size       int64    `json:"size"`
	Downloaded int64    `json:"downloaded"`
	Speed      int64    `json:"speed"`
	Error      string   `json:"error,omitempty"`
	// ExtractState/ExtractError track internal/extract's post-download
	// archive-extraction outcome for this item — absent/empty means
	// "not an archive, or extraction is disabled/hasn't run". See
	// Manager.maybeExtract/runExtraction.
	ExtractState ExtractState `json:"extract_state,omitempty"`
	ExtractError string       `json:"extract_error,omitempty"`
	ScheduledAt  *time.Time   `json:"scheduled_at,omitempty"`
	// RetryAfter/CaptchaURL reflect a hoster's plugins.FreeDownloader
	// fallback asking the client to wait or solve a captcha before a free/
	// limited download can proceed (see queue.Manager.run's free-download
	// fallback and SolveCaptcha). Deliberately not a distinct Item.State
	// value — the item stays StateQueued the whole time (not runnable
	// until whichever condition clears, see runnable below), the same way
	// ExtractState layers extraction status on top of State without
	// becoming a State itself. At most one of the two is set at a time.
	RetryAfter *time.Time `json:"retry_after,omitempty"`
	CaptchaURL string     `json:"captcha_url,omitempty"`
	// Attempts counts automatic retries after transient failures (see
	// Manager.failTransient); reset on success and on a manual retry.
	Attempts  int       `json:"attempts,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// runnable reports whether the item is eligible to be picked up by a
// worker right now.
func (it *Item) runnable(now time.Time) bool {
	if it.State != StateQueued {
		return false
	}
	if it.ScheduledAt != nil && it.ScheduledAt.After(now) {
		return false
	}
	if it.RetryAfter != nil && it.RetryAfter.After(now) {
		return false
	}
	if it.CaptchaURL != "" {
		return false
	}
	return true
}

// EventType identifies the kind of change an Event carries.
type EventType string

const (
	EventItemUpdated    EventType = "item_updated"
	EventItemRemoved    EventType = "item_removed"
	EventPackageUpdated EventType = "package_updated"
	// EventPackageRemoved is published once a package's last item is gone
	// and the package itself has been deleted (Event.Package carries only
	// its ID).
	EventPackageRemoved EventType = "package_removed"
)

// Event is published to subscribers (typically the WebSocket hub) on
// every queue state change.
type Event struct {
	Type    EventType `json:"type"`
	Item    *Item     `json:"item,omitempty"`
	Package *Package  `json:"package,omitempty"`
}
