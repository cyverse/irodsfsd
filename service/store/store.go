// Package store implements MountRepository on top of an embedded BadgerDB
// database, encrypted at rest, so mount intent and credentials survive a
// daemon restart.
package store

import (
	"encoding/binary"

	"github.com/cockroachdb/errors"
	badger "github.com/dgraph-io/badger/v3"
	log "github.com/sirupsen/logrus"
)

// CurrentSchemaVersion is the schema version this build of irodsfsd writes.
// Bump it and add a corresponding entry to migrations whenever the on-disk
// record format changes.
const CurrentSchemaVersion uint64 = 1

// blockCacheSize and indexCacheSize size Badger's in-memory caches. Badger
// requires a non-zero block cache whenever encryption is enabled; both are
// sized for a small mount-metadata store, not for bulk data.
const (
	blockCacheSize int64 = 64 << 20
	indexCacheSize int64 = 16 << 20
)

var metaSchemaVersionKey = []byte("meta/schema-version")

// migrations upgrades the on-disk schema from one version to the next, keyed
// by the version migrated FROM. There is nothing to migrate yet; add entries
// here as the record format evolves.
var migrations = map[uint64]func(txn *badger.Txn) error{}

// Store owns one embedded, encrypted BadgerDB instance.
type Store struct {
	db *badger.DB
}

// Open opens (creating if necessary) an encrypted BadgerDB database at
// dataDir and ensures its schema is current. Production startup must never
// fall back to plaintext, so an empty encryptionKey is rejected outright.
func Open(dataDir string, encryptionKey []byte) (*Store, error) {
	if len(encryptionKey) == 0 {
		return nil, errors.New("mount database encryption key is required")
	}

	options := badger.DefaultOptions(dataDir).
		WithEncryptionKey(encryptionKey).
		WithBlockCacheSize(blockCacheSize).
		WithIndexCacheSize(indexCacheSize).
		WithLogger(badgerLogger{entry: log.WithField("component", "badger")})

	db, err := badger.Open(options)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open mount database at %q", dataDir)
	}

	store := &Store{db: db}
	if err := store.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close closes the underlying database.
func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) ensureSchema() error {
	return store.db.Update(func(txn *badger.Txn) error {
		stored, err := readSchemaVersion(txn)
		if err != nil {
			return err
		}
		if stored > CurrentSchemaVersion {
			return errors.Newf("mount database schema version %d is newer than the version %d supported by this build", stored, CurrentSchemaVersion)
		}
		for version := stored; version < CurrentSchemaVersion; version++ {
			migrate, ok := migrations[version]
			if !ok {
				continue
			}
			if err := migrate(txn); err != nil {
				return errors.Wrapf(err, "failed to migrate mount database from schema version %d", version)
			}
		}
		if stored == CurrentSchemaVersion {
			return nil
		}
		return txn.Set(metaSchemaVersionKey, encodeUint64(CurrentSchemaVersion))
	})
}

func readSchemaVersion(txn *badger.Txn) (uint64, error) {
	item, err := txn.Get(metaSchemaVersionKey)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, errors.Wrap(err, "failed to read mount database schema version")
	}
	value, err := item.ValueCopy(nil)
	if err != nil {
		return 0, errors.Wrap(err, "failed to read mount database schema version")
	}
	if len(value) != 8 {
		return 0, errors.Newf("mount database schema version record has invalid length %d", len(value))
	}
	return binary.BigEndian.Uint64(value), nil
}

func encodeUint64(value uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, value)
	return encoded
}

// badgerLogger adapts a logrus entry to Badger's Logger interface.
type badgerLogger struct {
	entry *log.Entry
}

func (logger badgerLogger) Errorf(format string, args ...interface{}) {
	logger.entry.Errorf(format, args...)
}

func (logger badgerLogger) Warningf(format string, args ...interface{}) {
	logger.entry.Warnf(format, args...)
}

// Infof is downgraded to debug level: Badger logs routine compaction and
// startup detail at Info level, which is too chatty for the daemon's normal
// log level.
func (logger badgerLogger) Infof(format string, args ...interface{}) {
	logger.entry.Debugf(format, args...)
}

func (logger badgerLogger) Debugf(format string, args ...interface{}) {
	logger.entry.Debugf(format, args...)
}
