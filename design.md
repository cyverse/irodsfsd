# irodsfsd Design

## 1. Purpose

`irodsfsd` is a long-running Linux daemon written in Go. It manages irodsfs,
DAVFS, and NFS mounts through gRPC, REST, and an embedded web interface,
including supervised processes, state, and logs. Desired mount definitions are
persisted in BadgerDB. After a daemon restart, they are reconciled with the
actual system state and restored automatically.

The initial scope is a single `irodsfsd` instance managing multiple mounts on one Linux host. High availability, cross-host mount coordination, and reimplementation of iRODS are out of scope.

## 2. Core Design Principles

1. **Persist intent before process work.** A normal record means the mount must be running; an unmount tombstone means it must be removed. A reconciler drives child processes and Linux mounts toward that intent.
2. **Run `irodsfs` as a foreground child.** Force `foreground: true` or the equivalent CLI option so that `exec.Cmd` owns the real FUSE process. Allowing `irodsfs` to daemonize again would make PID tracking, exit detection, and stdout/stderr capture unreliable.
3. **Do not use PID existence as proof of a successful mount.** A mount becomes `mounted` only after the target and expected FUSE filesystem appear in `/proc/self/mountinfo`.
4. **Clean up a crash before retrying.** If a child exits unexpectedly while its mount remains, lazy-unmount it immediately to prevent `Transport endpoint is not connected`, then retry.
5. **Serialize operations per mount path.** Mount, unmount, and reconciliation for the same canonical path must never run concurrently.
6. **Never return secrets.** Passwords and other sensitive values are always redacted from API responses, the UI, events, and application logs.

## 3. Architecture

```text
CLI parent
  `-- go-daemonizer --> irodsfsd daemon
                           |-- gRPC API
                           |-- REST API + Web UI
                           |-- Mount Service
                           |     |-- per-mount controller/state machine
                           |     |-- retry scheduler
                           |     `-- mount table inspector
                           |-- Process Supervisor
                           |     `-- irodsfs --foreground children
                           |-- Log Manager
                           `-- BadgerDB
```

### Packages

```text
cmd/                 main entry point, CLI commands, and daemon lifecycle
cmd/commons/         shared CLI helpers such as PID-file handling
commons/             daemon configuration, endpoints, and shared helpers
client/              reusable gRPC client for mount operations
client_examples/     mount, unmount, and mount-list executables
service/             manager, supervisor, gRPC/REST servers, metrics
service/api/         protobuf contract and generated gRPC code
service/store/       Badger repository and schema migrations
service/logstore/    per-mount child log storage and queries
service/web/         embedded management UI
packaging/systemd/   service unit, documentation, and example config
```

gRPC and REST handlers do not launch processes directly. They invoke the same
`MountServer`, which delegates to `MountManager`. Mount records in BadgerDB are
the source of truth. The in-memory registry contains ephemeral process handles,
retry timers, logs, and per-mount locks.

## 4. Daemon and CLI

### CLI

Following the command and flag style of `irodsfs`, one binary provides:

```text
irodsfsd start   --config /etc/irodsfsd/config.yaml
irodsfsd run     --config /etc/irodsfsd/config.yaml   # foreground/systemd/development
irodsfsd stop    --config /etc/irodsfsd/config.yaml
irodsfsd status  --config /etc/irodsfsd/config.yaml
irodsfsd version
```

Common options are `--config/-c` and `--debug/-d`. Command-line values override
configuration-file values. The implementation uses Cobra and keeps command
declaration separate from validation and application startup.

### go-daemonizer

The `start` parent creates a daemonizer with `go-daemonizer.New()`, uses `IsDaemon()` to distinguish the parent from the detached execution, and calls `Daemonize()` to relaunch the same binary. The parent loads and validates the YAML or JSON configuration, applies explicit command-line overrides, and passes the resulting configuration through the daemonizer pipe. The parent prints the startup result and exits. The daemon child retrieves the parameters with `WaitForParent()` and starts the server without reopening a potentially changed file.

The daemon also:

- Creates the PID file atomically and rejects a second live instance.
- Stops accepting new HTTP work on `SIGTERM` or `SIGINT`.
- Safely unmounts managed mounts during graceful shutdown while preserving their ordinary records. The next daemon start restores them only when `restore_mounts_on_restart` is enabled. Only an explicit `Unmount` changes a record to a tombstone. The daemon exits immediately after all managed unmount operations complete.
- Sends `SIGTERM` to child process groups and terminates remaining processes as part of each bounded unmount operation.
- Supports `run` under systemd `Type=simple`, while retaining `start` for standalone daemonization.

Daemon stdout/stderr defaults to `/var/log/irodsfsd/irodsfsd.log`. The go-daemonizer options explicitly set the working directory, environment, and standard streams so behavior does not depend on the invoking shell.

## 5. Daemon Configuration

Both YAML and JSON are supported. The format is detected from content rather
than the file extension. Defaults are applied before decoding. Daemon config
currently ignores unknown fields for forward compatibility; the client-side
mount JSON loader and REST protobuf decoder reject unknown mount fields.

```yaml
service_endpoint: "tcp://0.0.0.0:13020"
management_service_port: 13021
irodsfs_executable_path: "/usr/local/bin/irodsfs"
mount_executable_path: "/usr/bin/mount"
unmount_executable_path: "/usr/bin/umount"
data_root_path: "/var/lib/irodsfsd"
pid_file: "/run/irodsfsd/irodsfsd.pid"
log_root_path: "/var/log/irodsfsd"
mount_root_path: "/var/lib/irodsfsd/mounts"
recovery_encryption_key: "" # base64-encoded 32-byte key; required in production
debug: false
allowed_mount_root_paths:
  - "/mnt/irods"
  - "/var/lib/kubelet"
allow_fuse_allow_other: false
# Retry only failures after a mount has already become mounted. Initial mount
# failures always follow retry settings.
auto_remount: false
# Restore ordinary persisted mounts during daemon startup. Unmount tombstones
# are always reconciled.
restore_mounts_on_restart: false

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 30s
  multiplier: 2
  jitter: 0.2

mount_timeout: 30s
unmount_timeout: 15s
davfs_unmount_timeout: 3m
reconcile_interval: 10s
max_concurrent_mounts: 40
```

Startup validates that the irodsfs, mount, and unmount binaries exist and are
executable, required directories have correct permissions, Badger opens
successfully, and either `fusermount3` or `fusermount` is available. FUSE is
checked once at daemon startup even though NFS mounts do not use it. The
service has no application-level authentication; network access is restricted
by the host firewall.

`mount_root_path` is the parent directory for managed irodsfs data roots. Each
mount uses a daemon-generated child directory such as
`/var/lib/irodsfsd/mounts/<mount-id>` as its irodsfs data root. It is distinct
from `allowed_mount_root_paths`, which restricts the local FUSE mount-point
paths requested through the API. `/var/lib/kubelet` is allowed by default for
CSI node staging and publishing paths. The daemon log file is
`<log_root_path>/irodsfsd.log`, and the daemon working directory is
`data_root_path`.

`allow_fuse_allow_other` is the daemon-wide policy gate for the FUSE
`allow_other` mount option (irodsfs and DAVFS). It defaults to `false`
(restrictive by default). When `false`, any mount request that explicitly
asks for `allow_other` (via `mount_options` or, for irodsfs, `fuse_options`)
is rejected, and `allow_other` is never applicable to NFS mounts regardless
of the policy setting. When `true`, the daemon forces `allow_other` onto
every irodsfs and DAVFS mount's options, regardless of whether the client
requested it; NFS mounts are left untouched, since NFS is not FUSE. Because
irodsfsd runs under one service account distinct from the users who access
the mounted volume, deployments where those users are different local
accounts than the service account will generally need
`allow_fuse_allow_other: true`.

`allow_fuse_allow_other` deliberately does NOT also force `default_permissions`.
The primary caller is irods-csi-driver: it sends the requesting container's
own UID/GID with the mount request, but that UID/GID belongs to the
container's own, unrelated account, with no mapping to any host identity —
irodsfsd has no way to learn what, if anything, it corresponds to on the
host. `default_permissions` would have the kernel check the *real* accessing
process's host UID/GID against the file's reported owner; with an
unmappable, essentially arbitrary reported owner, that check could just as
easily deny the legitimate accessing process as anyone else. Access control
therefore rests on `allow_other` (so the real, whatever-it-is, host UID can
reach the mount at all) plus the CSI driver/Kubernetes scoping each mount to
one workload's bind mount, not on kernel-enforced per-file ownership within
the mount. A deployment where the caller's UID/GID *is* authoritative on the
host may still request `default_permissions` explicitly in its mount
request; the daemon only ever refrains from adding it automatically.

Enabling `allow_fuse_allow_other` also requires the host's FUSE
configuration to actually permit it: unless irodsfsd runs as root, the
kernel FUSE module rejects `allow_other` from a non-root mounter unless
`user_allow_other` is uncommented in `/etc/fuse.conf`. The daemon checks
this at startup and on every readiness check, and fails fast (rather than
letting a later individual mount fail with a confusing kernel error) if
`allow_fuse_allow_other: true` is configured but `user_allow_other` is not
set.

## 6. Mount Model and State Machine

### Persistent model

```go
type Mount struct {
    ID              string          `json:"id"`
    MountPath       string          `json:"mount_path"`
    Config          MountConfig     `json:"config"` // encrypted at rest
    State           string          `json:"state"`
    Attempt         int             `json:"attempt"`
    LastError       *APIError       `json:"last_error,omitempty"`
    PID             int             `json:"pid,omitempty"` // observation only
    CreatedAt       time.Time       `json:"created_at"`
    UpdatedAt       time.Time       `json:"updated_at"`
    MountedAt       *time.Time      `json:"mounted_at,omitempty"`
}
```

`MountConfig` contains common mount settings and exactly one of `IRODSFSConfig`,
`DAVFSConfig`, or `NFSConfig`, and is encrypted at rest. API responses reuse the
same message shape, but are built from a copy whose iRODS password, ticket, PAM
token, Redis password, and DAVFS password fields have been redacted. The
original stored credentials are never modified or returned.

```text
pending_mount -> mounting -> mounted
                     |          |
                     v          v child crash
              retry_wait <- cleaning
                     |
                     v (attempts exhausted)
                   failed

mounted -> pending_unmount -> unmounting -> record deleted
                                  |
                                  v
                            retry_wait/failed
```

An ordinary mount record means that the mount must be running and restored
after a daemon restart. An accepted `Unmount` changes it to a tombstone before
process work begins, preventing restoration. Every transition is persisted
before an event and log record are emitted. If the daemon crashes in an
intermediate state, startup reconciliation uses the tombstone and mount table
to resume the correct operation.

### Invariants

- `mount_path` must be absolute and clean, and may not contain NUL bytes or escape through symlinks.
- Mounts are allowed only below configured roots such as `/mnt/irods`.
- Only one active mount record may own a canonical path; a unique path index enforces this.
- Root, daemon data/log/runtime paths, and conflicting ancestors or descendants are rejected.
- An existing unrelated filesystem is never covered by a new mount.
- User input is never assembled into a shell command. Use `exec.CommandContext(binary, args...)`.

## 7. Mount Workflow

`Mount` or `POST /api/v1/mounts` validates the request, stores the record and
iRODS configuration, and returns the accepted mount. A potentially long mount
and its retries are not tied to the client connection. If `mount_id` is absent,
the server generates one; otherwise the unique caller-supplied ID is used.

1. Validate the request schema, path policy, and uniqueness.
2. Create the local mount path with mode `0755` when it does not exist, after
   confirming that it is under an allowed root and does not conflict with a
   daemon-reserved path. Reject an existing non-directory.
3. Convert the API configuration to irodsfs JSON in memory. Do not create a credential-bearing configuration file.
4. Ignore untrusted values for `foreground` and `instanceid`. Force `foreground: true` and use the mount ID as the instance ID.
5. Start `irodsfs -f -c - <mount-path>` and write the JSON configuration to stdin. The exact contract is verified against a pinned irodsfs version in integration tests.
6. Write stdout and stderr to an append-only per-mount log and a bounded recent-line ring buffer.
7. Apply `mount_timeout` from the beginning of per-mount directory and client
   configuration preparation. The time already spent preparing is subtracted
   from the readiness wait. Until the resulting deadline, ensure the child
   remains alive and the expected entry appears in `/proc/self/mountinfo`.
8. Transition to `mounted` on success. On timeout, terminate the command,
   inspect the mount table, lazily detach any partial mount, and remove its
   per-mount data directory when doing so cannot discard staged DAVFS data.

For the iRODSFS client only, the daemon recognizes its documented startup
exit-status contract: `10` means configuration validation or work-directory
creation failed, and `11` means initial iRODS authentication failed. These
become `IRODSFS_CONFIGURATION_INVALID` and
`IRODSFS_AUTHENTICATION_FAILED`, respectively, and are terminal (`FAILED`,
not retryable), including for a first mount attempt. Any other non-zero
iRODSFS exit status follows the ordinary retry policy. DAVFS and NFS helpers
do not use this contract because their exit-status values belong to their own
tools.

DAVFS and NFS use the system mount helpers through
`mount -t davfs ...` and `mount -t nfs ...`. These are one-shot commands, so a
successful command exit is not treated as a client crash. DAVFS receives an
explicit non-anonymous password through stdin and stores its `davfs2.conf`
options with mode `0600` under the per-mount data root. NFS uses ordinary
`umount`. DAVFS also uses ordinary `umount`, allowing its helper to finish
synchronizing staged cache data before returning. irodsfs continues to use
`commons.UnmountFuse`. After a successful unmount and client-process exit, the
daemon removes the complete per-mount data directory, including DAVFS config
and cache data, before deleting the mount record. If DAVFS cache
synchronization exceeds `davfs_unmount_timeout`, the daemon uses `UnmountFuse` for a
lazy detach, returns `DAVFS_LAZY_UNMOUNT`, and keeps both the failed record and
data directory intact for recovery instead of discarding unsynchronized files.

The foreground irodsfs child is owned through `exec.Cmd`. A saved PID is only
an observation and is never used for recovery-time signaling; reconciliation
operates on verified mount paths instead, avoiding PID-reuse hazards.

## 8. Unmount and Crash Recovery

Normal unmount:

1. Persist an unmount tombstone first, disabling automatic remount.
2. For irodsfs, run irodsfs commons `UnmountFuse` and terminate the foreground
   child. For NFS, run ordinary `umount`. For DAVFS, run ordinary `umount` so
   its helper can synchronize staged cache files.
3. If DAVFS synchronization exceeds `davfs_unmount_timeout`, lazily detach it with
   `UnmountFuse`, retain its cache and record, and report
   `DAVFS_LAZY_UNMOUNT`.
4. Confirm removal from the mount table, terminate any remaining child process
   group, delete the per-mount data directory after a clean unmount, and then
   delete the mount record and encrypted credentials.

When `Wait()` reports an unexpected child exit, the supervisor immediately:

1. Records `cleaning`, exit code/signal, and `last_error`.
2. If the mount remains, runs `fusermount3 -uz <path>`. This is the critical path that prevents a broken FUSE endpoint from producing `Transport endpoint is not connected`.
3. Verifies that the mount disappeared. If cleanup fails, it transitions to `failed` and does not stack a new mount over the old one.
4. If the record is not an unmount tombstone, schedule a fresh child after backoff. Otherwise continue the unmount cleanup.

Lazy unmount is attempted before `SIGKILL`. The daemon never automatically unmounts an unrelated filesystem; mount identity must match the managed record.

## 9. Retry Policy

Mount and unmount operations have configurable maximum attempts, initial/maximum delay, multiplier, and jitter. Defaults are five total attempts with delays of approximately `1s, 2s, 4s, 8s, 16s`.

- Permanent errors such as invalid configuration, path-policy violations, or missing credentials fail immediately.
- iRODS connection failures, temporary FUSE failures, and `EBUSY` are retryable.
- A new mount or unmount request cancels an older retry timer.
- Global mount concurrency is limited, and startup restoration adds jitter to avoid a connection surge against the iRODS server.

## 10. Startup Restoration and Reconciliation

At startup:

1. Load all mount records from Badger.
2. Read `/proc/self/mountinfo` once and construct the actual mount map.
3. For an ordinary record with no actual mount, ignore stale PIDs, rematerialize configuration, and enqueue mounting.
4. For an ordinary record with an actual managed mount, clean it up and recreate it by default. A surviving process cannot be supervised reliably, so this restores log and crash ownership.
5. For an unmount tombstone, resume unmounting and delete the record after cleanup.
6. For an intermediate mount record with no actual mount, resume mounting or cleanup according to its state.

At a configurable interval, defaulting to ten seconds, the daemon compares child state and mount-table state. Per-ID/path locks prevent conflicts between API work and reconciliation. Each controller also uses a generation number; a completion from an obsolete goroutine is ignored.

The manager registry lock protects only mount-ID/path lookup and reservation.
It is never held during directory or configuration I/O, process startup,
mount readiness polling, child waits, mount/unmount commands, or cleanup. Each
mount has its own lifecycle lock, so a pending, stalled, or failed mount can
serialize operations for that mount ID without blocking unrelated mounts or
manager queries.

## 11. BadgerDB

One daemon process opens the embedded persistent store:

```text
meta/schema-version                  -> uint
mounts/<uuid>                        -> JSON MountRecord
mount-paths/<escaped-canonical-path> -> mount UUID
events/<uuid>/<timestamp-ulid>       -> JSON state-transition event
```

The mount record and path index are updated in one write transaction. Records include `schema_version`, with migrations run at startup. Runtime PID and process handles are not recovery evidence; the mount table is the final source for observed mount state.

Restoration requires persistent credentials. Badger encryption at rest is enabled, with its base64-encoded 32-byte key supplied through `recovery_encryption_key` in a root-only configuration file or injected from a secret manager. The data directory is readable only by the daemon account. Secrets are never copied into summaries or events. Production startup fails if encryption is required but no key is available; it never silently falls back to plaintext.

Operational procedures cover value-log GC, backup/restore, and disk-full failures. If a database write fails, the daemon neither reports success nor starts the related process operation.

## 12. gRPC and REST API

The canonical contract is the protobuf service `api.MountService` in
`service/api/api.proto`. The gRPC service is exposed through
`service_endpoint`, while the REST service uses JSON under `/api/v1` on
`management_service_port`. Both transports call the same application service and therefore
have identical validation, mount-ID handling, state transitions, and secret-redaction
rules. Errors have stable machine-readable codes.

`Mount` persists intent, starts the selected client, and returns before mount
readiness polling completes. Clients observe progress through the returned
`MountInfo.state` or subsequent `GetMount` and `ListMounts` calls. `Unmount`
waits for the bounded client-specific unmount and cleanup before returning. A
caller-supplied `mount_id` identifies a retried `Mount`; when it is omitted the
server generates one.

| gRPC method | REST method and path | Behavior |
|---|---|---|
| `Mount` | `POST /api/v1/mounts` | Create and start a mount, using an optional caller-supplied ID |
| `ListMounts` | `GET /api/v1/mounts` | List mounts with state, path-prefix, and client-user filters |
| `GetMount` | `GET /api/v1/mounts/{id}` | Return one secret-free mount resource |
| `Unmount` | `DELETE /api/v1/mounts/{id}` | Unmount and delete the record after cleanup |

### Create a mount

```http
POST /api/v1/mounts
Content-Type: application/json

{
  "mount_id": "optional-client-id",
  "config": {
    "irodsfs": {
      "account": {
        "irods_host": "data.example.org",
        "irods_port": 1247,
        "irods_user_name": "alice",
        "irods_zone_name": "tempZone",
        "irods_user_password": "secret"
      },
      "path_mappings": [
        {
          "irods_path": "/tempZone/home/alice/project",
          "mapping_path": "/",
          "resource_type": "dir"
        }
      ]
    },
    "mount_path": "/mnt/irods/alice",
    "read_only": true
  }
}
```

The response is `202 Accepted`, includes `Location: /api/v1/mounts/<id>`, and contains a redacted mount resource. Clients that may retry after a timeout should supply their own `mount_id`.

### Health endpoints

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/api/v1/healthz` | Process liveness |
| `GET` | `/api/v1/readyz` | Database and required-dependency readiness |

The shorter `/healthz` and `/readyz` aliases are also registered. Metrics are
served from `/metrics`, mount logs from
`/api/v1/mounts/{id}/logs`, and the embedded management UI from `/`.

### Go client and executable examples

The reusable `client.MountServiceClient` follows the same lifecycle style as
the irodsfs-pool client:

```go
mountClient := client.NewMountServiceClient(endpoint, 5*time.Minute, true, logger)
if err := mountClient.Connect(); err != nil { /* handle */ }
defer mountClient.Disconnect()

info, err := mountClient.Mount(config)                    // generated ID
info, err := mountClient.MountWithID("my-mount", config) // supplied ID
mounts, err := mountClient.ListMounts(&client.ListMountsFilter{
    States: []client.MountState{client.MountStateMounted},
})
info, err := mountClient.GetMount("my-mount")
info, err := mountClient.Unmount("my-mount")
```

The third constructor argument enables automatic reconnection. A gRPC
`Unavailable` error first triggers one immediate reconnect attempt. If that
succeeds, the RPC is retried once; otherwise one background reconnect loop
continues with exponential backoff. Calls made while it is reconnecting fail
immediately so callers can retry later. Every logger entry is enriched with a
unique `clientID`, and `Disconnect` cancels any running reconnect loop.

`client.LoadMountConfigJSONFile` strictly decodes protobuf JSON into a
`MountConfig`. Runnable examples live under `client_examples/mount`,
`client_examples/unmount`, and `client_examples/mount_list`; successful mount
and unmount commands print the mount ID and returned state.

A tombstone is retained while unmount is in progress so a daemon restart still completes cleanup before deleting the record and credentials.

```json
{
  "mount_id": "01J...",
  "state": "retry_wait",
  "attempt": 2,
  "next_retry_at": "2026-08-26T15:00:02Z",
  "pid": 12345,
  "config": {
    "irodsfs": {
      "account": {
        "irods_host": "data.example.org",
        "irods_port": 1247,
        "irods_zone_name": "tempZone",
        "irods_user_name": "alice",
        "irods_user_password": "[REDACTED]",
        "irods_ticket": "[REDACTED]",
        "irods_pam_token": "[REDACTED]"
      },
      "path_mappings": [{"irods_path": "/tempZone/home/alice/project", "mapping_path": "/"}]
    },
    "mount_path": "/mnt/irods/alice",
    "read_only": true
  },
  "last_error": {
    "code": "IRODSFS_EXITED",
    "message": "irodsfs exited before mount became ready"
  }
}
```

Status codes are `400` for validation, `404` for missing resources, `409` for
path or state conflicts, `413` for oversized bodies, `429` when mount capacity
is exhausted, `500` for internal errors, and `503` for unavailable
dependencies.

## 13. Logs

Each mount writes to `/var/log/irodsfsd/mounts/<id>/irodsfs.log`. Stdout and stderr are converted to line records with timestamp, stream, and mount ID.

```text
GET .../logs?tail=200
GET .../logs?since=2026-08-26T00:00:00Z&limit=1000
```

Response size and `limit` are bounded, and the current log is managed with
size- and retention-based rotation. Queries read the active log file; rotated
backups are retained on disk but are not included. A known-secret redaction
filter provides defense in depth, while configuration and passwords are never
placed on the command line. Live SSE streaming is not currently implemented.

irodsfs is launched with `log_path: "-"`, disabling its own default log file
under its data root (which would otherwise duplicate this same stream,
unredacted, and disappear when the mount's data root is removed on unmount);
irodsfs then logs to stderr only, which is exactly the stream irodsfsd
captures and redacts above.

The daemon audit log records API principal, action, mount ID/path, and result without passwords or complete configuration.

## 14. Web Monitoring

Static assets are embedded in the Go binary and served at `/`, sharing the REST API origin.

The current mount table displays:

- Current state
- iRODS user, zone, and host
- Local mount path
- Read-only status
- PID and attempt count
- Latest error
- Mount and Unmount actions
- Recent logs in a refreshable log drawer

The UI is a small embedded HTML/CSS/JavaScript page. State refreshes every few
seconds and logs refresh on demand. Secrets are never sent to the DOM.

## 15. Security and Permissions

- Prefer a dedicated OS account for the daemon and its `irodsfs` children.
- Restrict access to the API service port with appropriate host firewall rules.
- Limit allowed mount roots, request body size, path-mapping count, and log-query size.
- Canonicalize paths and inspect existing parent symlinks to prevent escape from an allowed root.
- Build a child environment from an allowlist instead of inheriting unexpected proxy or iRODS variables.
- Require data and runtime directories to be `0700` and verify PID/runtime ownership. Credential-bearing irodsfs configuration is sent only through child-process stdin.
- Reject an explicit client request for `allow_other` unless daemon-wide policy permits it (`allow_fuse_allow_other: true`); when the policy is enabled, force `allow_other` onto every irodsfs/DAVFS mount regardless of the request. `allow_other` never applies to NFS mounts. Do not also auto-force `default_permissions`: the caller-supplied UID/GID (e.g. from irods-csi-driver) is a container-local identity unmapped to any host account, so kernel-enforced owner-based checks against it cannot be trusted to admit the legitimate accessing process; a caller that knows its UID/GID is host-authoritative may still request `default_permissions` itself. Fail daemon startup and readiness checks if the policy is enabled but the host's `/etc/fuse.conf` does not have `user_allow_other` set (unless running as root).
- Apply request rate limits and audit logging.
- Record the request source address separately from the iRODS user used by a mount.

## 16. Observability

In addition to structured daemon logs, expose optional Prometheus metrics:

- `irodsfsd_mounts{state=...}`
- `irodsfsd_mount_operations_total{operation,result}`
- `irodsfsd_mount_operation_duration_seconds`
- `irodsfsd_child_crashes_total`
- `irodsfsd_reconcile_errors_total`
- `irodsfsd_log_dropped_lines_total`

Liveness and readiness remain separate. Individual mount failures do not make the daemon non-live; they are reported through mount state and metrics.

## 17. Concurrency and Shutdown

- A top-level `context.Context` owns the HTTP server, reconciler, log manager, and supervisor.
- A mutex protects the mount registry. Per-mount controllers serialize work using a command channel or mutex.
- Potentially blocking `cmd.Wait`, mountinfo polling, and log copying run in tracked, cancellable goroutines.
- Never hold a Badger transaction while launching an external process. Commit desired state, perform the operation, then persist the result in another short transaction.
- Shutdown order is HTTP drain, retry cancellation, safe unmount and child signaling, controller wait, then Badger close.

## 18. Testing

### Unit tests

- Configuration/API validation and secret redaction
- State transitions and retry/backoff/jitter
- Mountinfo parsing
- Canonical paths and allowed-root enforcement
- Badger repository, unique path index, and migrations
- Generation checks that ignore stale results

### Integration tests

A controllable fake `irodsfs` binary verifies:

- Foreground start, stdout/stderr capture, and normal exit
- Exit before mount readiness
- Lazy unmount after a crash
- Transient mount/unmount failures, eventual success, and retry exhaustion
- Restart recovery from `mounting`, `unmounting`, and tombstone states
- Exactly one winner for simultaneous requests on the same path
- PID reuse protection

A separate privileged CI job uses Linux FUSE and a test iRODS server to verify the pinned `irodsfs` contract:

- Foreground configuration really keeps the process in the foreground
- Expected filesystem/source representation in `/proc/self/mountinfo`
- `SIGTERM`, `fusermount3 -u`, and `fusermount3 -uz`
- Removal of `Transport endpoint is not connected` after a crash

API tests use `httptest`, the race detector, and fuzzing for paths, mountinfo, and configuration parsing.

## 19. Implementation Phases

### Phase 1: Foundation

- Go module, CLI, daemon configuration, and `run/start/stop/status`
- go-daemonizer integration and graceful shutdown
- Badger repository and initial schema
- Health and readiness endpoints

### Phase 2: Mount supervisor

- Mount CRUD model and API
- Foreground `irodsfs` child management
- Mountinfo verification and FUSE unmount
- State machine, retry, and crash cleanup
- Startup reconciliation

### Phase 3: Logs and UI

- Per-mount logs, rotation, queries, and SSE
- Monitoring table, details, and actions
- Stronger request auditing

### Phase 4: Operational readiness

- Prometheus metrics
- Database backup, migration, and GC
- systemd unit, packaging, and upgrade tests
- Privileged FUSE integration tests and fault injection

## 20. Decisions Required Before Implementation

1. Whether the daemon runs as root or a dedicated user, and whether mount ownership varies per request
2. Firewall rules controlling access to the API service port
3. Whether to store iRODS passwords in locally encrypted Badger or store references to an external secret manager
4. Whether to enable restoration of desired mounts at the next start (`restore_mounts_on_restart` defaults to `false`)
5. The supported `irodsfs` version and exact foreground CLI/config contract
6. Whether the API accepts a complete irodsfs configuration or only an approved field allowlist

Recommended initial choices are a dedicated OS account, firewall-restricted API access, a root-only configuration containing the Badger encryption key, safe unmount on shutdown, a pinned irodsfs version, and a configuration allowlist. Enable restart restoration only when the deployment requires it.

## 21. References

- [cyverse/irodsfs](https://github.com/cyverse/irodsfs): reference for configuration YAML, path mappings, foreground configuration, log levels, and normal/lazy `fusermount`
- [cyverse/go-daemonizer](https://github.com/cyverse/go-daemonizer): reference for same-binary relaunch, `IsDaemon`, pipe parameters, and detached process setup
- [dgraph-io/badger](https://github.com/dgraph-io/badger): reference for the embedded persistent key-value store and transactional updates
