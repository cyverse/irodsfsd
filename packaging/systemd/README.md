# systemd Installation

The service uses `Type=forking` because `irodsfsd start` launches a detached
child through go-daemonizer and exits only after the child reports readiness.
systemd follows the child using `PIDFile=/run/irodsfsd/irodsfsd.pid`.

## Install

Build and install the binary:

```sh
make build
sudo install -o root -g root -m 0755 bin/irodsfsd /usr/bin/irodsfsd
```

Create the dedicated service account if the package or host provisioning does
not already provide it:

```sh
sudo useradd --system --home-dir /var/lib/irodsfsd --shell /usr/sbin/nologin irodsfsd
```

Install the configuration, environment, and unit files:

```sh
sudo install -d -o root -g irodsfsd -m 0750 /etc/irodsfsd
sudo install -o root -g irodsfsd -m 0640 packaging/systemd/config.yaml /etc/irodsfsd/config.yaml
sudo install -o root -g irodsfsd -m 0640 packaging/systemd/irodsfsd.conf /etc/irodsfsd/irodsfsd.conf
sudo install -o root -g root -m 0644 packaging/systemd/irodsfsd.service /etc/systemd/system/irodsfsd.service
sudo systemctl daemon-reload
sudo systemctl enable --now irodsfsd
```

systemd creates and owns these directories according to the unit:

- `/run/irodsfsd`
- `/var/lib/irodsfsd`
- `/var/log/irodsfsd`

## Operate

```sh
sudo systemctl status irodsfsd
sudo systemctl restart irodsfsd
sudo systemctl stop irodsfsd
journalctl -u irodsfsd
tail -F /var/log/irodsfsd/irodsfsd.log
```

Do not run `irodsfsd start` manually while the systemd unit is active. The PID
file lock rejects the second instance, but all lifecycle operations should use
`systemctl` once the service is installed.

Configuration may be written as YAML or JSON. The parser detects the format
from the file content, so the path in `IRODSFSD_CONFIG` does not require a
particular extension. Invalid values prevent startup; unknown daemon-config
fields are currently ignored for forward compatibility.

The `pid_file` value must remain `/run/irodsfsd/irodsfsd.pid` when using this
unit because it must match systemd's `PIDFile` setting.
