# irodsfsd Design

## 1. Purpose

`irodsfsd` is a long-running Linux daemon written in Go. It manages multiple `irodsfs` mounts through a REST API and web interface, including each child process, its state, and its logs. Desired mount definitions are persisted in BadgerDB. After a daemon restart, they are reconciled with the actual system state and restored automatically.

The initial scope is a single `irodsfsd` instance managing multiple mounts on one Linux host. High availability, cross-host mount coordination, and reimplementation of iRODS are out of scope.

## 2. Core Design Principles

1. **Separate desired state from observed state.** The REST API and database record whether a mount should be `mounted` or `unmounted`. A reconciler drives child processes and Linux mounts toward that desired state.
2. **Run `irodsfs` as a foreground child.** Force `foreground: true` or the equivalent CLI option so that `exec.Cmd` owns the real FUSE process. Allowing `irodsfs` to daemonize again would make PID tracking, exit detection, and stdout/stderr capture unreliable.
3. **Do not use PID existence as proof of a successful mount.** A mount becomes `mounted` only after the target and expected FUSE filesystem appear in `/proc/self/mountinfo`.
4. **Clean up a crash before retrying.** If a child exits unexpectedly while its mount remains, lazy-unmount it immediately to prevent `Transport endpoint is not connected`, then retry.
5. **Serialize operations per mount path.** Mount, unmount, and reconciliation for the same canonical path must never run concurrently.
6. **Never return secrets.** Passwords and other sensitive values are always redacted from API responses, the UI, events, and application logs.

## 3. Architecture

```text
CLI parent
  `-- go-daemonizer --> irodsfsd daemon
                           |-- HTTP API + Web UI
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
cmd/irodsfsd/        main entry point and CLI commands/flags
internal/config/     daemon configuration loading and validation
internal/daemon/     go-daemonizer integration, PID lock, signals
internal/api/        REST handlers, middleware, DTOs
internal/mount/      service, controllers, state machine, reconciler
internal/process/    irodsfs start/stop/Wait and process groups
internal/fuse/       mountinfo inspection and fusermount execution
internal/store/      Badger repositories and schema migrations
internal/logstore/   child log files, tail, and queries
internal/web/        embedded static UI
```

HTTP handlers do not launch processes directly. They submit commands to `MountService`. Mount records in BadgerDB are the source of truth. The in-memory registry contains only ephemeral data such as running `exec.Cmd` instances, cancellation functions, and per-mount locks.

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

Common options are `--config/-c`, `--log-level`, and `--debug`. Command-line values override configuration-file values. The implementation uses Cobra and keeps command declaration separate from validation and application startup.

### go-daemonizer

The `start` parent creates a daemonizer with `go-daemonizer.New()`, uses `IsDaemon()` to distinguish the parent from the detached execution, and calls `Daemonize()` to relaunch the same binary. The parent loads and validates the YAML or JSON configuration, applies explicit command-line overrides, and passes the resulting configuration through the daemonizer pipe. The parent prints the startup result and exits. The daemon child retrieves the parameters with `WaitForParent()` and starts the server without reopening a potentially changed file.

The daemon also:

- Creates the PID file atomically and rejects a second live instance.
- Stops accepting new HTTP work on `SIGTERM` or `SIGINT`.
- Safely unmounts managed mounts during graceful shutdown while preserving `desired_state=mounted`; the next daemon start restores them. Only an explicit unmount request changes the desired state.
- Sends `SIGTERM` to child process groups and terminates remaining processes after the grace period.
- Supports `run` under systemd `Type=simple`, while retaining `start` for standalone daemonization.

Daemon stdout/stderr defaults to `/var/log/irodsfsd/irodsfsd.log`. The go-daemonizer options explicitly set the working directory, environment, and standard streams so behavior does not depend on the invoking shell.

## 5. Daemon Configuration

Both YAML and JSON are supported. The format is detected from content rather
than the file extension. Defaults are applied before decoding, and unknown
fields are rejected so misspelled settings cannot silently change behavior.

```yaml
api_service_port: 13021
irodsfs_binary: "/usr/local/bin/irodsfs"
data_dir: "/var/lib/irodsfsd"
runtime_dir: "/run/irodsfsd"
mount_root: "/var/lib/irodsfsd/mounts"
pid_file: "/run/irodsfsd/irodsfsd.pid"
daemon_log_file: "/var/log/irodsfsd/irodsfsd.log"
working_directory: "/var/lib/irodsfsd"
allowed_mount_roots:
  - "/mnt/irods"
  - "/var/lib/kubelet"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 30s
  multiplier: 2
  jitter: 0.2

mount_timeout: 30s
unmount_timeout: 15s
shutdown_grace_period: 20s
reconcile_interval: 10s
max_concurrent_mounts: 4
restore_on_start: true
```

Startup validates that the binary exists and is executable, required directories have correct permissions, Badger opens successfully, and either `fusermount3` or `fusermount` is available. The service has no application-level authentication; network access is restricted by the host firewall.

`mount_root` is the parent directory for managed irodsfs data roots. Each mount
uses a daemon-generated child directory such as
`/var/lib/irodsfsd/mounts/<mount-id>` as its irodsfs data root. It is distinct
from `allowed_mount_roots`, which restricts the local FUSE mount-point paths
requested through the API. `/var/lib/kubelet` is allowed by default for CSI
node staging and publishing paths.

## 6. Mount Model and State Machine

### Persistent model

```go
type Mount struct {
    ID              string          `json:"id"`
    MountPath       string          `json:"mount_path"`
    IRODSConfig     json.RawMessage `json:"-"`
    ConfigSummary   ConfigSummary   `json:"config"`
    DesiredState    string          `json:"desired_state"` // mounted|unmounted
    State           string          `json:"state"`
    Attempt         int             `json:"attempt"`
    LastError       *APIError       `json:"last_error,omitempty"`
    PID             int             `json:"pid,omitempty"` // observation only
    CreatedAt       time.Time       `json:"created_at"`
    UpdatedAt       time.Time       `json:"updated_at"`
    MountedAt       *time.Time      `json:"mounted_at,omitempty"`
    StoppedAt       *time.Time      `json:"stopped_at,omitempty"`
}
```

`ConfigSummary` contains only host, port, zone, user, path mappings, and read-only status. It never includes passwords, tokens, or private keys.

```text
pending_mount -> mounting -> mounted
                     |          |
                     v          v child crash
              retry_wait <- cleaning
                     |
                     v (attempts exhausted)
                   failed

mounted -> pending_unmount -> unmounting -> unmounted
                                  |
                                  v
                            retry_wait/failed
```

`desired_state` is persisted transactionally as soon as a request is accepted. `state` represents the controller's current observed stage. Every transition is persisted before an event and log record are emitted. If the daemon crashes in an intermediate state, startup reconciliation uses the mount table to determine the real state.

### Invariants

- `mount_path` must be absolute and clean, and may not contain NUL bytes or escape through symlinks.
- Mounts are allowed only below configured roots such as `/mnt/irods`.
- Only one active mount record may own a canonical path; a unique path index enforces this.
- Root, daemon data/log/runtime paths, and conflicting ancestors or descendants are rejected.
- An existing unrelated filesystem is never covered by a new mount.
- User input is never assembled into a shell command. Use `exec.CommandContext(binary, args...)`.

## 7. Mount Workflow

`POST /api/v1/mounts` validates the request, stores the record and iRODS configuration, and returns `202 Accepted`. A potentially long mount and its retries are not tied to the HTTP connection.

1. Validate the request schema, path policy, and uniqueness.
2. Create a missing mount directory when policy permits, with controlled ownership and mode.
3. Materialize the submitted configuration at `/var/lib/irodsfsd/mounts/<id>/irodsfs.yaml` with mode `0600`.
4. Ignore untrusted values for `foreground`, `childprocess`, `watchdogprocess`, `watchpid`, and `instanceid`. Force `foreground: true` and a unique instance ID.
5. Start the equivalent of `irodsfs --foreground -c <managed-config> <mount-path>`. The exact contract is verified against a pinned irodsfs version in integration tests.
6. Write stdout and stderr to an append-only per-mount log and a bounded recent-line ring buffer.
7. Until `mount_timeout`, ensure the child remains alive and the expected entry appears in `/proc/self/mountinfo`.
8. Transition to `mounted` on success. On failure, clean up before applying retry policy.

Each child runs in its own process group. A saved PID is only an observation and is not trusted after restart. The daemon must verify `/proc/<pid>` start time and command line before signaling any recovered PID, preventing harm from PID reuse.

## 8. Unmount and Crash Recovery

Normal unmount:

1. Persist `desired_state=unmounted` first, disabling automatic remount.
2. Send `SIGTERM` to the child and wait briefly for exit and unmount.
3. If the mount remains, execute `fusermount3 -u <path>`, or `fusermount -u` where appropriate.
4. Retry on `EBUSY` or timeout.
5. When normal retries are exhausted, or when the API uses `force=true`, run `fusermount3 -uz <path>` for a lazy unmount.
6. Confirm removal from the mount table, terminate any remaining child process group, and transition to `unmounted`.

When `Wait()` reports an unexpected child exit, the supervisor immediately:

1. Records `cleaning`, exit code/signal, and `last_error`.
2. If the mount remains, runs `fusermount3 -uz <path>`. This is the critical path that prevents a broken FUSE endpoint from producing `Transport endpoint is not connected`.
3. Verifies that the mount disappeared. If cleanup fails, it transitions to `failed` and does not stack a new mount over the old one.
4. If the desired state is still `mounted`, schedules a fresh child after backoff. Otherwise it stops.

Lazy unmount is attempted before `SIGKILL`. The daemon never automatically unmounts an unrelated filesystem; mount identity must match the managed record.

## 9. Retry Policy

Mount and unmount operations have configurable maximum attempts, initial/maximum delay, multiplier, and jitter. Defaults are five total attempts with delays of approximately `1s, 2s, 4s, 8s, 16s`.

- Permanent errors such as invalid configuration, path-policy violations, or missing credentials fail immediately.
- iRODS connection failures, temporary FUSE failures, and `EBUSY` are retryable.
- A new mount or unmount request cancels an older retry timer.
- `POST /mounts/{id}/retry` resets the counter for an exhausted operation.
- Global mount concurrency is limited, and startup restoration adds jitter to avoid a connection surge against the iRODS server.

## 10. Startup Restoration and Reconciliation

At startup:

1. Load all mount records from Badger.
2. Read `/proc/self/mountinfo` once and construct the actual mount map.
3. For `desired=mounted` with no actual mount, ignore stale PIDs, rematerialize configuration, and enqueue mounting.
4. For `desired=mounted` with an actual managed mount, clean it up and recreate it by default. A surviving process cannot be supervised reliably, so this restores log and crash ownership.
5. For `desired=unmounted` with an actual mount, enqueue unmounting.
6. For an intermediate record with no actual mount, resume according to desired state.

At a configurable interval, defaulting to ten seconds, the daemon compares child state and mount-table state. Per-ID/path locks prevent conflicts between API work and reconciliation. Each controller also uses a generation number; a completion from an obsolete goroutine is ignored.

## 11. BadgerDB

One daemon process opens the embedded persistent store:

```text
meta/schema-version                  -> uint
mounts/<uuid>                        -> JSON MountRecord
mount-paths/<escaped-canonical-path> -> mount UUID
events/<uuid>/<timestamp-ulid>       -> JSON state-transition event
```

The mount record and path index are updated in one write transaction. Records include `schema_version`, with migrations run at startup. Runtime PID and process handles are not recovery evidence; the mount table is the final source for observed mount state.

Restoration requires persistent credentials. Badger encryption at rest is enabled, with its key supplied from a root-only file or secret manager outside the database. Managed configuration files and the data directory are readable only by the daemon account. Secrets are never copied into summaries or events. Production startup fails if encryption is required but no key is available; it never silently falls back to plaintext.

Operational procedures cover value-log GC, backup/restore, and disk-full failures. If a database write fails, the daemon neither reports success nor starts the related process operation.

## 12. REST API

The API uses JSON under `/api/v1`. Errors have stable machine-readable codes.

### Create a mount

```http
POST /api/v1/mounts
Content-Type: application/json

{
  "mount_path": "/mnt/irods/alice",
  "irods_config": {
    "irods_host": "data.example.org",
    "irods_port": 1247,
    "irods_user_name": "alice",
    "irods_zone_name": "tempZone",
    "irods_user_password": "secret",
    "readonly": true,
    "path_mappings": [
      {
        "irods_path": "/tempZone/home/alice/project",
        "mapping_path": "/",
        "resource_type": "dir"
      }
    ]
  }
}
```

The response is `202 Accepted`, includes `Location: /api/v1/mounts/<id>`, and contains a secret-free mount resource. An optional idempotency key prevents duplicate mounts after client timeouts.

### Query and control

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/api/v1/mounts` | List mounts with state, user, and path filters plus pagination |
| `GET` | `/api/v1/mounts/{id}` | Return configuration summary, state, retry, and error |
| `DELETE` | `/api/v1/mounts/{id}` | Unmount and delete asynchronously; returns `202` |
| `POST` | `/api/v1/mounts/{id}/unmount` | Preserve the record and change desired state |
| `POST` | `/api/v1/mounts/{id}/mount` | Remount from stored configuration |
| `POST` | `/api/v1/mounts/{id}/retry` | Reset an exhausted retry counter |
| `GET` | `/api/v1/mounts/{id}/logs` | Query or tail stored logs |
| `GET` | `/api/v1/mounts/{id}/logs/stream` | Stream live logs over SSE |
| `GET` | `/api/v1/healthz` | Process liveness |
| `GET` | `/api/v1/readyz` | Database and required-dependency readiness |

`DELETE` accepts `?force=true`. A tombstone is retained while deletion is in progress so a daemon restart still completes the unmount before applying record, configuration, and log retention policy.

```json
{
  "id": "01J...",
  "mount_path": "/mnt/irods/alice",
  "desired_state": "mounted",
  "state": "retry_wait",
  "attempt": 2,
  "next_retry_at": "2026-08-26T15:00:02Z",
  "pid": 12345,
  "config": {
    "host": "data.example.org",
    "port": 1247,
    "zone": "tempZone",
    "user": "alice",
    "readonly": true,
    "collections": ["/tempZone/home/alice/project"]
  },
  "last_error": {
    "code": "IRODSFS_EXITED",
    "message": "irodsfs exited before mount became ready"
  }
}
```

Status codes are `400` for validation, `404` for missing resources, `409` for path or state conflicts, `413` for oversized bodies, `429` for rate limiting, `500` for internal errors, and `503` for unavailable dependencies. Every response and log entry carries a request ID.

## 13. Logs

Each mount writes to `/var/log/irodsfsd/mounts/<id>/irodsfs.log`. Stdout and stderr are converted to line records with timestamp, stream, and mount ID.

```text
GET .../logs?tail=200
GET .../logs?since=2026-08-26T00:00:00Z&limit=1000
GET .../logs/stream                       # text/event-stream
```

Response size and `limit` are bounded, with size- and retention-based rotation. A slow SSE client never creates an unbounded buffer; old live lines are dropped and a gap event is sent. A known-secret redaction filter provides defense in depth, but configuration and passwords are never placed on the command line.

The daemon audit log records API principal, action, mount ID/path, and result without passwords or complete configuration.

## 14. Web Monitoring

Static assets are embedded in the Go binary and served at `/`, sharing the REST API origin.

The mount table displays:

- Observed and desired-state badges
- iRODS user, zone, and host
- Local mount path
- Collections/data objects and mapping paths
- Read-only status
- PID, start time, and uptime
- Attempt count, next retry, and latest error
- Mount, Unmount, Retry, and Force Unmount actions
- Recent logs and a live-log drawer

The first version uses server rendering or a small vanilla TypeScript/JavaScript bundle. State initially refreshes every two to five seconds, while logs use SSE. State SSE can be added later. Secrets are never sent to the DOM.

## 15. Security and Permissions

- Prefer a dedicated OS account for the daemon and its `irodsfs` children.
- Restrict access to the API service port with appropriate host firewall rules.
- Limit allowed mount roots, request body size, path-mapping count, and log-query size.
- Canonicalize paths and inspect existing parent symlinks to prevent escape from an allowed root.
- Build a child environment from an allowlist instead of inheriting unexpected proxy or iRODS variables.
- Require configuration files to be `0600`, directories `0700`, and verify PID/runtime ownership.
- Accept `allow_other` only when daemon-wide policy permits it.
- Apply request rate limits and audit logging.
- Record the request source address separately from the iRODS user used by a mount.

## 16. Observability

In addition to structured daemon logs, expose optional Prometheus metrics:

- `irodsfsd_mounts{state=...}`
- `irodsfsd_mount_operations_total{operation,result}`
- `irodsfsd_mount_operation_duration_seconds`
- `irodsfsd_child_crashes_total`
- `irodsfsd_forced_unmounts_total`
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
- Mountinfo verification and normal/forced unmount
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
4. Whether to change the default policy of unmounting physical mounts during graceful shutdown and restoring desired mounts at the next start
5. The supported `irodsfs` version and exact foreground CLI/config contract
6. Whether the API accepts a complete irodsfs configuration or only an approved field allowlist

Recommended initial choices are a dedicated OS account, firewall-restricted API access, a separate root-only Badger encryption-key file, safe unmount on shutdown followed by restoration at restart, a pinned irodsfs version, and a configuration allowlist.

## 21. References

- [cyverse/irodsfs](https://github.com/cyverse/irodsfs): reference for configuration YAML, path mappings, foreground configuration, log levels, and normal/lazy `fusermount`
- [cyverse/go-daemonizer](https://github.com/cyverse/go-daemonizer): reference for same-binary relaunch, `IsDaemon`, pipe parameters, and detached process setup
- [dgraph-io/badger](https://github.com/dgraph-io/badger): reference for the embedded persistent key-value store and transactional updates
