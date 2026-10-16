package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simuuh/hopper-downloader/internal/queue"
)

func TestPatchQueueItem_EditsURLAndFilename(t *testing.T) {
	srv, q := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := q.AddLink("https://dead.example.com/a", queue.AddOptions{})
	q.items[it.ID].State = queue.StateError

	body := []byte(`{"url":"https://mirror.example.com/a","filename":"a.rar"}`)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/queue/"+it.ID, body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got queue.Item
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.URL != "https://mirror.example.com/a" || got.Filename != "a.rar" || got.State != queue.StateQueued {
		t.Errorf("item = %+v", got)
	}
}

func TestPatchQueueItem_DoneItemIsConflict(t *testing.T) {
	srv, q := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := q.AddLink("https://example.com/a", queue.AddOptions{})
	q.items[it.ID].State = queue.StateDone

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/queue/"+it.ID, []byte(`{"url":"https://example.com/b"}`), token))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/queue/"+it.ID, []byte(`{}`), token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty patch: status = %d, want 400", w.Code)
	}
}

func TestAddToPackage_Endpoint(t *testing.T) {
	srv, q := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	pkg, _, err := q.AddPackage("Release", []string{"https://example.com/1"}, queue.AddOptions{})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"text":"missing part: https://example.com/2 and https://example.com/3"}`)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/packages/"+pkg.ID+"/links", body, token))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var res queue.AddResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Items) != 2 || res.Items[0].PackageID != pkg.ID {
		t.Errorf("result = %+v", res)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/packages/nope/links", body, token))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown package: status = %d, want 404", w.Code)
	}
}
