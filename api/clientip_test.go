package api

import (
	"net/http/httptest"
	"testing"
)

// Behind a trusted reverse proxy, the client is the rightmost
// X-Forwarded-For entry that isn't a proxy; from anyone else the header
// is ignored (M8).
func TestClientIP_TrustedProxies(t *testing.T) {
	srv, _ := newTestServer(t, func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8"} })
	cases := []struct {
		name, remote, xff, want string
	}{
		{"header from an untrusted client ignored", "192.0.2.7:4000", "198.51.100.1", "192.0.2.7"},
		{"trusted proxy", "10.0.0.2:4000", "198.51.100.1", "198.51.100.1"},
		{"spoofed entries left of the real client", "10.0.0.2:4000", "203.0.113.9, 198.51.100.1", "198.51.100.1"},
		{"proxy chain", "10.0.0.2:4000", "198.51.100.1, 10.0.0.3", "198.51.100.1"},
		{"garbage stops the walk", "10.0.0.2:4000", "198.51.100.1, not-an-ip", "10.0.0.2"},
		{"no header from proxy", "10.0.0.2:4000", "", "10.0.0.2"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := srv.clientIP(r); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}
}
