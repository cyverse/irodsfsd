package client

import "github.com/cyverse/irodsfsd/service/api"

// Public aliases keep the common client result and filter types available
// without requiring callers to rename the generated API package.
type MountConfig = api.MountConfig
type IRODSFSConfig = api.IRODSFSConfig
type DAVFSConfig = api.DAVFSConfig
type NFSConfig = api.NFSConfig
type Account = api.Account
type PathMapping = api.PathMapping
type MountInfo = api.MountInfo
type MountState = api.MountState
type MountEvent = api.MountEvent
type MountEventType = api.MountEventType
type WatchMountEventsRequest = api.WatchMountEventsRequest
type MountEventStream = api.MountService_WatchMountEventsClient

// ListMountsFilter selects mounts returned by MountServiceClient.ListMounts.
// Empty fields do not restrict the result.
type ListMountsFilter struct {
	States          []MountState
	MountPathPrefix *string
	ClientUser      *string
}

const (
	MountStateUnspecified    = api.MountState_MOUNT_STATE_UNSPECIFIED
	MountStatePendingMount   = api.MountState_MOUNT_STATE_PENDING_MOUNT
	MountStateMounting       = api.MountState_MOUNT_STATE_MOUNTING
	MountStateMounted        = api.MountState_MOUNT_STATE_MOUNTED
	MountStatePendingUnmount = api.MountState_MOUNT_STATE_PENDING_UNMOUNT
	MountStateUnmounting     = api.MountState_MOUNT_STATE_UNMOUNTING
	MountStateRetryWait      = api.MountState_MOUNT_STATE_RETRY_WAIT
	MountStateFailed         = api.MountState_MOUNT_STATE_FAILED
	MountEventTypeSnapshot   = api.MountEventType_MOUNT_EVENT_TYPE_SNAPSHOT
	MountEventTypeUpdated    = api.MountEventType_MOUNT_EVENT_TYPE_UPDATED
	MountEventTypeRemoved    = api.MountEventType_MOUNT_EVENT_TYPE_REMOVED
)
