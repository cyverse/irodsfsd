#!/usr/bin/env bash
# Install irodsfsd as a systemd service. This script works both from a source
# checkout and from the release archive layout.
set -euo pipefail

service_name="irodsfsd"
service_user="irodsfsd"
install_prefix="/usr/bin"
config_dir="/etc/irodsfsd"
unit_dir="/etc/systemd/system"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Release archives place all installation assets next to this script. A source
# checkout keeps this script under packaging/systemd and the binary under bin.
if [[ -x "${script_dir}/${service_name}" ]]; then
    binary_path="${script_dir}/${service_name}"
else
    source_root="$(cd -- "${script_dir}/../.." && pwd)"
    binary_path="${source_root}/bin/${service_name}"
fi

if [[ $# -ne 0 ]]; then
    echo "usage: sudo $0" >&2
    exit 2
fi

if [[ ${EUID} -ne 0 ]]; then
    echo "run this script as root (for example, sudo $0)" >&2
    exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl is required to install ${service_name}" >&2
    exit 1
fi
if [[ ! -f ${binary_path} || ! -x ${binary_path} ]]; then
    echo "service binary is missing or not executable: ${binary_path}" >&2
    exit 1
fi

if ! getent group "${service_user}" >/dev/null; then
    groupadd --system "${service_user}"
fi
if ! id -u "${service_user}" >/dev/null 2>&1; then
    useradd --system --gid "${service_user}" --home-dir "/var/lib/${service_name}" \
        --shell /usr/sbin/nologin "${service_user}"
fi

install -d -o root -g root -m 0755 "${install_prefix}" "${unit_dir}"
install -o root -g root -m 0755 "${binary_path}" "${install_prefix}/${service_name}"
install -d -o root -g "${service_user}" -m 0750 "${config_dir}"

config_path="${config_dir}/config.yaml"
if [[ ! -e ${config_path} ]]; then
    install -o root -g "${service_user}" -m 0640 \
        "${script_dir}/config.yaml" "${config_path}"
else
    echo "preserving existing configuration: ${config_path}"
fi

# The recovery key encrypts persisted mount credentials and must remain
# stable across restarts. Generate it only when the packaged or existing
# configuration intentionally has no value yet.
if grep -Eq "^[[:space:]]*recovery_encryption_key:[[:space:]]*(\"\"|'')?[[:space:]]*(#.*)?$" "${config_path}"; then
    if ! command -v openssl >/dev/null 2>&1; then
        echo "openssl is required to generate recovery_encryption_key" >&2
        exit 1
    fi
    recovery_key="$(openssl rand -base64 32 | tr -d '\n')"
    sed -i -E \
        "s@^([[:space:]]*recovery_encryption_key:)[[:space:]]*(\"\"|'')?([[:space:]]*#.*)?\$@\\1 \"${recovery_key}\"@" \
        "${config_path}"
    echo "generated recovery_encryption_key in ${config_path}; back up this file before replacing the host"
fi

install -d -o "${service_user}" -g "${service_user}" -m 0750 \
    /var/lib/irodsfsd /var/lib/irodsfsd/mounts
install -o root -g root -m 0644 "${script_dir}/${service_name}.service" \
    "${unit_dir}/${service_name}.service"

systemctl daemon-reload
systemctl enable --now "${service_name}.service"

echo "installed ${service_name}"
