package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zeros compresses to almost nothing: the shape of a decompression bomb.
var zeros = strings.Repeat("\x00", 4<<20)

func TestExtractZip_StopsAtExpansionBudget(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bomb.zip")
	writeZipFixture(t, archive, map[string]string{"zeros.bin": zeros})

	dest := filepath.Join(dir, "out")
	e := &Extractor{minBudget: 1 << 20}
	_, err := e.Extract(context.Background(), archive, dest, "", nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Extract = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "zeros.bin")); !os.IsNotExist(err) {
		t.Fatalf("partially written entry left behind (stat err = %v)", err)
	}
}

func TestExtractTarGz_StopsAtExpansionBudget(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bomb.tar.gz")
	writeTarGzFixture(t, archive, map[string]string{"zeros.bin": zeros})

	e := &Extractor{minBudget: 1 << 20}
	_, err := e.Extract(context.Background(), archive, filepath.Join(dir, "out"), "", nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Extract = %v, want ErrTooLarge", err)
	}
}

func TestExtractZip_TooManyEntries(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "many.zip")
	files := map[string]string{}
	for i := range 5 {
		files[fmt.Sprintf("f%d.txt", i)] = "x"
	}
	writeZipFixture(t, archive, files)

	e := &Extractor{maxEntries: 3}
	_, err := e.Extract(context.Background(), archive, filepath.Join(dir, "out"), "", nil)
	if !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("Extract = %v, want ErrTooManyEntries", err)
	}
}

// zip and tar extraction stop when their context ends — the extraction
// timeout and hopperd's shutdown — like RAR extraction already did.
func TestExtract_StopsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "a.zip")
	writeZipFixture(t, zipPath, map[string]string{"file.txt": "hello"})
	tarPath := filepath.Join(dir, "a.tar.gz")
	writeTarGzFixture(t, tarPath, map[string]string{"file.txt": "hello"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &Extractor{}
	for _, archive := range []string{zipPath, tarPath} {
		_, err := e.Extract(ctx, archive, filepath.Join(dir, "out-"+filepath.Base(archive)), "", nil)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: Extract = %v, want context.Canceled", filepath.Base(archive), err)
		}
	}
}

// Only the end of unrar's output is kept for the error message (N14);
// its error lines come last.
func TestTailBuffer_KeepsTheEnd(t *testing.T) {
	tb := &tailBuffer{max: 8}
	_, _ = tb.Write([]byte("Extracting 1\nExtracting 2\n"))
	_, _ = tb.Write([]byte("CRC fail"))
	if got := tb.String(); got != "…CRC fail" {
		t.Fatalf("String() = %q, want the last 8 bytes marked as cut", got)
	}
	small := &tailBuffer{max: 64}
	_, _ = small.Write([]byte("short"))
	if got := small.String(); got != "short" {
		t.Fatalf("String() = %q, want it unchanged", got)
	}
}
