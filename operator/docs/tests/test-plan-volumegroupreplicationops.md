# Test Plan: Fencing a Superseded Replication Source

Related design: [`designs/design-volumegroupreplicationops.md`](../designs/design-volumegroupreplicationops.md)  
Harness: [`internal/controllers/replication`](../../internal/controllers/replication), [`atlas-lib/controlplane`](../../../atlas-lib/controlplane)

Scope: the operator, the CSI driver, and the Kubernetes surface of this
repository. Control-plane (`sbcli`) behavior is a dependency, faked at the
boundary.

Scenario IDs are permanent: `U-` unit (no cluster: pure functions, fake
`client.Client`, mock HTTP), `I-` integration (full reconcile loop against
`envtest` and a mock backend), `E-` end-to-end (live sites, real data path), `M-`
manual. Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The
`Test` column names the implementing function, or `—` when the scenario is not yet
covered. Every `—` also appears in §7.

---

## 1. Unit Tests

The control-plane client is tested against an HTTP test server, and the
reconciler against a fake `client.Client` and a fake control-plane client.

### Control-Plane Client (design §5.2)

File: `atlas-lib/controlplane/groupreplication_test.go`, `atlas-lib/controlplane/replication_test.go`, `atlas-lib/controlplane/consistencygroups_members_test.go`

| #    | Scenario                                                                            | Type     | Test                                            |
|------|-------------------------------------------------------------------------------------|----------|-------------------------------------------------|
| U-01 | `FenceGroup` posts the group demote with `superseded=true` and returns nil on 204   | Positive | `TestFenceGroupOrdersASupersededDemote`         |
| U-02 | `FenceGroup` returns a `*StatusError` carrying 202, 400, or 500                     | Negative | `TestFenceGroupAnythingButDoneIsAnError`        |
| U-03 | `FenceVolume` posts the volume demote with `superseded=true` and returns nil on 204 | Positive | `TestClientFenceVolumeOrdersASupersededDemote`  |
| U-04 | `FenceVolume` returns an error on 202, 400, or 500                                  | Negative | `TestClientFenceVolumeAnythingButDoneIsAnError` |
| U-26 | `ConsistencyGroupMembers` lists the current members only, and an empty group as an empty list | Positive | `TestConsistencyGroupMembersListsTheCurrentOnes` |

### Resolving the Group (design §5.1)

File: `internal/controllers/replication/volumegroupreplicationops_controller_test.go`

| #    | Scenario                                                                                                         | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------|----------|------|
| U-05 | The `VolumeGroupReplication` and its content resolve to `cg:<cluster>:<group>`, copied into `status.groupHandle` | Positive | `TestFence_ResolvesTheGroupAndFencesIt` |
| U-06 | The `VolumeGroupReplication` does not exist → `Failed`, message names it, no control-plane call                  | Negative | `TestFence_UnresolvableTargetFails` |
| U-07 | The content object is missing → `Failed`, no control-plane call                                                  | Negative | `TestFence_UnresolvableTargetFails` |
| U-08 | The handle is not `cg:<cluster>:<group>` (a volume handle, empty) → `Failed`, message quotes it                  | Negative | `TestFence_UnresolvableTargetFails` |
| U-09 | The csi-addons kind is not served → `Failed`, message names the kind                                             | Negative | `TestFence_UnresolvableTargetFails` |
| U-10 | `status.groupHandle` already set → the `VolumeGroupReplication` is not read again                                | Positive | `TestFence_ActsOnTheRecordedHandle` |

### Ordering the Fence (design §5.2, §6)

File: `internal/controllers/replication/volumegroupreplicationops_controller_test.go`

| #    | Scenario                                                                         | Type     | Test |
|------|----------------------------------------------------------------------------------|----------|------|
| U-11 | 204 → `Reporting`, then `Succeeded` / `Completed`, `GroupFenced` event           | Positive | `TestFence_ResolvesTheGroupAndFencesIt` |
| U-12 | Connection error → stays `Fencing`, requeued, `FenceRetrying` event              | Negative | `TestFence_RetryableAnswersKeepFencing` |
| U-13 | 5xx and 409 → stays `Fencing`, requeued                                          | Negative | `TestFence_RetryableAnswersKeepFencing` |
| U-14 | 400 and 404 → `Failed`, message carries the detail, `FenceFailed` event          | Negative | `TestFence_PermanentRefusalFails` |
| U-15 | `Fencing` past its deadline → `Failed`                                           | Boundary | `TestFence_StepDeadline` |
| U-16 | `Fencing` one second before its deadline and a retryable error → still `Fencing` | Boundary | `TestFence_StepDeadline` |
| U-17 | Restart in `Fencing` → one more call, then `Succeeded`                           | Positive | `TestFence_ActsOnTheRecordedHandle` |
| U-18 | A terminal operation is not requeued and makes no call                           | Positive | `TestFence_TerminalOperationIsInert` |
| U-19 | Every status write sets `observedGeneration`                                     | Positive | `TestFence_StatusCarriesObservedGeneration` |

### Reporting the Members (design §5.3)

| #    | Scenario                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------|----------|------|
| U-20 | The member list is recorded with `fenced: true` per member                         | Positive | `TestFence_ResolvesTheGroupAndFencesIt` |
| U-21 | The member list cannot be read → `Succeeded`, `members` empty, the message says so | Negative | `TestFence_MemberReport` |
| U-22 | A group with no members → `Succeeded`, `members` empty                             | Boundary | `TestFence_MemberReport` |

### Admission (design §4.3)

File: `internal/controllers/replication/cel_validation_test.go`

| #    | Scenario                                                               | Type     | Test |
|------|------------------------------------------------------------------------|----------|------|
| U-23 | An update changing `volumeGroupReplicationRef` or `action` is rejected | Negative | —    |
| U-24 | An action other than `Fence` is rejected at create                     | Negative | —    |
| U-25 | An empty `volumeGroupReplicationRef` is rejected at create             | Boundary | —    |

---

## 2. Integration Tests

The reconcile loop runs against `envtest`, with the csi-addons CRDs installed and a
mock control plane.

| #    | Scenario                                                                                                                  | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | A `Fence` operation for an existing `VolumeGroupReplication` reaches `Succeeded`, and the mock saw exactly one fence call | Positive | —    |
| I-02 | The `VolumeGroupReplication` is deleted after `Validating` → the operation still reaches `Succeeded` on the copied handle | Positive | —    |
| I-03 | The csi-addons CRDs are not installed → the operator starts, and the operation reaches `Failed`                           | Negative | —    |
| I-04 | Two operations for one group → both `Succeeded`                                                                           | Positive | —    |

---

## 3. E2E Tests

Two sites with split control planes and the DR stack on a hub
(`simplyblock-dr/test/realbed/splitdr.sh`). These rows are destructive: a site is
partitioned.

| #    | Scenario                                                                                                                                                                                                   | Type     | Test                                                                              |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------------------------------------------|
| E-01 | After a failover with the source partitioned and healed, the fence order is accepted, every member of the old group stops replicating, and the new site holds no copy of the old group made after the heal | Positive | `splitdr.sh fence` (the order is a direct control-plane call until Phase 1 ships) |
| E-02 | The same, with the order as a `VolumeGroupReplicationOps` created on the healed site                                                                                                                       | Positive | —                                                                                 |
| E-03 | The same, with the order written by the hub into the site's `ManifestWork` before the heal                                                                                                                 | Positive | —                                                                                 |
| E-04 | After the fence, the application is protected again from the new site (`lastGroupSyncTime` past the heal)                                                                                                  | Positive | `splitdr.sh reprotect`                                                            |

---

## 4. Phase 2 (Planned)

| #       | Scenario                                                                                               |
|---------|--------------------------------------------------------------------------------------------------------|
| U-P2-01 | `PersistentVolumeOps` with `action: Fence` reads the volume handle from the PV and calls `FenceVolume` |
| U-P2-02 | A PV whose handle is a group handle or not a simplyblock volume → `Failed`                             |
| E-P2-01 | An ungrouped volume failed over with its site partitioned is fenced after the heal                     |

---

## 5. Manual Scenarios and Test Concepts

### M-01: The fence arrives after Ramen's demote has started a final snapshot

**Design reference:** §5.2, P0-1, P0-2

**What to verify:** the new primary refuses the final snapshot's transfer, the
fence then marks every member demoted and cancels the transfer, and Ramen's next
`DemoteVolume` succeeds.

**Test concept:** the steps below run on a two-site bed with split control planes.

1. An application is failed over with its site partitioned.
2. The site is healed, and the CSI driver's first `DemoteVolume` for the group
   appears in the site's CSI controller log.
3. A `VolumeGroupReplicationOps` with `action: Fence` is created on the site.
4. The new site's control plane has logged the refusal, the operation is
   `Succeeded`, and the DRPC on the hub reaches `PeerReady`.

---

## 6. Axis Coverage

| Axis                        | Values covered                                     | IDs              | Not covered                                  |
|-----------------------------|----------------------------------------------------|------------------|----------------------------------------------|
| Who orders                  | direct call, `Ops` on the site, hub `ManifestWork` | E-01, E-02, E-03 | a third-party detector (same object as E-02) |
| Order versus Ramen's demote | after the heal, before Ramen's demote, after it    | E-01, M-01       | —                                            |
| Group size                  | 0, 1, 2+ members                                   | U-22, U-20, E-01 | —                                            |
| Lifecycle                   | restart mid-step, terminal re-reconcile            | U-17, U-18       | —                                            |
| csi-addons presence         | served, not served                                 | I-01, I-03       | —                                            |

---

## 7. What Is Not Yet Covered

| #           | Gap                                        | Reason                                                |
|-------------|--------------------------------------------|-------------------------------------------------------|
| U-23 … U-25 | Admission scenarios | Need the envtest CEL harness of the operator package |
| I-01 … I-04 | Integration scenarios                      | Phase 1 not implemented                               |
| E-02, E-03  | The order as an object and through the hub | Phase 1 not implemented. E-03 also needs the hub side |
| —           | A third-party detector as the orderer      | It creates the same object as E-02                    |
