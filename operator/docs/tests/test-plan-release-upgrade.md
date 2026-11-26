# Test Plan: Release Upgrades

Related design: [`designs/design-release-upgrade.md`](../designs/design-release-upgrade.md)  
Harness: [`operator/internal/controllers/cluster`](../../internal/controllers/cluster) for the operator actions, [`operator/internal/upgrade`](../../internal/upgrade) for the tool

Scope: the operator, the `simplyblock-upgrade` tool, and the Helm chart of this
repository. Control-plane (`sbcli`) behavior is a dependency, faked at the
boundary: the restart call, the node representation, the cluster
representation, the default images, and the write-protection call are mock
responses, and what is asserted is
this repository's response to them, including a 4xx, a 5xx, a timeout, and a
field the control plane does not report yet.

Scenario IDs are permanent: `U-` unit (no cluster: pure functions, fake
`client.Client`, mock HTTP), `I-` integration (full reconcile loop against
`envtest` and a mock control plane), `E-` end-to-end (live cluster, real data
path), `M-` manual (needs failure injection or orchestration not yet automated).
Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column
names the implementing function, or `—` when the scenario is not yet covered, and
every `—` also appears in §7. Phases 1 to 3 of the design are implemented in
[#998](https://github.com/simplyblock/simplyblock-operator/pull/998), whose tests
fill the `Test` column. Phases 4 and 5 are not implemented.

Selected axes, from the `test-scenarios` skill's `references/axes.md`:

- **Storage cluster topology** (A): single-node, three-node, and five or more,
  because the rolling restart selects and walks nodes.
- **Namespace scope** (B): the operator's namespace and `StorageCluster`s in
  other namespaces, because the rollout derives operation names from cluster
  names.
- **Cluster count** (C): zero, one, and two `StorageCluster`s, because the
  rollout loops over clusters.
- **Lifecycle and timing** (F): an operator restart and a Job pod restart in every
  step, because resume is a goal.
- **Trigger and actor** (G): the Helm hooks and the commands run by hand.
- **Component version skew** (H): a control plane that reports node images and
  one that does not, a local and a managed control plane, and a source one,
  several, or too many releases behind.

Excluded: failure domains and placement topology (D), because the walk's peer
check is unchanged by this design and is covered by the `StorageCluster` test
plan. Object scale (E) beyond the node counts above, because the upgrade touches
nodes and clusters, not volumes.

---

## 1. Unit Tests

All functions run without a cluster: the operator's helpers against a fake
`client.Client` and a mock control plane, and the tool's selection and rollout
logic against a fake client. Numbering runs continuously across the groups.

### Desired-Image Resolution (design §5.1)

File: `operator/internal/controllers/node/images_test.go` (new)

| #    | Scenario                                                                                                                                              | Type                | Test                                                         |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|--------------------------------------------------------------|
| U-01 | Node `spec.config.spdkImage` set, cluster and control-plane default set: the node's own image is desired                                              | Positive            | `TestTheDesiredImageIsTheNodesThenTheClustersThenTheDefault` |
| U-02 | Node image unset, cluster `spec.storageNodes.spdkImage` set: the cluster's image is desired                                                           | Positive            | `TestTheDesiredImageIsTheNodesThenTheClustersThenTheDefault` |
| U-03 | Node and cluster images unset: the control plane's default is desired                                                                                 | Positive            | `TestTheDesiredImageIsTheNodesThenTheClustersThenTheDefault` |
| U-04 | All three unset (the control plane reports no default): the desired image is empty, and no error is raised                                            | Boundary            | `TestNothingAskedForLeavesTheDesiredImagesEmpty`             |
| U-05 | The control plane's default endpoint returns 404 or 5xx: the desired image falls back to empty and the reconcile requeues instead of failing the node | Negative            | —                                                            |
| U-06 | The proxy image resolves from its own fields independently: node proxy set with node SPDK unset yields the node proxy and the cluster SPDK image      | Positive            | `TestTheProxyImageResolvesOnItsOwn`                          |
| U-07 | The cluster image changes: the desired image of every node without its own override changes, and of a node with one does not                          | Positive + Negative | —                                                            |

### ImagesCurrent Condition and Status (design §4.2)

File: `operator/internal/controllers/node/images_test.go` (new)

| #    | Scenario                                                                                                               | Type     | Test                                             |
|------|------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------|
| U-08 | Reported images equal desired: `imagesCurrent` true, not reported as becoming out of date                              | Positive | `TestANodeOnItsDesiredImagesIsCurrent`           |
| U-09 | Reported SPDK image differs: `imagesCurrent` false, reported once as becoming out of date (the `ImageOutOfDate` event) | Negative | `TestANodeOnOtherImagesIsOutOfDateOnce`          |
| U-10 | Only the proxy image differs: `imagesCurrent` false                                                                    | Boundary | `TestANodeOnOtherImagesIsOutOfDateOnce`          |
| U-11 | The control plane reports no images (field absent): `imagesCurrent` unset, not reported as out of date                 | Negative | `TestANodeWhoseImagesAreNotReportedIsUnknown`    |
| U-12 | The desired image is empty and the reported one set: `imagesCurrent` true, because no image is asked for               | Boundary | `TestNothingAskedForLeavesTheDesiredImagesEmpty` |
| U-13 | A repeated pass on an out-of-date node does not report it again                                                        | Negative | `TestANodeOnOtherImagesIsOutOfDateOnce`          |

### Restart Parameters (design §5.2)

File: `operator/internal/controllers/cluster/rollingrestart_test.go`, `operator/internal/controllers/node/actions_test.go`

| #    | Scenario                                                                                                               | Type     | Test                                                                                                                                                       |
|------|------------------------------------------------------------------------------------------------------------------------|----------|------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-14 | `RestartingNode` of a rolling restart sends `spdk_image` and `spdk_proxy_image` equal to the node's desired images     | Positive | `TestEveryRestartOfTheWalkCarriesTheNodesDesiredImages`                                                                                                    |
| U-15 | A `StorageNodeOps` `Restart` sends the same two fields                                                                 | Positive | `TestARestartOperationCarriesTheDesiredImages`, `TestTheRestartAfterMaintenanceCarriesTheDesiredImages`, `TestTheRelocationRestartCarriesTheDesiredImages` |
| U-16 | A desired image that is empty is omitted from the body, not sent as an empty string                                    | Boundary | `TestARestartWithoutDesiredImagesNamesNone`, `TestANodeWithoutAnObjectIsRestartedOnTheImageItRuns`                                                         |
| U-17 | The restart call returns 4xx: the step fails with the control plane's message, the mock records one call               | Negative | —                                                                                                                                                          |
| U-18 | The restart call returns 5xx or times out: the step requeues, and the write-ahead guard keeps the retry to one restart | Negative | —                                                                                                                                                          |

### Verification After Restart (design §5.3)

File: `operator/internal/controllers/cluster/rollingrestart_test.go`

| #    | Scenario                                                                                                                                         | Type     | Test                                                    |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------|
| U-19 | Node online on its desired images: the walk advances                                                                                             | Positive | `TestEveryRestartOfTheWalkCarriesTheNodesDesiredImages` |
| U-20 | Node online on another SPDK image: the operation fails with `came back on <image>, expected <image>`, one `ImageMismatch` event on the operation | Negative | `TestANodeThatCameBackOnOtherImagesFailsTheWalk`        |
| U-21 | Node online and the control plane reports no image: the walk advances without comparing                                                          | Boundary | —                                                       |

### OutOfDate Selection (design §4.3, §5.4)

File: `operator/internal/controllers/cluster/rollingrestart_test.go`

| #    | Scenario                                                                                                                              | Type     | Test                                                           |
|------|---------------------------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------------|
| U-22 | Five nodes, two out of date: the walk plans exactly those two, in the order `All` would                                               | Positive | `TestAnOutOfDateWalkRestartsOnlyTheNodesNotOnTheirImages`      |
| U-23 | No node out of date: the operation succeeds with an empty walk and `every node runs its desired images`, no shutdown call             | Boundary | `TestAnOutOfDateWalkOfACurrentFleetRestartsNothing`            |
| U-24 | A planned node becomes current before the walk reaches it: it is recorded in `current`, not `skipped`, and gets no shutdown call      | Positive | `TestANodeThatBecameCurrentBeforeTheWalkReachedItIsPassedOver` |
| U-25 | Every node's `imagesCurrent` is unset: the operation fails when the walk is planned, naming `selection All`, and no node is shut down | Negative | `TestAnOutOfDateWalkIsRefusedWhenNoNodeReportsItsImages`       |
| U-26 | Some nodes `Unknown`, some `False`: only the `False` ones are planned, the `Unknown` ones are not restarted                           | Boundary | —                                                              |
| U-27 | Selection unset: defaults to `All`, every non-leaving node is planned as today                                                        | Positive | `TestARollingRestartWalksEveryNodeAndFinishes`                 |
| U-28 | Single-node cluster, out of date: the walk plans the node, and the peer check has no peers to wait for                                | Boundary | —                                                              |
| U-29 | A node leaving the cluster and out of date: not planned, as for `All`                                                                 | Negative | —                                                              |

### SwitchWriteProtection (design §4.4)

File: `operator/internal/controllers/cluster/writeprotection_test.go` (new)

| #    | Scenario                                                                                                                                     | Type     | Test                                        |
|------|----------------------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------|
| U-30 | Cluster on `V1`, every node online: one switch call, then `Succeeded` once the cluster reports `V2`                                          | Positive | `TestTheSwitchIsMadeOnceAndFinishesOnV2`    |
| U-31 | Cluster already on `V2`: succeeds at admission with `already on v2 write protection`, zero calls                                             | Negative | `TestAClusterAlreadyOnV2IsNotSwitchedAgain` |
| U-32 | A node offline: holds in `CheckingPeers`, zero calls                                                                                         | Negative | `TestTheSwitchWaitsForEveryNodeToBeOnline`  |
| U-33 | The switch call returns 404 (control plane without the endpoint): the operation fails naming the missing endpoint                            | Negative | —                                           |
| U-34 | The switch call times out and is retried: the write-ahead guard keeps it to one call                                                         | Negative | —                                           |
| U-74 | The switch call answers `409` (a node rejected it): the step holds, calls again on the next pass, and succeeds once the cluster reports `V2` | Negative | —                                           |

### Release Detection and Module Selection (design §6.1, §6.4)

File: `operator/internal/upgrade/releases/releases_test.go` (new)

| #    | Scenario                                                                                                 | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------|----------|------|
| U-35 | Source from the deployed Helm release's chart version, target from the stamped version                   | Positive | —    |
| U-36 | No Helm release and no `--from`: refused naming `--from`                                                 | Negative | —    |
| U-37 | Source equal to target: succeeds with nothing to do, no step runs                                        | Boundary | —    |
| U-38 | Source newer than target: refused as a downgrade                                                         | Negative | —    |
| U-39 | Modules at `b7`, `b8`, `rc1`, source `b6.2`, target `b8`: selects `b7` and `b8` in that order, not `rc1` | Positive | —    |
| U-40 | A module at exactly the source release is not selected, one at exactly the target is                     | Boundary | —    |
| U-41 | Prerelease ordering: `26.4.0-beta6.2` < `26.4.0-beta7` < `26.4.0-rc1` < `26.4.0`                         | Boundary | —    |
| U-42 | A selected module's `MinimumSource` is newer than the source: refused, naming the newest floor           | Negative | —    |
| U-43 | Two modules with floors, source between them: refused naming the higher floor                            | Boundary | —    |
| U-44 | Any kind still stored at `v1alpha1`: refused with the instruction to run the API migration               | Negative | —    |

### Generic Sequence and Hook Points (design §6.3, §6.5)

File: `operator/internal/upgrade/releases/sequence_test.go` (new)

| #    | Scenario                                                                                                                           | Type                | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-45 | Two selected modules each with an `AfterStoragePlane` step: the older module's step runs first                                     | Positive            | —    |
| U-46 | A step registered at `AfterCRDs` runs in `prepare` and never in `rollout`                                                          | Negative            | —    |
| U-47 | A `StorageCluster` not `Active`, a node not `online`, or an operation `Running`: the preflight reports an `Error` finding for each | Negative            | —    |
| U-48 | A target image not pullable: an `Error` finding naming the image                                                                   | Negative            | —    |
| U-49 | `--skip` on a check: the check does not run and the report names it as skipped                                                     | Positive            | —    |
| U-50 | `release plan` and `release preflight` against the read-only client perform zero writes                                            | Negative            | —    |
| U-51 | Managed control plane at or above the target: step 5 is skipped. Below the target: refused                                         | Positive + Negative | —    |

### Rollout Idempotency (design §8)

File: `operator/internal/upgrade/releases/rollout_test.go` (new)

| #    | Scenario                                                                                                                                   | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-52 | No operation exists: the rollout creates `upgrade-<target>` and waits                                                                      | Positive | —    |
| U-53 | The operation exists and `Succeeded`: skipped, no create                                                                                   | Negative | —    |
| U-54 | The operation exists and is `Running`: waited for, no second create                                                                        | Negative | —    |
| U-55 | The operation `Failed` or `Aborted`: the rollout stops with its message and creates nothing after it                                       | Negative | —    |
| U-56 | Two clusters: the second cluster's restart is created only after the first `Succeeded`                                                     | Positive | —    |
| U-57 | Zero `StorageCluster`s: the storage-plane step succeeds with nothing to do                                                                 | Boundary | —    |
| U-58 | A cluster name whose derived operation name exceeds 63 characters: shortened by the framework's derivation, unique, and stable across runs | Boundary | —    |
| U-59 | Two clusters of the same name in different namespaces: their operations land in their own namespaces and do not collide                    | Boundary | —    |

### Release Module 26.4.0-beta6 (design §6.6)

File: `operator/internal/upgrade/releases/r26_4_0_beta7/module_test.go` (new)

| #    | Scenario                                                                                                                                                       | Type                | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-60 | An lblk cluster with a pending reweight: `reweight-pending-clear` reports an `Error`. None pending: no finding                                                 | Positive + Negative | —    |
| U-61 | A non-lblk cluster: `reweight-pending-clear` does not query it                                                                                                 | Negative            | —    |
| U-62 | Target SPDK image in the module's unsupported table: `spdk-image-supports-sizing` refuses. A supported one passes                                              | Positive + Negative | —    |
| U-63 | MDS StatefulSet with a `state` claim template: `pnfs-mds-legacy-state` deletes it with orphaned pods, the claims, the StorageClass, and the policy and binding | Positive            | —    |
| U-64 | MDS without the claim template, or no MDS: the step is skipped, zero deletes                                                                                   | Negative            | —    |
| U-65 | A cluster on `V1`: `write-protection-v2` creates the switch and no rolling restart. On `V2`: nothing                                                           | Positive + Negative | —    |
| U-66 | Release values carrying `driver.pnfs.mds.stateSize`: a warning finding, not an error                                                                           | Positive            | —    |

### Chart Rendering (design §7.2 to §7.4)

File: a chart-rendering test with the Helm SDK, as `operator/internal/upgrade/helm` already renders charts (new)

| #    | Scenario                                                                                                                               | Type       | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-67 | `helm upgrade`: the pre-upgrade hook Job, its ServiceAccount, role, and binding render with the hook annotations and ascending weights | Positive   | —    |
| U-68 | `helm install`: neither the hook nor the rollout Job renders                                                                           | Negative   | —    |
| U-69 | `upgrade.enabled: false`: neither renders. `upgrade.rollout.enabled: false`: only the hook renders                                     | Negative   | —    |
| U-70 | The rollout Job's name carries the target version, and `upgrade.skip` reaches the command line                                         | Positive   | —    |
| U-71 | A live `ControlPlane` exists: `spec.source.local.image` renders from it, not from `image.simplyblock`                                  | Positive   | —    |
| U-72 | No live `ControlPlane`: the image renders from `image.simplyblock`                                                                     | Boundary   | —    |
| U-73 | Values carrying a key the previous release removed pass the schema                                                                     | Regression | —    |

### Cluster Record, Client, Conversion, and Admission (design §4.1, §4.4, §12, §14)

Files: `operator/internal/controllers/cluster/upgrade_status_test.go`,
`operator/internal/controllers/cluster/writeprotection_test.go`,
`operator/internal/controllers/node/images_test.go`,
`operator/api/v1alpha1/hub_roundtrip_test.go`,
`operator/internal/webhook/stretched_admission_test.go`,
`atlas-lib/controlplane/clusters_upgrade_test.go`

| #    | Scenario                                                                                                                                            | Type                | Test                                                                                                                                                                                                                    |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-75 | The streamed cluster record's write-protection generation and default images reach `StorageCluster.status`: `V2`, `V1`, and empty when not reported | Positive + Negative | `TestTheStreamedWriteProtectionAndDefaultImagesReachTheStatus`                                                                                                                                                          |
| U-76 | The same fields read directly while the stream has no snapshot                                                                                      | Positive            | `TestTheDirectlyReadWriteProtectionAndDefaultImagesReachTheStatus`                                                                                                                                                      |
| U-77 | A control plane that does not report write protection fails the switch, with no call                                                                | Negative            | `TestAControlPlaneThatDoesNotReportWriteProtectionFailsTheSwitch`                                                                                                                                                       |
| U-78 | A switch the control plane refused with `409` is not made again within its claim, and the operation stays `Running`                                 | Negative            | `TestARefusedSwitchIsNotRepeatedWithinItsClaim`                                                                                                                                                                         |
| U-79 | `SwitchWriteProtection` on a stretched cluster's follower site is refused at admission                                                              | Negative            | `TestAFollowerRefusesClusterWideOperations`                                                                                                                                                                             |
| U-80 | A `selection: OutOfDate` walk and its `current` list survive a `v1alpha1` round trip                                                                | Regression          | `TestStorageClusterOpsOutOfDateWalkRoundTripsFromTheHub`                                                                                                                                                                |
| U-81 | The out-of-date gauge reads 1 for a node whose `imagesCurrent` is false, and 0 when true or unset                                                   | Positive + Boundary | `TestTheOutOfDateGaugeFollowsTheNode`                                                                                                                                                                                   |
| U-82 | The streamed node carries its images into the reading the node controllers use                                                                      | Positive            | `TestTheStreamedNodeCarriesItsImages`                                                                                                                                                                                   |
| U-83 | The atlas-lib client reads the default images and the generation, nil from an older control plane, and posts the switch, surfacing a `409`          | Positive + Negative | `TestGetClusterReadsTheDefaultImagesAndWriteProtection`, `TestGetClusterFromAnOlderControlPlaneReportsNoImages`, `TestSwitchWriteProtectionPostsTheSwitch`, `TestSwitchWriteProtectionReportsASwitchThatDidNotComplete` |

---

## 2. Integration Tests

The full reconcile loop against `envtest` and a mock control plane, with the
operator restarted between steps where a row says so.

### Image Convergence (design §5)

| #    | Scenario                                                                                                                                                                                                        | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | Setting `spec.storageNodes.spdkImage` on a three-node cluster marks every node `ImagesCurrent=False`, and a `RollingRestart` `OutOfDate` restarts all three with the image and ends with every condition `True` | Positive | —    |
| I-02 | The same with the control-plane default changed instead of the cluster field                                                                                                                                    | Positive | —    |
| I-03 | A walk failed on node two of five by a mismatch, then rerun with `OutOfDate`: restarts nodes two to five only (mock call count per node)                                                                        | Positive | —    |
| I-04 | The operator restarted in each step of an `OutOfDate` walk: each node is restarted exactly once                                                                                                                 | Negative | —    |
| I-05 | A `StorageNodeOps` `Restart` of one node moves that node, and a later `OutOfDate` walk passes over it as current                                                                                                | Positive | —    |

### SwitchWriteProtection (design §4.4)

| #    | Scenario                                                                                                      | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------|----------|------|
| I-06 | `V1` cluster: switch call, cluster status reaches `V2`, `Succeeded`                                           | Positive | —    |
| I-07 | The operator restarted in `Switching` after the call: no second call, completes when the cluster reports `V2` | Negative | —    |
| I-08 | A `RollingRestart` running on the cluster: the switch waits on the cluster's lock and does not interleave     | Negative | —    |

### Rollout Against the Operator (design §6.5, §8)

| #    | Scenario                                                                                                                                      | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-09 | `release rollout` against `envtest` with the operator running: creates the control-plane upgrade, then one restart per cluster, then succeeds | Positive | —    |
| I-10 | The rollout process killed after the control-plane upgrade and started again: no second `ControlPlaneOps`, continues with the clusters        | Negative | —    |
| I-11 | A cluster's restart fails: the rollout stops, the next cluster's restart is never created                                                     | Negative | —    |

---

## 3. E2E Tests

Run against a live cluster with `fio` in verify mode on volumes of every cluster,
so a data-path error fails the row, not only an interrupted workload.

### Upgrade Through Helm (design §7)

| #    | Scenario                                                                                                                                                                | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Three-node cluster, `helm upgrade` from the previous release with the hooks: the rollout Job succeeds, every node runs the target images, `fio` verify reports no error | Positive | —    |
| E-02 | Five-node cluster and a second `StorageCluster`: both clusters converge, one after the other                                                                            | Positive | —    |
| E-03 | A failing check (an operation left `Running`): `helm upgrade` fails, the release revision is unchanged, and no CRD was modified                                         | Negative | —    |
| E-04 | The rollout Job's pod deleted during the rolling restart: the replacement resumes and no node restarts twice                                                            | Negative | —    |
| E-05 | The same sequence run by hand with `--no-hooks` (§7.5) ends in the same state as E-01                                                                                   | Positive | —    |
| E-06 | Upgrade from `26.4.0-beta6.2` with the pNFS MDS enabled: the legacy state is removed, the MDS comes back with its standby, write protection ends on `V2`                | Positive | —    |
| E-07 | Single-node cluster: the upgrade completes and the volume is unavailable only while its node restarts                                                                   | Boundary | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01: The control plane restarts a node on its stored image after a failure

**Design reference:** §5.2

**What to verify:** a node the operator restarted onto a new image keeps that
image when the control plane later restarts it on its own (after a node crash,
for example), because the control plane stores the image a restart started it with.

**Test concept:**
1. Move a node to a new SPDK image with a `StorageNodeOps` `Restart`.
2. Kill the node's SPDK container so the control plane's own monitor restarts it.
3. The node comes back on the new image, and `ImagesCurrent` stays `True`.

### M-02: A Kubernetes node is drained during the rollout

**Design reference:** §8, §11

**What to verify:** a drained worker holds the walk on its peer check and the
rollout waits without failing, then continues once the worker returns.

**Test concept:**
1. Start an upgrade on a three-node cluster.
2. During the restart of node one, cordon and drain the worker of node two.
3. The walk holds in `CheckingPeers` with the peer message, the rollout Job keeps
   waiting, and both complete after the worker is uncordoned.

---

## 5. Axis Coverage

| Axis                     | Values covered                                                                                  | IDs                                      | Not covered                                  |
|--------------------------|-------------------------------------------------------------------------------------------------|------------------------------------------|----------------------------------------------|
| Storage cluster topology | 1 node, 3 nodes, 5 nodes                                                                        | U-28, E-07, I-01, E-01, U-22, I-03, E-02 | —                                            |
| Namespace scope          | operator namespace, clusters in other namespaces, same name in two namespaces                   | U-52, U-59, E-02                         | —                                            |
| Cluster count            | 0, 1, 2                                                                                         | U-57, E-01, U-56, E-02                   | —                                            |
| Lifecycle and timing     | operator restart per step, Job pod restart, rollout process restart                             | I-04, I-07, E-04, I-10                   | operator restart during the pre-upgrade hook |
| Trigger and actor        | Helm hooks, commands by hand                                                                    | E-01, E-05                               | OLM-installed deployment                     |
| Version skew             | images reported or not, local or managed control plane, 0, 1, 2+ releases behind, below a floor | U-11, U-21, U-25, U-51, U-37, U-39, U-42 | managed control plane end to end             |

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered                                                                   |
|-------------|-----------|---------|-------------------------------------------------------------------------------|
| Unit        | 83        | 33      | U-05, U-07, U-17, U-18, U-21, U-26, U-28, U-29, U-33, U-34, U-74, U-35 … U-73 |
| Integration | 11        | 0       | I-01 … I-11                                                                   |
| E2E         | 7         | 0       | E-01 … E-07                                                                   |
| Manual      | 2         | 0       | M-01, M-02                                                                    |

---

## 7. What Is Not Yet Covered

| #                | Gap                                                                                            | Reason                                                                                                                                       |
|------------------|------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------|
| U-05             | A failed read of the default images                                                            | Resolution reads `StorageCluster.status.defaultSpdkImage`, which a failed control-plane read leaves at its last value. No test pins that yet |
| U-07             | A changed cluster image moves every node without an override                                   | The resolution is tested per node (U-01 to U-03). A reconcile-level test across several nodes is not written                                 |
| U-17, U-18       | Restart call answers 4xx, 5xx, or times out                                                    | The restart sites keep their existing error handling, which the existing restart tests cover. No image-specific test                         |
| U-21             | A node whose images the control plane does not report                                          | The comparison is skipped. No test pins it                                                                                                   |
| U-26, U-28, U-29 | Mixed reported and unreported nodes, single-node cluster, leaving nodes in an `OutOfDate` walk | Not written yet                                                                                                                              |
| U-33, U-34, U-74 | Switch call answering 404, timing out, or succeeding after a `409` once the claim lapses       | U-77 and U-78 cover the neighboring cases. The lease-expiry retry needs the claim's clock injected                                           |
| U-35 … U-66      | Release modules and the rollout (design §6, §8)                                                | Phase 4 is not implemented                                                                                                                   |
| U-67 … U-73      | Chart rendering                                                                                | Phase 5 is not implemented, and the repository has no chart-rendering test harness yet                                                       |
| I-01 … I-11      | Every integration scenario                                                                     | Not written. The envtest harness does not run on the machine these tests were written on                                                     |
| E-01 … E-07      | Every end-to-end scenario                                                                      | Phases 4 and 5 are not implemented                                                                                                           |
| —                | Operator restart during the pre-upgrade hook                                                   | The hook runs before the new operator exists, and the old operator restarting changes nothing the hook reads                                 |
| —                | OLM-installed deployment                                                                       | Out of scope for the hooks (design §2). The manual path E-05 is the same commands                                                            |
| —                | Managed control plane end to end                                                               | Needs a hub deployment in the e2e environment                                                                                                |
