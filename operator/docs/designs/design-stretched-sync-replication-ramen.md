# Design Document: One Storage Cluster across Two Kubernetes Clusters, driven by RamenDR

**Status:** Draft  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-10-23  
**Related:** [PR #673](https://github.com/simplyblock/simplyblock-operator/pull/673), `design-sync-replication-csi-addons.md`, whose backend verbs, method dispatch, and staging gate this design reuses, and whose one-Kubernetes-cluster topology and `VolumeReplication`-driven switchover it replaces  
**Test Plan:** [`tests/test-plan-stretched-sync-replication-ramen.md`](../tests/test-plan-stretched-sync-replication-ramen.md)

---

## Phasing Overview

| Phase       | Status                                             | Scope                                                                                                                                                                        | Sections       |
|-------------|----------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------|
| **Phase 1** | Implemented                                        | Topology: the control plane on the OCM hub, one backend cluster whose nodes live in two Kubernetes clusters, a leader operator that creates it and a follower that adopts it | §3, §4, §5, §6 |
| **Phase 2** | Implemented; site link validated live              | The site link: the fleet add-on's agent as the forwarder, the hub as the relay, the operators and CSI plugins of both sites as peers of one gRPC fabric                      | §7             |
| **Phase 3** | Implemented but for group moves; relocate run live | RamenDR in sync mode: the metro `DRPolicy`, the Ramen-driven flip without `VolumeReplication`, cluster fencing through csi-addons `NetworkFence`                             | §8, §9, §10    |
| **Phase 4** | Implemented, not yet run live                      | Leader hand-over when the leader site is lost, and the day-two operations of a stretched cluster                                                                             | §11            |

Phase 1 stands the cluster up and is testable without Ramen: two sites, one control plane, volumes served from either site by the backend's own site model. Phase 2 gives the two operators and the hub one addressable fabric, which Phase 1 can live without (it crosses nothing but `ManifestWork`) and Phase 3 and 4 need. Phase 3 is the DR behavior. Phase 4 is what happens to the operator roles when a whole site is gone for a long time.

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                          | Kind                    | Blocks                                                                                | Status                                                                                                                                                                                                                                                                                       |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | A sync cluster (`sync_replication: true`), the `site` on every node, the sync routes `V/replication/{failover,demote,status}`, `G/...`, and the storage-node monitor's site records (`sync-replication.md` §1–§3, §5) | Control plane (`sbcli`) | Everything                                                                            | On the `sync-replication` branch, not released. The v2 DTOs must also expose `sync_replication`, `lost_site`, and `lost_site_state` on a cluster and `site` on a node, which the remote nodes and `status.stretched` read; that change is on sbcli branch `stretched-dto-fields`, not merged |
| P0-2 | A site-level fence route, `POST /clusters/{id}/sites/{site}/fence` and its `unfence` and `status` siblings (§10.3, §12), and `site` on the volume create body (§9.5)                                                  | Control plane (`sbcli`) | Phase 3 fencing                                                                       | On sbcli branch `stretched-dto-fields`, not merged: the site fence, unfence, and status routes, `site` on the create body, `leader_site` on the cluster record, and the reachability read by node or by address. The `GET /clusters/{id}/sites` listing of §12 is not built                  |
| P0-3 | The management network routes between the hub and every storage node of both sites, and the storage network routes between the two sites (§3.3)                                                                       | Infrastructure          | Everything                                                                            | A property of the deployment, verified by the preflight of §5.4                                                                                                                                                                                                                              |
| P0-4 | An OCM hub with both sites registered as `ManagedCluster`s, `ramen-hub` on the hub and `ramen-dr-cluster` on each site, the csi-addons manager with the `NetworkFence` CRD on each site                               | Ecosystem               | Phase 3                                                                               | What `regression_test/22` stands up today, minus the `NetworkFence` CRD, which the chart does not yet ship                                                                                                                                                                                   |
| P0-5 | The fleet component (`fleet/`): `fleet-manager` on the hub, the add-on template, `FleetMember`, `ClusterDeployment` ([`design-management-hub.md`](design-management-hub.md))                                          | This repository         | Phase 2 (the relay and the agent); classes and pools through `StorageClassDeployment` | Kinds, add-on, and the agent exist. The agent is in the add-on template with its `KubeClient` registration, and the fleet manager runs the relay (§7.2, §7.3). Phase 1 needs OCM only, as `StorageSiteDeployment` does                                                                       |
| P0-6 | An S3 store both sites reach, for Ramen's PV and PVC metadata (`DRCluster.spec.s3ProfileName`)                                                                                                                        | Infrastructure          | Phase 3                                                                               | `regression_test/22/05b-install-s3-store.sh`'s MinIO serves it in the lab                                                                                                                                                                                                                    |

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [The Hub Is the Arbiter](#4-the-hub-is-the-arbiter)
5. [Leader and Follower Operators](#5-leader-and-follower-operators)
6. [What Crosses Between Hub and Sites](#6-what-crosses-between-hub-and-sites)
7. [The Site Link](#7-the-site-link)
8. [RamenDR in Sync Mode](#8-ramendr-in-sync-mode)
9. [The Flip: Placement-Driven, Without VolumeReplication](#9-the-flip-placement-driven-without-volumereplication)
10. [Fencing a Lost Site](#10-fencing-a-lost-site)
11. [Leader Hand-Over and Day Two](#11-leader-hand-over-and-day-two)
12. [Backend API Requirements](#12-backend-api-requirements)
13. [Failure Modes](#13-failure-modes)
14. [Observability](#14-observability)
15. [Testing Strategy](#15-testing-strategy)
16. [Open Questions](#16-open-questions)

Appendices:

- [Appendix A: `stretchedcluster_types.go`](#appendix-a-stretchedcluster_typesgo)
- [Appendix B: `storagecluster_types.go` additions](#appendix-b-storagecluster_typesgo-additions)
- [Appendix C: `site_link.proto`](#appendix-c-site_linkproto)
- [Appendix D: Lab topology](#appendix-d-lab-topology)

---

## Overview

A synchronously replicated simplyblock cluster spans exactly two sites, with one control plane and one FoundationDB that must reach both (`sync-replication.md` §1, §2). PR #673 placed those two sites inside one Kubernetes cluster stretched over two zones, and let pod scheduling move a volume's leadership by patching a csi-addons `VolumeReplication`. This design keeps the backend and the staging gate and changes where the sites are: **each site is its own Kubernetes cluster**, both registered with an Open Cluster Management hub, and **the control plane runs on that hub**, which makes the hub the arbiter the backend's model already assumes: one management node that reaches both sites and judges which one is gone.

Three things follow from that split, and they are the design.

- **Two operators, one cluster.** Each site runs the operator exactly as a standalone installation does. The backend cluster is created once, by the operator of the leader site, and adopted by the operator of the follower site, which then adds its own site's nodes. The roles are decided on the hub and delivered as payloads; no operator discovers the other.
- **One fabric through the hub.** The CSI plugins already dial their operator over a reverse-dialed gRPC link. The fleet add-on's agent extends that link out of the site: it dials a relay the hub runs, and the hub lets the two sites' operators and plugins reach each other as peers of one registry. Nothing listens on a site, nothing needs a route between sites that OCM does not already have.
- **RamenDR flips placement, the driver flips storage.** Ramen's sync mode creates no `VolumeReplication` (§8), so the switchover cannot ride on one. The node plugin learns at stage time where the volume is led and asks its site's operator to move it; the operator demotes the old site and promotes this one against the control plane, and staging stays gated on the promote exactly as before. A lost site is fenced through csi-addons `NetworkFence`, which the driver maps onto a site-level fence the control plane runs as one unit.

---

## 1. Background

Three designs meet here.

**The backend's two-site model** (`sbcli/docs/sync-replication.md`) gives every node a site, pairs every logical volume store with a remote triplet on the other site, serves a volume only from the site its store is led from, and moves that leadership with a planned promote (both sites up) or a forced one (a site proven lost). It assumes a single management node that reaches both sites: its probes and the surviving site's votes are the only witness, there is no quorum of its own, and the loss of the management node stops monitoring and fail-over while the data plane keeps serving (§2, "Control plane").

**The sync design on PR #673** built the csi-addons verbs (`PromoteVolume` → `failover?site=`, `DemoteVolume` → `demote?site=`, the `412` escalation to a forced promote), the operator knobs (`enableSyncReplication`, `site` on node groups and nodes), and a staging gate that never attaches an un-promoted volume. It also committed to one Kubernetes cluster over two zones and to the node plugin patching a `VolumeReplication` to signal the site, with the csi-addons reconciler issuing the promote.

**The management hub design** ([`design-management-hub.md`](design-management-hub.md)) put one control plane beside an OCM hub and several Kubernetes clusters under it, each running the unchanged operator, with a `fleet-manager` on the hub composing `ManifestWork` payloads and a `fleet-agent` add-on reporting what only a member knows. Its rule that no kind exists on both sides, and its layout of three namespaces, are inherited unchanged.

What changed is the requirement: the two sites are two Kubernetes clusters, the switchover is RamenDR's to drive, and the control plane lives with the hub.

---

## 2. Goals and Non-Goals

### Goals

- One backend cluster whose storage nodes run in two Kubernetes clusters, one site each, with the control plane and FoundationDB on the OCM hub.
- The operator on each site is the standalone operator. The only additions are a role on its `StorageCluster` and a site on its nodes; a site that loses its hub keeps reconciling what it has.
- The leader site's operator creates the backend cluster; the follower site's operator adopts it and adds its nodes. Both are driven from the hub, from one object that names the two members and the leader.
- Both operators, both drivers and the hub share one gRPC fabric, reached through the OCM connection each site already holds. The existing link package is the fabric; a relay on the hub is the only new element.
- RamenDR in sync mode is the orchestrator of placement, fencing, and recovery, and every storage move it implies is carried out by the driver and the operator against the control plane, with the staging gate of PR #673 intact.
- The surviving site can fail over and fence without the leader site: no DR action depends on the leader role.
- A PVC created on a Kubernetes cluster gets a volume whose home site is that cluster's site, so the first pod to use it stages without a move (§9.5).
- A relocate or a failover removes the claim on one site and recreates it on the other, and the recreated claim inherits the backend volume: a move never deletes the volume and never creates a second one (§9.6).

### Non-Goals

- Replicating the control plane or FoundationDB across sites, or running without the hub. The backend's single-management-node assumption stands; the hub is that node, and its availability is a deployment concern (§13, Open Question 1).
- Three or more sites, or a site that is not a whole Kubernetes cluster.
- Async replication between stretched clusters. A stretched cluster is one cluster; cross-cluster async DR is [`design-csi-addons-replication.md`](design-csi-addons-replication.md) and applies to it as to any cluster.
- Carrying storage-plane traffic through the OCM tunnel. NVMe-oF, the journal links and the control plane's probes of storage nodes need a routed network (§3.3); the tunnel carries control traffic only.
- Running the two sites' operators as one leader-elected deployment. They are two installations with one coordination protocol, not replicas of one controller.

---

## 3. Architecture Overview

```
                                    HUB CLUSTER (arbiter)
 ┌──────────────────────────────────────────────────────────────────────────────────┐
 │  cluster-manager (OCM)        ramen-hub          fleet-manager                   │
 │   registration · work         DRPolicy (sync)     StretchedCluster → two          │
 │   addon-manager               DRPC · DRCluster    ClusterDeployments (§5)        │
 │                                                   site-link relay (§7)          │
 ├──────────────────────────────────────────────────────────────────────────────────┤
 │  simplyblock-operator (standalone) · ControlPlane source.local                   │
 │   FoundationDB · management API · tasks runners · storage-node monitor           │
 │   = the backend's "one management node that reaches both sites"                  │
 └────────┬───────────────────────────────┬──────────────────────────┬──────────────┘
          │ ManifestWork (payloads)        │ site link (outbound,     │ HTTPS, management API
          │ add-on: fleet-agent            │ agent → relay, yamux)    │ (operators, drivers,
          ▼                                ▼                          │  hub reads)
 ┌────────────────────────────┐   ┌────────────────────────────┐      │
 │ SITE A (ManagedCluster)    │   │ SITE B (ManagedCluster)    │      │
 │ klusterlet · ramen-dr      │   │ klusterlet · ramen-dr      │      │
 │ fleet-agent + link fwd     │   │ fleet-agent + link fwd     │      │
 │ simplyblock-operator       │   │ simplyblock-operator       │◄─────┘
 │  StorageCluster {leader,a} │   │  StorageCluster {follower,b}│
 │  StorageNode ×N (site a)   │   │  StorageNode ×M (site b)   │
 │  + ×M remote (site b)      │   │  + ×N remote (site a)      │
 │ CSI driver (controller,    │   │ CSI driver (controller,    │
 │  nodes) · csi-addons mgr   │   │  nodes) · csi-addons mgr   │
 │ storage-node pods (SPDK)   │   │ storage-node pods (SPDK)   │
 └────────────┬───────────────┘   └───────────────┬────────────┘
              │     storage network: NVMe-oF, journal links,   │
              └────────── remote triplets, both directions ────┘
                       management network: hub ↔ every storage node
```

### 3.1 One backend cluster, two Kubernetes clusters

The backend cluster is one record in one FoundationDB, created with `sync_replication: true`. Its storage nodes are the workers of two Kubernetes clusters, every node of site A's cluster with `site: a` and every node of site B's with `site: b`. A site is the whole Kubernetes cluster: there is no zone to read, the site is a property of the installation (§5.3). Everything the backend does per site, the remote triplets, the journal copies, the led site of each store, the site records of the monitor, is unchanged.

### 3.2 The control plane on the hub

The hub cluster runs the standalone operator with a `ControlPlane` of `source.local`, so FoundationDB, the management API and the background services are installed and owned there as on any single-cluster deployment. The two sites run the operator with a `ControlPlane` of `source.managed` whose endpoint is the hub's management API ([`crd-redesign/design-controlplane.md`](crd-redesign/design-controlplane.md) §5.2). Nothing about the control plane is site-aware: the backend already fronts several Kubernetes clusters from one FoundationDB, and the operator already assumes a managed control plane is shared.

The placement is the point. The backend judges a site lost from the management node's own probes and the other site's votes, and it needs that node outside both sites to judge either of them. A control plane inside site A would make A's loss the loss of all monitoring and fail-over, which is the one failure the two-site cluster exists to survive. The hub is the natural third place: it is where the DR orchestrator and the fleet already live, and it is the one cluster both sites already have a connection to.

### 3.3 Networks

Two networks are required, and neither goes through OCM.

| Traffic                                                              | Between                                     | Needs                                        |
|----------------------------------------------------------------------|---------------------------------------------|----------------------------------------------|
| Storage node API, SPDK RPC proxy, probes by the storage-node monitor | Hub ↔ every storage node of both sites      | Routed management network                    |
| NVMe-oF to the remote triplet, journal links, hublvol within a site  | Site A storage nodes ↔ site B storage nodes | Routed storage network                       |
| Management API from operators, drivers, the hub's own reads          | Sites → hub                                 | Routed, or the site link's HTTP relay (§7.5) |
| `ManifestWork`, add-on registration, agent report, the site link     | Sites → hub                                 | What OCM already has: member → hub           |
| Ramen's PV and PVC metadata                                          | Sites → S3                                  | Routed to the store                          |

The first two are hard requirements the preflight checks (§5.4). Synchronous writes cross the second on every acknowledgment, so site distance is bounded by the latency the application accepts, as on any stretched cluster.

### 3.4 What is new, and what is reused

| Element                                                     | Status                                                                                          |
|-------------------------------------------------------------|-------------------------------------------------------------------------------------------------|
| Backend sync model, routes, site records                    | Reused (P0-1)                                                                                   |
| `enableSyncReplication`, `site` on nodes, sync task runners | Reused from PR #673 §9.4–§9.6                                                                   |
| Sync verbs and `412` escalation in the driver               | Reused from PR #673 §7, called by the operator instead of by csi-addons (§9)                    |
| Staging gate                                                | Reused from PR #673 §6.4, keyed on the backend's status instead of a `VolumeReplication` (§9.3) |
| `ControlPlane` local and managed sources                    | Reused                                                                                          |
| Cluster adoption in the `StorageCluster` controller         | Reused ([`crd-redesign/design-storagecluster.md`](crd-redesign/design-storagecluster.md) §4.3)  |
| Fleet kinds, `ManifestWork` delivery, the add-on            | Reused (P0-5)                                                                                   |
| The link package (`atlas-lib/link`)                         | Reused as is; the relay and the forwarder are new users of it (§7)                              |
| `StretchedCluster` hub-side kind                            | **New** (Appendix A)                                                                            |
| `StorageCluster.spec.stretched`                             | **New** (Appendix B)                                                                            |
| Site-link relay on the hub, forwarder in the agent          | **New** (§7)                                                                                    |
| `SwitchoverService` in the operator                         | **New** (§9)                                                                                    |
| `NetworkFence` in the driver, site fence route              | **New** (§10, P0-2)                                                                             |
| Site on the volume create body, sent by the driver          | **New** (§9.5, P0-2)                                                                            |
| Metro `DRPolicy` and class conventions                      | **New** (§8)                                                                                    |

---

## 4. The Hub Is the Arbiter

The backend's liveness proof for a forced promote is: the node is not in a running state, the management node gets no answer from its SPDK, and no answering peer on the surviving site still reports the node's journal link enabled (`sync-replication.md` §5, "Disaster fail-over"). The management node is the only party that asks all three questions. Placing it on the hub gives it two properties the design depends on.

**It is outside both sites.** The hub is not in either site's failure domain, so a site's loss does not take the judge with it, and a partition between the sites does not partition the judge from both. The backend's stated limitations still apply, and they are restated for this topology in §13: a hub cut off from one site while the other site is healthy records that site; a hub cut off from both judges nothing.

**It is the OCM hub.** Ramen's hub operator, the fleet manager, the relay of §7 and the control plane all sit in the one place both sites keep an outbound connection to. The sites need no route to each other for control traffic, only to the hub, and that route is the one OCM registration already required.

The hub is not replicated, which is the backend's choice (Non-Goals). What this design adds is a clear statement of what the hub's loss costs (§13) and a preflight that refuses to create the cluster unless the hub reaches every node (§5.4). A highly available hub cluster, three nodes with FoundationDB on its own volumes, is the deployment answer, and a third-site hub is the recommended one (Open Question 1).

---

## 5. Leader and Follower Operators

Two operators manage one backend cluster. Without a rule, both would create it, both would run cluster-scoped operations, and the `StorageCluster` controller's adoption path, which exists for a cluster the operator did not create, would be reached by accident rather than design. The rule is a role, decided on the hub and carried on each site's `StorageCluster`.

### 5.1 The role is decided on the hub

The hub holds one `StretchedCluster` (Appendix A), a hub-side kind in `storage.simplyblock.io/v1alpha2` beside `StorageSiteDeployment`, naming two `ManagedCluster`s, the site each represents, and which one leads:

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StretchedCluster
metadata:
  name: production
  namespace: simplyblock
spec:
  clusterName: production
  sites:
    - cluster: site-a       # ManagedCluster
      site: a
      role: Leader
    - cluster: site-b
      site: b
      role: Follower
      discover:             # which workers the site's discovery inspects
        nodeSelector: {node-role.simplyblock.io/storage: ""}
  sizing:                   # what no probe can read, written onto both drafts
    stripe: {dataChunks: 2, parityChunks: 2}
    maxSubsystemCount: 64
  images:                   # what both sites' storage nodes run, written onto both drafts
    spdk: {image: public.ecr.aws/simply-block/ultra:main-sync-replica-test-latest}
  approved: false           # the one-way review gate, flipped here, delivered to both sites
```

The hub's operator reconciles it (`StretchedClusterReconciler`) as two `StorageSiteDeployment` flows joined by a sequence. Nothing is composed from nothing: the sites run the operator chart's `managed` profile, which raises no discovery of its own because "the cluster that manages this one raises the run," and the hub is that cluster. Per site, the reconciler never holds a site kubeconfig, every write is a `ManifestWork` in the site's hub namespace, and every read is a `ManagedClusterView` there:

1. The work carries an `OperatorOps` discovery, scoped by `sites[].discover`. The site inspects its workers and writes a `ClusterDeploymentConfig` in Draft, in the operator's namespace there.
2. Once the draft names nodes, the work carries a server-side apply of the sizing onto the draft's cluster template: the shared `spec.sizing`, `enableSyncReplication: true`, and the fields the role decides. A node group the discovery wrote without a site takes the cluster's `stretched.site` when the site's operator expands the draft, so the hub never applies into the draft's `nodeSets` list, which the discovery owns.
3. When `spec.approved` is flipped on the hub, the work carries the draft's approval. The draft expands into the site's `StorageCluster` and `StorageNode`s, and the view projects them back.

The two drafts differ in exactly the fields the role decides:

| Field on the delivered `ClusterDeploymentConfig` | Leader                                                 | Follower                                               |
|--------------------------------------------------|--------------------------------------------------------|--------------------------------------------------------|
| `spec.cluster.name`                              | `production`                                           | `production`                                           |
| `spec.cluster.enableSyncReplication`             | `true`                                                 | `true` (must match; the backend refuses otherwise)     |
| `spec.cluster.stretched.site` / `.role`          | `a` / `Leader`                                         | `b` / `Follower`                                       |
| `spec.clusterRef`                                | unset: create the `StorageCluster`                     | unset: create the `StorageCluster`, which adopts       |
| `spec.nodeSets`                                  | What site A's discovery found, sites defaulting to `a` | What site B's discovery found, sites defaulting to `b` |

Both discoveries run at once, so both drafts are reviewable together, and one approval covers both. The follower's approval is withheld until the leader's `StorageCluster`, read through its view, carries the backend cluster's UUID, so the follower never races the create. The same pass delivers `simplyblock-{name}-upgrade`, the backend cluster's UUID and secret, which the hub reads from its own control plane. Neither draft names a `clusterRef`: on a site, `clusterRef` grows a `StorageCluster` that already exists there, and the follower has none until its draft expands. `StretchedCluster.status.phase` walks `Discovering → Drafted → LeaderCreating → FollowerJoining → Ready`, and `status.sites[]` carries each draft's phase and node names, as `StorageSiteDeployment.status` does for one site.

### 5.2 What each operator does

**The leader** runs the ordinary creation path: `Claiming → CheckingControlPlane → ResolvingConfig → Creating → Persisting` ([`crd-redesign/design-storagecluster.md`](crd-redesign/design-storagecluster.md) §4), with `sync_replication: true` in the create body, then expands its `StorageNode`s with `site: a`. It activates the cluster once its own site's nodes are added, exactly as a single-site cluster does, and the activation holds until the backend's two-site rule is met: each site needs `ndcs + npcs` online nodes on enough hosts (`sync-replication.md` §2), so the first activation succeeds only after the follower's nodes are in. The leader's deployment holds its activation step meanwhile and reports `WaitingForPeerSite`: it raises the `Activate` operation only once the peer site's remote `StorageNode`s (§6.2) show `ndcs + npcs` online. The follower raises no activation at all (`ActivationLeftToLeader`).

**The follower** reaches the controller's existing adoption path through the credentials the hub delivers: its draft expands into a `StorageCluster` of the same name with `stretched.role: Follower`, and the `simplyblock-{name}-upgrade` Secret routes `CheckingControlPlane` to `Adopting`, the path a Helm migration takes. The control plane's listing carries no cluster secret, so an adoption by name would hold at `Persisting`, and every other exit of the creation path posts a create. A follower therefore waits in `CheckingControlPlane` (`PeerClusterMissing`), outside the step deadline, until the Secret arrives. It refuses a cluster that is not a sync cluster (`SyncReplicationMismatch`), and when it is deleted it lets go of the backend cluster instead of deleting it. The follower then expands and adds its `StorageNode`s with `site: b`. From here both operators read the same cluster from the same stream and keep their own `StorageCluster` status current.

**What the follower refuses.** Cluster-scoped actions have one owner. A `StorageClusterOps` on a follower's `StorageCluster` is refused at admission for every action but those that are site-local by nature; a rolling restart, an expansion, a shutdown or an activation runs on the leader and is delivered there. `StorageNodeOps` on a site's own nodes run on that site, because the backend serializes restarts cluster-wide on its own and the node's workload is local. The split is the same one [`design-management-hub.md`](design-management-hub.md) §6 draws for what a member owns.

| Scope                                                                                  | Leader                                                                                                         | Follower                 |
|----------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------|--------------------------|
| Create the backend cluster, activate it, cluster-wide `StorageClusterOps`              | Yes                                                                                                            | Refused (`FollowerSite`) |
| Add, restart, remove, migrate the nodes of its own site                                | Yes                                                                                                            | Yes                      |
| The storage-node workload (DaemonSet, Services, certificates) of its own site          | Yes                                                                                                            | Yes                      |
| A remote `StorageNode` (the other site's node, §6.2): status only, no workload, no ops | Yes                                                                                                            | Yes                      |
| Pools, StorageClasses, backup policies                                                 | Either site may author them; the backend objects are cluster-wide and the Kubernetes objects are per site (§6) |                          |
| DR actions: switchover, fence, unfence (§9, §10)                                       | Either site, whichever Ramen addresses                                                                         |                          |

### 5.3 The site is the installation's

PR #673 read a node's site from its zone label. On two Kubernetes clusters the site is not a label a scheduler consults; it is which cluster the node is in. The site is therefore carried once per installation and fanned out:

- `StorageCluster.spec.stretched.site` (Appendix B) is the site of every node this `StorageCluster` adds. The `StorageNode` reconciler sets `site` in the node-add body from it; a `NodeGroup` in the delivered config carries the same value, and the webhook refuses a group whose site differs from the cluster's.
- The driver's cluster secret entry, which the `StorageCluster` controller composes, gains `sync` and `site` beside `local`, so a node plugin knows its site without a `Node` read (§9.4). No `SimplyblockDriver` field carries it: a spec field written by a controller would break the rule that spec is the user's.

A zone label may still partition a site into failure domains; it no longer names the site.

### 5.4 Preflight

Before the leader posts the create, and before the follower adopts the cluster and adds its nodes, the operator checks what the backend will otherwise fail on later and less legibly:

- the control plane's `GET /_meta/ready` answers;
- for the follower, the control plane reaches the management addresses of the workers this site's `StorageNode`s run on: one run of the storage-node monitor's own check against them, `GET /clusters/{id}/reachability?address=...` (P0-2). The read takes addresses rather than nodes because the workers are not nodes yet. A control plane without the probe, which answers `404` or ignores the addresses, does not hold the follower, and an unreachable worker then surfaces when its node add fails. The leader has no cluster to ask before its create, so its workers are not probed;
- for the follower, the cluster exists, is a sync cluster, does not name the follower's site as its leader (`RoleConflict`, §11.1), and has no node yet on the follower's site from another installation;
- `enableSyncReplication` matches on both configs, which the reconciler guarantees and a CEL rule on `StorageCluster` re-checks: `stretched` is refused without `enableSyncReplication: true`, so both sites' configs carry it, and the follower's check above confirms the backend cluster agrees.

A failed preflight holds the `StorageCluster` in `CheckingControlPlane` with the reason, which the hub reads through the site's `ManagedClusterView` into `StretchedCluster.status.sites[]`, appended to the site's message. The follower's checks run in its wait for the leader, before `Adopting`, so a hold has no deadline: a follower handed the wrong cluster waits with the reason rather than failing an adoption it never began. A node on the follower's site counts as another installation's when its UUID is on none of the site's own `StorageNode`s, so a follower whose `StorageCluster` is recreated over nodes it added before adopts again.

A worker the probe does not reach holds the follower with `WorkerUnreachable`, naming the worker and its address.

---

## 6. What Crosses Between Hub and Sites

The table of [`design-management-hub.md`](design-management-hub.md) §6 holds, with the stretched additions:

| Kind                                          | Hub side                                                | Site side                                                                                      | Crosses as                                                                                                              |
|-----------------------------------------------|---------------------------------------------------------|------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------|
| `StretchedCluster`                            | Written by a person, reconciled by the hub's operator   | Nothing                                                                                        | Two `ManifestWork`s down, two `ManagedClusterView`s up                                                                  |
| `ClusterDeploymentConfig` ×2                  | Sized and approved by the `StretchedCluster` reconciler | Written by the site's discovery, in Draft                                                      | The discovery, the sizing, and the approval go down in the `ManifestWork`; the draft and its expansion come up in views |
| `StorageCluster`                              | Nothing                                                 | One per site, same name, same backend UUID                                                     | Nothing. State comes from the control plane                                                                             |
| `StorageNode`                                 | Nothing                                                 | One per node of the cluster: managed for this site's nodes, remote for the other site's (§6.2) | Nothing. Both come from the control plane                                                                               |
| `StoragePool`, `StorageClass`                 | `StorageClassDeployment` to both sites                  | One object per site                                                                            | Down, to both members, so names match (§8.2)                                                                            |
| `DRPolicy`, `DRCluster`, `DRPlacementControl` | Ramen's, on the hub                                     | VRG, `NetworkFence` via Ramen's `ManifestWork`                                                 | Ramen's own path                                                                                                        |
| Credentials for the managed control plane     | A Secret the fleet manager composes                     | `ControlPlane.spec.source.managed.credentialsSecretRef`                                        | Down, in the `DriverDeployment` payload                                                                                 |
| Agent report, link block                      | `FleetMember.status`                                    | The agent writes it to its OCM namespace                                                       | Up                                                                                                                      |

Two Kubernetes objects now exist for one backend object, the `StorageCluster` on each site, and the rule that no kind exists on both the hub and a site still holds: both copies are site-side. They are not twins of each other either; each reconciles the same control-plane state into its own status, and the only field they must agree on, the backend UUID, is a fact the control plane holds.

### 6.1 Who acts on an event

Three operators watch the same backend cluster, and two of them watch Kubernetes clusters that hold the same workloads at different times. The rule is the one §5.2 draws for ownership: **every operator reads every event, and an operator acts only on an event about an object it owns.** Reading is free, because the control plane's streams are scoped by cluster UUID and all three operators open the same scope. Acting is what the role and the site decide.

**Control-plane events.** The operator consumes the control plane's Server-Sent Events through its informer (`operator/internal/cpinformer`, [`design-sse-push-notifications.md`](design-sse-push-notifications.md)): one stream per resource and scope, a full snapshot on every connect, a reconcile trigger per change, and the subscription running on the leader replica of each operator. On a stretched cluster the three operators open the same cluster scope and receive identical events. The informer's ID-to-CR mapping finds a `StorageNode` for every node on every site (§6.2), so the filtering is the CR's own: a remote node's reconcile projects status and does nothing else. The rest is the role.

| Stream          | Event                                                                         | Leader site operator                                                                                                                             | Follower site operator                                                      | Hub operator (`StretchedCluster`)                                     |
|-----------------|-------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------|-----------------------------------------------------------------------|
| `clusters`      | `status` changes (active, suspended, degraded)                                | Updates its `StorageCluster` status; drives the activation hold (§5.2)                                                                           | Updates its `StorageCluster` status; acts on nothing                        | Rolls up into `status.phase`                                          |
| `clusters`      | `lost_site`, `lost_site_state` (`draining`, `down`, `fencing`, `done`)        | Status and event `SiteRecordChanged`; no action, the record is the monitor's                                                                     | Same                                                                        | `status.sites[].siteRecord`; the one place a console reads both sites |
| `clusters`      | `site_activation_*` (a site returning)                                        | Status only; the activation is the backend's                                                                                                     | Same                                                                        | Status                                                                |
| `clusters`      | `leader_site` names another site or none                                      | The site acting as leader writes its own site to it (§11.1), event `LeaderSiteRecorded`                                                          | Nothing; at adoption, refuses a record naming its own site (`RoleConflict`) | Nothing                                                               |
| `storage-nodes` | A node of **this** site changes (`online`, `offline`, `unreachable`, restart) | Reconciles that `StorageNode`: workload, status, the node's ops                                                                                  | Same, for its own site's nodes                                              | Counts nodes per site into `status.sites[]`                           |
| `storage-nodes` | A node of the **other** site changes                                          | Reconciles the remote `StorageNode`: status only, no workload, no ops; `status.stretched.peerNodesOnline` from the same pass                     | Same                                                                        | As above                                                              |
| `devices`       | A device of a node of this site                                               | Reconciles that `StorageDevice`: status, the device's ops                                                                                        | Same, own site                                                              | Nothing                                                               |
| `devices`       | A device of a node of the other site                                          | Reconciles the remote `StorageDevice` under the remote node: status only, no ops (§6.2)                                                          | Same                                                                        | Nothing                                                               |
| `tasks`         | A task on a node of this site (restart, migration)                            | Drives the `StorageNodeOps` that created it                                                                                                      | Same, own site                                                              | Nothing                                                               |
| `tasks`         | A cluster-wide task (activation, expansion, rolling restart)                  | Drives the `StorageClusterOps` that created it; the rolling restart walks both sites' nodes, the other site's pods through `SiteService` (§11.3) | Watches for status; cannot have created it (§5.2)                           | Nothing                                                               |
| `volumes`       | `sync_active_site`, `lvs_active_site` change (a move started or completed)    | `SwitchoverService` resolves a pending `Move` from the cache instead of polling (§9.3); event `VolumeMoved` on the PV if the PV is here          | Same                                                                        | Nothing                                                               |
| `volumes`       | `created`, `deleted`                                                          | Pool capacity and `StoragePool` status, as today                                                                                                 | Same                                                                        | Nothing                                                               |
| `storage-pools` | Any                                                                           | Reconciles its `StoragePool` objects                                                                                                             | Same; both sites may hold a `StoragePool` for the same backend pool (§5.2)  | Nothing                                                               |
| `backups`       | Any                                                                           | The site whose `StorageBackupPolicy` owns the backup                                                                                             | Same                                                                        | Nothing                                                               |

Two consequences. A stream that reaches one site and not the other, because the management network to that site is cut (§13), stalls only that site's reconcilers; the other site and the hub keep acting on what they own. And a follower that loses its stream still never acts beyond its site, so a stale cache on the follower cannot trigger a cluster-scoped call.

**Kubernetes and OCM events.** Each site's operator watches its own API server; the hub's operator watches the hub's. No operator watches another cluster's objects directly: the hub reads sites through `ManagedClusterView`, and sites learn hub intent through delivered objects.

| Object                                                   | Where it changes | Who acts                                                                                                                                                                |
|----------------------------------------------------------|------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `StretchedCluster`                                       | Hub              | Hub operator: composes and sequences the two works, re-delivers on a role swap                                                                                          |
| `ManagedClusterView` of a site's `StorageCluster`        | Hub              | Hub operator: `status.sites[]`, the follower's work once the leader's view carries a UUID                                                                               |
| `ManifestWork` status (applied, failed)                  | Hub              | Hub operator: `Failed` with the site's reason                                                                                                                           |
| `ClusterDeploymentConfig` draft                          | Site             | That site's operator: written by its discovery, expanded on approval into its `StorageCluster` and `StorageNode`s with the role and site (§5.1)                         |
| `StorageCluster`, `StorageNode`, `*Ops`                  | Site             | That site's operator; cluster-scoped ops only when it is the leader (§5.2); ops on a remote `StorageNode` refused (§6.2)                                                |
| `Node` readiness, worker labels                          | Site             | That site's operator labels its workers with its nodes' UUIDs; the other site never labels them (§9.5)                                                                  |
| `PersistentVolumeClaim`, `PersistentVolume`              | Site             | Nobody in the operator. The driver provisions; Ramen retains and restores (§9.6); the node plugin moves at stage                                                        |
| `DRPlacementControl`, `DRCluster`                        | Hub              | Ramen hub. The hub operator reads `clusterFence` into `status.sites[]` only                                                                                             |
| `VolumeReplicationGroup`                                 | Site             | Ramen's cluster operator. The site operator does not watch it: sync mode creates no `VolumeReplication` (§8.1)                                                          |
| `NetworkFence`                                           | Surviving site   | The csi-addons controller calls the driver; the driver calls the site fence (§10). The operator records `SiteFenceChanged` from the cluster stream, not from the object |
| `ManagedCluster` availability (a site's klusterlet gone) | Hub              | Hub operator: `status.sites[].reachable`, event `SiteUnreachable`; no action, because reachability of the API server is not loss of the site (§4)                       |
| Link session up or down                                  | Site and hub     | Each side on its own session: `SiteLinkChanged` on the site, `status.sites[].linkUp` on the hub                                                                         |

The only object two operators could both act on is a `StorageCluster` event about the cluster itself, and the role settles it: the follower records, the leader acts. Nothing in the DR path, the fence, the move, or the site's return, is acted on by an operator from an event. The control plane acts on its own monitor, the driver on csi-addons' call, the node plugin on the stage, and the operators record the outcome.

### 6.2 Every site lists every node

`kubectl get storagenode` and `kubectl get storagedevice` on either site list every node and every device of the cluster. A site's operator holds a `StorageNode` for each of its own nodes, as today, and a **remote** `StorageNode` for each node of the other site, created from the control plane's node stream (§6.1) and marked `spec.stretched.remote: true` (Appendix B). A remote record is listed only from an authoritative listing, the stream's synced snapshot or the control plane's own, so a cold cache never removes one.

A remote node is a record, not a workload, and the reconciler treats it as one. Its devices follow it: `StorageDevice` is already a discovered kind, created from the per-node device stream and owned by its node through `spec.nodeRef`, so the operator opens the device scope for a remote node exactly as for a managed one, and the devices it discovers are remote because their node is.

| Aspect           | Managed `StorageNode` (this site)                                  | Remote `StorageNode` (the other site)                                                                                         |
|------------------|--------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------|
| Created by       | The delivered `ClusterDeploymentConfig`, expanded by this operator | This operator, from the node stream, when a node with the peer site first appears                                             |
| Workload         | DaemonSet, Services, certificates on this cluster's worker         | None. No worker of this cluster is named                                                                                      |
| `status`         | From the control plane and the workload                            | From the control plane only: node status, site, led stores                                                                    |
| `StorageNodeOps` | Admitted                                                           | Refused at admission with `RemoteNode`, naming the site whose operator owns it                                                |
| `StorageDevice`s | One per device, from the node's device stream                      | One per device, from the same stream, owned by the remote node and gone with it; `StorageDeviceOps` refused with `RemoteNode` |
| Delete           | Removes the node from the cluster (the existing ops path)          | Removes nothing. The CR is recreated from the next snapshot; a user who wants it gone removes the node on its own site        |
| Disappears when  | The node is removed from the cluster                               | The node is removed from the cluster, detected by the stream's delete event or the reconnect relist                           |
| Print columns    | `Site`, `Remote` unset, status                                     | `Site`, `Remote=true`, status                                                                                                 |

Two writers write the same node, one managed CR on its own site and one remote CR on the other, and they never disagree for long: both are projections of the same control-plane record, and neither copies from the other. What keeps them apart is the name. The node DTO does not carry the name of the managed record on its own site, so a remote CR is named `{cluster}-{site}-{id[:8]}`: deterministic, so a recreate finds the same name, and unable to collide with a managed node of this site, whose names come from its workers.

The whole cluster is therefore visible in four places:

| Where         | Command                             | Shows                                                                                           |
|---------------|-------------------------------------|-------------------------------------------------------------------------------------------------|
| Either site   | `kubectl get storagenode -o wide`   | Every node of both sites, `Site` and `Remote` columns telling this site's from the other's      |
| Either site   | `kubectl get storagedevice -o wide` | Every device of both sites, `Node` naming the node and `Site` and `Remote` carried from it      |
| Either site   | `kubectl get storagecluster`        | Print columns `SITE`, `ROLE`, `PEER-SITE`, `PEER-ONLINE`, `SITE-RECORD` from `status.stretched` |
| Hub           | `kubectl get stretchedcluster`      | Both sites: role, node count, site record, link state, from `status.sites[]`                    |
| Control plane | `sbctl storage-node list`           | Every node with its `site`, the record both sites project                                       |

---

## 7. The Site Link

Phase 1 needs no connection between the sites: payloads go down through `ManifestWork`, state comes up through the control plane and the agent's report. Phases 3 and 4 need the two operators and the two drivers to reach each other: a surviving site asks the leader's role to move, the leader reads a follower's node fabric, and a flip issued on one site must be visible to the operator of the other. The sites have no route to each other that OCM promises, and nothing on a site may listen for the other. What they both have is an outbound connection to the hub.

### 7.1 The existing link is the tunnel

`atlas-lib/link` already solves the same problem one level down: the CSI plugins dial the operator and hold the connection, yamux makes the dialed connection symmetric, and ordinary gRPC runs in both directions over it; identity is a Kubernetes token the hub verifies with a `TokenReview`, so a peer never names itself (`link/doc.go`). A site-to-hub link is that design with a longer wire, and a site-to-site link is two such links joined at the hub. Three alternatives were weighed:

| Option                                        | Why not                                                                                                                                                                                                               |
|-----------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| OCM `cluster-proxy` (apiserver-network-proxy) | Gives the hub a path into a member's Services, not a member a path to another member. A relay would still be needed, and the proxy adds a second tunnel, its own agent, and its own identity model beside the link's. |
| A VPN or routed overlay between the sites     | A network prerequisite nobody can assume, and a listener on every site. The link exists precisely so that nothing listens.                                                                                            |
| Everything through `ManifestWork` and status  | Fine for intent and reports, which is what it carries (§6). Wrong for a switchover's demote-then-promote handshake, which wants a request, a deadline, and an answer, not a reconciled object.                        |

### 7.2 The relay on the hub

The fleet manager runs a link **Hub** (`link.NewHub`) on a TLS listener, fronted by a NodePort or LoadBalancer Service the members can reach, and is started with `--site-link-address`, the host and port they reach it at. Its `Authenticator` is `link.SiteAuthenticator`, which verifies an agent's token with a `TokenReview` against the hub's own API server and takes the site from the token's ServiceAccount namespace (§7.3). The relay registers one service, `RelayService` (Appendix C): a peer asks for a stream to another peer by its address, the relay sends it on to the target site's agent, and every hop splices the call it serves to the call it makes. Over the spliced stream the caller runs an ordinary `grpc.ClientConn` against whatever the target registered, so a service a site operator serves to its own plugins is served to the other site's operator the same way, with no hop parsing it past the first frame.

For each `ManagedCluster`, the fleet manager writes two objects into the member's namespace on the hub: the ServiceAccount `simplyblock-site-link`, whose tokens the relay accepts, and the ConfigMap `simplyblock-site-link`, which names the relay's address and the CA of its serving certificate. The certificate is self-signed and kept in a Secret beside the manager, so a restart serves the same one every agent already trusts.

The relay is leader-elected with the fleet manager, as the operator's hub is with the operator, and `Accepting` is wired to its leadership exactly as `link.HubConfig` describes. A site whose relay session drops redials; a peer that is not currently linked is `ErrNoSession`, retryable, and a reconciler requeues on it (`link/doc.go`).

### 7.3 The forwarder in the agent

The fleet add-on's agent, a Deployment in the add-on template ([`design-management-hub.md`](design-management-hub.md) §9), is the site's end of the site link. It dials the relay with `link.NewAgent`, identifying as `site/<member-name>`.

Its credential is a token for the `simplyblock-site-link` ServiceAccount in its own member namespace on the hub. The add-on's `KubeClient` registration gives the agent a client certificate for the hub, which a `TokenReview` cannot verify, so the agent uses that certificate for two things only: reading the relay endpoint ConfigMap and requesting the token (`serviceaccounts/token`), both in its own namespace, which is all the registration's hub ClusterRole (`simplyblock-site-link-agent`, bound with `CurrentCluster`) allows. The relay verifies the token and knows the site from its namespace. Open Cluster Management names a member's namespace after the member, and only that member's add-on can request a token there, which is why the identity is the member and not a claim. The token carries the site link's audience and is renewed well before it expires.

Inside the site, the agent is a second hub: it listens on a Service in the operator's namespace, and the site's operator and CSI controller dial it as they dial the operator's own link today, with their own in-cluster tokens the agent verifies against the site's API server. A stream a local peer opens to a remote `PeerID` is forwarded up the agent's relay session; a stream the relay delivers for a local peer is forwarded down. The agent holds no service logic and no credentials for the control plane. It is a forwarder with two authenticated ends, and the only process on a site that speaks to the hub over anything but the Kubernetes API.

The agent serves its site's peers with a self-signed certificate whose CA it publishes as the ConfigMap `simplyblock-site-link-ca` in its own namespace, readable by every ServiceAccount of the member. Its local hub is a `KubeAuthenticator` that admits the operator's and the CSI controller's ServiceAccounts as the `operator` and `controller` kinds, with tokens projected for the site link's audience.

The operator's own link to its plugins is untouched: node plugins and the controller keep dialing the operator. What the operator gains is a second link, reached through the agent, in which `site/b/operator` and `site/b/controller/<pod>` resolve. With the chart's `siteLink.enabled`, the elected operator replica links to the agent, and the `SimplyblockDriver` it renders gives the CSI controller plugin the same link. Both serve gRPC health to the other site, and the `StorageCluster` controller probes the other site's operator through it, addressed by `spec.stretched.peerCluster`, the other site's managed cluster, which the hub writes beside `peerSite`. The answer is `status.stretched.siteLink`, `Connected` or `Unreachable`, with a `SiteLinkChanged` event on each change.

### 7.4 Peers and identities

| PeerID                           | Who                                           | Authenticated by                                                                              |
|----------------------------------|-----------------------------------------------|-----------------------------------------------------------------------------------------------|
| `site/<member>`                  | The fleet agent of a site                     | The hub, from the add-on's registration token                                                 |
| `site/<member>/operator`         | That site's operator, through its agent       | The agent, from the operator's token; the relay trusts the agent for its own site's sub-peers |
| `site/<member>/controller/<pod>` | That site's CSI controller, through its agent | As above                                                                                      |
| `hub/fleet-manager`              | The relay itself, serving hub-side services   | n/a                                                                                           |

A sub-peer's identity is the pair of the agent's verified site and the agent's verified local peer, and the relay refuses a sub-peer whose site prefix is not the agent's own. A compromised agent can impersonate peers of its own site and no other, which is the ceiling [`design-management-hub.md`](design-management-hub.md) §10.2 sets for a member anyway.

### 7.5 What the fabric carries

| Service                                      | Served by                   | Called by                                                                | Used for                                                                                        |
|----------------------------------------------|-----------------------------|--------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------|
| `SwitchoverService`                          | Each site's operator        | That site's node plugins (local link) and the other site's operator (§9) | Move a volume's or group's leadership; serialized per store                                     |
| `SiteService`                                | Each site's operator        | The other site's operator                                                | `Describe` (site, role, cluster, nodes); `RefreshNode` for the leader's rolling restart (§11.3) |
| `nvme.subsystems`, `nvme.devices`, conntrack | Each node plugin (existing) | The leader's reconcilers, across the relay                               | The data-plane reads the operator already makes locally                                         |
| HTTP relay to the control plane (optional)   | The relay                   | A site operator or driver                                                | A deployment that will not route the management API to the sites (§3.3); off by default         |

The last row is the one concession to deployments that keep the management API hub-internal: the agent can forward the operator's and driver's HTTP to the management API over the same session. It is optional because the storage nodes need the routed management network regardless (§3.3), so most deployments will route it and not use the relay for it.

---

## 8. RamenDR in Sync Mode

### 8.1 What Ramen does in sync mode

A `DRPolicy` whose `schedulingInterval` is empty is a sync, or metro, policy. Ramen then treats the storage as one replicated instance under both clusters and does three things and no fourth:

- **Placement.** A `DRPlacementControl` relocates or fails over the application by moving its placement between the two clusters.
- **Kubernetes state.** The VRG on the primary uploads PV and PVC objects to the S3 profile of its `DRCluster`; the VRG on the target restores them, so the application's claims reappear bound to the same backend volumes, by the same `volumeHandle`.
- **Fencing.** On a failover, Ramen sets the lost `DRCluster`'s `clusterFence` and delivers a csi-addons `NetworkFence` to the surviving cluster, listing the lost cluster's `cidrs`; the surviving cluster's csi-addons controller calls the driver's fence RPC.

What it does not do is create a `VolumeReplication`: `vrg_volrep.go` returns before any VR work when `Spec.Sync` is set, and marks the protected PVCs ready on the reasoning that "there is no VolRep involved in sync mode." The driver's replication RPCs are therefore never called for a sync-protected volume. PR #673's switchover, a node patching a `VolumeReplication` that the csi-addons reconciler turns into a promote, has no object to patch. §9 replaces it.

### 8.2 Classes, labels, and the policy

Ramen pairs the two clusters' `StorageClass`es by name and by the `ramendr.openshift.io/storageid` label, and a sync peer is one whose storage id is the **same** on both clusters: "singleton if the storage instance is shared across the peers" (`PeerClass.StorageID`). That is this cluster exactly.

| Object                   | Site A                                                                                   | Site B                                                          | Rule                                                                                                                      |
|--------------------------|------------------------------------------------------------------------------------------|-----------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------|
| `StorageClass`           | `name: prod-sync`, `cluster_id: <uuid>`, `pool_name: <pool>`, label `storageid: <uuid>`  | identical                                                       | Same name, same backend cluster, same pool, same storage id. Delivered to both sites by one `StorageClassDeployment` (§6) |
| `VolumeReplicationClass` | none                                                                                     | none                                                            | Not needed for a sync peer; PR #673's single sync class is retired                                                        |
| `DRCluster` ×2           | `region: <one value>`, `cidrs: [site A node CIDRs]`, `s3ProfileName`                     | `region: <same>`, `cidrs: [site B node CIDRs]`, `s3ProfileName` | Same region puts them in one sync group; the CIDRs are what a fence names                                                 |
| `DRCluster` annotations  | `storage-driver`, `storage-clusterid`, `storage-secret-name`, `storage-secret-namespace` | identical                                                       | Ramen copies them into the `NetworkFence` it creates (§10.1). The cluster id is the backend cluster's UUID                |
| `DRPolicy`               | `schedulingInterval: ""`, `drClusters: [a, b]`                                           |                                                                 | Sync mode                                                                                                                 |

The `cluster_id` parameter names the one backend cluster on both sites, so a volume created on either site is a volume of the same cluster, and the handle the PV carries resolves on both sites through the driver's cluster secret, whose one entry is marked `local` on both (§9.4). Nothing in the driver's chain-walk or fail-over redirect runs for a sync volume; those are the async design's.

### 8.3 Relocate and failover, as Ramen sees them

**Relocate (planned, both sites up):** the DRPC switches placement; Ramen on A sets the VRG secondary and removes the application; Ramen makes the VRG on B primary, and it restores the PVs and PVCs from S3 with the same `volumeHandle` (§9.6); B's operator sees the VRG primary and moves its volumes to B (§9.2); Ramen points the placement at B once the VRG reports ready; the application's pods schedule on B, and their stage attaches once the volumes are led from B (§9.4).

**Failover (A lost):** the DRPC fails over; Ramen fences A through B's driver (§10); makes the VRG on B primary with action `Failover`; B's operator moves the volumes to B, forced, because the planned promote cannot pass against a lost site; the pods schedule on B and stage once the volumes are led from B.

**Fail-back:** when A returns, the backend's site activation rebuilds A's instances automatically (`sync-replication.md` §6, "Site return"); Ramen unfences A; a relocate back to A is a planned move again.

In every case Ramen's VRG on the target is the trigger, the target's operator moves the storage, and the node plugin is the gate.

---

## 9. The Flip: Ramen-Driven, Without VolumeReplication

### 9.1 The principle survives

PR #673's §6 stands on one property: a node never attaches a volume that is not led from its own site. What changes is what triggers the move. Ramen's sync mode assumes storage active on both sites at once, so it moves an application's placement and its PV and PVC objects and calls no storage verb: it creates no `VolumeReplication`, and nothing reaches the driver's `PromoteVolume`. This cluster leads each volume store from one site, so the move is made by the site's operator, and Ramen's own decision is its trigger: when Ramen makes an application's `VolumeReplicationGroup` primary on a site, that site's operator moves the application's volumes there, before Ramen points the placement at the site and the pods start. The node plugin only checks the result.

### 9.2 The operator follows Ramen's `VolumeReplicationGroup`

Each site's operator watches Ramen's `VolumeReplicationGroup`s (`operator/internal/controllers/vrgfollow`). A VRG it acts on is a sync VRG (`spec.sync` set; an async VRG's volumes are moved by Ramen's `VolumeReplication`s) whose `spec.replicationState` is `primary` on this site, on a cluster whose `StorageCluster` is one site of a stretched cluster. For such a VRG it selects the claims of `spec.pvcSelector` in the VRG's namespace, keeps those bound to a PV of this driver whose handle names the stretched cluster, and asks the `SwitchoverService` (§9.3) to move each volume to this site. The move is planned, and allowed to escalate to a forced promote only when the VRG's `spec.action` is `Failover`, which Ramen sets only after the lost cluster is fenced (§10).

The controller does not block. A move the backend is still running is asked about again after a named interval, and so is a VRG whose claims Ramen has not restored and bound yet. A refused move is reported as a `SyncMoveRefused` event on the VRG, naming the blocking volumes, and asked about again. It watches VRGs on spec changes only, and the claims of their namespaces, which Ramen restores after it has made the VRG primary. Ramen's kind is watched only once the API server serves it, because a watch on an absent kind stops the whole manager: the operator starts the controller when Ramen is installed, without a restart.

Ramen's own readiness does not wait for the move: in sync mode the VRG reports its data ready without asking the storage. Ramen may therefore point the placement at the site while a promote is still running, and the stage's gate (§9.4) covers that window.

### 9.3 `SwitchoverService`

The operator's `SwitchoverService.Move(volume, toSite)` (Appendix C) is the one caller of the backend's sync verbs, and it owns the sequencing PR #673 §6.3 assigned to the csi-addons reconciler. Its caller is the VRG follower of §9.2, in process:

1. Read `GET V/replication/sync-status`. A volume already led from `to_site` answers `MOVED` with no call to the backend, which makes a repeated `Move` free.
2. `POST V/replication/demote?site=<from>`. The backend answers `204`, or `409` naming the volumes of the store still served on the old site, which is the case of a store shared with a workload that did not move. The operator answers `REFUSED` with those volumes, which the VRG follower reports on the VRG. A demote the backend cannot reach because the old site is down is skipped; step 3 then escalates.
3. `POST V/replication/failover?site=<to>&planned=true`. `200` answers `MOVED`. `409 promote in progress` answers `IN_PROGRESS`. `412`, the other site not online, escalates to `planned=false` when the request allows it, and emits the `SyncPeerSiteOffline` event on the site's `StorageCluster`s: the same inversion PR #673 §7.1 defines, whose safety rests on the backend's proof that the site is lost (§10, §13). Any other `409` answers `REFUSED`.

The operator serializes moves per store: a `Move` for a store with a move in flight answers `IN_PROGRESS` without calling the backend, so the volumes of one application on one store produce one demote and one promote. The service does not poll: its caller asks again, and the next `Move` finds the backend's resumable promote further along, which is the non-blocking wait PR #673 §6.4 chose.

The implementation is `atlas-lib/switchover` (the service, over a backend interface) and `operator/internal/switchover` (the backend, through the control-plane client). Each site's operator also registers it on its site link, for the other site's operator. A group move through the `G/replication/...` routes is not built: `MoveRequest` names one volume, and a consistency group's members move one by one.

The demote in step 2 is issued against the **old** site by the **new** site's operator. The backend is site-agnostic on the API, so this needs no cooperation from the old site's operator, which is what lets a relocate complete when the old site's operator is slow, and lets a failover complete when it is gone. The old site's operator learns of the move from the control-plane stream it already watches, and its node plugins' next unstage finds nothing to do (PR #673 §6.5).

A claim moved without Ramen (a test, or an operator moving a workload by hand) has no VRG, and nothing moves its volume: whoever moves the claim moves the volume, through the backend's routes or `sbctl volume sync-promote`, before the pod is started on the new site (`regression_test/25/07-flip.sh`).

### 9.4 The gate

On a sync cluster every volume is a sync volume: the mode is the cluster's, not the class's (`sync-replication.md` §2), so the per-PVC `VolumeReplication` lookup of PR #673 §6.6 is replaced by one fact in the driver's cluster secret entry: `sync: true`, written by the operator from `StorageCluster.status`, beside `site: <this site>` (§5.3). Nothing is read from the `Node` object and nothing is correlated through PVCs.

At `NodeStageVolume` the plugin reads `GET V/replication/sync-status` (no site), which answers the site the store is led from, and compares it with its own:

- **Led here:** proceed to `GET V/connect` and attach. This is the common case: a volume is born on the site that created it (§9.5), and a moved one is moved before Ramen starts its pods.
- **Led elsewhere, or nowhere** (a promote still running, or the window after it before the first I/O): hold with `Unavailable`, naming the site it is led from; the kubelet stages again. The node never moves a volume.
- **A secret entry with `sync: true` and no `site`:** fail with `FailedPrecondition` before any backend call.

The node attaches only once the status names its site, so it never attaches on a stale status. The gate runs before the stage connects to the volume, because a connect on a site the volume is not led from clears the backend's record that the site never connected, which the birth-state demote depends on (`ensureLedHere` in `csi-driver/internal/csi/node/syncgate.go`). The first-I/O rule of the backend ("issue client I/O on at least one volume of every LVS that moved before any object operation on that LVS," `sync-replication.md` §6) is met by the mount itself; the operator defers any object operation on a store it has just moved until the status shows a leader.

### 9.5 Birth site: a PVC's volume is created on its own site

A volume is served from the site its logical volume store is led from, and at creation that is the store's home site, the site of its owner node (`sync-replication.md`, "Which site serves a volume"). The backend picks the owner node from the online nodes of the whole cluster when the create body carries no `host_id`, and on a stretched cluster that choice lands on either site. A PVC created on cluster A whose owner node is on site B is then led from B, and the first pod to use it on A pays a demote and a promote before it can stage (§9.3). The rule here removes that case: **a volume is born on the site of the cluster that created it.**

**What the driver sends.** The controller plugin knows its site from the driver's cluster secret entry (§5.3). On a sync cluster it sends `site: <own site>` in every volume create body, beside the `host_id` it may already carry, and sends no `site` on any other cluster, where the backend refuses one. The backend restricts its owner-node choice to the online nodes of that site and answers `400` for a site the cluster does not have. That parameter is the one addition to the create route (§12, P0-2); the driver's own placement code is otherwise untouched.

**The host pin still wins, and it agrees.** The driver already pins the owner node from a PVC annotation (`simplyblock.io/selected-storage-node`, the placement hint, or the legacy `host-id`) or, for a pod-affinitive claim, from the storage-node UUID the operator labels onto the worker the pod was scheduled to (`coLocatedHostID` in `csi-driver/internal/csi/controller/placement.go`). On a stretched cluster the worker labels of cluster A name only site A's storage nodes, because site A's operator labels only the workers it owns (§5.2), so a co-located pin always agrees with the site. An annotation naming a node of the other site is a contradiction the backend refuses with `409`, naming both, rather than a case the driver resolves: the annotation is a deliberate override, and silently moving it is worse than refusing it. The driver returns that refusal as `codes.FailedPrecondition`. It is told apart from the `409` of a name that exists by its detail, which names the node's site.

**A consistency group's pin wins over the site.** A volume joining a consistency group at creation is placed on the group's store, because a group's members share one store. The backend applies that pin before the site and logs that the site was not applied, so a member created on site B of a group whose store is homed on A is born on A, as a clone is.

**Binding mode does not matter.** With `WaitForFirstConsumer` the pod's topology arrives at create time and the co-location pin may apply; with `Immediate` nothing arrives and the backend picks within the site. Both land on the right site, because the site is the installation's and not read from topology, which is the property PR #673's zone-label model lacked.

**Clones and restores inherit their source.** A clone or a snapshot restore is created on its source's logical volume store, so it is born on the source's home site whatever cluster asks for it. A clone requested on cluster B of a volume whose home is A is led from A, and B's first pod moves it with the ordinary flip. The driver does not send `site` on a clone or restore, because the backend has no way to honor it, and §16 Q8 asks whether the backend should offer a cross-site clone.

**A recreated claim is not a new volume.** A relocate or a failover removes the PVC on one site and recreates it on the other, and that recreate inherits the existing volume instead of creating one (§9.6). The birth site is decided once, by the cluster that first provisioned the claim; from then on the led site moves with the workload and the home site stays where it was.

### 9.6 Relocate and failover recreate the claim, and the claim inherits the volume

A move is a Kubernetes operation on the claim and a backend operation on the leadership. The two are kept apart on purpose: the PVC and its PV are removed on the site the workload leaves and recreated on the site it arrives at, while the backend volume is neither deleted nor duplicated. The recreated claim inherits it.

**On the site the workload leaves.** Before any PVC is protected, Ramen sets its PV's `reclaimPolicy` to `Retain` and annotates the PV as retained for its own needs (`retainPVForPVC` in Ramen's `vrg_volrep.go`). When the placement moves, the application is removed and its PVC with it, and the retained PV goes `Released` without the kubelet ever calling `DeleteVolume`. In sync mode Ramen keeps the retention on the secondary, because both sites' VRGs map to the same volume, and it clears the PV's claim reference so the PV can bind again to a recreated PVC when the workload comes home (Ramen's comment at the VR-deletion path and `updateExistingPVForSync`). The volume therefore stays, with its home site and its data, and the driver on this site is not involved in the move at all.

**On the site the workload arrives at.** Ramen restores the PV and the PVC it uploaded to S3: the same PV name, the same `volumeHandle`, bound to each other. The handle names the one backend cluster, which the driver's secret on this site resolves, so the restored PV is a static PV of a volume this site already knows. No `CreateVolume` runs. If a PV of that name already exists here, from a previous stay of the workload, Ramen updates it in place for sync (`updateExistingPVForSync`) rather than refusing. This site's operator moves the leadership when Ramen makes the VRG primary here (§9.2, §9.3), and the first pod's stage attaches once the volume is led from this site (§9.4).

**Without Ramen.** A test or an operator-driven move does what Ramen does: it retains the PV on A, removes the PVC, and restores the same PV on B as a static PV bound to a new PVC (`regression_test/25/07-flip.sh`). Applying a PVC of the same name on B is not enough. The external provisioner names a volume after the claim's UID, `pvc-<uid>`, and a recreated claim has a new UID, so its create names a volume that does not exist and provisions a new, empty one.

**The create's reconcile on a sync cluster.** A create reaches a name that exists when the provisioner retries a claim whose earlier attempt the backend completed. The existing reconcile (`reconcileExistingVolume` in `csi-driver/internal/csi/controller/volume.go`) reuses the online volume of that name after a size check. Two rules are added to it for a sync cluster:

- A volume is reused whatever site it is led from. The driver does not compare the led site with its own; §9.3 moves it at stage time.
- A volume of that name that is not online is never deleted by the create. The existing reconcile deletes a non-online leftover as a failed earlier attempt, which on a sync cluster is also what a volume whose site is down looks like. The create answers `codes.Aborted` with the volume's status and holds until the volume is online or an administrator removes it.

The `site` parameter of §9.5 is sent on the create and ignored by the backend once the name exists, so a retried create never produces a second volume.

**A real delete.** A protected claim cannot delete its volume, which is the point of the retention. To delete a volume, the workload is unprotected first: with the DRPC gone, Ramen restores the PV's `Delete` policy on the primary (`undoPVRetention`, which runs for a sync VRG only while it is primary), and the PVC's deletion then reaches `DeleteVolume`, which deletes the volume cluster-wide from either site.

---

## 10. Fencing a Lost Site

### 10.1 What Ramen sends

On failover Ramen sets `DRCluster/<lost>.spec.clusterFence: Fenced` and delivers to the surviving cluster a `NetworkFence` naming the lost cluster's `cidrs`, with the driver, the cluster id, and the secret taken from the lost `DRCluster`'s storage annotations (§8.2). The csi-addons controller on the surviving cluster calls the driver's `FenceClusterNetwork` with those CIDRs, and later `UnfenceClusterNetwork` when the `DRCluster` is unfenced. A driver that does not implement the fence service, or does not advertise `NETWORK_FENCE` in the csi-addons identity, leaves the DRPC waiting on `Fenced`.

### 10.2 What the driver does with it

The CIDRs name hosts, and this cluster's fence unit is a site. The driver's controller (`csi-driver/internal/csi/fence`) takes the cluster from the fence's `clusterID` parameter, or the one sync cluster in its secret when the parameter is absent. It resolves the CIDRs to a site through the control plane's storage-node listing, where every node's management address is on record with its site, refuses with `InvalidArgument` a fence whose CIDRs are not CIDRs, span both sites, or match no node, and calls the site fence (P0-2):

```
POST /api/v2/clusters/{id}/sites/{site}/fence
```

The backend runs its disaster site steps once for the site: the liveness proof per node of the site (no running state, no SPDK answer, no surviving peer still reporting the journal link), then `lost_site = <site>`, `lost_site_state = fencing`, every device of the site unavailable, its nodes `OFFLINE`, every volume of every store led from it fenced on it, then `done` (`sync-replication.md` §5). The route is idempotent and resumable, as the steps already are. It runs the status check of the proof inline and the rest as a task: `202` with the task while it runs, which the driver returns as `codes.Unavailable`, `200` once the site is recorded `done`, and `409` with the reason when the fence is refused, which the driver returns as `codes.FailedPrecondition`. Either way csi-addons retries. The backend refuses while a node of the site is in a running state, while another site is recorded lost, while the monitor is draining the site, during an activation of the site, when no other site is online, and once when the task's liveness proof failed, naming the task; the next call starts a new attempt. The backend also holds a site's return while a fence task of it is open, so an activation cannot rebuild nodes the fence is taking offline. The site-level route is the one new backend requirement: today those steps run only inside a forced promote of one store, and a fence that promoted stores one by one would leave the site half-fenced between calls.

The fence does not move leadership. Stores led from the fenced site stay closed until a promote opens them on the survivor, which the survivor's operator requests (§9.2) when Ramen makes the VRG primary there with action `Failover`, escalating to `planned=false` because the planned gate cannot pass against a fenced site. Ramen's own order guarantees the VRG turns primary after the fence.

`UnfenceClusterNetwork` is a confirmation, not an action. The site returns by the backend's own activation when its nodes come back, and the route answers `200` once the control plane no longer records the site, `409` while it still does, which the driver returns as `codes.FailedPrecondition`. Ramen then holds `Unfenced` until the storage has healed, which is the behavior the DRPC expects before a relocate back.

### 10.3 Why the surviving site's operator never needs the leader

Nothing in §9 or §10 reads the leader role. Each site's operator follows its own site's VRG and calls the control plane; the fence is issued by whichever site Ramen addresses, which is always the survivor. The leader role governs cluster lifecycle (§5.2) and nothing in the DR path, which is what makes the loss of the leader site an ordinary failover (§11).

---

## 11. Leader Hand-Over and Day Two

### 11.1 When the leader site is lost

The follower keeps running every DR action (§10.3). What it cannot do while the leader is gone is cluster-scoped lifecycle: activate, expand, roll a restart across the cluster. For a short outage that is acceptable, and the backend's site return handles the data plane without an operator. For a long one, the hub promotes the follower: an edit of `StretchedCluster.spec.sites[].role` swaps the roles, the reconciler re-delivers both `ClusterDeploymentConfig`s with their new roles, and the new leader's `StorageCluster` admits cluster-scoped `StorageClusterOps` from the next reconcile. An approved `ClusterDeploymentConfig` is frozen but for `spec.cluster.stretched.role`: the freeze is the admission webhook's rather than a schema rule's, because a schema rule cannot exempt one nested field. The site's `ClusterDeploymentConfig` controller carries the role of an expanded document onto the `StorageCluster` it produced, and nothing else of it. A site that refuses a re-delivered document keeps its `StorageCluster` projected on the request, so a swap that failed can be swapped back. The old leader, when it returns, receives `Follower` and its controller drops the role on the next reconcile; an in-flight cluster-scoped ops on it is failed with `RoleLost`.

The role swap is a hub decision, taken by a person or by a `FleetOperation`, never by the sites themselves: two operators that each decided they lead would be the split brain the design otherwise has no path to. The `StretchedCluster` webhook admits a swap in two cases, and refuses every other with `RoleSwapNotReady`:

- **Both sites are Online.** The planned swap: cluster-wide operations move between two working sites.
- **The leader's site is lost and fenced.** The swap is admitted when the control plane records the current leader's site as lost with its fence `done`, and the survivor's `StorageCluster` is Online or Degraded. The fence is what proves the old leader cannot act: a swap while the record is `draining`, `down`, or `fencing` would hand the role over while the old leader may still be running, which is the two-leader case. A swap that would make the lost site the leader is refused too.

The guard reads the lost site, its fence, and the nodes' status from the control plane the hub runs, at admission. The request's own status, `status.lostSite` and `status.sites[]`, is read through the sites' views on their own schedule and lags a site going down by minutes; a swap admitted on it could land before the fence, while the old leader is cut off but still running. So with no site recorded lost, a swap also needs every node of both sites online, and a control plane the guard cannot read refuses the swap.

The cluster record carries `leader_site` (§12): the leader's create sets it, and the leader that takes up the role writes its own site to it. A follower that finds `leader_site` naming its own site at adoption refuses with `RoleConflict`. The control plane refuses `leader_site` for a site it records as lost, which is a second guard against the lost site leading.

### 11.2 Node operations on a site

A site's own `StorageNodeOps` behave as on a single-site cluster, and the backend already refuses or waits where the two-site model requires it (`sync-replication.md` §6, "Node restart"; §8). Adding a worker to a site after approval is the existing day-two path on that site: a `StorageNode` for the worker, with the site's `site`, added through the site's operator (the leader for its own site, the follower for its own). The `StretchedCluster`'s discovery is not rerun once approved.

### 11.3 Upgrades

The control plane is upgraded on the hub, by the hub operator's `ControlPlaneOps`. The driver is upgraded per site by its `DriverDeployment`.

The storage-node workload is upgraded by the leader's rolling restart, which covers both sites in one walk. The walk's node list is the backend cluster's, both sites' nodes, and its shutdown and restart are backend calls, which the leader makes for any node. What it cannot do for a node of the other site is replace the node's storage-node pod, the step that pulls a new image (`spec.rollingRestart.refreshSNodeAPI`), because the pod runs on the other Kubernetes cluster. For such a node, recognized by its remote `StorageNode`, the leader asks the other site's operator over the site link, `SiteService.RefreshNode(cluster, node, since)`, with the operation's creation time as `since`. The other site deletes the pod if it was created before `since`, and answers `IN_PROGRESS` until a pod created after `since` is Ready, then `REFRESHED`. Asking again is harmless, so the walk asks on every pass and never blocks. Without a site link, the walk holds on that node and says why. A node with no pod on the other site has nothing to refresh, the same as a node of this site without one (`operator/internal/sitesvc`).

---

## 12. Backend API Requirements

| Method | Endpoint                                                                                                    | Notes                                                                                                                                                                |
|--------|-------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `POST` | `/api/v2/clusters/` with `sync_replication: true` and `leader_site`                                         | Create the two-site cluster, naming the creating site its leader; `leader_site` without `sync_replication` is `400`                                                  |
| `PUT`  | `/api/v2/clusters/{id}/` with `leader_site`                                                                 | **New**, on `stretched-dto-fields`. `204`, idempotent; `400` for a cluster that is not sync, an empty value, or a site with no node; `409` for the lost site         |
| `POST` | `/api/v2/clusters/{id}/storage-nodes` with `site`                                                           | Add a node on a site. On the branch                                                                                                                                  |
| `POST` | `/api/v2/clusters/{id}/storage-pools/{id}/volumes` with `site`                                              | **New.** Restrict the owner-node choice to the online nodes of that site; `400` for an unknown site, `409` when `host_id` names a node of another site (§9.5)        |
| `POST` | `V/replication/demote?site=`, `V/replication/failover?site=&planned=`, and the `G/replication/...` siblings | The planned and forced moves the operator issues (§9.3). On the branch                                                                                               |
| `GET`  | `V/replication/sync-status`                                                                                 | The led site the VRG follower and the node's gate read (§9.2, §9.4). On the branch                                                                                   |
| `POST` | `/api/v2/clusters/{id}/sites/{site}/fence`                                                                  | **New**, on `stretched-dto-fields`. Run the disaster site steps for a whole site as a task: `202` running, `200` done, `409` refused (§10.2)                         |
| `POST` | `/api/v2/clusters/{id}/sites/{site}/unfence`                                                                | **New**, on `stretched-dto-fields`. Confirm the site is no longer recorded; `409` while it is (§10.2). `GET .../sites/{site}/status` reads the record                |
| `GET`  | `/api/v2/clusters/{id}/sites`                                                                               | **New**, not built. The sites, their records, and node membership, for the hub's status roll-up. The driver maps CIDRs through the storage-node listing instead      |
| `GET`  | `/api/v2/clusters/{id}/reachability?site=` or `?address=...`                                                | **New**, on `stretched-dto-fields`. One run of the monitor's ping against the nodes, or against up to 64 given IP literals; the follower's preflight reads it (§5.4) |

The fence route is the only one Phase 3 cannot ship without. The others improve legibility and remove reads the operator would otherwise reconstruct from the node listing.

---

## 13. Failure Modes

| Failure                                                                                       | What happens                                                                                                                                                                                                                                                                                                         | Covered by                                         |
|-----------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------|
| A site is lost                                                                                | The monitor records it `draining` then `down`; the survivor keeps serving its own stores; Ramen fails the application over, fences the site (§10), stage promotes on the survivor with `planned=false`. Site returns by activation.                                                                                  | Backend §5, this §9–§10                            |
| The leader site is lost                                                                       | As above, plus cluster-scoped lifecycle waits until the hub swaps roles (§11.1)                                                                                                                                                                                                                                      | §11                                                |
| The hub is lost                                                                               | No monitoring, no site records, no fail-over, no Ramen action, no payloads. Both sites' data planes keep serving what they served; both operators keep reconciling what they have; node plugins keep staging volumes led from their own site (the status read fails, so a move cannot start). Recovery is the hub's. | Backend §2 limitations; Non-Goals; Open Question 1 |
| Hub cut off from one site, the other healthy                                                  | The cut-off site's nodes turn `UNREACHABLE` and are vetoed by their peers, so no record and no forced promote passes while the storage network holds (backend's "management-only partition"). The site link to that site is down; the hub reports it.                                                                | Backend §2                                         |
| Hub cut off from both sites                                                                   | Judges nothing; no record proposed. Data plane unaffected. Same as hub lost, from the sites' view.                                                                                                                                                                                                                   | Backend §2                                         |
| Full partition: a live site looks lost to hub and peer                                        | Outside the backend's assumption: a forced promote passes and both sites may serve. The design does not add a witness (Non-Goals); Ramen's fence narrows the window to the CIDRs it blocks on the survivor, not on the isolated site.                                                                                | Open Question 2                                    |
| Site link relay down                                                                          | `ErrNoSession`; `SwitchoverService` from a node to its own operator is local and unaffected; cross-site reads and the leader's cross-site restart wait. `ManifestWork` and the control plane are unaffected.                                                                                                         | §7.2                                               |
| Fleet agent down on a site                                                                    | That site's operator keeps its local link; the hub's report for the site goes stale; cross-site calls to that site wait.                                                                                                                                                                                             | §7.3                                               |
| Store shared with a workload that did not move                                                | Demote answers `409` naming the still-served volumes; stage holds with that message. Operational: do not share a store across workloads that fail over separately.                                                                                                                                                   | PR #673 §12, §9.3 here                             |
| A PVC annotation pins a node of the other site                                                | The create answers `409` naming the annotation's node and the site; the PVC stays `Pending` with that message. No volume is created on the wrong site.                                                                                                                                                               | §9.5                                               |
| The site's nodes are all offline at create time                                               | The create answers `409` (no online node on the site) rather than placing on the other site; the PVC stays `Pending` until a node returns or the site is swapped out of the config.                                                                                                                                  | §9.5                                               |
| A claim is recreated on the target while the source site is down and the volume is not online | The create's reconcile finds the volume by name, not online, and holds with `codes.Aborted`; it never deletes it. Ramen's own path does not create and is unaffected.                                                                                                                                                | §9.6                                               |
| A PVC is deleted on a site while protected                                                    | The PV is retained; `DeleteVolume` is not called; the volume stays. Deleting the volume takes an unprotect first.                                                                                                                                                                                                    | §9.6                                               |
| Fence CIDRs match no node, or both sites                                                      | The driver refuses the fence with `codes.InvalidArgument`; the DRPC reports it; nothing is fenced.                                                                                                                                                                                                                   | §10.2                                              |
| Forced promote refused: site not proven lost yet                                              | `409` with the reason; the operator reports it and the stage holds; csi-addons and the kubelet retry.                                                                                                                                                                                                                | Backend §5                                         |
| Follower delivered before the leader created the cluster                                      | Cannot happen by the reconciler's sequencing; if a follower's adoption finds no cluster it holds in `CheckingControlPlane` with `PeerClusterMissing`.                                                                                                                                                                | §5.1                                               |
| Both payloads say `Leader`                                                                    | Refused at admission on the hub (`StretchedCluster` webhook) and, defensively, by the operator: the cluster record carries `leader_site`, set by the create, and a follower whose record names its own site refuses to adopt (`RoleConflict`). The control plane only stores the field; the check is the operator's. | Appendix A, §12 note below                         |

The cluster record's `leader_site` is a small backend addition under P0-2's umbrella: the create body sets it, the adoption read returns it, and `PUT /clusters/{id}/` moves it, which the site acting as leader does after a role swap (§11.1). The control plane refuses a lost site as leader.

---

## 14. Observability

The `StorageCluster` and `StorageClusterOps` controllers already emit events and the operator already serves a Prometheus registry; the rows below are additions to both. The CSI node plugin's surface stays `klog`, and the fleet agent's is new: it exports only the link gauge.

### Kubernetes Events

| Event                                                                                              | Type             | Reason                | On                                   |
|----------------------------------------------------------------------------------------------------|------------------|-----------------------|--------------------------------------|
| The leader's activation holds because the follower's nodes are not in yet                          | Normal           | `WaitingForPeerSite`  | `StorageCluster`                     |
| The follower adopted the cluster the leader created                                                | Normal           | `ClusterAdopted`      | `StorageCluster`                     |
| A `StorageNodeOps` or `StorageDeviceOps` was refused because the node belongs to the other site    | Warning          | `RemoteNode`          | `StorageNodeOps`, `StorageDeviceOps` |
| A cluster-scoped operation was refused because this site is the follower                           | Warning          | `FollowerSite`        | `StorageClusterOps`                  |
| The cluster's site record changed, naming the site and the state                                   | Normal / Warning | `SiteRecordChanged`   | `StorageCluster`                     |
| A site's klusterlet is no longer reachable from the hub                                            | Warning          | `SiteUnreachable`     | `StretchedCluster`                   |
| A role swap took effect, naming the old and the new role                                           | Normal           | `RoleChanged`         | `StorageCluster`                     |
| The site's relay session came up or went down                                                      | Normal / Warning | `SiteLinkChanged`     | `StorageCluster`                     |
| A switchover completed for the volume, naming the from and to sites and whether it was planned     | Normal           | `VolumeMoved`         | `PersistentVolume`                   |
| A planned promote answered `412` and was escalated to a forced one (PR #673 §13, retained)         | Warning          | `SyncPeerSiteOffline` | `PersistentVolume`                   |
| The driver's fence or unfence completed on the control plane                                       | Normal           | `SiteFenceChanged`    | `StorageCluster`                     |
| A fence was refused because its CIDRs resolved to no site or to both, or because the proof refused | Warning          | `FenceRefused`        | `NetworkFence`                       |

### Prometheus Metrics

| Metric                                    | Labels                          | Description                                                           |
|-------------------------------------------|---------------------------------|-----------------------------------------------------------------------|
| `simplyblock_stretched_site_role`         | `cluster`, `site`               | Gauge: 1 for the leader, 0 for the follower, as this operator sees it |
| `simplyblock_site_link_up`                | `site`                          | Gauge: 1 while the relay session is up                                |
| `simplyblock_switchover_duration_seconds` | `cluster`, `to_site`, `planned` | Histogram: from the node's request to the promote's `200`             |
| `simplyblock_site_fence_duration_seconds` | `cluster`, `site`               | Histogram: from `FenceClusterNetwork` to `done`                       |
| `simplyblock_volumes_led_from_site`       | `cluster`, `site`               | Gauge: the hub's roll-up of where volumes are served                  |

Two of these carry alerts. `simplyblock_site_link_up` at zero for longer than the relay's redial budget is the one signal that the sites have lost each other while the control plane still answers both. `simplyblock_site_fence_duration_seconds` growing past the backend's probe window means the proof is refusing, which is a failover that is not completing and is otherwise invisible until the DRPC times out.

`StretchedCluster.status.sites[].siteRecord` carries the site records from `GET /clusters/{id}/sites`, so a console sees `draining`, `down`, `fencing` or `done` beside each member.

---

## 15. Testing Strategy

The plan is [`tests/test-plan-stretched-sync-replication-ramen.md`](../tests/test-plan-stretched-sync-replication-ramen.md). What each class must prove:

- **Unit.** The role machine on `StorageCluster` (leader creates, follower adopts, follower refuses cluster-scoped ops, role swap drops an in-flight ops); the VRG follower's selection of what to move and when (sync, primary, this cluster, bound claims, `Failover` alone forcing); the node gate's wait from the secret file and the status read; `SwitchoverService`'s sequencing and per-store serialization against a mock backend, including `409 volumes` and the `412` escalation; the driver's CIDR-to-site resolution and its refusals; the relay's sub-peer prefix check.
- **Integration (envtest, two managers).** Two operator managers against one mock control plane, driven by two `ClusterDeploymentConfig`s with the role fields: the follower reaches `Adopting` without posting a create, both reach the same UUID, nodes are added with the right site. The hub operator's two discovery, sizing, and approval flows from one `StretchedCluster`, against a fake work and view client.
- **Link.** A relay, two agents and two local peers in one process: a stream from `site/a/operator` to `site/b/controller/x` carries gRPC end to end; a sub-peer with a foreign prefix is refused; a dropped relay session redials and the registry recovers.
- **Live.** The lab of Appendix D: a relocate round trip under a hashed writer, an unplanned failover with the leader site powered off, the site's return and relocate back, and the follower-as-leader swap. The pass criteria are Ramen's own conditions on the DRPC and VRG plus the backend's site records, as `design-ramen-integration.md` §6.3 insists, with the data check from `design-csi-addons-replication.md` §12.

The regression focus is the gate and the fence: a node attaching a volume led from the other site, and a `NetworkFence` that returns success before the control plane records `done`, are the two failures that would make the feature silently unsafe, and both get a red-first test.

---

## 16. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                         | Owner           |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------|
| Q1  | **Hub availability.** The hub is the single witness and the control plane. Is a three-node hub cluster with FoundationDB on durable volumes the baseline, and is a third-site hub a recommendation or a requirement? This design treats it as a recommendation.                                                                                  | Product / Infra |
| Q2  | **A witness of its own.** The backend has no quorum and no self-fencing (`sync-replication.md` §8). Should the isolated site stop serving when it loses the hub and its peer, which the data plane could decide from its journal links? Out of scope here, but the full-partition row of §13 depends on it.                                      | Control plane   |
| Q3  | **Site fence route semantics.** Should `sites/{site}/fence` also promote every store led from the fenced site to the survivor, so a failover is one call, or stay a fence and leave the promotes to the stage as specified (§10.2)? The latter keeps the fence idempotent and the promotes per workload.                                         | Control plane   |
| Q4  | **The management API through the relay.** §7.5 makes it optional. Is there a deployment that cannot route the management API to the sites but can route the storage network? If not, drop the HTTP relay.                                                                                                                                        | Infra           |
| Q5  | **Pools and classes authored on a follower.** §5.2 lets either site author pools and classes. Should the hub be the only author for a stretched cluster, so the two sites' `StorageClass`es cannot drift? `StorageClassDeployment` already exists for that.                                                                                      | Operator team   |
| Q6  | **The `local` flag in the driver's cluster secret.** On a stretched cluster both sites' drivers mark the one cluster `local`. The async chain-walk never runs for a sync volume, but the flag's documented meaning ("a cluster of the site this driver runs on") now holds for both sites. Rename, or add `site` and leave `local` as is (§5.3)? | Driver team     |
| Q7  | **Relocate of a store shared across workloads.** The `409 volumes` refusal is operational today. Should the operator offer `SwitchoverService.Move(store)` as an explicit, all-volumes move for an administrator, outside Ramen?                                                                                                                 | Operator team   |
| Q8  | **Cross-site clones.** A clone inherits its source's store and so its home site (§9.5). Should the backend offer a clone whose home is the requesting site, at the cost of a full copy across the storage network, so that a restore on cluster B of an A-born snapshot is born on B? Until then B's first pod moves it.                         | Control plane   |

---

## Appendix A: `stretchedcluster_types.go`

In `operator/api/v1alpha2/`, beside `StorageSiteDeployment`, the hub-side request kind it follows. The type as it is written; the doc comments are the ones the shipped file carries. `StorageSiteSizing`, `StorageSiteDiscovery`, `StorageSiteDraft`, `StorageSiteCluster`, and `StorageSiteDeploymentPhase` are the types `storagesitedeployment_types.go` declares, reused unchanged. `.claude/skills/api-design/scripts/check-crds.py --design` audits this appendix.

`status.sites` is bounded at two items because each item embeds the draft's cluster template, whose own CEL rules the apiserver costs per item: an unbounded list made the CRD's estimated rule cost exceed the budget, and the CRD was refused at install. A webhook refuses a role swap while either site's projected `StorageCluster` is not `Online`, so a swap never lands on a cluster mid-creation (U-23).

```go
// StretchedRole is which of a stretched cluster's two sites owns the backend
// cluster's cluster-wide operations.
// +kubebuilder:validation:Enum=Leader;Follower
type StretchedRole string

const (
	// StretchedRoleLeader is the site whose operator creates the backend
	// cluster, activates it, and runs every cluster-wide operation.
	StretchedRoleLeader StretchedRole = "Leader"
	// StretchedRoleFollower is the site whose operator adopts the cluster the
	// leader created and manages only its own site's nodes.
	StretchedRoleFollower StretchedRole = "Follower"
)

// StretchedClusterPhase is the request's own progress across both sites.
// +kubebuilder:validation:Enum=Pending;Discovering;Drafted;LeaderCreating;FollowerJoining;Ready;Degraded;Failed
type StretchedClusterPhase string

const (
	// StretchedClusterPhasePending is a request no site has answered yet.
	StretchedClusterPhasePending StretchedClusterPhase = "Pending"
	// StretchedClusterPhaseDiscovering is at least one site still writing its
	// draft.
	StretchedClusterPhaseDiscovering StretchedClusterPhase = "Discovering"
	// StretchedClusterPhaseDrafted is both drafts written and awaiting approval.
	StretchedClusterPhaseDrafted StretchedClusterPhase = "Drafted"
	// StretchedClusterPhaseLeaderCreating is the approved leader's draft
	// expanding into the backend cluster.
	StretchedClusterPhaseLeaderCreating StretchedClusterPhase = "LeaderCreating"
	// StretchedClusterPhaseFollowerJoining is the backend cluster existing and
	// the follower adopting it and adding its nodes.
	StretchedClusterPhaseFollowerJoining StretchedClusterPhase = "FollowerJoining"
	// StretchedClusterPhaseReady is both sites' StorageClusters Online on the
	// same backend cluster.
	StretchedClusterPhaseReady StretchedClusterPhase = "Ready"
	// StretchedClusterPhaseDegraded is a ready cluster one of whose sites is
	// unreachable from the hub, recorded out by the control plane, or not
	// Online.
	StretchedClusterPhaseDegraded StretchedClusterPhase = "Degraded"
	// StretchedClusterPhaseFailed is a site that refused what the hub
	// delivered, or a draft that failed.
	StretchedClusterPhaseFailed StretchedClusterPhase = "Failed"
)

// StretchedSite is one of the two sites: a managed cluster, the backend site
// its nodes join, and its role.
type StretchedSite struct {
	// Cluster is the OCM ManagedCluster hosting this site. Its namespace on the
	// hub holds the site's ManifestWork and views. It identifies the entry, so
	// it cannot change.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Cluster string `json:"cluster"`

	// Site is the backend site name every storage node of this managed cluster
	// joins with. Immutable: it is a property of the installation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	// +k8s:immutable
	Site string `json:"site"`

	// Role is Leader or Follower. It may be swapped between the two sites after
	// creation, as one change to both entries, once both sites' clusters are
	// Ready.
	// +kubebuilder:validation:Required
	Role StretchedRole `json:"role"`

	// SiteNamespace is the simplyblock operator's namespace on the site, where
	// the discovery, the draft, and the StorageCluster live.
	// +kubebuilder:default=simplyblock
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SiteNamespace string `json:"siteNamespace,omitempty"`

	// Discover is the discovery this site runs: which workers are inspected.
	// Changing it before approval runs another discovery on this site only,
	// which rewrites this site's draft; after approval it is refused.
	// +optional
	Discover StorageSiteDiscovery `json:"discover,omitempty"`
}

// StretchedClusterSpec is the request for one stretched cluster.
// +kubebuilder:validation:XValidation:rule="self.sites.exists_one(s, s.role == 'Leader')",message="exactly one site must have role Leader"
// +kubebuilder:validation:XValidation:rule="self.sites[0].site != self.sites[1].site",message="the two sites must name different backend sites"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.sites) || oldSelf.sites.all(o, self.sites.exists(s, s.cluster == o.cluster))",message="the managed clusters of a stretched cluster cannot be replaced"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approved) || !oldSelf.approved || self.approved",message="approval is one-way: an approved stretched cluster cannot be un-approved"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approved) || !oldSelf.approved || oldSelf.sites.all(o, self.sites.exists(s, s.cluster == o.cluster && has(s.discover) == has(o.discover) && (!has(s.discover) || s.discover == o.discover)))",message="discover cannot change once the stretched cluster is approved"
type StretchedClusterSpec struct {
	// ClusterName is the backend cluster's name, and the name of the
	// StorageCluster on both sites. Immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +k8s:immutable
	ClusterName string `json:"clusterName"`

	// Sites are the two managed clusters and the site each represents. Exactly
	// one has role Leader.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	// +listType=map
	// +listMapKey=cluster
	Sites []StretchedSite `json:"sites"`

	// Sizing is written onto both sites' drafts once they name nodes: the
	// fields no discovery can read. Synchronous replication is enabled on both
	// drafts whatever is written here.
	// +optional
	Sizing *StorageSiteSizing `json:"sizing,omitempty"`

	// Images are the images both sites' storage nodes run, written onto both
	// drafts with the sizing. A stretched cluster's two sites run one data plane,
	// so the images are stated once for both. Unstated, each site's expansion
	// keeps its own defaults.
	// +optional
	Images *DeploymentImages `json:"images,omitempty"`

	// Approved is the review gate for both drafts. One-way. The leader's draft
	// is approved as soon as this is set; the follower's only once the leader's
	// cluster exists, so the follower never races the create.
	// +kubebuilder:default=false
	// +optional
	Approved bool `json:"approved"`
}

// StretchedClusterSiteStatus is what one site reports back, projected.
type StretchedClusterSiteStatus struct {
	// Cluster is the managed cluster this entry reports.
	Cluster string `json:"cluster"`

	// Site is the backend site of this managed cluster.
	// +optional
	Site string `json:"site,omitempty"`

	// Role is the role last delivered to this site.
	// +optional
	Role StretchedRole `json:"role,omitempty"`

	// Phase is this site's own progress, in the phases a StorageSiteDeployment
	// reports for one site.
	// +optional
	Phase StorageSiteDeploymentPhase `json:"phase,omitempty"`

	// Message is the reason this site's phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// WorkName is the ManifestWork carrying the request to this site.
	// +optional
	WorkName string `json:"workName,omitempty"`

	// Reachable reports whether the hub's last read of the managed cluster found
	// it available. An unreachable API server is not a lost site: the control
	// plane judges that from the storage nodes, not from the hub's connection.
	// +optional
	Reachable *bool `json:"reachable,omitempty"`

	// SiteRecord is the control plane's record of this site, as this site's
	// StorageCluster reports it: empty while the site is in service, otherwise
	// the state of its drain or fence.
	// +optional
	SiteRecord string `json:"siteRecord,omitempty"`

	// Draft is the site's draft as the site reports it.
	// +optional
	Draft *StorageSiteDraft `json:"draft,omitempty"`

	// StorageCluster is the StorageCluster the site's approved draft produced.
	// +optional
	StorageCluster *StorageSiteCluster `json:"storageCluster,omitempty"`
}

// StretchedClusterStatus is what both sites report back, projected.
type StretchedClusterStatus struct {
	// Phase is the request's own progress across both sites.
	// +optional
	Phase StretchedClusterPhase `json:"phase,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the request moves.
	// +optional
	Message string `json:"message,omitempty"`

	// ClusterUUID is the backend cluster, once the leader's StorageCluster
	// reports it.
	// +optional
	ClusterUUID string `json:"clusterUUID,omitempty"`

	// LostSite is the site the control plane records as out of service, as a
	// surviving site's StorageCluster reports it. Empty while both sites serve.
	// The roles can be swapped away from a lost leader once its fence is done.
	// +optional
	LostSite string `json:"lostSite,omitempty"`

	// Sites reports each site, keyed by its managed cluster.
	// +optional
	// +kubebuilder:validation:MaxItems=2
	// +listType=map
	// +listMapKey=cluster
	Sites []StretchedClusterSiteStatus `json:"sites,omitempty"`

	// Conditions: Delivered (both works are applied), Drafted (both drafts name
	// nodes), Approved (both drafts are approved), Ready (both StorageClusters
	// are Online on one backend cluster).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbstr
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterName"
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=".spec.approved"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.clusterUUID",priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StretchedCluster requests one synchronously replicated storage cluster whose
// two sites are two OCM managed clusters: a discovery and a draft on each site,
// the sizing of both drafts, and the approval that creates the cluster on the
// leading site and joins it from the following one. The hub carries the request
// through OCM and projects both sites into the status. Deleting the request
// leaves both sites' storage alone.
type StretchedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StretchedClusterSpec   `json:"spec,omitempty"`
	Status StretchedClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StretchedClusterList contains a list of StretchedCluster.
type StretchedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StretchedCluster `json:"items"`
}
```

## Appendix B: `storagecluster_types.go`, `storagenode_types.go`, and `clusterdeploymentconfig_types.go` additions

In `operator/api/v1alpha2/`, the storage version; `api/v1alpha1` round-trips every field below through its conversion annotations, so a v1alpha1 read-modify-write cannot drop a site or a role. `enableSyncReplication` on `StorageClusterSpec` and `ClusterTemplate`, `site` on `NodeGroup`, and `site` on `StorageNodeConfig` are carried from PR #673 unchanged; a node's site is `spec.config.site`, not a second field on the stretched block.

On `StorageClusterSpec`, with two rules on the spec:

```go
// +kubebuilder:validation:XValidation:rule="!has(self.stretched) || (has(self.enableSyncReplication) && self.enableSyncReplication)",message="stretched requires enableSyncReplication to be true"
// +kubebuilder:validation:XValidation:rule="has(self.stretched) == has(oldSelf.stretched)",message="stretched cannot be added to or removed from an existing cluster"
type StorageClusterSpec struct {
	// ... the existing fields, then:

	// Stretched marks this StorageCluster as one site of a synchronously
	// replicated cluster whose two sites are two Kubernetes clusters. Every node
	// this StorageCluster adds joins stretched.site. A leader creates the backend
	// cluster; a follower adopts the one its leader created and never creates or
	// deletes it. Delivered by the hub with the site's deployment; a standalone
	// cluster leaves it unset. Requires enableSyncReplication, and cannot be added
	// to or removed from an existing cluster.
	// +optional
	Stretched *StretchedSpec `json:"stretched,omitempty"`
}

// StretchedSpec is one site's part of a stretched cluster: the backend site its
// nodes join, the other site's name, and the role it plays.
// +kubebuilder:validation:XValidation:rule="self.site != self.peerSite",message="site and peerSite must differ"
type StretchedSpec struct {
	// Site is the backend site every node this StorageCluster adds belongs to.
	// Immutable: a site is a property of the installation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	// +k8s:immutable
	Site string `json:"site"`

	// PeerSite is the other site's backend name. Nodes the control plane reports
	// on it are listed here as remote StorageNodes. Immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	// +k8s:immutable
	PeerSite string `json:"peerSite"`

	// Role is Leader or Follower. A leader creates and activates the backend
	// cluster and runs every cluster-wide operation; a follower adopts the
	// cluster and manages only its own site's nodes. A change is a role swap,
	// delivered from the hub.
	// +kubebuilder:validation:Required
	Role StretchedRole `json:"role"`
}

// StretchedStatus is this site's view of its stretched cluster, read from the
// control plane.
type StretchedStatus struct {
	// Role is the role this site's operator last acted on. It differs from
	// spec.stretched.role only while a role swap is being taken up.
	// +optional
	Role StretchedRole `json:"role,omitempty"`

	// LostSite is the site the control plane records as out of service, empty
	// while both sites serve.
	// +optional
	LostSite string `json:"lostSite,omitempty"`

	// SiteRecord is how far the control plane's handling of lostSite got, in the
	// control plane's own words: draining, down, fencing, or done. Empty while
	// both sites serve.
	// +optional
	SiteRecord string `json:"siteRecord,omitempty"`

	// PeerNodes is the number of the other site's storage nodes the control
	// plane lists.
	// +optional
	PeerNodes int32 `json:"peerNodes,omitempty"`

	// PeerNodesOnline is how many of them are online.
	// +optional
	PeerNodesOnline int32 `json:"peerNodesOnline,omitempty"`
}
```

On `StorageClusterStatus`:

```go
type StorageClusterStatus struct {
	// ... the existing fields, then:

	// Stretched is this site's view of its stretched cluster, read from the
	// control plane; set only when spec.stretched is.
	// +optional
	Stretched *StretchedStatus `json:"stretched,omitempty"`
}
```

On `ClusterTemplate`, expanded verbatim into the `StorageCluster`:

```go
type ClusterTemplate struct {
	// ... the existing fields, then:

	// Stretched makes the cluster one site of a stretched cluster, whose two
	// sites are two Kubernetes clusters: the site this document's nodes join,
	// the other site, and whether this site creates the cluster or adopts it.
	// Written by the hub that manages both sites, and expanded verbatim into the
	// StorageCluster. Requires enableSyncReplication; every node group's site
	// must equal stretched.site.
	// +optional
	Stretched *StretchedSpec `json:"stretched,omitempty"`
}
```

On `StorageNodeSpec`, `workerNode` widens from required to required-unless-remote, which admits everything it admitted before. The rule tests `size(self.workerNode) > 0` rather than comparing with an empty string literal, because gofmt rewrites a pair of single quotes in a doc comment into a typographic closing quote and the marker sits in the type's doc comment:

```go
// +kubebuilder:validation:XValidation:rule="(has(self.stretched) && self.stretched.remote) || (has(self.workerNode) && size(self.workerNode) > 0)",message="workerNode is required for a node of this site"
// +kubebuilder:validation:XValidation:rule="has(self.stretched) == has(oldSelf.stretched)",message="stretched cannot be added to or removed from an existing node"
type StorageNodeSpec struct {
	// ... the existing fields, with workerNode now:

	// WorkerNode is the Kubernetes worker hostname this node runs on. It is not
	// immutable, because a migration re-points it, but a change made by anything
	// outside the operator's namespace is rejected. Required for every node of
	// this site; a remote node (stretched.remote) runs on no worker of this
	// cluster and leaves it empty.
	// +optional
	WorkerNode string `json:"workerNode,omitempty"`

	// ... and, last:

	// Stretched marks a node of a stretched cluster that is listed here but
	// belongs to the other site. Unset on every node of this site and on a
	// standalone cluster. Cannot be added or removed after creation.
	// +optional
	Stretched *StretchedNodeSpec `json:"stretched,omitempty"`
}

// StretchedNodeSpec describes a node of a stretched cluster's other site.
type StretchedNodeSpec struct {
	// Remote marks a node of the other site, listed on this cluster from the
	// control plane so that either site shows every node. A remote node runs no
	// workload on this cluster, admits no StorageNodeOps, and is recreated from
	// the control plane if deleted; its own site's operator manages it.
	// Immutable.
	// +optional
	// +k8s:immutable
	Remote bool `json:"remote,omitempty"`
}
```

Print columns: `StorageNode` gains `Site` (`.spec.config.site`) and `Remote` (`.spec.stretched.remote`), and `StorageCluster` gains `Site`, `Role`, `Peer-Online`, and `Site-Record`, all at priority 1 so a standalone cluster's default listing is unchanged. `StorageDevice` needs no spec change: it is remote when its `spec.nodeRef` names a remote node. `SimplyblockDriver` gains no field: the node plugin reads its site from the CSI cluster-secret entry the `StorageCluster` controller composes (§5.3), since a spec field written by a controller would break the rule that spec is the user's.

## Appendix C: `site_link.proto`

`RelayService` is `atlas-lib/link/sitev1/relay.proto`, `SwitchoverService` is `switchover.proto`, and `SiteService` is `site.proto` (package `atlas.link.site.v1`), beside the link's own handshake in `linkv1`.

```proto
package simplyblock.link.site.v1;

// Served by the relay on the hub. A peer opens a stream to another peer; the
// relay splices it to the target's session and parses nothing further. The
// stream then carries whatever gRPC the target registered.
service RelayService {
  rpc Open(stream OpenRequest) returns (stream OpenResponse);
}
// The caller's first frame names the two ends; every frame after it, in both
// directions, is data. A hop that refuses the stream answers with a status
// before any data moves, and an accepting target's first answer is empty.
message OpenRequest {
  oneof kind {
    StreamEnds ends = 1;
    bytes data = 2;
  }
}
message OpenResponse {
  bytes data = 1;
}
// target and source are "site/<site>/<kind>[/<name>]"; a peer leaves source
// empty and its forwarder sets it from the peer's verified identity.
message StreamEnds {
  string target = 1;
  string source = 2;
}

// Served by each site's operator: called in process by its VRG follower
// (§9.2), and remotely through the relay by the other site's operator.
service SwitchoverService {
  rpc Move(MoveRequest) returns (MoveResponse);
}
message MoveRequest {
  string volume = 1;        // the volume handle, cluster:pool:volume
  string to_site = 2;
  bool allow_forced = 3;    // the 412 escalation; always true from a node stage
}
message MoveResponse {
  enum State { STATE_UNSPECIFIED = 0; STATE_MOVED = 1; STATE_IN_PROGRESS = 2; STATE_REFUSED = 3; }
  State state = 1;
  string led_from = 2;
  string reason = 3;        // the backend's message on REFUSED
  repeated string blocking_volumes = 4;  // the 409 "volumes" list
}

// Served by each site's operator to the other's (§11.3). A role swap is the
// hub's, through the works it delivers, so no RPC assumes a role.
service SiteService {
  rpc Describe(DescribeRequest) returns (SiteDescription);  // site, role, cluster, own nodes
  rpc RefreshNode(RefreshNodeRequest) returns (RefreshNodeResponse);
}
message RefreshNodeRequest {
  string cluster_uuid = 1;
  string node = 2;          // the backend node UUID
  int64 since_unix = 3;     // a pod created after this is the refresh
}
message RefreshNodeResponse {
  enum State { STATE_UNSPECIFIED = 0; STATE_REFRESHED = 1; STATE_IN_PROGRESS = 2; STATE_NOT_FOUND = 3; }
  State state = 1;
  string message = 2;
}
```

The sub-peer naming and the authentication of each hop are in §7.3 and §7.4. The relay enforces that `Open` from `site/<m>/...` names a target outside `site/<m>/...` only when the caller is an operator peer; a node plugin reaches only its own operator.

## Appendix D: Lab topology

`regression_test/22`'s dual topology maps onto this design with one change of placement: cluster A doubles as the OCM hub today and would host the control plane with a `ControlPlane` of `source.local`; a dedicated hub, even a single node, is preferable so that stopping site A is a real site loss. The storage cluster is one `StretchedCluster` on the hub instead of two hand-written `ClusterDeploymentConfig`s, sites `a` and `b` on the two K3s clusters, `.66` to `.69` and `.111` to `.114`. MinIO stays on the hub for Ramen's metadata. The management network is the lab's flat `192.168.10.0/24`, so P0-3 holds trivially; the `DRCluster` CIDRs are each site's node addresses as `/32`s.

The sequence a run exercises: register both managed clusters, apply the `StretchedCluster`, review both drafts in its status, and approve; watch the leader create and the follower adopt; apply the `StorageClassDeployment` and the metro `DRPolicy`; protect a workload; relocate it; power off site A and fail over; power A back on, watch the site return, unfence, relocate back; swap the roles and run a rolling restart from the new leader.
