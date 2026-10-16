package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeZipFixture(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZip_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.zip")
	writeZipFixture(t, archivePath, map[string]string{
		"file.txt":       "hello",
		"sub/nested.txt": "world",
	})

	destDir := filepath.Join(dir, "out")
	e := &Extractor{}
	res, err := e.Extract(context.Background(), archivePath, destDir, "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.ExtractedFiles) != 2 {
		t.Fatalf("ExtractedFiles = %v, want 2 entries", res.ExtractedFiles)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "file.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("file.txt = %q, %v, want \"hello\"", got, err)
	}
	got, err = os.ReadFile(filepath.Join(destDir, "sub", "nested.txt"))
	if err != nil || string(got) != "world" {
		t.Errorf("sub/nested.txt = %q, %v, want \"world\"", got, err)
	}
}

func TestExtractZip_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "evil.zip")
	writeZipFixture(t, archivePath, map[string]string{
		"../../escape.txt": "pwned",
	})

	destDir := filepath.Join(dir, "out")
	e := &Extractor{}
	if _, err := e.Extract(context.Background(), archivePath, destDir, "", nil); err == nil {
		t.Fatal("expected an error rejecting the path-traversal entry, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Fatal("path-traversal entry was written outside destDir")
	}
}

func writeTarGzFixture(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o640, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractTarGz_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTarGzFixture(t, archivePath, map[string]string{"file.txt": "hello gz"})

	destDir := filepath.Join(dir, "out")
	e := &Extractor{}
	res, err := e.Extract(context.Background(), archivePath, destDir, "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.ExtractedFiles) != 1 {
		t.Fatalf("ExtractedFiles = %v, want 1 entry", res.ExtractedFiles)
	}
	got, err := os.ReadFile(filepath.Join(destDir, "file.txt"))
	if err != nil || string(got) != "hello gz" {
		t.Errorf("file.txt = %q, %v, want \"hello gz\"", got, err)
	}
}

func TestExtractTarGz_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "evil.tar.gz")
	writeTarGzFixture(t, archivePath, map[string]string{"../../escape.txt": "pwned"})

	destDir := filepath.Join(dir, "out")
	e := &Extractor{}
	if _, err := e.Extract(context.Background(), archivePath, destDir, "", nil); err == nil {
		t.Fatal("expected an error rejecting the path-traversal entry, got nil")
	}
}

func TestExtractTarBz2_RoundTrip(t *testing.T) {
	bzip2Path, err := exec.LookPath("bzip2")
	if err != nil {
		t.Skip("bzip2 CLI not available to build a .tar.bz2 fixture (compress/bzip2 is decode-only)")
	}

	dir := t.TempDir()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := "hello bz2"
	if err := tw.WriteHeader(&tar.Header{Name: "file.txt", Mode: 0o640, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bzip2Path, "-z", "-c") //nolint:gosec // G204: bzip2Path is resolved via exec.LookPath("bzip2"), a fixed binary name, not user input
	cmd.Stdin = bytes.NewReader(tarBuf.Bytes())
	compressed, err := cmd.Output()
	if err != nil {
		t.Fatalf("bzip2 compress fixture: %v", err)
	}
	archivePath := filepath.Join(dir, "archive.tar.bz2")
	if err := os.WriteFile(archivePath, compressed, 0o600); err != nil {
		t.Fatal(err)
	}

	destDir := filepath.Join(dir, "out")
	e := &Extractor{}
	res, err := e.Extract(context.Background(), archivePath, destDir, "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.ExtractedFiles) != 1 {
		t.Fatalf("ExtractedFiles = %v, want 1 entry", res.ExtractedFiles)
	}
	got, err := os.ReadFile(filepath.Join(destDir, "file.txt"))
	if err != nil || string(got) != "hello bz2" {
		t.Errorf("file.txt = %q, %v, want \"hello bz2\"", got, err)
	}
}

func TestExtract_UnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Extractor{}
	if _, err := e.Extract(context.Background(), path, filepath.Join(dir, "out"), "", nil); err == nil {
		t.Fatal("expected ErrUnsupportedFormat, got nil")
	}
}
