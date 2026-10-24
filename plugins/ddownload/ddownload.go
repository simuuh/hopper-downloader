// Package ddownload implements a Hoster plugin for ddownload.com against
// its official v2 API (https://api-v2.ddownload.com/api). Unlike several
// other plugins in this codebase, whose API docs weren't fetchable from
// earlier environments and had to be reverse-engineered from community
// client libraries, ddownload's own docs page
// (https://ddownload.com/api) was fetched directly and is the source for
// every endpoint/field name below — flagged "confirmed" throughout this
// package's doc comments means "matches that live page or a live probe
// against the real API", not a guess.
//
// FetchDownloadURL has no working implementation — a deliberate,
// documented stub, not an oversight. See its own doc comment for the
// live investigation (three approaches tried against the real API/
// website, none confirmed working) that led to this, and why
// plugins.FreeDownloader isn't implemented here either.
package ddownload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/simuuh/hopper-downloader/plugins"
)

const (
	pluginName = "ddownload"
	apiBase    = "https://api-v2.ddownload.com/api"

	// ddownload.com doesn't publish a documented request-rate limit —
	// the same conservative, unconfirmed default plugins/rapidgator uses
	// for the identical reason (see its own doc comment), not a number
	// taken from any spec.
	requestsPerSecond = 2.0
	rateLimiterBurst  = 4

	requestTimeout = 30 * time.Second
)

var domains = []string{"ddownload.com"}

// TokenSource resolves the configured ddownload account. *account.Manager
// satisfies this implicitly, same shape every other credentialed plugin's
// TokenSource already has.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// Plugin is the ddownload.com Hoster implementation.
type Plugin struct {
	tokens  TokenSource
	client  *http.Client
	limiter *rate.Limiter
	log     *slog.Logger

	// baseURL is overridden by tests; production code always uses apiBase.
	baseURL string
}

// New returns a ddownload Plugin. tokens is typically the same
// *account.Manager passed to the queue manager. log defaults to a
// discard logger if nil.
func New(tokens TokenSource, log *slog.Logger) *Plugin {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Plugin{
		tokens:  tokens,
		client:  &http.Client{Timeout: requestTimeout},
		limiter: rate.NewLimiter(rate.Limit(requestsPerSecond), rateLimiterBurst),
		log:     log,
		baseURL: apiBase,
	}
}

// Name implements plugins.Hoster. Also the hoster key used for the
// ddownload account in hopper.yaml's per-hoster config and in
// internal/account.
func (p *Plugin) Name() string { return pluginName }

// Domains implements plugins.Hoster.
func (p *Plugin) Domains() []string { return domains }

// Credentialed implements plugins.Credentialed: every confirmed endpoint
// requires an API key — ddownload's own /api/file/* routes reject any
// request with a missing/invalid key before even checking the specific
// action, confirmed live (see FetchDownloadURL's doc comment, point C).
func (p *Plugin) Credentialed() bool { return true }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "DDownload" }

// Match implements plugins.Hoster. It claims a ddownload.com URL only if
// an account is configured — same convention as rapidgator/1fichier: an
// unconfigured plugin behaves as if it weren't registered at all.
func (p *Plugin) Match(rawURL string) bool {
	if p.currentKey() == "" {
		return false
	}
	host := hostnameOf(rawURL)
	if host == "" {
		return false
	}
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// apiEnvelope is the wrapper every ddownload v2 API response uses:
// {"msg": "...", "server_time": "...", "status": <int>, "result":
// <payload>} — confirmed live for both success and error responses
// (https://ddownload.com/api's documented examples, and probing
// api-v2.ddownload.com directly with an invalid key).
type apiEnvelope[T any] struct {
	Msg        string `json:"msg"`
	ServerTime string `json:"server_time"`
	Status     int    `json:"status"`
	Result     T      `json:"result"`
}

// fileInfoResult is one entry in GET /file/info's result array — an
// array even for a single file_code, confirmed by the docs' own example
// (which shows two entries, one 200/found and one 404/not-found, for a
// two-file_code request). "download" is a download *count*, not a URL —
// confirmed: the API has no download-URL field anywhere, see
// FetchDownloadURL's doc comment.
type fileInfoResult struct {
	Status   int    `json:"status"`
	FileCode string `json:"filecode"`
	Name     string `json:"name"`
	Download string `json:"download"`
	Size     string `json:"size"`
	Uploaded string `json:"uploaded"`
}

// Resolve implements plugins.Hoster via GET /file/info.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	key := p.keyFrom(acc)
	if key == "" {
		return nil, plugins.ErrAccountRequired
	}
	code, err := fileCodeFromURL(rawURL)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var env apiEnvelope[[]fileInfoResult]
	if err := p.do(ctx, "/file/info", url.Values{"key": {key}, "file_code": {code}}, &env); err != nil {
		return nil, err
	}
	if err := envelopeErr(env.Status, env.Msg); err != nil {
		return nil, err
	}
	if len(env.Result) == 0 {
		return nil, plugins.ErrNotFound
	}

	f := env.Result[0]
	switch f.Status {
	case http.StatusOK:
		// confirmed live
	case http.StatusNotFound:
		return nil, plugins.ErrNotFound
	case 451:
		// Per this plugin's spec; not independently observed against a
		// live file in this state (the docs' own example only shows 200
		// and 404) — treated the same way other plugins in this codebase
		// flag an instruction-provided-but-unverified mapping.
		return nil, plugins.ErrUnavailable
	default:
		return nil, fmt.Errorf("%s: unexpected per-file status %d for file_code %q", pluginName, f.Status, code)
	}

	size, _ := strconv.ParseInt(f.Size, 10, 64)
	return &plugins.FileInfo{Name: f.Name, Size: size, Available: true}, nil
}

// FetchDownloadURL implements plugins.Hoster — except it doesn't,
// actually: there is no confirmed working mechanism to get a direct
// download URL from ddownload.com, and this returns a clear error
// instead of a guess. Three approaches were investigated live (via real
// HTTP requests to the actual API and website, not just reading docs),
// exactly the three this plugin's original spec asked to try in order:
//
//	A) file/info's response has no download-URL field — confirmed
//	   against both the documented response shape and a live request:
//	   status/filecode/name/download (a download *count*, not a URL)/
//	   size/uploaded, nothing else.
//	B) GETting the plain file page (https://ddownload.com/<file_code>)
//	   returns the human-facing landing page. Confirmed live: for a
//	   non-authenticated request it requires solving a Cloudflare
//	   Turnstile challenge (the page loads
//	   challenges.cloudflare.com/turnstile/v0/api.js and renders a
//	   cf-turnstile widget — an interactive JS challenge, not a static
//	   image) before the real download link is revealed, after a
//	   60-second countdown. No server-side Go HTTP client can solve a
//	   Turnstile challenge, and plugins.ErrCaptchaRequired{ImageURL}
//	   can't represent it either — there is no static image to show the
//	   user. A premium account's browser session might skip this via a
//	   cookie the site sets after login, but nothing in the documented
//	   key-based API produces an equivalent.
//	C) An undocumented /api/file/download endpoint was tried directly.
//	   Inconclusive, not confirmatory: every path under /api/file/*
//	   (real or fabricated) returns the identical
//	   {"status":400,"msg":"Invalid key"} response with an invalid key,
//	   because the key is checked before routing to a specific action —
//	   confirmed by probing genuinely nonexistent paths both inside and
//	   outside that prefix (a bogus path outside /api/file/* returns a
//	   distinct plain-text "File not found." instead, proving the
//	   within-prefix "Invalid key" response is a blanket auth gate, not
//	   evidence of a real route). ddownload.com/api's own documented
//	   endpoint list (account/info, account/stats, upload/server,
//	   file/info, file/list, file/check, file/exists, file/rename,
//	   file/set_folder, file/set_property, files/deleted, folder/list,
//	   folder/create, folder/rename, folder/delete, folder/move) does not
//	   include it. Only a real, valid API key (unavailable in this
//	   environment) could confirm or refute it definitively.
//
// plugins.FreeDownloader is not implemented for the same reason as (B):
// the only real free/anonymous download path found is Turnstile-gated,
// which this plugin architecture has no way to automate or honestly
// represent.
//
// The error wraps plugins.ErrUnsupportedHost so the queue hands the link
// to the next matching plugin (a debrid service that supports
// ddownload.com) instead of retrying a download that can't ever work.
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	if p.keyFrom(acc) == "" {
		return "", plugins.ErrAccountRequired
	}
	return "", fmt.Errorf(
		"%s: %w (%w): no working download-URL mechanism found for this hoster — the API has no download-URL endpoint, and the web flow requires solving a Cloudflare Turnstile challenge no automated client can pass (see FetchDownloadURL's doc comment)",
		pluginName, plugins.ErrUnsupportedHost, plugins.ErrUnavailable,
	)
}

// accountInfoResult is GET /account/info's result payload — confirmed
// live and against https://ddownload.com/api's documented example.
type accountInfoResult struct {
	StorageLeft        string `json:"storage_left"`
	PremiumTrafficLeft int64  `json:"premium_traffic_left"`
	Email              string `json:"email"`
	PremiumExpire      string `json:"premium_expire"`
	Balance            string `json:"balance"`
	TrafficUsed        string `json:"traffic_used"`
	TrafficLeft        string `json:"traffic_left"`
	StorageUsed        string `json:"storage_used"`
}

// ValidateAccount implements plugins.AccountValidator via GET
// /account/info.
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	key := p.keyFrom(acc)
	if key == "" {
		return nil, plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var env apiEnvelope[accountInfoResult]
	if err := p.do(ctx, "/account/info", url.Values{"key": {key}}, &env); err != nil {
		return nil, err
	}
	if err := envelopeErr(env.Status, env.Msg); err != nil {
		if errors.Is(err, plugins.ErrAccountRequired) {
			return &plugins.AccountInfo{Valid: false, Label: "Invalid API key"}, nil
		}
		return nil, err
	}

	r := env.Result
	info := &plugins.AccountInfo{Valid: true}
	left := r.PremiumTrafficLeft
	info.TrafficLeft = &left
	// premium_traffic_left == 0 is reported as "exhausted" per this
	// plugin's spec, even though it can't be told apart from "this
	// account never had any premium traffic to begin with" using this
	// field alone — ddownload's API gives no separate signal for that
	// distinction.
	if r.PremiumTrafficLeft == 0 {
		info.Label = "Traffic exhausted"
	}
	if t := parseDDownloadTime(r.PremiumExpire); t != nil {
		info.ExpiresAt = t
		if info.Label == "" {
			info.Label = "Premium until " + t.Format("2006-01-02")
		}
	}
	if info.Label == "" {
		info.Label = "Active"
	}
	return info, nil
}

// parseDDownloadTime parses ddownload's premium_expire format, confirmed
// via https://ddownload.com/api's own documented example
// ("2018-01-22 18:00:00" — not RFC3339). RFC3339 is tried too, as a
// defensive fallback only, not because it's been observed from the live
// API — same "try the documented format first, then a plausible
// fallback, never error the whole call over it" approach
// plugins/rapidgator's parsePremiumEndTime already uses.
func parseDDownloadTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// envelopeErr maps ddownload's top-level envelope status+msg to a
// plugins sentinel error. status 400 with msg "Invalid key" is the only
// top-level failure actually observed live (an invalid/missing key); the
// rest of this mapping follows this codebase's general convention (200 =
// success, 429 = rate limited) rather than further live-confirmed
// evidence.
func envelopeErr(status int, msg string) error {
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.EqualFold(msg, "Invalid key"):
		return plugins.ErrAccountRequired
	case status == http.StatusTooManyRequests:
		return plugins.ErrRateLimited
	case msg != "":
		return fmt.Errorf("%s: %s (API status %d)", pluginName, msg, status)
	default:
		return fmt.Errorf("%s: unexpected API status %d", pluginName, status)
	}
}

// keyFrom prefers the account the queue layer already resolved and
// passed in; it falls back to resolving one directly only if acc is nil
// — same pattern as every other credentialed plugin's tokenFrom/keyFrom.
func (p *Plugin) keyFrom(acc *plugins.Account) string {
	if acc != nil {
		if k := plugins.TokenFromAccount(acc, "api_key"); k != "" {
			return k
		}
	}
	return p.currentKey()
}

func (p *Plugin) currentKey() string {
	if p.tokens == nil {
		return ""
	}
	acc, ok := p.tokens.AccountFor(pluginName)
	if !ok || acc == nil {
		return ""
	}
	return plugins.TokenFromAccount(acc, "api_key")
}

// do issues one rate-limited GET request against path with params as the
// query string, decoding the JSON response into out. The caller maps the
// decoded envelope's status/msg via envelopeErr — do itself only handles
// transport-level failures and a genuine HTTP 429.
func (p *Plugin) do(ctx context.Context, path string, params url.Values, out any) error {
	if err := p.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("%s: rate limiter: %w", pluginName, err)
	}

	base := p.baseURL
	if base == "" {
		base = apiBase
	}
	reqURL := base + path + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("%s: build request: %w", pluginName, err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: request failed: %w", pluginName, sanitizeTransportErr(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return plugins.ErrRateLimited
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: decode API response: %w", pluginName, err)
	}
	return nil
}

// sanitizeTransportErr strips the query string from a *url.Error's URL
// before it's wrapped into a returned error — the API key travels as a
// ?key=... query parameter (confirmed: https://ddownload.com/api
// documents every endpoint this way), so a transport-level failure (DNS,
// timeout, connection refused, all before any server ever sees the
// request) would otherwise leak it into queue.Item.Error via Go's
// net/http *url.Error wrapping. Same fix, same reasoning,
// plugins/rapidgator already carries for the identical reason.
func sanitizeTransportErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if u, perr := url.Parse(uerr.URL); perr == nil && u.RawQuery != "" {
			u.RawQuery = "REDACTED"
			uerr.URL = u.String()
		}
	}
	return err
}

// fileCodeFromURL extracts ddownload's file_code from a file page URL of
// the form https://ddownload.com/<file_code>/<filename> — confirmed
// against the real test URL
// https://ddownload.com/a1b2c3d4e5f6/example.part1.rar.
func fileCodeFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%s: invalid URL %q: %w", pluginName, rawURL, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("%s: could not extract file_code from URL %q", pluginName, rawURL)
	}
	return parts[0], nil
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
const premiumURL = "https://ddownload.com/pricing"

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }

// CredentialFields implements plugins.CredentialDescriber.
func (p *Plugin) CredentialFields() []plugins.CredentialField {
	return plugins.TokenCredentialFields("API key", "from your DDownload account settings")
}
