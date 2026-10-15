# Test Plan: S3 Gateway Provisioning (`ObjectStore`)

Related design: [`designs/design-objectstore.md`](../designs/design-objectstore.md)
Harness: [`internal/controller`](../../internal/controller)

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
also appears in §8 What Is Not Yet Covered.

---

## 1. Unit Tests

All pure functions and the controller's object-construction helpers are covered
without a cluster: the phase-transition table, and the Deployment/Service/Certificate
builders.

### Phase Transitions (§6)

File: `internal/controller/objectstore_phase_unit_test.go`

| #    | Scenario                                                                                               | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------|----------|------|
| U-01 | No Deployment yet → `Pending`                                                                          | Positive | —    |
| U-02 | Deployment/Service/Certificate created, Deployment not yet Available → `Provisioning`                  | Positive | —    |
| U-03 | Deployment Available, Certificate Ready → `Ready`                                                      | Positive | —    |
| U-04 | Deployment Available, Certificate not Ready → stays `Provisioning`                                     | Positive | —    |
| U-05 | `Provisioning` past its deadline with the image unpullable → `Degraded`, `DeploymentAvailable` `False` | Negative | —    |
| U-06 | `cert-manager` provider with no matching `Issuer` → `Degraded`, `CertificateReady` `False`             | Negative | —    |
| U-07 | `Ready`, `spec.replicas` changed → reconciles in place, never returns to `Pending`                     | Positive | —    |
| U-08 | Operator restart mid-`Provisioning` → resumes from `status`, does not recreate the Deployment          | Positive | —    |

### Object Construction (§4, §5)

File: `internal/controller/objectstore_build_unit_test.go`

| #    | Function                | Scenario                                                                                                 | Type     | Test |
|------|-------------------------|----------------------------------------------------------------------------------------------------------|----------|------|
| U-09 | `buildDeployment`       | Pod template carries label `objectstore.simplyblock.io/instance=<name>`                                  | Positive | —    |
| U-10 | `buildDeployment`       | `spec.envFrom` copied onto the gateway container's `envFrom` unmodified                                  | Positive | —    |
| U-11 | `buildDeployment`       | `spec.replicas` unset → Deployment requests 1 replica (default)                                          | Boundary | —    |
| U-12 | `buildService`          | `ClusterIP`, single port `spec.port`, selector matches the Deployment's pod label                        | Positive | —    |
| U-13 | `buildService`          | `spec.port` at its minimum (1) and maximum (65535) both accepted                                         | Boundary | —    |
| U-14 | TLS provider dispatch   | `tlsProvider: OpenShift` → `ServingCertServiceAnnotations`, no `Certificate` object created              | Positive | —    |
| U-15 | TLS provider dispatch   | `tlsProvider: cert-manager` → `utils.BuildServiceServingCertificate` called, no OpenShift annotation set | Positive | —    |
| U-16 | `spec.image` validation | A registry prefix outside the three allowed is rejected by the CRD's own `Pattern`                       | Negative | —    |

---

## 2. Integration Tests

Run the full `ObjectStoreReconciler` reconcile loop against `envtest`, with
cert-manager's CRDs installed, exercising both TLS providers. `Deployment.status`
is set by the test, since `envtest` runs no kubelet.

### Provisioning (§6, §7)

| #    | Scenario                                                                                                                                                                                                   | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | `ObjectStore` created → Deployment, Service, and (cert-manager provider) `Certificate` all created, owned via `ownerReferences`                                                                            | Positive | —    |
| I-02 | Deployment and Certificate faked as ready → `ObjectStore` reaches `Ready`, `status.readyReplicas` mirrors the Deployment                                                                                   | Positive | —    |
| I-03 | `spec.replicas` updated on an existing `ObjectStore` → Deployment's replica count updates, no Service or Certificate churn                                                                                 | Positive | —    |
| I-04 | `ObjectStore` deleted while `Ready` → Deployment, Service, and Certificate garbage-collected, no finalizer blocks it                                                                                       | Positive | —    |
| I-05 | Two reconciles triggered concurrently for the same object → converge to one Deployment/Service/Certificate, no duplicates                                                                                  | Positive | —    |
| I-06 | Operator process restarted mid-`Provisioning` (envtest manager stopped and restarted) → resumes from `status` without re-creating owned objects                                                            | Positive | —    |
| I-07 | `spec.envFrom` names a `Secret` that does not exist → `Degraded`, event naming the missing reference (this reconciler does not validate the `Secret`'s existence, and kubelet reports its absence instead) | Negative | —    |

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

| Axis                | Values covered                                             | IDs                    | Not covered                                                                                                                                |
|---------------------|------------------------------------------------------------|------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| TLS provider        | `OpenShift`, `cert-manager`                                | U-14, U-15, E-01, E-02 | —                                                                                                                                          |
| Replica count       | 1 (default), 2+, scaled in place                           | U-11, I-03             | 0 (rejected by CRD validation, not a runtime scenario)                                                                                     |
| Namespace scope     | single namespace                                           | I-01                   | multi-namespace (no cross-namespace behavior exists to test)                                                                               |
| Lifecycle / restart | mid-`Provisioning` operator restart, concurrent reconciles | U-08, I-05, I-06       | —                                                                                                                                          |
| Deletion            | `Ready` → deleted, no finalizer                            | I-04                   | deletion while an `S3Bucket` still references it (that invariant belongs to `design-bucket-provisioning.md`'s own test plan, not this one) |

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 16        | 0       | U-01 … U-16 |
| Integration | 7         | 0       | I-01 … I-07 |
| E2E         | 3         | 0       | E-01 … E-03 |

---

## 7. What Is Not Yet Covered

Nothing is implemented yet: every scenario above is a requirement, not yet a
passing test. Beyond that:

| #   | Gap                                                                    | Reason                                                                                         |
|-----|------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| —   | Graceful shutdown / in-flight request draining during a rolling update | Open Question 2 in the design: not designed here, may be entirely the gateway binary's concern |
| —   | Richer credential injection (projected volumes, a Vault Agent sidecar) | Open Question 3 in the design: `spec.envFrom` is the only mechanism this CRD has               |
