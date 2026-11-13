## Striped volumes

A striped volume is one PersistentVolume made of several simplyblock logical
volumes of equal size, each on a different storage node of one cluster. The
node that stages it attaches every member and stripes them into one block device
with LVM (dm-striped), so I/O to the volume is spread across the members' nodes
instead of being served by one.

Striping is opted into per StorageClass and is off by default. A class that does
not set `stripe_width`, or sets it to `0` or `1`, provisions exactly as before.

### StorageClass

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

**XFS geometry.** When the class does not set `xfs_su` and `xfs_sw`, the
controller sets them to the stripe: `su = stripe_size`, `sw = stripe_width`.

### Operations

| RPC                                   | What it does for a striped volume                                                                                                                                                                     |
|---------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `CreateVolume`                        | Creates every member on its own node. A retry finds members by name and creates only the missing ones, replacing a member that is not online. If a member cannot be created, every member is deleted. |
| `DeleteVolume`                        | Deletes every member, the first one last, so an interrupted delete is retried with the first member still there to find the rest. Members already gone are skipped.                                   |
| `ControllerExpandVolume`              | Resizes every member to `ceil(new size / width)`, skipping members already there. Node expansion is required.                                                                                         |
| `NodeExpandVolume`                    | `pvresize` of every member, `lvextend -l+100%FREE` of the striped logical volume (which keeps its stripes), then the filesystem grow.                                                                 |
| `CreateSnapshot`                      | Snapshots every member concurrently. If any member fails, the member snapshots already taken are deleted. See the limitation below.                                                                   |
| `DeleteSnapshot`                      | Deletes every member snapshot, the first one last.                                                                                                                                                    |
| `CreateVolume` from snapshot / volume | Clones member *i* into member *i*. The source must have the same width as the class. A clone lands on its source's node, so the members stay on distinct nodes.                                       |
| `NodeStageVolume`                     | Attaches every member (`members` → `lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume` → `filesystem`). The raw block variant stops at the logical volume.                                    |
| `NodeUnstageVolume`                   | Releases the stack recorded at stage time, with the members in the recorded order.                                                                                                                    |

The node asks the control plane for each member's connection at stage, heal,
publish, and expand. When the control plane does not answer, the members in the
stack record are used to find namespaces that are already attached. The
path-loss watch is given the members' namespace devices, under the volume's ID,
rather than the striped device-mapper node.

### Snapshot limitation

**Snapshots of a striped volume are not atomic across its members.** Each member
snapshot is taken on its own storage node, and there is no point in time they
share: a consistency group, which would provide one, is confined to a single
node's volume store. The member snapshots are taken concurrently to keep the
window small, but a write that lands inside it reaches some members' snapshots
and not others, and a filesystem striped across them can be torn by that.

A crash-consistent snapshot of a striped volume requires the application to be
quiesced, or the filesystem frozen (`fsfreeze --freeze`), while the snapshot is
taken.

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
- `operator/docs/designs/design-pnfs-striped.md` plans a `stripe_count`
  parameter for striped pNFS exports. The two describe the same quantity and
  should share one name before either ships.
