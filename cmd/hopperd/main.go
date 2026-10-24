// Command hopperd is Hopper's main daemon: queue manager, download
// engine, REST API, embedded Web UI, and (optionally) the Click'n'Load
// listener.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.etcd.io/bbolt"

	"github.com/simuuh/hopper-downloader/api"
	"github.com/simuuh/hopper-downloader/internal/account"
	"github.com/simuuh/hopper-downloader/internal/clicknload"
	"github.com/simuuh/hopper-downloader/internal/config"
	"github.com/simuuh/hopper-downloader/internal/dbschema"
	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/extract"
	"github.com/simuuh/hopper-downloader/internal/notify"
	"github.com/simuuh/hopper-downloader/internal/queue"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/internal/settings"
	"github.com/simuuh/hopper-downloader/internal/stats"
	"github.com/simuuh/hopper-downloader/internal/update"
	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/alldebrid"
	"github.com/simuuh/hopper-downloader/plugins/ddownload"
	"github.com/simuuh/hopper-downloader/plugins/debridlink"
	"github.com/simuuh/hopper-downloader/plugins/directhttp"
	"github.com/simuuh/hopper-downloader/plugins/hidecx"
	"github.com/simuuh/hopper-downloader/plugins/katfile"
	"github.com/simuuh/hopper-downloader/plugins/onefichier"
	"github.com/simuuh/hopper-downloader/plugins/premiumize"
	"github.com/simuuh/hopper-downloader/plugins/rapidgator"
	"github.com/simuuh/hopper-downloader/plugins/realdebrid"
	"github.com/simuuh/hopper-downloader/web"
)

// version is set via -ldflags "-X main.version=..." in release builds.
var version = "dev"

// settingsExtractAdapter adapts *settings.Manager to queue.ExtractSettings
// via a func indirection (see run()'s comment on why: queueMgr needs this
// before settingsMgr itself can be constructed).
type settingsExtractAdapter struct {
	get func() settings.Settings
}

func (a settingsExtractAdapter) ExtractEnabled() bool       { return a.get().ExtractEnabled }
func (a settingsExtractAdapter) ExtractDeleteAfter() bool   { return a.get().ExtractDeleteAfter }
func (a settingsExtractAdapter) ExtractPasswords() []string { return a.get().ExtractPasswords }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hopperd:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", envOr("HOPPER_CONFIG_FILE", "hopper.yaml"), "path to hopper.yaml")
	healthcheck := flag.Bool("healthcheck", false, "check a locally running hopperd's /healthz and exit (Docker HEALTHCHECK)")
	setAdminPassword := flag.Bool("set-admin-password", false, "read a new admin password from stdin, store it (secrets file next to the database) and exit; restart hopperd to apply")
	flag.Parse()

	if *healthcheck || *setAdminPassword {
		peek, err := config.Peek(*configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if *healthcheck {
			return runHealthcheck(peek.Server.Port)
		}
		return runSetAdminPassword(peek)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, logLevelVar, err := newLogger(cfg.Logging)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	log.Info("hopperd starting", "version", version, "config_file", *configPath)
	if cfg.ConfigFileMissing {
		log.Info("no config file found, running on defaults and HOPPER_* environment variables", "config_file", *configPath)
	}
	if cfg.GeneratedAdminPassword != "" {
		announceGeneratedPassword(cfg)
		log.Warn("first start: generated an admin password (shown once above); change it with: hopperd -set-admin-password", "secrets_file", cfg.SecretsPath())
	}

	if err := os.MkdirAll(cfg.Downloads.Path, 0o750); err != nil {
		return fmt.Errorf("create downloads directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o750); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}

	db, err := bbolt.Open(cfg.Database.Path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := dbschema.Check(db); err != nil {
		return err
	}

	accounts, err := account.New(db, cfg.Accounts.EncryptionKey, log.With("component", "account"))
	if err != nil {
		return fmt.Errorf("init account manager: %w", err)
	}

	// hidecx needs to search the very registry it's about to become a
	// member of (to re-match a resolved redirect against every other
	// plugin, Real-Debrid included) — the same circular-constructor
	// shape as internal/settings.Manager/queue.Manager (see docs/ARCHITECTURE.md).
	// Resolved the same way: registry is declared first and captured by
	// a closure, then assigned only once NewRegistry has actually run.
	// The closure itself isn't invoked until a real hide.cx link is
	// resolved, long after registry is populated, so the nil in between
	// is never observed.
	//
	// settingsMgr is assigned further below, after queueMgr — its own
	// Hooks need queueMgr.SetParallelFiles, so it can't be built first.
	// Everything that needs live settings earlier (accountPriority, the
	// hidecx resolver, extractSettingsAdapter) closes over the variable,
	// not its value, and is only called once setup has finished.
	var settingsMgr *settings.Manager
	accountPriority := func() []string {
		if settingsMgr == nil {
			return nil
		}
		return settingsMgr.Get().AccountPriority
	}
	var registry *plugins.Registry
	hc := hidecx.New(func(rawURL string) (plugins.Hoster, *plugins.Account, bool) {
		h, ok := queue.MatchHoster(registry.All(), rawURL, accounts, accountPriority())
		if !ok {
			return nil, nil, false
		}
		acc, _ := accounts.AccountFor(h.Name())
		return h, acc, true
	}, log.With("component", "plugin.hidecx"))

	// Registration order is the default account priority (see
	// queue.RankHosters): when plugins with a configured account all
	// match a URL, the one registered last wins — unless the user set
	// account_priority in the Web UI (Accounts), which overrides it. Among
	// plugins without an account (queue.MatchHoster's pass 2) the last
	// registered matching one wins too.
	//
	//  1. directhttp — catch-all fallback; must be registered first so
	//     it never shadows a more specific plugin.
	//  2. hidecx     — link redirector for hide.cx. Owns its domain
	//     exclusively; it implements Expander, which queue.Manager checks
	//     as soon as a URL resolves to it.
	//  3. native hosters (rapidgator, onefichier, ddownload, katfile) —
	//     each needs its own account; ddownload can resolve but not
	//     download (see its doc comment).
	//  4. multi-hosters (premiumize, debridlink, alldebrid, realdebrid) —
	//     registered last so that, by default, a configured multi-hoster
	//     account wins over a native plugin for every host it supports:
	//     Real-Debrid first, then AllDebrid, Debrid-Link, Premiumize. Their
	//     Match
	//     returns false without an account, so they never shadow anything
	//     unconfigured. A multi-hoster that can't serve a link right now
	//     returns plugins.ErrUnsupportedHost and the queue moves on to the
	//     next plugin in priority order.
	registry = plugins.NewRegistry(
		directhttp.New(),
		hc,
		rapidgator.New(accounts, log.With("component", "plugin.rapidgator")),
		onefichier.New(accounts, log.With("component", "plugin.onefichier")),
		ddownload.New(accounts, log.With("component", "plugin.ddownload")),
		katfile.New(accounts, log.With("component", "plugin.katfile")),
		premiumize.New(accounts, log.With("component", "plugin.premiumize")),
		debridlink.New(accounts, log.With("component", "plugin.debridlink")),
		alldebrid.New(accounts, log.With("component", "plugin.alldebrid")),
		realdebrid.New(accounts, log.With("component", "plugin.realdebrid")),
	)

	engine := downloader.New(downloader.Config{
		BaseDir:                  cfg.Downloads.Path,
		DefaultParallelChunks:    cfg.Downloads.ParallelChunks,
		RetryMax:                 cfg.Downloads.RetryMax,
		RetryBackoffBase:         cfg.Downloads.RetryBackoffBase,
		GlobalBandwidthLimit:     cfg.Downloads.BandwidthLimit,
		HosterBandwidthLimits:    hosterBandwidthLimits(cfg),
		HosterMinRequestInterval: hosterMinRequestIntervals(cfg),
	})

	queueStore, err := queue.OpenStore(db)
	if err != nil {
		return fmt.Errorf("open queue store: %w", err)
	}
	sched := scheduler.New()
	statsTracker := stats.NewTracker(db, log.With("component", "stats"))

	// settingsMgr is assigned below, after queueMgr — its own Hooks
	// need queueMgr.SetParallelFiles, so it can't be built first. queueMgr
	// needs a settings-backed ExtractSettings *now*, though, so
	// extractSettingsAdapter closes over the settingsMgr variable rather
	// than its value: by the time anything actually calls these methods
	// (an item completing, well after this function's setup section
	// finishes), settingsMgr is assigned. See settingsExtractAdapter.
	extractor := &extract.Extractor{UnrarPath: cfg.Extract.UnrarPath}
	extractSettingsAdapter := settingsExtractAdapter{get: func() settings.Settings {
		if settingsMgr == nil {
			return settings.Settings{}
		}
		return settingsMgr.Get()
	}}

	notifier := notify.New(notify.Config{
		WebhookURL: cfg.Notify.WebhookURL,
		NtfyURL:    cfg.Notify.NtfyURL,
		NtfyToken:  cfg.Notify.NtfyToken,
	}, nil, log.With("component", "notify"))
	if notifier.Enabled() {
		log.Info("notifications enabled", "webhook", cfg.Notify.WebhookURL != "", "ntfy", cfg.Notify.NtfyURL != "")
	}

	queueMgr, err := queue.New(queueStore, registry, engine, accounts, statsTracker, extractor, extractSettingsAdapter, sched, log.With("component", "queue"), queue.Config{
		ParallelFiles:     cfg.Downloads.ParallelFiles,
		DiskCheckAttempts: cfg.Extract.PartWaitAttempts,
		DiskCheckInterval: cfg.Extract.PartWaitInterval,
		SequentialParts:   cfg.Queue.SequentialParts,
		AutoRetry:         cfg.Queue.AutoRetry,
		AutoRetryDelay:    cfg.Queue.AutoRetryDelay,
		AccountPriority:   accountPriority,
		Notifier:          notifier,
	})
	if err != nil {
		return fmt.Errorf("init queue manager: %w", err)
	}

	// settingsMgr holds the runtime-editable subset of cfg (see PATCH
	// /api/v1/settings): it starts from cfg's values but, once loaded,
	// bbolt is authoritative — a prior settings change survives restart
	// even if hopper.yaml itself hasn't been touched. Hooks push each
	// field into whatever live component owns it; nil hooks (none here)
	// would mean "persisted only, no separate live-push needed" — not
	// the case for any of these, all six have real runtime effect.
	settingsMgr, err = settings.New(db, settings.Settings{
		DownloadPath:       cfg.Downloads.Path,
		ParallelFiles:      cfg.Downloads.ParallelFiles,
		ParallelChunks:     cfg.Downloads.ParallelChunks,
		BandwidthLimit:     cfg.Downloads.BandwidthLimit,
		RetryMax:           cfg.Downloads.RetryMax,
		AutoStartImports:   cfg.Downloads.AutoStartImports,
		DefaultPriority:    "normal",
		LogLevel:           cfg.Logging.Level,
		ExtractEnabled:     cfg.Extract.Enabled,
		ExtractDeleteAfter: cfg.Extract.DeleteAfter,
		ExtractPasswords:   cfg.Extract.Passwords,
	}, settings.Hooks{
		SetDownloadPath:   engine.SetBaseDir,
		SetParallelFiles:  queueMgr.SetParallelFiles,
		SetParallelChunks: engine.SetDefaultParallelChunks,
		SetBandwidthLimit: engine.SetGlobalBandwidthLimit,
		SetRetryMax:       engine.SetRetryMax,
		SetLogLevel: func(level string) error {
			var lvl slog.Level
			if err := lvl.UnmarshalText([]byte(level)); err != nil {
				return err
			}
			logLevelVar.Set(lvl)
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("init settings manager: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	queueMgr.Start(ctx)

	// Keep cached account status (premium expiry, traffic left) fresh so the
	// Web UI's "expires soon" hint is accurate and an expired account is
	// noticed before a download fails on it. Once shortly after startup,
	// then every accountRevalidateInterval.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		accounts.RevalidateAll(ctx, registry.All())
	}()
	sched.Every(ctx, accountRevalidateInterval, func(ctx context.Context) { accounts.RevalidateAll(ctx, registry.All()) })

	// The daily update check runs only while the user has it turned on
	// (settings.UpdateCheck, off by default): once shortly after startup
	// (the result is only kept in memory), then whenever the hourly tick
	// finds the last check older than updateCheckInterval — so turning it
	// on takes effect within the hour. The Web UI's "Check now" goes
	// through the API instead.
	updates := update.New(version, update.DefaultURL, nil, log.With("component", "update"))
	checkUpdates := func(ctx context.Context) {
		if settingsMgr.Get().UpdateCheck {
			updates.CheckIfDue(ctx, updateCheckInterval)
		}
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		checkUpdates(ctx)
	}()
	sched.Every(ctx, time.Hour, checkUpdates)

	webFS, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		return fmt.Errorf("mount embedded web ui: %w", err)
	}

	apiSrv := api.New(api.Config{
		Host:               cfg.Server.Host,
		Port:               cfg.Server.Port,
		JWTSecret:          cfg.Server.JWTSecret,
		AdminPasswordHash:  cfg.Server.AdminPasswordHash,
		TokenTTL:           cfg.Server.TokenTTL,
		CORSAllowedOrigins: cfg.Server.CORSAllowedOrigins,
		RateLimitRPM:       cfg.RateLimit.RequestsPerMinute,
		RateLimitBurst:     cfg.RateLimit.Burst,
		ReadHeaderTimeout:  cfg.Server.ReadHeaderTimeout,
		DLCEnabled:         cfg.DLC.Enabled,
		DLCAPIURL:          cfg.DLC.JDAPIURL,
		ReferralLinks:      cfg.Accounts.ReferralLinks,
		ConfigView:         redactedConfigView(cfg),
		Version:            version,
		Notifier:           notifier,
		Updates:            updates,
	}, queueMgr, accounts, registry, settingsMgr, statsTracker, &http.Client{Timeout: 30 * time.Second}, webFS, log.With("component", "api"))

	var cnlSrv *clicknload.Server
	if cfg.ClickNLoad.Enabled {
		trustedNets, err := cfg.TrustedNets()
		if err != nil {
			return fmt.Errorf("parse clicknload trusted CIDRs: %w", err)
		}
		cnlSrv = clicknload.New(queueMgr, clicknload.Config{
			Host:        cfg.ClickNLoad.Host,
			Port:        cfg.ClickNLoad.Port,
			TrustedNets: trustedNets,
		}, log.With("component", "clicknload"))
	}

	errCh := make(chan error, 2)
	go func() { errCh <- apiSrv.ListenAndServe() }()
	if cnlSrv != nil {
		go func() { errCh <- cnlSrv.ListenAndServe() }()
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			log.Error("server failed", "error", err)
		}
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := apiSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("api shutdown error", "error", err)
	}
	if cnlSrv != nil {
		if err := cnlSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("clicknload shutdown error", "error", err)
		}
	}
	if err := queueMgr.Shutdown(shutdownCtx); err != nil {
		log.Error("queue shutdown error", "error", err)
	}
	notifier.Wait()

	log.Info("hopperd stopped")
	return nil
}

// runHealthcheck hits a locally running hopperd's /healthz. It exists for
// Docker's HEALTHCHECK: the distroless runtime image has no shell,
// curl, or wget, so the check has to be the application binary itself.
func runHealthcheck(port int) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func hosterBandwidthLimits(cfg *config.Config) map[string]int64 {
	out := make(map[string]int64, len(cfg.Hosters))
	for name, h := range cfg.Hosters {
		if h.BandwidthLimit > 0 {
			out[name] = h.BandwidthLimit
		}
	}
	return out
}

func hosterMinRequestIntervals(cfg *config.Config) map[string]time.Duration {
	out := make(map[string]time.Duration, len(cfg.Hosters))
	for name, h := range cfg.Hosters {
		if h.MinRequestInterval > 0 {
			out[name] = h.MinRequestInterval
		}
	}
	return out
}

// redactedConfigView returns a JSON-safe snapshot of cfg for GET
// /api/v1/config: every secret (jwt_secret, admin_password_hash,
// accounts.encryption_key) is deliberately omitted.
func redactedConfigView(cfg *config.Config) any {
	return map[string]any{
		"server": map[string]any{
			"host":                 cfg.Server.Host,
			"port":                 cfg.Server.Port,
			"cors_allowed_origins": cfg.Server.CORSAllowedOrigins,
			"token_ttl":            cfg.Server.TokenTTL.String(),
		},
		"downloads": map[string]any{
			"path":            cfg.Downloads.Path,
			"parallel_files":  cfg.Downloads.ParallelFiles,
			"parallel_chunks": cfg.Downloads.ParallelChunks,
			"bandwidth_limit": cfg.Downloads.BandwidthLimit,
			"retry_max":       cfg.Downloads.RetryMax,
		},
		"clicknload": map[string]any{
			"enabled":       cfg.ClickNLoad.Enabled,
			"host":          cfg.ClickNLoad.Host,
			"port":          cfg.ClickNLoad.Port,
			"trusted_cidrs": cfg.ClickNLoad.TrustedCIDRs,
		},
		"ratelimit": map[string]any{
			"requests_per_minute": cfg.RateLimit.RequestsPerMinute,
			"burst":               cfg.RateLimit.Burst,
		},
		"dlc": map[string]any{
			"enabled": cfg.DLC.Enabled,
		},
		"version": version,
	}
}

// newLogger builds the process logger around a *slog.LevelVar rather than
// a fixed slog.Level, so a runtime settings change (see internal/settings
// and POST /api/v1/settings) can raise or lower verbosity by calling
// levelVar.Set — slog.Handler reads the Leveler on every log call, not
// just at construction, so this takes effect immediately with no
// restart. The returned LevelVar is otherwise just cfg.Level's parsed
// value; callers that don't need runtime changes can ignore it.
func newLogger(cfg config.LoggingConfig) (*slog.Logger, *slog.LevelVar, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, nil, fmt.Errorf("parse log level: %w", err)
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(level)
	opts := &slog.HandlerOptions{Level: levelVar}

	var handler slog.Handler
	switch cfg.Format {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler), levelVar, nil
}

// accountRevalidateInterval is how often every enabled hoster account is
// re-validated in the background (see account.Manager.RevalidateAll).
const accountRevalidateInterval = 12 * time.Hour

// updateCheckInterval is how often the opt-in update check asks GitHub
// for the latest release.
const updateCheckInterval = 24 * time.Hour

// announceGeneratedPassword prints the generated first-start admin
// password as a plain, easy-to-find block (docker compose logs hopperd).
// Deliberately stderr and not the structured log: it must not end up in a
// log aggregator, and appears exactly once — it is not stored anywhere.
func announceGeneratedPassword(cfg *config.Config) {
	fmt.Fprintf(os.Stderr, "\n"+
		"==============================================================\n"+
		"  Hopper first start — Web UI login\n"+
		"    user:     admin\n"+
		"    password: %s\n"+
		"  Shown only once. Change it any time:\n"+
		"    docker compose exec -it hopperd hopperd -set-admin-password\n"+
		"==============================================================\n\n",
		cfg.GeneratedAdminPassword)
}

// runSetAdminPassword reads a new admin password from stdin and stores it
// in the secrets file (hopperd -set-admin-password).
func runSetAdminPassword(cfg *config.Config) error {
	if cfg.Server.AdminPasswordHash != "" {
		fmt.Fprintln(os.Stderr, "warning: an admin password hash is configured explicitly (HOPPER_ADMIN_PASSWORD_HASH or hopper.yaml); it takes precedence over the one stored here.")
	}
	fmt.Fprint(os.Stderr, "New admin password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("read password: %w", err)
	}
	if err := config.SetAdminPassword(cfg.SecretsPath(), strings.TrimRight(line, "\r\n")); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Saved. Restart to apply: docker compose restart hopperd")
	return nil
}
