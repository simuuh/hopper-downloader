package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// A token issued under the previous admin password stops working once
// the password changes (M6).
func TestAuth_PasswordChangeRevokesTokens(t *testing.T) {
	old, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, old)

	changed, _ := newTestServer(t, func(c *Config) {
		c.AdminPasswordHash = "$2a$10$another.hash.for.a.changed.password.value.xxxxxxxxxxxxx"
	})
	w := httptest.NewRecorder()
	changed.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/queue", nil, token))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old token after a password change: status %d, want 401", w.Code)
	}
}

// Refreshing keeps the login time, so it can't extend a session beyond
// maxSessionAge (M6).
func TestRefresh_StopsAfterMaxSessionAge(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	sign := func(authTime time.Time) string {
		c := claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   adminSubject,
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
			AuthTime:        authTime.Unix(),
			PasswordVersion: srv.passwordVersion(),
		}
		tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(srv.cfg.JWTSecret))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/auth/refresh", nil, sign(time.Now().Add(-srv.maxSessionAge()-time.Hour))))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("refresh of a session older than the maximum: status %d, want 401", w.Code)
	}

	loggedIn := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/auth/refresh", nil, sign(loggedIn)))
	if w.Code != http.StatusOK {
		t.Fatalf("refresh of a young session: status %d", w.Code)
	}
	var resp loginResponse
	decodeBody(t, w, &resp)
	got, err := srv.verifyToken(resp.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthTime != loggedIn.Unix() {
		t.Fatalf("refreshed token's auth_time = %d, want the original login %d", got.AuthTime, loggedIn.Unix())
	}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}
