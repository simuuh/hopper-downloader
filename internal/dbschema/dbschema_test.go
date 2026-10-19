package dbschema

import (
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func openDB(t *testing.T) *bbolt.DB {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "hopper.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func stamped(t *testing.T, db *bbolt.DB) uint64 {
	t.Helper()
	var v uint64
	_ = db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(metaBucket); b != nil && len(b.Get(versionKey)) == 8 {
			v = uint64(b.Get(versionKey)[7]) // small versions only
		}
		return nil
	})
	return v
}

func TestCheck_StampsUnversionedDatabase(t *testing.T) {
	db := openDB(t)
	if err := Check(db); err != nil {
		t.Fatal(err)
	}
	if v := stamped(t, db); v != Current {
		t.Fatalf("stamped version = %d, want %d", v, Current)
	}
	if err := Check(db); err != nil {
		t.Fatalf("second Check on the same database: %v", err)
	}
}

func TestCheck_RefusesNewerSchema(t *testing.T) {
	db := openDB(t)
	err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return b.Put(versionKey, encode(Current+1))
	})
	if err != nil {
		t.Fatal(err)
	}
	err = Check(db)
	if err == nil || !strings.Contains(err.Error(), "newer Hopper") {
		t.Fatalf("Check = %v, want a refusal naming a newer Hopper", err)
	}
}

func TestCheck_RejectsMalformedVersion(t *testing.T) {
	db := openDB(t)
	err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return b.Put(versionKey, []byte("1"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(db); err == nil {
		t.Fatal("Check accepted a malformed version")
	}
}
