package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simuuh/hopper-downloader/internal/account"
	"github.com/simuuh/hopper-downloader/plugins"
)

type premiumHoster struct {
	fakeValidatingHoster
}

func (premiumHoster) PremiumURL() string  { return "https://hoster.example/premium" }
func (premiumHoster) ReferralURL() string { return "https://hoster.example/?ref=dev" }

func TestUpdateAccount_ReplacesCredentialsAndRevalidates(t *testing.T) {
	var seen string
	reg := plugins.NewRegistry(&fakeValidatingHoster{
		fakeCredentialedHoster: fakeCredentialedHoster{name: "realdebrid"},
		validateFn: func(acc *plugins.Account) (*plugins.AccountInfo, error) {
			seen = acc.Password
			return &plugins.AccountInfo{Valid: acc.Password == "new-token", Label: "checked"}, nil
		},
	})
	srv, _ := newTestServerWithRegistry(t, reg, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "realdebrid", Username: "alice", Password: "old-token"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	var acc account.Account
	_ = json.Unmarshal(w.Body.Bytes(), &acc)

	body, _ = json.Marshal(updateAccountRequest{Password: "new-token"})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPut, "/api/v1/accounts/"+acc.ID, body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var updated account.Account
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if seen != "new-token" || updated.Valid == nil || !*updated.Valid {
		t.Fatalf("revalidated with %q, valid=%v", seen, updated.Valid)
	}
	if updated.Username != "alice" {
		t.Errorf("username = %q, empty field should keep it", updated.Username)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPut, "/api/v1/accounts/nope", body, token))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPut, "/api/v1/accounts/"+acc.ID, []byte(`{}`), token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty update: %d", w.Code)
	}
}

func TestListPlugins_PremiumURLHonorsReferralSetting(t *testing.T) {
	for _, tc := range []struct {
		referral bool
		want     string
	}{{true, "https://hoster.example/?ref=dev"}, {false, "https://hoster.example/premium"}} {
		reg := plugins.NewRegistry(&premiumHoster{fakeValidatingHoster{fakeCredentialedHoster: fakeCredentialedHoster{name: "rd"}}})
		srv, _ := newTestServerWithRegistry(t, reg, func(c *Config) { c.ReferralLinks = tc.referral })
		token := loginAndGetToken(t, srv)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/plugins", nil, token))
		var out []pluginInfo
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if len(out) != 1 || out[0].PremiumURL != tc.want || out[0].IsReferral != tc.referral {
			t.Errorf("referral=%v: %+v", tc.referral, out)
		}
	}
}

type tokenHoster struct{ fakeCredentialedHoster }

func (tokenHoster) CredentialFields() []plugins.CredentialField {
	return plugins.TokenCredentialFields("API token", "")
}

func TestAddAccount_RequiresOnlyThePluginsRequiredFields(t *testing.T) {
	reg := plugins.NewRegistry(&tokenHoster{fakeCredentialedHoster{name: "tok"}}, &fakeCredentialedHoster{name: "userpass"})
	srv, _ := newTestServerWithRegistry(t, reg, nil)
	token := loginAndGetToken(t, srv)

	for _, tc := range []struct {
		req  addAccountRequest
		want int
	}{
		{addAccountRequest{HosterKey: "tok", Password: "t0ken"}, http.StatusCreated},      // no username needed
		{addAccountRequest{HosterKey: "tok", Username: "x"}, http.StatusBadRequest},       // token missing
		{addAccountRequest{HosterKey: "userpass", Password: "pw"}, http.StatusBadRequest}, // username required by default
		{addAccountRequest{HosterKey: "userpass", Username: "u", Password: "pw"}, http.StatusCreated},
	} {
		body, _ := json.Marshal(tc.req)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
		if w.Code != tc.want {
			t.Errorf("%+v: status %d, want %d (%s)", tc.req, w.Code, tc.want, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/plugins", nil, token))
	var out []pluginInfo
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	for _, p := range out {
		if p.Key == "tok" && (len(p.Fields) != 2 || p.Fields[1].Label != "API token" || p.Fields[0].Required) {
			t.Errorf("tok fields = %+v", p.Fields)
		}
		if p.Key == "userpass" && len(p.Fields) != 2 {
			t.Errorf("userpass fields = %+v", p.Fields)
		}
	}
}
