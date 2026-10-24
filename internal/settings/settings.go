// Package settings persists Hopper's runtime-editable configuration (see
// PATCH /api/v1/settings): the subset of config.Config a user can change
// from the Web UI without restarting hopperd. Values here start from
// config.Config at boot and take precedence over it from then on — the
// YAML file itself is never rewritten.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"go.etcd.io/bbolt"
)

var (
	bucketName = []byte("settings")
	recordKey  = []byte("current")
)

// Settings is the full set of runtime-editable values.
type Settings struct {
	// DownloadPath mirrors config.DownloadsConfig.Path, surfaced here so
	// it's visible/editable from the Web UI. Applied live via
	// Hooks.SetDownloadPath (downloader.Engine.SetBaseDir) — like
	// ParallelFiles and the other engine-level fields below, a change
	// takes effect immediately, no restart needed. Validated in Apply
	// (see validateDownloadPath) before it's applied at all: the path
	// must already exist and actually be writable, not just non-empty.
	DownloadPath     string `json:"download_path"`
	ParallelFiles    int    `json:"parallel_files"`
	ParallelChunks   int    `json:"parallel_chunks"`
	BandwidthLimit   int64  `json:"bandwidth_limit"` // bytes/s, 0 = unlimited
	RetryMax         int    `json:"retry_max"`
	AutoStartImports bool   `json:"auto_start_imports"`
	DefaultPriority  string `json:"default_priority"` // low|normal|high
	LogLevel         string `json:"log_level"`        // debug|info|warn|error

	// Extract* mirror config.ExtractConfig but, like everything else
	// here, are live-editable without a restart — read fresh by
	// queue.Manager on every item completion rather than pushed via a
	// Hooks callback, since extraction only happens at discrete
	// completion events, not continuously (nothing to "push into" in
	// between).
	ExtractEnabled     bool     `json:"extract_enabled"`
	ExtractDeleteAfter bool     `json:"extract_delete_after"`
	ExtractPasswords   []string `json:"extract_passwords"`

	// AccountPriority lists hoster plugin names (account hoster_key), most
	// preferred first: when accounts of several plugins can handle a link
	// — e.g. Real-Debrid and AllDebrid both support rapidgator.net — the
	// first one listed is used, the next one if it can't serve the link.
	// Plugins not listed follow in their default order. Read fresh by
	// queue.Manager on every match.
	AccountPriority []string `json:"account_priority"`

	// UpdateCheck lets hopperd look for a newer release on GitHub once a
	// day (see internal/update). Off unless the user turns it on: it is
	// the one outbound call that isn't caused by the user's own
	// downloads or configuration. A record persisted before this field
	// existed loads as false. Read fresh by the periodic job.
	UpdateCheck bool `json:"update_check"`
}

// Update is a partial change: nil fields are left as-is. Every provided
// field is validated before anything is applied — an Update either
// takes effect completely or not at all (see Manager.Apply).
type Update struct {
	DownloadPath     *string `json:"download_path,omitempty"`
	ParallelFiles    *int    `json:"parallel_files,omitempty"`
	ParallelChunks   *int    `json:"parallel_chunks,omitempty"`
	BandwidthLimit   *int64  `json:"bandwidth_limit,omitempty"`
	RetryMax         *int    `json:"retry_max,omitempty"`
	AutoStartImports *bool   `json:"auto_start_imports,omitempty"`
	DefaultPriority  *string `json:"default_priority,omitempty"`
	LogLevel         *string `json:"log_level,omitempty"`

	ExtractEnabled     *bool `json:"extract_enabled,omitempty"`
	ExtractDeleteAfter *bool `json:"extract_delete_after,omitempty"`
	// ExtractPasswords, when provided, replaces the whole list (there's
	// no "add one entry" partial-update semantics here) — nil means
	// "leave as-is", same as every other field; an explicit empty slice
	// clears it.
	ExtractPasswords []string `json:"extract_passwords,omitempty"`
	// AccountPriority, when provided, replaces the whole order; an
	// explicit empty list restores the default order.
	AccountPriority []string `json:"account_priority,omitempty"`

	UpdateCheck *bool `json:"update_check,omitempty"`
}

// Hooks lets the caller (cmd/hopperd/main.go, the only place that
// already holds references to every live runtime component) wire
// immediate effect for settings that need to reach a running component,
// not just be persisted. A nil hook just means that field is persisted
// but not separately pushed anywhere — still correct for fields every
// caller already reads fresh via Manager.Get() per-request (e.g.
// AutoStartImports, DefaultPriority), just not "instantaneous" in the
// sense the engine-level ones are.
type Hooks struct {
	SetDownloadPath   func(string)
	SetParallelFiles  func(int)
	SetParallelChunks func(int)
	SetBandwidthLimit func(int64)
	SetRetryMax       func(int)
	SetLogLevel       func(level string) error
}

// Manager persists Settings to bbolt and applies Hooks on every change.
type Manager struct {
	db    *bbolt.DB
	hooks Hooks

	// applyMu serializes Apply; mu guards cur for readers.
	applyMu sync.Mutex
	mu      sync.RWMutex
	cur     Settings
}

// New opens (creating if necessary) the settings bucket and loads any
// previously persisted record. If none exists yet (first run, or a
// fresh bbolt file), initial — derived from config.Config at startup —
// is persisted as the first one. Either way, every Hooks callback is
// invoked once with the resolved starting value, so live components
// start in sync with whatever Settings actually holds: on restart after
// a prior settings change, that's the persisted value, not whatever the
// YAML file happens to say.
func New(db *bbolt.DB, initial Settings, hooks Hooks) (*Manager, error) {
	err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("settings: init bucket: %w", err)
	}

	m := &Manager{db: db, hooks: hooks, cur: initial}

	var raw []byte
	err = db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(bucketName).Get(recordKey); v != nil {
			raw = append([]byte(nil), v...)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("settings: load: %w", err)
	}

	if raw != nil {
		var loaded Settings
		if err := json.Unmarshal(raw, &loaded); err != nil {
			return nil, fmt.Errorf("settings: unmarshal persisted record: %w", err)
		}
		m.cur = loaded
	} else if err := m.persist(m.cur); err != nil {
		return nil, err
	}

	// A nil ExtractPasswords (the config default, since hopper.yaml has
	// no reason to ship one) marshals to JSON `null`, not `[]` — the Web
	// UI reads settings.extract_passwords.length unconditionally, so a
	// null here is a hard crash on the Settings page, not just an empty
	// list. Normalize once here rather than relying on every caller
	// (including the API layer, this file's persist path, and legacy
	// bbolt records from before this field existed) to remember to.
	if m.cur.AccountPriority == nil {
		m.cur.AccountPriority = []string{}
	}
	if m.cur.ExtractPasswords == nil {
		m.cur.ExtractPasswords = []string{}
	}

	m.applyHooks(m.cur)
	return m, nil
}

// Get returns the current settings.
func (m *Manager) Get() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur
}

// Apply validates and merges u into the current settings, persists the
// result, and invokes Hooks so any live component reflects it
// immediately. Returns the full resulting Settings. An invalid field
// rejects the whole Update — nothing is applied or persisted — rather
// than silently applying the valid subset.
func (m *Manager) Apply(u Update) (Settings, error) {
	// One Apply at a time from validation through the hooks, and memory
	// changes only after the write succeeded: concurrent updates used to
	// interleave their persist and hook calls (database, memory and the
	// live components ending up with different values), and a failed
	// persist left memory changed although the call reported an error.
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.RLock()
	next := m.cur
	m.mu.RUnlock()

	if u.DownloadPath != nil {
		path := strings.TrimSpace(*u.DownloadPath)
		if path == "" {
			return Settings{}, fmt.Errorf("download_path must not be empty")
		}
		if err := validateDownloadPath(path); err != nil {
			return Settings{}, err
		}
		next.DownloadPath = path
	}
	if u.ParallelFiles != nil {
		if *u.ParallelFiles < 1 || *u.ParallelFiles > 10 {
			return Settings{}, fmt.Errorf("parallel_files must be between 1 and 10, got %d", *u.ParallelFiles)
		}
		next.ParallelFiles = *u.ParallelFiles
	}
	if u.ParallelChunks != nil {
		if *u.ParallelChunks < 1 || *u.ParallelChunks > 16 {
			return Settings{}, fmt.Errorf("parallel_chunks must be between 1 and 16, got %d", *u.ParallelChunks)
		}
		next.ParallelChunks = *u.ParallelChunks
	}
	if u.BandwidthLimit != nil {
		if *u.BandwidthLimit < 0 {
			return Settings{}, fmt.Errorf("bandwidth_limit must be >= 0, got %d", *u.BandwidthLimit)
		}
		next.BandwidthLimit = *u.BandwidthLimit
	}
	if u.RetryMax != nil {
		if *u.RetryMax < 0 || *u.RetryMax > 10 {
			return Settings{}, fmt.Errorf("retry_max must be between 0 and 10, got %d", *u.RetryMax)
		}
		next.RetryMax = *u.RetryMax
	}
	if u.AutoStartImports != nil {
		next.AutoStartImports = *u.AutoStartImports
	}
	if u.DefaultPriority != nil {
		switch *u.DefaultPriority {
		case "low", "normal", "high":
		default:
			return Settings{}, fmt.Errorf("default_priority must be one of low|normal|high, got %q", *u.DefaultPriority)
		}
		next.DefaultPriority = *u.DefaultPriority
	}
	if u.LogLevel != nil {
		switch *u.LogLevel {
		case "debug", "info", "warn", "error":
		default:
			return Settings{}, fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", *u.LogLevel)
		}
		next.LogLevel = *u.LogLevel
	}
	if u.ExtractEnabled != nil {
		next.ExtractEnabled = *u.ExtractEnabled
	}
	if u.ExtractDeleteAfter != nil {
		next.ExtractDeleteAfter = *u.ExtractDeleteAfter
	}
	if u.ExtractPasswords != nil {
		next.ExtractPasswords = u.ExtractPasswords
	}
	if u.AccountPriority != nil {
		order, err := normalizeAccountPriority(u.AccountPriority)
		if err != nil {
			return Settings{}, err
		}
		next.AccountPriority = order
	}
	if u.UpdateCheck != nil {
		next.UpdateCheck = *u.UpdateCheck
	}

	if err := m.persist(next); err != nil {
		return Settings{}, err
	}
	m.mu.Lock()
	m.cur = next
	m.mu.Unlock()
	m.applyHooks(next)
	return next, nil
}

// accountPriorityName matches a plugin name (hoster_key).
var accountPriorityName = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// normalizeAccountPriority trims and validates an account priority list:
// plugin-name-shaped entries, no duplicates, at most 50. Names that don't
// belong to a registered plugin are accepted (and ignored when matching),
// so a priority list survives a plugin being removed and re-added.
func normalizeAccountPriority(in []string) ([]string, error) {
	if len(in) > 50 {
		return nil, fmt.Errorf("account_priority has %d entries, at most 50 allowed", len(in))
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if !accountPriorityName.MatchString(name) {
			return nil, fmt.Errorf("account_priority: invalid plugin name %q", raw)
		}
		if seen[name] {
			return nil, fmt.Errorf("account_priority: %q listed twice", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

func (m *Manager) persist(s Settings) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("settings: marshal: %w", err)
	}
	return m.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).Put(recordKey, data)
	})
}

// applyHooks pushes every field of s into its wired live-effect
// component (see Hooks), unconditionally rather than only on the fields
// that changed — every hook is idempotent for an already-equal value,
// and this lets New's one-time startup sync and Apply's per-change path
// share the same code.
func (m *Manager) applyHooks(s Settings) {
	if m.hooks.SetDownloadPath != nil {
		m.hooks.SetDownloadPath(s.DownloadPath)
	}
	if m.hooks.SetParallelFiles != nil {
		m.hooks.SetParallelFiles(s.ParallelFiles)
	}
	if m.hooks.SetParallelChunks != nil {
		m.hooks.SetParallelChunks(s.ParallelChunks)
	}
	if m.hooks.SetBandwidthLimit != nil {
		m.hooks.SetBandwidthLimit(s.BandwidthLimit)
	}
	if m.hooks.SetRetryMax != nil {
		m.hooks.SetRetryMax(s.RetryMax)
	}
	if m.hooks.SetLogLevel != nil {
		_ = m.hooks.SetLogLevel(s.LogLevel) // already validated in Apply; can't fail here
	}
}

// validateDownloadPath checks that path is an absolute directory that
// already exists and is actually writable by hopperd — not just present.
// Existence and permissions can't be inferred from the string alone: a
// Docker bind mount can be missing entirely if the host directory
// doesn't exist yet, or present but read-only/wrong-owner, both of which
// silently broke every subsequent download under the old
// restart-required behavior (see Settings.DownloadPath) with no
// feedback until a download actually failed. Stat alone doesn't catch
// the read-only/permission case either, so this does a real
// create-and-remove probe file, the only reliable cross-platform way to
// confirm write access.
func validateDownloadPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("download_path must be an absolute path, got %q", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("download_path %q does not exist (in Docker: use /downloads or a subdirectory of it, and make sure it's bind-mounted)", path)
		}
		return fmt.Errorf("download_path %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("download_path %q is not a directory", path)
	}

	probe, err := os.CreateTemp(path, ".hopper-write-test-*")
	if err != nil {
		return fmt.Errorf("download_path %q is not writable (in Docker: check the volume mount's ownership/permissions): %w", path, err)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("download_path %q: failed to clean up write-test file %q: %w", path, name, err)
	}
	return nil
}
