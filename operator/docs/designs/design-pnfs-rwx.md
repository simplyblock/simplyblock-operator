# Design Document: Single-Volume pNFS (RWX) Support for the Simplyblock CSI Driver

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-07-02 (last updated 2026-10-08)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/278  
**Target release:** 26.4  
**Test Plan:** [`tests/test-plan-pnfs-rwx.md`](../tests/test-plan-pnfs-rwx.md)  
**Follow-on design:** [`design-pnfs-striped.md`](design-pnfs-striped.md) adds striping across
several lvols, consistency-group snapshots, and the user-facing `VolumeGroupSnapshot`.

---

## Phasing Overview

| Phase | Delivers                                                              | Depends on                      | Status  |
|-------|-----------------------------------------------------------------------|---------------------------------|---------|
| 0     | External prerequisites (§Phase 0)                                     | Other repos, branches, spikes   | Blocked |
| 1     | `NFSExport` CRD and reconciler, MDS binding, export create and delete | P0-1 through P0-4               | Planned |
| 2     | Per-export Service and EndpointSlice, planned restart (§13.4)         | Phase 1                         | Planned |
| 3     | MDS health probe, PR fencing, unplanned failover (§13.5)              | Phase 2, P0-1, P0-2, P0-7, P0-8 | Planned |
| 4     | Snapshot with `xfs_freeze`, clone, restore, online resize             | Phase 1                         | Planned |
| 5     | Export client restriction, host allow-listing, squash and tenancy     | Phase 1                         | Planned |
| 6     | Load and soak, scale limits, docs, distro matrix                      | Phases 1 through 5              | Planned |

Phases 2 and 4 are independent and can run in parallel. Phase 3 is the one that
turns a working export into a survivable one, and it is the phase whose promises
depend on spikes rather than on code (§13.3).

---

## Overview

**What this is.** RWX (`MULTI_NODE_MULTI_WRITER`) volumes for the Simplyblock CSI
driver, built on the pNFS SCSI layout (RFC 8154). A QEMU guest, one per storage
cluster, acts as the metadata server (MDS) and exports an XFS filesystem per
volume over NFS 4.1.
Every client mounts that export for metadata, and then reads and writes the data
**directly** over NVMe-oF to the same namespace the MDS made the filesystem on.
Metadata goes through the MDS, data does not.

**Scope: one lvol per RWX volume.** This design deliberately stops at a single
backing volume. There is no LVM stripe, no consistency group, and no group
snapshot, which means a snapshot of an RWX volume is the ordinary per-lvol
snapshot the driver already takes. Striping across `n` lvols, and the
consistency-group machinery it forces, is the follow-on design. That split is
what makes this document shippable: it depends on the persistent-reservation
flag alone, not on any of the group-snapshot work the control plane has not
started.

**The three moving parts.** An RWX volume is one ordinary lvol. The metadata
server of its storage cluster, a QEMU guest in a pod of its own
([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md)), connects it, makes an XFS
filesystem on it, mounts it, and exports it (§8). The `NFSExport` CR records which
MDS the volume is bound to, and at which `fsid` and generation (§7.1). csi-node on
each client connects the same namespace and mounts the export, and the client
kernel maps file layouts onto the local block device itself (§10).

**Why the layout matters.** Without the layout, every byte would cross the
MDS and RWX throughput would be capped by one node. With it, the MDS carries
metadata only, which is what makes the shared filesystem scale with the number of
clients rather than against it (§3).

**One MDS per storage cluster, many exports.** `nfsd` is a kernel service and its
export table is global to the kernel it runs in, so the guest runs one `nfsd`
serving every export of its storage cluster. The unit of placement is therefore
the export, not the server: each RWX volume is bound to the MDS of its storage
cluster, and one MDS carries many volumes. Per-MDS `fsid` uniqueness follows from
this rather than being an edge case (§8.4).

**What is not decided or not available yet.** Fencing needs a persistent
reservation on the backing namespace (P0-1, P0-2). That and every other external
dependency live in
[Phase 0 — External Prerequisites](#phase-0--external-prerequisites). The
document is a Draft: it states what the design requires, not what exists.

---

## Phase 0 — External Prerequisites

Everything this design depends on that is not part of implementing it. Most rows are
owned by another repository, another team, or the environment. Three are not:
**P0-3, P0-4, and P0-5 are changes inside this monorepo**, to `atlas-lib` and the
CSI driver, and they are listed here because they gate this design rather than
because they are external. Nothing here is delivered by the pNFS work itself. The per-section detail stays where it
is specified, so this table is only the index, and the answer to whether the work
can start.

| #     | Prerequisite                                                                                                                                                                                     | Kind                          | Blocks                                                     | Status                  |
|-------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------|------------------------------------------------------------|-------------------------|
| P0-1  | `ptpl_file` on the lvol's namespace, so it is PTPL-capable and `nfsd` can register its key (§6.2). Not a flag on `bdev_lvol_create`                                                              | Control plane (`sbcli`)       | **Any layout being issued at all**, so every phase         | **Merged**              |
| P0-2  | Persistent-reservation support behind it, so a fenced client's writes are actually refused (§6.2, §15)                                                                                           | Storage plane (SPDK)          | Fencing, and MDS failover safety (§13)                     | **Present**             |
| P0-3  | The csi-link export service on the MDS peer, so the operator can drive `CreateExport`, `CheckExport`, and `DeleteExport` in the guest (§6.4)                                                     | Platform (`atlas-lib`)        | Every export operation, so all of §8                       | **Present**             |
| P0-4  | The client attach on atlas `nvmeof`, the connect path the guest's agent uses too, so the two cannot disagree about how or as which host (§6.4, §10.1)                                            | Platform (`csi-driver`)       | P0-3                                                       | **Present**             |
| P0-5  | `nvme.DeviceSelector.NGUID` and a by-NGUID lookup in atlas-lib, plus an `nvme-eui.` alias helper, so the client stops shelling out to `nvme id-ns` (§10.1)                                       | Platform (`atlas-lib`)        | Client device identity, though a shell-out works meanwhile | Not started             |
| P0-7  | NFSv4.1 `server_owner` and `server_scope` stable across MDS restarts, so a client reclaims its state rather than starting over (§13.3)                                                           | MDS guest                     | The freeze bound in NFR-2, and what §13 may promise        | **Answered**            |
| P0-8  | Confirmation that a kernel sunrpc mount reaches a Service ClusterIP on the CNI dataplanes the product supports, including eBPF kube-proxy replacement (§13)                                      | Environment, and a spike here | The stable-address design in §13                           | Open, spike needed      |
| P0-9  | Client and guest kernels carrying `nvme_get_unique_id`, without which nfsd cannot identify an NVMe device (§5.3). Not a 6.11 floor: RHEL 9.8's 5.14.0-687 has it, RHEL 9.5's 5.14.0-503 does not | Node OS                       | Every phase: pNFS SCSI layout needs it                     | **Satisfied**           |
| P0-10 | The pNFS block layout client (`CONFIG_PNFS_BLOCK`) in client kernels, which serves the SCSI layout too (§5.3)                                                                                    | Node OS                       | The client direct path                                     | Environment requirement |
| P0-11 | A Debian and Ubuntu spike: `/dev/disk/by-id` naming and the `nfs-common` difference (§5.3)                                                                                                       | Node OS                       | The distro matrix commitment                               | Not started (§18, Q4)   |

**Without P0-1** no layout is issued at all: the feature does not degrade, it silently
does not happen, and the fallback in FM-2 hides that completely. P0-2 turns out to be
present already — SPDK implements reservations, and the target accepted every type once
P0-1 was fixed — so fencing is reachable once the namespace is capable. **With P0-1 fixed the
whole path works end to end**, with zero bytes through the MDS and `LAYOUTRETURN` staying at 0.
**P0-3** is the
channel every export operation travels: the operator reaches the agent in the MDS
guest over csi-link, so without it the server side of this design does not exist.
**P0-8** does not block a first implementation, but it decides what §13 is allowed
to promise, so it must be answered before the freeze bound in NFR-2 is treated as
a commitment. **P0-9 and P0-10** are node-image requirements: a node that does not
meet them must be excluded from RWX scheduling, which is a behavior this
repository owns and tests. The MDS guest brings its own kernel and meets both.

The consistency-group prerequisites that earlier drafts carried here, the group
snapshot API and the freeze it depends on, belong to
[`design-pnfs-striped.md`](design-pnfs-striped.md) and are not prerequisites of
this document.

---

## Table of Contents

- [Overview](#overview)
- [Phase 0 — External Prerequisites](#phase-0--external-prerequisites)

1. [Goals and Non-Goals](#1-goals-and-non-goals)
2. [Background and Current Architecture](#2-background-and-current-architecture)
3. [Why the pNFS SCSI Layout (RFC 8154)](#3-why-the-pnfs-scsi-layout-rfc-8154)
4. [High-Level Architecture](#4-high-level-architecture)
5. [Requirements](#5-requirements)
6. [Backend / Control-Plane (sbcli) Changes](#6-backend--control-plane-sbcli-changes)
7. [Export Registry and MDS Binding](#7-export-registry-and-mds-binding)
8. [Server-Side Design (MDS guest)](#8-server-side-design-mds-guest)
9. [CSI Controller Design](#9-csi-controller-design)
10. [CSI Node Design (pNFS client)](#10-csi-node-design-pnfs-client)
11. [Volume Handle and Data Model](#11-volume-handle-and-data-model)
12. [Volume Lifecycle Operations](#12-volume-lifecycle-operations)
13. [MDS Fault Tolerance, Migration, and Failover](#13-mds-fault-tolerance-migration-and-failover)
14. [Deployment: Helm, Operator, and Packaging](#14-deployment-helm-operator-and-packaging)
15. [Security Design](#15-security-design)
16. [Failure Modes and Edge Cases](#16-failure-modes-and-edge-cases)
17. [Observability](#17-observability)
18. [Open Questions](#18-open-questions)
19. [Phased Delivery Plan](#19-phased-delivery-plan)
20. [Test Plan](#20-test-plan)
21. [Appendix A — Reference Commands](#appendix-a--reference-commands)
22. [Appendix B — Glossary](#appendix-b--glossary)

---

## 1. Goals and Non-Goals

### 1.1 Goals

- Provide **`ReadWriteMany` (RWX)** persistent volumes backed by simplyblock storage, so that multiple pods on multiple worker nodes can share a single filesystem concurrently. The same path serves `ReadWriteOnce`, since what selects it is the StorageClass rather than the access mode (§9.1).
- Deliver near-block performance for the shared data path by using the **pNFS SCSI layout** (RFC 8154): the NFS server (Metadata Server, "MDS") hands out block layouts, and clients perform **direct NVMe-oF I/O** to the underlying namespaces, bypassing the MDS for bulk data.
- Reuse the existing simplyblock control-plane API, NVMe-oF connect/reconnect machinery, and CSI plumbing wherever possible.
- Support the full volume lifecycle for RWX volumes: create, delete, resize, snapshot, clone, and restore.
- Survive planned and unplanned MDS (server) migration with a bounded I/O freeze rather than data loss.

### 1.2 Non-Goals (initial release)

- Cross-cluster / cross-region RWX volumes (an RWX volume lives in exactly one simplyblock cluster).
- Automatic re-striping / re-balancing of an existing RWX volume across a changed set of storage nodes.
- Raw-block (`volumeMode: Block`) PVCs, which pNFS is not involved in. A raw-block claim is plain multi-attach: one namespace, several initiators, and no filesystem between them, which the existing block path already serves and KubeVirt live migration needs. Such a claim never reaches this path, because it has no filesystem to ask for: a `volumeMode: Block` claim whose class nevertheless names `fsType: pnfs` is refused, since volumeMode and the StorageClass contradict each other and honoring either would pick a winner the user did not.
- Windows / non-Linux clients (the pNFS SCSI layout client is Linux-only here).
- NFSv3 or plain (non-parallel) NFSv4 as a supported fallback product feature. Non-pNFS NFSv4.1 MDS-routed I/O exists only as an automatic degraded fallback (see §16).

---

## 2. Background and Current Architecture

The current driver provisions **RWO** (`SINGLE_NODE_WRITER`) volumes only. The relevant code:

| Concern                                                                    | Location                                   |
|----------------------------------------------------------------------------|--------------------------------------------|
| CSI entrypoint / flags                                                     | `cmd/main.go`                              |
| Controller RPCs (`CreateVolume`, `DeleteVolume`, snapshots, clone, expand) | `internal/csi/controller`                  |
| Node RPCs (`NodeStageVolume`, `NodePublishVolume`, heal/restage)           | `internal/csi/node`                        |
| Identity + capabilities                                                    | `internal/csi/identity`, `internal/driver` |
| Control-plane HTTP v2 client (`ClusterAPI` interface, `APIClient`)         | `internal/controlplane/client.go`          |
| Client wrapper, credential/TLS loading, `CreateLVolData`                   | `internal/controlplane/cluster.go`         |
| NVMe-oF initiator (`Connect`/`Disconnect`/`MonitorConnection`)             | `internal/initiator/initiator.go`          |
| Guardian (pod restart on total path loss)                                  | `internal/guardian/guardian.go`            |
| Volume handle parsing `{clusterID}:{poolID}:{lvolID}`                      | `atlas-lib/lvol/handle.go`                 |

Today's RWO data path:

1. `CreateVolume` → `sbclient.CreateVolume(CreateLVolData)` creates **one** lvol, and `publishVolume` fetches NVMe-oF connect info.
2. `NodeStageVolume` builds a host NQN from the node UID, calls `initiator.Connect()` (`nvme connect ...`), then `FormatAndMount` (default `ext4`, or `xfs`) at the staging path.
3. `NodePublishVolume` bind-mounts the staging path into the pod and registers with the Guardian.

Storage-node components already exist and are relevant to the server side of pNFS:

- **SNodeAPI:** a privileged, `hostNetwork` DaemonSet (`charts/spdk-csi/latest/spdk-csi/templates/storage-node.yaml`) launched with `python simplyblock_web/node_webapp.py storage_node_k8s`, health endpoint `/snode/check` on the snode API port. It host-mounts `/dev`, `/sys`, `/mnt`, `/lib/modules`, `/var/simplyblock`. It is SPDK/device-management focused, and pNFS adds nothing to it: the export assembly runs in the MDS guest (§6.4).
- **`csi-node`** (`csi-driver/internal/csi/node`): the CSI node plugin DaemonSet. It already owns NVMe-oF connect/reconnect and mount/format on every node. For pNFS it is the client: it attaches the namespace, mounts the export, and takes the first layout (§10). It serves no export.
- **Operator and CRDs** under `helm-charts/charts/simplyblock-operator/crds/`: `StorageCluster`, `StorageNodeSet`, `StorageNode`, `StorageNodeOps`, `StoragePool`, `ControlPlane`, `Task`, `VolumeMigration`, and the replication and backup families. Node state (`online`, `offline`, `in_restart`, and the rest) lives in `internal/utils/constants.go`.
- Per-node status query: `GET /api/v2/clusters/{clusterID}/storage-nodes/{nodeID}/` (`getStorageNodeStatus`, `internal/controlplane/client.go`).

---

## 3. Why the pNFS SCSI Layout (RFC 8154)

Plain NFS (v3 / v4.x) routes **all** data through a single server process, making the NFS head a throughput and latency bottleneck and a single point of contention. simplyblock's value proposition is direct, low-latency NVMe-oF I/O. **pNFS decouples metadata from data**:

- The **Metadata Server (MDS)** owns the filesystem namespace, handles `LOOKUP`/`OPEN`/locking, and hands clients a **layout** describing where a file's blocks physically live.
- With the **SCSI layout type**, that "where" is a set of block devices (here, the **NVMe-oF namespaces**) plus block extents. The client then does **direct block I/O** to those namespaces over NVMe/TCP, in parallel, bypassing the MDS entirely for data.

This matches the PoC notes precisely:

- Clients attach the underlying NVMe-oF namespaces directly (`nvme connect`) and create a
  `/dev/disk/by-id/` alias so the kernel can match the designator the MDS names in the
  layout. **The name is `nvme-eui.${NGUID}`.** `bl_parse_scsi` builds the path itself and
  tries exactly three prefixes, in order — `dm-uuid-mpath-0x`, `wwn-0x`, then `nvme-eui.`.
  udev creates none of them for an NVMe-oF namespace (it makes `nvme-uuid.` and a model
  alias), so csi-node must. The lookup is the client kernel's own, and it resolves the
  path in the mount namespace of the task that asked for the layout, which is why the
  node plugin takes the first layout itself (§10.1).
- The XFS filesystem is exported with the `pnfs` option. **XFS is the only Linux filesystem that can act as a pNFS SCSI-layout server**.
- **Persistent Reservations are required before a layout is issued at all.** `nfsd`
  registers its own reservation key (`NFSD_MDS_PR_KEY`) on the exported device inside
  `nfsd4_scsi_proc_getdeviceinfo`, and refuses to hand out a SCSI layout if that
  registration fails. Fencing a dead or misbehaving client is the second thing reservations
  buy, not the first. A namespace without the capability logs
  `pNFS: failed to register key for device nvme0n1` on the MDS, `GETDEVICEINFO` returns an
  error, and every client silently falls back to MDS-routed I/O (§16, FM-2).
- **The capability is `ptpl_file`, not a flag on the bdev.** A namespace is
  Persist-Through-Power-Loss capable exactly when it was added with a `ptpl_file`
  (SPDK `nvmf_ns_is_ptpl_capable()` is `ns->ptpl_file != NULL`), and only such a namespace
  advertises RESCAP bit 0. Without it `rescap` reads `0xfe` — every reservation type
  except persistence — and since the Linux kernel sets PTPL=1 unconditionally in
  `nvme_pr_register`, every kernel-side registration is rejected with
  "Invalid Field in Command." Fixed in sbcli's namespace-add path.
- **A kernel carrying `nvme_get_unique_id`** is required on both clients and MDS, which is
  what lets nfsd identify an NVMe device to name it in a layout. This is a symbol, not a
  version: RHEL 9.8's 5.14.0-687 has it and works, RHEL 9.5's 5.14.0-503 does not and
  reports `pnfs=not configured` on every client.

If the client cannot establish the block path (device missing, fenced, reservation conflict), NFSv4.1 **transparently falls back to routing that I/O through the MDS**. Correctness is preserved, throughput degrades. That is the safety net (§16).

---

## 4. High-Level Architecture

```
        ┌───────────────── simplyblock control plane (HTTP v2) ──────────────────┐
        │  create -pr lvol   ·   per-lvol snapshot   ·   node inventory          │
        └───────▲──────────────────────────────────────────────▲─────────────────┘
                │                                              │
   ┌────────────┴──────────────┐   NFSExport CR   ┌────────────┴────────────────┐
   │      CSI Controller       │◀────────────────▶│   Operator (leader)         │
   │  creates the lvol         │                  │  - binds the MDS pod        │
   │  creates the NFSExport CR │                  │  - Service + EndpointSlice  │
   └───────────────────────────┘                  │  - failover + PR fencing    │
                                                  └──────────┬──────────────────┘
                                                             │ csi-link
                                        ┌────────────────────▼─────────────────┐
                                        │  MDS pod: QEMU guest, per cluster    │
                                        │   mds-agent: connect ns, mkfs.xfs,   │
                                        │              mount, exportfs         │
                                        │   nfsd + rpc.mountd + nfsdcld        │
                                        └────────────▲─────────────────────────┘
                                                     │ NFSv4.1 metadata
                          Service ClusterIP ─────────┘ (stable across failover)
                                                     │
   ┌──────────── worker node (pNFS client) ──────────┴──┐
   │ csi-node                                           │
   │  - nvme connect the SAME namespace  ───────────────┼── direct NVMe-oF I/O ──▶ namespace
   │  - nvme-eui. alias, first-layout probe             │
   │  - mount -t nfs -o v4.1 <clusterIP>:/mnt/{pvc} …   │
   │ Pods: RWX mount, many nodes                        │
   └────────────────────────────────────────────────────┘
```

**Roles**:

- **pNFS clients:** Worker nodes that host pods with RWX mounts, gated by a kernel carrying `nvme_get_unique_id` and the pNFS block layout client (P0-9, P0-10). They run the CSI node plugin, which takes the first layout itself (§10.1).
- **MDS:** one pod per storage cluster, running a QEMU guest with its own kernel ([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md)). The guest runs `nfsd`, owns every exported XFS filesystem of its storage cluster, and runs `mds-agent`, which connects the backing namespaces and assembles exports on the operator's request. No Kubernetes node runs `nfsd` or mounts an export.
- **Operator:** owns the `NFSExport` CR, the authoritative mapping of `{RWX volume → MDS pod → backing lvol → export path}` (§7.1). It enforces the "one MDS per export" invariant, reconciles the per-export Service, and drives failover.

**Hard invariant (from PoC):** each client export (PVC) is associated with **exactly one** MDS server. The server (and its attached NVMe-oF namespaces) may *migrate*, but the client never fails over to a *different* export. Migration causes a bounded I/O freeze until clients reconnect.

---

## 5. Requirements

### 5.1 Functional

- **FR-1** Provision an RWX PVC of size `S` as one lvol of size `S`, GiB-aligned per `util.AlignToGiBBytes`.
- **FR-2** Each lvol is created with persistent reservations enabled (`-pr`).
- **FR-3** Bind each volume to the MDS of its storage cluster, durably. The binding must survive controller restarts.
- **FR-4** In the MDS guest: attach the lvol, `mkfs.xfs`, mount it, add a `pnfs` export, and publish it with `exportfs`.
- **FR-5** On each client node: attach the same lvol, create the `nvme-eui.${NGUID}` alias under `/dev/disk/by-id/`, mount the export's Service address via NFSv4.1 into the pod, and take the first layout from the node plugin's own container (§10.1).
- **FR-6** Support delete, online resize, snapshot (quiesced through `xfs_freeze` on the MDS), clone, and restore for RWX volumes.
- **FR-7** Advertise the `MULTI_NODE_MULTI_WRITER` access mode, and select the pNFS path from `csi.storage.k8s.io/fstype: pnfs`, leaving every class that does not name it behaving exactly as before.
- **FR-8** Handle planned and unplanned MDS migration with automatic client reconnect.

### 5.2 Non-Functional

- **NFR-1** Direct block data path throughput within ~10–15% of the same lvol accessed as an RWO volume, for large sequential I/O. Aggregate throughput above one lvol is the striped design's target, not this one's.
- **NFR-2** MDS migration client-visible freeze ≤ configurable bound (target: ≤ 30 s, driven by NFS lease + NVMe `ctrl-loss-tmo` + reconnect-delay, mirroring existing initiator tunables).
- **NFR-3** No data corruption under client death, layout recall, node partition, or MDS migration (PR fencing + XFS journaling must guarantee this).
- **NFR-4** RWX provisioning must not regress RWO provisioning latency or reliability.
- **NFR-5** All new host-side actions run through the CSI node plugin or the MDS guest's agent (no direct SSH). Operations must be idempotent and safely retryable.

### 5.3 Compatibility / Environment

- Client kernels carrying `nvme_get_unique_id` (RHEL 9.8's 5.14.0-687 qualifies) and the pNFS block layout client (`CONFIG_PNFS_BLOCK`). The MDS guest brings its own kernel.
- `mount.nfs` on clients, which the node plugin image ships in `nfs-utils`. No host runs an NFS server.
- RHEL-family (RHEL, Rocky, Alma, Oracle, Amazon Linux) is the validated baseline. Debian/Ubuntu (`nfs-common`, differing `/dev/disk/by-id` naming) is **explicitly a testing risk** (see §18, §20).

---

## 6. Backend / Control-Plane (sbcli) Changes

The control plane / CLI backend has **no existing NFS/pNFS code**, so this is greenfield, but it already has useful scaffolding: NVMe-oF **`allowed_hosts`** on lvols (reuse for client allow-listing, §15), erasure-coding geometry (`ndcs`/`npcs`), and the required API layers (**FastAPI v2**). The API and the SPDK RPC layer need work.

### 6.1 Preconditions (must land first)

Collected with every other external dependency in
[Phase 0 — External Prerequisites](#phase-0--external-prerequisites). The
backend-specific detail follows here. These must land in the simplyblock
backend/API **before** the CSI work can be completed and validated:

1. **`-pr` flag on lvol create.** Extend the v2 volume-create API and `util.CreateLVolData` with a persistent-reservations flag (e.g., `"pr": true` / `"persistent_reservation": true`). Without it, pNFS SCSI-layout fencing is impossible.
2. **The csi-link export service** (P0-3), so the operator can ask the MDS guest to assemble an export. Consistency-group snapshots are *not* a precondition of this design, because a single-volume export snapshots per lvol (§6.3).
3. **(Recommended) Export/server association store**, or agreement that the CSI-owned Export Registry (§7.1) is authoritative.

### 6.2 Persistent reservations (`ptpl_file`) on the lvol's namespace

This does not belong on `bdev_lvol_create` as an `enable_persistent_reservation` flag
threaded through the v2 API, the controller, the model, and the DTO. **That would be the
wrong layer.** Reservations are a property of the NVMe-oF *namespace*, not of the bdev
under it, and the parameter that governs them already exists: `ptpl_file` on
`nvmf_subsystem_add_ns`.

A namespace is Persist-Through-Power-Loss capable exactly when it was added with a
`ptpl_file` (SPDK's `nvmf_ns_is_ptpl_capable()` is `ns->ptpl_file != NULL`), which is what
sets RESCAP bit 0; without it every namespace reads `rescap = 0xfe`. sbcli's
`rpc_client.py` already sets `ptpl_file`, but only inside an `if eui64:` branch that no
caller ever triggers. The fix keys the file off whichever identifier the namespace has, so
no API, model, or DTO change is needed — which also removes the
`enable_persistent_reservation` field §9.3 step 3 asks `CreateLVolData` to carry.

Two operational consequences follow, both open:

- `ptpl_file` writes one small JSON file per namespace under `/mnt` on the storage node.
  That path must be writable and genuinely persistent (not the ramdisk SPDK keeps its
  socket on), and the file count scales with lvol count.
- The parameter applies at namespace-add time, so **volumes created before the fix stay
  `rescap = 0xfe`** until their namespace is re-added. Whether that needs a migration is a
  product decision.

Existing accepted create params to reuse: `size`, `pool`, `crypto` (encryption), QoS (`max_rw_iops`/…), `ha_type`, `host_id`, `priority_class`, `fabric`, `max_namespace_per_subsys`, `ndcs`/`npcs`, **`allowed_hosts`** (already present, used for §15 host allow-listing), `uid`, `pvc_name`. Note that the API spells encryption `encrypt` and the priority class `priority_class`.

> Note: the port-level `PortReservation` in `models/cluster.py` is a transactional FDB lock for NVMe-oF port allocation, **unrelated** to SCSI/NVMe persistent reservations.

### 6.3 Snapshots of a single-volume RWX export

A single-volume RWX export is one lvol, so a snapshot of it is the per-lvol
snapshot the control plane already takes: `controllers/snapshot_controller.py`
`add()` calling `rpc_client.lvol_create_snapshot()`. Nothing new is needed from
the backend for §12.2 to work.

The one thing the driver must do is quiesce the filesystem before it asks. A
snapshot of a mounted XFS that is still taking writes is crash-consistent at the
block layer only, so recovery on restore depends on the XFS log. The MDS guest
holds the only mount, which is what makes this tractable: its agent can
`xfs_freeze -f` the export, take the snapshot, and `xfs_freeze -u`, and no client
holds a competing mount. Freeze duration lands in the client-visible latency
budget and is bounded by the same control channel as every other export
operation (§6.4).

Snapshotting a **striped** RWX volume is a different problem: it needs all `n`
member snapshots to be crash-consistent with one another, which needs a backend
consistency group that does not exist. That, and the user-facing
`VolumeGroupSnapshot` built on the same primitive, is
[`design-pnfs-striped.md`](design-pnfs-striped.md).

### 6.4 Where the NFS server and the export logic run

Three concerns live in the MDS guest (full split in §8), and none of them on a
Kubernetes node. None is an sbcli change.

**(a) NVMe-oF connection of the backing namespace.** The guest is an NVMe-oF
initiator for the volume's namespace just like a client. `mds-agent` connects it
through atlas-lib's `nvmeof`, the connect path the node plugin uses too, as the
guest's own host NQN. That NQN derives from the MDS StatefulSet and survives pod
restarts ([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md) §6.5).

**(b) The NFS server.** The guest's kernel `nfsd` with `rpc.mountd` and `nfsdcld`,
started by the guest's init at boot. The client recovery database `nfsdcld` keeps
lives on the MDS state disk, a simplyblock volume, so clients reclaim their state
when the pod restarts on any node.

**(c) Export assembly and control.** `mkfs.xfs`, the mount, the `exports.d`
entry, and `exportfs` run in `mds-agent` (`csi-driver/cmd/mds-agent`, with the
assembler in `csi-driver/internal/nfsexport`). The SNodeAPI stays SPDK and
device-management focused and carries none of it.

**The control channel is csi-link.** No pod consumes a volume on the MDS, so
kubelet never issues `NodeStageVolume` for the export there, and the operator asks
the guest to assemble it. It does so over **csi-link**, the operator-to-CSI
channel (`atlas-lib/link` and `atlas-lib/node`, with `operator/internal/csilink`
and `csi-driver/internal/csilink` as the two ends). Two properties of that channel
shape this design:

- **The MDS dials the operator.** The MDS pod opens a TLS session
  carrying its ServiceAccount token, the operator authenticates it by TokenReview
  and derives the peer's identity from the token's pod claims, and both ends then
  speak gRPC over a yamux mux. Export calls reach the agent in the guest through
  the pod. The operator has no ingress to nodes and does not want any.
- **"No session for the MDS" is a normal state.** The MDS is
  legitimately disconnected while its guest boots or restarts. An export operation
  aimed at an MDS with no live session is a requeue, and §16 carries it as a
  failure mode rather than an outage.

Only the leader-elected operator replica accepts sessions, so export operations are
driven from the same replica that owns every other reconcile.

### 6.5 v2 API additions (CSI-facing)

Route handlers live under `simplyblock_web/api/v2/` (`volume.py`, `snapshot.py`, `cluster.py`, `storage_node.py`), request/response models in `api/v2/dtos.py`. The v2 tree already exposes `.../volumes/{id}/connect`, `.../hosts/`, `.../hosts/{nqn}/secret`, `.../migration/`, and `/tasks/`.

The endpoints this design calls, and their state today:

| Method | Endpoint                                                        | Notes                                                                                                                 |
|--------|-----------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| `POST` | `/api/v2/clusters/{id}/storage-pools/{id}/volumes/`             | Needs `enable_persistent_reservation` in the body (§6.2). Idempotent by volume name, as today. **Not shipped** (P0-1) |
| `POST` | `/api/v2/clusters/{id}/storage-pools/{id}/snapshots/`           | Existing per-lvol snapshot. Used unchanged (§6.3)                                                                     |
| `POST` | `/api/v2/clusters/{id}/storage-pools/{id}/volumes/{id}/connect` | Existing. Returns the connection set for the backing namespace                                                        |

Every mutating call above is retried by a reconciler after an ambiguous timeout,
so each one has to be idempotent by the name or id in the request: the operator
cannot distinguish a call that never arrived from a response that was lost.

---

## 7. Export Registry and MDS Binding

### 7.1 The `NFSExport` CRD

The authoritative record per RWX volume is **a CRD owned by the operator**, not a
control-plane object. Earlier drafts left the backing store open; it is settled
here, and §18 no longer carries the question.

The reasons are all about who reads it. Every consumer is in-cluster: the CSI
controller writes it while provisioning, csi-node reads it while staging, and the
operator rewrites it while draining or failing over a node. A CRD gives those
consumers a watch, which is what §13 actually needs when a node plugin has to
notice that its export moved. Against a control-plane object the same thing is a
poll. Single-writer semantics on the bound MDS are `resourceVersion` and
optimistic concurrency, which is the discipline every other controller here
already follows. And the control plane has no NFS concept at all, so putting the
record there means inventing a backend domain object for a feature that only
exists inside Kubernetes.

The argument the other way is that a backend record would outlive the cluster and
serve a non-Kubernetes consumer. There is no such consumer for RWX-over-pNFS.

```go
// NFSExportPhase is the lifecycle phase of an export.
// +kubebuilder:validation:Enum=Pending;Assembling;Ready;FailingOver;Degraded;Deleting
type NFSExportPhase string

const (
    NFSExportPhasePending     NFSExportPhase = "Pending"
    NFSExportPhaseAssembling  NFSExportPhase = "Assembling"
    NFSExportPhaseReady       NFSExportPhase = "Ready"
    NFSExportPhaseFailingOver NFSExportPhase = "FailingOver"
    NFSExportPhaseDegraded    NFSExportPhase = "Degraded"
    NFSExportPhaseDeleting    NFSExportPhase = "Deleting"
)

// NFSExportSubPhase is the step within a failover, which is multi-step and has to
// resume from a known point after an operator restart.
// +kubebuilder:validation:Enum=Quiescing;Fencing;Selecting;Assembling;Repointing;Verifying
type NFSExportSubPhase string

// NFSExportSpec is the desired state: which volume is exported, and under what policy.
// The bound MDS is NOT here. It is an observed binding the operator owns, so it
// lives in status where a user edit cannot race the failover machine.
type NFSExportSpec struct {
    // VolumeRef is the CSI volume handle this export serves. Immutable.
    // +kubebuilder:validation:Required
    // +k8s:immutable
    VolumeRef string `json:"volumeRef"`

    // ExportPath is the server-side mount point, carrying namespace and UID
    // information so two same-named PVCs cannot collide on one host. Immutable.
    // +kubebuilder:validation:Required
    // +k8s:immutable
    ExportPath string `json:"exportPath"`

    // FSID is the NFS fsid for this export, allocated cluster-wide unique and stable
    // for the export's lifetime so file handles survive a move (§8.4). Immutable.
    // +kubebuilder:validation:Required
    // +k8s:immutable
    FSID string `json:"fsid"`

    // ClientPolicy constrains which clients may mount, as policy rather than as a
    // membership list. The effective client set is observed in status, because it
    // changes as pods are scheduled and must not bump generation (§15).
    // +optional
    ClientPolicy *NFSExportClientPolicy `json:"clientPolicy,omitempty"`
}

// NFSExportStatus is what the export currently is. Everything the operator decides
// lives here.
type NFSExportStatus struct {
    // Phase is the export lifecycle position.
    // +optional
    Phase NFSExportPhase `json:"phase,omitempty"`

    // SubPhase is the active failover step, persisted so a restart between
    // quiescing, fencing, assembly, and re-pointing resumes rather than restarts.
    // +optional
    SubPhase NFSExportSubPhase `json:"subPhase,omitempty"`

    // MDSPodName names the metadata server pod serving this export. Operator-owned:
    // it is the serialization point for the one-MDS-per-export invariant (§13.2).
    // +optional
    MDSPodName string `json:"mdsPodName,omitempty"`

    // ServiceName is the Service whose ClusterIP clients mount (§13.3). Stable for
    // the export's lifetime, which is the point of it.
    // +optional
    ServiceName string `json:"serviceName,omitempty"`

    // MDSNodeIP is the MDS pod's IP currently behind that Service, recorded for
    // diagnosis rather than for clients to use.
    // +optional
    MDSNodeIP string `json:"mdsNodeIP,omitempty"`

    // FailoverGeneration counts completed failovers. A node plugin watching this CR
    // uses a bump to know its export was re-materialized elsewhere.
    // +optional
    FailoverGeneration int64 `json:"failoverGeneration,omitempty"`

    // LVolID and NGUID identify the backing namespace.
    // +optional
    LVolID string `json:"lvolID,omitempty"`
    // +optional
    NGUID string `json:"nguid,omitempty"`

    // AllowedClients is the effective client set written into the exports entry, derived
    // from ClientPolicy and current pod placement.
    // +optional
    AllowedClients []string `json:"allowedClients,omitempty"`

    // Conditions carry why an export is Degraded or FailingOver, which a phase alone
    // cannot express. Expected types: Assembled, Exported, Fenced, Addressable.
    // +optional
    // +patchStrategy=merge
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // +optional
    Message string `json:"message,omitempty"`
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=nfsexp
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".spec.volumeRef"
// +kubebuilder:printcolumn:name="MDS",type=string,JSONPath=".status.mdsPodName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SubPhase",type=string,JSONPath=".status.subPhase"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=".status.serviceName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// NFSExport is one pNFS export: a volume, the MDS serving it, and the address
// clients mount. The operator owns the binding and the failover.
type NFSExport struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   NFSExportSpec   `json:"spec,omitempty"`
    Status NFSExportStatus `json:"status,omitempty"`
}
```

The object is named by a DNS-safe encoding of the volume handle rather than the
handle itself (§11), because the handle contains colons.

**A finalizer is mandatory.** The CR is the only record of a mount and an exports
entry in the guest, a Service, and an attached namespace. Without
`storage.simplyblock.io/nfsexport` on it, a direct delete leaves every one of those
orphaned with nothing left to describe them. Deletion order is unexport, unmount,
release the namespace, delete the Service and EndpointSlice, then drop the
finalizer.

A CR as it exists once Ready:

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: NFSExport
metadata:
  name: nfsexp-7b41c0e2a9
  namespace: simplyblock
  finalizers:
    - storage.simplyblock.io/nfsexport
spec:
  volumeRef: "nfs:0f2a…:pool-a:3c81…"
  exportPath: /mnt/team-a-shared-data-3c81
  fsid: "0x9a41c07b"
  clientPolicy:
    mode: NodeScoped
status:
  phase: Ready
  mdsPodName: simplyblock-pnfs-mds-0f2ac1d3-0
  serviceName: nfsexp-7b41c0e2a9-mds
  mdsNodeIP: 10.244.3.17
  failoverGeneration: 2
  lvolID: 3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13
  nguid: eui.0025388501b8f3a2
  allowedClients:
    - 10.42.4.21
    - 10.42.5.33
  conditions:
    - type: Assembled
      status: "True"
      reason: MountPresent
    - type: Exported
      status: "True"
      reason: ExportfsApplied
  observedGeneration: 4
```

Requirements that fall out:

- **Durable and idempotent.** `CreateVolume` is retried by the external-provisioner, so the CR is created-or-fetched by a name derived from the volume handle, and a retry leaks neither an lvol nor an export.
- **Single-writer on `status.mdsPodName`.** This is the "one MDS per export" invariant, and it is the invariant that keeps two MDS instances from mounting one XFS (§13). Failover is a phase transition that bumps `status.generation`.
- **`NodeStageVolume` reads it** for the Service name, the backing lvol, and its NGUID, instead of re-deriving them.

### 7.2 MDS Binding

- **One MDS per storage cluster.** The operator binds an export to the MDS of the
  storage cluster its volume lives in, and creates that MDS on the cluster's first
  export ([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md) §7.3). An export waits
  in `Pending` until the MDS pod's guest is healthy. With no `spec.pnfs.mds` on any
  `SimplyblockDriver` there is no MDS to bind, and the export waits with a
  `NoMetadataServer` event.
- **Volume placement:** the backing lvol and the MDS are placed independently.
  The MDS reaches the namespace over NVMe-oF exactly as a client does, so it need
  not run near the data, and its pod can be rescheduled to any node offering KVM.

### 7.3 Kernel / Capability Enforcement

At Helm install and continuously via the operator, each worker node that may host
RWX pods is checked for a kernel carrying `nvme_get_unique_id` and the pNFS block
layout client. A node that does not qualify is marked **not compatible** (label +
event) so scheduling can avoid it. No node is checked for server capabilities,
because the MDS guest brings its own kernel and NFS server.

---

## 8. Server-Side Design (MDS guest)

Everything on the server side runs in the MDS guest
([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md)), split between the guest's own
services and `mds-agent` (§6.4):

- **NVMe-oF connection of the backing namespace** → **`mds-agent`**, through
  atlas-lib's `nvmeof`. The guest is an NVMe-oF *initiator* for the namespace
  exactly like a client, as its own host NQN.
- **Export assembly and control** (XFS, mount, and `exportfs`) → **`mds-agent`**,
  with the same volume stack plan the block path stages a filesystem volume with.
- **The NFS server:** the guest's kernel `nfsd` with `rpc.mountd` and `nfsdcld`,
  started at the guest's boot.

The `CreateExport`, `CheckExport`, and `DeleteExport` operations below are agent
routines. Each one is **idempotent**, safely re-runnable, and takes the export spec
the operator derives from the `NFSExport` CR. The operator invokes them on the MDS
over csi-link (§6.4). It is the sole caller, so export ownership does not straddle
two components.

### 8.1 Guest prerequisites

The guest image carries `nfs-utils`, and its kernel carries `nfsd` with the SCSI
layout (`CONFIG_NFSD_SCSILAYOUT`) and the NVMe-oF/TCP initiator. The agent checks
before each assembly that `nfsd` is running and its control filesystem mounted,
and brings either up when it is not, so an assembly never publishes an export
nothing serves.

### 8.2 `pnfs.CreateExport(record)` — assemble and export

The export path cannot be `/mnt/{pvc-name}`: PVC names are unique only within a
namespace, so two same-named PVCs in different namespaces would collide on one MDS
MDS, both in the mount point and in the exports table. The path carries namespace and
UID information, and the same identifier names the `fsid` allocation below.

Idempotent steps, each skipped when already satisfied:

1. **Attach the namespace:** the agent connects the backing namespace as the guest's host NQN and waits for the device to appear, then proceeds.
2. **Filesystem:** `mkfs.xfs` on the namespace, only if it is not already formatted, detected through `blkid`. XFS is not a choice here: it is the only filesystem a SCSI layout can be served from, which is why `fsType: pnfs` names the path rather than a format (§9.1).
3. **Mount:** create `/mnt/{pvc-name}` and mount the device there.
4. **Export:** write the guest's `/etc/exports.d/{pvc}.exports` entry:
   ```
   /mnt/{pvc-name} {allowed-client-set}(rw,sync,no_subtree_check,no_root_squash,pnfs,fsid={fsid})
   ```
   The PoC used `*` for the client set, and §15 tightens this.
5. **Publish:** `exportfs -ra`.

There is no volume manager in this path. A single namespace is formatted directly,
which removes `pvcreate`, `vgcreate`, `lvcreate`, and the deterministic VG and LV
naming a stripe depends on. Striping puts all of
that back, which is one of the reasons it is a separate design.

`fsid` comes from the CR and never changes, so a re-run after an MDS restart
reproduces the same file handles (§13.2). That forces the allocation to be
**cluster-wide unique**: one MDS serves every export of its storage cluster, and an
allocator that could repeat a value would collide two exports on one server. Allocation is therefore the
operator's, from a cluster-scoped range recorded on the CR (§18, Q1). Because the device is formatted rather than
assembled, re-materializing an export elsewhere is a mount, not a rebuild.

### 8.3 `pnfs.DeleteExport(record)` — teardown (reverse order)

1. `exportfs -u` and remove the drop-in file, then `exportfs -ra`.
2. `umount /mnt/{pvc-name}`, then remove the directory.
3. Release the backing namespace.
4. Signal the controller to delete the lvols (control-plane owns lvol deletion).

### 8.4 The NFS root (`fsid=0`) export

NFSv4 requires a pseudo-root. Establish **once per MDS** a root export:
```
mount -t nfs -o v4.1 {mds-ip}:/ /...    # requires an fsid=0 root on the server
```
The MDS exports a root with `fsid=0`, and each PVC export gets a **stable unique `fsid`** (stored in the `ExportRecord`) so remounts and restarts keep the same file handles. `fsid` allocation must be collision-free per MDS (Open Question §18).

### 8.5 Restart support

The agent's `CreateExport` is the primitive the restart flow uses (§13). Because the mount point and the export are derived deterministically from the volume identifier and the `fsid`, re-materializing the export in a restarted guest reproduces the same NFS file handles, so clients recover against handles they already hold rather than remounting from scratch. There is no volume manager in this path, so re-materializing is a mount rather than a rebuild.

---

## 9. CSI Controller Design

Changes in `internal/csi/controller`, `internal/controlplane/cluster.go`, `internal/controlplane/client.go`.

### 9.1 Access-mode / capability changes

- Advertise `MULTI_NODE_MULTI_WRITER` in the driver's access modes (`internal/csi-common/driver.go` via `AddVolumeCapabilityAccessModes`, currently only `SINGLE_NODE_WRITER` in `sanity_test.go`), because an export can serve many writers and a block volume cannot.
- **The StorageClass selects the pNFS path, through `csi.storage.k8s.io/fstype: pnfs`, and nothing else does.** `CreateVolume` branches on that string. The access mode is orthogonal: `ReadWriteOnce` over pNFS is as valid as `ReadWriteMany` and gets the same machinery, and a claim that does not ask for `pnfs` takes the block path whatever its access mode.
- The value names a request rather than an on-disk format. What the metadata server makes is XFS, the only filesystem a SCSI layout can be served from (§8.2), and what a client mounts is NFS 4.1. So `xfs` is not a way to ask for this path: it asks for a block device with XFS on it, which is a different product.
- Routing on the access mode instead would change what an existing StorageClass provisions. `ReadWriteMany` on an ordinary filesystem is what every release before this one served, and raw-block `ReadWriteMany` is the multi-attach KubeVirt live migration needs, so neither can be reinterpreted as a request for an export.
- The `MULTI_NODE_*` modes an export cannot serve, read-only and single-writer, are rejected rather than routed, so such a request does not silently get shared-writer behavior.
- No group capability is advertised. `GROUP_CONTROLLER_SERVICE` and `CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT` belong to the striped design (§9.5), and claiming them here would have the driver advertise a service it does not implement.

### 9.2 New StorageClass parameters

| Parameter                                                                                                   | Meaning                                    | Default |
|-------------------------------------------------------------------------------------------------------------|--------------------------------------------|---------|
| `pnfs` / `access_protocol: nfs`                                                                             | Opt into the pNFS path                     | off     |
| `nfs_mount_options`                                                                                         | Extra NFS mount options appended to `v4.1` | none    |
| (reuse) `pool_name`, `cluster_id`/`zone_cluster_map`/`region_cluster_map`, QoS, `compression`, `encryption` | as today                                   | —       |

Parsed alongside the existing keys in `prepareCreateVolumeReq` (`controllerserver.go`).

### 9.3 `CreateVolume` (pNFS path)

The order matters, because the CR's required fields are immutable and cannot be
filled in later. Everything `spec` needs is therefore known before the CR is
created, and everything the operator decides lands in `status` afterward.

1. Resolve cluster selection and pool (existing `resolveClusterSelection`, `NewsimplyBlockClient`).
2. **Derive the identity:** the export UUID, the volume handle (§11), the object name, the export path, and the `fsid` from the cluster-wide allocator (§8.2). These are all `spec` fields and all immutable, so they are computed before anything is created.
3. **Create the lvol** with persistent reservations at size `S`, GiB-aligned. `CreateLVolData` carries the flag under the **same name the backend uses**, `enable_persistent_reservation` (§6.2), rather than a shorter CSI-side spelling for a value that crosses the boundary.
4. **Create the `NFSExport` CR** idempotently by that object name, with the lvol id and NGUID written to `status`. It enters `status.phase = Pending` with no MDS bound.
5. **Hand off.** The operator's `NFSExportReconciler` binds the MDS pod, records it in `status.mdsPodName`, drives `CreateExport` over csi-link, reconciles the Service, and moves the CR to `Ready`. The CSI controller does not bind the MDS and does not call it, which is what keeps provisioning out of the failover path.
6. **Wait for `Ready`**, then build the CSI `Volume`:
   - `VolumeId` is the pNFS handle (§11).
   - `VolumeContext` carries `access_protocol=nfs`, `export_service`, `export_path`, `fsid`, and the backing `lvolID:nguid` pair with its NVMe connect hints.
   - `AccessibleTopology` where topology mapping applies.
7. Return. Every step is idempotent, because the external-provisioner retries.

Step 6 blocks on a reconciler, so `CreateVolume` returns `Unavailable` while the export
is still assembling and lets the provisioner retry rather than holding the RPC open.
That is the existing convention for asynchronous work behind a CSI call.

**RBAC this needs, which does not exist yet.** The CSI controller's ServiceAccount
currently has no access to `storage.simplyblock.io`: its roles cover PVs, PVCs,
snapshots, nodes, and attachments. It needs create, get, and watch on `nfsexports`
to do step 4 at all, and csi-node needs get and watch to read its export at stage
time and to notice a `failoverGeneration` bump (§13.4). Both are narrow additions to
the chart's `simplyblock-csi-controller-role` and `simplyblock-csi-node-role`, and
neither exists today, so this is work rather than an assumption.

### 9.4 Error handling / rollback

- If binding or `CreateExport` fails after lvols exist, the record stays in `Provisioning` and a retry resumes. A background reconciler (operator) garbage-collects orphaned lvols/exports for records stuck in `Provisioning`/`Deleting` past a timeout.
- Reuse existing error mapping (`ErrVolumeExists`, HTTP status → CSI codes).

### 9.5 Group Controller Service (out of scope)

A single-volume RWX export needs no group primitive, so this design implements no
CSI GroupController service and advertises no group capability. `CreateSnapshot` on
an RWX volume is the ordinary per-lvol path (§12.2).

The upstream `VolumeGroupSnapshot` feature, the GroupController service that backs
it, and the Kubernetes and sidecar version floors it forces are all in
[`design-pnfs-striped.md`](design-pnfs-striped.md), because the thing that makes
them necessary is striping.

## 10. CSI Node Design (pNFS client)

Changes in `internal/csi/node` and initiator reuse in `internal/initiator/initiator.go`.

### 10.1 `NodeStageVolume` (pNFS path)

Detect the pNFS path from `VolumeContext` (`access_protocol=nfs`). Then:

1. **Attach the namespace:** through atlas-lib's `nvmeof` (`nfsexport.Attach`), the connect path the guest's agent uses too, as the node's host NQN.
2. **Device identity:** resolve the namespace's NGUID and publish the `nvme-eui.`
   alias the client kernel looks for. This is a sysfs read, not a subprocess: `atlas-lib`'s
   `nvme` package already surfaces it as `Namespace.NGUID` (`sysfs_scan.go`), and
   its scan covers every NVMe subsystem on the host rather than only simplyblock
   ones, so nothing about it is volume-vendor specific.

   Two small additions to `atlas-lib` belong with it rather than here (P0-5).
   `nvme.DeviceSelector` today matches on NQN, NSID, UUID, and device path, so it
   needs an `NGUID` field and a by-NGUID lookup for the reverse direction. And the
   `/dev/disk/by-id/nvme-eui.${NGUID}` alias should be created by a helper in
   `atlas-lib`, which today only resolves such aliases (`nvmeof/wait.go`), instead
   of by shelling out from csi-node. Until those land, the shell equivalent is:
   ```
   NGUID=$(nvme id-ns /dev/nvmeXnY | awk '/^nguid/{print $3}')
   ln -sf /dev/nvmeXnY /dev/disk/by-id/nvme-eui.${NGUID}
   ```
   The symlink is what lets the client kernel open the device the layout names
   (§3). **RHEL-family validated. Debian and Ubuntu device naming must be
   tested** (P0-11).
3. **NFS mount** at the staging path:
   ```
   mount -t nfs -o v4.1[,<extra opts>] {export_service}:/mnt/{export-path} <stagingPath>
   ```
   The server-side `fsid=0` root export makes the pseudo-root resolvable.
4. **Take the first layout.** The node plugin writes and syncs a probe file
   (`nfsexport.LayoutProbeName`) in the export from its own container, which has
   the host's `/dev`, and removes it again (`primeLayout`,
   `csi-driver/internal/csi/node/pnfs_prime.go`). A pod's `/dev` has no
   `disk/by-id`, so a device lookup a pod triggers fails and marks the device
   unavailable for two minutes. The device the probe resolves is cached for the
   whole client, and every pod's later layout reuses it.
5. **Stash the volume context:** the member list, `mds_ip`, and `fsid` go through `StashVolumeContext` for use at unstage and heal.

The kernel automatically issues `LAYOUTGET` and performs direct block I/O over the attached namespaces. If the block path is unavailable it falls back to MDS-routed I/O.

### 10.2 `NodePublishVolume`

Bind-mount the staging NFS mount into the pod target path (existing bind-mount logic). RWX means the same staging mount may be published to multiple pods on the node, so publish and unpublish must be **reference-counted** so unpublishing one pod does not unmount a still-in-use export.

### 10.3 `NodeUnstageVolume` / `NodeUnpublishVolume`

- Unpublish: unmount the pod bind mount, then decrement the refcount.
- Unstage (only when refcount hits zero): `umount` the NFS mount, then `initiator.Disconnect()` each member namespace, then clean up the `nvme-eui.` aliases and stashed context.

### 10.4 Reconnect / heal for pNFS

- The existing **`MonitorConnection`** / Guardian machinery watches the underlying NVMe-oF namespaces. On total path loss it can trigger reconnect just like RWO.
- **An MDS restart keeps the mount address.** The client mounts the export's Service ClusterIP, and the operator repoints the Service's EndpointSlice at the restarted pod (§13.3), so the client reconnects and reclaims its state without a remount.
- **A restart drops the client's cached devices,** because the restarted server is a new server instance. The node plugin watches `/proc/self/mountstats` and probes each pNFS staging mount again when its transport to the MDS reconnects (`KeepLayoutsPrimed`). The guest's `nfsd` holds the client's other layouts until that probe has taken one, so the probe is first again rather than racing the pods (§13.4).
- Detect dead NFS mounts (`ESTALE`/`ENOTCONN`) using the same `stagingMountDead` approach adapted for NFS, and restage.

### 10.5 Node capabilities

Keep `STAGE_UNSTAGE_VOLUME`. `NodeGetVolumeStats` uses `statfs` on the NFS mount (works unchanged for filesystem mode). `NodeExpandVolume` for pNFS is a **no-op on the client**, because the XFS grow happens in the MDS guest (§12.3).

---

## 11. Volume Handle and Data Model

The current handle is `{clusterID}:{poolID}:{lvolID}` and `lvol.ParseHandle` enforces exactly three parts with a UUID cluster and lvol (`atlas-lib/lvol/handle.go`). A pNFS volume adds an MDS binding and an export, so the handle needs a form of its own.

**The handle is `nfs:{clusterID}:{poolID}:{exportUUID}`,** a synthetic four-part form that `lvol.ParseHandle` learns alongside the existing three-part one. `exportUUID` keys the `NFSExport` CR, and everything else is read from there. The handle stays synthetic even though this design has exactly one backing lvol, because reusing the lvol id would make the handle change identity the moment striping arrives.

**The handle is not the object name.** Colons are not valid in a Kubernetes object name, so the `NFSExport` CR is named by a deterministic DNS-safe encoding of the handle rather than the handle itself: a stable hash of `{clusterID}/{poolID}/{exportUUID}` truncated to the label limit, with the full handle carried in `status` for lookup. Collision handling is the usual one for a truncated hash, which is to detect a mismatch on read and fail rather than adopt someone else's export.

`csicommon.ParseVolumeHandle` and `lvol.ParseHandle` must learn the pNFS form while **remaining backward compatible** with existing three-part RWO handles, where an unknown or invalid handle is treated as belonging to another driver, as today.

`VolumeContext` (returned by `CreateVolume`, consumed by the node) for pNFS:

```
access_protocol = "nfs"
export_service  = "<Service DNS name or ClusterIP the client mounts (§13.3)>"
export_path     = "/mnt/{namespace}-{pvc-name}-{uid-suffix}"
fsid            = "<stable fsid>"
lvol            = "<lvolID:nguid>"
nvme_connect    = "<json: connect hints (nqn/port/transport)>"
generation      = "<failover counter>"
clusterID / topology keys as today
```

---

## 12. Volume Lifecycle Operations

Every operation acts on the single backing lvol and the export.

### 12.1 Delete
`DeleteVolume`: set `status.phase = Deleting` → `pnfs.DeleteExport` (unexport and unmount, after which the agent releases the namespace) → delete the lvol through the control plane → delete the CR. Idempotent, and tolerant of partial prior progress (`ErrVolumeNotFound` treated as success, as today).

### 12.2 Snapshot
`CreateSnapshot` on an RWX volume is the ordinary per-lvol snapshot, with one
addition: the MDS guest's agent freezes the filesystem with `xfs_freeze -f`
before the control-plane call and thaws it after, so the snapshot is taken against
a quiesced log rather than mid-write (§6.3). The MDS holds the only mount, which is
what makes the freeze safe to take. The CSI snapshot id keeps the existing
`{clusterID}:{poolID}:{snapshotUUID}` form.

### 12.3 Resize
`ControllerExpandVolume`: resize the lvol (GiB-aligned), then run **`xfs_growfs` in the MDS guest** over csi-link, because an XFS grow needs the mounted path and the MDS holds it. `NodeExpandVolume` is a client-side no-op (§10.5), which differs from RWO where the client grows the filesystem.

### 12.4 Clone / Restore
- **Clone from a volume or a snapshot, and restore:** the ordinary per-lvol clone the driver already performs, followed by the full §8.2 `CreateExport` flow on the MDS of the clone's storage cluster. The clone is an independent RWX volume with its own `NFSExport` CR, its own Service, and its own `fsid`. No consistency group is involved, because there is one lvol to clone.

---

## 13. MDS Fault Tolerance, Migration, and Failover

### 13.1 What tolerance is possible

A single XFS filesystem can be mounted by exactly one host, so there is no
active/active MDS and there never will be under this architecture. The shape is a
single MDS that restarts: one guest owns the mount, and when its pod is lost the
StatefulSet starts it again, on the same node or on any other node offering KVM,
and the new guest re-materializes the same exports.

That is less bad than it sounds, because of where the data path runs. Clients read
and write **directly over NVMe-oF**, so losing the MDS stops metadata operations
and leaves bulk I/O on already-held layouts running until the layout is recalled or
expires. The failure mode is a stall, not data loss, and not a full outage for
in-flight work. What the design owes is a bounded stall and a guarantee that
recovery cannot corrupt.

### 13.2 The invariant that matters

**Two MDS instances must never have the export's XFS mounted at the same time.** This
is the one place in the design where getting it wrong destroys data rather than
degrading service, and it is the reason persistent reservations are a hard
prerequisite rather than a hardening step.

A StatefulSet of one replica never runs two pods of the same name at once, and
the old guest dies with its pod. A node that loses contact is the exception: its
pod may still be running there, partitioned, paused, or about to resume with a
stale mount and dirty page cache. So a restart **fences before it serves**: the
restarted guest's `nfsd` preempts every reservation key that clients of an earlier
instance left on the namespace before it hands out a layout, which revokes their
write access and lets each client register its new key. Without P0-1 and P0-2 that step does
not exist, which is why this design cannot ship past a single-writer MVP without
them.

`status.mdsPodName` on the `NFSExport` CR is the serialization point. One writer,
optimistic concurrency, and no second MDS is even a candidate until that field is
rewritten. It is status rather than spec precisely so a user edit cannot make a
second MDS a candidate.

### 13.3 A stable mount address

**Clients mount a Kubernetes Service, not a node.** The operator gives each export a
Service and manages its EndpointSlice directly, with exactly one endpoint: the
MDS pod's IP, port 2049. When the pod restarts with a new IP the operator rewrites
that endpoint. The ClusterIP does not change, so the client's mount address does
not change, and no client remounts.

This is not a new mechanism here. The operator already fronts node-local services
this exact way: `BuildStorageNodeSetEndpointSlice` and `BuildSpdkProxyEndpointSlice`
in `operator/internal/utils/storage_nodeset_ds.go` build EndpointSlices over node
internal IPs, and `reconcileEndpointSlice` re-points them when a node moves.

`nfsd` itself is unaware of any of this. A ClusterIP is not an address anything
binds. It is a DNAT rule that kube-proxy programs on every node. The client
connects to the ClusterIP, the rule rewrites the destination to the MDS pod's IP,
and the pod passes the connection on to the guest, where `nfsd` sees an ordinary
connection on port 2049 ([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md)).
Host-originated traffic reaches those rules because `KUBE-SERVICES` is hooked into
`OUTPUT` as well as `PREROUTING`, and the mount here is issued by the client node's
host kernel. One ClusterIP per export, all targeting port 2049, composes
correctly: many exports share the guest's `nfsd`. A client reaches every export of
one MDS over a single connection, because the NFSv4.1 client recognizes the same
server behind each address and trunks the mounts onto one.

Three caveats ride along, and none of them is settled:

- **A kernel sunrpc mount is not a userspace socket.** Dataplanes that replace
  kube-proxy with eBPF socket load-balancing translate ClusterIPs by hooking
  `cgroup/connect4`, which sees userspace `connect()` calls. An NFS mount is a
  kernel sunrpc socket in the host namespace and may not be translated at all. The
  repository's own integration suites run stock kube-proxy, so the test beds are fine, but a
  cluster running eBPF kube-proxy replacement is a real risk and has to be
  validated rather than assumed (P0-8).
- **conntrack outlives the endpoint.** DNAT is decided per connection and cached, so
  after the endpoint is rewritten a stale entry can keep steering reconnects at the
  dead host until it is flushed or expires. That cleanup time is inside the NFR-2
  budget, not outside it.
- **A restart is a server reboot to the client.** An NFSv4.1 client identifies a
  server by the `server_owner` and `server_scope` it gets from `EXCHANGE_ID`, not
  by IP. The guest derives both from its hostname, which is the StatefulSet's pod
  name and survives restarts, so the client recognizes the server it knew. It
  reclaims its opens and locks during the server's grace period, which the client
  recovery database on the state disk lets `nfsd` admit (P0-7). Reclaims succeed
  because the file handles survive: the same `fsid` over the same on-disk XFS
  produces the same handles.

So the promise is a stable mount address and a reclaim. There is no remount and
no pod disruption. The client still performs
NFSv4.1 state recovery, and the cost of that recovery is what NFR-2 has to bound.

### 13.4 Planned restart

**Trigger:** the MDS pod is deleted or evicted, for a drain of its node or a new
MDS image.

1. The StatefulSet starts the pod again, on any node offering KVM, and the guest
   boots. `nfsd` starts its grace period.
2. The operator sees a guest instance other than the one that assembled each
   export (`status.assembledBy`) and runs `CreateExport` for every export of the
   storage cluster in the new guest, with the **same** `fsid` and export path,
   which reproduces the file handles
   ([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md) §7.5).
3. The operator rewrites each export's EndpointSlice to the pod's new IP.
4. Clients reconnect through the unchanged ClusterIP and reclaim their state
   (§13.3). Each client's node plugin sees the reconnect and takes the first
   layout again (§10.4).

The restart is transparent to a running application because of three changes to
the guest kernel's `nfsd` (pnfs-os `boards/pnfs-common/README.md`). It holds a
client's layouts until the node plugin's probe has taken one, so a pod never makes
the first device lookup. It fences reservation keys left by clients of an earlier
instance, falling back to a plain Preempt where the target refuses Preempt and
Abort, as SPDK's does. And during grace it answers a file handle of an export not
yet exported again with `NFS4ERR_DELAY` rather than `NFS4ERR_STALE`, so a reclaim
that arrives before its export waits instead of losing the open.

### 13.5 Unplanned failover

Same flow, with the loss detected rather than requested:

1. The operator observes the loss. The MDS pod turning unready is the coarse
   signal. `CheckExport` over csi-link, which asks the guest whether the export is
   mounted and published, is the precise one, because a guest can run with a dead
   `nfsd`.
2. **Fence.** The restarted guest's `nfsd` preempts every key of an earlier
   instance before it issues a layout (§13.2). Until the guest is back, the export
   stays bound and its clients stall rather than fail.
3. Continue at step 1 of §13.4.

The freeze bound in NFR-2 is therefore pod rescheduling and guest boot, plus
export assembly, plus EndpointSlice propagation and conntrack cleanup, plus the
grace period and client state recovery. Each term needs a measured number before
NFR-2 is a commitment rather than an aspiration. The test plan carries them as
`F-` scenarios.

## 14. Deployment: Helm, Operator, and Packaging

Since the CSI driver repository was merged into this one, there are **two chart
trees**, and both are still packaged and released: `helm-charts/charts/simplyblock-operator`
(built by `helm_lint.yaml` and merged by `helm_merge_charts.yaml`) and
`csi-driver/charts/spdk-csi/latest/spdk-csi` (built by `csi_chart_release.yaml`).
The operator-side work below lands in the first. Anything that changes the csi-node
DaemonSet has to be applied to both or deliberately dropped from the second, and
the csi-link wiring is a live example of the trap: it updated only the operator
chart.

### 14.1 Chart changes

**This section is written against a chart that installed the CSI driver. It no
longer does.** The node and controller plugins are rendered by the
`SimplyblockDriver` reconciler (`operator/internal/controllers/driver`), so
what this section calls a chart value is a field on that kind, and what it
calls a template is a Go builder. The list below restates this section in
those terms.

- **`spec.link`** on `SimplyblockDriver`, replacing the `csiLink.enabled` value
  the chart used to gate the plugins on. The chart value survives, and now
  configures only the operator's own end: the endpoint, the Service, and the
  certificate it serves. `spec.link` configures the plugins that dial it. Both
  halves have to be on, and they are separate objects, which is the one thing a
  reader of the old section would get wrong.
- **`spec.pnfs.mds`**, which runs the metadata server in a QEMU guest of its own
  (`design-pnfs-mds-vm.md`). The node plugin gets no NFS server state from the
  host: no node runs nfsd or mounts an export.
- **pNFS needs the link.** The operator assembles every export by calling the MDS
  over the link. With no link the MDS never has a session, so every export waits
  in `Assembling` until its five-minute deadline and then turns `Degraded`, with an
  `AssembleTimeout` event naming the MDS it could not reach.
- **Adoption compares rather than refuses.** csi-link was in `adoption.go`'s
  `inexpressible` list, so a deployment running it could not be adopted at all.
  With `spec.link` it becomes `linkAdoptionMismatch`, on `tlsAdoptionMismatch`'s
  pattern: a running link with no `spec.link` is refused because the next apply
  would drop it, and the reverse is refused because it would start every plugin
  dialing an endpoint the cluster may not serve.
- **RBAC.** Neither plugin could reach `storage.simplyblock.io` before this: their
  roles covered PVs, PVCs, snapshots, nodes, and attachments. The controller
  plugin now has a role of its own -- the first thing on that pod that is the
  driver's rather than an upstream sidecar's -- carrying create, get, list, and
  watch on `nfsexports` plus its status. The node plugin's role gains get, list,
  and watch, and deliberately no write: the operator drives assembly, and a node
  that could write its own record could bind an export to itself.
- **`NFSExport` CRD** shipped in `helm-charts/charts/simplyblock-operator/crds/`
  as `storage.simplyblock.io_nfsexports.yaml`, generated by `make manifests`.

**What a pod cannot provide.** Every host that may run an RWX pod needs a kernel
whose NVMe driver exports `nvme_get_unique_id` and whose NFS client carries the
pNFS block layout driver (P0-9, P0-10). These are node-OS prerequisites. The MDS
needs a node with `/dev/kvm` and nothing else from it: the guest brings its own
kernel and NFS server.

**The second chart tree.** `csi-driver/charts/spdk-csi/latest/spdk-csi` still
installs a node DaemonSet of its own, and this design does not extend it. That is
a deliberate drop rather than an oversight: that chart installs no operator, and
without one no MDS exists and nothing drives assembly.

The `snapshot-controller`, `csi-snapshotter`, and `csi-provisioner` version floors
that earlier drafts raised here belong to the striped design, because only the
group-snapshot feature needs them. For the record, the chart already ships
`csi-snapshotter` and `snapshot-controller` at v8.2.0 and `csi-provisioner` at
v5.1.0, so those floors are already met.

### 14.2 Operator changes

The operator owns cluster, node, and pool lifecycle plus drain and migration. pNFS
layers onto that without disturbing RWO. What it already has and this design uses:
node labeling (`io.simplyblock.storagenodeset`), a
`/snode/info` readiness poll (`SNODEAPIResponse`, `checkNodeInfoReachable()`,
`pollNodeOnline()`), EndpointSlice management over node IPs
(`BuildStorageNodeSetEndpointSlice`, `reconcileEndpointSlice`), the `Task` polling
CR, and `StorageNodeOps` for imperative node actions.

Two corrections to earlier drafts, both from the three-tier model that landed in
#319, and the `StoragePool` rename in #414:

- **`performNodeAction()` no longer exists.** Node actions are `StorageNodeOps` CRs
  with `spec.action` (`shutdown`, `restart`, `suspend`, `resume`, `remove`,
  `migrate`, from `internal/utils/constants.go`), driven by `StorageNodeOpsReconciler`
  through its own sub-phases. An export-quiesce step is a sub-phase of that
  reconciler, not a new case in a function.
- **Per-node state lives on `StorageNode`, not on `StorageNodeSet.status.nodes[]`.**
  `StorageNode` is one CR per (worker, socket, node index), owned by a
  `StorageNodeSet`, with `spec.storageNodeSetRef` immutable and `spec.workerNode`
  re-pointed by the operator only during a `migrate`.

**CRD and API-model additions:**

| CRD / type       | File                                    | Additions                                                                                                                                    |
|------------------|-----------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------|
| `NFSExport`      | `api/v1alpha1/nfsexport_types.go` (new) | The export registry itself (§7.1). One CR per RWX volume, owned by the operator, watched by csi-node.                                        |
| `StorageCluster` | `api/v1alpha1/storagecluster_types.go`  | spec: `nfsEnabled`, `nfsExportPolicy`, `nfsSecurityFlavor`. Reuse existing `snodeApiPort` and `clientDataIfname`.                            |
| `StoragePool`    | `api/v1alpha1/storagepool_types.go`     | spec: `supportedAccessModes[]` (RWO, RWX), `nfsExportPolicy`. Renamed from `Pool` in #414.                                                   |
| `Task`           | `api/v1alpha1/task_types.go`            | No schema change. Existing polling surfaces any new backend task types.                                                                      |
| API params       | `internal/utils/types.go`               | `ClusterAddParams` gains `nfs_enabled`; `PoolAddParams` gains `nfs_export_policy` and `access_modes`. Both structs keep their current names. |

**Reconciler changes:**

- **`NFSExportReconciler`** (new, `internal/controller/nfsexport_controller.go`): owns
  the export lifecycle. Creates the MDS on a storage cluster's first export, drives
  `CreateExport`, `CheckExport`, and `DeleteExport` on the bound MDS over csi-link,
  reconciles the Service and EndpointSlice for §13.3, and reassembles every export
  after an MDS restart (§13.4). It is the only writer of `status.mdsPodName`.
- **`StorageClusterReconciler`** and **`StoragePoolReconciler`**: pass the new NFS
  fields through.

> Division of labor: the **CSI controller** creates and deletes the `NFSExport` CR as
> part of provisioning (§9). The **operator** owns everything that happens to an
> export afterward, including the MDS binding, the Service, and restarts. The CR is
> the boundary, which is what keeps the CSI controller out of the failover path.

## 15. Security Design

The PoC exports are open to everyone (`*`). **This is the largest open security gap** and must be closed before GA.

**Threats:** any host that can reach the MDS IP can mount the export. Any host that can reach the NVMe-oF targets can attach the raw namespaces and read/write shared data, bypassing NFS permissions entirely.

**Controls (layered)**:

1. **NVMe-oF host allow-listing (most important).** The direct data path is raw block. Restrict each namespace's `allowed_hosts` to the specific client NQNs + the MDS NQN (the driver already threads `host_nqn` through `getLvolConnections`/`VolumeInfo`). Only nodes whose NQN is allow-listed can attach the namespaces. PR fencing (`-pr`) complements this by revoking access on layout recall.
2. **Export client restriction.** Replace `*` in the exports entry with the concrete set of client node data IPs/subnets bound to the volume, maintained from the ExportRecord as pods schedule/deschedule. Consider `sec=krb5`/`krb5i`/`krb5p` for authenticated/encrypted metadata (Open Question, because Kerberos needs infrastructure).
3. **Network isolation.** Keep NVMe-oF + NFS on the storage data network. NetworkPolicies and firewall rules prevent arbitrary pods or nodes from reaching MDS IPs and NVMe targets.
4. **In-pod user restriction.** `no_root_squash` (PoC) is dangerous for multi-tenant clusters, because a container root can write as root on the shared FS. Evaluate `root_squash`/`all_squash` + `anonuid`/`anongid`, and per-PVC ownership/`fsGroup`. This is an Open Question tied to tenancy model.
5. **Transport security.** Reuse existing TLS for control/SNodeAPI. NVMe/TCP TLS (if available) and Kerberos for NFS are stretch goals.

---

## 16. Failure Modes and Edge Cases

| #     | Scenario                                                                | Expected behavior                                                                                                                                                                                                                      |
|-------|-------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| FM-1  | Client dies holding a layout                                            | MDS recalls the layout, PR fences the dead client, XFS integrity is preserved, and other clients continue.                                                                                                                             |
| FM-2  | Block/direct path unavailable (namespace missing, reservation conflict) | NFSv4.1 falls back to MDS-routed I/O. Degraded throughput, no data loss. **Silent:** the mount succeeds, every byte is correct, and md5 matches across nodes. Only `LAYOUTGET` and the NFS read and write byte totals reveal it (§20). |
| FM-3  | MDS pod or its node lost (unplanned)                                    | The StatefulSet restarts the pod and the operator reassembles every export in the new guest (§13). Clients freeze, then reclaim within the NFR-2 bound.                                                                                |
| FM-4  | Partial provisioning failure (some lvols created, export not)           | Record stuck in `Provisioning`. A retry resumes, and the reconciler GCs after a timeout.                                                                                                                                               |
| FM-5  | The backing namespace is lost                                           | XFS errors and the export goes `Degraded`. Redundancy is the backend's responsibility through per-lvol erasure coding or replication. This design adds no redundancy of its own.                                                       |
| FM-6  | Snapshot requested on a volume whose filesystem cannot be frozen        | The freeze is attempted, and a failure aborts the snapshot rather than taking an unquiesced one. A single-volume snapshot needs no consistency group (§6.3), so it is never refused for that reason.                                   |
| FM-7  | `fsType: pnfs` on a `volumeMode: Block` claim                           | Rejected by `CreateVolume`: the class asks for a filesystem and the claim for a raw device.                                                                                                                                            |
| FM-8  | Two pods on different nodes writing same file                           | Handled by NFSv4 byte-range locking via the MDS. Correctness is NFS's responsibility.                                                                                                                                                  |
| FM-9  | A pod triggers the first device lookup                                  | The lookup resolves `/dev/disk/by-id` in the pod's mount namespace, which has none, so the device is marked unavailable and I/O silently falls back to the MDS. The node plugin takes the first layout itself (§10.1, §10.4).          |
| FM-10 | Debian/Ubuntu `/dev/disk/by-id` naming differs                          | The alias step may fail → no direct path, silently. Must be tested per-distro (§18).                                                                                                                                                   |
| FM-11 | Provisioner double-`CreateVolume`                                       | Idempotent fetch-or-create by stable name, yielding at most one set of lvols and one export.                                                                                                                                           |
| FM-12 | Node reboot with active RWX mounts                                      | Restage reconnects the namespaces and remounts NFS. Refcounted publish rebuilds the pod mounts.                                                                                                                                        |
| FM-13 | `fsid` collision on the MDS                                             | Provisioning fails cleanly. The allocator must guarantee per-MDS uniqueness.                                                                                                                                                           |

---

## 17. Observability

### Kubernetes Events

| Event                                                 | Type      | Reason                                         |
|-------------------------------------------------------|-----------|------------------------------------------------|
| Node kernel below the pNFS minimum                    | `Warning` | `NodeNotPNFSCapable`                           |
| No driver configures the MDS                          | `Warning` | `NoMetadataServer`                             |
| Export assembled and published                        | `Normal`  | `ExportReady`                                  |
| Export teardown completed                             | `Normal`  | `ExportRemoved`                                |
| Export state transition (`Provisioning`, `Migrating`) | `Normal`  | `ExportStateChanged`                           |
| MDS migration started, finished                       | `Normal`  | `MDSMigrationStarted`, `MDSMigrationCompleted` |
| Client fenced by persistent reservation               | `Warning` | `ClientFenced`                                 |
| Stripe member unavailable                             | `Warning` | `StripeMemberUnhealthy`                        |

### Prometheus Metrics

| Metric                                        | Labels                         | Description                                                      |
|-----------------------------------------------|--------------------------------|------------------------------------------------------------------|
| `simplyblock_pnfs_provision_duration_seconds` | `cluster`, `result`            | RWX provisioning latency and success or failure                  |
| `simplyblock_pnfs_migrations_total`           | `cluster`, `result`            | MDS migrations by outcome                                        |
| `simplyblock_pnfs_migration_freeze_seconds`   | `cluster`                      | Client freeze window per migration, the NFR-2 bound              |
| `simplyblock_pnfs_direct_io_ratio`            | `cluster`, `export`            | Direct block versus MDS I/O, read from the NFS layout statistics |
| `simplyblock_pnfs_export_failovers_total`     | `cluster`, `export`, `outcome` | Completed failovers, by outcome, including fence failures        |
| `simplyblock_pnfs_exports`                    | `cluster`, `state`             | Exports per MDS by state                                         |

### Logs and status

Export actions in the MDS guest are logged per command, including the
idempotency skips, so a partially assembled export is reconstructable from the
log alone. Member connect and disconnect keep the existing initiator logging, and
the client's layout probe is logged on stage and after each reconnect. The export record surfaces
`state`, `generation`, the bound MDS, and member health, which is what a
`kubectl describe` has to answer without reading a node log.

## 18. Open Questions

Four of the questions earlier drafts carried here are now decided, and are recorded
where they belong rather than left open: the export registry is a CRD (§7.1), the
control channel is csi-link (§6.4), the mount address is a Service ClusterIP
(§13.3), and the volume handle keeps the synthetic `nfs:` form (§11).

| #   | Question                                                                                                                                                                                                           | Owner      |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| Q1  | **`fsid` allocation:** which component allocates collision-free `fsid`s per MDS, and which one owns the pseudo-root (`fsid=0`)? Per-MDS uniqueness is structural here, because one MDS serves many exports (§8.4). | Operator   |
| Q3  | **Service ClusterIP from a kernel mount:** does a sunrpc mount reach a ClusterIP on every supported CNI dataplane, specifically under eBPF kube-proxy replacement (§13.3)?                                         | Spike      |
| Q4  | **Debian and Ubuntu:** `/dev/disk/by-id` naming and the `nfs-common` difference, and therefore whether the distro matrix can include them (§5.3).                                                                  | Spike      |
| Q5  | **Tenancy model:** `no_root_squash` lets a container root write as root on the shared filesystem. What squash and `fsGroup` model applies, and is Kerberos in scope for GA (§15)?                                  | Product    |
| Q7  | **Guardian interaction:** does the existing `MonitorConnection` and Guardian machinery extend to an NFS mount, or does a pNFS mount need its own monitor (§10.4)?                                                  | CSI driver |

---

## 19. Phased Delivery Plan

The phases below deliver this document. Striping and everything it forces is
[`design-pnfs-striped.md`](design-pnfs-striped.md), which begins where this ends.

- **Phase 0 (external enablers):** everything in
  [Phase 0 — External Prerequisites](#phase-0--external-prerequisites). The `-pr`
  flag and its SPDK support, the csi-link export service, and the spike that
  decides what §13 may promise. None of it is
  implementable in this repository.
- **Phase 1 (export lifecycle):** the `NFSExport` CRD, its reconciler, the MDS
  binding, and `CreateExport` and `DeleteExport` over csi-link.
  Access mode `MULTI_NODE_MULTI_WRITER`, the client NFS mount, and create and delete
  only. This is the end-to-end pNFS path with the fewest moving parts.
- **Phase 2 (stable addressing):** the per-export Service and operator-managed
  EndpointSlice, and the planned restart on top of it (§13.4).
- **Phase 3 (fault tolerance):** the MDS health probe, PR fencing, and the
  unplanned restart (§13.5), with the freeze bound measured rather than asserted.
- **Phase 4 (lifecycle completion):** snapshot with `xfs_freeze` on the MDS, clone,
  restore, and online resize.
- **Phase 5 (security hardening):** export client restriction, NVMe host
  allow-listing, the squash and tenancy model, and optionally Kerberos.
- **Phase 6 (scale and GA):** load and soak testing, scale limits, docs, and the
  distro matrix.

Phase 1 depends on P0-1 through P0-4. Phases 2 and 4 are independent of each other
and can run in parallel. Phase 3 depends on Phase 2 for the address and on P0-1 and
P0-2 for the fence.


## 20. Test Plan

The full scenario matrix for this design (with types, the axis
coverage, and the gap list) lives in
[`tests/test-plan-pnfs-rwx.md`](../tests/test-plan-pnfs-rwx.md). The plan covers
this repository only: the backend preconditions in §6.1 are external blockers,
and what the plan asserts is the operator and driver behavior against them, faked
at the boundary.

What each class of test has to prove:

- **CSI unit (`U-`):** StorageClass parsing and the `CreateVolume` plan, the
  volume handle round-trip, the node-stage plan, and the export record's state
  machine. Mock `ClusterAPI` and mock HTTP, with no cluster.
- **Operator (`O-`):** binding exports to the MDS, reassembly after an MDS
  restart, and teardown. Fake client and `envtest`.
- **Integration (`I-`):** the export assembly order and its idempotency, with
  host commands stubbed. The step sequence and the mid-way retry are what break.
- **End-to-end (`E-`, `F-`, `SEC-`, `L-`):** shared read and write across pods,
  the direct block path, migration freeze inside the NFR-2 bound, fencing, and
  soak. A live cluster on kernel 6.11 or later, with the environment requirements
  listed in the plan.

**Every end-to-end scenario must assert that a layout was actually used**, not that the
data was correct. Correctness holds in the degraded path by design (FM-2), so a test that
checks only the bytes passes just as happily with pNFS switched off entirely. The
assertions that bite are `pnfs=LAYOUT_SCSI` in the mount's `nfsv4:` line, a non-zero
`LAYOUTGET` with zero errors on `GETDEVICEINFO`, and an NFS read byte delta of
approximately zero across a large read with caches dropped. All three are in
`/proc/self/mountstats`.

Risk concentrates in three places, and their scenarios must not be the ones cut:
the export assembly's idempotency (`I-01`, `I-03`, `I-06`), the migration freeze
window (`F-01`, `F-02`, `F-06`, `F-07`), and data integrity under concurrent
writers (`E-03`, `F-09`, `L-04`). The matrix has no multi-namespace scenario yet,
which is where the export path and the `fsid` can collide. The plan records that
as a gap.

## Appendix A — Reference Commands

**MDS guest (server side, run by `mds-agent`):**
```bash
# nfsd, rpc.mountd, and nfsdcld are started by the guest's init at boot.
# the agent connects the namespace, then assembles the export on the device:
mkfs.xfs <dev>                           # only when blkid shows it is unformatted
mkdir -p /mnt/<pvc-name>
mount <dev> /mnt/<pvc-name>
# /etc/exports.d/<pvc>.exports  (tighten client set in §15)
#   /mnt/<pvc-name> <clients>(rw,sync,no_subtree_check,no_root_squash,pnfs,fsid=<fsid>)
exportfs -ra

# online grow (resize), after the lvol itself has grown
xfs_growfs /mnt/<pvc-name>

# quiesce for a snapshot (§12.2)
xfs_freeze -f /mnt/<pvc-name>
xfs_freeze -u /mnt/<pvc-name>

# teardown, in reverse
exportfs -u <clients>:/mnt/<pvc-name> && exportfs -ra
umount /mnt/<pvc-name> && rmdir /mnt/<pvc-name>
```

**Client (node side):**
```bash
nvme connect ...                  # attach the SAME namespace the MDS formatted
# NGUID and the nvme-eui. alias come from atlas-lib once P0-5 lands; until then:
NGUID=$(nvme id-ns /dev/nvme0n1 | awk '/^nguid/{print $3}')
ln -sf /dev/nvme0n1 /dev/disk/by-id/nvme-eui.${NGUID}
mount -t nfs -o v4.1 <export-service-clusterip>:/mnt/<pvc-name> <staging-path>
# take the first layout from a context with the host's /dev (the node plugin's):
dd if=/dev/zero of=<staging-path>/.simplyblock-pnfs-layout-probe bs=4k count=1 conv=fsync
rm <staging-path>/.simplyblock-pnfs-layout-probe
# NFSv4 pseudo-root (server exports fsid=0):
#   mount -t nfs -o v4.1 <export-service-clusterip>:/ <path>
```

## Appendix B — Glossary

- **pNFS:** Parallel NFS (NFSv4.1+ extension) separating metadata from data.
- **MDS:** Metadata Server: the `nfsd` owning the namespace and issuing layouts, here a QEMU guest per storage cluster.
- **Layout / SCSI layout:** description of where a file's data blocks live. The SCSI layout (RFC 8154) points clients at block devices (here, NVMe-oF namespaces) for direct I/O.
- **PR:** Persistent Reservations (SCSI-3 / NVMe): used by pNFS SCSI layout to fence clients and protect shared devices.
- **Consistency group:** a set of lvols snapshotted or cloned atomically so a striped filesystem stays crash-consistent. Out of scope here, and the subject of [`design-pnfs-striped.md`](design-pnfs-striped.md).
- **SNodeAPI:** the simplyblock storage-node agent (`simplyblock_web/node_webapp.py`). pNFS adds nothing to it.
- **`csi-node`:** the CSI node plugin (`csi-driver/internal/csi/node`). It owns NVMe-oF connect and reconnect and, for pNFS, the client side: the namespace attach, the mount, and the first-layout probe (§10).
- **`mds-agent`:** the export assembler in the MDS guest (`csi-driver/cmd/mds-agent`): namespace attach, XFS, mount, and `exportfs` (§8).
- **csi-link:** the operator-to-CSI channel (`atlas-lib/link`, `atlas-lib/node`) that carries export operations to the MDS (§6.4).