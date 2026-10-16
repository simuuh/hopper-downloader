package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// apiPort is hopperd's fixed REST API port (see docs/ARCHITECTURE.md's Deployment
// section — 9667, always published on 0.0.0.0, distinct from the CnL
// port 9666 that Target points at). Used to derive APITarget from
// Target when api_target isn't set explicitly.
const apiPort = "9667"

// fileConfig is the on-disk shape of hopperctl.yaml. DeleteAfter is a
// pointer so an absent key can be told apart from an explicit `false` —
// resolveConfig applies the default (true) only when it's nil.
type fileConfig struct {
	Target      string `yaml:"target"`
	APITarget   string `yaml:"api_target"`
	WatchDir    string `yaml:"watch_dir"`
	LogLevel    string `yaml:"log_level"`
	DeleteAfter *bool  `yaml:"delete_after"`
}

// Config is hopperctl's resolved runtime configuration: the file's
// contents (if any) with defaults filled in and flag overrides applied.
// Path is where it was (or would be) loaded from, kept for `status` and
// `service install` to report.
type Config struct {
	Path        string
	Target      string
	APITarget   string
	WatchDir    string
	LogLevel    string
	DeleteAfter bool
}

// ResolvedAPITarget returns the hopperd REST API base URL to use for
// `.dlc`/`.txt` import (`/api/v1/import`, JWT-authenticated): APITarget
// if explicitly configured, otherwise derived from Target by swapping
// its port for hopperd's fixed API port 9667. Target itself points at
// hopperd's CnL port (9666) — a separate listener with no JWT auth (see
// docs/ARCHITECTURE.md), so it must never be used directly as the import URL: doing
// so previously sent .dlc imports to whatever server the user's saved
// *login session* happened to point at (often a local hopperd used only
// to obtain a token), not the NAS/remote hopperd this watcher is
// actually meant to import into.
func (c *Config) ResolvedAPITarget() string {
	if c.APITarget != "" {
		return c.APITarget
	}
	return deriveAPITarget(c.Target)
}

// deriveAPITarget swaps target's port for apiPort, keeping its scheme
// and host. Returns "" if target is empty or not a valid absolute URL —
// callers treat that as "no API target available" rather than guessing.
func deriveAPITarget(target string) string {
	if target == "" {
		return ""
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.Host = net.JoinHostPort(u.Hostname(), apiPort)
	return u.String()
}

// isWindowsServiceFn is a package-level indirection over
// isWindowsService (winservice_windows.go/winservice_other.go) so
// ctlConfigPath's tests can simulate running as an installed Windows
// Service without needing a real SCM-launched process — swapped in
// ctlconfig_test.go, restored via t.Cleanup.
var isWindowsServiceFn = isWindowsService

// ctlConfigPath returns the hopperctl.yaml location for the current
// process. On Windows, when running as an installed Windows Service,
// this is %ProgramData%\hopper\hopperctl.yaml (see
// programDataConfigPath) rather than the usual %APPDATA%\hopper\
// hopperctl.yaml: sc.exe registers a service to run as LocalSystem by
// default, whose own %APPDATA% is a machine profile directory buried
// under the system account, unrelated to the interactively-logged-in
// user who ran `service install` and whose %APPDATA% that install wrote
// the config to — so the service could never find it (see docs/ARCHITECTURE.md).
// Every other Windows invocation (interactive CLI use — including
// `service install`/`uninstall` themselves, which are always run from a
// terminal, never by the SCM) still resolves to os.UserConfigDir()'s
// usual %APPDATA%\hopper\hopperctl.yaml. On Linux/macOS it is always
// ~/.config/hopper/hopperctl.yaml — deliberately not os.UserConfigDir(),
// which resolves to ~/Library/Application Support on macOS; the Phase 2
// spec pins ~/.config on both Unix platforms so a server admin's
// dotfiles/backup scripts only need to know one path. (Neither systemd
// --user nor a launchd user LaunchAgent has this LocalSystem-style
// identity split — both run as the installing user — so this is a
// Windows-only concern.)
func ctlConfigPath() (string, error) {
	if runtime.GOOS == "windows" {
		if isWindowsServiceFn() {
			return programDataConfigPath(), nil
		}
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "hopper", "hopperctl.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "hopper", "hopperctl.yaml"), nil
}

// programDataDir returns %ProgramData%\hopper — the machine-wide,
// account-agnostic directory a Windows Service reads its config from
// and writes its log to (see ctlConfigPath and winservice_windows.go's
// serviceLogPath), instead of the installing user's own %APPDATA%\
// hopper an interactive CLI invocation uses. Pure env-var/string logic,
// safe to reference on any OS (ProgramData is simply unset outside
// Windows), but only ever reached from Windows-Service-specific code
// paths.
func programDataDir() string {
	root := os.Getenv("ProgramData")
	if root == "" {
		root = `C:\ProgramData` // set on every supported Windows version; defensive fallback only
	}
	return filepath.Join(root, "hopper")
}

// programDataConfigPath returns %ProgramData%\hopper\hopperctl.yaml —
// see programDataDir and ctlConfigPath.
func programDataConfigPath() string {
	return filepath.Join(programDataDir(), "hopperctl.yaml")
}

func defaultWatchDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

// loadConfig reads hopperctl.yaml (if present) and resolves it against
// defaults. A missing file is not an error — every field just falls back
// to its default, so `hopperctl proxy -target ...` works standalone
// without ever requiring `service install` to have run first.
func loadConfig() (*Config, error) {
	path, err := ctlConfigPath()
	if err != nil {
		return nil, fmt.Errorf("resolve hopperctl config path: %w", err)
	}

	cfg := &Config{
		Path:        path,
		WatchDir:    defaultWatchDir(),
		LogLevel:    "info",
		DeleteAfter: true,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if fc.Target != "" {
		cfg.Target = fc.Target
	}
	if fc.APITarget != "" {
		cfg.APITarget = fc.APITarget
	}
	if fc.WatchDir != "" {
		cfg.WatchDir = fc.WatchDir
	}
	if fc.LogLevel != "" {
		cfg.LogLevel = fc.LogLevel
	}
	if fc.DeleteAfter != nil {
		cfg.DeleteAfter = *fc.DeleteAfter
	}
	return cfg, nil
}

// saveConfig writes cfg to its own Path, creating the parent directory
// if needed. Used by `hopperctl service install`.
func saveConfig(cfg *Config) error {
	return saveConfigAt(cfg, cfg.Path)
}

// saveConfigAt writes cfg's fields to an arbitrary path, creating the
// parent directory if needed — used by `service install` (on Windows)
// to also write a second copy to %ProgramData%\hopper (see
// programDataConfigPath) for the installed Windows Service to read,
// alongside the normal saveConfig(cfg) write to the interactive user's
// own %APPDATA%\hopper.
func saveConfigAt(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	deleteAfter := cfg.DeleteAfter
	fc := fileConfig{
		Target:      cfg.Target,
		APITarget:   cfg.APITarget,
		WatchDir:    cfg.WatchDir,
		LogLevel:    cfg.LogLevel,
		DeleteAfter: &deleteAfter,
	}
	data, err := yaml.Marshal(fc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
