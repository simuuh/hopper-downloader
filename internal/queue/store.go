package queue

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"
)

var (
	itemsBucket    = []byte("queue_items")
	packagesBucket = []byte("queue_packages")
)

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

// ListItems returns every persisted Item, in undefined order.
func (s *Store) ListItems() ([]*Item, error) {
	var out []*Item
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(itemsBucket).ForEach(func(_, v []byte) error {
			var it Item
			if err := json.Unmarshal(v, &it); err != nil {
				return fmt.Errorf("unmarshal item: %w", err)
			}
			out = append(out, &it)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
func (s *Store) ListPackages() ([]*Package, error) {
	var out []*Package
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(packagesBucket).ForEach(func(_, v []byte) error {
			var p Package
			if err := json.Unmarshal(v, &p); err != nil {
				return fmt.Errorf("unmarshal package: %w", err)
			}
			out = append(out, &p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePackage removes a Package by ID. It does not touch the package's
// items; Manager only calls it once no item references the package any
// more (see Manager.removePackageIfEmpty).
func (s *Store) DeletePackage(id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(packagesBucket).Delete([]byte(id))
	})
}
