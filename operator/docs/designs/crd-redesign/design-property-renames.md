# Property Renames Across the CRD Redesign

The ten designs under this directory each carry a "Migration from the Registered
API" section, and between them they rename a substantial number of properties on
CRDs that are already registered and in use. Each design states its own renames
next to the kind they belong to, which is the right place to decide them and the
wrong place to execute them: the renames share one mechanism, one deprecation
window, and one set of upgrade risks, and doing them kind by kind means writing
that mechanism ten times.

This document is the collected inventory and the migration mechanism. It decides
nothing about the target shapes — every row here is already settled by the design
that owns it, and is cited to it. What it adds is the classification that says
how each row breaks, and therefore what each row's upgrade path has to do.

Every row was verified against `operator/api/v1alpha1` rather than taken from the
designs alone, because a design describes an intended end state and some of its
rows describe fields that were never registered.

---

## 1. How a Renamed Property Breaks

The classification matters more than the count, because the rows do not share a
failure mode and three of the four modes are silent.

**A renamed spec field is silently ignored.** The API server accepts an object
carrying the old name only if the CRD schema still declares it, and drops it
otherwise; either way the operator reads the new name, finds nothing, and applies
a default. A `StoragePool` that sets `dhchap: true` loses authentication. Nothing
reports that it used to have it. This is the dangerous mode, and every user-authored
spec row is in it.

**A renamed status field costs nothing.** The operator is the only writer, so the
old value is overwritten on the first reconcile after the upgrade. The only readers
outside the operator are dashboards and `kubectl -o jsonpath`, which is a
documentation problem rather than a migration one.

**A renamed enum value fails loudly, which is the good failure.** A
`StorageClusterOps` with `action: activate` is rejected at admission once the
`Enum` marker lists `Activate`. Nobody discovers the rename by finding an operation
that silently never ran. This is the one class that needs no data migration and
only a deprecation window for the sake of scripts and runbooks.

**A renamed toggle that also inverts is the worst case**, because the mechanical
migration produces the opposite of the intended behavior. There is exactly one such
row, `skipKubeletConfiguration`, and `design-crd-model.md` §9.6 flags it twice.

---

## 2. The Inventory

Rows are grouped by what the migration has to do, not by kind. The `Class` column
is the failure mode of §1.

### 2.1 Spec field renames, same parent

These change a field's name and nothing else. They are the core of this work.

| Kind                | Registered                  | Target                      | Class  | Owning design                         |
|---------------------|-----------------------------|-----------------------------|--------|---------------------------------------|
| `StorageCluster`    | `spec.maxHugePagesSize`     | `spec.minHugePagesSize`     | Silent | `design-storagecluster.md` §12        |
| `StorageCluster`    | `spec.backup.localEndpoint` | `spec.backup.endpoint`      | Silent | `design-storagecluster.md` Appendix A |
| `StorageNode`       | `spec.overrides`            | `spec.config`               | Silent | `design-storagenode.md` §15.1         |
| `StorageNode`       | `spec.socketIndex`          | `spec.slot`                 | Silent | `design-storagenode.md` §15.1         |
| `StorageNodeOps`    | `spec.storageNodeRef`       | `spec.nodeRef`              | Silent | `design-storagenode.md` §15.2         |
| `StorageNodeOps`    | `spec.drain`                | `spec.remove`               | Silent | `design-storagenode.md` §15.2         |
| `StorageClusterOps` | `spec.nodeRollingRestart`   | `spec.rollingRestart`       | Silent | `design-storagecluster.md` §5.3       |
| `StoragePool`       | `spec.clusterName`          | `spec.clusterRef`           | Silent | `design-storagepool.md` §11           |
| `StorageBackup`     | `spec.clusterName`          | `spec.clusterRef`           | Silent | `design-storagebackup.md` §13         |
| `BackupPolicy`      | `spec.clusterName`          | `spec.clusterRef`           | Silent | `design-storagebackup.md` §13         |
| `BackupRestore`     | `spec.clusterName`          | `spec.clusterRef`           | Silent | `design-storagebackup.md` §13         |
| `VolumeMigration`   | `spec.pvName`               | `spec.persistentVolumeName` | Silent | `design-persistentvolumeops.md` §10   |

Two Go-side companions travel with these and change no wire format:
`StorageNodeOverrides` becomes `StorageNodeConfig`, and `NodeRollingRestartSpec`
becomes `RollingRestartSpec`.

`BackupImport` carries no `clusterName`: it names `sourceClusterName` and
`targetClusterName`, and the kind is retired rather than renamed
(`design-storagebackup.md` §13), so it takes no row here.

### 2.2 Status field renames

Free to make, because the operator is the only writer.

| Kind                | Registered                        | Target                  | Owning design                  |
|---------------------|-----------------------------------|-------------------------|--------------------------------|
| `StorageClusterOps` | `status.nodeRollingRestartStatus` | `status.rollingRestart` | `design-storagecluster.md` §7  |
| `BackupPolicy`      | `status.attachedLvols`            | `status.attachedClaims` | `design-storagebackup.md` §4.1 |

`status.nodeRollingRestartStatus` also replaces `pendingNodes` and
`processedNodes` with `nodes` and `nodeIndex`, which is a re-shaping rather than a
rename and belongs with the rolling-restart work.

### 2.3 Boolean toggle renames

`design-crd-model.md` §9.6 owns this list. It is stated there as eleven fields
across five kinds; nine of them are registered, one is registered in two places,
and one is not registered at all.

| Struct                        | Registered                 | Target                             | Default | Class                |
|-------------------------------|----------------------------|------------------------------------|---------|----------------------|
| `StorageNodeSpec`             | `skipKubeletConfiguration` | `enableKubeletConfiguration`       | off     | Inverting            |
| `StorageNodeSetSpec`          | `skipKubeletConfiguration` | `enableKubeletConfiguration`       | off     | Inverting            |
| `VolumeAutoPlacementSettings` | `migrationEnabled`         | `disableMigration`                 | on      | Inverting            |
| `VolumeAutoPlacementSettings` | `latencyBenchmarkEnabled`  | `enableLatencyBenchmark`           | off     | Silent               |
| `VolumeAutoPlacementSettings` | `enabled`                  | `spec.enableVolumeAutoPlacement`   | off     | Silent, and moves up |
| `DataRealignmentSettings`     | `enabled`                  | `spec.enableDataRealignment`       | on      | Silent, and moves up |
| `VolumeMigrationSettings`     | `enabled`                  | Removed                            | on      | Removal              |
| `BackupSpec`                  | `withCompression`          | Removed                            | off     | Removal              |
| `BackupSpec`                  | `snapshotBackups`          | Removed                            | off     | Removal              |
| `BackupSpec`                  | `localTesting`             | Removed                            | off     | Removal              |
| `StorageClassParameters`      | `encryption`               | `enableEncryption`                 | off     | Silent               |
| `StoragePoolSpec`             | `dhchap`                   | `spec.volumeDefaults.enableDHCHAP` | off     | Silent, and regroups |

**`replicate` is in the design's list and not in the API.**
`design-crd-model.md` §9.6 and `design-storagepool.md` §11 both name
`replicate` → `enableReplication` on `StorageClassParameters`, and the registered
struct has no such field. The row is a target-state addition rather than a rename,
so it is not migrated; it is created named correctly whenever replication becomes
expressible on a class.

**Three rows are removals rather than renames**, and `design-storagecluster.md`
§12 gives the reason for all three `BackupSpec` ones: the store is a location, and
how a copy is taken belongs to the control plane, which keeps accepting these
values and applies its own defaults once the operator stops sending them.
`VolumeMigrationSettings.enabled` is removed because migration cannot be turned
off — a drain, a rebalance, and a device replacement are all performed by moving
volumes.

**`skipKubeletConfiguration` inverts.** A deprecation window that reads the old
field and writes the new one has to negate it. Reading `skipKubeletConfiguration:
true` and writing `enableKubeletConfiguration: true` configures the kubelet on a
node that asked not to be configured.

**The chart carries these names too.** `skipKubeletConfiguration` appears in
`helm-charts/charts/simplyblock-operator/values.yaml`, and `multiCluster.enable`
is a third spelling that exists only there.

### 2.4 Regroupings — renames that also move

These change a field's path as well as its name, so the migration reads from one
place and writes to another. They are listed for completeness and are entangled
with new types the redesign introduces.

| Kind              | Registered                                        | Target                                   | Owning design                        |
|-------------------|---------------------------------------------------|------------------------------------------|--------------------------------------|
| `StorageCluster`  | `spec.hashicorpVaultSettings.baseURL`             | `spec.kms.vault.baseURL`                 | `design-storagecluster.md` §3.1      |
| `StoragePool`     | `spec.capacityLimit`, `spec.logicalVolumeMaxSize` | `spec.limits.capacity`, `.maxVolumeSize` | `design-storagepool.md` §3.1         |
| `StoragePool`     | `spec.qos.*`                                      | `spec.limits.{iops,throughput}`          | `design-storagepool.md` §3.1         |
| `StoragePool`     | `spec.storageClassParameters.*`                   | `spec.volumeDefaults.*`                  | `design-storagepool.md` §3.1         |
| `StorageNodeOps`  | `spec.targetWorkerNode`, `spec.newSsdPcie`        | `spec.migrate.*`                         | `design-storagenode.md` §6.1         |
| `ControlPlane`    | `spec.image`                                      | `spec.source.managed.image`              | `design-controlplane.md` §5.1        |
| `VolumeMigration` | `spec.targetNodeUUID`                             | `spec.migrate.targetNodeRef`             | `design-persistentvolumeops.md` §4.1 |

**`spec.volumeDefaults` is the sharpest of these**, and `design-storagepool.md`
§11 says why: it is immutable once set, so a pool that applies with the old
spelling gets an empty `volumeDefaults` that cannot then be corrected without
deleting the pool.

**`spec.image` is not only a regrouping.** `design-controlplane.md` §11 records
that the field is inherited by every `StorageNodeSet` that omits
`spec.clusterImage`, so the move has to send the storage-node default to
`StorageCluster.spec.storageNodes.image` rather than under
`spec.source.managed`.

### 2.5 Enum value recasing

`design-crd-model.md` §9.7 owns this list. These fail at admission rather than
silently.

| Type                      | Registered values                                             | Target                                                           |
|---------------------------|---------------------------------------------------------------|------------------------------------------------------------------|
| `StorageClusterOpsAction` | `activate;expand;shutdown;start;restart;node-rolling-restart` | `Activate;Expand;Shutdown;Start;Restart;RollingRestart`          |
| `StorageNodeOpsAction`    | `shutdown;restart;suspend;resume;remove;migrate`              | `Shutdown;Restart;Suspend;Resume;Remove;Migrate;HostMaintenance` |
| `MetricsBackend`          | `controlplane;prometheus;uniform`                             | `ControlPlane;Prometheus;Uniform`                                |
| `ControlPlane` phase      | `Initializing;Ready`                                          | `Available` replaces `Ready` (§3.3)                              |
| `VolumeMigration` phase   | `Completed`                                                   | `Succeeded`                                                      |

Both action fields are also typed today as a plain `string` and become named enum
types, which is a Go-side change the recasing carries anyway.

`NodeDrainState.Phase` is in the design's table and needs nothing: it is status
the operator alone writes, and the kind carrying it is retired.

The two phase rows are status the operator writes, so they are free in the sense
of §1 and expensive only for whatever reads them.

### 2.6 Renames outside the CRD schemas

These are renames of keys rather than of properties, so they do not move with the
API types and do not benefit from the mechanism of §3. Each is listed so that no
reader concludes the property work covered them.

| What                           | Registered                                                     | Target                                                                                  | Owning design                   |
|--------------------------------|----------------------------------------------------------------|-----------------------------------------------------------------------------------------|---------------------------------|
| `StorageClass.parameters` keys | `qos_rw_iops`, `qos_rw_mbytes`, `qos_r_mbytes`, `qos_w_mbytes` | `max_iops`, `max_mbytes_per_sec`, `max_read_mbytes_per_sec`, `max_write_mbytes_per_sec` | `design-storagepool.md` §5.1    |
| Claim QoS annotations          | `simplyblock.io/qos-*`                                         | `storage.simplyblock.io/max-*`                                                          | `design-storagepool.md` §5.1    |
| Annotation and label prefix    | `simplyblock.io/` on 28 keys                                   | `storage.simplyblock.io/`                                                               | `design-crd-model.md` §9.4      |
| `StorageCluster` finalizer     | `storage.simplyblock.io/cluster-finalizer`                     | `storage.simplyblock.io/storagecluster-finalizer`                                       | `design-storagecluster.md` §4.5 |
| `ControlPlane` event reasons   | `FDBReady`, `FDBNotReady`                                      | `ControlPlaneReady`, `ControlPlaneNotReady`                                             | `design-controlplane.md` §11    |

**The `StorageClass` parameter keys never migrate.** `StorageClass.parameters` is
immutable in the Kubernetes API, so a class an older operator generated can never
be rewritten. `design-storagepool.md` §5.1 settles this: both generations are read
indefinitely rather than for a window, the operator writes only the new keys, and
a class carrying both spellings is reported as a `QoSParameterConflict` rather
than merged.

**The finalizer rename is the one change here that wedges rather than degrades.**
An operator that reads only the new key leaves every object created by an older one
in `Terminating` forever, so both spellings are read for a release and the old one
is removed after.

The QoS keys reach beyond the operator: `atlas-lib/kube/names.go`,
`atlas-lib/kube/storageclass.go`, `csi-driver/internal/csi/controller/volume.go`,
`csi-driver/e2e/params.go`, three chart value files, and
`operator/internal/controller/simplyblockstoragepool_controller.go` all name them.

### 2.7 Renames blocked on structural work

Listed so they are not attempted as part of a rename sweep.

| Kind             | Registered                         | Target                                | Blocked on                                         |
|------------------|------------------------------------|---------------------------------------|----------------------------------------------------|
| `StorageNode`    | `spec.storageNodeSetRef`, required | `spec.clusterRef` plus `spec.nodeSet` | The `StorageNodeSet` retirement, §15.3             |
| `StorageNodeOps` | `status.subPhase`, a string        | `status.step`, an object              | The `Ops` step machine, `design-crd-model.md` §9.5 |
| `StorageCluster` | `status.subPhase`, a string        | `status.step`, an object              | The same                                           |

`storageNodeSetRef` is a reparent rather than a rename: the target names a
different object, and the value cannot be computed until `StorageCluster` owns the
workload. The two `subPhase` rows are renames whose type also changes from a string
to an object, and `design-crd-model.md` §9.5 already specifies their conversion —
the old string reads into `step.state` and leaves `step.deadline` absent, so an
operation in flight across the upgrade keeps running rather than expiring
immediately.

---

## 3. The Migration Mechanism

### 3.1 What the group's version allows, and what it does not excuse

The group is at `v1alpha1`, which is the version where a breaking change is
affordable, and every design leans on that. It is not a license to rename without
a path. `v1alpha1` says a user is not *entitled* to keep the old spelling; it does
not say their cluster should silently lose a setting on upgrade. The silent class
of §1 is the whole problem: the object stays valid, the apply succeeds, and the
behavior changes.

So the mechanism has to satisfy one requirement. **No upgrade may change the
effective configuration of an object nobody edited.**

### 3.2 A second version with a conversion webhook

`v1alpha2` is added as the storage version carrying the new names, `v1alpha1`
stays served and deprecated carrying the old ones, and a conversion webhook
translates between them. Every object already stored is readable under both
versions from the moment the webhook is up, which satisfies §3.1 without a
fallback path in any reconciler: a controller reads `v1alpha2` and never learns
that an older spelling exists.

**This is the mechanism that scales to the whole inventory rather than to part of
it.** A read-time fallback handles a field renamed in place, and degrades as soon
as a rename also moves. The regroupings of §2.4 turn one field into a path under a
struct that did not exist, and `spec.qos.*` becoming `spec.limits.{iops,throughput}`
redistributes four fields across two new parents. Expressing that as a per-field
fallback in a reconciler means the reconciler carrying the old shape and the new
one at once; expressing it as a conversion function is the ordinary case the
function signature was designed for. The same holds for the enum recasing of
§2.5, where the old and new value occupy one field and a fallback has nowhere to
put the second reading.

**It is also the only mechanism that keeps the old spelling working for a client
the operator does not control.** A read-time fallback lives in the operator, so it
covers what the operator reads and nothing else. A conversion webhook lives in the
API server's path, so `kubectl`, Argo CD, Flux, and anything else applying a
`v1alpha1` manifest keep working unchanged and unaware.

**Three costs come with it, and none of them is avoidable by care.**

**The webhook is in the read path for every request to these kinds.** A conversion
webhook that is down does not degrade the group, it makes it unreadable: a
`kubectl get storagecluster` fails rather than returning the old shape. During an
upgrade, which is when the webhook's own deployment is being replaced, is when this
is most likely, and a `failurePolicy` cannot help because there is no meaningful
answer to fall back to. What contains it is that the webhook is served by the
operator's existing manager, so it is up whenever the operator is, and the operator
being down already stops reconciliation.

**Conversion has to round-trip, and three rows lose data.**
`BackupSpec.withCompression`, `snapshotBackups`, `localTesting`, and
`VolumeMigrationSettings.enabled` are removals rather than renames (§2.3), so a
`v1alpha1` object carrying them converts to a `v1alpha2` with nowhere to put them,
and converting back would silently drop what the user wrote. §3.3 says where they
are stashed.

**Only a kind with a renamed property gains a `v1alpha2`.** A CRD declares its own
versions, so the group does not have to move as a unit, and a kind with nothing to
convert would gain a second version, a conversion function, and a webhook in its
read path in exchange for a copy. Seven kinds gain one: `ControlPlane`,
`StorageCluster`, `StorageClusterOps`, `StorageNode`, `StorageNodeOps`,
`StoragePool`, and `StorageBackup`. The group's version therefore depends on which
kind is being named, which is the cost of not paying the other one.

**A kind that becomes a different kind cannot use this mechanism at all.** A
conversion webhook converts between versions of one kind, and `BackupPolicy`
becoming `StorageBackupPolicy`, `BackupRestore` becoming `StorageBackupOps`, and
`VolumeMigration` becoming `PersistentVolumeOps` are new CRDs (§9.1 of
[`design-crd-model.md`](design-crd-model.md)). Their property renames —
`spec.clusterName`, `status.attachedLvols`, `spec.pvName` — land with the successor
kind, on whatever path that kind takes for its own objects, which
[`design-persistentvolumeops.md`](design-persistentvolumeops.md) §10 and
[`design-storagebackup.md`](design-storagebackup.md) §13 both say is draining
in-flight operations rather than converting them. `BackupImport` and `Task` are
retired and reworked-not-at-all respectively, and the four replication kinds carry
no row.

**`StorageNodeSet` stays at `v1alpha1` deliberately**, and §3.6 is why.

### 3.3 The shape of the conversion

**`v1alpha2` is the hub and `v1alpha1` is the spoke.** The hub is the storage
version and the shape every controller reads, so a conversion is written once per
kind rather than once per pair. `v1alpha1` implements `ConvertTo` and
`ConvertFrom`; `v1alpha2` implements the empty `Hub()` marker and nothing else.

**A rename converts by assignment, and the inverting rows negate.** The rows of
§2.1 and the non-inverting half of §2.3 are a field read on one side and written on
the other. `skipKubeletConfiguration`, `migrationEnabled`, and the toggles of §2.3
whose default is on are the exception, and §3.4 is why they need tests of their
own rather than review.

**A regrouping allocates its parent before writing into it.** `spec.qos` converting
to `spec.limits` has to leave `spec.limits` absent rather than empty when the source
is absent, because `spec.volumeDefaults` is immutable once set
(`design-storagepool.md` §11) and an empty struct written by a conversion is a value
the user can then never correct.

**A removed field is stashed in an annotation.** The four rows of §2.3 that are
removals are written to `storage.simplyblock.io/v1alpha1-<field>` on conversion up
and read back on conversion down. This is the convention Kubernetes uses for lossy
conversions in its own groups, and it exists so that round-tripping a `v1alpha1`
object through the API server returns what was applied. It is not a compatibility
surface: nothing reads these annotations except the conversion, and they disappear
with `v1alpha1`.

**An enum value converts by table.** `activate` becomes `Activate` going up and
back going down. A value in neither table is passed through unchanged rather than
rejected, because a conversion webhook is the wrong place to fail an object: the
`Enum` marker on each version already rejects what that version does not accept,
and a conversion that errors makes the object unreadable rather than invalid.

### 3.4 The inverting rows need their own tests

For `skipKubeletConfiguration`, `migrationEnabled`, and the toggles of §2.3 whose
default is on, the conversion negates rather than copies. Each needs a test that
asserts the *behavior* rather than the field value: a node that set
`skipKubeletConfiguration: true` must still not have its kubelet configured after
the upgrade. Asserting `enableKubeletConfiguration == false` would pass against a
conversion that got the polarity right and against one that never ran at all, since
`false` is also the zero value.

The same test has to run in both directions. A conversion that negates going up and
copies going down is a bug that a one-way test cannot see, and it corrupts on the
first `kubectl get -o yaml | kubectl apply -f -`.

### 3.5 What the conversion does not cover

The key renames of §2.6 are not properties, so no conversion reaches them. They
keep the both-spellings treatment stated there: the finalizer is read under both
names for a release, the annotation prefixes are read under both and the old one
deprecated in an event, and the `StorageClass` parameter keys are read under both
indefinitely, because `StorageClass.parameters` is immutable and a class an older
operator generated can never be rewritten.

The chart's own value names are outside it too. `skipKubeletConfiguration` in
`values.yaml` is a chart input rather than an API field, so the conversion never
sees it, and it needs the both-spellings treatment in the template or a documented
break.

### 3.6 StorageNodeSet keeps one version, and the node reparents lazily

`StorageNodeSet` is retired by
[`design-crd-model.md`](design-crd-model.md) §9.2, and a kind on its way out does
not earn a second version. Its one row, `skipKubeletConfiguration`, is dropped
rather than migrated: the field's replacement lives on `StorageNode.spec.config`
(§2.3), which is a kind that does gain a `v1alpha2`, so nothing is lost by leaving
the retiring kind spelled as it shipped.

**What this costs is that `StorageNode.spec.storageNodeSetRef` cannot be converted,
and that is the right outcome rather than a gap.** The target is
`spec.clusterRef` plus `spec.nodeSet` (§2.7), and the cluster's name is not in the
node — it is in the `StorageNodeSet` the node points at. A conversion function
cannot go and read it: conversion runs on every read of the object, has to be a
pure function of what it was handed, and a conversion that issues API calls turns
one `kubectl get` into two and fails the read when the second one does.

**So `v1alpha2` carries all three fields, and the controller fills the new two.**
`storageNodeSetRef` survives into `v1alpha2` as an optional deprecated field that
converts by copy, and `clusterRef` and `nodeSet` are optional beside it. A node
reconciled with the old field set and the new ones empty resolves the set, writes
the cluster and the group name, and from then on the node names its cluster
directly. A node created with the new fields never needs the old one.

**The workload reparents on the same schedule, not at start-up.** The DaemonSet,
the Services, the certificates, and the per-node ConfigMaps are owned by the
`StorageNodeSet` controller today
([`design-storagenode.md`](design-storagenode.md) §15.3), and moving them is what
the retirement is. Doing it as a sweep when the operator starts would rewrite
every owner reference in the fleet in one transaction, at the moment of an upgrade,
with a garbage collector watching: an owner reference written wrong, or written
before the new owner exists, deletes a running storage node. Reparenting one node's
objects as that node is reconciled keeps the blast radius at one node and makes a
mistake visible before it is fleet-wide.

This is the retirement's business rather than the renames', and it is stated here
because it is the reason this document leaves one row unconverted.

### 3.7 The trust has to exist before the operator starts

The operator both serves the conversion webhook and reads the kinds it converts,
and that is a cycle rather than a coincidence.

**A controller-runtime manager starts its HTTP servers, then its webhook servers,
then syncs its caches, and only then runs everything else.** The first two orders
are deliberate and documented in the manager itself: probes and webhooks come
first *because* a cache sync over a converted kind lists it at the hub version,
which makes the API server convert every stored object, which calls the webhook.
What the manager cannot order is anything that is not one of those servers.

**So a CA injected by a Runnable arrives too late by construction.** The list
fails, the cache never syncs, the manager exits, and the injection that would have
fixed it never runs. The operator crash-loops, and retrying inside it cannot help,
because the retry sits on the far side of the sync that is failing. This is a
bootstrap deadlock and not a race: waiting longer never resolves it.

**The serving certificate and the CA bundle are therefore provisioned before the
manager is constructed**, through a direct client rather than the manager's. The
two kinds this touches, `Secret` and `CustomResourceDefinition`, are core and
apiextensions kinds that no conversion webhook stands in front of, so the
bootstrap can always make progress no matter what state the converted kinds are
in. Rotation stays where it was: the certificate machinery keeps running under the
manager and re-injects whenever the material changes, and the bootstrap only
guarantees that the first pass has already happened.

**Reusing existing material matters as much as creating it.** An operator that
issued a fresh CA on every start would invalidate the bundle its CRDs already
carry, so every restart would open a window in which the API server rejects the
webhook it was just told to trust. The bootstrap therefore adopts what is already
stored whenever it is valid for the service's DNS name and not near expiry.

**The alternative was to ship the CRDs with `strategy: None` and have the operator
raise it to `Webhook` once it is serving.** That removes the cycle and replaces it
with something worse. Under `None` the API server answers a hub-version read of a
stored spoke object by relabeling the apiVersion and pruning every field the new
schema does not know, so a reader sees an object with fields silently missing —
and a controller that writes during that window persists the pruned form. A
startup failure that is loud and self-correcting is a better trade than a
data-losing window that is neither.

---

## 4. Sequencing

The conversion webhook has to exist before any kind can move, so the first item is
infrastructure rather than a rename and everything else waits on it.

**First, the `v1alpha2` package and the webhook wiring.** An empty `v1alpha2` for
one kind, its `Hub()`, a `ConvertTo`/`ConvertFrom` on `v1alpha1` that copies
verbatim, the `+kubebuilder:storageversion` marker moving, and the CA bundle
injected into that CRD. Proving a copy-only conversion serves correctly is what
de-risks every row after it, and it is the piece that can break `kubectl get`.

**Then one kind at a time, smallest first**, each carrying every row it owns rather
than each class of row sweeping across every kind. A kind is one `v1alpha2` type
file, one conversion with its round-trip test, and its controllers moved to read
the hub, which is a unit that can be reviewed and reverted. Sweeping by class
would leave every kind half-converted between sweeps, and a half-converted kind is
one whose controller reads a field the conversion does not yet write.

| Order | Kind                | Rows it carries                                                              |
|-------|---------------------|------------------------------------------------------------------------------|
| 1     | `ControlPlane`      | §2.4 image regrouping, §2.5 phase                                            |
| 2     | `StorageBackup`     | §2.1 `clusterName`                                                           |
| 3     | `StorageClusterOps` | §2.1 `nodeRollingRestart`, §2.2 its status twin, §2.5 the action enum        |
| 4     | `StorageNodeOps`    | §2.1 `storageNodeRef` and `drain`, §2.4 the migrate group, §2.5 action enum  |
| 5     | `StoragePool`       | §2.1 `clusterName`, §2.3 `dhchap` and `encryption`, §2.4 both regroupings    |
| 6     | `StorageNode`       | §2.1 `overrides` and `socketIndex`, §2.3 the inverting toggle, §3.6's bridge |
| 7     | `StorageCluster`    | §2.1 two rows, §2.3 six toggles, §2.4 the KMS regrouping, §2.5 the backend   |

`ControlPlane` is first because it is the smallest kind that carries both a
regrouping and an enum, so it proves the two hardest shapes on the least code.
`StorageCluster` is last because it carries the most rows and the removals of §3.3
that need the annotation stash.

The key renames of §2.6 share nothing with any of it and can proceed in parallel.

The blocked rows of §2.7 stay blocked, except that §3.6 changes why for one of
them: `storageNodeSetRef` is carried into `v1alpha2` unconverted and filled in by
the controller, rather than waiting for the retirement.

---


## 5. What Each Row Costs Outside the API Types

A rename is not finished when the Go field changes. Every row touches, at minimum,
the generated CRD manifests and the chart copy of them, and `make -C operator
manifests generate` plus `make helm-sync` regenerate both. Beyond that:

**`maxHugePagesSize` reaches further than the others and still stops short of a
full rename.** `design-storagecluster.md` §12 records the boundary: the CRD field
is the operator's to rename, but `hugepages_mem` belongs to the control-plane API
and `MAX_HUGE_PAGES_SIZE` is the variable each storage node's configuration is read
with. The field therefore lands emitting `MAX_HUGE_PAGES_SIZE` unchanged, and the
mismatch becomes a documented boundary rather than a bug. Four call sites move with
it: `controllers/cluster/storagecluster_controller.go`,
`controllers/node/pernodeconfig.go`, three unit tests, and the chart's
`operator_customresources.yaml`.

**`spec.overrides` is the widest of the spec rows**, because the struct is read
throughout node provisioning rather than at one call site.

**The chart carries user-facing spellings of its own.** `skipKubeletConfiguration`
is in `values.yaml`, and a chart value is not migrated by any webhook, so it needs
the same both-spellings treatment in the template or a documented break.

---

## 6. Open Questions

**Q1: When `v1alpha1` stops being served.** §3.2 keeps it served and deprecated,
and nothing here says for how long. Removing it is what ends the conversion's
maintenance and deletes the annotation stash of §3.3, and keeping it costs a
conversion function per kind that has to stay correct as `v1alpha2` evolves. The
answer does not block the work, because every row lands the same way regardless.

**Q2: Whether the three `enabled` toggles are flattened or renamed in place.**
`design-crd-model.md` §9.6 defers this and `design-storagecluster.md` §3.1 answers
it for two of the three by moving them to the cluster's top level as
`spec.enableVolumeAutoPlacement` and `spec.enableDataRealignment`. What is not
settled is `VolumeMigrationSettings.enabled`, which §12 removes outright, leaving
`VolumeMigrationSettings` with only its remaining members and no toggle. Whether
the struct survives that is not decided.

**Q3: Whether stored objects are migrated eagerly.** A conversion webhook makes
every object readable as `v1alpha2` without rewriting anything, so an object
applied as `v1alpha1` stays stored in whatever version it was written under until
something writes it again. That is correct and indefinite, and it means the
webhook cannot be retired by waiting. A storage-version migration — the
`StorageVersionMigration` kind, or a job that reads and rewrites every object —
would drain the old version deliberately. Which one, and whether the operator owns
it or the upgrade procedure does, is not decided.

**Q4: What the conversion does when a `v1alpha1` object is invalid.** §3.3 passes
an unrecognized enum value through rather than failing, on the grounds that a
conversion that errors makes the object unreadable. That leaves a stored object
whose value satisfies neither version's `Enum` marker, which is possible today only
if an object was written before a marker tightened. Whether such an object should
be reported, and by what, is not settled.
