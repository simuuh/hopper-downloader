// Package multihost holds what the multi-hoster ("debrid") plugins share:
// a cached list of supported host domains, a rate-limited HTTP client with
// 429 retries, and a short-lived per-link result cache. Each plugin keeps
// its own API specifics (endpoints, envelopes, error codes).
package multihost

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// HostnameOf returns rawURL's lowercased hostname, or "" if rawURL isn't
// an http(s) URL.
func HostnameOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Hosts is a set of supported host domains (lowercased).
type Hosts map[string]bool

// Add adds every domain in domains.
func (h Hosts) Add(domains ...string) {
	for _, d := range domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			h[d] = true
		}
	}
}

// Supports reports whether host is one of the domains or a subdomain of
// one (www.rapidgator.net for rapidgator.net).
func (h Hosts) Supports(host string) bool {
	host = strings.ToLower(host)
	for {
		if h[host] {
			return true
		}
		i := strings.IndexByte(host, '.')
		if i < 0 {
			return false
		}
		host = host[i+1:]
	}
}

// HostCache caches a plugin's supported-hosts list for TTL. On a failed
// refresh it keeps serving the previous list (a transient API error
// shouldn't stop routing links), until Clear drops it — which a plugin
// does once it knows its token is invalid.
type HostCache struct {
	TTL   time.Duration
	Fetch func(token string) (Hosts, error)

	mu        sync.Mutex
	hosts     Hosts
	fetchedAt time.Time
}

// Snapshot returns the cached hosts, refreshing them first if missing or
// older than TTL. If the refresh fails it returns the previous (possibly
// nil) list together with the error.
func (c *HostCache) Snapshot(token string) (Hosts, error) {
	c.mu.Lock()
	fresh := c.hosts != nil && time.Since(c.fetchedAt) < c.TTL
	cached := c.hosts
	c.mu.Unlock()
	if fresh {
		return cached, nil
	}
	hosts, err := c.Fetch(token)
	if err != nil {
		return cached, err
	}
	c.mu.Lock()
	c.hosts, c.fetchedAt = hosts, time.Now()
	c.mu.Unlock()
	return hosts, nil
}

// Domains returns the cached domains (unsorted, possibly empty).
func (c *HostCache) Domains() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.hosts))
	for d := range c.hosts {
		out = append(out, d)
	}
	return out
}

// Clear drops the cached list.
func (c *HostCache) Clear() {
	c.mu.Lock()
	c.hosts, c.fetchedAt = nil, time.Time{}
	c.mu.Unlock()
}

// Client sends API requests through a rate limiter and retries HTTP 429
// a few times with a growing delay.
type Client struct {
	HTTP       *http.Client
	Limiter    *rate.Limiter
	RetryDelay time.Duration // first 429 back-off; tests shorten it
	Name       string        // plugin name, for error messages
}

// NewClient builds a Client allowing perSecond requests (burst burst).
func NewClient(name string, perSecond float64, burst int, timeout time.Duration) *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: timeout},
		Limiter:    rate.NewLimiter(rate.Limit(perSecond), burst),
		RetryDelay: 2 * time.Second,
		Name:       name,
	}
}

const max429Retries = 3

// Do sends one request built by build (called again for every retry, so
// the body can be re-read). The caller closes the response body. After
// the retries are used up the last 429 response is returned as-is.
func (c *Client) Do(ctx context.Context, build func(context.Context) (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := c.Limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("%s: rate limiter: %w", c.Name, err)
		}
		req, err := build(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: build request: %w", c.Name, err)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: request failed: %w", c.Name, err)
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= max429Retries {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		select {
		case <-time.After(time.Duration(attempt+1) * c.RetryDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ResultCache remembers a per-link result for a short time, so Resolve
// and the FetchDownloadURL that follows it can share one API call where
// the service has no separate "check" endpoint (and each call would add
// the link to the account's history again).
type ResultCache[T any] struct {
	TTL time.Duration

	mu      sync.Mutex
	entries map[string]cached[T]
}

type cached[T any] struct {
	val T
	at  time.Time
}

// Get returns the cached value for key, if present and younger than TTL.
func (c *ResultCache[T]) Get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) >= c.TTL {
		var zero T
		return zero, false
	}
	return e.val, true
}

// Put stores val for key and drops expired entries.
func (c *ResultCache[T]) Put(key string, val T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]cached[T])
	}
	now := time.Now()
	for k, e := range c.entries {
		if now.Sub(e.at) >= c.TTL {
			delete(c.entries, k)
		}
	}
	c.entries[key] = cached[T]{val: val, at: now}
}
