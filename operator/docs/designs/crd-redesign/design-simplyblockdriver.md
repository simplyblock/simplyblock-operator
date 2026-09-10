# Design Document: The SimplyblockDriver

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-31 (last updated 2026-09-09)  
**Target Release:** simplyblock 26.4  
**Test Plan:** [`tests/test-plan-simplyblockdriver.md`](../../tests/test-plan-simplyblockdriver.md)  
**Example:** [`assets/releases.yaml`](assets/releases.yaml)

The kind does not exist, and the chart installs the CSI driver today, so every
cluster already running simplyblock holds the objects this kind is to own. §4.3
is how the operator takes them over, and §8 is what changes when it has.

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

**On an existing cluster the first of these objects adopts a deployment rather
than making one.** The chart has installed the driver for as long as the product
has shipped, so the reconcile that establishes this kind meets a node plugin on
every worker and a controller plugin provisioning volumes for workloads that are
running. §4.3 is that handover, and it is the path every upgraded cluster takes.

**The driver is a client of the control plane, and takes two things from it.**
The endpoint and credentials it provisions volumes through reach it in the
configuration the operator applies (§4.1), resolved from the one `ControlPlane`
the Kubernetes cluster holds
([`design-controlplane.md`](design-controlplane.md) §3.3). The second is a version
it must not run ahead of, which is what §5 is for.

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
- Specify how a deployment the chart installed becomes this kind's, without
  deleting an object, restarting a plugin that did not have to restart, or
  changing a running configuration (§4.3).
- Specify how many of these objects a Kubernetes cluster holds, and what stops a
  second one (§3.4).

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

Declared in `operator/api/v1alpha2/simplyblockdriver_types.go`, short name `sbd`.
The kind is born at `v1alpha2` and registers no older version, because nothing
was ever persisted at one
([`design-api-upgrade.md`](design-api-upgrade.md) §7.3), so it needs no conversion
function and appears in no storage rewrite. The type is Appendix A. What follows
quotes the field an argument turns on and no more.

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

**`controllerNodeSelector` and `controllerTolerations` place the controller
plugin.** The unprefixed pair is the node plugin's, because that placement
decides which workers can attach a volume, and the prefixed pair is ordinary pod
placement for the one workload that provisions them. The chart carries both as
`controller.nodeSelector` and `controller.tolerations`, which makes them state a
running deployment has and §4.3 has to be able to express.

**`sidecarImages` overrides the six CSI sidecars this deployment runs**, one
optional field each for `csi-provisioner`, `csi-attacher`, `csi-resizer`,
`csi-snapshotter`, and `csi-external-health-monitor-controller` on the controller
plugin, and `node-driver-registrar` on the node plugin. Unset takes the version
this operator release ships, which is the combination it was tested against, and
the field exists because a deployment that pinned one through the chart keeps it
across adoption (§4.3).

**An override is bound by the same registry pattern as `spec.image`.** The node
plugin is privileged and mounts `/dev`, `/sys`, and the kubelet's plugin
directory from the host, so a sidecar image is a container running beside it with
the same access, and the field that names one is worth the same restriction as the
field that names the driver.

**`enableServiceAccountAuth` makes both plugins authenticate with their pod's
service-account token** instead of the static cluster secret. It is one switch
for the deployment rather than one per plugin, because the control plane has to
list the accounts it will accept in `SB_K8S_ADMIN_SERVICE_ACCOUNTS` and a
deployment whose two plugins disagree about which credential they present is one
whose control plane has to be configured for both.

**`enableVolumeSnapshots` decides whether snapshot support is part of this
deployment.** It defaults to true, and true is the `VolumeSnapshotClass` for
`spec.driverName`, plus the CRDs and a controller where the cluster serves
neither (§4.1). False applies none of them, which is the chart's
`snapshotclass.create` and `snapshotcontroller.create` as one field.

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

`status.origin` is `Adopted` where the first reconcile met objects it did not
create and `Created` where it made all of them. It is decided once and never
revised, because what it records is where the running deployment came from
rather than what the controller did most recently, and on every cluster upgraded
from a chart install it reads `Adopted` (§4.3).

`status.observedGeneration` and `status.message` follow the group conventions
([`design-crd-model.md`](design-crd-model.md) §3.1, §7.9).

**The name is `SimplyblockDriver` rather than `CSIDriver`**, because `CSIDriver`
is a kind in core `storage.k8s.io/v1` and two kinds of one name in two groups is
an ambiguity every reader resolves by group. The two are not the same object
either: the core kind is the cluster's registration record, and this one is the
deployment that produces that record among the rest of what it installs
([`design-crd-model.md`](design-crd-model.md) §7.2).

### 3.4 The singleton

**A Kubernetes cluster holds one `SimplyblockDriver`**, not one per namespace, and
the second is rejected when it is written.

**Twelve of the objects it owns are cluster-scoped** (§4.3): five `ClusterRole`
and `ClusterRoleBinding` pairs, the `CSIDriver` registration, and the
`VolumeSnapshotClass`. Two objects deriving those names do not get a copy each.
They get one object written twice. The bindings are where that surfaces, because
each names a `ServiceAccount` together with the namespace it lives in, so two
controllers writing one binding alternate its subject and the deployment not
currently named loses the permissions its sidecars provision with. Neither
deployment is reliably broken and neither is reliably working, which is the shape
that takes longest to find.

**Below the names the contention is not a naming question at all.** The node
plugin registers at `/var/lib/kubelet/plugins/<driverName>/csi.sock` and mounts
that directory from the host, and the kubelet registers one plugin per driver
name. Two node plugins on one worker contend for a path and a registration that
no object name reaches.

**Enforcement is a validating webhook**, `SimplyblockDriverValidator` in
`operator/internal/webhook/simplyblockdriver_validator.go`, which denies a
`CREATE` where a `SimplyblockDriver` already exists in any namespace and carries
`failurePolicy=fail` like every other validator the operator serves. The
`ControlPlane` singleton is enforced by convention instead
([`design-controlplane.md`](design-controlplane.md) §3.1), and what separates the
two is what a second object does: a `ControlPlane` under another name is ignored
and sits inert, and a second `SimplyblockDriver` is reconciled.

**The controller refuses as well, for the object the webhook did not see.** A
webhook is deployed, upgraded, and occasionally not running, and an object written
in that window is one admission never re-examines. So a `SimplyblockDriver` that
is not the oldest in the Kubernetes cluster holds at `Installing`, applies
nothing, names the object that holds the deployment in `status.message`, and emits
`DuplicateDriver` (§6.1). The creation timestamp decides it, and the namespace and
name decide a tie, so that both controllers reach the same answer from the same
list without a lock between them.

**One driver and one control plane are the same limit counted twice.** A
Kubernetes cluster holds one `ControlPlane`
([`design-controlplane.md`](design-controlplane.md) §3.1), so the object the
driver is configured from is the one there is, and the namespace it happens to
live in decides nothing. The driver it deploys serves every workload in the
Kubernetes cluster either way, because a `StorageClass` is cluster-scoped and a
claim in any namespace may name one
([`design-crd-model.md`](design-crd-model.md) §5), so a driver configured from
somewhere other than that one control plane would be a driver provisioning
against a backend it holds no credentials for.

---

## 4. SimplyblockDriver: Controller

`SimplyblockDriverReconciler`, in
`operator/internal/controllers/driver/simplyblockdriver_controller.go`.

### 4.1 What it applies

The node `DaemonSet`, the controller `StatefulSet`, the RBAC both need, the node
configuration `ConfigMap`, and the core `CSIDriver` registration. Deleting the
`SimplyblockDriver` removes all of them, so the ownership spine starts at a real
edge rather than at a Helm release
([`design-crd-model.md`](design-crd-model.md) §5), and §4.3 is the inventory.

**Two mechanisms carry that ownership, because half the set is cluster-scoped.**
The workloads, their `ServiceAccounts`, and the two `ConfigMaps` are namespaced
and become children by controller reference, which hands their removal to the
garbage collector. The credentials `Secret` is namespaced too and is not a child,
because it is not this deployment's (§4.3). The `CSIDriver` registration, the `ClusterRole` and
`ClusterRoleBinding` pairs the sidecars and the node plugin need, and the
`VolumeSnapshotClass` are cluster-scoped, and Kubernetes treats a cluster-scoped
object owned by a namespaced one as having an owner it cannot resolve, which
leaves it uncollected rather than owned
([`design-crd-model.md`](design-crd-model.md) §7.3). Those carry
`storage.simplyblock.io/managed-by: simplyblockdriver` instead, and a finalizer on
the `SimplyblockDriver` deletes them. It is the same split
[`design-storagepool.md`](design-storagepool.md) §4.4 makes for the `StorageClass`
a pool produces, and for the same reason.

**The two plugins carry the CSI sidecars.** The controller `StatefulSet` runs
`csi-provisioner`, `csi-attacher`, `csi-resizer`, `csi-snapshotter`, and
`csi-external-health-monitor-controller` beside the driver. The node `DaemonSet`
runs `node-driver-registrar`, which carries its own HTTP liveness probe rather
than a separate `livenessprobe` sidecar. Each is addressed to this
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

**The control plane reaches the driver through a Secret this deployment does not
own.** The endpoint and the credential each plugin uses live in
`simplyblock-csi-secret-v2`, which the `StorageCluster` reconciler writes,
upserting one entry per cluster it creates or adopts. This deployment mounts that
Secret and never writes it, because two controllers writing one object alternate
its contents. What it does own is the pair of `ConfigMaps` beside it, which carry
no cluster state at all (§4.3).

**One control plane is not one backend cluster.** That Secret carries a
`clusters` list of `cluster_id`, `cluster_endpoint`, and `cluster_secret` triples,
one entry per `StorageCluster` the control plane fronts, and every entry names the
same endpoint because there is one control plane to name. So a deployment with
several backend clusters is expressed by the list growing rather than by a second
driver or a second control plane.

**A driver whose control plane is not reachable is applied and waits**, because a
plugin that cannot reach a backend is the same situation as a plugin that has not
been scheduled yet.

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

### 4.3 Adoption

**Every cluster running simplyblock already holds this deployment**, because the
chart installs it (§8). So the first `SimplyblockDriver` in such a namespace does
not create a driver. It takes one over while volumes are attached and workloads
are running on them.

The window this reconcile closes, and the `helm.sh/resource-policy: keep`
annotations that hold the objects open across the chart upgrade, are
[`design-api-upgrade.md`](design-api-upgrade.md) §12. What that document leaves to
this one is what the controller does when it meets them.

#### The object set

| Object                              | Name                                                                                | Ownership after adoption |
|-------------------------------------|-------------------------------------------------------------------------------------|--------------------------|
| `DaemonSet`                         | `simplyblock-csi-node`                                                              | Controller reference     |
| `StatefulSet`                       | `simplyblock-csi-controller`                                                        | Controller reference     |
| `ServiceAccount`                    | `simplyblock-csi-node-sa`, `simplyblock-csi-controller-sa`                          | Controller reference     |
| `ConfigMap`                         | `simplyblock-csi-cm`, `simplyblock-csi-nodeservercm`                                | Controller reference     |
| `ClusterRole`, `ClusterRoleBinding` | `simplyblock-csi-{node,provisioner,attacher,resizer,health-monitor}-{role,binding}` | `managed-by` label       |
| `CSIDriver`                         | `spec.driverName`                                                                   | `managed-by` label       |
| `VolumeSnapshotClass`               | `simplyblock-csi-snapshotclass`                                                     | `managed-by` label       |

**The three `snapshot.storage.k8s.io` CRDs and the `snapshot-controller` are not
in it.** The chart puts the controller in `kube-system` and annotates both it and
the CRDs `helm.sh/resource-policy: keep` already, so Helm leaves them where they
are. An adopted deployment finds the API served, applies nothing, and records
`status.snapshotSupport: Detected` (§4.1). That is what the field says, whether
this object's controller brought snapshot support to the cluster, and it did not.
What the handover leaves unowned is §9 Q2.

**The credentials `Secret` is not in it, and that is the one exclusion worth
reading twice.** `simplyblock-csi-secret-v2` carries the endpoint and the
credential each plugin actually uses, and the `StorageCluster` reconciler already
writes it, upserting one entry per cluster it creates or adopts. Claiming it here
would put two controllers on one object, alternating its contents, which is the
failure §3.4 keeps a Kubernetes cluster to one driver to avoid and is no better
between two kinds than between two drivers. The deployment mounts it and does not
own it.

**So the control plane reaches the driver through that Secret rather than through
this controller.** §4.1 has the operator resolve the one `ControlPlane` and write
its endpoint into the configuration the plugins mount, and the writer is the
`StorageCluster` reconciler, because the endpoint travels with the cluster
identity and the credential rather than separately from them.

**`Secret/simplyblock-csi-secret` is in nothing at all.** It is the pre-`v2`
credential, and no pod in a measured deployment mounts it. It goes with the chart
templates that rendered it rather than being adopted.

**`ConfigMap/simplyblock-clusters` is not in it either**, despite the name. It is
the storage-node controller's, mounted by that workload and not by either plugin.

#### The names the controller derives are the names that are running

**Every name in the table except the registration's is
`<object name>-csi-<component>`, and the chart writes the same strings
literally.** The `SimplyblockDriver` an upgraded cluster gets is named
`simplyblock`, beside the `ControlPlane` of that name
([`design-controlplane.md`](design-controlplane.md) §3.1), so the derivation lands
on the running objects and adoption is a `Get` on each rather than a mapping table
maintained against chart history. The `CSIDriver` is the exception because it is
named by `spec.driverName`, which is the name the cluster provisions through
rather than a name this object gets to pick.

**One object means the derivation needs no namespace in it** (§3.4). The twelve
cluster-scoped names carry none, and nothing else in the Kubernetes cluster
derives them, so `simplyblock-csi-node-role` identifies one object the way
`simplyblock-csi-node` identifies one `DaemonSet` in one namespace.

#### The spec is seeded from what is running

**The first reconcile after adoption has to be a no-op**, because these objects
were rendered from Helm values and are about to be rendered from a spec. A field
the translation cannot express is not a translation that fails visibly. It is a
running deployment reconfigured on the reconcile that adopts it, which is the
property [`design-api-upgrade.md`](design-api-upgrade.md) §12.3 captures and
diffs for.

| Running state                                                         | Field                                                                              |
|-----------------------------------------------------------------------|------------------------------------------------------------------------------------|
| `image.csi`                                                           | `spec.image`                                                                       |
| `Always`, the chart's pull policy, against this kind's `IfNotPresent` | `spec.imagePullPolicy`, written by the translation rather than left to the default |
| The name the live `CSIDriver` carries, from `driverName`              | `spec.driverName`, read from the registration rather than defaulted                |
| `controller.replicas`                                                 | `spec.controllerReplicas`                                                          |
| `controller.nodeSelector`, `controller.tolerations`                   | `spec.controllerNodeSelector`, `spec.controllerTolerations` (§3.1)                 |
| `snapshotclass.create`, `snapshotcontroller.create`                   | `spec.enableVolumeSnapshots` (§3.1)                                                |
| The six sidecar image and tag values                                  | `spec.sidecarImages`, written only where the release pinned one (§3.1)             |

**`driverName` is read rather than defaulted, and it is the row that would cost
the most.** The field is immutable (§3.2), so a translation that omits it defaults
to `csi.simplyblock.io` on a deployment that registered under another name, and
the object then declares one driver while the cluster attaches volumes through
another. Nothing later corrects it, because the correction is an edit admission
rejects.

**A pinned sidecar survives, and one left at the chart's default does not
become a pin.** The two are told apart by comparing the running image against the
default of the chart version the release was rendered from, which the release's
metadata records. A sidecar at that default translates to nothing and takes the
version this operator ships, so a deployment that never made a choice is not
frozen at a tag somebody stopped maintaining. A sidecar somebody moved translates
to `spec.sidecarImages`, and stays where it was put.

**Adoption therefore expects no difference at all**, which is what §12.3's capture
and diff is written for: the installer compares the operator's output against the
running deployment and treats any difference as a failure of the step. A default
sidecar rolling forward to this release's version is the one exception, and it is
visible in the spec that produced it rather than hidden in the comparison.

**The snapshot controller's image is not among the six.** It is the cluster's
rather than this deployment's, and §4.3 does not install it on a cluster that
already serves the API, which every adopted deployment does.

#### Taking the objects over

```text
1. Read every object of the set that exists
2. Compare what the spec cannot change against what is running
3. Take field ownership from Helm's field manager
4. Annotate helm.sh/resource-policy: keep wherever Helm release metadata is present
5. Set the controller reference, or the managed-by label where the object is
   cluster-scoped
6. Verify both are live on every object
7. Remove the Helm labels and annotations, keeping the resource policy
```

**Step 2 is where adoption can refuse, and it has one comparison to make.**
`driverName` is the only thing the spec cannot change, and the name a deployment
is actually registered under is read from the running node plugin's kubelet
registration path rather than from the registration object, because that path is
where the name takes effect. A mismatch holds the phase at `Installing` with
`status.message` naming both values, emits `AdoptionRefused` (§6.1), and changes
nothing. The counts of §4.2 are still published, because the plugins are running
and what is blocked is the handover rather than the driver.

**The endpoint and the credentials are not compared here, because they are not
this deployment's to write.** They live in the credentials `Secret` the
`StorageCluster` reconciler owns, so an adopted driver keeps whatever that
controller last wrote and this one never has an opportunity to point it
somewhere else.

**Step 3 is a server-side apply that takes the conflict deliberately.** Helm wrote
these objects under its own field manager, so an apply that does not claim the
fields meets what Helm recorded rather than replacing it.

**Step 4 is what makes an adoption outside the upgrade safe.** The installer
annotates before `helm upgrade` for the supported path, and an administrator who
creates the object against a chart-installed deployment by hand has had no such
step, which leaves the driver in a release Helm still tracks and prunes.
Annotating from the controller closes that, and Helm reads the annotation from the
live object rather than from the stored manifest
([`design-api-upgrade.md`](design-api-upgrade.md) §12.2), so annotating is enough.

**Step 7 removes seven keys and keeps one.** The `app.kubernetes.io/managed-by`,
`heritage`, `release`, `revision`, `chart`, and `chartVersion` labels and the
`meta.helm.sh/release-name` and `meta.helm.sh/release-namespace` annotations all
record a release that no longer contains the object, and something later reads a
leftover claim as a live one. `helm.sh/resource-policy: keep` stays, because what
it says, that this object outlives the release, is the part that became
permanently true.

**Adoption is in place at every step.** Nothing in the sequence deletes an object
and applies a replacement, because recreating the node `DaemonSet` restarts every
node plugin in the cluster at once and recreating the `CSIDriver` registration
takes the cluster's ability to attach a volume away for as long as it is absent.

**`status.origin` records which of the two happened** (§3.3), and `DriverAdopted`
is the event (§6.1).

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
| A running deployment was taken over rather than created      | `Normal`  | `DriverAdopted`     | `SimplyblockDriver` |
| Adoption stopped on what the spec cannot change              | `Warning` | `AdoptionRefused`   | `SimplyblockDriver` |
| A second object reached the API server past the webhook      | `Warning` | `DuplicateDriver`   | `SimplyblockDriver` |

**`VersionSkew` and `VersionTooOld` are the two the kind was built for.** Every
other row here reports a deployment's health, and these two report combinations
that are otherwise discoverable only by attaching a volume and watching it fail. A
control plane exactly one release ahead of the driver produces neither, which is
the supported state of §5.

**`AdoptionRefused` fires on a deployment that is working**, which is what makes
it worth an event rather than a phase. The plugins are serving, volumes attach,
and what has stopped is the handover, so nothing else in the cluster reports that
the objects still belong to a Helm release. It repeats on every reconcile that
finds the same mismatch, because the condition is standing rather than momentary.

**`DuplicateDriver` lands on the object that is not running anything.** The
webhook of §3.4 denies the second object at admission and leaves no object to
carry an event, so this row exists for the one that was written while the webhook
was not serving. It is the only report that object gets, since a deployment that
applies nothing has no plugins to derive a phase from.

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
rejection is the API server's. The singleton of §3.4 splits across both harnesses
for the same reason: the webhook's denial is admission and needs a real API
server, and the controller's refusal is a comparison over a list and belongs
beside the other fake-client rows.

Adoption divides the same way. Which objects the controller reaches for, which
of the two ownership mechanisms each one gets, and which Helm keys survive step 7
are all decidable against a fake client seeded with the objects the chart writes,
and so is every refusal of §4.3's step 2. The part that is not is field ownership,
because taking a field from another manager is the API server's behavior and a
fake client has no field managers at all, so it belongs in `envtest` beside the
immutability marker.

The risk unit tests do not reach is the skew itself, which needs a driver and a
control plane at two versions and a volume to attach, and it is where §1 says the
failure actually lands. Adoption has one of the same shape: a chart-installed
cluster with attached volumes, upgraded, is the only place where a plugin that
restarts when it did not have to shows up as anything a test can see.

---

## 8. What This Replaces

The objects do not change and their owner does. Every cluster running
simplyblock has the deployment already, applied by the chart, and §4.3 is the
reconcile that takes it over in place.

| Today                                               | After                                                       |
|-----------------------------------------------------|-------------------------------------------------------------|
| The chart renders the node `DaemonSet`              | The controller applies it (§4.1)                            |
| The chart renders the controller `StatefulSet`      | The controller applies it (§4.1)                            |
| The chart renders the `CSIDriver` registration      | The controller applies it, owned by the `SimplyblockDriver` |
| The driver's version is a Helm release's            | `spec.image`, compared against the control plane's (§5)     |
| The snapshot controller is a chart value            | `spec.enableVolumeSnapshots` (§3.1)                         |
| Nothing compares driver and control-plane versions  | `VersionSkew` and the two gauges (§6)                       |
| The RBAC and the snapshot class belong to a release | The `managed-by` label and a finalizer (§4.1, §4.3)         |
| `helm uninstall` removes the driver                 | It leaves it running, and deleting the object removes it    |
| Seven chart values pin the sidecar images           | The operator's release, or `spec.sidecarImages` (§3.1)      |

**Moving the install out of the chart is not free**, and it is the same cost
[`design-controlplane.md`](design-controlplane.md) §5.1 names for the control
plane: a chart template is a file a user can read, fork, and patch, and a
controller's apply is none of those. It cannot be a flag day either, for the same
reason and with the same unanswered question, which
[`design-controlplane.md`](design-controlplane.md) §12 records as its Q2.

**What a user loses is a value, and what they lose it to is a field.** Each row
above moves one setting from `values.yaml` to a spec, and §4.3's translation table
is where the two are matched up. Every value has a field on the other side, and
the ones that do not translate are the sidecars a release left at the chart's
default, which move to this operator's versions rather than staying where a chart
put them.

**`helm uninstall` stops being a teardown**, which inverts what the command has
meant. It removes the operator, and the driver keeps running because it is the
operator's and a custom resource is what removes it.
[`design-api-upgrade.md`](design-api-upgrade.md) §12.5 is where the upgrade says
so to the person running it.

---

## 9. Open Questions

Q1, Q4, Q5, and Q6 are settled, and their numbers are retired rather than reused
because all four are cited from review history.

§3.4 answered Q1 and Q4 together: a Kubernetes cluster holds one
`SimplyblockDriver`, so neither two drivers in one namespace nor two namespaces
each holding one is a topology the derivation has to separate. §3.4's closing
paragraph answered Q6 with the matching limit on the other kind, one `ControlPlane`
per Kubernetes cluster, which leaves the driver one object to be configured from
and §4.1's `clusters` list to carry the backends under it. §3.1 answered Q5 with
`spec.sidecarImages`: a pin a release made survives adoption, and a sidecar left
at a chart default does not become one.

**Q2: What removes an installed snapshot controller.** §4.1 has the operator
install the CRDs and a controller where the cluster has none, without a controller
reference, so nothing removes them when the `SimplyblockDriver` is deleted. A
cluster left with snapshot CRDs and a controller has working snapshot support and
no simplyblock, which is harmless and untidy. Removing them needs a count of what
else in the cluster relies on them, and `status.snapshotSupport` records only what
this object did. Leaving them is what §4.1 specifies.

An adopted deployment reaches the same place by a different route and leaves more
behind. The chart installed the CRDs and put a `snapshot-controller` in
`kube-system`, both annotated `helm.sh/resource-policy: keep`, so after the
handover Helm no longer tracks them, this operator did not install them, and
`status.snapshotSupport` reads `Detected` (§4.3). The `Deployment` is then a
running workload with no owner of any kind, which is a state neither this question
nor the annotation was written for.

**Q3: Whether `driverName` should be settable.** §3.1 carries it because the
chart carries it, as the value `driverName` in both `values.yaml` files. In the
chart the value reaches one object: the `CSIDriver` registration's name. The name
is written literally everywhere else it appears, in the node plugin's
`--kubelet-registration-path`, in the hostPath the plugin mounts, and in the
snapshot class's `driver` field, so a deployment that sets the value registers
under one name and serves a socket under another.

Carrying it forward means threading it through those four places. Dropping it
means `csi.simplyblock.io` is the driver's name and the field does not exist,
which removes the immutability rule of §3.2 along with it.

§3.4 narrows the question without closing it. A Kubernetes cluster holds one
driver, so no deployment needs two names and nothing new has a use for the field.
What still does is adoption: a release that set `driverName` registered under it,
every `PersistentVolume` it provisioned records it, and §4.3 has to be able to
express that or the object declares one driver while the cluster attaches volumes
through another. So the field survives for the installations that already set it,
and whether it stays settable for the ones that have not is what is left.


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
}

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

	// SidecarImages overrides the six CSI sidecars, one field each. Unset takes
	// the version this operator release ships.
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
