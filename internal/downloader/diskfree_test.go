package downloader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownload_RefusesFileLargerThanFreeSpace(t *testing.T) {
	payload := testPayload(64 * 1024)
	srv := rangeServer(t, payload, true)
	base := t.TempDir()

	e := New(Config{BaseDir: base})
	e.freeSpace = func(string) (uint64, error) { return 1024, nil }
	_, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "file.bin"})
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	if _, err := os.Stat(filepath.Join(base, "file.bin.part")); !os.IsNotExist(err) {
		t.Fatalf("a .part file was created despite the refused transfer (err=%v)", err)
	}
}

func TestDownload_SpaceCheckErrorDoesNotBlock(t *testing.T) {
	payload := testPayload(64 * 1024)
	srv := rangeServer(t, payload, true)

	e := New(Config{BaseDir: t.TempDir()})
	e.freeSpace = func(string) (uint64, error) { return 0, errors.New("not supported") }
	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "file.bin"}); err != nil {
		t.Fatalf("download failed although the free-space query only errored: %v", err)
	}
}

func TestCheckSpace_ResumeNeedsOnlyTheRest(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "file.bin.part")
	if err := os.WriteFile(tmp, make([]byte, 900), 0o600); err != nil {
		t.Fatal(err)
	}
	e := New(Config{BaseDir: dir})
	e.freeSpace = func(string) (uint64, error) { return 200, nil }

	if err := e.checkSpace(dir, tmp, 1000, true); err != nil {
		t.Fatalf("resume needing 100 of 200 free bytes refused: %v", err)
	}
	if err := e.checkSpace(dir, tmp, 1000, false); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("fresh start needing 1000 of 200 free bytes: err = %v", err)
	}
	if err := e.checkSpace(dir, tmp, 0, false); err != nil {
		t.Fatalf("unknown size refused: %v", err)
	}
}

func TestCheckSpace_ErrorNamesNoURL(t *testing.T) {
	e := New(Config{BaseDir: t.TempDir()})
	e.freeSpace = func(string) (uint64, error) { return 1, nil }
	err := e.checkSpace("dir", "dir/f.part", 10, false)
	if err == nil || strings.Contains(err.Error(), "http") {
		t.Fatalf("err = %v", err)
	}
}

func TestFreeBytes_RealDirectory(t *testing.T) {
	free, err := freeBytes(t.TempDir())
	if err != nil {
		t.Skipf("free space not available here: %v", err)
	}
	if free == 0 {
		t.Fatal("freeBytes reported 0 for the test temp dir")
	}
}
