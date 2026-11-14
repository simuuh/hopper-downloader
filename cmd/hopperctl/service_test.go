package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteServiceConfigs_WritesBothPathsOnWindows is the regression
// test for the bug where a Windows Service (running as LocalSystem)
// could never find the config `service install` wrote to the
// installing user's own %APPDATA% — see docs/ARCHITECTURE.md. It exercises the
// file-writing logic directly, without touching sc.exe/services.msc at
// all (installWindows itself is not unit-tested for that reason — it
// would register/remove a real Windows Service on whatever machine runs
// `go test`).
func TestWriteServiceConfigs_WritesBothPathsOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("dual APPDATA/ProgramData config write is Windows-only")
	}
	withIsolatedHome(t)
	pdDir := t.TempDir()
	t.Setenv("ProgramData", pdDir)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Target = "http://nas.local:9666"

	msg, err := writeServiceConfigs(cfg)
	if err != nil {
		t.Fatalf("writeServiceConfigs: %v", err)
	}

	pdPath := filepath.Join(pdDir, "hopper", "hopperctl.yaml")
	if _, err := os.Stat(pdPath); err != nil {
		t.Errorf("ProgramData config not written: %v", err)
	}
	if _, err := os.Stat(cfg.Path); err != nil {
		t.Errorf("CLI (APPDATA) config not written: %v", err)
	}
	if !strings.Contains(msg, pdPath) {
		t.Errorf("message %q does not mention the ProgramData path", msg)
	}
	if !strings.Contains(msg, cfg.Path) {
		t.Errorf("message %q does not mention the CLI path", msg)
	}
	if !strings.Contains(msg, "(service)") || !strings.Contains(msg, "(CLI)") {
		t.Errorf("message %q missing (service)/(CLI) labels", msg)
	}

	pdData, err := os.ReadFile(pdPath)
	if err != nil {
		t.Fatal(err)
	}
	cliData, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(pdData) != string(cliData) {
		t.Errorf("ProgramData and CLI config contents differ:\n%s\nvs\n%s", pdData, cliData)
	}
}

func TestWriteServiceConfigs_NonWindowsWritesOnlyCLIPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this checks the non-Windows single-write behavior")
	}
	withIsolatedHome(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := writeServiceConfigs(cfg)
	if err != nil {
		t.Fatalf("writeServiceConfigs: %v", err)
	}
	if strings.Contains(msg, "(service)") {
		t.Errorf("message %q should not mention (service) on non-Windows", msg)
	}
}

func TestRemoveServiceConfig_RemovesProgramDataCopyOnlyKeepsCLICopy(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ProgramData config removal is Windows-only")
	}
	withIsolatedHome(t)
	pdDir := t.TempDir()
	t.Setenv("ProgramData", pdDir)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeServiceConfigs(cfg); err != nil {
		t.Fatal(err)
	}

	pdPath := filepath.Join(pdDir, "hopper", "hopperctl.yaml")
	if err := removeServiceConfig(); err != nil {
		t.Fatalf("removeServiceConfig: %v", err)
	}
	if _, err := os.Stat(pdPath); !os.IsNotExist(err) {
		t.Errorf("ProgramData config should be removed; stat err = %v", err)
	}
	if _, err := os.Stat(cfg.Path); err != nil {
		t.Errorf("CLI (APPDATA) config should be kept; stat err = %v", err)
	}
}

func TestRemoveServiceConfig_MissingFileIsNotAnError(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ProgramData config removal is Windows-only")
	}
	t.Setenv("ProgramData", t.TempDir())
	if err := removeServiceConfig(); err != nil {
		t.Errorf("removeServiceConfig on a config that was never written = %v, want nil", err)
	}
}

// A binary path with a space or "%" stays one ExecStart argument (N24).
func TestSystemdUnit_QuotesExecutable(t *testing.T) {
	unit := systemdUnit(`/home/Jane Doe/bin/hop%per "x"`)
	want := `ExecStart="/home/Jane Doe/bin/hop%%per \"x\"" run`
	if !strings.Contains(unit, want) {
		t.Fatalf("unit has no %q:\n%s", want, unit)
	}
}

// Paths are XML-escaped in the LaunchAgent plist (N24).
func TestLaunchdPlist_EscapesPaths(t *testing.T) {
	plist := launchdPlist("/Users/a&b/<bin>/hopperctl", "/Users/a&b/log")
	if strings.Contains(plist, "a&b") || strings.Contains(plist, "<bin>") {
		t.Fatalf("unescaped path in plist:\n%s", plist)
	}
	if !strings.Contains(plist, "/Users/a&amp;b/&lt;bin&gt;/hopperctl") {
		t.Fatalf("escaped path missing:\n%s", plist)
	}
}

func TestInstallBinary_CopiesIntoInstallDir(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "downloaded-hopperctl")
	if err := os.WriteFile(src, []byte("binary v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "Hopper")

	got, err := installBinary(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, installedBinaryName); got != want {
		t.Fatalf("installBinary returned %q, want %q", got, want)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary v1" {
		t.Fatalf("installed copy holds %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("install dir holds %d entries, want only the binary (temp file left behind?)", len(entries))
	}
}

func TestInstallBinary_ReplacesOlderCopy(t *testing.T) {
	src := filepath.Join(t.TempDir(), "hopperctl-new")
	if err := os.WriteFile(src, []byte("binary v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, installedBinaryName), []byte("binary v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := installBinary(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary v2" {
		t.Fatalf("installed copy holds %q, want the new binary", data)
	}
}

// Re-running install from the installed copy must not try to copy the
// file onto itself.
func TestInstallBinary_AlreadyInstalledIsKept(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, installedBinaryName)
	if err := os.WriteFile(dst, []byte("binary v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := installBinary(dst, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dst {
		t.Fatalf("installBinary returned %q, want %q", got, dst)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary v1" {
		t.Fatalf("installed copy changed to %q", data)
	}
}

func TestRemoveInstalledBinary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Hopper")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, installedBinaryName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeInstalledBinary(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("empty install dir still exists (err=%v)", err)
	}
	// Second call: nothing left to remove, still not an error.
	if err := removeInstalledBinary(dir); err != nil {
		t.Fatalf("missing binary reported as error: %v", err)
	}
}

func TestWindowsInstallDir_UsesProgramFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", root)
	if got, want := windowsInstallDir(), filepath.Join(root, "Hopper"); got != want {
		t.Fatalf("windowsInstallDir() = %q, want %q", got, want)
	}
}

func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "hopperctl.exe")
	if !samePath(a, filepath.Join(dir, ".", "hopperctl.exe")) {
		t.Fatal("cleaned paths not treated as equal")
	}
	if samePath(a, filepath.Join(dir, "other.exe")) {
		t.Fatal("different files treated as equal")
	}
	if runtime.GOOS == "windows" && !samePath(a, strings.ToUpper(a)) {
		t.Fatal("Windows paths compared case-sensitively")
	}
}
