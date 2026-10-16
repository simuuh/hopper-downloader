// Package katfile implements a Hoster plugin for katfile.cc/katfile.biz
// against its official v2 API (https://katfile.cc/api/v2/). Unlike
// several other plugins in this codebase, whose API docs weren't
// fetchable from earlier environments and had to be reverse-engineered
// from community client libraries, katfile's own docs page
// (https://katfile.cc/api) was fetched directly and is the source for
// every endpoint/field name below — "confirmed" throughout this
// package's doc comments means "matches that live page or a live probe
// against the real API", not a guess.
//
// plugins.FreeDownloader is not implemented — also a deliberate,
// documented decision, not an oversight. See FetchDownloadURL's doc
// comment for the live investigation into katfile's free-download page
// (a real, non-captcha wait+form flow was found and partially automated,
// but never confirmed to actually complete end-to-end in this
// environment) that led to it being left out rather than shipped
// unverified.
package katfile

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
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/simuuh/hopper-downloader/plugins"
)

const (
	pluginName = "katfile"
	apiBase    = "https://katfile.cc/api/v2"

	// katfile doesn't publish a documented request-rate limit — the same
	// conservative, unconfirmed default plugins/rapidgator uses for the
	// identical reason (see its own doc comment), not a number taken
	// from any spec.
	requestsPerSecond = 2.0
	rateLimiterBurst  = 4

	requestTimeout = 30 * time.Second

	// An access_token is documented as valid for 1 hour of inactivity;
	// re-authorizing 5 minutes early avoids a token expiring mid-request
	// under normal use without needing to track "inactivity" precisely
	// (this plugin doesn't distinguish idle time from steady use — it
	// just never lets a cached token live longer than this regardless).
	tokenMaxAge = 55 * time.Minute
)

// domains covers both of katfile's confirmed active TLDs — the .cc
// domain the API itself lives under, and .biz, which this plugin's own
// test URL (https://katfile.biz/...) uses. Both are documented/observed
// live to serve the same file catalog against the same API.
var domains = []string{"katfile.cc", "katfile.biz"}

// TokenSource resolves the configured katfile account (key1+key2 — see
// credentialsFrom). *account.Manager satisfies this implicitly, same
// shape every other credentialed plugin's TokenSource already has.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// cachedSession holds the most recently obtained access_token+account_id
// for one key1/key2 pair, so Resolve/FetchDownloadURL/ValidateAccount
// don't each re-authorize independently — /authorize's own docs
// explicitly ask callers to reuse a token across requests rather than
// minting a new one per call.
type cachedSession struct {
	key1        string
	accessToken string
	accountID   string
	obtainedAt  time.Time
}

// Plugin is the katfile Hoster implementation.
type Plugin struct {
	tokens  TokenSource
	client  *http.Client
	limiter *rate.Limiter
	log     *slog.Logger

	// baseURL is overridden by tests; production code always uses apiBase.
	baseURL string

	mu   sync.Mutex
	sess *cachedSession
}

// New returns a katfile Plugin. tokens is typically the same
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
// katfile account in hopper.yaml's per-hoster config and in
// internal/account.
func (p *Plugin) Name() string { return pluginName }

// Domains implements plugins.Hoster.
func (p *Plugin) Domains() []string { return domains }

// Credentialed implements plugins.Credentialed: katfile is unusable here
// without key1+key2.
func (p *Plugin) Credentialed() bool { return true }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "Katfile" }

// Match implements plugins.Hoster. It claims a katfile.cc/katfile.biz URL
// only if an account is configured — same convention as rapidgator/
// 1fichier/ddownload: an unconfigured plugin behaves as if it weren't
// registered at all. Deliberately does not attempt to authorize — Match
// must stay cheap and account-presence-only, unlike Resolve/
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

// successEnvelope is the wrapper every katfile v2 API response uses on
// success: {"data": <payload>, "_status": "success", "_datetime": "..."}
// — confirmed live. A failed call uses a *different* shape
// (errorEnvelope, below): the payload key is "response", not "data", and
// it holds a plain string message rather than an object. decodeEnvelope
// tells the two apart via _status before trying to unmarshal "data" into
// a typed T, since a bare `{"data": T}` decode would otherwise silently
// zero-value T on an error response instead of surfacing the real
// message.
type successEnvelope[T any] struct {
	Data     T      `json:"data"`
	Status   string `json:"_status"`
	Datetime string `json:"_datetime"`
}

type errorEnvelope struct {
	Response string `json:"response"`
	Status   string `json:"_status"`
	Datetime string `json:"_datetime"`
}

type authorizeResult struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

// authorize exchanges key1+key2 for an access_token+account_id via POST
// /authorize, confirmed live (including the exact envelope shape and the
// "Could not authenticate user..." error message for bad keys).
func (p *Plugin) authorize(ctx context.Context, key1, key2 string) (accessToken, accountID string, err error) {
	var result authorizeResult
	if err := p.doAuthed(ctx, "/authorize", url.Values{"key1": {key1}, "key2": {key2}}, &result); err != nil {
		if isAuthError(err) {
			return "", "", plugins.ErrAccountRequired
		}
		return "", "", err
	}
	if result.AccessToken == "" || result.AccountID == "" {
		return "", "", fmt.Errorf("%s: %w: empty access_token/account_id in /authorize response", pluginName, plugins.ErrAccountRequired)
	}
	return result.AccessToken, result.AccountID, nil
}

// isAuthError reports whether err's message looks like one of katfile's
// documented authorization failures ("Could not authenticate user...",
// "Failed issuing access token.") — confirmed live for the first; the
// second is documented but not independently reproduced (it requires a
// specific server-side failure mode, not just bad credentials).
func isAuthError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not authenticate") || strings.Contains(msg, "failed issuing access token")
}

// sessionFor returns a valid (access_token, account_id) pair for
// key1/key2, from cache if still fresh (see tokenMaxAge) and for the same
// key1, else by calling authorize.
func (p *Plugin) sessionFor(ctx context.Context, key1, key2 string) (accessToken, accountID string, err error) {
	p.mu.Lock()
	if p.sess != nil && p.sess.key1 == key1 && time.Since(p.sess.obtainedAt) < tokenMaxAge {
		token, id := p.sess.accessToken, p.sess.accountID
		p.mu.Unlock()
		return token, id, nil
	}
	p.mu.Unlock()

	token, id, err := p.authorize(ctx, key1, key2)
	if err != nil {
		return "", "", err
	}
	p.mu.Lock()
	p.sess = &cachedSession{key1: key1, accessToken: token, accountID: id, obtainedAt: time.Now()}
	p.mu.Unlock()
	return token, id, nil
}

func (p *Plugin) invalidateSession() {
	p.mu.Lock()
	p.sess = nil
	p.mu.Unlock()
}

// callAuthorized invokes fn with a cached (or freshly obtained)
// access_token/account_id, retrying once with a freshly-authorized
// session if fn reports the documented "Could not validate access_token"
// error — confirmed live wording — the only reliable signal this API
// gives that a cached token has expired or been invalidated
// server-side. Mirrors plugins/rapidgator's callWithToken, adapted for a
// token *pair* instead of a single opaque token.
func (p *Plugin) callAuthorized(ctx context.Context, acc *plugins.Account, fn func(accessToken, accountID string) error) error {
	key1, key2, ok := p.credentialsFrom(acc)
	if !ok {
		return plugins.ErrAccountRequired
	}

	token, id, err := p.sessionFor(ctx, key1, key2)
	if err != nil {
		return err
	}
	err = fn(token, id)
	if err == nil || !isSessionInvalidError(err) {
		return err
	}

	p.invalidateSession()
	token, id, err = p.sessionFor(ctx, key1, key2)
	if err != nil {
		return err
	}
	return fn(token, id)
}

// isSessionInvalidError reports whether err's message is katfile's
// documented "Could not validate access_token and account_id, please
// reauthenticate or try again." — confirmed live (this exact string
// appears on every one of the file/account endpoints' documented error
// lists, and was reproduced by calling /file/download with a fabricated
// access_token/account_id).
func isSessionInvalidError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "reauthenticate")
}

// fileDownloadResult is POST /file/download's success payload — confirmed
// live and against https://katfile.cc/api's documented example. Accepts
// either file_id (numeric) or short_url as input (documented explicitly
// as "One of:") — this plugin always calls it with short_url, since that
// (not a numeric file_id) is all a katfile.cc/<short_url>/<filename> page
// URL ever yields.
type fileDownloadResult struct {
	FileID      string `json:"file_id"`
	Filename    string `json:"filename"`
	DownloadURL string `json:"download_url"`
}

func (p *Plugin) fileDownload(ctx context.Context, accessToken, accountID, shortURL string) (fileDownloadResult, error) {
	var result fileDownloadResult
	err := p.doAuthed(ctx, "/file/download", url.Values{
		"access_token": {accessToken},
		"account_id":   {accountID},
		"short_url":    {shortURL},
	}, &result)
	if err != nil {
		return fileDownloadResult{}, mapFileError(err)
	}
	return result, nil
}

// fileInfoResult is POST /file/info's success payload. The docs' own
// sample response is truncated ("..."), but /folder/listing's fuller
// per-file object (id/filename/shortUrl/fileType/extension/fileSize/
// status/downloads/folderId/keywords/url_file) is documented as the same
// underlying file model — fileSize is trusted to be present here too on
// that basis, not from a literal example in /file/info's own section.
type fileInfoResult struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	ShortURL string `json:"shortUrl"`
	FileSize string `json:"fileSize"`
}

func (p *Plugin) fileInfo(ctx context.Context, accessToken, accountID, fileID string) (fileInfoResult, error) {
	var result fileInfoResult
	err := p.doAuthed(ctx, "/file/info", url.Values{
		"access_token": {accessToken},
		"account_id":   {accountID},
		"file_id":      {fileID},
	}, &result)
	if err != nil {
		return fileInfoResult{}, mapFileError(err)
	}
	return result, nil
}

// mapFileError maps a decoded error message to plugins.ErrNotFound for
// katfile's documented "Could not find file based on file_id." — the
// same message /file/download and /file/info both use per their
// respective "Possible Errors" lists, confirmed live for /file/download.
// Any other error is passed through unchanged (isSessionInvalidError/
// isAuthError are checked separately, upstream of this, by
// callAuthorized/authorize).
func mapFileError(err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "could not find file") {
		return fmt.Errorf("%s: %w", pluginName, plugins.ErrNotFound)
	}
	return err
}

// Resolve implements plugins.Hoster. katfile's API has no endpoint that
// accepts a short_url and returns full metadata directly: /file/info
// (which does return fileSize) only documents accepting a numeric
// file_id, not short_url, unlike /file/download (which explicitly
// documents accepting either). So Resolve calls /file/download first —
// confirming the file exists and getting its numeric file_id and
// filename — then /file/info with that file_id to get fileSize. This
// means a single Resolve call already mints a download_url as a side
// effect (discarded here; FetchDownloadURL mints its own, independently,
// same as every other plugins.Hoster implementation's Resolve/
// FetchDownloadURL pair already do for their own, unrelated endpoints) —
// a real, confirmed consequence of katfile's documented API shape, not a
// workaround chosen for convenience.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	shortURL, err := shortURLFromURL(rawURL)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var dl fileDownloadResult
	if err := p.callAuthorized(ctx, acc, func(accessToken, accountID string) error {
		res, ferr := p.fileDownload(ctx, accessToken, accountID, shortURL)
		if ferr != nil {
			return ferr
		}
		dl = res
		return nil
	}); err != nil {
		return nil, err
	}

	name := dl.Filename
	var size int64
	if dl.FileID != "" {
		var info fileInfoResult
		if err := p.callAuthorized(ctx, acc, func(accessToken, accountID string) error {
			res, ferr := p.fileInfo(ctx, accessToken, accountID, dl.FileID)
			if ferr != nil {
				return ferr
			}
			info = res
			return nil
		}); err == nil {
			if info.Filename != "" {
				name = info.Filename
			}
			size, _ = strconv.ParseInt(info.FileSize, 10, 64)
		}
		// A file/info failure here is deliberately non-fatal: /file/download
		// already confirmed the file exists and gave us a filename, so a
		// size-lookup failure degrades to Size: 0 (unknown) rather than
		// failing the whole Resolve — the downloader already tolerates an
		// unknown ExpectedSize.
	}

	return &plugins.FileInfo{Name: name, Size: size, Available: true}, nil
}

// FetchDownloadURL implements plugins.Hoster via POST /file/download,
// confirmed live and documented — unlike plugins/ddownload, katfile's API
// does have a real, working download-URL endpoint.
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	shortURL, err := shortURLFromURL(rawURL)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var dl fileDownloadResult
	err = p.callAuthorized(ctx, acc, func(accessToken, accountID string) error {
		res, ferr := p.fileDownload(ctx, accessToken, accountID, shortURL)
		if ferr != nil {
			return ferr
		}
		dl = res
		return nil
	})
	if err != nil {
		return "", err
	}
	if dl.DownloadURL == "" {
		return "", fmt.Errorf("%s: %w: empty download_url in response", pluginName, plugins.ErrUnavailable)
	}
	return dl.DownloadURL, nil
}

type accountInfoResult struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	LevelID        string `json:"level_id"`
	Email          string `json:"email"`
	PaidExpiryDate string `json:"paidExpiryDate"`
}

type accountPackageResult struct {
	Label     string `json:"label"`
	LevelType string `json:"level_type"`
}

// ValidateAccount implements plugins.AccountValidator via /authorize
// (confirming the keys work at all), /account/package (for label/
// level_type), and /account/info (for paidExpiryDate) — three real,
// documented API calls, not one.
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	key1, key2, ok := p.credentialsFrom(acc)
	if !ok {
		return nil, plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	token, id, err := p.authorize(ctx, key1, key2)
	if err != nil {
		if errors.Is(err, plugins.ErrAccountRequired) {
			return &plugins.AccountInfo{Valid: false, Label: "Invalid key1/key2"}, nil
		}
		return nil, err
	}
	p.mu.Lock()
	p.sess = &cachedSession{key1: key1, accessToken: token, accountID: id, obtainedAt: time.Now()}
	p.mu.Unlock()

	var pkg accountPackageResult
	if err := p.doAuthed(ctx, "/account/package", url.Values{"access_token": {token}, "account_id": {id}}, &pkg); err != nil {
		return nil, err
	}

	info := &plugins.AccountInfo{Valid: true, Label: pkg.Label}
	if strings.EqualFold(pkg.LevelType, "free user") {
		info.Label = "Free account"
	}

	var acctInfo accountInfoResult
	if err := p.doAuthed(ctx, "/account/info", url.Values{"access_token": {token}, "account_id": {id}}, &acctInfo); err == nil {
		if t := parseKatfileTime(acctInfo.PaidExpiryDate); t != nil {
			info.ExpiresAt = t
		}
	}
	// An /account/info failure is non-fatal here — /account/package
	// already confirmed the account is valid; the expiry date is a nice-
	// to-have this shouldn't fail the whole validation over.

	return info, nil
}

// parseKatfileTime parses katfile's date fields (e.g. paidExpiryDate,
// lastPayment), confirmed live to use "YYYY-MM-DD HH:MM:SS" — not
// RFC3339. RFC3339 is tried too, as a defensive fallback only, not
// because it's been observed from the live API — same approach
// plugins/rapidgator's parsePremiumEndTime and plugins/ddownload's
// parseDDownloadTime already use for their own hosters' non-standard
// date fields.
func parseKatfileTime(s string) *time.Time {
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

// credentialsFrom prefers the account the queue layer already resolved
// and passed in; it falls back to resolving one directly only if acc is
// nil. key1/key2 are read from Extra["key1"]/Extra["key2"] first, falling
// back to Username=key1/Password=key2 (the Web UI's account form has no
// Extra fields), and unlike a single-token plugin's
// TokenFromAccount (one value, one fallback), a two-key credential has no
// existing shared helper in plugins/plugin.go to reuse, so both
// conventions are checked explicitly here.
func (p *Plugin) credentialsFrom(acc *plugins.Account) (key1, key2 string, ok bool) {
	if acc != nil {
		if k1, k2 := acc.Extra["key1"], acc.Extra["key2"]; k1 != "" && k2 != "" {
			return k1, k2, true
		}
		if acc.Username != "" && acc.Password != "" {
			return acc.Username, acc.Password, true
		}
	}
	if p.tokens == nil {
		return "", "", false
	}
	a, found := p.tokens.AccountFor(pluginName)
	if !found || a == nil {
		return "", "", false
	}
	if k1, k2 := a.Extra["key1"], a.Extra["key2"]; k1 != "" && k2 != "" {
		return k1, k2, true
	}
	if a.Username != "" && a.Password != "" {
		return a.Username, a.Password, true
	}
	return "", "", false
}

func (p *Plugin) hasAccount() bool {
	_, _, ok := p.credentialsFrom(nil)
	return ok
}

// doAuthed issues one rate-limited POST request against path with params
// as a form-encoded body, decoding either the success envelope's "data"
// (into out) or the error envelope's "response" message (returned as a
// plain error) depending on _status — see successEnvelope's doc comment
// for why the two shapes need to be told apart before decoding.
func (p *Plugin) doAuthed(ctx context.Context, path string, params url.Values, out any) error {
	if err := p.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("%s: rate limiter: %w", pluginName, err)
	}

	base := p.baseURL
	if base == "" {
		base = apiBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(params.Encode()))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", pluginName, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: request failed: %w", pluginName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return plugins.ErrRateLimited
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: read API response: %w", pluginName, err)
	}

	var probe struct {
		Status string `json:"_status"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("%s: decode API response: %w", pluginName, err)
	}
	if !strings.EqualFold(probe.Status, "success") {
		var eenv errorEnvelope
		if err := json.Unmarshal(body, &eenv); err != nil {
			return fmt.Errorf("%s: decode API error response: %w", pluginName, err)
		}
		if eenv.Response == "" {
			eenv.Response = "unknown error"
		}
		return fmt.Errorf("%s: %s", pluginName, eenv.Response)
	}

	var senv successEnvelope[json.RawMessage]
	if err := json.Unmarshal(body, &senv); err != nil {
		return fmt.Errorf("%s: decode API response: %w", pluginName, err)
	}
	if err := json.Unmarshal(senv.Data, out); err != nil {
		return fmt.Errorf("%s: decode API response data: %w", pluginName, err)
	}
	return nil
}

// shortURLFromURL extracts katfile's short_url from a file page URL of
// the form https://katfile.cc/<short_url>/<filename> or the .biz
// equivalent — confirmed against the real test URL
// https://katfile.biz/k7l8m9n0p1q2/example.part2.rar.
func shortURLFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%s: invalid URL %q: %w", pluginName, rawURL, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("%s: could not extract short_url from URL %q", pluginName, rawURL)
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
const premiumURL = "https://katfile.cc/?op=payments"

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }

// CredentialFields implements plugins.CredentialDescriber.
func (p *Plugin) CredentialFields() []plugins.CredentialField {
	return []plugins.CredentialField{
		{Name: "username", Label: "API key 1", Secret: true, Required: true, Help: "both keys from your Katfile account settings"},
		{Name: "password", Label: "API key 2", Secret: true, Required: true},
	}
}
