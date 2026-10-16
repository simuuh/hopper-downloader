package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIsImportCandidate(t *testing.T) {
	cases := map[string]bool{
		"file.dlc":  true,
		"FILE.DLC":  true,
		"links.txt": true,
		"readme.md": false,
		"archive":   false,
	}
	for name, want := range cases {
		if got := isImportCandidate(name); got != want {
			t.Errorf("isImportCandidate(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSeedSeen_RecordsExistingFilesOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.dlc"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	seen := seedSeen(dir)
	if !seen["old.dlc"] {
		t.Errorf("expected old.dlc to be seeded as seen")
	}
	if seen["notes.md"] {
		t.Errorf("notes.md should not be tracked (not an import candidate)")
	}
	if len(seen) != 1 {
		t.Errorf("seen = %v, want exactly 1 entry", seen)
	}
}

func TestScanOnce_ImportsNewFileAndSkipsSeeded(t *testing.T) {
	var gotAuth, gotFilename string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: r.Body is already capped via http.MaxBytesReader above; gosec's form-parsing-limits check has no sanitizer for it and flags ParseMultipartForm unconditionally regardless
			t.Fatal(err)
		}
		_, header, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		gotFilename = header.Filename
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"items":[{"id":"1"},{"id":"2"}]}`))
	}))
	defer target.Close()

	dir := t.TempDir()
	newFile := filepath.Join(dir, "new.dlc")
	if err := os.WriteFile(newFile, []byte("container-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Already-seen files must not be re-imported.
	seededFile := filepath.Join(dir, "already.txt")
	if err := os.WriteFile(seededFile, []byte("urls"), 0o600); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{"already.txt": true}
	client := target.Client()

	scanOnce(dir, seen, true, target.URL, "tok123", client, discardLogger())

	if gotFilename != "new.dlc" {
		t.Errorf("target saw filename %q, want new.dlc", gotFilename)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if !seen["new.dlc"] {
		t.Errorf("new.dlc should now be marked seen")
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Errorf("new.dlc should have been deleted after import (delete_after=true), stat err = %v", err)
	}
	if _, err := os.Stat(seededFile); err != nil {
		t.Errorf("already-seeded file should be untouched: %v", err)
	}
}

func TestScanOnce_KeepsFileWhenDeleteAfterFalse(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"items":[{"id":"1"}]}`))
	}))
	defer target.Close()

	dir := t.TempDir()
	f := filepath.Join(dir, "keep.dlc")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	scanOnce(dir, map[string]bool{}, false, target.URL, "tok", target.Client(), discardLogger())

	if _, err := os.Stat(f); err != nil {
		t.Errorf("file should still exist when delete_after=false: %v", err)
	}
}

func TestScanOnce_ImportFailureDoesNotDeleteFile(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	dir := t.TempDir()
	f := filepath.Join(dir, "fail.dlc")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	scanOnce(dir, map[string]bool{}, true, target.URL, "tok", target.Client(), discardLogger())

	if _, err := os.Stat(f); err != nil {
		t.Errorf("file should not be deleted on import failure: %v", err)
	}
}
