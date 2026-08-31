# Design Document: The ControlPlane and Its Operations

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-29  
**Test Plan:** [`tests/test-plan-controlplane.md`](../../tests/test-plan-controlplane.md)

This document specifies the target model. `ControlPlane` is registered and in a
shape that predates the conventions of
[`design-crd-model.md`](design-crd-model.md), `ControlPlaneOps` does not exist,
and §11 is the single record of what the rework changes against what ships.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [ControlPlane: API](#3-controlplane-api)
4. [ControlPlane: Controller](#4-controlplane-controller)
5. [Reuse and Install Are One Kind](#5-reuse-and-install-are-one-kind)
6. [ControlPlaneOps](#6-controlplaneops)
7. [Mutual Exclusion](#7-mutual-exclusion)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Observability](#9-observability)
10. [Testing Strategy](#10-testing-strategy)
11. [Migration from the Registered API](#11-migration-from-the-registered-api)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `controlplane_types.go`](#appendix-a-controlplane_typesgo)
- [Appendix B: `controlplaneops_types.go`](#appendix-b-controlplaneops_typesgo)

---

## Overview

`ControlPlane` is the simplyblock control plane, meaning FoundationDB together
with the management API, expressed as a Kubernetes resource. Everything else in
the API group depends on it: a `StorageCluster` cannot be created, a
`StorageNode` cannot be added, and a volume cannot be provisioned until it
reports `Available`.

It is the root of the ownership spine ([`design-crd-model.md`](design-crd-model.md)
§5) and the smallest kind in the group, which is a combination worth naming. Its
surface is one image and a phase. Its contract is that nothing downstream is
meaningful before it holds.

The CSI driver is the other half of what the operator installs so that everything
else can work, and it has a document of its own: [`design-simplyblockdriver.md`](design-simplyblockdriver.md).
The two kinds sit beside each other in the bootstrap layer rather than one inside
the other, and the only thing genuinely coupling them is the version skew that
document's §5 owns.

---

## 1. Background

The registered `ControlPlane` is not what the target model draws. It is an
observation with a spec field attached.

**The chart installs the control plane, and the operator watches it.** Five chart
templates bring up a `FoundationDBCluster`, a `MongoDBCommunity`, the management
API's `Deployment` and `StatefulSet`, their Services, Secrets, ServiceAccounts,
and serving certificates. A sixth writes a `ControlPlane` object naming an image.
The controller then polls `GET /api/v2/_meta/ready` every thirty seconds and
writes `Initializing` or `Ready`.

[`design-crd-model.md`](design-crd-model.md) §6 draws that edge as
`installs/reuses`, which is two capabilities the kind has neither of. It cannot
install, because the chart already did. It cannot reuse, because nothing in the
object says where the control plane is.

**The endpoint is an environment variable.** `webapi.NewClient` defaults to
`http://simplyblock-webappapi:5000` and is overridden by
`SIMPLYBLOCK_WEBAPI_BASE_URL`. A control plane running outside the Kubernetes
cluster, which is the case the reuse half of §6 exists for, is configured by
editing a Deployment rather than by editing the resource that represents it. The
API server therefore holds an object called `ControlPlane` that cannot tell anyone
which control plane it means.

**`spec.image` belongs to the storage nodes.** It is the image every
`StorageNodeSet` inherits when it omits `spec.clusterImage`, which is a
fleet-defaults question that has nothing to do with whether FoundationDB is
healthy. It is on this kind because this kind is the chart's singleton and the
value needed a home.

---

## 2. Goals and Non-Goals

### Goals

- Specify a `ControlPlane` that says which control plane it means, so that
  reusing an external one is a field rather than an environment variable (§3, §5).
- Specify the two modes as siblings under one block, so that choosing between
  them is expressible and setting both is rejected (§5).
- Specify what the operator installs when it installs, and what it does not touch
  when it reuses (§5).
- Specify `ControlPlaneOps` and the operations a control plane has that are not
  expressible as desired state (§6).
- Move `spec.image` to where the fleet defaults live, and say what that costs
  (§11).

### Non-Goals

- **Not FoundationDB's own operation.** Backup, restore, coordinator changes, and
  recovery are the FoundationDB operator's mechanisms and its documentation's
  subject. What this document specifies is the behavior at the boundary: what the
  operator installs, what it waits for, what it reports, and where it asks. §6's
  `Backup` is such an ask, creating a `FoundationDBBackup` and waiting on it.
  Restore stays outside entirely, for the reason §6 gives.
- **Not the management API's internals.** The control plane is a dependency, not
  a subject.
- **Not the CSI driver.** Its deployment, its phases, and the version skew
  against this kind are [`design-simplyblockdriver.md`](design-simplyblockdriver.md). This
  document publishes `status.version` and stops there.
- **Not multi-control-plane.** One `ControlPlane` per namespace, named
  `simplyblock`, which is what ships and what §3.1 keeps. Whether that limit
  should be lifted is §12 Q1, and lifting it changes kinds this document does not
  own.
- **Not the API group's conventions.** The entity and action split, the enum
  casing, the lock, and the observed generation belong to
  [`design-crd-model.md`](design-crd-model.md) and are cited rather than restated.

---

## 3. ControlPlane: API

Declared in `operator/api/v1alpha1/controlplane_types.go`, short name `cp`. The
type is Appendix A. What follows quotes the field an argument turns on and no
more.

### 3.1 The singleton

`ControlPlane` is one object per namespace, named `simplyblock`. The controller
ignores any other name, which is enforcement by convention rather than by the API
server. §12 Q1 is whether one per namespace is the right limit at all, which is
the question that enforcement is downstream of.

A namespace is the unit because a namespace is one simplyblock deployment: its own
control plane, its own clusters, its own CSI credentials. Two namespaces are two
deployments that share a Kubernetes cluster and nothing else.

### 3.2 Spec

The spec is one block with one member per mode, which is the shape
[`design-storagecluster.md`](design-storagecluster.md) §3.1 uses for key
management and for the same reason: two modes as siblings make the choice
expressible, where two top-level fields make it a convention.

```go
// Source selects where the control plane comes from. Exactly one member is set.
// +kubebuilder:validation:XValidation:rule="(has(self.managed) ? 1 : 0) + (has(self.external) ? 1 : 0) == 1",message="set exactly one of managed or external"
// +kubebuilder:validation:Required
// +k8s:immutable
Source ControlPlaneSource `json:"source"`
```

**The block is immutable, not its members.** Switching a live deployment from a
control plane the operator installed to one it did not is not a reconfiguration,
it is a different deployment: the clusters, their UUIDs, and their volumes live in
the FoundationDB behind the old one. Making the block immutable says that in the
schema rather than in a runbook.

`spec.managed` is what the operator installs (§5.1). `spec.external` is where an
existing control plane already is (§5.2).

### 3.3 Status

`status.phase` is `Installing`, `Available`, `Degraded`, or `Unavailable`, and
`status.step` is the installation machine's position within `Installing` (§4.2).
The three post-installation values are decided by §4.3, and the distinction that
matters is that a `Degraded` control plane answers and an `Unavailable` one does
not.

**`status.endpoint` is the resolved base URL, and it is the field that makes the
object useful to anything but a human.** Every controller in the operator reaches
the control plane, and today every one of them calls `webapi.NewClient()`, which
reads an environment variable. In the target they read the endpoint the
`ControlPlane` publishes, so that one object answers where the control plane is
and a change to it reaches every reader without a Deployment rollout.

`status.components` is the per-component readiness §4.3 derives the phase from:
each component's desired count, its ready count, and whether it is essential. It
is what makes the phase explainable, and it is the only place an administrator
learns which of eleven workloads is the one restarting.

`status.version` is the management API's reported version, which is what a
`ControlPlaneOps` upgrade moves and what [`design-simplyblockdriver.md`](design-simplyblockdriver.md) §5
compares a driver against.
`status.lastChecked` is when the readiness probe last ran, and
`status.activeOpsRef` is the operation lock
([`design-crd-model.md`](design-crd-model.md) §3.2).

`status.message` carries the reason the phase is what it is, and on a failed
readiness probe that is the control plane's own error rather than a
paraphrase of it.

### 3.4 Examples

A control plane the operator installs, which is what a fresh deployment gets:

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: ControlPlane
metadata:
  name: simplyblock
  namespace: simplyblock
spec:
  source:
    managed:
      image: quay.io/simplyblock-io/simplyblock:26.2.8
      foundationDB:
        replicas: 3
status:
  phase: Available
  endpoint: https://simplyblock-webappapi.simplyblock.svc.cluster.local:5000
  version: 26.2.8
  lastChecked: "2026-08-29T09:14:02Z"
  observedGeneration: 1
```

A control plane that already exists, running outside the Kubernetes cluster:

```yaml
spec:
  source:
    external:
      endpoint: https://sb-control.example.com:5000
      credentialsSecretRef:
        name: simplyblock-control-plane
status:
  phase: Available
  endpoint: https://sb-control.example.com:5000
  version: 26.2.8
  observedGeneration: 1
```

`status.endpoint` repeats `spec.source.external.endpoint` in the second case and
is derived in the first. That is the point: a reader and a controller ask
`status.endpoint` either way, and neither has to know which mode the deployment
is in.

An installation part-way through:

```yaml
status:
  phase: Installing
  step:
    state: AwaitingFoundationDB
    deadline: "2026-08-29T09:22:00Z"
  message: FoundationDBCluster simplyblock has 2 of 3 coordinators available
  observedGeneration: 1
```

---

## 4. ControlPlane: Controller

`ControlPlaneReconciler`, in
`operator/internal/controllers/controlplane/controlplane_controller.go`.

### 4.1 Reconcile

```
┌──────────────────────────────────────────────────────────────┐
│                  Kubernetes Control Plane                    │
│   ┌──────────────────────────────────────────────────────┐   │
│   │                ControlPlaneReconciler                │   │
│   │  1. Name is "simplyblock"? Otherwise ignore          │   │
│   │  2. spec.source.external → resolve and probe (§5.2)  │   │
│   │  3. spec.source.managed  → the install machine (§5.1)│   │
│   │  4. Available: probe, publish endpoint and version   │   │
│   └──────────────────────────────────────────────────────┘   │
│  ControlPlane CR   spec.source   status.phase   status.step  │
└──────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼────────────────────────────────────────────────┐
│                  Simplyblock Control Plane                   │
│  GET /api/v2/_meta/ready                                     │
│  GET /api/v2/_meta/version                                   │
└──────────────────────────────────────────────────────────────┘
```

### 4.2 The installation machine

Installing a control plane is several objects that have to arrive in order and a
wait on something outside the operator's control, which is the shape a step
machine exists for ([`design-crd-model.md`](design-crd-model.md) §3.1). An entity
declares one graph, because it has no `spec.action` to key a `MultiConfig` on.

```
  spec.source.managed
    │
    ▼
  ApplyingFoundationDB  ← the FoundationDBCluster and its RBAC
    │
    ▼
  AwaitingFoundationDB  ← wait for the cluster to report available
    │
    ▼
  ApplyingDatastore     ← the document store the management API needs
    │
    ▼
  ApplyingAPI           ← the management API workload, Services, certificates
    │
    ▼
  AwaitingAPI           ← GET /_meta/ready succeeds
    │
    ▼
  phase: Available
```

**Every step is an apply, so re-entering one is a no-op.** The objects are
server-side applied with the `ControlPlane` as their owner, which means a step
recorded whose apply never landed re-applies to the same result. That is what
lets the machine carry no `triggered` flag, exactly as an `Ops` kind does
([`design-crd-model.md`](design-crd-model.md) §3.1).

**`AwaitingFoundationDB` is the step that can take the longest and the one whose
deadline matters.** A three-coordinator FoundationDB on slow storage is minutes,
and a FoundationDB that never reaches quorum is indistinguishable from one still
starting without a bound. The step reports what it is waiting for in
`status.message`, read from the `FoundationDBCluster`'s own status, so a stalled
install names the coordinator rather than the operator.

### 4.3 Steady state

With `phase: Available`, the controller re-applies what §5.1 installs, probes
readiness, and republishes the endpoint and version. Re-applying is what keeps an
object somebody deleted or edited from staying that way, and it is why no operation
exists for checking the install (§6). The probe is a direct `GET` rather than a streamed value, because it
is the check that the stream itself can be established
([`design-crd-model.md`](design-crd-model.md) §7.7 makes the stream the way state
arrives, and this is the one read that cannot depend on it).

**Two signals decide the phase, and they answer different questions.** The probe
answers whether the control plane responds. The components answer whether it is
one restart away from not responding, and for a managed control plane those are
the workloads §5.1 installs, which the operator owns and can therefore watch.

**A component's readiness is a count against a count.** For a
`Deployment` or a `StatefulSet` it is ready replicas against desired replicas. For
the two that carry their own operator it is that resource's own report, because a
`FoundationDBCluster` at two of three coordinators is serving and a replica count
cannot say so.

| Component                                                                                                                | Readiness is                         | Essential |
|--------------------------------------------------------------------------------------------------------------------------|--------------------------------------|-----------|
| `simplyblock-webappapi`                                                                                                  | Ready against desired replicas       | Yes       |
| `simplyblock-fdb-cluster`                                                                                                | The `FoundationDBCluster` health     | Yes       |
| `simplyblock-mongo`                                                                                                      | The `MongoDBCommunity` member report | Yes       |
| `simplyblock-fdb-controller-manager`                                                                                     | Ready against desired replicas       | No        |
| `simplyblock-tasks`                                                                                                      | Ready against desired replicas       | No        |
| `simplyblock-minio`, `simplyblock-admin-control`                                                                         | Ready against desired replicas       | §12 Q5    |
| `simplyblock-monitoring`, `simplyblock-graylog`, `simplyblock-thanos`, `simplyblock-grafana`, `simplyblock-fdb-exporter` | Ready against desired replicas       | No        |

**Essential means the work stops, not that it slows down.** A component is
essential when its absence loses work or stops the control plane answering.
`simplyblock-tasks` is the case that draws the line: its work is queued, so a task
runner at zero defers what is waiting rather than dropping it, and the queue is
still there when it comes back. That is a control plane doing less than it should
while remaining correct, which is what `Degraded` is for.

**The phase is the worst verdict among the probe and every component.**

| Signal                                      | Verdict       |
|---------------------------------------------|---------------|
| The probe fails                             | `Unavailable` |
| An essential component at zero ready        | `Unavailable` |
| Any component below desired but above zero  | `Degraded`    |
| A non-essential component at zero ready     | `Degraded`    |
| Everything at desired and the probe passing | `Available`   |

**Only a component the table marks essential can produce `Unavailable`, and that
asymmetry is the safety property.** `Unavailable` holds every controller in the
operator, so the set of things able to cause it has to be a closed list somebody
reviewed. A component added to the install without a decision about it lands in
the default, which is that it can reach `Degraded` and cannot reach
`Unavailable`. The failure that costs is halting a fleet over Grafana, not
reporting a warning about it.

**The table lives in the operator, not in the API.** It sits beside the applies
it describes, because a user cannot add a component to a managed control plane and
so has nothing to configure. A spec field here would exist only to let somebody
mark the management API non-essential, which turns an outage into a warning
without changing the outage.

**The counts are published, because a phase that cannot be explained is a phase
nobody trusts.** `status.components` carries each component's desired count, its
ready count, and whether it is essential (§3.3), so `Degraded` names what is wrong
instead of asserting that something is.

**`Degraded` is a control plane that answers, so nothing holds on it.** A
management API pod restarting behind a Service with more than one replica, and a
FoundationDB pod recycled without losing quorum, are both situations in which
every request still succeeds. The phase exists to keep a survivable event
survivable.

What `Degraded` is for is the administrator, and what it says is that a control
plane answering today has something wrong with it. That is the window in which a
restart loop is a fixable problem rather than a post-mortem.

**`Unavailable` is a control plane that does not answer, and it is what
downstream controllers hold on**, together with `Installing`. Those two are
different from each other in turn: `Installing` has never worked, and
`Unavailable` worked and stopped. An administrator needs to tell them apart, and a
controller does not, because it holds on both.

**The name states what was observed rather than what is wrong.** A failing probe
cannot distinguish a crashed process from a wedged one, from a network partition,
or from a certificate that expired an hour ago, and `Unavailable` is the only
claim all four support. Where this API group uses `Failed` it means something that
does not come back without intervention, an operation that has finished and will
not resume or a device somebody has to walk into a datacenter and replace
([`design-storagedevice.md`](design-storagedevice.md) §4.2). A control plane
recovers on the next probe with nothing having restarted it.

**`Degraded` means here what it means on a `StorageDevice`**, which is something
that is serving and should not be
([`design-storagedevice.md`](design-storagedevice.md) §4.2). One word carrying one
idea across the group is worth more than a word chosen to fit each kind slightly
better.

**An external control plane never reaches `Degraded`.** The operator installs
nothing and owns no components there (§5.2), so `status.components` is empty, the
probe is the only signal, and its phase is `Available` or `Unavailable`. That is a real difference in
what the two sources can report rather than a gap to be filled, since the pods
behind somebody else's endpoint are not the operator's to watch.

**Events are emitted on transition, not on every probe.** A thirty-second probe
that emitted on every failure would produce two thousand events a day from one
outage. This is behavior the registered controller already has and the rework
keeps.

### 4.4 Deletion

The finalizer is `storage.simplyblock.io/controlplane-finalizer`.

**Deleting a `ControlPlane` with `spec.source.managed` deletes a database.** The
finalizer therefore refuses while any `StorageCluster` in the namespace still
exists, emits `ClustersStillPresent`, and requeues. That is a hold rather than a
failure: removing the clusters resolves it, and nothing else can.

With `spec.source.external` the operator installed nothing, so deletion removes
the object and touches neither the endpoint nor its data. The same
`StorageCluster` hold still applies, because a namespace whose clusters have no
control plane to reach is a namespace of objects nothing can reconcile.

---

## 5. Reuse and Install Are One Kind

[`design-crd-model.md`](design-crd-model.md) §6 draws one edge reading
`installs/reuses`, and the reason it is one edge rather than two kinds is that
everything downstream asks the same question of it: where is the control plane,
and is it ready. Which of the two produced the answer is this kind's business and
nobody else's.

### 5.1 Managed

The operator applies what the chart applies today: the `FoundationDBCluster` and
its RBAC, the document store, the management API's workload, its Services, and
its serving certificates. They become children of the `ControlPlane` by
controller reference, so the ownership spine starts at a real edge rather than at
a Helm release.

```go
// ManagedControlPlane is a control plane the operator installs and owns.
type ManagedControlPlane struct {
	// Image is the management API and control-plane image.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +kubebuilder:validation:Required
	Image string `json:"image"`
	// ...
}
```

#### Four components may already be in the cluster

The management API's workload is the operator's to install. Reaching it needs four
things that are not: a FoundationDB operator to reconcile the
`FoundationDBCluster`, a MongoDB operator to reconcile the document store, an
issuer for the serving certificates, and a `StorageClass` for the volumes both
databases claim. Each is a component a cluster may already run, and each is
detected by asking the API server what it serves.

| Component             | Detected by                                                  | Where it is absent                                             |
|-----------------------|--------------------------------------------------------------|----------------------------------------------------------------|
| FoundationDB operator | `apps.foundationdb.org/v1beta2` is served                    | The operator applies the CRDs and the controller               |
| MongoDB operator      | `mongodbcommunity.mongodb.com/v1` is served                  | §12 Q6                                                         |
| Certificate issuer    | `cert-manager.io/v1` is served, or the platform is OpenShift | `Installing` holds, and `status.message` names what is missing |
| `StorageClass`        | A default `StorageClass` exists                              | §12 Q7                                                         |

**A detected component is used and never re-applied.** A cluster running the
FoundationDB operator for something else runs one operator afterward, and the
`FoundationDBCluster` this design creates is reconciled by it.

**An installed component is cluster-scoped and carries no controller reference**,
for the reason
[`design-simplyblockdriver.md`](design-simplyblockdriver.md) §4.1 gives for the
snapshot controller: deleting a CRD deletes every object of that kind in the
cluster, including the ones another deployment created.

**The certificate issuer is detected rather than declared.** The chart takes it as
`tls.provider`, one of `openshift` or `cert-manager`, which is a fact about the
cluster written down by hand. The API answers it: OpenShift is identifiable from
the markers its distribution leaves
([`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §8.1),
and cert-manager from the group it registers.

**The management API runs two instances by default, and the number is
load-bearing.** A second instance is what lets a pod be replaced while the control
plane keeps answering, and three things in this document rest on that: `Degraded`
is defined as a component below its desired count while the probe still passes
(§4.3), `Restart` recycles the workload after draining rather than instead of
serving (§6), and `Upgrade` rolls the same Deployment. With one instance each of
those becomes an outage, correctly reported as `Unavailable` and no less an
outage for being reported well.

**One instance stays expressible, because an edge deployment may prefer it.** What
it costs is stated rather than prevented: the phase never reaches `Degraded` for
the management API, and every operation in §6 that touches the workload interrupts
service. FoundationDB defaults to three for the same class of reason, and a
deployment that wants neither is choosing what it is trading.

**Moving the install out of the chart is the substantive part of this document,
and it is not free.** A chart template is a file a user can read, fork, and patch.
A controller's apply is none of those. What it buys is that the control plane's
version becomes a field the operator reconciles rather than a value baked into a
release, which is the same argument [`design-simplyblockdriver.md`](design-simplyblockdriver.md) §8
makes for the CSI driver and the reason
[`design-crd-model.md`](design-crd-model.md) §6 draws the edge at all. §12 Q2 is
the transition, which cannot be a flag day.

### 5.2 External

An external control plane is an endpoint and a credential.

```go
// ExternalControlPlane is a control plane that already exists. The operator
// installs nothing and owns nothing; it resolves, probes, and reports.
type ExternalControlPlane struct {
	// Endpoint is the management API's base URL. Rejected unless it resolves to
	// an external address, which is the SSRF guard atlas-lib/net carries.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`
	// ...
}
```

**The credential is a Secret reference rather than a field**, because it is one,
and because a token in a spec is a token in every `kubectl get -o yaml`, every
GitOps repository, and every audit log entry that records the object.

**No `ControlPlaneOps` action applies to an external control plane** (§6).
Restarting and upgrading are both operations on a workload, and the operator
installed no workload here. What it does for an external control plane is resolve
the endpoint, probe it, and report what it says.

**An external control plane may be shared, and the operator must assume it is.**
Another Kubernetes cluster, or another namespace, may hold clusters in the same
FoundationDB. Nothing this operator does may assume it is the only writer, which
is a property every controller already needs for a different reason: the control
plane is an independent system whose state changes without the operator's
involvement ([`design-crd-model.md`](design-crd-model.md) §7.7).

---

## 6. ControlPlaneOps

Declared in `operator/api/v1alpha1/controlplaneops_types.go`, short name `cpops`,
and reconciled by `ControlPlaneOpsReconciler` in
`operator/internal/controllers/controlplane/controlplaneops_controller.go`, beside
the entity's own. The type is Appendix B.

Most of what an administrator does to a control plane is expressible as desired
state: changing the image is an edit, scaling FoundationDB is an edit, and an
object somebody edited by hand is put back by the next reconcile (§4.3). What is
left is what this kind carries, and it is three things: recycling a workload,
moving it to a new version, and asking FoundationDB for a backup.

**Every action requires a managed control plane.** Each acts on what the operator
installed: `Restart` recycles a workload, `Upgrade` replaces its image, and
`Backup` asks the `FoundationDBCluster` the operator applied. An external control
plane is an endpoint and a credential (§5.2), owning none of those, so there is
nothing for any of the three to act on.

```go
// +kubebuilder:validation:Enum=Restart;Upgrade;Backup
type ControlPlaneOpsAction string
```

| Action     | Steps                                                            | What it is for                                                    | Source  |
|------------|------------------------------------------------------------------|-------------------------------------------------------------------|---------|
| `Restart`  | `Draining` → `Restarting` → `Awaiting`                           | A workload is wedged and has to be recycled                       | Managed |
| `Upgrade`  | `Preflight` → `Draining` → `Applying` → `Awaiting` → `Verifying` | Moving the control plane to a new version                         | Managed |
| `Backup`   | `Requesting` → `Awaiting`                                        | Asking FoundationDB for a backup outside whatever schedule exists | Managed |
| `Rollback` | `Requesting` → `Awaiting`                                        | <desc>                                                            | Managed |

**`Restart` takes a component scope.** `spec.restart.components` names entries
from §4.3's table, and an empty list recycles the whole control plane. Restarting
one wedged component is the common case, and the table already enumerates what
there is to name, so the action reuses it rather than inventing a second list of
workload names.

**An operation that recycles a workload drains first.** The management API is
what every controller in the operator talks to, so replacing or restarting it
mid-flight fails whatever is in flight. `Draining` holds while any
`StorageClusterOps`, `StorageNodeOps`, or `PersistentVolumeOps` in the namespace
is `Running`, emits `OperationsInFlight`, and proceeds when the last one finishes.
It does not cancel them, because an operation canceled to make a restart
convenient is a worse outcome than a restart that waited.

**`Restart` and `Upgrade` both carry the step, because both roll the same
Deployment.** An upgrade replaces the management API's image, which recycles its
pods exactly as a restart does, and an operation interrupted by that has been
interrupted whichever field caused it. `Backup` carries no `Draining`, because
asking FoundationDB for a snapshot recycles nothing.

**`Upgrade` drains after `Preflight` rather than before it.** An upgrade refused
for naming the image already running is refused in a moment, and making it first
wait out a twenty-minute node add would spend the fleet's time to reach an error
that was available immediately.

**A scoped restart drains only when it names a component something depends on.**
Recycling Grafana interrupts nothing, so a restart of it has nothing to wait for.
`Draining` therefore runs
when the component list is empty or names any component §4.3 marks essential, and
is skipped otherwise. A task-runner restart is the case worth naming: it skips the
drain, because its queue is what makes the interruption a delay rather than a lost
operation.

**An operation naming an external control plane is rejected at creation.**
`ControlPlaneOpsValidator`, in
`operator/internal/webhook/controlplaneops_validator.go`, resolves
`spec.controlPlaneRef` on `create` and denies the request when it names no
`ControlPlane` or names an external one. `ReplicationOpsValidator` carries the
same shape for the same reason: an operation that can only fail belongs in an
error message on the terminal that wrote it.

**Admission is where the check can live because the answer cannot move.**
`spec.controlPlaneRef` is immutable (Appendix B) and `ControlPlane.spec.source` is
immutable (§3.2), so a control plane admitted as managed stays managed for the
life of the operation. What can still happen is the target being deleted, and an
operation whose target has vanished is a missing-target failure every `Ops` kind
in the group handles.

**A rejected request leaves nothing to clean up.** `ControlPlaneOps.spec` is
immutable, so an operation that reaches `Failed` cannot be corrected, only deleted
and rewritten.

**`Upgrade` carries a `Preflight` step and `Restart` carries none.** `Preflight`
reads live state, which admission cannot: it holds until the control plane is
`Available`, and it fails when the requested image is the one already running, because
rolling a Deployment to its current image produces no change to verify. A restart
has neither precondition, since recycling a wedged workload is what it is for.

**`Verifying` is what makes an upgrade more than an image bump.** It re-probes
readiness, compares the reported version against what was asked for, and fails
the operation when they disagree, so a rollout that started and did not finish is
a `Failed` operation rather than an `Available` control plane running the old version.

**`Backup` asks rather than implements.** `Requesting` creates or updates a
`FoundationDBBackup` naming the cluster and the destination from
`spec.backup.blobStore`, and `Awaiting` completes when that object reports a
snapshot. Backup itself is the FoundationDB operator's, and this action is the
one-shot trigger a schedule cannot express: a restore is imminent, an upgrade is
about to start, or somebody wants a copy before touching anything.

**The `FoundationDBBackup` outlives the operation and is not owned by it.**
Deleting the record of a backup having been taken must not delete the backup's
configuration, which is the same rule `spec.controlPlaneRef` follows for its
target. An operation that finds an existing `FoundationDBBackup` for the cluster
triggers a snapshot on it rather than applying a second one beside it.

**`Restore` is not an action, and the operator does not restore at any point.**
What FoundationDB holds is cluster definitions, node registrations, and lvol
metadata, and the data those records describe lives on the storage nodes'
devices. Restoring the records puts back a control plane's belief about volumes
without putting back the volumes, so a restore beside devices that were wiped
produces a control plane confidently describing data that is gone, and nothing the
operator can read tells it which case it is in.

**That check needs the fleet the restore would precede.** Only a storage node
adopting its devices can say whether the records match what is on disk, and no
`StorageNode` exists at the moment a restore would run. An operation that cannot
know whether it is recovering a deployment or fabricating one is not an operation
this kind should offer, so `FoundationDBRestore` stays an object an administrator
applies deliberately, having decided that question themselves.

**There is no action for checking what the operator installed.** Putting a
deleted object back and correcting an edited one is what reconciling means, and
§4.3 does it on every pass.

---

## 7. Mutual Exclusion

`status.activeOpsRef` on the `ControlPlane` names the operation currently allowed
to act on it, which is the lock every entity in this group carries under that name
([`design-crd-model.md`](design-crd-model.md) §3.2). That document states the
mechanism.

**One operation per control plane is the strongest form of the limit in this
group**, because a control-plane operation interrupts every controller rather
than one cluster or one node. A second operation is admitted, sits at `Pending`,
and runs when the lock frees.

The release path this kind adds is the finalizer
`storage.simplyblock.io/controlplaneops-finalizer`.

---

## 8. Backend API Requirements

| Method | Endpoint                | Notes                                                                                            |
|--------|-------------------------|--------------------------------------------------------------------------------------------------|
| `GET`  | `/api/v2/_meta/ready`   | The readiness probe, and the one read that cannot come from the stream because it establishes it |
| `GET`  | `/api/v2/_meta/version` | The reported version. Not provided today, and a prerequisite for §6 and §9.2                     |

**`/_meta/version` does not exist, and three things in this document wait on it.**
`status.version` has nothing to publish (§3.3), the `Upgrade` action's `Verifying`
step degenerates into a readiness probe that cannot tell a completed upgrade from
a rollout that failed back (§6), and the version-skew pair against the CSI driver
has one half ([`design-simplyblockdriver.md`](design-simplyblockdriver.md) §5).

**It is a prerequisite rather than a degradation to design around.** An `Upgrade`
that cannot verify is an operation reporting success on the evidence that
something answered, which is the failure the step exists to catch. The endpoint is
the control plane's to add, and the work that depends on it is sequenced behind
it rather than built to cope without it.

---

## 9. Observability

The registered controller emits two event reasons and exports no metric. The
events table below is a consolidation of the surface that exists, and the metrics
table is new infrastructure.

### 9.1 Kubernetes events

Events land on the object an administrator has open. For this kind that is the
`ControlPlane` itself, which is a singleton, and for an operation it is the
`ControlPlaneOps`, which outlives the operation as its audit record.

| Event                                                                  | Type      | Reason                 | On                |
|------------------------------------------------------------------------|-----------|------------------------|-------------------|
| The readiness probe failed and the phase became `Unavailable`          | `Warning` | `ControlPlaneNotReady` | `ControlPlane`    |
| A component is below its desired count and the phase became `Degraded` | `Warning` | `ControlPlaneDegraded` | `ControlPlane`    |
| The readiness probe recovered                                          | `Normal`  | `ControlPlaneReady`    | `ControlPlane`    |
| An installation step is waiting on FoundationDB                        | `Normal`  | `AwaitingDependency`   | `ControlPlane`    |
| An installation step's deadline expired                                | `Warning` | `StepDeadlineExceeded` | `ControlPlane`    |
| A deletion is held because clusters still exist                        | `Warning` | `ClustersStillPresent` | `ControlPlane`    |
| The external endpoint could not be resolved or reached                 | `Warning` | `EndpointUnreachable`  | `ControlPlane`    |
| The credentials Secret is missing or malformed                         | `Warning` | `CredentialsError`     | `ControlPlane`    |
| A `Backup` run created a `FoundationDBBackup`                          | `Normal`  | `BackupRequested`      | `ControlPlaneOps` |
| A `Backup` run triggered the one already configured                    | `Normal`  | `BackupTriggered`      | `ControlPlaneOps` |
| The operation is waiting for another to release the lock               | `Normal`  | `OperationQueued`      | `ControlPlaneOps` |
| The operation is holding for in-flight operations to finish            | `Normal`  | `OperationsInFlight`   | `ControlPlaneOps` |
| The operation acquired the lock and started                            | `Normal`  | `OperationStarted`     | `ControlPlaneOps` |
| The operation finished successfully                                    | `Normal`  | `OperationSucceeded`   | `ControlPlaneOps` |
| The operation failed                                                   | `Warning` | `OperationFailed`      | `ControlPlaneOps` |
| The operation was aborted and its unwind finished                      | `Normal`  | `OperationAborted`     | `ControlPlaneOps` |
| The reported version disagrees with the requested one                  | `Warning` | `VersionMismatch`      | `ControlPlaneOps` |

**`ControlPlaneNotReady` is the load-bearing one**, because every controller in
the operator holds when it fires and none of them says why. A cluster that will
not create, a node that will not add, and a volume that will not provision are one
event on one object.

**`ControlPlaneDegraded` fires while every request is still being served** (§4.3),
so it reaches an administrator before an outage rather than during one, and
nothing holds on it. It names the component and its two counts, because one reason
covering eleven workloads sends a reader to `status.components` anyway.

The registered controller's `FDBReady` and `FDBNotReady` become
`ControlPlaneReady` and `ControlPlaneNotReady`, because FoundationDB is one of
the things that can be unready and the probe does not distinguish them.

### 9.2 Prometheus metrics

| Metric                                                   | Labels                                | Description                                                                                                                      |
|----------------------------------------------------------|---------------------------------------|----------------------------------------------------------------------------------------------------------------------------------|
| `simplyblock_controlplane_ready_state`                   | `namespace`                           | Gauge, 1 while the probe passes, which a `Degraded` control plane still does. The alert every other alert in the group hangs off |
| `simplyblock_controlplane_component_ready_count`         | `namespace`, `component`, `essential` | Gauge of a component's ready count. The half of the phase the probe cannot see (§4.3)                                            |
| `simplyblock_controlplane_component_desired_count`       | `namespace`, `component`, `essential` | Gauge of its desired count, so the ratio is the alert and neither half means anything alone                                      |
| `simplyblock_controlplane_probe_duration_seconds`        | `namespace`                           | Histogram of readiness-probe latency, which degrades before readiness does                                                       |
| `simplyblock_controlplane_probe_failures_total`          | `namespace`, `reason`                 | Failed probes by transport error, non-2xx, and timeout                                                                           |
| `simplyblock_controlplane_install_step_duration_seconds` | `namespace`, `step`                   | Histogram of per-step installation duration (§4.2)                                                                               |
| `simplyblock_controlplane_operations_total`              | `namespace`, `action`, `result`       | Operations reaching a terminal phase                                                                                             |
| `simplyblock_controlplane_operation_duration_seconds`    | `namespace`, `action`                 | Histogram of operation durations                                                                                                 |
| `simplyblock_controlplane_version_info`                  | `namespace`, `version`                | Gauge, 1 for the reported version, so a skew against the driver is graphable                                                     |

**`simplyblock_controlplane_version_info` is half of a pair.** Its other half is
`simplyblock_simplyblockdriver_version_info`, in
[`design-simplyblockdriver.md`](design-simplyblockdriver.md) §6.2. The pair shows
an ordering: a control plane ahead of the driver is the supported state that every
upgrade passes through, and a driver ahead of the control plane fails in the data
path at attach time, on a workload's pod. Reading the two gauges beside each other
turns that into a dashboard panel.

**The component pair is an alert and a warning, and not the same severity.**
`simplyblock_controlplane_ready_state` going to zero is an outage. A component's ready
count falling below its desired count while that gauge stays at one is the restart
loop that has not become an outage yet, and the `essential` label is what lets one
rule page for the management API and open a ticket for Grafana. Paging on that is
how `Degraded` gets used for what §4.3 built it for.

**`simplyblock_controlplane_ready_state` is the metric to alert on first.** Everything
else in this API group is downstream of it, so an alert storm that starts here
has one cause and one page.

---

## 10. Testing Strategy

Scenarios live in
[`tests/test-plan-controlplane.md`](../../tests/test-plan-controlplane.md) and only
there.

Unit tests with a fake client and a mock backend cover the probe's branches, the
phase transitions, the event-on-transition rule, and the `Source` block's
resolution. The installation machine is a declared graph, so its illegal
transitions are cheap unit tests.

The risk that unit tests do not reach is concentrated in the install path, which
applies a `FoundationDBCluster` and waits on an operator this repository does not
build. Proving that `AwaitingFoundationDB` reports what it is waiting for, and
that a FoundationDB which never reaches quorum expires the step rather than
hanging, needs `envtest` with the FoundationDB CRDs installed and a real cluster
for the timing.

The external mode's risk is different and smaller: it is a URL, a Secret, and a
probe, and all three are unit-testable. What is not is a shared external control
plane with a second writer, which is the scenario §5.2 says the operator must
assume and nothing exercises.

---

## 11. Migration from the Registered API

| Registered                                    | This design                                 | Cost                                                                                                                                                                                                                             |
|-----------------------------------------------|---------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.image`                                  | `spec.source.managed.image` (§5.1)          | Spec regrouping, and the field stops doubling as the storage-node default (see below)                                                                                                                                            |
| No way to name an external control plane      | `spec.source.external` (§5.2)               | Additive, and it is the half of `design-crd-model.md` §6 that does not exist today                                                                                                                                               |
| The endpoint is `SIMPLYBLOCK_WEBAPI_BASE_URL` | `status.endpoint` (§3.3)                    | Behavioral. Every caller of `webapi.NewClient()` moves, which is every controller in the operator                                                                                                                                |
| The chart installs the control plane          | The operator installs it (§5.1)             | The largest piece of work here, and it cannot be a flag day (§12, Q2)                                                                                                                                                            |
| `status.phase` untyped, two values            | `ControlPlanePhase`, four values (§3.3)     | `Degraded` and `Unavailable` are both new, and together they separate a control plane that is impaired from one that is not answering. `Ready` becomes `Available`, which is the opposite of `Unavailable` where `Ready` was not |
| No step field                                 | `status.step` (§4.2)                        | Additive. A stalled install currently reports one message and no position                                                                                                                                                        |
| No `observedGeneration`                       | Present (§3.3)                              | Required by `design-crd-model.md` §7.9                                                                                                                                                                                           |
| No `shortName`                                | `cp`                                        | Additive                                                                                                                                                                                                                         |
| No `ControlPlaneOps`                          | Two actions (§6)                            | New kind                                                                                                                                                                                                                         |
| `FDBReady`, `FDBNotReady`                     | `ControlPlaneReady`, `ControlPlaneNotReady` | Event reasons rename. Anything alerting on the old names moves                                                                                                                                                                   |
| No metric                                     | The nine metrics of §9.2                    | New infrastructure                                                                                                                                                                                                               |

**The `spec.image` move is not only a regrouping.** The field is inherited by
every `StorageNodeSet` that omits `spec.clusterImage`, so moving it under
`spec.source.managed` would leave the external mode with no storage-node default
at all, which is wrong: the storage-node image has nothing to do with where the
control plane runs. It belongs with the other fleet defaults, in
`StorageCluster.spec.storageNodes.image`
([`design-storagenode.md`](design-storagenode.md) §5.1), whose doc comment
already says it defaults from the `ControlPlane` singleton. That default reverses
with this change: the cluster states the storage-node image, and the control plane
states its own.

Every row above is audited by
`.claude/skills/api-design/scripts/check-crds.py --kind ControlPlane` where a
checker covers it.

---

## 12. Open Questions

**Q1: Whether one control plane per namespace is the right limit.** §3.1 makes a
namespace one simplyblock deployment, so serving several tenants means several
namespaces with one control plane in each. Whether that covers what a
per-customer deployment needs, or whether one namespace has to hold two backends,
is not settled, and the answer decides everything else about the singleton: a name
the controller merely ignores is worth tightening into a webhook or a CEL rule
only once the limit is agreed.

The cost of lifting it does not fall on this kind. Nothing else in the group names
a control plane, because there is only ever one to name: every controller reads
`status.endpoint` from the object in its own namespace (§3.3), and
`ControlPlaneOps` is the only kind carrying a `controlPlaneRef` (§6). A second
control plane in one namespace puts that reference on every entity that reaches a
backend, or makes it inherited down the ownership spine
([`design-crd-model.md`](design-crd-model.md) §5), which is an API-wide change
rather than one this document could make.

What would settle it is a deployment that a namespace per tenant cannot express,
and whether one exists is a question for the field rather than for the code.

**Q2: How the install moves from the chart to the operator.** §5.1 has the
operator apply what the chart applies today, and both cannot own the same objects
at once. The candidates are a chart flag that stops rendering the templates once
the operator is capable of applying them, an adoption pass where the operator
takes ownership of objects the chart already created, and leaving the chart in
place for existing deployments while new ones use the operator. Nothing here
settles it, and it is the reason §5.1's work is larger than its specification.

**Q3: Whether backup belongs to the action or to the spec.**
`FoundationDBBackup` describes a continuous backup, carrying a `backupState` and a
`snapshotPeriodSeconds` rather than a one-shot request, so §6's `Backup` action
creates or triggers a long-lived object and finishes. That works for the ad-hoc
case and says nothing about the ordinary one, where a deployment wants backups
running on a schedule and would express it as a field on
`spec.source.managed`. Whether both exist, with the action triggering the
configured backup and the field owning its existence, or the field alone is
enough, is not settled.

**Q4: Whether a `Degraded` control plane should start new long-running work.**
§4.3 has downstream controllers proceed on `Degraded`, because the control plane
answers. Whether the auto-rebalancer should nonetheless decline to begin a
rebalance while a management API pod is restarting, as against letting in-flight
work finish, is not settled here and belongs with whichever design owns the
rebalancer's admission. The reconcile-level answer and the
start-a-long-operation answer are not obliged to agree.

**Q5: Whether `simplyblock-minio` and `simplyblock-admin-control` are
essential.** §4.3 classifies every other component the managed install applies,
and these two are the ones this document cannot classify from the outside. The
test is the one `simplyblock-tasks` established: whether the control plane's work
is lost while the component is at zero, or merely waits. An object store holding
backups and an admin surface both look like the second, and neither is a guess
this document should make on the control plane's behalf. They take the
non-essential default meanwhile, so a wrong answer under-reports rather than
halting a fleet.

**Q6: What supplies the MongoDB operator where a cluster has none.** §5.1 has the
document store applied as a `MongoDBCommunity` object, and the chart ships that
CRD while installing no operator to reconcile it, so a cluster without one accepts
the object and does nothing with it. The candidates are installing the community
operator alongside the FoundationDB one, replacing the document store with
something the management API already carries, and holding the install with a named
prerequisite. The first two are the control plane's to choose between, since what
it stores where is its own.

**Q7: Whether the operator installs a `StorageClass`.** §5.1 detects a default
one. The chart applies a `hostpath.csi.k8s.io` provisioner and a `local-hostpath`
class, which suits a laptop and a single-node test. A FoundationDB whose volumes
are host paths keeps its data on one machine, so the install either refuses
without a class, names the hostpath path as a development mode, or keeps applying
it.

---

## Appendix A: `controlplane_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, and this is the only place any type appears whole.

```go
// ControlPlanePhase is where the operator has got to with this control plane.
// +kubebuilder:validation:Enum=Installing;Available;Degraded;Unavailable
type ControlPlanePhase string

const (
	// ControlPlanePhaseInstalling is a control plane that has not worked yet.
	ControlPlanePhaseInstalling ControlPlanePhase = "Installing"
	// ControlPlanePhaseAvailable is one whose readiness probe passes and whose
	// workload pods are settled.
	ControlPlanePhaseAvailable ControlPlanePhase = "Available"
	// ControlPlanePhaseDegraded is one whose readiness probe passes while a
	// management API or FoundationDB pod is restarting. It answers every
	// request, so nothing holds on it and it exists to be read by a person. A
	// control plane the operator does not manage never reaches it, because the
	// operator owns no pods there to watch.
	ControlPlanePhaseDegraded ControlPlanePhase = "Degraded"
	// ControlPlanePhaseUnavailable is one whose readiness probe fails: it
	// worked and stopped, which is a different situation from one that never
	// started, and it is what downstream controllers hold on. The word claims
	// only what the probe observed, since a failing probe cannot tell a crashed
	// process from a wedged one or from a partition.
	ControlPlanePhaseUnavailable ControlPlanePhase = "Unavailable"
)

// ControlPlaneStep is one step of the installation path. There is one graph
// rather than a MultiConfig, because an entity has no spec.action to key one on.
// +kubebuilder:validation:Enum=ApplyingFoundationDB;AwaitingFoundationDB;ApplyingDatastore;ApplyingAPI;AwaitingAPI
type ControlPlaneStep string

const (
	ControlPlaneStepApplyingFoundationDB ControlPlaneStep = "ApplyingFoundationDB"
	ControlPlaneStepAwaitingFoundationDB ControlPlaneStep = "AwaitingFoundationDB"
	ControlPlaneStepApplyingDatastore    ControlPlaneStep = "ApplyingDatastore"
	ControlPlaneStepApplyingAPI          ControlPlaneStep = "ApplyingAPI"
	ControlPlaneStepAwaitingAPI          ControlPlaneStep = "AwaitingAPI"
)

// FoundationDBSpec is the sizing of the FoundationDB the operator installs.
type FoundationDBSpec struct {
	// Replicas is the number of coordinators. Three is the smallest count that
	// survives one loss, which is why it is the default.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// StorageClassName is the class the coordinators' volumes are provisioned
	// from. It cannot be a class this operator provides, because the control
	// plane has to exist before any simplyblock volume can.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Resources sets requests and limits for the coordinator pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ManagedControlPlane is a control plane the operator installs and owns. Its
// objects carry a controller reference to the ControlPlane, so the ownership
// spine starts at a real edge rather than at a Helm release.
type ManagedControlPlane struct {
	// Image is the management API and control-plane image.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// ImagePullPolicy controls when that image is pulled.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// FoundationDB sizes the FoundationDB the management API stores its state in.
	// +optional
	FoundationDB *FoundationDBSpec `json:"foundationDB,omitempty"`

	// Replicas is the number of management API instances. Two is what the chart
	// ships and what the phases assume: a single instance makes Degraded
	// unreachable for this component and every restart an outage (§5.1).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources sets requests and limits for the management API pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Tolerations are applied to every pod the operator installs for the control
	// plane.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// ExternalControlPlane is a control plane that already exists. The operator
// installs nothing and owns nothing: it resolves, probes, and reports.
type ExternalControlPlane struct {
	// Endpoint is the management API's base URL. It is validated against the
	// same outbound-URL guard every other external endpoint in this group uses,
	// so a loopback or link-local address is rejected.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// CredentialsSecretRef names a Secret in this namespace holding the bearer
	// token the operator authenticates with. It is a reference rather than a
	// field because a token in a spec is a token in every kubectl get -o yaml.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`

	// CABundleSecretRef names a Secret holding the CA certificate the endpoint
	// is verified against. Absent means the system trust store.
	// +optional
	CABundleSecretRef *corev1.LocalObjectReference `json:"caBundleSecretRef,omitempty"`
}

// ControlPlaneSource selects where the control plane comes from. Exactly one
// member is set, which is what makes the two modes siblings rather than two
// unrelated top-level fields.
type ControlPlaneSource struct {
	// Managed is a control plane the operator installs.
	// +optional
	Managed *ManagedControlPlane `json:"managed,omitempty"`

	// External is a control plane that already exists.
	// +optional
	External *ExternalControlPlane `json:"external,omitempty"`
}

// ControlPlaneSpec is the desired state of the simplyblock control plane for one
// namespace.
type ControlPlaneSpec struct {
	// Source selects where the control plane comes from. Immutable: switching a
	// live deployment between an installed control plane and an existing one is
	// not a reconfiguration, because the clusters and their volumes live in the
	// FoundationDB behind the old one.
	// +kubebuilder:validation:XValidation:rule="(has(self.managed) ? 1 : 0) + (has(self.external) ? 1 : 0) == 1",message="set exactly one of managed or external"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Source ControlPlaneSource `json:"source"`
}

// ControlPlaneStatus is the observed state of the control plane.
// ControlPlaneComponentStatus is one workload of a managed control plane and how
// much of it is running. The phase is the worst verdict across these and the
// readiness probe, and only an essential component at zero ready can make it
// Unavailable.
type ControlPlaneComponentStatus struct {
	// Name is the workload's name, as applied.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Desired is how many replicas the component should have. For the two
	// components carrying their own operator it is that resource's own count,
	// because a FoundationDBCluster reports quorum rather than replicas.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Desired int32 `json:"desired"`

	// Ready is how many of them are.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Ready int32 `json:"ready"`

	// Essential states whether this component at zero ready makes the control
	// plane Unavailable rather than Degraded. It is decided by the table in
	// §4.3 rather than by a user, and it is reported here so that a phase can be
	// explained without reading the operator's source.
	// +optional
	Essential bool `json:"essential,omitempty"`
}

type ControlPlaneStatus struct {
	// Phase is the operator's own view of the control plane.
	// +optional
	Phase ControlPlanePhase `json:"phase,omitempty"`

	// Step is the position of the installation machine within Installing.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['ApplyingFoundationDB','AwaitingFoundationDB','ApplyingDatastore','ApplyingAPI','AwaitingAPI']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Endpoint is the resolved management API base URL, derived in the managed
	// case and echoed in the external one. It is what every controller in the
	// operator reads to reach the control plane, so that one object answers
	// where it is.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Version is the version the management API reports.
	// +optional
	Version string `json:"version,omitempty"`

	// LastChecked is when the readiness probe last ran.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Components is the per-component readiness the phase is derived from
	// (§4.3), one entry per workload the managed install applies. It is empty
	// for an external control plane, which has no components the operator owns.
	// Without it a Degraded phase says that something is wrong and not what.
	// +optional
	// +listType=map
	// +listMapKey=name
	Components []ControlPlaneComponentStatus `json:"components,omitempty"`

	// ActiveOpsRef names the ControlPlaneOps currently allowed to act on this
	// control plane. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the control plane moves, and never a log. On a failed probe it is the
	// control plane's own error rather than a paraphrase of it.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cp
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ControlPlane is the simplyblock control plane for one namespace: FoundationDB
// together with the management API, either installed by the operator or already
// existing. It is a singleton named "simplyblock", and it is the root of the
// ownership spine: nothing else in this API group reconciles meaningfully before
// it reports Available.
type ControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneSpec   `json:"spec,omitempty"`
	Status ControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneList contains a list of ControlPlane.
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}
```

---

## Appendix B: `controlplaneops_types.go`

```go
// ControlPlaneOpsAction is the operation a ControlPlaneOps performs. Every action
// acts on what the operator installed, so every action requires a managed control
// plane, and the validating webhook of §6 rejects an operation naming an external
// one at creation rather than letting it be created and fail.
// +kubebuilder:validation:Enum=Restart;Upgrade;Backup
type ControlPlaneOpsAction string

const (
	ControlPlaneOpsActionRestart ControlPlaneOpsAction = "Restart"
	ControlPlaneOpsActionUpgrade ControlPlaneOpsAction = "Upgrade"
	ControlPlaneOpsActionBackup  ControlPlaneOpsAction = "Backup"
)

// ControlPlaneOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type ControlPlaneOpsPhase string

const (
	ControlPlaneOpsPhasePending   ControlPlaneOpsPhase = "Pending"
	ControlPlaneOpsPhaseRunning   ControlPlaneOpsPhase = "Running"
	ControlPlaneOpsPhaseSucceeded ControlPlaneOpsPhase = "Succeeded"
	ControlPlaneOpsPhaseFailed    ControlPlaneOpsPhase = "Failed"
	ControlPlaneOpsPhaseAborted   ControlPlaneOpsPhase = "Aborted"
)

// ControlPlaneOpsStep is one step of a running control-plane operation. Which
// steps belong to which action is declared by that action's graph rather than by
// this type, which is why the enum stays flat as actions are added.
// +kubebuilder:validation:Enum=Draining;Restarting;Awaiting;Preflight;Applying;Verifying;Requesting
type ControlPlaneOpsStep string

const (
	// Restart and Upgrade both recycle the management API, so both drain.
	ControlPlaneOpsStepDraining ControlPlaneOpsStep = "Draining"

	// Restart.
	ControlPlaneOpsStepRestarting ControlPlaneOpsStep = "Restarting"
	ControlPlaneOpsStepAwaiting   ControlPlaneOpsStep = "Awaiting"

	// Upgrade.
	ControlPlaneOpsStepPreflight ControlPlaneOpsStep = "Preflight"
	ControlPlaneOpsStepApplying  ControlPlaneOpsStep = "Applying"
	ControlPlaneOpsStepVerifying ControlPlaneOpsStep = "Verifying"

	// Backup. Awaiting is shared with Restart.
	ControlPlaneOpsStepRequesting ControlPlaneOpsStep = "Requesting"
)

// UpgradeSpec parameterizes the Upgrade action and is ignored by the others.
type UpgradeSpec struct {
	// Image is the version to move to. It replaces
	// ControlPlane.spec.source.managed.image when the operation succeeds, so the
	// entity keeps describing what is running.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +kubebuilder:validation:Required
	Image string `json:"image"`
}

// RestartSpec parameterizes the Restart action and is ignored by the others.
type RestartSpec struct {
	// Components names the workloads to recycle, from the table in §4.3. Empty
	// recycles the whole control plane. Naming only components that table marks
	// non-essential skips the drain, because recycling them interrupts nothing.
	// +listType=set
	// +optional
	Components []string `json:"components,omitempty"`
}

// BackupSpec parameterizes the Backup action and is ignored by the others.
type BackupSpec struct {
	// BlobStore is the destination, in the form the FoundationDBBackup CRD takes
	// it. The operator copies it through rather than interpreting it, since the
	// backup is the FoundationDB operator's to perform.
	// +kubebuilder:validation:Required
	BlobStore string `json:"blobStore"`

	// BackupName is the FoundationDBBackup to create or trigger. Absent uses the
	// one already configured for the cluster, and fails when there is none and
	// no name to create.
	// +optional
	BackupName string `json:"backupName,omitempty"`
}

// ControlPlaneOpsSpec is one operation to perform against the control plane.
type ControlPlaneOpsSpec struct {
	// ControlPlaneRef names the ControlPlane this operation acts on. The
	// operation never owns its target, because deleting the record of an
	// operation must not delete the control plane it operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ControlPlaneRef string `json:"controlPlaneRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action ControlPlaneOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	// Whether an abort is expressible from the current step is declared by that
	// action's graph rather than checked here.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Upgrade parameterizes action Upgrade and is ignored by the others.
	// +optional
	Upgrade *UpgradeSpec `json:"upgrade,omitempty"`

	// Restart parameterizes action Restart and is ignored by the others.
	// +optional
	Restart *RestartSpec `json:"restart,omitempty"`

	// Backup parameterizes action Backup and is ignored by the others.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`
}

// ControlPlaneOpsStatus is the observed state of one control-plane operation.
type ControlPlaneOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase ControlPlaneOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Draining','Restarting','Awaiting','Preflight','Applying','Verifying','Requesting']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// BackupRef names the FoundationDBBackup a Backup run created or triggered.
	// The operation does not own it, because deleting the record of a backup must
	// not delete the backup's configuration.
	// +optional
	BackupRef string `json:"backupRef,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation acquired its target's lock.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cpops
// +kubebuilder:printcolumn:name="ControlPlane",type=string,JSONPath=".spec.controlPlaneRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ControlPlaneOps is a single operation performed against the control plane. It
// runs to a terminal phase and stays afterward as the audit record of what was
// done, with which parameters, and how it ended.
type ControlPlaneOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneOpsSpec   `json:"spec,omitempty"`
	Status ControlPlaneOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneOpsList contains a list of ControlPlaneOps.
type ControlPlaneOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlaneOps `json:"items"`
}
```

