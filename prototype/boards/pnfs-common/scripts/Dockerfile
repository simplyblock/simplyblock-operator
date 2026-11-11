# The pNFS MDS guest for one architecture: the kernel and the root filesystem,
# nothing else. The MDS image copies both out of it (simplyblock-operator,
# csi-driver/deploy/image/Dockerfile.mds), so this image is never run.
FROM scratch
ARG KERNEL_IMAGE
COPY ${KERNEL_IMAGE} /vmlinuz
COPY disk.qcow2 /disk.qcow2
