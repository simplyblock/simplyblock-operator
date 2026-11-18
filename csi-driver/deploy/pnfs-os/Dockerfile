# The published pNFS guest image, built from the context <arch>/{vmlinuz,disk.qcow2}
# that `make -C csi-driver pnfs-os-<arch>` writes for each architecture. It is
# the MDS image's GUEST_IMAGE (deploy/image/Dockerfile.mds), which copies both
# files out of it, so this image is never run. Only COPY, so buildx builds
# every platform without emulation.
FROM scratch
ARG TARGETARCH
COPY ${TARGETARCH}/vmlinuz /vmlinuz
COPY ${TARGETARCH}/disk.qcow2 /disk.qcow2
