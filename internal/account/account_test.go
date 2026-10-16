package account

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/simuuh/hopper-downloader/plugins"
)

var testKey = "01234567890123456789012345678901"[:32]

func newTestManager(t *testing.T, key string) *Manager {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	db, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(db, key, log)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNew_RejectsWrongKeyLength(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	db, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(db, "too-short", log); err == nil {
		t.Fatal("expected error for short key, got nil")
	}
}

func TestAdd_And_AccountFor_RoundTrips(t *testing.T) {
	m := newTestManager(t, testKey)

	pub, err := m.Add("rapidgator", "alice", "s3cr3t", map[string]string{"token": "abc"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if pub.Username != "alice" || pub.HosterKey != "rapidgator" {
		t.Errorf("unexpected public record: %+v", pub)
	}

	acc, ok := m.AccountFor("rapidgator")
	if !ok {
		t.Fatal("AccountFor returned false")
	}
	if acc.Username != "alice" || acc.Password != "s3cr3t" || acc.Extra["token"] != "abc" {
		t.Errorf("unexpected decrypted account: %+v", acc)
	}
}

func TestAccountFor_UnknownHoster(t *testing.T) {
	m := newTestManager(t, testKey)
	if _, ok := m.AccountFor("nope"); ok {
		t.Error("expected AccountFor to return false for unknown hoster")
	}
}

func TestAccountFor_SkipsDisabled(t *testing.T) {
	m := newTestManager(t, testKey)
	pub, err := m.Add("rapidgator", "alice", "s3cr3t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled(pub.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.AccountFor("rapidgator"); ok {
		t.Error("expected AccountFor to skip a disabled account")
	}
}

func TestDelete_RemovesAccount(t *testing.T) {
	m := newTestManager(t, testKey)
	pub, err := m.Add("rapidgator", "alice", "s3cr3t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(pub.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := m.Get(pub.ID); ok {
		t.Error("expected account gone after Delete")
	}
	if err := m.Delete(pub.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound deleting twice, got %v", err)
	}
}

func TestPersistence_SurvivesReopenWithSameKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(db, testKey, log)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := m.Add("1fichier", "bob", "hunter2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	m2, err := New(db2, testKey, log)
	if err != nil {
		t.Fatal(err)
	}
	acc, ok := m2.AccountFor("1fichier")
	if !ok {
		t.Fatal("expected account to survive reopen")
	}
	if acc.Password != "hunter2" {
		t.Errorf("password = %q, want hunter2", acc.Password)
	}
	_ = pub
}

func TestPersistence_WrongKeyFailsToDecrypt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(db, testKey, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("rapidgator", "alice", "s3cr3t", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	wrongKey := "98765432109876543210987654321098"[:32]
	m2, err := New(db2, wrongKey, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m2.AccountFor("rapidgator"); ok {
		t.Error("expected AccountFor to fail with the wrong encryption key")
	}
}

func TestCredentials_ReturnsExactAccountRegardlessOfEnabled(t *testing.T) {
	m := newTestManager(t, testKey)
	pub, err := m.Add("realdebrid", "alice", "s3cr3t", map[string]string{"api_token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled(pub.ID, false); err != nil {
		t.Fatal(err)
	}

	creds, err := m.Credentials(pub.ID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if creds.Username != "alice" || creds.Password != "s3cr3t" || creds.Extra["api_token"] != "tok" {
		t.Errorf("unexpected credentials: %+v", creds)
	}
}

func TestCredentials_NotFound(t *testing.T) {
	m := newTestManager(t, testKey)
	if _, err := m.Credentials("nope"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSetValidation_PersistsValidResult(t *testing.T) {
	m := newTestManager(t, testKey)
	pub, err := m.Add("realdebrid", "alice", "s3cr3t", nil)
	if err != nil {
		t.Fatal(err)
	}

	expires := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	info := &plugins.AccountInfo{Valid: true, ExpiresAt: &expires, Label: "Premium until 2026-12-31"}
	updated, err := m.SetValidation(pub.ID, info)
	if err != nil {
		t.Fatalf("SetValidation: %v", err)
	}
	if updated.Valid == nil || !*updated.Valid {
		t.Errorf("Valid = %v, want true", updated.Valid)
	}
	if updated.Label != "Premium until 2026-12-31" {
		t.Errorf("Label = %q", updated.Label)
	}
	if updated.ValidatedAt == nil {
		t.Error("expected ValidatedAt to be set")
	}

	// Also reflected by Get, not just the return value.
	got, ok := m.Get(pub.ID)
	if !ok {
		t.Fatal("Get: account not found")
	}
	if got.Valid == nil || !*got.Valid {
		t.Errorf("Get().Valid = %v, want true", got.Valid)
	}
}

func TestSetValidation_NilInfoClearsToUnknown(t *testing.T) {
	m := newTestManager(t, testKey)
	pub, err := m.Add("realdebrid", "alice", "s3cr3t", nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := true
	if _, err := m.SetValidation(pub.ID, &plugins.AccountInfo{Valid: valid}); err != nil {
		t.Fatal(err)
	}

	updated, err := m.SetValidation(pub.ID, nil)
	if err != nil {
		t.Fatalf("SetValidation: %v", err)
	}
	if updated.Valid != nil {
		t.Errorf("Valid = %v, want nil after a nil-info validation attempt", *updated.Valid)
	}
	if updated.ValidatedAt == nil {
		t.Error("expected ValidatedAt to still be set even though the result was unknown")
	}
}

func TestSetValidation_NotFound(t *testing.T) {
	m := newTestManager(t, testKey)
	if _, err := m.SetValidation("nope", &plugins.AccountInfo{Valid: true}); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCiphertext_DoesNotContainPlaintextPassword(t *testing.T) {
	m := newTestManager(t, testKey)
	pub, err := m.Add("rapidgator", "alice", "super-secret-password", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	r := m.accounts[pub.ID]
	m.mu.RUnlock()

	blob := string(r.Ciphertext)
	if strings.Contains(blob, "super-secret-password") {
		t.Error("ciphertext contains the plaintext password")
	}
}

func TestUpdateCredentials_KeepsEmptyFieldsAndClearsValidation(t *testing.T) {
	m := newTestManager(t, testKey)
	acc, err := m.Add("realdebrid", "alice", "old", map[string]string{"api_token": "x"})
	if err != nil {
		t.Fatal(err)
	}
	valid := true
	if _, err := m.SetValidation(acc.ID, &plugins.AccountInfo{Valid: valid, Label: "Premium"}); err != nil {
		t.Fatal(err)
	}
	updated, err := m.UpdateCredentials(acc.ID, "", "new", nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Username != "alice" || updated.Valid != nil || updated.Label != "" {
		t.Fatalf("updated = %+v", updated)
	}
	creds, _ := m.Credentials(acc.ID)
	if creds.Password != "new" || creds.Extra["api_token"] != "x" {
		t.Fatalf("creds = %+v", creds)
	}
	if _, err := m.UpdateCredentials("missing", "", "x", nil); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}
}

type countingValidator struct {
	plugins.Hoster
	calls int
}

func (c *countingValidator) Name() string { return "realdebrid" }
func (c *countingValidator) ValidateAccount(*plugins.Account) (*plugins.AccountInfo, error) {
	c.calls++
	return &plugins.AccountInfo{Valid: true, Label: "Premium"}, nil
}

func TestRevalidateAll_SkipsDisabledAndUpdatesCache(t *testing.T) {
	m := newTestManager(t, testKey)
	a, _ := m.Add("realdebrid", "a", "t1", nil)
	b, _ := m.Add("realdebrid", "b", "t2", nil)
	_ = m.SetEnabled(b.ID, false)
	_, _ = m.Add("other", "c", "t3", nil)

	v := &countingValidator{}
	m.RevalidateAll(context.Background(), []plugins.Hoster{v})
	if v.calls != 1 {
		t.Fatalf("calls = %d, want 1 (only the enabled realdebrid account)", v.calls)
	}
	got, _ := m.Get(a.ID)
	if got.Valid == nil || !*got.Valid || got.ValidatedAt == nil {
		t.Fatalf("cache not updated: %+v", got)
	}
}
