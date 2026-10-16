package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

func TestLogin_PerIPAttemptLimit(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler()
	codes := []int{}
	for i := 0; i < loginBurst+2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusUnauthorized {
		t.Fatalf("first attempt = %d, want 401", codes[0])
	}
	if last := codes[len(codes)-1]; last != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d, want 429 after the login burst (codes %v)", len(codes), last, codes)
	}
}

func TestVerifyToken_RejectsOtherHMACAlgorithms(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	c := claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   adminSubject,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS512, c).SignedString([]byte(srv.cfg.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.verifyToken(signed); err == nil {
		t.Fatal("HS512 token accepted, want only HS256")
	}
}

func TestIPLimiterStore_EvictsIdleEntries(t *testing.T) {
	s := newIPLimiterStore(60, 1)
	s.get("192.0.2.1")
	s.limiters["192.0.2.1"].lastSeen = time.Now().Add(-2 * limiterIdleTTL)
	s.lastSweep = time.Now().Add(-2 * limiterIdleTTL)
	s.get("192.0.2.2")
	if _, ok := s.limiters["192.0.2.1"]; ok {
		t.Fatal("idle limiter not evicted")
	}
	if _, ok := s.limiters["192.0.2.2"]; !ok {
		t.Fatal("active limiter missing")
	}
}

func TestWebSocket_AuthViaSubprotocolHeader(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"
	dialer := websocket.Dialer{Subprotocols: []string{wsSubprotocol, token}}
	conn, resp, err := dialer.Dial(wsURL, nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial with subprotocol token: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if conn.Subprotocol() != wsSubprotocol {
		t.Errorf("negotiated subprotocol = %q, want %q", conn.Subprotocol(), wsSubprotocol)
	}

	bad := websocket.Dialer{Subprotocols: []string{wsSubprotocol, "not-a-jwt"}}
	_, resp2, err := bad.Dial(wsURL, nil)
	if resp2 != nil {
		defer resp2.Body.Close()
	}
	if err == nil || resp2 == nil || resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid subprotocol token: err=%v resp=%v, want 401", err, resp2)
	}
}

func TestSubprotocolToken(t *testing.T) {
	cases := map[string]string{
		"hopper.bearer, abc.def.ghi": "abc.def.ghi",
		"other, hopper.bearer,x.y.z": "x.y.z",
		"hopper.bearer":              "",
		"":                           "",
		"abc.def.ghi":                "",
	}
	for in, want := range cases {
		if got := subprotocolToken(in); got != want {
			t.Errorf("subprotocolToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQueueActions(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	do := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/actions", []byte(body), token))
		return w
	}
	if w := do(`{"ids":["a","b"],"action":"resume"}`); w.Code != http.StatusOK || fq.lastPackageAction != "resume" || !strings.Contains(w.Body.String(), `"changed":2`) {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	if w := do(`{"ids":["a"],"action":"explode"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad action: %d", w.Code)
	}
	if w := do(`{"ids":[],"action":"pause"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no ids: %d", w.Code)
	}
}
