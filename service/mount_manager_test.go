package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/logstore"
	"github.com/cyverse/irodsfsd/service/store"
	"google.golang.org/protobuf/types/known/durationpb"
)

func testRepositoryEncryptionKey() []byte {
	return []byte("abcdefghijklmnopqrstuvwxyz012345")
}

// newTestRepository returns a real BadgerDB-backed repository rooted in a
// temporary directory. MountManager depends on the concrete store type, so
// tests exercise real persistence (including realistic conflicts by
// pre-seeding records) instead of a hand-rolled fake.
func newTestRepository(t *testing.T) *store.MountRepository {
	t.Helper()
	dataStore, err := store.Open(t.TempDir(), testRepositoryEncryptionKey())
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	return store.NewMountRepository(dataStore)
}

type fakeFuseController struct {
	mutex        sync.Mutex
	checkCalls   []bool
	checkErr     error
	unmountPaths []string
	unmountErr   error
}

func (controller *fakeFuseController) Check(checkFusermount bool) error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	controller.checkCalls = append(controller.checkCalls, checkFusermount)
	return controller.checkErr
}

func (controller *fakeFuseController) Unmount(mountPath string) error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	controller.unmountPaths = append(controller.unmountPaths, mountPath)
	return controller.unmountErr
}

func (controller *fakeFuseController) unmountCalls() []string {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	return append([]string(nil), controller.unmountPaths...)
}

func TestMountManagerMountAndUnmount(t *testing.T) {
	tempDir := t.TempDir()
	stdinPath := filepath.Join(tempDir, "stdin.json")
	argsPath := filepath.Join(tempDir, "args.txt")
	executablePath := makeFakeIRODSFS(t, tempDir, stdinPath, argsPath, false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	daemonConfig := commons.NewDefaultConfig()
	daemonConfig.IRODSFSExecutablePath = executablePath
	daemonConfig.MountRootPath = filepath.Join(tempDir, "data")
	daemonConfig.LogRootPath = filepath.Join(tempDir, "logs")
	daemonConfig.AllowedMountRootPaths = []string{tempDir}
	daemonConfig.MountTimeout = commons.Duration(time.Second)
	daemonConfig.UnmountTimeout = commons.Duration(3 * time.Second)

	fuse := &fakeFuseController{}
	manager, err := newMountManager(daemonConfig, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatalf("newMountManager() error = %v", err)
	}
	if !reflect.DeepEqual(fuse.checkCalls, []bool{true}) {
		t.Fatalf("Check calls = %v, want [true]", fuse.checkCalls)
	}

	mountID := "client-mount-id"
	response, err := manager.Mount(context.Background(), &api.MountRequest{
		MountId: &mountID,
		Config: &api.MountConfig{
			MountPath:    mountPath,
			ReadOnly:     true,
			MountOptions: []string{"allow_other"},
			ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
				Account: &api.Account{
					IrodsAuthenticationScheme:    stringPointer("native"),
					IrodsClientServerNegotiation: boolPointer(true),
					IrodsClientServerPolicy:      stringPointer("CS_NEG_REQUIRE"),
					IrodsHost:                    "irods.example.org",
					IrodsPort:                    1247,
					IrodsZoneName:                "tempZone",
					IrodsUserName:                "alice",
					IrodsUserPassword:            stringPointer("password-value"),
					IrodsTicket:                  stringPointer("ticket-value"),
					IrodsPamToken:                stringPointer("pam-value"),
				},
				PathMappings: []*api.PathMapping{{
					IrodsPath:           "/tempZone/home/alice",
					MappingPath:         "/",
					ResourceType:        "dir",
					CreateDir:           true,
					IgnoreNotExistError: true,
				}},
				ReadAheadMax: int32Pointer(256),
				ReadWriteMax: int32Pointer(128),
				FuseOptions:  []string{"direct_io"},
				MetadataConnection: &api.ConnectionConfig{
					CreationTimeout: durationpb.New(4 * time.Second),
					MaxNumber:       int32Pointer(12),
				},
				Cache: &api.CacheConfig{Backend: &api.CacheBackendConfig{
					Type: "redis",
					Redis: &api.RedisBackendConfig{
						Address:  stringPointer("redis.example.org:6379"),
						Password: stringPointer("redis-password"),
					},
				}},
				PoolEndpoint: stringPointer("unix:///run/irodsfs-pool.sock"),
				Debug:        true,
				Description:  stringPointer("test mount"),
			}},
		},
	})
	if err != nil {
		t.Fatalf("Mount() error = %v", err)
	}
	if response.MountId != mountID {
		t.Errorf("Mount ID = %q, want %q", response.MountId, mountID)
	}
	assertRedacted(t, response.Config.GetIrodsfs().Account)

	waitForMountState(t, manager, mountID, api.MountState_MOUNT_STATE_MOUNTED)
	waitForFile(t, stdinPath)

	argsBytes, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(argsBytes)), []string{"-f", "-c", "-", mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("irodsfs args = %v, want %v", got, want)
	}

	stdinBytes, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err = json.Unmarshal(stdinBytes, &input); err != nil {
		t.Fatalf("stdin is not valid JSON: %v\n%s", err, stdinBytes)
	}
	assertJSONValue(t, input, "irods_host", "irods.example.org")
	assertJSONValue(t, input, "irods_user_password", "password-value")
	assertJSONValue(t, input, "irods_ticket", "ticket-value")
	assertJSONValue(t, input, "irods_pam_token", "pam-value")
	assertJSONValue(t, input, "mount_path", mountPath)
	assertJSONValue(t, input, "readonly", true)
	assertJSONValue(t, input, "foreground", true)
	assertJSONValue(t, input, "instanceid", mountID)
	assertJSONValue(t, input, "read_ahead_max", float64(256))
	assertJSONValue(t, input, "read_write_max", float64(128))
	assertJSONValue(t, input, "description", "test mount")
	if got, want := input["fuse_options"], []any{"allow_other", "direct_io"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fuse_options = %#v, want %#v", got, want)
	}
	metadataConnection, ok := input["metadata_connection"].(map[string]any)
	if !ok {
		t.Fatalf("metadata_connection = %#v, want object", input["metadata_connection"])
	}
	assertJSONValue(t, metadataConnection, "creation_timeout", "4s")
	assertJSONValue(t, metadataConnection, "max_number", float64(12))
	cache, ok := input["cache"].(map[string]any)
	if !ok {
		t.Fatalf("cache = %#v, want object", input["cache"])
	}
	backend, ok := cache["backend"].(map[string]any)
	if !ok {
		t.Fatalf("cache.backend = %#v, want object", cache["backend"])
	}
	assertJSONValue(t, backend, "type", "redis")
	redis, ok := backend["redis"].(map[string]any)
	if !ok {
		t.Fatalf("cache.backend.redis = %#v, want object", backend["redis"])
	}
	assertJSONValue(t, redis, "address", "redis.example.org:6379")
	assertJSONValue(t, redis, "password", "redis-password")
	if _, exists := input["account"]; exists {
		t.Error("stdin config must flatten Account instead of containing an account object")
	}
	mappings, ok := input["path_mappings"].([]any)
	if !ok || len(mappings) != 1 {
		t.Fatalf("path_mappings = %#v, want one mapping", input["path_mappings"])
	}
	mapping := mappings[0].(map[string]any)
	assertJSONValue(t, mapping, "create_dir", true)
	if _, exists := mapping["create_directory"]; exists {
		t.Error("stdin mapping must use create_dir, not create_directory")
	}

	getResult, err := manager.GetMount(context.Background(), mountID)
	if err != nil {
		t.Fatalf("GetMount() error = %v", err)
	}
	assertRedacted(t, getResult.Config.GetIrodsfs().Account)
	if got := getResult.Config.GetIrodsfs().GetCache().GetBackend().GetRedis().GetPassword(); got != redactedValue {
		t.Errorf("Redis password = %q, want %q", got, redactedValue)
	}

	unmountResult, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: mountID})
	if err != nil {
		t.Fatalf("Unmount() error = %v", err)
	}
	if unmountResult.UnmountedAt == nil {
		t.Error("Unmount() did not set unmounted_at")
	}
	if got, want := fuse.unmountCalls(), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("Unmount calls = %v, want %v", got, want)
	}
	assertPathDoesNotExist(t, filepath.Join(daemonConfig.MountRootPath, mountID))
	if _, err = manager.GetMount(context.Background(), mountID); !errors.Is(err, ErrMountNotFound) {
		t.Errorf("GetMount() after unmount error = %v, want ErrMountNotFound", err)
	}
}

func TestMountManagerRejectsUnavailableFuse(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), true)
	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath

	fuseErr := errors.New("no fuse")
	_, err := newMountManager(config, &fakeFuseController{checkErr: fuseErr}, func(string) (bool, error) {
		return false, nil
	}, time.Now, newTestRepository(t))
	if !errors.Is(err, fuseErr) {
		t.Fatalf("newMountManager() error = %v, want %v", err, fuseErr)
	}
}

func TestMountTimeoutCleansPartialFuseMount(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.MountTimeout = commons.Duration(80 * time.Millisecond)
	config.Retry.MaxAttempts = 1 // this test wants the first timeout to be terminal, not retried
	fuse := &fakeFuseController{}
	probeCalls := 0
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		probeCalls++
		return path == mountPath && probeCalls > 1, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_FAILED)
	if got, want := fuse.unmountCalls(), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("timeout cleanup unmount calls = %v, want %v", got, want)
	}
	assertPathDoesNotExist(t, filepath.Join(config.MountRootPath, result.MountId))
	failed, err := manager.GetMount(context.Background(), result.MountId)
	if err != nil {
		t.Fatal(err)
	}
	if failed.GetLastError().GetCode() != "MOUNT_TIMEOUT" {
		t.Errorf("timeout error code = %q", failed.GetLastError().GetCode())
	}
}

func TestMountPreparationUsesMountTimeout(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.MountTimeout = commons.Duration(time.Nanosecond)
	// A deadline this short would fail every retry too, scheduling an
	// unbounded background retry loop past the end of this test.
	config.Retry.MaxAttempts = 1
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err == nil {
		t.Fatal("Mount unexpectedly succeeded after its preparation deadline")
	}
	if result.GetLastError().GetCode() != "MOUNT_TIMEOUT" {
		t.Errorf("preparation timeout code = %q", result.GetLastError().GetCode())
	}
	assertPathDoesNotExist(t, filepath.Join(config.MountRootPath, result.MountId))
}

func TestMountLifecycleLockDoesNotBlockManagerAccess(t *testing.T) {
	waiting := &managedMount{info: &api.MountInfo{
		MountId: "waiting",
		State:   api.MountState_MOUNT_STATE_MOUNTING,
		Config:  &api.MountConfig{MountPath: "/mnt/waiting"},
	}}
	other := &managedMount{info: &api.MountInfo{
		MountId: "other",
		State:   api.MountState_MOUNT_STATE_MOUNTED,
		Config:  &api.MountConfig{MountPath: "/mnt/other"},
	}}
	manager := &MountManager{mounts: map[string]*managedMount{
		waiting.info.MountId: waiting,
		other.info.MountId:   other,
	}}

	waiting.operation.Lock()
	defer waiting.operation.Unlock()

	done := make(chan error, 1)
	go func() {
		if _, err := manager.GetMount(context.Background(), "other"); err != nil {
			done <- err
			return
		}
		mounts, err := manager.ListMounts(context.Background(), nil)
		if err == nil && len(mounts) != 2 {
			err = fmt.Errorf("ListMounts returned %d mounts, want 2", len(mounts))
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("an unrelated manager query was blocked by a mount lifecycle lock")
	}
}

func TestRedactMountInfoRedactsDAVFSPassword(t *testing.T) {
	password := "davfs-password"
	info := &api.MountInfo{Config: &api.MountConfig{
		ClientConfig: &api.MountConfig_Davfs{Davfs: &api.DAVFSConfig{
			Url:      "https://dav.example.org",
			Username: stringPointer("alice"),
			Password: &password,
		}},
	}}

	redacted := redactMountInfo(info)
	if got := redacted.GetConfig().GetDavfs().GetPassword(); got != redactedValue {
		t.Errorf("redacted DAVFS password = %q, want %q", got, redactedValue)
	}
	if got := redacted.GetConfig().GetDavfs().GetUsername(); got != "alice" {
		t.Errorf("DAVFS username = %q, want input value %q", got, "alice")
	}
	if got := info.GetConfig().GetDavfs().GetPassword(); got != password {
		t.Errorf("original DAVFS password was modified: got %q", got)
	}
}

func TestMountManagerDAVFSMountAndUnmount(t *testing.T) {
	tempDir := t.TempDir()
	mountArgsPath := filepath.Join(tempDir, "mount-args")
	mountStdinPath := filepath.Join(tempDir, "mount-stdin")
	unmountArgsPath := filepath.Join(tempDir, "unmount-args")
	mountExecutable := makeRecordingCommand(t, tempDir, "mount", mountArgsPath, mountStdinPath)
	unmountExecutable := makeRecordingCommand(t, tempDir, "unmount", unmountArgsPath, filepath.Join(tempDir, "unmount-stdin"))
	irodsfsExecutable := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "irods-stdin"), filepath.Join(tempDir, "irods-args"), true)
	mountPath := filepath.Join(tempDir, "davfs-mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = irodsfsExecutable
	config.MountExecutablePath = mountExecutable
	config.UnmountExecutablePath = unmountExecutable
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath:    mountPath,
		ReadOnly:     true,
		MountOptions: []string{"dir_mode=0755"},
		ClientConfig: &api.MountConfig_Davfs{Davfs: &api.DAVFSConfig{
			Url:      "https://dav.example.org",
			Username: stringPointer("alice"),
			Password: stringPointer("davfs-password"),
			Config:   map[string]string{"use_locks": "0"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)
	waitForFile(t, mountArgsPath)

	args, err := os.ReadFile(mountArgsPath)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(args))
	if len(fields) != 6 || fields[0] != "-t" || fields[1] != "davfs" || fields[2] != "-o" || fields[4] != "https://dav.example.org" || fields[5] != mountPath {
		t.Fatalf("DAVFS mount args = %v", fields)
	}
	for _, option := range []string{"dir_mode=0755", "ro", "username=alice"} {
		if !strings.Contains(fields[3], option) {
			t.Errorf("DAVFS options %q do not contain %q", fields[3], option)
		}
	}
	waitForFile(t, mountStdinPath)
	stdin, err := os.ReadFile(mountStdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(stdin)); got != "davfs-password" {
		t.Errorf("DAVFS stdin = %q", got)
	}
	davfsConfigPath := filepath.Join(config.MountRootPath, result.MountId, "davfs2.conf")
	davfsConfig, err := os.ReadFile(davfsConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(davfsConfig), "use_locks 0\n") || !strings.Contains(string(davfsConfig), "cache_dir ") || !strings.Contains(string(davfsConfig), "kernel_fs fuse\n") {
		t.Errorf("DAVFS config = %q", davfsConfig)
	}

	if _, err = manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, unmountArgsPath)
	unmountArgs, err := os.ReadFile(unmountArgsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(unmountArgs)), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("DAVFS unmount args = %v, want %v", got, want)
	}
	assertPathDoesNotExist(t, filepath.Join(config.MountRootPath, result.MountId))
}

func TestMountManagerNFSMountDoesNotUseFuseUnmount(t *testing.T) {
	tempDir := t.TempDir()
	mountArgsPath := filepath.Join(tempDir, "mount-args")
	unmountArgsPath := filepath.Join(tempDir, "unmount-args")
	mountExecutable := makeRecordingCommand(t, tempDir, "mount", mountArgsPath, filepath.Join(tempDir, "mount-stdin"))
	unmountExecutable := makeRecordingCommand(t, tempDir, "unmount", unmountArgsPath, filepath.Join(tempDir, "unmount-stdin"))
	irodsfsExecutable := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "irods-stdin"), filepath.Join(tempDir, "irods-args"), true)
	mountPath := filepath.Join(tempDir, "nfs-mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = irodsfsExecutable
	config.MountExecutablePath = mountExecutable
	config.UnmountExecutablePath = unmountExecutable
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	fuse := &fakeFuseController{}
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fuse.checkCalls, []bool{true}) {
		t.Fatalf("FUSE startup checks = %v, want [true]", fuse.checkCalls)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath:    mountPath,
		ReadOnly:     true,
		MountOptions: []string{"vers=4.2"},
		ClientConfig: &api.MountConfig_Nfs{Nfs: &api.NFSConfig{
			Host: "nfs.example.org",
			Port: 2050,
			Path: "/exports/data",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)
	waitForFile(t, mountArgsPath)
	args, err := os.ReadFile(mountArgsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(args)), []string{"-t", "nfs", "-o", "vers=4.2,ro,port=2050", "nfs.example.org:/exports/data", mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("NFS mount args = %v, want %v", got, want)
	}

	if _, err = manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}
	if calls := fuse.unmountCalls(); len(calls) != 0 {
		t.Errorf("NFS called FUSE unmount: %v", calls)
	}
	unmountArgs, err := os.ReadFile(unmountArgsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(unmountArgs)), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("NFS unmount args = %v, want %v", got, want)
	}
	assertPathDoesNotExist(t, filepath.Join(config.MountRootPath, result.MountId))
}

func TestDAVFSUnmountTimeoutUsesLazyFuseAndPreservesCache(t *testing.T) {
	tempDir := t.TempDir()
	mountExecutable := makeRecordingCommand(t, tempDir, "mount", filepath.Join(tempDir, "mount-args"), filepath.Join(tempDir, "mount-stdin"))
	unmountExecutable := makeHangingCommand(t, tempDir, "unmount", filepath.Join(tempDir, "unmount-started"))
	irodsfsExecutable := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "irods-stdin"), filepath.Join(tempDir, "irods-args"), true)
	mountPath := filepath.Join(tempDir, "davfs-mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = irodsfsExecutable
	config.MountExecutablePath = mountExecutable
	config.UnmountExecutablePath = unmountExecutable
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.DAVFSUnmountTimeout = commons.Duration(30 * time.Millisecond)
	// A real retry would hit the same always-hanging fake unmount command
	// again in the background well after this test (and its repository) is
	// gone; this test only cares about the first attempt.
	config.Retry.MaxAttempts = 1
	fuse := &fakeFuseController{}
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Davfs{Davfs: &api.DAVFSConfig{
			Url:      "https://dav.example.org",
			Username: stringPointer("alice"),
			Password: stringPointer("davfs-password"),
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	unmounted, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId})
	if !errors.Is(err, ErrDAVFSLazyUnmount) {
		t.Fatalf("Unmount error = %v, want ErrDAVFSLazyUnmount", err)
	}
	if unmounted.GetLastError().GetCode() != "DAVFS_LAZY_UNMOUNT" {
		t.Errorf("lazy unmount error code = %q", unmounted.GetLastError().GetCode())
	}
	if got, want := fuse.unmountCalls(), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("DAVFS lazy FUSE unmount calls = %v, want %v", got, want)
	}
	dataRootPath := filepath.Join(config.MountRootPath, result.MountId)
	if _, statErr := os.Stat(dataRootPath); statErr != nil {
		t.Errorf("DAVFS cache directory %q was not preserved: %v", dataRootPath, statErr)
	}
}

func TestMountManagerGeneratesMountID(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.UnmountTimeout = commons.Duration(3 * time.Second)
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return true, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
			Account: &api.Account{
				IrodsHost:     "irods.example.org",
				IrodsUserName: "alice",
				IrodsZoneName: "tempZone",
			},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.MountId == "" {
		t.Fatal("Mount() did not generate a mount ID")
	}
	if err = validateMountID(result.MountId); err != nil {
		t.Errorf("generated mount ID is invalid: %v", err)
	}
	if _, err = manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}
}

func makeFakeIRODSFS(t *testing.T, directory string, stdinPath string, argsPath string, exitImmediately bool) string {
	t.Helper()
	executablePath := filepath.Join(directory, fmt.Sprintf("fake-irodsfs-%d", time.Now().UnixNano()))
	exitCommand := ""
	if exitImmediately {
		exitCommand = "exit 1"
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
cat > %q
%s
trap 'exit 0' INT TERM
while :; do sleep 1; done
`, argsPath, stdinPath, exitCommand)
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executablePath
}

func newTestIRODSFSMountConfig(mountPath string) *api.MountConfig {
	return &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
			Account: &api.Account{
				IrodsHost:     "irods.example.org",
				IrodsUserName: "alice",
				IrodsZoneName: "tempZone",
			},
		}},
	}
}

func makeRecordingCommand(t *testing.T, directory string, name string, argsPath string, stdinPath string) string {
	t.Helper()
	executablePath := filepath.Join(directory, fmt.Sprintf("fake-%s-%d", name, time.Now().UnixNano()))
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat > %q\n", argsPath, stdinPath)
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executablePath
}

func makeHangingCommand(t *testing.T, directory string, name string, markerPath string) string {
	t.Helper()
	executablePath := filepath.Join(directory, fmt.Sprintf("fake-hanging-%s-%d", name, time.Now().UnixNano()))
	script := fmt.Sprintf("#!/bin/sh\nprintf started > %q\nwhile :; do :; done\n", markerPath)
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executablePath
}

func waitForMountState(t *testing.T, manager *MountManager, mountID string, state api.MountState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, err := manager.GetMount(context.Background(), mountID)
		if err == nil && info.State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := manager.GetMount(context.Background(), mountID)
	t.Fatalf("mount state = %v, error = %v, want %v", info.GetState(), err, state)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q was not written", path)
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("path %q still exists or could not be checked: %v", path, err)
	}
}

func assertRedacted(t *testing.T, account *api.Account) {
	t.Helper()
	if account.GetIrodsUserPassword() != redactedValue || account.GetIrodsTicket() != redactedValue || account.GetIrodsPamToken() != redactedValue {
		t.Errorf("secrets were not redacted: password=%q ticket=%q pam_token=%q", account.GetIrodsUserPassword(), account.GetIrodsTicket(), account.GetIrodsPamToken())
	}
}

func assertJSONValue(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	if got := object[key]; !reflect.DeepEqual(got, want) {
		t.Errorf("JSON field %q = %#v, want %#v", key, got, want)
	}
}

func stringPointer(value string) *string {
	return &value
}

func boolPointer(value bool) *bool {
	return &value
}

func int32Pointer(value int32) *int32 {
	return &value
}

func TestMountPersistsRecordBeforeAndAfterEachTransition(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	repository := newTestRepository(t)
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	password := "super-secret"
	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
			Account: &api.Account{
				IrodsHost:         "irods.example.org",
				IrodsUserName:     "alice",
				IrodsZoneName:     "tempZone",
				IrodsUserPassword: &password,
			},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// The record must already exist, unredacted, before the mount is
	// confirmed: it was persisted before any process work began.
	record, err := repository.Get(context.Background(), result.MountId)
	if err != nil {
		t.Fatalf("repository.Get() error = %v", err)
	}
	if got := record.Info.GetConfig().GetIrodsfs().GetAccount().GetIrodsUserPassword(); got != password {
		t.Errorf("persisted password = %q, want %q (unredacted)", got, password)
	}
	if record.Tombstone {
		t.Error("Tombstone = true before Unmount, want false")
	}

	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)
	record = waitForPersistedState(t, repository, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)
	if record.Info.MountedAt == nil {
		t.Error("persisted record is missing mounted_at")
	}

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(context.Background(), result.MountId); !errors.Is(err, store.ErrMountNotFound) {
		t.Errorf("repository.Get() after unmount error = %v, want ErrMountNotFound", err)
	}
}

func TestMountFailsFastWhenRepositoryHasConflictingPath(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	repository := newTestRepository(t)
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return true, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a record already on disk for this path that MountManager has
	// not loaded into memory, e.g. a stale record from before startup
	// reconciliation runs.
	stalePassword := "stale"
	staleRecord := &store.MountRecord{Info: &api.MountInfo{
		MountId: "stale-mount",
		State:   api.MountState_MOUNT_STATE_MOUNTED,
		Config: &api.MountConfig{
			MountPath: mountPath,
			ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
				Account: &api.Account{
					IrodsHost:         "irods.example.org",
					IrodsUserName:     "bob",
					IrodsZoneName:     "tempZone",
					IrodsUserPassword: &stalePassword,
				},
			}},
		},
	}}
	if err := repository.Create(context.Background(), staleRecord); err != nil {
		t.Fatalf("repository.Create() error = %v", err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err == nil {
		t.Fatal("Mount() error = nil, want a persisted mount-path conflict error")
	}
	if !errors.Is(err, store.ErrMountPathConflict) {
		t.Errorf("Mount() error = %v, want store.ErrMountPathConflict", err)
	}
	if result.GetLastError().GetCode() != "MOUNT_PERSIST_FAILED" {
		t.Errorf("error code = %q, want MOUNT_PERSIST_FAILED", result.GetLastError().GetCode())
	}
	if _, statErr := os.Stat(filepath.Join(tempDir, "args")); !os.IsNotExist(statErr) {
		t.Error("Mount() started the irodsfs process despite a repository persist failure")
	}
}

func TestUnmountFailsFastWhenTombstonePersistFails(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	fuse := &fakeFuseController{}
	repository := newTestRepository(t)
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	// Remove the backing record out from under the manager so the tombstone
	// persist inside Unmount fails.
	if err := repository.Delete(context.Background(), result.MountId); err != nil {
		t.Fatalf("repository.Delete() error = %v", err)
	}

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); !errors.Is(err, store.ErrMountNotFound) {
		t.Fatalf("Unmount() error = %v, want store.ErrMountNotFound", err)
	}
	if len(fuse.unmountCalls()) != 0 {
		t.Error("Unmount() attempted a physical unmount despite a failed tombstone persist")
	}

	after, err := manager.GetMount(context.Background(), result.MountId)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != api.MountState_MOUNT_STATE_MOUNTED {
		t.Errorf("state after a failed tombstone persist = %v, want MOUNTED (reverted)", after.State)
	}

	// Restore a placeholder record so a real Unmount can tear the mount
	// down cleanly instead of leaking the still-running fake irodsfs
	// process past the end of this test.
	if err := repository.Create(context.Background(), &store.MountRecord{
		Info: &api.MountInfo{MountId: result.MountId, Config: &api.MountConfig{MountPath: mountPath}},
	}); err != nil {
		t.Fatalf("repository.Create() error = %v", err)
	}
	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatalf("Unmount() error = %v", err)
	}
}

func TestFailedUnmountPersistsTombstoneForRetry(t *testing.T) {
	tempDir := t.TempDir()
	mountExecutable := makeRecordingCommand(t, tempDir, "mount", filepath.Join(tempDir, "mount-args"), filepath.Join(tempDir, "mount-stdin"))
	unmountExecutable := makeHangingCommand(t, tempDir, "unmount", filepath.Join(tempDir, "unmount-started"))
	irodsfsExecutable := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "irods-stdin"), filepath.Join(tempDir, "irods-args"), true)
	mountPath := filepath.Join(tempDir, "davfs-mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = irodsfsExecutable
	config.MountExecutablePath = mountExecutable
	config.UnmountExecutablePath = unmountExecutable
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.DAVFSUnmountTimeout = commons.Duration(30 * time.Millisecond)
	// This test only exercises a single failed attempt; a real retry would
	// hit the same always-hanging fake unmount command again in the
	// background well after the test (and its repository) is gone.
	config.Retry.MaxAttempts = 1
	repository := newTestRepository(t)
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Davfs{Davfs: &api.DAVFSConfig{
			Url:      "https://dav.example.org",
			Username: stringPointer("alice"),
			Password: stringPointer("davfs-password"),
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); !errors.Is(err, ErrDAVFSLazyUnmount) {
		t.Fatalf("Unmount() error = %v, want ErrDAVFSLazyUnmount", err)
	}

	record := waitForPersistedState(t, repository, result.MountId, api.MountState_MOUNT_STATE_FAILED)
	if !record.Tombstone {
		t.Error("Tombstone = false after a failed unmount, want true so a restart resumes unmounting rather than remounting")
	}
}

// waitForPersistedState polls the repository, rather than the in-memory
// manager, since best-effort persistence happens slightly after an
// in-memory state transition.
func waitForPersistedState(t *testing.T, repository *store.MountRepository, mountID string, state api.MountState) *store.MountRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		record, err := repository.Get(context.Background(), mountID)
		if err != nil {
			t.Fatalf("repository.Get() error = %v", err)
		}
		if record.Info.State == state {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted state = %v, want %v", record.Info.State, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReconcileRemountsRecordWithNoActualMount(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	repository := newTestRepository(t)

	// Seed a record as if it were left behind by a previous daemon run that
	// is no longer actually mounted (e.g. it crashed before mounting, or
	// the mount was torn down by something else).
	if err := repository.Create(context.Background(), &store.MountRecord{Info: &api.MountInfo{
		MountId: "mount-1",
		State:   api.MountState_MOUNT_STATE_MOUNTED,
		Config:  newTestIRODSFSMountConfig(mountPath),
		Pid:     999999,
	}}); err != nil {
		t.Fatalf("repository.Create() error = %v", err)
	}

	probeCalls := 0
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		probeCalls++
		// Not mounted when Reconcile checks (call 1); mounted once the
		// fresh process has had a chance to start (later calls).
		return path == mountPath && probeCalls > 1, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	waitForMountState(t, manager, "mount-1", api.MountState_MOUNT_STATE_MOUNTED)
	waitForFile(t, filepath.Join(tempDir, "args"))

	after, err := manager.GetMount(context.Background(), "mount-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Pid == 999999 {
		t.Error("reconciliation carried over the stale PID instead of the new process's PID")
	}

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: "mount-1"}); err != nil {
		t.Fatalf("Unmount() error = %v", err)
	}
}

func TestReconcileDetachesAndRemountsSurvivingMount(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	repository := newTestRepository(t)

	if err := repository.Create(context.Background(), &store.MountRecord{Info: &api.MountInfo{
		MountId: "mount-1",
		State:   api.MountState_MOUNT_STATE_MOUNTED,
		Config:  newTestIRODSFSMountConfig(mountPath),
	}}); err != nil {
		t.Fatalf("repository.Create() error = %v", err)
	}

	fuse := &fakeFuseController{}
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if got, want := fuse.unmountCalls(), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("detach calls = %v, want %v (a surviving mount must be detached before remounting)", got, want)
	}
	waitForMountState(t, manager, "mount-1", api.MountState_MOUNT_STATE_MOUNTED)

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: "mount-1"}); err != nil {
		t.Fatalf("Unmount() error = %v", err)
	}
}

func TestReconcileResumesTombstoneCleanupWhenNotMounted(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	repository := newTestRepository(t)

	dataRootPath := filepath.Join(config.MountRootPath, "mount-1")
	if err := os.MkdirAll(dataRootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(context.Background(), &store.MountRecord{
		Info: &api.MountInfo{
			MountId: "mount-1",
			State:   api.MountState_MOUNT_STATE_UNMOUNTING,
			Config:  newTestIRODSFSMountConfig(mountPath),
		},
		Tombstone: true,
	}); err != nil {
		t.Fatalf("repository.Create() error = %v", err)
	}

	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if _, err := repository.Get(context.Background(), "mount-1"); !errors.Is(err, store.ErrMountNotFound) {
		t.Errorf("repository.Get() after reconciliation error = %v, want ErrMountNotFound", err)
	}
	assertPathDoesNotExist(t, dataRootPath)
	if _, err := manager.GetMount(context.Background(), "mount-1"); !errors.Is(err, ErrMountNotFound) {
		t.Errorf("a resumed-and-cleaned-up tombstone must not become a managed mount, GetMount() error = %v", err)
	}
}

func TestReconcilePreservesDAVFSCacheOnRepeatedLazyUnmountTimeout(t *testing.T) {
	tempDir := t.TempDir()
	irodsfsExecutable := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "irods-stdin"), filepath.Join(tempDir, "irods-args"), true)
	unmountExecutable := makeHangingCommand(t, tempDir, "unmount", filepath.Join(tempDir, "unmount-started"))
	mountPath := filepath.Join(tempDir, "davfs-mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = irodsfsExecutable
	config.UnmountExecutablePath = unmountExecutable
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.DAVFSUnmountTimeout = commons.Duration(30 * time.Millisecond)
	repository := newTestRepository(t)

	dataRootPath := filepath.Join(config.MountRootPath, "mount-1")
	cachePath := filepath.Join(dataRootPath, "cache")
	if err := os.MkdirAll(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(cachePath, "marker")
	if err := os.WriteFile(markerPath, []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}

	davfsConfig := &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Davfs{Davfs: &api.DAVFSConfig{
			Url:      "https://dav.example.org",
			Username: stringPointer("alice"),
			Password: stringPointer("davfs-password"),
		}},
	}
	if err := repository.Create(context.Background(), &store.MountRecord{
		Info:      &api.MountInfo{MountId: "mount-1", State: api.MountState_MOUNT_STATE_UNMOUNTING, Config: davfsConfig},
		Tombstone: true,
	}); err != nil {
		t.Fatalf("repository.Create() error = %v", err)
	}

	fuse := &fakeFuseController{}
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if got, want := fuse.unmountCalls(), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("lazy FUSE unmount calls = %v, want %v", got, want)
	}
	record, err := repository.Get(context.Background(), "mount-1")
	if err != nil {
		t.Fatalf("repository.Get() error = %v, want the record to be preserved", err)
	}
	if !record.Tombstone {
		t.Error("Tombstone = false, want true (still preserved for a future retry)")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("DAVFS cache marker was not preserved: %v", err)
	}
}

func TestReconcileConvergesAfterSimulatedRestart(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}

	databasePath := filepath.Join(tempDir, "db")
	dataStore, err := store.Open(databasePath, testRepositoryEncryptionKey())
	if err != nil {
		t.Fatal(err)
	}
	repository := store.NewMountRepository(dataStore)

	// Before the crash: a real Mount() through the normal API path.
	firstManager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}
	result, err := firstManager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, firstManager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)
	waitForPersistedState(t, repository, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	firstManager.mutex.RLock()
	firstEntry := firstManager.mounts[result.MountId]
	firstManager.mutex.RUnlock()

	// firstManager is about to be abandoned to simulate a crash, but its
	// waitForProcess goroutine is still blocked on Wait() and will remain so
	// until the leaked process is killed during test cleanup, by which time
	// this test's own repository has long since closed. Mark the entry as
	// already unmounting so that goroutine takes no action (in particular,
	// no persist against a closed database) once the process it is
	// supervising eventually dies; this is a test-cleanup concern only and
	// has no bearing on what was already durably persisted before the
	// simulated crash, which is what secondManager's Reconcile actually
	// exercises below.
	firstEntry.mutex.Lock()
	firstEntry.unmounting = true
	firstEntry.mutex.Unlock()
	t.Cleanup(func() { _ = firstEntry.command.Process.Kill() })

	// Simulate a daemon crash: discard firstManager and its supervising
	// goroutines without ever calling Unmount. The persisted record and the
	// "real" mount both survive; the daemon process does not.
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}

	// After the restart: a fresh manager sharing the same on-disk database.
	reopenedStore, err := store.Open(databasePath, testRepositoryEncryptionKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedStore.Close() })
	reopenedRepository := store.NewMountRepository(reopenedStore)

	fuse := &fakeFuseController{}
	secondManager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, reopenedRepository)
	if err != nil {
		t.Fatal(err)
	}

	if err := secondManager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if got, want := fuse.unmountCalls(), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("detach calls after restart = %v, want %v", got, want)
	}
	waitForMountState(t, secondManager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	if _, err := secondManager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatalf("Unmount() after reconciliation error = %v", err)
	}
}

func TestComputeRetryDelay(t *testing.T) {
	retry := commons.RetryConfig{
		MaxAttempts:  5,
		InitialDelay: commons.Duration(time.Second),
		MaxDelay:     commons.Duration(30 * time.Second),
		Multiplier:   2,
	}
	noJitter := func() float64 { return 0 }
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // capped at MaxDelay
		{0, time.Second},      // clamped to attempt 1
	}
	for _, tc := range tests {
		if got := computeRetryDelay(retry, tc.attempt, noJitter); got != tc.want {
			t.Errorf("computeRetryDelay(attempt=%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestComputeRetryDelayAppliesJitterWithoutGoingBelowBase(t *testing.T) {
	retry := commons.RetryConfig{
		InitialDelay: commons.Duration(time.Second),
		MaxDelay:     commons.Duration(30 * time.Second),
		Multiplier:   2,
		Jitter:       0.2,
	}
	base := computeRetryDelay(retry, 1, func() float64 { return 0 })
	jittered := computeRetryDelay(retry, 1, func() float64 { return 1 })
	if jittered <= base {
		t.Errorf("jittered delay %v should exceed the base delay %v", jittered, base)
	}
	if want := base + base*2/10; jittered != want {
		t.Errorf("jittered delay = %v, want %v (base + 20%% jitter at randFloat()=1)", jittered, want)
	}
}

// makeFlakyIRODSFS builds a fake irodsfs that crashes on its first
// invocation (after touching markerPath) and runs persistently on every
// invocation after that (after touching runningPath, so tests can tell a
// genuinely-running retry apart from the crashed first attempt without
// racing the crash).
func makeFlakyIRODSFS(t *testing.T, directory string, stdinPath string, argsPath string, markerPath string, runningPath string) string {
	t.Helper()
	executablePath := filepath.Join(directory, fmt.Sprintf("fake-flaky-irodsfs-%d", time.Now().UnixNano()))
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
cat > %q
if [ -e %q ]; then
	touch %q
	trap 'exit 0' INT TERM
	while :; do sleep 1; done
fi
touch %q
exit 1
`, argsPath, stdinPath, markerPath, runningPath, markerPath)
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executablePath
}

func TestMountRetriesTransientCrashThenSucceeds(t *testing.T) {
	tempDir := t.TempDir()
	argsPath := filepath.Join(tempDir, "args")
	markerPath := filepath.Join(tempDir, "marker")
	runningPath := filepath.Join(tempDir, "running")
	executablePath := makeFlakyIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), argsPath, markerPath, runningPath)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.Retry.InitialDelay = commons.Duration(20 * time.Millisecond)
	repository := newTestRepository(t)
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		if path != mountPath {
			return false, nil
		}
		_, statErr := os.Stat(runningPath)
		return statErr == nil, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}

	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	info, err := manager.GetMount(context.Background(), result.MountId)
	if err != nil {
		t.Fatal(err)
	}
	if info.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2 (one crash, one successful retry)", info.Attempt)
	}
	waitForPersistedState(t, repository, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}
}

func TestMountRetryExhaustionReachesFailed(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), true) // always crashes
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.Retry.InitialDelay = commons.Duration(10 * time.Millisecond)
	config.Retry.MaxAttempts = 2
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}

	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_FAILED)

	info, err := manager.GetMount(context.Background(), result.MountId)
	if err != nil {
		t.Fatal(err)
	}
	if info.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2 (retries exhausted at max_attempts)", info.Attempt)
	}
	if info.GetLastError().GetRetryable() {
		t.Error("LastError.Retryable = true after retries are exhausted, want false")
	}
	if info.NextRetryAt != nil {
		t.Error("NextRetryAt is set after retries are exhausted, want nil")
	}
}

func TestUnmountCancelsPendingMountRetryTimer(t *testing.T) {
	tempDir := t.TempDir()
	argsPath := filepath.Join(tempDir, "args")
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), argsPath, true) // always crashes
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.Retry.InitialDelay = commons.Duration(300 * time.Millisecond)
	repository := newTestRepository(t)
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_RETRY_WAIT)

	if err := os.Remove(argsPath); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Error("a cancelled retry timer still started a new mount attempt")
	}
	if _, err := repository.Get(context.Background(), result.MountId); !errors.Is(err, store.ErrMountNotFound) {
		t.Errorf("repository.Get() error = %v, want ErrMountNotFound", err)
	}
}

func TestUnmountSupersedesInFlightMountAttempt(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, nil // never appears mounted, so the attempt stays in MOUNTING
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTING)

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}

	// Give the superseded attempt's goroutines a moment to notice the
	// process exit; they must recognize their generation is stale and take
	// no action, rather than resurrecting the mount.
	time.Sleep(150 * time.Millisecond)
	if _, err := manager.GetMount(context.Background(), result.MountId); !errors.Is(err, ErrMountNotFound) {
		t.Error("mount reappeared after being superseded by Unmount while still MOUNTING")
	}
}

func TestShutdownStopsMountWithoutDeletingRecord(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	repository := newTestRepository(t)
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	manager.Shutdown(context.Background())

	record, err := repository.Get(context.Background(), result.MountId)
	if err != nil {
		t.Fatalf("repository.Get() error = %v, want the record preserved after a graceful shutdown", err)
	}
	if record.Tombstone {
		t.Error("Tombstone = true after graceful shutdown, want false so a restart remounts it")
	}
	if record.Info.Pid != 0 {
		t.Errorf("Pid = %d after shutdown, want 0", record.Info.Pid)
	}
	dataRootPath := filepath.Join(config.MountRootPath, result.MountId)
	if _, err := os.Stat(dataRootPath); err != nil {
		t.Errorf("data directory %q was removed by shutdown: %v", dataRootPath, err)
	}
}

func TestShutdownPreservesDAVFSCacheOnTimeout(t *testing.T) {
	tempDir := t.TempDir()
	mountExecutable := makeRecordingCommand(t, tempDir, "mount", filepath.Join(tempDir, "mount-args"), filepath.Join(tempDir, "mount-stdin"))
	unmountExecutable := makeHangingCommand(t, tempDir, "unmount", filepath.Join(tempDir, "unmount-started"))
	irodsfsExecutable := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "irods-stdin"), filepath.Join(tempDir, "irods-args"), true)
	mountPath := filepath.Join(tempDir, "davfs-mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = irodsfsExecutable
	config.MountExecutablePath = mountExecutable
	config.UnmountExecutablePath = unmountExecutable
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.DAVFSUnmountTimeout = commons.Duration(30 * time.Millisecond)
	fuse := &fakeFuseController{}
	repository := newTestRepository(t)
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Davfs{Davfs: &api.DAVFSConfig{
			Url:      "https://dav.example.org",
			Username: stringPointer("alice"),
			Password: stringPointer("davfs-password"),
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	manager.Shutdown(context.Background())

	if got, want := fuse.unmountCalls(), []string{mountPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("lazy FUSE unmount calls = %v, want %v", got, want)
	}
	record, err := repository.Get(context.Background(), result.MountId)
	if err != nil {
		t.Fatalf("repository.Get() error = %v, want the record preserved after shutdown", err)
	}
	if record.Tombstone {
		t.Error("Tombstone = true after graceful shutdown, want false")
	}
	dataRootPath := filepath.Join(config.MountRootPath, result.MountId)
	if _, err := os.Stat(dataRootPath); err != nil {
		t.Errorf("DAVFS cache directory %q was not preserved by shutdown: %v", dataRootPath, err)
	}
}

func TestShutdownStopsMountsInParallel(t *testing.T) {
	tempDir := t.TempDir()
	unmountExecutable := makeHangingCommand(t, tempDir, "unmount", filepath.Join(tempDir, "unmount-started"))
	irodsfsExecutable := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "irods-stdin"), filepath.Join(tempDir, "irods-args"), true)

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = irodsfsExecutable
	config.UnmountExecutablePath = unmountExecutable
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.UnmountTimeout = commons.Duration(200 * time.Millisecond)
	repository := newTestRepository(t)

	// Both mount paths are fixed before any Mount call so the probe
	// closure, read concurrently by background goroutines, is never
	// written to after manager construction begins.
	const mountCount = 2
	mountPaths := make([]string, mountCount)
	for i := range mountPaths {
		mountPath := filepath.Join(tempDir, fmt.Sprintf("nfs-mount-%d", i))
		if err := os.Mkdir(mountPath, 0o755); err != nil {
			t.Fatal(err)
		}
		mountPaths[i] = mountPath
	}

	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		for _, candidate := range mountPaths {
			if path == candidate {
				return true, nil
			}
		}
		return false, nil
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}

	for _, mountPath := range mountPaths {
		result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
			MountPath:    mountPath,
			ClientConfig: &api.MountConfig_Nfs{Nfs: &api.NFSConfig{Host: "nfs.example.org", Path: "/exports/data"}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)
	}

	start := time.Now()
	manager.Shutdown(context.Background())
	elapsed := time.Since(start)

	if elapsed > 350*time.Millisecond {
		t.Errorf("Shutdown() of %d mounts took %v, want close to one unmount_timeout (~200ms), not the sum of every mount's timeout", mountCount, elapsed)
	}
}

func TestReconcileSkipsAlreadyManagedMount(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	fuse := &fakeFuseController{}
	manager, err := newMountManager(config, fuse, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	// A periodic reconcile pass must never treat an actively-supervised,
	// healthy mount as an unsupervised survivor left by a previous daemon.
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if calls := fuse.unmountCalls(); len(calls) != 0 {
		t.Errorf("Reconcile() detached an already-managed mount: %v", calls)
	}
	info, err := manager.GetMount(context.Background(), result.MountId)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != api.MountState_MOUNT_STATE_MOUNTED {
		t.Errorf("state after a periodic reconcile pass = %v, want MOUNTED (unchanged)", info.State)
	}

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}
}

func TestMountLogCapturesOutputAcrossRetriesWithSecretsRedacted(t *testing.T) {
	tempDir := t.TempDir()
	markerPath := filepath.Join(tempDir, "marker")
	runningPath := filepath.Join(tempDir, "running")
	secret := "super-secret-password"
	executablePath := filepath.Join(tempDir, "fake-irodsfs")
	script := fmt.Sprintf(`#!/bin/sh
cat > /dev/null
if [ -e %q ]; then
	echo "connecting with password %s"
	touch %q
	trap 'exit 0' INT TERM
	while :; do sleep 1; done
fi
echo "boom" >&2
touch %q
exit 1
`, markerPath, secret, runningPath, markerPath)
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.Retry.InitialDelay = commons.Duration(20 * time.Millisecond)
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		if path != mountPath {
			return false, nil
		}
		_, statErr := os.Stat(runningPath)
		return statErr == nil, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	password := secret
	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: &api.MountConfig{
		MountPath: mountPath,
		ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
			Account: &api.Account{
				IrodsHost:         "irods.example.org",
				IrodsUserName:     "alice",
				IrodsZoneName:     "tempZone",
				IrodsUserPassword: &password,
			},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}

	rawLog, err := os.ReadFile(config.GetMountLogPath(result.MountId))
	if err != nil {
		t.Fatalf("failed to read mount log: %v", err)
	}
	if strings.Contains(string(rawLog), secret) {
		t.Errorf("mount log contains the raw secret: %s", rawLog)
	}

	records, err := logstore.Query(config.GetMountLogPath(result.MountId), logstore.QueryOptions{})
	if err != nil {
		t.Fatalf("logstore.Query() error = %v", err)
	}
	var sawCrash, sawRedactedConnect bool
	for _, record := range records {
		if record.Stream == "stderr" && record.Message == "boom" {
			sawCrash = true
		}
		if record.Stream == "stdout" && record.Message == "connecting with password [REDACTED]" {
			sawRedactedConnect = true
		}
	}
	if !sawCrash {
		t.Errorf("log did not retain the first (crashed) attempt's output: %+v", records)
	}
	if !sawRedactedConnect {
		t.Errorf("log did not contain the redacted line from the successful retry: %+v", records)
	}
}

func TestMountRejectsPathsConflictingWithReservedDaemonPaths(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	dataRootPath := filepath.Join(tempDir, "data")
	logRootPath := filepath.Join(tempDir, "logs")
	if err := os.MkdirAll(dataRootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(logRootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedInsideData := filepath.Join(dataRootPath, "nested")
	if err := os.MkdirAll(nestedInsideData, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.DataRootPath = dataRootPath
	config.MountRootPath = filepath.Join(dataRootPath, "mounts")
	config.LogRootPath = logRootPath
	config.AllowedMountRootPaths = []string{tempDir, string(filepath.Separator)}
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{"the filesystem root", string(filepath.Separator)},
		{"exactly the data root path", dataRootPath},
		{"exactly the log root path", logRootPath},
		{"a descendant of the data root path", nestedInsideData},
		{"an ancestor of the data root path", tempDir},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(test.path)})
			if err == nil {
				t.Fatalf("Mount() with mount path %q succeeded, want a reserved-path conflict error", test.path)
			}
		})
	}

	// A path that is merely a sibling of a reserved path (not an ancestor
	// or descendant of it) must still be allowed.
	siblingPath := filepath.Join(tempDir, "unrelated-mount")
	if err := os.Mkdir(siblingPath, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(siblingPath)})
	if err != nil {
		t.Fatalf("Mount() with an unrelated sibling path failed: %v", err)
	}
	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}
}
