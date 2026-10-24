// Package hidecx implements a Hoster plugin for hide.cx
// (https://hide.cx/container/<uuid>, or the shorter https://hide.cx/
// <uuid>), a link-protection service that sits in front of the actual
// file-hoster links.
//
// hide.cx never hosts a file itself: a container page is a thin wrapper
// around a JSON API — GET https://api.hide.cx/containers/<uuid> — whose
// `links` describe the real hoster(s) underneath (rapidgator.net, etc.).
// This plugin queries that API and then hands the real URL to whichever
// other registered Hoster plugin actually claims it — including
// Real-Debrid, if an account is configured — via the injected Resolver,
// rather than trying to speak every hoster's protocol itself.
//
// The API isn't publicly documented; the request shape (headers,
// endpoints, response fields) below is confirmed against live responses,
// not a spec — see docs/ARCHITECTURE.md for what was actually observed. Notably,
// GET /containers/<uuid> reports each link's `hoster_url` as null; the
// real URL needs a second call, GET
// /containers/<uuid>/links/<link_id>, which returns it as {"url": ...}.
//
// A container can hold links for a multi-part archive (several
// `.partNN.rar` entries, all under one container UUID, variable count —
// see docs/ARCHITECTURE.md). plugins.Hoster's Resolve/FetchDownloadURL contract is
// one URL in, one URL out, so those are only ever used as a single-link
// fallback (see pickLink); the real multi-part path is Expand (see
// plugins.Expander), which returns every online link so queue.Manager
// can add them all as one package.
package hidecx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

const (
	pluginName = "hidecx"
	apiBase    = "https://api.hide.cx"

	httpTimeout = 30 * time.Second

	// userAgent is required by the API in practice (see docs/ARCHITECTURE.md);
	// hide.cx has no first-party client library to match a version
	// string against, so a generic recent-Chrome UA is used.
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// uuidPattern matches hide.cx's container UUID (standard RFC 4122
// textual form), used to pull it out of either URL shape hide.cx
// accepts: https://hide.cx/container/<uuid> and https://hide.cx/<uuid>.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Resolver looks up the best Hoster plugin for rawURL — the same
// account-aware, two-pass priority queue.Manager applies internally
// (queue.MatchHoster) — plus the Account already resolved for it, if
// any. Both are needed together: the `acc` hidecx.Resolve/
// FetchDownloadURL themselves receive is always nil (hidecx is
// anonymous, see Credentialed), and is for hidecx's own hoster key
// regardless, never the underlying hoster's, so it cannot simply be
// forwarded to whatever plugin the container resolves to.
//
// Injected as a closure from cmd/hopperd/main.go, which is the one place
// that has both the fully-populated plugins.Registry (hidecx is itself
// one of its members — see docs/ARCHITECTURE.md's note on this exact circular-
// construction pattern, also used for the settings/queue dependency) and
// the account manager. hidecx depends on neither type directly, only on
// this function, which keeps it trivially testable with a fake.
type Resolver func(rawURL string) (hoster plugins.Hoster, acc *plugins.Account, ok bool)

// Plugin is the hide.cx Hoster implementation.
type Plugin struct {
	client  *http.Client
	resolve Resolver
	log     *slog.Logger

	// apiBase is overridden by tests; production code always uses the
	// real api.hide.cx.
	apiBase string
}

// New returns a hidecx Plugin. resolve is typically a closure over
// cmd/hopperd/main.go's plugins.Registry and account.Manager (see the
// Resolver doc comment). log defaults to a discard logger if nil.
func New(resolve Resolver, log *slog.Logger) *Plugin {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Plugin{
		client:  &http.Client{Timeout: httpTimeout},
		resolve: resolve,
		log:     log,
		apiBase: apiBase,
	}
}

// Name implements plugins.Hoster.
func (p *Plugin) Name() string { return pluginName }

// Domains implements plugins.Hoster.
func (p *Plugin) Domains() []string { return []string{"hide.cx"} }

// Credentialed implements plugins.Credentialed: hide.cx is a public
// redirector with no login of its own, so it's excluded from the Web
// UI's "add account" dropdown (see api's GET /api/v1/plugins).
func (p *Plugin) Credentialed() bool { return false }

// Match implements plugins.Hoster, claiming hide.cx and any of its
// subdomains.
func (p *Plugin) Match(rawURL string) bool {
	return isHideCx(rawURL)
}

func isHideCx(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "hide.cx" || strings.HasSuffix(host, ".hide.cx")
}

// containerUUID extracts the container UUID from a hide.cx page URL,
// which hide.cx accepts in two forms: .../container/<uuid> and the
// shorter .../<uuid>.
func containerUUID(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%s: parse URL: %w", pluginName, err)
	}

	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	var candidate string
	switch {
	case len(segments) == 2 && segments[0] == "container":
		candidate = segments[1]
	case len(segments) == 1:
		candidate = segments[0]
	}

	if !uuidPattern.MatchString(candidate) {
		return "", fmt.Errorf("%s: could not find a container UUID in %q", pluginName, rawURL)
	}
	return candidate, nil
}

// containerResponse is hide.cx's GET /containers/<uuid> response shape.
// Only the fields this plugin actually uses are declared.
type containerResponse struct {
	AccessStatus  string `json:"access_status"`
	PrimaryHoster string `json:"primary_hoster"`
	// Password is the archive password for every link in this
	// container, or null if none — confirmed against a live response
	// (see docs/ARCHITECTURE.md).
	Password *string         `json:"password"`
	Links    []containerLink `json:"links"`
}

// containerLink is one entry of containerResponse.Links. HosterURL is
// observed to always be null in the container listing itself — see
// resolveLinkURL — but is read here too in case that's not universally
// true, saving a second call when it is already present.
type containerLink struct {
	ID         string  `json:"id"`
	Host       string  `json:"host"`
	Name       string  `json:"name"`
	LinkStatus string  `json:"link_status"`
	LinkSize   int64   `json:"link_size"`
	HosterURL  *string `json:"hoster_url"`
}

// linkResolveResponse is hide.cx's GET
// /containers/<uuid>/links/<link_id> response shape.
type linkResolveResponse struct {
	URL string `json:"url"`
}

// Resolve implements plugins.Hoster: resolve the container, then
// delegate to whatever plugin actually claims the real hoster URL
// underneath. acc is ignored — hide.cx needs no account of its own, and
// the account that matters is resolved (for the *delegate* plugin) via
// Resolver, not this parameter.
func (p *Plugin) Resolve(rawURL string, _ *plugins.Account) (*plugins.FileInfo, error) {
	finalURL, hoster, acc, err := p.resolveTarget(rawURL)
	if err != nil {
		return nil, err
	}
	return hoster.Resolve(finalURL, acc)
}

// FetchDownloadURL implements plugins.Hoster: resolve the container as
// Resolve does, then let the matched hoster plugin (Real-Debrid or a
// native hoster plugin) exchange the real URL for a direct one.
func (p *Plugin) FetchDownloadURL(rawURL string, _ *plugins.Account) (string, error) {
	finalURL, hoster, acc, err := p.resolveTarget(rawURL)
	if err != nil {
		return "", err
	}
	return hoster.FetchDownloadURL(finalURL, acc)
}

// Expand implements plugins.Expander: unlike Resolve/FetchDownloadURL,
// which can only act on a single link (see pickLink), Expand returns
// every online link in the container — the correct handling for a
// multi-part archive, which real containers can and do hold (a
// live-fetched sample had 6 `.partN.rar` links; the count is otherwise
// unbounded). Each link still needs its own resolveLinkURL call — the
// container listing doesn't return real URLs in bulk — so Expand costs
// one request plus one more per online link.
func (p *Plugin) Expand(rawURL string) ([]plugins.ExpandedLink, error) {
	uuid, err := containerUUID(rawURL)
	if err != nil {
		return nil, err
	}

	container, err := p.fetchContainer(uuid)
	if err != nil {
		return nil, err
	}

	password := ""
	if container.Password != nil {
		password = *container.Password
	}

	links := make([]plugins.ExpandedLink, 0, len(container.Links))
	for _, l := range container.Links {
		if l.LinkStatus != "online" {
			continue
		}
		hosterURL, err := p.resolveLinkURL(uuid, l)
		if err != nil {
			return nil, fmt.Errorf("%s: resolve link %s: %w", pluginName, l.ID, err)
		}
		links = append(links, plugins.ExpandedLink{
			URL:      hosterURL,
			Name:     expandedLinkName(l.Name, hosterURL),
			Size:     l.LinkSize,
			Password: password,
		})
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("%s: %w: no online links in container", pluginName, plugins.ErrUnavailable)
	}

	sortLinksByPart(links)
	p.log.Info("hidecx: expanded container", "uuid", uuid, "links", len(links))
	return links, nil
}

// expandedLinkName picks the filename an expanded item should be added
// with: the container link's own "name" field when hide.cx actually
// sends one (the common case observed live), falling back to deriving
// one from the resolved real hoster URL (plugins.FilenameFromURL, which
// strips a rapidgator.net-style ".html" info-page suffix) when it
// doesn't. Without this fallback, an empty name would leave
// queue.Manager's addExpandedItems to pass "" through as the item's
// Filename, which later gets backfilled from whatever the delegate
// plugin's own Resolve derives — directhttp's own URL fallback, in
// particular, if no hoster-specific plugin has an account configured —
// showing the raw URL path (hash/uuid segments and all) instead of a
// real filename. Deriving it here, once, up front, means the filename
// is right regardless of which plugin ends up actually downloading it.
func expandedLinkName(name, hosterURL string) string {
	if name != "" {
		return name
	}
	return plugins.FilenameFromURL(hosterURL)
}

// partNumberRe extracts the numeric index from a "part" filename, e.g.
// "movie.part1.rar" or "movie.part014.rar" -> 1, 14.
var partNumberRe = regexp.MustCompile(`(?i)\.part0*(\d+)\.rar$`)

// sortLinksByPart orders links by their "partN" number when present,
// numerically rather than lexicographically — plain string sorting
// would put "part10" before "part2", and real containers can have more
// than 9 parts. Falls back to a case-insensitive name comparison for
// anything that doesn't match the pattern (a single-file container, or
// a naming convention other than .partN.rar).
func sortLinksByPart(links []plugins.ExpandedLink) {
	sort.SliceStable(links, func(i, j int) bool {
		ni, oki := partNumber(links[i].Name)
		nj, okj := partNumber(links[j].Name)
		if oki && okj {
			return ni < nj
		}
		return strings.ToLower(links[i].Name) < strings.ToLower(links[j].Name)
	})
}

func partNumber(name string) (int, bool) {
	m := partNumberRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// resolveTarget extracts the container UUID from rawURL, fetches the
// container, picks a single link to act on (see pickLink), resolves that
// link's real hoster URL, and re-runs plugin matching on it. Resolve and
// FetchDownloadURL share this so "what does this hide.cx link actually
// point to" has exactly one implementation.
func (p *Plugin) resolveTarget(rawURL string) (finalURL string, hoster plugins.Hoster, acc *plugins.Account, err error) {
	uuid, err := containerUUID(rawURL)
	if err != nil {
		return "", nil, nil, err
	}

	container, err := p.fetchContainer(uuid)
	if err != nil {
		return "", nil, nil, err
	}

	link, err := pickLink(container)
	if err != nil {
		return "", nil, nil, err
	}

	finalURL, err = p.resolveLinkURL(uuid, link)
	if err != nil {
		return "", nil, nil, err
	}

	hoster, acc, err = p.delegateFor(finalURL)
	if err != nil {
		return "", nil, nil, err
	}
	p.log.Info("hidecx: resolved container", "uuid", uuid, "url", finalURL, "hoster", hoster.Name())
	return finalURL, hoster, acc, nil
}

// delegateFor re-runs plugin matching on an already-resolved hoster URL
// and picks the plugin (and account) that should take over from here.
// Split out from resolveTarget as its own pure, network-free function so
// this decision logic — the part the "re-run plugin matching ... so
// Real-Debrid can pick it up if configured" requirement is actually
// about — is directly unit-testable without a real API call.
func (p *Plugin) delegateFor(finalURL string) (plugins.Hoster, *plugins.Account, error) {
	if isHideCx(finalURL) {
		// Defensive: nothing observed in the real API suggests a link
		// can resolve back to hide.cx itself, but refuse cleanly rather
		// than recursing into this same plugin if it ever does.
		return nil, nil, fmt.Errorf("%s: could not resolve hide.cx link: %q still resolves to hide.cx", pluginName, finalURL)
	}

	hoster, acc, ok := p.resolve(finalURL)
	if !ok {
		return nil, nil, fmt.Errorf("%s: could not resolve hide.cx link: no plugin handles %q", pluginName, finalURL)
	}
	if hoster.Name() == pluginName {
		return nil, nil, fmt.Errorf("%s: could not resolve hide.cx link: %q resolved back to hidecx", pluginName, finalURL)
	}
	return hoster, acc, nil
}

// fetchContainer calls GET /containers/<uuid> and validates the result
// is usable: access_status must be "online" (the only value observed so
// far; anything else is treated as temporarily unavailable rather than
// guessed at), and it must have at least one link.
func (p *Plugin) fetchContainer(uuid string) (*containerResponse, error) {
	var c containerResponse
	if err := p.getJSON("/containers/"+uuid, &c); err != nil {
		return nil, err
	}
	if c.AccessStatus != "online" {
		return nil, fmt.Errorf("%s: %w: container access_status is %q", pluginName, plugins.ErrUnavailable, c.AccessStatus)
	}
	if len(c.Links) == 0 {
		return nil, fmt.Errorf("%s: %w: container has no links", pluginName, plugins.ErrUnavailable)
	}
	return &c, nil
}

// pickLink chooses a single link to delegate to, per this plugin's
// single-URL-in/single-URL-out scope limitation (see package doc):
// prefer an online link whose host matches the container's
// primary_hoster, falling back to the first online link of any host.
func pickLink(c *containerResponse) (containerLink, error) {
	var firstOnline *containerLink
	for i := range c.Links {
		l := &c.Links[i]
		if l.LinkStatus != "online" {
			continue
		}
		if firstOnline == nil {
			firstOnline = l
		}
		if l.Host == c.PrimaryHoster {
			return *l, nil
		}
	}
	if firstOnline != nil {
		return *firstOnline, nil
	}
	return containerLink{}, fmt.Errorf("%s: %w: no online links in container", pluginName, plugins.ErrUnavailable)
}

// resolveLinkURL returns link's real hoster URL. The container listing
// itself reports hoster_url as null in every case observed so far, so
// this normally makes a second call, GET
// /containers/<uuid>/links/<link.ID>, which returns {"url": "..."}; the
// direct field is still checked first in case that's not universal.
func (p *Plugin) resolveLinkURL(uuid string, link containerLink) (string, error) {
	if link.HosterURL != nil && *link.HosterURL != "" {
		return *link.HosterURL, nil
	}

	var res linkResolveResponse
	// link.ID comes from the container's API response: escaped, an ID
	// like "../../x?y=" can't rewrite the path or add a query.
	if err := p.getJSON(fmt.Sprintf("/containers/%s/links/%s", uuid, url.PathEscape(link.ID)), &res); err != nil {
		return "", err
	}
	if res.URL == "" {
		return "", fmt.Errorf("%s: %w: empty hoster URL for link %s", pluginName, plugins.ErrUnavailable, link.ID)
	}
	return res.URL, nil
}

// getJSON issues a GET against p.apiBase+path with the headers the API
// requires in practice (see package doc) and decodes a JSON response
// into out. HTTP-level failures map to the shared plugins sentinels
// where one applies; the response body is size-capped defensively even
// though real responses are a couple KB at most.
//
// Bounded by its own httpTimeout-scoped context, the same
// context.WithTimeout(context.Background(), ...) pattern every other
// Hoster plugin's own request helper already uses (rapidgator, realdebrid,
// onefichier) — plugins.Hoster itself takes no context parameter for a
// caller (queue.Manager.run) to thread through, so this is a self-scoped
// bound, not end-to-end cancellation from the download's own context.
// Previously used the contextless http.NewRequest, which relied solely on
// p.client.Timeout with no way to make that bound explicit at the request
// level or to shorten it via a passed-in context.
func (p *Plugin) getJSON(path string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+path, nil)
	if err != nil {
		return fmt.Errorf("%s: build request: %w", pluginName, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://hide.cx")
	req.Header.Set("Referer", "https://hide.cx/")
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w: %v", pluginName, plugins.ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return plugins.ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return plugins.ErrRateLimited
	case resp.StatusCode >= 400:
		return fmt.Errorf("%s: %w: HTTP %d", pluginName, plugins.ErrUnavailable, resp.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: decode response from %s: %w", pluginName, path, err)
	}
	return nil
}
