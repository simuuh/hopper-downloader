// Package dbschema stamps hopperd's bbolt database with the version of
// its layout, so a hopperd that doesn't know a layout refuses it instead
// of misreading it — typically after a downgrade to an older release.
//
// A change to how any package stores its records that an older hopperd
// would misread bumps Current, and Check gains the step that migrates a
// database from the previous version.
package dbschema

import (
	"encoding/binary"
	"fmt"

	"go.etcd.io/bbolt"
)

// Current is the layout this build writes and reads. Databases created
// before the version was stamped have this layout too.
const Current = 1

var (
	metaBucket = []byte("meta")
	versionKey = []byte("schema_version")
)

// Check stamps a database without a version with Current and fails for
// one stamped with a newer version than this build knows.
func Check(db *bbolt.DB) error {
	return db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return fmt.Errorf("dbschema: init bucket: %w", err)
		}
		raw := b.Get(versionKey)
		if raw == nil {
			return b.Put(versionKey, encode(Current))
		}
		if len(raw) != 8 {
			return fmt.Errorf("dbschema: malformed schema version (%d bytes)", len(raw))
		}
		v := binary.BigEndian.Uint64(raw)
		if v > Current {
			return fmt.Errorf("dbschema: the database was written by a newer Hopper (schema %d, this version knows %d); run that version or newer", v, Current)
		}
		return nil
	})
}

func encode(v uint64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, v)
	return out
}
