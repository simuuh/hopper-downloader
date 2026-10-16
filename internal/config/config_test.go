package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// yamlOpts customizes the config fixture built by buildYAML. Zero values
// fall back to sane, valid defaults so each test only needs to set the
// field(s) it cares about.
type yamlOpts struct {
	downloadsPath string
	// omitAdminHash leaves server.admin_password_hash out (generated then).
	omitAdminHash bool
	databasePath  string
	cidrs         string // pre-formatted YAML list, e.g. `["127.0.0.1/32"]`
}

func buildYAML(t *testing.T, o yamlOpts) string {
	t.Helper()
	dir := t.TempDir()
	if o.downloadsPath == "" {
		o.downloadsPath = filepath.Join(dir, "downloads")
	}
	if o.databasePath == "" {
		o.databasePath = filepath.Join(dir, "hopper.db")
	}
	if o.cidrs == "" {
		o.cidrs = `["127.0.0.1/32"]`
	}
	adminHash := `  admin_password_hash: "$2a$10$abcdefghijklmnopqrstuvuvwxyzABCDEFGHIJKLMNOPQRSTUV"`
	if o.omitAdminHash {
		adminHash = ""
	}
	return fmt.Sprintf(`
server:
  port: 9667
  host: 0.0.0.0
  jwt_secret: ${HOPPER_JWT_SECRET}
%s
downloads:
  path: %q
  parallel_files: 3
  parallel_chunks: 4
  bandwidth_limit: 0
clicknload:
  enabled: true
  trusted_cidrs: %s
accounts:
  encryption_key: ${HOPPER_ENCRYPTION_KEY}
database:
  path: %q
`, adminHash, o.downloadsPath, o.cidrs, o.databasePath)
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "hopper.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func setValidSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("HOPPER_JWT_SECRET", "this-is-a-32-plus-byte-test-secret-value")
	t.Setenv("HOPPER_ENCRYPTION_KEY", "01234567890123456789012345678901"[:32])
}

func TestLoad_MissingSecretsAreGeneratedAndReused(t *testing.T) {
	p := writeTemp(t, buildYAML(t, yamlOpts{omitAdminHash: true}))
	first, err := Load(p)
	if err != nil {
		t.Fatalf("Load without secrets: %v", err)
	}
	if len(first.Server.JWTSecret) < 32 || len(first.Accounts.EncryptionKey) != 32 || first.GeneratedAdminPassword == "" {
		t.Fatalf("secrets not generated: jwt=%d key=%d pw=%q", len(first.Server.JWTSecret), len(first.Accounts.EncryptionKey), first.GeneratedAdminPassword)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(first.Server.AdminPasswordHash), []byte(first.GeneratedAdminPassword)); err != nil {
		t.Fatalf("generated hash doesn't match generated password: %v", err)
	}
	info, err := os.Stat(first.SecretsPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("secrets file mode = %v, want 0600", info.Mode().Perm())
	}

	second, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if second.Server.JWTSecret != first.Server.JWTSecret || second.Accounts.EncryptionKey != first.Accounts.EncryptionKey ||
		second.Server.AdminPasswordHash != first.Server.AdminPasswordHash {
		t.Fatal("second start generated new secrets instead of reusing the stored ones")
	}
	if second.GeneratedAdminPassword != "" {
		t.Error("password reported as newly generated on the second start")
	}
}

func TestLoad_ExplicitSecretsWinAndAreNotStored(t *testing.T) {
	setValidSecrets(t)
	p := writeTemp(t, buildYAML(t, yamlOpts{}))
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GeneratedAdminPassword != "" {
		t.Error("generated a password although one is configured")
	}
	if _, err := os.Stat(cfg.SecretsPath()); !os.IsNotExist(err) {
		t.Errorf("secrets file written although every secret is configured (err=%v)", err)
	}
}

func TestLoad_MissingConfigFileRunsOnDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOPPER_DATABASE_PATH", filepath.Join(dir, "hopper.db"))
	t.Setenv("HOPPER_DOWNLOADS_PATH", filepath.Join(dir, "downloads"))
	cfg, err := Load(filepath.Join(dir, "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load with missing config file: %v", err)
	}
	if !cfg.ConfigFileMissing || cfg.Server.Port != 9667 || cfg.GeneratedAdminPassword == "" {
		t.Fatalf("cfg = missing:%v port:%d pw:%q", cfg.ConfigFileMissing, cfg.Server.Port, cfg.GeneratedAdminPassword)
	}
}

func TestSetAdminPassword(t *testing.T) {
	p := writeTemp(t, buildYAML(t, yamlOpts{omitAdminHash: true}))
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAdminPassword(cfg.SecretsPath(), "short"); err == nil {
		t.Error("accepted a too short password")
	}
	if err := SetAdminPassword(cfg.SecretsPath(), "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(again.Server.AdminPasswordHash), []byte("correct horse battery")); err != nil {
		t.Fatalf("new password not active: %v", err)
	}
	if again.Server.JWTSecret != cfg.Server.JWTSecret {
		t.Error("SetAdminPassword replaced the other secrets")
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	setValidSecrets(t)

	p := writeTemp(t, buildYAML(t, yamlOpts{}))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9667 {
		t.Errorf("expected port 9667, got %d", cfg.Server.Port)
	}
	if cfg.Downloads.ParallelChunks != 4 {
		t.Errorf("expected 4 parallel chunks, got %d", cfg.Downloads.ParallelChunks)
	}
}

func TestLoad_ShortJWTSecretRejected(t *testing.T) {
	t.Setenv("HOPPER_JWT_SECRET", "too-short")
	t.Setenv("HOPPER_ENCRYPTION_KEY", "01234567890123456789012345678901"[:32])

	p := writeTemp(t, buildYAML(t, yamlOpts{}))
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for short jwt_secret, got nil")
	}
}

func TestLoad_InvalidCIDRRejected(t *testing.T) {
	setValidSecrets(t)

	p := writeTemp(t, buildYAML(t, yamlOpts{cidrs: `["not-a-cidr"]`}))
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid CIDR, got nil")
	}
}

func TestLoad_RelativeDownloadsPathRejected(t *testing.T) {
	setValidSecrets(t)

	p := writeTemp(t, buildYAML(t, yamlOpts{downloadsPath: "relative/path"}))
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for relative downloads.path, got nil")
	}
}

func TestLoad_StripsUTF8BOM(t *testing.T) {
	setValidSecrets(t)

	yamlText := buildYAML(t, yamlOpts{})
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte(yamlText)...)
	dir := t.TempDir()
	p := filepath.Join(dir, "hopper.yaml")
	if err := os.WriteFile(p, withBOM, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error loading BOM-prefixed config: %v", err)
	}
	if cfg.Server.Port != 9667 {
		t.Errorf("expected port 9667, got %d", cfg.Server.Port)
	}
}

func TestApplyEnvOverrides_TakesPrecedenceOverFile(t *testing.T) {
	setValidSecrets(t)
	t.Setenv("HOPPER_PORT", "9999")

	p := writeTemp(t, buildYAML(t, yamlOpts{}))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("expected env override port 9999, got %d", cfg.Server.Port)
	}
}

func TestLoad_NegativeAutoRetryRejected(t *testing.T) {
	setValidSecrets(t)
	p := writeTemp(t, buildYAML(t, yamlOpts{})+"\nqueue:\n  auto_retry: -1\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "auto_retry") {
		t.Fatalf("err = %v, want auto_retry validation error", err)
	}
}

func TestLoad_NotifyEmptyEnvKeepsFileValue(t *testing.T) {
	setValidSecrets(t)
	p := writeTemp(t, buildYAML(t, yamlOpts{})+"\nnotify:\n  ntfy_url: https://ntfy.example.com/from-file\n")

	// An empty line in the Docker .env arrives as "".
	t.Setenv("HOPPER_NOTIFY_NTFY_URL", "")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.NtfyURL != "https://ntfy.example.com/from-file" {
		t.Errorf("empty env cleared the file value: %q", cfg.Notify.NtfyURL)
	}

	t.Setenv("HOPPER_NOTIFY_NTFY_URL", "https://ntfy.example.com/from-env")
	cfg, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.NtfyURL != "https://ntfy.example.com/from-env" {
		t.Errorf("env did not override: %q", cfg.Notify.NtfyURL)
	}

	t.Setenv("HOPPER_NOTIFY_WEBHOOK_URL", "not a url")
	if _, err := Load(p); err == nil {
		t.Error("invalid webhook URL accepted")
	}
}
