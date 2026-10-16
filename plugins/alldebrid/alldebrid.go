// Package alldebrid implements a multi-hoster plugin for AllDebrid
// (https://docs.alldebrid.com/): the original hoster URL is "unlocked"
// into a direct download URL with the user's API key.
//
// Verified against the official API documentation (October 2026) and
// live, unauthenticated calls: the public /v4/hosts shape (hosts keyed by
// name, each with a domains list) and the error envelope
// ({"status":"error","error":{"code":"AUTH_MISSING_APIKEY",...}}).
// Not verified against a real account: the success bodies of
// /v4.1/user/hosts, /v4/link/infos, /v4/link/unlock, /v4/link/delayed and
// /v4/user are implemented as documented.
package alldebrid

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

	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/internal/multihost"
)

const (
	pluginName = "alldebrid"
	apiBase    = "https://api.alldebrid.com"

	// Documented limits: 12 requests/s and 600/min.
	requestsPerSecond = 9
	rateBurst         = 10

	hostsCacheTTL  = time.Hour
	requestTimeout = 30 * time.Second
	// A delayed link is polled at the documented minimum interval until
	// it's ready, for at most delayedMaxWait.
	delayedPollInterval = 5 * time.Second
	delayedMaxWait      = 2 * time.Minute

	premiumURL = "https://alldebrid.com/offer/"
)

// TokenSource resolves the configured AllDebrid account for Match, which
// receives no *Account. *account.Manager satisfies it.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// Plugin is the AllDebrid Hoster implementation.
type Plugin struct {
	tokens TokenSource
	client *multihost.Client
	hosts  *multihost.HostCache
	log    *slog.Logger

	// Overridden by tests.
	baseURL      string
	pollInterval time.Duration
}

// New returns an AllDebrid plugin. log may be nil.
func New(tokens TokenSource, log *slog.Logger) *Plugin {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Plugin{
		tokens:       tokens,
		client:       multihost.NewClient(pluginName, requestsPerSecond, rateBurst, requestTimeout),
		log:          log,
		baseURL:      apiBase,
		pollInterval: delayedPollInterval,
	}
	p.hosts = &multihost.HostCache{TTL: hostsCacheTTL, Fetch: p.fetchHosts}
	return p
}

// Name implements plugins.Hoster.
func (p *Plugin) Name() string { return pluginName }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "AllDebrid" }

// Credentialed implements plugins.Credentialed.
func (p *Plugin) Credentialed() bool { return true }

// CredentialFields implements plugins.CredentialDescriber.
func (p *Plugin) CredentialFields() []plugins.CredentialField {
	return plugins.TokenCredentialFields("API key", "alldebrid.com/apikeys")
}

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }

// Domains implements plugins.Hoster: the domains of the last host list.
func (p *Plugin) Domains() []string { return p.hosts.Domains() }

// Match implements plugins.Hoster: only with a configured account, and
// only for hosts the account can currently use (see fetchHosts).
func (p *Plugin) Match(rawURL string) bool {
	token := p.currentToken()
	host := multihost.HostnameOf(rawURL)
	if token == "" || host == "" {
		return false
	}
	hosts, err := p.hosts.Snapshot(token)
	if err != nil {
		if hosts == nil {
			p.log.Warn("alldebrid: host list unavailable, not matching any URL", "error", err)
			return false
		}
		p.log.Warn("alldebrid: host list refresh failed, using the previous list", "error", err)
	}
	return hosts.Supports(host)
}

// fetchHosts reads the account's own host list (GET /v4.1/user/hosts).
// Hosts AllDebrid currently reports as not working, or whose per-host
// quota is used up, are left out, so Match lets another plugin take
// those links up front.
func (p *Plugin) fetchHosts(token string) (multihost.Hosts, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var data struct {
		Hosts map[string]struct {
			Domains  []string `json:"domains"`
			Status   *bool    `json:"status"`
			Quota    *float64 `json:"quota"`
			QuotaMax *float64 `json:"quotaMax"`
		} `json:"hosts"`
	}
	if err := p.call(ctx, http.MethodGet, "/v4.1/user/hosts", token, nil, &data); err != nil {
		return nil, err
	}
	out := multihost.Hosts{}
	for _, h := range data.Hosts {
		if h.Status != nil && !*h.Status {
			continue
		}
		if h.Quota != nil && h.QuotaMax != nil && *h.QuotaMax > 0 && *h.Quota <= 0 {
			continue
		}
		out.Add(h.Domains...)
	}
	return out, nil
}

// Resolve implements plugins.Hoster via POST /v4/link/infos.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var data struct {
		Infos []struct {
			Filename string    `json:"filename"`
			Size     int64     `json:"size"`
			Error    *apiError `json:"error"`
		} `json:"infos"`
	}
	if err := p.call(ctx, http.MethodPost, "/v4/link/infos", token, url.Values{"link[]": {rawURL}}, &data); err != nil {
		return nil, err
	}
	if len(data.Infos) == 0 {
		return nil, fmt.Errorf("%s: link/infos returned no entry", pluginName)
	}
	info := data.Infos[0]
	if info.Error != nil {
		return nil, info.Error.err()
	}
	return &plugins.FileInfo{Name: info.Filename, Size: info.Size, Available: true}, nil
}

// FetchDownloadURL implements plugins.Hoster via POST /v4/link/unlock. A
// "delayed" answer (the link is still being generated) is polled via
// /v4/link/delayed for up to delayedMaxWait.
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return "", plugins.ErrAccountRequired
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout+delayedMaxWait)
	defer cancel()
	var data struct {
		Link    string `json:"link"`
		Delayed int64  `json:"delayed"`
	}
	if err := p.call(ctx, http.MethodPost, "/v4/link/unlock", token, url.Values{"link": {rawURL}}, &data); err != nil {
		return "", err
	}
	if data.Link != "" {
		return data.Link, nil
	}
	if data.Delayed == 0 {
		return "", fmt.Errorf("%s: %w: no download link in unlock response", pluginName, plugins.ErrUnavailable)
	}
	return p.waitDelayed(ctx, token, data.Delayed)
}

func (p *Plugin) waitDelayed(ctx context.Context, token string, id int64) (string, error) {
	deadline := time.Now().Add(delayedMaxWait)
	for {
		var d struct {
			Status int    `json:"status"`
			Link   string `json:"link"`
		}
		if err := p.call(ctx, http.MethodPost, "/v4/link/delayed", token, url.Values{"id": {strconv.FormatInt(id, 10)}}, &d); err != nil {
			return "", err
		}
		switch d.Status {
		case 2:
			if d.Link != "" {
				return d.Link, nil
			}
		case 3:
			return "", fmt.Errorf("%s: %w: generating the download link failed", pluginName, plugins.ErrUnsupportedHost)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s: %w: download link still not ready after %s", pluginName, plugins.ErrUnavailable, delayedMaxWait)
		}
		select {
		case <-time.After(p.pollInterval):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// ValidateAccount implements plugins.AccountValidator via GET /v4/user.
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var data struct {
		User struct {
			IsPremium    bool  `json:"isPremium"`
			IsTrial      bool  `json:"isTrial"`
			PremiumUntil int64 `json:"premiumUntil"`
		} `json:"user"`
	}
	if err := p.call(ctx, http.MethodGet, "/v4/user", token, nil, &data); err != nil {
		if errors.Is(err, plugins.ErrAccountRequired) {
			p.hosts.Clear()
			return &plugins.AccountInfo{Valid: false, Label: "Invalid API key"}, nil
		}
		return nil, err
	}
	u := data.User
	info := &plugins.AccountInfo{Valid: true}
	switch {
	case u.IsPremium && u.PremiumUntil > 0:
		t := time.Unix(u.PremiumUntil, 0).UTC()
		info.ExpiresAt = &t
		info.Label = "Premium until " + t.Format("2006-01-02")
	case u.IsPremium:
		info.Label = "Premium"
	case u.IsTrial:
		info.Label = "Trial"
	default:
		info.Label = "Free account (no premium)"
	}
	if _, err := p.hosts.Snapshot(token); err != nil {
		p.log.Warn("alldebrid: warming the host list failed", "error", err)
	}
	return info, nil
}

// apiError is AllDebrid's error object.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// err maps an AllDebrid error code to a plugins sentinel.
func (e *apiError) err() error {
	var sentinel error
	switch e.Code {
	case "AUTH_MISSING_APIKEY", "AUTH_BAD_APIKEY", "AUTH_BLOCKED", "AUTH_USER_BANNED",
		"ACCOUNT_INVALID", "MUST_BE_PREMIUM", "FREE_TRIAL_LIMIT_REACHED":
		sentinel = plugins.ErrAccountRequired
	case "LINK_HOST_NOT_SUPPORTED", "LINK_NOT_SUPPORTED", "LINK_HOST_UNAVAILABLE", "LINK_HOST_FULL",
		"LINK_HOST_LIMIT_REACHED", "LINK_TOO_MANY_DOWNLOADS", "LINK_ERROR", "NO_SERVER":
		// The link may well work through another plugin. NO_SERVER: the
		// request came from a server/VPN address, which AllDebrid refuses.
		sentinel = plugins.ErrUnsupportedHost
	case "LINK_DOWN":
		sentinel = plugins.ErrNotFound
	case "LINK_TEMPORARY_UNAVAILABLE", "MAINTENANCE", "GENERIC":
		sentinel = plugins.ErrUnavailable
	}
	if sentinel == nil {
		return fmt.Errorf("%s: %s: %s", pluginName, e.Code, e.Message)
	}
	return fmt.Errorf("%s: %w: %s (%s)", pluginName, sentinel, e.Message, e.Code)
}

// call sends one API request and decodes the "data" member of a success
// envelope into out (if non-nil). GET parameters go into the query, POST
// parameters into a form body.
func (p *Plugin) call(ctx context.Context, method, path, token string, params url.Values, out any) error {
	resp, err := p.client.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		u := p.baseURL + path
		var body io.Reader
		if method == http.MethodGet && len(params) > 0 {
			u += "?" + params.Encode()
		} else if len(params) > 0 {
			body = strings.NewReader(params.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, u, body)
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

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("%s: %w (HTTP %d)", pluginName, plugins.ErrRateLimited, resp.StatusCode)
	}
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  *apiError       `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%s: %w", pluginName, plugins.ErrAccountRequired)
		}
		return fmt.Errorf("%s: decode %s response (HTTP %d): %w", pluginName, path, resp.StatusCode, err)
	}
	if env.Status != "success" {
		if env.Error != nil {
			return env.Error.err()
		}
		return fmt.Errorf("%s: %s failed (HTTP %d)", pluginName, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("%s: decode %s data: %w", pluginName, path, err)
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
