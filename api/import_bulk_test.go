package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simuuh/hopper-downloader/internal/queue"
)

func TestImport_MultipleFilesInOneRequest(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range map[string]string{
		"a.txt": "http://example.com/a1\nhttp://example.com/a2",
		"b.txt": "see http://example.com/b1 and http://example.com/b2.",
		"c.txt": "no links here",
	} {
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write([]byte(content))
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var resp importResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 4 {
		t.Errorf("items = %d, want 4", len(resp.Items))
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "c.txt") {
		t.Errorf("errors = %v, want one for c.txt", resp.Errors)
	}
	if len(fq.packages) != 2 {
		t.Errorf("packages = %d, want 2", len(fq.packages))
	}
}

func TestAddLinksBulk_ExtractsURLsFromText(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(map[string]any{
		"text": "Part1: https://example.com/x.part1.rar\r\nPart2: https://example.com/x.part2.rar\nnoise",
		"name": "X",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links/bulk", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if fq.lastAddLinksName != "X" {
		t.Errorf("name = %q", fq.lastAddLinksName)
	}
	if len(fq.items) != 2 {
		t.Errorf("items = %d, want 2", len(fq.items))
	}
}

func TestAddLinksBulk_NoURLs(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links/bulk", strings.NewReader(`{"text":"nothing"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPackageActions(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	pkg, _, _ := fq.AddPackage("p", []string{"http://example.com/a", "http://example.com/b"}, queue.AddOptions{})

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	if w := do(http.MethodPost, "/api/v1/packages/"+pkg.ID+"/pause", ""); w.Code != http.StatusOK || fq.lastPackageAction != "pause" {
		t.Fatalf("pause: %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodPost, "/api/v1/packages/"+pkg.ID+"/explode", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", w.Code)
	}
	if w := do(http.MethodPatch, "/api/v1/packages/"+pkg.ID, `{"name":"renamed"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "renamed") {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodPatch, "/api/v1/packages/"+pkg.ID, `{"name":"  "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty rename: %d", w.Code)
	}
	if w := do(http.MethodDelete, "/api/v1/packages/"+pkg.ID, ""); w.Code != http.StatusOK || len(fq.items) != 0 {
		t.Fatalf("delete: %d items=%d", w.Code, len(fq.items))
	}
	if w := do(http.MethodDelete, "/api/v1/packages/"+pkg.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", w.Code)
	}
}
