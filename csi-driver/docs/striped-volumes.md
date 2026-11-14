## Striped volumes

A striped volume is one PersistentVolume made of several simplyblock logical
volumes of equal size, each on a different storage node of one cluster. The
node that stages it attaches every member and stripes them into one block device
with LVM (dm-striped), so I/O to the volume is spread across the members' nodes
instead of being served by one.

Striping is opted into per StorageClass and is off by default. A class that does
not set `stripe_width`, or sets it to `0` or `1`, provisions exactly as before.

### Turning it on

**Through a StoragePool.** The operator writes the class of a pool it manages from
the pool's `spec.volumeDefaults`, and `stripeWidth` and `stripeSize` there become
`stripe_width` and `stripe_size`:

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StoragePool
metadata:
  name: fast
spec:
  clusterRef: production
  volumeDefaults:
    stripeWidth: 4
    stripeSize: 64Ki
```

The CRD refuses a `stripeWidth` above 128, a `stripeSize` that is not a power of
two from 4Ki to 1Mi or that is set without a `stripeWidth` of 2 or more, and a
striped pool with client-side compression or deduplication or with replication.
The StoragePool admission webhook refuses a striped pool on a stretched cluster,
and one wider than the cluster's storage nodes (or, on a supercluster, than any
member that takes new volumes) once the cluster has nodes to count.
`volumeDefaults` is immutable, so a pool's width is fixed when it is created.

**Through the Helm chart.** For a cluster given in `csiConfig.simplybk` or
`multiCluster.clusters`, which has no StoragePool, the chart's class takes
`stripeWidth` and `stripeSize` beside `pool`, with the same bounds.

**By hand:**

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simplyblock-striped
provisioner: csi.simplyblock.io
parameters:
  cluster_id: 8ffac363-0c46-4714-a71b-f9c0b58a1269
  pool_name: pool-1
  csi.storage.k8s.io/fstype: xfs
  # Four members, each on its own storage node.
  stripe_width: "4"
  # Optional: the chunk each member takes before the next one does.
  # A power of two from 4Ki to 1Mi; 64Ki when absent.
  stripe_size: "64Ki"
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
```

| Parameter      | Meaning                                                                                       |
|----------------|-----------------------------------------------------------------------------------------------|
| `stripe_width` | Number of members, 2 to 128 (LVM's limit). Absent, `0`, or `1` is an ordinary volume.         |
| `stripe_size`  | Chunk size, a Kubernetes quantity (`64Ki`, `128Ki`, `1Mi`, or bytes). Read only when striped. |

The width cannot be larger than the number of online storage nodes in the
cluster the class provisions into. A claim asking for more is refused with
`InvalidArgument` before anything is created. Members never span clusters: a
class naming a supercluster has the operator choose one member cluster, and
every member of the volume is created there.

### How it works

**Members and identity.** The control plane has no notion of a striped volume.
The members are held together by the first member's identity and a naming rule:
member 0 carries the claim's volume name (`pvc-<uid>`) exactly as an ordinary
volume does, and member *i* carries `pvc-<uid>-s<i>`. A consistency group is not
used for this, because a consistency group is confined to one storage node's
volume store and the members are on different nodes by construction.

**Volume ID.** CSI caps a volume ID at 128 bytes, and the members' UUIDs do not
fit. The ID is the first member's handle with the width appended as a fourth
segment:

```
{clusterUUID}:{poolUUID}:{member0UUID}:s{width}     e.g. …:a1111111-…-111111111111:s4
```

Every three-segment ID parses exactly as before. The width is in the ID because
`DeleteVolume`, `ControllerExpandVolume`, and `CreateSnapshot` receive nothing
but the ID, and the marker tells them the volume is striped without a lookup.
The other members are found from the first one's name. `lvol.ParseHandle` in
atlas-lib still refuses the marker, so a consumer that does not know about
striping cannot mistake a striped volume for its first member.
`lvol.ParseStripedHandle` reads both forms.

**Snapshot ID.** The same scheme: `{cluster}:{pool}:{snapshot0UUID}:s{width}`.
Member snapshot *i* is named `<snapshot name>-s<i>`. `ListSnapshots` reports a
striped snapshot once, under the first member's ID with the marker, at the size
of all members, and does not list the other members' snapshots on their own.

**Volume context.** The controller writes `stripeWidth`, `stripeMembers` (the
member UUIDs in stripe order), and `stripeChunkBytes` into the volume context.
The member order is the stripe: the same members in another order are a
different device, so the order is fixed at creation and never rewritten.

**Placement.** Each member is created with `host_id` set to a different online
storage node. The first member honors an explicit placement (the
`selected-storage-node`, `placement-hint`, or `host-id` annotation, or pod
affinity). The rest are chosen deterministically: round robin across failure
domains, starting at a hash of the volume name, so a retried create picks the
same nodes and consecutive members land in different failure domains where the
cluster has them.

**Sizing.** Each member is `ceil(capacity / width)` rounded up to a whole GiB,
and the volume reports `width × member size`. LVM keeps a few MiB of each member
for its own metadata.

**QoS.** The class's ceilings are divided evenly across the members (rounded
up), so the volume as a whole is held to the class's numbers. The volume context
records the class's numbers.

**XFS geometry.** The node's filesystem layer formats XFS with the striped
logical volume's own geometry (`-d su=<stripe_size>,sw=<stripe_width>`). The
class's `xfs_su`/`xfs_sw` and the driver's default stripe hints are not used for
a striped volume: mkfs.xfs refuses a command line that states the stripe unit
twice.

### Operations

| RPC                                   | What it does for a striped volume                                                                                                                                                                     |
|---------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `CreateVolume`                        | Creates every member on its own node. A retry finds members by name and creates only the missing ones, replacing a member that is not online. If a member cannot be created, every member is deleted. |
| `DeleteVolume`                        | Deletes every member, the first one last, so an interrupted delete is retried with the first member still there to find the rest. Members already gone are skipped.                                   |
| `ControllerExpandVolume`              | Resizes every member to `ceil(new size / width)`, skipping members already there. Node expansion is required.                                                                                         |
| `NodeExpandVolume`                    | `pvresize` of every member, `lvextend -l+100%FREE` of the striped logical volume (which keeps its stripes), then the filesystem grow.                                                                 |
| `CreateSnapshot`                      | Freezes the volume on its node, snapshots every member concurrently, and thaws (see below). Anything short of a freeze that held for every member deletes the member snapshots and is retried.        |
| `DeleteSnapshot`                      | Deletes every member snapshot, the first one last.                                                                                                                                                    |
| `CreateVolume` from snapshot / volume | Clones member *i* into member *i*, a live source volume under its freeze. The source must have the same width as the class. A clone lands on its source's node, so members stay on distinct nodes.    |
| `NodeStageVolume`                     | Attaches every member (`members` → `lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume` → `filesystem`). The raw block variant stops at the logical volume.                                    |
| `NodeUnstageVolume`                   | Releases the stack recorded at stage time, with the members in the recorded order.                                                                                                                    |

The node asks the control plane for each member's connection at stage, heal,
publish, and expand. When the control plane does not answer, the members in the
stack record are used to find namespaces that are already attached. The
path-loss watch is given the members' namespace devices, under the volume's ID,
rather than the striped device-mapper node.

### Crash-consistent snapshots: the freeze

The control plane snapshots each member on its own storage node, and has no
snapshot that is atomic across nodes: a consistency group is confined to one
node's volume store. The consistency comes from the host instead. For the length
of a snapshot, the node that stages the volume suspends the stripe's
device-mapper device (`dmsetup suspend` on the striped logical volume): in-flight
I/O drains, new I/O waits, and a mounted filesystem is frozen and flushed first
(dmsetup's default lockfs). Every member then holds the same moment, and the
member snapshots taken while it lasts are one crash-consistent image of the
volume. A raw block volume gets the same drain and hold, without a filesystem to
freeze.

The protocol, as `CreateSnapshot` runs it:

1. The controller finds where the volume is staged, from its VolumeAttachments.
   Attached nowhere, nothing writes to it, and the members are snapshotted
   without a freeze. Attached to more than one node is refused
   (`FailedPrecondition`).
2. `Freeze(node, volume, token, deadline)` goes to the operator over the CSI
   link (`atlas-lib/freeze/freezerpc`), and the operator relays it down the
   node's link. The controller has no link to a node, and both link to the
   operator. The token is a fresh UUID per attempt. The node suspends the device,
   holds the volume's lock (nothing stages, unstages, publishes, or grows it
   meanwhile), records the freeze on disk, and starts the deadline. A node that has
   the volume attached and not staged answers that it froze nothing, and the
   members are taken without a freeze.
3. The controller snapshots every member concurrently, under a context that ends
   at 80% of the deadline.
4. `Thaw(node, volume, token)`. The node resumes the device and answers whether
   it stayed suspended from `Freeze` until now.
5. Only a freeze that held for every member snapshot is a snapshot. A freeze that
   could not be taken (`Unavailable`), a member that failed, or a freeze the
   deadline ended first (`Aborted`) deletes the member snapshots, and the
   snapshotter retries.

**The deadline.** 10 seconds by default, set on the controller with
`SPDKCSI_STRIPE_FREEZE_DEADLINE` (a Go duration). The node resumes the device at
the deadline by itself, whether or not the controller, the operator, or the link
is still there, and caps any deadline at 30 seconds. The freeze is recorded under
the stack-record directory while it lasts, so a node plugin that restarts in the
middle resumes the device at start rather than leaving it suspended. In practice
the freeze lasts as long as the slowest member snapshot.

**Retries and tokens.** A `Freeze` repeated under its token is the same freeze. A
`Thaw` under another token leaves the freeze alone. Member snapshots an earlier
attempt left are deleted rather than reused, since they were not taken under this
freeze.

**Clones of a live volume** are taken the same way: the members are cloned
concurrently under the source's freeze. A restore from a striped snapshot needs
no freeze, because the snapshot is already consistent.

**Without the CSI link** (the operator's link is off, or down), a striped volume
that is attached somewhere is not snapshotted or cloned: the request fails with
`Unavailable` rather than producing a torn image. An unattached one is.

**While a volume is frozen**, LVM commands on its node that scan devices may wait
on the suspended device until it is resumed, for at most the deadline.

### Not supported

The following are refused for a striped volume, because each acts on one
logical volume where a striped volume has several and would silently act on the
first member alone:

| Feature                                                      | Refused with         | Where                                            |
|--------------------------------------------------------------|----------------------|--------------------------------------------------|
| pNFS (`csi.storage.k8s.io/fstype: pnfs`)                     | `InvalidArgument`    | `CreateVolume`                                   |
| Client-side compression or deduplication (VDO)               | `InvalidArgument`    | `CreateVolume`                                   |
| `replicate: "true"`                                          | `InvalidArgument`    | `CreateVolume`                                   |
| Consistency-group label on the claim                         | `InvalidArgument`    | `CreateVolume`                                   |
| A stretched (sync-replicated) cluster                        | `InvalidArgument`    | `CreateVolume`                                   |
| csi-addons replication                                       | `FailedPrecondition` | every replication RPC, destination info included |
| csi-addons volume groups                                     | `FailedPrecondition` | `CreateVolumeGroup`                              |
| `VolumeGroupSnapshot`                                        | `FailedPrecondition` | `CreateVolumeGroupSnapshot`                      |
| Restoring or cloning across widths, or striped into ordinary | `InvalidArgument`    | `CreateVolume` with a content source             |

Encryption, QoS, `max_namespace_per_subsys`, and placement annotations work:
each applies to every member on its own. The dm-linear indirection
(`SPDKCSI_DM_INDIRECTION`) does not apply to striped volumes, whose stack is
already device-mapper.

### Notes

- The node's LVM commands that name a device are scoped to it with
  `--devices`, and the others address the volume group by name. kubelet's loop
  device for a raw block volume sits on the published device, which for a
  striped volume is the striped logical volume and never a member, so it does
  not duplicate a member's physical-volume label.
- Striped pNFS exports (`operator/docs/designs/design-pnfs-striped.md`) use the
  same parameter, member naming, and handle marker. They differ on snapshots:
  that design refuses them for exports, because pNFS clients write to the
  members directly, and suspending the device on the metadata server does not
  stop them unless every client's layout is recalled first.
