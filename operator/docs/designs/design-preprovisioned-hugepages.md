# Design Document: Preprovisioned Huge-Page Mode

**Status:** Phase 1 Implemented  
**Author:** Max Schettler  
**Date:** 2026-11-24 (last updated 2026-11-26)  
**Test Plan:** [`tests/test-plan-preprovisioned-hugepages.md`](../tests/test-plan-preprovisioned-hugepages.md)

---

## Phasing Overview

| Phase       | Status          | Mode(s)          | Platform(s)    | Sections |
|-------------|-----------------|------------------|----------------|----------|
| **Phase 1** | **Implemented** | `Preprovisioned` | Every platform | §4–§10   |
| **Phase 2** | Planned         | `Exclusive`      | Talos          | §14      |
| **Phase 3** | Planned         | `Shared`         | OpenShift      | §15      |

Each phase is independently shippable: `hugePagesMode` is read fresh at every reconcile and admission, so a cluster can run Phase 1's check today and gain Phase 2 or 3 the moment the operator and `sbcli` versions that implement them are deployed, with no migration step of its own (§12).

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                         | Kind                    | Blocks        | Status  |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------------|---------|
| P0-1 | `POST /cluster/sizing/hugepages-estimate` (§7)                                                                                                                                       | Control plane (`sbcli`) | Phase 1 (§5)  | Shipped |
| P0-2 | A kernel-boot-argument huge-page reservation (`hugepagesz=2M hugepages=N`) distributes pages predictably enough across NUMA nodes on the RHCOS kernel versions this product supports | Node OS                 | Phase 3 (§15) | Unknown |

P0-1 shipped alongside Phase 1: `simplyblock_web/api/v2/cluster/__init__.py:198`, `atlas-lib/controlplane/clusters.go:214`. Without P0-2, Phase 3's kernel-argument reservation (§15) may hand SPDK a pool whose NUMA placement does not match `_hugepages_per_numa`'s per-socket sizing, which is the reason §15 Step 4 also verifies per-NUMA capacity after the reboot rather than trusting the total alone.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Design — Field Additions](#4-api-design--field-additions)
5. [The Preprovisioned Precheck](#5-the-preprovisioned-precheck)
6. [Controller and Webhook Design](#6-controller-and-webhook-design)
7. [Backend API Requirements](#7-backend-api-requirements)
8. [Configuration](#8-configuration)
9. [Failure Modes and Fallback](#9-failure-modes-and-fallback)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Migration Strategy](#12-migration-strategy)
13. [Open Questions](#13-open-questions)
14. [Exclusive Mode — Talos (Phase 2)](#14-exclusive-mode--talos-phase-2)
15. [Shared Mode — OpenShift (Phase 3)](#15-shared-mode--openshift-phase-3)

---

## Overview

A `ClusterDeploymentConfig` (or a `StorageCluster`/`StorageNode` written directly) selects one of three values for `hugePagesMode`, which states who is responsible for the huge pages a storage node's SPDK pod needs and when that responsibility is discharged:

| Mode             | Who provisions                                                                                                                                   | When                              | Platform            |
|------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------|---------------------|
| `Preprovisioned` | The administrator, by whatever means, before the cluster is deployed                                                                             | Verified before admission         | Every platform (§5) |
| `Exclusive`      | The operator, through the Talos API, growing a pool dedicated to simplyblock on top of whatever the host already carried                         | At node-add time                  | Talos (§14)         |
| `Shared`         | The operator, through an OpenShift `MachineConfig` kernel argument, growing a pool that stays open to every other huge-page consumer on the host | At node-add time, across a reboot | OpenShift (§15)     |

`Preprovisioned` never mutates a host. `Exclusive` and `Shared` do, through the one platform-specific mechanism each of their platforms already offers for changing what a host carries outside the running kernel: Talos's API, for a host with no shell and no writable sysfs, and OpenShift's `MachineConfig`/`MachineConfigPool` rollout, for a host whose only supported way to add a boot-time kernel parameter is a managed reboot. Neither mode is offered, or needed, on a platform without such a mechanism: a vanilla host's existing, unconditional sysfs write (`storage_init_job.yaml.j2`, `set_hugepages_if_needed`) already does what `Exclusive` would do there, with no mode selection required.

---

## 1. Background

Today, huge-page provisioning for a storage node's SPDK pod is `sbcli`'s responsibility end to end, not the operator's:

- The pod's manifest (`storage_deploy_spdk.yaml.j2`) requests `hugepages-2Mi` as a resource limit, sized from `spdk_mem`.
- Outside Talos, the node-preparation Job (`storage_init_job.yaml.j2`) and the node agent (`simplyblock_web/api/internal/storage_node/kubernetes.py`, `/snode/apply_config`, `utils.set_hugepages_if_needed`) write `nr_hugepages` to the host's sysfs directly, claiming the pages for simplyblock.
- On Talos, `StorageCluster.spec.storageNodes.talosMachineConfig` (`TalosMachineConfigMode`, values `Preprovisioned`/`Managed`, `storagecluster_types.go:743-753`) splits the same two ideas this design generalizes, but bundles two concerns under one value: `Preprovisioned` verifies the huge-page pool and the kubelet's CPU topology and fails the add with the exact `talosctl patch` for whichever is short. `Managed` has the node apply both itself through the Talos API (`storage_talos_hugepages_job.yaml.j2`, `_configure_talos_kubelet_topology`). `StorageCluster.spec.storageNodes.talos` (`storagecluster_types.go:727-734`) is the bool that turns this path on, set by the expansion from `ClusterDeploymentConfig.spec.environment` (`clusterdeploymentconfig_types.go:904-911`).

In every path, a shortfall that is not caught before the `StorageNode` is created surfaces as the SPDK pod staying `Pending` with Kubernetes' own `Insufficient hugepages-2Mi` scheduling event, discovered late and without saying which of two different causes it is: the host never had enough, or it did but something else is holding the pages.

This design introduces `HugePagesMode` on `ClusterDeploymentConfig`/`StorageCluster`/`StorageNode` as the one field that answers how a cluster's huge pages are obtained, on every platform. `HugePagesMode` is the only knob huge pages read going forward. `TalosMachineConfigMode` keeps its existing two values and its existing behavior, but narrowed to the CPU-topology half of what it governs today (§4.3, §14). A Talos cluster that set `talosMachineConfig` before this field existed keeps its exact huge-page behavior without being rewritten, through a one-time default (§12): `HugePagesMode` left unset on a Talos document inherits `Preprovisioned` or `Exclusive` from `talosMachineConfig`'s existing value. The two fields answering overlapping questions, unreconciled, is what let a document set `hugePagesMode: Preprovisioned` together with `talosMachineConfig: Managed` and have the precheck fail every time, since `Managed` provisioned the pages after admission and the precheck only ever saw what already existed. Narrowing `talosMachineConfig` to CPU topology (§4.3) removes the contradiction rather than forbidding it: the same combination now means "CPU topology managed through `talosctl`, huge pages verified only," which is a legitimate choice `TalosMachineConfigMode` alone could not express.

The sizing figure the precheck compares against is not new either: `calculate_minimum_hp_memory` (`simplyblock_core/utils/__init__.py:762`) already computes it from `max_subsys` and `vcpu_count` alone, at every node add and node restart (`storage_node_ops.py:9465-9470`), and `MinHugePagesSize` (`ClusterTemplate`/`NodeSetClusterOverrides`, `clusterdeploymentconfig_types.go:396`/`465`) is already nothing more than a manual floor clamped onto that same formula's output. Nothing about "how much" is new. This design is about running that same arithmetic, and the capacity comparison it enables, before the pod exists instead of after.

---

## 2. Goals and Non-Goals

### Goals

- A `ClusterDeploymentConfig`, `StorageCluster`, or `StorageNode` with `hugePagesMode: Preprovisioned` fails before any `StorageNode` is created when a target worker does not have enough `hugepages-2Mi` capacity for the cluster's sizing (§5).
- The same document fails, with a distinguishable reason, when the worker's capacity is enough on its own but is contended by non-simplyblock pods already scheduled on it (§5).
- The required huge-page figure is computed exactly once, in `sbcli`, and the operator never re-implements `calculate_minimum_hp_memory` (§7).
- `hugePagesMode: Exclusive` is available on Talos, growing the host's huge-page pool through the Talos API the way `talosMachineConfig: Managed` already does, reachable under its generalized name (§14).
- `hugePagesMode: Shared` is available on OpenShift, growing the host's huge-page pool through a `MachineConfig` kernel argument that stays open to every other consumer of the pool, reusing the `MachineConfigPool`/`MachineConfig`/`KubeletConfig` rollout the CPU-topology Job already creates for the cluster (§15).
- `hugePagesMode: Exclusive`/`Shared` are refused at admission on any platform that does not implement them (§6.2), rather than silently accepted as a no-op or accepted everywhere regardless of platform.
- A document that leaves `hugePagesMode` unset behaves exactly as it does today (§12). A Talos document that set `talosMachineConfig` before `hugePagesMode` existed keeps its exact huge-page behavior (§12).

### Non-Goals

- **Generalizing `Exclusive` beyond Talos.** No other platform offers a mechanism to change a running host's huge-page pool outside the kernel the way the Talos API does. A vanilla host's existing unconditional sysfs write already behaves the way `Exclusive` would, without needing the mode selected (Overview).
- **A Node Tuning Operator-based `Shared` implementation.** OpenShift's Node Tuning Operator (`PerformanceProfile`) is a second way to reserve huge pages declaratively, but this design reuses the `MachineConfig`/`MachineConfigPool`/`KubeletConfig` objects the CPU-topology Job already creates and already waits on (§15), rather than adding a dependency on a CRD this operator does not otherwise require.
- **Retiring `TalosMachineConfigMode`.** It keeps its existing two values, its existing field, and its existing CPU-topology behavior (§4.3). Only its huge-page half is superseded.
- **Re-deriving the sizing formula in Go.** The operator consumes `calculate_minimum_hp_memory`'s result through a new control-plane endpoint (§7). It never computes the pool-count/reference-node arithmetic itself.
- **Changing the pod's own resource request.** `storage_deploy_spdk.yaml.j2`'s `hugepages-2Mi` limit is unchanged. This design only decides, earlier, whether that limit will schedule.

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                     Kubernetes Control Plane (operator)              │
│  ┌──────────────────────────────────────────────────────────────┐    │
│  │              ClusterDeploymentConfigReconciler                │    │
│  │  1. Resolve the node set a Preprovisioned document targets    │    │
│  │  2. hugepages.Evaluate(...) against those workers             │    │
│  │  3. Set HugePagesReady on the document; block expansion on    │    │
│  │     False, same as any other validation failure               │    │
│  └──────────────────────────────────────────────────────────────┘    │
│  ClusterDeploymentConfig CR  spec.cluster.hugePagesMode               │
│                               status.conditions                       │
│  ┌──────────────────────────────────────────────────────────────┐    │
│  │   ClusterDeploymentConfigValidator / StorageClusterValidator  │    │
│  │   platform.HugePagesModeSupported(mode, platform) — refuses   │    │
│  │   Exclusive off Talos and Shared off OpenShift (§6.2)          │    │
│  └──────────────────────────────────────────────────────────────┘    │
│  ┌──────────────────────────────────────────────────────────────┐    │
│  │             StorageNodeValidator (admitCreate)                │    │
│  │   Same hugepages.Evaluate(...), against the one worker the    │    │
│  │   node names, for direct-API writes and CDC/add-time drift    │    │
│  └──────────────────────────────────────────────────────────────┘    │
│  StorageNode CR  status.conditions                                    │
└───────────────────────────────┬────────────────────────────────────┘
                                 │ HTTP (management-API client, bearer token —
                                 │ ControlPlane-scoped, not per-StorageCluster)
┌────────────────────────────────▼────────────────────────────────────┐
│                     Simplyblock Backend API (sbcli)                  │
│  POST /cluster/sizing/hugepages-estimate                             │
└────────────────────────────────────────────────────────────────────┘
```

**The two check sites run the same evaluation against different scopes.** The `ClusterDeploymentConfig` controller evaluates every worker the document's node sets would place a node on, before any `StorageNode` exists. The `StorageNode` webhook evaluates the one worker a single create names, which is what catches a node written directly against the API, or one added after the document's own check already ran.

**The management-API client already exists and already runs before a cluster exists.** `creationParams()` (`internal/controllers/cluster/storagecluster_controller.go:1119-1138`) calls the control plane's `CreateCluster` through it to create a cluster that, at that point in the reconcile, does not yet exist. `managementapi.go`'s own description ("the REST surface every controller in this operator calls") is `ControlPlane`-scoped, not `StorageCluster`-scoped, so §7's new endpoint needs no new auth plumbing and no cluster to already exist either.

**This diagram is Phase 1's.** `Exclusive` (§14) and `Shared` (§15) provision at node-add time, entirely inside the node-add Job `sbcli`'s `/snode/apply_config` already runs, below the HTTP boundary this diagram stops at. Neither adds a new operator-to-backend call: the operator's only role in either is the platform-aware admission decision shown here.

---

## 4. API Design — Field Additions

No new CRD. Three existing types gain fields. Each is specified by another document and only the addition is shown here (bare fields with their markers, not the surrounding type).

### 4.1 `ClusterTemplate` / `NodeSetClusterOverrides` — `hugePagesMode` (new)

Specified by `design-clusterdeploymentconfig.md`, whose Appendix A is `clusterdeploymentconfig_types.go`. Added as a sibling to the existing `MinHugePagesSize string` field in both `ClusterTemplate` (`:459-465`) and `NodeSetClusterOverrides` (`:392-396`), not nested into a new struct, which would rename the already-shipped `minHugePagesSize` field for no reason tied to this design.

```go
// HugePagesMode selects how huge pages are managed for this cluster's
// storage nodes. Preprovisioned assumes the workers already carry enough
// and checks that before any StorageNode is created, rather than letting a
// shortfall surface as a Pending SPDK pod (§5). Exclusive and Shared have
// the operator grow the pool itself, through a mechanism only some
// platforms offer (§14, §15); selecting one platform.HugePagesModeSupported
// refuses is denied at admission (§6.2). Left unset, no huge-page check or
// provisioning runs, which is this deployment's behavior without this
// field, except on Talos, where talosMachineConfig's existing value is
// read instead (§12).
// +kubebuilder:validation:Enum=Preprovisioned;Exclusive;Shared
// +optional
HugePagesMode HugePagesMode `json:"hugePagesMode,omitempty"`
```

The named type and its constants are new and belong beside `KubernetesEnvironment` (`clusterdeploymentconfig_types.go:85-101`):

```go
// HugePagesMode is how a cluster's storage nodes obtain the huge pages
// SPDK needs. See ClusterTemplate.HugePagesMode.
// +kubebuilder:validation:Enum=Preprovisioned;Exclusive;Shared
type HugePagesMode string

const (
	// HugePagesModePreprovisioned assumes the workers already carry enough
	// huge pages for the cluster's sizing. Available on every platform; see
	// platform.HugePagesModeSupported (§6.2).
	HugePagesModePreprovisioned HugePagesMode = "Preprovisioned"

	// HugePagesModeExclusive has the operator configure and own the host's
	// huge-page pool, on top of whatever it already carried. Available on
	// Talos only, through the Talos API (§14).
	HugePagesModeExclusive HugePagesMode = "Exclusive"

	// HugePagesModeShared has the operator grow the host's huge-page pool
	// while leaving it open to every other consumer of it. Available on
	// OpenShift only, through a MachineConfig kernel argument (§15).
	HugePagesModeShared HugePagesMode = "Shared"
)
```

`HugePagesMode` is propagated from `ClusterTemplate`/`NodeSetClusterOverrides` onto `StorageCluster.Spec` the same way `MinHugePagesSize` already is: `storagesitedeployment_controller.go:268-269`, `expansion.go:338,792`, `supercluster.go:117-118`. On Talos, the propagation also applies the default of §12: a `StorageCluster.Spec.HugePagesMode` left empty after that copy is filled from `TalosMachineConfig` before the webhook or the precheck ever reads it, so no call site downstream of expansion has to know the old field exists.

### 4.2 `StorageClusterStatus` / `StorageNodeStatus` — `Conditions` (new)

Specified by `design-storagecluster.md` and `design-storagenode.md` respectively. Neither status type carries `Conditions` today (`storagecluster_types.go:1293-1333`, `storagenode_types.go:541-581`). Other kinds in this group already do, in the same shape added here (`storagegroupbackup_types.go:125-129`):

```go
// Conditions carries HugePagesReady, which a Preprovisioned document's
// reviewer, or a client watching the object, reads for the precheck's
// outcome (§5). The operator writes no other condition type yet.
// +listType=map
// +listMapKey=type
// +optional
Conditions []metav1.Condition `json:"conditions,omitempty"`
```

`metav1.Condition.Type` is `"HugePagesReady"`. `.Reason` is one of `InsufficientHugePages`, `HugePagesContended`, or `Ready`. Both are plain strings on `metav1.Condition` itself, so neither is a new named Go type.

### 4.3 `TalosMachineConfigMode` — narrowed to CPU topology

`StorageCluster.spec.storageNodes.talosMachineConfig` keeps its field, its two values, and its CPU-topology behavior (`_configure_talos_kubelet_topology`, `storagecluster_types.go:743-753`) unchanged. Only its doc comment and its huge-page half change:

```go
// TalosMachineConfig is who writes a Talos worker's machine config for the
// kubelet's CPU topology (machine.kubelet.extraConfig). Preprovisioned, the
// default, is the cluster's operator, before the node is added: the node
// verifies the topology, applies nothing, and fails the add with the patch
// to apply, so no Talos API credentials exist in the namespace. Managed is
// the node itself, through the Talos API as talosctl Jobs, which needs the
// os:admin Secret the Talos API access feature issues. Read only when Talos
// is stated; the node agent reads it as TALOS_MACHINE_CONFIG. The
// huge-page pool this field also governed before HugePagesMode existed is
// hugePagesMode's now (§14); a document that set this field and leaves
// hugePagesMode unset keeps its exact huge-page behavior (§12).
// +optional
TalosMachineConfig TalosMachineConfigMode `json:"talosMachineConfig,omitempty"`
```

`_talos_machine_config_managed()` (`kubernetes.py:537-556`) is unchanged and keeps governing `_configure_talos_kubelet_topology` alone. A new, independent check, `_talos_hugepages_managed()`, reads a new `TALOS_HUGEPAGES_MODE` environment variable the operator sets from the resolved `HugePagesMode` (§12), and is what `_provision_talos_hugepages` reads going forward (§14). The two checks can disagree from this point on, by design: `hugePagesMode: Preprovisioned` with `talosMachineConfig: Managed` is now a cluster whose CPU topology the operator manages through `talosctl` and whose huge pages it only verifies, which §1 covers.

---

## 5. The Preprovisioned Precheck

### Step 1 — Required huge pages

The required figure is `MEM_MEGA` as `sbcli` already computes it (`simplyblock_web/api/internal/storage_node/kubernetes.py:1270-1271`), not the bare output of `calculate_minimum_hp_memory`: the comparison in Step 2 has to use the same number the real pod's `hugepages-2Mi` limit will carry, because that is the number that actually decides whether the pod schedules.

```go
formulaMiB, err := cpClient.EstimateHugePages(ctx, maxSubsys, vcpuCount) // §7
requiredMiB := max(formulaMiB, minHugePagesSizeMiB)                      // the MinHugePagesSize floor (§1), parsed via utils.ParseSize as storagecluster_controller.go:1119-1138 already does
memMegaMiB := (requiredMiB/2)*2 + 512                                    // mirrors kubernetes.py's MEM_MEGA rounding exactly
```

The clamp against the floor runs before the rounding, matching `sbcli`'s own order of operations (`minimum_hp_memory = max(computed, max_prov)` precedes `MEM_MEGA` being derived from it). This is why the rounding cannot live behind the §7 endpoint: the endpoint has no floor to clamp against, by design (§7), so whichever side applies the floor also has to apply the rounding that follows it.

### Step 2 — Capacity

For each candidate worker, read `discovery.KubeNode.HugePages` (`internal/discovery/kubenode.go:51-52,96-105`), already populated from the kubelet's reported `hugepages-2Mi` capacity and allocatable, which is not the same figure the kernel reports (`kubenode.go:10-15`). `AllocatableBytes` below `memMegaMiB`, or no entry for the 2 MiB size at all, is `InsufficientHugePages`.

### Step 3 — Contention

A worker whose allocatable passes Step 2 may still not have `memMegaMiB` actually free: Kubernetes reports capacity and allocatable as static Node fields, not "what is free right now," so a pod already consuming part of the pool would not shrink either number. List the Pods scheduled on that worker, sum the `hugepages-2Mi` resource requests (or, lacking one, the limits: `podHugePagesBytes`) of every pod that does not carry this cluster's own `simplyblock.io/cluster` label (`clusterUUID`, `StorageCluster.Status.UUID`, `precheck.go:239`), and subtract from `AllocatableBytes`. A remainder below `memMegaMiB` is `HugePagesContended`, and the failure message names the pods that hold the difference. This replicates the scheduler's own extended-resource bookkeeping, because no Kubernetes API reports an extended resource's free amount directly. Excluding the cluster's own label is what keeps a restart from contending with the node it is restarting (U-12), since that pod's `hugepages-2Mi` is the precheck's own prior work, not something holding pages against it.

### Algorithm Summary

```go
// internal/hugepages/precheck.go
func Evaluate(
	ctx context.Context,
	k8sClient client.Client,
	cpClient EstimateHugePagesClient,
	workers []discovery.KubeNode,
	clusterUUID string,
	maxSubsys, vcpuCount int32,
	minHugePagesSizeMiB int64,
) (*Result, error) {
	requiredMiB, err := requiredHugePagesMiB(ctx, cpClient, maxSubsys, vcpuCount, minHugePagesSizeMiB) // Step 1
	if err != nil {
		return nil, err
	}
	result := &Result{RequiredMiB: requiredMiB}
	for _, worker := range workers {
		allocatable, ok := twoMiBAllocatableBytes(worker)
		if !ok || allocatable < requiredMiB*mibBytes {
			result.fail(worker.Name, ReasonInsufficientHugePages, ...) // Step 2
			continue
		}
		holders, heldBytes, err := contendingPods(ctx, k8sClient, worker.Name, clusterUUID) // Step 3
		if err != nil {
			return nil, err
		}
		if allocatable-heldBytes < requiredMiB*mibBytes {
			result.fail(worker.Name, ReasonHugePagesContended, ...)
			continue
		}
		result.pass(worker.Name)
	}
	return result, nil
}
```

Only reached when `HugePagesMode == HugePagesModePreprovisioned` (§6). `Exclusive` and `Shared` never call `Evaluate`: neither has anything to verify before the node-add provisions the pool (§14, §15).

---

## 6. Controller and Webhook Design

### 6.1 `Evaluate` call sites

| Site                                | Package / file                                                                                   | Scope                                            | Runs before                                                    |
|-------------------------------------|--------------------------------------------------------------------------------------------------|--------------------------------------------------|----------------------------------------------------------------|
| `ClusterDeploymentConfig` reconcile | `internal/controllers/deployment/clusterdeploymentconfig_controller.go`                          | Every worker the document's node sets resolve to | Any `StorageCluster`/`StorageNode` the document would generate |
| `StorageNode` admission             | `internal/webhook/storagenode_validator.go`, `admitCreate`, beside `sizingDenial()` (`:304-322`) | The one worker `spec.workerNode` names           | The `StorageNode` object itself                                |

Both are gated on `HugePagesMode == HugePagesModePreprovisioned`, read off the owning `StorageCluster` for the `StorageNode` site. A failing evaluation sets `HugePagesReady=False` on the `ClusterDeploymentConfig`'s (or `StorageCluster`'s) `status.conditions` in the first case, and denies the admission request in the second.

### 6.2 Refusing `Exclusive` and `Shared` off their platform

```go
// internal/platform/hugepages.go

// Platform is what HugePagesModeSupported needs to know about the cluster a
// mode is being selected for. Zero value is a platform with neither
// mechanism, which is every platform's default.
type Platform struct {
	// Talos is whether the cluster's workers run Talos Linux (Exclusive's
	// platform, §14).
	Talos bool
	// OpenShift is whether the cluster declares the OpenShift block (Shared's
	// platform, §15). Its presence is the statement (OpenShiftSpec's own doc
	// comment), so this is a pointer's nil check, not a second boolean.
	OpenShift bool
}

// HugePagesModeSupported reports whether this operator can act on mode for
// platform. Preprovisioned needs nothing beyond the check in §5 and is
// available everywhere. Exclusive is conceivable only on Talos (§14), and
// Shared only on OpenShift (§15): each is the one platform whose own
// mechanism this design's Exclusive/Shared implementation reuses, so
// selecting either anywhere else is refused the same way an
// implemented-nowhere mode was before. This answers only whether the
// platform could support the mode, not whether it is actually configured
// for it (the Talos API access Secret, the OpenShift MachineConfigPool
// rollout): that readiness is a controller-level condition, not an
// admission-time refusal (§13, Q2, resolved).
func HugePagesModeSupported(mode v1alpha2.HugePagesMode, platform Platform) bool {
	switch mode {
	case "", v1alpha2.HugePagesModePreprovisioned:
		return true
	case v1alpha2.HugePagesModeExclusive:
		return platform.Talos
	case v1alpha2.HugePagesModeShared:
		return platform.OpenShift
	default:
		return false
	}
}
```

Called from `ClusterDeploymentConfigValidator.Handle` and `StorageClusterValidator.Handle` (both already validate other fields on create/update, `clusterdeploymentconfig_validator.go:82-90`, `storagecluster_validator.go:60-86`), denying with `hugePagesMode: Exclusive is only supported on Talos` (worded accordingly for `Shared`/OpenShift). `StorageClusterValidator` and `StorageNodeValidator` build `Platform` from the `StorageCluster`'s own `spec.storageNodes.talos`/`.openshift`, already resolved by expansion. `ClusterDeploymentConfigValidator` builds it from `spec.environment` directly, since a document's `StorageCluster` does not exist yet to read it from. This cannot be a `+kubebuilder:validation:XValidation` rule: a CEL rule evaluated against a single object cannot read another field's derived platform the way `Platform` is assembled here, which is why this was webhook logic from the start rather than a later migration off CEL.

---

## 7. Backend API Requirements

| Method | Endpoint                             | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
|--------|--------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `/cluster/sizing/hugepages-estimate` | **New.** Body `{ max_subsys: int, vcpu_count: int }`, response `{ minimum_hp_memory_mib: int }`. Stateless: no cluster needs to exist, and no `hugepages_mem` floor is accepted: the caller applies that clamp itself (§5, Step 1), because the caller already has it and the clamp has to precede a rounding step that cannot live behind this endpoint. Calls `calculate_minimum_hp_memory` (`simplyblock_core/utils/__init__.py:762`) directly: the same formula `storage_node_ops.py:9465-9470` already uses at node add and restart, so this endpoint and the real provisioning path can never compute two different answers. Idempotent (pure function of its two inputs). Belongs in `simplyblock_web/api/v2/cluster/__init__.py`, beside the existing `validate_spdk_sizing`/`set_spdk_sizing`. |

`kubernetes/atlas-lib/controlplane` gains `EstimateHugePages(ctx context.Context, maxSubsys, vcpuCount int32) (mib int64, err error)`, alongside `CreateClusterParams` (`clusters.go:76-117`), backed by a regenerated `cpapi.gen.go` client method (the `build-system` skill's atlas-generated-client regeneration step).

---

## 8. Configuration

| Field                                                               | Type            | Default               | Description                                                                                       |
|---------------------------------------------------------------------|-----------------|-----------------------|---------------------------------------------------------------------------------------------------|
| `spec.cluster.hugePagesMode` (`ClusterTemplate`)                    | `HugePagesMode` | unset                 | Mode for every node this document creates, unless a node set overrides it.                        |
| `spec.nodeSets[].cluster.hugePagesMode` (`NodeSetClusterOverrides`) | `HugePagesMode` | inherits the template | Per-node-set override, valid under the same conditions `MinHugePagesSize`'s existing override is. |
| `spec.storageNodes.hugePagesMode` (`StorageCluster`)                | `HugePagesMode` | unset                 | The field the document's value is copied into, and may also be set directly.                      |

Reconfigurable at any time before expansion (a `Draft` document is re-validated on every reconcile, §1). Once a `ClusterDeploymentConfig` is `approved` it is immutable in its entirety (§3.2 of `design-clusterdeploymentconfig.md`), `hugePagesMode` included.

---

## 9. Failure Modes and Fallback

| Failure                                                                                                          | Detection                                            | Behavior                                                                                                                                                                                        |
|------------------------------------------------------------------------------------------------------------------|------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Target worker lacks `hugepages-2Mi` capacity                                                                     | Step 2 (§5)                                          | `HugePagesReady=False`, reason `InsufficientHugePages`, naming the required and reported MiB                                                                                                    |
| Target worker's huge pages are held by other pods                                                                | Step 3 (§5)                                          | `HugePagesReady=False`, reason `HugePagesContended`, naming the holding pods                                                                                                                    |
| `hugePagesMode: Exclusive`/`Shared` on create or update, off its platform                                        | Admission (§6.2)                                     | Denied. No object is persisted                                                                                                                                                                  |
| `POST /cluster/sizing/hugepages-estimate` unreachable or erroring, from the `ClusterDeploymentConfig` controller | `cpClient.EstimateHugePages` error                   | Reconcile error, requeued. `HugePagesReady` is left at its last-known value rather than flipped to a guess (never to `True` on an error)                                                        |
| Same, from the `StorageNode` webhook                                                                             | `cpClient.EstimateHugePages` error                   | Denied (§13, Q1): the node add fails downstream regardless once the reconcile reaches the control plane, so failing closed at admission moves that failure earlier rather than adding a new one |
| `Exclusive`, Talos API access Secret missing                                                                     | `_talos_api_access_missing` (§14)                    | Node add fails with the Secret's name and how to create it, before any Job runs                                                                                                                 |
| `Exclusive`, host's huge-page pool does not converge within the wait window                                      | `_wait_for_talos_pool` (§14)                         | Node add fails, naming the NUMA nodes still short                                                                                                                                               |
| `Shared`, `MachineConfigPool` render does not complete within the wait window                                    | The CPU-topology Job's own render wait, reused (§15) | Node add fails or joins the host to an incomplete render with a warning, exactly as the CPU-topology-only case does today                                                                       |
| `Shared`, kernel argument does not reflect in the kubelet's reported capacity after the reboot                   | A new per-NUMA capacity wait (§15 Step 4)            | Node add fails, naming the sockets still short, rather than starting SPDK against a pool that may not match `_hugepages_per_numa`'s per-socket sizing                                           |

---

## 10. Observability

`ClusterDeploymentConfig` and `StorageCluster`/`StorageNode` already emit `Eventf` for other validation failures (`storagecluster_controller.go:1104-1116`) but carry no `Conditions` and no huge-page-specific metric. Both tables below are new.

### Kubernetes Events

Target: the `ClusterDeploymentConfig` for the controller path, the object a reviewer owns and that outlives the `StorageNode`s it has not yet created. The `StorageNode` webhook path denies the request itself. A denied admission carries its own message and needs no separate Event.

| Event                                                                                                                         | Type    | Reason                  |
|-------------------------------------------------------------------------------------------------------------------------------|---------|-------------------------|
| Worker `<name>` does not have enough preprovisioned huge pages: needs `<X>`Mi of `hugepages-2Mi`, reports `<Y>`Mi allocatable | Warning | `InsufficientHugePages` |
| Worker `<name>`'s `hugepages-2Mi` is held by other pods (`<names>`), leaving `<Z>`Mi free of the `<X>`Mi needed               | Warning | `HugePagesContended`    |

`Exclusive` and `Shared` emit no Kubernetes event of their own (§14, §15): both provision at node-add time, inside the same `sbcli` Job whose failure already fails the `StorageNode` add with a message naming the cause, logged where every other node-add failure is.

### Prometheus Metrics

| Metric                                             | Labels                                    | Description                                                                                                                                         |
|----------------------------------------------------|-------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `simplyblock_hugepages_precheck_failures_total`    | `cluster`, `reason`                       | Count of Preprovisioned precheck failures, by `InsufficientHugePages`/`HugePagesContended`, each time a reconcile finds the condition still `False` |
| `simplyblock_hugepages_estimate_duration_seconds`  | —                                         | Histogram of the §7 endpoint's call latency as observed by the operator                                                                             |
| `simplyblock_hugepages_provision_duration_seconds` | `cluster`, `mode` (`exclusive`\|`shared`) | Histogram of how long a node add spent provisioning huge pages (§14, §15), from the control plane's side of the Job, not the operator's             |

`precheck_failures_total` is what lets an operator tell "this cluster is undersized" apart from "something else is eating the pages" from a dashboard, without reading events per node. `estimate_duration_seconds` is what decides whether the call is slow enough to threaten the `StorageNode` admission webhook's timeout, since that path blocks on it synchronously. `provision_duration_seconds` is what tells a slow `Shared` reboot wave apart from a stuck one, since §15's wait windows are long enough that "still running" and "never going to finish" look the same without it.

---

## 11. Testing Strategy

Full scenario matrix: [`tests/test-plan-preprovisioned-hugepages.md`](../tests/test-plan-preprovisioned-hugepages.md)

- **Unit:** the three-step algorithm in `internal/hugepages` against a fake `client.Client` and a stub `controlplane.Client`, covering each of `InsufficientHugePages`, `HugePagesContended`, and the pass case, the Step 1 clamp-then-round arithmetic at its boundaries, `platform.HugePagesModeSupported` for all three mode values against every `Platform` combination, and all three validators' denial paths. `sbcli` unit tests cover `_talos_hugepages_managed`/`_talos_machine_config_managed` disagreeing (§4.3), the kernel-argument text `oc_storage_cpu_topology.yaml.j2`'s hugepages block renders, and the `HugePagesMode`-to-`TalosMachineConfig` default (§12).
- **Integration:** the `ClusterDeploymentConfig` reconcile loop against `envtest` and a mock `sbcli` HTTP server. A document whose node set resolves to an undersized worker never reaches `CreatingNodes` (§1's step machine), and `HugePagesReady` is set before any `StorageNode` is created. Platform-aware admission (§6.2) against every `Talos`/`OpenShift` combination.
- **E2E:** a live cluster with one worker whose `hugepages-2Mi` is deliberately short, or deliberately held by an unrelated pod, confirming no SPDK pod is ever created and the condition names the right reason. A live Talos cluster exercising `Exclusive` end to end (§14), and a live OpenShift cluster exercising `Shared` end to end, reboot included (§15): these need the real platform, so no fake or `envtest` double exists for either.

The risk concentrates in Step 3 (§5) for Phase 1, which is the one piece of that phase with no existing precedent in the codebase (Step 1 and Step 2 both read numbers something already computes), so its boundary cases (zero other pods, a pod requesting exactly the remainder, a pod that is one of this cluster's own nodes) are where a cut corner hides. For Phase 3, the risk concentrates in the reboot wait (§15 Step 3): it is the one new wait this design adds to a sequence that already has three (MCP render, MCO reboot, NUMA-plugin readiness), and distinguishing "still rebooting" from "the kernel argument did not take" needs the per-NUMA capacity check of §15 Step 4, not the total alone.

---

## 12. Migration Strategy

Purely additive on every platform but Talos. `HugePagesMode` left unset (every document written before this field existed) runs no check and provisions nothing, exactly as today. A cluster opts in by setting `hugePagesMode` explicitly, to whichever of the three values its platform supports (§6.2).

**On Talos, the propagation step that copies `HugePagesMode` onto `StorageCluster.Spec` (§4.1) defaults it from `TalosMachineConfig` when it is left unset:**

```go
if cluster.Spec.StorageNodes.Talos != nil && *cluster.Spec.StorageNodes.Talos &&
	cluster.Spec.StorageNodes.HugePagesMode == "" {
	switch cluster.Spec.StorageNodes.TalosMachineConfig {
	case v1alpha2.TalosMachineConfigModeManaged:
		cluster.Spec.StorageNodes.HugePagesMode = v1alpha2.HugePagesModeExclusive
	default: // "" and Preprovisioned both read as Preprovisioned today
		cluster.Spec.StorageNodes.HugePagesMode = v1alpha2.HugePagesModePreprovisioned
	}
}
```

This is what makes a Talos cluster that set `talosMachineConfig: Managed` before `HugePagesMode` existed keep provisioning its huge pages exactly as before, without being rewritten: the default runs on every reconcile, not once at an upgrade boundary, so it needs no migration step and nothing to run against an already-existing `StorageCluster`. A cluster that explicitly sets `hugePagesMode` is never defaulted, on Talos or anywhere else.

`ClusterDeploymentConfig` has no `v1alpha1` type (confirmed against `api/v1alpha1/`), so no conversion webhook is affected.

---

## 13. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | Owner             |
|-----|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------|
| 1   | ~~**Webhook behavior when `/cluster/sizing/hugepages-estimate` is unreachable.**~~ **Resolved.** Deny the `StorageNode` create (fail closed). An unreachable control plane fails every subsequent operation against it anyway, so the node add was never going to succeed once the reconcile reached it. Failing closed at admission only moves that failure earlier, not wider. §9 carries the decision.                                                                                                                                                                                                                                                                           | Resolved          |
| 2   | ~~**When `Exclusive`/`Shared` are implemented, does `platform.HugePagesModeSupported` gain an `Environment` parameter?**~~ **Resolved.** It gains a `Platform` struct instead, assembled from `StorageCluster.spec.storageNodes.talos`/`.openshift` (or `ClusterDeploymentConfig.spec.environment` pre-expansion), per §6.2: whether the platform makes a mode conceivable is cheap and belongs at admission, the same "existence" half the `api-design` skill already draws for object references. Whether the live cluster is actually *configured* for it is the "readiness" half, and belongs in a controller-level condition mirroring `HugePagesReady`, not in this function. | Resolved          |
| 3   | **Per-NUMA distribution of an OpenShift kernel-argument huge-page reservation.** `hugepagesz=2M hugepages=N` is a single cluster-wide count. RHCOS's default placement policy is believed to spread it evenly across online NUMA nodes, but this is not yet verified against a real multi-socket OpenShift worker, which is why §15 Step 4 checks per-NUMA capacity rather than trusting the total (P0-2).                                                                                                                                                                                                                                                                          | SPDK/Backend team |
| 4   | **Env var naming for the split in §4.3** (`TALOS_HUGEPAGES_MODE`, `OPENSHIFT_HUGEPAGES_MODE`): proposed here, confirmed or changed at implementation time against `sbcli`'s existing `TALOS_`/`OPENSHIFT_`-prefixed convention (`constants.py:424-441`).                                                                                                                                                                                                                                                                                                                                                                                                                            | `sbcli` team      |

---

## 14. Exclusive Mode — Talos (Phase 2)

`hugePagesMode: Exclusive` reuses `talosMachineConfig: Managed`'s existing mechanism (`_provision_talos_hugepages`, `_talos_managed_pool`, `storage_talos_hugepages_job.yaml.j2`) under its generalized name, rather than replacing it with an operator-driven one. The Talos API is the one mechanism Talos offers for changing a host's huge-page pool outside the running kernel, and that mechanism is already built, tested, and in production use under `talosMachineConfig`: nothing about it is Preprovisioned-vs-Exclusive-specific, only Talos-specific, so generalizing the name is the entire scope of this phase.

### 14.1 What changes in `sbcli`

`_talos_machine_config_managed()` (`kubernetes.py:537-556`) stops being `_provision_talos_hugepages`'s gate. A new, independent function takes its place for huge pages alone:

```python
_TALOS_HUGEPAGES_MODES = ("exclusive", "preprovisioned")


def _talos_hugepages_managed():
    """Whether this node may grow a Talos worker's huge-page pool through
    the Talos API. TALOS_HUGEPAGES_MODE is exclusive or preprovisioned; the
    operator sets it from the resolved HugePagesMode (design
    §4.3/§12), independently of TALOS_MACHINE_CONFIG, which now governs the
    kubelet's CPU topology alone. preprovisioned is the default, and what an
    unknown value falls back to with a warning, for the same reason
    _talos_machine_config_managed's default is preprovisioned: the mode
    that touches nothing is the safe one.
    """
    mode = (os.environ.get("TALOS_HUGEPAGES_MODE") or constants.TALOS_HUGEPAGES_MODE).strip().lower()
    if mode not in _TALOS_HUGEPAGES_MODES:
        logger.warning("TALOS_HUGEPAGES_MODE=%r is not one of %s; treating the huge-page pool as pre-provisioned",
                       mode, "/".join(_TALOS_HUGEPAGES_MODES))
        return False
    return mode == "exclusive"
```

`_provision_talos_hugepages` replaces its one call to `_talos_machine_config_managed()` with `_talos_hugepages_managed()` (`kubernetes.py:1037`). Nothing else in that function changes: `_talos_managed_pool`'s user-baseline arithmetic, the `patch`/`kubelet` Job sequence, and `_wait_for_talos_pool` are exactly what `Exclusive` runs. `_configure_talos_kubelet_topology` keeps calling `_talos_machine_config_managed()`, unchanged, since CPU topology is no longer this field's concern (§4.3).

### 14.2 What changes in the operator

The expansion sets `TALOS_HUGEPAGES_MODE` on the node agent's DaemonSet from `StorageCluster.Spec.StorageNodes.HugePagesMode` (already resolved by the default of §12), the same way it already sets `TALOS_MACHINE_CONFIG` from `TalosMachineConfig`. `platform.HugePagesModeSupported` admits `Exclusive` when `Platform.Talos` is true (§6.2). No `Evaluate` call runs for `Exclusive` (§5): there is nothing to verify before the node add, since the node add is what grows the pool.

### 14.3 What does not change

The Talos API access Secret requirement, the per-NUMA sizing (`_hugepages_per_numa`), the kubelet restart, and every wait and failure message in `_provision_talos_hugepages` (§9) are exactly as `talosMachineConfig: Managed` already runs them. A reader auditing `Exclusive`'s behavior reads that function, not a new one.

---

## 15. Shared Mode — OpenShift (Phase 3)

`hugePagesMode: Shared` has the operator grow the OpenShift storage `MachineConfig` the CPU-topology Job already creates (`storage-mc-<cluster>`, `storage_mco_names`) to also carry a boot-time huge-page reservation, through the kernel argument OpenShift's `MachineConfig` CRD supports natively (`spec.kernelArguments`), rather than through the Node Tuning Operator's `PerformanceProfile`. The reservation stays a Kubernetes `hugepages-2Mi` extended resource like any other: Kubernetes schedules every hugepage-requesting pod against it by ordinary resource accounting, so the pool is genuinely shared with whatever else on the host requests the same page size, and the contention accounting of §5 Step 3 remains meaningful after this mode provisions, unlike `Exclusive`'s dedicated pool.

### 15.1 Reusing the CPU-topology `MachineConfig`

`storage-mc-<cluster>` is created today with an empty Ignition payload (`oc_storage_cpu_topology.yaml.j2:122-134`), existing purely so the storage `MachineConfigPool` has a `MachineConfig` to select alongside the generated `KubeletConfig`. When `HugePagesMode == HugePagesModeShared`, its creation carries `spec.kernelArguments` as well:

```yaml
apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  name: $MC_NAME
  labels:
    machineconfiguration.openshift.io/role: $MCP_NAME
spec:
  config:
    ignition:
      version: 3.2.0
  kernelArguments:
    - hugepagesz=2M
    - hugepages={{ HUGEPAGES_TOTAL_PAGES }}
```

`HUGEPAGES_TOTAL_PAGES` is `MEM_MEGA // 2`, the same figure `spdk_process_start` already derives for every other platform's pod resource limit, rounded up to a whole 2 MiB page. Because the `MachineConfig` is only ever created once per cluster (`kubernetes.py:118-134`: `kubectl get machineconfig "$MC_NAME"` skips creation if it already exists), the kernel argument is fixed at the cluster's first node add and does not grow with later sizing changes within the same cluster. A resize large enough to need more huge pages than the cluster's first node requested is `design-storagecluster.md`'s concern, not this one's.

### 15.2 Forcing the Job to run

The CPU-topology Job runs only when `wants_host_config` (`kubernetes.py:1367`: `cpu_topology_enabled and not skip_kubelet_configuration`, or `core_isolate`). A document could set `hugePagesMode: Shared` while leaving CPU topology disabled, which would otherwise mean the `MachineConfig` that carries the kernel argument is never created. `wants_host_config` gains a third disjunct, `openshift_hugepages_shared`, so `Shared` always runs the Job regardless of the CPU-topology flags:

```python
openshift_hugepages_shared = (os.environ.get("OPENSHIFT_HUGEPAGES_MODE") or "").strip().lower() == "shared"
wants_host_config = (
    (cpu_topology_enabled and not skip_kubelet_configuration)
    or (core_isolate and not cpu_topology_enabled)
    or openshift_hugepages_shared
)
```

### 15.3 Sequencing

The reboot this mode needs is the same one the CPU-topology Job already waits for (`kubernetes.py:236-277`, `_wait_for_job_completion` with its 700-second timeout), and the SPDK-start gate that already blocks on the MCO reboot actually landing (`kubernetes.py:1449-1460`) blocks for the same reason either setting changes the pool's render. No new wait is added for the render or the reboot: `Shared` adds a field to a request this code already makes and already waits on.

### 15.4 Per-NUMA capacity after the reboot

The MCO reboot gate confirms the node is `Ready` again and that the NUMA resource plugin reports the sockets the cluster's node configs name, not that the kernel actually placed `HUGEPAGES_TOTAL_PAGES` the way `_hugepages_per_numa` assumed. Before starting SPDK, the node reads its own `hugepages-2Mi` capacity per NUMA node from `/sys/devices/system/node/node<N>/hugepages/hugepages-2048kB/nr_hugepages` (the same path `_talos_nr_hugepages` already reads) and fails the add, naming the sockets still short, when any one of them carries less than `_hugepages_per_numa` requires for it. This is the check P0-2 (§13, Q3) exists for: a cluster-wide kernel argument is not guaranteed to land evenly, and `_wait_for_kubelet_hugepages`'s existing total-capacity check cannot tell an even split from a lopsided one that happens to sum to the same total.

### 15.5 What does not change

`Preprovisioned` stays available on OpenShift exactly as on every other platform (§6.2's `Platform.OpenShift` only widens what is possible, it narrows nothing already possible): an administrator who already manages huge pages through the Node Tuning Operator's own `PerformanceProfile`, or by any other means, selects `Preprovisioned` and the operator verifies rather than provisions. `Shared` is the additional choice for a cluster that wants the operator to make the reservation itself, through the mechanism this product already builds for CPU topology.
