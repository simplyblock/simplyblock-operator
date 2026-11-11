# Design Document: Warm-Standby pNFS Metadata Server

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-10-11 (last updated 2026-10-19)  
**Related Design:** [`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md), which this design amends: it replaces the single-replica workload, the state PVC, and the guest's identity, and leaves the guest, the runner, the export assembly, and the client path unchanged. [`design-pnfs-rwx.md`](design-pnfs-rwx.md) §13 states the invariant this design enforces. [`design-pnfs-striped.md`](design-pnfs-striped.md) §2.1 assembles a striped export from several namespaces in the same guest, each of which is fenced here.  
**Test Plan:** [`tests/test-plan-pnfs-mds-standby.md`](../tests/test-plan-pnfs-mds-standby.md)

---

## Phase 0: External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                                                                                                                                                                                                                                                              | Kind                                       | Blocks                                                                                          | Status                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------|-------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | A guest kernel patch making the MDS reservation key a per-instance, runtime-writable `nfsd` parameter, with the four behaviors of §6.3                                                                                                                                                                                                                                                                                                                    | Node OS (guest)                            | Fencing (§6), and so every takeover                                                             | Not started. A pnfs-os patch on top of the current series (0001 to 0022, 0006 dropped), against the guest's 6.18.55 kernel                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| P0-2 | The operator adds every MDS pod's host NQN to the allowed hosts of each export volume and of the state volume, through the control plane's allowed-hosts API (`sbcli` `add_host_to_lvol`, `remove_host_from_lvol`), and gives each guest its own host's DH-CHAP secret for a volume that uses DH-CHAP                                                                                                                                                     | Operator, on an existing control-plane API | A standby's promotion (§7.3)                                                                    | The control-plane API exists and serves DH-CHAP's allowed hosts. `atlas-lib/controlplane` has no add-host call yet                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| P0-3 | The target accepts Reservation Register, Replace, Acquire with Preempt, and Report with the extended data structure (each registrant's host identifier) on a volume's namespace                                                                                                                                                                                                                                                                           | Storage plane (SPDK)                       | Fencing (§6.4) and the collision check                                                          | Verified in the code of simplyblock's SPDK fork (`lib/nvmf/subsystem.c`): all are implemented, the Report requires the extended data structure, registrants are keyed by host identifier, and Preempt and Abort returns Invalid Field                                                                                                                                                                                                                                                                                                                       |
| P0-4 | At least `1 + standbys` nodes carry `storage.simplyblock.io/kvm-capable=true`                                                                                                                                                                                                                                                                                                                                                                             | Node OS                                    | Each standby (§5.1)                                                                             | Required of the deployer for HA, stated in the user docs. A standby without a node stays `Pending`                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| P0-5 | A volume's reservation state is shared by every storage node serving the volume (primary, secondary, and tertiary) and survives the replacement of a storage node's SPDK pod. Today, each SPDK process keeps its own: a host sends reservation commands down one path, the secondaries' `non_optimized` listeners accept I/O with no reservation, and the persist-through-power-loss file `/mnt/ns_resv<id>.json` lives in the container's writable layer | Storage plane (SPDK, `sbcli`)              | Fencing across a change of the volume's primary node or an SPDK pod replacement (§6.1, §6.6)    | Not started. The state is to be kept as an attribute of the volume's blob in the shared lvstore, written by the leader and reloaded wherever a node becomes leader, including the data plane's own promotion that bypasses `sbcli`. Reservation commands that arrive on a non-leader are forwarded to the leader, which pushes the resulting state, tagged with its term, to every node serving the volume, so each node enforces it. SPDK's `spdk_nvmf_set_custom_ns_reservation_ops` hook needs an asynchronous `update` and a reload for live namespaces |
| P0-6 | SPDK accepts more than 16 registrants per namespace (`SPDK_NVMF_MAX_NUM_REGISTRANTS`). A client never unregisters on SPDK, which rejects the Replace with a zero key the Linux client sends on release, so every node that opened a volume keeps its slot                                                                                                                                                                                                 | Storage plane (SPDK)                       | RWX volumes on more than about 14 nodes, and a takeover's registration on a full namespace (§9) | Implemented on the SPDK fork's branch `fix/spdk-nvmf-reservations`, not merged: the limit is 64 and enforced, a Replace with a zero key from a registered host unregisters it, and the persisted reservation file is written whole, where it was cut to its last 4 KiB                                                                                                                                                                                                                                                                                      |
| P0-7 | The target accepts Reservation Acquire with Preempt and Abort, and completes it only once no command of a preempted host can still execute on the namespace                                                                                                                                                                                                                                                                                               | Storage plane (SPDK)                       | Closing the window of §6.6                                                                      | Upstream SPDK master implements it: in-flight commands of the preempted hosts are waited for, reservation commands on a namespace are serialized, and after 10 seconds the command fails with an error rather than completing. Upstream's series is being backported to the fork's branch `fix/spdk-nvmf-reservations`, in place of the branch's own implementation                                                                                                                                                                                         |

Without P0-1 there is no fencing, and the design cannot ship: a takeover without it is the double mount `design-pnfs-rwx.md` §13.2 forbids. Without P0-2 a standby cannot connect any volume, its promotion fails on the first device, and the cluster runs as if `standbys` were 0, with an `MDSFenceFailed` event naming the refusal. Without the extended report of P0-3 the collision check of §6.2 cannot run, and the agent refuses every promotion rather than skip it. Without P0-4 a standby stays `Pending`, the active serves as before, and an `MDSStandbyMissing` event says so while no standby runs. Without P0-5 a fence holds only on the volume's current primary storage node (§6.1). Without P0-6 a takeover on a namespace that already has 16 registrants cannot register the new key, which is a fence failure. Without P0-7 the agent fences with a plain Preempt, and the window of §6.6 stays open.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Changes](#4-api-changes)
5. [The Workload](#5-the-workload)
6. [Fencing](#6-fencing)
7. [Takeover](#7-takeover)
8. [Control Channel](#8-control-channel)
9. [Failure Modes](#9-failure-modes)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `simplyblockdriver_types.go`](#appendix-a-simplyblockdriver_typesgo)

---

## Overview

Each storage cluster runs one active MDS guest and, by default, one standby, each on its own node. The active has the state volume and every export mounted, and its nfsd serves. A standby has booted, its agent answers, and it holds no mount, no NVMe connection, and no running nfsd. When the active is lost, the operator promotes a standby. The promoted guest fences every device first, by taking the persistent reservation away from every earlier MDS key, and only then mounts the state volume, starts nfsd, and reassembles the exports. To clients this is a server restart, which they recover from as `design-pnfs-rwx.md` §13.4 describes.

The client-recovery database moves from a Kubernetes RWO volume to a simplyblock volume that the guest connects over NVMe-oF itself, as it does for export namespaces. No Kubernetes volume attach is involved, so a pod stuck `Terminating` on a dead node does not hold anything a standby needs.

Fencing is what prevents a double mount. The record and the events decide which guest is promoted, and do not need to be correct for the data to be safe. Each promotion gets a new epoch, recorded on the workload with optimistic concurrency, and each epoch a reservation key that no earlier guest holds. A guest mounts a device only after it holds that device's reservation under its own key, and a fenced guest has no code path that registers again. A partitioned old active that resumes has its writes refused by the target, whether or not it knows it was replaced.

---

## 1. Background

`design-pnfs-mds-vm.md` runs the MDS as a single-replica StatefulSet with a RWO state PVC, and recovers by restarting it. That covers a pod or guest crash, a drain, and an image change. It does not cover a lost node: the pod stays `Terminating` until the API server force-deletes it, the StatefulSet does not start a second pod of that name meanwhile, and the state PVC stays attached to the dead node. Exports are unserved for that whole time (`design-pnfs-mds-vm.md` §9, "MDS pod's node is lost"), and the bound is the node's eviction, not anything the operator controls.

Restarting faster does not fix this, because the obstacle is the identity of the second instance. Kubernetes refuses a second pod of the same name and a second attach of the same RWO volume, and both refusals exist to prevent the double mount, which this design prevents by fencing instead.

The guest's nfsd already fences clients. pnfs-os patch 0002 preempts reservation keys left by clients of an earlier boot when it registers a device, and 0018 does the same for every device of an export when the export is set up (`nfsd4_scsi_claim_export`), before the export is published. Neither fences an earlier server: the MDS key is a constant, `NFSD_MDS_PR_KEY = 0x0100000000000000ULL` in `fs/nfsd/blocklayout.c`. nfsd registers it with `pr_register(bdev, 0, NFSD_MDS_PR_KEY, true)` and reserves with `pr_reserve(..., PR_EXCLUSIVE_ACCESS_REG_ONLY)`, at export setup with 0018 and again at the first GETDEVICEINFO, and the stale-key fence of both (`nfsd4_scsi_fence_stale_keys`) skips `key == NFSD_MDS_PR_KEY`. Two guests therefore present the same key, and neither can tell the other's registration from its own.

The primitives reused are the guest, runner, and assembler of `design-pnfs-mds-vm.md`, the metadata server's Service and EndpointSlice and the conntrack forget of its §8.1, the resync of its §7.5, the path repair of its §7.6, the mds-proxy of its §8.5, the control-plane client in `atlas-lib/controlplane` (`CreateVolume`, `DeleteVolume`), and csi-link.

---

## 2. Goals and Non-Goals

### Goals

- A storage cluster's exports are served again after the loss of the active's node, without waiting for the node or for its pod to be deleted.
- No two guests ever have the same filesystem mounted with write access to it, under any interleaving of failure, partition, operator restart, or concurrent reconcile. The storage target enforces it, so it holds when the operator's view is stale.
- A promotion whose fencing fails on any device mounts nothing.
- A brief API server or operator outage does not cause a failover.
- Clients recover through the same path as a restart: reclaim in grace, the layout hold, and the re-probe. No client remounts.
- Every takeover, held decision, fence result, and missing standby is an event, and the takeover's phases are measured.

### Non-Goals

- **Active/active sharding.** Exports of one storage cluster are served by one guest at a time. Splitting them across guests is a later design.
- **Moving a guest without a server restart.** Every takeover is a server restart to the clients. Live migration of the guest is not provided.
- **NFSv4.1 persistent sessions.** A takeover loses sessions, and requests in flight are retried by clients under a new session.
- **A migration path.** Nothing has shipped, so the state PVC, its fields, and the single-replica workload are replaced outright.
- **Fencing clients.** Client keys are fenced by nfsd as before (0002). This design fences servers.

---

## 3. Architecture Overview

```
┌──────────────────────────────────── operator namespace ───────────────────────────────────┐
│                                                                                           │
│  operator (leader)                                                                        │
│  ┌───────────────────────────────────────────┐                                            │
│  │ NFSExportReconciler                       │   StatefulSet <mds> (1 + standbys replicas)│
│  │ 1. ensure StatefulSet and state volume    │   annotations: mds-active, mds-epoch,      │
│  │ 2. detect the active's loss (§7.2)        │   mds-active-instance, mds-state-volume    │
│  │ 3. write the active record (CAS)          │                                            │
│  │ 4. Promote over csi-link                  │                                            │
│  │ 5. per export: mdsPodName, CreateExport;  │   Service <mds>-nfs, one EndpointSlice     │
│  │    EndpointSlice, conntrack forget        │   endpoint: the active's pod IP            │
│  └──────────────────┬────────────────────────┘                                            │
│                     │ csi-link                                                            │
│        ┌────────────┴────────────────┐                                                    │
│  ┌─────▼─────────────────────┐ ┌─────▼─────────────────────┐                              │
│  │ <mds>-0 (node A)  ACTIVE  │ │ <mds>-1 (node B)  STANDBY │                              │
│  │ guest: nfsd serving       │ │ guest: booted, agent up   │                              │
│  │  state volume mounted     │ │  nothing connected        │                              │
│  │  exports mounted          │ │  nfsd not started         │                              │
│  │  key K(epoch n)           │ │                           │                              │
│  └─────────────┬─────────────┘ └───────────────────────────┘                              │
└────────────────┼──────────────────────────────────────────────────────────────────────────┘
                 │ NVMe/TCP from the active guest only
┌────────────────▼──────────────────────────────────────────────────────────────────────────┐
│ Simplyblock storage cluster                                                               │
│  export namespaces and the state volume, each reserved by K(epoch n)                      │
│  POST /api/v2/clusters/{id}/storage-pools/{id}/volumes   (state volume, once)             │
└───────────────────────────────────────────────────────────────────────────────────────────┘
```

**The operator decides, and the target enforces.** The operator is the only writer of the active record and the only caller of `Promote`. The storage target's reservation is what stops a second writer, and it holds whether or not the operator's view is current.

**Roles are not pod ordinals.** Either ordinal can be active. A guest always boots as a standby, and only `Promote` makes it active.

---

## 4. API Changes

### 4.1 `DriverPNFSMDS` (existing type, on `SimplyblockDriver`)

`stateSize` and `stateStorageClassName` are removed, since the state volume is no longer a PVC (§5.4). One field is added:

```go
// Standbys is how many standby guests run beside the active one, each on
// its own node. A standby takes over when the active is lost, in about the
// time a restart takes. At 0, exports stay unserved after the loss of the
// active's node until the lost pod is deleted and started again elsewhere.
// Each standby needs another node offering KVM, and one without such a node
// stays pending while the active serves.
// +kubebuilder:validation:Minimum=0
// +kubebuilder:validation:Maximum=4
// +kubebuilder:default=1
// +optional
Standbys *int32 `json:"standbys,omitempty"`
```

| Field      | Type    | Default | Description                                                                                                                        |
|------------|---------|---------|------------------------------------------------------------------------------------------------------------------------------------|
| `standbys` | `int32` | `1`     | Standby guests beside the active one, 0 to 4. The StatefulSet runs `1 + standbys` replicas, and at `0` one guest restarts in place |

**Why one standby is the default.** The constraint is P0-4: each standby needs its own KVM-capable node. A cluster without a second such node loses nothing to the default: the standby stays `Pending`, the active serves, and an `MDSStandbyMissing` warning says why. A cluster with one is protected against the unbounded outage of §1 without anyone setting a field. A second standby keeps the cluster protected while the first is down or being rolled, at the cost of one more pod's reservation (§5.5) and one more host on every volume (P0-2), which is what the maximum of 4 bounds.

The field is reconfigurable. Raising it scales the StatefulSet up, and every new pod boots as a standby. Lowering it removes the highest ordinals, as a StatefulSet scale-down does. When the active is on one of them, the operator first switches over to a standby on a remaining ordinal (§7.4), then scales down.

The chart carries the field as `driver.pnfs.mds.standbys` beside the scheduling, resources, and data network it already renders under `driver.pnfs.mds` (`design-pnfs-mds-vm.md` §4.2), and drops `driver.pnfs.mds.stateSize` with the field it set. A chart install, which renders `mds: {}` by default, therefore runs one standby wherever a second eligible node exists.

`dataNetwork` (`design-pnfs-mds-vm.md` §8.4) is unchanged in shape. Every pod takes one address from its range, so the range holds at least `1 + standbys` addresses, and a pod for which whereabouts has none left does not start (§5.1).

The full type is Appendix A.

### 4.2 `NFSExportStatus` (unchanged)

No field changes. `status.mdsPodName` keeps its meaning, the pod serving the export, and is rewritten to the new active on every takeover. It stays the per-export serialization point of `design-pnfs-rwx.md` §13.2: a pod becomes a candidate for an export only when that field names it. `status.assembledBy` keeps the format of `design-pnfs-mds-vm.md` §7.5, and a takeover rewrites it after the new active reassembles the export.

### 4.3 The active record

The active instance and its epoch are annotations on the storage cluster's MDS StatefulSet:

| Annotation                                   | Value                                                                     |
|----------------------------------------------|---------------------------------------------------------------------------|
| `storage.simplyblock.io/mds-active`          | the active pod's name, empty before the first promotion                   |
| `storage.simplyblock.io/mds-active-instance` | the active's pod UID and runner container ID, in the `assembledBy` format |
| `storage.simplyblock.io/mds-epoch`           | the current epoch, a decimal integer, `1` at the first promotion          |
| `storage.simplyblock.io/mds-state-volume`    | the state volume's handle (§5.4)                                          |

The first three are written in one update conditioned on the StatefulSet's `resourceVersion`. A conflict means another writer moved the record, and the writer that lost does not promote.

**Why annotations on the StatefulSet and not a status field.** No kind exists per storage cluster's MDS. `SimplyblockDriver` status is one object for every cluster the driver serves, so a status list there puts each cluster's failover in conflict with writes for the others, and that status already has a writer in the driver controller. The StatefulSet is per cluster, operator-owned, and its lifetime is the lifetime of the record: deleting it ends every guest and the epoch together. Its other writer, the template-hash update of `design-pnfs-mds-vm.md` §5.1, uses the same conditioned update and retries a conflict. A record a user can edit is not trusted for safety: the epoch only selects the key, and §6.2's collision check refuses a promotion whose key another host already holds.

---

## 5. The Workload

### 5.1 StatefulSet

The StatefulSet of `design-pnfs-mds-vm.md` §5.1 keeps its name, owner, and template hash, and changes in four ways:

- `replicas: 1 + standbys`.
- `podManagementPolicy: Parallel`, so a pod stuck on a lost node does not stop the other ordinal from being created or restarted.
- A required pod anti-affinity on `kubernetes.io/hostname` against the StatefulSet's own pods, in addition to the `kvm-capable` node selector. Two guests on one node are lost together with it.
- `updateStrategy: OnDelete`. The operator rolls the pods itself (§7.4), because the StatefulSet controller's rolling update restarts ordinals in a fixed order regardless of which one is active.

No `volumeClaimTemplates`. The per-cluster state StorageClass, its `ValidatingAdmissionPolicy`, and the PVC permission of `design-pnfs-mds-vm.md` §5.3 and §7.8 are removed.

Every pod shares one template, so the scheduling of `design-pnfs-mds-vm.md` §5.4 applies to each guest alike. With `dataNetwork` set, each pod requires a node carrying the subnet on the chosen interface and prefers one without a storage node, and the operator attaches the template to the data network when at least one eligible node carries the subnet, as for a single pod. A standby for which no further node on the subnet exists, or for which the range holds no free address, stays `Pending` or fails its sandbox, and `MDSStandbyMissing` says so while no standby runs. The mds-proxy DaemonSet already runs on every node the template admits, so a standby's node has a proxy before the standby is promoted.

The metadata server's Service, `<mds>-nfs`, and its EndpointSlice stay one per StatefulSet and owned by it (`design-pnfs-mds-vm.md` §8.1). The Service has no selector, so the pods' readiness does not decide its endpoint: the EndpointSlice names the active's pod IP and nothing else, and the operator rewrites it on every takeover (§7.3). Both names derive from the StatefulSet's name. `utils.MDSServiceName` derives them from the pod's name by trimming its `-0`, which names another Service for any other ordinal, so it takes the StatefulSet's name instead.

### 5.2 Host identity

Each pod has its own host NQN, so the control plane authorizes, and the target records, a distinct host per ordinal:

```
hostNQN(ordinal) = nqn.Host(uuid.NewSHA1(<StatefulSet UID>, "<ordinal>"))
```

The seed is a name-based UUID because `nqn.HostUUID` requires a UUID and the host identifier is derived from it. It is stable across pod restarts and changes when the StatefulSet is recreated, which keeps `design-pnfs-mds-vm.md` §6.5's reasoning. The operator computes it from the pod's owner reference and ordinal and sends it with every call. The target keys a registration by the host identifier, which `atlas-lib/nvmeof` derives from the host NQN's UUID, so each ordinal is its own registrant, and the same one across that pod's restarts.

Every guest boots with the same hostname, the StatefulSet's name, rather than the pod name. nfsd derives its server owner and scope from the hostname, and NFSv4.1 clients reclaim only from a server whose identity did not change, so a client of `<mds>-0` reclaims on `<mds>-1` exactly as it would on a restarted `<mds>-0`. Only the active's nfsd runs, so the shared name never answers twice. This replaces the pod-name hostname of `design-pnfs-mds-vm.md` §6.6.

### 5.3 Roles and boot

Every guest boots as a standby: systemd starts the agent and does not start `nfs-server.service`, mount `/var/lib/nfs`, or connect anything. The agent reports its role and epoch through `Describe` (§8). The readiness probe is the agent's health, which is `SERVING` for a booted standby and, once active, additionally requires the state volume mounted and nfsd threads running. Readiness carries no traffic, because the operator writes the EndpointSlices.

A standby connects no volume ahead of its promotion. Under the reservation an unregistered host cannot read, so the kernel's partition scan of a device connected before promotion fails with reservation conflicts, and every such device is one more controller per volume per standby at the target. The runner of a standby still sets up the guest's network as for any guest (`design-pnfs-mds-vm.md` §8.2, §8.4, §8.5): the bridge, the masquerade, the data network attachment when there is one, and the redirect of connections to its own node through the mds-proxy. Its first connect at promotion therefore reaches every storage node, including the one on its own node.

A guest becomes active only through `Promote`, at most once per boot. A guest that was active and rebooted is a standby again, whatever the record says, and the record's instance no longer matches it.

### 5.4 State volume

The client-recovery database lives on a 1 GiB simplyblock volume per storage cluster, created by the operator through `POST /api/v2/clusters/{id}/storage-pools/{id}/volumes` (`controlplane.Client.CreateVolume`) in the pool of the first export bound to the cluster, and recorded in `mds-state-volume` before any guest is promoted. A recorded handle is never created again. Every pod's host NQN is authorized on it (P0-2).

The guest connects it over NVMe-oF during `Promote`, fences it like an export namespace (§6.4), and mounts it at `/var/lib/nfs`, formatting it ext4 when it carries no filesystem. The runner resolves its connection with the pod's own host NQN, as it does for exports (`design-pnfs-mds-vm.md` §6.4), and the agent keeps that connection for path repair beside the exports' (`design-pnfs-mds-vm.md` §7.6), so a state volume path that drops comes back as an export's does. The size is fixed because `nfsdcld`'s database holds one row per client, and the fields that sized it are gone.

A finalizer on the StatefulSet, `storage.simplyblock.io/mds-state-volume`, deletes the volume (`DELETE /api/v2/clusters/{id}/storage-pools/{id}/volumes/{id}`, a 404 counted as done) when the StatefulSet is deleted, which happens when pNFS is turned off on the driver.

### 5.5 Resources

Each pod takes `spec.pnfs.mds.resources`, so every standby adds one pod's reservation. The guests share one pod template, so a standby requests what the active does. The defaults stay those of `design-pnfs-mds-vm.md` §5.5: a memory request of 512Mi and a memory limit of 2Gi. QEMU peaked at 315 MiB over a pnfs-fio run, below that request, and a booted standby uses less, since the guest's free page reporting returns what it does not touch.

The request is bounded by eviction: under node memory pressure the kubelet evicts pods using more than their request first, and the active, whose working set grows with its clients and exports, is the pod that reaches it. A deployment that cannot accept that sets the request to the active's working set and reserves it for every standby as well.

---

## 6. Fencing

### 6.1 What a reservation guarantees here

nfsd reserves each device `PR_EXCLUSIVE_ACCESS_REG_ONLY`: every registered host may read and write, and an unregistered host may do neither. Clients are registered with their own keys, so they write directly. Fencing a guest therefore means removing its registration, which a Reservation Acquire with Preempt does: it unregisters every host whose key is the preempted key and, when that key held the reservation, moves the reservation to the preempting host.

A fence lasts only while the fenced host does not register again, and NVMe has nothing to prevent that. The guest is fenced reliably only when the guest itself contains no path that registers once fenced. That is what P0-1 requires of nfsd (§6.3), and what the agent guarantees of itself (§6.4).

**The guarantee holds on the storage node that holds the reservation.** The target keeps reservation state per SPDK process, not per volume. A host sends every reservation command down the one path its multipath layer has selected (`nvme_find_path`), so the registrations and the reservation exist only on the storage node behind that path, the volume's current primary. The secondaries' listeners are `non_optimized` and accept I/O, and they hold no reservation, so a fenced guest whose path to the primary fails over writes through a secondary unrefused. A change of the volume's primary node, and the replacement of a storage node's SPDK pod, whose reservation file lives in the container's writable layer, leave the reservation empty or stale on the node that serves next. Fencing that holds across those events is P0-5.

### 6.2 The instance key and the epoch

Each promotion uses a key no earlier guest of the cluster holds:

```
key(epoch) = 0x01 << 56  |  (sha256(<StatefulSet UID>)[0:3] << 32)  |  epoch
```

- The top byte `0x01` keeps the range nfsd's constant key is in. A client key is `(cl_boot << 32) | cl_id` (`nfsd4_scsi_pr_key`), and `cl_boot` is the server's boot time in seconds, so a client key's top byte is `0x01` only for a boot between July 1970 and January 1971. Every key with top byte `0x01` is an MDS key.
- The 24-bit tag separates StatefulSets, so a recreated StatefulSet whose epoch restarts at 1 does not reuse the keys of a guest of the old one still running on a lost node.
- The epoch is the record's `mds-epoch`, incremented by every promotion.

The function lives in `atlas-lib/nfsexport` (`MDSReservationKey`), because the operator computes the key and the agent checks it.

**Collision check.** Before it registers, the agent reads the reservation report with host identifiers and refuses the promotion when a host other than itself is registered with the key it was given. A collision needs two StatefulSets with the same 24-bit tag at the same epoch, or a hand-edited record that repeats an epoch. On a refusal the operator increments the epoch and promotes again.

### 6.3 Guest kernel requirements (P0-1)

A pnfs-os patch adds the parameter `nfsd.pnfs_mds_pr_key`, a `u64` writable at runtime, default `0`. At `0`, nfsd behaves as today. When set:

1. The parameter replaces `NFSD_MDS_PR_KEY` wherever nfsd uses it: in the stale-key fence of 0002 and 0018, in 0018's claim of a device at export setup, and as the current key in `nfsd4_scsi_fence_client`, which 0019 also calls before it drops a layout a client never returned.
2. Neither the claim at export setup nor GETDEVICEINFO registers or reserves. Each reads the reservation and goes ahead only when the holder's key is the parameter's value. The claim then runs its stale-key fence alone, and an export whose device the guest does not hold is not published. GETDEVICEINFO fails the request. Registration is the agent's (§6.4), so a fenced guest whose nfsd still answers, or that sets an export up again, cannot register itself back.
3. The stale-key fence skips every key whose top byte is `0x01`, not only its own, so a fenced guest never preempts a newer active.
4. A write while nfsd has threads running is refused with `EBUSY`. The agent sets the key before it starts nfsd, and a guest is promoted once per boot.

### 6.4 Fence before mount

For every device the active will mount (the state volume and each export namespace, which for a striped export is each of its members), the agent:

1. Connects the namespace with the pod's host NQN.
2. Reads the reservation report. Refuses on the collision of §6.2, when the holder's key is outside the MDS range, since a client never holds the reservation, or when a key with the agent's tag and a higher epoch than its own is registered. A later promotion has then fenced this guest's epoch, and the agent reports the refusal rather than preempting a newer active.
3. Registers its key with Replace and ignore existing key (`IOC_PR_REGISTER` with a non-zero old key and `PR_FL_IGNORE_KEY`). SPDK answers Register with a conflict for a host already registered under another key, and a pod's host identifier is the same across its reboots, so a guest that was active in an earlier boot is still registered with that boot's key. Replace with ignore existing key registers a host that is not registered and replaces the key of one that is.
4. When another key holds the reservation, acquires with Preempt of that key. When none does, acquires.
5. Preempts every other registered key in the MDS range, so a guest of an earlier epoch that was registered without holding is unregistered too.
6. Reads the report again and confirms that its key holds the reservation and that no other MDS-range key is registered.

Client keys are never touched. The agent preempts with Preempt and Abort (P0-7), and with a plain Preempt when the target answers that with Invalid Field, as SPDK's fork does today (`nvmf_ns_reservation_acquire`). When Preempt and Abort fails with any other error, the preemption has taken effect but the target did not confirm that the preempted host's commands finished, as when its 10-second wait expires. The agent treats that as a failed fence of the device: `Promote` mounts nothing and the operator retries with backoff. The retry reads the report again, finds the old key already gone, and preempts only what is still registered. Nothing re-establishes the wait for the earlier key, so a command still stuck at the target is the exposure of §6.6 again, narrowed to commands that outlived the 10 seconds and the retry's backoff.

A promotion fences every device before it mounts any. When any step fails on any device, `Promote` returns the device and the step, mounts nothing, starts no nfsd, and leaves the guest a standby. Devices already fenced stay fenced: the old active has lost them, which is the intended outcome of a takeover whatever happens next.

**A striped export** (`design-pnfs-striped.md` §2.1) is fenced member by member, before its volume group is activated. Activation reads LVM's metadata from every member, which an unregistered host cannot read, and the volume group's `system_id` is the same on every guest, so it does not keep a second guest from activating the group. The fence does. After activation, nfsd's holder check of §6.3 reads the reservation through the device-mapper device, whose persistent-reservation operations reach the members only while its table has one target (`design-pnfs-striped.md` §2.2), and whose read answers from the first member that answers (`dm_pr_read_reservation`). The check therefore sees one member. It keeps a fenced guest from registering again, and the exclusion itself is the target's: a fenced guest's writes are refused on every member the fence reached, and the agent's report check of step 6 reads each member, so a member the fence missed fails the promotion before the group is activated. Whether the guest's 6.18.55 kernel carries device-mapper's reservation read is Open Question 5.

`CreateExport` on an active guest fences a new export's namespace the same way before it makes the filesystem. A `CreateExport` for an export the guest already holds, as the runner's endpoint refresh sends when a volume's published endpoints change (`design-pnfs-mds-vm.md` §7.6), reconnects the paths and fences nothing. On a standby `CreateExport` is refused, so no reconcile, however stale, can make a standby mount. The reservation calls are a new `atlas-lib/nvme` primitive, register, acquire, and report through the block layer's persistent-reservation ioctls and an NVMe passthrough Reservation Report, since the guest agent is its first user and the CSI node plugin's client keys are a likely second.

### 6.5 What a fenced guest does

A fenced guest's writes to the namespace fail with a reservation conflict. Its filesystem sees I/O errors on its next metadata write or log flush, and XFS is expected to shut the filesystem down, after which nfsd answers its clients with errors (Open Question 2). Its clients are not on it by then, because the EndpointSlice was moved and their flows forgotten (§7.3), and their data path never went through it. Its reads also fail, since `PR_EXCLUSIVE_ACCESS_REG_ONLY` excludes unregistered readers.

Its repair loops keep running. The agent reconnects the paths of every namespace it holds, and the runner keeps refreshing the endpoints of every export it assembled (`design-pnfs-mds-vm.md` §7.6). Neither registers: a connect makes no reservation call, a `CreateExport` for an export the guest holds fences nothing (§6.4), and the export setup it may cause fails the holder check of §6.3 rather than claiming the device.

### 6.6 Residual exposure

A plain Preempt does not abort commands already queued at the target from the preempted host. A write the old active issued before the preempt and the target executes after it reaches the device. The window is the target's queue depth for that host at the instant of preemption, measured in milliseconds, and the new active's mount follows the preemption, so XFS log recovery on the new active sees the device after those writes. The case is pinned by a test (I-09 in the test plan). Preempt and Abort closes it (P0-7): the target completes it only after the preempted host's commands on the namespace have finished, so the new active's mount follows every write the old active issued.

Until P0-5, the fence also holds only on the volume's primary storage node (§6.1). A fenced guest whose path fails over to a secondary, or that reaches a primary whose reservation was lost with its SPDK pod, is not refused there. The agent's fence and the old active's withdrawal (§7.3 Step 3) make a write from it unlikely, and nothing on the target excludes it.

---

## 7. Takeover

### 7.1 State

The takeover's progress is the active record (§4.3) and each export's status. The record is the write-ahead: once it names the new active at epoch `n+1`, every later pass promotes that pod at that epoch until it is active, and an operator restart at any point resumes from there. `Promote` is idempotent for the epoch the guest has accepted.

```
  active lost (§7.2), or active pod deleted, or first export bound
    │
    ▼
  Record        ← CAS write: mds-active=<target>, instance, mds-epoch=n+1
    │  write succeeded (a conflict ends the pass: no Promote)
    ▼
  Withdraw      ← the metadata server's EndpointSlice empty, conntrack forget
    │
    ▼
  Promote       ← agent: connect, fence every device, mount state, set key, start nfsd
    │  agent active at n+1
    ▼
  Reassemble    ← per export, 8 at a time: mdsPodName, CreateExport, assembledBy
    │
    ▼
  Serve         ← EndpointSlice → target IP, conntrack forget; per export: mdsNodeIP
```

### 7.2 Detecting the loss of the active

The signals are the active's csi-link session, its pod's `Ready` condition and deletion timestamp, its node's `Ready` condition, and `CheckExport`. A pod with a deletion timestamp is a planned loss and starts the takeover at once (§7.4). Anything else needs every condition below to hold continuously for the detection window:

- The active has no csi-link session.
- The active's pod is not `Ready`, or its node is not `Ready`.
- A standby has a session and its agent reports the standby role.

The link session is the signal that does not depend on the API server, and the pod and node conditions are the signals that do not depend on the operator's own connections. Requiring both keeps an API server blip of a few seconds (pod and node conditions stale, sessions up) and a broken link alone (sessions down, pod `Ready`) from starting a takeover. The window restarts when the operator acquires leadership, so a new leader never counts time it did not observe, and every link session is new to it.

**API server reads.** Every API read the decision depends on (the pods, the nodes, and the StatefulSet) is retried 3 times, 2 seconds apart. A read that still fails ends the pass: it never counts as a condition observed, and it never advances the window. An API server outage of up to about 6 seconds costs a delayed pass and nothing else, and a longer one holds the decision for as long as it lasts.

A lost node's `Ready` condition turns false only after the node controller's `node-monitor-grace-period`, 40 seconds by default, and its pod reads `Ready` until then, so that period bounds the detection of a lost node.

While a loss is observed and the window has not elapsed, the decision is held with an `MDSFailoverHeld` event. The window's length, and whether `CheckExport` failures alone should count, are Open Question 1.

### 7.3 Promotion

#### Step 1: Choose the target

Every standby with a session, `Ready`, and reporting the standby role is a candidate. The operator prefers a candidate on a node whose `topology.kubernetes.io/zone` differs from the lost active's, when both nodes carry the label, and among those the lowest ordinal. The choice depends only on the observed pods and nodes, so two passes deciding from the same observation choose the same pod, and the record's conditioned write (Step 2) settles any that do not. With no candidate, the takeover waits, and the first guest that becomes one is the target, including the old active's pod once it has restarted. A cluster with `standbys: 0` has only that case.

#### Step 2: Record

Write the record with the target, its instance, and `epoch+1`, conditioned on the `resourceVersion` the decision was made on. A conflict ends the pass without promoting. The next pass reads the new record and either finds the takeover already decided or decides again.

#### Step 3: Withdraw

Empty the metadata server's EndpointSlice and run the conntrack forget of `design-pnfs-mds-vm.md` §8.1 for the old active's pod IP. On a planned loss this already happened when the pod started terminating. On a partition it has not, and clients still holding flows to the old guest are moved off it here rather than when its filesystem fails.

#### Step 4: Promote

Call `Promote(epoch, key, state volume, export volumes)` on the target. The runner resolves every connection for its own host NQN and relays the call. The agent fences every device (§6.4), mounts the state volume, writes `nfsd.pnfs_mds_pr_key`, and starts the NFS server, whose grace period begins and whose `nfsdcld` reads the database the old active wrote. The runner records the call's exports as assembled by it, and the agent keeps every connection the call carried, so the endpoint refresh and the path repair of `design-pnfs-mds-vm.md` §7.6 cover them from the promotion on. A refused promotion is an `MDSFenceFailed` event and a retry with backoff at the same epoch. A collision refusal increments the epoch through Step 2 before the retry.

#### Step 5: Reassemble and serve

For every export bound to the cluster, eight at a time (`design-pnfs-mds-vm.md` §7.5): rewrite `status.mdsPodName` to the target, call `CreateExport`, record `assembledBy`, and set `mdsNodeIP`. The metadata server's one EndpointSlice is pointed at the target's pod IP before the first reassembly, and the nodes are asked once to forget flows to the old IP. The existing resync performs all of it once `mdsPodName` names the target, so this step is the resync, triggered by the rewrite. `CreateExport` mounts each export as a first assembly does, with `-o discard` and the zeroed write layouts of `design-pnfs-mds-vm.md` §6.7.

#### Algorithm Summary

```
takeover(cluster):
  rec := statefulSet.annotations
  if rec.active is lost (§7.2) or deleting or empty:
      target := chooseStandby() or return wait     // §7.3 Step 1
      rec' := {active: target, instance: target.instance, epoch: rec.epoch + 1}
      if !update(statefulSet, rec', resourceVersion): return requeue   // lost the race
      withdraw(endpointSlice, oldIP)
  if describe(rec.active) is standby or (active at epoch < rec.epoch):
      err := promote(rec.active, rec.epoch, key(rec.epoch), stateVolume, exports)
      if collision(err): bump epoch via the update above; return requeue
      if err: event MDSFenceFailed; return backoff
  serve(endpointSlice, rec.active.ip)               // conntrack forget for oldIP
  for export in exportsOf(cluster), 8 at a time:
      if export.mdsPodName != rec.active: export.mdsPodName = rec.active   // status update
      resync(export)    // CreateExport, assembledBy, mdsNodeIP
```

### 7.4 Planned switchover and rollout

A pod with a deletion timestamp is the planned case. Its runner drains on `SIGTERM` (`design-pnfs-mds-vm.md` §5.2): data-carrying packets to the guest's NFS port are dropped and requests already in nfsd are answered. The operator does not wait for the old guest to exit before fencing. Exports use the `sync` option, so every metadata request nfsd answered was committed, and the new active's log recovery starts from that.

A template change rolls the guests without serving from an old template longer than one restart: the operator deletes the standbys one at a time, each after the previous one's replacement reports the standby role and is `Ready`, then deletes the active, which promotes a standby on the new template. The old active's pod comes back as a standby on the new template.

A manual switchover is the `SwitchoverMDS` action of a `SimplyblockDriverOps` kind, the `Ops` companion of `SimplyblockDriver` in the pattern `ControlPlaneOps` follows for `ControlPlane`. The action names a storage cluster, since each storage cluster has its own MDS, and optionally the standby pod to promote. It deletes the active's pod, which is the planned case above, with the named standby as Step 1's target when it is a candidate. The kind's definition is a follow-up design (Open Question 3).

Lowering `standbys` below the active's ordinal first switches over to a standby on a remaining ordinal, then scales down (§4.1).

### 7.5 The old active returns

A guest whose instance is not the record's and whose agent reports active is a returning old active: a partition healed, or a paused node resumed. It is already fenced, so it cannot write, but it holds dead mounts and a running nfsd. The operator deletes its pod with an `MDSDemoted` event, and the restarted pod boots as a standby. A pod on a node that never returns stays `Terminating`, its ordinal is missing from the standbys, and `MDSStandbyMissing` is emitted while no standby runs.

### 7.6 What clients see

A takeover is a server restart under the same server owner and scope, so the recovery path is the one `design-pnfs-rwx.md` §13.4 describes, with each guest kernel patch doing what it does after a restart:

- **Reclaim (§5.2, §5.4).** Clients reclaim opens and locks in grace from the database on the state volume. A client that established its state after the old active's last database write that reached the device cannot reclaim, and reopens.
- **0003 and 0007.** A reclaim or request for an export not reassembled yet gets `NFS4ERR_DELAY` for 60 seconds after nfsd starts (`nfsd.pnfs_export_restore=60`). Every export has to be back inside that window, which is why reassembly runs eight at a time.
- **0001 and the re-probe.** The layout hold keeps clients from taking a layout until the CSI node plugin's probe has, under the filesystem's current device ID generation, and the probe's NFS WRITE-count check proves the client is back on the direct path. A client therefore looks each device up again under the new active, with its current size, including an export grown just before the takeover (0021).
- **0005, 0020, and 0022.** With zeroed layouts, data a client wrote through a layout before the takeover is written on the device, whether or not its LAYOUTCOMMIT reached the old active. The new active mounts with `-o discard`, and 0022 skips the zeroing of free space only once a trim of the whole device after that mount has marked its free space, so until then a LAYOUTGET on reused space zeroes up to 0020's 64 MiB as on a first mount.
- **Fencing.** 0018 fences the client keys of earlier boots on every device of an export when the new active sets the export up, before the export is published, and 0002 does again when a client asks for a device. A client of the old active is unregistered from then until its next GETDEVICEINFO registers it under the new active's boot, and writes nothing directly in between. The agent's fence of §6.4 leaves client keys alone.

---

## 8. Control Channel

The guest agent gains an `MDSService` beside the `ExportService`, defined in `atlas-lib/nfsexport/nfsexportrpc/nfsexportv1/mds.proto` and relayed by the runner:

| RPC        | Request                                                                                                        | Effect                                                                                                                |
|------------|----------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| `Describe` | none                                                                                                           | The role (`Standby`, `Promoting`, `Active`), the accepted epoch, and the key                                          |
| `Promote`  | epoch, key, host NQN, the state volume, and every export's volumes, each member of a striped export among them | §6.4 then §7.3 Step 4. Idempotent at the accepted epoch. Refused below it, and refused at any other epoch once active |

The runner adds the connections, resolved with the pod's host NQN, and drops any the operator sent, as for `CreateExport`. `CreateExport` is refused on a guest that is not `Active`.

`PeerKindMDS` stays as `design-pnfs-mds-vm.md` §7.1 defines it, one peer per pod name, so each pod is a peer of its own. The operator's ClusterRole loses `persistentvolumeclaims` `create` and keeps `statefulsets` `update`, which the annotations and finalizer use.

---

## 9. Failure Modes

| Failure                                                                                                       | Detection                                                                                    | Behavior                                                                                                                                                                                    |
|---------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| The active's node is lost                                                                                     | No session, and the node or pod not `Ready`, for the window (§7.2)                           | A standby is promoted (§7.3 Step 1). The old pod stays `Terminating`, and `MDSStandbyMissing` while no other standby runs                                                                   |
| The active is partitioned from the operator and the API server                                                | As above                                                                                     | A standby is promoted and fences the devices. The old guest's next write fails (§6.5). Withdraw and conntrack forget move its clients first (§7.3 Step 3)                                   |
| API server unavailable for seconds                                                                            | Reads fail after 3 retries 2 s apart, or pod and node conditions stale while sessions are up | Held. A failed read counts toward nothing (§7.2), and `MDSFailoverHeld` while any condition is observed                                                                                     |
| Operator restarted or leadership moved                                                                        | New leader, every session new                                                                | The window restarts. A takeover in progress resumes from the record (§7.1)                                                                                                                  |
| Fencing fails on one device                                                                                   | `Promote` returns the device and step                                                        | Nothing mounted, no nfsd. `MDSFenceFailed`, retry with backoff. Exports stay withdrawn                                                                                                      |
| Key collision                                                                                                 | Report shows the key on another host                                                         | Promotion refused, epoch incremented, retried                                                                                                                                               |
| Two writers decide a takeover                                                                                 | `resourceVersion` conflict on the record                                                     | One record write succeeds and only its writer promotes. A hand-edited record that duplicates an epoch is caught by the collision check                                                      |
| Fewer KVM-capable nodes than `1 + standbys`                                                                   | A standby pod unschedulable                                                                  | The pods that fit serve and the rest stay `Pending`. `MDSStandbyMissing` while no standby runs. A loss of the active with no standby waits for any guest to come back                       |
| Every guest lost                                                                                              | No standby with a session                                                                    | The takeover waits, and the first guest to come back as a standby is promoted at a new epoch                                                                                                |
| The old active returns                                                                                        | A session whose instance is not the record's, reporting `Active`                             | Its pod is deleted, `MDSDemoted`, and it boots as a standby                                                                                                                                 |
| Control plane refuses a standby's host NQN                                                                    | Connect fails in `Promote`                                                                   | As fencing failure. The prerequisite is P0-2                                                                                                                                                |
| State volume creation fails                                                                                   | `CreateVolume` error                                                                         | No promotion, exports `Pending`, `MDSStateUnavailable`, retry                                                                                                                               |
| Export bound during a takeover                                                                                | Record names a target that is not `Active` yet                                               | The export waits in `Pending` until the target is active                                                                                                                                    |
| Commands queued at the target when the preempt lands                                                          | Not detected                                                                                 | With Preempt and Abort (P0-7), the preempt completes after them and the new active mounts after that. With a plain Preempt they execute (§6.6), and the new active mounts after the preempt |
| The volume's primary storage node changes, or its SPDK pod is replaced                                        | Not detected by the operator                                                                 | The reservation on the node serving next is empty or stale, so a fenced guest is refused only where the reservation lives (§6.1). Closed by P0-5                                            |
| The namespace already has 16 registrants                                                                      | Register fails in `Promote`                                                                  | As fencing failure. Closed by P0-6                                                                                                                                                          |
| A fenced old active keeps repairing paths and refreshing endpoints                                            | Not detected                                                                                 | Its paths reconnect and its `CreateExport` for an export it holds fences nothing, so nothing registers (§6.5). Its writes stay refused                                                      |
| A promotion meets a key with its tag and a higher epoch                                                       | The report in `Promote`                                                                      | Refused without a preempt (§6.4). `MDSFenceFailed`. The record names the newer active, which the next pass serves                                                                           |
| A member of a striped export is not fenced                                                                    | The per-member report of §6.4 step 6                                                         | As fencing failure. The volume group is not activated                                                                                                                                       |
| `dataNetwork` set and fewer eligible nodes on the subnet, or free addresses in its range, than `1 + standbys` | A standby pod unschedulable, or its sandbox failing                                          | The pods that fit serve, the rest stay `Pending` or `ContainerCreating`. `MDSStandbyMissing` while no standby runs                                                                          |
| No mds-proxy answers on the promoted guest's node                                                             | The runner's address query fails                                                             | As in `design-pnfs-mds-vm.md` §9: each volume with a copy on that node runs one path short. The promotion is not held for it                                                                |

---

## 10. Observability

`design-pnfs-mds-vm.md` §11 specifies the MDS events and four metrics, and the events below are additions to it. Neither table's metrics are exported yet, so the metrics below are new infrastructure in the same registry and naming.

### Kubernetes Events

A takeover is a property of a storage cluster's MDS, and no kind exists for that. Cluster-level events go to the `SimplyblockDriver`, the object a user configured the MDS on and one that outlives every pod. Events a user of one export needs go to each affected `NFSExport` as well, as `design-pnfs-mds-vm.md` §11 does.

| Event                                                                                                 | Type                                   | Reason                 | On                               |
|-------------------------------------------------------------------------------------------------------|----------------------------------------|------------------------|----------------------------------|
| A guest was promoted to active at a new epoch, with the cause (deleted, lost, first) and the duration | `Normal` if planned, `Warning` if lost | `MDSPromoted`          | `SimplyblockDriver`, `NFSExport` |
| Every device was fenced under the new key, with the count and the keys preempted                      | `Normal`                               | `MDSFenced`            | `SimplyblockDriver`              |
| Fencing failed on a device, so the promotion mounted nothing and is being retried                     | `Warning`                              | `MDSFenceFailed`       | `SimplyblockDriver`, `NFSExport` |
| The active looks lost and the takeover is held until the detection window elapses                     | `Normal`                               | `MDSFailoverHeld`      | `SimplyblockDriver`              |
| Another writer moved the active record first, so this pass did not promote                            | `Normal`                               | `MDSPromotionConflict` | `SimplyblockDriver`              |
| No standby is running and ready, so exports are unserved after a loss of the active                   | `Warning`                              | `MDSStandbyMissing`    | `SimplyblockDriver`              |
| A guest that was active under an earlier epoch came back and its pod was restarted as a standby       | `Normal`                               | `MDSDemoted`           | `SimplyblockDriver`              |

`MDSStateUnavailable` keeps its reason and now names a failed state volume creation.

### Prometheus Metrics

| Metric                                  | Labels                       | Description                                                                                         |
|-----------------------------------------|------------------------------|-----------------------------------------------------------------------------------------------------|
| `simplyblock_pnfs_mds_role`             | `cluster`, `pod`, `role`     | One for the role each guest reports (`Active`, `Standby`, `Promoting`), else zero                   |
| `simplyblock_pnfs_mds_epoch`            | `cluster`                    | The record's current epoch                                                                          |
| `simplyblock_pnfs_mds_failovers_total`  | `cluster`, `cause`, `result` | Takeovers by cause (`Deleted`, `Lost`, `First`) and result (`Succeeded`, `FenceFailed`, `Conflict`) |
| `simplyblock_pnfs_mds_takeover_seconds` | `cluster`, `phase`           | Histogram of each phase: `Detect`, `Fence`, `Start`, `Reassemble`, `Serve`                          |

`simplyblock_pnfs_mds_role{role="Standby"}` summed per cluster is the alert for a cluster that has lost its protection: zero with `standbys` above 0 means the next node loss is an outage. `simplyblock_pnfs_mds_failovers_total{cause="Lost"}` is the alert for false failovers: each one is a server restart for every client, and a rate above the rate of node failures means the detection window is too short. `simplyblock_pnfs_mds_takeover_seconds` per phase is what turns the restore budget into a number: `Fence` plus `Start` plus `Reassemble` must stay under the 60 seconds of `nfsd.pnfs_export_restore`.

---

## 11. Testing Strategy

The scenario matrix, its coverage status, and the gaps are in [`tests/test-plan-pnfs-mds-standby.md`](../tests/test-plan-pnfs-mds-standby.md).

- **Unit:** the key derivation and its separation from client keys, the fence sequence against a fake reservation device, including every refusal, and the agent's role transitions.
- **Operator:** the StatefulSet, the record's conditioned write and its conflict, detection with its held and boundary cases, restart at each step, and demotion, against a fake client and a fake link.
- **Guest integration:** two guests and a real target, proving the kernel parameter's four behaviors and that a fenced guest cannot write or register again, through its path repair, an endpoint refresh, or the members of a striped export. This needs a harness that does not exist yet.
- **Failure injection:** a lost node, a partition, a fence failure, a false failover, and a double promotion, each under `pnfs-fio` with checksum verification.
- **Where the risk concentrates:** §6.3 and §6.4, the dead-node case, and the double-promotion race. Those scenarios must not be cut.

---

## 12. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                            | Owner         |
|-----|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| 1   | **Detection thresholds.** API reads are retried 3 times, 2 s apart (§7.2). A lost node's pod and node read `Ready` until `node-monitor-grace-period` (40 s by default) passes, which bounds detection of a lost node. Open are the window's length after it and whether `CheckExport` failures count. A window that is too short fails over on an API server blip, which has taken storage nodes offline before. One that is too long is the outage | Operator team |
| 2   | **The fenced old guest.** What XFS and ext4 do when every write fails with a reservation conflict: the expected forced shutdown, and whether nfsd or the agent can hang on it. Verifiable today on the volume's primary storage node, where the reservation lives. The same behavior after a change of primary waits for P0-5                                                                                                                       | Operator team |
| 3   | **`SimplyblockDriverOps`.** The kind's definition: its spec, status, phases, and the fields of the `SwitchoverMDS` action (§7.4), with whatever other driver actions it carries                                                                                                                                                                                                                                                                     | Operator team |
| 4   | **The state volume's pool.** The state volume is created in the pool of the first export bound to the cluster. Once that pool's exports are deleted, deleting the pool is still blocked by the state volume, which nothing shows the user. Where the state volume lives instead, or how the block is made visible                                                                                                                                   | Operator team |
| 5   | **device-mapper reservations in the guest.** A striped export's holder check of §6.3 reads the reservation through device-mapper (`dm_pr_read_reservation`), present in the 7.3 source. Whether the guest's 6.18.55 kernel carries it, and, where it does not, whether the check reads the first member directly or P0-1 adds the read                                                                                                              | Operator team |
---

## Appendix A: `simplyblockdriver_types.go`

The shipped file with the change of §4.1: `DriverPNFSMDS` loses `stateSize` and `stateStorageClassName` and gains `standbys`, and the comments of `Resources`, `NodeSelector`, `Tolerations`, `DataNetwork`, `DriverPNFS.MDS`, and `DriverPNFSMDS` describe the active and its standbys. Every other type is unchanged and appears as shipped.

```go
// SimplyblockDriverPhase is where the operator has got to with the CSI driver.
// Installing covers the applies, and the three values after it are decided by
// what the node plugins and the controller plugin report.
// +kubebuilder:validation:Enum=Installing;Ready;Degraded;Unavailable
type SimplyblockDriverPhase string

const (
	// SimplyblockDriverPhaseInstalling is a deployment whose objects are not all
	// applied yet.
	SimplyblockDriverPhaseInstalling SimplyblockDriverPhase = "Installing"
	// SimplyblockDriverPhaseReady is every plugin pod serving.
	SimplyblockDriverPhaseReady SimplyblockDriverPhase = "Ready"
	// SimplyblockDriverPhaseDegraded is a node plugin restarting while the
	// controller plugin still provisions, which strands one worker's volumes
	// rather than the namespace's.
	SimplyblockDriverPhaseDegraded SimplyblockDriverPhase = "Degraded"
	// SimplyblockDriverPhaseUnavailable is a controller plugin that is not
	// running, which is when provisioning stops.
	SimplyblockDriverPhaseUnavailable SimplyblockDriverPhase = "Unavailable"
)

// SidecarImages overrides the CSI sidecar images this deployment runs. An unset
// field takes the version this operator release ships, which is the combination
// it was tested against, and the fields exist so that a pin a Helm release made
// survives the adoption of that release's deployment.
//
// Every field carries the registry pattern Image carries. The node plugin is
// privileged and mounts /dev, /sys, and the kubelet's plugin directory from the
// host, so a sidecar beside it runs with the same access.
type SidecarImages struct {
	// Provisioner is csi-provisioner, on the controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Provisioner string `json:"provisioner,omitempty"`

	// Attacher is csi-attacher, on the controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Attacher string `json:"attacher,omitempty"`

	// Resizer is csi-resizer, on the controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Resizer string `json:"resizer,omitempty"`

	// Snapshotter is csi-snapshotter, on the controller plugin. It is this
	// driver's sidecar and not the cluster's snapshot-controller, whose image
	// is not overridable here because that component belongs to the cluster.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Snapshotter string `json:"snapshotter,omitempty"`

	// HealthMonitor is csi-external-health-monitor-controller, on the
	// controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	HealthMonitor string `json:"healthMonitor,omitempty"`

	// NodeDriverRegistrar is node-driver-registrar, on the node plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	NodeDriverRegistrar string `json:"nodeDriverRegistrar,omitempty"`

	// CSIAddons is the kubernetes-csi-addons sidecar, on the controller
	// plugin. It connects to the plugin's socket, probes the csi-addons
	// Identity service for capabilities, and publishes a CSIAddonsNode so the
	// kubernetes-csi-addons controller-manager (design
	// design-csi-addons-replication.md §4.1) can reach the Replication
	// service this driver serves.
	//
	// Unlike the other sidecars above, this one's upstream home is the
	// csi-addons project's own registry, not simplyblock's: the allowlist
	// carries quay.io/csiaddons alongside the simplyblock registries so a
	// deployment can run the stock kubernetes-csi-addons sidecar image
	// directly, ahead of (or instead of) a quay.io/simplyblock-io mirror.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block|quay\.io/csiaddons)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	CSIAddons string `json:"csiAddons,omitempty"`
}

// DriverTLSProvider is where the TLS certificate on this connection comes
// from. The values are not this group's to spell: OpenShift and cert-manager
// are the two products, and the operator's own internal/utils package already
// carries these exact strings for the control plane's own SB_TLS_PROVIDER, so
// a driver and a control plane in the same namespace agree on the same word
// without a translation table between them.
// +kubebuilder:validation:Enum=OpenShift;cert-manager
type DriverTLSProvider string

const (
	// DriverTLSProviderOpenShift is OpenShift's service-ca operator: a
	// ConfigMap carrying the cluster CA, and a Secret an administrator
	// provisions for each plugin's client certificate.
	DriverTLSProviderOpenShift DriverTLSProvider = "OpenShift"
	// DriverTLSProviderCertManager is cert-manager: a ClusterIssuer already
	// installed by this chart mints a Certificate per plugin, and the Secret
	// it writes carries the CA bundle alongside the client keypair.
	DriverTLSProviderCertManager DriverTLSProvider = "cert-manager"
)

// DriverTLS configures whether this deployment's two plugins reach the
// control plane over TLS. Unset (every field at its zero value) is a
// plaintext data path, which is what every deployment ran as before this field
// existed.
//
// The client-certificate Secret each plugin mounts is not named here. It is
// `<object name>-csi-controller-client-tls` and
// `<object name>-csi-node-client-tls`, derived from the object's name like
// every other object this deployment creates, and a field naming them again
// would be a second place for the two to disagree.
type DriverTLS struct {
	// EnableTLS turns on TLS between both plugins and the control plane.
	// +kubebuilder:default=false
	// +optional
	EnableTLS *bool `json:"enableTLS,omitempty"`

	// EnableMutualTLS additionally requires each plugin to present a client
	// certificate, rather than dialing the control plane anonymously over
	// the encrypted connection EnableTLS alone provides. Ignored when
	// EnableTLS is false, the same as the Helm value it replaces.
	// +kubebuilder:default=false
	// +optional
	EnableMutualTLS *bool `json:"enableMutualTLS,omitempty"`

	// Provider selects where the CA bundle (and, with EnableMutualTLS, the
	// client certificate) comes from. Required reading whenever EnableTLS is
	// true: the two providers mount a differently shaped volume, and neither
	// shape can be inferred from anything else on this object.
	// +kubebuilder:default=cert-manager
	// +optional
	Provider DriverTLSProvider `json:"provider,omitempty"`
}

// DriverPNFS configures pNFS support: ReadWriteMany volumes served by an NFS
// metadata server, whose clients read and write the volume's NVMe-oF namespace
// directly.
type DriverPNFS struct {
	// MDS runs the metadata server: per storage cluster, an active pod and,
	// by default, one standby on another node, each a QEMU guest running the
	// NFS server with its own kernel, so no node runs nfsd, mounts an
	// export, or needs nfs-utils. The pods need nodes with /dev/kvm. Unset,
	// no pNFS volume can be exported: its export waits in Pending until this
	// is set.
	// +optional
	MDS *DriverPNFSMDS `json:"mds,omitempty"`
}

// DriverPNFSMDS is the metadata server: an active guest and standbys on other
// nodes, one of which takes over when the active is lost.
type DriverPNFSMDS struct {
	// Image is the metadata server image: QEMU, the guest kernel, the guest
	// root filesystem, and the runner. Unset takes the operator's own registry
	// in the spdkcsi repository, tagged with the operator's tag prefixed
	// pnfs-mds-, so a deployment that states nothing runs the image belonging
	// to the operator reconciling it.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Image string `json:"image,omitempty"`

	// Resources are each pod's requests and limits, so every standby adds
	// one pod's worth. The guest's vCPUs are the whole cores the CPU limit
	// covers, at least one, and its memory is the memory limit less 256Mi for
	// QEMU, so the memory limit has to be at least 512Mi. A CPU or memory
	// limit left unset defaults to 2 and 2Gi, and a memory request left unset
	// to 512Mi, or the limit when that is lower. The guest returns the memory
	// it frees to the node, so each pod uses about what its guest does.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector is merged with the kvm-capable label the pods always
	// require. No two of the guests run on one node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let the pods schedule onto tainted KVM nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Standbys is how many standby guests run beside the active one, each on
	// its own node. A standby takes over when the active is lost, in about the
	// time a restart takes. At 0, exports stay unserved after the loss of the
	// active's node until the lost pod is deleted and started again
	// elsewhere. Each standby needs another node offering KVM, and one
	// without such a node stays pending while the active serves.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4
	// +kubebuilder:default=1
	// +optional
	Standbys *int32 `json:"standbys,omitempty"`

	// DataNetwork attaches the metadata server to the storage data network, so
	// its guests reach the storage nodes there rather than through the pod
	// network. Each pod gets a macvlan interface on whichever host interface
	// carries the subnet, and runs only on nodes that have one, preferring
	// nodes that are not storage nodes. It needs Multus and the whereabouts IPAM
	// plugin, which OpenShift ships, and a range of at least one address per
	// pod. Unset, the guests reach the storage nodes through the pod network,
	// which needs the nodes to forward traffic arriving on their data
	// interfaces. Changing it rolls every guest onto the new template, the
	// active last.
	// +optional
	DataNetwork *DriverPNFSDataNetwork `json:"dataNetwork,omitempty"`
}

// DriverPNFSDataNetwork is the storage data network the metadata server is
// attached to, and the addresses it may take on it.
// +kubebuilder:validation:XValidation:rule="!isCIDR(self.subnet) || !isIP(self.rangeStart) || cidr(self.subnet).containsIP(self.rangeStart)",message="rangeStart must be an address in subnet"
// +kubebuilder:validation:XValidation:rule="!isCIDR(self.subnet) || !isIP(self.rangeEnd) || cidr(self.subnet).containsIP(self.rangeEnd)",message="rangeEnd must be an address in subnet"
type DriverPNFSDataNetwork struct {
	// Subnet is the data network in CIDR form, such as 10.10.10.0/24. A node
	// whose interface holds an address in it can run the metadata server.
	// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self).ip().family() == 4",message="subnet must be an IPv4 CIDR"
	// +kubebuilder:validation:MaxLength=18
	// +required
	Subnet string `json:"subnet"`

	// RangeStart is the first address the metadata server pods may take. The
	// range must hold no address anything else on the network uses: one pod
	// takes one address, and a restarted pod can take another.
	// +kubebuilder:validation:XValidation:rule="isIP(self) && ip(self).family() == 4",message="rangeStart must be an IPv4 address"
	// +kubebuilder:validation:MaxLength=15
	// +required
	RangeStart string `json:"rangeStart"`

	// RangeEnd is the last address the metadata server pods may take.
	// +kubebuilder:validation:XValidation:rule="isIP(self) && ip(self).family() == 4",message="rangeEnd must be an IPv4 address"
	// +kubebuilder:validation:MaxLength=15
	// +required
	RangeEnd string `json:"rangeEnd"`
}

// SimplyblockDriverSpec is the CSI driver deployment: the node plugin, the
// controller plugin, their RBAC, and the CSIDriver registration they produce.
type SimplyblockDriverSpec struct {
	// Image is the CSI driver image, used by both plugins. Unset takes the
	// operator's own registry and tag with the CSI driver's repository, so a
	// deployment that states nothing runs the driver belonging to the operator
	// reconciling it, which is the pairing the release was tested as.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy controls when that image is pulled. It defaults to Always
	// because the default Image is a moving tag: it follows the operator's own,
	// and a development build's tag is rebuilt in place. IfNotPresent against a
	// tag that moved leaves the workers that already pulled it running the old
	// plugin and the workers that had not running the new one, which is version
	// skew inside one deployment and invisible from the object.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// DriverName is the CSI driver name a StorageClass provisions with. Every
	// PersistentVolume the driver created records it in spec.csi.driver and
	// every VolumeAttachment records it too, so changing it orphans every volume
	// in the namespace rather than renaming anything.
	// +kubebuilder:default=csi.simplyblock.io
	// +optional
	// +k8s:immutable
	DriverName string `json:"driverName,omitempty"`

	// ControllerReplicas is the number of controller-plugin instances.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	ControllerReplicas *int32 `json:"controllerReplicas,omitempty"`

	// NodeSelector restricts which workers run the node plugin. Empty means
	// every schedulable worker, which is the usual case: a node that cannot
	// attach a volume cannot run a workload that needs one.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are applied to the node plugin, which usually needs to run
	// where workloads run rather than where the operator does.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// ControllerNodeSelector and ControllerTolerations place the controller
	// plugin. The unprefixed pair above is the node plugin's, because that
	// placement decides which workers can attach a volume, and this pair is
	// ordinary pod placement for the one workload that provisions them.
	// +optional
	ControllerNodeSelector map[string]string `json:"controllerNodeSelector,omitempty"`
	// +optional
	ControllerTolerations []corev1.Toleration `json:"controllerTolerations,omitempty"`

	// ControllerResources and NodeResources set requests and limits for the two
	// plugins. Unset enforces no limits.
	// +optional
	ControllerResources corev1.ResourceRequirements `json:"controllerResources,omitempty"`
	// +optional
	NodeResources corev1.ResourceRequirements `json:"nodeResources,omitempty"`

	// SidecarImages overrides the seven CSI sidecars, one field each. Unset
	// takes the version this operator release ships.
	// +optional
	SidecarImages SidecarImages `json:"sidecarImages,omitempty"`

	// EnableServiceAccountAuth makes both plugins authenticate to the management
	// API with their pod's Kubernetes service-account token instead of the
	// static cluster secret. The control plane has to list those accounts in
	// SB_K8S_ADMIN_SERVICE_ACCOUNTS for it to work, which is why this is a
	// deployment-wide switch rather than a per-plugin one.
	// +kubebuilder:default=false
	// +optional
	EnableServiceAccountAuth *bool `json:"enableServiceAccountAuth,omitempty"`

	// EnableVolumeSnapshots decides whether snapshot support is part of this
	// deployment: the VolumeSnapshotClass for DriverName, and the CRDs and a
	// controller where the cluster serves neither. False applies none of them.
	// +kubebuilder:default=true
	// +optional
	EnableVolumeSnapshots *bool `json:"enableVolumeSnapshots,omitempty"`

	// EnableCSIAddons adds the kubernetes-csi-addons sidecar to the controller
	// plugin, with the Role and bindings it needs. The sidecar publishes a
	// CSIAddonsNode object, so it only works on a cluster that serves the
	// csiaddons.openshift.io CRDs. Without them it restarts in a loop, so leave
	// it false unless those CRDs are installed.
	// +kubebuilder:default=false
	// +optional
	EnableCSIAddons *bool `json:"enableCSIAddons,omitempty"`

	// TLS configures whether both plugins reach the control plane over TLS.
	// Unset is plaintext, the shape every deployment ran before this field
	// existed. Adopting a deployment that already runs TLS requires this to
	// agree with what its plugins are configured for, because it is not read
	// off the running objects.
	// +optional
	TLS DriverTLS `json:"tls,omitempty"`

	// PNFS configures pNFS support on the node plugin.
	// +optional
	PNFS DriverPNFS `json:"pnfs,omitempty"`
}

// SnapshotSupportOrigin is where the cluster's snapshot support came from.
// +kubebuilder:validation:Enum=Detected;Installed
type SnapshotSupportOrigin string

const (
	// SnapshotSupportOriginDetected is a cluster that already served
	// snapshot.storage.k8s.io/v1, so the operator applied no CRDs and no
	// controller.
	SnapshotSupportOriginDetected SnapshotSupportOrigin = "Detected"
	// SnapshotSupportOriginInstalled is a cluster where the operator applied
	// them. They are cluster-scoped and shared, so they carry no controller
	// reference and outlive this object.
	SnapshotSupportOriginInstalled SnapshotSupportOrigin = "Installed"
)

// SimplyblockDriverOrigin is where the running deployment came from. Every
// cluster upgraded from a chart install reads Adopted, because the chart had
// applied the objects before this kind existed.
// +kubebuilder:validation:Enum=Created;Adopted
type SimplyblockDriverOrigin string

const (
	// SimplyblockDriverOriginCreated is a deployment whose objects the operator
	// applied from nothing.
	SimplyblockDriverOriginCreated SimplyblockDriverOrigin = "Created"
	// SimplyblockDriverOriginAdopted is a deployment the operator took over in
	// place, taking field ownership from Helm and removing the release's
	// metadata once the handover was verified.
	SimplyblockDriverOriginAdopted SimplyblockDriverOrigin = "Adopted"
)

// SimplyblockDriverStatus is the observed state of the CSI driver deployment.
type SimplyblockDriverStatus struct {
	// Phase is the operator's own view of the deployment.
	// +optional
	Phase SimplyblockDriverPhase `json:"phase,omitempty"`

	// SnapshotSupport is whether the cluster already had snapshot support or the
	// operator installed it, which is what says whether other drivers depend on
	// what this one applied.
	// +optional
	SnapshotSupport SnapshotSupportOrigin `json:"snapshotSupport,omitempty"`

	// Origin is whether the first reconcile created this deployment's objects
	// or met ones it did not create. It is decided once and never revised,
	// because what it records is where the running deployment came from rather
	// than what the controller did most recently.
	// +optional
	Origin SimplyblockDriverOrigin `json:"origin,omitempty"`

	// Version is the version the deployed driver reports, published so that a
	// skew against ControlPlane.status.version is visible on one screen.
	// +optional
	Version string `json:"version,omitempty"`

	// NodesReady is how many workers run a ready node plugin, and NodesTotal how
	// many are expected to. Neither takes omitempty: zero ready plugins is the
	// condition worth seeing.
	// +kubebuilder:validation:Minimum=0
	NodesReady int32 `json:"nodesReady"`
	// +kubebuilder:validation:Minimum=0
	NodesTotal int32 `json:"nodesTotal"`

	// ControllerReady is whether the controller plugin is serving.
	// +optional
	ControllerReady bool `json:"controllerReady,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the deployment moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbd
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="NodesReady",type=integer,JSONPath=".status.nodesReady"
// +kubebuilder:printcolumn:name="NodesTotal",type=integer,JSONPath=".status.nodesTotal"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SimplyblockDriver is the deployment of simplyblock's CSI driver: the node
// plugin, the controller plugin, their RBAC, and the core CSIDriver registration
// they produce. It is named for the brand rather than the interface because
// CSIDriver is already a kind in core storage.k8s.io/v1, and the two are not the
// same object: the core kind is the cluster's registration record, and this one
// is the deployment that produces it.
type SimplyblockDriver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SimplyblockDriverSpec   `json:"spec,omitempty"`
	Status SimplyblockDriverStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SimplyblockDriverList contains a list of SimplyblockDriver.
type SimplyblockDriverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SimplyblockDriver `json:"items"`
}
```
