// Package update checks whether a newer Hopper release exists. It is the
// only part of Hopper that contacts the project itself, so it runs only
// when the user turned it on (settings.UpdateCheck) or asked for a check
// from the Web UI — never by default. A check is a single anonymous GET
// of the latest GitHub release; nothing about the installation is sent
// beyond what any HTTP request carries (IP address, a generic
// User-Agent).
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is GitHub's "latest release" endpoint for Hopper. It never
// returns drafts or pre-releases.
const DefaultURL = "https://api.github.com/repos/simuuh/hopper-downloader/releases/latest"

// releasePage is where a release's notes and downloads live. The link is
// built from the validated tag rather than taken from the response, so
// the Web UI never links somewhere the response chose.
const releasePage = "https://github.com/simuuh/hopper-downloader/releases/tag/"

// minCheckInterval keeps repeated "Check now" clicks from each reaching
// GitHub, whose unauthenticated API allows 60 requests per hour and IP.
const minCheckInterval = time.Minute

// maxResponseBytes caps the release JSON read; a release with long notes
// is a few KiB.
const maxResponseBytes = 1 << 20

// releaseTag matches the tags Hopper releases are published under.
var releaseTag = regexp.MustCompile(`^v?\d{1,6}\.\d{1,6}\.\d{1,6}$`)

// Status is the result of the most recent check.
type Status struct {
	// Current is the running build's version ("dev" for local builds).
	Current string `json:"current"`
	// Latest is the newest release's tag, empty until a check succeeded.
	Latest string `json:"latest,omitempty"`
	// Available is true when Latest is newer than Current. Always false
	// for a build whose version can't be compared (e.g. "dev").
	Available bool `json:"available"`
	// ReleaseURL links to Latest's release notes.
	ReleaseURL string `json:"release_url,omitempty"`
	// CheckedAt is when a check last succeeded; nil if none has yet.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	// Error describes why the most recent check failed; empty after a
	// successful one. Latest/Available keep the last successful result.
	Error string `json:"error,omitempty"`
}

// Checker fetches and caches the latest release. Safe for concurrent use.
type Checker struct {
	current string
	url     string
	client  *http.Client
	log     *slog.Logger

	// checking serializes requests, so concurrent callers share the
	// throttle instead of each reaching GitHub.
	checking    sync.Mutex
	mu          sync.Mutex
	st          Status
	lastAttempt time.Time
}

// New builds a Checker for the running version current. url is the
// release endpoint (DefaultURL outside tests); client may be nil (a
// 15s-timeout default is used).
func New(current, url string, client *http.Client, log *slog.Logger) *Checker {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Checker{current: current, url: url, client: client, log: log, st: Status{Current: current}}
}

// Status returns the cached result of the last check without contacting
// anything.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

// Check fetches the latest release and returns the updated Status. Within
// minCheckInterval of the previous attempt it returns the cached Status
// instead. A failed check is recorded in Status.Error and also returned.
func (c *Checker) Check(ctx context.Context) (Status, error) {
	c.checking.Lock()
	defer c.checking.Unlock()

	c.mu.Lock()
	if time.Since(c.lastAttempt) < minCheckInterval {
		st := c.st
		c.mu.Unlock()
		return st, nil
	}
	c.lastAttempt = time.Now()
	c.mu.Unlock()

	tag, err := c.fetchLatest(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.st.Error = err.Error()
		c.log.Warn("update check failed", "error", err)
		return c.st, err
	}
	now := time.Now()
	c.st = Status{
		Current:    c.current,
		Latest:     tag,
		Available:  newer(tag, c.current),
		ReleaseURL: releasePage + tag,
		CheckedAt:  &now,
	}
	if c.st.Available {
		c.log.Info("a newer Hopper release is available", "current", c.current, "latest", tag)
	}
	return c.st, nil
}

// CheckIfDue runs Check when no check has succeeded within maxAge. It is
// meant for a periodic job; failures are only logged (and kept in
// Status), the next tick tries again.
func (c *Checker) CheckIfDue(ctx context.Context, maxAge time.Duration) {
	st := c.Status()
	if st.CheckedAt != nil && time.Since(*st.CheckedAt) < maxAge {
		return
	}
	_, _ = c.Check(ctx)
}

func (c *Checker) fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return "", fmt.Errorf("update: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// GitHub rejects requests without a User-Agent; the version is left
	// out on purpose.
	req.Header.Set("User-Agent", "Hopper")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: request latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update: latest release: HTTP %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&rel); err != nil {
		return "", fmt.Errorf("update: decode latest release: %w", err)
	}
	if !releaseTag.MatchString(rel.TagName) {
		return "", fmt.Errorf("update: unexpected release tag %q", rel.TagName)
	}
	return rel.TagName, nil
}

// newer reports whether release tag latest is a later version than
// current. A current version with a pre-release suffix (v1.2.0-rc1) is
// older than the same version without one. Anything that isn't a
// version — "dev", a commit hash — compares as not older, so a local
// build never claims an update.
func newer(latest, current string) bool {
	l, _, ok := parseVersion(latest)
	if !ok {
		return false
	}
	c, pre, ok := parseVersion(current)
	if !ok {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return pre
}

// parseVersion splits "v1.2.3[-suffix][+build]" into its numbers and
// whether it carries a pre-release suffix.
func parseVersion(s string) (nums [3]int, pre bool, ok bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], true
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nums, false, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nums, false, false
		}
		nums[i] = n
	}
	return nums, pre, true
}
