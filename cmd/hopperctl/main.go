// Command hopperctl is Hopper's client CLI: a thin wrapper around the
// hopperd REST API, plus the Phase 2 remote-Click'n'Load tooling for the
// NAS/server scenario — a local CnL proxy relay (`proxy`), a `.dlc`/
// `.txt` folder watcher (`watch`), both together (`run`, the default),
// and OS service registration (`service install`/`uninstall`) so `run`
// survives a reboot without a login shell open.
package main

import (
	"fmt"
	"os"
)

func main() {
	// On Windows, `service install` registers hopperctl as a real
	// Windows Service (sc.exe, not Task Scheduler — see service.go) that
	// invokes this same binary as `hopperctl run`. When the Service
	// Control Manager launches it that way, isWindowsService reports
	// true and this dispatches into the SCM-driven lifecycle
	// (winservice_windows.go) instead of normal CLI argument parsing —
	// a real service process has no console and no argv beyond what the
	// SCM passes, so it must never fall through to run()'s interactive
	// path. On every other platform (and when run from a terminal on
	// Windows), isWindowsService is always false and this is a no-op.
	if isWindowsService() {
		if err := runAsWindowsService(); err != nil {
			os.Exit(1)
		}
		return
	}

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hopperctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return cmdRun(nil)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "hash-password":
		return cmdHashPassword(rest)
	case "login":
		return cmdLogin(rest)
	case "add":
		return cmdAdd(rest)
	case "list":
		return cmdList(rest)
	case "pause":
		return cmdItemAction(rest, "pause")
	case "resume":
		return cmdItemAction(rest, "resume")
	case "retry":
		return cmdItemAction(rest, "retry")
	case "cancel":
		return cmdItemAction(rest, "cancel")
	case "stats":
		return cmdStats(rest)
	case "proxy":
		return cmdProxy(rest)
	case "watch":
		return cmdWatch(rest)
	case "run":
		return cmdRun(rest)
	case "service":
		return cmdService(rest)
	case "status":
		return cmdStatus(rest)
	case "version":
		return cmdVersion(rest)
	case "-h", "-help", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `hopperctl - Hopper client CLI

Setup:
  hash-password         Read a password from stdin, print its bcrypt hash
                         for server.admin_password_hash in hopper.yaml

Session:
  login -server URL     Read a password from stdin, store a session token
                         (default server: http://127.0.0.1:9667 or $HOPPER_SERVER)

Queue:
  add <url> [-priority low|normal|high] [-filename NAME] [-password PW]
  list                   List queue items
  pause <id>
  resume <id>
  retry <id>
  cancel <id>
  stats                  Aggregate queue stats

Remote Click'n'Load (NAS/server scenario; config: ~/.config/hopper/hopperctl.yaml
on Linux/macOS, %APPDATA%\hopper\hopperctl.yaml on Windows for interactive CLI
use — a Windows Service instead reads/writes %ProgramData%\hopper\hopperctl.yaml,
since it runs as LocalSystem, not the installing user; see 'service install'):
  proxy [-addr host:port] [-target URL]
                         Local Click'n'Load relay to a remote hopperd
  watch [-dir PATH]      Watch a directory for new .dlc/.txt files and
                         import them into api_target's hopperd (NOT the
                         login session's server — see api_target below)
  run [-addr ..] [-target ..] [-dir ..]
                         proxy + watch together (default if no command given)
  service install [-target URL] [-api-target URL] [-watch-dir DIR]
                         Write config and register hopperctl as an OS
                         service running 'run' (a real Windows Service on
                         Windows; systemd --user / launchd elsewhere)
  service uninstall      Remove the OS service, keep the config
  status                 Config, reachability, watch dir, service state,
                         proxy TLS certificate

hopperctl.yaml fields:
  target:       hopperd's CnL endpoint (e.g. http://NAS-IP:9666), used by
                'proxy'
  api_target:   hopperd's REST API endpoint (e.g. http://NAS-IP:9667),
                used by 'watch' for /api/v1/import. If empty, derived
                from target by replacing its port with 9667.

  version                Print hopperctl's version
`)
}
