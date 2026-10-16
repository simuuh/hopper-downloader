package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// statusCheckTimeout bounds each individual reachability probe `status`
// performs, so a hung/unreachable endpoint can't make the command itself
// hang — every check is independent and best-effort.
const statusCheckTimeout = 3 * time.Second

// cmdStatus prints a diagnostic snapshot: config, whether the local proxy
// and the configured target answer, whether a login session exists and
// its server is reachable, whether the watch directory exists, and
// whether the OS service is registered. It never returns a non-nil error
// for an unreachable endpoint — that's exactly the kind of thing this
// command exists to report, not fail on.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	fmt.Printf("config:      %s\n", cfg.Path)
	fmt.Printf("  target:        %s\n", orNotSet(cfg.Target))
	fmt.Printf("  api_target:    %s\n", apiTargetDisplay(cfg))
	fmt.Printf("  watch_dir:     %s\n", cfg.WatchDir)
	fmt.Printf("  log_level:     %s\n", cfg.LogLevel)
	fmt.Printf("  delete_after:  %t\n", cfg.DeleteAfter)

	fmt.Println("proxy (127.0.0.1:9666, HTTPS):", checkProxyURL("https://"+defaultProxyAddr+"/flash/"))
	fmt.Println("proxy (127.0.0.1:9666, HTTP): ", checkURL("http://"+defaultProxyAddr+"/flash/"))
	fmt.Println("  cert:", certDisplay())

	if cfg.Target != "" {
		fmt.Println("target:", checkURL(joinURL(cfg.Target, "/flash/")))
	} else {
		fmt.Println("target: not configured")
	}

	if apiTarget := cfg.ResolvedAPITarget(); apiTarget != "" {
		fmt.Println("api_target:", checkURL(joinURL(apiTarget, "/api/v1/version")))
	} else {
		fmt.Println("api_target: not configured")
	}

	fmt.Println("browsers: open https://127.0.0.1:9666/flash/ once per browser profile to trust the proxy's certificate")

	if sess, err := loadSession(); err == nil {
		fmt.Printf("login session: %s\n", sess.Server)
		fmt.Println("  api:", checkURL(joinURL(sess.Server, "/api/v1/version")))
	} else {
		fmt.Println("login session: none (run `hopperctl login`)")
	}

	if info, err := os.Stat(cfg.WatchDir); err == nil && info.IsDir() {
		fmt.Printf("watch dir: %s (exists)\n", cfg.WatchDir)
	} else {
		fmt.Printf("watch dir: %s (missing)\n", cfg.WatchDir)
	}

	fmt.Println("service:", checkService())
	return nil
}

// apiTargetDisplay shows the configured api_target, or the value it
// would resolve to via Config.ResolvedAPITarget when unset — so `status`
// never just prints an empty field for the common case of "derive it
// from target".
func apiTargetDisplay(cfg *Config) string {
	if cfg.APITarget != "" {
		return cfg.APITarget
	}
	if d := deriveAPITarget(cfg.Target); d != "" {
		return d + " (derived from target)"
	}
	return "(not set)"
}

// certDisplay reports the proxy's self-signed TLS certificate's path and
// expiry, so `status` gives an operator everything needed for the
// one-time-per-browser trust step (see docs/REMOTE_CLICKNLOAD.md's certificate
// section) without them having to go spelunking for the file themselves.
func certDisplay() string {
	dir, err := certDir()
	if err != nil {
		return fmt.Sprintf("unknown (%v)", err)
	}
	certPath := filepath.Join(dir, certFileName)
	data, err := os.ReadFile(certPath)
	if err != nil {
		return certPath + " (not yet generated — starts on first `proxy`/`run`)"
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return certPath + " (unreadable)"
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certPath + " (unreadable)"
	}
	return fmt.Sprintf("%s (expires %s)", certPath, leaf.NotAfter.Format("2006-01-02"))
}

func orNotSet(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

func checkURL(url string) string {
	client := &http.Client{Timeout: statusCheckTimeout}
	resp, err := client.Get(url) //nolint:noctx // short-lived, timeout-bounded diagnostic probe
	if err != nil {
		return fmt.Sprintf("unreachable (%v)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return fmt.Sprintf("reachable (HTTP %d)", resp.StatusCode)
}

// checkProxyURL probes the local proxy's own HTTPS listener specifically
// — unlike checkURL, it skips certificate verification, since the whole
// point of ensureCert's self-signed certificate is that it isn't (and
// can't be) in any system trust store; failing this diagnostic check
// over an untrusted-but-otherwise-healthy cert would defeat the purpose
// of running it. This is the one place in hopperctl that's safe to do
// that: the request never leaves the loopback interface, and its only
// purpose is "does anything answer here at all", not verifying the
// identity of a remote party.
func checkProxyURL(url string) string {
	client := &http.Client{
		Timeout: statusCheckTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: probing our own self-signed loopback-only cert, not a remote party's identity
		},
	}
	resp, err := client.Get(url) //nolint:noctx // short-lived, timeout-bounded diagnostic probe
	if err != nil {
		return fmt.Sprintf("unreachable (%v)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return fmt.Sprintf("reachable HTTPS ✅ (HTTP %d)", resp.StatusCode)
}

// checkService reports whether the OS service/task hopperctl registers
// via `service install` currently exists, using each platform's own
// query command rather than just checking for the definition file, so a
// stale/removed registration reads as "not registered" too.
func checkService() string {
	switch runtime.GOOS {
	case "windows":
		if err := exec.Command("sc", "query", windowsServiceName).Run(); err != nil { //nolint:gosec // G204: fixed binary name, constant args
			return "not registered"
		}
		return "registered (Windows Service: " + windowsServiceName + ")"
	case "darwin":
		out, err := exec.Command("launchctl", "list").Output() //nolint:gosec // G204: fixed binary name, constant args
		if err != nil || !strings.Contains(string(out), darwinLabel) {
			return "not registered"
		}
		return "registered (launchd: " + darwinLabel + ")"
	default:
		out, err := exec.Command("systemctl", "--user", "is-active", linuxServiceName).Output() //nolint:gosec // G204: fixed binary name, constant args

		state := strings.TrimSpace(string(out))
		if err != nil && state == "" {
			return "not registered"
		}
		return fmt.Sprintf("registered (systemd --user: %s, %s)", linuxServiceName, state)
	}
}
