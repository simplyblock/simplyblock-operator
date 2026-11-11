# pNFS MDS guest

Root filesystem and kernel of the pNFS metadata server guest described in
`design-pnfs-mds-vm.md`. The guest is started by `mds-runner` in the MDS pod and
serves exports assembled by `mds-agent`. Built for `pnfs_qemu_x64_defconfig` and
`pnfs_qemu_arm64_defconfig`.

## Runner contract

What the runner has to provide when it starts QEMU.

### Devices

| Device                 | QEMU                                                                       | Guest                                               |
|------------------------|----------------------------------------------------------------------------|-----------------------------------------------------|
| Root disk (first disk) | `disk.qcow2` from the image as virtio-blk with `readonly=on`              | `/dev/vda`, mounted read-only                       |
| Network                | one `virtio-net-pci` on the runner's tap                                   | `eth0`                                              |
| Console                | `virtconsole` on stdio                                                     | `hvc0`                                              |

There is no state disk. The client recovery database lives on a simplyblock
volume that `mds-agent` connects, fences, and mounts at `/var/lib/nfs` when the
operator promotes the guest, and the guest boots as a standby: `mds-agent` runs,
and `nfs-server.service`, `nfs-mountd.service`, and `nfsdcld.service` do not start.
Each carries a drop-in conditioning it on `/run/mds/active`, which `mds-agent`
creates when the promotion reaches that step.

### Kernel command line

```
root=/dev/vda ro console=hvc0 panic=-1 ip=<guest>::<gateway>:<netmask>:<hostname>:eth0:off
nfsd.pnfs_probe_name=<probe file name> nfsd.pnfs_layout_hold=<seconds>
```

arm64 additionally needs `acpi=on`.

- `ip=` is configured by the kernel before init runs. There is no DHCP client
  and no network manager in the guest.
- `<hostname>` must be the same on every guest of a storage cluster (the
  StatefulSet's name) and stable across restarts: nfsd derives its server owner
  and scope from it, and NFSv4.1 clients only reclaim state from a server whose
  identity did not change, so a client of one guest reclaims on another.
- `init=` is not needed, the default `/sbin/init` is systemd.
- `nfsd.pnfs_probe_name` and `nfsd.pnfs_layout_hold` turn on the layout hold
  of the guest's nfsd patch (see Kernel). The name must be the one the CSI
  node writes its layout probe under.

### Architecture specifics

| Architecture | QEMU binary            | Machine / firmware                         | Kernel image |
|--------------|------------------------|--------------------------------------------|--------------|
| amd64        | `qemu-system-x86_64`   | `-machine q35`                             | `bzImage`    |
| arm64        | `qemu-system-aarch64`  | `-machine virt -bios <EDK2 QEMU_EFI.fd>`   | `Image`      |

Both boot the kernel directly with `-kernel`. On arm64 the UEFI firmware is only
there for ACPI and loads the kernel through its EFI stub.

### Shutdown

`system_powerdown` over QMP. systemd-logind handles the ACPI power button and
powers the guest off. `panic=-1` together with QEMU's `-no-reboot` makes a
guest panic end the QEMU process.

## Packaging

The guest is a `scratch` image holding only the files below. `post-image.sh`
builds it locally as `pnfs-guest:<arch>` when the target is the host's own
architecture, and CI publishes every architecture with buildx from
`scripts/Dockerfile.publish`:

| Path          | Content                                           |
|---------------|---------------------------------------------------|
| `/vmlinuz`    | the kernel (`bzImage` on amd64, `Image` on arm64) |
| `/disk.qcow2` | the read-only root filesystem                     |

Published as one multi-architecture `pnfs-guest` image, it is the
`GUEST_IMAGE` of the MDS image (`csi-driver/deploy/image/Dockerfile.mds` in
simplyblock-operator), which copies both to `/mds/kernel/vmlinuz` and
`/mds/disk.qcow2` next to QEMU and `mds-runner`. The arm64 firmware is not part
of the guest: the MDS image installs Alpine's `aavmf` and the runner uses
`/usr/share/AAVMF/QEMU_EFI.fd`.

## Kernel

The guest runs Linux 6.18, the newest long-term series. The pNFS SCSI layout
depends on the NVMe-oF and NFSv4.1 server code, and both change a lot between
releases.

| File                                     | Content                                                              |
|------------------------------------------|----------------------------------------------------------------------|
| `pnfs-qemu-x64/linux.config`             | x86 base config, vela's config carried to 6.18 (savedefconfig)       |
| `pnfs-qemu-arm64/linux.config`           | arm64 base config (savedefconfig)                                    |
| `pnfs-common/linux-pnfs.fragment`        | nfsd, layouts, NVMe-oF/TCP, and the guest contract, on top of either |
| `pnfs-common/patches/linux/linux.hash`   | the tarball's sha256                                                 |
| `pnfs-common/patches/linux/00*.patch`    | the guest's own patches and the upstream backports below             |

The toolchain's kernel headers stay in the 6.12 series (`BR2_KERNEL_HEADERS_6_12`)
rather than following the kernel. The kernel does not build against them, only
userspace does, and headers older than the running kernel are always safe. It
also keeps the prebuilt SDK usable: the archive is named by target and build
host only, so vela and the guest share it, and vela still runs 6.12.

The guest does not share vela's patch directory, because Buildroot applies
every patch in it, and needs none of vela's patches:

- `0001` to `0004` fix x86 page-table setup during memory hot-add. The runner
  starts QEMU with a fixed `-m` and no hotplug slots, so that code never runs.
- `0005` is Neon's kcompactd debug logging. It prints a line on every kcompactd
  wake, several a second, and the guest console carries each into the MDS pod
  log.

It carries six patches of its own ahead of the backports. The first,
`0001-nfsd-hold-layouts-until-the-client-has-probed-the-filesystem.patch`: a
pNFS client opens the device a SCSI layout names in the mount namespace of the
task that asked for the layout, and an application pod's `/dev` has no
`disk/by-id`. The CSI node makes the first lookup itself, from the host's
`/dev`, and the client caches the device. A server restart drops that cache,
and if the application asks first, the lookup fails and its I/O goes through
the metadata server from then on. With both parameters set, nfsd answers a
client's layout requests with `NFS4ERR_LAYOUTTRYLATER` until the client has
taken a layout on the probe file of that filesystem, during the grace period
and for `pnfs_layout_hold` seconds after the client's first request past it on
that filesystem. A grown filesystem hands out layouts under a new device ID
generation, which the client looks up again, so the hold is per generation: a
probe counts only if its layout carries the filesystem's current generation, and
a new generation holds the client on that filesystem again until the CSI node
has probed once more. The client then caches the device with its new size.
A grow can raise the generation while a layout's blocks are being mapped, so the
hold is checked again under the generation the layout carries, and a layout the
client is not primed for is not handed out. Without that second check the
client is given a device it cannot look up, returns its layouts without
committing what it wrote under them, and its next write through the metadata
server zeroes that data. Loopback clients are not held.

The second,
`0002-nfsd-fence-SCSI-registrations-left-by-an-earlier-server-instance.patch`:
a client registers the NVMe reservation key nfsd gives it, and the key carries
the server's boot time. On releasing the device the client's unregister becomes
a Replace with a zero key on NVMe, so its registration outlives the server, and
after a restart the client cannot register its new key: the device is marked
unavailable and its I/O goes through the metadata server. When nfsd reserves the
device for a client, it now preempts every registered key that is neither its
own nor from the current boot. SPDK's target does not implement Preempt and
Abort and answers it with Invalid Field, which also defeated nfsd's fencing of
a client whose recall timed out, so both fall back to a plain Preempt.

The third,
`0003-nfsd-ask-clients-to-retry-a-file-handle-of-an-export-not-back-yet.patch`:
nfsd starts, and grace begins, before the agent has exported every filesystem
again, and a client reclaiming an open on an export not yet back was answered
STALE, which drops the open and fails the application's I/O with EBADF. During
grace an unknown export is now answered NFS4ERR_DELAY, which clients retry.

The fourth,
`0004-nfsd-never-wait-for-a-layout-break-with-an-nfsd-thread-held.patch`: a
write through the metadata server, a size change, and an fallocate make XFS
break the layouts other clients hold on the file, and XFS waits for them with
the nfsd thread held. Clients give layouts back with LAYOUTRETURN, which needs
a free nfsd thread, so once every thread waited in `__break_lease` nfsd served
nothing until each recall timed out and its client was fenced. nfsd now starts
the break without waiting and answers NFS4ERR_DELAY while layouts are out, as
it already does for delegations.

The fifth,
`0005-xfs-optionally-hand-out-pNFS-write-layouts-over-zeroed-written-blocks.patch`:
XFS hands out a write layout over unwritten blocks, and only the client's
LAYOUTCOMMIT converts them to written. The Linux client never resends a commit
a restarted server lost, so data written between a client's last commit and a
server restart reads back as zeros on every other client. With
`xfs.pnfs_zeroed_layouts=1` on the kernel command line, which the runner sets
unless started with `--zeroed-layouts=false`, those blocks are zeroed and
written at allocation, at the cost of a WRITE ZEROES per allocation. The kernel
parameter itself defaults to off, and can be changed at runtime through
`/sys/module/xfs/parameters/pnfs_zeroed_layouts`. The file size still travels
only in LAYOUTCOMMIT.

There is no `0006`. It kept a SCSI layout's device ID at generation 0 across a
grow, so clients never looked the device up again in an application's mount
namespace, but they also kept the device size from before the grow: every
extent in the new space fell outside the device they knew, and they wrote
through the metadata server. The first patch's per-generation hold and the
twenty-first patch replace it.

The seventh,
`0007-nfsd-answer-an-unknown-export-with-a-retry-for-a-while-after-starting.patch`:
the third patch answers a request for an export that is not exported yet with
NFS4ERR_DELAY only during grace, and grace ends as soon as every client has
reclaimed. After a restart that took 12.5 seconds, while the exports came back
one at a time, and a request for an export restored after that got
NFS4ERR_STALE: ESTALE for the application, or EIO on a final write-back. With
`nfsd.pnfs_export_restore` seconds on the kernel command line, which the runner
sets, the retry is answered for that long after nfsd starts, whether or not
grace has ended. It defaults to 0.

The patches named `00NN-backport-<version>-<subject>.patch` are fixes from a
later mainline kernel that 6.18.y has not taken, `<version>` being the first
release that carries one. Each keeps its upstream author and trailers, names
the upstream commit on its first line, and says in a bracketed note where the
6.18 version differs from it. `0008` stops XFS from handing out a layout built
from an uninitialized mapping when reading the block map fails. `0009` and
`0010` keep grace from ending while clients still reclaim and keep the nfsdcld
reclaim table to one record per client. `0011` to `0017` are NVMe host fixes
for namespace-head and controller teardown and for parsing the ANA log.

Eight more patches of its own follow the backports. The eighteenth,
`0018-nfsd-fence-an-export-s-stale-SCSI-registrations-when-the-export-is-set-up.patch`:
the second patch fences an earlier boot's client keys only when some client asks
for the device, so a client cut off from the server across its restart stayed
registered and kept writing the device with its old layout while the new server
let another client truncate the file. The device is now claimed and its earlier
boots' keys fenced when the export is set up, once per boot of nfsd, before the
export answers any request.

The nineteenth,
`0019-nfsd-fence-a-client-before-dropping-layouts-it-never-returned.patch`:
nfsd fenced a client that did not return a recalled layout only from the
recall's done callback. A recall given up because the client's callback channel
was down or its lease had expired, and an expiring client, had their layouts
freed without a fence, and the client could keep writing through them. The
client is now fenced on the layout's device before either path drops a layout it
still holds, unless it answered the recall or was fenced already.

The twentieth,
`0020-xfs-bound-the-allocation-a-zeroed-pNFS-write-layout-zeroes-at-once.patch`:
with zeroed layouts, LAYOUTGET zeroes its whole allocation in the nfsd thread,
which held a thread for 15 seconds on a 1 GiB file. `xfs.pnfs_zeroed_layout_max_mb`
caps the allocation one LAYOUTGET zeroes, 64 MiB by default and 0 for no limit;
the client asks again for the rest of its range.

The twenty-first,
`0021-xfs-raise-the-layout-generation-before-a-grow-commits-new-space.patch`:
`xfs_growfs` raised the generation only after the new space was committed, so a
write layout in between could carry blocks past the old end of the device under
the old generation, which the client refused. A grow now raises the generation
before the new space can be allocated; the device already has its new size by
then, since the grow reads its new last block first.

The twenty-second,
`0022-nfsd-never-wait-for-a-layout-break-with-an-nfsd-thread-held-in-copy.patch`:
a server-side `COPY` into a file that still carries another client's write
layout breaks that layout the same way a write does, and the fourth patch's
fix did not reach it: `nfsd_copy_file_range` has the identical hazard as
`nfsd_setattr`, `nfsd4_vfs_fallocate`, and `nfsd_vfs_write`, but was not one of
the three call sites that patch touched. A synchronous `COPY` merely
reproduces the bug the fourth patch fixed, bounded by nfsd's own layout-recall
timeout. An asynchronous one does not: it runs in its own kthread rather than
an nfsd thread, so nothing times it out, and the kthread never reaches the
point where it would send `CB_OFFLOAD`, so the client's wait for one never
ends. Seen as an S3 `CompleteMultipartUpload` that never returned: the
gateway's join of two uploaded parts is a `copy_file_range` against the
destination object's file, which still carried the write layout the
gateway's own preceding writes had taken out on it. `nfsd_copy_file_range`
now starts the break without waiting, the same as the fourth patch's other
three sites, and returns the error to the caller instead of blocking;
`nfsd4_do_copy` already turns a negative return into an NFS4 error for both
the synchronous and the asynchronous caller, so the synchronous RPC answers
with it directly and the asynchronous kthread carries it into
`nfsd4_send_cb_offload`.

The twenty-third,
`0023-nfsd-send-CB_OFFLOAD-with-the-async-copy-s-own-callback-stateid.patch`:
an asynchronous `COPY` answers the client with a callback stateid and reports
completion later in `CB_OFFLOAD`, which the client matches against that stateid
by `memcmp`. nfsd sent zeros instead, so the match never succeeded and the
client's waiter was never completed. `nfsd4_copy` duplicates the copy fields
before `nfs4_init_copy_state` creates the stateid, so the durable `async_copy`
carries a `cp_res` whose `cb_stateid` is still zero; the memcpy that follows
writes the real one only into the reply's copy. `nfsd4_send_cb_offload` takes
`co_res` from the `async_copy`, and `encode_cb_offload4args` puts
`co_res.cb_stateid` on the wire. The ordering itself is deliberate — upstream
`d0beaee498e1` moved the dup ahead of the stateid so the `s2s_cp_stateids` IDR
is not registered into per-request memory the next request reuses — so this
fills in `async_copy->cp_res.cb_stateid` once the stateid exists rather than
moving the dup back. Found as the twenty-second patch's `CompleteMultipartUpload`
still never returning: the copy succeeds and `CB_OFFLOAD` is delivered and
acknowledged, but one trace of it shows the reply carrying
`cb_stateid=1:0xd062d9e9` and the callback carrying `cb_stateid=0:0x7bd5c66f`
for the same file handle. Mainline never needs it: the async COPY rework,
`f307b4f7ed17`, allocates the stateid up front and warns in a comment that
duplicating first would leave the callback stateid zeroed. That rework was
judged too risky to backport, so 6.18.y took the same targeted two-line fix in
**6.18.58**, as `22732b0f290a`. **Drop this patch when the kernel moves to
6.18.58 or later** — it is byte-identical to what stable already carries from
that release on, and will fail to apply.

The twenty-fourth,
`0024-xfs-skip-zeroing-free-space-a-pNFS-write-layout-gets.patch`: with zeroed
layouts, space a volume freed and reuses is zeroed for real at LAYOUTGET, since
XFS cannot tell it from space it never used, and one LAYOUTGET on a volume
rewritten for an hour spent 15 seconds in `blkdev_issue_zeroout`. With
`xfs.pnfs_discarded_free_space=1` on the kernel command line, a filesystem
mounted with `-o discard` on a device that announces that discarded blocks read
as zeros (an NVMe namespace whose DLFEAT says so) discards every extent it
frees, including the unwritten ones XFS otherwise frees without a discard, and
zeroes the extents of a discard that fails before releasing them. Once a FITRIM
of the whole filesystem has completed, a zeroed write layout over a hole is
allocated written without the WRITE ZEROES, and without the twentieth patch's
cap. An unwritten extent under a write layout is still zeroed. The parameter
defaults to off and is read only at mount, and the dmesg line `free space reads
as zeros` says when a mount has stopped zeroing.

The twenty-fifth,
`0025-nfsd-let-a-metadata-server-instance-register-under-a-key-of-its-own.patch`:
every guest registered the same constant reservation key, so two guests of one
storage cluster could not tell each other's registration from their own.
`nfsd.pnfs_mds_pr_key` is a `u64` the runner's agent writes before it starts
nfsd, and it is `0`, the constant key and the behavior above, until then. When
set, nfsd registers and reserves nothing: it serves a device only while that key
holds the device's reservation, a device it does not hold is not exported, and
GETDEVICEINFO for it fails. The stale-key fence leaves every key whose top byte
is `0x01` alone, so a fenced instance never preempts a newer one. A write while
nfsd has threads running fails with `EBUSY`.

## Guest layout

| Path                            | Kind                  | Content                                                   |
|---------------------------------|-----------------------|-----------------------------------------------------------|
| `/`                             | read-only             | image                                                     |
| `/var`                          | tmpfs                 | populated from `/usr/share/factory/var` on boot           |
| `/var/lib/nfs`                  | state disk            | `nfsdcld/` client recovery database                       |
| `/var/lib/simplyblock/exports`  | tmpfs (under `/var`)  | export mount points                                       |
| `/etc/exports.d`                | → `/run/exports.d`    | export drop-ins, rebuilt by the operator after every boot |
| `/run/nfs`                      | tmpfs                 | nfs-utils state (`etab`, `rpc_pipefs`)                    |
| `/root/.ssh`                    | → `/run/root-ssh`     | debug SSH `authorized_keys`, written at boot from fw_cfg  |
| `/run/dropbear`                 | tmpfs                 | debug SSH host key, made fresh at each boot               |

## Services

- `nfs-server.service` with `nfs-mountd`, `nfs-idmapd` and `nfsdcld`. NFSv4.1 and
  4.2 only (`/etc/nfs.conf`). rpcbind, statd and blkmapd are masked.
- `mds-agent.service` starts `/usr/bin/mds-agent` once it is part of the image
  (skipped until then). It may run `EnsureNFSD` itself, which is idempotent
  against the already running server.
- journald forwards to the console, so the runner's pod log carries the
  guest's service logs.
- `debug-ssh.service` runs dropbear on port 22, root by key only (`-s`), and
  only with `simplyblock.debug_ssh=1` on the kernel command line, which the
  runner adds when started with `-debug-ssh`. Buildroot's own `dropbear.service`
  and `dropbear.socket` are masked, so nothing else starts it.
  `debug-ssh-key.service`, under the same condition, copies the runner's public
  key from the QEMU fw_cfg item `opt/io.simplyblock/ssh_authorized_keys` to
  `/run/root-ssh/authorized_keys`. The guest's address is reachable only from
  the runner, so this is a shell for whoever may exec into the runner
  container, and a guest started without the flag has no SSH at all.
