# Design Document: The SimplyblockDriver

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-31  
**Target Release:** simplyblock 26.4  
**Test Plan:** [`tests/test-plan-simplyblockdriver.md`](../../tests/test-plan-simplyblockdriver.md)  
**Example:** [`assets/releases.yaml`](assets/releases.yaml)

The kind does not exist. The chart installs the CSI driver today, so §8 is a list
of what this replaces rather than a migration.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [SimplyblockDriver: API](#3-simplyblockdriver-api)
4. [SimplyblockDriver: Controller](#4-simplyblockdriver-controller)
5. [Version Skew](#5-version-skew)
6. [Observability](#6-observability)
7. [Testing Strategy](#7-testing-strategy)
8. [What This Replaces](#8-what-this-replaces)
9. [Open Questions](#9-open-questions)

Appendices:

- [Appendix A: `simplyblockdriver_types.go`](#appendix-a-simplyblockdriver_typesgo)

---

## Overview

`SimplyblockDriver` is the deployment of simplyblock's CSI driver expressed as a
Kubernetes resource: the node plugin, the controller plugin, their RBAC, and the
core `CSIDriver` registration they produce.

It is a bootstrap-layer kind the operator installs and versions
([`design-crd-model.md`](design-crd-model.md) §8.1), beside the `ControlPlane`
rather than under it.

**The driver is a client of the control plane, and takes two things from it.**
The endpoint and credentials it provisions volumes through reach it in the
configuration the operator applies (§4.1), resolved from the one control plane in
the namespace ([`design-controlplane.md`](design-controlplane.md) §3.3). The
second is a version it must not run ahead of, which is what §5 is for.

---

## 1. Background

**The chart installs the driver, so the driver's version is a property of a Helm
release.** `csi-driver/charts/spdk-csi` and the operator chart between them apply
a `CSIDriver` registration, a node `DaemonSet`, a controller `StatefulSet`, their
RBAC, and a node configuration `ConfigMap`. Upgrading the driver means upgrading a
chart, and upgrading the control plane means something else entirely, so the two
versions move on two cadences that nothing compares.

**A driver release states which control planes it works against.** The management
API is backward compatible across a window the release declares rather than one a
version number implies, and §5 is where that declaration is read. Every upgrade
leaves the control plane ahead of the driver for as long as the two steps are
apart, and a deployment whose control plane belongs to somebody else stays that
way.

**A driver newer than its control plane may call API that is not there.** The
failure appears in the data path, at attach time, on a workload's pod, which is
the moment furthest from the change that caused it and the person who made it.

Making the deployment a resource the operator reconciles puts both versions in one
place, and §5 is what it does with them.

---

## 2. Goals and Non-Goals

### Goals

- Specify `SimplyblockDriver`, so that CSI driver version and control-plane
  version stop drifting between two release cadences (§3, §5).
- Specify what the operator applies, and what it leaves to the cluster (§4.1).
- Specify the phases, and why a node plugin down on one worker is not the same
  event as a controller plugin that is not running (§4.2).
- Specify which version ordering the deployment requires, and what the operator
  does when it does not hold (§5).

### Non-Goals

- **Not the CSI driver's implementation.** This document specifies the deployment
  of the driver, meaning which objects exist and what versions them. What the
  driver does at `NodeStageVolume` is `csi-driver/` and its own designs.
- **Not the control plane.** Whether one exists, what it installs, and what it
  reports is [`design-controlplane.md`](design-controlplane.md). This document
  reads `ControlPlane.status.version` and nothing else from it.
- **Not the snapshot controller's behavior.** `spec.enableVolumeSnapshots`
  decides whether one is installed. What it then does is the upstream project's.
- **Not the API group's conventions.** The entity and action split, the enum
  casing, and the observed generation belong to
  [`design-crd-model.md`](design-crd-model.md) and are cited rather than
  restated.

---

## 3. SimplyblockDriver: API

Declared in `operator/api/v1alpha1/simplyblockdriver_types.go`, short name `sbd`.
The type is Appendix A. What follows quotes the field an argument turns on and no
more.

### 3.1 Spec

The spec is an image, a name, and the placement the two plugins need.

```go
// Image is the CSI driver image, used by both plugins.
// +kubebuilder:validation:Required
Image string `json:"image"`

// DriverName is the CSI driver name a StorageClass provisions with.
// +kubebuilder:default=csi.simplyblock.io
// +optional
// +k8s:immutable
DriverName string `json:"driverName,omitempty"`
```

**One image versions both plugins**, because a node plugin and a controller
plugin from two builds is the skew of §5 inside one deployment, and there is no
rollout in which it is wanted.

**`nodeSelector` empty means every schedulable worker**, which is the usual case:
a node that cannot attach a volume cannot run a workload that needs one.
Restricting it is for a cluster where some workers are deliberately not storage
clients, and the tolerations beside it exist because the node plugin usually has
to run where workloads run rather than where the operator does.

### 3.2 Immutability

**`driverName` is immutable, and it is the field most likely to be edited by
somebody who does not know that.** Every `PersistentVolume` the driver
provisioned records it in `spec.csi.driver`, and every `VolumeAttachment` records
it too. Changing it does not rename anything: it orphans every volume in the
namespace, which then has no driver willing to claim it. `+k8s:immutable` is what
turns that into a rejection at admission rather than an incident.

Whether the field should be settable at all is §9 Q3.

Nothing else in the spec is immutable. An image, a replica count, a selector, and
a resource block are all things a rollout is allowed to change, which is the
difference between this kind and a `StorageNode`
([`design-storagenode.md`](design-storagenode.md) §3.2): a driver deployment
makes no claim about a layout that is already on disk.

### 3.3 Status

`status.phase` is `Installing` while the objects of §4.1 are being applied, and
afterward one of three values the two plugins decide (§4.2): `Ready` with every
plugin serving and the registration in place, `Degraded` with the controller
plugin provisioning while a node plugin is not ready, and `Unavailable` with the
controller plugin not running.

`status.nodesReady` and `status.nodesTotal` are how many workers run a ready node
plugin and how many are expected to. Neither takes `omitempty`, because zero ready
plugins is the condition worth seeing and a field that disappears at zero is a
field that hides it.

`status.controllerReady` is whether the controller plugin is serving, which is the
single fact that decides whether provisioning happens at all.

`status.snapshotSupport` is `Detected` when the cluster already served
`snapshot.storage.k8s.io/v1` and `Installed` when this operator applied the CRDs
and a controller (§4.1). It is the field that says whether other drivers in the
cluster depend on what this one installed.

`status.version` is the version the deployed driver reports, published so that a
skew against `ControlPlane.status.version` is visible on one screen (§5).

`status.observedGeneration` and `status.message` follow the group conventions
([`design-crd-model.md`](design-crd-model.md) §3.1, §7.9).

**The name is `SimplyblockDriver` rather than `CSIDriver`**, because `CSIDriver`
is a kind in core `storage.k8s.io/v1` and two kinds of one name in two groups is
an ambiguity every reader resolves by group. The two are not the same object
either: the core kind is the cluster's registration record, and this one is the
deployment that produces that record among the rest of what it installs
([`design-crd-model.md`](design-crd-model.md) §7.2).

---

## 4. SimplyblockDriver: Controller

`SimplyblockDriverReconciler`, in
`operator/internal/controllers/driver/simplyblockdriver_controller.go`.

### 4.1 What it applies

The node `DaemonSet`, the controller `StatefulSet`, the RBAC both need, the node
configuration `ConfigMap`, and the core `CSIDriver` registration. Every one of
them becomes a child of the `SimplyblockDriver` by controller reference, so that
deleting the object removes the deployment and the ownership spine starts at a
real edge rather than at a Helm release
([`design-crd-model.md`](design-crd-model.md) §5).

**The two plugins carry the CSI sidecars.** The controller `StatefulSet` runs
`csi-provisioner`, `csi-attacher`, `csi-resizer`, `csi-snapshotter`, and
`csi-external-health-monitor-controller` beside the driver. The node `DaemonSet`
runs `node-driver-registrar` and `livenessprobe`. Each is addressed to this
driver's own socket and acts on the objects naming `spec.driverName`, so a cluster
running a second CSI driver runs a second set of its own.

**Snapshot support is one component for the whole cluster, and the operator
supplies it where the cluster has none.** A `snapshot-controller` turns a
`VolumeSnapshot` into a `VolumeSnapshotContent` for whichever driver its class
names, so one of them serves every driver. Its presence is read from the API
serving `snapshot.storage.k8s.io/v1`, since that controller exists to reconcile
those kinds and its Deployment is named differently by every distribution. Where
the API is served the operator adds nothing. Where it is absent the operator
applies the CRDs and a controller, which is what the chart does for the CRDs alone
today (§8).

**The `VolumeSnapshotClass` for this driver is applied either way.** It names
`spec.driverName` and belongs to this deployment, unlike the CRDs and the
controller, which belong to the cluster.

**What the operator installs here it does not own.** The CRDs and the controller
are cluster-scoped and shared, and a second CSI driver installed afterward
reconciles its snapshots through the same controller. They are applied without a
controller reference and survive the `SimplyblockDriver`, since deleting a
`VolumeSnapshot` CRD deletes every `VolumeSnapshot` in the cluster. §9 Q2 is what
removes them.

**`status.snapshotSupport` records which of the two happened**, so an
administrator reading the object learns whether this deployment brought snapshot
support to the cluster or found it.

**The node configuration `ConfigMap` is where the control plane reaches the
driver.** The operator resolves the namespace's `ControlPlane` and writes its
endpoint and the credentials for it into the configuration both plugins mount, so
the driver is told where the backend is rather than being configured with it
separately. A driver whose `ControlPlane` is not `Ready` is applied and waits,
because a plugin that cannot reach a backend is the same situation as a plugin
that has not been scheduled yet.

**It has no `Ops` companion.** A driver is applied rather than operated: its
version is a field, its rollout is the DaemonSet's and the StatefulSet's, and
there is no imperative verb it has that desired state cannot express
([`design-crd-model.md`](design-crd-model.md) §3).

### 4.2 The phases

The phase is derived from what the deployment's two plugins report, and the two
of them fail differently.

| Signal                                           | Verdict       |
|--------------------------------------------------|---------------|
| The controller plugin is not running             | `Unavailable` |
| One or more node plugins are not ready           | `Degraded`    |
| Every plugin ready and the registration in place | `Ready`       |

**A node plugin down on one worker strands that worker's volumes and leaves every
other worker untouched.** That is a partial failure, and `Degraded` is the phase
for it.

**A controller plugin that is not running is where provisioning stops**, because
it is the plugin that creates and deletes volumes. Existing attachments survive
it, which is why the word is `Unavailable` rather than a claim about data.

**`nodesTotal` of zero is reported rather than failed.** A `nodeSelector` that
matches no worker is a configuration a person wrote, and the phase that suits it
is one that says so in `status.message` rather than one that pretends the
deployment is broken.

---

## 5. Version Skew

**The kind exists so that driver version and control-plane version stop being
independently settable.** §1 is what that costs today, and this is the mechanism.
`status.version` on this object and `status.version` on the `ControlPlane` are the
two halves, and the operator publishes both and compares them.

### 5.1 Compatibility is declared, not computed

**`https://install.simplyblock.io/releases.yaml` states which control planes each
driver release works against.** It lists the components, and under each its
releases newest first, and a client release carries the versions it is compatible
with. [`assets/releases.yaml`](assets/releases.yaml) is the example.

```yaml
components:
  - name: csi-driver
    releases:
      - version: "26.2.1"
        released: "2026-07-02"
        compatible:
          controlplane:
            - "26.2.x"
            - "26.1.x"
```

**The lookup is two steps.** The driver's version is found under `csi-driver`, and
the control plane's version is tested against that release's
`compatible.controlplane` patterns, where `26.2.x` matches any patch of 26.2 and a
full version matches itself. A control plane the patterns admit is supported, and
the driver has nothing to report.

**`compatible` is keyed by component**, so a release that has to agree with the
storage backend as well as the management API says both. A flat list leaves which
component each version belongs to for a reader to infer, and the components ship
on their own cadences precisely so that they can differ.

**The order of a component's releases is authoritative.** It decides whether a
control plane the patterns reject is older or newer than what the driver declares,
which is what separates the two events of §5.2. `released` is informational, and
the earliest entries carry none.

**`schema` names the document's shape**, so a reader built against one version
recognizes another rather than mis-parsing it.

**Declaring it rather than deriving it is what makes the document worth
fetching.** Which control planes a driver works against is decided when that
driver is released and known to nobody else, and no rule over version numbers
recovers it: the components ship on their own cadences, a version is a year and a
release within that year, and the history runs back through a `0.x` series that
followed neither. An operator built before a release shipped reads that release's
declaration correctly, because the document outlives the binary.

**Control-plane releases carry no `compatible` list**, because the promise runs
one way. A client states which backends it works against, and a backend released
afterward cannot state anything about clients that did not exist.

**The address is operator configuration rather than a field on this kind**, since
which releases exist is a fact about the product and not about one deployment. It
is fetched on a timer and cached, and an air-gapped cluster points it at a mirror.

**An unreadable document reports nothing.** Where it cannot be fetched, where the
driver's version is absent from it, or where a release carries no `compatible`
list, the operator publishes both versions and emits neither event. A comparison
needs the declaration, and the alternative to having it is silence rather than a
guess.

### 5.2 What each combination produces

**A control plane the driver's release declares compatible produces nothing.**
That covers the equal case and the ordinary upgrade, which moves the control plane
first and leaves it ahead for as long as the two steps are apart. Where the control
plane is external it may be moved by somebody who does not operate this Kubernetes
cluster ([`design-controlplane.md`](design-controlplane.md) §5.2), which makes
being ahead a standing condition there.

**A control plane older than everything the driver declares produces
`VersionSkew`.** It is reached by upgrading this Kubernetes cluster's operator and
driver before the backend they call, and nothing else notices it until a volume
fails to attach.

**A control plane newer than everything the driver declares produces
`VersionTooOld`.** It is reached by upgrading the control plane without moving the
driver, past the point the driver's release was built to reach.

**The two are separate events because their remedies differ.** One is answered by
moving the control plane forward or the driver back, and the other by moving the
driver forward.

**The operator reports and changes nothing, whichever it finds.** A driver rollout
replaces every node plugin in the cluster, which is a change an administrator
makes deliberately. That is also what makes reading a document off the network
safe enough to do at all: a document that is stale, mirrored, or wrong moves a
warning rather than a deployment.

### 5.3 The half that does not exist yet

**There is no `GET /_meta/version` on the management API**, so
`ControlPlane.status.version` has nothing to publish
([`design-controlplane.md`](design-controlplane.md) §8). Until that endpoint
lands, the driver's version is knowable and the control plane's is not, and half a
comparison reports no skew where it cannot tell. §5 and §6.2 wait on the endpoint.

---

## 6. Observability

The kind is new, so both tables are new infrastructure.

### 6.1 Kubernetes events

| Event                                                        | Type      | Reason              | On                  |
|--------------------------------------------------------------|-----------|---------------------|---------------------|
| The deployment reached `Ready`                               | `Normal`  | `DriverReady`       | `SimplyblockDriver` |
| A node plugin is not ready and the phase became `Degraded`   | `Warning` | `DriverDegraded`    | `SimplyblockDriver` |
| The controller plugin is not running                         | `Warning` | `DriverUnavailable` | `SimplyblockDriver` |
| The driver is newer than the control plane it calls          | `Warning` | `VersionSkew`       | `SimplyblockDriver` |
| The driver is more than one release behind the control plane | `Warning` | `VersionTooOld`     | `SimplyblockDriver` |
| A `nodeSelector` matches no schedulable worker               | `Normal`  | `NoMatchingWorkers` | `SimplyblockDriver` |
| The snapshot controller was applied                          | `Normal`  | `SnapshotsEnabled`  | `SimplyblockDriver` |

**`VersionSkew` and `VersionTooOld` are the two the kind was built for.** Every
other row here reports a deployment's health, and these two report combinations
that are otherwise discoverable only by attaching a volume and watching it fail. A
control plane exactly one release ahead of the driver produces neither, which is
the supported state of §5.

### 6.2 Prometheus metrics

| Metric                                                 | Labels                 | Description                                                                                   |
|--------------------------------------------------------|------------------------|-----------------------------------------------------------------------------------------------|
| `simplyblock_simplyblockdriver_version_info`           | `namespace`, `version` | Gauge, 1 for the reported version. Beside the control plane's, the pair is the skew alert     |
| `simplyblock_simplyblockdriver_nodes_ready_count`      | `namespace`            | Gauge of workers running a ready node plugin                                                  |
| `simplyblock_simplyblockdriver_nodes_expected_count`   | `namespace`            | Gauge of workers expected to, so the ratio is the alert and neither half means anything alone |
| `simplyblock_simplyblockdriver_controller_ready_state` | `namespace`            | Gauge, 1 while the controller plugin serves. Provisioning stops when it is zero               |

**The version pair is worth more than either half.** A driver and a control plane
that disagree fail in the data path at attach time, on a workload's pod, and the
two gauges beside each other are what turns that into a dashboard panel rather
than an incident. The control plane's half is
[`design-controlplane.md`](design-controlplane.md) §9.2.

---

## 7. Testing Strategy

Scenarios live in
[`tests/test-plan-simplyblockdriver.md`](../../tests/test-plan-simplyblockdriver.md)
and only there.

What the controller applies is a pure function of the spec, so most of it is
unit-testable against a fake client: that every object carries a controller
reference, that the registration is created with `spec.driverName`, and that the
snapshot controller appears only when asked for. The phase derivation is the same
shape and belongs beside it.

`driverName` immutability is one marker and belongs in `envtest`, because the
rejection is the API server's.

The risk unit tests do not reach is the skew itself, which needs a driver and a
control plane at two versions and a volume to attach, and it is where §1 says the
failure actually lands.

---

## 8. What This Replaces

Nothing is migrated, because the kind does not exist. What changes is where the
driver's deployment comes from.

| Today                                              | After                                                       |
|----------------------------------------------------|-------------------------------------------------------------|
| The chart renders the node `DaemonSet`             | The controller applies it (§4.1)                            |
| The chart renders the controller `StatefulSet`     | The controller applies it (§4.1)                            |
| The chart renders the `CSIDriver` registration     | The controller applies it, owned by the `SimplyblockDriver` |
| The driver's version is a Helm release's           | `spec.image`, compared against the control plane's (§5)     |
| The snapshot controller is a chart value           | `spec.enableVolumeSnapshots` (§3.1)                         |
| Nothing compares driver and control-plane versions | `VersionSkew` and the two gauges (§6)                       |

**Moving the install out of the chart is not free**, and it is the same cost
[`design-controlplane.md`](design-controlplane.md) §5.1 names for the control
plane: a chart template is a file a user can read, fork, and patch, and a
controller's apply is none of those. It cannot be a flag day either, for the same
reason and with the same unanswered question, which
[`design-controlplane.md`](design-controlplane.md) §12 records as its Q2.

---

## 9. Open Questions

**Q1: Whether the driver should be a singleton like the `ControlPlane`.**
[`design-controlplane.md`](design-controlplane.md) §3.1 makes a control plane one
object per namespace named `simplyblock`. This document does not impose the same
rule, because two drivers with two `driverName` values in one namespace is
expressible and might even be wanted during a migration between driver names.
Whether that is a capability or an accident is not settled.

**Q2: What removes an installed snapshot controller.** §4.1 has the operator
install the CRDs and a controller where the cluster has none, without a controller
reference, so nothing removes them when the `SimplyblockDriver` is deleted. A
cluster left with snapshot CRDs and a controller has working snapshot support and
no simplyblock, which is harmless and untidy. Removing them needs a count of what
else in the cluster relies on them, and `status.snapshotSupport` records only what
this object did. Leaving them is what §4.1 specifies.

**Q3: Whether `driverName` should be settable.** §3.1 carries it because the
chart carries it, as the value `driverName` in both `values.yaml` files. In the
chart the value reaches one object: the `CSIDriver` registration's name. The name
is written literally everywhere else it appears, in the node plugin's
`--kubelet-registration-path`, in the hostPath the plugin mounts, and in the
snapshot class's `driver` field, so a deployment that sets the value registers
under one name and serves a socket under another.

Carrying it forward means threading it through those four places. Dropping it
means `csi.simplyblock.io` is the driver's name and the field does not exist,
which removes the immutability rule of §3.2 along with it. The only deployment
that needs two names is one running two drivers, which is Q1, so the two questions
have one answer between them.

---

## Appendix A: `simplyblockdriver_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, and this is the only place any type appears whole.

```go
// SimplyblockDriverPhase is where the operator has got to with the CSI driver.
// Installing covers the applies of §4.1, and the three values after it are
// decided by what the node plugins and the controller plugin report (§4.2).
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

// SimplyblockDriverSpec is the CSI driver deployment: the node plugin, the
// controller plugin, their RBAC, and the CSIDriver registration they produce.
type SimplyblockDriverSpec struct {
	// Image is the CSI driver image, used by both plugins.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// ImagePullPolicy controls when that image is pulled.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
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

	// ControllerResources and NodeResources set requests and limits for the two
	// plugins. Unset enforces no limits.
	// +optional
	ControllerResources corev1.ResourceRequirements `json:"controllerResources,omitempty"`
	// +optional
	NodeResources corev1.ResourceRequirements `json:"nodeResources,omitempty"`

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
	// reference and outlive this object (§4.1).
	SnapshotSupportOriginInstalled SnapshotSupportOrigin = "Installed"
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
