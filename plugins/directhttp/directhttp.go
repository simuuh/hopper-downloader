// Package directhttp implements the reference Hoster plugin: plain
// HTTP/HTTPS URLs with no hoster-specific auth or token exchange. It also
// serves as the catch-all fallback plugin and the template new plugin
// authors should copy (see docs/PLUGIN_GUIDE.md).
//
// SSRF note: this plugin (like every Hoster plugin's Resolve/
// FetchDownloadURL, and downloader.Engine's actual transfer afterward)
// fetches whatever URL it's given with no allowlist/denylist on
// scheme-valid hosts — no block on 127.0.0.0/8, RFC1918 ranges, link-local
// (169.254.0.0/16, including cloud metadata endpoints), etc. This is
// intentional, not an oversight: the request that reaches here already
// passed api's JWT auth (POST /api/v1/links, see
// api/queue_handlers.go's own SSRF note), and Hopper's threat model
// treats an authenticated caller as trusted to name any URL, the same way
// curl/wget would — restricting it would break legitimate self-hosted/
// homelab use (an internal NAS/Nextcloud/etc. URL is a normal target for
// this audience, not an attack). It is not a gate a browser Click'n'Load
// button or unauthenticated request can reach: internal/clicknload has
// its own, separate trusted_cidrs gate for that surface.
package directhttp

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

const pluginName = "directhttp"

// Plugin is the direct HTTP/HTTPS Hoster implementation.
type Plugin struct {
	client *http.Client
}

// New returns a directhttp Plugin using a client with a sane request
// timeout for metadata lookups. It performs no I/O itself.
func New() *Plugin {
	return &Plugin{
		client: &http.Client{
			Timeout: 30 * time.Second,
			// Never auto-follow to a different scheme/host silently past
			// what net/http already restricts; explicit is fine here.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("%s: too many redirects", pluginName)
				}
				return nil
			},
		},
	}
}

// Name implements plugins.Hoster.
func (p *Plugin) Name() string { return pluginName }

// Domains implements plugins.Hoster. directhttp is a catch-all and does
// not claim specific domains.
func (p *Plugin) Domains() []string { return nil }

// Match implements plugins.Hoster, accepting any well-formed http(s) URL.
func (p *Plugin) Match(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// Resolve implements plugins.Hoster using a HEAD request to read Content-
// Length and Content-Disposition without transferring the file body. acc
// is ignored: direct HTTP has no account concept.
func (p *Plugin) Resolve(rawURL string, acc *plugins.Account) (*plugins.FileInfo, error) {
	req, err := http.NewRequest(http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", pluginName, err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %v", pluginName, plugins.ErrUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, plugins.ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, plugins.ErrRateLimited
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%s: %w: HTTP %d", pluginName, plugins.ErrUnavailable, resp.StatusCode)
	}

	return &plugins.FileInfo{
		Name:      filenameFor(rawURL, resp.Header.Get("Content-Disposition")),
		Size:      contentLength(resp),
		Available: true,
	}, nil
}

// FetchDownloadURL implements plugins.Hoster. Direct HTTP links need no
// token exchange, so the input URL is returned unchanged.
func (p *Plugin) FetchDownloadURL(rawURL string, acc *plugins.Account) (string, error) {
	return rawURL, nil
}

func contentLength(resp *http.Response) int64 {
	if resp.ContentLength >= 0 {
		return resp.ContentLength
	}
	if v := resp.Header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return -1
}

// filenameFor derives a display filename from a Content-Disposition header
// if present, falling back to the URL's final path segment (see
// plugins.FilenameFromURL — stripping a trailing ".html" there matters
// here too: directhttp is the catch-all a hoster page URL falls through
// to whenever no hoster-specific/account-aware plugin claims it, e.g. a
// rapidgator.net link with no Rapidgator or Real-Debrid account
// configured, so its own URL-derived fallback is just as likely to hit
// a "<hash>/<name>.html" info-page URL as any other plugin's). It
// performs no filesystem-safety sanitization — that is the downloader's
// responsibility immediately before the name is used to build a
// destination path.
func filenameFor(rawURL, contentDisposition string) string {
	if contentDisposition != "" {
		if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
			if name := strings.TrimSpace(params["filename"]); name != "" {
				return name
			}
		}
	}
	if name := plugins.FilenameFromURL(rawURL); name != "" {
		return name
	}
	return "download"
}
