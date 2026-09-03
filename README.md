# irodsfsd

`irodsfsd` is a Linux daemon that manages multiple [`irodsfs`](https://github.com/cyverse/irodsfs)
FUSE mounts (and plain DAVFS/NFS mounts) through a gRPC/REST API and a small
embedded web UI. Desired mounts are persisted in an embedded, encrypted
database, so a daemon restart reconciles them back to the running state
automatically instead of relying on an external process supervisor to know
about each mount.

> **Status:** under active development. The core daemon — persistence,
> startup reconciliation, retry/crash recovery, graceful shutdown, per-mount
> logs, metrics, and the management UI — is implemented and tested. See
> [`design.md`](design.md) for the full design.

## Features

- **Mount/unmount over gRPC and REST**, backed by the same application
  service and validation rules for both transports.
- **irodsfs, DAVFS, and NFS** mount clients. `irodsfs` always runs in the
  foreground as a supervised child process; DAVFS/NFS use the system
  `mount`/`umount` helpers.
- **Crash-safe persistence**: mount intent (including credentials) is
  encrypted at rest in an embedded BadgerDB. An explicit `Unmount` is the
  only thing that tombstones a record — a mount that's simply not running
  yet is reconciled and restarted automatically, at startup and
  periodically thereafter.
- **Retry with backoff and jitter** for transient mount/unmount failures,
  with attempt count, next-retry time, and the last error exposed through
  the API.
- **Graceful shutdown** that safely unmounts every managed mount in
  parallel, each bounded by its own timeout, before the process exits.
- **Per-mount logs** (stdout/stderr, rotated, secrets redacted) queryable
  over REST, and Prometheus metrics for mount state, operation
  results/durations, child crashes, and reconciliation errors.
- **A small embedded management UI** at `/` — mount list, state, recent
  logs, and mount/unmount actions — served from the same origin as the REST
  API.

## Requirements

- Linux with FUSE (`/dev/fuse`) and `fusermount`/`fusermount3` available.
- The [`irodsfs`](https://github.com/cyverse/irodsfs) binary, plus the
  system `mount` and `umount` utilities for DAVFS/NFS support.
- Go 1.25+ to build from source.

## Building

```sh
make build          # -> bin/irodsfsd
```

or directly with Go:

```sh
go build -o bin/irodsfsd ./cmd
```

Run the test suite with `go test ./...`. Add `-race` when touching
`MountManager`'s concurrent code.

Reusable Go client code is available in [`client/`](client/). Runnable mount,
unmount, and mount-list examples are documented in
[`client_examples/README.md`](client_examples/README.md); build all three with
`make build-client-examples`.

## Configuration

`irodsfsd` accepts YAML or JSON (detected from content, not the file
extension); unknown daemon-config fields are currently ignored for forward
compatibility, while mount JSON is decoded strictly. See
[`packaging/systemd/config.yaml`](packaging/systemd/config.yaml) for a
complete annotated example, and design.md section 5 for the full field
reference. The only field with no safe default is
`recovery_encryption_key`, a base64-encoded 32-byte AES key used to encrypt
the mount database:

```sh
openssl rand -base64 32
```

Minimal example:

```yaml
service_endpoint: "tcp://0.0.0.0:13020"
management_service_port: 13021
irodsfs_executable_path: "/usr/local/bin/irodsfs"
data_root_path: "/var/lib/irodsfsd"
pid_file: "/run/irodsfsd/irodsfsd.pid"
log_root_path: "/var/log/irodsfsd"
recovery_encryption_key: "<base64 32-byte key>"
allowed_mount_root_paths:
  - "/mnt/irods"
```

Set `allow_fuse_allow_other: true` when the mount's end users are different
local accounts than the service account irodsfsd runs as. The daemon then
forces `allow_other` onto every irodsfs/DAVFS mount automatically (it does
not apply to NFS mounts), and refuses to start unless the host's
`/etc/fuse.conf` has `user_allow_other` set (skipped when running as root).
It does not also force `default_permissions`: a caller such as
irods-csi-driver supplies the container's own UID/GID, which has no mapping
to any host account, so kernel-enforced ownership checks against it can't be
trusted — a caller may still request `default_permissions` itself if its
UID/GID is host-authoritative. See `design.md` section 5 for details.

## Running

```sh
irodsfsd run     -c /etc/irodsfsd/config.yaml   # foreground, e.g. under systemd Type=simple
irodsfsd start   -c /etc/irodsfsd/config.yaml   # daemonize and detach
irodsfsd stop    -c /etc/irodsfsd/config.yaml --wait 20s
irodsfsd status  -c /etc/irodsfsd/config.yaml
irodsfsd version
```

For a systemd-managed install, see
[`packaging/systemd/README.md`](packaging/systemd/README.md) and
`make install`/`make uninstall`.

## Using the API

Create a mount:

```sh
curl -sX POST http://localhost:13021/api/v1/mounts \
  -H 'Content-Type: application/json' \
  -d '{
        "config": {
          "mount_path": "/mnt/irods/alice",
          "read_only": true,
          "irodsfs": {
            "account": {
              "irods_host": "data.example.org",
              "irods_zone_name": "tempZone",
              "irods_user_name": "alice",
              "irods_user_password": "secret"
            },
            "path_mappings": [
              {"irods_path": "/tempZone/home/alice", "mapping_path": "/"}
            ]
          }
        }
      }'
```

List mounts, fetch one, tail its logs, and unmount it:

```sh
curl http://localhost:13021/api/v1/mounts
curl http://localhost:13021/api/v1/mounts/<mount_id>
curl 'http://localhost:13021/api/v1/mounts/<mount_id>/logs?tail=100'
curl -X DELETE http://localhost:13021/api/v1/mounts/<mount_id>
```

Health, readiness, and metrics:

```sh
curl http://localhost:13021/healthz
curl http://localhost:13021/readyz
curl http://localhost:13021/metrics
```

The same operations are available over gRPC via `service/api/api.proto`
(`api.MountService`), and visually through the web UI at
`http://localhost:13021/`.

Passwords, tickets, and tokens are always redacted in API responses, the
UI, logs, and events — the daemon never echoes a submitted credential back.

## Project layout

```text
cmd/                 CLI entry point and daemon lifecycle (start/run/stop/status)
cmd/commons/         shared CLI helpers (flags, PID file handling)
commons/             daemon configuration, endpoints, and shared helpers
service/             MountManager, gRPC/REST servers, retry/reconciliation, metrics
service/api/         the protobuf/gRPC contract (api.proto) and generated code
service/store/       the BadgerDB-backed mount repository
service/logstore/    per-mount log files (rotation, query, redaction)
service/web/         the embedded management UI
packaging/systemd/   systemd unit and example configuration
```

## Documentation

- [`design.md`](design.md) — full design: architecture, state machine,
  persistence, retry/recovery, API contract, security, and testing
  strategy.
