package store

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cyverse/irodsfsd/service/api"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newTestRepository(t *testing.T) *MountRepository {
	t.Helper()
	return NewMountRepository(openTestStore(t))
}

func newMountRecord(mountID string, mountPath string, password string) *MountRecord {
	now := timestamppb.New(time.Now())
	return &MountRecord{
		Info: &api.MountInfo{
			MountId: mountID,
			State:   api.MountState_MOUNT_STATE_PENDING_MOUNT,
			Attempt: 1,
			Config: &api.MountConfig{
				MountPath: mountPath,
				ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
					Account: &api.Account{
						IrodsHost:         "irods.example.org",
						IrodsZoneName:     "tempZone",
						IrodsUserName:     "alice",
						IrodsUserPassword: &password,
					},
				}},
			},
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
}

func mountPassword(record *MountRecord) string {
	return record.Info.GetConfig().GetIrodsfs().GetAccount().GetIrodsUserPassword()
}

func TestCreateAndGetRoundTripsUnredactedSecret(t *testing.T) {
	repository := newTestRepository(t)
	ctx := context.Background()

	if err := repository.Create(ctx, newMountRecord("mount-1", "/mnt/irods/alice", "super-secret")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := repository.Get(ctx, "mount-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if mountPassword(got) != "super-secret" {
		t.Errorf("password = %q, want %q", mountPassword(got), "super-secret")
	}
	if got.Tombstone {
		t.Error("Tombstone = true, want false")
	}
}

func TestCreateDuplicateMountIDConflicts(t *testing.T) {
	repository := newTestRepository(t)
	ctx := context.Background()

	if err := repository.Create(ctx, newMountRecord("mount-1", "/mnt/irods/a", "x")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	err := repository.Create(ctx, newMountRecord("mount-1", "/mnt/irods/b", "y"))
	if !errors.Is(err, ErrMountIDConflict) {
		t.Fatalf("Create() error = %v, want ErrMountIDConflict", err)
	}

	// the original record must be untouched.
	got, err := repository.Get(ctx, "mount-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Info.Config.MountPath != "/mnt/irods/a" {
		t.Errorf("mount path = %q, want %q", got.Info.Config.MountPath, "/mnt/irods/a")
	}
}

func TestCreateDuplicateMountPathConflicts(t *testing.T) {
	repository := newTestRepository(t)
	ctx := context.Background()

	if err := repository.Create(ctx, newMountRecord("mount-1", "/mnt/irods/shared", "x")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	err := repository.Create(ctx, newMountRecord("mount-2", "/mnt/irods/shared", "y"))
	if !errors.Is(err, ErrMountPathConflict) {
		t.Fatalf("Create() error = %v, want ErrMountPathConflict", err)
	}

	if _, err := repository.Get(ctx, "mount-2"); !errors.Is(err, ErrMountNotFound) {
		t.Fatalf("Get() error = %v, want ErrMountNotFound", err)
	}
}

func TestCreateRejectsRelativeMountPath(t *testing.T) {
	repository := newTestRepository(t)
	err := repository.Create(context.Background(), newMountRecord("mount-1", "relative/path", "x"))
	if err == nil {
		t.Fatal("Create() error = nil, want error for a relative mount path")
	}
}

func TestCreateRejectsMissingMountID(t *testing.T) {
	repository := newTestRepository(t)
	err := repository.Create(context.Background(), newMountRecord("", "/mnt/irods/a", "x"))
	if err == nil {
		t.Fatal("Create() error = nil, want error for a missing mount ID")
	}
}

func TestUpdateUnknownMountFails(t *testing.T) {
	repository := newTestRepository(t)
	err := repository.Update(context.Background(), newMountRecord("missing", "/mnt/irods/a", "x"))
	if !errors.Is(err, ErrMountNotFound) {
		t.Fatalf("Update() error = %v, want ErrMountNotFound", err)
	}
}

func TestUpdatePersistsStateAndTombstone(t *testing.T) {
	repository := newTestRepository(t)
	ctx := context.Background()
	record := newMountRecord("mount-1", "/mnt/irods/alice", "x")
	if err := repository.Create(ctx, record); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	record.Info.State = api.MountState_MOUNT_STATE_UNMOUNTING
	record.Tombstone = true
	if err := repository.Update(ctx, record); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	got, err := repository.Get(ctx, "mount-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Info.State != api.MountState_MOUNT_STATE_UNMOUNTING {
		t.Errorf("state = %v, want %v", got.Info.State, api.MountState_MOUNT_STATE_UNMOUNTING)
	}
	if !got.Tombstone {
		t.Error("Tombstone = false, want true")
	}
}

func TestDeleteRemovesRecordAndFreesMountPath(t *testing.T) {
	repository := newTestRepository(t)
	ctx := context.Background()
	if err := repository.Create(ctx, newMountRecord("mount-1", "/mnt/irods/alice", "x")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repository.Delete(ctx, "mount-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := repository.Get(ctx, "mount-1"); !errors.Is(err, ErrMountNotFound) {
		t.Fatalf("Get() error = %v, want ErrMountNotFound", err)
	}

	// the freed mount path must be usable by a new mount.
	if err := repository.Create(ctx, newMountRecord("mount-2", "/mnt/irods/alice", "y")); err != nil {
		t.Fatalf("Create() error = %v, want the freed mount path to be reusable", err)
	}
}

func TestDeleteUnknownMountIsNoop(t *testing.T) {
	repository := newTestRepository(t)
	if err := repository.Delete(context.Background(), "missing"); err != nil {
		t.Fatalf("Delete() error = %v, want nil for an unknown mount ID", err)
	}
}

func TestGetUnknownMountFails(t *testing.T) {
	repository := newTestRepository(t)
	if _, err := repository.Get(context.Background(), "missing"); !errors.Is(err, ErrMountNotFound) {
		t.Fatalf("Get() error = %v, want ErrMountNotFound", err)
	}
}

func TestListReturnsAllRecords(t *testing.T) {
	repository := newTestRepository(t)
	ctx := context.Background()
	want := map[string]bool{"mount-1": false, "mount-2": false, "mount-3": false}
	for id := range want {
		if err := repository.Create(ctx, newMountRecord(id, "/mnt/irods/"+id, "x")); err != nil {
			t.Fatalf("Create(%q) error = %v", id, err)
		}
	}

	records, err := repository.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(records) != len(want) {
		t.Fatalf("List() returned %d records, want %d", len(records), len(want))
	}
	for _, record := range records {
		if _, ok := want[record.Info.MountId]; !ok {
			t.Errorf("List() returned unexpected mount ID %q", record.Info.MountId)
		}
		want[record.Info.MountId] = true
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("List() did not return mount ID %q", id)
		}
	}
}

func TestRecordsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	key := testEncryptionKey()
	ctx := context.Background()

	s, err := Open(dir, key)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	repository := NewMountRepository(s)
	if err := repository.Create(ctx, newMountRecord("mount-1", "/mnt/irods/alice", "secret")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(dir, key)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer reopened.Close()

	got, err := NewMountRepository(reopened).Get(ctx, "mount-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if mountPassword(got) != "secret" {
		t.Error("password did not survive closing and reopening the database")
	}
}
