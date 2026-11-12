# pnfs-os

The kernel and root filesystem of the pNFS metadata server guest, built with
buildroot as an external tree. The MDS image (`../image/Dockerfile.mds`) takes
the result as its `GUEST_IMAGE` and adds QEMU, `mds-runner`, and `mds-agent`.
What the guest contains and what the runner has to provide is described in
`boards/pnfs-common/README.md`.

## Origin

Imported from [simplyblock/vela-os](https://github.com/simplyblock/vela-os),
branch `pnfs-minimal`, commit `5040d7d7e1dc4d7cd9db72d760a024628c7e70da`, as a
squashed `git subtree`. The history of the kernel patches and the board
configuration up to the import is in that repository. The import commit is
followed by one that removes everything Vela-specific: the PostgreSQL packages,
the Vela board, the buildroot submodule and its patches, and the builder
scripts, which are replaced by `builder/`.

## Layout

| Path                                      | Content                                                                  |
|-------------------------------------------|--------------------------------------------------------------------------|
| `configs/pnfs_qemu_{x64,arm64}_defconfig` | the buildroot configuration of each architecture                         |
| `configs/*.fragment`                      | the prebuilt SDK and per-package directories, merged over a defconfig    |
| `boards/pnfs-common/`                     | kernel patches and fragment, root filesystem overlay, post-build scripts |
| `boards/pnfs-qemu-{x64,arm64}/`           | the base kernel configuration of each architecture                       |
| `builder/`                                | the build environment (pinned buildroot release) and `build.sh`          |
| `Dockerfile`                              | the published scratch image holding `/vmlinuz` and `/disk.qcow2`         |

## Building

```bash
make -C csi-driver pnfs-os-amd64        # build/pnfs-os/amd64/{vmlinuz,disk.qcow2}
make -C csi-driver pnfs-os-arm64        # build/pnfs-os/arm64/{vmlinuz,disk.qcow2}
make -C csi-driver pnfs-os-image PNFS_OS_BUILDX_FLAGS=--push
```

Both architectures are cross-compiled on an x86_64 host, which the prebuilt SDK
requires. `PNFS_OS_PREBUILT_SDK=0` builds the toolchain instead.

## Tags

The guest is tagged by content: `spdkcsi:pnfsos-<tree>`, where `<tree>` is the
git tree object of this directory (`make -C csi-driver pnfs-os-tag`). It
changes exactly when a file here changes, so CI builds the guest only for a
tree it has not published yet and every MDS image names the guest it embeds.
A build from a modified working tree is tagged `-dirty`.

| Tag                             | Content                                   |
|---------------------------------|-------------------------------------------|
| `pnfsos-<tree>-amd64`, `-arm64` | one architecture                          |
| `pnfsos-<tree>`                 | the multi-architecture manifest over both |
| `pnfsos-latest`                 | the manifest of the latest tree on `main` |
