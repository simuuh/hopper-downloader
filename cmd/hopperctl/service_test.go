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
