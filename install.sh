#!/usr/bin/env bash
# Download and install the latest irodsfs and irodsfsd Linux releases.
set -euo pipefail

# irodsfsd runs irodsfs as its mount client, so both are installed by default.
# Set IRODSFSD_SKIP_IRODSFS=1 to keep an existing irodsfs installation as is.
skip_irodsfs="${IRODSFSD_SKIP_IRODSFS:-0}"

for command in curl tar uname mktemp grep; do
    if ! command -v "${command}" >/dev/null 2>&1; then
        echo "${command} is required to install irodsfsd" >&2
        exit 1
    fi
done

if [[ $(uname -s) != Linux ]]; then
    echo "irodsfsd is supported only on Linux" >&2
    exit 1
fi

case "$(uname -m)" in
    x86_64) release_arch="amd64" ;;
    i386|i486|i586|i686) release_arch="386" ;;
    aarch64|arm64) release_arch="arm64" ;;
    arm|armv6l|armv7l) release_arch="arm" ;;
    *)
        echo "unsupported Linux architecture: $(uname -m)" >&2
        exit 1
        ;;
esac

if [[ ${EUID} -ne 0 ]] && ! command -v sudo >/dev/null 2>&1; then
    echo "sudo is required when the installer is not run as root" >&2
    exit 1
fi

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/irodsfsd-install.XXXXXX")"
trap 'rm -rf "${work_dir}"' EXIT

# install_release <repository> <binary_name> <required file>...
# Downloads the latest release archive of the repository and runs the
# installer it ships, which is the layout both projects publish.
install_release() {
    local repository="$1"
    local binary_name="$2"
    shift 2

    local release_json
    release_json="$(curl -fsSL "https://api.github.com/repos/${repository}/releases/latest")" || {
        echo "failed to find the latest ${binary_name} GitHub release" >&2
        exit 1
    }

    local asset_suffix="-linux-${release_arch}.tar.gz"
    local asset_url
    asset_url="$(printf '%s' "${release_json}" |
        grep -oE '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]+"' |
        cut -d '"' -f 4 |
        grep -F -- "${asset_suffix}" |
        head -n 1 || true)"

    if [[ -z ${asset_url} ]]; then
        echo "the latest ${binary_name} release has no ${release_arch} Linux archive" >&2
        exit 1
    fi

    local package_dir="${work_dir}/${binary_name}"
    mkdir -p "${package_dir}"

    echo "downloading ${asset_url}"
    curl -fsSL "${asset_url}" -o "${package_dir}/release.tar.gz"
    tar -xzf "${package_dir}/release.tar.gz" -C "${package_dir}"

    local required_file
    for required_file in "$@"; do
        if [[ ! -f "${package_dir}/${required_file}" ]]; then
            echo "${binary_name} release archive is missing ${required_file}" >&2
            exit 1
        fi
    done
    if [[ ! -x "${package_dir}/${binary_name}" || ! -x "${package_dir}/install.sh" ]]; then
        echo "${binary_name} release archive contains a non-executable installer or binary" >&2
        exit 1
    fi

    if [[ ${EUID} -eq 0 ]]; then
        "${package_dir}/install.sh"
    else
        sudo "${package_dir}/install.sh"
    fi
}

if [[ ${skip_irodsfs} == 1 ]]; then
    echo "skipping irodsfs installation (IRODSFSD_SKIP_IRODSFS=1)"
else
    install_release "cyverse/irodsfs" "irodsfs" "irodsfs" mount.irodsfs install.sh
fi

install_release "cyverse/irodsfsd" "irodsfsd" "irodsfsd" install.sh config.yaml irodsfsd.service
