package queue

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"
)

var (
	itemsBucket    = []byte("queue_items")
	packagesBucket = []byte("queue_packages")
	// quarantineBucket keeps records that no longer decode, keyed
	// "<bucket>/<key>", for inspection instead of deleting them.
	quarantineBucket = []byte("queue_quarantine")
)

// DamagedRecord is a stored record that couldn't be decoded and was moved
// to the quarantine bucket.
type DamagedRecord struct {
	Key string // "<bucket>/<key>" in the quarantine bucket
	Err error
}

// Store persists Items and Packages to a bbolt database file. It performs
// no business logic; Manager is the only caller and owns all invariants.
type Store struct {
	db *bbolt.DB
}

// OpenStore opens (creating if necessary) the bbolt database at path and
// ensures the queue buckets exist.
func OpenStore(db *bbolt.DB) (*Store, error) {
	err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(itemsBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(packagesBucket); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("queue: init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// PutItem inserts or replaces an Item.
func (s *Store) PutItem(it *Item) error {
	data, err := json.Marshal(it)
	if err != nil {
		return fmt.Errorf("queue: marshal item: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(itemsBucket).Put([]byte(it.ID), data)
	})
}

// DeleteItem removes an Item by ID.
func (s *Store) DeleteItem(id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(itemsBucket).Delete([]byte(id))
	})
}

// ListItems returns every persisted Item, in undefined order. Records
// that don't decode are moved to quarantine and reported, not returned
// (see loadAll).
func (s *Store) ListItems() ([]*Item, []DamagedRecord, error) {
	var out []*Item
	damaged, err := s.loadAll(itemsBucket, func(v []byte) error {
		var it Item
		if err := json.Unmarshal(v, &it); err != nil {
			return err
		}
		out = append(out, &it)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("queue: load items: %w", err)
	}
	return out, damaged, nil
}

// PutPackage inserts or replaces a Package.
func (s *Store) PutPackage(p *Package) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("queue: marshal package: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(packagesBucket).Put([]byte(p.ID), data)
	})
}

// ListPackages returns every persisted Package, in undefined order.
// Records that don't decode are moved to quarantine and reported, not
// returned (see loadAll).
func (s *Store) ListPackages() ([]*Package, []DamagedRecord, error) {
	var out []*Package
	damaged, err := s.loadAll(packagesBucket, func(v []byte) error {
		var p Package
		if err := json.Unmarshal(v, &p); err != nil {
			return err
		}
		out = append(out, &p)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("queue: load packages: %w", err)
	}
	return out, damaged, nil
}

// loadAll passes every record of bucket to decode. A record decode
// rejects is moved to quarantineBucket instead of failing the load: one
// damaged record (a torn write, a value a newer version wrote) must not
// keep hopperd from starting, and moving it keeps it for inspection
// without it being reported again on every start.
func (s *Store) loadAll(bucket []byte, decode func([]byte) error) ([]DamagedRecord, error) {
	type bad struct {
		key, value []byte
		err        error
	}
	var bads []bad
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(k, v []byte) error {
			if err := decode(v); err != nil {
				bads = append(bads, bad{key: append([]byte(nil), k...), value: append([]byte(nil), v...), err: err})
			}
			return nil
		})
	})
	if err != nil || len(bads) == 0 {
		return nil, err
	}

	damaged := make([]DamagedRecord, 0, len(bads))
	err = s.db.Update(func(tx *bbolt.Tx) error {
		q, err := tx.CreateBucketIfNotExists(quarantineBucket)
		if err != nil {
			return err
		}
		src := tx.Bucket(bucket)
		for _, b := range bads {
			qkey := string(bucket) + "/" + string(b.key)
			if err := q.Put([]byte(qkey), b.value); err != nil {
				return err
			}
			if err := src.Delete(b.key); err != nil {
				return err
			}
			damaged = append(damaged, DamagedRecord{Key: qkey, Err: b.err})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("quarantine damaged records: %w", err)
	}
	return damaged, nil
}

// DeletePackage removes a Package by ID. It does not touch the package's
// items; Manager only calls it once no item references the package any
// more (see Manager.removePackageIfEmpty).
func (s *Store) DeletePackage(id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(packagesBucket).Delete([]byte(id))
	})
}
