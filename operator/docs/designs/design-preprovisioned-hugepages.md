# Design Document: Preprovisioned Huge-Page Mode

**Status:** Draft  
**Author:** Max Schettler  
**Date:** 2026-11-24  
**Test Plan:** [`tests/test-plan-preprovisioned-hugepages.md`](../tests/test-plan-preprovisioned-hugepages.md)

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                   | Kind                    | Blocks                  | Status      |
|------|------------------------------------------------|-------------------------|-------------------------|-------------|
| P0-1 | `POST /cluster/sizing/hugepages-estimate` (§7) | Control plane (`sbcli`) | The whole precheck (§5) | Not shipped |

Without P0-1 the operator has no way to learn the huge-page figure `calculate_minimum_hp_memory` would compute for a given `maxSubsys`/`vcpuCount` without re-implementing that formula in Go. Every section below assumes it exists.

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

---

## Overview

A `ClusterDeploymentConfig` (or a `StorageCluster`/`StorageNode` written directly) can select `hugePagesMode: Preprovisioned`, which states that the workers already carry the huge pages their storage nodes need. The operator then checks that before any `StorageNode` is created: it asks the control plane how many MiB of `hugepages-2Mi` the cluster's sizing actually requires, reads what each candidate worker's kubelet reports as huge-page capacity and allocatable, and reports `InsufficientHugePages` or `HugePagesContended` on the document rather than letting the shortfall surface as a `StorageNode`'s SPDK pod stuck `Pending`.

`Exclusive` and `Shared` are named in the same enum but implemented on no platform yet, and selecting either is refused at admission (§6.2) rather than silently accepted as a no-op.

---

## 1. Background

Today, huge-page provisioning for a storage node's SPDK pod is `sbcli`'s responsibility end to end, not the operator's:

- The pod's manifest (`storage_deploy_spdk.yaml.j2`) requests `hugepages-2Mi` as a resource limit, sized from `spdk_mem`.
- Outside Talos, the node-preparation Job (`storage_init_job.yaml.j2`) and the node agent (`simplyblock_web/api/internal/storage_node/kubernetes.py`, `/snode/apply_config`, `utils.set_hugepages_if_needed`) write `nr_hugepages` to the host's sysfs directly, claiming the pages for simplyblock.
- On Talos, `StorageCluster.spec.storageNodes.talosMachineConfig` (`TalosMachineConfigMode`, values `Preprovisioned`/`Managed`, `storagecluster_types.go:743-753`) already splits the same two ideas this design generalizes: `Preprovisioned` verifies only and fails the add with the exact `talosctl patch` to apply. `Managed` has the node apply it itself through the Talos API (`storage_talos_hugepages_job.yaml.j2`). `StorageCluster.spec.storageNodes.talos` (`storagecluster_types.go:727-734`) is the bool that turns this path on, set by the expansion from `ClusterDeploymentConfig.spec.environment` (`clusterdeploymentconfig_types.go:904-911`).

In every path, a shortfall that is not caught before the `StorageNode` is created surfaces as the SPDK pod staying `Pending` with Kubernetes' own `Insufficient hugepages-2Mi` scheduling event, discovered late and without saying which of two different causes it is: the host never had enough, or it did but something else is holding the pages.

This design introduces `HugePagesMode` on `ClusterDeploymentConfig`/`StorageCluster`/`StorageNode`, generalizing the Talos `Preprovisioned`/`Managed` split by name (`Preprovisioned`, `Exclusive`, `Shared`) and by starting the implementation with the verify-only half, available on every platform, before any node is created. `Exclusive` would generalize Talos's `Managed` path beyond `talosctl`. `Shared` has no existing precedent in this codebase. Neither is designed here (§2).

The sizing figure the precheck compares against is not new either: `calculate_minimum_hp_memory` (`simplyblock_core/utils/__init__.py:762`) already computes it from `max_subsys` and `vcpu_count` alone, at every node add and node restart (`storage_node_ops.py:9465-9470`), and `MinHugePagesSize` (`ClusterTemplate`/`NodeSetClusterOverrides`, `clusterdeploymentconfig_types.go:396`/`465`) is already nothing more than a manual floor clamped onto that same formula's output. Nothing about "how much" is new. This design is about running that same arithmetic, and the capacity comparison it enables, before the pod exists instead of after.

---

## 2. Goals and Non-Goals

### Goals

- A `ClusterDeploymentConfig`, `StorageCluster`, or `StorageNode` with `hugePagesMode: Preprovisioned` fails before any `StorageNode` is created when a target worker does not have enough `hugepages-2Mi` capacity for the cluster's sizing (§5).
- The same document fails, with a distinguishable reason, when the worker's capacity is enough on its own but is contended by non-simplyblock pods already scheduled on it (§5).
- The required huge-page figure is computed exactly once, in `sbcli`, and the operator never re-implements `calculate_minimum_hp_memory` (§7).
- `hugePagesMode: Exclusive` and `hugePagesMode: Shared` are refused at admission everywhere, now, rather than accepted and silently ignored (§6.2).
- A document that leaves `hugePagesMode` unset behaves exactly as it does today (§12).

### Non-Goals

- **`Exclusive` mode's implementation:** generalizing the Talos `Managed` Job mechanism (`storage_talos_hugepages_job.yaml.j2`) to other platforms, and replacing `sbcli`'s `apply_config`/`set_hugepages_if_needed` call path with an operator-driven one. Named in the enum only.
- **`Shared` mode's implementation:** requesting huge pages alongside other consumers of a host, reliably only on a platform (OpenShift) with its own huge-page reservation mechanism. Named in the enum only.
- **Platform-aware admission.** §6.2's rejection of `Exclusive`/`Shared` is unconditional across every value of `ClusterDeploymentConfigSpec.Environment` (`clusterdeploymentconfig_types.go:904-911`), not a per-platform decision. `Environment`, `StorageCluster.spec.storageNodes.talos`, and `.openshift` already exist and are exactly what a future per-platform decision would key off (§13, Q2), but this design's own gate does not consume them, because the answer is "no" regardless of their value today.
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
│  │   platform.HugePagesModeSupported(mode) — refuses Exclusive/  │    │
│  │   Shared at admission, both kinds                              │    │
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

---

## 4. API Design — Field Additions

No new CRD. Three existing types gain fields. Each is specified by another document and only the addition is shown here (bare fields with their markers, not the surrounding type).

### 4.1 `ClusterTemplate` / `NodeSetClusterOverrides` — `hugePagesMode` (new)

Specified by `design-clusterdeploymentconfig.md`, whose Appendix A is `clusterdeploymentconfig_types.go`. Added as a sibling to the existing `MinHugePagesSize string` field in both `ClusterTemplate` (`:459-465`) and `NodeSetClusterOverrides` (`:392-396`), not nested into a new struct, which would rename the already-shipped `minHugePagesSize` field for no reason tied to this design.

```go
// HugePagesMode selects how huge pages are managed for this cluster's
// storage nodes. Preprovisioned assumes the workers already carry enough
// and checks that before any StorageNode is created, rather than letting a
// shortfall surface as a Pending SPDK pod (§5). It is the only mode this
// deployment can currently select: Exclusive and Shared are reserved for
// future platform-specific support and are refused at admission (§6.2).
// Left unset, no huge-page check runs, which is this deployment's behavior
// without this field.
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
	// huge pages for the cluster's sizing. The only mode available today;
	// see platform.HugePagesModeSupported (§6.2).
	HugePagesModePreprovisioned HugePagesMode = "Preprovisioned"

	// HugePagesModeExclusive has simplyblock configure and own the host's
	// huge-page pool. Not implemented on any platform; refused at
	// admission (§6.2).
	HugePagesModeExclusive HugePagesMode = "Exclusive"

	// HugePagesModeShared requests huge pages alongside other consumers of
	// the host. Not implemented on any platform; refused at admission
	// (§6.2).
	HugePagesModeShared HugePagesMode = "Shared"
)
```

`HugePagesMode` is propagated from `ClusterTemplate`/`NodeSetClusterOverrides` onto `StorageCluster.Spec` the same way `MinHugePagesSize` already is: `storagesitedeployment_controller.go:268-269`, `expansion.go:338,792`, `supercluster.go:117-118`.

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

A worker whose allocatable passes Step 2 may still not have `memMegaMiB` actually free: Kubernetes reports capacity and allocatable as static Node fields, not "what is free right now," so a pod already consuming part of the pool would not shrink either number. List the Pods scheduled on that worker, sum the `hugepages-2Mi` resource requests of every pod that is not one of this cluster's own storage nodes, and subtract from `AllocatableBytes`. A remainder below `memMegaMiB` is `HugePagesContended`, and the failure message names the pods that hold the difference. This replicates the scheduler's own extended-resource bookkeeping, because no Kubernetes API reports an extended resource's free amount directly.

### Algorithm Summary

```go
// internal/hugepages/precheck.go
func Evaluate(
	ctx context.Context,
	k8sClient client.Client,
	cpClient controlplane.Client,
	workers []discovery.KubeNode,
	maxSubsys, vcpuCount int32,
	minHugePagesSizeMiB int64,
) (*Result, error) {
	memMegaMiB, err := requiredMiB(ctx, cpClient, maxSubsys, vcpuCount, minHugePagesSizeMiB) // Step 1
	if err != nil {
		return nil, err
	}
	result := &Result{RequiredMiB: memMegaMiB}
	for _, worker := range workers {
		cap, ok := worker.HugePages.Find(twoMiB)
		if !ok || cap.AllocatableBytes < memMegaMiB*mib {
			result.Fail(worker.Name, InsufficientHugePages, ...) // Step 2
			continue
		}
		contendedBy, free := contention(ctx, k8sClient, worker.Name, memMegaMiB) // Step 3
		if free < memMegaMiB {
			result.Fail(worker.Name, HugePagesContended, contendedBy, ...)
		}
	}
	return result, nil
}
```

Only reached when `HugePagesMode == HugePagesModePreprovisioned` (§6).

---

## 6. Controller and Webhook Design

### 6.1 `Evaluate` call sites

| Site                                | Package / file                                                                                   | Scope                                            | Runs before                                                    |
|-------------------------------------|--------------------------------------------------------------------------------------------------|--------------------------------------------------|----------------------------------------------------------------|
| `ClusterDeploymentConfig` reconcile | `internal/controllers/deployment/clusterdeploymentconfig_controller.go`                          | Every worker the document's node sets resolve to | Any `StorageCluster`/`StorageNode` the document would generate |
| `StorageNode` admission             | `internal/webhook/storagenode_validator.go`, `admitCreate`, beside `sizingDenial()` (`:304-322`) | The one worker `spec.workerNode` names           | The `StorageNode` object itself                                |

Both are gated on `HugePagesMode == HugePagesModePreprovisioned`, read off the owning `StorageCluster` for the `StorageNode` site. A failing evaluation sets `HugePagesReady=False` on the `ClusterDeploymentConfig`'s (or `StorageCluster`'s) `status.conditions` in the first case, and denies the admission request in the second.

### 6.2 Refusing `Exclusive` and `Shared`

```go
// internal/platform/hugepages.go

// HugePagesModeSupported reports whether this operator can act on mode.
// Preprovisioned needs nothing beyond the check in §5 and is always
// available. Exclusive and Shared are not implemented on any platform yet,
// so this returns false for both unconditionally: it does not consult
// ClusterDeploymentConfigSpec.Environment, StorageCluster.spec.storageNodes.talos,
// or .openshift, even though all three already exist and are exactly what a
// future per-platform decision would read (§13, Q2). Threading an unused
// platform argument through now would not change today's answer for any of
// them.
func HugePagesModeSupported(mode v1alpha2.HugePagesMode) bool {
	return mode == "" || mode == v1alpha2.HugePagesModePreprovisioned
}
```

Called from `ClusterDeploymentConfigValidator.Handle` and `StorageClusterValidator.Handle` (both already validate other fields on create/update, `clusterdeploymentconfig_validator.go:82-90`, `storagecluster_validator.go:60-86`), denying with `hugePagesMode: Exclusive is not supported on any platform yet` (worded accordingly for `Shared`). This cannot be a `+kubebuilder:validation:XValidation` rule: determining platform support is meant to vary per `Environment` once `Exclusive`/`Shared` are implemented, and a CEL rule evaluated against a single object cannot read another object's state, which a later, platform-aware version of this function will need to do. It is webhook logic from the start, so that later version is a change to one function, not a migration off CEL.

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

| Failure                                                                                                          | Detection                          | Behavior                                                                                                                                                                                        |
|------------------------------------------------------------------------------------------------------------------|------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Target worker lacks `hugepages-2Mi` capacity                                                                     | Step 2 (§5)                        | `HugePagesReady=False`, reason `InsufficientHugePages`, naming the required and reported MiB                                                                                                    |
| Target worker's huge pages are held by other pods                                                                | Step 3 (§5)                        | `HugePagesReady=False`, reason `HugePagesContended`, naming the holding pods                                                                                                                    |
| `hugePagesMode: Exclusive`/`Shared` on create or update                                                          | Admission (§6.2)                   | Denied. No object is persisted                                                                                                                                                                  |
| `POST /cluster/sizing/hugepages-estimate` unreachable or erroring, from the `ClusterDeploymentConfig` controller | `cpClient.EstimateHugePages` error | Reconcile error, requeued. `HugePagesReady` is left at its last-known value rather than flipped to a guess (never to `True` on an error)                                                        |
| Same, from the `StorageNode` webhook                                                                             | `cpClient.EstimateHugePages` error | Denied (§13, Q1): the node add fails downstream regardless once the reconcile reaches the control plane, so failing closed at admission moves that failure earlier rather than adding a new one |

---

## 10. Observability

`ClusterDeploymentConfig` and `StorageCluster`/`StorageNode` already emit `Eventf` for other validation failures (`storagecluster_controller.go:1104-1116`) but carry no `Conditions` and no huge-page-specific metric. Both tables below are new.

### Kubernetes Events

Target: the `ClusterDeploymentConfig` for the controller path, the object a reviewer owns and that outlives the `StorageNode`s it has not yet created. The `StorageNode` webhook path denies the request itself. A denied admission carries its own message and needs no separate Event.

| Event                                                                                                                         | Type    | Reason                  |
|-------------------------------------------------------------------------------------------------------------------------------|---------|-------------------------|
| Worker `<name>` does not have enough preprovisioned huge pages: needs `<X>`Mi of `hugepages-2Mi`, reports `<Y>`Mi allocatable | Warning | `InsufficientHugePages` |
| Worker `<name>`'s `hugepages-2Mi` is held by other pods (`<names>`), leaving `<Z>`Mi free of the `<X>`Mi needed               | Warning | `HugePagesContended`    |

### Prometheus Metrics

| Metric                                            | Labels              | Description                                                                                                                                         |
|---------------------------------------------------|---------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `simplyblock_hugepages_precheck_failures_total`   | `cluster`, `reason` | Count of Preprovisioned precheck failures, by `InsufficientHugePages`/`HugePagesContended`, each time a reconcile finds the condition still `False` |
| `simplyblock_hugepages_estimate_duration_seconds` | —                   | Histogram of the §7 endpoint's call latency as observed by the operator                                                                             |

`precheck_failures_total` is what lets an operator tell "this cluster is undersized" apart from "something else is eating the pages" from a dashboard, without reading events per node. `estimate_duration_seconds` is what decides whether the call is slow enough to threaten the `StorageNode` admission webhook's timeout, since that path blocks on it synchronously.

---

## 11. Testing Strategy

Full scenario matrix: [`tests/test-plan-preprovisioned-hugepages.md`](../tests/test-plan-preprovisioned-hugepages.md)

- **Unit:** the three-step algorithm in `internal/hugepages` against a fake `client.Client` and a stub `controlplane.Client`, covering each of `InsufficientHugePages`, `HugePagesContended`, and the pass case, the Step 1 clamp-then-round arithmetic at its boundaries, `platform.HugePagesModeSupported` for all three mode values, and both validators' new denial path.
- **Integration:** the `ClusterDeploymentConfig` reconcile loop against `envtest` and a mock `sbcli` HTTP server. A document whose node set resolves to an undersized worker never reaches `CreatingNodes` (§1's step machine), and `HugePagesReady` is set before any `StorageNode` is created.
- **E2E:** a live cluster with one worker whose `hugepages-2Mi` is deliberately short, or deliberately held by an unrelated pod, confirming no SPDK pod is ever created and the condition names the right reason.

The risk concentrates in Step 3 (§5): it is the one piece of this design with no existing precedent in the codebase (Step 1 and Step 2 both read numbers something already computes), so its boundary cases (zero other pods, a pod requesting exactly the remainder, a pod that is one of this cluster's own nodes) are where a cut corner hides.

---

## 12. Migration Strategy

Purely additive. `HugePagesMode` left unset (every document written before this field existed) runs no check and behaves exactly as today. A cluster opts in by setting `hugePagesMode: Preprovisioned` explicitly. There is no default that selects it automatically, and no "upgrade" step, because this design implements only one of the three modes, and defaulting existing clusters into a mode with no implementation defaults them into nothing.

`ClusterDeploymentConfig` has no `v1alpha1` type (confirmed against `api/v1alpha1/`), so no conversion webhook is affected.

---

## 13. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Owner    |
|-----|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|
| 1   | ~~**Webhook behavior when `/cluster/sizing/hugepages-estimate` is unreachable.**~~ **Resolved.** Deny the `StorageNode` create (fail closed). An unreachable control plane fails every subsequent operation against it anyway, so the node add was never going to succeed once the reconcile reached it. Failing closed at admission only moves that failure earlier, not wider. §9 carries the decision.                                                                                                                                                                                                                                                                                                                                                                                                                       | Resolved |
| 2   | ~~**When `Exclusive`/`Shared` are implemented, does `platform.HugePagesModeSupported` gain an `Environment` parameter?**~~ **Resolved**, as direction for whichever design implements those modes, not as a change here. `HugePagesModeSupported` stays declarative: whether a declared `Environment` makes a mode conceivable (`OpenShift` for `Shared`, `Talos` for `Exclusive`) is cheap and belongs at admission, the same "existence" half the `api-design` skill already draws for object references. Whether the live cluster is actually *configured* for it (the Node Tuning Operator applied, the static CPU Manager policy set) is the "readiness" half, and belongs in a controller-level condition mirroring `HugePagesReady`, not in this function. This design implements neither mode, so nothing here changes. | Resolved |
