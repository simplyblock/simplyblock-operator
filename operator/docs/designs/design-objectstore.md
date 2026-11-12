# Design Document: S3 Gateway Provisioning (`ObjectStore`)

**Status:** Draft  
**Author:** Max Schettler  
**Date:** 2026-10-14  
**Issue:** none yet — no GitHub issue has been filed for this design  
**Test Plan:** [`tests/test-plan-objectstore.md`](../tests/test-plan-objectstore.md)

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Design — New CRD](#4-api-design--new-crd)
5. [TLS and Service Exposure](#5-tls-and-service-exposure)
6. [State Machine](#6-state-machine)
7. [Controller Design](#7-controller-design)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Failure Modes and Fallback](#9-failure-modes-and-fallback)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `objectstore_types.go`](#appendix-a-objectstore_typesgo)

---

## Overview

An `ObjectStore` is the desired existence of one S3 gateway: a `Deployment` running the gateway binary (a separate repository, `embedgw` plus a simplyblock backend, per `design-bucket-provisioning.md` §1), a `ClusterIP` `Service` in front of it, and that `Service`'s serving certificate. `S3Bucket.spec.objectStoreRef` (`design-bucket-provisioning.md` §4) names one by its Kubernetes object name, which is also the Deployment's and the Service's name and the label every gateway pod carries: nothing about finding an `ObjectStore`'s gateway pods requires reading the `ObjectStore` object itself.

This document provisions the workload only. It does not provision buckets (`design-bucket-provisioning.md`), does not touch the gateway binary's own code, S3 API behavior, or its Vault-backed IAM (a separate repository, out of this operator's scope entirely, same boundary that design draws), and does not expose the gateway outside the cluster: `spec.tls` and the `Service` it produces are for in-cluster and cluster-edge termination, and reaching an `ObjectStore` from outside the cluster is left to whatever Ingress, Gateway API route, or LoadBalancer the deployer already uses for other cluster-internal `Service`s.

---

## 1. Background

`design-bucket-provisioning.md` reads an `ObjectStore` to find the nodes a bucket's mount has to reach (§5.3: "the `ObjectStore`'s gateway Deployment's pod→node placement") and otherwise treats the CRD as given (§2 Non-Goals: "the gateway's Deployment, Service, and TLS, and replica count"). Nothing provisions any of that today. This design is that CRD and its reconciler.

The gateway itself needs nothing from the control plane (`sbcli`) or from `atlas-lib`: it is an ordinary stateless HTTP(S) workload from the operator's point of view, the same shape `SimplyblockDriverSpec` already provisions for the CSI plugins (image, replicas, placement, resources) and the same TLS primitive `utils.BuildServiceServingCertificate` already issues for the webapi, storage-node-set API, SPDK proxy, and the operator's own webhook server (`operator/internal/utils/tls.go`). Nothing here is a new mechanism. It is a new object built from mechanisms this repository already has.

---

## 2. Goals and Non-Goals

### Goals

- Represent one S3 gateway's workload as Kubernetes desired state (`ObjectStore`), reconciled the same way every other operator-managed workload converges.
- Make an `ObjectStore`'s gateway pods findable by name alone: the Deployment, the Service, and the pod label are all derived from `metadata.name`, so `design-bucket-provisioning.md`'s `S3BucketReconciler` needs no field read off `ObjectStore` to locate them (resolves that design's Open Question 2).
- Reuse the existing dual-provider (OpenShift serving-cert annotation, cert-manager `Certificate`) TLS primitive rather than adding a third way to issue a server certificate.
- Pass the gateway's own credentials and IAM configuration through opaquely (`spec.envFrom`), so this CRD does not have to change if the gateway repository's Vault integration changes.
- Converge a replica count or placement change, and survive an operator restart mid-provisioning, without manual intervention.

### Non-Goals

- **The gateway binary itself:** its S3 API behavior, its `CreateBucket` handler, and its Vault-backed IAM. A separate repository, out of this operator's scope entirely (`design-bucket-provisioning.md` §2).
- **Bucket provisioning.** `S3Bucket` and its mount-delivery mechanism (`design-bucket-provisioning.md`) are a separate design that reads this one's output.
- **External exposure.** `spec.tls` and the `Service` this design produces are `ClusterIP`, cluster-internal. An Ingress, Gateway API route, or cloud LoadBalancer in front of that `Service` is the deployer's own choice, made the same way for any other in-cluster `Service`, not a field on `ObjectStore`.
- **A dedicated node pool for gateways.** `spec.nodeSelector`/`spec.tolerations` are plain passthrough to the pod spec, the same pattern `SimplyblockDriverSpec` already uses for the CSI node plugin. No node-pool concept, no gateway-specific scheduling.
- **Horizontal autoscaling.** `spec.replicas` is a fixed count. Scaling an `ObjectStore` in response to load is a decision the deployer makes (directly, or by pointing an HPA at the Deployment this design creates), not something this CRD wires up itself.
- **Credential or secret rotation mechanics beyond what `envFrom` already gives Kubernetes.** This CRD names which `Secret`s and `ConfigMap`s feed the gateway container's environment. Whatever rotates their contents is out of scope here.
- **Bucket (object) versioning.** Deliberately deferred, not merely unbuilt. Enabling versioning on a bucket is irreversible in S3 (never-versioned → `Enabled` → `Suspended`, never back), so it has to be right before it ships rather than added as a quick follow-up. Two separate blockers, neither solved by deferring the decision:
  - **Placement.** `versitygw`'s own POSIX backend hard-rejects any `VersioningDir` placed under its `rootdir` (`isDirBelowRoot`/`validateSubDir`) — not an integration gap this operator could close, `versitygw`'s own code. The gateway also runs one `posix.New(cfg.RootDir, ...)` per process, and `cfg.RootDir` is an `ObjectStore`-scoped subdirectory of the shared mount with every one of that store's buckets bind-mounted under it (`design-bucket-provisioning.md` §5.1), not a per-bucket root. There is nowhere to place a version directory that `versitygw` will accept and that this operator's volume/capacity accounting would actually see.
  - **Data path.** Every object a server-side copy produces (every multipart-assembled object today, every `CopyObject` once wired, every object version if this ever ships) becomes an XFS reflinked inode, and the kernel pNFS server explicitly refuses a block layout for a reflinked inode (`fs/xfs/xfs_pnfs.c`, `-ENXIO`, "the Linux pNFS server doesn't implement it yet"). That object is then served through the metadata server for the rest of its life, never over direct NVMe-oF — not a capacity or write-amplification cost, a permanent loss of this design's own direct data path for exactly the objects versioning would multiply.

  Both need solving, not just one, before versioning is safe to expose as an `S3Bucket`/`ObjectStore` field. `CreateBucket`/`PutBucketVersioning` against this gateway today returns `VersioningNotConfigured`, `versitygw`'s own answer for a backend that never set `PosixOpts.VersioningDir`.

---

## 3. Architecture Overview

```
┌──────────────────────────────── Kubernetes API ─────────────────────────────────┐
│                                                                                    │
│   ┌───────────────────────── ObjectStoreReconciler ─────────────────────────┐    │
│   │ 1. Ensure a Deployment named <name> exists, pods labeled                 │    │
│   │    storage.simplyblock.io/objectstore=<name>                           │    │
│   │ 2. Ensure a ClusterIP Service named <name>, same selector               │    │
│   │ 3. Ensure that Service's serving certificate (utils.tls)                 │    │
│   │ 4. Phase = Ready once the Deployment reports Available                  │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│    ObjectStore CR   spec.{replicas,image,port,tls,envFrom,nodeSelector,…}        │
│                      status.{phase,readyReplicas,conditions}                      │
│                                                                                    │
│   watches: Deployment, Service, Certificate (owned)                              │
└──────────────┬────────────────────────────────────────────────────────┬──────────┘
               │ owns                                                   │ owns
               ▼                                                         ▼
┌──────────────────────────────┐                          ┌───────────────────────────┐
│  Deployment <name>             │                          │  Service <name>            │
│  - gateway container (embedgw) │◀──────── selects ────── │  ClusterIP, port spec.port │
│  - envFrom: spec.envFrom       │                          │  serving-cert annotation / │
│  - TLS secret volume            │                          │  cert-manager Certificate  │
└──────────────────────────────┘                          └───────────────────────────┘
```

**What the diagram cannot show:** this reconciler runs in the existing `kubernetes/operator` manager, the same process `design-bucket-provisioning.md`'s `S3BucketReconciler` does (`0005-s3bucket-joins-the-main-operator-manager.md`), not a manager of its own.

---

## 4. API Design — New CRD

### 4.1 `ObjectStore` Spec

`Image` carries no default. Unlike `SimplyblockDriverSpec.Image`, which defaults to the operator's own release because the two are built and tested as a pair, the gateway is a separate repository with no tagged releases yet (confirmed by a feasibility spike that embedded `versitygw` as a library with no forking needed), so there is no "the same release" to default to. `Image` is `crdtypes.ImageReference`, the same shared type every other image field on this operator's CRDs now uses, rather than a field-specific registry allowlist regex — the CRD briefly carried a three-registry pattern of its own, from before `ObjectStore` existed, and was brought in line with the rest once it did.

```go
// Image is the gateway image: embedgw plus the simplyblock POSIX backend.
// Required: unlike the CSI driver and the operator, this repository and the
// gateway's are not released together, so there is no sensible default.
// +kubebuilder:validation:Required
Image crdtypes.ImageReference `json:"image"`

// Port is the gateway's listening port, passed to the container and used
// as both the container port and the Service's port. Required rather than
// defaulted: the gateway binary's own default is not this repository's to
// assert (Open Question 1).
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:Maximum=65535
Port int32 `json:"port"`

// Replicas is the gateway Deployment's replica count.
// +kubebuilder:validation:Minimum=1
// +kubebuilder:default=1
// +optional
Replicas *int32 `json:"replicas,omitempty"`
```

`NodeSelector`, `Tolerations`, and `Resources` are plain passthrough to the gateway pod spec, each the same type and doc-comment convention `SimplyblockDriverSpec` already carries for the CSI node plugin (`kubernetes/operator/api/v1alpha2/simplyblockdriver_types.go`):

```go
// NodeSelector restricts which workers run gateway pods. Empty means every
// schedulable worker, ordinary Deployment scheduling with no gateway-specific
// node-pool concept.
// +optional
NodeSelector map[string]string `json:"nodeSelector,omitempty"`

// +optional
Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

// +optional
Resources corev1.ResourceRequirements `json:"resources,omitempty"`
```

`EnvFrom` is the whole of this design's credential passthrough (§2 Goals, Non-Goals): the gateway container's environment, and nothing more structured than that.

```go
// EnvFrom is injected into the gateway container's environment unmodified,
// the mechanism for the gateway's own credentials and Vault-backed IAM
// configuration (a separate repository's concern; design-bucket-provisioning.md
// §2 Non-Goals). This operator reads none of it.
// +optional
EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

// TLS configures the Service's serving certificate, reusing
// utils.BuildServiceServingCertificate / utils.ServingCertServiceAnnotations
// (operator/internal/utils/tls.go), the same primitive the webapi Service,
// the storage-node-set API, the SPDK proxy, and the webhook server already
// use. Always issued: an S3 gateway is assumed to always serve over TLS.
// +kubebuilder:validation:Enum=OpenShift;cert-manager
// +kubebuilder:default=OpenShift
// +optional
TLSProvider string `json:"tlsProvider,omitempty"`
```

`ImagePullPolicy` reuses `SimplyblockDriverSpec`'s own enum and default (`Always`, matching the rationale there: a moving tag rebuilt in place must not be cached stale by `IfNotPresent`). Carried here for the same reason, not repeated as prose.

### 4.2 `ObjectStore` Status

```go
// ObjectStorePhase is the lifecycle phase of a gateway's provisioning.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded
type ObjectStorePhase string

// ReadyReplicas mirrors Deployment.Status.ReadyReplicas, so a reader does
// not have to look at the owned Deployment to answer "how many gateway pods
// are actually serving."
// +optional
ReadyReplicas int32 `json:"readyReplicas,omitempty"`
```

The whole type is in Appendix A. No finalizer: the Deployment, the Service, and the `Certificate` (or the OpenShift serving-cert annotation, which needs none) are all owned via `ownerReferences` and garbage-collected by Kubernetes on delete, and nothing outside this design's own objects needs cleanup. Deleting an `ObjectStore` while an `S3Bucket` still names it in `spec.objectStoreRef` is not blocked: `design-bucket-provisioning.md` §6 already tolerates a gateway Deployment disappearing out from under existing buckets ("Mount-holders... existing deliveries are left in place... until the `S3Bucket` itself is deleted"), so an `ObjectStore`'s own deletion needs no stronger guarantee than that design already assumes.

A CR as it exists once `Ready`:

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: ObjectStore
metadata:
  name: team-a-store
  namespace: s3-system
spec:
  image: quay.io/simplyblock-io/simplyblock-s3-gateway:main
  port: 9000
  replicas: 3
  envFrom:
    - secretRef:
        name: team-a-store-vault-token
status:
  phase: Ready
  readyReplicas: 3
  conditions:
    - type: DeploymentAvailable
      status: "True"
      reason: MinimumReplicasAvailable
    - type: CertificateReady
      status: "True"
      reason: CertificateIssued
  observedGeneration: 2
```

---

## 5. TLS and Service Exposure

The `Service` this design creates is `ClusterIP`, named identically to the `ObjectStore`, selecting pods labeled `storage.simplyblock.io/objectstore=<name>`, and carrying exactly one port, `spec.port`. Its serving certificate is issued by `utils.BuildServiceServingCertificate(namespace, name, secretName)`, the identical call `operator/internal/webhook/certmanager.go` and `operator/internal/controllers/node/workload_controller.go` already make for their own Services, branching on `spec.tlsProvider` the same way `utils.NormalizeTLSProvider`/`TLSProviderSupported` already do. The resulting Secret is mounted into the gateway container at `/etc/simplyblock/tls`, and `GATEWAY_TLS_CERT_FILE` and `GATEWAY_TLS_KEY_FILE` point the gateway at its `tls.crt` and `tls.key`. The gateway re-reads both files when the provider rotates the Secret, so a renewed certificate is served without restarting the pod. `spec.port` is the only thing that varies between an `ObjectStore` and any other TLS-terminating Service in this operator.

Reaching this Service from outside the cluster is not this design's concern (§2 Non-Goals): it is an ordinary `ClusterIP` Service, fronted the same way any other one in the cluster is.

---

## 6. State Machine

```
  ObjectStore created
    │
    ▼
  Pending          ← no Deployment yet
    │  Deployment, Service, Certificate created
    ▼
  Provisioning     ← waiting on the Deployment to report Available and the
    │                certificate to report Ready
    │  both satisfied
    ▼
  Ready            ← status.conditions all True
    │  spec changed (replicas, image, placement): reconciles in place,
    │  stays Ready or returns to Provisioning, never Pending
    ▼
  (re-enters Provisioning only via Degraded, never directly from Ready)

  deletion requested at any phase
    │
    ▼
  owned objects garbage-collected; no finalizer, no Deleting phase
```

| Condition                                                                                                                  | Phase        | Result                                                                                                                                     |
|----------------------------------------------------------------------------------------------------------------------------|--------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| The gateway image cannot be pulled, or the Deployment's pods crash-loop                                                    | Provisioning | Stays `Provisioning` past its deadline, then `Degraded`, `DeploymentAvailable` `False` with the pod's own reason                           |
| The TLS provider cannot issue a certificate (cert-manager `Issuer` missing, OpenShift serving-cert controller not running) | Provisioning | `Degraded`, `CertificateReady` `False` naming the cause                                                                                    |
| Operator restarts mid-`Provisioning`                                                                                       | Provisioning | Resumes from `status`: re-reads the Deployment, Service, and Certificate rather than restarting from `Pending`                             |
| `spec.replicas` lowered while gateway pods are serving traffic                                                             | Ready        | Ordinary `Deployment` rolling scale-down. This design adds no draining beyond what the gateway's own readiness probe already gives kubelet |

---

## 7. Controller Design

### 7.1 Location

The existing `kubernetes/operator` manager, the same process `design-bucket-provisioning.md`'s `S3BucketReconciler` registers into (`design-bucket-provisioning.md` §7.1): its own `DeliverBucket`/`WithdrawBucket` calls need the main operator's in-process `PeerRegistry`, which only exists in that manager, so a separate manager for either kind would need a new cross-process channel to reach it for no other benefit.

Both are kinds of `storage.simplyblock.io/v1alpha2`, the group and version every other kind this manager reconciles already uses, rather than a group of their own (`objectstore.simplyblock.io`, which an earlier draft of this design used). Reconsidered once drafted: controller-runtime starts a watch for every registered kind at manager startup, and a watch whose cache cannot sync fails `mgr.Start` once the cache-sync timeout expires, which exits the process. A cluster carrying the storage CRDs but not this separate group's CRDs would therefore crash-loop the *entire* operator — every reconciler in it, not just these two kinds — roughly two minutes after start, reporting a cache-sync timeout rather than a missing CRD; reachable by any upgrade that rolls the Deployment before applying new CRDs. A shared group has no such failure mode: its CRDs ship and version together, as they already do for every other kind. The independence a separate group would have bought was never exercised either — neither kind had shipped yet, so there was no stored data, no conversion, and no migration for the move to break.

### 7.2 Reconciliation Trigger

Watches: `ObjectStore` (primary), `Deployment`, `Service`, and `Certificate` (owned).

### 7.3 Concurrency and Mutual Exclusion

One `ObjectStore` reconciles independently of another. Nothing here is cluster-wide singleton state. Within one `ObjectStore`, every owned object is idempotently applied on each pass, so two concurrent reconciles converge to the same result: the standard controller-runtime guarantee.

### 7.4 Interaction with Existing Controllers

Creates and owns `Deployment`, `Service`, and `Certificate` (or sets the OpenShift serving-cert annotation, which needs no object of its own). Writes nothing outside objects it owns. `design-bucket-provisioning.md`'s `S3BucketReconciler` reads this design's Deployment and its pods (by the documented label), but never writes to anything this design owns.

### 7.5 RBAC

| Resource                              | Verbs                                    | Why                                                           |
|---------------------------------------|------------------------------------------|---------------------------------------------------------------|
| `objectstores`, `objectstores/status` | get, list, watch, update, patch          | Primary reconciled kind                                       |
| `deployments.apps`                    | get, list, watch, create, update, delete | The gateway workload                                          |
| `services`                            | get, list, watch, create, update, delete | The gateway's `ClusterIP` Service                             |
| `certificates.cert-manager.io`        | get, list, watch, create, update, delete | The Service's serving certificate, cert-manager provider only |

---

## 8. Backend API Requirements

None. This design calls no `sbcli` endpoint: the gateway binary's own interaction with `sbcli`, if any, is that repository's concern, not this reconciler's.

---

## 9. Failure Modes and Fallback

| Failure                                                                                     | Detection                                               | Behavior                                                                                                                                                    |
|---------------------------------------------------------------------------------------------|---------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.image` does not exist or cannot be pulled                                             | Pod `ImagePullBackOff`                                  | `Degraded`, `DeploymentAvailable` `False`, event naming the image                                                                                           |
| `spec.tlsProvider: cert-manager` with no matching `Issuer`/`ClusterIssuer` in the namespace | `Certificate` stays `False`                             | `Degraded`, `CertificateReady` `False`, event naming the missing issuer                                                                                     |
| `spec.envFrom` names a `Secret`/`ConfigMap` that does not exist                             | Pod stays `CreateContainerConfigError`                  | `Degraded`, `DeploymentAvailable` `False`, event naming the missing reference                                                                               |
| A gateway pod crashes under live S3 traffic                                                 | `Deployment`'s own `Pod` restart, `readyReplicas` drops | Ordinary Kubernetes pod restart. `S3Bucket`'s own mount delivery (`design-bucket-provisioning.md` §5) is unaffected, since it targets the node, not the pod |

---

## 10. Observability

Addition to a surface that exists: `Deployment`/`Pod` events and the Kubernetes `Event` API are reused patterns, applied to a new kind.

### Kubernetes Events

Target: the `ObjectStore` object, the audit record a deployer watches, which outlives any individual `Deployment` revision or pod.

| Event                                                                   | Type    | Reason                   |
|-------------------------------------------------------------------------|---------|--------------------------|
| The gateway Deployment has not become Available within the retry budget | Warning | `DeploymentNotAvailable` |
| The Service's certificate could not be issued                           | Warning | `CertificateNotReady`    |
| The `ObjectStore` reached `Ready`                                       | Normal  | `ObjectStoreReady`       |

### Prometheus Metrics

| Metric                                         | Labels                 | Description                                                                                                                   |
|------------------------------------------------|------------------------|-------------------------------------------------------------------------------------------------------------------------------|
| `simplyblock_objectstore_phase_state`          | `objectstore`, `phase` | Gauge, 1 for the current phase and 0 for the others. What a dashboard sums to show gateways stuck outside `Ready`             |
| `simplyblock_objectstore_ready_replicas_count` | `objectstore`          | Gauge, mirrors `status.readyReplicas`, the load-bearing one: it is what an alert on "fewer gateway pods than requested" reads |

---

## 11. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts: [`tests/test-plan-objectstore.md`](../tests/test-plan-objectstore.md)

- **Unit:** the `ObjectStorePhase` transition table against every input combination in §6's failure table, and the Deployment/Service/Certificate construction functions against a fake client, independent of a real cert-manager or OpenShift installation.
- **Integration:** the full reconcile loop against `envtest`, with cert-manager's CRDs installed and a fake Deployment controller (`envtest` does not run kubelet, so `Deployment.status.availableReplicas` is set by the test, not by real pods). Proves the `ObjectStore` state machine, object ownership, and RBAC.
- **E2E:** a live cluster, both TLS providers, proving the one thing integration tests cannot: that a real `Certificate` is issued and actually lands in the Secret the gateway pod mounts, and that the pod serves TLS with it.

---

## 12. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                          | Owner                                                                                                                                                                                                                                                                                                                              |
|-----|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 1   | **The gateway's own default port:** `spec.port` is required here rather than defaulted because this repository does not know `embedgw`'s own default. Once the gateway repository documents one, revisit whether `spec.port` should default to it.                                                                                                                | Gateway repository                                                                                                                                                                                                                                                                                                                 |
| 2   | **Readiness and graceful shutdown during a scale-down or rolling update:** §6's failure table notes this design adds no draining beyond kubelet's ordinary pod-restart behavior. Whether the gateway binary's own readiness/liveness probes are enough to avoid dropping in-flight S3 requests during a rollout is that repository's question, not designed here. | Gateway repository                                                                                                                                                                                                                                                                                                                 |
| 3   | **Richer credential injection:** `spec.envFrom` (§4.1, §2 Non-Goals) covers environment variables only. If the gateway's Vault integration needs a projected volume (a Vault Agent sidecar, a Kubernetes-auth service-account token) rather than env vars, this CRD has no field for it yet.                                                                      | Resolved: env vars suffice. The gateway authenticates to Vault with a token or AppRole (`gateway/README.md` "Why no backend or IAM code lives here"), both plain strings `VGW_IAM_VAULT_*` carries, and `embedgw.Config`'s Vault fields take PEM contents directly rather than file paths — nothing here needs a projected volume. |

---

## Appendix A: `objectstore_types.go`

```go
// Package v1alpha2 of the storage.simplyblock.io API group.

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/crdtypes"
)

// ObjectStorePhase is the lifecycle phase of a gateway's provisioning.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded
type ObjectStorePhase string

const (
	ObjectStorePhasePending      ObjectStorePhase = "Pending"
	ObjectStorePhaseProvisioning ObjectStorePhase = "Provisioning"
	ObjectStorePhaseReady        ObjectStorePhase = "Ready"
	ObjectStorePhaseDegraded     ObjectStorePhase = "Degraded"
)

// ObjectStoreSpec is the desired state of one S3 gateway's workload.
type ObjectStoreSpec struct {
	// Image is the gateway image: embedgw plus the simplyblock POSIX backend.
	// Required: unlike the CSI driver and the operator, this repository and
	// the gateway's are not released together, so there is no sensible
	// default.
	// +kubebuilder:validation:Required
	Image crdtypes.ImageReference `json:"image"`

	// ImagePullPolicy controls when that image is pulled. Defaults to Always
	// for the same reason SimplyblockDriverSpec's does: the default tag is a
	// moving one, and IfNotPresent against a tag that moved leaves workers at
	// different versions invisibly.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// Port is the gateway's listening port, used as both the container port
	// and the Service's port. Required rather than defaulted: the gateway
	// binary's own default is not this repository's to assert (Open
	// Question 1).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Region is the AWS region string this gateway's S3 API answers to.
	// Every client must sign its requests for exactly this region; the
	// gateway rejects any other with AuthorizationHeaderMalformed. Also
	// reported by GetBucketLocation.
	// +kubebuilder:default=us-east-1
	// +optional
	Region string `json:"region,omitempty"`

	// Replicas is the gateway Deployment's replica count.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// NodeSelector restricts which workers run gateway pods. Empty means
	// every schedulable worker: ordinary Deployment scheduling, no
	// gateway-specific node-pool concept.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let gateway pods schedule onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Resources are the gateway container's requests and limits. Unset
	// enforces no limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// EnvFrom is injected into the gateway container's environment
	// unmodified: the gateway's own credentials and Vault-backed IAM
	// configuration (a separate repository's concern). This operator reads
	// none of it.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// TLSProvider selects how the Service's serving certificate is issued,
	// reusing utils.BuildServiceServingCertificate /
	// utils.ServingCertServiceAnnotations (operator/internal/utils/tls.go).
	// Always issued: an S3 gateway is assumed to always serve over TLS.
	// +kubebuilder:validation:Enum=OpenShift;cert-manager
	// +kubebuilder:default=OpenShift
	// +optional
	TLSProvider string `json:"tlsProvider,omitempty"`

	// Buckets is what every S3Bucket the gateway creates for an S3
	// CreateBucket request is provisioned with: the request itself carries
	// no size or storage class.
	// +kubebuilder:validation:Required
	Buckets ObjectStoreBuckets `json:"buckets"`
}

// ObjectStoreBuckets is the backing-volume shape of a bucket created through
// the gateway's S3 API.
type ObjectStoreBuckets struct {
	// StorageClassName becomes each S3Bucket's spec.storageClassName. It
	// must select the pnfs fstype, the same requirement S3Bucket states.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	StorageClassName string `json:"storageClassName"`

	// CapacityBytes becomes each S3Bucket's spec.capacityBytes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	CapacityBytes int64 `json:"capacityBytes"`
}

// ObjectStoreStatus is what the gateway's provisioning currently is.
type ObjectStoreStatus struct {
	// Phase is the operator's own view of the deployment.
	// +optional
	Phase ObjectStorePhase `json:"phase,omitempty"`

	// ReadyReplicas mirrors Deployment.Status.ReadyReplicas, so a reader
	// does not have to look at the owned Deployment to answer "how many
	// gateway pods are actually serving."
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Conditions carries why an ObjectStore is Degraded, which a phase
	// alone cannot express. Expected types: DeploymentAvailable,
	// CertificateReady.
	// +optional
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// PhaseDeadline is when the current phase gives up waiting and degrades,
	// carried in status rather than only in memory so a restarted operator
	// resumes the same deadline instead of granting a fresh one (§6). Added
	// beyond this appendix's first draft, the same gap S3BucketStatus's own
	// appendix had.
	// +optional
	PhaseDeadline *metav1.Time `json:"phaseDeadline,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=s3store
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ObjectStore is the desired existence of one S3 gateway: its Deployment,
// its Service, and that Service's serving certificate. Every gateway pod
// carries the label storage.simplyblock.io/objectstore=<name>, which is
// how design-bucket-provisioning.md's S3BucketReconciler finds them without
// reading this object.
type ObjectStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStoreSpec   `json:"spec,omitempty"`
	Status ObjectStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ObjectStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectStore `json:"items"`
}
```
