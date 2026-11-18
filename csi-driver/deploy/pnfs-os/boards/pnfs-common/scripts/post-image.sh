#!/bin/sh

if grep -q '^BR2_aarch64=y$' "${BR2_CONFIG}"; then
  KERNEL_IMAGE="Image"
else
  KERNEL_IMAGE="bzImage"
fi

printf "Converting RAW image to QCOW2... "
qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on "${BINARIES_DIR}/rootfs.ext2" "${BINARIES_DIR}/disk.qcow2"
rm -rf "${BINARIES_DIR}/rootfs.ext2"
echo "done."

if [ -f "${BINARIES_DIR}/${KERNEL_IMAGE}" ]; then
  echo "Symlinking Linux kernel image..."
  [ -e "${BINARIES_DIR}/vmlinuz" ] && rm "${BINARIES_DIR}/vmlinuz"
  ln -s "${KERNEL_IMAGE}" "${BINARIES_DIR}/vmlinuz"
fi
rm -rf "${BINARIES_DIR}/vmlinux"

echo "Build completed."
