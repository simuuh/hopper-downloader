//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
)

// serviceLogPath is where the Windows Service redirects hopperctl's logs
// to — a service has no console to write to (unlike an interactively
// run `hopperctl run`), and Windows Event Log integration is out of
// scope here. %ProgramData%\hopper\, the same machine-wide directory
// serviceLogPath's sibling ctlConfigPath resolves to for a service (see
// its doc comment): sc.exe runs the service as LocalSystem, whose own
// %APPDATA% (what this used to resolve to via os.UserConfigDir) is a
// machine profile directory nobody would think to look in, defeating
// the entire point of a findable log file. This function is itself only
// ever called from newServiceLogger, itself only ever called from
// hopperServiceHandler.Execute — i.e. only while genuinely running as a
// service — so there's no separate "interactive CLI" branch needed here
// the way ctlConfigPath has. The login session file
// (session.go's sessionPath) is deliberately not part of this: logging
// in is an interactive-user action, not something a service does on its
// own, so it stays under the installing user's own %APPDATA% regardless.
func serviceLogPath() string {
	return filepath.Join(programDataDir(), "hopperctl.log")
}

// newServiceLogger opens (or creates) the service log file in append
// mode — never truncated, so a service restart (crash, reboot, `sc
// stop`+`sc start`) doesn't discard prior log history the way a plain
// os.Create would.
func newServiceLogger() (*slog.Logger, *os.File, error) {
	path := serviceLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	return slog.New(slog.NewTextHandler(f, nil)), f, nil
}

// isWindowsService reports whether this process was launched by the
// Windows Service Control Manager rather than a terminal — main uses
// this to decide whether to dispatch into runAsWindowsService (blocking,
// SCM-driven lifecycle) or the normal interactive CLI dispatch in run().
func isWindowsService() bool {
	v, err := svc.IsWindowsService()
	return err == nil && v
}

// runAsWindowsService registers hopperServiceHandler with the SCM and
// blocks until the service is asked to stop — svc.Run itself blocks the
// calling goroutine for the service's entire lifetime, handing control
// to hopperServiceHandler.Execute.
func runAsWindowsService() error {
	return svc.Run(windowsServiceName, hopperServiceHandler{})
}

// hopperServiceHandler implements svc.Handler, running the same
// proxy+watch logic `hopperctl run` uses interactively (via
// runProxyAndWatch, cmd/hopperctl/run.go) for the service's lifetime,
// translating SCM stop/shutdown requests into context cancellation for a
// graceful shutdown — the identical shutdown path runProxy/runWatch
// already use for Ctrl+C/SIGTERM.
type hopperServiceHandler struct{}

func (hopperServiceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	log, logFile, err := newServiceLogger()
	if err != nil {
		// No log file to write this failure to — nothing more we can
		// do but report it as a service-specific exit code.
		return true, 1
	}
	defer func() { _ = logFile.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := loadConfig()
	if err != nil {
		log.Error("hopperctl: service failed to load config", "error", err)
		return true, 1
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- runProxyAndWatch(ctx, cfg, defaultProxyAddr, log)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	log.Info("hopperctl: Windows Service started", "service", windowsServiceName)

	for {
		select {
		case err := <-runErr:
			if err != nil {
				log.Error("hopperctl: service stopped with error", "error", err)
				changes <- svc.Status{State: svc.Stopped}
				return true, 1
			}
			changes <- svc.Status{State: svc.Stopped}
			return false, 0
		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				changes <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				log.Info("hopperctl: service received stop/shutdown request")
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				// Wait for runProxyAndWatch's graceful shutdown rather
				// than returning immediately — same shutdown contract
				// runProxy's own ctx.Done() handling already provides
				// for Ctrl+C/SIGTERM.
				if err := <-runErr; err != nil {
					log.Error("hopperctl: service stopped with error", "error", err)
					changes <- svc.Status{State: svc.Stopped}
					return true, 1
				}
				changes <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}
