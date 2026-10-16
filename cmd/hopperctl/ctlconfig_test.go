package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withIsolatedHome points hopperctl's config-path resolution at a fresh
// temp dir, on whichever env var each OS's path actually reads
// (os.UserConfigDir reads %APPDATA% on windows, os.UserHomeDir reads
// $HOME/%USERPROFILE% elsewhere), so tests never touch a real user's
// config.
func withIsolatedHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", dir)
	} else {
		t.Setenv("HOME", dir)
	}
}

func TestLoadConfig_DefaultsWhenFileMissing(t *testing.T) {
	withIsolatedHome(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Target != "" {
		t.Errorf("Target = %q, want empty", cfg.Target)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if !cfg.DeleteAfter {
		t.Errorf("DeleteAfter = false, want true (default)")
	}
	if cfg.WatchDir == "" {
		t.Errorf("WatchDir is empty, want a default")
	}
}

func TestLoadConfig_ReadsFileAndAppliesExplicitFalse(t *testing.T) {
	withIsolatedHome(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig (for path): %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "target: http://192.0.2.10:9666\nwatch_dir: /tmp/dl\nlog_level: debug\ndelete_after: false\n"
	if err := os.WriteFile(cfg.Path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.Target != "http://192.0.2.10:9666" {
		t.Errorf("Target = %q", got.Target)
	}
	if got.WatchDir != "/tmp/dl" {
		t.Errorf("WatchDir = %q", got.WatchDir)
	}
	if got.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", got.LogLevel)
	}
	if got.DeleteAfter {
		t.Errorf("DeleteAfter = true, want false (explicit in file)")
	}
}

func TestResolvedAPITarget_DerivesFromTargetWhenUnset(t *testing.T) {
	cfg := &Config{Target: "http://192.0.2.10:9666"}
	if got, want := cfg.ResolvedAPITarget(), "http://192.0.2.10:9667"; got != want {
		t.Errorf("ResolvedAPITarget() = %q, want %q", got, want)
	}
}

func TestResolvedAPITarget_PrefersExplicitAPITarget(t *testing.T) {
	cfg := &Config{Target: "http://192.0.2.10:9666", APITarget: "http://other-host:9667"}
	if got, want := cfg.ResolvedAPITarget(), "http://other-host:9667"; got != want {
		t.Errorf("ResolvedAPITarget() = %q, want %q", got, want)
	}
}

func TestResolvedAPITarget_EmptyWhenTargetUnset(t *testing.T) {
	cfg := &Config{}
	if got := cfg.ResolvedAPITarget(); got != "" {
		t.Errorf("ResolvedAPITarget() = %q, want empty", got)
	}
}

func TestLoadConfig_ReadsAPITarget(t *testing.T) {
	withIsolatedHome(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig (for path): %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "target: http://192.0.2.10:9666\napi_target: http://192.0.2.10:9667\n"
	if err := os.WriteFile(cfg.Path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.APITarget != "http://192.0.2.10:9667" {
		t.Errorf("APITarget = %q", got.APITarget)
	}
}

// TestCtlConfigPath_UsesProgramDataWhenRunningAsWindowsService is the
// regression test for the bug where a Windows Service (running as
// LocalSystem, per sc.exe's default) could never find the config
// `service install` wrote to the installing user's own %APPDATA% — see
// docs/ARCHITECTURE.md.
func TestCtlConfigPath_UsesProgramDataWhenRunningAsWindowsService(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Service ProgramData config path only applies on Windows")
	}
	withIsolatedHome(t)
	pdDir := t.TempDir()
	t.Setenv("ProgramData", pdDir)

	old := isWindowsServiceFn
	isWindowsServiceFn = func() bool { return true }
	t.Cleanup(func() { isWindowsServiceFn = old })

	got, err := ctlConfigPath()
	if err != nil {
		t.Fatalf("ctlConfigPath: %v", err)
	}
	want := filepath.Join(pdDir, "hopper", "hopperctl.yaml")
	if got != want {
		t.Errorf("ctlConfigPath() = %q, want %q", got, want)
	}
}

func TestCtlConfigPath_UsesAPPDATAWhenNotRunningAsService(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("APPDATA vs ProgramData distinction is Windows-only")
	}
	withIsolatedHome(t)

	old := isWindowsServiceFn
	isWindowsServiceFn = func() bool { return false }
	t.Cleanup(func() { isWindowsServiceFn = old })

	got, err := ctlConfigPath()
	if err != nil {
		t.Fatalf("ctlConfigPath: %v", err)
	}
	want := filepath.Join(os.Getenv("APPDATA"), "hopper", "hopperctl.yaml")
	if got != want {
		t.Errorf("ctlConfigPath() = %q, want %q", got, want)
	}
}

func TestProgramDataConfigPath_UsesProgramDataEnvVar(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ProgramData", dir)
	want := filepath.Join(dir, "hopper", "hopperctl.yaml")
	if got := programDataConfigPath(); got != want {
		t.Errorf("programDataConfigPath() = %q, want %q", got, want)
	}
}

func TestSaveConfigAt_WritesToArbitraryPathWithoutTouchingCfgPath(t *testing.T) {
	withIsolatedHome(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Target = "http://nas.local:9666"

	altPath := filepath.Join(t.TempDir(), "alt", "hopperctl.yaml")
	if err := saveConfigAt(cfg, altPath); err != nil {
		t.Fatalf("saveConfigAt: %v", err)
	}

	data, err := os.ReadFile(altPath)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(data), "nas.local") {
		t.Errorf("written file missing expected target: %s", data)
	}
	if _, err := os.Stat(cfg.Path); !os.IsNotExist(err) {
		t.Errorf("saveConfigAt must not touch cfg.Path; stat err = %v", err)
	}
}

func TestSaveConfig_RoundTrips(t *testing.T) {
	withIsolatedHome(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	cfg.Target = "http://nas.local:9666"
	cfg.WatchDir = "/downloads/watch"
	cfg.DeleteAfter = false

	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig after save: %v", err)
	}
	if got.Target != cfg.Target || got.WatchDir != cfg.WatchDir || got.DeleteAfter != cfg.DeleteAfter {
		t.Errorf("round-tripped config mismatch: got %+v, want %+v", got, cfg)
	}
}
