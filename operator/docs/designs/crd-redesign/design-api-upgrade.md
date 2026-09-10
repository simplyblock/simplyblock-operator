# Design Document: API Upgrade and Resource-Model Migration

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-09-09  
**Related designs:** [`design-crd-model.md`](design-crd-model.md) §9 is the migration inventory this document delivers  
**Test Plan:** [`test-plan-api-upgrade.md`](../../tests/test-plan-api-upgrade.md), not yet written

---

## Table of Contents

1. [Purpose](#1-purpose)
2. [Design Goals](#2-design-goals)
3. [Non-Goals](#3-non-goals)
4. [High-Level Architecture](#4-high-level-architecture)
5. [Installation and Ownership Model](#5-installation-and-ownership-model)
6. [Conversion Webhook](#6-conversion-webhook)
7. [API Version Strategy](#7-api-version-strategy)
8. [TLS and Conversion Webhook Bootstrap](#8-tls-and-conversion-webhook-bootstrap)
9. [`upgrade`](#9-upgrade)
10. [Conversion Smoke Test](#10-conversion-smoke-test)
11. [CRD Installation During `upgrade`](#11-crd-installation-during-upgrade)
12. [Release Handover](#12-release-handover)
13. [Operator Upgrade](#13-operator-upgrade)
14. [User Verification Point](#14-user-verification-point)
15. [`migrate`](#15-migrate)
16. [Application-Level Resource Migration](#16-application-level-resource-migration)
17. [Discover](#17-discover)
18. [Validate](#18-validate)
19. [Name and Identity Validation](#19-name-and-identity-validation)
20. [Ownership Migration](#20-ownership-migration)
21. [Resource Creation and Deletion](#21-resource-creation-and-deletion)
22. [Idempotency](#22-idempotency)
23. [Checkpoints and Progress](#23-checkpoints-and-progress)
24. [Storage-Version Migration](#24-storage-version-migration)
25. [Failure Handling](#25-failure-handling)
26. [Rollback Considerations](#26-rollback-considerations)
27. [The Plan](#27-the-plan)
28. [Temporary Conversion Webhook Lifecycle](#28-temporary-conversion-webhook-lifecycle)
29. [Implementation Layout](#29-implementation-layout)
30. [Testing Requirements](#30-testing-requirements)
31. [Definition of Done](#31-definition-of-done)
32. [Open Questions](#32-open-questions)

---

## Phase 0 — External Prerequisites

| Prerequisite                                       | Where it comes from                                  | Specified in |
|----------------------------------------------------|------------------------------------------------------|--------------|
| Kubernetes 1.32 or newer, the OpenShift 4.19 floor | The declared support floor                           | §6           |
| Helm 3                                             | The user's tooling, for chart-installed deployments  | §5, §13      |
| Operator Lifecycle Manager                         | OpenShift, for bundle-installed deployments          | §13.2        |
| cert-manager                                       | Optional, selected by `SB_TLS_PROVIDER=cert-manager` | §8           |
| A reachable simplyblock control plane              | The existing installation                            | §13.3        |

The Kubernetes `StorageVersionMigration` API is deliberately absent from this
table. It is not used at all (§24).

**The floor is OpenShift 4.19, which is Kubernetes 1.32.** It is already
declared, as the OLM bundle's `com.redhat.openshift.versions` from
`OPENSHIFT_VERSION ?= v4.19` in `operator/Makefile`, and every mechanism this
document uses is older than it: webhook conversion is `apiextensions.k8s.io/v1`,
and CRD validation ratcheting is on by default from 1.30 (§19.9). The envtest
assets the unit suites run against are newer, being derived from the
`k8s.io/api` version in `operator/go.mod`, so a unit suite proves nothing about
the floor.

---

## Requirements Marked in the Other Designs

A design that specifies a kind also specifies what moving an existing deployment
onto that kind costs, and the cost is this document's to pay rather than the
kind's. Those obligations are marked where they arise, so that they can be swept
for rather than remembered:

```bash
grep -rn '\*\*Upgrade tool:\*\*' operator/docs/designs/
```

**The marker is the words `Upgrade tool`, in bold, followed by a colon, at the
head of a paragraph.** The paragraph then says what this tool has to do and what
breaks if it does not. The bold is part of the token rather than decoration: a
design that merely mentions the upgrade tool in prose is not stating a
requirement, and a sweep that matched those would return a list nobody trusts.
This paragraph spells the token out rather than showing it, so that defining the
convention does not add a hit to the sweep it defines.

**The sweep is the index, and this document holds no copy of it.** A list here
would be a second place to update and the one that goes stale, since the
requirement is discovered while the kind is being designed and belongs beside
the decision that created it.

**A marked paragraph is a requirement, not a suggestion.** Each names a concrete
failure: an object pruned, a spec defaulted to something immutable and wrong, a
CRD absent when the chart that needs it renders. Where the tool cannot satisfy
one, the answer is to stop the upgrade and say so, which is what §25 is for.

---

## 1. Purpose

This document defines the upgrade and migration architecture for breaking
Kubernetes API changes in the simplyblock operator.

The operator serves one API group, `storage.simplyblock.io`, at one version,
`v1alpha1`, with seventeen kinds registered in `operator/config/crd/bases/`.
`operator/docs/designs/crd-redesign/` specifies a target model that renames
fields, recases enum values, renames kinds, retires kinds, removes a level from
the ownership spine, and adds ten kinds. `design-crd-model.md` §9 is the inventory of
those changes, and §7.2 below decides what each one costs this migration.

The migration must support existing installations without requiring an
intermediate operator release solely to bootstrap API conversion.

The design separates the upgrade into two explicit phases:

1. **`upgrade`** makes the cluster capable of running the new operator
   while leaving the existing resource model intact.
2. **`migrate`** performs the application-level migration of existing
   resources after the user has had an opportunity to verify the new operator.

The first phase is as reversible and non-destructive as it can be made. The
second phase commits the changes to the resource model that cannot be undone.

---

## 2. Design Goals

The implementation MUST:

- Support breaking changes between `storage.simplyblock.io/v1alpha1` and
  `v1alpha2`, and the objects already written at `v1alpha1`.
- Carry them through a CRD conversion webhook where conversion can express them
  (§6.3), served from the operator's image, deployed separately from it, and
  working while the operator is not.
- Reach `v1alpha2` without an intermediate operator release.
- Validate that every object that already exists can be represented under the
  new rules, before anything is deployed or changed (§19).
- Hand every resource the chart installs today to an operator-driven owner
  without deleting and recreating it (§12).
- Change the resource graph itself: fields, ownership, creation, deletion, and
  the persisted storage version.
- Put an explicit user verification point before `migrate`.
- Be idempotent, safely retryable, and closed against state it cannot migrate.
- Allow `v1alpha1` and the conversion webhook to be removed afterward.
- Work for both installation channels, the Helm chart and the OLM bundle (§13).

The implementation SHOULD:

- Provide one read-only command that reports the checks and the plan (§27).
- Validate the whole resource graph, not only the objects it changes.
- Make progress observable.
- Perform changes in a safe dependency order, verifying each one.
- Keep API conversion separate from application-level migration.

---

## 3. Non-Goals

The conversion webhook MUST NOT:

- Perform application-level resource migration.
- Create or delete unrelated Kubernetes resources.
- Change ownership relationships.
- Reconcile resources.
- Contact the simplyblock control plane.
- Perform long-running operations.
- Depend on the operator deployment.
- Perform business-level validation that cannot be represented by API
  conversion.

The conversion webhook is an API representation converter, not a migration
controller.

**The Kubernetes `StorageVersionMigration` API is not used.** §24 states the
constraint.

**The migration does not move data.** No logical volume is copied, re-striped,
or relocated by it, and a volume's identity on the wire, its subsystem NQN, is
composed from UUIDs and does not change (§19.12). Where the target model
reparents or renames the Kubernetes object that describes a volume, the volume
itself is untouched.

Volume handles are the one exception, and they are in scope: the pool segment of
a handle written before the v2 API migration carries a pool name rather than a
UUID, and normalizing those is §16.4. That rewrites an identifier, not data.

---

## 4. High-Level Architecture

The upgrade consists of three distinct mechanisms:

| Mechanism               | Responsibility                                                                                                        |
|-------------------------|-----------------------------------------------------------------------------------------------------------------------|
| Conversion webhook      | Converts API representations between `v1alpha1` and `v1alpha2` for one kind at a time                                 |
| Storage-version rewrite | Reads and writes back every object so the persisted representation becomes `v1alpha2` (§24)                           |
| `migrate` migration     | Performs the application-level migration of the resource model, including everything conversion cannot express (§6.3) |

The overall lifecycle is two phases with a person between them:

```text
  preflight ───────────────────────────────┐  read-only, any time (§27)
                                           │
  ┌───────────────────────────────┐        │
  │ upgrade (§9.1)                │        │
  │   conversion webhook          │        │
  │   new CRDs, both versions     │        │
  │   release handover (§12)      │        │
  │   operator, Helm or OLM       │        │
  └───────────────┬───────────────┘        │
                  ▼                        │
         USER VERIFICATION (§14)           │
                  │                        │
  ┌───────────────▼───────────────┐        │
  │ migrate (§23)                 │◄───────┘
  │   validate                    │
  │   transform, reparent         │
  │   normalize handles (§16.4)   │
  │   delete obsolete             │
  │   rewrite storage (§24)       │
  └──────────────┬────────────────┘
                 ▼
    Remove v1alpha1 and the
    conversion webhook (§28)
```

---

## 5. Installation and Ownership Model

The operator ships through two channels, and both matter to this design.

**The Helm chart** at `helm-charts/charts/simplyblock-operator` is the primary
one. It is a single release that installs the operator deployment
(`templates/simplyblock-operator.yaml`), the CSI controller and node plugins
(`templates/controller.yaml`, `templates/node.yaml`), the storage-node workload,
the control plane, and the admission webhook configurations
(`templates/simplyblock-operator-webhook.yaml`). Its CRDs live in
`charts/simplyblock-operator/crds/`.

**That is the release this upgrade dismantles**, and §12 is how it survives
being dismantled.

**The OLM bundle** is consumed on OpenShift and generated rather than
committed, so `bundle/` is written at release time.

Every CRD in both channels comes from
`operator/config/crd/bases/storage.simplyblock.io_<plural>.yaml`:

```text
operator/api/v1alpha1/*_types.go
        │  make -C operator manifests  (controller-gen)
        ▼
operator/config/crd/bases/
        │
        ├── make -C operator build-installer ──▶ operator/dist/install.yaml
        ├── make helm-sync ────────────────────▶ helm-charts/charts/simplyblock-operator/crds/
        └── make -C operator bundle ───────────▶ operator/bundle/manifests/
```

`.github/workflows/operator_manifests.yaml` re-runs the first two and fails on
any resulting diff, so a `v1alpha2` added to the Go types and not regenerated
does not reach a release. The bundle is the one copy no drift check covers.

**Helm 3 installs CRDs from `crds/` and deliberately never upgrades or deletes
them on `helm upgrade`.** The migration therefore MUST NOT rely on the chart to
introduce `v1alpha2`:

```text
helm install
    └── initial CRD installation only

upgrade
    └── explicit CRD update during upgrade
```

The CRD update SHOULD be a server-side apply through the Kubernetes API rather
than a shell-out to `kubectl` (§29.4). On OLM-managed installations the CRD
update belongs to OLM instead, which changes the shape of the phase (§13.2).

Ownership of the migration-specific resources has to be explicit, because the
conversion webhook deployment, its Service, and its serving-certificate Secret
are created by `upgrade` and not by Helm. A subsequent `helm upgrade`
must not adopt or prune them, so they carry neither the release's labels nor its
`meta.helm.sh/release-name` annotation, and they are removed by
`migrate` instead (§28).

---

## 6. Conversion Webhook

### 6.1 Separate Deployment

The conversion webhook MUST run independently from the operator. A CRD whose
conversion strategy is `Webhook` cannot be read at all while the webhook is
unreachable, and the objects an administrator reads in order to diagnose a
failed operator are simplyblock custom resources.

The two processes use the same image and different entry points:

```text
quay.io/simplyblock-io/simplyblock-operator:<tag>
┌────────────────────────────────────────────────┐
│  /manager             (operator/cmd/main.go)   │
│  /conversion-webhook  (operator/cmd/           │
│                        conversion-webhook)     │
└───────────────┬──────────────────┬─────────────┘
                │                  │
                ▼                  ▼
      Operator Deployment    Conversion Deployment
      (Helm or OLM)          (upgrade)
```

The conversion webhook stays in the operator image so that the conversion code
and the API types are versioned together with the operator that reads them.
§29.3 has the build.

### 6.2 Conversion Webhook Responsibilities

The webhook MUST convert in both directions, statelessly, deterministically, and
performing representation conversion and nothing else. It follows
controller-runtime's hub-and-spoke model: `v1alpha2` implements
`conversion.Hub`, `v1alpha1` implements `conversion.Convertible`'s `ConvertTo`
and `ConvertFrom`, and the handler is registered at `/convert`.

Information that cannot be represented in both versions MUST NOT silently
disappear. Where a field has no counterpart, the conversion preserves it in an
annotation keyed `storage.simplyblock.io/conversion-<field>` on the way down and
restores it on the way up, so a `v1alpha1` client that reads and writes an object
back does not truncate it.

`skipKubeletConfiguration` is the one field whose conversion is not a copy. It
becomes `enableKubeletConfiguration`, which inverts the sense, so a mechanical
rename produces the wrong behavior and the conversion negates the value in both
directions (`design-crd-model.md` §9.6).

### 6.3 What Conversion Can and Cannot Carry

Conversion operates on one CRD, between two versions of the same group and
kind. This boundary decides which of the target model's changes belong to
`upgrade` and which belong to `migrate`, and it is the reason
the upgrade needs two phases rather than a webhook.

| Change                                                                             | Carried by         | Reason                                                                                                                                                                        |
|------------------------------------------------------------------------------------|--------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Boolean toggle renames, eleven fields across five kinds                            | Conversion         | Same kind, both spellings expressible                                                                                                                                         |
| Enum recasing, `StorageClusterOpsAction`, `StorageNodeOpsAction`, `MetricsBackend` | Conversion         | Same kind, value maps one to one                                                                                                                                              |
| `status.subPhase` string becoming `status.step` object                             | Conversion         | The old string reads into `step.state`, leaving `step.deadline` absent, which restores as a step with no deadline, so an operation in flight across the upgrade keeps running |
| `StorageNode.spec.storageNodeSetRef` becoming a cluster reference                  | Conversion, partly | The field converts, but the value it should hold is only known once §20 has reparented the node                                                                               |
| `BackupPolicy` becoming `StorageBackupPolicy`                                      | `migrate`          | A different kind is a different CRD, and no conversion webhook is invoked across kinds                                                                                        |
| `VolumeMigration` absorbed into `PersistentVolumeOps`                              | `migrate`          | Different kind, and the target is cluster-scoped while the source is namespaced                                                                                               |
| `BackupRestore` absorbed into `StorageBackupOps`                                   | `migrate`          | Different kind                                                                                                                                                                |
| `BackupImport` retired                                                             | `migrate`          | Objects are deleted, not converted                                                                                                                                            |
| `StorageNodeSet` retired, children reparented                                      | `migrate`          | Owner references on the DaemonSet, Services, Endpoints, certificates, ServiceAccount, and per-node ConfigMaps are not API representation                                      |
| Twenty-eight annotation and label keys reprefixed                                  | `migrate`          | The keys sit on core objects, including `PersistentVolumeClaim`, which no conversion webhook for this group ever sees                                                         |
| Ten new kinds                                                                      | Neither            | New CRDs are installed, and the objects are created by the reconcilers or by discovery                                                                                        |

**Whatever conversion cannot express losslessly belongs in the
application-level migration**, and nothing belongs in both.

---

## 7. API Version Strategy

### 7.1 Which CRDs Get a `v1alpha2`

**A registered CRD moves to `v1alpha2` when the redesign renames or removes a
property, or when its kind name changes. A CRD that only gains fields stays at
`v1alpha1`.**

A renamed or removed spec property is silently ignored on an object that still
sets the old name, so the two spellings have to coexist behind a conversion, and
a version is what makes them coexist. A new field is absent on every object that
predates it, which `+optional` already covers.

The rule decides the size of every other step here, because a second version
costs a conversion function, a round trip, a `conversion` stanza, a CA-bundle
target, and a pass of the storage rewrite, per CRD. Three consequences follow:

- **A kind rename is a new CRD, and the new CRD is born at `v1alpha2`.** No
  conversion webhook is invoked across kinds (§6.3), so the old CRD keeps its
  single `v1alpha1` and is retired, while the kind that replaces it registers
  `v1alpha2` as its only version. It never has a `v1alpha1`, because no object
  was ever written at one.
- **A status-only change does not need a version**, because the operator is the
  only writer of status. Every kind in the inventory below has spec changes too,
  so this exempts none of them on its own.
- **A retired kind gets no `v1alpha2`.** `StorageNodeSet` is deleted rather than
  converted (§16.1), so it stays at `v1alpha1` until the CRD goes.

### 7.2 The Inventory

Every one of the seventeen registered CRDs, and what the rule decides for it.
The Delta column cites the design that owns the change.

| Kind                | Delta                                                                                                                                                                                             | Versions served        |
|---------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------|
| `StorageCluster`    | `maxHugePagesSize` → `minHugePagesSize`, `hashicorpVaultSettings` → `kms.vault`, six toggles renamed, `backup.localEndpoint` → `endpoint`, and five spec removals. `design-storagecluster.md` §12 | `v1alpha1`, `v1alpha2` |
| `StorageClusterOps` | `nodeRollingRestart` → `rollingRestart`, six action values recased, `status.triggered` removed. `design-storagecluster.md` §12                                                                    | `v1alpha1`, `v1alpha2` |
| `StorageNode`       | `storageNodeSetRef` → `clusterRef` and `nodeSet`, `overrides` → `config`, `socketIndex` → `slot`, `skipKubeletConfiguration` inverted, four dead fields removed. `design-storagenode.md` §15.1    | `v1alpha1`, `v1alpha2` |
| `StorageNodeOps`    | `storageNodeRef` → `nodeRef`, `drain` → `remove`, six action values recased, `status.triggered` removed. `design-storagenode.md` §15.2                                                            | `v1alpha1`, `v1alpha2` |
| `StoragePool`       | `clusterName` → `clusterRef`, `dhchap` → `volumeDefaults.enableDHCHAP`, `encryption` and `replicate` renamed, `spec.action` and `spec.status` removed. `design-storagepool.md` §11                | `v1alpha1`, `v1alpha2` |
| `ControlPlane`      | `spec.image` → `spec.source.managed.image`, and `status.phase` becomes a four-value typed phase. `design-controlplane.md` §11                                                                     | `v1alpha1`, `v1alpha2` |
| `StorageBackup`     | `clusterName` → `clusterRef`, and twenty-two flat status fields regrouped. `design-storagebackup.md` §13                                                                                          | `v1alpha1`, `v1alpha2` |
| `StorageNodeSet`    | Retired. `design-storagenode.md` §15.3                                                                                                                                                            | `v1alpha1`, then gone  |
| `BackupPolicy`      | Renamed to `StorageBackupPolicy`. `design-storagebackup.md` §13                                                                                                                                   | `v1alpha1`, then gone  |
| `BackupRestore`     | Absorbed into `StorageBackupOps` as `action: Restore`. `design-storagebackup.md` §13                                                                                                              | `v1alpha1`, then gone  |
| `BackupImport`      | Retired. `design-storagebackup.md` §13                                                                                                                                                            | `v1alpha1`, then gone  |
| `VolumeMigration`   | Absorbed into `PersistentVolumeOps`, which is also cluster-scoped. `design-persistentvolumeops.md` §10                                                                                            | `v1alpha1`, then gone  |
| `Task`              | Not reworked, and what becomes of the kind is undecided. `design-crd-model.md` §9.1                                                                                                               | `v1alpha1` only        |
| `ReplicationPolicy` | Out of scope of the redesign. `design-crd-model.md` §7.1                                                                                                                                          | `v1alpha1` only        |
| `ReplicationPair`   | Out of scope of the redesign                                                                                                                                                                      | `v1alpha1` only        |
| `ReplicationSlot`   | Out of scope of the redesign                                                                                                                                                                      | `v1alpha1` only        |
| `ReplicationOps`    | Out of scope of the redesign                                                                                                                                                                      | `v1alpha1` only        |

**Seven CRDs carry two versions. Five keep one and are then removed. Five keep
one and are untouched.** So the conversion webhook serves seven kinds, seven
CRDs take the `conversion` stanza and the CA bundle (§8), and the storage
rewrite of §24 covers seven kinds rather than seventeen.

The five untouched kinds are the reason the rule is worth having. `Task` and the
four replication kinds are outside the redesign, so `v1alpha1` still describes
them exactly, and every version this migration does not add is a conversion
function, a served version, and a rewrite pass it does not have to carry.

### 7.3 The Kinds Born at `v1alpha2`

Eleven CRDs are created by this migration rather than converted, and each
registers `v1alpha2` as its only version: the ten additions of
`design-crd-model.md` §7.2, which are `OperatorOps`, `ClusterDeploymentConfig`,
`ControlPlaneOps`, `SimplyblockDriver`, `StorageDevice`, `StorageDeviceOps`,
`StoragePoolOps`, `PersistentVolumeOps`, `StorageBackupOps`, and `NFSExport`,
plus `StorageBackupPolicy`, which is `BackupPolicy` under its new name.

None of them needs a conversion function, and none appears in the storage
rewrite, because nothing was ever persisted at an older version of them.

**They ship embedded in the installer**, along with the seven converting CRDs
(§11), rather than being fetched from a release tag at run time. So a CRD is
installed before the controller that reconciles it exists, which is inert: a
registered kind with no controller and no objects does nothing until the
operator carrying its controller is running.

### 7.4 The Staging

Each of the seven converting CRDs is installed with both versions served and
`v1alpha1` retained as the storage version:

```yaml
# operator/config/crd/bases/storage.simplyblock.io_storageclusters.yaml
spec:
  group: storage.simplyblock.io
  names:
    kind: StorageCluster
    plural: storageclusters
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
    - name: v1alpha2
      served: true
      storage: false
  conversion:
    strategy: Webhook
    webhook:
      conversionReviewVersions: ["v1"]
      clientConfig:
        service:
          namespace: simplyblock
          name: simplyblock-operator-conversion-webhook-service
          path: /convert
          port: 443
```

The `conversion` stanza is not written by hand. `operator/config/crd/kustomization.yaml`
carries the `+kubebuilder:scaffold:crdkustomizewebhookpatch` marker and a
commented `patches` block, and `operator/config/default/kustomization.yaml`
carries `+kubebuilder:scaffold:crdkustomizecainjectionns` and
`crdkustomizecainjectionname` with a comment stating that the markers exist so
`kubebuilder create webhook --conversion` can wire up a future conversion
webhook. That scaffold is the intended entry point, and the one deviation from
what it generates is the CA bundle: the scaffold injects it with cert-manager's
`cert-manager.io/inject-ca-from` annotation, and this repository provisions
webhook certificates at runtime instead (§8).

**The patch is applied per CRD and not to the whole `crd/bases` directory.** Ten
of the seventeen keep `strategy: None`, and a `conversion` stanza pointing at a
webhook on a CRD with one version is a dependency on a Deployment that has no
reason to exist for that kind, which is what §27 eventually removes.

The staging exists so that five things can fail separately:

1. Introducing the new API.
2. Proving conversion works.
3. Upgrading the operator.
4. Migrating the application resource model.
5. Changing the persisted storage representation.

Once the new operator is verified and the application-level migration has run,
storage switches on those same seven:

```yaml
versions:
  - name: v1alpha1
    served: true
    storage: false
  - name: v1alpha2
    served: true
    storage: true
```

`v1alpha1` becomes `served: false` on the seven under §28's conditions. Its
readers are the operator's own reconcilers and webhooks under
`operator/internal/`, the only consumers of `operator/api/v1alpha1` in this
repository, plus the chart's `operator_customresources.yaml`, the
`test-cluster*.yaml` manifests, `operator/test/utils/`, and whatever a user
keeps in their own repository. The CSI driver imports no API types.

The five untouched kinds never stop serving `v1alpha1`, so the group serves both
versions for as long as `Task` and the replication kinds are registered.

---

## 8. TLS and Conversion Webhook Bootstrap

The conversion webhook requires TLS, and the bootstrap MUST NOT introduce this
cycle:

```text
CRD conversion
    ↓
conversion webhook
    ↓
certificate provisioning
    ↓
Kubernetes API operation on a simplyblock CR
    ↓
CRD conversion
```

The mechanism this repository already uses breaks the cycle by construction, and
the conversion webhook reuses it. `operator/internal/webhook/cert.go` provisions
the operator's own serving certificate at runtime and returns a channel that
closes once the certificate is on disk at `utils.WebhookCertDir`
(`/tmp/k8s-webhook-server/serving-certs`) and the CA bundle has been injected
into the webhook configurations. `operator/cmd/main.go` waits on that channel
before it registers any handler, because controller-runtime's certwatcher fails
on a missing file. Two providers sit behind it, selected by `SB_TLS_PROVIDER`:

| Provider       | Mechanism                                                                         | Where                             |
|----------------|-----------------------------------------------------------------------------------|-----------------------------------|
| Default        | Self-signed, generated and rotated by `open-policy-agent/cert-controller` v0.15.0 | `internal/webhook/cert.go`        |
| `cert-manager` | A `Certificate` the operator creates, whose Secret cert-manager fills in          | `internal/webhook/certmanager.go` |

The conversion webhook process runs its own instance of the same code, with two
differences.

**Its CA-injection targets are CRDs rather than webhook configurations.**
`rotator.WebhookInfo` accepts `rotator.CRDConversion` alongside
`rotator.Mutating` and `rotator.Validating`, and cert-controller maps it to
`apiextensions.k8s.io/v1` `CustomResourceDefinition`, writing the bundle into
`.spec.conversion.webhook.clientConfig.caBundle`. The seven converting CRDs of
§7.2 are listed and no others, because a CRD with one version has no
`clientConfig` to inject into. The rotator's RBAC therefore grows a
`customresourcedefinitions` `get;list;watch;update;patch` grant that the
operator's ClusterRole does not carry today, and it can be narrowed to those
seven by `resourceNames`.

**Its Secret, Service, and DNS name are its own.** The operator's rotator
pre-creates and owns `webhook-server-cert` (`internal/webhook/cert.go`), so a
webhook that waits on that Secret waits on the operator having started, which is
what §6.1 exists to prevent.

`upgrade` waits until:

1. TLS material exists in the conversion webhook's Secret.
2. The conversion webhook Pod is ready.
3. The Service has ready endpoints.
4. Every CRD's `caBundle` is populated.
5. The webhook is accepting requests.
6. A conversion smoke test succeeds (§10).

A self-signed CA is acceptable for a webhook whose lifetime is one upgrade. What
matters is that certificate bootstrap does not itself depend on reading a
simplyblock custom resource.

---

## 9. `upgrade`

The first phase is:

```text
simplyblock-upgrade upgrade
```

Its purpose is:

> Make the cluster capable of running the new operator without performing the
> application-level resource migration.

It MUST NOT perform destructive changes to the existing resource topology.

### 9.1 Sequence

```text
 1. Validate prerequisites
 2. Deploy the conversion webhook
 3. Wait for TLS material
 4. Wait for webhook readiness
 5. Smoke-test conversion
 6. Apply the updated CRDs
 7. Verify the version set on every CRD the apply touched (§7.2)
 8. Re-test conversion
 9. Hand the release over, so the upgrade prunes nothing
10. Upgrade the operator (Helm or OLM)
11. Wait for operator readiness
12. Run operator and API smoke tests
13. Report a successful upgrade
```

### 9.2 Prerequisite Validation

The installer SHOULD verify:

- Kubernetes 1.32 or newer, which is the OpenShift 4.19 floor.
- The `storage.simplyblock.io` CRDs exist and are established.
- The existing custom resources can be listed in every namespace.
- The installation channel, by looking for a Helm release secret or an OLM
  `Subscription` and `ClusterServiceVersion` (§13).
- The operator's RBAC covers what the migration performs.
- The target operator image is pullable.
- The operator namespace exists.
- No incompatible migration is already in progress.
- The control plane is reachable and reports the cluster healthy, because a
  cluster that is already degraded should not be asked to absorb an upgrade.
- Every existing object satisfies the constraints the new API adds, and every
  name and label the new model derives is unique and within the limit that binds
  it (§19).

If prerequisites fail, the operation terminates before modifying the cluster.

**The name check is the one that can send a user away for a week.** Its
remediation is sometimes a data migration rather than an edit (§19.11), so it
runs here, before the conversion webhook is deployed and while the cluster is
still untouched.

---

## 10. Conversion Smoke Test

Pod readiness does not prove that conversion works. The installer MUST perform
an actual conversion.

The test uses an existing custom resource, read through the new version:

```text
existing StorageCluster/simplyblock-cluster (v1alpha1)
        │
        ▼
GET storage.simplyblock.io/v1alpha2 StorageCluster
        │
        ▼
conversion webhook
        │
        ▼
verify the response
```

The smoke test verifies that:

- The resource can be retrieved through `v1alpha2`.
- The conversion webhook is reachable.
- Conversion succeeds for each of the seven converting kinds that has at least
  one object (§7.2).
- The fields whose conversion is not a copy are correct, which means at minimum
  a recased action enum, a renamed boolean toggle, and the.
  `skipKubeletConfiguration` inversion (§6.2).
- A `v1alpha2` read followed by a `v1alpha1` read returns the original
  representation.

A read is a conversion, so the test needs no writes and does not mutate the
user's resources. Where a kind has no objects, conversion is exercised against a
synthetic object in a scratch namespace that is deleted afterward.

---

## 11. CRD Installation During `upgrade`

After the conversion webhook is proven operational, the installer applies the
new CRDs. The apply set has three parts, and §7.2 decides which CRD is in which:

```text
seven converting CRDs   v1alpha1: served=true,  storage=true
                        v1alpha2: served=true,  storage=false
                        conversion: strategy=Webhook

eleven new CRDs         v1alpha2: served=true,  storage=true
                        conversion: strategy=None

ten untouched CRDs      not applied at all
```

Eighteen CRDs are written, then. The ten that are not are the five kinds §7.2
leaves at `v1alpha1` and the five whose CRDs are retired later rather than
changed now, and applying a CRD whose content is byte-identical to what is
installed is a write that can only introduce risk.

The whole set is embedded in the installer binary so that the version of the
CRDs always matches the version of the conversion code that converts them.

The installer MUST wait until each applied CRD is established, which means
checking `.status.conditions` for `Established` and `NamesAccepted` and
confirming `.spec.versions` and `.status.storedVersions` hold what the phase
expects for that CRD's part of the set. A CRD in the second group is verified
against a different expectation from one in the first, and checking every CRD
against "both versions are served" is how a new kind reports a false failure.

The installer MUST NOT proceed to the operator upgrade if the API server has not
accepted every new CRD. A partially applied CRD set is the state that leaves the
operator reconciling one kind at `v1alpha2` and another at `v1alpha1`.

---

## 12. Release Handover

The chart release that carries the new operator carries the operator and nothing
else. Everything else the chart installs today becomes operator-driven through a
custom resource: the control plane's workload, its `FoundationDBCluster`, its
Services, and its certificates become children of `ControlPlane`
([`design-controlplane.md`](design-controlplane.md) §5.1),
and the CSI node `DaemonSet`, the controller `StatefulSet`, their RBAC, the node
`ConfigMap`, and the `CSIDriver` registration become children of
`SimplyblockDriver`
([`design-simplyblockdriver.md`](design-simplyblockdriver.md) §4.1).
Both designs make the same point about why: the ownership spine then starts at a
real edge rather than at a Helm release.

**Helm deletes what a release used to contain and no longer does.** So the
upgrade that installs the operator-only chart is also the upgrade that deletes
the running data plane, and this step is what stops it. It runs before
`helm upgrade` (§13.1) and it is the reason `upgrade`'s sequence requires a
step between installing the CRDs and upgrading the operator.

### 12.1 What the Release Owns Today

The current chart renders 110 objects at default values, and fifteen of them are
the operator and its RBAC. The other ninety-five are the release's blast
radius:

| Group               | Objects                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Adopted by                                              |
|---------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------|
| Control plane       | `FoundationDBCluster/simplyblock-fdb-cluster`, the `simplyblock-webappapi`, `admin-control`, `monitoring`, `tasks`, `fdb-controller-manager`, and `fdb-exporter` Deployments, `StatefulSet/simplyblock-minio`, their Services, and the `simplyblock-config`, `prometheus-config`, and `objstore-config` ConfigMaps                                                                                                                                                                             | `ControlPlane`, `design-controlplane.md` §5.1           |
| CSI driver          | `CSIDriver/csi.simplyblock.io`, `StatefulSet/simplyblock-csi-controller`, `DaemonSet/simplyblock-csi-node`, their two ServiceAccounts and five ClusterRole and ClusterRoleBinding pairs, the `simplyblock-csi-cm` and `simplyblock-csi-nodeservercm` ConfigMaps, and `VolumeSnapshotClass/simplyblock-csi-snapshotclass`. `simplyblock-csi-secret-v2` is not in the set: the StorageCluster reconciler writes it and the driver only mounts it. `simplyblock-csi-secret` is mounted by nothing | `SimplyblockDriver`, `design-simplyblockdriver.md` §4.3 |
| The adopter itself  | `ControlPlane/simplyblock`, which the chart renders from `templates/controlplane_cr.yaml`                                                                                                                                                                                                                                                                                                                                                                                                      | Nothing. It is the adopter (§12.2)                      |
| Ecosystem subcharts | The Prometheus `StatefulSet`, its two Services and its ConfigMap, `Deployment/simplyblock-reloader`, and the MongoDB and OpenSearch releases where `controlplane.observability.enabled` is set                                                                                                                                                                                                                                                                                                 | Undecided (§32, Q5)                                     |
| Unattributed        | `StorageClass/local-hostpath`, `DaemonSet/simplyblock-numa-resource-plugin` and its ConfigMap, and `ConfigMap/simplyblock-caching-node-restart-script-cm`                                                                                                                                                                                                                                                                                                                                      | Undecided (§32, Q5)                                     |
| Already kept        | The three `snapshot.storage.k8s.io` CRDs and `Deployment/simplyblock-snapshot-controller`                                                                                                                                                                                                                                                                                                                                                                                                      | Helm already leaves them (§12.2)                        |
| The operator        | `Deployment/simplyblock-operator`, its webhook Service and two webhook configurations, its metrics Service and `APIService`, and its RBAC                                                                                                                                                                                                                                                                                                                                                      | The new chart renders these                             |

**The set is per-cluster, so it is computed and never read from a table.** Two
subcharts are conditional on values, and a cluster with observability enabled
has a MongoDB and an OpenSearch release inside the same Helm release. The table
above is what the defaults produce, and the step derives the real one from the
release that is actually deployed.

Four of the prunes would end the upgrade rather than degrade the cluster:

- `Deployment/simplyblock-webappapi` is the control-plane API the operator calls
  for everything, the rest of this upgrade included.
- `FoundationDBCluster/simplyblock-fdb-cluster` is the control plane's database.
  Deleting the object hands its teardown to the FoundationDB operator, which is
  itself in the prune set.
- `CSIDriver/csi.simplyblock.io` and `DaemonSet/simplyblock-csi-node` are what
  give the cluster the ability to attach, detach, and unmount a volume.
  Workloads keep running on volumes that nothing can then release.
- `ControlPlane/simplyblock` is the custom resource whose reconciler is supposed
  to adopt the first group in this table.

### 12.2 The Mechanism

`helm.sh/resource-policy: keep` on a resource makes Helm skip its deletion, and
**the annotation is read from the live object rather than from the stored release
manifest.** Helm's prune loop walks the difference between the old release's
resources and the new one's, issues a live `Get` on each candidate, and skips
whatever comes back carrying the annotation (`pkg/kube/client.go`). Annotating
the objects in the cluster is therefore enough, and nothing has to rewrite the
release.

This chart already relies on that mechanism. The three `snapshot.storage.k8s.io`
CRDs and the snapshot controller carry the annotation in their own templates,
for the same reason on a smaller scale: deleting a CRD deletes every object of
that kind.

So the step renders the deployed release and the new chart, differences them,
and annotates every object in that difference that has to survive.

**An object in the difference that nobody classified stops the upgrade.**
Neither disposition is safe as a default. Keeping everything leaves orphans that
no controller reconciles and no uninstall removes, and pruning by default deletes
a running storage system. So the classification is explicit, and a template
somebody adds to the chart without deciding its disposition fails the step
rather than silently taking the wrong branch.

### 12.3 Annotating Is Not Adopting

The annotation buys a window, and the window is a cluster whose data plane
belongs to nobody: Helm has stopped tracking these objects, the operator does
not own them yet, and nothing reconciles them. It closes when the new operator
reconciles `ControlPlane` and `SimplyblockDriver` for the first time.

Three properties make that reconcile a handover instead of a rebuild.

**Adoption is in place.** The operator takes the existing object over rather than
deleting and recreating it. Recreating the `FoundationDBCluster` rebuilds the
control plane's database, and recreating the node `DaemonSet` restarts every
node plugin in the cluster at once.

**Field ownership is taken deliberately.** Helm wrote these objects under its own
field manager, and an adopting controller that writes them under another one
meets whatever Helm recorded. The controller takes ownership explicitly, and the
Helm metadata that is left over, `app.kubernetes.io/managed-by: Helm` and the
`meta.helm.sh/release-name` and `meta.helm.sh/release-namespace` annotations, is
removed once adoption is verified, so that nothing later reads it as a claim.

**The spec the operator reconciles toward is the state that is running.** These
objects were rendered from Helm values, and they are about to be rendered from a
custom resource's spec instead. A field the values-to-spec translation of §13.1
gets wrong does not surface as a failed translation: it surfaces as the first
reconcile after adoption reconfiguring a running control plane. So the step
captures every survivor as it stands before the upgrade, and the installer diffs
the operator's output against that capture afterward. A difference is a failure
of the step, not a change to accept.

### 12.4 The Sequence

```text
 1. Render the deployed release and the new chart
 2. Difference the two manifests
 3. Classify every object in the difference, and refuse on anything unclassified
 4. Capture each survivor as it currently stands
 5. Annotate each survivor with helm.sh/resource-policy: keep
 6. Verify the annotation is live on every one of them
 7. helm upgrade                                        (§13.1)
 8. Verify every survivor still exists, and is unchanged
 9. Wait for the new operator, and for it to adopt      (§13.3)
10. Verify adoption: owner references are in place, and nothing drifted
    against the capture from step 4
11. Remove the Helm metadata from the adopted objects
```

Steps 6 and 8 are not ceremony. Step 6 is what turns a wrong assumption about
where Helm reads the annotation into a failure before the upgrade rather than
after it, and step 8 is the only check that would catch a prune the
classification missed while the objects can still be restored from the release
that created them.

### 12.5 What Changes for the User Afterward

`helm uninstall` stops being a full teardown. It removes the operator, and the
control plane, the CSI driver, and the storage cluster keep running, because they
are the operator's now and a custom resource is what removes them. That is the
ownership this migration exists to establish, and it inverts what the command
has meant until now, so `upgrade` says so in its closing report.

`helm rollback` changes meaning too, and §26 has it.

---

## 13. Operator Upgrade

### 13.1 Helm-Installed Deployments

```text
helm upgrade <release> helm-charts/charts/simplyblock-operator
```

The chart carries the same field names the API does, so it moves with the API.
`values.yaml` holds `skipKubeletConfiguration` under the storage-node settings,
and `multiCluster.enable` is a spelling that exists only there. Two consequences
follow.

**A user's existing values file may not validate against the new chart.**
`helm-charts/charts/simplyblock-operator/values.schema.json` sets
`additionalProperties: false` on several objects, so a renamed key is a failed
upgrade rather than a silently ignored one. That is the better failure, and it
means the installer translates the recorded values before it upgrades: it reads
the values of the deployed release, rewrites the keys the release renames, and
passes the result to the upgrade.

**The chart's CRDs are not what installed `v1alpha2`.** §11 did that. The chart
carries the same CRDs so that a fresh install of the new chart is correct, and
`helm upgrade` ignores them.

The installer waits for the operator deployment to become ready.

### 13.2 OLM-Installed Deployments

On OpenShift the operator is upgraded by OLM through its `Subscription`, and OLM
owns the CRDs in the bundle. The installer does not run `helm upgrade` there,
and it must not apply CRDs OLM considers its own, because OLM reconciles them
back.

The phase therefore inverts: the installer deploys the conversion webhook and
proves it works, then the user approves the `InstallPlan`, and the installer
verifies afterward that the CRDs OLM applied carry both versions and the
conversion stanza. Whether the bundle can declare the conversion webhook at all,
given that its deployment is not part of the bundle, is unsettled (§32, Q1).

### 13.3 Operator Smoke Test

Once the operator is ready it is exercised through the new version:

```text
create, read, and update a v1alpha2 resource
```

The specific test matters less than that it covers a write, because a read
proves conversion and a write proves that admission accepts a `v1alpha2` object.
Two of the five handlers registered in `operator/cmd/main.go` are pathed by
version (`/validate-storage-simplyblock-io-v1alpha1-storagenode` and
`/validate-storage-simplyblock-io-v1alpha1-replicationops`), and the rules
generated for them in `operator/config/webhook/manifests.yaml` match
`apiVersions: [v1alpha1]` alone. `matchPolicy` is unset and therefore
`Equivalent`, so the API server converts a `v1alpha2` write down to `v1alpha1`
and calls the webhook anyway. Admission of a `v1alpha2` object therefore runs
through the conversion webhook, and the write is what exercises that path.

At this point:

> The new operator is running, and the old application-level resource model is
> still intact.

---

## 14. User Verification Point

`upgrade` terminates successfully before any application-level migration
begins, and the user inspects the cluster:

```text
Operator version:      <tag>
API v1alpha1:          served, storage
API v1alpha2:          served
Conversion webhook:    healthy
Operator:              healthy
Application state:     not yet migrated
```

The user can now verify operator health, control-plane health, the existing
`StorageCluster`, `StorageNodeSet`, and `StorageNode` objects, workload
behavior, metrics, logs, and application functionality.

Only after the user is satisfied is `migrate` executed.

---

## 15. `migrate`

The second phase is:

```text
simplyblock-upgrade migrate
```

Its purpose is:

> Perform the actual application-level migration from the old resource model to
> the new resource model.

Unlike API conversion, these operations change the semantic resource model.
§16 is what they are, and §23 is the graph that walks them.

---

## 16. Application-Level Resource Migration

### 16.1 The Ownership Spine

The change that defines this phase is the retirement of `StorageNodeSet`, which
takes a level out of the spine and turns the fleet template into
`ClusterDeploymentConfig.nodeSets[]` (`design-clusterdeploymentconfig.md`):

```text
StorageCluster                 StorageCluster
      │                              ├── StorageNode A
StorageNodeSet            →          ├── StorageNode B
      ├── StorageNode A              └── StorageNode C
      ├── StorageNode B
      └── StorageNode C
```

No CRD conversion can express this. `StorageNodeSet` owns its `StorageNode`
objects by controller reference, which is the one real ownership edge in the
spine (`design-crd-model.md` §9.3), and it also owns the workload that runs the
storage nodes: the DaemonSet, the storage-node Services and their Endpoints, the
certificates, the ServiceAccount, and the per-node ConfigMaps. All of them
become children of `StorageCluster`. Until that reparenting is done the
retirement cannot proceed, because deleting a `StorageNodeSet` today is what
tears those objects down.

`StorageNodeSet.spec.nodeConfigs[workerNode]` is the source of truth for
per-node configuration today, written into each `StorageNode` by the set's
controller. The migration reads it, and the deployment config it produces
carries the worker list, the management and data interfaces, and the devices,
while the sizing moves to `spec.cluster` because it is uniform across a cluster.

`StorageNodeSet.status.drainCoordination` carries an eight-phase workflow driven
by a controller of its own, and it becomes a `StorageNodeOps` action
(`design-storagenode.md` §10). A drain that is in progress when
`migrate` starts is a state the migration refuses to migrate, and it is
a validation failure rather than something to translate (§18).

### 16.2 Renamed and Absorbed Kinds

Four kinds change identity, and each is a copy-then-delete rather than a
conversion:

| From              | To                                      | Objects                                                                                                                                 |
|-------------------|-----------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------|
| `BackupPolicy`    | `StorageBackupPolicy`                   | User-authored, so every object is copied                                                                                                |
| `VolumeMigration` | `PersistentVolumeOps`, action `Migrate` | Namespaced to cluster-scoped, so the fan-out's owner reference becomes `spec.creatorRef`, a `managed-by` label, and a finalizer cascade |
| `BackupRestore`   | `StorageBackupOps`, action `Restore`    | The spec becomes the action's parameters                                                                                                |
| `BackupImport`    | Retired                                 | Deleted, because the store is the inventory                                                                                             |

A `VolumeMigration` or `BackupRestore` that is still running is not copied
mid-flight. The migration refuses to start while either has an incomplete
object, for the same reason it refuses on an in-progress drain.

### 16.3 Annotation and Label Keys

Twenty-eight keys move from the bare `simplyblock.io` prefix to
`storage.simplyblock.io` (`design-crd-model.md` §9.4). This is the quietest
break in the whole migration: no API server rejects the old key, so a
`PersistentVolumeClaim` annotated `simplyblock.io/backup-policy` simply stops
having a backup policy, and nothing reports that it used to.

Two properties make this a `migrate` concern rather than a conversion
concern. The keys sit on core Kubernetes objects that no conversion webhook for
this group is invoked for, and the operator has to read both spellings for a
release regardless, which means the rewrite is a convenience rather than a
correctness requirement. So the migration rewrites every key it finds, on both
custom and core objects, writing the new key, preserving the value verbatim, and
leaving the old key in place for the deprecation window rather than deleting it.

The old keys are removed by a later release, after the operator has stopped
reading them.

### 16.4 Volume Handles

A CSI volume handle is `clusterID:poolID:volumeID`, and the middle segment is not
always an id. Volumes provisioned before the v2 API migration encode the **pool's
name** there, so a cluster upgraded across that boundary holds both spellings.
`atlas-lib/lvol` records this in the type that reads them: `ParseHandle` requires
the cluster and volume segments to be canonical UUIDs and accepts anything
non-empty as the pool, while `Split`, which returns three typed UUIDs, rejects a
legacy handle outright.

Snapshots carry the same shape. The CSI controller composes a snapshot id as
`clusterID:poolID:snapshotUUID`
(`csi-driver/internal/csi/controller/snapshot.go`), so a `VolumeSnapshotContent`
written before the boundary carries a pool name too.

**`migrate` normalizes every handle it can write, and replaces no object to
reach one it cannot.** The API server draws that boundary:

```text
PersistentVolume.spec.csi.volumeHandle       immutable
VolumeSnapshotContent.spec.source            immutable
the annotations on both of those             writable
records the operator writes                  writable
the control plane's own pool reference       writable
```

`ValidatePersistentVolumeUpdate` rejects any change to
`spec.persistentVolumeSource`, the CSI source included, with
`spec.persistentvolumesource is immutable after creation`. So a bound volume's
handle is the spelling it was provisioned with for the life of the object, so
the phase writes the rows below that line instead.

**Resolve and report.** Every `PersistentVolume` and `VolumeSnapshotContent`
whose pool segment is not a canonical UUID is listed with the name it carries and
the UUID that name resolves to, through `lvol.Resolver` against the control
plane. A pool name resolving to nothing is a finding: the handle names a pool
that no longer exists, and the migration reports it and does not proceed. This
part is a read, so it belongs to `preflight` (§19.10) and runs long before the
migration does.

**Rewrite the records the migration owns.** Anywhere the operator has written a
handle into a field it controls, a custom resource's status or a `ConfigMap`, the
normalized form replaces it. The set is empty today, and the rule is stated so
that a field carrying a handle is normalized with the rest of them whenever one
is added.

**Record the normalized handle in an annotation on the objects whose field is
immutable.** Metadata is writable where `spec` is not, so the resolved handle is
written to the `PersistentVolume` and the `VolumeSnapshotContent` as:

```yaml
metadata:
  annotations:
    storage.simplyblock.io/volume-handle: <clusterUUID>:<poolUUID>:<volumeUUID>
```

The field keeps the spelling it was provisioned with, and the annotation carries
the identity every reader wants. It is an annotation rather than a label because
a handle is 110 bytes and a label value stops at 63 (§19.1), which is also why
the declared and unused `kube.LabelVolumeHandle` cannot become this and is
replaced by an annotation constant beside it.

**A reader takes the annotation when it is present and consistent, and the field
otherwise.** Consistency is exact: the annotation's cluster and volume segments
MUST equal the field's, and only the pool segment may differ. A reader finding
any other difference ignores the annotation and reports it, so a hand-edited
annotation cannot redirect a volume to another cluster. That rule is one
function in `atlas-lib`, beside `ParseHandle`, and no call site implements it
twice.

**This is what makes the normalized pool reachable without the control plane.**
The CSI driver already indexes `PersistentVolume` objects by the lvol id in
their handle (`indexPersistentVolumeByLvolID`,
`csi-driver/internal/kubernetes/persistent_volumes.go`), and the volume segment
is a canonical UUID in both spellings, so that index finds the object carrying
the annotation. The pool name is therefore resolved once, by `migrate`, rather
than on every read.

`ParseHandle` stays tolerant for the objects that have no annotation: one a
GitOps repository recreates, one restored from a backup another cluster wrote,
and every object in a cluster nobody has migrated.

---

## 17. Discover

The migration lists every resource relevant to it and builds an explicit graph,
rather than processing arbitrary objects as it encounters them. For the
`StorageNodeSet` retirement that is:

```text
StorageClusters
StorageNodeSets
StorageNodes
the DaemonSet, Services, Endpoints, Secrets, ServiceAccounts, and ConfigMaps
    each StorageNodeSet owns
```

producing:

```text
StorageCluster cluster-a
    └── StorageNodeSet nodeset-a
          ├── StorageNode node-a
          ├── StorageNode node-b
          └── StorageNode node-c
```

Discovery is namespace-wide rather than cluster-wide by default, because every
kind in the group except the cluster-scoped additions is `Namespaced` and a
cluster may hold several independent installations.

---

## 18. Validate

Before modifying resources, the migration MUST validate the complete graph:

```text
✓ StorageNodeSet nodeset-a belongs to StorageCluster cluster-a
✓ StorageNode node-a belongs to nodeset-a
✓ StorageNode node-b belongs to nodeset-a
✓ StorageNode node-c belongs to nodeset-a
✓ every referenced resource exists
✓ no unexpected owners
✓ no duplicate relationships
✓ every target resource can be represented
✓ no StorageNodeSet is mid-drain
✓ no StorageClusterOps, StorageNodeOps, VolumeMigration, or BackupRestore is
  in flight
✓ every StorageNode is online, or explicitly acknowledged as offline
```

If validation fails, the migration stops without making changes and reports
enough for the user to correct the problem and rerun.

The in-flight checks are the ones that matter most. An operation the operator is
partway through is state that lives in an object this migration is about to
rewrite, and finishing it takes minutes while migrating it takes a design.

The name and identity checks of §19.10 run here too.

---

## 19. Name and Identity Validation

The operator derives Kubernetes object names and label values from user-chosen
resource names, and the target model changes what several of them are derived
from. Both halves of that sentence are validation problems, and neither is
checked: nothing in this repository validates a label or a name before writing
it, and no field in the seventeen registered kinds carries a `MaxLength` marker.

**An overflow becomes a reconcile that retries forever.** The API server
refuses the derived object or the label patch, the reconciler requeues, and the
resource it was reconciling reports nothing about the name that caused it. That
is why these rules are written before the upgrade rather than after the first
support case.

The thresholds below are measured rather than reasoned, using
`k8s.io/apimachinery/pkg/util/validation` (`IsQualifiedName`,
`IsValidLabelValue`, and `IsDNS1123Subdomain`) against
`k8s.io/kubernetes@v1.36.2`. The full audit is recorded in
[the name-limit findings](https://claude.ai/code/artifact/032166a1-2007-4c17-883f-cc1fa6358d21).

### 19.1 The Limits, and Which One Binds

Two limits matter. **63 bytes** applies to a label value and to the name part of
a label key, after the slash. **253 bytes** applies to an object name, custom
resources and `ConfigMap`, `Secret`, `DaemonSet`, and `StorageClass` alike.

**Where a name is copied into a label, the smaller limit is the one that binds.**
A `Job`'s name is copied into its pods as `batch.kubernetes.io/job-name`, so a
`Job` name is held to 63 and not to 253, and a name that satisfies only the
larger limit is refused outright. `nodeprobe.ObjectName` states this in its own
constants and is the reason it budgets 63 for a `ConfigMap` that could have had
253.

So a limit is derived per name from every place the name travels to, and not
from the object it is written on.

### 19.2 The Label Cases

Eight labels are built from a name a user chose. Every row is live today.

| What is built                                                      | Breaks when                                                                    | Longest input that works    | Fix               |
|--------------------------------------------------------------------|--------------------------------------------------------------------------------|-----------------------------|-------------------|
| `simplyblock.io/pool.<ns>.<cluster>.<pool>`, a key                 | The namespace, cluster, and pool names together exceed 56 characters           | A 27-character pool name    | Truncate and hash |
| `io.simplyblock.node-type` = `simplyblock-storage-plane-<cluster>` | The cluster name exceeds 37 characters, or ends in `-`, `.`, or `_`            | A 37-character cluster name | Bound the input   |
| `storage.simplyblock.io/cluster` on a `StorageClass`               | The cluster name exceeds 63 characters                                         | A 63-character cluster name | Use a UUID        |
| `storage.simplyblock.io/pool` on a `StorageClass`                  | The `StoragePool` name exceeds 63 characters                                   | A 63-character pool name    | Use a UUID        |
| `io.simplyblock.storagenodeset`                                    | The `StorageNodeSet` name exceeds 63 characters                                | A 63-character set name     | Bound the input   |
| `storage.simplyblock.io/worker`                                    | The `Node` name exceeds 63 characters                                          | A 63-character node name    | Truncate and hash |
| `simplyblock.io/drain-node`                                        | Character 63 is `-` or `.`, which a label value may not end on                 | A 62-character node name    | Truncate and hash |
| `simplyblock.io/storage-node-uuid.<clusterUUID>.<n>`, a key        | The socket index needs nine digits or more, the rest of the key being 55 bytes | An 8-digit socket index     | None needed       |

The `node-type` row is the tightest limit in the product: 63 less a
26-character prefix leaves **37 characters for a `StorageCluster` name**, where
the API server allows 253.

The `worker` row is the one whose input this repository does not own.
`storage.simplyblock.io/worker` is written through `sanitiseDNSLabel`
(`internal/controller/simplyblockstoragenodeset_storagenode.go`), which replaces
every character a label may not carry and trims the ends, and does not bound the
length. A cloud that names a worker after its fully qualified domain name
produces the overflow.

### 19.3 The Object-Name Cases

Nine object names are built the same way, against the 253-byte limit.

| What is built                                          | Bounded by                                                | Longest input that works | Fix               |
|--------------------------------------------------------|-----------------------------------------------------------|--------------------------|-------------------|
| `simplyblock-<ns>-<cluster>-<pool>`, a `StorageClass`  | The pool name, at the namespace and cluster names CI uses | 210 characters           | Truncate and hash |
| `<set>-per-node-config`, a `ConfigMap`                 | The `StorageNodeSet` name                                 | 237 characters           | Truncate and hash |
| `simplyblock-storage-node-ds-<set>`, a `DaemonSet`     | The `StorageNodeSet` name                                 | 225 characters           | Truncate and hash |
| `<set>-storage-node-api-endpoints`, an `EndpointSlice` | The `StorageNodeSet` name                                 | 226 characters           | Truncate and hash |
| `<policy>-<pvc>`, a `ReplicationSlot`                  | Two names of up to 253 characters each                    | 246 characters           | Truncate and hash |
| `simplyblock-cluster-<cluster>`, a `Secret`            | The cluster name, which is unbounded                      | 233 characters           | Truncate and hash |
| `simplyblock-<cluster>-upgrade`, a `Secret`            | The cluster name, which is unbounded                      | 233 characters           | Truncate and hash |
| `<node>-remove`, a `StorageNodeOps`                    | The `StorageNode` name                                    | 246 characters           | Truncate and hash |
| `<name>-restored` and `<name>-imported`, backup kinds  | The source resource's name                                | 244 characters           | Truncate and hash |

These are the roomier half of the problem, and they are still reachable: a
`ReplicationSlot` joins two names that Kubernetes each allows to be 253
characters long.

### 19.4 One Field Closes Most of the List

`spec.clusterName` carries no maximum length and no pattern on either
`StoragePoolSpec` (`storagepool_types.go:115`) or `StorageNodeSetSpec`
(`storagenodeset_types.go:40`), and it feeds four of the eight labels and three
of the object names above. **A `+kubebuilder:validation:MaxLength=37` on it
closes more of this list than any other one-line change**, and 37 is what
§19.2's tightest row leaves. The marker lands on `v1alpha2`'s
`spec.clusterRef`, because §7.2 renames the field and retires `StorageNodeSet`,
and never on `v1alpha1` (§19.9).

**The 37 characters belong to the cluster's own name, which no `MaxLength` can
reach.** `metadata.name` is one of the two metadata fields a CRD validation rule
can see (§19.7), so the name itself is bounded by a type-level rule and the
reference by the marker:

```go
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 37",message="a StorageCluster name is at most 37 characters, because it is written into a node label behind a 26-character prefix"
```

### 19.5 The Three Fixes

Every row above resolves one of three ways, and which one applies follows from
who owns the name rather than from how long it is.

**Use a UUID.** When a stable identifier is already at hand, nothing reads the
current value, and the label exists to be selected on rather than read. The two
`StorageClass` labels are this case.

**Bound the input.** When the long name is this API's to refuse. A field
somebody types has no business being 200 characters, so the answer is no at
creation time, which is §19.4 and the `io.simplyblock.storagenodeset` row.

**Truncate and hash.** When the name belongs to somebody else, a worker named
by a cloud, or when the old value has to keep working because it already sits
inside live `PersistentVolume` objects or is an object name that cannot be
renamed. This is the majority of §19.3.

Bounding an input a cloud owns breaks enrollment on a legal node name, and
truncating a value the control plane correlates on breaks the correlation, so
the three do not substitute for one another.

### 19.6 The Patterns That Are Already Right

Four call sites already do this correctly, and they are the precedent the rest
adopt rather than a new invention:

- **`nodeprobe.ObjectName`** lowercases, replaces every character a DNS
  subdomain may not carry, trims the cut end, holds the stem to a 63-byte budget
  that leaves room for the prefix and the digest, and appends eight hex
  characters of a SHA-256 of the full input. It is deterministic in its inputs,
  so two processes derive the same name without coordinating. This is the
  truncate-and-hash reference.
- **The volume-migration `Job` name** holds to 60 bytes by capping the migration
  UUID at 20 and the node part at 16 plus a hash (`nodeSuffix`,
  `volumemigration_controller.go`).
- **`simplyblock-storage-node-binding-<ns>`** is safe because Kubernetes caps a
  namespace at 63, so the total cannot exceed 96.
- **The Helm chart** names every resource literally rather than building names
  from the release name.

`nodeprobe.ObjectName` lands with the discovery work, so the shared helper is
extracted from it rather than written twice, into `atlas-lib/kube/names.go`
beside the formulas it bounds.

### 19.7 Where a Rule Is Enforced

| Rule                                                      | Schema marker | CEL `XValidation` | Admission webhook | Preflight |
|-----------------------------------------------------------|---------------|-------------------|-------------------|-----------|
| One field's own length and shape                          | Yes           | —                 | —                 | Yes       |
| A derived name's length, from fields of one object        | —             | Yes               | Yes               | Yes       |
| A derived name's length, from an object and its namespace | —             | No                | Yes               | Yes       |
| Uniqueness across the objects of a kind                   | No            | No                | Yes, with a race  | Yes       |
| Uniqueness after a namespaced kind becomes cluster-scoped | No            | No                | No                | Yes       |
| Objects that already exist                                | No            | No                | No                | Yes       |

Two limits on the cheaper layers decide the split. A CRD validation rule sees
`self.metadata.name` and `self.metadata.generateName` and no other metadata, so
the `StorageClass` rule cannot be written as CEL at all: it needs the namespace.
And no schema rule of any kind can see a second object, so uniqueness is an
admission webhook or nothing.

The webhook races itself. Two creates admitted concurrently each see a free
derived name, so the reconciler treats a collision as a terminal condition with
an event rather than as something admission prevented.

The last row is the one this document turns on: every mechanism above it runs on
a write, and the objects an upgrade has to survive were written before the rule
existed.

### 19.8 Uniqueness, and What the Migration Changes About It

Length is the live problem and uniqueness is the one the migration introduces.
Four routes take two resources to one derived name:

- **Concatenation is ambiguous.** `simplyblock-<ns>-<cluster>-<pool>` joins
  three names with a separator that is legal inside all three, so cluster `a-b`
  with pool `c` and cluster `a` with pool `b-c` derive one `StorageClass` name
  in one namespace. The `simplyblock.io/pool.<ns>.<cluster>.<pool>` label key
  has the same defect with dots.
- **A cluster-scoped derived name drops the namespace.** The
  `io.simplyblock.node-type` value carries the cluster name and nothing else, so
  two `StorageCluster` objects of the same name in two namespaces claim the same
  worker nodes.
- **The `StorageNodeSet` retirement re-derives from the cluster what is derived
  from the set today.** The `DaemonSet`, the per-node `ConfigMap`, and the
  `EndpointSlice` are named per set precisely so several sets can coexist in one
  cluster, so two sets collapse onto one name the moment the parent becomes the
  cluster (§16.1).
- **A kind that becomes cluster-scoped loses the namespace that kept its objects
  apart.** `VolumeMigration` is namespaced and `PersistentVolumeOps` is not
  (§16.2), so two migrations of the same name in two namespaces are one object
  in the target model.

Truncate-and-hash answers the first two by construction, because the digest is
taken over the whole input rather than over the truncated stem. The last two are
migration blockers that no naming scheme fixes, and they are the reason the
preflight exists.

### 19.9 The Rules Go on `v1alpha2` Only

Tightening a served version's schema rejects updates to the objects that already
violate the new rule. Adding `MaxLength=37` to `v1alpha1` would therefore start
failing writes on exactly the clusters that are about to be upgraded, before the
upgrade had offered them anything. So `v1alpha1` keeps its schema until it is
retired (§7), `v1alpha2` carries the rules, and the preflight covers the objects
that predate them.

CRD validation ratcheting, beta and enabled by default from Kubernetes 1.30,
admits an update whose violating field is unchanged, which is exactly what the
storage rewrite of §24 writes. The preflight does not rely on it: it is a
feature gate this operator does not control, and where it does apply it defers
the failure to whichever user edit comes next rather than removing it.

### 19.10 The Preflight

The check is its own subcommand, so that a user can run it well before the
upgrade window:

```text
simplyblock-upgrade preflight
```

`upgrade` runs the same code as a prerequisite (§9.2), and `migrate` runs it
again during validation (§18), because an object created between the two phases
has never been checked.

It verifies that:

1. Every derived name and label under the new model is within the limit that
   binds it (§19.1).
2. No two source objects derive the same name (§19.8).
3. Every derived name is a valid DNS-1123 subdomain, and every derived label
   value and key is qualified, checked with the same `apimachinery` helpers the
   API server uses.
4. Every existing object satisfies every constraint `v1alpha2` adds, and not
   only the ones about names: fields that become required, values outside a
   recased enum, patterns, and numeric bounds.
5. No kind that becomes cluster-scoped has same-named objects in two namespaces.
6. No object carries both the old and the new spelling of an annotation with two
   different values (§16.3).
7. Conversion is stable for every object that exists: convert to `v1alpha2`,
   convert back, and compare against what was read.
8. Every legacy volume handle's pool name resolves to a pool that exists
   (§16.4), because a handle naming a pool that does not is a finding the
   migration cannot normalize.

Check 7 is §6.2's round trip run against the cluster's own data. An unstable
conversion turns the storage rewrite's unchanged write into an apparent edit,
and an apparent edit of a field `v1alpha2` makes immutable is rejected.

### 19.11 Failing Closed, and What a User Can Do About It

Every violation names the object, the derived value, the limit, and the change
that resolves it:

```text
ERROR  StorageCluster simplyblock/production-cluster-eu-central-1-primary
       name is 41 characters, the maximum is 37
       derived: Node label io.simplyblock.node-type
                = simplyblock-storage-plane-production-cluster-eu-central-1-primary
                  (67 bytes, a label value stops at 63)

ERROR  StoragePool simplyblock/prod-gold with pool tier, and
       StoragePool simplyblock/prod with pool gold-tier,
       derive one StorageClass name
       derived: simplyblock-simplyblock-prod-gold-tier

Preflight failed: 2 violations. No changes were made.
```

**Some violations can only be resolved by replacing the resource.** Kubernetes
has no rename, so a `StoragePool` whose name is too long needs a new
`StorageClass`, and a bound claim pins `spec.storageClassName` immutably, so its
volumes have to move to claims on the new class. That is a data migration on a
running cluster, which is why the check runs in `upgrade`'s prerequisites: found
there, the cluster is still untouched.

Where a name only feeds derived names and no data hangs off it, the migration
resolves the violation itself by adopting the truncate-and-hash form of §19.5.
Whether it may rewrite a derived name a user has scripted against is not settled
(§32, Q4).

### 19.12 The Limits Outside Kubernetes

The NVMe NQN, the SPDK serial number, and the CSI specification's 128-byte
volume id are **fixed by construction**, each composed from UUIDs of known
width, so none varies with what a user names anything.

A legacy volume handle is the exception: normalized it is 110 bytes, but with a
pool name it is 74 plus that name, so a pool named with more than 54 characters
produced a CSI volume id over the limit. §16.4 normalizes it.

---

## 20. Ownership Migration

Ownership migration is a first-class operation:

```text
OLD                          NEW

StorageCluster               StorageCluster
      │                            │
      ▼                            ▼
StorageNodeSet               StorageNode
      │
      ▼
StorageNode
```

The new ownership is established before the old ownership is removed:

```text
1. Update StorageNode ownerReference → StorageCluster
2. Verify StorageNode is owned by StorageCluster
3. Reparent the DaemonSet, Services, Endpoints, certificates,
   ServiceAccount, and per-node ConfigMaps → StorageCluster
4. Verify each reparented object
5. Remove the StorageNodeSet ownerReference
6. Verify every dependent object remains present
7. Delete StorageNodeSet
```

Kubernetes garbage collection deletes dependents when their owner goes, and
§16.1 lists what depends on a `StorageNodeSet`. So steps 3 and 4 are load
bearing, and the migration MUST NOT delete an old owner before every dependent
intended to survive has been transferred and verified.

`StorageCluster` owning `StoragePool` is established in the same phase, and that
edge is a cascade: a `kubectl delete storagecluster` deletes the cluster's pools,
and deleting a pool deletes the `StorageClass` objects it produced, which leaves
every bound claim pointing at a class that no longer exists.

**The pool's finalizer is what keeps the cascade away from tenant data.** A pool
with bound volumes refuses to finish deleting, so the cascade starts, the pool
holds in `Terminating`, and the cluster holds behind it. The delete visibly does
not complete, which is the outcome `design-storagepool.md` §6 decides on.

---

## 21. Resource Creation and Deletion

`migrate` creates and deletes resources:

```text
old resource exists
       │
       ▼
create the new resource
       │
       ▼
verify the new resource
       │
       ▼
transfer references and ownership
       │
       ▼
delete the old resource
```

Ordering keeps a valid representation of the resource in place wherever
practical, and deletion happens only after the replacement state is verified.

The `ClusterDeploymentConfig` that replaces the fleet template is created in
this phase, from the `StorageNodeSet` objects being retired, and it is verified
before any of them is deleted.

---

## 22. Idempotency

`migrate` MUST be safe to rerun. Every operation is preceded by a
determination of whether it has already been performed:

```text
StorageNode ownerReference = StorageNodeSet
    → transfer ownership

StorageNode ownerReference = StorageCluster
    → already migrated, continue
```

and:

```text
StorageNodeSet exists and is still required
    → leave it

StorageNodeSet exists, is obsolete, and its dependents transferred
    → delete it
```

A failed migration is therefore restartable without corrupting the graph.

### 22.1 Where the Position Is Kept

**The cluster is the record, and the position is derived from it.** An owner
reference points at the cluster or at the set, a renamed kind's object exists or
does not, and a handle carries a UUID or a name. Every determination above is a
read, which is what makes a rerun safe.

Three things are decisions rather than object state, so nothing can derive them:

- That the preflight passed, and against which chart and CRD version.
- The pre-upgrade capture §12.4 takes for its drift check.
- Which step was in flight when a run was killed, for the one case where the
  side effect is not visible in the object it acted on.

They are held in a `ConfigMap` in the operator namespace, named for the
migration. It is namespace-scoped, as the migration is, and it outlives every
object the migration touches, including the `StorageCluster`, which a migration
may have to be diagnosed after. Nothing else writes it, and it carries its own
`resourceVersion` as an optimistic lock, so two concurrent runs cannot both hold
the migration. `migrate` deletes it on success.

The migration is a CLI rather than a controller, so nothing else is watching for
this record and a `ConfigMap` is the whole of the durable state.

---

## 23. Checkpoints and Progress

**The walk MUST be declared as an `atlas-lib/statemachine` graph.** The package
exists and is tested, and `design-crd-model.md` §9.5 records that no `Ops` kind
is backed by it yet: `StorageNodeOps` drives its steps from a hand-rolled
`switch`, and `StorageClusterOps` carries one action's steps in a message field.
Both are being converted to the package, and this migration starts there.

The graph is declared as data, so every state names the states it may reach:

```go
// The migrate phase, as one Config. An edge to an undeclared state is caught by
// statemachine.New rather than at runtime on the unhappy path.
var migratePhases = statemachine.Config[Phase]{
    Initial: PhasePending,
    States: map[Phase]statemachine.StateDef[Phase]{
        PhasePending:      {To: []Phase{PhaseValidating}},
        PhaseValidating:   {To: []Phase{PhaseTransforming, PhaseFailed}, OnEnter: m.validate},
        PhaseTransforming: {To: []Phase{PhaseOwnership, PhaseFailed}, OnEnter: m.transform},
        PhaseOwnership:    {To: []Phase{PhaseHandles, PhaseFailed}, OnEnter: m.reparent},
        PhaseHandles:      {To: []Phase{PhaseDeleting, PhaseFailed}, OnEnter: m.normalizeHandles},
        PhaseDeleting:     {To: []Phase{PhaseRewriting, PhaseFailed}, OnEnter: m.deleteObsolete},
        PhaseRewriting:    {To: []Phase{PhaseVerifying, PhaseFailed}, OnEnter: m.rewriteStorage},
        PhaseVerifying:    {To: []Phase{PhaseCompleted, PhaseFailed}, OnEnter: m.verify},
        PhaseCompleted:    {},
        PhaseFailed:       {},
    },
}
```

Four properties of the package carry requirements this migration has:

- **An entry hook returns the deadline of the state it entered**, so a phase that
  hangs is detectable rather than indistinguishable from a phase that is working.
  §25 needs that, because a migration that is correctly waiting and one that has
  stalled look identical without it.
- **A snapshot carries the state and its deadline together**, through
  `Machine.Snapshot` and `NewFromSnapshot`, and restoring runs no entry hooks. So
  a resumed run does not repeat the side effect of the phase it resumes into,
  which is §22's requirement expressed as a type.
- **`statemachine.KubeSnapshot` is the durable form**, with `ToKube` and
  `FromKube` at the boundary. It is what §22.1 stores.
- **`DeclaredStates` enumerates the graph**, so the phase list a user sees and
  the graph the code walks cannot drift.

`MultiConfig` is not needed here, because `migrate` has one graph.

The machine is rebuilt from the stored snapshot on every run, so no in-memory
process has to stay alive for the migration to be recoverable.

---

## 24. Storage-Version Migration

Changing the storage version is separate from application-level migration. For
each CRD:

```text
v1alpha1 storage=true      v1alpha1 storage=false
v1alpha2 storage=false  →  v1alpha2 storage=true
```

After the switch, objects that have not been written since still exist in etcd
in the old representation, and `.status.storedVersions` still lists
`v1alpha1`. Until that list holds `v1alpha2` alone, `v1alpha1` cannot be
removed from the CRD, because the API server refuses to drop a version it still
has stored objects in.

**The migration rewrites the objects itself.** The Kubernetes
`StorageVersionMigration` API is not used: it is served at
`storagemigration.k8s.io/v1alpha1`, gated behind the `StorageVersionMigrator`
feature gate, and disabled by default, which makes it unavailable on the
managed distributions and the OpenShift versions this operator supports. A
migration path that only works where a cluster administrator has enabled an
alpha feature gate is not a migration path.

The rewrite is:

1. Switch the CRD to the new storage version.
2. List every object of that kind, in every namespace.
3. Write each object back unchanged.
4. Verify the write.
5. Patch `.status.storedVersions` down to `v1alpha2`.
6. Verify that the CRD reports the new stored version alone.

Conceptually:

```text
list all objects of a kind
      │
      ├── object A → update → v1alpha2 storage
      ├── object B → update → v1alpha2 storage
      ├── object C → update → v1alpha2 storage
      └── ...
```

Three properties this rewrite needs, none of which the semantic transformation
of §16 needs:

**It is a forced write of unchanged content.** A no-op update is what moves an
object between storage representations, so the rewrite deliberately writes
objects it has nothing to say about. The write reaches etcd because the
persisted bytes no longer match what the current storage version encodes, and
the API server skips an update whose encoded content is already identical. The
rewrite is therefore free to run twice: the second pass writes nothing.

**It races the operator.** The operator is running and writing status, so a
rewrite that reads and then writes can conflict. The rewrite retries on
conflict, and it uses the object's `resourceVersion` so that a conflict is
detected rather than a concurrent update overwritten.

**It is paced.** Seven kinds across every namespace is still a large number of
sequential writes on a cluster whose API server is also serving a running
storage system, so the rewrite is rate-limited and reports progress per kind.
The kinds it does not touch are the ten of §7.2 whose storage version never
changes, and skipping them is not an optimization: rewriting an object of a kind
with one version accomplishes nothing and can still conflict with the operator.

The implementation keeps this separate from §16, and both separate from the
conversion of §6.

---

## 25. Failure Handling

Failures are expected operational events. The migration:

- Stops on unexpected state.
- Never deletes a resource before its replacement state is verified.
- Provides actionable errors.
- Is safe to rerun.
- Detects partially completed migrations.
- Never silently skips a resource.

For example:

```text
ERROR
StorageNode node-a references StorageNodeSet nodeset-a,
but nodeset-a references a different StorageCluster.

Migration aborted.
No destructive changes were performed.
```

or:

```text
ERROR
StorageNode node-b was expected to be owned by StorageCluster cluster-a,
but the ownership update could not be verified.

Migration stopped before deleting StorageNodeSet nodeset-a.
Rerun migrate after resolving the issue.
```

Every blocked or held decision owes the user a Kubernetes event as well as a
line of output, because a migration that is correctly refusing to proceed and a
migration that has hung are indistinguishable otherwise.

---

## 26. Rollback Considerations

`upgrade` is as reversible as it can be made. Before `migrate` runs, the
cluster is:

```text
v1alpha1 served, storage
v1alpha2 served
conversion webhook available
new operator running
old resource topology intact
```

Rolling the operator back at this stage means a `helm rollback` or an OLM
rollback to the previous `ClusterServiceVersion`, and the CRDs keep both
versions, which the old operator ignores. The conversion webhook stays deployed
until the CRDs stop serving `v1alpha2`.

**The release handover narrows that window, and adoption closes it.** A
`helm rollback` restores a release manifest that lists the control plane and the
CSI driver, so a rollback taken after the operator has adopted them leaves Helm
and the operator both claiming one object. Rolling back after adoption therefore
means dropping the adopted objects from the restored release first, which is the
handover of §12 run backward: annotate, roll back, and let the previous release
reclaim only what it should. Rolling back before adoption is an ordinary Helm
rollback, because the objects are still exactly as the old release left them.

After `migrate`, rollback of the data model is no longer guaranteed.

```text
StorageCluster
    └── StorageNode
```

cannot be restored to:

```text
StorageCluster
    └── StorageNodeSet
          └── StorageNode
```

without reconstructing the deleted `StorageNodeSet` objects, and reconstructing
them means recreating the DaemonSet ownership that the reparenting moved, on a
cluster whose storage nodes are running.

> `migrate` is the point after which rollback of the application data
> model is no longer guaranteed.

This is stated to the user before the phase runs, and the phase requires
confirmation.

---

## 27. The Plan

**The plan belongs to `preflight`, which is the one read-only command.** It
already discovers the graph and validates it (§19.10), and calculating the
transformations is that same walk with the writes left out. One command that
reads therefore means one implementation of the walk that reads.

```bash
simplyblock-upgrade preflight
```

`preflight` reports both halves: the checks, and the plan for whichever phase
the cluster is positioned for. Before `upgrade` it reports what §19.10 verifies
plus the release handover's classification (§12.1), and after `upgrade` it
reports the migration plan below. It knows which by reading the cluster, which
is the same derivation §22.1 uses, so the user does not pass a phase.

The plan mutates nothing, including through server-side dry-run writes, which it
does not issue.

```text
StorageCluster cluster-a

  StorageNodeSet nodeset-a
    StorageNode node-a
      owner:     StorageNodeSet/nodeset-a
      new owner: StorageCluster/cluster-a

    StorageNode node-b
      owner:     StorageNodeSet/nodeset-a
      new owner: StorageCluster/cluster-a

  REPARENT
    DaemonSet/simplyblock-storage-node        → StorageCluster/cluster-a
    Service/simplyblock-storage-node-node-a   → StorageCluster/cluster-a
    Service/simplyblock-storage-node-node-b   → StorageCluster/cluster-a
    ConfigMap/simplyblock-node-a              → StorageCluster/cluster-a
    ConfigMap/simplyblock-node-b              → StorageCluster/cluster-a

  CREATE
    ClusterDeploymentConfig/cluster-a

  DELETE
    StorageNodeSet/nodeset-a

Migration validation successful.

2 StorageNodes will change ownership.
3 volume handles will be normalized.
5 dependent objects will be reparented.
1 ClusterDeploymentConfig will be created.
1 StorageNodeSet will be deleted.
0 StorageNodes will be deleted.
7 CRDs will be rewritten into v1alpha2 storage.
```

---

## 28. Temporary Conversion Webhook Lifecycle

The conversion webhook exists because seven CRDs serve two versions, so it is
removed when they stop. That is only true once:

- No supported client requires `v1alpha1`.
- The storage rewrite has completed and `.status.storedVersions` holds
  `v1alpha2` alone on those seven.
- Every relevant resource has been migrated.
- The operator no longer requires conversion.

Removal deletes the conversion webhook's deployment, Service, Secret, RBAC, and
ServiceAccount, and drops the `conversion` stanza from each CRD back to
`strategy: None`. Because none of those objects belongs to the Helm release
(§5), removing them is the installer's job and not a chart change.

If another breaking API migration follows, the same deployment mechanism is
reused with a new conversion implementation.

---

## 29. Implementation Layout

The work splits along the three mechanisms of §4, and the split is what keeps
API conversion from being conflated with application migration.

### 29.1 The `v1alpha2` API Package

`operator/api/v1alpha2/`, alongside the existing `v1alpha1`, with its own
`groupversion_info.go` declaring
`schema.GroupVersion{Group: "storage.simplyblock.io", Version: "v1alpha2"}`.
`operator/cmd/main.go` registers it beside
`simplyblockv1alpha1.AddToScheme(scheme)` at the
`+kubebuilder:scaffold:scheme` marker.

Three repository rules apply to the new package. `kubebuilder create api` writes
an Apache license header, which this repository does not use in new files, so it
is removed and replaced with a comment stating what is in the file. The types
are bound by the `api-design` skill, and
`.claude/skills/api-design/scripts/check-crds.py` audits them. And
`make -C operator manifests generate`, `make -C operator build-installer`, and
`make helm-sync` all have to run, because
`.github/workflows/operator_manifests.yaml` fails on the drift otherwise.

### 29.2 Conversion Functions

`operator/api/v1alpha1/*_conversion.go`, one file per converting kind, holding
`ConvertTo` and `ConvertFrom`, with `Hub()` on the `v1alpha2` types. There are
seven of them and not seventeen (§7.2), and the ten kinds without a `v1alpha2`
get no file: `conversion.IsConvertible` reports false for a type that implements
neither interface, which is what keeps them out of the webhook's registry.

§6.2 has what a conversion may do, and §30.1 and §30.2 have what it is tested
against. The fuzz corpus is seeded from `operator/config/samples/` and the
`test-cluster*.yaml` manifests.

### 29.3 The Conversion Webhook Binary

`operator/cmd/conversion-webhook/main.go`, following the layout of
`operator/cmd/simplyblock-rebalancer` but built into the operator image by a
second `go build` in `operator/Dockerfile` rather than into its own.

It builds a manager with a webhook server, provisions its certificate as §8
describes, and registers `conversion.NewWebhookHandler` at `/convert` once the
ready channel closes, which is the deferred registration `operator/cmd/main.go`
already uses for its five admission handlers.

It runs no controllers, holds no leader-election lease, and reads no simplyblock
custom resource.

### 29.4 The Migration Tool

`operator/cmd/simplyblock-upgrade/`, also built into the operator image, so that
it can run either from a workstation against a kubeconfig or as a Job in the
cluster.

```text
simplyblock-upgrade preflight    # read-only: the checks and the plan (§27)
simplyblock-upgrade upgrade      # phase one (§9)
simplyblock-upgrade migrate      # phase two (§15)
```

It uses the Kubernetes client libraries rather than shelling out to `kubectl`,
and Helm's Go SDK rather than shelling out to `helm`, so that the values
translation of §13.1 operates on the release's recorded values rather than on a
file the user has to supply.

**It is a command.** Nothing watches on its behalf, which is why §22.1's record
is a `ConfigMap` and why every phase determination is a read of the cluster
rather than of a bookmark.

Shared primitives belong in `atlas-lib` rather than here: Kubernetes object
correlation, error classification, locks, and the state machine of §23 are
already there (`atlas-lib/README.md`).

### 29.5 What Each Command Owes

`upgrade` walks §9.1 and MUST NOT perform application-level migration.
`migrate` walks the graph of §23 and MUST be idempotent (§22).

---

## 30. Testing Requirements

The migration is tested at four levels, and three of them have a harness in this
repository already.

| Level                              | Where                                                                       | What it proves here                                                                                            |
|------------------------------------|-----------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------|
| Go unit, fake client and mock HTTP | `operator/internal/**/*_unit_test.go`, `make -C operator test`              | Conversion functions, round trips, graph validation, and the idempotency decisions                             |
| envtest                            | `operator/internal/controller/suite_test.go`, the same target               | A real API server serving both versions, conversion through the API, and garbage collection during reparenting |
| Operator e2e on Kind               | `operator/test/e2e/`, `make -C operator test-e2e`                           | `upgrade` end to end, including the webhook bootstrap and the CRD application                                  |
| Live cluster, manual               | The shell drivers in `operator/test/utils/`, against a cluster holding data | `migrate` on a running cluster, which no automated harness covers                                              |

`test/integration/` is not this migration's harness. It boots a Talos cluster in
QEMU to exercise node-level NVMe-oF, LVM, and filesystem behavior, and it knows
nothing about the operator's API versions.

The regression rule of `AGENTS.md` applies throughout: the test comes first and
is proven red before the fix.

### 30.1 Conversion Tests

Every field mapping, renamed field, moved field, default value, removed field,
enum change, type change, nested object, list, map, and nil or empty value.
`skipKubeletConfiguration` gets its own test for the inversion, and each of the
three recased action enums gets a test per value.

### 30.2 Round-Trip Tests

```text
v1alpha1 → v1alpha2 → v1alpha1
v1alpha2 → v1alpha1 → v1alpha2
```

for each of the seven converting kinds, over hand-written objects and over
fuzzed ones. A test that a kind without a `v1alpha2` implements neither
`conversion.Hub` nor `conversion.Convertible` belongs here too, because the
failure it catches is a `v1alpha2` type added to a kind §7.2 says has none.

### 30.3 Migration Tests

Empty cluster, single resource, multiple resource trees, partially migrated
state, unexpected ownership, missing references, duplicate references, already
migrated resources, resources created between discovery and migration, deletion
failures, API failures, and operator failures.

### 30.4 Ownership Tests

Reparent as §20 orders it, then delete the `StorageNodeSet` and assert that
every dependent §16.1 lists survives. This is the test the ordering of §20
exists for, and it runs under envtest, where garbage collection is real.

### 30.5 Storage-Rewrite Tests

`.status.storedVersions` reaching `v1alpha2` alone, the conflict retry against
an operator writing status concurrently, and a rewrite interrupted partway
through and rerun.

### 30.6 Failure and Retry Tests

Terminate the migration at every major step and rerun it. The second invocation
must detect the partially migrated state and continue from it.

### 30.7 Release Handover Tests

The prune only happens against a real release, so this runs as `upgrade` on a
Kind cluster carrying the previous chart.

- A difference classified from a release with
  `controlplane.observability.enabled` set, so the MongoDB and OpenSearch
  objects are in it.
- An unclassified object: the step refuses, and refuses before annotating.
- The annotation's effect against the Helm version in use, which is what keeps
  §12.2 true when Helm changes.
- Object UIDs unchanged across adoption, since a new UID is a rebuilt database.
- The operator's rendered output equal to the pre-upgrade capture.
- A rollback before adoption, and one after.

### 30.8 Name and Identity Tests

**This is where the boundary cases are, and boundary cases are what gets
skipped.** Every row of §19.2 and §19.3 gets three tests: one input at the
longest length that works, one a single byte longer, and one whose final
character is the `-` or `.` a label value may not end on. The measured numbers in
those tables are the expected values, so a test that disagrees with them has
found either a formula change or an error in the audit.

Beyond the boundaries:

- The derived-name helper is deterministic: the same input yields the same name
  in two processes, and truncation does not make two distinct inputs collide,
  because the digest covers the whole input.
- The four uniqueness routes of §19.8, each as a case the preflight rejects:
  the ambiguous concatenation, the namespace-free cluster label, the two node
  sets collapsing onto one derived name, and the two same-named objects of a
  kind that becomes cluster-scoped.
- The preflight fails closed and mutates nothing, asserted against a fake client
  that fails the test on any write.
- Validity is asserted with `k8s.io/apimachinery/pkg/util/validation` rather
  than with a regular expression written for the test, so the assertion tracks
  the API server rather than a second opinion about it.

---

## 31. Definition of Done

Each item is a check somebody can run. Where a section states a requirement in
prose, its check is here and not repeated in both places.

**The API**

- [ ] `operator/api/v1alpha2` passes `check-crds.py` and carries no license
      headers.
- [ ] Conversion exists for the seven kinds of §7.2 and for no others, and the
      round-trip and fuzz tests pass.
- [ ] A `v1alpha2` write is validated by the operator's admission webhooks.
- [ ] `make -C operator manifests generate`, `make -C operator build-installer`,
      and `make helm-sync` are clean.

**The conversion webhook**

- [ ] It runs with the operator stopped, from the operator's own image.
- [ ] Its TLS bootstrap reads no simplyblock custom resource.
- [ ] The CA bundle reaches `conversion.webhook.clientConfig` on the seven.
- [ ] It can be removed, along with its RBAC and the `conversion` stanzas, once
      §28's conditions hold.

**The release handover (§12)**

- [ ] The survivor list is computed from the deployed release, and an
      unclassified object refuses the step.
- [ ] The annotation's effect is verified before `helm upgrade` runs.
- [ ] Adoption leaves object UIDs unchanged, drifts nothing against the
      pre-upgrade capture, and strips the Helm metadata afterward.

**`upgrade`**

- [ ] It verifies real conversion rather than Pod readiness.
- [ ] It translates the deployed release's values, and handles an OLM install
      (§32, Q1).
- [ ] It modifies no application resource topology, and the user can verify the
      cluster before `migrate` runs.

**`migrate`**

- [ ] It walks an `atlas-lib/statemachine` graph, is idempotent, and resumes a
      partial run.
- [ ] It validates before it mutates.
- [ ] Reparenting follows §20's order, and deleting the old owner
      garbage-collects nothing that must survive, proven under envtest.
- [ ] Renamed and absorbed kinds are copied and their sources deleted, and the
      annotation and label keys are rewritten.
- [ ] The storage rewrite is paced, retries on conflict, and reaches
      `.status.storedVersions` of `v1alpha2` alone on the seven.

**Volume handles (§16.4)**

- [ ] Every legacy handle is reported with the UUID its pool name resolves to,
      and an unresolvable one fails the preflight.
- [ ] A `PersistentVolume` is replaced only under `Retain`, one at a time, and
      never while a pod has the claim mounted.
- [ ] The normalized handle is written to
      `storage.simplyblock.io/volume-handle` on every `PersistentVolume` and
      `VolumeSnapshotContent` whose field carries a pool name.
- [ ] One `atlas-lib` function decides between the annotation and the field, and
      it rejects an annotation whose cluster or volume segment differs.
- [ ] `lvol.ParseHandle` stays tolerant for objects with no annotation.

**Names (§19)**

- [ ] Every name and label of §19.2 and §19.3 has a bounded derivation.
- [ ] `metadata.name` on the `v1alpha2` `StorageCluster` is bounded at 37 by an
      `XValidation` rule, and `StoragePoolSpec.clusterRef` by `MaxLength`.
- [ ] The truncate-and-hash helper is extracted from `nodeprobe.ObjectName` into
      `atlas-lib/kube`, and no call site rolls its own.
- [ ] §19.8's uniqueness rules are enforced at admission, and a collision that
      races admission is terminal with an event.

**The tool**

- [ ] `preflight` runs standalone, reports the plan as well as the checks, and
      covers all eight checks of §19.10.
- [ ] A test plan exists at `operator/docs/tests/test-plan-api-upgrade.md`.

---

## 32. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                         | Owner         |
|-----|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| Q1  | How does an OLM bundle declare a conversion webhook whose deployment the bundle does not contain? A `ClusterServiceVersion` can declare a `ConversionWebhook` for a deployment it installs, which the temporary webhook of §6.1 is not                                                                                                                                                                           | Operator team |
| Q2  | What becomes of the registered `Task` kind? `design-crd-model.md` §9.1 states explicitly that this is not decided, and the answer decides whether §16.2 gains a fifth row                                                                                                                                                                                                                                        | Operator team |
| Q3  | Does `MaxLength` join the marker set the `api-design` skill owns, and does `check-crds.py` audit a name-bearing field that carries none? No field in the seventeen kinds carries one today, so every one of them is a finding on the audit's first run                                                                                                                                                           | Operator team |
| Q4  | May the migration rewrite a derived name into the truncate-and-hash form on a user's behalf? It resolves the violation without a data migration, and it changes a string a runbook or a dashboard may select on. §19.5 says the value has to keep working when it is already inside live `PersistentVolume` objects, which is the case that decides this                                                         | Operator team |
| Q5  | Who owns the objects §12.1 leaves unattributed: the Prometheus and Reloader subcharts, MongoDB and OpenSearch where observability is enabled, `StorageClass/local-hostpath`, the NUMA resource plugin, and the caching-node restart script. Each is either adopted by a custom resource, left to the user to install separately, or annotated and abandoned, and the third produces an orphan nothing reconciles | Operator team |
