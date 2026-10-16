// Package onefichier implements a Hoster plugin for 1fichier.com
// (https://1fichier.com/api.html): resolving and downloading via its
// REST API using an account's API key, rather than scraping the file
// page.
//
// The base URL, auth convention (POST JSON, "Authorization: Bearer
// <key>", https://api.1fichier.com/v1), and the endpoint paths and
// response fields used by Resolve/FetchDownloadURL below
// (/file/info.cgi -> filename/size, /download/get_token.cgi ->
// status/url/message) are all cross-checked against 1fichier's own
// community-maintained API client libraries (there's no first-party Go
// client to check against, unlike Real-Debrid's rclone backend, and
// 1fichier.com/api.html itself wasn't fetchable from this environment).
// ValidateAccount's premium-expiry field is a best-effort guess at that
// same level of confidence — see its doc comment.
package onefichier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

const (
	pluginName     = "onefichier"
	apiBase        = "https://api.1fichier.com/v1"
	requestTimeout = 30 * time.Second
)

// TokenSource resolves the current 1fichier API key from the configured
// hoster account. *account.Manager satisfies this implicitly (it
// already implements the identical method for queue.AccountLookup), so
// no import of internal/account is needed here.
//
// Needed for the same reason as realdebrid.TokenSource: Match takes no
// *Account parameter, so this is the only way to learn whether an
// account is configured before deciding whether to claim a URL.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// Plugin is the 1fichier Hoster implementation.
type Plugin struct {
	tokens TokenSource
	client *http.Client
	log    *slog.Logger

	// baseURL is overridden by tests; production code always uses
	// apiBase.
	baseURL string
}

// New returns a 1fichier Plugin. tokens is typically the same
// *account.Manager passed to the queue manager. log defaults to a
// discard logger if nil.
func New(tokens TokenSource, log *slog.Logger) *Plugin {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Plugin{
		tokens:  tokens,
		client:  &http.Client{Timeout: requestTimeout},
		log:     log,
		baseURL: apiBase,
	}
}

// Name implements plugins.Hoster. Also the hoster key used for the
// 1fichier account in hopper.yaml's per-hoster config and in
// internal/account.
func (p *Plugin) Name() string { return pluginName }

// Domains implements plugins.Hoster.
func (p *Plugin) Domains() []string { return []string{"1fichier.com"} }

// Credentialed implements plugins.Credentialed: 1fichier is unusable
// here without an api_key (this plugin is specifically the
// authenticated-API path; it doesn't fall back to anonymous/scraped
// access).
func (p *Plugin) Credentialed() bool { return true }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "1fichier" }

// Match implements plugins.Hoster. It claims a 1fichier.com URL only if
// an account (api_key) is configured, same convention as Real-Debrid: an
// unconfigured plugin behaves as if it weren't registered at all, and
// the URL falls through to whatever's registered before it.
func (p *Plugin) Match(rawURL string) bool {
	if p.currentToken() == "" {
		return false
	}
	host := hostnameOf(rawURL)
	return host == "1fichier.com" || strings.HasSuffix(host, ".1fichier.com")
}

// Resolve implements plugins.Hoster via POST /file/info.cgi.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, "/file/info.cgi", token, map[string]any{"url": rawURL})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := statusErr(resp.StatusCode); err != nil {
		return nil, err
	}

	var body struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
		Status   string `json:"status"`
		Message  string `json:"message"`
	}
	if err := decodeJSON(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("%s: decode file/info response: %w", pluginName, err)
	}
	if body.Status == "KO" {
		return nil, fmt.Errorf("%s: %s: %w", pluginName, body.Message, plugins.ErrNotFound)
	}
	if body.Filename == "" {
		return nil, fmt.Errorf("%s: %w", pluginName, plugins.ErrNotFound)
	}

	return &plugins.FileInfo{Name: body.Filename, Size: body.Size, Available: true}, nil
}

// FetchDownloadURL implements plugins.Hoster via POST
// /download/get_token.cgi.
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return "", plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, "/download/get_token.cgi", token, map[string]any{"url": rawURL})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := statusErr(resp.StatusCode); err != nil {
		return "", err
	}

	var body struct {
		Status  string `json:"status"`
		URL     string `json:"url"`
		Message string `json:"message"`
	}
	if err := decodeJSON(resp.Body, &body); err != nil {
		return "", fmt.Errorf("%s: decode download/get_token response: %w", pluginName, err)
	}
	if body.Status == "KO" {
		return "", fmt.Errorf("%s: %s: %w", pluginName, body.Message, plugins.ErrUnavailable)
	}
	if body.URL == "" {
		return "", fmt.Errorf("%s: %w: empty download URL in response", pluginName, plugins.ErrUnavailable)
	}
	return body.URL, nil
}

// freeDownloadTokenResponse extends the plain {"status":..., "url":...,
// "message":...} shape FetchDownloadURL decodes with a "delay" field:
// 1fichier's free-tier flow can ask the client to wait before the token
// becomes valid (unverified against a live free account). No
// captcha-image field is known for 1fichier's free flow, so a message that
// mentions a captcha is surfaced as a plain error here rather than a
// plugins.ErrCaptchaRequired carrying a fabricated/empty ImageURL — a
// broken challenge image would be worse than no captcha support at all.
// Revisit if 1fichier's real free-tier response is ever confirmed to
// include one.
type freeDownloadTokenResponse struct {
	Status  string `json:"status"`
	URL     string `json:"url"`
	Message string `json:"message"`
	Delay   int    `json:"delay"`
}

// FetchFreeDownloadURL implements plugins.FreeDownloader for a configured
// 1fichier account on the free tier: the same
// POST /download/get_token.cgi endpoint FetchDownloadURL already calls,
// interpreted for a "delay" (wait) response in addition to the existing
// status/url/message handling. An anonymous, no-account request
// (api_key="") is deliberately out of scope: Match still requires a
// configured account regardless of tier (see its own doc comment).
func (p *Plugin) FetchFreeDownloadURL(ctx context.Context, rawURL string, acc *plugins.Account) (string, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return "", plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, "/download/get_token.cgi", token, map[string]any{"url": rawURL})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := statusErr(resp.StatusCode); err != nil {
		return "", err
	}

	var body freeDownloadTokenResponse
	if err := decodeJSON(resp.Body, &body); err != nil {
		return "", fmt.Errorf("%s: decode free download/get_token response: %w", pluginName, err)
	}

	switch {
	case body.URL != "":
		return body.URL, nil
	case body.Delay > 0:
		return "", plugins.ErrRetryAfter{Seconds: body.Delay}
	case body.Message != "":
		return "", fmt.Errorf("%s: %s", pluginName, body.Message)
	default:
		return "", fmt.Errorf("%s: %w: empty response from free download endpoint", pluginName, plugins.ErrUnavailable)
	}
}

// statusErr maps a 1fichier HTTP status code to a plugins sentinel error
// where one applies. 1fichier reports most application-level failures
// (bad URL, file removed) via a 200 response with "status":"KO" (handled
// by each caller above, since the message text is useful), not via HTTP
// status — this only covers transport/auth-level failures.
func statusErr(code int) error {
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return plugins.ErrAccountRequired
	case http.StatusTooManyRequests:
		return plugins.ErrRateLimited
	case http.StatusServiceUnavailable:
		return plugins.ErrUnavailable
	default:
		return fmt.Errorf("%s: unexpected HTTP status %d", pluginName, code)
	}
}

// tokenFrom prefers the account the queue layer already resolved and
// passed in; it falls back to resolving one directly only if acc is nil.
func (p *Plugin) tokenFrom(acc *plugins.Account) string {
	if acc != nil {
		if t := plugins.TokenFromAccount(acc, "api_key"); t != "" {
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
	return plugins.TokenFromAccount(acc, "api_key")
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

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(v)
}

// premiumURL is the hoster's own page for buying/extending premium. The
// developer referral link lives in plugins/referral.go.
const premiumURL = "https://1fichier.com/tarifs.html"

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }

// CredentialFields implements plugins.CredentialDescriber.
func (p *Plugin) CredentialFields() []plugins.CredentialField {
	return plugins.TokenCredentialFields("API key", "from your 1fichier account settings")
}
