package client

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyverse/irodsfsd/service/api"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type testMountServer struct {
	api.UnimplementedMountServiceServer

	mutex           sync.Mutex
	mounts          map[string]*api.MountInfo
	lastListRequest *api.ListMountsRequest
}

type testMountAPIClient struct {
	server      *testMountServer
	unavailable bool
}

func (client *testMountAPIClient) Mount(ctx context.Context, request *api.MountRequest, _ ...grpc.CallOption) (*api.MountResponse, error) {
	if client.unavailable {
		return nil, status.Error(codes.Unavailable, "test server unavailable")
	}
	return client.server.Mount(ctx, request)
}

func (client *testMountAPIClient) Unmount(ctx context.Context, request *api.UnmountRequest, _ ...grpc.CallOption) (*api.UnmountResponse, error) {
	if client.unavailable {
		return nil, status.Error(codes.Unavailable, "test server unavailable")
	}
	return client.server.Unmount(ctx, request)
}

func (client *testMountAPIClient) ListMounts(ctx context.Context, request *api.ListMountsRequest, _ ...grpc.CallOption) (*api.ListMountsResponse, error) {
	if client.unavailable {
		return nil, status.Error(codes.Unavailable, "test server unavailable")
	}
	return client.server.ListMounts(ctx, request)
}

func (client *testMountAPIClient) GetMount(ctx context.Context, request *api.GetMountRequest, _ ...grpc.CallOption) (*api.GetMountResponse, error) {
	if client.unavailable {
		return nil, status.Error(codes.Unavailable, "test server unavailable")
	}
	return client.server.GetMount(ctx, request)
}

func newTestMountServer() *testMountServer {
	return &testMountServer{mounts: map[string]*api.MountInfo{}}
}

func (server *testMountServer) Mount(_ context.Context, request *api.MountRequest) (*api.MountResponse, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	mountID := request.GetMountId()
	if mountID == "" {
		mountID = "generated-id"
	}
	if _, exists := server.mounts[mountID]; exists {
		return nil, status.Error(codes.AlreadyExists, "mount already exists")
	}
	info := &api.MountInfo{
		MountId: mountID,
		State:   api.MountState_MOUNT_STATE_MOUNTING,
		Config:  proto.Clone(request.Config).(*api.MountConfig),
	}
	server.mounts[mountID] = info
	return &api.MountResponse{Mount: proto.Clone(info).(*api.MountInfo)}, nil
}

func (server *testMountServer) Unmount(_ context.Context, request *api.UnmountRequest) (*api.UnmountResponse, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	info, exists := server.mounts[request.MountId]
	if !exists {
		return nil, status.Error(codes.NotFound, "mount not found")
	}
	delete(server.mounts, request.MountId)
	result := proto.Clone(info).(*api.MountInfo)
	result.State = api.MountState_MOUNT_STATE_UNMOUNTING
	return &api.UnmountResponse{Mount: result}, nil
}

func (server *testMountServer) ListMounts(_ context.Context, request *api.ListMountsRequest) (*api.ListMountsResponse, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.lastListRequest = proto.Clone(request).(*api.ListMountsRequest)
	result := make([]*api.MountInfo, 0, len(server.mounts))
	for _, info := range server.mounts {
		result = append(result, proto.Clone(info).(*api.MountInfo))
	}
	return &api.ListMountsResponse{Mounts: result}, nil
}

func (server *testMountServer) GetMount(_ context.Context, request *api.GetMountRequest) (*api.GetMountResponse, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	info, exists := server.mounts[request.MountId]
	if !exists {
		return nil, status.Error(codes.NotFound, "mount not found")
	}
	return &api.GetMountResponse{Mount: proto.Clone(info).(*api.MountInfo)}, nil
}

func TestMountServiceClientLifecycleAndOperations(t *testing.T) {
	client := startTestClient(t)
	config := &api.MountConfig{MountPath: "/mnt/test"}

	generated, err := client.Mount(config)
	if err != nil {
		t.Fatal(err)
	}
	if generated.MountId != "generated-id" || generated.State != api.MountState_MOUNT_STATE_MOUNTING {
		t.Fatalf("generated mount = %v", generated)
	}

	specified, err := client.MountWithID("specified-id", config)
	if err != nil {
		t.Fatal(err)
	}
	if specified.MountId != "specified-id" {
		t.Fatalf("specified mount ID = %q", specified.MountId)
	}

	fetched, err := client.GetMount("specified-id")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.GetConfig().GetMountPath() != "/mnt/test" {
		t.Fatalf("fetched mount path = %q", fetched.GetConfig().GetMountPath())
	}

	mountPathPrefix := "/mnt"
	clientUser := "test-user"

	mounts, err := client.ListMounts(&ListMountsFilter{
		States:          []MountState{MountStateMounting},
		MountPathPrefix: &mountPathPrefix,
		ClientUser:      &clientUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mount count = %d", len(mounts))
	}
	testAPIClient := client.apiClient.(*testMountAPIClient)
	testAPIClient.server.mutex.Lock()
	listRequest := proto.Clone(testAPIClient.server.lastListRequest).(*api.ListMountsRequest)
	testAPIClient.server.mutex.Unlock()
	if len(listRequest.States) != 1 || listRequest.States[0] != api.MountState_MOUNT_STATE_MOUNTING ||
		listRequest.GetMountPathPrefix() != "/mnt" || listRequest.GetClientUser() != "test-user" {
		t.Fatalf("list request = %v", listRequest)
	}

	unmounted, err := client.Unmount("specified-id")
	if err != nil {
		t.Fatal(err)
	}
	if unmounted.MountId != "specified-id" || unmounted.State != api.MountState_MOUNT_STATE_UNMOUNTING {
		t.Fatalf("unmounted = %v", unmounted)
	}
	if _, err := client.GetMount("specified-id"); status.Code(err) != codes.NotFound {
		t.Fatalf("GetMount error = %v", err)
	}
}

func TestMountServiceClientRequiresConnectionAndValidArguments(t *testing.T) {
	client := NewMountServiceClient("tcp://bufnet", time.Second, false, nil)
	if _, err := client.Mount(&api.MountConfig{}); err == nil {
		t.Fatal("Mount unexpectedly succeeded before Connect")
	}
	if _, err := client.Mount(nil); err == nil {
		t.Fatal("Mount unexpectedly accepted nil config")
	}
	if _, err := client.Unmount(""); err == nil {
		t.Fatal("Unmount unexpectedly accepted an empty ID")
	}
	if _, err := client.GetMount(""); err == nil {
		t.Fatal("GetMount unexpectedly accepted an empty ID")
	}
}

func TestMountServiceClientRejectsSecondConnect(t *testing.T) {
	client := startTestClient(t)
	if err := client.Connect(); err == nil {
		t.Fatal("second Connect unexpectedly succeeded")
	}
	client.Disconnect()
	client.Disconnect()
}

func TestMountServiceClientAddsClientIDToLogger(t *testing.T) {
	logger := log.New()
	logger.SetOutput(io.Discard)
	client := NewMountServiceClient("tcp://bufnet", time.Second, false, logger.WithField("component", "test"))
	if client.id == "" || client.logger.Data["clientID"] != client.id {
		t.Fatalf("client logger fields = %v", client.logger.Data)
	}
	if client.logger.Data["component"] != "test" {
		t.Fatalf("supplied logger fields were not retained: %v", client.logger.Data)
	}
}

func TestMountServiceClientGetContextWithDeadline(t *testing.T) {
	client := NewMountServiceClient("tcp://test.invalid:13020", time.Second, false, nil)
	ctx, cancel := client.getContextWithDeadline()
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("positive operation timeout did not create a deadline")
	}

	client.operationTimeout = 0
	ctx, cancel = client.getContextWithDeadline()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero operation timeout unexpectedly created a deadline")
	}
}

func TestMountServiceClientStartsAndCancelsBackgroundReconnect(t *testing.T) {
	serviceClient := startTestClient(t)
	serviceClient.autoReconnect = true
	serviceClient.apiClient = &testMountAPIClient{server: newTestMountServer(), unavailable: true}
	if _, err := serviceClient.ListMounts(nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("ListMounts error = %v, want Unavailable", err)
	}
	if atomic.LoadInt32(&serviceClient.reconnectingFlag) != 1 {
		t.Fatal("background reconnect was not started")
	}
	serviceClient.Disconnect()
	if atomic.LoadInt32(&serviceClient.reconnectingFlag) != 0 {
		t.Fatal("Disconnect did not cancel background reconnect")
	}
}

func startTestClient(t *testing.T) *MountServiceClient {
	t.Helper()
	serviceClient := NewMountServiceClient("tcp://test.invalid:13020", time.Second, false, nil)
	if err := serviceClient.Connect(); err != nil {
		t.Fatal(err)
	}
	serviceClient.apiClient = &testMountAPIClient{server: newTestMountServer()}
	t.Cleanup(func() {
		serviceClient.Disconnect()
	})
	return serviceClient
}
