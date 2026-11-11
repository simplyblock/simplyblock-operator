#!/usr/bin/env bash
#
# Builds the pNFS guest for one architecture inside the builder image and
# copies the kernel and the root filesystem to /out. The source tree is mounted
# read-only at /src, and buildroot's output, downloads, and compiler cache stay
# in /work, inside the container. Called by `make -C csi-driver pnfs-os-<arch>`.
#
# Usage: build.sh <amd64|arm64>
#
# PNFS_OS_PREBUILT_SDK=0 builds the toolchain instead of downloading the
# prebuilt SDK, and PNFS_OS_PARALLEL=0 turns off per-package directories.
# PNFS_OS_VERSION is written to /etc/pnfs-os in the guest.

set -euo pipefail

arch="${1:?usage: build.sh <amd64|arm64>}"
src=/src
out=/out
buildroot=/opt/buildroot
output=/work/output

case "${arch}" in
    amd64) defconfig=pnfs_qemu_x64_defconfig tuple=x86_64-buildroot-linux-gnu ;;
    arm64) defconfig=pnfs_qemu_arm64_defconfig tuple=aarch64-buildroot-linux-gnu ;;
    *) echo "unsupported architecture ${arch}" >&2; exit 1 ;;
esac

fragments=()
if [ "${PNFS_OS_PREBUILT_SDK:-1}" = 1 ]; then
    # The prebuilt SDKs are built for x86_64 Linux hosts only.
    if [ "$(uname -m)" != x86_64 ]; then
        echo "the prebuilt SDK needs an x86_64 host, set PNFS_OS_PREBUILT_SDK=0" >&2
        exit 1
    fi
    fragments+=(prebuilt-sdk)
fi
if [ "${PNFS_OS_PARALLEL:-1}" = 1 ]; then
    fragments+=(parallel)
fi

# prebuilt-sdk.fragment names the archive through SDK_NAME.
export SDK_NAME="${tuple}_sdk-buildroot-host-linux-amd64"
export PNFS_OS_VERSION="${PNFS_OS_VERSION:-unknown}"
export BR2_DL_DIR=/work/dl

make=(make -C "${buildroot}" O="${output}" BR2_EXTERNAL="${src}")

"${make[@]}" "${defconfig}"

# Fragments are merged over the board's defconfig and resolved by kconfig.
# Every option a fragment sets has to survive that, or the build would quietly
# be something else.
if [ "${#fragments[@]}" -gt 0 ]; then
    for fragment in "${fragments[@]}"; do
        echo "Merging ${fragment}.fragment"
        "${buildroot}/support/kconfig/merge_config.sh" -m -O "${output}" \
            "${output}/.config" "${src}/configs/${fragment}.fragment"
    done
    "${make[@]}" olddefconfig
    for fragment in "${fragments[@]}"; do
        grep -E '^BR2_[A-Za-z0-9_]+=' "${src}/configs/${fragment}.fragment" | while read -r line; do
            if ! grep -qxF "${line}" "${output}/.config"; then
                echo "${fragment}: ${line} did not take, the board's configuration rejected it" >&2
                exit 1
            fi
        done
    done
fi

# With per-package directories, packages build side by side, each with
# BR2_JLEVEL jobs of its own. The load limit keeps the product of the two from
# swamping the host.
cpus="$(nproc)"
if grep -q '^BR2_PER_PACKAGE_DIRECTORIES=y$' "${output}/.config"; then
    "${make[@]}" -j"${cpus}" -l"${cpus}"
else
    "${make[@]}"
fi

mkdir -p "${out}"
cp -L "${output}/images/vmlinuz" "${output}/images/disk.qcow2" "${out}/"
echo "Guest for ${arch} written to ${out}"
