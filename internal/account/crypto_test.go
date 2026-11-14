package account

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func openTestDB(t *testing.T) *bbolt.DB {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "accounts.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func readRecord(t *testing.T, db *bbolt.DB, id string) record {
	t.Helper()
	var r record
	if err := db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket(bucketName).Get([]byte(id)), &r)
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func writeRecord(t *testing.T, db *bbolt.DB, r record) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(r.ID), data)
	}); err != nil {
		t.Fatal(err)
	}
}

// writeLegacyRecord stores an account the way versions before cipher
// versioning did: AES-256-GCM with the raw key, no associated data, no
// cipher_version field.
func writeLegacyRecord(t *testing.T, db *bbolt.DB, id, hosterKey, password string) {
	t.Helper()
	gcm, err := newAEAD([]byte(testKey))
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(secret{Password: password})
	ct, err := seal(gcm, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeRecord(t, db, record{
		Account:    Account{ID: id, HosterKey: hosterKey, Username: "user", Enabled: true, CreatedAt: now, UpdatedAt: now},
		Ciphertext: ct,
	})
}

func TestNew_MigratesLegacyRecords(t *testing.T) {
	db := openTestDB(t)
	writeLegacyRecord(t, db, "legacy1", "rapidgator", "placeholder-pw")

	m, err := New(db, testKey, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	acc, err := m.Credentials("legacy1")
	if err != nil || acc.Password != "placeholder-pw" {
		t.Fatalf("Credentials after migration = %+v, %v", acc, err)
	}
	stored := readRecord(t, db, "legacy1")
	if stored.CipherVersion != cipherV1 {
		t.Fatalf("stored cipher_version = %d, want %d", stored.CipherVersion, cipherV1)
	}
	// The migrated record must open with the current scheme only.
	if _, err := open(m.ciphers.legacy, stored.Ciphertext, nil); err == nil {
		t.Fatal("migrated record still opens with the legacy cipher")
	}

	// A second start finds nothing left to migrate and still reads it.
	m2, err := New(db, testKey, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := m2.Credentials("legacy1"); err != nil || acc.Password != "placeholder-pw" {
		t.Fatalf("Credentials after restart = %+v, %v", acc, err)
	}
}

// The key in use is derived via HKDF, not the configured key itself.
func TestSeal_UsesDerivedKey(t *testing.T) {
	m := newTestManager(t, testKey)
	pub, err := m.Add("1fichier", "u", "placeholder-pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := m.accounts[pub.ID]
	raw, err := newAEAD([]byte(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(raw, r.Ciphertext, recordAAD(r.ID, r.HosterKey)); err == nil {
		t.Fatal("ciphertext opens with the raw configured key")
	}
}

// A ciphertext copied into another record (other id or hoster key)
// must not decrypt as that account's secret.
func TestCiphertext_BoundToIDAndHoster(t *testing.T) {
	db := openTestDB(t)
	m, err := New(db, testKey, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.Add("rapidgator", "a", "secret-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Add("rapidgator", "b", "secret-b", nil)
	if err != nil {
		t.Fatal(err)
	}

	recA, recB := readRecord(t, db, a.ID), readRecord(t, db, b.ID)
	recB.Ciphertext = recA.Ciphertext
	writeRecord(t, db, recB)
	recA.HosterKey = "1fichier"
	writeRecord(t, db, recA)

	m2, err := New(db, testKey, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := m2.Credentials(b.ID); err == nil {
		t.Fatalf("swapped ciphertext decrypted as %q", acc.Password)
	}
	if acc, err := m2.Credentials(a.ID); err == nil {
		t.Fatalf("record with changed hoster key decrypted as %q", acc.Password)
	}
}

// A wrong key is reported once at startup, and the records stay as they
// were so the right key can still open them.
func TestNew_WrongKeyIsReportedAndLeavesRecordsUntouched(t *testing.T) {
	db := openTestDB(t)
	m, err := New(db, testKey, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	pub, err := m.Add("rapidgator", "a", "placeholder-pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyRecord(t, db, "legacy1", "1fichier", "placeholder-pw-2")
	before := readRecord(t, db, "legacy1")

	var logBuf bytes.Buffer
	wrongKey := "98765432109876543210987654321098"
	if _, err := New(db, wrongKey, slog.New(slog.NewTextHandler(&logBuf, nil))); err != nil {
		t.Fatal(err)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "cannot be decrypted") || !strings.Contains(logged, "count=2") {
		t.Fatalf("no startup error for undecryptable accounts; log:\n%s", logged)
	}
	if after := readRecord(t, db, "legacy1"); !bytes.Equal(after.Ciphertext, before.Ciphertext) || after.CipherVersion != cipherLegacy {
		t.Fatal("undecryptable legacy record was rewritten")
	}

	m3, err := New(db, testKey, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pub.ID, "legacy1"} {
		if _, err := m3.Credentials(id); err != nil {
			t.Fatalf("%s unreadable with the right key after a wrong-key start: %v", id, err)
		}
	}
}
