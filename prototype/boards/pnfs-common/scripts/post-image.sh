#!/bin/sh

BOARD_DIR="$( dirname "${0}" )"

if grep -q '^BR2_aarch64=y$' "${BR2_CONFIG}"; then
  TARGET_ARCH="arm64"
  KERNEL_IMAGE="Image"
else
  TARGET_ARCH="amd64"
  KERNEL_IMAGE="bzImage"
fi

echo -n "Converting RAW image to QCOW2... "
qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on ${BINARIES_DIR}/rootfs.ext2 ${BINARIES_DIR}/disk.qcow2
rm -rf ${BINARIES_DIR}/rootfs.ext2
echo "done."

if [ -f ${BINARIES_DIR}/${KERNEL_IMAGE} ]; then
  echo "Symlinking Linux kernel image..."
  [ -e ${BINARIES_DIR}/vmlinuz ] && rm ${BINARIES_DIR}/vmlinuz
  ln -s ${KERNEL_IMAGE} ${BINARIES_DIR}/vmlinuz
fi
rm -rf ${BINARIES_DIR}/vmlinux

# A scratch image holding /vmlinuz and /disk.qcow2 for local use, which the
# MDS image can take as GUEST_IMAGE. Only for the host's own architecture: the
# builder container has Docker's legacy builder, which cannot build for another
# one. CI publishes every architecture with buildx (Dockerfile.publish).
case "$(uname -m)" in
  x86_64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
  *) HOST_ARCH="$(uname -m)" ;;
esac
if [ "${HOST_ARCH}" = "${TARGET_ARCH}" ]; then
  echo "Building guest image pnfs-guest:${TARGET_ARCH}..."
  if ! docker build --build-arg KERNEL_IMAGE=${KERNEL_IMAGE} \
      -t pnfs-guest:${TARGET_ARCH} -f "${BOARD_DIR}/Dockerfile" "${BINARIES_DIR}"; then
    echo "Building pnfs-guest:${TARGET_ARCH} failed." >&2
    exit 1
  fi
else
  echo "Skipping the local guest image: ${TARGET_ARCH} is not this host's architecture (${HOST_ARCH})."
fi

echo "Build completed."
