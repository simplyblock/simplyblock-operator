# Test Plan: The NodeStageVolume Format Decision

Covers the one decision the CSI node plugin can never get wrong: whether the
device it has just connected may be handed to `mkfs`. The behavior under test
lives in `csi-driver/pkg/spdk/nodeserver.go` (`formatAndMount`) and
`atlas-lib/blockfs` (the probe it decides on).

## Why this plan exists

The driver used to leave the decision to `k8s.io/mount-utils`, whose
`SafeFormatAndMount` probes with `blkid` and cannot tell a device carrying no
filesystem from one whose reads failed: `blkid` reports exit status 2 for both,
`mount-utils` resolves that to "unformatted," and `mkfs.ext4 -F -m0` runs. A
volume behind a degraded NVMe-oF path — reads timing out under
`nvme_core.io_timeout`, or a controller past its `ctrl_loss_tmo` — was therefore
reformatted rather than staged, and a production cluster lost a volume's data
that way. Upstream tracks the same defect, reached through a corrupted primary
superblock rather than an unreadable device, as
[kubernetes/kubernetes#140376](https://github.com/kubernetes/kubernetes/issues/140376),
open and untriaged.

The rule the plan pins: only a device that answered a read and proved to be all
zeros may be formatted. Every other outcome is either somebody's data or an
unanswered question, and staging fails rather than guessing.

## Coverage map

| Prefix | Level             | What it needs                                                          |
|--------|-------------------|------------------------------------------------------------------------|
| `PB-`  | Unit, probe       | nothing; a temporary file or an injected opener stands in for a device |
| `FD-`  | Unit, decision    | a fake mounter and a fake exec; **Linux**, see below                   |
| `LV-`  | Live              | a node with a staged simplyblock volume, run by hand                   |
| `FI-`  | Failure injection | a host with `dm-flakey`, run by hand                                   |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test`
column names the implementing function, or `—` when nothing covers it yet.

**The `FD-` rows only exercise anything on Linux.** `mount-utils` implements the
format decision in `mount_linux.go`; on every other platform
`FormatAndMountSensitive` mounts without probing, so these cases pass vacuously
on macOS. Run them on a Linux host, or in CI, before trusting them:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o /tmp/spdk.test ./pkg/spdk/
# then, on a Linux host
/tmp/spdk.test -test.run TestStageVolume -test.v
```

---

## 1. The probe (`atlas-lib/blockfs`)

| ID    | Scenario                                                                           | Type       | Test                                                    |
|-------|------------------------------------------------------------------------------------|------------|---------------------------------------------------------|
| PB-01 | An ext2/3/4 superblock is reported as a formatted device                           | Positive   | `TestProbeClassifiesDeviceContent`                      |
| PB-02 | An XFS superblock is reported as a formatted device                                | Positive   | `TestProbeClassifiesDeviceContent`                      |
| PB-03 | A Btrfs superblock, at 64 KiB, is inside the probe window                          | Boundary   | `TestProbeClassifiesDeviceContent`                      |
| PB-04 | LUKS, LVM2, swap, and partition-table signatures are foreign data, not filesystems | Negative   | `TestProbeClassifiesDeviceContent`                      |
| PB-05 | An LVM2 label in any of the first four sectors is found                            | Boundary   | `TestProbeClassifiesDeviceContent`                      |
| PB-06 | An all-zero device is blank, the only state that permits a format                  | Positive   | `TestProbeClassifiesDeviceContent`                      |
| PB-07 | Readable, non-zero, unrecognized content is `Unknown`, not blank                   | Negative   | `TestProbeClassifiesDeviceContent`                      |
| PB-08 | A device shorter than the probe window is still classified                         | Boundary   | `TestProbeClassifiesDeviceContent`                      |
| PB-09 | A device whose reads fail with EIO is `Unreadable`, never blank                    | Regression | `TestProbeReportsUnreadableRatherThanBlank`             |
| PB-10 | A device that cannot be opened at all is `Unreadable`                              | Regression | `TestProbeReportsUnreadableWhenTheDeviceCannotBeOpened` |
| PB-11 | A device answering zero bytes is `Unreadable`, not blank                           | Boundary   | `TestProbeReportsUnreadableWhenTheDeviceReturnsNothing` |
| PB-12 | A read that never returns gives up at the timeout, below `nvme_core.io_timeout`    | Negative   | `TestProbeTimesOutRatherThanBlocking`                   |
| PB-13 | Caller cancellation ends the probe                                                 | Negative   | `TestProbeHonorsCallerCancellation`                     |
| PB-14 | A signature in a partially read device is trusted: the data is there regardless    | Boundary   | `TestProbeTrustsASignatureFoundInAPartialRead`          |
| PB-15 | Zeros from a partial read do **not** conclude blank                                | Regression | `TestProbeDoesNotConcludeBlankFromAPartialRead`         |
| PB-16 | Every probe closes its device, so one stage per volume leaks no descriptor         | Positive   | `TestProbeClosesTheDevice`                              |

## 2. The decision (`csi-driver/pkg/spdk`)

| ID    | Scenario                                                                                           | Type       | Test                                                               |
|-------|----------------------------------------------------------------------------------------------------|------------|--------------------------------------------------------------------|
| FD-01 | A device holding a filesystem is mounted, never formatted, even when `blkid` reports nothing       | Regression | `TestStageVolumeNeverFormatsADeviceHoldingAFilesystem`             |
| FD-02 | A device that cannot be read fails the stage instead of being formatted                            | Regression | `TestStageVolumeRefusesADeviceItCannotRead`                        |
| FD-03 | A blank device is still formatted, so first-time provisioning does not regress                     | Positive   | `TestStageVolumeFormatsABlankDevice`                               |
| FD-04 | A device carrying a foreign signature fails the stage                                              | Negative   | `TestStageVolumeRefusesADeviceHoldingForeignData`                  |
| FD-05 | A filesystem that does not match the requested `fsType` is mounted with a warning, not reformatted | Boundary   | `TestStageVolumeMountsAMismatchedFilesystemRatherThanReformatting` |
| FD-06 | A raw block volume skips the decision entirely                                                     | Positive   | `TestStageVolumeSkipsRawBlockVolumes`                              |

## 3. Live, run by hand

Needs a node with a staged simplyblock volume. `SBTEST_DEVICE` names the device
of a volume that holds data, `SBTEST_BLANK_DEVICE` that of a raw-block volume
the driver has never formatted:

```bash
SBTEST_DEVICE=/dev/nvme0n1 SBTEST_EXPECT_FILE=precious.txt \
  SBTEST_BLANK_DEVICE=/dev/nvme2n1 \
  ./spdk.test -test.run TestLive -test.v
```

| ID    | Scenario                                                                                           | Type       | Test                                            |
|-------|----------------------------------------------------------------------------------------------------|------------|-------------------------------------------------|
| LV-01 | A real volume holding data is staged with its data intact while `blkid` reports nothing            | Regression | `TestLiveStageVolumeDoesNotReformatARealVolume` |
| LV-02 | `mount-utils` still chooses `mkfs` for that same device, which is why the driver does not delegate | Regression | `TestLiveUpstreamFormatDecisionOnARealVolume`   |
| LV-03 | A freshly provisioned lvol reads as blank, so it will still be formatted                           | Positive   | `TestLiveProbeSeesAFreshVolumeAsBlank`          |

Verified on a four-node K3s cluster (Rocky 9.5, kernel 5.14) on 2026-09-03.
LV-01 issued no commands at all, and the volume's marker file and its 64 MiB
payload both survived. LV-02 chose `mkfs.ext4 -F -m0 /dev/nvme0n1` against that
same device. LV-03 reported `Blank`.

## 4. Failure injection, run by hand

Not automated: no Go test can make a device fail reads on demand. Two recipes
follow. The loop device is the quicker one and needs no cluster; the NVMe-oF one
runs the whole failure against a real simplyblock volume and is what establishes
that this is reachable in production rather than only in a model.

A degraded NVMe-oF path presents in three different ways, and only one of them
destroys anything, so a reproduction has to say which it is reaching:

| The paths are                         | The device node | I/O       | `blkid` | Consequence                                       |
|---------------------------------------|-----------------|-----------|---------|---------------------------------------------------|
| down, within `ctrl_loss_tmo`          | present         | queues    | blocks  | staging hangs, kubelet retries; nothing formatted |
| down, past `ctrl_loss_tmo`            | **gone**        | n/a       | exit 2  | mkfs hits a missing path and fails                |
| down, with `fast_io_fail_tmo` reached | **present**     | **fails** | exit 2  | **mkfs runs against the volume**                  |

The third is the production case. Reaching it needs `fast_io_fail_tmo`, which
tells the driver to fail I/O quickly while the controller keeps reconnecting, so
the namespace stays present and unreadable rather than queueing or vanishing.
`CONFIG_FAULT_INJECTION` is not required and is absent from the kernels this
product runs on.

| ID    | Scenario                                                                                 | Type       | Test |
|-------|------------------------------------------------------------------------------------------|------------|------|
| FI-01 | `blkid` exits 2 with empty output on an ext4 device whose reads fail                     | Regression | —    |
| FI-02 | `mkfs` on such a device discards its blocks, then fails, leaving no valid filesystem     | Regression | —    |
| FI-03 | With reads recovered between probe and `mkfs`, the volume is silently reformatted empty  | Regression | —    |
| FI-04 | A volume whose reads fail during a cold stage is refused rather than formatted           | Regression | —    |
| FI-05 | On a real NVMe-oF volume, `fast_io_fail_tmo` yields exit 2 with the device still present | Regression | —    |
| FI-06 | That volume is then reformatted once the path recovers, and its data is gone             | Regression | —    |

Reproduction for FI-01 through FI-03, on any host with `dm-flakey`. It runs
against a loop device, never a real volume:

```bash
dd if=/dev/zero of=disk.img bs=1M count=256
LOOP=$(losetup --find --show disk.img)
mkfs.ext4 -q -F "$LOOP"
mount "$LOOP" mnt && echo data > mnt/precious.txt && sync && umount mnt

modprobe dm-flakey
dmsetup create sbrepro --table "0 $(blockdev --getsz "$LOOP") linear $LOOP 0"
blkid -p -s TYPE -s PTTYPE -o export /dev/mapper/sbrepro    # TYPE=ext4, exit 0

# Reads fail, writes still land: what a degraded NVMe-oF path does.
dmsetup suspend sbrepro
dmsetup load sbrepro --table "0 $(blockdev --getsz "$LOOP") flakey $LOOP 0 0 60 1 error_reads"
dmsetup resume sbrepro
blkid -p -s TYPE -s PTTYPE -o export /dev/mapper/sbrepro    # empty, exit 2
```

### FI-05 and FI-06, on a real simplyblock volume

Needs a node with a simplyblock volume attached, and destroys that volume's
data, so run it against one provisioned for the purpose. `<A>` and `<B>` are the
storage nodes the volume's paths point at, from
`cat /sys/class/nvme/nvme*/address`.

```bash
DEV=/dev/nvme0n1
mkfs.ext4 -q -F $DEV                       # give the volume a filesystem
mount $DEV /mnt && echo data > /mnt/precious.txt && sync && umount /mnt
blkid -p -s UUID -o value $DEV             # note the UUID

# Fail I/O quickly while the controller keeps reconnecting, so the namespace
# stays present rather than queueing I/O or being torn down.
for c in /sys/class/nvme/nvme0 /sys/class/nvme/nvme1; do
    echo 3600 > $c/ctrl_loss_tmo
    echo 5    > $c/fast_io_fail_tmo
done

# Sever every path.
iptables -I OUTPUT -d <A> -p tcp --dport 4428 -j DROP
iptables -I OUTPUT -d <B> -p tcp --dport 4428 -j DROP
sleep 25
echo 3 > /proc/sys/vm/drop_caches          # or the probe is answered from cache

blkid -p -s TYPE -s PTTYPE -o export $DEV  # FI-05: exit 2, no output, device present
```

Then let the paths recover and run what the driver would run next:

```bash
iptables -D OUTPUT -d <A> -p tcp --dport 4428 -j DROP
iptables -D OUTPUT -d <B> -p tcp --dport 4428 -j DROP
mkfs.ext4 -F -m0 $DEV                      # FI-06: exits 0
blkid -p -s UUID -o value $DEV             # a different UUID: the volume is gone
```

Verified on 2026-09-03 against a four-node cluster, kernel 5.14, on a 2 GiB
volume with two live HA paths. The probe returned exit 2 with empty output while
`/dev/nvme0n1` was present and its ext4 intact, `mkfs` then exited 0, and the
volume came back with a new UUID holding nothing but `lost+found`. Dropping the
page cache matters: without it the probe is answered from cache and returns exit
0 even with every path severed.

Verified on 2026-09-03 with util-linux 2.40.2 and dm-flakey v1.5.0. The probe
returned exit 2 with empty output, and `mkfs.ext4 -F -m0` then destroyed the
filesystem in both the still-failing and the recovered case.
