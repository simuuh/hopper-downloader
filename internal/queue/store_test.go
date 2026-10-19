package queue

import (
	"testing"

	"go.etcd.io/bbolt"
)

// putRaw writes value under key in bucket, bypassing Store's encoding.
func putRaw(t *testing.T, store *Store, bucket []byte, key, value string) {
	t.Helper()
	err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), []byte(value))
	})
	if err != nil {
		t.Fatal(err)
	}
}

// One damaged record — a torn write, a field a newer version wrote — must
// not keep hopperd from starting. It is set aside for inspection and the
// rest of the queue loads (M24).
func TestNew_DamagedRecordsAreQuarantinedNotFatal(t *testing.T) {
	store := newTestStore(t)
	good := &Item{ID: "good", URL: "https://host.example/a.bin", State: StateQueued}
	if err := store.PutItem(good); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPackage(&Package{ID: "pkg", Name: "Example"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutItem(&Item{ID: "inpkg", PackageID: "pkg", URL: "https://host.example/b.bin", State: StateQueued}); err != nil {
		t.Fatal(err)
	}
	putRaw(t, store, itemsBucket, "torn", `{"id":"torn","url":"https://host.exa`)
	putRaw(t, store, itemsBucket, "newer", `{"id":"newer","priority":"urgent"}`)
	putRaw(t, store, packagesBucket, "badpkg", `not json`)

	mgr, _ := newTestManager(t, store, Config{})

	if _, ok := mgr.Get("good"); !ok {
		t.Fatal("intact item not loaded")
	}
	if _, ok := mgr.Get("inpkg"); !ok {
		t.Fatal("intact packaged item not loaded")
	}
	if n := len(mgr.List()); n != 2 {
		t.Fatalf("loaded %d items, want the 2 intact ones", n)
	}

	var quarantined []string
	var leftInPlace int
	err := store.db.View(func(tx *bbolt.Tx) error {
		if q := tx.Bucket(quarantineBucket); q != nil {
			_ = q.ForEach(func(k, _ []byte) error {
				quarantined = append(quarantined, string(k))
				return nil
			})
		}
		for _, k := range []string{"torn", "newer"} {
			if tx.Bucket(itemsBucket).Get([]byte(k)) != nil {
				leftInPlace++
			}
		}
		if tx.Bucket(packagesBucket).Get([]byte("badpkg")) != nil {
			leftInPlace++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 3 || leftInPlace != 0 {
		t.Fatalf("quarantined %v, %d damaged records left in place; want all 3 moved", quarantined, leftInPlace)
	}
}
