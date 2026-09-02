package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	irodsfs_commons "github.com/cyverse/irodsfs/commons"
	irodsfsd_commons "github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/logstore"
	"github.com/cyverse/irodsfsd/service/store"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	redactedValue         = "[REDACTED]"
	mountProbeInterval    = 100 * time.Millisecond
	defaultMountTimeout   = 30 * time.Second
	defaultUnmountTimeout = 15 * time.Second
)

var (
	ErrMountNotFound     = errors.New("mount not found")
	ErrMountIDConflict   = errors.New("mount ID is already in use")
	ErrMountPathConflict = errors.New("mount path is already in use")
	ErrMountLimitReached = errors.New("maximum number of mounts reached")
	ErrDAVFSLazyUnmount  = errors.New("DAVFS was lazily unmounted with cache data preserved")
)

type fuseController interface {
	Check(bool) error
	Unmount(string) error
}

type systemFuseController struct{}

func (systemFuseController) Check(checkFusermount bool) error {
	return irodsfs_commons.CheckFuse(checkFusermount)
}

func (systemFuseController) Unmount(mountPath string) error {
	return irodsfs_commons.UnmountFuse(mountPath)
}

type mountProbe func(string) (bool, error)

type managedMount struct {
	mutex      sync.RWMutex
	operation  sync.Mutex
	info       *api.MountInfo
	command    *exec.Cmd
	client     mountClientType
	persistent bool
	exitDone   chan struct{}
	unmounting bool
	timedOut   bool
	// stopping is set by graceful shutdown before it signals the child
	// process, so waitForProcess recognizes the resulting exit as expected
	// rather than a crash. It is distinct from unmounting: shutdown must
	// never tombstone the record (persistRecord's Tombstone is derived from
	// unmounting only), since an ordinary record is what makes the next
	// restart's reconciliation remount it.
	stopping bool

	// generation identifies the current mount or unmount attempt. It is
	// bumped every time a fresh attempt starts (initial Mount, a scheduled
	// retry, or an accepted Unmount superseding an in-flight mount attempt),
	// so a goroutine or timer left over from a superseded attempt can
	// recognize itself as stale and take no action.
	generation uint64
	// retryTimer is the pending scheduled retry for this entry's current
	// generation, if any. A new mount or unmount request cancels it.
	retryTimer *time.Timer

	// mountLog is opened once per entry (not per attempt) and reused across
	// retries and remounts, so its history stays queryable across a child
	// restart. It is nil until the first startMount call for this entry.
	mountLog *logstore.MountLog
}

// MountManager owns irodsfs processes, system mount commands, and their
// in-memory state.
type MountManager struct {
	config     *irodsfsd_commons.Config
	fuse       fuseController
	probe      mountProbe
	now        func() time.Time
	repository *store.MountRepository
	// randFloat returns a value in [0, 1) used to jitter retry delays. Tests
	// may overwrite it directly for deterministic backoff timing.
	randFloat func() float64
	// metrics is nil unless explicitly set (see Service, which owns
	// creating it): every metrics call site must tolerate that.
	metrics *Metrics

	// mutex protects only the mounts registry. It is never held while running
	// filesystem operations or waiting for a client process.
	mutex  sync.RWMutex
	mounts map[string]*managedMount
}

// NewMountManager validates FUSE and constructs a process manager. Mount
// intent and state transitions are persisted through repository so they
// survive a daemon restart.
func NewMountManager(config *irodsfsd_commons.Config, repository *store.MountRepository) (*MountManager, error) {
	return newMountManager(config, systemFuseController{}, isMountPoint, time.Now, repository)
}

func newMountManager(config *irodsfsd_commons.Config, fuse fuseController, probe mountProbe, now func() time.Time, repository *store.MountRepository) (*MountManager, error) {
	if config == nil {
		return nil, errors.New("daemon config is required")
	}
	if fuse == nil {
		return nil, errors.New("FUSE controller is required")
	}
	if probe == nil {
		return nil, errors.New("mount probe is required")
	}
	if repository == nil {
		return nil, errors.New("mount repository is required")
	}
	if now == nil {
		now = time.Now
	}
	if err := fuse.Check(true); err != nil {
		return nil, errors.Wrap(err, "FUSE is unavailable")
	}
	if err := validateExecutable(config.IRODSFSExecutablePath); err != nil {
		return nil, err
	}
	if err := validateExecutable(config.MountExecutablePath); err != nil {
		return nil, err
	}
	if err := validateExecutable(config.UnmountExecutablePath); err != nil {
		return nil, err
	}

	return &MountManager{
		config:     config,
		fuse:       fuse,
		probe:      probe,
		now:        now,
		repository: repository,
		randFloat:  mathrand.Float64,
		mounts:     map[string]*managedMount{},
	}, nil
}

// maxAttempts returns the configured maximum number of attempts for a mount
// or unmount operation, falling back to the documented default.
func (manager *MountManager) maxAttempts() int {
	if manager.config.Retry.MaxAttempts > 0 {
		return manager.config.Retry.MaxAttempts
	}
	return irodsfsd_commons.RetryMaxAttemptsDefault
}

// computeRetryDelay returns the backoff delay after the given attempt
// number has failed, following the configured initial delay, multiplier,
// and max delay, with optional jitter added on top (never subtracted, so
// the delay never drops below the unjittered backoff value).
func computeRetryDelay(retry irodsfsd_commons.RetryConfig, attempt int, randFloat func() float64) time.Duration {
	initial := time.Duration(retry.InitialDelay)
	if initial <= 0 {
		initial = irodsfsd_commons.RetryInitialDelayDefault
	}
	maxDelay := time.Duration(retry.MaxDelay)
	if maxDelay <= 0 {
		maxDelay = irodsfsd_commons.RetryMaxDelayDefault
	}
	multiplier := retry.Multiplier
	if multiplier < 1 {
		multiplier = irodsfsd_commons.RetryMultiplierDefault
	}
	if attempt < 1 {
		attempt = 1
	}

	delay := float64(initial) * math.Pow(multiplier, float64(attempt-1))
	if delay > float64(maxDelay) {
		delay = float64(maxDelay)
	}

	if retry.Jitter > 0 && randFloat != nil {
		delay += delay * retry.Jitter * randFloat()
	}
	return time.Duration(delay)
}

func (manager *MountManager) computeRetryDelay(attempt int) time.Duration {
	return computeRetryDelay(manager.config.Retry, attempt, manager.randFloat)
}

// recordMountOperation reports the outcome of a synchronous Mount/Unmount
// API call. It is a no-op unless metrics has been set (see Service).
func (manager *MountManager) recordMountOperation(operation string, err error, start time.Time) {
	if manager.metrics == nil {
		return
	}
	result := "accepted"
	if err != nil {
		result = "rejected"
	}
	manager.metrics.observeMountOperation(operation, result, manager.now().Sub(start))
}

func (manager *MountManager) recordChildCrash() {
	if manager.metrics != nil {
		manager.metrics.recordChildCrash()
	}
}

func (manager *MountManager) recordReconcileError() {
	if manager.metrics != nil {
		manager.metrics.recordReconcileError()
	}
}

// cancelRetryTimer stops any retry timer pending for entry. A new mount or
// unmount request always cancels an older retry timer.
func (entry *managedMount) cancelRetryTimer() {
	entry.mutex.Lock()
	timer := entry.retryTimer
	entry.retryTimer = nil
	entry.mutex.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

// persistRecord writes entry's current state to the repository. The mount
// path never changes after creation, so callers use Update; the initial
// record is created explicitly by Mount before any process work begins.
//
// The clone below takes the exclusive lock, not RLock: two goroutines (for
// example waitForProcess reacting to a crash and waitForMount reacting to a
// near-simultaneous mount detection) can otherwise both enter this function
// concurrently, and a shared RLock lets both run proto.Clone over the same
// entry.info at once, which is not safe.
func (manager *MountManager) persistRecord(ctx context.Context, entry *managedMount) error {
	entry.mutex.Lock()
	record := &store.MountRecord{
		Info:      proto.Clone(entry.info).(*api.MountInfo),
		Tombstone: entry.unmounting,
	}
	entry.mutex.Unlock()
	return manager.repository.Update(ctx, record)
}

// persistBestEffort persists entry's current state and logs, rather than
// fails, on a database error. It is used after process work has already
// happened and cannot be undone by refusing to acknowledge it; only the
// initial Create in Mount and the unmount tombstone Update in Unmount are
// fail-fast.
func (manager *MountManager) persistBestEffort(entry *managedMount) {
	if err := manager.persistRecord(context.Background(), entry); err != nil {
		log.WithError(err).WithField("mount_id", entry.mountID()).Warn("failed to persist mount state")
	}
}

// Mount starts the selected mount client. irodsfs runs in foreground mode and
// receives its JSON configuration through stdin; DAVFS and NFS use one-shot
// system mount commands.
func (manager *MountManager) Mount(ctx context.Context, request *api.MountRequest) (result *api.MountInfo, err error) {
	start := manager.now()
	defer func() { manager.recordMountOperation("mount", err, start) }()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("mount request is required")
	}
	if err := manager.validateMountConfig(request.Config); err != nil {
		return nil, err
	}
	mountTimeout := time.Duration(manager.config.MountTimeout)
	if mountTimeout <= 0 {
		mountTimeout = defaultMountTimeout
	}
	mountDeadline := time.Now().Add(mountTimeout)

	mountID := request.GetMountId()
	if mountID == "" {
		var err error
		mountID, err = newMountID()
		if err != nil {
			return nil, err
		}
	} else if err := validateMountID(mountID); err != nil {
		return nil, err
	}

	storedConfig := proto.Clone(request.Config).(*api.MountConfig)
	mountPath := filepath.Clean(storedConfig.MountPath)
	storedConfig.MountPath = mountPath
	dataRootPath := filepath.Join(manager.config.GetMountRootPath(), mountID)
	now := manager.now()
	entry := &managedMount{
		info: &api.MountInfo{
			MountId:   mountID,
			State:     api.MountState_MOUNT_STATE_PENDING_MOUNT,
			Config:    storedConfig,
			Attempt:   1,
			CreatedAt: timestamppb.New(now),
			UpdatedAt: timestamppb.New(now),
		},
		exitDone:   make(chan struct{}),
		generation: 1,
	}
	entry.operation.Lock()
	defer entry.operation.Unlock()

	manager.mutex.Lock()
	if _, exists := manager.mounts[mountID]; exists {
		manager.mutex.Unlock()
		return nil, errors.Wrapf(ErrMountIDConflict, "mount ID %q", mountID)
	}
	if manager.config.MaxConcurrentMounts > 0 && len(manager.mounts) >= manager.config.MaxConcurrentMounts {
		manager.mutex.Unlock()
		return nil, ErrMountLimitReached
	}
	for existingID, existing := range manager.mounts {
		if filepath.Clean(existing.mountPath()) == mountPath {
			manager.mutex.Unlock()
			return nil, errors.Wrapf(ErrMountPathConflict, "mount path %q is owned by %q", mountPath, existingID)
		}
	}
	manager.mounts[mountID] = entry
	manager.mutex.Unlock()

	// Persist intent before any process work begins so a crash leaves an
	// intent record behind, never an unrecorded child process. The clone is
	// taken under the exclusive lock: entry is already visible through
	// manager.mounts, so a concurrent GetMount/ListMounts could otherwise
	// run its own proto.Clone over the same entry.info at the same time.
	entry.mutex.Lock()
	initialRecord := &store.MountRecord{Info: proto.Clone(entry.info).(*api.MountInfo)}
	entry.mutex.Unlock()
	if err := manager.repository.Create(ctx, initialRecord); err != nil {
		persistErr := errors.Wrap(err, "failed to persist mount record")
		entry.mutex.Lock()
		entry.info.State = api.MountState_MOUNT_STATE_FAILED
		entry.info.UpdatedAt = timestamppb.New(manager.now())
		entry.info.LastError = &api.APIError{Code: "MOUNT_PERSIST_FAILED", Message: persistErr.Error(), Retryable: false}
		close(entry.exitDone)
		result := redactMountInfo(entry.info)
		entry.mutex.Unlock()
		return result, persistErr
	}

	return manager.startMount(entry, entry.generation, dataRootPath, mountDeadline)
}

// startMount runs the directory-creation and process-startup sequence for
// entry, which must already be registered in manager.mounts with a
// persisted record, and launches its supervising goroutines. Callers must
// hold entry.operation locked (Mount holds it for the duration of the call
// via defer; Reconcile's and retryMount's callers do the same). generation
// must be entry's current generation as of the moment this attempt began.
func (manager *MountManager) startMount(entry *managedMount, generation uint64, dataRootPath string, mountDeadline time.Time) (*api.MountInfo, error) {
	if err := os.MkdirAll(dataRootPath, 0o700); err != nil {
		startErr := errors.Wrapf(err, "failed to create mount data directory %q", dataRootPath)
		manager.finishMountStartFailure(entry, generation, "MOUNT_DATA_DIRECTORY_FAILED", startErr.Error())
		return manager.snapshot(entry), startErr
	}
	if !time.Now().Before(mountDeadline) {
		return manager.failMountBeforeStart(entry, generation, dataRootPath)
	}

	entry.mutex.Lock()
	mountLog := entry.mountLog
	entry.mutex.Unlock()
	if mountLog == nil {
		// Opened once per entry, not per attempt: its file path is stable
		// across retries and remounts, so a fresh Open here just resumes
		// the same history rather than starting a new one.
		logPath := manager.config.GetMountLogPath(entry.info.MountId)
		var err error
		mountLog, err = logstore.Open(logPath, collectMountSecrets(entry.info.Config))
		if err != nil {
			manager.finishMountStartFailure(entry, generation, "MOUNT_LOG_OPEN_FAILED", err.Error())
			return manager.snapshot(entry), err
		}
		entry.mutex.Lock()
		entry.mountLog = mountLog
		entry.mutex.Unlock()
	}

	command, client, err := manager.makeMountCommand(entry.info.Config, entry.info.MountId, dataRootPath, mountLog)
	if err != nil {
		manager.finishMountStartFailure(entry, generation, "MOUNT_CONFIGURATION_FAILED", err.Error())
		return manager.snapshot(entry), err
	}
	if !time.Now().Before(mountDeadline) {
		return manager.failMountBeforeStart(entry, generation, dataRootPath)
	}
	if err = command.Start(); err != nil {
		manager.finishMountStartFailure(entry, generation, "MOUNT_COMMAND_START_FAILED", err.Error())
		return manager.snapshot(entry), errors.Wrapf(err, "failed to start %s mount command", client)
	}

	entry.mutex.Lock()
	entry.command = command
	entry.client = client
	entry.persistent = client == mountClientIRODSFS
	if entry.persistent {
		entry.info.Pid = int64(command.Process.Pid)
	}
	entry.info.State = api.MountState_MOUNT_STATE_MOUNTING
	entry.info.UpdatedAt = timestamppb.New(manager.now())
	exitDone := entry.exitDone
	entry.mutex.Unlock()
	manager.persistBestEffort(entry)

	go manager.waitForProcess(entry, generation, command, exitDone)
	go manager.waitForMount(entry, generation, mountDeadline, exitDone)

	return manager.snapshot(entry), nil
}

// Unmount uses the client-appropriate unmount command and removes the mount
// record after any persistent child process has stopped.
func (manager *MountManager) Unmount(ctx context.Context, request *api.UnmountRequest) (result *api.MountInfo, err error) {
	start := manager.now()
	defer func() { manager.recordMountOperation("unmount", err, start) }()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request == nil || request.MountId == "" {
		return nil, errors.New("mount ID is required")
	}

	manager.mutex.RLock()
	entry, exists := manager.mounts[request.MountId]
	manager.mutex.RUnlock()
	if !exists {
		return nil, errors.Wrapf(ErrMountNotFound, "mount ID %q", request.MountId)
	}

	entry.operation.Lock()
	defer entry.operation.Unlock()
	manager.mutex.RLock()
	current, exists := manager.mounts[request.MountId]
	manager.mutex.RUnlock()
	if !exists || current != entry {
		return nil, errors.Wrapf(ErrMountNotFound, "mount ID %q", request.MountId)
	}

	// A new request always cancels an older retry timer, whether it belongs
	// to a pending mount retry (superseded by this unmount) or a pending
	// unmount retry (superseded by this fresh attempt).
	entry.cancelRetryTimer()

	entry.mutex.Lock()
	wasUnmounting := entry.unmounting
	previousState := entry.info.State
	entry.generation++
	generation := entry.generation
	entry.unmounting = true
	entry.info.State = api.MountState_MOUNT_STATE_UNMOUNTING
	if !wasUnmounting {
		// Attempt now counts unmount attempts, not the mount attempts that
		// may have preceded this operation.
		entry.info.Attempt = 1
	}
	entry.info.UpdatedAt = timestamppb.New(manager.now())
	tombstoneRecord := &store.MountRecord{Info: proto.Clone(entry.info).(*api.MountInfo), Tombstone: true}
	entry.mutex.Unlock()

	// Persist the tombstone before any physical unmount work begins: if this
	// fails, a restart must still see the mount as active, not half torn
	// down with no record of the unmount ever being accepted.
	if err := manager.repository.Update(ctx, tombstoneRecord); err != nil {
		entry.mutex.Lock()
		entry.unmounting = wasUnmounting
		entry.info.State = previousState
		entry.info.UpdatedAt = timestamppb.New(manager.now())
		entry.mutex.Unlock()
		return nil, errors.Wrap(err, "failed to persist unmount tombstone")
	}

	return manager.performUnmountAttempt(ctx, entry, generation)
}

// performUnmountAttempt runs the physical unmount work for entry, which
// must already be tombstoned, in UNMOUNTING state, and at generation. It is
// used both by the initial Unmount call and by a scheduled unmount retry.
func (manager *MountManager) performUnmountAttempt(ctx context.Context, entry *managedMount, generation uint64) (*api.MountInfo, error) {
	mountPath := entry.mountPath()

	// A mount stuck in retry_wait, or one that crashed and was already
	// cleaned up, may have nothing left to physically unmount; probing
	// first avoids failing an otherwise-successful unmount over a helper
	// command that has nothing to act on.
	mounted, probeErr := manager.probe(mountPath)
	if probeErr != nil {
		unmountErr := errors.Wrap(probeErr, "failed to check the mount table before unmount")
		manager.recordUnmountFailure(entry, generation, "UNMOUNT_FAILED", unmountErr.Error())
		return manager.snapshot(entry), unmountErr
	}
	if mounted {
		if err := manager.unmountClient(ctx, entry); err != nil {
			unmountErr := err
			code := "UNMOUNT_FAILED"
			if errors.Is(err, ErrDAVFSLazyUnmount) {
				code = "DAVFS_LAZY_UNMOUNT"
			} else {
				unmountErr = errors.Wrap(err, "failed to unmount mount")
			}
			manager.recordUnmountFailure(entry, generation, code, unmountErr.Error())
			return manager.snapshot(entry), unmountErr
		}
	}

	entry.mutex.RLock()
	persistent := entry.persistent
	command := entry.command
	exitDone := entry.exitDone
	entry.mutex.RUnlock()

	if persistent && command != nil && command.Process != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
	}

	timeout := time.Duration(manager.config.UnmountTimeout)
	if timeout <= 0 {
		timeout = defaultUnmountTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-exitDone:
	case <-timer.C:
		if command != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-exitDone:
		case <-ctx.Done():
			manager.recordUnmountFailure(entry, generation, "UNMOUNT_FAILED", ctx.Err().Error())
			return manager.snapshot(entry), ctx.Err()
		}
	case <-ctx.Done():
		manager.recordUnmountFailure(entry, generation, "UNMOUNT_FAILED", ctx.Err().Error())
		return manager.snapshot(entry), ctx.Err()
	}

	mountID := entry.mountID()
	dataRootPath := filepath.Join(manager.config.GetMountRootPath(), mountID)
	if err := os.RemoveAll(dataRootPath); err != nil {
		cleanupErr := errors.Wrapf(err, "failed to delete mount data directory %q", dataRootPath)
		manager.recordUnmountFailure(entry, generation, "UNMOUNT_FAILED", cleanupErr.Error())
		return manager.snapshot(entry), cleanupErr
	}

	entry.mutex.Lock()
	now := manager.now()
	entry.info.UnmountedAt = timestamppb.New(now)
	entry.info.UpdatedAt = timestamppb.New(now)
	result := redactMountInfo(entry.info)
	mountLog := entry.mountLog
	entry.mutex.Unlock()

	// The log file itself is left in place (subject to its own retention),
	// only the handle is released: it may still be useful for diagnosing
	// why the mount was removed.
	if mountLog != nil {
		if err := mountLog.Close(); err != nil {
			log.WithError(err).WithField("mount_id", mountID).Warn("failed to close mount log cleanly")
		}
	}

	// The mount and its data are already gone; a record-deletion failure is
	// logged, not returned, since a future startup reconciliation pass can
	// still notice the orphaned, tombstoned record and delete it.
	if err := manager.repository.Delete(ctx, mountID); err != nil {
		log.WithError(err).WithField("mount_id", mountID).Warn("failed to delete mount record after unmount")
	}

	manager.mutex.Lock()
	delete(manager.mounts, mountID)
	manager.mutex.Unlock()

	return result, nil
}

// Ready reports whether the manager's dependencies are currently usable:
// the mount repository, FUSE, and the configured irodsfs/mount/unmount
// helper binaries. It is meant for /readyz, which must reflect the
// daemon's actual current ability to do mount work, not merely that a
// MountManager object exists.
func (manager *MountManager) Ready(ctx context.Context) error {
	if _, err := manager.repository.List(ctx); err != nil {
		return errors.Wrap(err, "mount repository is not ready")
	}
	if err := manager.fuse.Check(true); err != nil {
		return errors.Wrap(err, "FUSE is not ready")
	}
	if err := validateExecutable(manager.config.IRODSFSExecutablePath); err != nil {
		return err
	}
	if err := validateExecutable(manager.config.MountExecutablePath); err != nil {
		return err
	}
	if err := validateExecutable(manager.config.UnmountExecutablePath); err != nil {
		return err
	}
	return nil
}

// GetMount returns one redacted mount snapshot.
func (manager *MountManager) GetMount(ctx context.Context, mountID string) (*api.MountInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manager.mutex.RLock()
	entry, exists := manager.mounts[mountID]
	manager.mutex.RUnlock()
	if !exists {
		return nil, errors.Wrapf(ErrMountNotFound, "mount ID %q", mountID)
	}
	return manager.snapshot(entry), nil
}

// ListMounts returns redacted snapshots matching all supplied filters.
func (manager *MountManager) ListMounts(ctx context.Context, request *api.ListMountsRequest) ([]*api.MountInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request == nil {
		request = &api.ListMountsRequest{}
	}

	states := make(map[api.MountState]struct{}, len(request.States))
	for _, state := range request.States {
		states[state] = struct{}{}
	}

	manager.mutex.RLock()
	entries := make([]*managedMount, 0, len(manager.mounts))
	for _, entry := range manager.mounts {
		entries = append(entries, entry)
	}
	manager.mutex.RUnlock()

	result := make([]*api.MountInfo, 0, len(entries))
	for _, entry := range entries {
		info := manager.snapshot(entry)
		if len(states) > 0 {
			if _, include := states[info.State]; !include {
				continue
			}
		}
		if request.MountPathPrefix != nil && !strings.HasPrefix(info.Config.MountPath, request.GetMountPathPrefix()) {
			continue
		}
		if request.ClientUser != nil && accountClientUser(info.Config.GetIrodsfs().GetAccount()) != request.GetClientUser() {
			continue
		}
		result = append(result, info)
	}

	sort.Slice(result, func(left int, right int) bool {
		return result[left].MountId < result[right].MountId
	})
	return result, nil
}

// Reconcile loads every persisted mount record and reconciles it against
// the real system state observed through probe. It must run once,
// synchronously, before the API begins accepting requests: a saved PID is
// never trusted as proof that this process still owns a mount, so identity
// is established solely through the mount table, never by signaling a
// recovered PID.
func (manager *MountManager) Reconcile(ctx context.Context) error {
	records, err := manager.repository.List(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to list mount records for reconciliation")
	}
	for _, record := range records {
		manager.reconcileRecord(ctx, record)
	}
	return nil
}

func (manager *MountManager) reconcileRecord(ctx context.Context, record *store.MountRecord) {
	manager.mutex.RLock()
	_, alreadyManaged := manager.mounts[record.Info.MountId]
	manager.mutex.RUnlock()
	if alreadyManaged {
		// Reconcile is safe to call periodically, not just at startup, but a
		// record already supervised in this process (an active mount, or an
		// in-flight/retrying unmount) must be left to its own controller
		// rather than second-guessed here.
		return
	}

	logger := log.WithField("mount_id", record.Info.MountId)
	mounted, err := manager.probe(record.Info.GetConfig().GetMountPath())
	if err != nil {
		manager.recordReconcileError()
		logger.WithError(err).Warn("failed to probe the mount table during reconciliation")
		return
	}
	if record.Tombstone {
		manager.reconcileTombstone(ctx, record, mounted)
		return
	}
	manager.reconcileOrdinaryRecord(ctx, record, mounted)
}

// reconcileTombstone resumes an unmount that was accepted before a crash or
// restart. A DAVFS cache that is still preserved from a previous lazy
// unmount is never discarded: the record and its data directory are left
// alone whenever cleanup does not finish this time either.
func (manager *MountManager) reconcileTombstone(ctx context.Context, record *store.MountRecord, mounted bool) {
	mountID := record.Info.MountId
	logger := log.WithField("mount_id", mountID)

	if mounted {
		client, err := clientType(record.Info.Config)
		if err != nil {
			manager.recordReconcileError()
			logger.WithError(err).Warn("failed to resume unmount during reconciliation: unrecognized client config")
			return
		}
		if err := manager.unmountByClientType(ctx, client, record.Info.GetConfig().GetMountPath()); err != nil {
			if errors.Is(err, ErrDAVFSLazyUnmount) {
				logger.Warn("DAVFS cache synchronization has still not finished; preserving cache and record for a future retry")
			} else {
				manager.recordReconcileError()
				logger.WithError(err).Warn("failed to resume unmount during reconciliation; will retry on next restart")
			}
			return
		}
	}

	dataRootPath := filepath.Join(manager.config.GetMountRootPath(), mountID)
	if err := os.RemoveAll(dataRootPath); err != nil {
		manager.recordReconcileError()
		logger.WithError(err).Warn("failed to delete mount data directory during reconciliation")
		return
	}
	if err := manager.repository.Delete(ctx, mountID); err != nil {
		manager.recordReconcileError()
		logger.WithError(err).Warn("failed to delete mount record during reconciliation")
	}
}

// reconcileOrdinaryRecord restores a mount that was never explicitly
// unmounted. A mount that is still physically present cannot be supervised
// reliably across a restart (this process has no exec.Cmd handle for its
// child), so it is detached and remounted fresh to regain log and crash
// ownership, exactly as design.md requires.
func (manager *MountManager) reconcileOrdinaryRecord(ctx context.Context, record *store.MountRecord, mounted bool) {
	mountID := record.Info.MountId
	logger := log.WithField("mount_id", mountID)

	if mounted {
		client, err := clientType(record.Info.Config)
		if err != nil {
			manager.recordReconcileError()
			logger.WithError(err).Warn("failed to reconcile mount: unrecognized client config")
			return
		}
		if err := manager.unmountByClientType(ctx, client, record.Info.GetConfig().GetMountPath()); err != nil {
			manager.recordReconcileError()
			logger.WithError(err).Warn("failed to detach a surviving mount during reconciliation; will retry on next restart")
			return
		}
	}

	manager.remount(ctx, record)
}

// remount registers record as a managed mount and starts a fresh mount
// process for it, reusing its existing mount ID, mount path, and stored
// configuration. Unlike Mount, it never calls repository.Create: the record
// already exists.
func (manager *MountManager) remount(ctx context.Context, record *store.MountRecord) {
	mountID := record.Info.MountId
	logger := log.WithField("mount_id", mountID)

	manager.mutex.Lock()
	if _, exists := manager.mounts[mountID]; exists {
		manager.mutex.Unlock()
		logger.Warn("mount ID is already managed; skipping reconciliation")
		return
	}
	info := proto.Clone(record.Info).(*api.MountInfo)
	info.State = api.MountState_MOUNT_STATE_PENDING_MOUNT
	info.Pid = 0
	info.LastError = nil
	info.NextRetryAt = nil
	info.UpdatedAt = timestamppb.New(manager.now())
	entry := &managedMount{info: info, exitDone: make(chan struct{}), generation: 1}
	manager.mounts[mountID] = entry
	manager.mutex.Unlock()

	entry.operation.Lock()
	defer entry.operation.Unlock()

	if err := manager.persistRecord(ctx, entry); err != nil {
		manager.recordReconcileError()
		logger.WithError(err).Warn("failed to persist mount state before remount")
	}

	mountTimeout := time.Duration(manager.config.MountTimeout)
	if mountTimeout <= 0 {
		mountTimeout = defaultMountTimeout
	}
	mountDeadline := time.Now().Add(mountTimeout)
	dataRootPath := filepath.Join(manager.config.GetMountRootPath(), mountID)

	if _, err := manager.startMount(entry, entry.generation, dataRootPath, mountDeadline); err != nil {
		manager.recordReconcileError()
		logger.WithError(err).Warn("failed to remount during reconciliation; will retry on next restart")
	}
}

// Shutdown safely stops every managed mount without deleting its record or
// marking it as a tombstone: an ordinary record means the mount must be
// running, so the next daemon start's reconciliation remounts it. Only an
// explicit Unmount ever tombstones a record. Mounts are stopped in
// parallel so one slow mount cannot block the others; each individual stop
// remains bounded by unmount_timeout or davfs_unmount_timeout. Shutdown
// returns once every mount has been stopped (or its own bounded stop
// attempt has given up), so the caller can close the repository and exit
// immediately afterward with no separate grace period.
func (manager *MountManager) Shutdown(ctx context.Context) {
	manager.mutex.RLock()
	entries := make([]*managedMount, 0, len(manager.mounts))
	for _, entry := range manager.mounts {
		entries = append(entries, entry)
	}
	manager.mutex.RUnlock()

	var wait sync.WaitGroup
	wait.Add(len(entries))
	for _, entry := range entries {
		go func(entry *managedMount) {
			defer wait.Done()
			manager.shutdownEntry(ctx, entry)
		}(entry)
	}
	wait.Wait()
}

// shutdownEntry stops one managed mount for shutdown. A mount already being
// unmounted through the ordinary Unmount path (explicit request or its own
// retry) is left alone: entry.operation blocks until that attempt's own
// bounded timeout finishes on its own, and its outcome (deleted record, or
// a tombstoned RETRY_WAIT/FAILED record) is correct either way.
func (manager *MountManager) shutdownEntry(ctx context.Context, entry *managedMount) {
	entry.cancelRetryTimer()

	entry.operation.Lock()
	defer entry.operation.Unlock()

	entry.mutex.Lock()
	if entry.unmounting {
		entry.mutex.Unlock()
		return
	}
	// Set before any action that could cause the child to exit (the FUSE
	// detach below, or the SIGTERM further down): waitForProcess is still
	// running for this attempt and must recognize the exit that follows as
	// expected, not as a crash to retry.
	entry.stopping = true
	entry.mutex.Unlock()

	logger := log.WithField("mount_id", entry.mountID())
	mountPath := entry.mountPath()
	if mounted, err := manager.probe(mountPath); err != nil {
		logger.WithError(err).Warn("failed to check the mount table during shutdown")
	} else if mounted {
		if err := manager.unmountClient(ctx, entry); err != nil && !errors.Is(err, ErrDAVFSLazyUnmount) {
			logger.WithError(err).Warn("failed to unmount cleanly during shutdown")
		}
	}

	entry.mutex.RLock()
	persistent := entry.persistent
	command := entry.command
	exitDone := entry.exitDone
	entry.mutex.RUnlock()

	if persistent && command != nil && command.Process != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
	}

	timeout := time.Duration(manager.config.UnmountTimeout)
	if timeout <= 0 {
		timeout = defaultUnmountTimeout
	}
	select {
	case <-exitDone:
	case <-time.After(timeout):
		if command != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		<-exitDone
	}

	// The record and data directory are deliberately left in place: an
	// ordinary record means the mount must be running, so a restart's
	// reconciliation will see no actual mount at this path and remount it.
	entry.mutex.Lock()
	entry.info.State = api.MountState_MOUNT_STATE_PENDING_MOUNT
	entry.info.Pid = 0
	entry.info.LastError = nil
	entry.info.NextRetryAt = nil
	entry.info.UpdatedAt = timestamppb.New(manager.now())
	entry.mutex.Unlock()
	manager.persistBestEffort(entry)

	entry.mutex.Lock()
	mountLog := entry.mountLog
	entry.mountLog = nil
	entry.mutex.Unlock()
	if mountLog != nil {
		if err := mountLog.Close(); err != nil {
			logger.WithError(err).Warn("failed to close mount log cleanly during shutdown")
		}
	}
}

// waitForProcess supervises one attempt's child process. command and
// exitDone are the specific objects created for this attempt (not read
// live from entry), so a goroutine left over from a superseded attempt
// never touches a newer attempt's process or channel.
func (manager *MountManager) waitForProcess(entry *managedMount, generation uint64, command *exec.Cmd, exitDone chan struct{}) {
	err := command.Wait()

	entry.mutex.Lock()
	stale := entry.generation != generation
	close(exitDone)
	if !stale && !entry.persistent {
		entry.info.Pid = 0
	}
	crashed := !stale && !entry.unmounting && !entry.timedOut && !entry.stopping && (entry.persistent || err != nil)
	client := entry.client
	mountPath := entry.info.GetConfig().GetMountPath()
	entry.mutex.Unlock()
	if !crashed {
		return
	}
	manager.recordChildCrash()

	message := processExitMessage(err)

	// Clear a broken FUSE endpoint left behind by the crash before deciding
	// whether to retry: a fresh attempt must never be stacked on top of a
	// mount point that is still claimed by the dead process.
	if mounted, probeErr := manager.probe(mountPath); probeErr == nil && mounted {
		if unmountErr := manager.unmountByClientType(context.Background(), client, mountPath); unmountErr != nil {
			manager.recordTerminalMountFailure(entry, generation, "MOUNT_COMMAND_EXITED",
				fmt.Sprintf("%s; failed to clean up the broken mount: %v", message, unmountErr))
			return
		}
	}
	manager.recordMountFailure(entry, generation, "MOUNT_COMMAND_EXITED", message, nil)
}

func (entry *managedMount) mountPath() string {
	entry.mutex.RLock()
	defer entry.mutex.RUnlock()
	return entry.info.Config.MountPath
}

func (entry *managedMount) mountID() string {
	entry.mutex.RLock()
	defer entry.mutex.RUnlock()
	return entry.info.MountId
}

// finishMountStartFailure closes exitDone for an attempt that never
// actually started a process (so nothing else will), then defers to
// recordMountFailure to decide whether to retry or fail terminally.
func (manager *MountManager) finishMountStartFailure(entry *managedMount, generation uint64, code string, message string) {
	entry.mutex.Lock()
	close(entry.exitDone)
	entry.mutex.Unlock()
	manager.recordMountFailure(entry, generation, code, message, nil)
}

func (manager *MountManager) failMountBeforeStart(entry *managedMount, generation uint64, dataRootPath string) (*api.MountInfo, error) {
	timeoutErr := errors.New("timed out while preparing filesystem mount")
	entry.mutex.Lock()
	entry.timedOut = true
	entry.mutex.Unlock()

	message := timeoutErr.Error()
	if err := os.RemoveAll(dataRootPath); err != nil {
		message = fmt.Sprintf("%s; cleanup failed: %v", timeoutErr, err)
	}
	manager.finishMountStartFailure(entry, generation, "MOUNT_TIMEOUT", message)
	return manager.snapshot(entry), timeoutErr
}

// recordMountFailure decides whether a failed mount attempt should be
// retried or left terminally FAILED, using the configured retry policy, and
// persists the result. It is a no-op if generation no longer matches
// entry's current generation (a newer attempt already superseded this one)
// or if an Unmount request is now in control of entry.
func (manager *MountManager) recordMountFailure(entry *managedMount, generation uint64, code string, message string, details map[string]string) {
	entry.mutex.Lock()
	if entry.generation != generation || entry.unmounting {
		entry.mutex.Unlock()
		return
	}
	attempt := int(entry.info.Attempt)
	retrying := attempt < manager.maxAttempts()

	entry.info.UpdatedAt = timestamppb.New(manager.now())
	entry.info.LastError = &api.APIError{Code: code, Message: message, Retryable: retrying, Details: details}
	var delay time.Duration
	if retrying {
		delay = manager.computeRetryDelay(attempt)
		entry.info.State = api.MountState_MOUNT_STATE_RETRY_WAIT
		entry.info.NextRetryAt = timestamppb.New(manager.now().Add(delay))
	} else {
		entry.info.State = api.MountState_MOUNT_STATE_FAILED
		entry.info.NextRetryAt = nil
	}
	entry.mutex.Unlock()
	manager.persistBestEffort(entry)

	if !retrying {
		return
	}
	timer := time.AfterFunc(delay, func() { manager.retryMount(entry, generation) })
	entry.mutex.Lock()
	entry.retryTimer = timer
	entry.mutex.Unlock()
}

// recordTerminalMountFailure leaves entry FAILED with no further retry,
// regardless of remaining attempts: it is used when a broken mount could
// not be confirmed clean, since a retry must never stack a new attempt on
// top of a mount point still claimed by a previous one.
func (manager *MountManager) recordTerminalMountFailure(entry *managedMount, generation uint64, code string, message string) {
	entry.mutex.Lock()
	if entry.generation != generation || entry.unmounting {
		entry.mutex.Unlock()
		return
	}
	entry.info.State = api.MountState_MOUNT_STATE_FAILED
	entry.info.UpdatedAt = timestamppb.New(manager.now())
	entry.info.LastError = &api.APIError{Code: code, Message: message, Retryable: false}
	entry.info.NextRetryAt = nil
	entry.mutex.Unlock()
	manager.persistBestEffort(entry)
}

// retryMount fires after a scheduled mount-retry delay and starts a fresh
// attempt, unless entry has moved on (a newer attempt or an unmount already
// superseded this timer).
func (manager *MountManager) retryMount(entry *managedMount, generation uint64) {
	entry.operation.Lock()
	defer entry.operation.Unlock()

	entry.mutex.Lock()
	if entry.generation != generation || entry.unmounting {
		entry.mutex.Unlock()
		return
	}
	entry.retryTimer = nil
	entry.generation++
	newGeneration := entry.generation
	entry.info.Attempt++
	entry.info.State = api.MountState_MOUNT_STATE_PENDING_MOUNT
	entry.info.UpdatedAt = timestamppb.New(manager.now())
	entry.exitDone = make(chan struct{})
	entry.timedOut = false
	entry.command = nil
	entry.client = ""
	entry.persistent = false
	entry.mutex.Unlock()
	manager.persistBestEffort(entry)

	mountTimeout := time.Duration(manager.config.MountTimeout)
	if mountTimeout <= 0 {
		mountTimeout = defaultMountTimeout
	}
	mountDeadline := time.Now().Add(mountTimeout)
	dataRootPath := filepath.Join(manager.config.GetMountRootPath(), entry.mountID())

	if _, err := manager.startMount(entry, newGeneration, dataRootPath, mountDeadline); err != nil {
		log.WithError(err).WithField("mount_id", entry.mountID()).Warn("scheduled mount retry failed")
	}
}

func (manager *MountManager) waitForMount(entry *managedMount, generation uint64, deadline time.Time, exitDone chan struct{}) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		manager.handleMountTimeout(entry, generation)
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	ticker := time.NewTicker(mountProbeInterval)
	defer ticker.Stop()

	check := func() bool {
		mounted, err := manager.probe(entry.mountPath())
		if err != nil || !mounted {
			return false
		}
		entry.mutex.Lock()
		transitioned := false
		if entry.generation == generation && entry.info.State == api.MountState_MOUNT_STATE_MOUNTING && !entry.unmounting {
			now := manager.now()
			entry.info.State = api.MountState_MOUNT_STATE_MOUNTED
			entry.info.MountedAt = timestamppb.New(now)
			entry.info.UpdatedAt = timestamppb.New(now)
			transitioned = true
		}
		entry.mutex.Unlock()
		if transitioned {
			manager.persistBestEffort(entry)
		}
		return true
	}

	if check() {
		return
	}
	for {
		select {
		case <-exitDone:
			entry.mutex.RLock()
			stopped := entry.persistent ||
				entry.generation != generation ||
				entry.info.State == api.MountState_MOUNT_STATE_FAILED ||
				entry.info.State == api.MountState_MOUNT_STATE_RETRY_WAIT
			entry.mutex.RUnlock()
			if stopped {
				return
			}
			exitDone = nil
		case <-ticker.C:
			if check() {
				return
			}
		case <-timer.C:
			manager.handleMountTimeout(entry, generation)
			return
		}
	}
}

func (manager *MountManager) handleMountTimeout(entry *managedMount, generation uint64) {
	entry.operation.Lock()
	defer entry.operation.Unlock()

	entry.mutex.Lock()
	if entry.generation != generation || entry.unmounting || entry.info.State == api.MountState_MOUNT_STATE_MOUNTED {
		entry.mutex.Unlock()
		return
	}
	entry.timedOut = true
	command := entry.command
	client := entry.client
	mountPath := entry.info.Config.MountPath
	mountID := entry.info.MountId
	entry.mutex.Unlock()

	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}

	preserved, cleanupErr := manager.cleanupTimedOutMount(client, mountPath, mountID)
	if cleanupErr != nil {
		manager.recordTerminalMountFailure(entry, generation, "MOUNT_TIMEOUT",
			fmt.Sprintf("timed out waiting for filesystem mount; cleanup failed: %v", cleanupErr))
		return
	}
	var details map[string]string
	if preserved {
		details = map[string]string{"davfs_cache": "preserved for recovery after lazy unmount"}
	}
	manager.recordMountFailure(entry, generation, "MOUNT_TIMEOUT", "timed out waiting for filesystem mount", details)
}

func (manager *MountManager) cleanupTimedOutMount(client mountClientType, mountPath string, mountID string) (bool, error) {
	mounted, err := manager.probe(mountPath)
	if err != nil {
		return false, errors.Wrap(err, "failed to check timed-out mount")
	}
	preserveData := false
	if mounted {
		switch client {
		case mountClientIRODSFS:
			if err := manager.fuse.Unmount(mountPath); err != nil {
				return false, errors.Wrap(err, "failed to lazily unmount timed-out FUSE mount")
			}
		case mountClientDAVFS:
			if err := manager.fuse.Unmount(mountPath); err != nil {
				return true, errors.Wrap(err, "failed to lazily unmount timed-out DAVFS mount")
			}
			preserveData = true
		case mountClientNFS:
			if err := manager.runSystemUnmount(context.Background(), mountPath, true, time.Duration(manager.config.UnmountTimeout)); err != nil {
				return false, errors.Wrap(err, "failed to lazily unmount timed-out NFS mount")
			}
		}
	}
	if preserveData {
		return true, nil
	}

	dataRootPath := filepath.Join(manager.config.GetMountRootPath(), mountID)
	if err := os.RemoveAll(dataRootPath); err != nil {
		return false, errors.Wrapf(err, "failed to delete mount data directory %q", dataRootPath)
	}
	return false, nil
}

// recordUnmountFailure decides whether a failed unmount attempt should be
// retried or left terminally FAILED, mirroring recordMountFailure.
// entry.unmounting is never reset to false here: the tombstone was already
// persisted, so a restart or a retried Unmount must keep trying to unmount
// rather than treat the mount as active again.
func (manager *MountManager) recordUnmountFailure(entry *managedMount, generation uint64, code string, message string) {
	entry.mutex.Lock()
	if entry.generation != generation {
		entry.mutex.Unlock()
		return
	}
	attempt := int(entry.info.Attempt)
	retrying := attempt < manager.maxAttempts()

	entry.info.UpdatedAt = timestamppb.New(manager.now())
	entry.info.LastError = &api.APIError{Code: code, Message: message, Retryable: retrying}
	var delay time.Duration
	if retrying {
		delay = manager.computeRetryDelay(attempt)
		entry.info.State = api.MountState_MOUNT_STATE_RETRY_WAIT
		entry.info.NextRetryAt = timestamppb.New(manager.now().Add(delay))
	} else {
		entry.info.State = api.MountState_MOUNT_STATE_FAILED
		entry.info.NextRetryAt = nil
	}
	entry.mutex.Unlock()
	manager.persistBestEffort(entry)

	if !retrying {
		return
	}
	timer := time.AfterFunc(delay, func() { manager.retryUnmount(entry, generation) })
	entry.mutex.Lock()
	entry.retryTimer = timer
	entry.mutex.Unlock()
}

// retryUnmount fires after a scheduled unmount-retry delay and resumes the
// unmount, unless entry has moved on (a newer request already superseded
// this timer).
func (manager *MountManager) retryUnmount(entry *managedMount, generation uint64) {
	entry.operation.Lock()
	defer entry.operation.Unlock()

	entry.mutex.Lock()
	if entry.generation != generation || !entry.unmounting {
		entry.mutex.Unlock()
		return
	}
	entry.retryTimer = nil
	entry.generation++
	newGeneration := entry.generation
	entry.info.Attempt++
	entry.info.State = api.MountState_MOUNT_STATE_UNMOUNTING
	entry.info.UpdatedAt = timestamppb.New(manager.now())
	entry.mutex.Unlock()
	manager.persistBestEffort(entry)

	if _, err := manager.performUnmountAttempt(context.Background(), entry, newGeneration); err != nil {
		log.WithError(err).WithField("mount_id", entry.mountID()).Warn("scheduled unmount retry failed")
	}
}

// snapshot clones and redacts entry's current info. It takes the exclusive
// lock, not RLock, for the same reason as persistRecord: proto.Clone is not
// safe to run concurrently on the same message from two goroutines that
// each believe they merely hold a shared read lock (e.g. two concurrent
// GetMount/ListMounts calls for the same mount).
func (manager *MountManager) snapshot(entry *managedMount) *api.MountInfo {
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	return redactMountInfo(entry.info)
}

func redactMountInfo(info *api.MountInfo) *api.MountInfo {
	copy := proto.Clone(info).(*api.MountInfo)
	if account := copy.GetConfig().GetIrodsfs().GetAccount(); account != nil {
		account.IrodsUserPassword = redactOptional(account.IrodsUserPassword)
		account.IrodsTicket = redactOptional(account.IrodsTicket)
		account.IrodsPamToken = redactOptional(account.IrodsPamToken)
	}
	if redis := copy.GetConfig().GetIrodsfs().GetCache().GetBackend().GetRedis(); redis != nil {
		redis.Password = redactOptional(redis.Password)
	}
	if davfs := copy.GetConfig().GetDavfs(); davfs != nil {
		davfs.Password = redactOptional(davfs.Password)
	}
	return copy
}

func redactOptional(value *string) *string {
	if value == nil {
		return nil
	}
	redacted := ""
	if *value != "" {
		redacted = redactedValue
	}
	return &redacted
}

func accountClientUser(account *api.Account) string {
	if account == nil {
		return ""
	}
	if account.IrodsClientUserName != nil {
		return account.GetIrodsClientUserName()
	}
	return account.IrodsUserName
}

func (manager *MountManager) validateMountConfig(config *api.MountConfig) error {
	if config == nil {
		return errors.New("mount config is required")
	}
	client, err := clientType(config)
	if err != nil {
		return err
	}
	switch client {
	case mountClientIRODSFS:
		irodsfsConfig := config.GetIrodsfs()
		if irodsfsConfig.Account == nil {
			return errors.New("iRODS account is required")
		}
		if irodsfsConfig.Account.IrodsHost == "" {
			return errors.New("iRODS host is required")
		}
		if irodsfsConfig.Account.IrodsUserName == "" || irodsfsConfig.Account.IrodsZoneName == "" {
			return errors.New("iRODS user and zone are required")
		}
	case mountClientDAVFS:
		if err := validateDAVFSConfig(config.GetDavfs()); err != nil {
			return err
		}
	case mountClientNFS:
		if err := validateNFSConfig(config.GetNfs()); err != nil {
			return err
		}
	}
	if config.MountPath == "" || !filepath.IsAbs(config.MountPath) {
		return errors.New("mount path must be absolute")
	}
	info, err := os.Stat(config.MountPath)
	if err != nil {
		return errors.Wrapf(err, "invalid mount path %q", config.MountPath)
	}
	if !info.IsDir() {
		return errors.Errorf("mount path %q is not a directory", config.MountPath)
	}
	if len(manager.config.AllowedMountRootPaths) > 0 && !pathWithinAnyRoot(config.MountPath, manager.config.AllowedMountRootPaths) {
		return errors.Errorf("mount path %q is outside allowed mount roots", config.MountPath)
	}
	if conflict := manager.reservedPathConflict(config.MountPath); conflict != "" {
		return errors.Errorf("mount path %q conflicts with the daemon's own reserved path %q", config.MountPath, conflict)
	}
	return nil
}

func pathWithinAnyRoot(target string, roots []string) bool {
	target = filepath.Clean(target)
	for _, root := range roots {
		root = filepath.Clean(root)
		relative, err := filepath.Rel(root, target)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// reservedPathConflict returns the first reserved daemon path that target
// conflicts with (target equals it, is its ancestor, or is its
// descendant), or "" if there is no conflict. This is checked independent
// of AllowedMountRootPaths: an operator could otherwise configure an
// allowed root that happens to contain the daemon's own data, log, or
// runtime directories.
func (manager *MountManager) reservedPathConflict(target string) string {
	// The filesystem root is a special case, checked for exact equality
	// only: every absolute path is trivially "within" root, so treating it
	// like the other reserved paths below (ancestor-or-descendant) would
	// reject every mount path outright.
	if filepath.Clean(target) == string(filepath.Separator) {
		return string(filepath.Separator)
	}

	reserved := []string{manager.config.GetDataRootPath(), manager.config.GetLogRootPath(), manager.config.GetMountRootPath(), manager.config.GetMountDatabasePath()}
	if pidDir := filepath.Dir(manager.config.PIDFile); pidDir != "" && pidDir != "." {
		reserved = append(reserved, pidDir)
	}
	if scheme, endpoint, err := irodsfsd_commons.ParseServiceEndpoint(manager.config.GetServiceEndpoint()); err == nil && scheme == "unix" {
		reserved = append(reserved, filepath.Dir(endpoint))
	}
	for _, path := range reserved {
		if path == "" {
			continue
		}
		if pathsConflict(target, path) {
			return filepath.Clean(path)
		}
	}
	return ""
}

// pathsConflict reports whether a and b are the same path once cleaned, or
// one is an ancestor or descendant of the other.
func pathsConflict(a string, b string) bool {
	return pathWithinAnyRoot(a, []string{b}) || pathWithinAnyRoot(b, []string{a})
}

func validateMountID(mountID string) error {
	if len(mountID) > 128 {
		return errors.New("mount ID must not exceed 128 characters")
	}
	for _, character := range mountID {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return errors.Errorf("mount ID %q contains an invalid character", mountID)
	}
	return nil
}

func newMountID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.Wrap(err, "failed to generate mount ID")
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}

func validateExecutable(path string) error {
	if path == "" {
		return errors.New("irodsfs executable path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrapf(err, "invalid irodsfs executable %q", path)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return errors.Errorf("irodsfs executable %q is not executable", path)
	}
	return nil
}

func processExitMessage(err error) string {
	if err == nil {
		return "irodsfs exited unexpectedly"
	}
	return fmt.Sprintf("irodsfs exited unexpectedly: %v", err)
}

func isMountPoint(mountPath string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, errors.Wrap(err, "failed to read mountinfo")
	}
	want := filepath.Clean(mountPath)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if filepath.Clean(unescapeMountInfoPath(fields[4])) == want {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountInfoPath(value string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(value)
}
