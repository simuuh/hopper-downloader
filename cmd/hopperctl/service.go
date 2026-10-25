package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	linuxServiceName   = "hopperctl.service"
	darwinLabel        = "ch.hopper.hopperctl"
	windowsServiceName = "HopperCtl"
)

func cmdService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hopperctl service install|uninstall")
	}
	action, rest := args[0], args[1:]
	switch action {
	case "install":
		return cmdServiceInstall(rest)
	case "uninstall":
		return cmdServiceUninstall(rest)
	default:
		return fmt.Errorf("unknown `service` action %q (want install|uninstall)", action)
	}
}

func cmdServiceInstall(args []string) error {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	target := fs.String("target", "", "hopperd CnL endpoint, written to hopperctl.yaml")
	apiTarget := fs.String("api-target", "", "hopperd API endpoint for .dlc/.txt import, written to hopperctl.yaml (default: derived from -target with port 9667)")
	watchDir := fs.String("watch-dir", "", "directory to watch, written to hopperctl.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *target != "" {
		cfg.Target = *target
	}
	if *apiTarget != "" {
		cfg.APITarget = *apiTarget
	}
	if *watchDir != "" {
		cfg.WatchDir = *watchDir
	}
	msg, err := writeServiceConfigs(cfg)
	if err != nil {
		return err
	}
	fmt.Println(msg)

	exe, err := resolvedExecutable()
	if err != nil {
		return fmt.Errorf("resolve hopperctl binary path: %w", err)
	}

	var instructions string
	switch runtime.GOOS {
	case "windows":
		instructions, err = installWindows(exe)
	case "darwin":
		instructions, err = installDarwin(exe)
	default:
		instructions, err = installLinux(exe)
	}
	if err != nil {
		return fmt.Errorf("register service: %w", err)
	}

	fmt.Println(instructions)
	return nil
}

// writeServiceConfigs writes cfg to its own (CLI) Path and, on Windows,
// also to %ProgramData%\hopper\hopperctl.yaml (see
// programDataConfigPath) — the location the installed Windows Service
// actually reads from, since it runs as LocalSystem, not the
// interactively-logged-in user `service install` itself always runs as
// (see ctlConfigPath). Returns the message to print. Split out from
// cmdServiceInstall so this file-writing logic is directly testable
// without touching sc.exe/systemctl/launchctl at all.
func writeServiceConfigs(cfg *Config) (string, error) {
	if err := saveConfig(cfg); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	if runtime.GOOS != "windows" {
		return fmt.Sprintf("wrote config: %s", cfg.Path), nil
	}
	pdPath := programDataConfigPath()
	if err := saveConfigAt(cfg, pdPath); err != nil {
		return "", fmt.Errorf("write service config: %w", err)
	}
	return fmt.Sprintf("wrote config: %s (service)\n              %s (CLI)", pdPath, cfg.Path), nil
}

// removeServiceConfig removes the %ProgramData% config copy
// writeServiceConfigs wrote for the Windows Service (see
// uninstallWindows) — the interactive user's own %APPDATA%\hopper\
// hopperctl.yaml is left alone, same as every other platform's
// uninstall: the config is kept in case hopperctl is still used
// interactively (proxy/watch/status) after removing the service. A
// missing file (already removed, or the service was never actually
// installed via `service install`) is not an error. Split out from
// uninstallWindows so it's directly testable without a real `sc.exe`
// call.
func removeServiceConfig() error {
	pdPath := programDataConfigPath()
	if err := os.Remove(pdPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", pdPath, err)
	}
	return nil
}

func cmdServiceUninstall(args []string) error {
	fs := flag.NewFlagSet("service uninstall", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var err error
	switch runtime.GOOS {
	case "windows":
		err = uninstallWindows()
	case "darwin":
		err = uninstallDarwin()
	default:
		err = uninstallLinux()
	}
	if err != nil {
		return err
	}

	cfg, cfgErr := loadConfig()
	if cfgErr == nil {
		fmt.Printf("service removed. config kept at %s\n", cfg.Path)
	} else {
		fmt.Println("service removed.")
	}
	return nil
}

// resolvedExecutable returns the absolute path to the running hopperctl
// binary, following symlinks — the service definition needs a stable
// real path, not e.g. a version-managed `~/.local/bin/hopperctl` symlink
// that might later point elsewhere.
func resolvedExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real, nil
	}
	return exe, nil
}

func linuxUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", linuxServiceName), nil
}

// systemdUnit is the user unit running exe. The path is quoted for
// systemd: unquoted, a space in it (a home directory like "/home/Jane
// Doe") split ExecStart into separate arguments, and "%" starts a
// specifier.
func systemdUnit(exe string) string {
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(exe) + `"`
	return fmt.Sprintf(`[Unit]
Description=Hopper CnL proxy + folder watcher

[Service]
ExecStart=%s run
Restart=on-failure

[Install]
WantedBy=default.target
`, quoted)
}

// launchdPlist is the LaunchAgent running exe, with the paths
// XML-escaped: a "&" or "<" in a path broke the plist.
func launchdPlist(exe, logPath string) string {
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, esc(darwinLabel), esc(exe), esc(logPath), esc(logPath))
}

func installLinux(exe string) (string, error) {
	unitPath, err := linuxUnitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o750); err != nil {
		return "", err
	}

	if err := os.WriteFile(unitPath, []byte(systemdUnit(exe)), 0o600); err != nil {
		return "", err
	}

	var warnings string
	if err := runQuiet("systemctl", "--user", "daemon-reload"); err != nil {
		warnings += fmt.Sprintf("\nwarning: `systemctl --user daemon-reload` failed: %v", err)
	}
	if err := runQuiet("systemctl", "--user", "enable", "--now", linuxServiceName); err != nil {
		warnings += fmt.Sprintf("\nwarning: `systemctl --user enable --now %s` failed: %v", linuxServiceName, err)
	}

	return fmt.Sprintf(`wrote unit: %s
service enabled and started (systemd --user).

Check status:   systemctl --user status %s
Follow logs:    journalctl --user -u %s -f
Uninstall:      hopperctl service uninstall%s`, unitPath, linuxServiceName, linuxServiceName, warnings), nil
}

func uninstallLinux() error {
	_ = runQuiet("systemctl", "--user", "disable", "--now", linuxServiceName)
	unitPath, err := linuxUnitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", unitPath, err)
	}
	_ = runQuiet("systemctl", "--user", "daemon-reload")
	return nil
}

func darwinPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", darwinLabel+".plist"), nil
}

func installDarwin(exe string) (string, error) {
	plistPath, err := darwinPlistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o750); err != nil {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	logPath := filepath.Join(home, "Library", "Logs", "hopperctl.log")

	if err := os.WriteFile(plistPath, []byte(launchdPlist(exe, logPath)), 0o600); err != nil {
		return "", err
	}

	var warnings string
	_ = runQuiet("launchctl", "unload", plistPath) // ignore: fails harmlessly if not already loaded
	if err := runQuiet("launchctl", "load", "-w", plistPath); err != nil {
		warnings += fmt.Sprintf("\nwarning: `launchctl load -w %s` failed: %v", plistPath, err)
	}

	return fmt.Sprintf(`wrote launch agent: %s
service loaded and started (launchd).

Check status:   launchctl list | grep %s
Logs:           %s
Uninstall:      hopperctl service uninstall%s`, plistPath, darwinLabel, logPath, warnings), nil
}

func uninstallDarwin() error {
	plistPath, err := darwinPlistPath()
	if err != nil {
		return err
	}
	_ = runQuiet("launchctl", "unload", "-w", plistPath)
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", plistPath, err)
	}
	return nil
}

// installWindows registers hopperctl as a real Windows Service via
// sc.exe (the Service Control Manager), not Task Scheduler — a
// scheduled task starts an interactive console process tied to whatever
// session/window created it (closing that window kills it); a Windows
// Service runs independently of any logon session, session 0, no
// console window, and survives every terminal being closed. `exe run`
// is the invocation the service runs; when hopperctl detects (via
// golang.org/x/sys/windows/svc.IsWindowsService, see
// winservice_windows.go) that it was launched by the SCM rather than a
// terminal, `main` dispatches to a Windows-Handler-shaped run of the
// exact same proxy+watch logic `run` uses interactively, logging to
// %ProgramData%\hopper\hopperctl.log instead of stderr (a service has
// no console to write to) — and, per writeServiceConfigs (called by
// cmdServiceInstall before this function runs), reading its config from
// %ProgramData%\hopper\hopperctl.yaml too: sc.exe runs the service as
// LocalSystem by default, a different account from whoever is running
// `service install` right now, so a config or log path resolved via the
// normal interactive-user %APPDATA% would be invisible to it.
func installWindows(exe string) (string, error) {
	binPathValue := fmt.Sprintf(`"%s" run`, exe)
	if err := runQuiet("sc", "create", windowsServiceName, "binPath=", binPathValue, "start=", "auto"); err != nil {
		return "", fmt.Errorf("`sc create` failed: %w", err)
	}
	if err := runQuiet("sc", "description", windowsServiceName, "Hopper Click'n'Load proxy and folder watcher"); err != nil {
		return "", fmt.Errorf("`sc description` failed: %w", err)
	}

	var warnings string
	if err := runQuiet("sc", "start", windowsServiceName); err != nil {
		warnings = fmt.Sprintf("\nwarning: `sc start %s` failed (it will still start at next boot, start type auto): %v", windowsServiceName, err)
	}

	return fmt.Sprintf(`registered Windows Service: %s (start type: auto, visible in services.msc)

Check status:   sc query %s
Stop:           sc stop %s
Uninstall:      hopperctl service uninstall
Logs:           %%ProgramData%%\hopper\hopperctl.log

Runs with no console window — closing any terminal does not stop it.%s`, windowsServiceName, windowsServiceName, windowsServiceName, warnings), nil
}

// uninstallWindows stops and deletes the registered Windows Service via
// sc.exe, and removes the %ProgramData% config copy installWindows (via
// writeServiceConfigs) wrote for it — see removeServiceConfig. The
// interactive user's own %APPDATA%\hopper\hopperctl.yaml is left alone,
// same as every other platform's uninstall: the config is kept in case
// hopperctl is still used interactively (proxy/watch/status) after
// removing the service.
func uninstallWindows() error {
	_ = runQuiet("sc", "stop", windowsServiceName)
	if err := runQuiet("sc", "delete", windowsServiceName); err != nil {
		return fmt.Errorf("`sc delete` failed: %w", err)
	}
	if err := removeServiceConfig(); err != nil {
		return err
	}
	return nil
}

// runQuiet runs an external service-manager command, discarding its
// stdout but returning stderr as part of the error on failure — these
// are all short, local, non-interactive commands (systemctl/launchctl/
// schtasks), never something whose output hopperctl needs to stream.
func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // G204: name is always one of this file's own constants (systemctl/launchctl/schtasks); args are hopperctl-constructed paths/flags, not user input
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, out)
	}
	return nil
}
