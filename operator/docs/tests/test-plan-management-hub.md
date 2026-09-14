# Test Plan: Fleet Management on Open Cluster Management

**Related design:** [`designs/design-management-hub.md`](../designs/design-management-hub.md)  
**Status:** Draft  
**Date:** 2026-09-13 (last updated 2026-09-13)

Scenario IDs are permanent. `U-` is a unit test with no cluster, a fake client, and a mock control plane. `I-` runs the reconcile loop against `envtest` with the fleet CRDs, Open Cluster Management's CRDs, and a mock control plane. `E-` needs two Kubernetes clusters and one shared control plane. `M-` needs orchestration this repository cannot automate yet. `Type` is `Positive`, `Negative`, `Boundary`, or `Regression`.

---

## Coverage Axes

Four axes are selected, and each is selected because it changes which code path runs rather than because it is available.

| Axis                         | Values                                                                                         | Why it changes behavior here                                                                                                                                                   |
|------------------------------|------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Member count                 | One member, several members, several members in one tenant namespace                           | Payload names are derived per member, and a name derived without the member in it, or a loop variable captured by a closure, fails only past one                               |
| Namespace scope              | One tenant namespace, several tenant namespaces, and the payload's own namespace in the member | The hub-side namespace, the member's OCM namespace, and the payload's namespace are three different things (design §8.1), and conflating any two is invisible with one of each |
| Link state                   | Both halves current, the member unreachable, the control plane unreachable, both unreachable   | The two halves of a member's picture go stale independently and usually together (design §13), and the failure this design most likely presents as one symptom                 |
| Cluster topology in a member | One eligible worker, three, five or more                                                       | Composition reads the inventory summary, and a draft for a member with one eligible worker is where the node-set expansion has no answer                                       |

**Excluded: the data path.** Nothing in this design reads or writes a volume, issues a fabric connection, or touches SPDK. The topology axis is kept because composition counts workers, and it stops at counting.

**Excluded: single- versus multi-version storage groups**, as an axis. A member serving an older `storage.simplyblock.io` is one scenario (U-24, I-15) rather than a dimension, because the design's answer is to report the skew and not to adapt to it.

---

## 1. Unit Tests

No cluster. Pure functions, a fake `client.Client`, a mock control-plane HTTP server, and for the guard a CEL evaluator carrying the strings extension that Kubernetes puts in an admission policy's environment.

### The Admission Guard (§10)

Files: `fleet/internal/guard/policy_cel_test.go` and `fleet/internal/guard/allowlist_test.go`. The expressions and the allowlist both come out of the objects `internal/guard` builds, which are the objects `hack/gen-addon` renders into the shipped template, so what is tested is the payload rather than a copy of it.

| #    | Scenario                                                                                                                     | Type                | Test                                                 |
|------|------------------------------------------------------------------------------------------------------------------------------|---------------------|------------------------------------------------------|
| U-01 | Every expression the policy carries compiles, including the message and the audit annotation                                 | Positive            | `TestGuardExpressionsCompile`                        |
| U-02 | The operator's chart-installed service account is allowed                                                                    | Positive            | `TestGuardDecisions`                                 |
| U-03 | The operator's Kustomize and OLM service account is allowed                                                                  | Positive            | `TestGuardDecisions`                                 |
| U-04 | The Open Cluster Management work agent is allowed                                                                            | Positive            | `TestGuardDecisions`                                 |
| U-05 | The garbage collector is allowed, so a cascade down the ownership spine completes                                            | Positive            | `TestGuardDecisions`                                 |
| U-06 | The namespace controller is allowed, so deleting a namespace removes its objects                                             | Positive            | `TestGuardDecisions`                                 |
| U-07 | An add-on service account is allowed by its namespace group                                                                  | Positive            | `TestGuardDecisions`                                 |
| U-08 | A member of the break-glass group is allowed                                                                                 | Positive            | `TestGuardDecisions`                                 |
| U-09 | A cluster administrator with a kubeconfig is refused                                                                         | Negative            | `TestGuardDecisions`                                 |
| U-10 | A service account in an unrelated namespace is refused, so the group match does not generalize                               | Negative            | `TestGuardDecisions`                                 |
| U-11 | An identity named in the add-on namespace without the group membership is refused                                            | Negative            | `TestGuardDecisions`                                 |
| U-12 | An allowlist entry written across several lines is matched after trimming, so every entry past the first is reachable        | Boundary            | `TestRenderRoundTrips`                               |
| U-13 | A trailing separator and a blank entry yield no empty entry that an empty username would match                               | Boundary            | `TestRenderEmitsNoEmptyEntry`                        |
| U-14 | A username that is a prefix of an allowed one is refused                                                                     | Boundary            | `TestGuardDecisions`                                 |
| U-48 | The default allowlist names the work agent, the garbage collector, and the namespace controller                              | Positive            | `TestDefaultAllowlistCarriesWhatTheFleetDependsOn`   |
| U-49 | The default allowlist names all three spellings of the operator's service account                                            | Positive            | `TestDefaultAllowlistCarriesEveryOperatorSpelling`   |
| U-50 | Composing a member's operator adds one identity and does not mutate the list it was composed from                            | Positive            | `TestWithOperatorAddsOneIdentityAndKeepsTheRest`     |
| U-51 | Composing the same identity twice leaves the list unchanged, so a reconcile that runs twice is harmless                      | Boundary            | `TestWithOperatorIsIdempotent`                       |
| U-52 | A member's own operator is refused by the default list and allowed by the composed one                                       | Positive + Negative | `TestGuardAllowsAMembersOwnOperator`                 |
| U-53 | An allowlist entry carrying the separator is refused, rather than becoming two entries                                       | Negative            | `TestValidateRefusesAnEntryCarryingTheSeparator`     |
| U-54 | The default allowlist validates                                                                                              | Positive            | `TestValidateAcceptsTheDefault`                      |
| U-55 | An empty allowlist refuses every identity rather than failing evaluation                                                     | Boundary            | `TestGuardMissingAllowlistKeysDenyRatherThanError`   |
| U-56 | The allowlist's own policy carries no parameter, so a member whose allowlist is gone is not deadlocked                       | Boundary            | `TestSelfPolicyCarriesNoParameter`                   |
| U-57 | The allowlist's own policy matches the allowlist by name                                                                     | Positive            | `TestSelfPolicyMatchesTheAllowlist`                  |
| U-58 | It guards create as well as update and delete, so the allowlist cannot be rewritten in two steps                             | Negative            | `TestSelfPolicyGuardsEveryWriteToTheAllowlist`       |
| U-59 | It claims no rule on `admissionregistration.k8s.io`, which the API server never evaluates                                    | Negative            | `TestSelfPolicyClaimsNoRuleOnAdmissionConfiguration` |
| U-60 | The work agent and the break-glass group may change the allowlist, and the operator, an add-on, and an administrator may not | Positive + Negative | `TestSelfPolicyDecisions`                            |

### Payload Composition (§8.3)

File: `fleet/internal/payload/compose_test.go`.

| #    | Scenario                                                                                                       | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------|----------|------|
| U-15 | A three-worker inventory composes a document whose node sets name those workers                                | Positive | —    |
| U-16 | A five-worker inventory composes a document at the requested node count and leaves the rest out                | Positive | —    |
| U-17 | A one-worker inventory composes a single-node document                                                         | Boundary | —    |
| U-18 | An inventory with no eligible worker composes nothing, and the deployment holds with `CompositionHeld`         | Negative | —    |
| U-19 | A member whose inventory has not been reported yet holds rather than composing an empty document               | Negative | —    |
| U-20 | The composed payload states the namespace it creates objects in, and never the operator's                      | Positive | —    |
| U-21 | The payload's namespace is unrelated to the hub-side namespace the deployment was written in                   | Negative | —    |
| U-22 | Two members composed in one reconcile produce two payloads naming their own members                            | Positive | —    |
| U-23 | The configuration fingerprint changes when the document changes and is stable across a re-encode               | Positive | —    |
| U-24 | A member serving only an older version of the storage group is reported as skewed, and the payload is not sent | Negative | —    |
| U-25 | An unreadable discovery report is counted rather than failing the composition                                  | Boundary | —    |

### Delivery Status (§11.3)

File: `fleet/internal/delivery/status_test.go`.

| #    | Scenario                                                                                                        | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------|----------|------|
| U-26 | A `ManifestWork` reporting `Applied` maps to a delivery condition against the fleet object's own generation     | Positive | —    |
| U-27 | A work reporting `Applied: False` with an admission message maps to `DeliveryRefused`, and the message survives | Negative | —    |
| U-28 | A work with no conditions yet maps to a delivering phase rather than to a failure                               | Boundary | —    |
| U-29 | Feedback values for phase, step, message, and observed generation populate the remote block                     | Positive | —    |
| U-30 | A feedback value that is absent leaves its field empty rather than writing a zero that reads as an answer       | Negative | —    |
| U-31 | The member's step deadline is not carried into the hub's status                                                 | Negative | —    |
| U-32 | A remote observed generation is never compared against a hub generation                                         | Negative | —    |

### Pool References and Class Assignment (§8.4)

File: `fleet/internal/payload/class_test.go`.

| #    | Scenario                                                                            | Type     | Test |
|------|-------------------------------------------------------------------------------------|----------|------|
| U-33 | A pool reference becomes the three assignment labels on the class                   | Positive | —    |
| U-34 | The generated class carries the fleet's managed-by marker and not the cluster's     | Positive | —    |
| U-35 | Two classes drawing on one pool are both produced, and neither overwrites the other | Positive | —    |
| U-36 | A class whose pool is in another namespace of the member states that namespace      | Positive | —    |
| U-37 | A class name at the length limit is accepted, and one past it is refused            | Boundary | —    |

### Detach (§8.2)

File: `fleet/internal/controller/fleetmember_detach_test.go`.

| #    | Scenario                                                                               | Type     | Test |
|------|----------------------------------------------------------------------------------------|----------|------|
| U-38 | `Retain` patches every work to orphan before deleting it                               | Positive | —    |
| U-39 | `Delete` leaves the propagation policy alone, so the member's objects go with the work | Positive | —    |
| U-40 | The add-on is withdrawn after the works are orphaned, never before                     | Boundary | —    |
| U-41 | A detach with no works to orphan completes rather than blocking on an empty list       | Boundary | —    |
| U-42 | A detach that cannot reach the hub's own API leaves the finalizer in place             | Negative | —    |

### The Operation Target (§8.5)

File: `fleet/internal/payload/operation_test.go`.

| #    | Scenario                                                                                             | Type     | Test |
|------|------------------------------------------------------------------------------------------------------|----------|------|
| U-43 | Each of the five declared operation variants yields the storage-group object it names                | Positive | —    |
| U-44 | A target with no variant set is refused                                                              | Negative | —    |
| U-45 | A target with two variants set is refused                                                            | Negative | —    |
| U-46 | The payload is marked create-only, so a re-apply cannot write the object twice                       | Positive | —    |
| U-47 | Setting `abort` on a delivered operation edits the member's object rather than creating a second one | Positive | —    |

---

## 2. Integration Tests

The full reconcile loop against `envtest`, with the fleet CRDs, Open Cluster Management's `ManagedCluster`, `ManifestWork`, and add-on CRDs, and a mock control-plane HTTP server. There is no member: what a member would do is simulated by writing the `ManifestWork`'s status, which is exactly what the work agent does.

### Enrollment and the Add-On (§9)

File: `fleet/internal/controller/fleetmember_controller_test.go`.

| #    | Scenario                                                                                                  | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | A `FleetMember` naming an accepted `ManagedCluster` reaches `Ready` and creates the `ManagedClusterAddOn` | Positive | —    |
| I-02 | A `FleetMember` naming a cluster that is not accepted holds in `Pending` and emits `MemberNotReady`       | Negative | —    |
| I-03 | A `FleetMember` naming a cluster that does not exist holds rather than erroring in a loop                 | Negative | —    |
| I-04 | The agent's report in the member's OCM namespace is rolled into the member's link block                   | Positive | —    |
| I-05 | An agent report that stops arriving ages `lastContact` and moves the member to `Unreachable`              | Negative | —    |
| I-06 | The storage roll-up is read from the control plane and stamped with `observedAt`                          | Positive | —    |
| I-07 | A control plane that does not answer leaves the previous roll-up in place with its old `observedAt`       | Negative | —    |
| I-08 | Two members in one tenant namespace each get their own add-on and their own report                        | Positive | —    |
| I-09 | Two members in different tenant namespaces do not read each other's reports                               | Negative | —    |

### The Delivery Loop (§12.2)

File: `fleet/internal/controller/clusterdeployment_controller_test.go`.

| #    | Scenario                                                                                                              | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------|----------|------|
| I-10 | An approved deployment writes one `ManifestWork` in the member's OCM namespace, carrying the fleet's managed-by label | Positive | —    |
| I-11 | An unapproved deployment writes no work and stays in `Draft`                                                          | Negative | —    |
| I-12 | A change to the work's `Applied` condition reaches the deployment that created it                                     | Positive | —    |
| I-13 | An edit to an approved deployment's config is refused by the CRD's own validation                                     | Negative | —    |
| I-14 | Withdrawing approval is refused                                                                                       | Negative | —    |
| I-15 | A payload for a member serving an older group version is held, and the member's versions say why                      | Negative | —    |
| I-16 | The step machine resumes at its persisted step after the manager restarts mid-delivery                                | Positive | —    |
| I-17 | A second deployment for one member does not disturb the first member's work                                           | Positive | —    |
| I-18 | A `FleetOperation` writes a create-only work, and a second reconcile does not rewrite it                              | Boundary | —    |
| I-19 | Feedback on the operation's work populates the remote block and reaches a terminal phase                              | Positive | —    |

### Validation Inherited from the Storage Group (§8.3)

File: `fleet/internal/controller/clusterdeployment_validation_test.go`.

| #    | Scenario                                                                                                | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------|----------|------|
| I-20 | A config whose device selection names both NVMe addresses and block devices is refused by the fleet CRD | Negative | —    |
| I-21 | A config whose groups name different device classes is refused by the fleet CRD                         | Negative | —    |
| I-22 | A valid config is accepted, so the inherited rules do not refuse what the storage group accepts         | Positive | —    |

### Detach (§8.2)

File: `fleet/internal/controller/fleetmember_detach_controller_test.go`.

| #    | Scenario                                                                                           | Type     | Test |
|------|----------------------------------------------------------------------------------------------------|----------|------|
| I-23 | Deleting a member with `Retain` orphans every work, withdraws the add-on, and clears the finalizer | Positive | —    |
| I-24 | Deleting a member with `Delete` removes the works without orphaning                                | Positive | —    |
| I-25 | Deleting a member mid-delivery reaches `Detaching` and completes                                   | Boundary | —    |
| I-26 | Deleting a member collects its deployments, its classes, and its operations through ownership      | Positive | —    |

### The Guard, Bound in an API Server (§10)

File: `fleet/internal/guard/policy_envtest_test.go`. The suite installs the policy, its binding, and the allowlist into an `envtest` API server beside one kind of the guarded group and one outside it, and puts several requesters in front of the policy by impersonation. It exists because a policy that compiles is not a policy that installs: the unit environment declares its inputs dynamically and accepts expressions the API server's type-checker refuses.

| #    | Scenario                                                                                                                       | Type     | Test                                             |
|------|--------------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------|
| I-27 | The policy installs, its binding reaches it, and a write by an allowed identity is admitted                                    | Positive | `TestGuardInAPIServer`                           |
| I-28 | A write by an identity outside the allowlist is refused, and the message names the requester                                   | Negative | `TestGuardInAPIServer`                           |
| I-29 | A status subresource write by a refused identity is admitted, because the match excludes subresources                          | Boundary | `TestGuardInAPIServer`                           |
| I-30 | A write to a group other than `storage.simplyblock.io` is admitted                                                             | Negative | `TestGuardInAPIServer`                           |
| I-31 | The garbage collector may create and delete, so a cascade completes                                                            | Positive | `TestGuardInAPIServer`                           |
| I-32 | Removing the allowlist refuses every write, per `parameterNotFoundAction`                                                      | Negative | `TestGuardAllowlistInAPIServer`                  |
| I-33 | Under an audit-only binding, an identity the deny binding refuses is admitted                                                  | Boundary | `TestGuardAllowlistInAPIServer`                  |
| I-34 | An administrator cannot add themselves to the allowlist                                                                        | Negative | `TestGuardAllowlistInAPIServer`                  |
| I-35 | An administrator cannot delete the allowlist and write one of their own                                                        | Negative | `TestGuardAllowlistInAPIServer`                  |
| I-36 | A break-glass operator can change the allowlist, so the guard has a lever that is not deleting it                              | Positive | `TestGuardAllowlistInAPIServer`                  |
| I-37 | A member whose allowlist was deleted is restored by the fleet, and writes resume                                               | Boundary | `TestGuardAllowlistInAPIServer`                  |
| I-38 | An administrator deletes the guard's binding and writes freely afterward, because admission never covers its own configuration | Negative | `TestAdmissionCannotGuardAdmissionConfiguration` |
| I-39 | The member's work-agent grant is applied before the add-on, and the guard's four admission objects arrive                      | Positive | `TestFleetGuard`                                 |
| I-40 | Without the grant, the allowlist applies and every policy is refused, so the guard is absent rather than partial               | Negative | —                                                |

---

## 3. E2E Tests

Two Kubernetes clusters, one shared control plane, and real storage. This repository has no such harness today, so every row is a gap and is repeated in §5 as a manual scenario where it is executable by hand.

| #    | Scenario                                                                                        | Type     | Test |
|------|-------------------------------------------------------------------------------------------------|----------|------|
| E-01 | A member is enrolled, the add-on installs, and the member reports its inventory                 | Positive | —    |
| E-02 | A composed and approved deployment builds a three-node storage cluster in the member            | Positive | —    |
| E-03 | The member's storage roll-up on the hub matches what the member's own operator reports          | Positive | —    |
| E-04 | A class shipped from the hub provisions a volume in the member                                  | Positive | —    |
| E-05 | A `FleetOperation` carrying a node operation runs in the member, and its phase reaches the hub  | Positive | —    |
| E-06 | A detach with `Retain` leaves the member serving I/O and reconciling without the hub            | Positive | —    |
| E-07 | The hub is taken down while a deployment is in flight, and the delivery completes on its return | Negative | —    |
| E-08 | Two members receive different driver versions, and neither rollout disturbs the other           | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01 — The guard is rolled out onto a cluster whose writers are not fully known

**Design reference:** §16 step 4, §10.2.

**What to verify:** that the allowlist is complete before the guard enforces, and that the discovery costs nothing when it is not.

**Test concept:**

1. Enroll a member whose add-on ships the binding with `validationActions: ["Audit"]` alone.
2. Run a full working day: a driver rollout, a pool creation, a node operation, and a backup.
3. Read the `denied-requester` audit annotation out of the API server's audit log.
4. Every identity it names is either added to the allowlist or is a writer that should indeed be refused.
5. Restore `["Deny", "Audit"]` and repeat step 2, expecting no refusals.

### M-02 — The API upgrade tool runs against a guarded member

**Design reference:** §10.2, Q7.

**What to verify:** what actually happens when the tool writes under an administrator's kubeconfig with the guard enforcing.

**Current behavior:** unspecified. `operator/internal/upgrade/steps/migrate.go` and `steps/ownership.go` write objects of the group, and the tool runs with the caller's credentials.

**Test concept:**

1. Enroll a member and let the guard enforce.
2. Run `simplyblock-upgrade` against it as a cluster administrator.
3. Record which step is refused first and what the message says.
4. Repeat with the caller bound into `simplyblock:break-glass` and confirm the upgrade completes.

**Open question:** whether the break-glass binding is the answer, or the tool gains an identity of its own.

### M-03 — Both halves of a member's picture go stale at once

**Design reference:** §13, Q9.

**What to verify:** that the two timestamps stay distinguishable, and that nothing reports a stale roll-up as current.

**Test concept:**

1. With a member enrolled and reporting, partition it from both the hub and the control plane at once.
2. Read `status.link.lastContact` and `status.storage.observedAt` and confirm both age.
3. Confirm `status.phase` reaches `Unreachable` and the storage block is not cleared, since an empty block is indistinguishable from a member that holds no storage.
4. Restore the hub alone and confirm the link block refreshes while the storage block continues to age.

### M-04 — A member is detached and re-enrolled

**Design reference:** §8.2, §16.

**What to verify:** that `Retain` is genuinely reversible and that re-enrollment adopts rather than rebuilds.

**Test concept:**

1. Detach a member carrying a running storage cluster, with `spec.detachPolicy: Retain`.
2. Confirm the storage cluster keeps serving and the operator keeps reconciling.
3. Create a `FleetMember` naming the same `ManagedCluster` again.
4. Confirm the member's roll-up shows the existing cluster, and that no deployment recreates it.

### M-05 — A payload is refused by the member

**Design reference:** §5, §13.

**What to verify:** that a refused create is visible on the hub, given that it produces no object in the member.

**Test concept:**

1. Ship a `StoragePool` payload naming a storage cluster that does not exist in the member.
2. Confirm the member's webhook refuses the create.
3. Confirm the hub's deployment carries `DeliveryRefused` with the admission message, and that no remote status is invented.

---

## 5. Axis Coverage

| Axis             | Value                                 | Covered by                   |
|------------------|---------------------------------------|------------------------------|
| Member count     | One member                            | U-15, U-38, I-01, I-10, E-02 |
| Member count     | Several members                       | U-22, I-08, I-17, E-08       |
| Member count     | Several in one tenant namespace       | I-08                         |
| Namespace scope  | One tenant namespace                  | I-01, I-10                   |
| Namespace scope  | Several tenant namespaces             | I-09                         |
| Namespace scope  | The payload's namespace in the member | U-20, U-21, U-36             |
| Link state       | Both current                          | I-01, I-06, E-03             |
| Link state       | Member unreachable                    | I-05, E-07                   |
| Link state       | Control plane unreachable             | I-07                         |
| Link state       | Both unreachable                      | M-03                         |
| Cluster topology | One eligible worker                   | U-17                         |
| Cluster topology | Three                                 | U-15, E-02                   |
| Cluster topology | Five or more                          | U-16                         |

**Untested combinations, deliberately.** Member count is not crossed with cluster topology, because composition reads one member's inventory and the member loop does not reach the node-set expansion. Link state is not crossed with member count past I-08, because a partition is per member by construction and one partitioned member among several is I-05 with a second member that nothing asserts about.

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 60        | 27      | 33          |
| Integration | 40        | 13      | 27          |
| E2E         | 8         | 0       | 8           |
| Manual      | 5         | —       | —           |
| **Total**   | **108**   | **40**  | **68**      |

Every covered row is the admission guard, which is the one part of this design that is built. Twenty test functions in `fleet/internal/guard/` cover them, and `make -C fleet test` runs all of them, the API server suites included.

---

## 7. What Is Not Yet Covered

| #           | Gap                                                                           | Reason                                                                                                                                 |
|-------------|-------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------|
| U-13, U-14  | Allowlist parsing at its edges                                                | The two shipped tests cover the identities that matter and not the parser's boundaries                                                 |
| U-15 – U-47 | Composition, delivery status, class assignment, detach, and operation targets | The packages do not exist yet                                                                                                          |
| I-01 – I-33 | Every integration scenario                                                    | Needs an `envtest` harness carrying Open Cluster Management's CRDs beside the fleet's, which no suite in this repository sets up today |
| I-27 – I-33 | The guard bound in a real API server                                          | The same harness, plus an API server configured to impersonate the identities under test                                               |
| E-01 – E-08 | Every end-to-end scenario                                                     | Needs two Kubernetes clusters and one shared control plane. M-01 through M-05 are the executable subset                                |
| Q3          | Whether an approved deployment lands approved in the member                   | Undecided in the design, so there is no behavior to assert                                                                             |
| Q7          | The upgrade tool under an enforcing guard                                     | M-02 is the discovery, and the fix it points at is undecided                                                                           |
