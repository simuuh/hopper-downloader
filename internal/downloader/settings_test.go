package downloader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSetBaseDir_AppliesToNextDownload(t *testing.T) {
	payload := testPayload(1024)
	srv := rangeServer(t, payload, true)
	oldDir, newDir := t.TempDir(), t.TempDir()

	e := New(Config{BaseDir: oldDir})
	if got := e.BaseDir(); got != oldDir {
		t.Fatalf("BaseDir() = %q, want %q", got, oldDir)
	}

	e.SetBaseDir(newDir)
	if got := e.BaseDir(); got != newDir {
		t.Fatalf("BaseDir() = %q, want %q", got, newDir)
	}

	if _, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "file.bin"}); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "file.bin")); err != nil {
		t.Errorf("file should be written under the new BaseDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldDir, "file.bin")); !os.IsNotExist(err) {
		t.Errorf("file should not be written under the old BaseDir")
	}
}

func TestSetDefaultParallelChunks_AffectsNextDownload(t *testing.T) {
	payload := testPayload(64 * 1024)
	srv := rangeServer(t, payload, true)
	base := t.TempDir()

	e := New(Config{BaseDir: base, DefaultParallelChunks: 1})
	e.SetDefaultParallelChunks(4)
	if got := e.defaultChunks.Load(); got != 4 {
		t.Fatalf("defaultChunks = %d, want 4", got)
	}

	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "file.bin"})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
}

func TestSetDefaultParallelChunks_ClampsBelowOne(t *testing.T) {
	e := New(Config{BaseDir: t.TempDir()})
	e.SetDefaultParallelChunks(0)
	if got := e.defaultChunks.Load(); got != 1 {
		t.Errorf("defaultChunks = %d, want clamped to 1", got)
	}
	e.SetDefaultParallelChunks(-5)
	if got := e.defaultChunks.Load(); got != 1 {
		t.Errorf("defaultChunks = %d, want clamped to 1", got)
	}
}

func TestSetRetryMax_ClampsBelowZero(t *testing.T) {
	e := New(Config{BaseDir: t.TempDir(), RetryMax: 3})
	e.SetRetryMax(-1)
	if got := e.retryMax.Load(); got != 0 {
		t.Errorf("retryMax = %d, want clamped to 0", got)
	}
	e.SetRetryMax(7)
	if got := e.retryMax.Load(); got != 7 {
		t.Errorf("retryMax = %d, want 7", got)
	}
}

func TestSetGlobalBandwidthLimit_UnlimitedToLimitedAndBack(t *testing.T) {
	e := New(Config{BaseDir: t.TempDir()}) // starts unlimited (nil limiter)
	if e.globalLimiter.Load() != nil {
		t.Fatal("setup: expected a nil (unlimited) limiter with GlobalBandwidthLimit unset")
	}

	e.SetGlobalBandwidthLimit(1024)
	lim := e.globalLimiter.Load()
	if lim == nil {
		t.Fatal("expected a non-nil limiter after SetGlobalBandwidthLimit(1024)")
	}

	e.SetGlobalBandwidthLimit(0)
	if e.globalLimiter.Load() != nil {
		t.Error("expected SetGlobalBandwidthLimit(0) to restore unlimited (nil limiter)")
	}
}

func TestSetGlobalBandwidthLimit_AppliesToInProgressEngine(t *testing.T) {
	payload := testPayload(32 * 1024)
	srv := rangeServer(t, payload, true)
	base := t.TempDir()

	e := New(Config{BaseDir: base, GlobalBandwidthLimit: 1024})
	e.SetGlobalBandwidthLimit(0) // lift the limit before downloading

	res, err := e.Download(context.Background(), Request{URL: srv.URL, RelPath: "file.bin"})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
}
