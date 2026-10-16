// Package plugins defines the Hoster contract implemented by each
// file-hosting service integration, plus the small set of shared types
// plugins exchange with the downloader.
//
// Hopper does not use Go's native plugin package (`plugin.Open`): it is
// Linux-only, ties plugin and host binaries to an exact toolchain version,
// and cannot produce the Windows/macOS artifacts Hopper ships. Instead,
// plugins are ordinary Go packages compiled into hopperd and registered
// explicitly at startup via Registry.Register. See docs/PLUGIN_GUIDE.md.
package plugins

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// Sentinel errors a Hoster implementation should return (via errors.Is-
// compatible wrapping) so the downloader and API can react appropriately
// without parsing error strings.
var (
	// ErrNotFound indicates the remote file no longer exists.
	ErrNotFound = errors.New("plugins: file not found on hoster")
	// ErrUnavailable indicates the file exists but cannot be downloaded
	// right now (server maintenance, region block, etc).
	ErrUnavailable = errors.New("plugins: file temporarily unavailable")
	// ErrAccountRequired indicates the operation needs an authenticated
	// account but none (or an invalid one) was supplied.
	ErrAccountRequired = errors.New("plugins: hoster account required")
	// ErrRateLimited indicates the hoster asked the client to back off;
	// callers should retry after the hoster's advertised wait time.
	ErrRateLimited = errors.New("plugins: rate limited by hoster")
	// ErrUnsupportedHost indicates that this plugin can't serve this URL
	// right now although its account is fine — typically a multi-hoster
	// (debrid) service that doesn't support the host, has it down for
	// maintenance, or whose quota for that one host is used up. The queue
	// then tries the next matching plugin in the account priority order,
	// without putting the account into cooldown (that's what
	// ErrAccountRequired does).
	ErrUnsupportedHost = errors.New("plugins: host not served by this plugin right now")
)

// ErrRetryAfter is returned by FreeDownloader.FetchFreeDownloadURL when
// the hoster's free-download flow requires the client to wait before the
// file becomes downloadable (a "click here after N seconds" style
// cooldown, typical of anonymous/free-tier hoster downloads). Seconds is
// the hoster's own advertised wait; queue.Manager schedules a retry after
// it rather than treating the item as failed. A value type (not *Error)
// so a plugin can return it directly as ErrRetryAfter{Seconds: n} and
// callers compare/extract it via errors.As.
type ErrRetryAfter struct{ Seconds int }

func (e ErrRetryAfter) Error() string {
	return fmt.Sprintf("plugins: retry after %ds", e.Seconds)
}

// ErrCaptchaRequired is returned by FreeDownloader.FetchFreeDownloadURL
// when the hoster's free-download flow requires solving a captcha before
// the file becomes downloadable. ImageURL points at the challenge image
// to show the user; queue.Manager pauses the item pending a solution
// (see its SolveCaptcha) rather than treating it as failed.
type ErrCaptchaRequired struct{ ImageURL string }

func (e ErrCaptchaRequired) Error() string {
	return "plugins: captcha required: " + e.ImageURL
}

// FreeDownloader is an optional interface a Hoster plugin implements when
// it supports a fallback, unauthenticated-or-limited download path
// distinct from its normal (typically premium-account) Resolve/
// FetchDownloadURL flow — e.g. a free-tier account that can't use a
// premium-only download endpoint but can still download slowly, subject
// to a wait or a captcha. Checked via type assertion (h.(FreeDownloader))
// by queue.Manager's run(), only after Resolve has already succeeded and
// FetchDownloadURL has failed with ErrAccountRequired with no other
// plugin left to fall back to (see matchHosterExcluding) — this is a
// last-resort attempt on the *same* plugin/account, not a replacement for
// the normal premium flow.
type FreeDownloader interface {
	// FetchFreeDownloadURL attempts a free/limited download of url. acc
	// is the same account (if any) queue.Manager already resolved for
	// this plugin — it may be nil for a plugin that also supports
	// fully-anonymous access. A caller retrying after a captcha was
	// solved (see queue.Manager.SolveCaptcha) passes the solution back in
	// acc.Extra["captcha_solution"]; a plugin that can act on it should
	// look there, but isn't required to support captcha-solving at all.
	//
	// May return ErrRetryAfter or ErrCaptchaRequired in addition to the
	// usual plugins sentinel errors; any other error is treated as a
	// terminal failure of the free-download attempt.
	FetchFreeDownloadURL(ctx context.Context, url string, acc *Account) (string, error)
}

// FileInfo describes a remote file as reported by a Hoster, prior to
// download.
type FileInfo struct {
	Name      string
	Size      int64
	Available bool
}

// Account carries the credentials a Hoster needs to act on behalf of a
// user. It intentionally holds only plaintext runtime values — persistence
// and at-rest encryption are internal/account's responsibility; that
// package decrypts a stored record into an Account immediately before a
// plugin call and lets it go out of scope afterward.
type Account struct {
	// ID is the stored account's identifier (set by internal/account, empty
	// for ad-hoc credentials such as an inline validation). Plugins don't
	// need it; queue.Manager uses it to report a failing account so the
	// next attempt rotates to another one.
	ID       string
	Username string
	Password string
	// Extra carries hoster-specific fields (API tokens, session cookies)
	// that don't fit Username/Password, keyed by plugin-defined names.
	Extra map[string]string
}

// Hoster is implemented by each file-hosting service plugin.
type Hoster interface {
	// Match reports whether this plugin handles the given URL.
	Match(url string) bool
	// Resolve looks up metadata for url without downloading it. acc may
	// be nil for hosters that support anonymous resolution.
	Resolve(url string, acc *Account) (*FileInfo, error)
	// FetchDownloadURL exchanges url for a direct, time-limited download
	// URL the downloader can issue Range requests against. acc may be nil
	// for hosters that support anonymous downloads.
	FetchDownloadURL(url string, acc *Account) (string, error)
	// Name returns the plugin's human-readable identifier, e.g. "rapidgator".
	Name() string
	// Domains returns the hostnames this plugin is responsible for.
	Domains() []string
}

// Credentialed is an optional interface a Hoster plugin implements if it
// needs a configured account to function at all (as opposed to e.g.
// directhttp, which works anonymously). The REST API and Web UI use it
// to decide which plugins to offer in the "add account" flow — see
// api's GET /api/v1/plugins. Checked via type assertion
// (h.(Credentialed)); a plugin that doesn't implement it is treated the
// same as one that does but returns false.
type Credentialed interface {
	Credentialed() bool
}

// DisplayNamer is an optional interface a Hoster plugin implements to
// give the Web UI a nicer label than its Name() key (e.g. "Real-Debrid"
// vs "realdebrid"). Name() remains the canonical identifier everywhere
// else (accounts, hopper.yaml per-hoster config) — this is presentation
// only. Falls back to Name() when not implemented.
type DisplayNamer interface {
	DisplayName() string
}

// AccountInfo describes the result of checking a Hoster account against
// the hoster's own API, returned by an optional AccountValidator
// implementation. ExpiresAt and TrafficLeft are nil when the hoster
// doesn't report them (or reports no expiry, e.g. a lifetime account).
type AccountInfo struct {
	Valid       bool       `json:"valid"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	TrafficLeft *int64     `json:"traffic_left,omitempty"` // bytes remaining, if the hoster reports it
	Label       string     `json:"label,omitempty"`        // e.g. "Premium until 2026-12-31"
}

// AccountValidator is an optional interface a Hoster plugin implements to
// check a configured account against the hoster's own API (e.g.
// Real-Debrid's GET /user) instead of trusting stored credentials
// blindly. The API calls it inline on POST /api/v1/accounts and
// on-demand via POST /api/v1/accounts/{id}/validate; a plugin that
// doesn't implement it (rapidgator, still a stub) reports unknown
// validity rather than a false positive or negative.
type AccountValidator interface {
	ValidateAccount(acc *Account) (*AccountInfo, error)
}

// ExpandedLink is one real, downloadable link an Expander plugin
// discovers behind a single "container" URL — e.g. one part of a
// multi-part archive a link-redirector wraps behind one page.
type ExpandedLink struct {
	URL  string
	Name string // destination filename, e.g. "file.part1.rar"
	Size int64  // bytes; 0 if unknown
	// Password is the archive password the container reports, if any —
	// the same value for every link belonging to one container.
	Password string
}

// TokenFromAccount extracts a hoster's API token from acc: acc.Extra
// [extraKey] takes priority (the documented convention for any caller
// that constructs a plugins.Account directly, e.g. tests or another API
// client), falling back to acc.Password because the Web UI's "Add
// Account" form only collects username/password — it has no field for
// Extra at all — so an account added that way stores the token as
// Password instead. Without this fallback, a plugin's own currentToken
// would return "" for any such account, and its Match would silently
// return false before ever reaching the hoster's API: no error, no log
// line, nothing to debug from (this is exactly the bug that was found
// and fixed for Real-Debrid, then generalized here for 1fichier too, as
// both plugins had independently identical copies of this logic
// differing only in which Extra key each checks).
//
// rapidgator does not use this: its account is a username+password pair
// exchanged for a session token via a login call, not a token stored
// directly on the account, so its credential-resolution shape is
// genuinely different, not a third copy of this same logic.
func TokenFromAccount(acc *Account, extraKey string) string {
	if acc == nil {
		return ""
	}
	if t := acc.Extra[extraKey]; t != "" {
		return t
	}
	return acc.Password
}

// FilenameFromURL extracts a display filename from a raw hoster URL's
// last path segment: percent-decoded, and with a trailing ".html"
// stripped — many hosters serve a file's info/download page at
// .../<id>/<filename>.html (e.g. rapidgator.net), so the real filename
// and its extension are still underneath that suffix. Returns "" if
// rawURL doesn't parse or has no usable path segment, leaving the
// caller to pick its own fallback.
//
// Shared by directhttp (its own URL-derived fallback when a hoster
// sends no Content-Disposition header) and hidecx (its own fallback
// when a hide.cx container link's own "name" field comes back empty) —
// same logic either plugin would otherwise have needed independently,
// same reasoning TokenFromAccount above was extracted for.
// internal/clicknload has its own private, pre-existing copy of this
// same idea (for deriving a package name from Click'n'Load URLs, not a
// plugins.Hoster concern) — left as-is rather than folded in here.
func FilenameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" {
		return ""
	}
	if unescaped, err := url.QueryUnescape(base); err == nil {
		base = unescaped
	}
	return strings.TrimSuffix(base, ".html")
}

// Expander is an optional interface a Hoster plugin implements when a
// single URL it matches can actually represent *multiple* downloadable
// files — e.g. hide.cx wrapping every part of a multi-part archive
// behind one container page (see plugins/hidecx). Checked via type
// assertion (h.(Expander)) by queue.Manager before an item is added,
// same pattern as Credentialed/DisplayNamer/AccountValidator. A plugin
// that doesn't implement it is always treated as single-file.
type Expander interface {
	Expand(url string) ([]ExpandedLink, error)
}

// Registry holds the set of Hoster plugins available to the downloader.
// It carries no package-level state; callers construct one explicitly and
// register plugins into it, so the set of active plugins is always an
// explicit, testable value rather than implicit global state.
type Registry struct {
	hosters []Hoster
}

// NewRegistry builds a Registry from an initial set of plugins.
func NewRegistry(hosters ...Hoster) *Registry {
	r := &Registry{}
	for _, h := range hosters {
		r.Register(h)
	}
	return r
}

// Register adds a plugin to the registry. Later registrations take
// priority over earlier ones when matching a URL, so callers should
// register catch-all fallback plugins (e.g. directhttp) first.
func (r *Registry) Register(h Hoster) {
	r.hosters = append(r.hosters, h)
}

// Match returns the highest-priority plugin (by registration order alone)
// willing to handle url. It has no notion of accounts — queue.Manager's
// two-pass resolution (preferring a plugin with a configured account
// over one without, regardless of registration order) is built on top of
// All(), not this method; Match is registration-order tiebreaking within
// each of those two passes.
func (r *Registry) Match(url string) (Hoster, bool) {
	for i := len(r.hosters) - 1; i >= 0; i-- {
		if r.hosters[i].Match(url) {
			return r.hosters[i], true
		}
	}
	return nil, false
}

// All returns every registered plugin, in registration order.
func (r *Registry) All() []Hoster {
	out := make([]Hoster, len(r.hosters))
	copy(out, r.hosters)
	return out
}
