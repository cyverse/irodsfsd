package client

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	irodsfs_common_util "github.com/cyverse/irodsfs-common/util"
	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/rs/xid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	reconnectInitialInterval time.Duration = 1 * time.Second
	reconnectMaxInterval     time.Duration = 1 * time.Minute
	reconnectTimeout         time.Duration = 1 * time.Hour
)

// MountServiceClient is a client of mount service
type MountServiceClient struct {
	id               string
	address          string // host:port
	operationTimeout time.Duration
	grpcConnection   *grpc.ClientConn
	apiClient        api.MountServiceClient
	connected        bool
	autoReconnect    bool
	reconnectingFlag int32 // atomic: 0=normal, 1=reconnect in progress

	bgCancelMu sync.Mutex
	bgCancel   context.CancelFunc // cancels the running backgroundReconnect goroutine

	logger *log.Entry

	reconnectSequence uint64
	mutex             sync.RWMutex
}

// NewMountServiceClient creates a new mount service client
func NewMountServiceClient(address string, operationTimeout time.Duration, autoReconnect bool, logger *log.Entry) *MountServiceClient {
	clientID := xid.New().String()

	if logger == nil {
		logger = log.WithFields(log.Fields{
			"clientID": clientID,
		})
	} else {
		logger = logger.WithFields(log.Fields{
			"clientID": clientID,
		})
	}

	return &MountServiceClient{
		id:               clientID,
		address:          address,
		operationTimeout: operationTimeout,
		grpcConnection:   nil,
		connected:        false,
		autoReconnect:    autoReconnect,

		logger: logger,
	}
}

// isTransportError returns true for gRPC errors that indicate the server is unreachable.
func isTransportError(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	return st.Code() == codes.Unavailable
}

// waitForReady blocks until conn reaches connectivity.Ready, or the context
// expires, or the connection is shut down.
func waitForReady(ctx context.Context, conn *grpc.ClientConn) bool {
	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return true
		}
		if state == connectivity.Shutdown {
			return false
		}
		if !conn.WaitForStateChange(ctx, state) {
			return false
		}
	}
}

// startBackgroundReconnect creates a cancellable context, stores the cancel
// func so Disconnect() can stop the goroutine, then starts backgroundReconnect.
func (client *MountServiceClient) startBackgroundReconnect() {
	bgCtx, bgCancel := context.WithCancel(context.Background())
	sequence := atomic.AddUint64(&client.reconnectSequence, 1)

	client.bgCancelMu.Lock()
	if client.bgCancel != nil {
		client.bgCancel() // cancel any previous (should not happen, but be safe)
	}
	client.bgCancel = bgCancel
	client.bgCancelMu.Unlock()

	go client.backgroundReconnect(bgCtx, sequence)
}

// backgroundReconnect runs until ctx is cancelled (by Disconnect) or the
// connection is re-established. It uses exponential backoff capped at 1 min
// and gives up after 1 hr. While running, reconnectingFlag == 1 and every
// API call returns an error immediately.
func (client *MountServiceClient) backgroundReconnect(parent context.Context, sequence uint64) {
	defer func() {
		client.bgCancelMu.Lock()
		if atomic.LoadUint64(&client.reconnectSequence) == sequence {
			atomic.StoreInt32(&client.reconnectingFlag, 0)
			client.bgCancel = nil
		}
		client.bgCancelMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(parent, reconnectTimeout)
	defer cancel()

	interval := reconnectInitialInterval
	for ctx.Err() == nil && client.isConnected() {
		connection, apiClient, _, err := client.newConnection()
		if err != nil {
			client.logger.WithError(err).Warn("background reconnect failed to create connection")
		} else {
			connection.Connect()
			waitContext, waitCancel := context.WithTimeout(ctx, interval)
			ready := waitForReady(waitContext, connection)
			waitCancel()
			if !ready {
				_ = connection.Close()
			} else {
				client.mutex.Lock()
				if !client.connected || ctx.Err() != nil {
					client.mutex.Unlock()
					_ = connection.Close()
					return
				}
				oldConnection := client.grpcConnection
				client.grpcConnection = connection
				client.apiClient = apiClient
				client.mutex.Unlock()

				if oldConnection != nil {
					if closeErr := oldConnection.Close(); closeErr != nil {
						client.logger.WithError(closeErr).Warn("failed to close replaced gRPC connection")
					}
				}
				client.logger.Info("reconnected to irodsfsd")
				return
			}
		}

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		if interval < reconnectMaxInterval {
			interval *= 2
			if interval > reconnectMaxInterval {
				interval = reconnectMaxInterval
			}
		}
	}
	if parent.Err() == nil && client.isConnected() {
		client.logger.Error("background reconnect timed out")
	}
}

// newConnection creates a gRPC connection for the configured service endpoint.
func (client *MountServiceClient) newConnection() (*grpc.ClientConn, api.MountServiceClient, string, error) {
	scheme, endpoint, err := commons.ParseServiceEndpoint(client.address)
	if err != nil {
		return nil, nil, "", err
	}

	client.logger.Infof("scheme: %s, endpoint: %s", scheme, endpoint)
	if scheme != "unix" && scheme != "tcp" {
		schemeErr := errors.Newf("unknown protocol %q", scheme)
		client.logger.Error(schemeErr)
		return nil, nil, "", schemeErr
	}
	client.logger.Infof("Connecting to %s endpoint: %q", scheme, endpoint)

	dialer := &net.Dialer{}
	grpcDialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, scheme, endpoint)
	}
	connection, err := grpc.NewClient(
		"passthrough:///"+endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(grpcDialer),
	)
	if err != nil {
		return nil, nil, "", errors.Wrapf(err, "failed to create gRPC client for %q", client.address)
	}
	return connection, api.NewMountServiceClient(connection), scheme + "://" + endpoint, nil
}

// Connect connects to mount service
func (client *MountServiceClient) Connect() error {
	defer irodsfs_common_util.StackTraceFromPanic(client.logger)

	connection, apiClient, endpointDescription, err := client.newConnection()
	if err != nil {
		client.logger.WithError(err).Error("failed to create irodsfsd connection")
		return err
	}

	client.mutex.Lock()
	if client.connected {
		client.mutex.Unlock()
		_ = connection.Close()
		return errors.Newf("already connected to %q", client.address)
	}
	client.grpcConnection = connection
	client.apiClient = apiClient
	client.connected = true
	client.mutex.Unlock()
	client.logger.Infof("connected to irodsfsd at %s", endpointDescription)
	return nil
}

// disconnectConn tears down the gRPC connection without touching bgCancel or
// reconnectingFlag. Used internally by the reconnect paths so they don't
// accidentally cancel themselves or reset the in-progress flag.
func (client *MountServiceClient) disconnectConn() {
	client.mutex.Lock()
	connection := client.grpcConnection
	client.apiClient = nil
	client.grpcConnection = nil
	client.connected = false
	client.mutex.Unlock()

	if connection != nil {
		if err := connection.Close(); err != nil {
			client.logger.WithError(err).Warn("failed to close gRPC connection")
		}
	}
}

// Disconnect disconnects connection from mount service
func (client *MountServiceClient) Disconnect() {
	// Stop any running background reconnect goroutine.
	atomic.AddUint64(&client.reconnectSequence, 1)
	client.bgCancelMu.Lock()
	if client.bgCancel != nil {
		client.bgCancel()
		client.bgCancel = nil
	}
	client.bgCancelMu.Unlock()
	atomic.StoreInt32(&client.reconnectingFlag, 0)

	client.disconnectConn()
}

func (client *MountServiceClient) getAPIClient() (api.MountServiceClient, error) {
	if atomic.LoadInt32(&client.reconnectingFlag) == 1 {
		return nil, status.Error(codes.Unavailable, "irodsfsd reconnect in progress; retry later")
	}
	client.mutex.RLock()
	defer client.mutex.RUnlock()
	if !client.connected || client.apiClient == nil {
		return nil, errors.New("client is not connected")
	}
	return client.apiClient, nil
}

func (client *MountServiceClient) isConnected() bool {
	client.mutex.RLock()
	defer client.mutex.RUnlock()
	return client.connected
}

func (client *MountServiceClient) getContextWithDeadline() (context.Context, context.CancelFunc) {
	if client.operationTimeout > 0 {
		return context.WithTimeout(context.Background(), client.operationTimeout)
	}
	return context.WithCancel(context.Background())
}

func (client *MountServiceClient) doWithReconnect(f func() (interface{}, error)) (interface{}, error) {
	// While background reconnect is running every call fails immediately.
	if atomic.LoadInt32(&client.reconnectingFlag) == 1 {
		return nil, status.Error(codes.Unavailable, "irodsfsd reconnect in progress; please retry later")
	}

	res, err := f()
	if err == nil {
		return res, nil
	}
	client.logger.WithError(err).Error("mount service request failed")
	if !client.autoReconnect || !isTransportError(err) || !client.isConnected() {
		return res, err
	}

	// Only one goroutine handles the reconnect; others return immediately.
	if !atomic.CompareAndSwapInt32(&client.reconnectingFlag, 0, 1) {
		return res, err
	}

	// Try one immediate connection before handing recovery to the background loop.
	connection, apiClient, _, reconnectErr := client.newConnection()
	if reconnectErr == nil {
		connection.Connect()
		waitContext, waitCancel := context.WithTimeout(context.Background(), reconnectInitialInterval)
		ready := waitForReady(waitContext, connection)
		waitCancel()
		if ready {
			client.mutex.Lock()
			if client.connected {
				oldConnection := client.grpcConnection
				client.grpcConnection = connection
				client.apiClient = apiClient
				client.mutex.Unlock()

				if oldConnection != nil {
					if closeErr := oldConnection.Close(); closeErr != nil {
						client.logger.WithError(closeErr).Warn("failed to close replaced gRPC connection")
					}
				}
				atomic.StoreInt32(&client.reconnectingFlag, 0)
				return f()
			}
			client.mutex.Unlock()
		}
		_ = connection.Close()
	} else {
		client.logger.WithError(reconnectErr).Warn("immediate reconnect failed to create connection")
	}

	if client.isConnected() {
		client.logger.Warn("immediate reconnect failed; starting background reconnect")
		client.startBackgroundReconnect()
	} else {
		atomic.StoreInt32(&client.reconnectingFlag, 0)
	}
	return res, err
}

// Mount requests a mount with a server-generated mount ID. When wait is
// true, it returns only after the mount reaches MOUNTED or FAILED.
func (client *MountServiceClient) Mount(config *MountConfig, wait bool) (*MountInfo, error) {
	return client.MountWithID("", config, wait)
}

// MountWithID requests a mount using mountID. An empty mountID has the same
// behavior as Mount and lets the server generate the ID. When wait is true,
// the client consumes the daemon's lifecycle event stream until the mount is
// usable or has failed.
func (client *MountServiceClient) MountWithID(mountID string, config *MountConfig, wait bool) (*MountInfo, error) {
	if config == nil {
		return nil, errors.New("mount config is required")
	}

	defer irodsfs_common_util.StackTraceFromPanic(client.logger)

	mountFunc := func() (interface{}, error) {
		ctx, cancel := client.getContextWithDeadline()
		defer cancel()

		request := &api.MountRequest{
			Config: config,
		}

		if mountID != "" {
			request.MountId = &mountID
		}

		apiClient, err := client.getAPIClient()
		if err != nil {
			return nil, err
		}
		return apiClient.Mount(ctx, request)
	}

	res, err := client.doWithReconnect(mountFunc)
	if err != nil {
		client.logger.Error(err)
		return nil, err
	}

	response, ok := res.(*api.MountResponse)
	if !ok {
		err = errors.New("failed to convert interface to MountResponse")
		client.logger.Error(err)
		return nil, err
	}
	mount := response.GetMount()
	if !wait {
		return mount, nil
	}
	return client.waitForMount(mount.GetMountId())
}

func (client *MountServiceClient) waitForMount(mountID string) (*MountInfo, error) {
	if mountID == "" {
		return nil, errors.New("mount ID is required")
	}
	ctx, cancel := client.getContextWithDeadline()
	defer cancel()
	stream, err := client.WatchMountEvents(ctx, &WatchMountEventsRequest{
		MountIds:       []string{mountID},
		IncludeCurrent: true,
	})
	if err != nil {
		return nil, err
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			return nil, errors.Wrapf(err, "failed while waiting for mount %q", mountID)
		}
		mount := event.GetMount()
		if mount == nil || mount.GetMountId() != mountID {
			continue
		}
		switch mount.GetState() {
		case api.MountState_MOUNT_STATE_MOUNTED, api.MountState_MOUNT_STATE_FAILED:
			return mount, nil
		}
		if event.GetType() == api.MountEventType_MOUNT_EVENT_TYPE_REMOVED {
			return nil, errors.Newf("mount %q was removed while waiting", mountID)
		}
	}
}

// WatchMountEvents opens a server-streaming lifecycle event subscription.
// The caller owns ctx and should cancel it when it no longer needs events.
func (client *MountServiceClient) WatchMountEvents(ctx context.Context, request *WatchMountEventsRequest) (MountEventStream, error) {
	if request == nil {
		request = &WatchMountEventsRequest{}
	}
	apiClient, err := client.getAPIClient()
	if err != nil {
		return nil, err
	}
	return apiClient.WatchMountEvents(ctx, request)
}

// Unmount requests removal of the mount identified by mountID.
func (client *MountServiceClient) Unmount(mountID string) (*MountInfo, error) {
	if mountID == "" {
		return nil, errors.New("mount ID is required")
	}

	defer irodsfs_common_util.StackTraceFromPanic(client.logger)

	unmountFunc := func() (interface{}, error) {
		ctx, cancel := client.getContextWithDeadline()
		defer cancel()

		request := &api.UnmountRequest{
			MountId: mountID,
		}

		apiClient, err := client.getAPIClient()
		if err != nil {
			return nil, err
		}
		return apiClient.Unmount(ctx, request)
	}

	res, err := client.doWithReconnect(unmountFunc)
	if err != nil {
		client.logger.Error(err)
		return nil, err
	}

	response, ok := res.(*api.UnmountResponse)
	if !ok {
		err = errors.New("failed to convert interface to UnmountResponse")
		client.logger.Error(err)
		return nil, err
	}
	return response.GetMount(), nil
}

// ListMounts returns mounts matching filter. A nil filter lists all mounts.
func (client *MountServiceClient) ListMounts(filter *ListMountsFilter) ([]*MountInfo, error) {
	defer irodsfs_common_util.StackTraceFromPanic(client.logger)

	listMountsFunc := func() (interface{}, error) {
		ctx, cancel := client.getContextWithDeadline()
		defer cancel()

		request := &api.ListMountsRequest{
			States: []api.MountState{},
		}

		if filter != nil {
			request.States = append(request.States, filter.States...)
			request.MountPathPrefix = filter.MountPathPrefix
			request.ClientUser = filter.ClientUser
		}

		apiClient, err := client.getAPIClient()
		if err != nil {
			return nil, err
		}
		return apiClient.ListMounts(ctx, request)
	}

	res, err := client.doWithReconnect(listMountsFunc)
	if err != nil {
		client.logger.Error(err)
		return nil, err
	}

	response, ok := res.(*api.ListMountsResponse)
	if !ok {
		err = errors.New("failed to convert interface to ListMountsResponse")
		client.logger.Error(err)
		return nil, err
	}
	return response.GetMounts(), nil
}

// GetMount returns the mount identified by mountID.
func (client *MountServiceClient) GetMount(mountID string) (*MountInfo, error) {
	if mountID == "" {
		return nil, errors.New("mount ID is required")
	}

	defer irodsfs_common_util.StackTraceFromPanic(client.logger)

	getMountFunc := func() (interface{}, error) {
		ctx, cancel := client.getContextWithDeadline()
		defer cancel()

		request := &api.GetMountRequest{
			MountId: mountID,
		}

		apiClient, err := client.getAPIClient()
		if err != nil {
			return nil, err
		}
		return apiClient.GetMount(ctx, request)
	}

	res, err := client.doWithReconnect(getMountFunc)
	if err != nil {
		client.logger.Error(err)
		return nil, err
	}

	response, ok := res.(*api.GetMountResponse)
	if !ok {
		err = errors.New("failed to convert interface to GetMountResponse")
		client.logger.Error(err)
		return nil, err
	}
	return response.GetMount(), nil
}
