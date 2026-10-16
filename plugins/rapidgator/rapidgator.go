// Package rapidgator implements a Hoster plugin for rapidgator.net
// (and its rg.to URL shortener) against Rapidgator's own v2 API
// (https://rapidgator.net/article/api/index), rather than requiring a
// Real-Debrid account to download from Rapidgator at all.
//
// The docs page itself wasn't fetchable from this environment (same
// class of gap plugins/onefichier already documents); the endpoint
// paths, request parameters, and response field names below were
// cross-checked against several independent community client
// implementations instead (a Python downloader script, a Go-oriented
// TypeScript client with its own type definitions, and others) — they
// agree with each other on every point documented as "confirmed" below.
// Two real corrections to the original assumption this plugin was
// speculated to have, found this way rather than by further guessing:
//
//  1. file/info and file/download take a numeric-looking `file_id`
//     path segment extracted from the file's page URL
//     (`https://rapidgator.net/file/<file_id>/<name>.html`), *not* the
//     raw URL as a `url=` parameter — no reference implementation
//     found sends the full URL to either endpoint.
//  2. Every endpoint below (including login) is called via HTTP GET
//     with query parameters in every community client checked, not
//     POST — login is the one deliberate deviation: this plugin still
//     sends it as POST with a form-encoded body instead of a query
//     string, so a plaintext password never lands in a URL (and
//     therefore never in an access log, proxy history, etc.) — a
//     defensive choice, not a claim that Rapidgator's API rejects GET
//     there.
//
// The response envelope (`{"response": ..., "status": ..., "details":
// ...}`) and its user-object fields (email, is_premium, traffic,
// storage) are confirmed the same way. `premium_end_time`'s exact JSON
// type is not documented anywhere found (same category of gap as
// onefichier's premium-expiry field) — parsePremiumEndTime handles it
// best-effort and leaves AccountInfo.ExpiresAt nil rather than erroring
// if it can't make sense of it.
package rapidgator

import (
	"context"
	"encoding/json"
	"errors"
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
	pluginName = "rapidgator"
	apiBase    = "https://rapidgator.net/api/v2"

	// Rapidgator doesn't publish a documented request-rate limit (unlike
	// Real-Debrid's 250/min) — this is a conservative, unconfirmed
	// default chosen to avoid hammering the API, not a number taken from
	// any spec.
	requestsPerSecond = 2.0
	rateLimiterBurst  = 4

	requestTimeout = 30 * time.Second
)

// rg.to is Rapidgator's own URL shortener (rg.to/file/<hash> redirects
// to rapidgator.net/file/...) — see docs/ARCHITECTURE.md's rg.to entry. Its file
// page URLs use the same /file/<file_id>/<name> shape, so fileIDFromURL
// needs no domain-specific handling.
var domains = []string{"rapidgator.net", "rg.to"}

// TokenSource resolves the configured Rapidgator account (username +
// password — no Extra fields needed, unlike Real-Debrid/1fichier, since
// Rapidgator's v2 API has no long-lived API-key concept to store
// there). *account.Manager satisfies this implicitly, same as every
// other credentialed plugin's identically-shaped TokenSource.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// cachedSession holds the most recently obtained Rapidgator API token
// for one username. Unlike Real-Debrid/1fichier (where the stored
// account credential *is* the API token), Rapidgator's v2 API only
// ever hands out a token via a username+password login exchange — this
// cache avoids re-logging in on every Resolve/FetchDownloadURL call.
// Rapidgator doesn't document the token's TTL, so expiry is detected
// reactively (an API call reporting the token invalid), never on a
// timer.
type cachedSession struct {
	username string
	token    string
}

// Plugin is the Rapidgator Hoster implementation.
type Plugin struct {
	tokens  TokenSource
	client  *http.Client
	limiter *rate.Limiter
	log     *slog.Logger

	// baseURL is overridden by tests; production code always uses
	// apiBase.
	baseURL string

	mu   sync.Mutex
	sess *cachedSession
}

// New returns a Rapidgator Plugin. tokens is typically the same
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
// Rapidgator account in hopper.yaml's per-hoster config and in
// internal/account.
func (p *Plugin) Name() string { return pluginName }

// Domains implements plugins.Hoster.
func (p *Plugin) Domains() []string { return domains }

// Credentialed implements plugins.Credentialed: this plugin needs a
// configured username+password to do anything at all.
func (p *Plugin) Credentialed() bool { return true }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "Rapidgator" }

// Match implements plugins.Hoster. It claims a rapidgator.net/rg.to URL
// only if a Rapidgator account is configured — same convention as
// Real-Debrid/1fichier: an unconfigured plugin behaves as if it weren't
// registered at all, and (per queue.Manager's two-pass resolution) a
// configured Real-Debrid account still wins for either domain since it
// gets first refusal in pass 1. Deliberately does not attempt a login —
// Match must stay cheap and account-presence-only, unlike Resolve/
// FetchDownloadURL, which do need a live token.
func (p *Plugin) Match(rawURL string) bool {
	if !p.hasAccount() {
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

// Resolve implements plugins.Hoster via GET /file/info.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	fileID, err := fileIDFromURL(rawURL)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var file fileObject
	err = p.callWithToken(ctx, acc, func(token string) error {
		f, ferr := p.fileInfo(ctx, fileID, token)
		if ferr != nil {
			return ferr
		}
		file = f
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &plugins.FileInfo{Name: file.Name, Size: file.Size, Available: true}, nil
}

// FetchDownloadURL implements plugins.Hoster via GET /file/download.
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	fileID, err := fileIDFromURL(rawURL)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var downloadURL string
	err = p.callWithToken(ctx, acc, func(token string) error {
		u, derr := p.fileDownload(ctx, fileID, token)
		if derr != nil {
			return derr
		}
		downloadURL = u
		return nil
	})
	if err != nil {
		return "", err
	}
	return downloadURL, nil
}

// freeDownloadResponse is the shape of a free-tier POST /file/download
// response: one of a direct url, a wait_time cooldown (seconds), a
// captcha challenge, or an error — flat, *not* wrapped in the
// {"response":...,"status":...,"details":...} envelope every other
// endpoint in this package uses (apiEnvelope[T]). Unlike the other
// endpoints in this package, this shape is not cross-checked against a
// live account or a community client — no reference implementation of a
// free-tier download flow was found. Treat it as unconfirmed until
// verified against a real free/non-premium Rapidgator account.
type freeDownloadResponse struct {
	URL      string `json:"url"`
	WaitTime int    `json:"wait_time"`
	Captcha  *struct {
		ImageURL string `json:"image_url"`
	} `json:"captcha"`
	Error string `json:"error"`
}

// FetchFreeDownloadURL implements plugins.FreeDownloader for a configured
// Rapidgator account that turns out not to be premium — the plain GET
// /file/download FetchDownloadURL uses requires premium and fails with
// plugins.ErrAccountRequired otherwise (see mapAPIError's "session not
// exist"/401 handling). It POSTs (not GETs) to the same /file/download
// path, with the session token still included — a fully anonymous,
// no-account request is deliberately out of scope: Match still requires
// a configured account regardless of that account's premium status (see
// Match's own doc comment for why that's staying as-is).
func (p *Plugin) FetchFreeDownloadURL(ctx context.Context, rawURL string, acc *plugins.Account) (string, error) {
	fileID, err := fileIDFromURL(rawURL)
	if err != nil {
		return "", err
	}

	var out freeDownloadResponse
	err = p.callWithToken(ctx, acc, func(token string) error {
		resp, derr := p.do(ctx, http.MethodPost, "/file/download", url.Values{"file_id": {fileID}, "token": {token}})
		if derr != nil {
			return derr
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == http.StatusTooManyRequests {
			return plugins.ErrRateLimited
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return plugins.ErrAccountRequired
		}
		out = freeDownloadResponse{}
		if jerr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); jerr != nil {
			return fmt.Errorf("%s: decode free file/download response: %w", pluginName, jerr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	switch {
	case out.URL != "":
		return out.URL, nil
	case out.WaitTime > 0:
		return "", plugins.ErrRetryAfter{Seconds: out.WaitTime}
	case out.Captcha != nil:
		return "", plugins.ErrCaptchaRequired{ImageURL: out.Captcha.ImageURL}
	case out.Error != "":
		return "", fmt.Errorf("%s: %s", pluginName, out.Error)
	default:
		return "", fmt.Errorf("%s: %w: empty response from free download endpoint", pluginName, plugins.ErrUnavailable)
	}
}

// ValidateAccount implements plugins.AccountValidator via GET
// /user/info.
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var user userObject
	err := p.callWithToken(ctx, acc, func(token string) error {
		u, uerr := p.userInfo(ctx, token)
		if uerr != nil {
			return uerr
		}
		user = u
		return nil
	})
	if err != nil {
		if errors.Is(err, plugins.ErrAccountRequired) {
			return &plugins.AccountInfo{Valid: false, Label: "Invalid username or password"}, nil
		}
		return nil, err
	}

	info := &plugins.AccountInfo{Valid: true, Label: "Free (not premium)"}
	if user.IsPremium {
		info.Label = "Premium"
	}
	if user.Traffic.Left > 0 {
		left := user.Traffic.Left
		info.TrafficLeft = &left
	}
	if t := parsePremiumEndTime(user.PremiumEndTimeRaw); t != nil {
		info.ExpiresAt = t
		info.Label = "Premium until " + t.Format("2006-01-02")
	}
	return info, nil
}

// callWithToken invokes fn with a cached (or freshly obtained) session
// token, retrying once with a freshly-logged-in token if fn reports
// plugins.ErrAccountRequired — the only reliable signal this API gives
// that a cached token has expired or been invalidated server-side,
// since Rapidgator doesn't document a TTL to pre-empt it with.
func (p *Plugin) callWithToken(ctx context.Context, acc *plugins.Account, fn func(token string) error) error {
	token, err := p.tokenFor(ctx, acc)
	if err != nil {
		return err
	}
	err = fn(token)
	if !errors.Is(err, plugins.ErrAccountRequired) {
		return err
	}

	p.invalidateSession()
	token, err = p.tokenFor(ctx, acc)
	if err != nil {
		return err
	}
	return fn(token)
}

// tokenFor resolves the username/password to authenticate as (acc if
// the queue layer already resolved one and passed it in, else whatever
// account is configured for this hoster key) and returns a valid
// session token for it, logging in only if the cache is empty or holds
// a different username.
func (p *Plugin) tokenFor(ctx context.Context, acc *plugins.Account) (string, error) {
	username, password, ok := p.credentialsFrom(acc)
	if !ok {
		return "", plugins.ErrAccountRequired
	}

	p.mu.Lock()
	if p.sess != nil && p.sess.username == username {
		token := p.sess.token
		p.mu.Unlock()
		return token, nil
	}
	p.mu.Unlock()

	token, err := p.login(ctx, username, password)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.sess = &cachedSession{username: username, token: token}
	p.mu.Unlock()
	return token, nil
}

func (p *Plugin) invalidateSession() {
	p.mu.Lock()
	p.sess = nil
	p.mu.Unlock()
}

func (p *Plugin) hasAccount() bool {
	_, _, ok := p.credentialsFrom(nil)
	return ok
}

// credentialsFrom prefers the account the queue layer already resolved
// and passed in; it falls back to resolving one directly only if acc is
// nil (Resolve/FetchDownloadURL are documented as accepting a nil acc
// for hosters that support anonymous access, which Rapidgator does not,
// so this fallback keeps working even if a caller passes nil) — same
// pattern as realdebrid.tokenFrom/onefichier.tokenFrom, adapted for a
// username+password pair instead of a single opaque token.
func (p *Plugin) credentialsFrom(acc *plugins.Account) (username, password string, ok bool) {
	if acc != nil && acc.Username != "" && acc.Password != "" {
		return acc.Username, acc.Password, true
	}
	if p.tokens == nil {
		return "", "", false
	}
	a, found := p.tokens.AccountFor(pluginName)
	if !found || a == nil || a.Username == "" || a.Password == "" {
		return "", "", false
	}
	return a.Username, a.Password, true
}

// apiEnvelope is the wrapper every Rapidgator v2 response uses:
// {"response": <payload>, "status": <int>, "details": <string, often
// empty on success>}.
type apiEnvelope[T any] struct {
	Response T      `json:"response"`
	Status   int    `json:"status"`
	Details  string `json:"details"`
}

type loginResponse struct {
	Token string `json:"token"`
}

type fileObject struct {
	FileID string `json:"file_id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
}

type downloadObject struct {
	DownloadURL string `json:"download_url"`
}

type userObject struct {
	Email     string `json:"email"`
	IsPremium bool   `json:"is_premium"`
	// PremiumEndTimeRaw is decoded lazily by parsePremiumEndTime — see
	// the package doc comment for why its type isn't trusted upfront.
	PremiumEndTimeRaw json.RawMessage `json:"premium_end_time"`
	Traffic           struct {
		Total int64 `json:"total"`
		Left  int64 `json:"left"`
	} `json:"traffic"`
}

// login exchanges username+password for a session token via POST
// (deliberately not GET — see the package doc comment) with a
// form-encoded body.
func (p *Plugin) login(ctx context.Context, username, password string) (string, error) {
	resp, err := p.do(ctx, http.MethodPost, "/user/login", url.Values{"login": {username}, "password": {password}})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := decodeEnvelope[loginResponse](resp)
	if err != nil {
		return "", err
	}
	if body.Token == "" {
		return "", fmt.Errorf("%s: empty token in user/login response", pluginName)
	}
	return body.Token, nil
}

func (p *Plugin) fileInfo(ctx context.Context, fileID, token string) (fileObject, error) {
	resp, err := p.do(ctx, http.MethodGet, "/file/info", url.Values{"file_id": {fileID}, "token": {token}})
	if err != nil {
		return fileObject{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeEnvelope[fileObject](resp)
}

func (p *Plugin) fileDownload(ctx context.Context, fileID, token string) (string, error) {
	resp, err := p.do(ctx, http.MethodGet, "/file/download", url.Values{"file_id": {fileID}, "token": {token}})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := decodeEnvelope[downloadObject](resp)
	if err != nil {
		return "", err
	}
	if body.DownloadURL == "" {
		return "", fmt.Errorf("%s: %w: empty download_url in response", pluginName, plugins.ErrUnavailable)
	}
	return body.DownloadURL, nil
}

func (p *Plugin) userInfo(ctx context.Context, token string) (userObject, error) {
	resp, err := p.do(ctx, http.MethodGet, "/user/info", url.Values{"token": {token}})
	if err != nil {
		return userObject{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeEnvelope[userObject](resp)
}

// do issues one rate-limited Rapidgator API request. The caller is
// responsible for consuming/closing the returned response body (done
// uniformly by decodeEnvelope).
func (p *Plugin) do(ctx context.Context, method, path string, form url.Values) (*http.Response, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("%s: rate limiter: %w", pluginName, err)
	}

	base := p.baseURL
	if base == "" {
		base = apiBase
	}
	reqURL := base + path

	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			reqURL += "?" + form.Encode()
		}
	} else if len(form) > 0 {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", pluginName, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", pluginName, sanitizeTransportErr(err))
	}
	return resp, nil
}

// sanitizeTransportErr strips the query string from a *url.Error's URL
// before it's wrapped into a returned error. Unlike Real-Debrid/1fichier
// (which send their token via an Authorization header), every
// authenticated GET here carries the session token as a ?token=...
// query parameter (see the package doc comment on why every endpoint is
// GET-with-query, not POST) — a transport-level failure (DNS, timeout,
// connection refused, all before any server ever sees the request) has
// Go's net/http wrap the full request URL, token included, into the
// error text. That text is threaded straight through to queue.Item.Error
// (persisted to bbolt, shown in the Web UI, and logged via
// queue.Manager's "queue: item failed") with no chance to redact it
// downstream, unlike a request that actually reached the server (whose
// token is only ever in a header the server logs, not this error path).
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

// decodeEnvelope reads resp.Body (closing it is the caller's
// responsibility), decoding it as apiEnvelope[T] and translating a
// non-success envelope status into a plugins sentinel error via
// mapAPIError. Transport-level 429 and 401 are checked first, ahead of
// decoding: a rate-limiting proxy in front of the API might not return
// Rapidgator's own JSON envelope shape at all, and neither does a bare
// HTTP 401 with an empty or non-JSON body — every 401 observed from the
// real API so far does carry a well-formed envelope (handled below, by
// mapAPIError, once decoding succeeds), but treating a real HTTP-level
// 401 as ErrAccountRequired unconditionally, rather than only trying to
// parse it as JSON and returning a "decode API response" error instead,
// is what lets callWithToken's cached-token-expired retry actually catch
// this case too.
func decodeEnvelope[T any](resp *http.Response) (T, error) {
	var zero T

	if resp.StatusCode == http.StatusTooManyRequests {
		return zero, plugins.ErrRateLimited
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return zero, plugins.ErrAccountRequired
	}

	var env apiEnvelope[T]
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return zero, fmt.Errorf("%s: decode API response: %w", pluginName, err)
	}
	if err := mapAPIError(env.Status, env.Details); err != nil {
		return zero, err
	}
	return env.Response, nil
}

// mapAPIError maps a Rapidgator v2 envelope's status+details to a
// plugins sentinel error where one applies. details substring matches
// are checked *before* treating status == http.StatusOK as success:
// no community reference pins down whether an application-level error
// like "File not found" travels with a distinct status code or with
// a plain 200 (only 200-success and 401-auth-failure are confirmed —
// see the package doc comment), so a recognized error string always
// wins regardless of status. details substring matches are based on
// the exact error strings those implementations document ("Login or
// password is wrong", "File not found"/"Folder not found"/"Link not
// found", "Session not exist") plus "session doesn't exist" — the
// exact wording a live, unauthenticated GET /file/info actually
// returned during integration testing ({"status":401,"details":"Error.
// Session doesn't exist"}), confirming a real API response differs
// slightly from the community-documented "Session not exist". In
// practice this exact case is already caught by the
// status == http.StatusUnauthorized check regardless (decodeEnvelope's
// own 401 short-circuit hits first), so this substring only matters as
// a backstop for the same message arriving with some other status.
func mapAPIError(status int, details string) error {
	d := strings.ToLower(details)
	switch {
	case status == http.StatusTooManyRequests:
		return plugins.ErrRateLimited
	case status == http.StatusUnauthorized ||
		strings.Contains(d, "login or password is wrong") ||
		strings.Contains(d, "session not exist") ||
		strings.Contains(d, "session doesn't exist"):
		return plugins.ErrAccountRequired
	case strings.Contains(d, "not found"):
		return plugins.ErrNotFound
	case status == http.StatusServiceUnavailable:
		return plugins.ErrUnavailable
	case status == http.StatusOK:
		return nil
	case details != "":
		return fmt.Errorf("%s: %s (API status %d)", pluginName, details, status)
	default:
		return fmt.Errorf("%s: unexpected API status %d", pluginName, status)
	}
}

// parsePremiumEndTime is best-effort — see the package doc comment for
// why premium_end_time's exact JSON type isn't trusted upfront. It tries
// a Unix-seconds integer first (the more common convention for this
// kind of field in similar APIs), then a couple of plausible date-string
// formats, and returns nil — rather than erroring the whole
// ValidateAccount call — if none match. An unrecognized expiry
// shouldn't block reporting the rest of the account's validity.
func parsePremiumEndTime(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		t := time.Unix(n, 0).UTC()
		return &t
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return &t
			}
		}
	}
	return nil
}

// fileIDFromURL extracts Rapidgator's file_id from a file page URL of
// the form https://rapidgator.net/file/<file_id>/<name>.html (or the
// rg.to short-link form, which shares the same path shape) — the v2
// API's file/info and file/download endpoints take file_id, not the raw
// URL; see the package doc comment.
func fileIDFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%s: invalid URL %q: %w", pluginName, rawURL, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, seg := range parts {
		if seg == "file" && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("%s: could not extract file_id from URL %q", pluginName, rawURL)
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
const premiumURL = "https://rapidgator.net/article/premium"

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }
