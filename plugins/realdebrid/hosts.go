package realdebrid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/plugins/internal/multihost"
)

// hostsSnapshot returns the current supported-hosts map, refreshing it
// from /hosts/status if the cache is missing or older than
// hostsCacheTTL. If a refresh is attempted and fails, it returns the
// previous (possibly stale, possibly nil) cache alongside the error, so
// Match can degrade gracefully instead of refusing every URL until the
// next successful refresh.
//
// Like multihost.HostCache, a failed refresh is remembered for
// multihost.FailureBackoff per token, and only one refresh runs at a time:
// Match runs for every added link, and with the API down each call used
// to wait out its own request timeout.
func (p *Plugin) hostsSnapshot(token string) (map[string]hostStatus, error) {
	if hosts, err, done := p.cachedHosts(token); done {
		return hosts, err
	}
	p.hostsFetchMu.Lock()
	defer p.hostsFetchMu.Unlock()
	if hosts, err, done := p.cachedHosts(token); done {
		return hosts, err
	}

	fetched, err := p.fetchHostsStatus(token)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.hostsFailedAt, p.hostsFailToken, p.hostsFailErr = time.Now(), token, err
		return p.hosts, err
	}
	p.hosts = fetched
	p.hostsFetchedAt = time.Now()
	p.hostsFailedAt, p.hostsFailToken, p.hostsFailErr = time.Time{}, "", nil
	return fetched, nil
}

// cachedHosts answers from the cache when it is fresh or a refresh for
// token failed within multihost.FailureBackoff.
func (p *Plugin) cachedHosts(token string) (hosts map[string]hostStatus, err error, done bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hosts != nil && time.Since(p.hostsFetchedAt) < hostsCacheTTL {
		return p.hosts, nil, true
	}
	if p.hostsFailErr != nil && p.hostsFailToken == token && time.Since(p.hostsFailedAt) < multihost.FailureBackoff {
		return p.hosts, p.hostsFailErr, true
	}
	return nil, nil, false
}

// WarmHostsCache eagerly refreshes the /hosts/status cache using token,
// so Match can succeed on its very next call instead of returning false
// until whatever on-demand refresh happens to run next. Intended to be
// called right after ValidateAccount confirms a token actually works, so
// a newly added (or re-validated) Real-Debrid account starts routing
// URLs immediately rather than only after Match's own lazy fetch. Errors
// are logged, not returned: this is a best-effort optimization, not
// something worth failing account validation over.
func (p *Plugin) WarmHostsCache(token string) {
	if token == "" {
		return
	}
	hosts, err := p.fetchHostsStatus(token)
	if err != nil {
		p.log.Warn("realdebrid: host cache empty, Match() will return false for all URLs", "error", err)
		return
	}
	p.mu.Lock()
	p.hosts = hosts
	p.hostsFetchedAt = time.Now()
	p.mu.Unlock()
	p.log.Info("realdebrid: host cache warmed after account validation", "hosts", len(hosts))
}

// ClearHostsCache drops the cached /hosts/status snapshot outright, rather
// than leaving it to expire on its own TTL. Called from ValidateAccount on
// a confirmed-invalid token (401/403 — see its doc comment): without this,
// Match's own stale-cache fallback (see its doc comment — deliberately
// tolerant of a *transient* refresh failure, so a network blip doesn't
// stop routing) can't tell "hosts/status failed because the network
// hiccuped" apart from "hosts/status failed because this token is
// permanently dead", and keeps matching against the last-known-good
// snapshot for up to hostsCacheTTL either way. That silently strands a
// queued item on a Real-Debrid account that can no longer actually
// resolve/download anything, instead of letting queue.Manager's own
// plugin fallback (matchHosterExcluding, triggered by the
// plugins.ErrAccountRequired that Resolve/FetchDownloadURL correctly
// still return) hand the URL to a different plugin on the very next
// attempt.
func (p *Plugin) ClearHostsCache() {
	p.mu.Lock()
	p.hosts = nil
	p.hostsFetchedAt = time.Time{}
	p.hostsFailedAt, p.hostsFailToken, p.hostsFailErr = time.Time{}, "", nil
	p.mu.Unlock()
}

func (p *Plugin) fetchHostsStatus(token string) (map[string]hostStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hostsFetchTimeout)
	defer cancel()

	resp, err := p.do(ctx, http.MethodGet, "/hosts/status", token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := statusErr(resp.StatusCode); err != nil {
		return nil, err
	}

	// The response is a map keyed by domain name, e.g.:
	//   {"1fichier.com": {"supported": 1, "status": "up", ...}, ...}
	var raw map[string]struct {
		Supported int    `json:"supported"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: decode hosts/status response: %w", pluginName, err)
	}

	out := make(map[string]hostStatus, len(raw))
	for domain, v := range raw {
		out[strings.ToLower(domain)] = hostStatus{Supported: v.Supported == 1, Status: v.Status}
	}
	// rg.to's alias to rapidgator.net is applied explicitly in Match's
	// hostSupported, not mirrored into the cache here — see its doc
	// comment. Keeping this map an unmodified reflection of what
	// /hosts/status actually reported also makes Domains() (which reads
	// this map directly) report only real entries.
	return out, nil
}
