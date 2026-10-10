# Design Document: pNFS Metadata Server in an Isolated-Kernel Pod

**Status:** Draft  
**Author:** Manohar Reddy  
**Date:** 2026-10-07 (last updated 2026-10-09)  
**Related Design:** [`design-pnfs-rwx.md`](design-pnfs-rwx.md), which this design amends: it replaces where the metadata server runs and leaves the export, the layout, and the client path unchanged.

---

## Phase 0: External Prerequisites

| #    | Prerequisite                                                                                                                 | Kind                    | Blocks                 | Status                                                                                                                                     |
|------|------------------------------------------------------------------------------------------------------------------------------|-------------------------|------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | Nodes that may run the MDS pod expose `/dev/kvm`. On a cloud instance this is nested virtualization, enabled on the instance | Node OS                 | The MDS pod (§5.4)     | Required of the deployer, stated in the user docs                                                                                          |
| P0-2 | A guest kernel carrying `nvme_get_unique_id` and the NVMe persistent-reservation operations, built with the options in §6.1  | Node OS (guest)         | Any SCSI layout (§6.1) | Present in 6.12.26 (§6.1). The same dependency as `design-pnfs-rwx.md` P0-9                                                                |
| P0-3 | The control plane accepts the guest's host NQN (§6.5) as an allowed host of every exported volume                            | Control plane (`sbcli`) | Assembly (§7.4)        | Unknown                                                                                                                                    |
| P0-4 | Persistent reservations on the volume's namespace (`ptpl_file`), as `design-pnfs-rwx.md` P0-1 and P0-2 require               | Storage plane (SPDK)    | Any SCSI layout        | Merged (P0-1) and present (P0-2) per that design                                                                                           |
| P0-5 | The csi-link mutation gate open for `CreateExport` and `DeleteExport`, as `design-pnfs-rwx.md` P0-3 and P0-4 require         | Ecosystem (this repo)   | Assembly (§7.1)        | Built, unmerged, and read-only (P0-3), and not started (P0-4) per that design, which the code may have outpaced                            |
| P0-6 | A way to build and publish the MDS image (§5.6) next to the operator and CSI images                                          | Ecosystem (release)     | Every phase            | In progress: the pnfs-os workflow publishes the guest image, the MDS image builds with `make -C csi-driver mds-image` and is not in CI yet |

Without P0-1 the MDS pod stays Pending and every export waits in `Pending` with an event naming the cause. QEMU is never started without KVM, because the software-emulation fallback runs the guest too slowly to serve an export. Without P0-2 the guest's nfsd cannot name the device and every client falls back to metadata-server-routed I/O, the failure mode `design-pnfs-rwx.md` calls FM-2, so the design cannot ship on a kernel that fails the check. Without P0-3 the guest cannot connect the namespace and assembly times out into `Degraded`. P0-4 and P0-5 are the dependencies `design-pnfs-rwx.md` names for any pNFS export.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Changes](#4-api-changes)
5. [The MDS Pod](#5-the-mds-pod)
6. [The Guest](#6-the-guest)
7. [Control Channel and Reconciliation](#7-control-channel-and-reconciliation)
8. [Networking and Client Access](#8-networking-and-client-access)
9. [Failure Modes](#9-failure-modes)
10. [Security](#10-security)
11. [Observability](#11-observability)
12. [Testing Strategy](#12-testing-strategy)
13. [Migration Strategy](#13-migration-strategy)
14. [Open Questions](#14-open-questions)

Appendices:

- [Appendix A: `nfsexport_types.go`](#appendix-a-nfsexport_typesgo)
- [Appendix B: `simplyblockdriver_types.go`](#appendix-b-simplyblockdriver_typesgo)

---

## Overview

The pNFS metadata server (MDS) runs in a pod in the operator's namespace whose workload is a QEMU guest. The guest has its own Linux kernel with nfsd, the NFS client, and the NVMe/TCP initiator built in. It connects the volume's namespace itself, makes the XFS filesystem, and serves the export. The customer's node contributes `/dev/kvm` and nothing else: no nfs-utils, no nfsd, no mounts, no exports.

The export assembler (`csi-driver/internal/nfsexport`) runs in the guest as `mds-agent`. The `NFSExport` kind, the `ExportService` protocol, the per-export Service, the SCSI layout, and the client mount path are those of `design-pnfs-rwx.md`. Clients attach the namespace themselves and write to it directly, and the guest serves metadata only.

One MDS pod serves every export of one storage cluster, because nfsd and its export table are global to the kernel they run in. The pod is created by the `NFSExport` reconciler when the first export of a storage cluster binds. No CRD, controller, or device plugin is added to the cluster, and nothing from NeonVM or KubeVirt is installed. The guest launch follows the technique NeonVM's runner uses (QEMU with a direct kernel boot and a tap bridge in a privileged pod) and uses none of its API.

---

## 1. Background

An NFS server is kernel state. nfsd's threads, its export table, and the mounts it serves belong to the kernel they run in, and pNFS SCSI layouts are a kernel nfsd feature, so no userspace server can stand in for it. Serving exports from a customer's Kubernetes node makes that node's kernel the server, which has four costs, and they are the reason the MDS has a kernel of its own.

- **The node's kernel is the server.** nfsd, `nfsdcld`, and `rpc.idmapd` run against the host, and their threads outlive any pod. A node that served an export keeps an NFS server after the export is gone.
- **The node plugin is privileged for the job.** It needs the host's `/etc/exports.d`, `/var/lib/nfs`, and an export root mounted with bidirectional propagation, and Talos, among others, mounts `/etc` read-only.
- **Host prerequisites are invisible to the operator.** A node without nfsd fails only when an export is assembled on it.
- **The export's mounts are host state.** A node that crashes mid-export leaves mounts and an export table entry on a machine the operator does not own, and nothing can move them to another host.

The primitives this design builds on are the `NFSExport` kind and its phases, the `ExportService` protocol in `atlas-lib/nfsexport/nfsexportrpc`, csi-link with its `Registry`, the per-export Service and EndpointSlice (`operator/internal/controller/nfsexport_service.go`), and the assembler package. The pattern reused for scheduling is the VDO capability probe, which publishes a node label from a probe in the node plugin (`atlas-lib/kube/names.go`, `LabelVDOCapable`).

---

## 2. Goals and Non-Goals

### Goals

- No customer node runs nfsd, mounts an export's filesystem, or holds an exports table entry. The only node requirement is `/dev/kvm`.
- No CRD, controller, or device plugin is installed beyond the operator's own. The MDS pod is created by the existing `NFSExport` reconciler.
- The guest connects the volume's namespace with its own NVMe/TCP initiator, so the device identity and persistent-reservation keys nfsd uses are those of the real namespace.
- The cluster secret never enters the guest. The pod resolves connection details and passes them in.
- A restarted MDS pod converges: every `Ready` export bound to it is reassembled without operator action.
- Observability covers the MDS pod's boot, address, and resync, and a refusal to start (no KVM, no state volume, no kernel) is an event rather than a stall.

### Non-Goals

- **Failover and live migration.** A restarted pod is a cold boot followed by a resync (§7.5). Moving an MDS without a restart, fencing a partitioned one, and unplanned failover remain `design-pnfs-rwx.md` §13, which this design does not implement.
- **Several MDS pods per storage cluster.** One pod serves every export of a storage cluster, because nfsd's export table is global to its kernel.
- **A change to the data path.** Layouts, `nvme-eui.` aliases, and client mounts are `design-pnfs-rwx.md` §10.
- **A general VM facility.** The runner starts one guest with one disk layout. Hotplug, resize, snapshots of the guest, and a guest API are not provided.
- **Software emulation.** The runner refuses to start without `/dev/kvm`.
- **Client addressing beyond a Service.** Clients mount the per-export ClusterIP exactly as they do today (§8).

---

## 3. Architecture Overview

```
┌────────────────────────────── operator namespace ─────────────────────────────┐
│                                                                               │
│   operator (leader)                                                           │
│   ┌────────────────────────────┐        csi-link (TLS, gRPC over yamux)       │
│   │ NFSExportReconciler        │◀──────────────────────────────┐              │
│   │ 1. ensure MDS StatefulSet  │                               │ dials out    │
│   │ 2. bind export to MDS pod  │                               │              │
│   │ 3. CreateExport / Check /  │        ┌──────────────────────┴───────────┐  │
│   │    DeleteExport            │        │ MDS pod (privileged, KVM node)   │  │
│   │ 4. EndpointSlice → pod IP  │        │ ┌──────────────────────────────┐ │  │
│   └─────────────┬──────────────┘        │ │ runner                       │ │  │
│                 │ Service +             │ │  link peer, control-plane    │ │  │
│                 │ EndpointSlice         │ │  client, QEMU launcher,      │ │  │
│                 │ per export            │ │  bridge + DNAT               │ │  │
│                 ▼                       │ └───────────────┬──────────────┘ │  │
│   export namespace (PVC's)              │      private bridge (link-local)  │  │
│   Service nfsexp-…-nfs ClusterIP ──────▶│ ┌───────────────▼──────────────┐ │  │
│                                         │ │ guest (own kernel)           │ │  │
│                                         │ │  agent → assembler           │ │  │
│                                         │ │  nvme connect, mkfs.xfs,     │ │  │
│                                         │ │  nfsd, nfsdcld, exportfs     │ │  │
│                                         │ │  /var/lib/nfs ← state disk   │ │  │
│                                         │ └───────────────┬──────────────┘ │  │
│                                         └─────────────────┼────────────────┘  │
└───────────────────────────────────────────────────────────┼───────────────────┘
                              NVMe/TCP, from the guest      │
┌───────────────────────────────────────────────────────────▼───────────────────┐
│  Simplyblock storage cluster                                                  │
│  namespace (lvol) shared with the clients, ptpl_file reservations             │
└───────────────────────────────────────────────────────────────────────────────┘

  client node: kernel NFSv4.1 mount of the per-export ClusterIP (metadata),
               NVMe/TCP to the same namespace (data, direct)
```

The runner is the csi-link peer. It dials the operator with the pod's bound ServiceAccount token, exactly as a node plugin does, and relays the `ExportService` calls to the guest agent over a private bridge. The guest agent is the assembler. The guest holds no Kubernetes credential and no control-plane credential.

**Where the export logic runs.** The assembler package runs in one place, the guest agent. The node plugin uses only its attach half, to connect a client's namespace.

---

## 4. API Changes

Two existing types change. No kind is added.

### 4.1 `NFSExportStatus` (existing kind)

The bound MDS is named by `mdsPodName`, and `mdsNodeIP` is the pod's IP, which the export's EndpointSlice points at. `assembledBy` records the guest instance that assembled the export.

```go
// MDSPodName is the MDS pod serving this export. Empty until the export is
// bound, and the serialization point for one MDS per export.
// +optional
MDSPodName string `json:"mdsPodName,omitempty"`

// AssembledBy is the MDS instance that last assembled this export: the pod
// UID and runner container ID. A mismatch with the live instance means the
// guest has rebooted and the export must be reassembled.
// +optional
AssembledBy string `json:"assembledBy,omitempty"`
```

The type's full text is in Appendix A. The `MDS` printcolumn shows `mdsPodName`.

### 4.2 `DriverPNFS` (existing type, on `SimplyblockDriver`)

```go
// MDS configures the metadata server. Unset, no pNFS volume can be exported.
// +optional
MDS *DriverPNFSMDS `json:"mds,omitempty"`
```

`DriverPNFSMDS` carries the image, resources, scheduling constraints, and the state volume's size and class. Its full text is in Appendix B. `MDS` is the only field of `DriverPNFS`. With it unset, an export waits in `Pending` with a `NoMetadataServer` event.

---

## 5. The MDS Pod

### 5.1 Workload

The `NFSExport` reconciler ensures one StatefulSet of one replica per storage cluster in the operator's namespace. It is named by atlas-lib's `kube.Formula` from the full cluster ID, held to the 52 characters a StatefulSet name may have: as much of the ID as fits and a digest of all of it, so two clusters whose IDs share a prefix never share a StatefulSet. An existing StatefulSet of that name labeled with another cluster's ID is not used: the export waits with an `MDSNameCollision` event. The StatefulSet is rendered by the `SimplyblockDriver` controller's package (`operator/internal/controllers/driver`), beside the node plugin's DaemonSet and the controller plugin's Deployment, because its volumes (the cluster secret, the TLS client certificate, the link token) and its image default are the driver's. The `NFSExport` reconciler calls that builder when the first export of a storage cluster binds. A StatefulSet gives the pod a stable identity and a stable PVC for the state disk, and its UID is the seed of the guest's host NQN (§6.5), so the NQN survives pod restarts. The StatefulSet is owned by the `SimplyblockDriver` object, so disabling pNFS removes it.

The StatefulSet is created on first need and reconciled with the same create-or-leave discipline as the per-export Service. It is not deleted when its last export is.

### 5.2 Containers

The pod runs one container, the runner, built from the MDS image (§5.6). The runner:

1. Checks that `/dev/kvm` is usable and exits non-zero with a reason if it is not.
2. Creates the private network (§8.2) and the QEMU command line.
3. Starts QEMU with `-enable-kvm`, `-kernel` the image's guest kernel, `-append` carrying the root device and the guest's static address, the image's root disk as the first virtio-blk device (read-only), and the state disk as the second, carrying the virtio serial `pnfs-state`.
4. Dials the operator over csi-link, authenticated by the pod's projected ServiceAccount token, once the guest agent answers.
5. Serves the readiness probe from the guest agent's health call, so a pod is Ready only when the guest can assemble an export. Until the agent exists, the probe is a TCP connect to the guest's port 2049.
6. On `SIGTERM`, presses the guest's power button over QMP and kills QEMU when the guest has not exited within the shutdown grace. A guest that exits on its own, with any status, ends the runner with an error, and kubelet restarts the pod. The runner never restarts QEMU itself.

The command-line construction follows `neonvm-runner/cmd/main.go` in the autoscaling repository (machine type, `-cpu`, virtio devices, direct kernel boot), and the network setup follows its `cmd/net.go` (bridge, tap, dnsmasq-free static addressing, DNAT). The packages are `csi-driver/internal/mds/qemu` (the command line), `netsetup` (bridge, tap, rules), `qmp` (the power button), and `runner` (sizing and the lifecycle), and the binary is `csi-driver/cmd/mds-runner`.

The guest's architecture is the node's. On x86-64 the runner starts `qemu-system-x86_64` with the `q35` machine. On ARM64 it starts `qemu-system-aarch64` with the `virt` machine, passes UEFI firmware with `-bios` so the guest gets ACPI, and appends `acpi=on`. Both boot the kernel directly.

### 5.3 State disk

A RWO PVC of `stateSize` is attached to QEMU as a raw block device, so the guest sees a virtio-blk disk with the serial `pnfs-state`. The guest's fstab mounts `/dev/disk/by-id/virtio-pnfs-state` at `/var/lib/nfs` with `x-systemd.makefs`, so systemd formats it (ext4) on first boot, and `nfsdcld` keeps its client-recovery database there (`storagedir=/var/lib/nfs/nfsdcld`). The root disk is attached read-only from the image, so the state disk is the only state the guest keeps. A guest without its state disk never reaches `local-fs.target`, never turns healthy, and is restarted by the boot deadline.

The state disk is always a simplyblock volume. A node-local disk pins the pod to the node it first ran on, and the client-recovery database is what lets NFS clients reclaim their state when the pod restarts on another worker. When `stateStorageClassName` is unset, each storage cluster gets a class of its own, `<driver>-<cluster>-pnfs-mds-state`, which the reconciler writes before it creates the StatefulSet. It is derived from a class of that cluster: one this driver provisions, whose `cluster_id` is that cluster's, and which is not a pNFS class, preferring one the operator wrote for a pool. It takes only the cluster, pool, fabric and encryption parameters, so none of the QoS caps meant for user volumes reach the state disk, and keeping it on the same cluster adds no failure the exports do not already have. The class carries a managed-by label of its own and no pool label: a pool label would assign it to the pool, whose deletion waits on its classes. A named class must be one this driver provisions and not a pNFS class. When no class qualifies, the StatefulSet is not created, the export stays `Pending`, and an `MDSStateUnavailable` event names the reason. The class is chosen once, when the StatefulSet is created, because a claim template cannot change afterward.

Kubernetes has no permission for using a StorageClass, so the class is reserved by a `ValidatingAdmissionPolicy` the reconciler creates before the class. It refuses a new claim whose class name ends in `-pnfs-mds-state`, and its binding applies it to every namespace but the operator's, where the StatefulSet controller creates the state disk's claim. A policy evaluated in the API server, rather than a webhook, costs nothing to serve and cannot make every claim in the cluster wait on the operator. It needs Kubernetes 1.30, the supported minimum.

### 5.4 Scheduling and privilege

The pod is privileged. The container needs `/dev/kvm` and `/dev/net/tun`, and without a device plugin the only way to open them is the host device nodes through a privileged container. The pod has no `hostNetwork` and no `hostPID`, and it mounts no host directory other than the two device nodes.

Scheduling uses `storage.simplyblock.io/kvm-capable=true`, a node label published by the csi-node plugin from a probe that opens `/dev/kvm` for reading and writing (`AdvertiseKVMCapability`). It follows the VDO probe and shares its publishing step: it runs once at plugin start, stamps its value with `storage.simplyblock.io/kvm-capable-managed-by: auto-detect`, and leaves a hand-set label (one without that annotation) alone, so a golden-image node can assert capability. The pod's `nodeSelector` is that label merged with `spec.pnfs.mds.nodeSelector`, and its tolerations come from `spec.pnfs.mds.tolerations`.

A pod that cannot schedule leaves its exports in `Pending`, and the reconciler reads the pod's `PodScheduled` condition to emit the `NoKVMCapableNode` event rather than waiting silently.

### 5.5 Resources

`spec.pnfs.mds.resources` sets the pod's requests and limits. The guest's memory and vCPU count are derived from them and passed on the QEMU command line, so one setting controls both. The limits reach the runner through the downward API (`limits.cpu` with divisor `1m`, `limits.memory` with divisor `1Mi`). The guest gets the whole cores the CPU limit covers, and at least one, and the memory limit less a 256 MiB allowance for QEMU and the runner. A guest needs at least 256 MiB, so the smallest memory limit is 512 Mi, and a limit below it fails the runner with that reason. A CPU or memory limit the spec leaves unset defaults to 2 CPUs and 2 Gi, because without a limit the downward API reports the node's whole allocatable capacity and the guest would take the node. A memory request the spec leaves unset defaults to 512 Mi, or to the limit when that is lower. The guest has a virtio balloon with free page reporting (`-device virtio-balloon-pci,deflate-on-oom=on,free-page-reporting=on`), so QEMU returns the pages the guest frees and the pod holds about what the guest uses: QEMU peaked at 315 MiB over a pnfs-fio run with a 1792 MiB guest. Under memory pressure on the node, the balloon deflates rather than the guest being killed. The allowance is an estimate until boots are measured.

### 5.6 Image

Two images, each built for `linux/amd64` and `linux/arm64`:

| Image                    | Built from                                                   | Holds                                                                                     |
|--------------------------|--------------------------------------------------------------|-------------------------------------------------------------------------------------------|
| `spdkcsi:pnfsos`         | pnfs-os (vela-os, buildroot), `prototype/boards/pnfs-common` | `/vmlinuz` and `/disk.qcow2`, from `scratch`. Never run                                   |
| `spdkcsi:pnfs-mds-<tag>` | `csi-driver/deploy/image/Dockerfile.mds`                     | the target's QEMU, the ARM64 UEFI firmware (`aavmf`), iptables, the guest, and the runner |

The guest image carries the kernel built with the configuration fragment of §6.1 and the root filesystem of §6.2, and is published by the pnfs-os workflow as `pnfsos-amd64`, `pnfsos-arm64`, and the multi-architecture `pnfsos`. The MDS image copies the kernel out of it (`/mds/kernel/vmlinuz`) and writes `mds-agent` into the root filesystem (`/usr/bin/mds-agent`) before copying the disk (`/mds/disk.qcow2`), using `debugfs` on the ext4 image, which needs no mount and no privilege. The agent is therefore always the runner's version, a guest change rebuilds no Go, and a runner change rebuilds no buildroot. It is tagged and released with the operator and CSI images, and `spec.pnfs.mds.image` defaults to the operator's own registry and tag in the `spdkcsi` repository with the tag prefixed `pnfs-mds-`. Both binaries (the runner and the guest agent) are built from the `csi-driver` Go module, which is where the assembler package lives, as `cmd/mds-runner` and `cmd/mds-agent`.

---

## 6. The Guest

### 6.1 Kernel

The guest kernel is a single built-in image with no module tree. Its configuration enables:

| Group             | Options                                                                                                           |
|-------------------|-------------------------------------------------------------------------------------------------------------------|
| NFS server        | `NETWORK_FILESYSTEMS`, `NFSD`, `NFSD_V4`, `NFSD_PNFS`, `NFSD_SCSILAYOUT`, `EXPORTFS_BLOCK_OPS`, `SUNRPC`, `LOCKD` |
| NFS client        | `NFS_FS`, `NFS_V4`, `NFS_V4_1`, for the assembler's loopback self-check (`selfCheckNFSD`)                         |
| NVMe-oF initiator | `NVME_CORE`, `NVME_MULTIPATH`, `NVME_FABRICS`, `NVME_TCP`                                                         |
| Filesystem        | `XFS_FS`, `FHANDLE`                                                                                               |
| Virtualization    | `VIRTIO_PCI`, `VIRTIO_BLK`, `VIRTIO_NET`, and the console and entropy devices the guest init needs                |
| Guest contract    | `IP_PNP`, for the address on the kernel command line                                                              |

The base is the 6.12.26 configuration in the autoscaling repository's `neonvm-kernel`, one per architecture, which enables XFS and virtio and leaves NFSD and every NVMe option off. pnfs-os applies the options above as a fragment (`boards/pnfs-common/linux-pnfs.fragment`). The block layout (`NFSD_BLOCKLAYOUT`) is built beside the SCSI layout, and the PCIe driver `BLK_DEV_NVME` is not, since the guest has no NVMe device of its own.

6.12.26 carries what the SCSI layout needs (P0-2). `nvme_get_unique_id` is the namespace disk's `get_unique_id` operation, and `nvme_pr_ops` its register, reserve, and preempt operations (`drivers/nvme/host/core.c`, `pr.c`), which are the calls `fs/nfsd/blocklayout.c` makes. The native multipath head disk implements both as well (`drivers/nvme/host/multipath.c`), so a filesystem on the multipath device is one nfsd can lay out. The mkfs feature baseline pinned in `csi-driver/internal/nfsexport/format.go` applies to this kernel as it does to a host kernel.

### 6.2 Root filesystem

The root filesystem is read-only and carries `nfs-utils` (`rpc.nfsd`, `exportfs`, `nfsdcld`, `rpc.idmapd`, `mount.nfs`), `nvme-cli` (the `nvmeof` package's CLI connector shells out to it), `xfsprogs`, `e2fsprogs` (the state disk), `util-linux`, and the guest agent, under systemd. It is the only place `nfs-utils` is installed for pNFS.

| Path                           | Kind               | Content                                                |
|--------------------------------|--------------------|--------------------------------------------------------|
| `/`                            | read-only          | the image                                              |
| `/var`                         | tmpfs              | populated from `/usr/share/factory/var` on every boot  |
| `/var/lib/nfs`                 | state disk         | `nfsdcld/`, the client-recovery database               |
| `/var/lib/simplyblock/exports` | tmpfs              | the exports' mount points                              |
| `/etc/exports.d`               | → `/run/exports.d` | the exports' drop-ins, rebuilt after every boot (§7.5) |
| `/run/nfs`                     | tmpfs              | nfs-utils' own state (`etab`, `rpc_pipefs`)            |

`nfs-server.service` brings up `rpc.mountd`, `rpc.idmapd`, and `nfsdcld` for NFSv4.1 and 4.2 only (`/etc/nfs.conf`), after the state disk is mounted. `rpcbind`, `rpc.statd`, and `blkmapd` are masked. `mds-agent.service` starts the agent once it is part of the image. journald writes to the console, and the runner passes the console to the pod log.

### 6.3 Guest agent

The guest agent serves the `ExportService` (`CreateExport`, `DeleteExport`, `CheckExport`) with `nfsexport.NewAssembler` and `WithNFSD`, the same construction `csi-driver/internal/driver/driver.go` registers on the node plugin's link agent. On a guest, `EnsureNFSD` needs no host cooperation: the control filesystem, the threads, `nfsdcld`, and `rpc.idmapd` all run in the guest's own namespaces, and the loopback self-check mounts the guest's own nfsd.

The agent adds two things the node plugin does not have: a health call and a connection source that takes its input from the request (§6.4). It serves both the export service and gRPC's standard health service on TCP 7070 of the guest's address, which only the runner reaches across the bridge (§8.2). The health is `SERVING` while the state disk is mounted at `/var/lib/nfs` and nfsd has threads running, and `NOT_SERVING` from start until the first check passes, so a booting guest is never reported ready. The agent holds no credential and derives no host identity: both arrive with each call.

The export service also carries one read-only diagnostic, `FileExtents`, which the node plugin does not serve. It reports what a byte range of one file in an export is made of on the guest's disk, piece by piece, as written data with its device offset, an unwritten extent, a delayed allocation, or a hole, together with the file's size. All but the first read back as zeros, so the answer says where a write that a client reads as zeros was lost. The agent answers only for a regular file directly inside an export directly under `/var/lib/simplyblock/exports`, opened without following links, because the answer carries device offsets. The guest has no shell, so `mds-runner extents -export <path> -file <name> -offset <n> -length <n>`, run in the runner container, asks the agent across the bridge and prints the answer as JSON.

### 6.4 Connection details

The node plugin's assembler resolves a volume's NVMe-oF connections by calling the control plane (`csi-driver/internal/nfsexport/attach.go`). The guest does not do this. It boots from a shared image that holds no credential, so the cluster secret and the control plane's TLS material stay with the pod.

The runner resolves them instead, with the same control-plane client the node plugin uses (`nfsexport.PublishedConnection`), so the TLS settings (`SB_TLS_CONNECT`, `SB_TLS_CERTIFICATE_AUTHORITY`, and the client certificate) apply. Its relay (`csi-driver/internal/mds/relay`) resolves the connection for the host NQN of §6.5 and passes it on with the spec. `CreateExport` requires the connection, because a guest without one reports a missing device, which names the wrong problem. `DeleteExport` carries one when the control plane answers and goes ahead without it otherwise, since the guest then releases the namespace its stack record names and a teardown has to work without the control plane. `CheckExport` attaches nothing and carries none. A connection arriving with the operator's call is dropped, so DHCHAP secrets reach the guest only from the pod's own resolution.

`ExportSpec` in `atlas-lib/nfsexport/nfsexportrpc/nfsexportv1/export.proto` carries the two as `host_nqn` and `connection`, mirroring `lvol.Connection`, with the two connect timeouts optional so that zero and unset stay apart. The assembler's planner takes a supplied connection before asking the control plane or reading the stack record.

The runner mounts the same cluster secret the CSI driver uses, so a storage cluster the driver can reach is one the MDS can reach.

### 6.5 Host NQN

The guest's host NQN is `nqn.Host(<StatefulSet UID>)`. The operator computes it from the controller owner reference of the MDS pod and sends it with every call as `host_nqn`, since the pod's ServiceAccount has no permission to read its StatefulSet. The StatefulSet's UID is stable across pod restarts and changes only when the StatefulSet is recreated, which is when the old authorization should not carry over. A stable NQN gives the guest the same persistent-reservation key after every restart, which is what `attach.go` requires of a host: one key per host.

### 6.6 Boot sequence

The kernel configures the guest's address from `ip=` on its command line before init runs, on `eth0`, which the guest keeps by not renaming interfaces. systemd then mounts `/var` and the state disk, formatting the state disk when it carries no filesystem, starts the NFS server, and starts the agent. The runner treats the pod as not Ready until the agent's health call succeeds. A guest that has not answered within the boot deadline (`mdsBootDeadline`, 120 seconds) is killed and the pod restarts.

The kernel command line is `root=/dev/vda ro console=hvc0 panic=-1 ip=<guest>::<gateway>:<netmask>:<hostname>:eth0:off`, with `acpi=on` added on ARM64. The hostname is the pod name. nfsd derives its server owner and scope from it, and NFSv4.1 clients reclaim their state after a restart only from a server whose identity did not change, so the StatefulSet's stable pod name is what lets a restarted guest keep its clients. `panic=-1` turns a guest panic into a reboot, which QEMU's `-no-reboot` turns into QEMU exiting.

The command line also carries `xfs.pnfs_zeroed_layouts`, `1` unless the runner is started with `--zeroed-layouts=false`. It is a parameter of the guest kernel's patch 0005 (pnfs-os). Without it, XFS hands out a write layout over unwritten blocks, and only the client's LAYOUTCOMMIT converts them to written. The Linux client never resends a LAYOUTCOMMIT that a restarted server lost: it encodes no reclaim, ignores `NFS4ERR_GRACE` and `NFS4ERR_BADLAYOUT` on the commit, and drops its layouts when the server reboots. Data a client wrote between its last commit and a server restart is then on the device, but reads back as zeros everywhere except the writer's own page cache. With the parameter set, the blocks of a write layout are zeroed and written when they are allocated, so a client's write is durable once it completes on the device. Each allocation costs one WRITE ZEROES on the volume. The file size still travels only in LAYOUTCOMMIT, so an append whose size update was lost is still cut off.

With `--debug-ssh`, the runner also runs an SSH server in the guest for debugging. It generates an ed25519 key pair at every boot and keeps the private half in the container. It hands the public half to the guest as the fw_cfg item `opt/io.simplyblock/ssh_authorized_keys`, and adds `simplyblock.debug_ssh=1` to the command line, which is the condition the guest's dropbear unit starts on. The runner image installs a wrapper as `/usr/local/bin/ssh`, so `kubectl exec -it <pod> -- ssh` logs in to the guest as root over the private bridge, and `-- ssh <command>` runs one command. Without the flag the guest runs no SSH server. Anyone who can exec into the pod can log in, and nobody else can: nothing is forwarded out of the pod, and no secret is stored.

---

## 7. Control Channel and Reconciliation

### 7.1 Peer identity

A third peer kind, `PeerKindMDS`, is added to `atlas-lib/link/peer.go`, named by the pod name as the controller kind is. `KubeAuthenticator.authorizeKind` restricts it to the MDS ServiceAccount, so a node plugin cannot register as an MDS and an MDS cannot register as a node. The runner's `Claim.InstanceUID` is the pod UID, which the hub already treats as the process lifetime.

### 7.2 Assembler addressing

`ExportAssembler`'s methods address a `link.PeerID`, the MDS pod's `link.MDSPeer(podName)`, which the reconciler derives from the export's `mdsPodName`. A missing session is a requeue, not a failure.

### 7.3 Selection

`reconcilePending` takes these steps, and waits in `Pending` with a `NoMetadataServer` event while no `SimplyblockDriver` sets `spec.pnfs.mds`:

1. Ensure the StatefulSet for the export's storage cluster (§5.1).
2. Read the pod. If it is not scheduled, emit `NoKVMCapableNode` or `MDSStateUnavailable` according to the pod's conditions and requeue.
3. If the pod is not Ready, requeue.
4. Bind: write `mdsPodName`, `mdsNodeIP` (the pod IP), the Service address, and the client set, then transition to `Assembling`. The binding is written before anything is asked of the guest, so a reconcile that dies mid-assembly finds it.

The client set is every cluster node's InternalIP and pod CIDR (`clusterNodeAddresses`), because the pod CIDR is the address the guest sees (§8.3).

### 7.4 Assembly

`reconcileAssembling` calls `CreateExport` on the MDS peer. The runner resolves the connections (§6.4), the guest agent assembles, and the reconciler records `assembledBy`, the pod UID and the runner container's ID, in the same status write that moves the export to `Ready`.

### 7.5 Resync

A guest that has rebooted has lost its mounts, its exports table, and its nfsd threads, and the state disk holds only the client-recovery database. The reconciler detects this without polling the guest. `reconcileReady` compares `status.assembledBy` with the MDS instance currently bound: the pod's UID and its runner container's ID. The UID alone misses a guest that crashed, since kubelet restarts the runner container inside the same pod; the container's ID is new on every start. A mismatch means the exports on this pod were assembled by a previous boot, and `CreateExport` is called again (the call is idempotent) and `assembledBy` rewritten. The StatefulSet's pod is watched, and a pod event enqueues every export bound to that storage cluster.

Every pass also repoints the export's EndpointSlice and `status.mdsNodeIP` when they name another address than the running pod's (§8.1), before any reassembly so a client's retry reaches the new guest. The comparison does not wait for a restart to be detected: an export bound while its pod was being replaced records the old pod's address and is then assembled by the new one, so its instance matches while its address does not. A pod that is gone, or whose guest has not linked yet, is waited for, and `assembledBy` is rewritten only after the reassembly succeeded. The pod watch matches pods carrying `storage.simplyblock.io/cluster-id` and maps them to the exports whose `mdsPodName` names them, which also wakes an export waiting in `Pending` for its pod to turn Ready.

### 7.6 Health

`CheckExport` is relayed to the guest as today. A failed check is an event and a fast recheck and never a phase change, for the reason `reconcileHealth` already states.

### 7.7 Deletion

`reconcileDelete` calls `DeleteExport` on the MDS peer. When the MDS pod is gone, the guest's mounts and exports table are gone with it, so a deletion that finds no pod and no session has nothing to tear down and releases the finalizer. A pod that exists without a session may still hold the mount, and the finalizer waits for it.

### 7.8 RBAC

The operator's ClusterRole gains:

```go
// The MDS pod's workload: the StatefulSet that hosts the guest, and the pod it
// runs, read to bind an export and to tell why it has not scheduled.
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// The MDS pod's state disk.
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
```

The MDS pod's own ServiceAccount has no Kubernetes permissions. It exists to carry the projected token csi-link authenticates.

---

## 8. Networking and Client Access

### 8.1 Service and EndpointSlice

The per-export Service and EndpointSlice are unchanged in shape. The EndpointSlice's single endpoint is the MDS pod's IP instead of a node's InternalIP, in the export's namespace, and an EndpointSlice endpoint is an address with no namespace requirement, so the pod's namespace does not matter. The pod IP changes when the pod restarts, and §7.5 repoints every affected EndpointSlice. A client's mount address (`status.serviceAddress`) does not change.

A pod that is terminating or gone has its address withdrawn at once: each bound export's EndpointSlice keeps no endpoint and `status.mdsNodeIP` is cleared until the replacement has an address. Clients reconnect within a second of the guest shutting down, and kube-proxy translates a new connection to whatever the EndpointSlice lists. With the dead pod listed, the connection's conntrack entry points at it, and the client's SYN retries on the same tuple keep that entry alive for up to two minutes after the replacement serves (run `pnfs-1791575321`). With no endpoint, kube-proxy refuses the connection and the client retries against the replacement once it is listed. An entry created before the withdrawal took effect is still translated to the dead address, so the operator also asks every CSI node plugin over the link to delete the host's conntrack entries for TCP port 2049 to the export Service's ClusterIP whose reply source is the old pod IP: about two seconds after the withdrawal, so kube-proxy has applied it, and again when the replacement's address is set. The ClusterIP is part of every request because a dead pod's IP can be handed to another pod serving the same port behind another Service. A replacement that receives the old pod's IP needs no second flush, since its flows already reach the live pod, and the withdrawal's request is dropped if it has not run yet. The flush is `atlas-lib/conntrack`, served by the node plugin on the host's network, and it touches no other address or port. A failed or unreachable node is logged and skipped, and a reconcile never waits on it.

### 8.2 Inside the pod

The runner creates a bridge with a tap device for the guest and gives the guest a link-local address on it: the bridge is `169.254.100.1/30` and the guest `169.254.100.2`, a subnet that cannot collide with a pod, Service, or node CIDR and stays clear of the cloud metadata address `169.254.169.254`. The pod's own interface is the one carrying the default route, whatever the CNI names it. Inbound traffic to the pod's port 2049 is DNATed to the guest, so nfsd is reachable at the pod IP, and the guest agent's gRPC port is reachable only from the runner on the bridge. Outbound traffic from the guest (NVMe/TCP to the storage cluster) is masqueraded behind the pod IP. The masquerade rule matches only the guest's source address leaving through the pod's interface, so replies to DNATed clients keep their addresses. The runner enables IPv4 forwarding in the pod's network namespace and installs each rule only when it is absent, since a restarted runner container finds the previous one's bridge, tap, and rules in the namespace the pod kept. The technique is `neonvm-runner/cmd/net.go`, without dnsmasq, because the guest's address is static.

Only port 2049 is forwarded. NFSv4.1 needs no portmapper, and `rpc.mountd` and `rpc.statd` serve only earlier protocol versions (Open Question 5). The guest agent's port, TCP 7070, is not forwarded and is reached only by the runner.

### 8.3 Allow-list

The exports entry lists client addresses. DNAT preserves the source address of a connection, so the guest sees whatever address the pod network delivers, and a node's traffic to a pod is rewritten by the CNI to the node's own address in its pod CIDR: a tunnel address across nodes and a bridge address on the same node, both depending on the CNI. Seen on the lab cluster, every node reached the MDS pod from its pod-network address and never from its InternalIP, so an entry of InternalIPs alone refused every mount with `access denied`.

An export therefore lists each node's pod CIDR (`spec.podCIDRs`) beside its InternalIP. The CIDR covers both of the node's source addresses whichever the CNI uses, and it also admits the node's own pods. Which client moves data through the metadata server is therefore not decided by this entry. The design requires the node plugin to verify after mounting that the client holds a layout, and to refuse a mount whose I/O would route through the MDS (not implemented yet).

---

## 9. Failure Modes

| Failure                                                  | Detection                                                                                                           | Behavior                                                                                                                                                                      |
|----------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| No node carries `kvm-capable=true`                       | Pod `PodScheduled=False`                                                                                            | Exports stay `Pending`. `NoKVMCapableNode` event, requeue every 30 seconds                                                                                                    |
| `/dev/kvm` missing or unopenable at start                | Runner exits non-zero with the reason in its log                                                                    | Pod crash-loops. `MDSBootTimeout` after the boot deadline. QEMU never falls back to software emulation                                                                        |
| State PVC does not bind                                  | Pod `Pending` on its volume                                                                                         | Exports stay `Pending`. `MDSStateUnavailable` event                                                                                                                           |
| Guest does not answer within `mdsBootDeadline`           | Runner readiness never turns true                                                                                   | Runner kills the guest and exits, kubelet restarts the pod. `MDSBootTimeout` event                                                                                            |
| MDS pod or guest restarts                                | New pod UID or runner container ID, new link session                                                                | Cold boot. Every `Ready` export bound to it is reassembled (§7.5). Clients see the NFSv4.1 recovery path while the guest boots                                                |
| Clients reconnect into the replaced pod's address        | A client flow still translated to the old pod IP after the replacement is Ready (`pnfs.conntrack-pinned` in sbtest) | The address is withdrawn when the pod terminates, and the nodes forget the flows to it (§8.1). Without both, metadata stalls for up to two minutes and soft mounts return EIO |
| MDS pod's node is lost                                   | Pod stuck `Terminating` until the API server force-deletes                                                          | A second pod is not started while the first is `Terminating`, because it is a StatefulSet. Exports are unserved until it is gone                                              |
| Control plane unreachable from the runner                | Connection resolution fails                                                                                         | `CreateExport` returns an error and is retried with backoff. A TLS failure is reported as the error, not retried silently                                                     |
| Control plane refuses the guest's host NQN               | Connect returns an authorization error                                                                              | Assembly fails and times out into `Degraded` after five minutes                                                                                                               |
| Guest cannot reach the storage cluster over NVMe/TCP     | `nvme connect` fails in the guest                                                                                   | Assembly fails and times out into `Degraded`                                                                                                                                  |
| Reconnect after a restart refused for a stale controller | Connect returns a duplicate host error                                                                              | Assembly retries until the target ages out the old controller. Open Question 4                                                                                                |
| Image pull fails                                         | Pod `ErrImagePull`                                                                                                  | Exports stay `Pending`. `MDSBootTimeout` event                                                                                                                                |
| Export deleted while the MDS pod is down                 | No pod, no session                                                                                                  | The finalizer is released, since the guest's mounts left with it (§7.7)                                                                                                       |
| Two guests mount one filesystem                          | Not detected                                                                                                        | Data loss. The mutual exclusion is the one-replica StatefulSet and, for a partitioned node, the fencing `design-pnfs-rwx.md` §13.2 designs and does not build                 |

---

## 10. Security

The MDS pod is privileged, as csi-node is, but its blast radius differs. The pod holds the host device nodes `/dev/kvm` and `/dev/net/tun` and runs QEMU, and an escape from the guest into the runner is an escape into a privileged container. Nothing in the pod mounts a host directory, uses `hostNetwork`, or uses `hostPID`, so a compromised guest reaches the node only through the runner.

The guest holds no Kubernetes token and no control-plane secret (§6.4). It holds the connection details of the volumes it serves, which are the same details any client of those volumes holds, and the guest's host NQN, which the control plane authorizes per volume.

The operator's namespace must permit a privileged pod. That is already so where the driver's node plugin runs in the same namespace. The `rbac-hardening` skill's justification for the grant is that `/dev/kvm` and `/dev/net/tun` cannot be opened by an unprivileged container without a device plugin, and a device plugin is the new component this design declines to add.

The guest's export entries inherit the open items of `design-pnfs-rwx.md` §15 (`no_root_squash`, tenancy), unchanged.

---

## 11. Observability

The `NFSExport` reconciler already emits `MDSSelected`, `NoEligibleMDS`, `NoAllowedClients`, `MDSUnaddressable`, `AssembleTimeout`, `ExportReady`, `AwaitingNodeForGrow`, `MDSUnhealthy`, `AwaitingNodeForTeardown`, `NothingToTearDown`, and `PhaseUnrecognized`. The Kubernetes events below are additions to that surface. The reconciler emits no Prometheus metrics for pNFS, and `design-pnfs-rwx.md` §17 specifies a set that is not implemented, so the metrics below are new infrastructure and follow that document's naming.

### Kubernetes Events

Events land on the `NFSExport`, the object a user owns and looks at, and one that outlives the MDS pod whose condition is being reported. When one storage cluster's MDS affects many exports, each export receives the event.

| Event                                                                                             | Type      | Reason                |
|---------------------------------------------------------------------------------------------------|-----------|-----------------------|
| No node offers KVM, so the MDS pod cannot be scheduled and the export is waiting                  | `Warning` | `NoKVMCapableNode`    |
| The MDS pod's state volume has not bound                                                          | `Warning` | `MDSStateUnavailable` |
| The MDS guest did not answer within the boot deadline                                             | `Warning` | `MDSBootTimeout`      |
| The MDS pod restarted and its exports were reassembled                                            | `Normal`  | `MDSResynced`         |
| The MDS pod is terminating or gone, and its address was withdrawn from the export's EndpointSlice | `Normal`  | `MDSAddressWithdrawn` |
| The MDS pod's address changed and the export's EndpointSlice was repointed                        | `Normal`  | `MDSAddressChanged`   |

### Prometheus Metrics

| Metric                                  | Labels              | Description                                                                |
|-----------------------------------------|---------------------|----------------------------------------------------------------------------|
| `simplyblock_pnfs_mds_up`               | `cluster`           | One while the cluster's MDS pod is Ready and has a link session, else zero |
| `simplyblock_pnfs_mds_boot_seconds`     | `cluster`           | Histogram of pod start to guest agent healthy                              |
| `simplyblock_pnfs_mds_boots_total`      | `cluster`, `result` | Boots by outcome (`Ready`, `Timeout`, `KVMUnavailable`)                    |
| `simplyblock_pnfs_export_resyncs_total` | `cluster`, `result` | Exports reassembled after an MDS restart, by outcome                       |

`simplyblock_pnfs_mds_up` is the alert for exports that are bound and not served, and it is the only one of the four whose absence a user feels. `simplyblock_pnfs_mds_boot_seconds` measures the unavailability window each pod restart costs, and its distribution is what decides whether a warm standby (Open Question 6) is worth building. A non-zero `simplyblock_pnfs_export_resyncs_total{result="Error"}` is a Ready export that is not served after a restart, which no phase shows.

---

## 12. Testing Strategy

The scenario matrix belongs in a companion test plan, which does not exist yet. The classes and risks below are what it has to cover.

- **Unit:** the StatefulSet and QEMU command-line builders, MDS selection and the resync decision against a fake client and a fake assembler, peer-kind authorization, and the two connection sources of the assembler.
- **Integration:** the reconcile loop against `envtest` with a fake MDS pod, covering scheduling failures, restart resync, and EndpointSlice repointing.
- **E2E:** a live cluster with KVM nodes. The assertion that matters is the one `design-pnfs-rwx.md` uses: NFS `WRITE` stays at zero while `LAYOUTGET` increments, because a guest that cannot name the device falls back to routing every byte through the MDS and still passes every functional check.
- **Where the risk concentrates:** device identity in the guest (§6.1, §6.4, §6.5), resync after a restart (§7.5), and the client-address question (§8.3). Those scenarios must not be cut when the schedule slips.

---

## 13. Migration Strategy

The guest is the only MDS host, and no release served exports from a node, so there is nothing to migrate. A cluster turns pNFS on by setting `spec.pnfs.mds` on its `SimplyblockDriver`, and its first export brings the MDS pod up.

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                     | Owner             |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------|
| 1   | **One MDS pod per storage cluster or per Kubernetes cluster.** A storage cluster per pod keeps the guest's reach to one cluster and breaks the "one MDS" simplification for a multi-cluster deployment. The choice sets the StatefulSet's name and the unit of the secret mounted in the pod | Operator team     |
| 4   | **Reconnecting after a restart with the same host NQN.** Whether the target refuses a connect while its old controller for that NQN is alive, and how long the keep-alive takes to expire it relative to the five-minute assembly deadline                                                   | SPDK/Backend team |
| 5   | **Whether NFSv4.1-only serving needs `rpc.mountd` and `rpc.statd`.** `design-pnfs-rwx.md` requires them and no code starts them                                                                                                                                                              | Operator team     |
| 6   | **Warm standby.** A second guest cannot mount the same filesystem, so a standby could only be a guest that has booted and not assembled. Whether the measured boot time (`simplyblock_pnfs_mds_boot_seconds`) justifies it                                                                   | Operator team     |
| 8   | **Whether the StatefulSet is removed when its last export is deleted.** Retaining it keeps an idle guest per storage cluster, removing it adds a boot to the next export's provisioning                                                                                                      | Operator team     |
| 9   | **How the hub exposes the live instance of a peer.** §7.5 needs the pod UID of the current session, and `Registry.Peer` returns the peer that carries it, which has to be confirmed                                                                                                          | Operator team     |
| 10  | **Pod Security and OpenShift.** Whether the operator's namespace admits a privileged pod, and which SCC the MDS ServiceAccount needs                                                                                                                                                         | Operator team     |
| 11  | **NVMe multipath in the guest.** `NVME_MULTIPATH` is built (§6.1). How the assembler's connect path treats several controller paths of one namespace in the guest                                                                                                                            | Operator team     |

---

## Appendix A: `nfsexport_types.go`

The shipped type with the two additions of §4.1. Fields marked unchanged appear with their shipped names, markers, and types, and their doc comments are shortened here. The shipped file is authoritative for the unchanged text.

```go
// NFSExportPhase is the lifecycle position of an export.
// +kubebuilder:validation:Enum=Pending;Assembling;Ready;Degraded
type NFSExportPhase string

const (
	NFSExportPhasePending    NFSExportPhase = "Pending"
	NFSExportPhaseAssembling NFSExportPhase = "Assembling"
	NFSExportPhaseReady      NFSExportPhase = "Ready"
	NFSExportPhaseDegraded   NFSExportPhase = "Degraded"
)

// NFSExportSpec is the desired state. Unchanged.
type NFSExportSpec struct {
	// VolumeRef is the CSI volume handle of the volume this export serves.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:[^:]+:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +k8s:immutable
	VolumeRef string `json:"volumeRef"`

	// ExportPath is the server-side mount point.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/var/lib/simplyblock/exports/[A-Za-z0-9._-]+$`
	// +k8s:immutable
	ExportPath string `json:"exportPath"`

	// Encrypted says the control plane stacks a crypto bdev under this volume.
	// +optional
	// +k8s:immutable
	Encrypted bool `json:"encrypted"`

	// SizeBytes is the capacity the backing volume was last grown to.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// NFSExportStatus is what the export currently is.
type NFSExportStatus struct {
	// Phase is the export's lifecycle position. Unchanged.
	// +optional
	Phase NFSExportPhase `json:"phase,omitempty"`

	// PhaseDeadline is when the current phase must be given up on. Unchanged.
	// +optional
	PhaseDeadline *metav1.Time `json:"phaseDeadline,omitempty"`

	// MDSPodName is the MDS pod serving this export. Empty until the export is
	// bound.
	// +optional
	MDSPodName string `json:"mdsPodName,omitempty"`

	// MDSNodeIP is the MDS pod's IP, which the export's EndpointSlice points
	// at. It is not what a client mounts.
	// +optional
	MDSNodeIP string `json:"mdsNodeIP,omitempty"`

	// AssembledBy is the MDS instance that last assembled this export, the pod
	// UID and runner container ID. A mismatch with the live instance means the
	// guest has rebooted and the export must be reassembled.
	// +optional
	AssembledBy string `json:"assembledBy,omitempty"`

	// ServiceAddress is the ClusterIP of the Service fronting this export, which
	// is the address a client mounts. Unchanged.
	// +optional
	ServiceAddress string `json:"serviceAddress,omitempty"`

	// AllowedClients is what goes into the exports(5) entry. Unchanged.
	// +optional
	AllowedClients []string `json:"allowedClients,omitempty"`

	// Message is a human-readable note on the current phase. Unchanged.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=nfsexp
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".spec.volumeRef"
// +kubebuilder:printcolumn:name="MDS",type=string,JSONPath=".status.mdsPodName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// NFSExport is one pNFS export: a volume, the host serving it, and the address
// clients mount.
type NFSExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NFSExportSpec   `json:"spec,omitempty"`
	Status NFSExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NFSExportList contains a list of NFSExport.
type NFSExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NFSExport `json:"items"`
}
```

---

## Appendix B: `simplyblockdriver_types.go`

The block of `SimplyblockDriver` that §4.2 describes. `DriverPNFS` is a nested struct of the `SimplyblockDriver` kind and carries no root kind of its own.

```go
// DriverPNFS configures pNFS support.
type DriverPNFS struct {
	// MDS configures the metadata server. Unset, no pNFS volume can be
	// exported.
	// +optional
	MDS *DriverPNFSMDS `json:"mds,omitempty"`
}

// DriverPNFSMDS is the pod that hosts the metadata server's guest.
type DriverPNFSMDS struct {
	// Image is the MDS image: QEMU, the guest kernel, the guest root filesystem,
	// and the runner. Unset takes the operator's own registry in the spdkcsi
	// repository, tagged with the operator's tag prefixed pnfs-mds-, so a
	// deployment that states nothing runs the image belonging to the operator
	// reconciling it.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Image string `json:"image,omitempty"`

	// Resources are the pod's requests and limits. The guest's memory and vCPU
	// count are derived from the limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector is merged with the kvm-capable label the pod always requires.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let the pod schedule onto tainted KVM nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// StateSize is the size of the guest's state disk, which holds the NFS
	// client-recovery database.
	// +kubebuilder:default="1Gi"
	// +optional
	StateSize resource.Quantity `json:"stateSize,omitempty"`

	// StateStorageClassName is the storage class of the state disk, which is
	// always a simplyblock volume (§5.3). Unset gives the storage cluster a
	// class of its own, reserved for the state disk.
	// +optional
	StateStorageClassName *string `json:"stateStorageClassName,omitempty"`
}
```
