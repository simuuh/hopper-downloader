package api

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// securityHeaders sets a conservative baseline of hardening headers on
// every response, per OWASP secure-headers guidance. CSP forbids inline
// and third-party scripts; the Web UI build must not need either.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		// connect-src 'self' covers same-origin ws:/wss: too (browsers
		// match websocket connections against 'self' by host, not
		// scheme) — no need for a broader "ws: wss:" source that would
		// also permit cross-origin WebSocket connections.
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// cors implements a strict, allowlist-only CORS policy: with no
// configured origins (the default), no CORS headers are emitted at all,
// which browsers treat as same-origin-only.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	for _, o := range s.cfg.CORSAllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// checkWSOrigin applies the same allowlist to WebSocket upgrades, which
// gorilla/websocket does not run through the cors middleware above. A
// missing Origin header (non-browser clients) is allowed; a same-host
// Origin is allowed when no explicit allowlist is configured.
func (s *Server) checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if len(s.cfg.CORSAllowedOrigins) > 0 {
		return s.originAllowed(origin)
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// ipLimiterStore hands out a per-client-IP token bucket. Idle entries are
// swept periodically so a stream of distinct addresses (trivial with
// IPv6) can't grow the map without bound.
type ipLimiterStore struct {
	mu        sync.Mutex
	limiters  map[string]*ipLimiter
	rpm       int
	burst     int
	lastSweep time.Time
}

type ipLimiter struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// limiterIdleTTL is how long an unused per-IP limiter is kept. Longer than
// a full bucket refill at any sane rate, so evicting it never grants a
// client more than a fresh bucket would.
const limiterIdleTTL = 10 * time.Minute

// Login attempt budget per client IP (see handleLogin).
const (
	loginAttemptsPerMinute = 10
	loginBurst             = 5
)

func newIPLimiterStore(rpm, burst int) *ipLimiterStore {
	return &ipLimiterStore{limiters: make(map[string]*ipLimiter), rpm: rpm, burst: burst, lastSweep: time.Now()}
}

func (s *ipLimiterStore) get(ip string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.lastSweep) > limiterIdleTTL {
		for k, l := range s.limiters {
			if now.Sub(l.lastSeen) > limiterIdleTTL {
				delete(s.limiters, k)
			}
		}
		s.lastSweep = now
	}
	l, ok := s.limiters[ip]
	if !ok {
		l = &ipLimiter{lim: rate.NewLimiter(rate.Limit(float64(s.rpm)/60.0), s.burst)}
		s.limiters[ip] = l
	}
	l.lastSeen = now
	return l.lim
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiters.get(s.clientIP(r)).Allow() {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the address rate limits and login logs go by. Behind a
// reverse proxy every request comes from the proxy, so all clients would
// share one bucket and a single attacker could lock everyone out of the
// login. When the connection comes from a trusted proxy, the rightmost
// X-Forwarded-For entry that isn't itself a trusted proxy is the client:
// entries further left were written by the client and prove nothing.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !s.isTrustedProxy(host) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if _, err := netip.ParseAddr(hop); err != nil {
			break
		}
		if !s.isTrustedProxy(hop) {
			return hop
		}
	}
	return host
}

func (s *Server) isTrustedProxy(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range s.trustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
