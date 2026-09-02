package service

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/store"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// contextKey namespaces context values this package sets, so they never
// collide with keys set by other packages.
type contextKey string

const sourceAddressContextKey contextKey = "irodsfsd-source-address"

// withSourceAddress attaches the REST caller's address to ctx, so audit
// logging can record it the same way it already can for gRPC (via peer
// info baked into ctx by grpc-go itself).
func withSourceAddress(ctx context.Context, address string) context.Context {
	return context.WithValue(ctx, sourceAddressContextKey, address)
}

func sourceAddressFromContext(ctx context.Context) string {
	if address, ok := ctx.Value(sourceAddressContextKey).(string); ok && address != "" {
		return address
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

type mountOperations interface {
	Mount(context.Context, *api.MountRequest) (*api.MountInfo, error)
	Unmount(context.Context, *api.UnmountRequest) (*api.MountInfo, error)
	ListMounts(context.Context, *api.ListMountsRequest) ([]*api.MountInfo, error)
	GetMount(context.Context, string) (*api.MountInfo, error)
	Ready(context.Context) error
}

// MountServer exposes MountManager through the generated gRPC contract.
type MountServer struct {
	api.UnimplementedMountServiceServer

	manager mountOperations
}

func NewMountServer(manager *MountManager) (*MountServer, error) {
	return newMountServer(manager)
}

func newMountServer(manager mountOperations) (*MountServer, error) {
	if manager == nil {
		return nil, errors.New("mount manager is required")
	}
	return &MountServer{manager: manager}, nil
}

func (server *MountServer) Mount(ctx context.Context, request *api.MountRequest) (*api.MountResponse, error) {
	mount, err := server.manager.Mount(ctx, request)
	mountID := request.GetMountId()
	if mount != nil {
		mountID = mount.GetMountId()
	}
	auditMountEvent(ctx, "mount", mountID, request.GetConfig().GetMountPath(), err)
	if err != nil {
		return nil, mountStatusError(err, mount == nil)
	}
	return &api.MountResponse{Mount: mount}, nil
}

func (server *MountServer) Unmount(ctx context.Context, request *api.UnmountRequest) (*api.UnmountResponse, error) {
	if request == nil || request.MountId == "" {
		return nil, status.Error(codes.InvalidArgument, "mount ID is required")
	}
	mount, err := server.manager.Unmount(ctx, request)
	auditMountEvent(ctx, "unmount", request.MountId, mount.GetConfig().GetMountPath(), err)
	if err != nil {
		return nil, mountStatusError(err, false)
	}
	return &api.UnmountResponse{Mount: mount}, nil
}

// auditMountEvent records who asked for a mount-changing action, on which
// mount ID and path, and whether it was accepted — without ever logging
// credentials or a complete configuration.
func auditMountEvent(ctx context.Context, action string, mountID string, mountPath string, err error) {
	entry := log.WithFields(log.Fields{
		"audit":      true,
		"action":     action,
		"mount_id":   mountID,
		"mount_path": mountPath,
		"source":     sourceAddressFromContext(ctx),
	})
	if err != nil {
		entry.WithError(err).Warn("mount request denied or failed")
		return
	}
	entry.Info("mount request accepted")
}

func (server *MountServer) ListMounts(ctx context.Context, request *api.ListMountsRequest) (*api.ListMountsResponse, error) {
	mounts, err := server.manager.ListMounts(ctx, request)
	if err != nil {
		return nil, mountStatusError(err, false)
	}
	return &api.ListMountsResponse{Mounts: mounts}, nil
}

// Ready reports whether the underlying manager's dependencies are
// currently usable. It is not part of the gRPC MountService contract
// (design.md keeps health/readiness as plain REST endpoints); it exists so
// the REST handler can reach the manager without depending on the
// concrete *MountManager type.
func (server *MountServer) Ready(ctx context.Context) error {
	return server.manager.Ready(ctx)
}

func (server *MountServer) GetMount(ctx context.Context, request *api.GetMountRequest) (*api.GetMountResponse, error) {
	if request == nil || request.MountId == "" {
		return nil, status.Error(codes.InvalidArgument, "mount ID is required")
	}
	mount, err := server.manager.GetMount(ctx, request.MountId)
	if err != nil {
		return nil, mountStatusError(err, false)
	}
	return &api.GetMountResponse{Mount: mount}, nil
}

func mountStatusError(err error, invalidWhenUnknown bool) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}

	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrDAVFSLazyUnmount):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, ErrMountNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrMountIDConflict), errors.Is(err, ErrMountPathConflict),
		errors.Is(err, store.ErrMountIDConflict), errors.Is(err, store.ErrMountPathConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrMountLimitReached):
		return status.Error(codes.ResourceExhausted, err.Error())
	case invalidWhenUnknown:
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
