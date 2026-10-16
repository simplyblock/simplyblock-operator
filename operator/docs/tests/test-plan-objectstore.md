# Test Plan: S3 Gateway Provisioning (`ObjectStore`)

Related design: [`designs/design-objectstore.md`](../designs/design-objectstore.md)
Harness: `internal/controllers/objectstore`. A new nested controller package
rather than the legacy flat `internal/controller` this plan's file-path
citations originally named: `S3Bucket`'s own reconciler
(design-bucket-provisioning.md) already settled on the newer `TestMain` +
shared `envtest.Environment` + plain `testing.T` convention, and
`ObjectStoreReconciler` runs in the same manager process
(0005-s3bucket-joins-the-main-operator-manager.md), so it lives in the same
package rather than starting a second one.

Scope: the operator's Kubernetes surface. Whether a pulled image actually runs a
working gateway, and whether cert-manager or OpenShift's serving-cert controller
actually issues a certificate, are dependencies of this repository, faked at the
boundary for unit and integration tests and exercised for real only in the E2E
class.

Scenario IDs are permanent: `U-` unit (no cluster — pure functions, fake
`client.Client`), `I-` integration (full reconcile loop against `envtest` + cert-manager's
CRDs installed), `E-` end-to-end (live cluster, both TLS providers). Types are
`Positive`, `Negative`, `Boundary`, `Regression`. The `Test` column names the
implementing function, or `—` when the scenario is not yet covered — every `—`
also appears in §7 What Is Not Yet Covered.

---

## 1. Unit Tests

All pure functions and the controller's object-construction helpers are covered
without a cluster: the phase-transition table, and the Deployment/Service/Certificate
builders.

### Phase Transitions (§6)

File: `internal/controllers/objectstore/objectstore_phase_unit_test.go`

| #    | Scenario                                                                                               | Type     | Test                                                              |
|------|--------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------------|
| U-01 | No Deployment yet → `Pending`                                                                          | Positive | `TestFreshObjectStoreRestoresToPending`                           |
| U-02 | Deployment/Service/Certificate created, Deployment not yet Available → `Provisioning`                  | Positive | `TestFirstReconcileCreatesObjectsAndEntersProvisioning`           |
| U-03 | Deployment Available, Certificate Ready → `Ready`                                                      | Positive | `TestProvisioningReachesReadyOnceBothConditionsAreMet`            |
| U-04 | Deployment Available, Certificate not Ready → stays `Provisioning`                                     | Positive | `TestProvisioningStaysWhileCertificateIsNotReady`                 |
| U-05 | `Provisioning` past its deadline with the image unpullable → `Degraded`, `DeploymentAvailable` `False` | Negative | `TestProvisioningDeadlinePassedWithNoAvailableDeploymentDegrades` |
| U-06 | `cert-manager` provider with no matching `Issuer` → `Degraded`, `CertificateReady` `False`             | Negative | `TestProvisioningDeadlinePassedWithNoReadyCertificateDegrades`    |
| U-07 | `Ready`, `spec.replicas` changed → reconciles in place, never returns to `Pending`                     | Positive | `TestReadyObjectStoreReconcilesReplicasInPlace`                   |
| U-08 | Operator restart mid-`Provisioning` → resumes from `status`, does not recreate the Deployment          | Positive | `TestRestartMidProvisioningDoesNotRecreateTheDeployment`          |

### Object Construction (§4, §5)

File: `internal/controllers/objectstore/objectstore_build_unit_test.go`

| #    | Function              | Scenario                                                                                                 | Type     | Test                                                          |
|------|-----------------------|----------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-09 | `buildDeployment`     | Pod template carries label `objectstore.simplyblock.io/instance=<name>`                                  | Positive | `TestBuildDeploymentCarriesTheGatewayInstanceLabel`           |
| U-10 | `buildDeployment`     | `spec.envFrom` copied onto the gateway container's `envFrom` unmodified                                  | Positive | `TestBuildDeploymentCopiesEnvFromUnmodified`                  |
| U-11 | `buildDeployment`     | `spec.replicas` unset → Deployment requests 1 replica (default)                                          | Boundary | `TestBuildDeploymentDefaultsReplicasToOne`                    |
| U-12 | `buildService`        | `ClusterIP`, single port `spec.port`, selector matches the Deployment's pod label                        | Positive | `TestBuildServiceIsClusterIPOnSpecPortSelectingTheDeployment` |
| U-13 | `buildService`        | `spec.port` at its minimum (1) and maximum (65535) both accepted                                         | Boundary | `TestBuildServicePortAtItsLimits`                             |
| U-14 | TLS provider dispatch | `tlsProvider: OpenShift` → `ServingCertServiceAnnotations`, no `Certificate` object created              | Positive | `TestOpenShiftTLSProviderAnnotatesTheServiceOnly`             |
| U-15 | TLS provider dispatch | `tlsProvider: cert-manager` → `utils.BuildServiceServingCertificate` called, no OpenShift annotation set | Positive | `TestCertManagerTLSProviderBuildsACertificateAndNoAnnotation` |

~~U-16 `spec.image` validation~~ **Moved to Integration (I-16).** A fake
`client.Client` applies no OpenAPI schema validation at all, so a CRD
`Pattern` rejection can only be proven against a real apiserver.

---

## 2. Integration Tests

Run the full `ObjectStoreReconciler` reconcile loop against `envtest`, with
cert-manager's CRDs installed (`internal/controllers/objectstore/testdata/cert-manager-crds.yaml`,
vendored from cert-manager v1.21.2's own release), exercising both TLS providers.
`Deployment.status` and the Certificate's `status.conditions` are set by the test
directly, since `envtest` runs no kubelet and no cert-manager controller either.

### Provisioning (§6, §7)

File: `internal/controllers/objectstore/objectstore_controller_test.go`

| #    | Scenario                                                                                                                                                                                                   | Type     | Test                                                                                                                                                                                                                                                                                                         |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| I-01 | `ObjectStore` created → Deployment, Service, and (cert-manager provider) `Certificate` all created, owned via `ownerReferences`                                                                            | Positive | `TestObjectStoreCreatesItsOwnedObjects`                                                                                                                                                                                                                                                                      |
| I-02 | Deployment and Certificate faked as ready → `ObjectStore` reaches `Ready`, `status.readyReplicas` mirrors the Deployment                                                                                   | Positive | `TestObjectStoreReachesReadyOnceDeploymentAndCertificateAreReady`                                                                                                                                                                                                                                            |
| I-03 | `spec.replicas` updated on an existing `ObjectStore` → Deployment's replica count updates, no Service or Certificate churn                                                                                 | Positive | `TestObjectStoreReplicaChangeUpdatesTheDeploymentInPlace`                                                                                                                                                                                                                                                    |
| I-04 | `ObjectStore` deleted while `Ready` → no finalizer blocks the delete                                                                                                                                       | Positive | `TestObjectStoreDeletionIsNotBlockedByAFinalizer` (narrower than first scoped: see §7)                                                                                                                                                                                                                       |
| I-05 | Two reconciles triggered concurrently for the same object → converge to one Deployment/Service/Certificate, no duplicates                                                                                  | Positive | — (design §7.3: the standard controller-runtime guarantee that one object's key is never processed by two workers at once, the same reasoning design-bucket-provisioning.md §7.3 states for its own I-08; a test driving two direct `Reconcile` calls would only reprove controller-runtime's own workqueue) |
| I-06 | Operator process restarted mid-`Provisioning` (envtest manager stopped and restarted) → resumes from `status` without re-creating owned objects                                                            | Positive | `TestObjectStoreResumesMidProvisioningWithoutRecreatingTheDeployment`                                                                                                                                                                                                                                        |
| I-07 | `spec.envFrom` names a `Secret` that does not exist → `Degraded`, event naming the missing reference (this reconciler does not validate the `Secret`'s existence, and kubelet reports its absence instead) | Negative | — (needs a real kubelet to produce `CreateContainerConfigError`; `envtest` runs none, same boundary §9's own failure-table row draws)                                                                                                                                                                        |
| I-16 | `spec.image` naming a registry outside the three the CRD's `Pattern` allows is rejected at `Create` (moved from U-16: only a real apiserver enforces it)                                                   | Negative | `TestObjectStoreImageOutsideTheAllowedRegistriesIsRejectedAtCreate`                                                                                                                                                                                                                                          |

---

## 3. E2E Tests

A live cluster, run once with `tlsProvider: OpenShift` and once with
`tlsProvider: cert-manager`, proving what integration tests fake: that a real
certificate is issued, lands in the Secret the gateway pod mounts, and the pod
actually serves TLS with it.

### Certificate Issuance and Service Reachability (§5)

| #    | Scenario                                                                                                                                                       | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | `ObjectStore` created with `tlsProvider: cert-manager` → `Certificate` issued, Secret populated, gateway pod mounts it and serves TLS on `spec.port`           | Positive | —    |
| E-02 | Same, with `tlsProvider: OpenShift` → the service-ca-operator populates the annotated Secret the same way                                                      | Positive | —    |
| E-03 | A client inside the cluster reaches the `ObjectStore`'s `ClusterIP` Service on `spec.port` and completes a TLS handshake verified against the cluster's own CA | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01 — A gateway pod is killed under live S3 traffic

**Design reference:** §9's "A gateway pod crashes under live S3 traffic" row.

**What to verify:** the Deployment replaces the pod, and `design-bucket-provisioning.md`'s
mount delivery (targeted at the node, not the pod) is unaffected: the replacement
pod, once scheduled, sees every bucket already delivered to that node via the
same `HostToContainer` mount, with no re-delivery needed.

**Open question:** whether the gateway binary's own readiness probe gates traffic
away from the pod during the restart window, or whether this is the gateway
repository's concern entirely (`design-objectstore.md` Open Question 2).

**Test concept:**
1. Create an `ObjectStore` with 2+ replicas and at least one `S3Bucket` already `Ready`.
2. Kill one gateway pod (`kubectl delete pod`).
3. Confirm the replacement pod, once `Ready`, can read and write the already-delivered bucket without any `S3Bucket` reconcile or mount-delivery call.

---

## 5. Axis Coverage

| Axis                | Values covered                                             | IDs                    | Not covered                                                                                                                                                                                                           |
|---------------------|------------------------------------------------------------|------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| TLS provider        | `OpenShift`, `cert-manager`                                | U-14, U-15, E-01, E-02 | —                                                                                                                                                                                                                     |
| Replica count       | 1 (default), 2+, scaled in place                           | U-11, I-03             | 0 (rejected by CRD validation, not a runtime scenario)                                                                                                                                                                |
| Namespace scope     | single namespace                                           | I-01                   | multi-namespace (no cross-namespace behavior exists to test)                                                                                                                                                          |
| Lifecycle / restart | mid-`Provisioning` operator restart, concurrent reconciles | U-08, I-05, I-06       | —                                                                                                                                                                                                                     |
| Deletion            | `Ready` → deleted, no finalizer                            | I-04                   | deletion while an `S3Bucket` still references it (that invariant belongs to `design-bucket-provisioning.md`'s own test plan, not this one); actual garbage collection of the owned objects (needs a real cluster, §7) |

---

## 6. Coverage Summary

| Class       | Scenarios                      | Covered | Not covered |
|-------------|--------------------------------|---------|-------------|
| Unit        | 15 (U-16 moved to Integration) | 15      | —           |
| Integration | 8 (I-16, moved from U-16)      | 6       | I-05, I-07  |
| E2E         | 3                              | 0       | E-01 … E-03 |

---

## 7. What Is Not Yet Covered

| #    | Gap                                                                          | Reason                                                                                                                                                                 |
|------|------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| I-05 | Two concurrent reconciles converge to one set of owned objects               | Design §7.3's own "standard controller-runtime guarantee," the same reasoning design-bucket-provisioning.md's I-08 is not covered for                                  |
| I-07 | `spec.envFrom` naming a missing `Secret` degrades, naming the reference      | Needs a real kubelet to produce `CreateContainerConfigError`; `envtest` runs none                                                                                      |
| I-04 | Deleting a `Ready` `ObjectStore` actually garbage-collects its owned objects | `envtest` has no garbage-collector controller; only `TestObjectStoreDeletionIsNotBlockedByAFinalizer`'s narrower claim (no finalizer blocks the delete) is proven here |
| —    | Graceful shutdown / in-flight request draining during a rolling update       | Open Question 2 in the design: not designed here, may be entirely the gateway binary's concern                                                                         |
| —    | Richer credential injection (projected volumes, a Vault Agent sidecar)       | Open Question 3 in the design: `spec.envFrom` is the only mechanism this CRD has                                                                                       |
| —    | E-01 … E-03                                                                  | Need a live cluster with both TLS providers available, which this session has no access to                                                                             |
