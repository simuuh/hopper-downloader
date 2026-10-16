// Package premiumize implements a multi-hoster plugin for Premiumize.me
// (https://www.premiumize.me/api): the original hoster URL goes to
// /transfer/directdl, which answers with a direct download URL.
// Authentication with the user's API key (Bearer).
//
// Verified against the official API documentation (October 2026) and
// live, unauthenticated calls: the public /services/list shape
// (directdl service keys plus per-service domain aliases) and the error
// envelope ({"status":"error","message":"Not logged in",
// "code":"authentication_failed"}, HTTP 200). Not verified against a
// real account: the success bodies of /transfer/directdl and
// /account/info are implemented as documented.
package premiumize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/internal/multihost"
)

const (
	pluginName = "premiumize"
	apiBase    = "https://www.premiumize.me/api"

	// Premiumize publishes no numeric limit (it answers
	// rate_limit_reached) — stay conservative.
	requestsPerSecond = 2
	rateBurst         = 4

	hostsCacheTTL  = time.Hour
	requestTimeout = 60 * time.Second
	// Resolve and FetchDownloadURL share one /transfer/directdl call.
	linkCacheTTL = 10 * time.Minute

	premiumURL = "https://www.premiumize.me/premium"
)

// TokenSource resolves the configured Premiumize account for Match.
// *account.Manager satisfies it.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// file is one /transfer/directdl content entry.
type file struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Link string `json:"link"`
}

// Plugin is the Premiumize.me Hoster implementation.
type Plugin struct {
	tokens TokenSource
	client *multihost.Client
	hosts  *multihost.HostCache
	links  *multihost.ResultCache[file]
	log    *slog.Logger

	baseURL string // overridden by tests
}

// New returns a Premiumize plugin. log may be nil.
func New(tokens TokenSource, log *slog.Logger) *Plugin {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Plugin{
		tokens:  tokens,
		client:  multihost.NewClient(pluginName, requestsPerSecond, rateBurst, requestTimeout),
		links:   &multihost.ResultCache[file]{TTL: linkCacheTTL},
		log:     log,
		baseURL: apiBase,
	}
	p.hosts = &multihost.HostCache{TTL: hostsCacheTTL, Fetch: p.fetchHosts}
	return p
}

// Name implements plugins.Hoster.
func (p *Plugin) Name() string { return pluginName }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "Premiumize.me" }

// Credentialed implements plugins.Credentialed.
func (p *Plugin) Credentialed() bool { return true }

// CredentialFields implements plugins.CredentialDescriber.
func (p *Plugin) CredentialFields() []plugins.CredentialField {
	return plugins.TokenCredentialFields("API key", "premiumize.me/account")
}

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }

// Domains implements plugins.Hoster: the domains of the last host list.
func (p *Plugin) Domains() []string { return p.hosts.Domains() }

// Match implements plugins.Hoster: only with a configured account, and
// only for services Premiumize can download directly (directdl).
func (p *Plugin) Match(rawURL string) bool {
	token := p.currentToken()
	host := multihost.HostnameOf(rawURL)
	if token == "" || host == "" {
		return false
	}
	hosts, err := p.hosts.Snapshot(token)
	if err != nil {
		if hosts == nil {
			p.log.Warn("premiumize: host list unavailable, not matching any URL", "error", err)
			return false
		}
		p.log.Warn("premiumize: host list refresh failed, using the previous list", "error", err)
	}
	return hosts.Supports(host)
}

// fetchHosts reads GET /services/list: the directdl service keys are
// domains themselves (e.g. "rapidgator.net"); aliases add the others
// ("rg.to", ...).
func (p *Plugin) fetchHosts(token string) (multihost.Hosts, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var list struct {
		DirectDL []string            `json:"directdl"`
		Aliases  map[string][]string `json:"aliases"`
	}
	if err := p.call(ctx, http.MethodGet, "/services/list", token, nil, &list); err != nil {
		return nil, err
	}
	out := multihost.Hosts{}
	for _, svc := range list.DirectDL {
		out.Add(svc)
		out.Add(list.Aliases[svc]...)
	}
	return out, nil
}

// Resolve implements plugins.Hoster. Premiumize has no separate check for
// hoster links, so this already calls /transfer/directdl and keeps the
// result for FetchDownloadURL.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	f, err := p.directDL(rawURL, acc)
	if err != nil {
		return nil, err
	}
	return &plugins.FileInfo{Name: path.Base(f.Path), Size: f.Size, Available: true}, nil
}

// FetchDownloadURL implements plugins.Hoster via POST /transfer/directdl
// (or the result Resolve just fetched).
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	f, err := p.directDL(rawURL, acc)
	if err != nil {
		return "", err
	}
	return f.Link, nil
}

func (p *Plugin) directDL(rawURL string, acc *plugins.Account) (file, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return file{}, plugins.ErrAccountRequired
	}
	if f, ok := p.links.Get(rawURL); ok {
		return f, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var resp struct {
		Content []file `json:"content"`
	}
	if err := p.call(ctx, http.MethodPost, "/transfer/directdl", token, url.Values{"src": {rawURL}}, &resp); err != nil {
		return file{}, err
	}
	switch {
	case len(resp.Content) == 0:
		return file{}, fmt.Errorf("%s: %w: no file in directdl response", pluginName, plugins.ErrUnavailable)
	case len(resp.Content) > 1:
		return file{}, fmt.Errorf("%s: link is a folder with %d files; add the files individually", pluginName, len(resp.Content))
	case resp.Content[0].Link == "":
		return file{}, fmt.Errorf("%s: %w: no download URL in directdl response", pluginName, plugins.ErrUnavailable)
	}
	p.links.Put(rawURL, resp.Content[0])
	return resp.Content[0], nil
}

// ValidateAccount implements plugins.AccountValidator via GET
// /account/info. A used-up fair-use allowance is reported as no traffic
// left, so the account rotation skips it.
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var ai struct {
		PremiumUntil *int64  `json:"premium_until"`
		LimitUsed    float64 `json:"limit_used"`
	}
	if err := p.call(ctx, http.MethodGet, "/account/info", token, nil, &ai); err != nil {
		if errors.Is(err, plugins.ErrAccountRequired) {
			p.hosts.Clear()
			return &plugins.AccountInfo{Valid: false, Label: "Invalid API key"}, nil
		}
		return nil, err
	}
	info := &plugins.AccountInfo{Valid: true}
	if ai.PremiumUntil == nil || *ai.PremiumUntil == 0 {
		info.Label = "Free account (no premium)"
	} else {
		t := time.Unix(*ai.PremiumUntil, 0).UTC()
		info.ExpiresAt = &t
		info.Label = fmt.Sprintf("Premium until %s, fair use %.0f%% used", t.Format("2006-01-02"), ai.LimitUsed*100)
	}
	if ai.LimitUsed >= 1 {
		zero := int64(0)
		info.TrafficLeft = &zero
	}
	if _, err := p.hosts.Snapshot(token); err != nil {
		p.log.Warn("premiumize: warming the host list failed", "error", err)
	}
	return info, nil
}

// mapError maps a Premiumize error code to a plugins sentinel.
func mapError(code, message string) error {
	var sentinel error
	switch code {
	case "authentication_failed", "permission_denied", "account_limit_reached":
		// account_limit_reached: fair use used up — try another account.
		sentinel = plugins.ErrAccountRequired
	case "service_unsupported", "service_down", "service_limit_reached":
		sentinel = plugins.ErrUnsupportedHost
	case "not_found":
		sentinel = plugins.ErrNotFound
	case "rate_limit_reached":
		sentinel = plugins.ErrRateLimited
	case "link_generation_failed", "transient_error", "semi_permanent_error", "unknown_error":
		sentinel = plugins.ErrUnavailable
	}
	if sentinel == nil {
		return fmt.Errorf("%s: %s (%s)", pluginName, message, code)
	}
	return fmt.Errorf("%s: %w: %s (%s)", pluginName, sentinel, message, code)
}

// call sends one API request and decodes the whole success body into
// out. Premiumize reports business errors with HTTP 200, so the status
// field decides.
func (p *Plugin) call(ctx context.Context, method, apiPath, token string, params url.Values, out any) error {
	resp, err := p.client.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		var body io.Reader
		if len(params) > 0 {
			body = strings.NewReader(params.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, p.baseURL+apiPath, body)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%s: %w", pluginName, plugins.ErrRateLimited)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s: read %s response: %w", pluginName, apiPath, err)
	}
	var env struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%s: %w", pluginName, plugins.ErrAccountRequired)
		}
		return fmt.Errorf("%s: decode %s response (HTTP %d): %w", pluginName, apiPath, resp.StatusCode, err)
	}
	if env.Status != "success" {
		return mapError(env.Code, env.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode %s response: %w", pluginName, apiPath, err)
	}
	return nil
}

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
