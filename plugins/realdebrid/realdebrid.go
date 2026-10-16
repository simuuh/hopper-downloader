// Package realdebrid implements a Hoster plugin backed by the Real-Debrid
// "unrestrict" API (https://api.real-debrid.com/): rather than talking to
// each supported hoster directly, it hands the original hoster URL to
// Real-Debrid and gets back a direct, hoster-account-free download URL.
//
// Deviation from a literal reading of the original spec, verified
// against Real-Debrid's own API docs, its production Go client
// (rclone's realdebrid backend), and a structured third-party doc
// mirror: /unrestrict/check and /unrestrict/link are POST endpoints with
// form-encoded bodies, not GET with a query string. All three sources
// agree; implementing GET would not work against the real API.
package realdebrid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/simuuh/hopper-downloader/plugins"
)

const (
	pluginName = "realdebrid"
	apiBase    = "https://api.real-debrid.com/rest/1.0"

	// Real-Debrid documents a 250 requests/minute cap; stay under it to
	// leave headroom for clock-boundary jitter and any other process
	// sharing the same token.
	requestsPerMinute = 240
	rateLimiterBurst  = 10

	hostsCacheTTL     = time.Hour
	requestTimeout    = 30 * time.Second
	hostsFetchTimeout = 15 * time.Second
)

// TokenSource resolves the current Real-Debrid API token from the
// configured hoster account. *account.Manager satisfies this implicitly
// (it already implements the identical method for queue.AccountLookup),
// so no import of internal/account is needed here.
//
// This plugin needs it because plugins.Hoster.Match takes no *Account
// parameter — unlike Resolve/FetchDownloadURL, which receive one from
// the queue manager on every call — so it has no other way to learn
// whether an account is configured before deciding whether it can claim
// a URL.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

type hostStatus struct {
	Supported bool
	Status    string
}

// Plugin is the Real-Debrid Hoster implementation.
type Plugin struct {
	tokens  TokenSource
	client  *http.Client
	limiter *rate.Limiter
	log     *slog.Logger

	// baseURL and retryBaseDelay are overridden by tests; production
	// code always uses apiBase and the multi-second real backoff.
	baseURL        string
	retryBaseDelay time.Duration

	mu             sync.Mutex
	hosts          map[string]hostStatus
	hostsFetchedAt time.Time
}

// New returns a Real-Debrid Plugin. tokens is typically the same
// *account.Manager passed to the queue manager. log defaults to a
// discard logger if nil.
func New(tokens TokenSource, log *slog.Logger) *Plugin {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Plugin{
		tokens:         tokens,
		client:         &http.Client{Timeout: requestTimeout},
		limiter:        rate.NewLimiter(rate.Limit(float64(requestsPerMinute)/60.0), rateLimiterBurst),
		log:            log,
		baseURL:        apiBase,
		retryBaseDelay: 2 * time.Second,
	}
	return p
}

// Name implements plugins.Hoster. It is also the hoster key used for the
// Real-Debrid account in hopper.yaml's per-hoster limits and in
// internal/account.
func (p *Plugin) Name() string { return pluginName }

// Domains implements plugins.Hoster, returning whatever the most recent
// /hosts/status fetch reported as supported. It's best-effort and may be
// empty (before the first successful fetch) or briefly stale; Match, not
// Domains, is what actually decides routing.
func (p *Plugin) Domains() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.hosts))
	for d := range p.hosts {
		out = append(out, d)
	}
	return out
}

// Credentialed implements plugins.Credentialed: Real-Debrid is unusable
// without an api_token.
func (p *Plugin) Credentialed() bool { return true }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "Real-Debrid" }

// Match implements plugins.Hoster. It claims a URL only if a Real-Debrid
// account is configured (an api_token is set) and that URL's host is a
// currently-supported Real-Debrid hoster per /hosts/status. Registering
// this plugin after the native hoster plugins (see cmd/hopperd) gives it
// priority over them, but only for hosts it actually supports — an
// unconfigured or non-matching URL falls through to the native plugin
// (or directhttp) as if this plugin weren't registered at all.
func (p *Plugin) Match(rawURL string) bool {
	token := p.currentToken()
	if token == "" {
		return false
	}
	host := hostnameOf(rawURL)
	if host == "" {
		return false
	}

	hosts, err := p.hostsSnapshot(token)
	if err != nil {
		if hosts == nil {
			p.log.Warn("realdebrid: host cache empty, Match() will return false for all URLs", "error", err)
			return false
		}
		p.log.Warn("realdebrid: hosts/status refresh failed, using stale cache", "error", err)
		// Fall through and match against the stale cache rather than
		// refusing every link until the next successful refresh.
	}

	return hostSupported(hosts, host)
}

// hostSupported reports whether host is a Real-Debrid-supported hoster
// per hosts (a /hosts/status snapshot). rg.to (Rapidgator's own URL
// shortener — rg.to/file/<hash> redirects to rapidgator.net/file/...) is
// handled explicitly here, not by mirroring a synthetic entry into the
// cached hosts map itself (see docs/ARCHITECTURE.md's rg.to entry for why that used
// to happen in fetchHostsStatus): Real-Debrid's API accepts rg.to links
// directly in /unrestrict/link under the same account, but /hosts/status
// never lists "rg.to" as its own entry, only "rapidgator.net". Checked
// ahead of the general domain/suffix loop so a real, explicit "rg.to"
// entry — never observed so far, but not documented as impossible either
// — still wins over the rapidgator.net fallback if the API ever starts
// sending one.
func hostSupported(hosts map[string]hostStatus, host string) bool {
	if host == "rg.to" || strings.HasSuffix(host, ".rg.to") {
		if st, ok := hosts["rg.to"]; ok {
			return st.Supported
		}
		if st, ok := hosts["rapidgator.net"]; ok {
			return st.Supported
		}
		return false
	}
	for domain, st := range hosts {
		if !st.Supported {
			continue
		}
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// Resolve implements plugins.Hoster via POST /unrestrict/check.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, http.MethodPost, "/unrestrict/check", token, url.Values{"link": {rawURL}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := statusErr(resp.StatusCode); err != nil {
		return nil, err
	}

	var body struct {
		Filename  string `json:"filename"`
		Filesize  int64  `json:"filesize"`
		Supported int    `json:"supported"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: decode unrestrict/check response: %w", pluginName, err)
	}

	return &plugins.FileInfo{
		Name:      body.Filename,
		Size:      body.Filesize,
		Available: body.Supported == 1,
	}, nil
}

// FetchDownloadURL implements plugins.Hoster via POST /unrestrict/link.
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return "", plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, http.MethodPost, "/unrestrict/link", token, url.Values{"link": {rawURL}})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := statusErr(resp.StatusCode); err != nil {
		return "", err
	}

	var body struct {
		Download string `json:"download"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("%s: decode unrestrict/link response: %w", pluginName, err)
	}
	if body.Download == "" {
		return "", fmt.Errorf("%s: %w: empty download URL in response", pluginName, plugins.ErrUnavailable)
	}
	return body.Download, nil
}

// rdErrorCodeTrafficExhausted is Real-Debrid's /unrestrict/link
// error_code for "this account's free-tier daily traffic quota is used
// up" — not verified against a live account (see the package doc
// comment's confirmed/unconfirmed
// split for /unrestrict/check and /unrestrict/link's success shape).
const rdErrorCodeTrafficExhausted = 9

// unrestrictLinkResponse extends the plain {"download": "..."} shape
// FetchDownloadURL decodes with the error fields Real-Debrid's API is
// documented to send on failure ({"error": "...", "error_code": N}) —
// FetchDownloadURL never needed them since statusErr's HTTP-status-only
// mapping was enough for the premium path; FetchFreeDownloadURL needs to
// tell rdErrorCodeTrafficExhausted apart from every other failure.
type unrestrictLinkResponse struct {
	Download  string `json:"download"`
	Error     string `json:"error"`
	ErrorCode int    `json:"error_code"`
}

// FetchFreeDownloadURL implements plugins.FreeDownloader. A Real-Debrid
// free-tier account can still unrestrict some links (limited by daily
// traffic) via the exact same /unrestrict/link endpoint the premium
// FetchDownloadURL already calls — this method exists to interpret its
// error_code distinctly, not to hit a different endpoint. error_code
// rdErrorCodeTrafficExhausted is treated as a hard stop
// (plugins.ErrAccountRequired, not ErrRetryAfter/ErrCaptchaRequired):
// there's no known wait or captcha that resolves it — the account simply
// has no free quota left until Real-Debrid's own reset — so
// queue.Manager's tryFreeFallback correctly reports this as a plain
// failure rather than scheduling a retry or a captcha prompt that would
// never actually help.
func (p *Plugin) FetchFreeDownloadURL(ctx context.Context, rawURL string, acc *plugins.Account) (string, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return "", plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, http.MethodPost, "/unrestrict/link", token, url.Values{"link": {rawURL}})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body unrestrictLinkResponse
	if decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); decErr != nil {
		if serr := statusErr(resp.StatusCode); serr != nil {
			return "", serr
		}
		return "", fmt.Errorf("%s: decode free unrestrict/link response: %w", pluginName, decErr)
	}

	if body.ErrorCode == rdErrorCodeTrafficExhausted {
		return "", plugins.ErrAccountRequired
	}
	if body.Download != "" {
		return body.Download, nil
	}
	if serr := statusErr(resp.StatusCode); serr != nil {
		return "", serr
	}
	if body.Error != "" {
		return "", fmt.Errorf("%s: %s (error_code %d)", pluginName, body.Error, body.ErrorCode)
	}
	return "", fmt.Errorf("%s: %w: empty download URL in free-download response", pluginName, plugins.ErrUnavailable)
}

// ValidateAccount implements plugins.AccountValidator via GET /user. On a
// confirmed-valid token it also eagerly warms the /hosts/status cache
// (WarmHostsCache), so Match can start routing URLs to Real-Debrid on its
// very next call instead of waiting for its own on-demand refresh —
// queue.Manager's two-pass resolution only prefers this plugin once
// Match actually returns true for it.
//
// On a confirmed-*invalid* token (401/403), it does the mirror-image
// thing: ClearHostsCache. Without this, a token that goes bad after
// having already been warmed once (the common case — an account that
// worked fine and only later expired or got revoked) leaves Match()
// matching against the last-known-good /hosts/status snapshot for up to
// hostsCacheTTL, since Match's own stale-cache fallback exists precisely
// to tolerate a *transient* refresh failure and has no way to distinguish
// that from a permanent one. Clearing it here — the one place a "this
// token is definitely dead, not just a network blip" answer actually
// exists — is what lets queue.Manager's plugin fallback
// (matchHosterExcluding, triggered by Resolve/FetchDownloadURL's
// unaffected plugins.ErrAccountRequired) hand a queued item to a
// different plugin immediately instead of retrying this one for up to an
// hour.
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, http.MethodGet, "/user", token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		p.ClearHostsCache()
		return &plugins.AccountInfo{Valid: false, Label: "Invalid API token"}, nil
	}
	if err := statusErr(resp.StatusCode); err != nil {
		return nil, err
	}

	var body struct {
		Type       string `json:"type"`
		Expiration string `json:"expiration"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: decode user response: %w", pluginName, err)
	}

	// A free account (type == "free") reports no expiration field at all
	// per Real-Debrid's /user response — but even if it did send one (a
	// stray/placeholder value, not confirmed impossible), a free account
	// genuinely has no expiry to report, so this branch ignores
	// Expiration unconditionally for it rather than only when it happens
	// to be empty. This is the fix for "Valid until" showing a bogus
	// date for free accounts: the general parse-and-report path below is
	// only ever reached for a non-free type with a non-empty Expiration.
	info := &plugins.AccountInfo{Valid: true}
	switch {
	case strings.EqualFold(body.Type, "free") || body.Expiration == "":
		info.Label = "Free account (no expiry)"
	default:
		info.Label = capitalize(body.Type)
		if t, err := time.Parse(time.RFC3339, body.Expiration); err == nil {
			info.ExpiresAt = &t
			info.Label += " until " + t.Format("2006-01-02")
		}
	}

	p.WarmHostsCache(token)

	return info, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// statusErr maps a Real-Debrid HTTP status code to a plugins sentinel
// error where one applies, per the documented error codes for
// /unrestrict/check and /unrestrict/link (401/403 bad or locked
// account, 503 file unavailable) plus the general 429 rate-limit code.
func statusErr(code int) error {
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusServiceUnavailable:
		return plugins.ErrUnavailable
	case http.StatusUnauthorized, http.StatusForbidden:
		return plugins.ErrAccountRequired
	case http.StatusTooManyRequests:
		return plugins.ErrRateLimited
	default:
		return fmt.Errorf("%s: unexpected HTTP status %d", pluginName, code)
	}
}

// tokenFrom prefers the account the queue layer already resolved and
// passed in; it falls back to resolving one directly only if acc is nil,
// for robustness (Resolve/FetchDownloadURL are documented as accepting a
// nil acc for hosters that support anonymous access, which Real-Debrid
// does not, so this fallback keeps working even if a caller passes nil).
func (p *Plugin) tokenFrom(acc *plugins.Account) string {
	if acc != nil {
		if t := plugins.TokenFromAccount(acc, "api_token"); t != "" {
			return t
		}
	}
	return p.currentToken()
}

func (p *Plugin) currentToken() string {
	if p.tokens == nil {
		return ""
	}
	acc, ok := p.tokens.AccountFor(pluginName)
	if !ok || acc == nil {
		return ""
	}
	return plugins.TokenFromAccount(acc, "api_token")
}

func hostnameOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// premiumURL is the hoster's own page for buying/extending premium. The
// developer referral link lives in plugins/referral.go.
const premiumURL = "https://real-debrid.com/premium"

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }

// CredentialFields implements plugins.CredentialDescriber.
func (p *Plugin) CredentialFields() []plugins.CredentialField {
	return plugins.TokenCredentialFields("API token", "real-debrid.com/apitoken")
}
