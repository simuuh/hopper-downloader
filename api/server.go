// Package api implements Hopper's REST API and WebSocket live-update
// endpoint: JWT-authenticated, versioned under /api/v1, plus the
// Click'n'Load-adjacent /healthz probe and the embedded Web UI.
package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/simuuh/hopper-downloader/internal/account"
	"github.com/simuuh/hopper-downloader/internal/queue"
	"github.com/simuuh/hopper-downloader/internal/settings"
	"github.com/simuuh/hopper-downloader/internal/stats"
	"github.com/simuuh/hopper-downloader/plugins"
)

// QueueService is the subset of *queue.Manager the API needs. Depending
// on the interface (not the concrete type) keeps this package unit
// testable without a real downloader/database stack.
type QueueService interface {
	AddLink(rawURL string, opts queue.AddOptions) (*queue.Item, error)
	AddPackage(name string, urls []string, opts queue.AddOptions) (*queue.Package, []*queue.Item, error)
	AddLinks(links []queue.LinkInput, name string, opts queue.AddOptions) (*queue.AddResult, error)
	List() []*queue.Item
	Get(id string) (*queue.Item, bool)
	ListPackages() []*queue.Package
	Pause(id string) error
	Resume(id string) error
	Retry(id string) error
	Cancel(id string) error
	CancelMany(ids []string) (deleted []string, failed []string)
	RetryExtraction(id, password string) error
	SolveCaptcha(id, solution string) error
	Reorder(ids []string) error
	PauseAll() error
	ResumeAll() error
	SetPriority(id string, p queue.Priority) error
	EditItem(id string, e queue.ItemEdit) (*queue.Item, error)
	AddToPackage(id string, links []queue.LinkInput, opts queue.AddOptions) (*queue.AddResult, error)
	SetPackagePath(id, path string) (*queue.Package, error)
	RenamePackage(id, name string) (*queue.Package, error)
	ApplyPackageAction(id string, action queue.PackageAction) (int, error)
	ApplyAction(ids []string, action queue.PackageAction) (int, error)
	Subscribe() (<-chan queue.Event, func())
}

// SettingsService is the subset of *settings.Manager the API needs.
type SettingsService interface {
	Get() settings.Settings
	Apply(u settings.Update) (settings.Settings, error)
}

// StatsService is the subset of *stats.Tracker the API needs.
type StatsService interface {
	Session() stats.SessionStats
	Today() stats.DailyStats
	Period(days int) stats.AggregateStats
	AllTime() stats.AllTimeStats
}

// AccountService is the subset of *account.Manager the API needs.
type AccountService interface {
	Add(hosterKey, username, password string, extra map[string]string) (*account.Account, error)
	List() []*account.Account
	Get(id string) (*account.Account, bool)
	SetEnabled(id string, enabled bool) error
	Delete(id string) error
	// Credentials decrypts and returns the plaintext credentials for one
	// exact account id, for (re-)validation flows.
	Credentials(id string) (*plugins.Account, error)
	// SetValidation persists the cached result of validating an
	// account's credentials against its hoster's API.
	SetValidation(id string, info *plugins.AccountInfo) (*account.Account, error)
	// UpdateCredentials replaces an account's credentials in place.
	UpdateCredentials(id, username, password string, extra map[string]string) (*account.Account, error)
}

// PluginService is the subset of *plugins.Registry the API needs.
// *plugins.Registry already implements this exactly.
type PluginService interface {
	All() []plugins.Hoster
}

// Config configures the API server. It intentionally holds no reference
// to internal/config.Config, so this package stays independent of the
// YAML/env config layer.
type Config struct {
	Host               string
	Port               int
	JWTSecret          string
	AdminPasswordHash  string
	TokenTTL           time.Duration
	CORSAllowedOrigins []string
	RateLimitRPM       int
	RateLimitBurst     int
	ReadHeaderTimeout  time.Duration
	// DLCEnabled gates .dlc file imports (config.DLC.Enabled); plaintext
	// link-list imports are always available.
	DLCEnabled bool
	// DLCAPIURL is the JDownloader key-exchange endpoint used to decrypt
	// .dlc containers (config.DLC.JDAPIURL).
	DLCAPIURL string
	// ReferralLinks selects plugins.PremiumLink's referral variant for the
	// premium_url GET /api/v1/plugins reports (accounts.referral_links).
	ReferralLinks bool
	// ConfigView is a pre-redacted (no secrets) snapshot of the active
	// configuration, returned as-is by GET /api/v1/config.
	ConfigView any
	// Version is the running build's version string (main.version, set
	// via -ldflags in release builds; "dev" otherwise), returned by the
	// public GET /api/v1/version.
	Version string
	// Notifier backs GET /api/v1/notify and POST /api/v1/notify/test. May
	// be nil.
	Notifier Notifier
	// Updates backs GET /api/v1/update and POST /api/v1/update/check. May
	// be nil.
	Updates UpdateChecker
}

// Server serves Hopper's REST API, WebSocket endpoint, and (if webUI is
// non-nil) the embedded Web UI.
type Server struct {
	cfg           Config
	queue         QueueService
	accounts      AccountService
	plugins       PluginService
	settings      SettingsService
	stats         StatsService
	log           *slog.Logger
	webUI         fs.FS
	dlcClient     *http.Client
	limiters      *ipLimiterStore
	loginLimiters *ipLimiterStore
	upgrader      websocket.Upgrader
	srv           *http.Server
}

// New builds a Server. Call ListenAndServe to start it.
func New(cfg Config, q QueueService, acc AccountService, pl PluginService, st SettingsService, statsSvc StatsService, dlcClient *http.Client, webUI fs.FS, log *slog.Logger) *Server {
	if cfg.RateLimitRPM < 1 {
		cfg.RateLimitRPM = 120
	}
	if cfg.RateLimitBurst < 1 {
		cfg.RateLimitBurst = 30
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 24 * time.Hour
	}
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	if dlcClient == nil {
		dlcClient = &http.Client{Timeout: 30 * time.Second}
	}

	s := &Server{
		cfg:           cfg,
		queue:         q,
		accounts:      acc,
		plugins:       pl,
		settings:      st,
		stats:         statsSvc,
		log:           log,
		webUI:         webUI,
		dlcClient:     dlcClient,
		limiters:      newIPLimiterStore(cfg.RateLimitRPM, cfg.RateLimitBurst),
		loginLimiters: newIPLimiterStore(loginAttemptsPerMinute, loginBurst),
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// Echoed back so a browser that authenticated via the
		// Sec-WebSocket-Protocol header (see wsSubprotocol) accepts the
		// handshake.
		Subprotocols: []string{wsSubprotocol},
		CheckOrigin:  s.checkWSOrigin,
	}
	return s
}

// Handler builds the full HTTP handler, including all middleware. It is
// exported mainly for tests, which exercise it directly without binding a
// real listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.Handle("POST /api/v1/auth/refresh", s.auth(http.HandlerFunc(s.handleRefreshToken)))

	mux.Handle("POST /api/v1/links", s.auth(http.HandlerFunc(s.handleAddLink)))
	mux.Handle("POST /api/v1/links/bulk", s.auth(http.HandlerFunc(s.handleAddLinks)))
	mux.Handle("POST /api/v1/packages", s.auth(http.HandlerFunc(s.handleAddPackage)))
	mux.Handle("GET /api/v1/packages", s.auth(http.HandlerFunc(s.handleListPackages)))

	mux.Handle("GET /api/v1/queue", s.auth(http.HandlerFunc(s.handleListQueue)))
	mux.Handle("PUT /api/v1/queue/order", s.auth(http.HandlerFunc(s.handleReorder)))
	mux.Handle("POST /api/v1/queue/actions", s.auth(http.HandlerFunc(s.handleQueueActions)))
	mux.Handle("POST /api/v1/queue/pause-all", s.auth(http.HandlerFunc(s.handlePauseAll)))
	mux.Handle("POST /api/v1/queue/resume-all", s.auth(http.HandlerFunc(s.handleResumeAll)))
	mux.Handle("GET /api/v1/queue/{id}", s.auth(http.HandlerFunc(s.handleGetQueueItem)))
	mux.Handle("PATCH /api/v1/queue/{id}", s.auth(http.HandlerFunc(s.handlePatchQueueItem)))
	mux.Handle("POST /api/v1/queue/{id}/pause", s.auth(http.HandlerFunc(s.handlePause)))
	mux.Handle("POST /api/v1/queue/{id}/resume", s.auth(http.HandlerFunc(s.handleResume)))
	mux.Handle("POST /api/v1/queue/{id}/retry", s.auth(http.HandlerFunc(s.handleRetry)))
	mux.Handle("POST /api/v1/queue/{id}/extract", s.auth(http.HandlerFunc(s.handleRetryExtract)))
	mux.Handle("POST /api/v1/queue/{id}/captcha", s.auth(http.HandlerFunc(s.handleSolveCaptcha)))
	mux.Handle("DELETE /api/v1/queue/{id}", s.auth(http.HandlerFunc(s.handleCancel)))
	mux.Handle("DELETE /api/v1/queue", s.auth(http.HandlerFunc(s.handleBulkCancel)))

	mux.Handle("PATCH /api/v1/packages/{id}", s.auth(http.HandlerFunc(s.handlePatchPackage)))
	mux.Handle("POST /api/v1/packages/{id}/links", s.auth(http.HandlerFunc(s.handleAddToPackage)))
	mux.Handle("POST /api/v1/packages/{id}/{action}", s.auth(http.HandlerFunc(s.handlePackageAction)))
	mux.Handle("DELETE /api/v1/packages/{id}", s.auth(http.HandlerFunc(s.handleDeletePackage)))

	mux.Handle("GET /api/v1/settings", s.auth(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("POST /api/v1/settings", s.auth(http.HandlerFunc(s.handleUpdateSettings)))

	mux.Handle("GET /api/v1/plugins", s.auth(http.HandlerFunc(s.handleListPlugins)))

	mux.Handle("GET /api/v1/accounts", s.auth(http.HandlerFunc(s.handleListAccounts)))
	mux.Handle("POST /api/v1/accounts", s.auth(http.HandlerFunc(s.handleAddAccount)))
	mux.Handle("POST /api/v1/accounts/{id}/enabled", s.auth(http.HandlerFunc(s.handleSetAccountEnabled)))
	mux.Handle("POST /api/v1/accounts/{id}/validate", s.auth(http.HandlerFunc(s.handleValidateAccount)))
	mux.Handle("DELETE /api/v1/accounts/{id}", s.auth(http.HandlerFunc(s.handleDeleteAccount)))
	mux.Handle("PUT /api/v1/accounts/{id}", s.auth(http.HandlerFunc(s.handleUpdateAccount)))

	mux.Handle("GET /api/v1/config", s.auth(http.HandlerFunc(s.handleGetConfig)))
	mux.Handle("GET /api/v1/notify", s.auth(http.HandlerFunc(s.handleNotifyStatus)))
	mux.Handle("POST /api/v1/notify/test", s.auth(http.HandlerFunc(s.handleNotifyTest)))
	mux.Handle("GET /api/v1/update", s.auth(http.HandlerFunc(s.handleUpdateStatus)))
	mux.Handle("POST /api/v1/update/check", s.auth(http.HandlerFunc(s.handleUpdateCheck)))
	mux.Handle("GET /api/v1/stats", s.auth(http.HandlerFunc(s.handleStats)))
	mux.Handle("POST /api/v1/import", s.auth(http.HandlerFunc(s.handleImport)))

	mux.Handle("GET /ws", s.wsAuth(http.HandlerFunc(s.handleWebSocket)))

	if s.webUI != nil {
		mux.Handle("/", http.FileServer(http.FS(s.webUI)))
	}

	return s.securityHeaders(s.cors(s.rateLimit(mux)))
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// handleVersion is deliberately unauthenticated — same rationale as
// /healthz: it's harmless, non-sensitive build metadata, and the Web UI
// fetches it once on load before there's necessarily a reason to force
// an auth round-trip for it.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
}

// ListenAndServe starts the listener and blocks until it stops. A clean
// Shutdown returns nil rather than http.ErrServerClosed.
func (s *Server) ListenAndServe() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		// Closes idle keep-alive connections. No ReadTimeout/WriteTimeout:
		// they would also cut long-lived WebSocket streams and large
		// imports on slow links; request bodies are size-capped instead.
		IdleTimeout: 2 * time.Minute,
	}
	s.log.Info("api: listening", "addr", addr)
	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the listener.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}
