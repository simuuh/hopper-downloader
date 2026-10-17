// Package config loads and validates Hopper's YAML configuration, with
// environment variable overrides following 12-factor conventions.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root Hopper configuration.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Downloads  DownloadsConfig  `yaml:"downloads"`
	ClickNLoad ClickNLoadConfig `yaml:"clicknload"`
	Accounts   AccountsConfig   `yaml:"accounts"`
	RateLimit  RateLimitConfig  `yaml:"ratelimit"`
	Logging    LoggingConfig    `yaml:"logging"`
	Database   DatabaseConfig   `yaml:"database"`
	DLC        DLCConfig        `yaml:"dlc"`
	Extract    ExtractConfig    `yaml:"extract"`
	Queue      QueueConfig      `yaml:"queue"`
	Notify     NotifyConfig     `yaml:"notify"`
	// Hosters holds optional per-hoster overrides, keyed by plugin Name()
	// (e.g. "rapidgator"). Absent entries mean "no per-hoster limit".
	Hosters map[string]HosterConfig `yaml:"hosters"`

	// ConfigFileMissing is set when Load found no config file and ran on
	// defaults plus HOPPER_* environment variables (the Docker image's
	// zero-config mode).
	ConfigFileMissing bool `yaml:"-"`
	// GeneratedAdminPassword is the plaintext admin password created on
	// this start because none was configured (see ensureSecrets); empty
	// otherwise. Shown once in the log, never stored.
	GeneratedAdminPassword string `yaml:"-"`
}

// HosterConfig overrides bandwidth/rate behavior for one hoster, e.g. to
// respect a free-tier account's advertised wait time between downloads.
type HosterConfig struct {
	BandwidthLimit     int64         `yaml:"bandwidth_limit"` // bytes/s, 0 = unlimited
	MinRequestInterval time.Duration `yaml:"min_request_interval"`
}

// ServerConfig controls the REST API / Web UI HTTP listener.
type ServerConfig struct {
	Host      string        `yaml:"host"`
	Port      int           `yaml:"port"`
	JWTSecret string        `yaml:"jwt_secret"`
	TokenTTL  time.Duration `yaml:"token_ttl"`
	// AdminPasswordHash is a bcrypt hash (see `hopperctl hash-password`)
	// checked by POST /api/v1/auth/login. Hopper has exactly one
	// operator identity ("admin"); there is no multi-user account store.
	AdminPasswordHash  string        `yaml:"admin_password_hash"`
	CORSAllowedOrigins []string      `yaml:"cors_allowed_origins"`
	ReadHeaderTimeout  time.Duration `yaml:"read_header_timeout"`
	TLSCertFile        string        `yaml:"tls_cert_file"`
	TLSKeyFile         string        `yaml:"tls_key_file"`
}

// DownloadsConfig controls the download engine.
type DownloadsConfig struct {
	Path             string        `yaml:"path"`
	ParallelFiles    int           `yaml:"parallel_files"`
	ParallelChunks   int           `yaml:"parallel_chunks"`
	BandwidthLimit   int64         `yaml:"bandwidth_limit"` // bytes/s, 0 = unlimited
	RetryMax         int           `yaml:"retry_max"`
	RetryBackoffBase time.Duration `yaml:"retry_backoff_base"`
	// AutoStartImports controls the initial state of items added via the
	// .dlc/.txt import (POST /api/v1/import, also used by the browser
	// extension and hopperctl watch): true (the default) queues them to
	// start right away; false adds them paused for review first (the Web
	// UI then offers "Start"). Runtime-editable in Settings. Links added
	// directly via POST /api/v1/links are unaffected. Click'n'Load ignores
	// this setting: any web page can submit to that listener, so its links
	// are always added paused and started by the user in the Web UI.
	AutoStartImports bool `yaml:"auto_start_imports"`
}

// ExtractConfig controls automatic post-download archive extraction
// (internal/extract). Disabled by default: it shells out to an external
// unrar binary for .rar archives, which isn't present in every
// environment and isn't something Hopper should do to a user's
// downloads without opt-in.
type ExtractConfig struct {
	Enabled bool `yaml:"enabled"`
	// DeleteAfter removes the archive (all parts, for multi-part RAR)
	// once extraction succeeds. Never applies on a failed extraction.
	DeleteAfter bool `yaml:"delete_after"`
	// UnrarPath is the unrar binary to invoke for .rar archives; .zip and
	// .tar.{gz,bz2} are handled with the standard library and don't need
	// it. Only resolved/required when Enabled and a .rar is encountered.
	UnrarPath string `yaml:"unrar_path"`
	// Passwords are tried, in order, after the package's own password (if
	// any). Hopper ships no built-in password list.
	Passwords []string `yaml:"passwords"`
	// PartWaitAttempts/PartWaitInterval bound how long queue.Manager
	// waits for every part of a multi-part archive to actually become
	// visible on disk (StateDone doesn't guarantee that on every
	// filesystem — see queue.Manager's diskCheckAttempts doc comment)
	// before giving up and marking extraction failed. The defaults
	// (10 * 500ms = 5s) were raised from an original 5 * 200ms = 1s,
	// which proved too short for slower NAS/network-mounted downloads
	// volumes.
	PartWaitAttempts int           `yaml:"part_wait_attempts"`
	PartWaitInterval time.Duration `yaml:"part_wait_interval"`
}

// QueueConfig controls queue.Manager's dispatch behavior.
type QueueConfig struct {
	// SequentialParts keeps a multi-part archive's parts from overtaking
	// each other: a part only starts once every lower-numbered part of
	// its set has started. Parts still run in parallel (parallel_files
	// fills the topmost package first). Default true, set in Default():
	// false is a legitimate explicit choice.
	SequentialParts bool `yaml:"sequential_parts"`
	// AutoRetry is how many times a download that failed for a transient
	// reason (network error, hoster temporarily unavailable or rate
	// limiting, a dropped connection after the per-chunk retries were
	// exhausted) is automatically re-queued before it is left in the error
	// state for a manual retry. 0 disables it. Permanent failures (file not
	// found, no usable account) are never retried automatically.
	AutoRetry int `yaml:"auto_retry"`
	// AutoRetryDelay is the wait before the first automatic retry; it
	// doubles on each further attempt (capped at 30 minutes).
	AutoRetryDelay time.Duration `yaml:"auto_retry_delay"`
}

// NotifyConfig controls push notifications when a package finishes (see
// internal/notify). Both targets are optional and independent; with
// neither set, nothing is ever sent.
type NotifyConfig struct {
	// WebhookURL receives a JSON POST (notify.Event) per finished package —
	// for Home Assistant, n8n, Node-RED, a custom script, etc.
	WebhookURL string `yaml:"webhook_url"`
	// NtfyURL is a full ntfy topic URL, e.g. https://ntfy.sh/my-hopper or
	// https://ntfy.example.lan/downloads. NtfyToken is an optional access
	// token for a protected topic (sent as a Bearer token).
	NtfyURL   string `yaml:"ntfy_url"`
	NtfyToken string `yaml:"ntfy_token"`
}

// ClickNLoadConfig controls the JDownloader-compatible CNL listener.
type ClickNLoadConfig struct {
	Enabled      bool     `yaml:"enabled"`
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	TrustedCIDRs []string `yaml:"trusted_cidrs"`
}

// AccountsConfig controls hoster-account credential storage.
type AccountsConfig struct {
	EncryptionKey string `yaml:"encryption_key"`
	// ReferralLinks makes the Web UI's "buy / extend premium" links use
	// the Hopper developer's referral link where a plugin has one
	// configured (see plugins.PremiumLinker) — supports development at no
	// extra cost. false always links the plain premium page. Nothing is
	// ever fetched or tracked by hopperd itself either way; it's only the
	// URL a button in the UI opens. Default true.
	ReferralLinks bool `yaml:"referral_links"`
}

// RateLimitConfig controls per-client API rate limiting.
type RateLimitConfig struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	Burst             int `yaml:"burst"`
}

// LoggingConfig controls structured logging output.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // json|text
}

// DatabaseConfig controls the embedded bbolt store location.
type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// DLCConfig controls JDownloader-compatible .dlc container decryption.
type DLCConfig struct {
	Enabled  bool   `yaml:"enabled"`
	JDAPIURL string `yaml:"jd_api_url"`
}

var envVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default} references in raw YAML
// bytes with values from the process environment, without touching literal
// '$' characters that aren't part of a reference.
func expandEnv(raw []byte) []byte {
	return envVarRef.ReplaceAllFunc(raw, func(match []byte) []byte {
		sub := envVarRef.FindSubmatch(match)
		name := string(sub[1])
		if v, ok := os.LookupEnv(name); ok {
			return []byte(v)
		}
		if len(sub[2]) > 2 {
			return sub[2][2:] // strip ":-"
		}
		return nil
	})
}

// Load reads a YAML config file from path, expands environment variable
// references, applies HOPPER_* environment overrides, defaults, and
// validates the result. It never returns a Config with an insecure or
// empty secret.
//
// A missing config file is not an error: hopperd then runs on Default()
// plus HOPPER_* environment overrides, which is all the Docker image
// needs. Secrets that are still unset after that are generated and kept
// next to the database (see ensureSecrets).
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}
	// Strip a leading UTF-8 BOM: some Windows editors (Notepad, older
	// PowerShell redirects) save UTF-8 files with one, which would
	// otherwise land inside the first YAML key and break parsing.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	cfg := Default()
	expanded := expandEnv(raw)
	if err := yaml.Unmarshal(expanded, cfg); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}

	applyEnvOverrides(cfg)
	cfg.ConfigFileMissing = missing

	if err := cfg.ensureSecrets(); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// Default returns a Config populated with safe, non-secret defaults.
// Fields with no safe default (JWT secret, encryption key) are left empty
// and Validate will reject them until explicitly configured.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host:               "0.0.0.0",
			Port:               9667,
			TokenTTL:           24 * time.Hour,
			ReadHeaderTimeout:  10 * time.Second,
			CORSAllowedOrigins: []string{},
		},
		Downloads: DownloadsConfig{
			Path:             "/downloads",
			ParallelFiles:    3,
			ParallelChunks:   4,
			BandwidthLimit:   0,
			RetryMax:         5,
			RetryBackoffBase: 2 * time.Second,
			AutoStartImports: true,
		},
		ClickNLoad: ClickNLoadConfig{
			Enabled:      true,
			Host:         "127.0.0.1",
			Port:         9666,
			TrustedCIDRs: []string{"127.0.0.1/32", "::1/128"},
		},
		Accounts: AccountsConfig{
			ReferralLinks: true,
		},
		RateLimit: RateLimitConfig{
			RequestsPerMinute: 120,
			Burst:             30,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		Database: DatabaseConfig{
			Path: "/config/hopper.db",
		},
		DLC: DLCConfig{
			Enabled:  true,
			JDAPIURL: "http://service.jdownloader.org/dlcrypt/service.php",
		},
		Extract: ExtractConfig{
			Enabled:          false,
			DeleteAfter:      true,
			UnrarPath:        "unrar",
			PartWaitAttempts: 10,
			PartWaitInterval: 500 * time.Millisecond,
		},
		Queue: QueueConfig{
			SequentialParts: true,
			AutoRetry:       3,
			AutoRetryDelay:  30 * time.Second,
		},
	}
}

// applyEnvOverrides applies direct HOPPER_* environment variable overrides
// on top of file values, per 12-factor convention. These take precedence
// over both the file and ${VAR} substitutions within it.
func applyEnvOverrides(cfg *Config) {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	strSlice := func(key string, dst *[]string) {
		if v, ok := os.LookupEnv(key); ok {
			parts := strings.Split(v, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			*dst = parts
		}
	}
	intVal := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				*dst = n
			}
		}
	}
	int64Val := func(key string, dst *int64) {
		if v, ok := os.LookupEnv(key); ok {
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				*dst = n
			}
		}
	}
	boolVal := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v == "1" || strings.EqualFold(v, "true")
		}
	}

	str("HOPPER_JWT_SECRET", &cfg.Server.JWTSecret)
	str("HOPPER_ADMIN_PASSWORD_HASH", &cfg.Server.AdminPasswordHash)
	str("HOPPER_HOST", &cfg.Server.Host)
	intVal("HOPPER_PORT", &cfg.Server.Port)
	strSlice("HOPPER_CORS_ALLOWED_ORIGINS", &cfg.Server.CORSAllowedOrigins)

	str("HOPPER_DOWNLOADS_PATH", &cfg.Downloads.Path)
	intVal("HOPPER_PARALLEL_FILES", &cfg.Downloads.ParallelFiles)
	intVal("HOPPER_PARALLEL_CHUNKS", &cfg.Downloads.ParallelChunks)
	int64Val("HOPPER_BANDWIDTH_LIMIT", &cfg.Downloads.BandwidthLimit)
	boolVal("HOPPER_AUTO_START_IMPORTS", &cfg.Downloads.AutoStartImports)

	boolVal("HOPPER_CLICKNLOAD_ENABLED", &cfg.ClickNLoad.Enabled)
	str("HOPPER_CLICKNLOAD_HOST", &cfg.ClickNLoad.Host)
	intVal("HOPPER_CLICKNLOAD_PORT", &cfg.ClickNLoad.Port)
	strSlice("HOPPER_CLICKNLOAD_TRUSTED_CIDRS", &cfg.ClickNLoad.TrustedCIDRs)

	str("HOPPER_ENCRYPTION_KEY", &cfg.Accounts.EncryptionKey)

	str("HOPPER_LOG_LEVEL", &cfg.Logging.Level)
	str("HOPPER_LOG_FORMAT", &cfg.Logging.Format)

	str("HOPPER_DATABASE_PATH", &cfg.Database.Path)

	// Only non-empty values override: an empty line in the Docker .env
	// (passed to the container via env_file) arrives as "" and must not
	// clear a hopper.yaml value.
	nonEmpty := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	nonEmpty("HOPPER_NOTIFY_WEBHOOK_URL", &cfg.Notify.WebhookURL)
	nonEmpty("HOPPER_NOTIFY_NTFY_URL", &cfg.Notify.NtfyURL)
	nonEmpty("HOPPER_NOTIFY_NTFY_TOKEN", &cfg.Notify.NtfyToken)
}

// Validate rejects insecure or nonsensical configuration. It is the single
// gate that prevents Hopper from starting with a hardcoded/empty secret,
// a weak encryption key, or a download path vulnerable to traversal.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.JWTSecret) == "" {
		return fmt.Errorf("server.jwt_secret is required (set HOPPER_JWT_SECRET); Hopper does not ship an insecure default")
	}
	if len(c.Server.JWTSecret) < 32 {
		return fmt.Errorf("server.jwt_secret must be at least 32 bytes long, got %d", len(c.Server.JWTSecret))
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	if strings.TrimSpace(c.Server.AdminPasswordHash) == "" {
		return fmt.Errorf("server.admin_password_hash is required (set HOPPER_ADMIN_PASSWORD_HASH); generate one with `hopperctl hash-password`")
	}
	if !strings.HasPrefix(c.Server.AdminPasswordHash, "$2") {
		return fmt.Errorf("server.admin_password_hash does not look like a bcrypt hash (expected a $2.. prefix)")
	}

	if strings.TrimSpace(c.Accounts.EncryptionKey) == "" {
		return fmt.Errorf("accounts.encryption_key is required (set HOPPER_ENCRYPTION_KEY); used to encrypt hoster credentials at rest")
	}
	if len(c.Accounts.EncryptionKey) != 32 {
		return fmt.Errorf("accounts.encryption_key must be exactly 32 bytes (AES-256), got %d", len(c.Accounts.EncryptionKey))
	}

	if !filepath.IsAbs(c.Downloads.Path) {
		return fmt.Errorf("downloads.path must be an absolute path, got %q", c.Downloads.Path)
	}
	if c.Downloads.ParallelFiles < 1 {
		return fmt.Errorf("downloads.parallel_files must be >= 1, got %d", c.Downloads.ParallelFiles)
	}
	if c.Downloads.ParallelChunks < 1 {
		return fmt.Errorf("downloads.parallel_chunks must be >= 1, got %d", c.Downloads.ParallelChunks)
	}
	if c.Downloads.BandwidthLimit < 0 {
		return fmt.Errorf("downloads.bandwidth_limit must be >= 0, got %d", c.Downloads.BandwidthLimit)
	}
	if c.Queue.AutoRetry < 0 {
		return fmt.Errorf("queue.auto_retry must be >= 0, got %d", c.Queue.AutoRetry)
	}
	if c.Queue.AutoRetryDelay < 0 {
		return fmt.Errorf("queue.auto_retry_delay must be >= 0, got %s", c.Queue.AutoRetryDelay)
	}

	if c.ClickNLoad.Enabled {
		if c.ClickNLoad.Port <= 0 || c.ClickNLoad.Port > 65535 {
			return fmt.Errorf("clicknload.port must be between 1 and 65535, got %d", c.ClickNLoad.Port)
		}
		if len(c.ClickNLoad.TrustedCIDRs) == 0 {
			return fmt.Errorf("clicknload.trusted_cidrs must not be empty when clicknload is enabled")
		}
		for _, cidr := range c.ClickNLoad.TrustedCIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("clicknload.trusted_cidrs: invalid CIDR %q: %w", cidr, err)
			}
		}
	}

	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		return fmt.Errorf("logging.format must be one of json|text, got %q", c.Logging.Format)
	}

	if !filepath.IsAbs(c.Database.Path) {
		return fmt.Errorf("database.path must be an absolute path, got %q", c.Database.Path)
	}

	if c.Extract.Enabled && strings.TrimSpace(c.Extract.UnrarPath) == "" {
		return fmt.Errorf("extract.unrar_path must not be empty when extract.enabled is true")
	}

	for key, raw := range map[string]string{"notify.webhook_url": c.Notify.WebhookURL, "notify.ntfy_url": c.Notify.NtfyURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s must be an http(s) URL, got %q", key, raw)
		}
	}

	for name, h := range c.Hosters {
		if h.BandwidthLimit < 0 {
			return fmt.Errorf("hosters.%s.bandwidth_limit must be >= 0, got %d", name, h.BandwidthLimit)
		}
		if h.MinRequestInterval < 0 {
			return fmt.Errorf("hosters.%s.min_request_interval must be >= 0, got %s", name, h.MinRequestInterval)
		}
	}

	if c.RateLimit.RequestsPerMinute < 1 {
		return fmt.Errorf("ratelimit.requests_per_minute must be >= 1, got %d", c.RateLimit.RequestsPerMinute)
	}
	if c.RateLimit.Burst < 1 {
		return fmt.Errorf("ratelimit.burst must be >= 1, got %d", c.RateLimit.Burst)
	}

	return nil
}

// TrustedNets parses ClickNLoad.TrustedCIDRs into net.IPNet values. It is
// called after Validate has already confirmed each entry parses cleanly.
func (c *Config) TrustedNets() ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(c.ClickNLoad.TrustedCIDRs))
	for _, cidr := range c.ClickNLoad.TrustedCIDRs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		nets = append(nets, ipnet)
	}
	return nets, nil
}

// Peek loads path like Load (defaults, file, HOPPER_* overrides) but
// without generating secrets or validating — for helper modes such as
// -healthcheck and -set-admin-password, which only need ports/paths and
// must never create files (they may run as a different user than the
// daemon, e.g. via docker exec).
func Peek(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	cfg := Default()
	if err := yaml.Unmarshal(expandEnv(raw), cfg); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}
	applyEnvOverrides(cfg)
	return cfg, nil
}
