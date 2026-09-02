package store

import (
	"testing"

	badger "github.com/dgraph-io/badger/v3"
)

func testEncryptionKey() []byte {
	return []byte("abcdefghijklmnopqrstuvwxyz012345")
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), testEncryptionKey())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenRequiresEncryptionKey(t *testing.T) {
	if _, err := Open(t.TempDir(), nil); err == nil {
		t.Fatal("Open() error = nil, want error for an empty encryption key")
	}
}

func TestOpenRejectsInvalidKeyLength(t *testing.T) {
	if _, err := Open(t.TempDir(), []byte("too-short")); err == nil {
		t.Fatal("Open() error = nil, want error for an invalid key length")
	}
}

func TestOpenInitializesSchemaVersion(t *testing.T) {
	s := openTestStore(t)

	var version uint64
	err := s.db.View(func(txn *badger.Txn) error {
		var readErr error
		version, readErr = readSchemaVersion(txn)
		return readErr
	})
	if err != nil {
		t.Fatalf("readSchemaVersion() error = %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", version, CurrentSchemaVersion)
	}
}

func TestOpenRunsPendingMigrations(t *testing.T) {
	originalMigrations := migrations
	defer func() { migrations = originalMigrations }()

	called := false
	migrations = map[uint64]func(txn *badger.Txn) error{
		0: func(txn *badger.Txn) error {
			called = true
			return nil
		},
	}

	openTestStore(t)

	if !called {
		t.Error("migration from schema version 0 was not invoked on a fresh database")
	}
}

func TestOpenPropagatesMigrationFailure(t *testing.T) {
	originalMigrations := migrations
	defer func() { migrations = originalMigrations }()

	migrationErr := "boom"
	migrations = map[uint64]func(txn *badger.Txn) error{
		0: func(txn *badger.Txn) error { return errFor(migrationErr) },
	}

	if _, err := Open(t.TempDir(), testEncryptionKey()); err == nil {
		t.Fatal("Open() error = nil, want error propagated from a failing migration")
	}
}

func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testEncryptionKey())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(metaSchemaVersionKey, encodeUint64(CurrentSchemaVersion+1))
	})
	if err != nil {
		t.Fatalf("failed to seed schema version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := Open(dir, testEncryptionKey()); err == nil {
		t.Fatal("Open() error = nil, want error for a schema version newer than supported")
	}
}

func TestOpenPersistsDataAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testEncryptionKey())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := s.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte("probe"), []byte("value"))
	}); err != nil {
		t.Fatalf("failed to write probe value: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(dir, testEncryptionKey())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer reopened.Close()

	var value []byte
	err = reopened.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte("probe"))
		if err != nil {
			return err
		}
		value, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		t.Fatalf("failed to read probe value: %v", err)
	}
	if string(value) != "value" {
		t.Errorf("probe value = %q, want %q", value, "value")
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testEncryptionKey())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	wrongKey := []byte("zyxwvutsrqponmlkjihgfedcba543210")
	if _, err := Open(dir, wrongKey); err == nil {
		t.Fatal("Open() error = nil, want error when reopening with the wrong encryption key")
	}
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

func errFor(message string) error { return simpleError(message) }
