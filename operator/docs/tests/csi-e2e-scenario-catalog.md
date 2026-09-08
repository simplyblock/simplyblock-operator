# CSI E2E Scenario Catalog

This catalog is the backlog for **additional** end-to-end coverage in
[`csi-driver/e2e`](../../../csi-driver/e2e).  It is based on the currently
implemented Ginkgo specs and the driver's supported CSI operations.  It does
not propose duplicating unit or CSI-sanity tests unless a Kubernetes-to-backend
integration boundary is not otherwise exercised.

An E2E test is successful only when it verifies both sides of the contract:

1. the Kubernetes object, event, condition, or workload outcome; and
2. the externally observable storage result (PV CSI attributes, backend
   object/state through `sbctl`, or NVMe device/mount state on the node).

Tests which deliberately disrupt a node, NVMe path, controller, or backend are
labelled **Disruptive**.  They belong in a serial, isolated lane with a
dedicated pool and must restore the environment in `DeferCleanup`.

## Current coverage and boundaries

The current suite already proves the following, so these are not repeated as
new scenarios below:

| Area | Existing proof |
|---|---|
| Basic lifecycle | CSI controller and node DaemonSet become ready; a dynamically provisioned filesystem PVC binds and mounts. |
| Filesystem and block I/O | ext4 nested-file persistence; XFS format and persistence; raw-block presence, persistence, and basic expansion. |
| Expansion | An in-use filesystem PVC updates PVC capacity and filesystem size; a raw-block PVC completes the resize flow. |
| Snapshots and clones | Two point-in-time snapshot restores preserve the expected data; a clone contains source data; explicit snapshot and Delete-reclaim PV removal work. |
| Parameters | `qos_rw_iops`, `qos_rw_mbytes`, PVC QoS override, and `encryption=True` provision mountable volumes. |
| Basic invalid requests | Unknown StorageClass, PVC shrink, and clone from a missing PVC remain rejected/pending as appropriate. |
| Connectivity recovery | One lost multipath controller reconnects for managed volumes; unmanaged lvols are not healed; total path loss recovers through republish and through the opt-in guardian. |
| Multi-cluster | An opt-in two-cluster test verifies zone-pinned pods use the expected cluster. |

The suite should retain its per-spec namespace isolation.  New fixed-name
StorageClasses, PVs, `VolumeSnapshotContents`, and disruption resources should
include the namespace/run ID to remain safe with `E2E_PROCS > 1`.

## Test tiers and common assertions

| Tier | Meaning | Suggested cadence |
|---|---|---|
| P0 | Data loss, corruption, attach safety, or cleanup leak risk | Required release gate; serial if disruptive |
| P1 | Supported feature contract or a high-probability operational failure | Nightly / release candidate |
| P2 | Compatibility, diagnostics, limits, or unusual topology | Nightly or scheduled hardware lane |

For all failure cases, assert a bounded terminal result: an API validation
error, a PVC/PVC datasource `Pending` state with the relevant event, or a pod
that remains unready for the intended reason.  Do **not** only assert that a
timeout happened.  For success cases, verify read-after-write data, not merely
`PodRunning`.

## Provisioning, binding, and scheduling

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| PR-01 | P0 | Create a `WaitForFirstConsumer` StorageClass, then its PVC without a consumer. Add a node-pinned consumer afterwards. | Distinguishes scheduler-delayed provisioning from an unusable provisioner; required for topology-aware classes. | PVC remains `Pending` before scheduling with no backend lvol; after the pod is scheduled, exactly one lvol/PV is created, bound, mounted, and writable. |
| PR-02 | P1 | A `WaitForFirstConsumer` PVC with a pod whose node affinity cannot be scheduled. | Ensures no volume leaks when Kubernetes cannot choose a topology. | Pod is `Pending` with an unschedulable event; PVC remains unbound and no backend lvol is created. |
| PR-03 | P0 | Provision several same-size PVCs concurrently (at least 10, with a bounded concurrency). | Exercises external-provisioner retries, driver name/idempotency, and backend concurrency. | Every PVC binds to a distinct PV/lvol, every pod writes a unique marker, and no duplicate or orphan lvol remains after cleanup. |
| PR-04 | P1 | Delete a PVC while `CreateVolume` is deliberately delayed (backend fault injection or controller restart). | Covers the create/delete race that most easily leaves orphaned backend state. | The PVC disappears; its PV and backend lvol eventually disappear; the controller recovers without a stuck finalizer. |
| PR-05 | P1 | Create two PVCs with the same `dataSource` source concurrently. | Verifies clone/snapshot fan-out and unique CSI volume identities. | Both destination PVCs bind, preserve source data, can diverge independently, and use different lvol IDs. |
| PR-06 | P1 | Provision an RWO PVC, mount it on node A, and schedule another pod using it on node B. | Proves attachment exclusivity is enforced at the Kubernetes/CSI boundary. | Second pod stays unready with a multi-attach/attach failure event; node A continues I/O intact. |
| PR-07 | P1 | Two pods on the *same* node concurrently mount the same RWO filesystem PVC. | RWO permits multiple consumers on one node; validates staging/publish reference counting. | Both pods become ready, see each other's files, and deleting one does not disturb the other. |
| PR-08 | P1 | Delete and recreate a pod on the same node rapidly, before kubelet has fully unstaged the volume. | Specifically exercises `NodePublishVolume` healing when kubelet skips a fresh stage. | Replacement pod becomes ready, data persists, and no stale mount/device prevents later cleanup. |
| PR-09 | P2 | Create a PVC with a `volumeMode: Filesystem` capability incompatible with the requested StorageClass volume type, if the backend exposes one. | Makes capability validation visible to users. | PVC fails provisioning with an explanatory event; no backend lvol or partial PV is left. |

## Filesystem, mount, and block semantics

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| FS-01 | P0 | For both ext4 and XFS, write a multi-megabyte checksum file, `fsync`, delete/recreate the pod, and verify SHA-256. | Existing marker tests can miss torn/corrupted writes. | Checksum and byte count match exactly after republish. |
| FS-02 | P1 | Mount ext4 and XFS volumes with explicit mount flags such as `noatime`; inspect `/proc/mounts` in the workload. | Proves `NodeStageVolume` forwards mount flags and does not silently drop them. | Requested supported flags appear on the mount and normal read/write works. |
| FS-03 | P1 | Use an XFS StorageClass carrying stripe geometry parameters supported by the driver; inspect `xfs_info`. | Exercises the driver's XFS stripe option path, not only generic XFS formatting. | The filesystem mounts and reports the expected supported stripe geometry. |
| FS-04 | P1 | Format/mount a filesystem volume, fill it near a safe threshold, delete/recreate the pod, then delete the PVC. | Validates capacity accounting and cleanup under real allocation pressure. | Writes fail only at filesystem ENOSPC; no corruption occurs; PV and lvol are fully deleted afterwards. |
| FS-05 | P1 | Write data, snapshot/clone it, then change the source data and remount all three. | Establishes copy-on-write isolation, not merely initial copy correctness. | Source has new content; snapshot restore and clone retain the earlier checksum; their writes do not affect the source. |
| FS-06 | P1 | Create a raw-block PVC; write nontrivial binary data at multiple offsets; republish and verify per-offset checksums. | A first-sector text marker is insufficient for raw-block data integrity. | Every offset reads exactly as written after pod restart. |
| FS-07 | P1 | Expand a raw-block PVC, record device size before and after, and write/read a marker in the newly added tail region. | Current test only proves that a nonzero device remains accessible. | Device size increases to at least requested capacity (allowing documented alignment); tail I/O succeeds and old data remains unchanged. |
| FS-08 | P1 | Expand in-use ext4 and XFS PVCs after writing checksummed data. | Expansion must preserve existing data across both supported filesystems. | PVC and filesystem sizes grow; original checksum remains exact; new tail-space write succeeds. |
| FS-09 | P2 | Create and mount a PVC with an unsupported/invalid `csi.storage.k8s.io/fstype`. | Checks failure containment around mkfs/mount errors. | Pod does not become ready; events/logs identify the format/mount error; cleanup removes any created backend lvol. |
| FS-10 | P2 | Request an unsupported mount option. | Ensures invalid options do not result in a falsely successful publish. | Pod remains unready with a clear mount failure; no leaked target mount survives after deletion. |

## Resize and capacity failure handling

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| RS-01 | P0 | Expand a bound-but-unmounted filesystem PVC, then mount it. | Covers controller expansion followed by first node expansion rather than online-only flow. | PVC reaches requested capacity; first mount has the expanded filesystem and accepts data. |
| RS-02 | P1 | Issue two increasing resize updates before the first completes. | Tests coalescing/idempotency under normal Kubernetes retries. | Final PV/PVC/device/filesystem capacity reaches the larger request; no error or capacity regression occurs. |
| RS-03 | P1 | Request a size beyond pool capacity or impose an injected backend ENOSPC. | Prevents an ambiguous indefinitely pending resize. | PVC retains its previous usable size, exposes a resize/provisioning failure event, and pre-existing data remains readable. |
| RS-04 | P1 | Restart the CSI controller between `ControllerExpandVolume` and node expansion. | Verifies expansion state is recoverable rather than in-memory. | Reconciliation resumes; exactly one effective resize occurs; final capacity and data are correct. |
| RS-05 | P2 | Resize an encrypted volume and an XFS-striped volume. | Combines controller parameters with node-side resize paths. | Expansion succeeds with encryption/format semantics retained and checksum preserved. |

## Snapshots, restores, and clones

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| SN-01 | P0 | Take a snapshot while a pod continuously writes numbered, `fsync`ed records; restore it after the writer stops. | Tests crash-consistent snapshot semantics under I/O, rather than only quiesced sources. | Restore is readable and internally consistent at a documented point-in-time boundary; no filesystem corruption occurs. |
| SN-02 | P0 | Create a snapshot, create a PVC from it, then delete the source PVC before mounting the restore. | Proves snapshot independence and backend reference handling. | Restored PVC binds and contains snapshot data; source deletion does not delete or corrupt it. |
| SN-03 | P1 | Delete a source PVC while its snapshot is retained and later delete the snapshot. | Verifies ordering and finalizer behavior across source/snapshot dependencies. | Source PV/lvol is handled according to policy; snapshot remains usable until deleted; all snapshot backend resources disappear after final deletion. |
| SN-04 | P1 | Delete a `VolumeSnapshot` while a PVC restore from it is provisioning. | Exercises competing CSI snapshot/volume operations. | Restore either completes consistently or fails cleanly with no orphan; snapshot deletion eventually completes once references are released. |
| SN-05 | P1 | Make a clone, mutate source and clone independently, and verify both after pod recreation. | Confirms no accidental shared writable backend state. | Each volume retains only its own post-clone mutation. |
| SN-06 | P1 | Clone a source PVC while the source is mounted, where that configuration is advertised as supported. | The current test deliberately unmounts first; this records the live-clone contract. | If supported, clone binds with a consistent snapshot of source data; otherwise, provisioning fails with a documented, actionable event and no orphan. |
| SN-07 | P1 | Snapshot an XFS volume and a raw-block volume, restore each, and verify checksums. | Current snapshot coverage is only the default filesystem. | Both restores are usable with the correct volume mode/filesystem and preserve data. |
| SN-08 | P1 | Create several snapshots of one source and list/delete them in non-creation order. | Exercises identity, pagination, and reference accounting visible through the Kubernetes snapshot API. | Each snapshot maps to the correct content, deletion removes only its own backend snapshot, and remaining restores work. |
| SN-09 | P2 | Create a snapshot from a nonexistent, unbound, or wrong-driver PVC reference. | Validates CSI error mapping and absence of partial snapshot content. | `VolumeSnapshot` is not ready and exposes a clear error; no backend snapshot is created. |
| SN-10 | P2 | Restore from a nonexistent/deleted `VolumeSnapshot` or request less capacity than its restore size. | Covers invalid data-source and size admission/provisioner paths. | Destination PVC remains `Pending` or is rejected with a precise event; no destination lvol leaks. |
| SN-11 | P2 | Restart the CSI controller while `VolumeSnapshotContent` is pending creation or deletion. | Verifies idempotent snapshot operations across controller failover. | Exactly one backend snapshot is associated; deletion eventually removes it and no finalizer remains stuck. |

## Deletion, reclaim policy, and static volumes

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| DL-01 | P0 | Create a dynamic PVC with `Retain`, write data, delete its PVC, bind a replacement PVC to the released PV, and read it. | `Delete` is covered; Retain is a different user-data safety contract. | PV becomes `Released` (or documented retained state), backend lvol remains, and replacement claim can safely recover the data. |
| DL-02 | P0 | Bind a documented static NVMf PV/PVC, write data, delete the claim and PV. | The static-volume documentation promises the driver must not delete the underlying lvol. | Static PV is usable; backend lvol still exists with its data after Kubernetes resources are removed. |
| DL-03 | P1 | Delete the PVC while its pod still uses it, then terminate the pod. | Validates PV-protection/finalizer ordering. | PVC/PV are protected or terminate only after unpublish; workload I/O remains valid until stopped; backend cleanup then completes. |
| DL-04 | P1 | Delete a dynamically provisioned PVC during active continuous I/O, then stop its pod. | Finds deletion races between controller, kubelet, and backend. | No premature data-path removal while mounted; after consumer exit, PV/lvol are deleted without stuck resources. |
| DL-05 | P1 | Restart controller during dynamic PVC deletion and snapshot deletion. | Tests idempotent `DeleteVolume` and `DeleteSnapshot`. | Resources reach deletion exactly once; no finalizer, PV, `VolumeSnapshotContent`, or backend object leaks. |
| DL-06 | P2 | Simulate a transient backend delete failure. | Ensures retry behavior does not falsely claim cleanup. | Kubernetes resource remains terminating/retryable with event/log evidence; once backend recovers, deletion completes. |

## StorageClass parameters, security, and validation

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| PA-01 | P0 | Verify QoS parameters and PVC overrides in the backend object, not just a writable mount. Cover `qos_rw_iops`, `qos_rw_mbytes`, `qos_r_mbytes`, and `qos_w_mbytes`. | Current tests prove parameter acceptance but not that the backend enforced the requested values. | `sbctl`/API returns the expected limits, with PVC override taking precedence where documented. |
| PA-02 | P1 | Supply invalid QoS values (non-numeric, negative, conflicting units) and invalid boolean/integer parameters such as `encryption` and `max_namespace_per_subsys`. | Validates user-facing parameter errors and cleanup. | PVC cannot provision and has a precise event; no lvol is created. |
| PA-03 | P1 | Provision with `max_namespace_per_subsys > 1`; attach two PVCs and inspect their shared subsystem/namespace layout. | Exercises a material data-path configuration and provides a baseline for guardian behavior. | Both volumes are independently usable; backend/NVMe topology matches configured sharing; teardown of one does not break the other. |
| PA-04 | P1 | Induce total path loss for one volume in a shared subsystem with guardian opt-in. | The guardian intentionally has a safety guard for shared subsystems; it must not restart workloads that would disrupt peers. | Guardian emits its skip reason/event, does not restart the workload, and the peer workload remains untouched. |
| PA-05 | P1 | Provision encrypted volume, inspect backend metadata/state through a privileged test endpoint, then snapshot, clone, expand, and remount it. | A mount alone cannot prove encryption was requested or retained across operations. | Backend reports encryption enabled; all derived/resized volumes follow documented encryption semantics and retain data. |
| PA-06 | P1 | Use a StorageClass with a missing/invalid pool name and a pool that is unavailable. | Separates configuration faults from generic provisioning timeouts. | PVC remains unbound with an actionable provisioning event; no lvol is created in another/default pool. |
| PA-07 | P2 | Request `max_namespace_per_subsys=0`, a huge value, and malformed `zone_cluster_map`/`region_cluster_map` JSON. | Tests parameter-boundary validation at the driver edge. | CreateVolume fails with `InvalidArgument`-equivalent event; no fallback to a wrong configuration occurs. |
| PA-08 | P2 | Toggle a pool/StorageClass setting that affects allowed initiators or topology, then provision from allowed and disallowed worker nodes. | Validates that access restrictions are honored in data-path connection setup. | Allowed workload mounts; disallowed workload fails safely without exposing a device or corrupting volume state. |

## Topology and multi-cluster

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| MC-01 | P0 | Use one `zone_cluster_map` StorageClass and provision one delayed-binding PVC from each mapped zone. | Current test uses separate class names; this verifies the advertised single-class mapping behavior. | Each PV's `cluster_id` and backend lvol belong to the cluster mapped for its scheduled zone. |
| MC-02 | P1 | Repeat MC-01 with `region_cluster_map` across two regions. | Region mapping has distinct resolver logic. | Each PV is created in the cluster mapped to the selected region. |
| MC-03 | P1 | Schedule a delayed-binding consumer in a zone/region absent from the mapping. | Prevents silently provisioning to an incorrect default cluster. | PVC remains unbound with a topology/mapping error; no backend lvol is created. |
| MC-04 | P1 | Supply a mapping to a cluster absent from the CSI secret/configuration. | Verifies credential/config isolation and clear failure behavior. | Provisioning fails with an actionable error and does not fall back to another configured cluster. |
| MC-05 | P1 | Create a volume in cluster A, restart its pod on another worker in the same permitted topology. | Verifies node-side connection selection and publish across workers. | Pod remounts, reads checksum, and connects only to cluster A. |
| MC-06 | P1 | Update the multi-cluster Secret/ConfigMap to add a cluster, wait for reload, and provision using a new mapping. | The documentation says configuration can be updated dynamically. | Newly mapped workload provisions on the new cluster without restarting the CSI deployment; existing volumes remain usable. |
| MC-07 | P2 | Remove or corrupt a now-unused cluster credential while workloads on another cluster run. | Ensures one bad backend config does not globally break the driver. | Existing unrelated workloads retain I/O; new requests to bad cluster fail clearly. |
| MC-08 | P2 | Use a StatefulSet with anti-affinity across two zones and a shared mapped StorageClass. | Exercises real scheduler topology input and multiple delayed provision requests. | Each replica's PVC is provisioned in the matching cluster and all replica checksums are isolated. |

## Attach, node lifecycle, and reconnect

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| RC-01 | P0 | Run continuous checksummed I/O while deleting one path from a managed multipath volume. | Existing reconnect validates a prewritten marker after recovery; this proves no live I/O interruption/corruption. | I/O has no errors or checksum mismatch; lost path returns to original live-path count. |
| RC-02 | P0 | Run continuous I/O while all paths are lost, then recover via republish/guardian. | Establishes the documented behavior during total loss and the integrity result after recovery. | Workload is replaced/recovered per mode; recovery is bounded; post-recovery checksum matches the last fully synced write. |
| RC-03 | P1 | Restart the `csi-node` pod serving an active volume. | Common node-plugin lifecycle event; validates mount reconstruction. | Workload either continues or recovers within a bounded window; data is intact and no stale mount/device persists. |
| RC-04 | P1 | Restart the CSI controller while provisioning, attaching, snapshotting, and resizing separate PVCs. | Tests leader-side recovery of all externally provisioned CSI operations. | Each operation eventually has one correct terminal result; no duplicate backend entities or stuck Kubernetes objects. |
| RC-05 | P1 | Cordon/drain the workload node, allow rescheduling to another node, then verify data and old-node cleanup. | Exercises `NodeUnpublish`/`NodeUnstage` plus fresh attach/stage/publish in the real eviction flow. | Replacement pod mounts and passes checksum; old node has no stale target mount/controller; no multi-attach conflict remains. |
| RC-06 | P1 | Force a backend endpoint/network outage during initial publish, restore it, and observe kubelet retry. | Initial connection failure must be recoverable and must not present a half-mounted volume. | Pod remains unready with meaningful events while unavailable; becomes ready and writable after recovery without PVC recreation. |
| RC-07 | P1 | Break the device path after staging but before a same-node replacement pod publishes. | Directly exercises the driver's `healVolumeBeforePublish` / restage path. | Replacement pod restages/reconnects and reads checksum; temporary staging artifacts are cleaned. |
| RC-08 | P2 | Use a single-path environment and induce its only path loss. | Existing degraded-path test skips single-path systems; this establishes supported behavior explicitly. | Recovery follows documented single-path semantics (retry/restart/failure), never reports a false healthy mount, and recovers after endpoint restoration if supported. |
| RC-09 | P2 | Reboot or power-cycle a storage target node during active I/O in a fault-tolerant cluster. | Validates the complete backend/CSI recovery path rather than local controller deletion. | I/O follows the service's availability contract; paths reconnect/rebalance as appropriate and data remains consistent. |

## Negative, safety, and observability scenarios

| ID | Pri. | Scenario and setup | Why it exists / what it proves | Expected outcome |
|---|---:|---|---|---|
| NG-01 | P0 | Attempt to mount the same RWO volume from two different nodes (PR-06), then delete the first pod and retry the second. | Confirms both rejection and recovery from an attachment conflict. | Second pod initially reports multi-attach; after first unpublishes it becomes ready with intact data. |
| NG-02 | P1 | Create a PVC from a `VolumeSnapshot` belonging to another namespace. | Kubernetes snapshot data sources are namespace-scoped; validates no cross-tenant access. | Request is rejected or remains unbound with a clear reference error; no cross-namespace lvol is created. |
| NG-03 | P1 | Use a `VolumeSnapshotClass` for another driver or a nonexistent class. | Prevents accidental handoff to the wrong snapshot provider. | Snapshot never becomes ready and reports driver/class error; no Simplyblock snapshot is created. |
| NG-04 | P1 | Attempt to provision while controller credentials are invalid/rotated incorrectly; restore valid credentials. | Ensures authentication failures do not become silent data-path errors. | PVC gets an authentication-related provisioning event and no lvol; after restoration a retry succeeds once. |
| NG-05 | P1 | Remove a node-plugin's privilege/resource prerequisite or simulate missing NVMe tooling on one worker, then schedule a workload there. | Validates clear node-stage failure and isolation to the affected node. | Pod fails to mount with diagnostic event/log; no false success; workloads on healthy nodes still work. |
| NG-06 | P1 | Verify the event and condition chain for provision, attach, mount, resize, snapshot failure, and reconnect failure. | Tests that operators can diagnose real incidents without inspecting implementation logs. | Each injected failure produces a reasoned Kubernetes event/condition and retries stop or resume according to error class. |
| NG-07 | P2 | Run a namespace deletion containing PVC, pod, snapshot, clone, and pending provisioning operations. | Namespace teardown is a frequent leak source in CI and tenant removal. | Namespace completes deletion; cluster-scoped PV/content and backend lvol/snapshot artifacts obey reclaim policy and do not leak unexpectedly. |
| NG-08 | P2 | Delete/recreate a StorageClass with the same name but changed parameters while old PVs remain. | Ensures PV `volumeAttributes` preserve immutable provisioning context. | Existing volumes continue to mount with their original attributes; new PVCs use only new parameters. |

## Implementation order

1. **P0 first:** PR-01, PR-03, FS-01, FS-05, FS-07, FS-08, SN-01, SN-02,
   DL-01, DL-02, MC-01, RC-01, RC-02, and NG-01.
2. Add reusable helpers before individual specs: unique resource naming,
   checksum/continuous-I/O workload, backend lvol/snapshot lookup, event
   assertions, PV/PVC waiters, and node mount/NVMe inspection.
3. Put RC-02/RC-09 and backend fault injection in a serial `Disruptive` label;
   leave CRUD, filesystem, snapshot, and parameter coverage parallel-safe.
4. Treat all resource cleanup as test assertions.  A failed cleanup should
   collect PVC/PV/snapshot events, CSI controller and node logs, mount/NVMe
   state, and backend lvol/snapshot inventory before the next disruptive test.

## Non-goals and prerequisites

- Do not claim RWX behavior unless the driver advertises it; use RWO safety
  tests instead.
- Do not make throughput thresholds a correctness gate in ordinary E2E.  QoS
  tests should verify backend configuration; performance enforcement needs a
  calibrated, isolated benchmark lane.
- Static-volume scenarios require a test-owned lvol and the connection metadata
  described in [`static-pvc.md`](../../../csi-driver/docs/static-pvc.md).
- Multi-cluster scenarios require two independently healthy configured clusters,
  distinct zone/region-labelled workers, and `MULTI_CLUSTER_E2E=true`.
- Fault scenarios require an approved injection mechanism (target isolation,
  network policy/iptables, or a controllable test backend).  They must never
  run against shared production storage.
