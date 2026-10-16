package extract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets this test binary act as a fake "unrar" when re-exec'd via
// exec.Command (the standard Go pattern for testing exec.Command-based
// code without a real external dependency — see os/exec's own
// TestHelperProcess). A real unrar binary isn't assumed to be present in
// every environment this test suite runs in.
func TestMain(m *testing.M) {
	if os.Getenv("HOPPER_FAKE_UNRAR") == "1" {
		os.Exit(fakeUnrarMain())
	}
	os.Exit(m.Run())
}

// fakeUnrarMain emulates just enough of unrar's CLI contract for
// extractRAR's tests: it reads the "-p<password>" (or "-p-" for none)
// argument and the trailing srcPath/destDir positional args, then either
// simulates a successful extraction (writes a marker file into destDir)
// or fails with wrong-password-shaped stderr output, based on the
// HOPPER_FAKE_UNRAR_PASSWORD env var (the "correct" password for this
// run; "" means the archive isn't encrypted and any/no password works).
func fakeUnrarMain() int {
	var gotPassword string
	var positional []string
	for _, a := range os.Args[1:] {
		switch {
		case a == "x" || a == "-o+" || a == "-y":
		case strings.HasPrefix(a, "-p"):
			gotPassword = strings.TrimPrefix(a, "-p")
			if gotPassword == "-" {
				gotPassword = ""
			}
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "fake unrar: expected <src> <destDir>")
		return 1
	}
	destDir := positional[len(positional)-1]

	if counterFile := os.Getenv("HOPPER_FAKE_UNRAR_COUNTER_FILE"); counterFile != "" {
		f, _ := os.OpenFile(counterFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if f != nil {
			_, _ = f.WriteString("x")
			_ = f.Close()
		}
	}
	if os.Getenv("HOPPER_FAKE_UNRAR_REAL_ERROR") == "1" {
		fmt.Fprintln(os.Stderr, "disk full")
		return 1
	}

	want := os.Getenv("HOPPER_FAKE_UNRAR_PASSWORD")
	if want != "" && gotPassword != want {
		fmt.Fprintln(os.Stderr, "CRC failed in encrypted file. Corrupt file or wrong password.")
		return 1
	}

	if err := os.MkdirAll(destDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(destDir, "extracted.txt"), []byte("ok"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// withFakeUnrar returns an Extractor pointed at this test binary
// (re-exec'd as the fake unrar above) and sets the env vars
// fakeUnrarMain reads.
func withFakeUnrar(t *testing.T, correctPassword string) *Extractor {
	t.Helper()
	t.Setenv("HOPPER_FAKE_UNRAR", "1")
	t.Setenv("HOPPER_FAKE_UNRAR_PASSWORD", correctPassword)
	return &Extractor{UnrarPath: os.Args[0]}
}

func TestExtractRAR_SucceedsWithoutPassword(t *testing.T) {
	e := withFakeUnrar(t, "")
	dir := t.TempDir()
	res, err := e.Extract(context.Background(), filepath.Join(dir, "archive.rar"), filepath.Join(dir, "out"), "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.ExtractedFiles) != 1 {
		t.Fatalf("ExtractedFiles = %v, want 1 entry", res.ExtractedFiles)
	}
	if res.PasswordUsed != "" {
		t.Errorf("PasswordUsed = %q, want empty (no password needed)", res.PasswordUsed)
	}
}

func TestExtractRAR_TriesPackagePasswordFirst(t *testing.T) {
	e := withFakeUnrar(t, "secret123")
	dir := t.TempDir()
	res, err := e.Extract(context.Background(), filepath.Join(dir, "archive.rar"), filepath.Join(dir, "out"), "secret123", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.PasswordUsed != "secret123" {
		t.Errorf("PasswordUsed = %q, want secret123", res.PasswordUsed)
	}
}

func TestExtractRAR_FallsThroughToConfiguredPasswordList(t *testing.T) {
	e := withFakeUnrar(t, "configured-pw")
	dir := t.TempDir()

	// pkgPassword is wrong/empty, so it must fall through userPasswords
	// in order until "configured-pw" (the second entry) succeeds.
	res, err := e.Extract(context.Background(), filepath.Join(dir, "archive.rar"), filepath.Join(dir, "out"), "also-wrong", []string{"wrong-guess", "configured-pw"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.PasswordUsed != "configured-pw" {
		t.Errorf("PasswordUsed = %q, want configured-pw", res.PasswordUsed)
	}
}

func TestExtractRAR_AllPasswordsFail(t *testing.T) {
	e := withFakeUnrar(t, "nobody-will-guess-this-exact-string")
	dir := t.TempDir()
	_, err := e.Extract(context.Background(), filepath.Join(dir, "archive.rar"), filepath.Join(dir, "out"), "", nil)
	if err == nil {
		t.Fatal("expected an error when no candidate password matches")
	}
}

func TestExtractRAR_MissingBinaryFailsImmediately(t *testing.T) {
	// exec's own "not found" error must propagate as a real error, not
	// be misread as a wrong-password signal.
	dir := t.TempDir()
	e := &Extractor{UnrarPath: filepath.Join(dir, "does-not-exist-unrar-binary")}
	if _, err := e.Extract(context.Background(), filepath.Join(dir, "archive.rar"), filepath.Join(dir, "out"), "", nil); err == nil {
		t.Fatal("expected an error for a missing unrar binary")
	}
}

func TestExtractRAR_RealErrorAbortsWithoutTryingEveryPassword(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "invocations")
	t.Setenv("HOPPER_FAKE_UNRAR", "1")
	t.Setenv("HOPPER_FAKE_UNRAR_REAL_ERROR", "1")
	t.Setenv("HOPPER_FAKE_UNRAR_COUNTER_FILE", counterFile)

	e := &Extractor{UnrarPath: os.Args[0]}
	if _, err := e.Extract(context.Background(), filepath.Join(dir, "archive.rar"), filepath.Join(dir, "out"), "pkg-pw", []string{"a", "b", "c"}); err == nil {
		t.Fatal("expected an error")
	}

	data, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read invocation counter: %v", err)
	}
	if got := len(data); got != 1 {
		t.Errorf("unrar invoked %d times, want 1 (a real error must abort immediately, not retry every password)", got)
	}
}
