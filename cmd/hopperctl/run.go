package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
)

// cmdRun starts the CnL proxy and folder watcher together — the default
// when hopperctl is invoked with no subcommand, and what `service
// install` registers as the long-running process. Unlike `hopperctl
// watch`, a missing login session doesn't fail the whole command: a
// NAS-only CnL relay (no .dlc folder watch wanted) is a legitimate setup,
// so watch is disabled with a logged warning instead.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	addr := fs.String("addr", defaultProxyAddr, "local address for the CnL proxy")
	target := fs.String("target", "", "hopperd CnL endpoint (overrides hopperctl.yaml)")
	dir := fs.String("dir", "", "directory to watch (overrides hopperctl.yaml)")
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
	if *dir != "" {
		cfg.WatchDir = *dir
	}

	log := newLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runProxyAndWatch(ctx, cfg, *addr, log)
}

// runProxyAndWatch runs the CnL proxy and folder watcher together until
// ctx is cancelled, exactly as `hopperctl run` does interactively. It's
// factored out of cmdRun so a Windows Service (see winservice_windows.go)
// can drive the identical logic under svc.Handler's lifecycle instead of
// a terminal's — the only things that differ between the two are how ctx
// gets cancelled (Ctrl+C/SIGTERM vs. an SCM stop request) and where log
// writes go (stderr vs. %APPDATA%\hopper\hopperctl.log).
func runProxyAndWatch(ctx context.Context, cfg *Config, addr string, log *slog.Logger) error {
	tlsCertDir, err := certDir()
	if err != nil {
		return fmt.Errorf("resolve TLS certificate directory: %w", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if cfg.Target == "" {
			log.Warn("hopperctl: proxy has no target configured, refusing to start it (set `target:` in " + cfg.Path + " or pass -target)")
			return
		}
		errs[0] = runProxy(ctx, addr, cfg.Target, tlsCertDir, log)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		sess, err := loadSession()
		if err != nil {
			log.Warn("hopperctl: no login session, folder watch disabled (run `hopperctl login` to enable it)", "error", err)
			return
		}
		apiTarget := cfg.ResolvedAPITarget()
		if apiTarget == "" {
			log.Warn("hopperctl: no api_target (or target to derive it from) configured, folder watch disabled")
			return
		}
		errs[1] = runWatch(ctx, cfg.WatchDir, cfg.DeleteAfter, apiTarget, sess.Token, log)
	}()

	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return fmt.Errorf("hopperctl run: %w", e)
		}
	}
	return nil
}
