# irodsfsd systemd installation

## Prerequisites

- A systemd-based Linux system.
- The `irodsfs` executable installed at the path configured by
  `irodsfs_executable_path` (the packaged default is `/usr/local/bin/irodsfs`).

## Install

Build from source, then run:

```sh
make build
sudo ./packaging/systemd/install.sh
```

The script also works from a release archive. After extracting the archive,
run `sudo ./install.sh`. It installs:

- `/usr/bin/irodsfsd` — service binary
- `/etc/irodsfsd/config.yaml` — configuration file
- `/etc/systemd/system/irodsfsd.service` — systemd unit

It creates the `irodsfsd` system user/group and the daemon data directories.
An existing configuration file is preserved, so reinstalling never replaces a
local recovery key or other local settings. The service is enabled and started
immediately.

When `recovery_encryption_key` is empty, the installer generates and stores a
base64-encoded 32-byte key before it starts the service. Back up
`/etc/irodsfsd/config.yaml`: this key must remain stable so persisted mount
credentials can be decrypted after a host replacement.

## Configuration

Edit `/etc/irodsfsd/config.yaml` before restarting the service if the packaged
defaults do not match the host. In particular, verify
`irodsfs_executable_path`, the permitted mount roots, and network endpoints.
The `pid_file` value must remain `/run/irodsfsd/irodsfsd.pid` when using the
packaged unit because it must match systemd's `PIDFile` setting.

## Service management

The installer already enables and starts the service. After configuration
changes:

```sh
sudo systemctl restart irodsfsd.service
sudo systemctl status irodsfsd.service
journalctl -u irodsfsd.service -f
```

## Uninstall

```sh
sudo systemctl disable --now irodsfsd.service
sudo rm -f /etc/systemd/system/irodsfsd.service /usr/bin/irodsfsd
sudo systemctl daemon-reload
```

The configuration and data directories are intentionally preserved because
they contain the recovery key and encrypted mount state.
