package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func listPluginKeys(t *testing.T, srv *Server, token string) []pluginInfo {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/plugins", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /plugins = %d", w.Code)
	}
	var out []pluginInfo
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListPlugins_InAccountPriorityOrder(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	got := listPluginKeys(t, srv, token)
	if len(got) != 2 || got[0].Key != "realdebrid" || got[0].Rank != 1 || got[1].Key != "rapidgator" || got[1].Rank != 2 {
		t.Fatalf("default order = %+v, want realdebrid (registered last) first", got)
	}

	fs := srv.settings.(*fakeSettings)
	fs.mu.Lock()
	fs.cur.AccountPriority = []string{"rapidgator"}
	fs.mu.Unlock()

	got = listPluginKeys(t, srv, token)
	if got[0].Key != "rapidgator" || got[0].Rank != 1 || got[1].Key != "realdebrid" {
		t.Fatalf("with priority [rapidgator] = %+v", got)
	}
}
