package service

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type fakeMountOperations struct {
	mutex    sync.Mutex
	mounts   map[string]*api.MountInfo
	readyErr error
}

func newFakeMountOperations() *fakeMountOperations {
	return &fakeMountOperations{mounts: map[string]*api.MountInfo{}}
}

func (manager *fakeMountOperations) Mount(_ context.Context, request *api.MountRequest) (*api.MountInfo, error) {
	if request == nil || request.Config == nil {
		return nil, errors.New("mount config is required")
	}
	mountID := request.GetMountId()
	if mountID == "" {
		mountID = "generated-mount"
	}
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if _, exists := manager.mounts[mountID]; exists {
		return nil, ErrMountIDConflict
	}
	info := &api.MountInfo{MountId: mountID, State: api.MountState_MOUNT_STATE_MOUNTED, Config: proto.Clone(request.Config).(*api.MountConfig)}
	manager.mounts[mountID] = info
	return proto.Clone(info).(*api.MountInfo), nil
}

func (manager *fakeMountOperations) Unmount(_ context.Context, request *api.UnmountRequest) (*api.MountInfo, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	info, exists := manager.mounts[request.MountId]
	if !exists {
		return nil, ErrMountNotFound
	}
	delete(manager.mounts, request.MountId)
	return proto.Clone(info).(*api.MountInfo), nil
}

func (manager *fakeMountOperations) ListMounts(_ context.Context, _ *api.ListMountsRequest) ([]*api.MountInfo, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	mounts := make([]*api.MountInfo, 0, len(manager.mounts))
	for _, info := range manager.mounts {
		mounts = append(mounts, proto.Clone(info).(*api.MountInfo))
	}
	return mounts, nil
}

func (manager *fakeMountOperations) GetMount(_ context.Context, mountID string) (*api.MountInfo, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	info, exists := manager.mounts[mountID]
	if !exists {
		return nil, errors.Wrapf(ErrMountNotFound, "mount ID %q", mountID)
	}
	return proto.Clone(info).(*api.MountInfo), nil
}

func (manager *fakeMountOperations) Ready(_ context.Context) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	return manager.readyErr
}

func TestGRPCMountService(t *testing.T) {
	config := commons.NewDefaultConfig()
	config.ServiceEndpoint = "tcp://127.0.0.1:0"
	config.ManagementServicePort = 0
	svc, err := newService(config, newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	svc.listen = func(string, string) (net.Listener, error) { return listener, nil }
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Release)

	connection, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := api.NewMountServiceClient(connection)

	mountID := "mount-one"
	mounted, err := client.Mount(context.Background(), &api.MountRequest{
		MountId: &mountID,
		Config:  &api.MountConfig{MountPath: "/mnt/example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mounted.GetMount().GetMountId() != mountID {
		t.Fatalf("Mount ID = %q", mounted.GetMount().GetMountId())
	}

	got, err := client.GetMount(context.Background(), &api.GetMountRequest{MountId: mountID})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMount().GetConfig().GetMountPath() != "/mnt/example" {
		t.Fatalf("mount path = %q", got.GetMount().GetConfig().GetMountPath())
	}

	listed, err := client.ListMounts(context.Background(), &api.ListMountsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Mounts) != 1 || listed.Mounts[0].MountId != mountID {
		t.Fatalf("listed mounts = %v", listed.Mounts)
	}

	if _, err := client.Mount(context.Background(), &api.MountRequest{MountId: &mountID, Config: &api.MountConfig{MountPath: "/mnt/example"}}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate Mount code = %s, error = %v", status.Code(err), err)
	}
	if _, err := client.Unmount(context.Background(), &api.UnmountRequest{MountId: mountID}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetMount(context.Background(), &api.GetMountRequest{MountId: mountID}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing GetMount code = %s, error = %v", status.Code(err), err)
	}
	if _, err := client.GetMount(context.Background(), &api.GetMountRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty GetMount code = %s, error = %v", status.Code(err), err)
	}
}

func TestMountStatusErrorCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code codes.Code
	}{
		{name: "not found", err: ErrMountNotFound, code: codes.NotFound},
		{name: "path conflict", err: ErrMountPathConflict, code: codes.AlreadyExists},
		{name: "limit", err: ErrMountLimitReached, code: codes.ResourceExhausted},
		{name: "DAVFS timeout", err: ErrDAVFSLazyUnmount, code: codes.DeadlineExceeded},
		{name: "canceled", err: context.Canceled, code: codes.Canceled},
		{name: "internal", err: errors.New("failure"), code: codes.Internal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := status.Code(mountStatusError(test.err, false)); got != test.code {
				t.Fatalf("code = %s, want %s", got, test.code)
			}
		})
	}
}

func TestServiceRemovesUnixSocket(t *testing.T) {
	socketPath := t.TempDir() + "/irodsfsd.sock"
	config := commons.NewDefaultConfig()
	config.ServiceEndpoint = "unix://" + socketPath
	config.ManagementServicePort = 0
	svc, err := newService(config, newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	svc.listen = func(string, string) (net.Listener, error) { return listener, nil }
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketPath, []byte("socket placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Stop()
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("unix socket was not removed: %v", err)
	}
}

func TestMountManagerReadyChecksFuse(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	fuse := &fakeFuseController{}
	manager, err := newMountManager(config, fuse, func(string) (bool, error) { return false, nil }, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() error = %v, want nil", err)
	}

	fuse.checkErr = errors.New("fuse is unavailable")
	if err := manager.Ready(context.Background()); err == nil {
		t.Error("Ready() error = nil, want the FUSE check failure surfaced")
	}
}

func TestAuditLogRecordsMountAndUnmountWithoutSecrets(t *testing.T) {
	var buf bytes.Buffer
	originalOutput := log.StandardLogger().Out
	log.SetOutput(&buf)
	defer log.SetOutput(originalOutput)

	server, err := newMountServer(newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}

	password := "top-secret-audit-password"
	ctx := withSourceAddress(context.Background(), "203.0.113.5:54321")
	mounted, err := server.Mount(ctx, &api.MountRequest{
		MountId: stringPointer("audit-mount"),
		Config: &api.MountConfig{
			MountPath: "/mnt/audit",
			ClientConfig: &api.MountConfig_Irodsfs{Irodsfs: &api.IRODSFSConfig{
				Account: &api.Account{
					IrodsHost:         "irods.example.org",
					IrodsUserName:     "alice",
					IrodsZoneName:     "tempZone",
					IrodsUserPassword: &password,
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Unmount(ctx, &api.UnmountRequest{MountId: mounted.GetMount().GetMountId()}); err != nil {
		t.Fatal(err)
	}

	logged := buf.String()
	if strings.Contains(logged, password) {
		t.Errorf("audit log contains the raw password: %s", logged)
	}
	if !strings.Contains(logged, "action=mount") || !strings.Contains(logged, "mount_id=audit-mount") {
		t.Errorf("audit log missing the mount event: %s", logged)
	}
	if !strings.Contains(logged, "action=unmount") {
		t.Errorf("audit log missing the unmount event: %s", logged)
	}
	if !strings.Contains(logged, "203.0.113.5:54321") {
		t.Errorf("audit log missing the request source address: %s", logged)
	}
}

func TestRESTReadinessReflectsManagerReadiness(t *testing.T) {
	fake := newFakeMountOperations()
	server, err := newMountServer(fake)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewRESTHandler(server, commons.NewDefaultConfig()).RegisterRoutes(mux)

	response := performRESTRequest(t, mux, http.MethodGet, "/readyz", "")
	if response.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200 when the manager is ready", response.Code)
	}

	fake.mutex.Lock()
	fake.readyErr = errors.New("repository is unreachable")
	fake.mutex.Unlock()

	response = performRESTRequest(t, mux, http.MethodGet, "/readyz", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 when the manager is not ready, body = %s", response.Code, response.Body.String())
	}
}
