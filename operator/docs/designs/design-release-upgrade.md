# Design Document: Release Upgrades

**Status:** Draft  
**Author:** michael  
**Date:** 2026-11-26 (last updated 2026-11-27)  
**Test Plan:** [`tests/test-plan-release-upgrade.md`](../tests/test-plan-release-upgrade.md)

---

## Phasing Overview

| Phase       | Status                                                                           | Scope                                                                                                | Sections    |
|-------------|----------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------|-------------|
| **Phase 1** | In review ([#998](https://github.com/simplyblock/simplyblock-operator/pull/998)) | A storage-node restart carries the SPDK images, and the control plane reports the images a node runs | §5, Phase 0 |
| **Phase 2** | In review ([#998](https://github.com/simplyblock/simplyblock-operator/pull/998)) | `RollingRestart` converges the fleet on the desired images and resumes where it stopped              | §4.3, §5.4  |
| **Phase 3** | In review ([#998](https://github.com/simplyblock/simplyblock-operator/pull/998)) | `SwitchWriteProtection` as a `StorageClusterOps` action                                              | §4.4        |
| **Phase 4** | Planned                                                                          | Release modules in `simplyblock-upgrade`, selected by source and target release                      | §6          |
| **Phase 5** | Planned                                                                          | The upgrade runs from the chart as a pre-upgrade hook Job and a rollout Job                          | §7          |

Each phase is useful on its own. Phases 1 and 2 already remove the manual image
handling from a rolling upgrade, phase 3 removes the last step that needs `sbctl`,
and phases 4 and 5 turn the documented sequence into one that runs itself. The
user documentation follows phase 5, so that it describes what the tooling does.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                   | Kind                    | Blocks                         | Status      |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|--------------------------------|-------------|
| P0-1 | `POST /api/v2/clusters/{id}/storage-nodes/{node}/restart` accepts `spdk_image` and `spdk_proxy_image` and passes them to `restart_storage_node`                | Control plane (`sbcli`) | Phase 1 (§5.2)                 | Not shipped |
| P0-2 | The storage-node representation of API v2 reports `spdk_image` and `spdk_proxy_image`, the images the control plane starts the node with                       | Control plane (`sbcli`) | Phase 1 (§5.3), Phase 2 (§5.4) | Not shipped |
| P0-3 | The cluster representation of API v2 reports the default SPDK and SPDK proxy images (`default_spdk_image`, `default_spdk_proxy_image`)                         | Control plane (`sbcli`) | Phase 1 (§5.1)                 | Not shipped |
| P0-4 | API v2 exposes the write-protection switch that `sbctl cluster switch-write-protection` performs, and the cluster representation reports `write_protection_v2` | Control plane (`sbcli`) | Phase 3 (§4.4)                 | Not shipped |
| P0-5 | An SPDK image states which control-plane sizing it supports, readable without starting it                                                                      | Storage plane (SPDK)    | The capability check of §6.6   | Unknown     |

Without P0-1 a restart cannot move a node to a new image, which is the gap this
design exists to close, so phase 1 is not shippable without it. Without P0-2 the
operator cannot tell a converged node from one that is not: phase 2 then has to
restart every node, as `RollingRestart` does today, and its verification step
reports the image as unknown instead of failing. Without P0-3 a cluster that
leaves its SPDK image unset keeps whatever each node was added with, and the
upgrade Job has to set the image on the cluster explicitly (§5.1). Without P0-4
the write-protection switch stays a manual `sbctl` step of the documented
sequence. Without P0-5 the capability check of §6.6 compares image tags against
a table the release module carries, which is weaker but sufficient.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Changes](#4-api-changes)
5. [Storage-Plane Image Convergence](#5-storage-plane-image-convergence)
6. [Release Modules](#6-release-modules)
7. [Running the Upgrade](#7-running-the-upgrade)
8. [Idempotency and Resume](#8-idempotency-and-resume)
9. [Backend API Requirements](#9-backend-api-requirements)
10. [Configuration](#10-configuration)
11. [Failure Modes](#11-failure-modes)
12. [Observability](#12-observability)
13. [Testing Strategy](#13-testing-strategy)
14. [Migration Strategy](#14-migration-strategy)
15. [Open Questions](#15-open-questions)

Appendices:

- [Appendix A: `storagecluster_types.go`](#appendix-a-storagecluster_typesgo)
- [Appendix B: `storagenode_types.go`](#appendix-b-storagenode_typesgo)
- [Appendix C: `storageclusterops_types.go`](#appendix-c-storageclusterops_typesgo)

---

## Overview

A release upgrade moves a deployment from the release it runs, the source, to
the release a chart or a binary carries, the target. It always has the same
skeleton: check that the deployment can take the upgrade, apply the target's
CRDs, upgrade the operator, upgrade the control plane, and roll the storage plane
onto the target's images node by node. What differs between upgrades is what
has to happen around that skeleton. Upgrading from `26.4.0-beta6` needs a lblk
reweight finished first and the old pNFS MDS state removed, and a later upgrade
will need something else.

The design has three layers. **The operator's operations carry the skeleton.**
`ControlPlaneOps` with the action `Upgrade` already moves the control plane.
`StorageClusterOps` with the action `RollingRestart` already walks the nodes,
and this design makes each restart carry the node's desired SPDK images (§5),
lets a walk restart only the nodes not yet on them (§4.3), and adds the
write-protection switch as an action (§4.4).

**Release modules carry the differences.** A release module is a Go package in
`operator/internal/upgrade/releases`, one per release that needs something done
when a deployment upgrades to it. It contributes checks and steps at named hook
points of the skeleton. `simplyblock-upgrade` detects the source release, takes
its own version as the target, and runs every module whose release lies after
the source and at or before the target, in release order (§6).

**The chart runs the upgrade.** A pre-upgrade hook Job runs the checks and
applies the CRDs before Helm changes anything, so a deployment that cannot take
the upgrade is refused while it is still untouched. A rollout Job then drives the
operations above, one after the other, and holds no state of its own: the
operations are the record of how far the upgrade got (§7, §8).

---

## 1. Background

The documented upgrade of a Kubernetes deployment
(`docs/kubernetes/operations/cluster/cluster-upgrade.md` in the documentation
repository) has four steps: apply the CRDs of the new chart version, upgrade the
Helm release, upgrade the control plane with a `ControlPlaneOps`, and roll the
storage plane with a `StorageClusterOps` `RollingRestart` with `refreshSNodeAPI`
set. Every step is performed by hand, and five things the procedure relies on do
not hold.

**A restart does not change a node's SPDK image.** The control plane stores the
SPDK image per node in FoundationDB when the node is added, and
`restart_storage_node` starts the node with `spdk_image or snode.spdk_image or
constants.SIMPLY_BLOCK_SPDK_ULTRA_IMAGE`. The restart endpoint of API v2 takes no
image, and the operator reads `StorageNode.spec.config.spdkImage` only when it
adds the node (`addParams` in
`operator/internal/controllers/node/storagenode_controller.go`). A rolling
restart therefore restarts every node on the image it was added with, and the
only call that moves an existing node to another image is
`POST /api/v2/clusters/{id}/update` with `spdk_image` and `restart`, which the
operator never calls and which restarts every node with `force` and without a
peer check.

**A rolling restart restarts everything, every time.** `planWalk` in
`operator/internal/controllers/cluster/rollingrestart.go` plans every node that
is not leaving the cluster. A walk that failed on node seven of twenty restarts
nodes one to six again when it is rerun.

**The write-protection switch is a CLI command.** An upgraded cluster keeps v1
distrib write protection until `sbctl cluster switch-write-protection` runs.
The switch sends a runtime RPC to every online node and rewrites the stored
distrib parameters of every node, so an offline node comes back on v2 at its
next restart and no node needs a restart for it (`switch_write_protection` in
`simplyblock_core/cluster_ops.py`). API v2 has no endpoint for the switch, so a
Kubernetes deployment cannot perform it through the operator.

**The chart moves the control plane directly.** `_bootstrap_controlplane.tpl`
renders `spec.source.local.image` from `image.simplyblock`, and the operator
applies what the chart rendered. A `helm upgrade` that raises that value rolls
the management API without the drain and the verification of a `ControlPlaneOps`
`Upgrade`, and does so at the same time as the operator and the node agents roll.

**`simplyblock-upgrade` knows one upgrade.** The tool in
`operator/cmd/simplyblock-upgrade` and its framework in `operator/internal/upgrade`
(`design-api-upgrade.md` in `crd-redesign`) perform the `v1alpha1` to `v1alpha2`
API migration. Its catalog is one list, and nothing in it depends on which
release a deployment runs or which it is moving to. The checks a particular
upgrade needs, such as the five steps the `26.4.0-beta6` upgrade analysis found,
exist only as prose.

---

## 2. Goals and Non-Goals

### Goals

- A storage-node restart, by any operation, starts the node on its desired SPDK
  and SPDK proxy images, and the node's status reports the images it runs.
- A rolling restart can restart only the nodes whose running images differ from
  the desired ones, so a rerun continues where a failed walk stopped.
- The write-protection switch is an operation of the operator.
- The checks and steps an upgrade needs are selected by source and target
  release, from release modules that live apart from the generic sequence, so
  that adding what a new release needs is adding one package.
- `helm upgrade` refuses a deployment that cannot take the upgrade before it
  changes anything, and otherwise upgrades the deployment without further manual
  steps.
- The upgrade resumes after any interruption, whether of the Job, the operator,
  or a node, without repeating completed work.
- The same commands run by hand, for a deployment that does not upgrade through
  Helm hooks.

### Non-Goals

- **Downgrades.** A rollback stays the documented manual procedure. The release
  modules describe forward paths only.
- **OLM-installed deployments as a hook.** OLM has no pre-upgrade hook. They run
  the commands by hand (§7.5), which is the path `design-api-upgrade.md` §13.2
  already describes for them.
- **Deployments outside Kubernetes.** A Docker or bare-metal deployment upgrades
  with `sbctl cluster update`, which this design does not change.
- **The `v1alpha1` API migration.** A deployment still storing `v1alpha1` runs
  `simplyblock-upgrade upgrade` and `migrate` first, as `design-api-upgrade.md`
  describes. The release modules start where that migration ends (§6.1).
- **On-disk format migrations of the storage plane.** A change to the alceml or
  journal formats is the storage plane's to migrate. A release module can only
  refuse an upgrade that needs one.
- **Upgrading a managed control plane.** A deployment whose control plane is
  `spec.source.managed` upgrades it with the hub that owns it. The upgrade here
  checks that the managed control plane is new enough and leaves it alone.

---

## 3. Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────┐
│                     helm upgrade (target chart)                        │
│                                                                        │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │ pre-upgrade hook Job: simplyblock-upgrade release prepare        │  │
│  │  1. detect source (Helm release), target (own version)           │  │
│  │  2. generic checks + checks of the selected release modules      │  │
│  │  3. BeforeCRDs steps, apply the embedded CRDs, AfterCRDs steps   │  │
│  └──────────────────────────────────────────────────────────────────┘  │
│        │ succeeds: Helm upgrades the operator, CSI, node agents        │
│        ▼                                                               │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │ rollout Job: simplyblock-upgrade release rollout                 │  │
│  │  4. wait for the operator at the target version                  │  │
│  │  5. ControlPlaneOps Upgrade           (AfterControlPlane steps)  │  │
│  │  6. per StorageCluster: StorageClusterOps RollingRestart         │  │
│  │     selection OutOfDate               (AfterStoragePlane steps)  │  │
│  │  7. verify                                                       │  │
│  └──────────────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────────┘
          │ creates and waits for Ops objects
┌─────────▼──────────────────────────────────────────────────────────────┐
│                     simplyblock operator (target)                      │
│  ControlPlaneOpsReconciler     Preflight → Draining → Applying → …     │
│  StorageClusterOpsReconciler   RollingRestart, SwitchWriteProtection   │
│  StorageNodeReconciler         desired images, status.spdkImage        │
└────────────────────────────────────────────────────────────────────────┘
          │ HTTP (API v2)
┌─────────▼──────────────────────────────────────────────────────────────┐
│                     simplyblock control plane                          │
│  POST …/storage-nodes/{node}/restart   {spdk_image, spdk_proxy_image}  │
│  GET  …/storage-nodes/{node}           spdk_image, spdk_proxy_image    │
│  POST …/clusters/{id}/switch-write-protection (P0-4)                   │
└────────────────────────────────────────────────────────────────────────┘
```

**The Jobs decide the order, and the operator does the work.** Neither Job
restarts a node, writes an image into a workload, or calls the control plane
except to read. Everything that changes the storage plane is an operation the
operator already guards with its locks, peer checks, and deadlines, so a
release upgrade has the same safety properties as the same operations started
by hand.

**The pre-upgrade hook runs the target's binary.** Helm runs a hook with the
images of the chart being installed, so the checks and the CRDs are always the
target's. This is the same constraint that keeps `simplyblock-upgrade` out of the
operator image (`design-api-upgrade.md` §29.4): the tool has to run before the
operator it upgrades.

---

## 4. API Changes

The changes are additions to three kinds whose full types are specified in
`crd-redesign/design-storagecluster.md` and `crd-redesign/design-storagenode.md`.
The appendices hold the changed blocks, whole, in the form they land in.

### 4.1 StorageCluster: Cluster-Wide SPDK Images

`StorageNode.spec.config.spdkImage` and `spdkProxyImage` exist per node, which is
what makes a phased rollout expressible. A whole-fleet upgrade needs one place
to state the images for every node of a cluster, so `StorageNodesSpec` gains:

```go
// SpdkImage is the SPDK image every storage node of this cluster runs, unless
// the node's own spec.config.spdkImage overrides it.
// +optional
SpdkImage crdtypes.ImageReference `json:"spdkImage,omitempty"`

// SpdkProxyImage is the SPDK proxy image every storage node of this cluster
// runs, unless the node's own spec.config.spdkProxyImage overrides it.
// +optional
SpdkProxyImage crdtypes.ImageReference `json:"spdkProxyImage,omitempty"`
```

`StorageClusterStatus` gains `writeProtection`, the version the control plane
reports (§4.4), and `defaultSpdkImage` and `defaultSpdkProxyImage`, the images
the control plane starts a node with when nothing names one (P0-3). The cluster
reconciler copies all three from the cluster record, so a node's reconciler
resolves its desired images from objects it already reads (§5.1). A control
plane that does not report the generation leaves `writeProtection` empty rather
than `V1`.

### 4.2 StorageNode: The Images a Node Runs

`StorageNodeStatus` gains `spdkImage` and `spdkProxyImage`, copied from what the
control plane reports for the node (P0-2), and `desiredSpdkImage` and
`desiredSpdkProxyImage`, the resolution of §5.1. A node whose two pairs differ is
out of date: `imagesCurrent` is `false`, and the node's reconciler emits
`ImageOutOfDate` when it becomes so. `imagesCurrent` is unset when the control
plane reports no images, because nothing is known then, and `true` for a node
nobody asks an image of. The printer column `Images` shows it, so `kubectl get
storagenodes` lists which nodes a rollout has not reached. `StorageNodeStatus`
carries no conditions list, so the answer is a field of its own rather than a
condition.

### 4.3 StorageClusterOps: RollingRestart Selection

`RollingRestartSpec` gains `selection`:

```go
// Selection decides which nodes the walk restarts. All restarts every node
// that is not leaving the cluster. OutOfDate restarts only the nodes whose
// running images differ from their desired images, and is what a release
// upgrade uses, because a rerun then continues where a stopped walk ended.
// +kubebuilder:validation:Enum=All;OutOfDate
// +kubebuilder:default=All
// +optional
Selection RollingRestartSelection `json:"selection,omitempty"`
```

The default keeps today's behavior. Every restart of the walk carries the node's
desired images whatever the selection, so `All` converges the fleet too, and
`OutOfDate` only spares the nodes that already have.

`RollingRestartStatus.Skipped` keeps its meaning (nodes passed over without a
restart). A node passed over because it is already current is recorded in the
new `current` list instead, because a node that left the cluster and a node that
needed nothing are different outcomes for a reader of the walk.

### 4.4 StorageClusterOps: SwitchWriteProtection

`StorageClusterOpsAction` gains `SwitchWriteProtection`. It moves a cluster's
distribs from v1 to v2 write protection, which is the state every cluster
created by a current control plane starts in and which an upgraded cluster
reaches only through this switch.

```
SwitchWriteProtection
    CheckingPeers ──► Switching ──► Completed
```

| Step            | Side effect on entry                                      | Complete when                             |
|-----------------|-----------------------------------------------------------|-------------------------------------------|
| `CheckingPeers` | None                                                      | Every node of the cluster is `online`     |
| `Switching`     | The write-protection call of P0-4, repeated after a `409` | The cluster reports `writeProtection: V2` |

`CheckingPeers` holds while any node that is not leaving the cluster is not
`online`. A cluster that already reports `V2` succeeds without a call, and one
whose control plane does not report the generation fails, because that control
plane predates the switch. The action acts on the backend cluster as a whole, so
on a stretched cluster it is refused on the follower site like the other
cluster-wide actions, and the operation cannot be deleted while it is at
`Switching`. The switch restarts
nothing and needs no restart afterward: the control plane moves every online
node's distribs at runtime and stores v2 in every node's distrib parameters, so
a node that was offline comes back on v2. The control plane answers `409` when
no node is online or a node rejected the switch, and leaves the cluster on v1.
The step then holds and retries the call, which is safe because the switch is
idempotent.

---

## 5. Storage-Plane Image Convergence

### 5.1 The Desired Images

A node's desired SPDK image is the first that is set of:

1. `StorageNode.spec.config.spdkImage`
2. `StorageCluster.spec.storageNodes.spdkImage`
3. `StorageCluster.status.defaultSpdkImage`, the control plane's default (P0-3)

The SPDK proxy image resolves the same way from the proxy fields. The third
source is what makes an upgrade need no image handling at all for a cluster that
states no image: upgrading the control plane changes its default, the nodes'
desired images follow, and the rolling restart moves them. It mirrors
`StorageCluster.spec.storageNodes.image`, which already defaults to the control
plane's own image.

The `StorageNode` reconciler writes the resolution into
`status.desiredSpdkImage` and `status.desiredSpdkProxyImage` whenever one of the
three changes. Without P0-3 the third source is empty, the desired image is
empty, and a restart carries no image, which is today's behavior.

### 5.2 Every Restart Carries the Desired Images

The operator calls the restart endpoint from the `RestartingNode` step of
`RollingRestart` (`restartNode` in `rollingrestart.go`) and from three
`StorageNodeOps` actions: `Restart`, the restart that ends `HostMaintenance`, and
the restart that relocates a node in `Migrate`. Every one passes `spdk_image` and
`spdk_proxy_image` from the node's `status.desiredSpdkImage` and
`status.desiredSpdkProxyImage`, and omits a field whose desired value is empty. The control plane stores what a
restart starts the node with, so its own restarts later, such as the automatic
restart after a node failure, keep the image the operator last asked for.

### 5.3 Verifying the Result

`RestartingNode` completes when the node is `online`, as it does today. A walk
then compares the node's reported images with its desired ones. A mismatch fails
the operation with the message
`node <uuid> came back on <image>, expected <image>`, because a node the control
plane silently started on a different image is not a node the walk may count as
converged. The operation emits `ImageMismatch`. When the control plane reports
no image (P0-2 missing), the comparison is skipped and the walk carries on.

### 5.4 OutOfDate Selection and Resume

With `selection: OutOfDate`, `planWalk` plans the nodes whose `imagesCurrent`
is `false`, in the order it plans them today. Each node is checked
again when the walk reaches it, and a node that became current in the meantime
(restarted by another operation or by the control plane) is passed over and
recorded in `current`. A walk that stopped on node seven of twenty and is
started again plans only the nodes still out of date, which is the resume
property the release upgrade relies on (§8).

A cluster with no out-of-date node succeeds with an empty walk and the message
`every node runs its desired images`. When P0-2 is missing, `imagesCurrent` is
unset for every node, and the operation fails when the walk is planned, naming
`selection All` as the one that works without images, before any node is touched.

---

## 6. Release Modules

### 6.1 Releases and Their Identity

A release is a chart version, `26.4.0-beta6.2` for example, compared as a
semantic version with its prerelease. The target is the version stamped into the
`simplyblock-upgrade` binary at build time, which is the chart version of the
release the binary belongs to. The source is read, in this order, from:

1. The chart version of the deployed Helm release of the operator chart
   (`operator/internal/upgrade/release` already decodes the release Secret).
2. The `--from` flag, for a deployment without a Helm release.

A source that stores any kind at `v1alpha1` is refused with the instruction to
run the API migration first, because every release module assumes `v1alpha2`.
A source newer than the target is refused as a downgrade, and a source equal to
the target succeeds with nothing to do.

### 6.2 The Module Contract

A release module is a package under `operator/internal/upgrade/releases`, named
for its release (`r26_4_0_beta7` for `26.4.0-beta7`), that exports one value:

```go
// Module is what one release needs when a deployment upgrades to it.
type Module struct {
	// Release is the release this module belongs to. The module runs when a
	// deployment upgrades across it: source < Release <= target.
	Release semver.Version

	// MinimumSource is the oldest release a deployment may upgrade from in one
	// step that crosses this release. An older source is refused with the
	// instruction to upgrade to MinimumSource first. Zero means no floor.
	MinimumSource semver.Version

	// Checks run in the preflight. A finding of severity Error refuses the
	// upgrade.
	Checks []upgrade.Check

	// Steps run at their hook point (§6.3), after the generic work of that
	// point.
	Steps map[HookPoint][]upgrade.Step
}
```

Checks and steps are the framework's existing units (`design-api-upgrade.md`
§29.5 and §29.6), so a release module's step has the same five-question contract
as a migration step: described, skipped when its effect is already present,
validated, applied, and verified. A module is registered in
`operator/internal/upgrade/releases/releases.go`, one line per release in
release order, which is the list somebody reads to find out what any upgrade
does.

### 6.3 Hook Points

The generic sequence (§6.5) has five hook points, and a module's step runs at
exactly one of them:

| Hook point          | Runs in          | When                                                                          |
|---------------------|------------------|-------------------------------------------------------------------------------|
| `BeforeCRDs`        | Pre-upgrade hook | After the checks, before the target's CRDs are applied                        |
| `AfterCRDs`         | Pre-upgrade hook | After the CRDs are applied and established, before Helm upgrades the operator |
| `AfterOperator`     | Rollout Job      | After the operator runs the target version                                    |
| `AfterControlPlane` | Rollout Job      | After the control plane runs the target version                               |
| `AfterStoragePlane` | Rollout Job      | After every cluster's rolling restart succeeded                               |

A step that has to run while the old operator is still running, such as removing
an object the new operator would otherwise adopt, belongs to `BeforeCRDs` or
`AfterCRDs`. A step that needs a new operator action belongs to a rollout hook
point.

### 6.4 Selecting the Modules

`simplyblock-upgrade` selects every registered module with
`source < Release <= target` and runs them in release order at each hook point.
An upgrade that crosses three releases therefore runs the checks of all three in
the preflight, and at `AfterStoragePlane` runs the first release's steps, then
the second's, then the third's. The upgrade is refused when any selected
module's `MinimumSource` is newer than the source, and the message names the
newest such floor as the release to upgrade to first.

A release with nothing special needs no module. The generic sequence is complete
on its own, and a module exists only for what the generic sequence does not do.

### 6.5 The Generic Sequence

Every upgrade runs these, and the modules add to them:

| #   | Work                                                                                                                                                                                                                 | Hook point after it |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|
| 1   | Checks: every `StorageCluster` is `Active`, every node is `online`, no `ControlPlaneOps`, `StorageClusterOps`, or `StorageNodeOps` is `Running`, every target image is pullable, and no kind is stored at `v1alpha1` |                     |
| 2   | Plus every selected module's checks                                                                                                                                                                                  | `BeforeCRDs`        |
| 3   | Apply the embedded CRDs with a server-side apply (field manager `simplyblock-upgrade`, conflicts forced) and wait until each is `Established`                                                                        | `AfterCRDs`         |
| 4   | Wait until the operator Deployment runs the target version and is available                                                                                                                                          | `AfterOperator`     |
| 5   | For a local control plane, a `ControlPlaneOps` `Upgrade` to the target's `simplyblock` image, and wait for `Succeeded`. For a managed one, check `ControlPlane.status.version` is at least the target's              | `AfterControlPlane` |
| 6   | For each `StorageCluster`, one at a time: a `StorageClusterOps` `RollingRestart` with `selection: OutOfDate` and `refreshSNodeAPI: true`, and wait for `Succeeded`                                                   | `AfterStoragePlane` |
| 7   | Verify: every node `online`, `imagesCurrent` true, every cluster `Active` and not rebalancing                                                                                                                        |                     |

Steps 1 to 3 are the pre-upgrade hook, and steps 4 to 7 the rollout Job.

### 6.6 Example: Upgrading from 26.4.0-beta6

The analysis of an upgrade from `26.4.0-beta6.2` to the current `main` found five
things the generic sequence does not do. They become the module of the first
release after `26.4.0-beta6` (named here `r26_4_0_beta7`, see §15):

| Rule                         | Kind  | Hook point          | What it does                                                                                                                                                                                                                                                                    |
|------------------------------|-------|---------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `reweight-pending-clear`     | Check |                     | Refuses while any lblk cluster reports a pending reweight, and names `cluster grow-devices` on the source release as the fix, because the target no longer completes one                                                                                                        |
| `spdk-image-supports-sizing` | Check |                     | Refuses a target SPDK image that predates the hugepage and system-memory budgets of the target control plane (ultra #241, #245, #247), read from P0-5 or from a table in the module                                                                                             |
| `pnfs-mds-legacy-state`      | Step  | `AfterCRDs`         | Where the MDS StatefulSet still has its `state` volume claim template: deletes the StatefulSet with orphaned pods, the `*-pnfs-mds-state` claims, the derived StorageClass, and the admission policy and its binding, so the target operator recreates the MDS with its standby |
| `write-protection-v2`        | Step  | `AfterStoragePlane` | For each cluster reporting `V1`: a `StorageClusterOps` `SwitchWriteProtection`                                                                                                                                                                                                  |
| `helm-values-removed-keys`   | Check |                     | Reports `driver.pnfs.mds.stateSize` and `stateStorageClassName` in the release's values as a warning, because the target chart ignores them (§7.4)                                                                                                                              |

Ordering the rolling restart before the write-protection switch is what makes
every node run the target's SPDK image before the switch reaches it, and the
target control plane's sizing is applied only through restarts that carry the
target image, which is what the analysis required of this upgrade.

---

## 7. Running the Upgrade

### 7.1 Commands

`simplyblock-upgrade` gains a `release` command group beside the API migration's
commands:

| Command             | Does                                                                                            | Writes                                |
|---------------------|-------------------------------------------------------------------------------------------------|---------------------------------------|
| `release plan`      | Detects source and target, lists the selected modules, and prints every check and step in order | Nothing                               |
| `release preflight` | Runs the checks of §6.5 step 1 and 2                                                            | Nothing                               |
| `release prepare`   | `preflight`, then `BeforeCRDs`, the CRDs, and `AfterCRDs`                                       | CRDs and what the steps change        |
| `release rollout`   | Steps 4 to 7 of §6.5 with their hook points                                                     | Ops objects and what the steps change |

Each command accepts the framework's `--skip <rule>` and `--dry-run`, and the
version flags `--from` and, for a test, `--to`.

### 7.2 The Pre-Upgrade Hook Job

The chart renders a Job annotated `helm.sh/hook: pre-upgrade` with the delete
policy `before-hook-creation,hook-succeeded`. It runs `simplyblock-upgrade release
prepare` from the `simplyblock-upgrade` image of the target release. A failed Job
fails `helm upgrade` before Helm has changed any object of the release, and its
pod log is the report of what refused the upgrade. The pod is kept on failure,
because the delete policy deletes the Job only when it succeeds.

A pre-upgrade hook cannot use objects of the release it precedes, so its
ServiceAccount, ClusterRole, and ClusterRoleBinding are hooks too, with a lower
`helm.sh/hook-weight` and the same delete policy. The role grants `get`, `list`,
and `watch` on every kind the checks read, `create` and `patch` on
`customresourcedefinitions`, and the writes the selected modules' `BeforeCRDs` and
`AfterCRDs` steps declare. The hook is not rendered on `helm install`.

### 7.3 The Rollout Job

The rollout is long: a twenty-node rolling restart takes hours, which is beyond
what a Helm hook is meant to hold. So the rollout Job is an ordinary object of
the release, named `simplyblock-upgrade-<target>`, rendered only on an upgrade
(`.Release.IsUpgrade`). The name carries the target, so every release creates a
new Job and the next `helm upgrade` to the same release leaves the running one
alone. `helm upgrade` returns once the operator is upgraded, and the rollout
continues in the cluster.

It runs `simplyblock-upgrade release rollout` with `backoffLimit: 6` and no
`activeDeadlineSeconds`, because how long a rolling restart takes depends on the
fleet. A pod that dies is replaced and resumes (§8). The Job's
`ttlSecondsAfterFinished` is seven days, which keeps its log for a week after a
successful upgrade.

### 7.4 What the Chart Changes

**The chart stops moving the control plane after install.** `_bootstrap_controlplane.tpl`
renders `spec.source.local.image` from the live `ControlPlane` when one exists
(`lookup`), and from `image.simplyblock` only when none does. After install, the
control plane moves only through a `ControlPlaneOps` `Upgrade`, which is the
rollout Job's step 5, and a `helm upgrade` can no longer roll the management API
behind the operator's back or move it back with a stale value.

**A values key a release removes stays accepted for one release.** Helm
validates the values against `values.schema.json` before it runs any hook, so a
key the target's schema refuses fails the upgrade before the preflight can
explain it. A removed key therefore stays in the schema for the release after
its removal, marked deprecated and ignored, and the release module reports it
(§6.6).

### 7.5 Running It by Hand

Every step of the Jobs is a command, so a deployment that cannot use the hooks
(OLM, a GitOps controller that renders the chart without hooks, or an operator
who wants to watch each step) runs the same sequence:

```bash
simplyblock-upgrade release plan
simplyblock-upgrade release prepare
helm upgrade simplyblock-operator simplyblock/simplyblock-operator \
    --version <target> -n simplyblock --reuse-values --no-hooks
simplyblock-upgrade release rollout
```

`--no-hooks` keeps Helm from running the prepare a second time.

---

## 8. Idempotency and Resume

The rollout Job keeps no state. Every step it performs creates an operation with
a name derived from the target release and the subject, and the operation is the
record:

| Step                             | Operation                                          |
|----------------------------------|----------------------------------------------------|
| Control plane                    | `ControlPlaneOps` `upgrade-<target>`               |
| Rolling restart of cluster `<c>` | `StorageClusterOps` `upgrade-<target>-<c>-restart` |
| Write-protection switch of `<c>` | `StorageClusterOps` `upgrade-<target>-<c>-wp`      |

Before creating an operation, a step reads it. One that exists and `Succeeded`
is skipped, one that is `Pending` or `Running` is waited for, and one that
`Failed` or was `Aborted` stops the rollout with its message. A new pod of the
Job therefore walks the list, skips what is done, waits for what is running, and
continues. A long name is shortened by the derivation rules the framework
already has (`design-api-upgrade.md` §19).

**A failed operation is fixed and the upgrade restarted.** The user deletes the
failed operation once its cause is fixed, and deletes the Job (or runs
`release rollout` by hand). The new run recreates the operation, and with
`selection: OutOfDate` the rolling restart restarts only the nodes it had not
reached.

The pre-upgrade hook is idempotent by the step contract: applying a CRD that is
already current is a no-op apply, and every module step is skipped when its
effect is present.

---

## 9. Backend API Requirements

| Method | Endpoint                                             | Notes                                                                                                                                                                                                              |
|--------|------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `POST` | `/api/v2/clusters/{id}/storage-nodes/{node}/restart` | Exists. Gains `spdk_image` and `spdk_proxy_image` (P0-1). The operator retries a restart only after reading the node's status (write-ahead, `design-storagecluster.md` §6.2), so the call needs no idempotency key |
| `GET`  | `/api/v2/clusters/{id}/storage-nodes/{node}`         | Exists. Gains `spdk_image` and `spdk_proxy_image` in the response (P0-2)                                                                                                                                           |
| `GET`  | `/api/v2/clusters/{id}/storage-nodes/`               | Exists. The same two fields per node, which the node stream delivers                                                                                                                                               |
| `GET`  | `/api/v2/clusters/{id}`                              | Exists. Gains `default_spdk_image` and `default_spdk_proxy_image`, behind the cluster's image prefix (P0-3)                                                                                                        |
| `POST` | `/api/v2/clusters/{id}/switch-write-protection`      | New (P0-4). `204` once switched, `409` when no node is online or a node rejected it. Idempotent: a cluster already on v2 answers `204` without acting                                                              |
| `GET`  | `/api/v2/clusters/{id}`                              | Exists. Gains `write_protection_v2` (P0-4)                                                                                                                                                                         |

---

## 10. Configuration

| Value                      | Type           | Default                                      | Description                                                                                             |
|----------------------------|----------------|----------------------------------------------|---------------------------------------------------------------------------------------------------------|
| `upgrade.enabled`          | bool           | `true`                                       | Renders the pre-upgrade hook and the rollout Job. `false` leaves the upgrade to §7.5                    |
| `upgrade.image.repository` | string         | `quay.io/simplyblock-io/simplyblock-upgrade` | The tool's image                                                                                        |
| `upgrade.image.tag`        | string         | The chart's `appVersion`                     | Pinned per release like every other image                                                               |
| `upgrade.skip`             | list of string | `[]`                                         | Rule identities passed to `--skip`, for a check the user has accepted                                   |
| `upgrade.rollout.enabled`  | bool           | `true`                                       | `false` runs the hook only, for a deployment that rolls the storage plane in its own maintenance window |

The new `StorageCluster` fields of §4.1 are reconfigurable at any time. Changing
them changes the nodes' desired images, which marks them out of date and takes
effect at each node's next restart. Nothing restarts a node because an image
field changed.

---

## 11. Failure Modes

| Failure                                        | Detection                                              | Behavior                                                                                                    |
|------------------------------------------------|--------------------------------------------------------|-------------------------------------------------------------------------------------------------------------|
| A check fails                                  | `release prepare` reports an `Error` finding           | The hook Job fails, `helm upgrade` fails, and nothing was changed                                           |
| A CRD does not become `Established`            | Wait in `release prepare` times out after five minutes | The hook fails. The CRDs applied so far stay, which the old operator tolerates because they only add fields |
| The operator does not reach the target version | Rollout step 4 times out after fifteen minutes         | The Job fails with the Deployment's rollout condition                                                       |
| The control-plane upgrade fails                | `ControlPlaneOps` `Failed`                             | The rollout stops before touching any storage node                                                          |
| A node comes back on the wrong image           | `RollingRestart` fails (§5.3)                          | The walk stops on that node, the remaining nodes stay on the source images                                  |
| A peer is not online during the walk           | `CheckingPeers` holds                                  | The rollout waits, and the operation's events say why                                                       |
| The Job pod dies                               | The Job controller replaces it                         | The new pod resumes (§8)                                                                                    |
| The source cannot be detected                  | No Helm release and no `--from`                        | `release prepare` refuses, naming `--from`                                                                  |
| Source older than a module's `MinimumSource`   | §6.4                                                   | Refused, naming the release to upgrade to first                                                             |

---

## 12. Observability

The operations the rollout creates already emit events and metrics
(`design-storagecluster.md` §10, `design-controlplane.md` §6). This section adds
what the new behavior needs.

### Kubernetes Events

The tool's events land on the `ControlPlane` singleton, because it is the one
object every deployment has, it outlives both Jobs, and `kubectl describe
controlplane` is where a user already looks for the deployment's version.
Image events land on the `StorageNode` they concern.

| On                | Event                                                                                                | Type    | Reason                  |
|-------------------|------------------------------------------------------------------------------------------------------|---------|-------------------------|
| ControlPlane      | The upgrade from the source to the target started, naming the selected modules                       | Normal  | ReleaseUpgradeStarted   |
| ControlPlane      | A check refused the upgrade, naming the rule and its finding                                         | Warning | ReleaseUpgradeRefused   |
| ControlPlane      | The rollout is waiting for an operation it created, naming it                                        | Normal  | ReleaseUpgradeWaiting   |
| ControlPlane      | An operation the rollout created failed, so the rollout stopped                                      | Warning | ReleaseUpgradeFailed    |
| ControlPlane      | Every step succeeded and the deployment runs the target                                              | Normal  | ReleaseUpgradeSucceeded |
| StorageNode       | The node's running images differ from its desired images                                             | Normal  | ImageOutOfDate          |
| StorageClusterOps | A node came back from the walk's restart on an image other than the desired one, so the walk stopped | Warning | ImageMismatch           |
| StorageClusterOps | The walk passed over a node because it already runs its desired images                               | Normal  | NodeCurrent             |

### Prometheus Metrics

| Metric                                                            | Labels            | Description                                                                                                       |
|-------------------------------------------------------------------|-------------------|-------------------------------------------------------------------------------------------------------------------|
| `simplyblock_storagenode_images_out_of_date`                      | `cluster`, `node` | 1 while the node's `imagesCurrent` is `false`. Summed by `cluster`, the number of nodes a rollout has not reached |
| `simplyblock_storagecluster_rolling_restart_image_mismatch_total` | `cluster`         | Counter of restarts that came back on an unexpected image                                                         |
| `simplyblock_storagecluster_rolling_restart_nodes_current_total`  | `cluster`         | Counter of nodes an `OutOfDate` walk passed over as current                                                       |

The Jobs export no metrics: they are short-lived processes, and the operations
they create are what a dashboard follows.

`simplyblock_storagenode_images_out_of_date` is the load-bearing one. It is the
alert for a fleet left half upgraded, which is the state a stopped rollout leaves
behind and the one that otherwise goes unnoticed, since every node is `online`.
`simplyblock_storagecluster_rolling_restart_image_mismatch_total` is the alert for a control plane
that ignores the image a restart asked for.

---

## 13. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-release-upgrade.md`](../tests/test-plan-release-upgrade.md)

- **Unit:** Module selection over every relation of source, target, and
  `MinimumSource`, including prerelease ordering. The desired-image resolution of
  §5.1. The restart parameters of both call sites. `OutOfDate` planning and the
  per-node recheck. The rollout's skip, wait, and stop decisions over existing
  operations.
- **Integration:** The `RollingRestart` and `SwitchWriteProtection` graphs
  against `envtest` and a mock control plane, including an operator restart in
  every step and a rerun after a failed walk.
- **E2E:** An upgrade from the last release to the target through `helm upgrade`
  with the hooks, under sustained `fio`, ending with every node on the target
  images and no I/O error.

The risk concentrates in §5.3 and §8. A node counted as converged when it is not,
or a resumed rollout that restarts a node twice in a row, are the defects that
reach production, so their scenarios are not cut.

---

## 14. Migration Strategy

**The first release with this design cannot use it for its own upgrade.** The
pre-upgrade hook and the rollout Job exist from that release on, but the
operator actions they drive need the target operator, and P0-1 to P0-4 need the
target control plane. Both are the target's, so the first upgrade to a release
with this design already runs it. What it cannot rely on is P0-2 on the nodes
before they restart, which is why §5.4 refuses `OutOfDate` only when the control
plane reports no images at all, not when one node does.

**The new fields have no `v1alpha1` counterpart.** `StorageCluster`,
`StorageNode`, and `StorageClusterOps` still serve `v1alpha1` and store
`v1alpha2`, so a client reading `v1alpha1` would drop them on a write back. They
are carried through that round trip by the annotation stash the conversion
functions already use for fields `v1alpha1` lacks (`stashOpsHubOnly` in
`operator/api/v1alpha1/storageclusterops_conversion.go` is the pattern), and
`SwitchWriteProtection` and `OutOfDate` have no `v1alpha1` spelling, so a
`v1alpha1` read of such an operation restores them from the stash.

**The chart's change to the control-plane image (§7.4) changes documented
behavior.** The warning in the user documentation that a chart upgrade moves the
control plane is replaced by the rollout Job's description.

---

## 15. Open Questions

| #   | Question                                                                                                                                                                             | Owner              |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------|
| 1   | **The release after `26.4.0-beta6`:** its version decides the module name of §6.6 and the upgrade-notes page in the documentation                                                    | Release management |
| 2   | **SPDK capability metadata (P0-5):** a label or an RPC that states which sizing an SPDK image supports, so §6.6's check needs no table                                               | SPDK/Backend team  |
| 3   | ~~**Restart after the write-protection switch**~~ **Resolved.** The switch converts online nodes at runtime and stores v2 for offline ones, so no restart follows it (§4.4)          | Resolved           |
| 4   | **Clusters in parallel:** §6.5 rolls one `StorageCluster` after the other. A deployment with many small clusters may want them in parallel, which the operator's locks already allow | —                  |
| 5   | **The tool's image:** `simplyblock-upgrade` is built as binaries and published nowhere today. The hook needs it as a container image per release                                     | —                  |

---

## Appendix A: `storagecluster_types.go`

The blocks below are added to the type `design-storagecluster.md` Appendix A
specifies, which stays the full copy.

```go
// In StorageNodesSpec, after ImagePullPolicy.

	// SpdkImage is the SPDK image every storage node of this cluster runs,
	// unless the node's own spec.config.spdkImage overrides it. Unset follows
	// the control plane's default SPDK image, so a cluster that states no image
	// moves with its control plane. A change takes effect at each node's next
	// restart.
	// +optional
	SpdkImage crdtypes.ImageReference `json:"spdkImage,omitempty"`

	// SpdkProxyImage is the SPDK proxy image every storage node of this cluster
	// runs, unless the node's own spec.config.spdkProxyImage overrides it. It
	// resolves like SpdkImage.
	// +optional
	SpdkProxyImage crdtypes.ImageReference `json:"spdkProxyImage,omitempty"`

// WriteProtectionVersion is the distrib write-protection scheme a cluster runs.
// +kubebuilder:validation:Enum=V1;V2
type WriteProtectionVersion string

const (
	// WriteProtectionV1 is the scheme of clusters created before v2 existed.
	WriteProtectionV1 WriteProtectionVersion = "V1"
	// WriteProtectionV2 is the scheme every current cluster is created with.
	WriteProtectionV2 WriteProtectionVersion = "V2"
)

// In StorageClusterStatus.

	// WriteProtection is the write-protection scheme the control plane reports
	// for the cluster. A StorageClusterOps SwitchWriteProtection moves it from
	// V1 to V2.
	// +optional
	WriteProtection WriteProtectionVersion `json:"writeProtection,omitempty"`

	// DefaultSpdkImage is the SPDK image the control plane starts a node with
	// when nothing names one, which a node's desired image falls back to.
	// +optional
	DefaultSpdkImage string `json:"defaultSpdkImage,omitempty"`

	// DefaultSpdkProxyImage is the SPDK proxy image the control plane starts a
	// node with when nothing names one.
	// +optional
	DefaultSpdkProxyImage string `json:"defaultSpdkProxyImage,omitempty"`
```

---

## Appendix B: `storagenode_types.go`

Added to the type `design-storagenode.md` specifies.

```go
// In StorageNodeStatus.

	// SpdkImage is the SPDK image the control plane reports it starts this node
	// with. Empty when the control plane does not report one.
	// +optional
	SpdkImage string `json:"spdkImage,omitempty"`

	// SpdkProxyImage is the SPDK proxy image the control plane reports for this
	// node.
	// +optional
	SpdkProxyImage string `json:"spdkProxyImage,omitempty"`

	// DesiredSpdkImage is the SPDK image this node should run: its own
	// spec.config.spdkImage, else its cluster's spec.storageNodes.spdkImage,
	// else the control plane's default. Every restart the operator requests
	// carries it.
	// +optional
	DesiredSpdkImage string `json:"desiredSpdkImage,omitempty"`

	// DesiredSpdkProxyImage is the SPDK proxy image this node should run,
	// resolved like DesiredSpdkImage.
	// +optional
	DesiredSpdkProxyImage string `json:"desiredSpdkProxyImage,omitempty"`

	// ImagesCurrent is true when the node runs its desired images and false
	// when it does not, so a restart is due. Unset when the control plane
	// reports no images and the answer is unknown.
	// +optional
	ImagesCurrent *bool `json:"imagesCurrent,omitempty"`

// The printer column, on the root type.

// +kubebuilder:printcolumn:name="Images",type=boolean,JSONPath=".status.imagesCurrent"
```

---

## Appendix C: `storageclusterops_types.go`

Added to the type `design-storagecluster.md` Appendix B specifies.

```go
// The action enum gains SwitchWriteProtection.
// +kubebuilder:validation:Enum=Activate;Expand;Shutdown;Start;Restart;RollingRestart;CancelTask;GrowDevices;SwitchWriteProtection

	// StorageClusterOpsActionSwitchWriteProtection moves the cluster's distribs
	// from v1 to v2 write protection.
	StorageClusterOpsActionSwitchWriteProtection StorageClusterOpsAction = "SwitchWriteProtection"

// The step enum gains Switching.

	// StorageClusterOpsStepSwitching is SwitchWriteProtection's call and its
	// wait for the cluster to report V2.
	StorageClusterOpsStepSwitching StorageClusterOpsStep = "Switching"

// RollingRestartSelection decides which nodes a rolling restart walks.
// +kubebuilder:validation:Enum=All;OutOfDate
type RollingRestartSelection string

const (
	// RollingRestartSelectionAll restarts every node not leaving the cluster.
	RollingRestartSelectionAll RollingRestartSelection = "All"
	// RollingRestartSelectionOutOfDate restarts only the nodes whose running
	// images differ from their desired images.
	RollingRestartSelectionOutOfDate RollingRestartSelection = "OutOfDate"
)

// RollingRestartSpec parameterizes the RollingRestart action.
type RollingRestartSpec struct {
	// RefreshSNodeAPI restarts each node's storage-node DaemonSet pod between
	// its shutdown and its restart, so the latest image is running when the
	// node returns.
	// +optional
	RefreshSNodeAPI bool `json:"refreshSNodeAPI,omitempty"`

	// Selection decides which nodes the walk restarts. All restarts every node
	// that is not leaving the cluster. OutOfDate restarts only the nodes whose
	// running images differ from their desired images, so a rerun continues
	// where a stopped walk ended. Every restart carries the node's desired
	// images whatever the selection.
	// +kubebuilder:validation:Enum=All;OutOfDate
	// +kubebuilder:default=All
	// +optional
	Selection RollingRestartSelection `json:"selection,omitempty"`
}

// In RollingRestartStatus, after Skipped.

	// Current are the nodes an OutOfDate walk passed over because they already
	// ran their desired images when the walk reached them, in walk order.
	// +optional
	Current []string `json:"current,omitempty"`
```
