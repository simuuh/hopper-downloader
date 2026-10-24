package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// adminSubject is the fixed (and only) identity Hopper's JWTs are issued
// for: there is no multi-user account store, only a single configured
// operator password (server.admin_password_hash).
const adminSubject = "admin"

type claimsCtxKeyType struct{}

var claimsCtxKey claimsCtxKeyType

// claims is the JWT payload Hopper issues and verifies.
type claims struct {
	jwt.RegisteredClaims
	// AuthTime is when the password was entered (Unix seconds). Refreshes
	// carry it over, so a session can't be extended past maxSessionAge.
	AuthTime int64 `json:"auth_time"`
	// PasswordVersion ties the token to the admin password it was issued
	// under (see passwordVersion): changing the password revokes every
	// token issued before.
	PasswordVersion string `json:"pwv"`
}

// minSessionAge is how long a session lasts at least from its login,
// however often it is refreshed; a configured server.token_ttl longer
// than this is the limit instead (see maxSessionAge).
const minSessionAge = 30 * 24 * time.Hour

func (s *Server) maxSessionAge() time.Duration {
	return max(minSessionAge, s.cfg.TokenTTL)
}

// passwordVersion derives a short, non-reversible tag of the current
// admin password hash, keyed with the JWT secret.
func (s *Server) passwordVersion() string {
	mac := hmac.New(sha256.New, []byte(s.cfg.JWTSecret))
	mac.Write([]byte(s.cfg.AdminPasswordHash))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}

// issueToken signs a token for a session that logged in at authTime.
func (s *Server) issueToken(authTime time.Time) (token string, expiresAt time.Time, err error) {
	now := time.Now()
	expiresAt = now.Add(s.cfg.TokenTTL)
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   adminSubject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		AuthTime:        authTime.Unix(),
		PasswordVersion: s.passwordVersion(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(s.cfg.JWTSecret))
	return signed, expiresAt, err
}

func (s *Server) verifyToken(raw string) (*claims, error) {
	c := &claims{}
	tok, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || !tok.Valid {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	// Issued under another admin password (or before tokens carried the
	// version): the password change revoked it.
	if !hmac.Equal([]byte(c.PasswordVersion), []byte(s.passwordVersion())) {
		return nil, fmt.Errorf("invalid token: issued for a previous admin password")
	}
	return c, nil
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return header[len(prefix):]
	}
	return ""
}

// auth requires a valid Bearer JWT in the Authorization header.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r.Header.Get("Authorization"))
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		c, err := s.verifyToken(raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsCtxKey, c)))
	})
}

// wsSubprotocol is the WebSocket subprotocol a browser client offers
// together with its JWT: new WebSocket(url, [wsSubprotocol, token]).
// Browsers can't set an Authorization header on the handshake, and a
// ?token= query parameter ends up in reverse-proxy access logs; the
// Sec-WebSocket-Protocol header doesn't.
const wsSubprotocol = "hopper.bearer"

// wsAuth requires a valid JWT via the Authorization header, the
// Sec-WebSocket-Protocol header (see wsSubprotocol) or, as a fallback for
// older clients, a ?token= query parameter.
func (s *Server) wsAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r.Header.Get("Authorization"))
		if raw == "" {
			raw = subprotocolToken(r.Header.Get("Sec-WebSocket-Protocol"))
		}
		if raw == "" {
			raw = r.URL.Query().Get("token")
		}
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "missing token")
			return
		}
		c, err := s.verifyToken(raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsCtxKey, c)))
	})
}

// subprotocolToken returns the token following wsSubprotocol in a
// Sec-WebSocket-Protocol header value ("hopper.bearer, <jwt>"), or "".
func subprotocolToken(header string) string {
	parts := strings.Split(header, ",")
	for i := 0; i+1 < len(parts); i++ {
		if strings.TrimSpace(parts[i]) == wsSubprotocol {
			return strings.TrimSpace(parts[i+1])
		}
	}
	return ""
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// A dedicated, much stricter per-IP budget than the general API rate
	// limit: bcrypt makes each guess slow, this caps how many guesses a
	// single client gets at all.
	ip := clientIP(r)
	if !s.loginLimiters.get(ip).Allow() {
		s.log.Warn("api: login rate limit exceeded", "remote_ip", ip)
		writeError(w, http.StatusTooManyRequests, "too many login attempts, try again later")
		return
	}
	var req loginRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// bcrypt runs even for a wrong username so both failures take the
	// same time.
	pwErr := bcrypt.CompareHashAndPassword([]byte(s.cfg.AdminPasswordHash), []byte(req.Password))
	if req.Username != adminSubject || pwErr != nil {
		// Logged with the client IP so tools like fail2ban can act on it.
		s.log.Warn("api: failed login", "remote_ip", ip)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token, expiresAt, err := s.issueToken(time.Now())
	if err != nil {
		s.log.Error("api: issue token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to issue token")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token, ExpiresAt: expiresAt})
}

// handleRefreshToken issues a fresh token with a full new TokenTTL
// window, without requiring the password again — a standard sliding-
// session refresh. Mounted behind s.auth like any other endpoint, so it
// can only be called with a *currently still-valid* bearer token: an
// already-expired one is rejected by that middleware before this handler
// ever runs, the same as any other request, so this can't be used to
// resurrect a dead session, only extend a live one. The Web UI calls
// this proactively when the stored token is close to expiring, right
// before firing a bulk action — see web/src/lib/api.ts's
// tokenNearExpiry — so a long-running multi-select operation doesn't
// have some of its parallel requests legitimately 401 mid-batch just
// because the token happened to expire partway through.
//
// The new token keeps the session's login time: once maxSessionAge has
// passed since the password was entered, refreshing stops and the client
// has to log in again, so a stolen token can't be kept alive forever.
func (s *Server) handleRefreshToken(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Context().Value(claimsCtxKey).(*claims)
	if c == nil || c.AuthTime == 0 {
		writeError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	authTime := time.Unix(c.AuthTime, 0)
	if time.Since(authTime) > s.maxSessionAge() {
		writeError(w, http.StatusUnauthorized, "session too old, log in again")
		return
	}
	token, expiresAt, err := s.issueToken(authTime)
	if err != nil {
		s.log.Error("api: refresh token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to issue token")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token, ExpiresAt: expiresAt})
}
