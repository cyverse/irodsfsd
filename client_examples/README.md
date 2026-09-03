# irodsfsd client examples

The examples use the reusable Go client in `client/` and connect to the gRPC
endpoint. The default endpoint is
`unix:///var/lib/irodsfsd/comm.sock`; use `-endpoint tcp://host:13020` when the
daemon listens on TCP. They enable background reconnection and attach a
command field to the logger; the client adds a unique `clientID` field.

## Mount

`mount` reads one JSON file containing a protobuf `MountConfig`. It asks the
server to generate a mount ID unless `-mount-id` is supplied.

```sh
cp client_examples/mount/mount_config.json.example /tmp/mount.json
# Edit /tmp/mount.json before running it; it contains placeholder credentials.
go run ./client_examples/mount -endpoint tcp://127.0.0.1:13020 /tmp/mount.json
go run ./client_examples/mount -mount-id my-mount /tmp/mount.json
```

A successful request prints the assigned mount ID and the state returned by
the server:

```text
mount_id: my-mount
state: MOUNT_STATE_MOUNTING
```

## List mounts

```sh
go run ./client_examples/mount_list -endpoint tcp://127.0.0.1:13020
```

The command prints mount ID, current state, and local mount path as a table.

## Unmount

```sh
go run ./client_examples/unmount -endpoint tcp://127.0.0.1:13020 my-mount
```

DAVFS unmount may wait for cache synchronization, so the default unmount RPC
timeout is five minutes. Override it with `-timeout` when necessary.
