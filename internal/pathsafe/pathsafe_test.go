package pathsafe

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSafeJoin_RejectsTraversal(t *testing.T) {
	base := t.TempDir()
	cases := []string{
		"../etc/passwd",
		"../../etc/passwd",
		"a/../../b",
		"..",
	}
	for _, c := range cases {
		if _, err := SafeJoin(base, c); err == nil {
			t.Errorf("SafeJoin(%q) = nil error, want ErrEscapesBase", c)
		}
	}
}

func TestSafeJoin_AllowsNestedPaths(t *testing.T) {
	base := t.TempDir()
	got, err := SafeJoin(base, "package-1", "file.zip")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(base, "package-1", "file.zip")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSafeJoin_RejectsRelativeBase(t *testing.T) {
	if _, err := SafeJoin("relative/base", "file.txt"); err == nil {
		t.Error("expected error for relative base, got nil")
	}
}

func TestSafeJoin_AbsoluteElementCannotEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("absolute-path-as-element semantics differ on windows")
	}
	base := t.TempDir()
	got, err := SafeJoin(base, "/etc/passwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(base, "etc", "passwd")
	if got != want {
		t.Errorf("got %q, want %q (absolute element should be treated as relative)", got, want)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":               "report.pdf",
		"../../etc/passwd":         "passwd",
		"..\\..\\windows\\win.ini": "win.ini",
		"":                         "download",
		".":                        "download",
		"..":                       "download",
		"  spaced.txt  ":           "spaced.txt",
		"Part 1: Intro?.mkv":       "Part 1_ Intro_.mkv",
		"a<b>c|d*e\"f.rar":         "a_b_c_d_e_f.rar",
		"bell\anamex.zip":          "bellnamex.zip",
		"trailing dots...":         "trailing dots",
		"CON":                      "_CON",
		"nul.txt":                  "_nul.txt",
		"console.log":              "console.log",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilename_TruncatesLongNamesKeepingExtension(t *testing.T) {
	long := strings.Repeat("ä", 200) + ".part01.rar" // 400+ bytes
	got := SanitizeFilename(long)
	if len(got) > maxFilenameBytes {
		t.Fatalf("len = %d, want <= %d", len(got), maxFilenameBytes)
	}
	if !strings.HasSuffix(got, ".rar") || !utf8.ValidString(got) {
		t.Fatalf("got %q, want valid UTF-8 ending in .rar", got)
	}
}

func TestContains(t *testing.T) {
	sep := string(filepath.Separator)
	base := filepath.Join(sep+"data", "hopper")
	cases := []struct {
		dir, target string
		want        bool
	}{
		{base, base, true},
		{base, filepath.Join(base, "sub"), true},
		{base, filepath.Join(base, "..", "hopper", "x"), true},
		{base, filepath.Join(sep+"data", "hopper-other"), false},
		{base, filepath.Join(sep + "data"), false},
		{filepath.Join(sep + "data"), base, true},
	}
	for _, c := range cases {
		if got := Contains(c.dir, c.target); got != c.want {
			t.Errorf("Contains(%q, %q) = %v, want %v", c.dir, c.target, got, c.want)
		}
	}
}

// Invisible and control characters, every reserved device name and a cut
// ending on a dot are handled (N4).
func TestSanitizeFilename_Hardening(t *testing.T) {
	cases := map[string]string{
		"invoice\u202Etxt.exe": "invoicetxt.exe",
		"zero\u200Bwidth.bin":  "zerowidth.bin",
		"c1\u0085ctrl.bin":     "c1ctrl.bin",
		"COM0.txt":             "_COM0.txt",
		"lpt\u00B9":            "_lpt\u00B9",
		"CONIN$":               "_CONIN$",
		"con .txt":             "_con .txt",
		"plain name.part1.rar": "plain name.part1.rar",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", maxFilenameBytes-1) + ". .x"
	if got := SanitizeFilename(long); strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") {
		t.Errorf("truncated name %q ends on a dot or space", got)
	}
}
