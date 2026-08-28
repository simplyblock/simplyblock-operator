# Design Document: Single-Volume pNFS (RWX) Support for the Simplyblock CSI Driver

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-07-02 (last updated 2026-08-25)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/278  
**Target release:** 26.4  
**Test Plan:** [`tests/test-plan-pnfs-rwx.md`](../tests/test-plan-pnfs-rwx.md)  
**Follow-on design:** [`design-pnfs-striped.md`](design-pnfs-striped.md) adds striping across
several lvols, consistency-group snapshots, and the user-facing `VolumeGroupSnapshot`.

---

## Phasing Overview

| Phase | Delivers                                                                | Depends on                      | Status  |
|-------|-------------------------------------------------------------------------|---------------------------------|---------|
| 0     | External prerequisites (§Phase 0)                                       | Other repos, branches, spikes   | Blocked |
| 1     | `NFSExport` CRD and reconciler, MDS selection, export create and delete | P0-1 through P0-4               | Planned |
| 2     | Per-export Service and EndpointSlice, planned migration (§13.4)         | Phase 1                         | Planned |
| 3     | MDS health probe, PR fencing, unplanned failover (§13.5)                | Phase 2, P0-1, P0-2, P0-7, P0-8 | Planned |
| 4     | Snapshot with `xfs_freeze`, clone, restore, online resize               | Phase 1                         | Planned |
| 5     | Export client restriction, host allow-listing, squash and tenancy       | Phase 1                         | Planned |
| 6     | Load and soak, scale limits, docs, distro matrix                        | Phases 1 through 5              | Planned |

Phases 2 and 4 are independent and can run in parallel. Phase 3 is the one that
turns a working export into a survivable one, and it is the phase whose promises
depend on spikes rather than on code (§13.3).

---

## Overview

**What this is.** RWX (`MULTI_NODE_MULTI_WRITER`) volumes for the Simplyblock CSI
driver, built on pNFS with the SCSI/block layout. One storage node acts as the
metadata server (MDS) for a volume and exports an XFS filesystem over NFS 4.1.
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

**The three moving parts.** An RWX volume is one ordinary lvol. csi-node on the
MDS host connects it, makes an XFS filesystem on it, mounts it, and exports it
(§8). The `NFSExport` CR records which MDS the volume is bound to, and at which
`fsid` and generation (§7.1). csi-node on each client connects the same namespace
and mounts the export, so `blkmapd` can map file layouts onto the local block
device (§10).

**Why the layout matters.** Without the block layout, every byte would cross the
MDS and RWX throughput would be capped by one node. With it, the MDS carries
metadata only, which is what makes the shared filesystem scale with the number of
clients rather than against it (§3).

**One MDS host, many exports.** `nfsd` is a kernel service and `/etc/exports` is
host-global, so a host runs one `nfsd` serving every export bound to it. The unit
of placement is therefore the export, not the server: each RWX volume is bound to
one MDS host, and one MDS host carries many volumes. Per-host `fsid` uniqueness
follows from this rather than being an edge case (§8.4).

**What is not decided or not available yet.** Fencing needs a persistent
reservation the control plane does not expose, and the operator-to-csi-node
control channel this design drives the export through is built but unmerged. Both
live in [Phase 0 — External Prerequisites](#phase-0--external-prerequisites). The
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

| #     | Prerequisite                                                                                                                                                                      | Kind                             | Blocks                                                     | Status                     |
|-------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------|------------------------------------------------------------|----------------------------|
| P0-1  | `-pr` flag on lvol create: a persistent-reservations flag on the v2 volume-create API, passed through to `bdev_lvol_create` (§6.1, §6.2)                                          | Control plane (`sbcli`)          | All fencing, so every phase past a single-writer MVP       | Not shipped                |
| P0-2  | Persistent-reservation support behind that flag, so a fenced client's writes are actually refused (§6.2, §15)                                                                     | Storage plane (SPDK)             | Fencing, and MDS failover safety (§13)                     | Unknown                    |
| P0-3  | The csi-link mutation gate opened, so the operator may drive `CreateExport` and `DeleteExport` on a node rather than only read from it (§6.4)                                     | Platform (`atlas-lib`, unmerged) | Every export operation, so all of §8                       | Built, unmerged, read-only |
| P0-4  | csi-node moved off nvme-cli `initiator.go` onto atlas `nvmeof`, which is what the mutation gate waits on (§6.4, §10.1)                                                            | Platform (`csi-driver`)          | P0-3                                                       | Not started                |
| P0-5  | `nvme.DeviceSelector.NGUID` and a by-NGUID lookup in atlas-lib, plus an `eui64` symlink helper, so the client stops shelling out to `nvme id-ns` (§10.1)                          | Platform (`atlas-lib`)           | Client device identity, though a shell-out works meanwhile | Not started                |
| P0-6  | Storage-node capability and inventory fields: `nfs_capable`, `nfsd_version`, `pnfs_available`, `kernel_version`, and the NFS data-network IP, surfaced on `StorageNodeDTO` (§6.6) | Control plane (`sbcli`)          | MDS selection without a per-node probe (§7.2)              | Not shipped, recommended   |
| P0-7  | An answer on NFSv4.1 `server_owner` and `server_scope` across MDS hosts: can they be made to match, or must the client do full state recovery on failover (§13)                   | Node OS, and a spike here        | The freeze bound in NFR-2, and what §13 may promise        | Open, spike needed         |
| P0-8  | Confirmation that a kernel sunrpc mount reaches a Service ClusterIP on the CNI dataplanes the product supports, including eBPF kube-proxy replacement (§13)                       | Environment, and a spike here    | The stable-address design in §13                           | Open, spike needed         |
| P0-9  | Client and MDS kernels ≥ 6.11 (§5.3)                                                                                                                                              | Node OS                          | Every phase: pNFS SCSI layout needs it                     | Environment requirement    |
| P0-10 | `nfs-utils` on MDS hosts (`nfsd`, `exportfs`) and `nfs-blkmap`/`blkmapd` on clients, plus `nfs-common` on Debian-family (§5.3)                                                    | Node OS                          | Export assembly and the client direct path                 | Environment requirement    |
| P0-11 | A Debian and Ubuntu spike: `/dev/disk/by-id` naming and the `nfs-common` difference (§5.3)                                                                                        | Node OS                          | The distro matrix commitment                               | Not started (§18, Q4)      |

**Without P0-1 and P0-2** there is no fencing, so an MDS failover cannot be made
safe and the feature cannot ship past a single-writer MVP. **Without P0-3** the
operator can read fabric state from a node but cannot ask it to assemble an
export, which is the whole server side of this design. **Without P0-6** MDS
selection falls back to a per-node capability probe (§7.2), which is slower and
less precise but not blocking. **P0-7 and P0-8** do not block a first
implementation, but they decide what §13 is allowed to promise, so they must be
answered before the freeze bound in NFR-2 is treated as a commitment. **P0-9 and
P0-10** are node-image requirements: a node that does not meet them must be
excluded from MDS selection and from RWX scheduling, which is a behavior this
repository owns and tests.

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
3. [Why pNFS SCSI/Block Layout](#3-why-pnfs-scsiblock-layout)
4. [High-Level Architecture](#4-high-level-architecture)
5. [Requirements](#5-requirements)
6. [Backend / Control-Plane (sbcli) Changes](#6-backend--control-plane-sbcli-changes)
7. [Export Registry and MDS Selection](#7-export-registry-and-mds-selection)
8. [Server-Side Design (MDS host)](#8-server-side-design-mds-host)
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

- Provide **`ReadWriteMany` (RWX)** persistent volumes backed by simplyblock storage, so that multiple pods on multiple worker nodes can share a single filesystem concurrently.
- Deliver near-block performance for the shared data path by using **pNFS SCSI/block layout**: the NFS server (Metadata Server, "MDS") hands out block layouts, and clients perform **direct NVMe-oF I/O** to the underlying namespaces, bypassing the MDS for bulk data.
- Reuse the existing simplyblock control-plane API, NVMe-oF connect/reconnect machinery, and CSI plumbing wherever possible.
- Support the full volume lifecycle for RWX volumes: create, delete, resize, snapshot, clone, and restore.
- Survive planned and unplanned MDS (server) migration with a bounded I/O freeze rather than data loss.

### 1.2 Non-Goals (initial release)

- Cross-cluster / cross-region RWX volumes (an RWX volume lives in exactly one simplyblock cluster).
- Automatic re-striping / re-balancing of an existing RWX volume across a changed set of storage nodes.
- RWX for raw-block (`volumeMode: Block`) PVCs. pNFS exports a **filesystem** (XFS), so RWX is filesystem-mode only.
- Windows / non-Linux clients (pNFS block layout + `blkmapd` is Linux-only here).
- NFSv3 or plain (non-parallel) NFSv4 as a supported fallback product feature. Non-pNFS NFSv4.1 MDS-routed I/O exists only as an automatic degraded fallback (see §16).

---

## 2. Background and Current Architecture

The current driver provisions **RWO** (`SINGLE_NODE_WRITER`) volumes only. The relevant code:

| Concern                                                                    | Location                                           |
|----------------------------------------------------------------------------|----------------------------------------------------|
| CSI entrypoint / flags                                                     | `cmd/main.go`                                      |
| Controller RPCs (`CreateVolume`, `DeleteVolume`, snapshots, clone, expand) | `pkg/spdk/controllerserver.go`                     |
| Node RPCs (`NodeStageVolume`, `NodePublishVolume`, heal/restage)           | `pkg/spdk/nodeserver.go`                           |
| Identity + capabilities                                                    | `pkg/spdk/identityserver.go`, `pkg/spdk/driver.go` |
| Control-plane HTTP v2 client (`ClusterAPI` interface, `APIClient`)         | `pkg/util/jsonrpc.go`                              |
| Client wrapper, credential/TLS loading, `CreateLVolData`                   | `pkg/util/nvmf.go`                                 |
| NVMe-oF initiator (`Connect`/`Disconnect`/`MonitorConnection`)             | `pkg/util/initiator.go`                            |
| Guardian (pod restart on total path loss)                                  | `pkg/util/guardian.go`                             |
| Volume handle parsing `{clusterID}:{poolID}:{lvolID}`                      | `pkg/kubernetes/volumehandle/index.go`             |

Today's RWO data path:

1. `CreateVolume` → `sbclient.CreateVolume(CreateLVolData)` creates **one** lvol, and `publishVolume` fetches NVMe-oF connect info.
2. `NodeStageVolume` builds a host NQN from the node UID, calls `initiator.Connect()` (`nvme connect ...`), then `FormatAndMount` (default `ext4`, or `xfs`) at the staging path.
3. `NodePublishVolume` bind-mounts the staging path into the pod and registers with the Guardian.

Storage-node components already exist and are relevant to the server side of pNFS:

- **SNodeAPI:** a privileged, `hostNetwork` DaemonSet (`charts/spdk-csi/latest/spdk-csi/templates/storage-node.yaml`) launched with `python simplyblock_web/node_webapp.py storage_node_k8s`, health endpoint `/snode/check` on the snode API port. It host-mounts `/dev`, `/sys`, `/mnt`, `/lib/modules`, `/var/simplyblock`. It is SPDK/device-management focused. For pNFS it only grows **capability reporting** (kernel/nfs eligibility), because the export assembly itself lives in csi-node (§6.4).
- **`csi-node`** (`csi-driver/pkg/spdk/nodeserver.go`): the CSI node plugin DaemonSet. It already owns NVMe-oF connect/reconnect and mount/format on every node. For pNFS it also runs on MDS and storage hosts and performs the server-side export assembly (XFS, mount, and `exportfs`).
- **Operator and CRDs** under `helm-charts/charts/simplyblock-operator/crds/`: `StorageCluster`, `StorageNodeSet`, `StorageNode`, `StorageNodeOps`, `StoragePool`, `ControlPlane`, `Task`, `VolumeMigration`, and the replication and backup families. Node state (`online`, `offline`, `in_restart`, and the rest) lives in `internal/utils/constants.go`.
- Per-node status query: `GET /api/v2/clusters/{clusterID}/storage-nodes/{nodeID}/` (`getStorageNodeStatus`, `pkg/util/jsonrpc.go`).

---

## 3. Why pNFS SCSI/Block Layout

Plain NFS (v3 / v4.x) routes **all** data through a single server process, making the NFS head a throughput and latency bottleneck and a single point of contention. simplyblock's value proposition is direct, low-latency NVMe-oF I/O. **pNFS decouples metadata from data**:

- The **Metadata Server (MDS)** owns the filesystem namespace, handles `LOOKUP`/`OPEN`/locking, and hands clients a **layout** describing where a file's blocks physically live.
- With the **SCSI/block layout type**, that "where" is a set of block devices (here, the **NVMe-oF namespaces**) plus block extents. The client then does **direct block I/O** to those namespaces over NVMe/TCP, in parallel, bypassing the MDS entirely for data.

This matches the PoC notes precisely:

- Clients attach the underlying NVMe-oF namespaces directly (`nvme connect`) and create `/dev/disk/by-id/nvme-eui64.${NGUID}` symlinks so the kernel's **`blkmapd`** (`nfs-blkmap.service`) can match the device signatures the MDS references in the layout.
- The XFS filesystem is exported with the `pnfs` option. **XFS is the only Linux filesystem that can act as a pNFS SCSI-layout server**.
- **Persistent Reservations (`-pr`)** are required: pNFS SCSI layout uses SCSI-3 / NVMe reservations so the MDS can **fence** a client (revoke its access to the shared namespaces) when it recalls a layout or a client misbehaves/dies. Without PR-based fencing, a stale client could corrupt shared data. This is why `-pr` is a hard backend precondition.
- **Kernel ≥ 6.11** on both clients and MDS is required for the XFS/nfsd pNFS block-layout code paths this design relies on.

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
   │  creates the lvol         │                  │  - selects the MDS host     │
   │  creates the NFSExport CR │                  │  - Service + EndpointSlice  │
   └───────────────────────────┘                  │  - failover + PR fencing    │
                                                  └──────────┬──────────────────┘
                                                             │ csi-link
                                        ┌────────────────────▼─────────────────┐
                                        │  MDS host                            │
                                        │   csi-node: connect ns, mkfs.xfs,    │
                                        │             mount, exportfs          │
                                        │   nfsd + rpc.mountd + rpc.statd      │
                                        └────────────▲─────────────────────────┘
                                                     │ NFSv4.1 metadata
                          Service ClusterIP ─────────┘ (stable across failover)
                                                     │
   ┌──────────── worker node (pNFS client) ──────────┴──┐
   │ csi-node                                           │
   │  - nvme connect the SAME namespace  ───────────────┼── direct NVMe-oF I/O ──▶ namespace
   │  - eui64 symlink + blkmapd                         │
   │  - mount -t nfs -o v4.1 <clusterIP>:/mnt/{pvc} …   │
   │ Pods: RWX mount, many nodes                        │
   └────────────────────────────────────────────────────┘
```

**Roles**:

- **pNFS clients:** worker nodes that host pods with RWX mounts. Selected/gated by kernel ≥ 6.11 and (optionally) a node label/taint. Run the CSI Node plugin + `blkmapd`.
- **MDS / export servers:** storage nodes (or dedicated export nodes) that run `nfsd`, own the exported XFS filesystem, and host `/etc/exports`. The co-located **csi-node** connects the backing namespace *and* drives the export lifecycle (XFS, mount, and `exportfs`). `nfsd`, `rpc.mountd`, and `rpc.statd` run as a co-located daemon (systemd or sidecar, §6.4(b)). One host serves every export bound to it. Gated by kernel ≥ 6.11 and eligibility recorded on the `StorageNode` CR.
- **Operator:** owns the `NFSExport` CR, the authoritative mapping of `{RWX volume → MDS host → backing lvol → export path}` (§7.1). It enforces the "one MDS per export" invariant, reconciles the per-export Service, and drives failover.

**Hard invariant (from PoC):** each client export (PVC) is associated with **exactly one** MDS server. The server (and its attached NVMe-oF namespaces) may *migrate*, but the client never fails over to a *different* export. Migration causes a bounded I/O freeze until clients reconnect.

---

## 5. Requirements

### 5.1 Functional

- **FR-1** Provision an RWX PVC of size `S` as one lvol of size `S`, GiB-aligned per `util.AlignToGiBBytes`.
- **FR-2** Each lvol is created with persistent reservations enabled (`-pr`).
- **FR-3** Select exactly one eligible MDS server per volume and bind it durably. The binding must survive controller restarts.
- **FR-4** On the MDS host: attach the lvol, `mkfs.xfs`, mount at `/mnt/{pvc-name}`, add a `pnfs` export, and run `exportfs -ra`.
- **FR-5** On each client node: attach the same lvol, create the `eui64` symlink, ensure `blkmapd` is running, and mount the export's Service address via NFSv4.1 into the pod.
- **FR-6** Support delete, online resize, snapshot (quiesced through `xfs_freeze` on the MDS), clone, and restore for RWX volumes.
- **FR-7** Advertise `MULTI_NODE_MULTI_WRITER` access mode for RWX volumes while keeping RWO behavior unchanged.
- **FR-8** Handle planned and unplanned MDS migration with automatic client reconnect.

### 5.2 Non-Functional

- **NFR-1** Direct block data path throughput within ~10–15% of the same lvol accessed as an RWO volume, for large sequential I/O. Aggregate throughput above one lvol is the striped design's target, not this one's.
- **NFR-2** MDS migration client-visible freeze ≤ configurable bound (target: ≤ 30 s, driven by NFS lease + NVMe `ctrl-loss-tmo` + reconnect-delay, mirroring existing initiator tunables).
- **NFR-3** No data corruption under client death, layout recall, node partition, or MDS migration (PR fencing + XFS journaling must guarantee this).
- **NFR-4** RWX provisioning must not regress RWO provisioning latency or reliability.
- **NFR-5** All new host-side actions run through the CSI node plugin / storage-node agent (no direct SSH). Operations must be idempotent and safely retryable.

### 5.3 Compatibility / Environment

- Client and MDS kernels **≥ 6.11**.
- `nfs-utils` present on MDS hosts (`nfsd`, `exportfs`) and clients (`nfs-blkmap`/`blkmapd`).
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
2. **The csi-link mutation gate opened** (P0-3), so the operator may ask a node to assemble an export rather than only read fabric state from it. Consistency-group snapshots are *not* a precondition of this design, because a single-volume export snapshots per lvol (§6.3).
3. **(Recommended) Eligible-node inventory (P0-6).** An endpoint returning storage nodes with role, data-network IP, kernel version, and health, so MDS selection does not need a per-node probe.
4. **(Recommended) Eligible-server / node-inventory query.** An endpoint returning storage nodes with role, data-network IP, kernel version, and health so the controller can select an MDS without scraping `master-lvols`. If it is unavailable initially, the controller bootstraps from CRD status plus a per-node capability probe (§7.2).
5. **(Recommended) Export/server association store**, or agreement that the CSI-owned Export Registry (§7.1) is authoritative.

### 6.2 Persistent reservations (`-pr`) on lvol create

The lvol create path, with **no** PR support today:

| Layer      | File                                                                 | Change                                                         |
|------------|----------------------------------------------------------------------|----------------------------------------------------------------|
| v2 API     | `simplyblock_web/api/v2/volume.py` → `add()` + `_CreateParams` model | Add `enable_persistent_reservation: bool = False`.             |
| Controller | `simplyblock_core/controllers/lvol_controller.py` → `add_lvol_ha()`  | Thread the flag through.                                       |
| SPDK RPC   | `simplyblock_core/rpc_client.py` → `create_lvol()`                   | Pass `persist_reservation` into the `bdev_lvol_create` params. |
| Model      | `simplyblock_core/models/lvol_model.py`                              | Persist `persistent_reservation: bool`.                        |
| DTO        | `simplyblock_web/api/v2/dtos.py` → `VolumeDTO`                       | Surface `persistent_reservation` for CSI visibility.           |

Existing accepted create params to reuse: `size`, `pool`, `crypto` (encryption), QoS (`max_rw_iops`/…), `ha_type`, `host_id`, `priority_class`, `fabric`, `max_namespace_per_subsys`, `ndcs`/`npcs`, **`allowed_hosts`** (already present, used for §15 host allow-listing), `uid`, `pvc_name`. Note that the API spells encryption `encrypt` and the priority class `priority_class`.

> Note: the port-level `PortReservation` in `models/cluster.py` is a transactional FDB lock for NVMe-oF port allocation, **unrelated** to SCSI/NVMe persistent reservations.

### 6.3 Snapshots of a single-volume RWX export

A single-volume RWX export is one lvol, so a snapshot of it is the per-lvol
snapshot the control plane already takes: `controllers/snapshot_controller.py`
`add()` calling `rpc_client.lvol_create_snapshot()`. Nothing new is needed from
the backend for §12.2 to work.

The one thing the driver must do is quiesce the filesystem before it asks. A
snapshot of a mounted XFS that is still taking writes is crash-consistent at the
block layer only, so recovery on restore depends on the XFS log. The MDS host
holds the only mount, which is what makes this tractable: csi-node on the MDS can
`xfs_freeze -f` the export, take the snapshot, and `xfs_freeze -u`, and no client
holds a competing mount. Freeze duration lands in the client-visible latency
budget and is bounded by the same control channel as every other export
operation (§6.4).

Snapshotting a **striped** RWX volume is a different problem: it needs all `n`
member snapshots to be crash-consistent with one another, which needs a backend
consistency group that does not exist. That, and the user-facing
`VolumeGroupSnapshot` built on the same primitive, is
[`design-pnfs-striped.md`](design-pnfs-striped.md).

### 6.4 NFS server (co-located) and where the export logic lives

Three **separate** concerns live on the MDS host, and conflating them is the usual mistake (full split in §8). Only **(b)** and the capability reporting below are genuinely sbcli / host-packaging changes. **(a)** and **(c)** are CSI-driver (**csi-node**) responsibilities in *this* repo:

**(a) NVMe-oF connection of the backing namespace: csi-node.** The MDS host is an NVMe-oF initiator for the volume's namespace just like a client, so the existing **csi-node** service (`csi-driver/pkg/spdk/nodeserver.go` and `csi-driver/pkg/util/initiator.go`) connects it and owns reconnect, ANA, and the Guardian. This requires the CSI node DaemonSet to run on MDS/storage hosts (§14.1).

**(b) The NFS server: a co-located system service.** The kernel `nfsd` threads plus the userspace daemons `rpc.mountd` and `rpc.statd`. Kernel `nfsd` and `/etc/exports` are host-global and must run where the XFS is actually mounted, which is also why one host serves every export bound to it. This is a **long-running daemon set started at node bring-up**, either a systemd unit on the host or a container in the storage-node DaemonSet with `hostNetwork` and access to `/proc/fs/nfsd`. Ship `nfs-utils` in the storage-node image (sbcli packaging). It is **not** an HTTP surface and nothing "serves" it on request. §8.1 and §14.1 cover provisioning. (`blkmapd`/`nfs-blkmap` is a **client-side** daemon (§10) and does *not* run on the MDS.)

**(c) Export assembly and control: also csi-node.** Building an export on top of the connected device (`mkfs.xfs` → mount `/mnt/{pvc}` → write `/etc/exports.d/{pvc}` → `exportfs -ra`) is a natural extension of what csi-node already does for RWO (`SafeFormatAndMount`, mount lifecycle, device-wait). It lives in the **csi-node** service (§8), which runs `exportfs` against the co-located `nfsd`. This is **not** the SNodeAPI, which is SPDK/device-management focused and should not grow NFS responsibilities. (A dedicated NFS sidecar remains an option only for hosting the (b) daemon, never the export logic.)

**The control channel is csi-link.** Because no consuming pod runs on the MDS host,
kubelet never issues `NodeStageVolume` for the export, so something else has to ask
the MDS-host csi-node to assemble it. Earlier drafts left this open between an admin
gRPC endpoint, a node-watched CR, and a synthetic stage call. It is settled: the
operator drives export assembly over **csi-link**, the operator-to-CSI channel that
lands in 26.4 (`atlas-lib/link` and `atlas-lib/node`, with `operator/internal/csilink`
and `csi-driver/pkg/csilink` as the two ends). Nothing new gets invented for pNFS.

Three properties of that channel shape this design rather than merely enabling it:

- **The node dials the operator, not the reverse.** csi-node opens a TLS session
  carrying its ServiceAccount token, the operator authenticates it by TokenReview
  and derives the node identity from the token's pod claims, and both ends then
  speak gRPC over a yamux mux. The operator has no ingress to nodes and does not
  want any.
- **"No session for this node" is a normal state, not an error.** A node is
  legitimately disconnected during a rollout. An export operation aimed at a node
  with no live session is a requeue, and §16 carries it as a failure mode rather
  than an outage.
- **The channel is read-only today.** Mutations stay gated until csi-node moves off
  nvme-cli `initiator.go` onto atlas `nvmeof`, because two connect implementations
  on one node is the thing that gate exists to prevent. `CreateExport` and
  `DeleteExport` are mutations, so opening that gate is a prerequisite of this
  design and not a detail (P0-3, P0-4).

Only the leader-elected operator replica accepts sessions, so export operations are
driven from the same replica that owns every other reconcile.

**Capability reporting (the one sbcli/SNodeAPI piece).** Extend the operator's existing `/snode/info` readiness poll (§14) so MDS eligibility (§7.2) can be evaluated: `nfs_capable`, `nfsd_version`, `kernel_version`, and `pnfs_available`. These four names are the wire schema, and P0-6, the endpoint table, and §6.6 all use them. Earlier drafts also spelled two of them `nfs_version` and `pnfs_enabled`, which is the same data under a second name and is not carried forward. This stays in `simplyblock_web` because it reports storage-node host state the operator already scrapes there.

### 6.5 v2 API additions (CSI-facing)

Route handlers live under `simplyblock_web/api/v2/` (`volume.py`, `snapshot.py`, `cluster.py`, `storage_node.py`), request/response models in `api/v2/dtos.py`. The v2 tree already exposes `.../volumes/{id}/connect`, `.../hosts/`, `.../hosts/{nqn}/secret`, `.../migration/`, and `/tasks/`.

The endpoints this design calls, and their state today:

| Method | Endpoint                                                        | Notes                                                                                                                                           |
|--------|-----------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------|
| `POST` | `/api/v2/clusters/{id}/storage-pools/{id}/volumes/`             | Needs `enable_persistent_reservation` in the body (§6.2). Idempotent by volume name, as today. **Not shipped** (P0-1)                           |
| `POST` | `/api/v2/clusters/{id}/storage-pools/{id}/snapshots/`           | Existing per-lvol snapshot. Used unchanged (§6.3)                                                                                               |
| `GET`  | `/api/v2/clusters/{id}/storage-nodes/`                          | Must carry `nfs_capable`, `nfsd_version`, `pnfs_available`, `kernel_version`, and the NFS data-network IP (§6.6). **Fields not shipped** (P0-6) |
| `POST` | `/api/v2/clusters/{id}/storage-pools/{id}/volumes/{id}/connect` | Existing. Returns the connection set for the backing namespace                                                                                  |

Every mutating call above is retried by a reconciler after an ambiguous timeout,
so each one has to be idempotent by the name or id in the request: the operator
cannot distinguish a call that never arrived from a response that was lost.

### 6.6 Storage-node model, capability & inventory

`simplyblock_core/models/storage_node.py` (`StorageNode`) is inventory-only today (`hostname`, `status`, `primary_ip`/`mgmt_ip`, `nvme_devices[]`, HA node ids, `nvmf_port`, …). Registration goes through `storage_node_ops.py` `add_storage_node()`, and listing through `GET /api/v2/clusters/{id}/storage-nodes`. Add capability fields so the controller can pick an MDS without scraping `master-lvols`:

- `nfs_capable`, `nfsd_version`, `pnfs_available`, `kernel_version`, and a data-network IP for NFS, which may differ from `mgmt_ip`.
- Surface these on `StorageNodeDTO` and optionally a `GET /storage-nodes/{id}/nfs-capability` endpoint → **this is the "eligible-node inventory query"** referenced in §6.1(4) and §7.2.

---

## 7. Export Registry and MDS Selection

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
// The bound MDS host is NOT here. It is an observed binding the operator owns, so it
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

    // StorageNodeRef names the StorageNode CR currently acting as MDS. Operator-owned:
    // it is the serialization point for the one-MDS-per-export invariant (§13.2).
    // +optional
    StorageNodeRef string `json:"storageNodeRef,omitempty"`

    // ServiceName is the Service whose ClusterIP clients mount (§13.3). Stable for
    // the export's lifetime, which is the point of it.
    // +optional
    ServiceName string `json:"serviceName,omitempty"`

    // MDSNodeIP is the node address currently behind that Service, recorded for
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

    // AllowedClients is the effective client set written into /etc/exports, derived
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
// +kubebuilder:printcolumn:name="MDS",type=string,JSONPath=".status.storageNodeRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SubPhase",type=string,JSONPath=".status.subPhase"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=".status.serviceName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// NFSExport is one pNFS export: a volume, the MDS host serving it, and the address
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

**A finalizer is mandatory.** The CR is the only record of an external mount, an
`/etc/exports` entry, a Service, and an attached namespace. Without
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
  storageNodeRef: sn-worker-3-0
  serviceName: nfsexp-7b41c0e2a9-mds
  mdsNodeIP: 10.42.3.17
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

Requirements that fall out:Requirements that fall out:

- **Durable and idempotent.** `CreateVolume` is retried by the external-provisioner, so the CR is created-or-fetched by a name derived from the volume handle, and a retry leaks neither an lvol nor an export.
- **Single-writer on `status.storageNodeRef`.** This is the "one MDS per export" invariant, and it is the invariant that keeps two hosts from mounting one XFS (§13). Failover is a phase transition that bumps `status.generation`.
- **`NodeStageVolume` reads it** for the Service name, the backing lvol, and its NGUID, instead of re-deriving them.

### 7.2 MDS Eligibility and Selection

- **Eligibility gate** (evaluated by the operator, cached in `StorageNode.status`):
  - Kernel ≥ 6.11 (probe via SNodeAPI `uname -r`).
  - `nfs-utils` installed and `nfsd` loadable.
  - Node marked as an eligible export host (label/taint, §14).
  - `StorageNode.status.status` is `online`. Note the field is `status`, not `state`.
- **Selection policy** for a new volume: round-robin across eligible MDS hosts (PoC default), with capacity/affinity as future refinement. Optionally, honor the existing zone/region cluster mapping so the MDS lands in the pod's topology.
- **Volume placement:** the backing lvol and the MDS host are chosen independently. Co-locating them is not required, because the MDS reaches the namespace over NVMe-oF exactly as a client does, and forcing co-location would constrain failover to the node holding the data.

### 7.3 Kernel / Capability Enforcement

At Helm install and continuously via the operator:
- For each worker node that may host RWX pods: verify kernel ≥ 6.11. A node that does not qualify is marked **not compatible** (label + event) so scheduling can avoid it.
- For each MDS-eligible node: verify kernel ≥ 6.11, `nfs-utils`, and start the NFS-server daemons (`nfsd`/`rpc.mountd`/`rpc.statd`) at launch (§8.1). (`blkmapd` is client-side only.)

---

## 8. Server-Side Design (MDS host)

Two concerns on the MDS host are owned by the **csi-node** service (this repo), one by a co-located daemon (§6.4):

- **NVMe-oF connection of the member namespaces** → **csi-node** (`pkg/spdk/nodeserver.go` + `pkg/util/initiator.go`). The MDS host is an NVMe-oF *initiator* for the members exactly like a client, so it reuses the existing, battle-tested connect/`MonitorConnection`/ANA-reconnect/Guardian machinery rather than re-implementing `nvme connect`. Requires the CSI node DaemonSet to run on MDS/storage hosts (§14.1).
- **Export assembly and control** (XFS, mount, and `exportfs`) → **csi-node**, extending its existing `SafeFormatAndMount` and mount lifecycle. It runs `exportfs` against the co-located `nfsd`.
- **The NFS server:** a co-located long-running daemon set (kernel `nfsd` + `rpc.mountd` + `rpc.statd`), a systemd unit or sidecar (§6.4(b)). `blkmapd` is **not** here, because it is client-side only (§10).

The `CreateExport` and `DeleteExport` operations below are csi-node routines. Each one is **idempotent**, safely re-runnable, and takes the `ExportRecord` (or its fields). The operator invokes them on the MDS-host csi-node over csi-link (§6.4). It is the sole caller, so export ownership does not straddle two components.

### 8.1 Host prerequisites (the co-located NFS server, once per MDS host)

- Ensure package `nfs-utils` is present (baked into the storage-node image or installed on the host).
- Run the NFS-server daemons as a **system service** (systemd or sidecar): kernel `nfsd` (`rpc.nfsd`), `rpc.mountd`, `rpc.statd`. The MDS does **not** run `blkmapd` (client-side only, §10).
- Ensure the pNFS/SCSI-layout sysctls/modules are loaded and `/proc/fs/nfsd` is mounted.
- The MDS-host **csi-node** already has `/dev`/`/sys` access. It additionally needs `/mnt` and, **new**, `/etc/exports` (or an `/etc/exports.d/` drop-in dir) mounted so it can manage exports (§14.1).

### 8.2 `pnfs.CreateExport(record)` — assemble and export

The export path cannot be `/mnt/{pvc-name}`: PVC names are unique only within a
namespace, so two same-named PVCs in different namespaces would collide on one MDS
host, both in the mount point and in `/etc/exports`. The path carries namespace and
UID information, and the same identifier names the `fsid` allocation below.

Idempotent steps, each skipped when already satisfied:

1. **Attach the namespace:** csi-node connects the backing namespace and owns its reconnect lifecycle (§8 intro, `initiator.Connect` and `MonitorConnection`). CreateExport waits for the device to appear, then proceeds.
2. **Filesystem:** `mkfs.xfs` on the namespace, only if it is not already formatted, detected through `blkid`. XFS is mandatory for the pNFS SCSI layout, so a request for any other `fsType` is rejected at admission rather than here (FM-7).
3. **Mount:** create `/mnt/{pvc-name}` and mount the device there.
4. **Export:** write the `/etc/exports.d/{pvc}.exports` entry:
   ```
   /mnt/{pvc-name} {allowed-client-set}(rw,sync,no_subtree_check,no_root_squash,pnfs,fsid={fsid})
   ```
   The PoC used `*` for the client set, and §15 tightens this.
5. **Publish:** `exportfs -ra`.

There is no volume manager in this path. A single namespace is formatted directly,
which removes `pvcreate`, `vgcreate`, `lvcreate`, and the deterministic VG and LV
naming that a stripe would need to reproduce on another host. Striping puts all of
that back, which is one of the reasons it is a separate design.

`fsid` comes from the CR and never changes, so a re-run on another host reproduces
the same file handles (§13.2). That forces the allocation to be **cluster-wide
unique, not merely per-host**: an export must be able to move to any eligible host
without colliding with an export already there, and a per-host allocator cannot
promise both stability and non-collision at once. Allocation is therefore the
operator's, from a cluster-scoped range recorded on the CR (§18, Q1). Because the device is formatted rather than
assembled, re-materializing an export elsewhere is a mount, not a rebuild.

### 8.3 `pnfs.DeleteExport(record)` — teardown (reverse order)

1. `exportfs -u` and remove the drop-in file, then `exportfs -ra`.
2. `umount /mnt/{pvc-name}`, then remove the directory.
4. Release the member namespaces via csi-node's existing `initiator.Disconnect` path.
5. Signal the controller to delete the lvols (control-plane owns lvol deletion).

### 8.4 The NFS root (`fsid=0`) export

NFSv4 requires a pseudo-root. Establish **once per MDS host** a root export:
```
mount -t nfs -o v4.1 {mds-ip}:/ /...    # requires an fsid=0 root on the server
```
The MDS host exports a root with `fsid=0`, and each PVC export gets a **stable unique `fsid`** (stored in the `ExportRecord`) so remounts and migrations keep the same file handles. `fsid` allocation must be collision-free per MDS host (Open Question §18).

### 8.5 Migration support

csi-node's `CreateExport` and `DeleteExport` routines are the primitives the failover flow uses (§13). Because the mount point and the export are derived deterministically from the volume identifier and the `fsid`, re-materializing the export on another host reproduces the same NFS file handles, so clients recover against handles they already hold rather than remounting from scratch. There is no volume manager in this path, so re-materializing is a mount rather than a rebuild.

---

## 9. CSI Controller Design

Changes in `pkg/spdk/controllerserver.go`, `pkg/util/nvmf.go`, `pkg/util/jsonrpc.go`.

### 9.1 Access-mode / capability changes

- Advertise `MULTI_NODE_MULTI_WRITER` in the driver's access modes (`pkg/csi-common/driver.go` via `AddVolumeCapabilityAccessModes`, currently only `SINGLE_NODE_WRITER` in `sanity_test.go`).
- In `CreateVolume`, branch on the requested access mode: `MULTI_NODE_MULTI_WRITER` alone, or the StorageClass flag, selects the **pNFS path**. The other `MULTI_NODE_*` modes are not specified by this design and are rejected rather than routed, so a read-only or single-writer multi-node request does not silently get RWX behavior.
- No group capability is advertised. `GROUP_CONTROLLER_SERVICE` and `CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT` belong to the striped design (§9.5), and claiming them here would have the driver advertise a service it does not implement.

### 9.2 New StorageClass parameters

| Parameter                                                                                                   | Meaning                                                 | Default |
|-------------------------------------------------------------------------------------------------------------|---------------------------------------------------------|---------|
| `pnfs` / `access_protocol: nfs`                                                                             | Opt into the pNFS path                                  | off     |
| `mds_selector`                                                                                              | Optional label/affinity to constrain MDS host selection | none    |
| `nfs_mount_options`                                                                                         | Extra NFS mount options appended to `v4.1`              | none    |
| (reuse) `pool_name`, `cluster_id`/`zone_cluster_map`/`region_cluster_map`, QoS, `compression`, `encryption` | as today | — |

Parsed alongside the existing keys in `prepareCreateVolumeReq` (`controllerserver.go`).

### 9.3 `CreateVolume` (pNFS path)

The order matters, because the CR's required fields are immutable and cannot be
filled in later. Everything `spec` needs is therefore known before the CR is
created, and everything the operator decides lands in `status` afterward.

1. Resolve cluster selection and pool (existing `resolveClusterSelection`, `NewsimplyBlockClient`).
2. **Derive the identity:** the export UUID, the volume handle (§11), the object name, the export path, and the `fsid` from the cluster-wide allocator (§8.2). These are all `spec` fields and all immutable, so they are computed before anything is created.
3. **Create the lvol** with persistent reservations at size `S`, GiB-aligned. `CreateLVolData` carries the flag under the **same name the backend uses**, `enable_persistent_reservation` (§6.2), rather than a shorter CSI-side spelling for a value that crosses the boundary.
4. **Create the `NFSExport` CR** idempotently by that object name, with the lvol id and NGUID written to `status`. It enters `status.phase = Pending` with no MDS bound.
5. **Hand off.** The operator's `NFSExportReconciler` selects the MDS host, records it in `status.storageNodeRef`, drives `CreateExport` over csi-link, reconciles the Service, and moves the CR to `Ready`. The CSI controller does not select the host and does not call the node, which is what keeps provisioning out of the failover path.
6. **Wait for `Ready`**, then build the CSI `Volume`:
   - `VolumeId` is the pNFS handle (§11).
   - `VolumeContext` carries `access_protocol=nfs`, `export_service`, `export_path`, `fsid`, and the backing `lvolID:nguid` pair with its NVMe connect hints.
   - `AccessibleTopology` where topology mapping applies.
7. Return. Every step is idempotent, because the external-provisioner retries.

Step 6 blocks on a reconciler, so `CreateVolume` returns `Aborted` while the export
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

- If MDS selection or `CreateExport` fails after lvols exist, the record stays in `Provisioning` and a retry resumes. A background reconciler (operator) garbage-collects orphaned lvols/exports for records stuck in `Provisioning`/`Deleting` past a timeout.
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

Changes in `pkg/spdk/nodeserver.go` and initiator reuse in `pkg/util/initiator.go`.

### 10.1 `NodeStageVolume` (pNFS path)

Detect the pNFS path from `VolumeContext` (`access_protocol=nfs`). Then:

1. **Attach the namespace:** `initiator.Connect()`, which is the existing RWO connect path unchanged.
2. **Device identity:** resolve the namespace's NGUID and publish the `eui64` alias
   `blkmapd` looks for. This is a sysfs read, not a subprocess: `atlas-lib`'s
   `nvme` package already surfaces it as `Namespace.NGUID` (`sysfs_scan.go`), and
   its scan covers every NVMe subsystem on the host rather than only simplyblock
   ones, so nothing about it is volume-vendor specific.

   Two small additions to `atlas-lib` belong with it rather than here (P0-5).
   `nvme.DeviceSelector` today matches on NQN, NSID, UUID, and device path, so it
   needs an `NGUID` field and a by-NGUID lookup for the reverse direction. And the
   `/dev/disk/by-id/nvme-eui64.${NGUID}` symlink should be created by a helper in
   `atlas-lib`, which today only resolves such aliases (`nvmeof/wait.go`), instead
   of by shelling out from csi-node. Until those land, the shell equivalent is:
   ```
   NGUID=$(nvme id-ns /dev/nvmeXnY | awk '/^nguid/{print $3}')
   ln -sf /dev/nvmeXnY /dev/disk/by-id/nvme-eui64.${NGUID}
   ```
   The symlink is what lets `blkmapd` match the device signature the MDS advertises
   in the layout. **RHEL-family validated. Debian and Ubuntu device naming must be
   tested** (P0-11).
3. **Ensure `blkmapd`** (`nfs-blkmap`) is running on the client (started by the node plugin's init/host prerequisite, §14).
4. **NFS mount** at the staging path:
   ```
   mount -t nfs -o v4.1[,<extra opts>] {export_service}:/mnt/{export-path} <stagingPath>
   ```
   The server-side `fsid=0` root export makes the pseudo-root resolvable.
5. **Stash the volume context:** the member list, `mds_ip`, and `fsid` go through `StashVolumeContext` for use at unstage and heal.

The kernel automatically issues `LAYOUTGET` and performs direct block I/O over the attached namespaces. If the block path is unavailable it falls back to MDS-routed I/O.

### 10.2 `NodePublishVolume`

Bind-mount the staging NFS mount into the pod target path (existing bind-mount logic). RWX means the same staging mount may be published to multiple pods on the node, so publish and unpublish must be **reference-counted** so unpublishing one pod does not unmount a still-in-use export.

### 10.3 `NodeUnstageVolume` / `NodeUnpublishVolume`

- Unpublish: unmount the pod bind mount, then decrement the refcount.
- Unstage (only when refcount hits zero): `umount` the NFS mount, then `initiator.Disconnect()` each member namespace, then clean up the `eui64` symlinks and stashed context.

### 10.4 Reconnect / heal for pNFS

- The existing **`MonitorConnection`** / Guardian machinery watches the underlying NVMe-oF namespaces. On total path loss it can trigger reconnect just like RWO.
- **Additional pNFS concern:** on **MDS migration** the NFS mount's server IP effectively moves. Because the export is re-created with the same `fsid`/file handles on the new MDS host at (ideally) the same or a re-pointed IP, the NFS client reconnects after its lease/`ctrl-loss-tmo` window. If the IP changes, the node plugin must detect the migration (via `ExportRecord.generation`) and remount/redirect (§13). This mirrors the existing "primary IP changes + monitor reconnects" behavior for RWO.
- Detect dead NFS mounts (`ESTALE`/`ENOTCONN`) using the same `stagingMountDead` approach adapted for NFS, and restage.

### 10.5 Node capabilities

Keep `STAGE_UNSTAGE_VOLUME`. `NodeGetVolumeStats` uses `statfs` on the NFS mount (works unchanged for filesystem mode). `NodeExpandVolume` for pNFS is a **no-op on the client**, because the XFS grow happens on the MDS host (§12.3).

---

## 11. Volume Handle and Data Model

The current handle is `{clusterID}:{poolID}:{lvolID}` and `volumehandle.Parse` enforces exactly three parts with a UUID cluster and lvol (`csi-driver/pkg/kubernetes/volumehandle/index.go`). A pNFS volume adds an MDS binding and an export, so the handle needs a form of its own.

**The handle is `nfs:{clusterID}:{poolID}:{exportUUID}`,** a synthetic four-part form that `volumehandle.Parse` learns alongside the existing three-part one. `exportUUID` keys the `NFSExport` CR, and everything else is read from there. The handle stays synthetic even though this design has exactly one backing lvol, because reusing the lvol id would make the handle change identity the moment striping arrives.

**The handle is not the object name.** Colons are not valid in a Kubernetes object name, so the `NFSExport` CR is named by a deterministic DNS-safe encoding of the handle rather than the handle itself: a stable hash of `{clusterID}/{poolID}/{exportUUID}` truncated to the label limit, with the full handle carried in `status` for lookup. Collision handling is the usual one for a truncated hash, which is to detect a mismatch on read and fail rather than adopt someone else's export.

`parseVolumeID` (`controllerserver.go`) and `volumehandle.Parse` must learn the pNFS form while **remaining backward compatible** with existing three-part RWO handles, where an unknown or invalid handle is treated as belonging to another driver, as today.

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
`DeleteVolume`: set `status.phase = Deleting` → `pnfs.DeleteExport` (unexport and unmount, after which csi-node releases the namespace) → delete the lvol through the control plane → delete the CR. Idempotent, and tolerant of partial prior progress (`ErrVolumeNotFound` treated as success, as today).

### 12.2 Snapshot
`CreateSnapshot` on an RWX volume is the ordinary per-lvol snapshot, with one
addition: csi-node on the MDS host freezes the filesystem with `xfs_freeze -f`
before the control-plane call and thaws it after, so the snapshot is taken against
a quiesced log rather than mid-write (§6.3). The MDS holds the only mount, which is
what makes the freeze safe to take. The CSI snapshot id keeps the existing
`{clusterID}:{poolID}:{snapshotUUID}` form.

### 12.3 Resize
`ControllerExpandVolume`: resize the lvol (GiB-aligned), then run **`xfs_growfs` on the MDS host** over csi-link, because an XFS grow needs the mounted path and the MDS holds it. `NodeExpandVolume` is a client-side no-op (§10.5), which differs from RWO where the client grows the filesystem.

### 12.4 Clone / Restore
- **Clone from a volume or a snapshot, and restore:** the ordinary per-lvol clone the driver already performs, followed by the full §8.2 `CreateExport` flow on a newly selected MDS host. The clone is an independent RWX volume with its own `NFSExport` CR, its own Service, and its own `fsid`. No consistency group is involved, because there is one lvol to clone.

---

## 13. MDS Fault Tolerance, Migration, and Failover

### 13.1 What tolerance is possible

A single XFS filesystem can be mounted by exactly one node, so there is no
active/active MDS and there never will be under this architecture. The only shape
available is active/passive: one host owns the mount, and on loss another host
re-materializes the same export.

That is less bad than it sounds, because of where the data path runs. Clients read
and write **directly over NVMe-oF**, so losing the MDS stops metadata operations
and leaves bulk I/O on already-held layouts running until the layout is recalled or
expires. The failure mode is a stall, not data loss, and not a full outage for
in-flight work. What the design owes is a bounded stall and a guarantee that
recovery cannot corrupt.

### 13.2 The invariant that matters

**Two MDS hosts must never have the export's XFS mounted at the same time.** This
is the one place in the design where getting it wrong destroys data rather than
degrading service, and it is the reason persistent reservations are a hard
prerequisite rather than a hardening step.

Planned migration can rely on the old host cooperating: it unexports, unmounts, and
releases the namespace before anything else touches it. Unplanned failover cannot,
because the whole premise is that the old host stopped answering. An unreachable
host is not a stopped host: it may be partitioned, paused, or about to resume with
a stale mount and dirty page cache. So failover **fences before it assembles**,
using the persistent reservation on the namespace to revoke the old host's write
access, and only then mounts on the new one. Without P0-1 and P0-2 that step does
not exist, which is why this design cannot ship past a single-writer MVP without
them.

`status.storageNodeRef` on the `NFSExport` CR is the serialization point. One writer,
optimistic concurrency, and no second host is even a candidate until that field is
rewritten. It is status rather than spec precisely so a user edit cannot make a
second host a candidate.

### 13.3 A stable mount address

**Clients mount a Kubernetes Service, not a node.** The operator gives each export a
Service and manages its EndpointSlice directly, with exactly one endpoint: the
current MDS node's address, port 2049. On failover the operator rewrites that
endpoint. The ClusterIP does not change, so the client's mount address does not
change, and the "the IP moved, so remount the staging mount" branch that earlier
drafts carried disappears.

This is not a new mechanism here. The operator already fronts node-local services
this exact way: `BuildStorageNodeSetEndpointSlice` and `BuildSpdkProxyEndpointSlice`
in `operator/internal/utils/storage_nodeset_ds.go` build EndpointSlices over node
internal IPs, and `reconcileEndpointSlice` re-points them when a node moves.

`nfsd` itself is unaware of any of this, which is worth stating plainly because it
is the first question the design invites. A ClusterIP is not an address anything
binds. It is a DNAT rule that kube-proxy programs on every node. `nfsd` binds
`0.0.0.0:2049` in the host network namespace exactly as it always does; the client
connects to the ClusterIP, the rule rewrites the destination to the MDS node's real
address, and `nfsd` sees an ordinary connection. Host-originated traffic reaches
those rules because `KUBE-SERVICES` is hooked into `OUTPUT` as well as
`PREROUTING`, and the mount here is issued by the client node's host kernel. One
ClusterIP per export, all targeting port 2049, composes correctly: many exports
share the one host `nfsd`, and a failover rewrites only the moved export's
endpoint.

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
- **A stable address is not a stable server identity.** An NFSv4.1 client identifies
  a server by the `server_owner` and `server_scope` it gets from `EXCHANGE_ID`, not
  by IP. Re-materializing the export on a different host yields different values, so
  the client concludes it reached a different server and performs full state
  recovery, re-establishing its clientid and reclaiming opens and locks, rather than
  the plain reconnect a floating IP would give. Recovery is possible because the
  file handles survive: the same `fsid` over the same on-disk XFS produces the same
  handles. Whether it is transparent to an application, and whether the two values
  can be made to match across hosts at all, is P0-7.

So the honest promise is narrower than a transparent reconnect. The mount
address is stable, so no remount and no pod disruption. The client still performs
NFSv4.1 state recovery, and the cost of that recovery is what NFR-2 has to bound.

### 13.4 Planned migration

**Trigger:** an MDS host drained for an upgrade, or a `StorageNodeOps` CR with
action `shutdown`, `restart`, `remove`, or `migrate` targeting the host.

1. The operator sets `status.phase = Failing Over` on the `NFSExport` and stops
   admitting new exports to the old host.
2. On the **old** host, over csi-link, csi-node quiesces: `exportfs -u`, `umount`,
   then release the namespace.
3. The operator selects a new eligible host (§7.2) and rewrites
   `status.storageNodeRef`.
4. On the **new** host, csi-node connects the namespace and runs `CreateExport`
   with the **same** `fsid` and the same export path, reproducing the file handles.
5. The operator rewrites the Service's EndpointSlice to the new node address.
6. `status.phase = Ready`, and `status.generation` is bumped.
7. Clients recover as described in §13.3. Node plugins watching the CR see the
   generation bump, which is what tells them the export moved even though its
   address did not.

### 13.5 Unplanned failover

Same flow with step 2 replaced, because the old host is not answering:

1. The operator observes the loss. `StorageNode.status` going non-`online` is the
   coarse signal; an MDS health probe (`nfsd` responding, the mount present, the
   export present) is the precise one and is worth having because a host can be
   `online` with a dead `nfsd`.
2. **Fence.** Revoke the old host's write access to the namespace through the
   persistent reservation, and only proceed once that is confirmed. A failover that
   cannot confirm the fence does not proceed: the export stays `Degraded` and the
   operator emits an event, because a stalled export is recoverable and a
   double-mounted one is not.
3. Continue at step 3 of §13.4.

The freeze bound in NFR-2 is therefore fence confirmation, plus export assembly on
the new host, plus EndpointSlice propagation and conntrack cleanup, plus client
state recovery. Each term needs a measured number before NFR-2 is a commitment
rather than an aspiration; the test plan carries them as `F-` scenarios.

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

- **Client gating** (`helm-charts/charts/simplyblock-operator/templates/node.yaml`):
  a `pnfs.enabled` value; `blkmapd` available and `nfs-blkmap` started on nodes that
  may host RWX pods, either as an init step in the node DaemonSet or a small
  privileged prerequisite DaemonSet. The operator labels nodes with kernel ≥ 6.11 as
  RWX-capable and marks the rest, so scheduling avoids them.
- **Server gating** (`templates/storage-node.yaml`): `nfs-utils` present on MDS
  hosts, and the NFS-server daemons started at bring-up (§8.1). `blkmapd` is **not**
  started on the server.
- **csi-node on MDS hosts.** The node DaemonSet already takes `nodeSelector` and
  `tolerations` from values, so running it on storage hosts is configuration rather
  than new machinery. It needs `/mnt` and `/etc/exports.d/` host-mounted to manage
  exports (§6.4(c)).
- **`NFSExport` CRD** shipped in `helm-charts/charts/simplyblock-operator/crds/`
  as `storage.simplyblock.io_nfsexports.yaml`, generated by `make manifests`.
- **RBAC** for the operator over `nfsexports`, plus `services` and
  `endpointslices` write access for §13.3 if the manager does not already hold it.
- **csi-link enabled.** This design requires the link that is optional today, so
  `csiLink.*` moves from opt-in to required when `pnfs.enabled` is set.
- **StorageClass example** (`templates/storageclass.yaml`) with `pnfs: "true"`.

The `snapshot-controller`, `csi-snapshotter`, and `csi-provisioner` version floors
that earlier drafts raised here belong to the striped design, because only the
group-snapshot feature needs them. For the record, the chart already ships
`csi-snapshotter` and `snapshot-controller` at v8.2.0 and `csi-provisioner` at
v5.1.0, so those floors are already met.

### 14.2 Operator changes

The operator owns cluster, node, and pool lifecycle plus drain and migration. pNFS
layers onto that without disturbing RWO. What it already has and this design uses:
node labeling (`io.simplyblock.node-type`, `io.simplyblock.storagenodeset`), a
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
  re-pointed by the operator only during a `migrate`. That is exactly the granularity
  MDS eligibility and role need, so the NFS fields go there.

**CRD and API-model additions:**

| CRD / type        | File                                    | Additions                                                                                                                                                                                                                                                                                                     |
|-------------------|-----------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `NFSExport`       | `api/v1alpha1/nfsexport_types.go` (new) | The export registry itself (§7.1). One CR per RWX volume, owned by the operator, watched by csi-node.                                                                                                                                                                                                         |
| `StorageNode`     | `api/v1alpha1/storagenode_types.go`     | spec: `nfsServerEnabled` only, since that is the one thing a user chooses. status: `kernelVersion`, `nfsCapable`, `nfsdVersion`, `pnfsAvailable`, `mdsEligible`, `exportedVolumes[]`. Eligibility is computed by the operator (§7.2), so a user-settable spec field would let an ineligible node be selected. |
| `StorageNodeSet`  | `api/v1alpha1/storagenodeset_types.go`  | spec: `nfsServerEnabled` and `kernelVersionMin` as fleet defaults that a `StorageNode` may override, following the existing `nodeConfigs` precedence.                                                                                                                                                         |
| `StorageCluster`  | `api/v1alpha1/storagecluster_types.go`  | spec: `nfsEnabled`, `nfsExportPolicy`, `nfsSecurityFlavor`. Reuse existing `snodeApiPort` and `clientDataIfname`.                                                                                                                                                                                             |
| `StoragePool`     | `api/v1alpha1/storagepool_types.go`     | spec: `supportedAccessModes[]` (RWO, RWX), `nfsExportPolicy`. Renamed from `Pool` in #414.                                                                                                                                                                                                                    |
| `StorageNodeOps`  | `api/v1alpha1/storagenodeops_types.go`  | A `Quiescing` sub-phase in the existing enum, entered before `Suspending` when the target node carries active exports.                                                                                                                                                                                        |
| `VolumeMigration` | `api/v1alpha1/volumemigration_types.go` | status: an `ExportTransitioning` phase between `Running` and `Completed`.                                                                                                                                                                                                                                     |
| `Task`            | `api/v1alpha1/task_types.go`            | No schema change. Existing polling surfaces any new backend task types.                                                                                                                                                                                                                                       |
| API params        | `internal/utils/types.go`               | `ClusterAddParams` gains `nfs_enabled`; `PoolAddParams` gains `nfs_export_policy` and `access_modes`. Both structs keep their current names.                                                                                                                                                                  |

**Reconciler changes:**

- **`NFSExportReconciler`** (new, `internal/controller/nfsexport_controller.go`): owns
  the export lifecycle. Drives `CreateExport` and `DeleteExport` on the bound node
  over csi-link, reconciles the Service and EndpointSlice for §13.3, and runs the
  failover state machine in §13.5. It is the only writer of `status.storageNodeRef`.
- **`StorageNodeReconciler`:** evaluate MDS eligibility from `/snode/info` and record
  it in status, so §7.2 selects without probing.
- **`StorageNodeOpsReconciler`:** add the `Quiescing` sub-phase, which fails over
  every export bound to the target before the node action proceeds.
- **`NodeDrainCoordinatorReconciler`:** quiesce exports before `shutdown_called`,
  tracked in `NodeDrainState`.
- **`VolumeMigrationReconciler`:** the `ExportTransitioning` phase, re-creating the
  export on the target with the same `fsid`.
- **`StorageClusterReconciler`** and **`StoragePoolReconciler`**: pass the new NFS
  fields through.

> Division of labor: the **CSI controller** creates and deletes the `NFSExport` CR as
> part of provisioning (§9). The **operator** owns everything that happens to an
> export afterward, including placement, the Service, drain, and failover. The CR is
> the boundary, which is what keeps the CSI controller out of the failover path.

## 15. Security Design

The PoC exports are open to everyone (`*`). **This is the largest open security gap** and must be closed before GA.

**Threats:** any host that can reach the MDS IP can mount the export. Any host that can reach the NVMe-oF targets can attach the raw namespaces and read/write shared data, bypassing NFS permissions entirely.

**Controls (layered)**:

1. **NVMe-oF host allow-listing (most important).** The direct data path is raw block. Restrict each namespace's `allowed_hosts` to the specific client NQNs + the MDS NQN (the driver already threads `host_nqn` through `getLvolConnections`/`VolumeInfo`). Only nodes whose NQN is allow-listed can attach the namespaces. PR fencing (`-pr`) complements this by revoking access on layout recall.
2. **Export client restriction.** Replace `*` in `/etc/exports` with the concrete set of client node data IPs/subnets bound to the volume, maintained from the ExportRecord as pods schedule/deschedule. Consider `sec=krb5`/`krb5i`/`krb5p` for authenticated/encrypted metadata (Open Question, because Kerberos needs infrastructure).
3. **Network isolation.** Keep NVMe-oF + NFS on the storage data network. NetworkPolicies and firewall rules prevent arbitrary pods or nodes from reaching MDS IPs and NVMe targets.
4. **In-pod user restriction.** `no_root_squash` (PoC) is dangerous for multi-tenant clusters, because a container root can write as root on the shared FS. Evaluate `root_squash`/`all_squash` + `anonuid`/`anongid`, and per-PVC ownership/`fsGroup`. This is an Open Question tied to tenancy model.
5. **Transport security.** Reuse existing TLS for control/SNodeAPI. NVMe/TCP TLS (if available) and Kerberos for NFS are stretch goals.

---

## 16. Failure Modes and Edge Cases

| #     | Scenario                                                                | Expected behavior                                                                                                                                                                                    |
|-------|-------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| FM-1  | Client dies holding a layout                                            | MDS recalls the layout, PR fences the dead client, XFS integrity is preserved, and other clients continue.                                                                                           |
| FM-2  | Block/direct path unavailable (namespace missing, reservation conflict) | NFSv4.1 falls back to MDS-routed I/O. Degraded throughput, no data loss.                                                                                                                             |
| FM-3  | MDS host crash (unplanned)                                              | Migration flow (§13) re-materializes the export on a new host. Clients freeze, then reconnect within the NFR-2 bound.                                                                                |
| FM-4  | Partial provisioning failure (some lvols created, export not)           | Record stuck in `Provisioning`. A retry resumes, and the reconciler GCs after a timeout.                                                                                                             |
| FM-5  | The backing namespace is lost                                           | XFS errors and the export goes `Degraded`. Redundancy is the backend's responsibility through per-lvol erasure coding or replication. This design adds no redundancy of its own.                     |
| FM-6  | Snapshot requested on a volume whose filesystem cannot be frozen        | The freeze is attempted, and a failure aborts the snapshot rather than taking an unquiesced one. A single-volume snapshot needs no consistency group (§6.3), so it is never refused for that reason. |
| FM-7  | Non-XFS fsType requested for RWX                                        | Rejected. pNFS SCSI layout requires XFS.                                                                                                                                                             |
| FM-8  | Two pods on different nodes writing same file                           | Handled by NFSv4 byte-range locking via the MDS. Correctness is NFS's responsibility.                                                                                                                |
| FM-9  | `blkmapd` not running on client                                         | No block layout is obtained, and I/O silently falls back to the MDS. Node plugin must detect and (re)start `blkmapd`, and log.                                                                       |
| FM-10 | Debian/Ubuntu `/dev/disk/by-id` naming differs                          | `eui64` symlink step may fail → no direct path. Must be tested and handled per-distro (§18).                                                                                                         |
| FM-11 | Provisioner double-`CreateVolume`                                       | Idempotent fetch-or-create by stable name, yielding at most one set of lvols and one export.                                                                                                         |
| FM-12 | Node reboot with active RWX mounts                                      | Restage reconnects the namespaces and remounts NFS. Refcounted publish rebuilds the pod mounts.                                                                                                      |
| FM-13 | `fsid` collision on an MDS host                                         | Provisioning fails cleanly. The allocator must guarantee per-host uniqueness.                                                                                                                        |

---

## 17. Observability

### Kubernetes Events

| Event                                                 | Type      | Reason                                         |
|-------------------------------------------------------|-----------|------------------------------------------------|
| Node kernel below the pNFS minimum                    | `Warning` | `NodeNotPNFSCapable`                           |
| Export assembled and published                        | `Normal`  | `ExportReady`                                  |
| Export teardown completed                             | `Normal`  | `ExportRemoved`                                |
| Export state transition (`Provisioning`, `Migrating`) | `Normal`  | `ExportStateChanged`                           |
| MDS migration started, finished                       | `Normal`  | `MDSMigrationStarted`, `MDSMigrationCompleted` |
| Client fenced by persistent reservation               | `Warning` | `ClientFenced`                                 |
| Stripe member unavailable                             | `Warning` | `StripeMemberUnhealthy`                        |

### Prometheus Metrics

| Metric                                        | Labels                          | Description                                                      |
|-----------------------------------------------|---------------------------------|------------------------------------------------------------------|
| `simplyblock_pnfs_provision_duration_seconds` | `cluster`, `result`             | RWX provisioning latency and success or failure                  |
| `simplyblock_pnfs_mds_selections_total`       | `cluster`, `node_uuid`          | MDS selection distribution across eligible hosts                 |
| `simplyblock_pnfs_migrations_total`           | `cluster`, `result`             | MDS migrations by outcome                                        |
| `simplyblock_pnfs_migration_freeze_seconds`   | `cluster`                       | Client freeze window per migration, the NFR-2 bound              |
| `simplyblock_pnfs_direct_io_ratio`            | `cluster`, `export`             | Direct block versus MDS I/O, read from the NFS layout statistics |
| `simplyblock_pnfs_export_failovers_total`     | `cluster`, `export`, `outcome`  | Completed failovers, by outcome, including fence failures        |
| `simplyblock_pnfs_exports`                    | `cluster`, `node_uuid`, `state` | Exports per MDS host by state                                    |

### Logs and status

Export actions on the MDS host are logged per command, including the
idempotency skips, so a partially assembled export is reconstructable from the
log alone. Member connect and disconnect keep the existing initiator logging, and
the client-side `blkmapd` status is logged on stage. The export record surfaces
`state`, `generation`, the bound MDS, and member health, which is what a
`kubectl describe` has to answer without reading a node log.

## 18. Open Questions

Four of the questions earlier drafts carried here are now decided, and are recorded
where they belong rather than left open: the export registry is a CRD (§7.1), the
control channel is csi-link (§6.4), the mount address is a Service ClusterIP
(§13.3), and the volume handle keeps the synthetic `nfs:` form (§11).

| #  | Question                                                                                                                                                                                                                        | Owner               |
|----|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|
| Q1 | **`fsid` allocation:** which component allocates collision-free `fsid`s per MDS host, and which one owns the pseudo-root (`fsid=0`)? Per-host uniqueness is structural here, because one host serves many exports (§8.4).       | Operator            |
| Q2 | **NFSv4.1 server identity:** can `server_owner` and `server_scope` be made to match across MDS hosts, so failover is a reconnect rather than full state recovery? If not, what does state recovery cost an application (§13.3)? | Backend team, spike |
| Q3 | **Service ClusterIP from a kernel mount:** does a sunrpc mount reach a ClusterIP on every supported CNI dataplane, specifically under eBPF kube-proxy replacement (§13.3)?                                                      | Spike               |
| Q4 | **Debian and Ubuntu:** `/dev/disk/by-id` naming and the `nfs-common` difference, and therefore whether the distro matrix can include them (§5.3).                                                                               | Spike               |
| Q5 | **Tenancy model:** `no_root_squash` lets a container root write as root on the shared filesystem. What squash and `fsGroup` model applies, and is Kerberos in scope for GA (§15)?                                               | Product             |
| Q6 | **MDS health probe:** is `StorageNode.status` plus a `/snode/info` field enough to detect a dead `nfsd`, or does the export need its own probe over csi-link (§13.5)?                                                           | Operator            |
| Q7 | **Guardian interaction:** does the existing `MonitorConnection` and Guardian machinery extend to an NFS mount, or does a pNFS mount need its own monitor (§10.4)?                                                               | CSI driver          |

---

## 19. Phased Delivery Plan

The phases below deliver this document. Striping and everything it forces is
[`design-pnfs-striped.md`](design-pnfs-striped.md), which begins where this ends.

- **Phase 0 (external enablers):** everything in
  [Phase 0 — External Prerequisites](#phase-0--external-prerequisites). The `-pr`
  flag and its SPDK support, the csi-link mutation gate and the `nvmeof` migration
  it waits on, and the two spikes that decide what §13 may promise. None of it is
  implementable in this repository.
- **Phase 1 (export lifecycle):** the `NFSExport` CRD, its reconciler, MDS
  eligibility and selection, and `CreateExport` and `DeleteExport` over csi-link.
  Access mode `MULTI_NODE_MULTI_WRITER`, the client NFS mount, and create and delete
  only. This is the end-to-end pNFS path with the fewest moving parts.
- **Phase 2 (stable addressing):** the per-export Service and operator-managed
  EndpointSlice, and planned migration on top of it (§13.4).
- **Phase 3 (fault tolerance):** the MDS health probe, PR fencing, and unplanned
  failover (§13.5), with the freeze bound measured rather than asserted.
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
- **Operator (`O-`):** NFS-server rollout and node eligibility, the
  export-quiesce phase of a drain, and the export transition inside a volume
  migration. Fake client and `envtest`.
- **Integration (`I-`):** the export assembly order and its idempotency, with
  host commands stubbed. The step sequence and the mid-way retry are what break.
- **End-to-end (`E-`, `F-`, `SEC-`, `L-`):** shared read and write across pods,
  the direct block path, migration freeze inside the NFR-2 bound, fencing, and
  soak. A live cluster on kernel 6.11 or later, with the environment requirements
  listed in the plan.

Risk concentrates in three places, and their scenarios must not be the ones cut:
the export assembly's idempotency (`I-01`, `I-03`, `I-06`), the migration freeze
window (`F-01`, `F-02`, `F-06`, `F-07`), and data integrity under concurrent
writers (`E-03`, `F-09`, `L-04`). The matrix has no multi-namespace scenario yet,
which is where the export path and the `fsid` can collide. The plan records that
as a gap.

## Appendix A — Reference Commands

**MDS host (server side, with the `nfsd` daemon co-located and the rest run by the MDS-host csi-node):**
```bash
# prerequisites (once per host): NFS server daemon (systemd or sidecar), NOT blkmapd
yum install -y nfs-utils                 # RHEL family
systemctl enable --now nfs-server        # nfsd + rpc.mountd + rpc.statd

# the namespace is connected by csi-node (initiator.Connect); NOT run here.
# csi-node then assembles the export on the resulting device:
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
systemctl start nfs-blkmap        # blkmapd for block-layout device mapping
nvme connect ...                  # attach the SAME namespace the MDS formatted
# NGUID and the eui64 alias come from atlas-lib once P0-5 lands; until then:
NGUID=$(nvme id-ns /dev/nvme0n1 | awk '/^nguid/{print $3}')
ln -sf /dev/nvme0n1 /dev/disk/by-id/nvme-eui64.${NGUID}
mount -t nfs -o v4.1 <export-service-clusterip>:/mnt/<pvc-name> <staging-path>
# NFSv4 pseudo-root (server exports fsid=0):
#   mount -t nfs -o v4.1 <export-service-clusterip>:/ <path>
```

## Appendix B — Glossary

- **pNFS:** Parallel NFS (NFSv4.1+ extension) separating metadata from data.
- **MDS:** Metadata Server: the `nfsd` host owning the namespace and issuing layouts.
- **Layout / block (SCSI) layout:** description of where a file's data blocks live. The SCSI/block layout points clients at block devices (here, NVMe-oF namespaces) for direct I/O.
- **`blkmapd` / `nfs-blkmap`:** client daemon that maps block-layout device signatures to local block devices.
- **PR:** Persistent Reservations (SCSI-3 / NVMe): used by pNFS SCSI layout to fence clients and protect shared devices.
- **Consistency group:** a set of lvols snapshotted or cloned atomically so a striped filesystem stays crash-consistent. Out of scope here, and the subject of [`design-pnfs-striped.md`](design-pnfs-striped.md).
- **SNodeAPI:** the simplyblock storage-node agent (`simplyblock_web/node_webapp.py`). For pNFS it only grows capability reporting for MDS eligibility, not the export logic.
- **`csi-node`:** the CSI node plugin (`csi-driver/pkg/spdk/nodeserver.go`). It owns NVMe-oF connect and reconnect and, for pNFS, the server-side export assembly (XFS, mount, and `exportfs`) on the MDS host.
- **csi-link:** the operator-to-CSI channel (`atlas-lib/link`, `atlas-lib/node`) that carries export operations to the MDS host (§6.4).
- **Export agent:** earlier term for the export executor. In this design that role is filled by **csi-node**.