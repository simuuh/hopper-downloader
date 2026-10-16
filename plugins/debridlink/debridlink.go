// Package debridlink implements a multi-hoster plugin for Debrid-Link
// (https://debrid-link.com/api_doc/v2/introduction): the original hoster
// URL is added to the account's downloader and comes back with a direct
// download URL. Authentication with the user's private API key (Bearer).
//
// Verified against the official API documentation (October 2026) and
// live, unauthenticated calls: the public /downloader/hosts list and the
// error envelope ({"success":false,"error":"badToken",...} with HTTP
// 401). Not verified against a real account: the success bodies of
// /downloader/add and /account/infos are implemented as documented.
package debridlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/internal/multihost"
)

const (
	pluginName = "debridlink"
	apiBase    = "https://debrid-link.com/api/v2"

	// Debrid-Link publishes no numeric limit, only "floodDetected" (block
	// for ~1h) when exceeded — stay conservative.
	requestsPerSecond = 2
	rateBurst         = 4

	hostsCacheTTL  = time.Hour
	requestTimeout = 30 * time.Second
	// Resolve and FetchDownloadURL share one /downloader/add call per link
	// (each call adds the link to the account's list again).
	linkCacheTTL = 10 * time.Minute

	premiumURL = "https://debrid-link.com/premium"
)

// TokenSource resolves the configured Debrid-Link account for Match.
// *account.Manager satisfies it.
type TokenSource interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// link is one /downloader/add result.
type link struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	DownloadURL  string `json:"downloadUrl"`
	IsProcessing bool   `json:"isProcessing"`
}

// Plugin is the Debrid-Link Hoster implementation.
type Plugin struct {
	tokens TokenSource
	client *multihost.Client
	hosts  *multihost.HostCache
	links  *multihost.ResultCache[link]
	log    *slog.Logger

	baseURL string // overridden by tests
}

// New returns a Debrid-Link plugin. log may be nil.
func New(tokens TokenSource, log *slog.Logger) *Plugin {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Plugin{
		tokens:  tokens,
		client:  multihost.NewClient(pluginName, requestsPerSecond, rateBurst, requestTimeout),
		links:   &multihost.ResultCache[link]{TTL: linkCacheTTL},
		log:     log,
		baseURL: apiBase,
	}
	p.hosts = &multihost.HostCache{TTL: hostsCacheTTL, Fetch: p.fetchHosts}
	return p
}

// Name implements plugins.Hoster.
func (p *Plugin) Name() string { return pluginName }

// DisplayName implements plugins.DisplayNamer.
func (p *Plugin) DisplayName() string { return "Debrid-Link" }

// Credentialed implements plugins.Credentialed.
func (p *Plugin) Credentialed() bool { return true }

// CredentialFields implements plugins.CredentialDescriber.
func (p *Plugin) CredentialFields() []plugins.CredentialField {
	return plugins.TokenCredentialFields("API key", "debrid-link.com/webapp/apikey")
}

// PremiumURL implements plugins.PremiumLinker.
func (p *Plugin) PremiumURL() string { return premiumURL }

// ReferralURL implements plugins.PremiumLinker.
func (p *Plugin) ReferralURL() string { return plugins.ReferralLinkFor(pluginName) }

// Domains implements plugins.Hoster: the domains of the last host list.
func (p *Plugin) Domains() []string { return p.hosts.Domains() }

// Match implements plugins.Hoster: only with a configured account, and
// only for hosts Debrid-Link currently reports as online.
func (p *Plugin) Match(rawURL string) bool {
	token := p.currentToken()
	host := multihost.HostnameOf(rawURL)
	if token == "" || host == "" {
		return false
	}
	hosts, err := p.hosts.Snapshot(token)
	if err != nil {
		if hosts == nil {
			p.log.Warn("debridlink: host list unavailable, not matching any URL", "error", err)
			return false
		}
		p.log.Warn("debridlink: host list refresh failed, using the previous list", "error", err)
	}
	return hosts.Supports(host)
}

// fetchHosts reads GET /downloader/hosts; a host counts when it's a file
// host (not a stream site or link protector) and online (status >= 1).
func (p *Plugin) fetchHosts(token string) (multihost.Hosts, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var hosts []struct {
		Type    string   `json:"type"`
		Status  int      `json:"status"`
		Domains []string `json:"domains"`
	}
	if err := p.call(ctx, http.MethodGet, "/downloader/hosts", token, nil, &hosts); err != nil {
		return nil, err
	}
	out := multihost.Hosts{}
	for _, h := range hosts {
		if h.Type == "host" && h.Status >= 1 {
			out.Add(h.Domains...)
		}
	}
	return out, nil
}

// Resolve implements plugins.Hoster. Debrid-Link has no separate check
// endpoint, so this already adds the link (/downloader/add) and keeps the
// result for FetchDownloadURL.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	l, err := p.add(rawURL, acc)
	if err != nil {
		return nil, err
	}
	return &plugins.FileInfo{Name: l.Name, Size: l.Size, Available: true}, nil
}

// FetchDownloadURL implements plugins.Hoster via POST /downloader/add (or
// the result Resolve just fetched).
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	l, err := p.add(rawURL, acc)
	if err != nil {
		return "", err
	}
	if l.IsProcessing {
		// Debrid-Link is still preparing the file on its servers (rare);
		// the queue's automatic retry asks again later.
		return "", fmt.Errorf("%s: %w: file is still being processed by Debrid-Link", pluginName, plugins.ErrUnavailable)
	}
	if l.DownloadURL == "" {
		return "", fmt.Errorf("%s: %w: no download URL in response", pluginName, plugins.ErrUnavailable)
	}
	return l.DownloadURL, nil
}

func (p *Plugin) add(rawURL string, acc *plugins.Account) (link, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return link{}, plugins.ErrAccountRequired
	}
	if l, ok := p.links.Get(rawURL); ok {
		return l, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var raw json.RawMessage
	if err := p.call(ctx, http.MethodPost, "/downloader/add", token, map[string]string{"url": rawURL}, &raw); err != nil {
		return link{}, err
	}
	// "value" is a link object, or an array of them for a folder URL.
	var l link
	if len(raw) > 0 && raw[0] == '[' {
		var many []link
		if err := json.Unmarshal(raw, &many); err != nil {
			return link{}, fmt.Errorf("%s: decode downloader/add: %w", pluginName, err)
		}
		if len(many) != 1 {
			return link{}, fmt.Errorf("%s: link is a folder with %d files; add the files individually", pluginName, len(many))
		}
		l = many[0]
	} else if err := json.Unmarshal(raw, &l); err != nil {
		return link{}, fmt.Errorf("%s: decode downloader/add: %w", pluginName, err)
	}
	if !l.IsProcessing && l.DownloadURL != "" {
		p.links.Put(rawURL, l)
	}
	return l, nil
}

// ValidateAccount implements plugins.AccountValidator via GET
// /account/infos.
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var infos struct {
		AccountType int   `json:"accountType"` // 0 free, 1 premium, 2 lifetime
		PremiumLeft int64 `json:"premiumLeft"` // seconds
	}
	if err := p.call(ctx, http.MethodGet, "/account/infos", token, nil, &infos); err != nil {
		if errors.Is(err, plugins.ErrAccountRequired) {
			p.hosts.Clear()
			return &plugins.AccountInfo{Valid: false, Label: "Invalid API key"}, nil
		}
		return nil, err
	}
	info := &plugins.AccountInfo{Valid: true}
	switch infos.AccountType {
	case 2:
		info.Label = "Lifetime premium"
	case 1:
		info.Label = "Premium"
		if infos.PremiumLeft > 0 {
			t := time.Now().Add(time.Duration(infos.PremiumLeft) * time.Second).UTC()
			info.ExpiresAt = &t
			info.Label = "Premium until " + t.Format("2006-01-02")
		}
	default:
		info.Label = "Free account (no premium)"
	}
	if _, err := p.hosts.Snapshot(token); err != nil {
		p.log.Warn("debridlink: warming the host list failed", "error", err)
	}
	return info, nil
}

// mapError maps a Debrid-Link error code to a plugins sentinel.
func mapError(code string, status int) error {
	var sentinel error
	switch code {
	case "badToken", "unverifiedEmail", "notFreeHost", "maxLink", "maxData":
		// maxLink/maxData: the account's daily limit — try another account.
		sentinel = plugins.ErrAccountRequired
	case "hostNotValid", "maintenanceHost", "notDebrid", "maxLinkHost", "maxDataHost":
		sentinel = plugins.ErrUnsupportedHost
	case "fileNotFound":
		sentinel = plugins.ErrNotFound
	case "fileNotAvailable", "internalError", "freeServerOverload":
		sentinel = plugins.ErrUnavailable
	case "floodDetected":
		sentinel = plugins.ErrRateLimited
	case "":
		if status == http.StatusUnauthorized {
			sentinel = plugins.ErrAccountRequired
		}
	}
	if sentinel == nil {
		return fmt.Errorf("%s: error %q (HTTP %d)", pluginName, code, status)
	}
	return fmt.Errorf("%s: %w (%s)", pluginName, sentinel, code)
}

// call sends one API request (body as JSON, if any) and decodes the
// "value" member of a success envelope into out.
func (p *Plugin) call(ctx context.Context, method, path, token string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("%s: encode request: %w", pluginName, err)
		}
	}
	resp, err := p.client.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		var r io.Reader
		if payload != nil {
			r = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, r)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
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
	var env struct {
		Success bool            `json:"success"`
		Value   json.RawMessage `json:"value"`
		Error   string          `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		if resp.StatusCode >= 400 {
			return mapError("", resp.StatusCode)
		}
		return fmt.Errorf("%s: decode %s response: %w", pluginName, path, err)
	}
	if !env.Success {
		return mapError(env.Error, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Value, out); err != nil {
		return fmt.Errorf("%s: decode %s value: %w", pluginName, path, err)
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
