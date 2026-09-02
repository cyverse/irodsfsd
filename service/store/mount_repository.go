package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"

	"github.com/cockroachdb/errors"
	"github.com/cyverse/irodsfsd/service/api"
	badger "github.com/dgraph-io/badger/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	mountKeyPrefix     = "mounts/"
	mountPathKeyPrefix = "mount-paths/"
)

var (
	ErrMountNotFound     = errors.New("mount not found")
	ErrMountIDConflict   = errors.New("mount ID is already in use")
	ErrMountPathConflict = errors.New("mount path is already in use")
)

// MountRecord is the persisted, unredacted representation of one mount.
// Info.Config retains original iRODS/DAVFS credentials; callers must redact
// a copy before returning it through any API. Info.Config.MountPath is
// treated as immutable for the lifetime of a record.
type MountRecord struct {
	Info *api.MountInfo

	// Tombstone marks a record whose Unmount has been accepted. A
	// tombstoned record is never restored as a fresh mount after a daemon
	// restart; only its unmount cleanup is resumed.
	Tombstone bool
}

// MountRepository persists MountRecords in a Store, keeping a canonical
// mount-path index alongside each record so mount-ID and mount-path
// uniqueness can be enforced within a single transaction. This is the only
// implementation of mount persistence, so it is exposed as a concrete type
// rather than behind an interface.
type MountRepository struct {
	store *Store
}

// NewMountRepository returns a MountRepository backed by store.
func NewMountRepository(store *Store) *MountRepository {
	return &MountRepository{store: store}
}

// maxConflictRetries bounds how many times a write transaction is retried
// after badger.ErrConflict, which Badger's optimistic concurrency control
// returns (and explicitly documents as retryable) when a concurrent
// transaction touched the same keys first.
const maxConflictRetries = 10

// updateWithConflictRetry runs fn as a read-write transaction, retrying on
// badger.ErrConflict so a momentary overlap with another writer (e.g. a
// best-effort state persist racing an explicit Unmount) does not surface as
// a spurious failure to the caller.
func (repository *MountRepository) updateWithConflictRetry(fn func(txn *badger.Txn) error) error {
	var err error
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		err = repository.store.db.Update(fn)
		if !errors.Is(err, badger.ErrConflict) {
			return err
		}
	}
	return err
}

func mountKey(mountID string) []byte {
	return []byte(mountKeyPrefix + mountID)
}

// mountPathKey base64-encodes the canonical path so the key never collides
// with the "/"-separated key namespaces used elsewhere in the database.
func mountPathKey(canonicalPath string) []byte {
	return []byte(mountPathKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(canonicalPath)))
}

// Create stores a new mount record and reserves its canonical mount path
// atomically. It returns ErrMountIDConflict or ErrMountPathConflict if the
// mount ID or mount path is already in use by another record.
func (repository *MountRepository) Create(ctx context.Context, record *MountRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	canonicalPath, err := validateRecordForWrite(record)
	if err != nil {
		return err
	}
	mountID := record.Info.MountId
	data, err := encodeRecord(record)
	if err != nil {
		return err
	}
	pathKey := mountPathKey(canonicalPath)

	return repository.updateWithConflictRetry(func(txn *badger.Txn) error {
		if _, getErr := txn.Get(mountKey(mountID)); getErr == nil {
			return errors.Wrapf(ErrMountIDConflict, "mount ID %q", mountID)
		} else if !errors.Is(getErr, badger.ErrKeyNotFound) {
			return errors.Wrap(getErr, "failed to check existing mount record")
		}
		if item, getErr := txn.Get(pathKey); getErr == nil {
			owner, valueErr := item.ValueCopy(nil)
			if valueErr != nil {
				return errors.Wrap(valueErr, "failed to check mount path index")
			}
			return errors.Wrapf(ErrMountPathConflict, "mount path %q is owned by %q", canonicalPath, owner)
		} else if !errors.Is(getErr, badger.ErrKeyNotFound) {
			return errors.Wrap(getErr, "failed to check mount path index")
		}
		if err := txn.Set(mountKey(mountID), data); err != nil {
			return errors.Wrap(err, "failed to store mount record")
		}
		if err := txn.Set(pathKey, []byte(mountID)); err != nil {
			return errors.Wrap(err, "failed to store mount path index")
		}
		return nil
	})
}

// Update replaces the stored record for record.Info.MountId. It returns
// ErrMountNotFound if no record exists for that ID.
func (repository *MountRepository) Update(ctx context.Context, record *MountRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := validateRecordForWrite(record); err != nil {
		return err
	}
	mountID := record.Info.MountId
	data, err := encodeRecord(record)
	if err != nil {
		return err
	}

	return repository.updateWithConflictRetry(func(txn *badger.Txn) error {
		if _, getErr := txn.Get(mountKey(mountID)); errors.Is(getErr, badger.ErrKeyNotFound) {
			return errors.Wrapf(ErrMountNotFound, "mount ID %q", mountID)
		} else if getErr != nil {
			return errors.Wrap(getErr, "failed to check existing mount record")
		}
		if err := txn.Set(mountKey(mountID), data); err != nil {
			return errors.Wrap(err, "failed to update mount record")
		}
		return nil
	})
}

// Delete removes the mount record and its path index atomically. Deleting an
// unknown mount ID is not an error.
func (repository *MountRepository) Delete(ctx context.Context, mountID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mountID == "" {
		return errors.New("mount ID is required")
	}

	return repository.updateWithConflictRetry(func(txn *badger.Txn) error {
		item, err := txn.Get(mountKey(mountID))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return errors.Wrap(err, "failed to read mount record for deletion")
		}
		data, err := item.ValueCopy(nil)
		if err != nil {
			return errors.Wrap(err, "failed to read mount record for deletion")
		}
		record, err := decodeRecord(data)
		if err != nil {
			return err
		}
		if err := txn.Delete(mountKey(mountID)); err != nil {
			return errors.Wrap(err, "failed to delete mount record")
		}
		canonicalPath := filepath.Clean(record.Info.GetConfig().GetMountPath())
		if err := txn.Delete(mountPathKey(canonicalPath)); err != nil {
			return errors.Wrap(err, "failed to delete mount path index")
		}
		return nil
	})
}

// Get returns the stored record for mountID, or ErrMountNotFound.
func (repository *MountRepository) Get(ctx context.Context, mountID string) (*MountRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var record *MountRecord
	err := repository.store.db.View(func(txn *badger.Txn) error {
		item, getErr := txn.Get(mountKey(mountID))
		if errors.Is(getErr, badger.ErrKeyNotFound) {
			return errors.Wrapf(ErrMountNotFound, "mount ID %q", mountID)
		}
		if getErr != nil {
			return errors.Wrap(getErr, "failed to read mount record")
		}
		data, valueErr := item.ValueCopy(nil)
		if valueErr != nil {
			return errors.Wrap(valueErr, "failed to read mount record")
		}
		decoded, decodeErr := decodeRecord(data)
		if decodeErr != nil {
			return decodeErr
		}
		record = decoded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// List returns every stored mount record in unspecified order.
func (repository *MountRepository) List(ctx context.Context) ([]*MountRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var records []*MountRecord
	err := repository.store.db.View(func(txn *badger.Txn) error {
		options := badger.DefaultIteratorOptions
		options.PrefetchValues = true
		prefix := []byte(mountKeyPrefix)
		iterator := txn.NewIterator(options)
		defer iterator.Close()
		for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
			data, err := iterator.Item().ValueCopy(nil)
			if err != nil {
				return errors.Wrap(err, "failed to read mount record")
			}
			record, err := decodeRecord(data)
			if err != nil {
				return err
			}
			records = append(records, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// Close releases the underlying Store.
func (repository *MountRepository) Close() error {
	return repository.store.Close()
}

func validateRecordForWrite(record *MountRecord) (string, error) {
	if record == nil || record.Info == nil {
		return "", errors.New("mount record is required")
	}
	if record.Info.MountId == "" {
		return "", errors.New("mount ID is required")
	}
	mountPath := record.Info.GetConfig().GetMountPath()
	if mountPath == "" || !filepath.IsAbs(mountPath) {
		return "", errors.New("mount path must be absolute")
	}
	return filepath.Clean(mountPath), nil
}

// storedRecord is the on-disk envelope for a MountRecord. Info is kept as
// protojson so schema evolution in the .proto file does not require a
// matching Go struct change here.
type storedRecord struct {
	SchemaVersion uint64          `json:"schema_version"`
	Tombstone     bool            `json:"tombstone"`
	Info          json.RawMessage `json:"info"`
}

func encodeRecord(record *MountRecord) ([]byte, error) {
	infoJSON, err := protojson.Marshal(record.Info)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal mount record")
	}
	data, err := json.Marshal(storedRecord{
		SchemaVersion: CurrentSchemaVersion,
		Tombstone:     record.Tombstone,
		Info:          infoJSON,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal mount record")
	}
	return data, nil
}

func decodeRecord(data []byte) (*MountRecord, error) {
	var stored storedRecord
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal mount record")
	}
	info := &api.MountInfo{}
	if err := protojson.Unmarshal(stored.Info, info); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal mount record")
	}
	return &MountRecord{Info: info, Tombstone: stored.Tombstone}, nil
}
