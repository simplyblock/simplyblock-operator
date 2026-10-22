# Test Plan: One Storage Cluster across Two Kubernetes Clusters, driven by RamenDR

Related design: [`designs/design-stretched-sync-replication-ramen.md`](../designs/design-stretched-sync-replication-ramen.md)

Scope is the operator's stretched role machine, the driver's birth-site placement, the hub operator's expansion of a `StretchedCluster`, the site link (relay, forwarder, and peers), the driver's stage-time determination and `NetworkFence` service, and the switchover service the operator serves. The backend's two-site model (site records, the liveness proof, planned and forced promotes, site return) is the control plane's to prove on the `sync-replication` branch and is exercised here only at the boundary, through a mock that answers the sync routes. RamenDR and OCM are stock upstream and are not re-tested; what is tested is this repository's conformance to what they drive.

Scenario IDs are permanent and are never reused or renumbered. `U-` is unit (no cluster: mock control plane, fake `client.Client`, an in-process link), `I-` is integration (two operator managers against `envtest` and one mock backend), `E-` is end-to-end (the lab of design Appendix D: a hub and two sites), and `M-` is manual. Types are `Positive`, `Negative`, `Boundary`, and `Regression`. A `—` in the `Test` column means nothing implements the scenario yet, and every such row reappears in §7 with its reason.

Nothing below is implemented. Every row is a requirement, and the `Test` column fills in as the phases of the design's Phasing Overview land.

---

## 1. Unit Tests

Pure functions, fake clients, and a mock control plane answering the sync routes. The link scenarios run a relay, two agents, and their peers in one process. Numbering runs continuously across the groups.

### Stretched role machine on `StorageCluster` (§5.2, §11.1)

File: `operator/internal/controllers/cluster/storagecluster_stretched_test.go`

| #    | Scenario                                                                                                                                                            | Type     | Test                                                                                                                                         |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 | A `Leader` with no `clusterRef` runs the create path: one `POST /clusters/` with `sync_replication: true` (`leader_site` waits on P0-2)                             | Positive | `TestALeaderCreatesASyncReplicationCluster`                                                                                                  |
| U-02 | A `Follower` adopts through the delivered `simplyblock-{name}-upgrade` Secret: no create is posted, `simplyblock-cluster-{name}` is written, status is populated    | Positive | `TestAFollowerAdoptsThroughTheDeliveredCredentials`                                                                                          |
| U-03 | A `Follower` whose adoption finds no cluster of that name holds in `CheckingControlPlane` with reason `PeerClusterMissing` and requeues                             | Negative | `TestAFollowerWaitsForItsLeaderInsteadOfCreating`, `TestAFollowerWaitsForItsCredentialsWhenTheClusterExists`                                 |
| U-04 | A `Follower` whose adoption finds a cluster without `sync_replication` refuses with a terminal condition naming the mismatch                                        | Negative | `TestAFollowerRefusesAClusterThatIsNotASyncCluster`                                                                                          |
| U-05 | A `Follower` whose adoption finds `leader_site` equal to its own site refuses with `RoleConflict`                                                                   | Negative | —                                                                                                                                            |
| U-06 | The `Leader`'s activation holds with `WaitingForPeerSite` while the listing shows fewer than `ndcs + npcs` online nodes on the peer site, and proceeds once it does | Boundary | `TestALeaderWaitsForThePeerSiteBeforeActivating`                                                                                             |
| U-07 | Exactly `ndcs + npcs` online peer nodes passes the hold; one fewer does not                                                                                         | Boundary | `TestALeaderWaitsForThePeerSiteBeforeActivating`                                                                                             |
| U-08 | A role swap from `Leader` to `Follower` fails an in-flight cluster-scoped `StorageClusterOps` with `RoleLost` and emits `RoleChanged`                               | Positive | `TestARoleSwapEndsAStoppableClusterWideOperation`, `TestARoleSwapLeavesAnUnstoppableOperationRunning`, `TestARoleSwapIsRecordedAndAnnounced` |
| U-09 | A role swap from `Follower` to `Leader` admits the next cluster-scoped `StorageClusterOps` and emits `RoleChanged`                                                  | Positive | `TestALeaderAndAStandaloneClusterAdmitEveryAction`, `TestARoleSwapIsRecordedAndAnnounced`                                                    |
| U-10 | `spec.stretched` set without `enableSyncReplication` is refused by the schema (CEL)                                                                                 | Negative | `TestStorageClusterStretchedRequiresSyncReplication`                                                                                         |
| U-11 | `spec.stretched.site` and `spec.stretched.peerSite` are immutable; a change is refused                                                                              | Negative | `TestStorageClusterStretchedSitesAreImmutableAndTheRoleIsNot`                                                                                |
| U-12 | Every `StorageNode` the cluster adds carries `site` from `spec.stretched.site` in its add body                                                                      | Positive | `TestAGroupWithoutASiteTakesTheStretchedClustersSite`                                                                                        |
| U-13 | A `NodeGroup` in the delivered config whose `site` differs from the cluster's is refused                                                                            | Negative | `TestANodeGroupOnAnotherSiteIsRefused`, `TestANodeGroupAddedToAStretchedClusterMustBeOnItsSite`                                              |

### `StorageClusterOps` admission on a follower (§5.2)

File: `operator/internal/webhook/storageclusterops_stretched_test.go`

| #    | Scenario                                                                                                       | Type       | Test                                                                                             |
|------|----------------------------------------------------------------------------------------------------------------|------------|--------------------------------------------------------------------------------------------------|
| U-14 | `Activate`, `Expand`, `RollingRestart`, and `Shutdown` on a follower's cluster are refused with `FollowerSite` | Negative   | `TestAFollowerRefusesClusterWideOperations`                                                      |
| U-15 | The same actions on a leader's cluster are admitted                                                            | Positive   | `TestALeaderAndAStandaloneClusterAdmitEveryAction`                                               |
| U-16 | A `StorageNodeOps` on a follower's own node is admitted                                                        | Positive   | `TestAnOperationOnARemoteNodeIsRefused`                                                          |
| U-17 | A standalone cluster (no `spec.stretched`) is admitted for every action, unchanged                             | Regression | `TestALeaderAndAStandaloneClusterAdmitEveryAction`, `TestAStandaloneClusterHasNoStretchedStatus` |

### `StretchedCluster` expansion on the hub (§5.1, Appendix A)

File: `operator/internal/controller/stretchedcluster_controller_unit_test.go`

| #    | Scenario                                                                                                                                                                                                                                                                                                                                    | Type                | Test                                                                                                    |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|---------------------------------------------------------------------------------------------------------|
| U-18 | One `StretchedCluster` produces two `ManifestWork`s, one per managed cluster, each carrying an `OperatorOps` discovery scoped by that site's `discover`                                                                                                                                                                                     | Positive            | `TestAStretchedClusterDeliversADiscoveryToEachSite`                                                     |
| U-19 | Once a site's view shows a draft with nodes, its work carries the sizing: the shared `sizing`, `enableSyncReplication: true`, `stretched.site`, `.peerSite`, and `.role`; neither names a `clusterRef`                                                                                                                                      | Positive            | `TestEachDraftIsSizedAsOneSiteOfASyncCluster`                                                           |
| U-20 | `enableSyncReplication` is forced `true` on both drafts whatever the sizing says                                                                                                                                                                                                                                                            | Positive            | `TestEachDraftIsSizedAsOneSiteOfASyncCluster`                                                           |
| U-21 | `approved: true` on the hub delivers the leader's approval at once and the follower's only after the leader's view carries a cluster UUID                                                                                                                                                                                                   | Boundary            | `TestTheFollowerIsApprovedOnlyOnceTheLeadersClusterExists`                                              |
| U-22 | Two `Leader` entries are refused by CEL; zero `Leader` entries are refused                                                                                                                                                                                                                                                                  | Negative            | `TestStretchedClusterNeedsExactlyOneLeader`                                                             |
| U-23 | A role swap while either site's `StorageCluster` view is not `Ready` is refused by the webhook; a swap on a ready pair re-delivers both works                                                                                                                                                                                               | Negative + Positive | `TestARoleSwapIsRefusedUntilBothSitesAreOnline`, `TestARoleSwapOnAReadyPairIsAdmitted`                  |
| U-24 | `cluster` and `site` are immutable after creation                                                                                                                                                                                                                                                                                           | Negative            | `TestStretchedClusterClustersAndSitesAreImmutable`                                                      |
| U-25 | A node group without a site takes its cluster's `stretched.site` in the nodes it expands into, and is not held as `MissingSites`                                                                                                                                                                                                            | Positive            | `TestAGroupWithoutASiteTakesTheStretchedClustersSite`                                                   |
| U-93 | `approved` cannot go from true to false                                                                                                                                                                                                                                                                                                     | Negative            | `TestStretchedClusterApprovalIsOneWayAndFreezesDiscovery`                                               |
| U-94 | A change to a site's `discover` runs another discovery on that site only, rewriting its draft while unapproved; refused once approved                                                                                                                                                                                                       | Boundary            | `TestStretchedClusterApprovalIsOneWayAndFreezesDiscovery`, `TestAChangedDiscoveryReRunsThatSiteOnly`    |
| U-95 | Every `ManifestWork` the hub writes, including the draft's server-side apply once a draft names nodes, is admitted by OCM's published `ManifestWork` schema (the apply's field manager carries the `work-agent` prefix). Regression: 2026-10-22-work-fieldmanager-pattern, the hub's work was refused and the request held in `Discovering` | Regression          | `TestTheStretchedClustersSiteWorksAreAdmittedByOCM`, `TestTheStorageSiteDeploymentsWorkIsAdmittedByOCM` |
| U-96 | The control plane's cluster read carries `sync_replication`, `lost_site`, and `lost_site_state` into what the operator acts on, so a follower handed a sync cluster adopts it. Regression: 2026-10-22-cluster-response-drops-sync-fields, the follower refused the leader's sync cluster as not one                                         | Regression          | `TestParseClusterResponseReadsTheSyncReplicationFacts`                                                  |

### The node plugin's determination (§9.2, §9.4)

File: `csi-driver/internal/csi/node/stage_sync_test.go`

| #    | Scenario                                                                                                                                  | Type       | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-26 | The cluster secret entry carries `sync: true` and `site: a`; a status read answering `led_from: a`, in sync, proceeds to connect          | Positive   | —    |
| U-27 | `led_from: b` calls `SwitchoverService.Move` for the volume to site `a` and returns `codes.Unavailable` while the answer is `IN_PROGRESS` | Positive   | —    |
| U-28 | `led_from: ""` (between promote and first I/O) returns `codes.Unavailable` without calling `Move`                                         | Boundary   | —    |
| U-29 | `MOVED` answered but the next status read still says `led_from: b`: one more `Unavailable`, no connect                                    | Boundary   | —    |
| U-30 | `REFUSED` with `blocking_volumes` returns `codes.FailedPrecondition` quoting the operator's reason and the volumes                        | Negative   | —    |
| U-31 | A secret entry without `sync` takes the async path unchanged: no status read, no `Move`                                                   | Regression | —    |
| U-32 | A secret entry with `sync: true` and no `site` fails the stage with `codes.FailedPrecondition` before any backend call                    | Negative   | —    |
| U-33 | A volume of a consistency group calls `Move` with the group handle, not the volume                                                        | Positive   | —    |
| U-34 | Unstage after the volume moved away: the disconnect finds no subsystem and succeeds                                                       | Boundary   | —    |

### Birth site on `CreateVolume` (§9.5)

File: `csi-driver/internal/csi/controller/volume_sync_test.go`

| #    | Scenario                                                                                                                 | Type       | Test |
|------|--------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-67 | A create on a sync cluster sends `site` equal to the driver's own site, with no `host_id`                                | Positive   | —    |
| U-68 | A create with a co-located `host_id` sends both `site` and `host_id`                                                     | Positive   | —    |
| U-69 | The backend answers `409` (the pinned node is on the other site): `codes.FailedPrecondition` quoting both, no retry loop | Negative   | —    |
| U-70 | The backend answers `400` (unknown site): `codes.FailedPrecondition`, the message names the secret entry's `site`        | Negative   | —    |
| U-71 | A clone or a snapshot restore sends no `site` and logs the inherited home site                                           | Boundary   | —    |
| U-72 | A create on a cluster whose secret entry has no `sync` sends no `site` (async path unchanged)                            | Regression | —    |

### Inheriting the volume on a recreated claim (§9.6)

File: `csi-driver/internal/csi/controller/volume_sync_test.go`

| #    | Scenario                                                                                                                                            | Type       | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-73 | A create whose name exists and is online, led from the other site: the handle of the existing volume is returned, no second create, no site compare | Positive   | —    |
| U-74 | A create whose name exists with a different size: `codes.AlreadyExists` naming both sizes (unchanged)                                               | Regression | —    |
| U-75 | A create whose name exists and is not online, on a sync cluster: `codes.Aborted` with the status, no delete call                                    | Negative   | —    |
| U-76 | The same on a cluster without `sync`: the non-online leftover is deleted and the create retried (unchanged)                                         | Regression | —    |
| U-77 | `DeleteVolume` on a sync cluster deletes the volume whatever site it is led from                                                                    | Positive   | —    |

### Event ownership (§6.1)

File: `operator/internal/controllers/cluster/storagecluster_stretched_events_test.go`

| #    | Scenario                                                                                                                                                           | Type     | Test                                                                                          |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------------------------------------------------------|
| U-78 | A `storage-nodes` event for a node of the other site reconciles its remote `StorageNode`: status updated, no workload objects created, `peerNodesOnline` refreshed | Positive | `TestARemoteNodesStatusComesFromTheControlPlane`, `TestPeerNodesAreCountedFromTheRemoteNodes` |
| U-79 | A `clusters` event with `lost_site` set emits `SiteRecordChanged` on both sites' `StorageCluster`s and calls no backend route                                      | Positive | `TestTheSiteRecordIsProjectedAndAnnouncedOnce`                                                |
| U-80 | A `tasks` event for a cluster-wide task on the follower updates status and drives no ops                                                                           | Negative | —                                                                                             |
| U-81 | A `volumes` event showing the move complete resolves a pending `SwitchoverService.Move` without a status poll                                                      | Positive | —                                                                                             |
| U-82 | A `clusters` event changing `leader_site` to the other site drops the role on this operator with `RoleChanged`                                                     | Positive | —                                                                                             |

### Remote `StorageNode`s (§6.2)

File: `operator/internal/controllers/node/storagenode_remote_test.go`

| #    | Scenario                                                                                                                                                                | Type       | Test                                                                                                          |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|---------------------------------------------------------------------------------------------------------------|
| U-83 | A node of the peer site appears in the stream's snapshot: a `StorageNode` with `stretched.remote: true` and the node's site is created                                  | Positive   | `TestARemoteNodeIsListedForEveryPeerSiteNode`                                                                 |
| U-84 | A remote `StorageNode` is deleted by a user: it is recreated on the next reconcile, nothing is called on the control plane                                              | Negative   | `TestADeletedRemoteNodeComesBack`, `TestDeletingARemoteNodeRemovesNothingFromTheControlPlane`                 |
| U-85 | The node leaves the cluster (stream `deleted`, or missing from a reconnect snapshot): the remote CR is removed                                                          | Positive   | `TestARemoteNodeWhoseNodeLeftIsRemoved`                                                                       |
| U-86 | `StorageNodeOps` on a remote node is refused at admission with `RemoteNode` naming the owning site                                                                      | Negative   | `TestAnOperationOnARemoteNodeIsRefused`, `TestAnOperationOnARemoteNodeFailsWithoutABackendCall`               |
| U-87 | A remote node never produces workload objects, whatever its status                                                                                                      | Negative   | —                                                                                                             |
| U-88 | `spec.stretched.remote` and `spec.stretched.site` are immutable                                                                                                         | Negative   | `TestStorageNodeRemoteIsImmutable`                                                                            |
| U-89 | A standalone cluster (no `spec.stretched` on the `StorageCluster`) creates no remote nodes, unchanged                                                                   | Regression | `TestAStandaloneClusterListsNoRemoteNodes`                                                                    |
| U-90 | A remote node's device stream is opened; each device becomes a `StorageDevice` with `nodeRef` naming the remote node and `status.site` and `status.remote=true` from it | Positive   | `TestARemoteNodesDevicesSayTheyAreRemote`                                                                     |
| U-91 | `StorageDeviceOps` on a device of a remote node is refused at admission with `RemoteNode`                                                                               | Negative   | `TestAnOperationOnARemoteNodesDeviceIsRefused`, `TestAnOperationOnARemoteNodesDeviceFailsWithoutABackendCall` |
| U-92 | The remote node is removed: its `StorageDevice`s go with it through the owner reference                                                                                 | Positive   | —                                                                                                             |

### `SwitchoverService` in the operator (§9.3)

File: `operator/internal/controllers/driver/switchover_test.go`

| #    | Scenario                                                                                                                                               | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-35 | `Move(volume, to: a)`: demote `?site=b` for every volume of the store, then `failover?site=a&planned=true`, answer `MOVED` on `200`                    | Positive | —    |
| U-36 | Demote answers `409` with `volumes`: no promote, answer `REFUSED` with `blocking_volumes` and the backend's message                                    | Negative | —    |
| U-37 | Promote answers `409 in progress`: the service polls, the caller sees `IN_PROGRESS`, then `MOVED`                                                      | Boundary | —    |
| U-38 | Promote answers `412` with `allow_forced`: one `failover?site=a` without `planned`, event `SyncPeerSiteOffline`, answer `MOVED`                        | Positive | —    |
| U-39 | Promote answers `412` without `allow_forced`: answer `REFUSED`, no forced promote                                                                      | Negative | —    |
| U-40 | Two concurrent `Move`s for volumes of one store: one demote, one promote, both answered `MOVED`                                                        | Boundary | —    |
| U-41 | A `Move` for a store already led from `to_site`: `MOVED` with no backend call                                                                          | Boundary | —    |
| U-42 | A group `Move` uses the `G/replication/...` routes once for the group                                                                                  | Positive | —    |
| U-43 | A `Move` from a remote peer (`site/b/operator`) is served; a `Move` from a remote node peer (`site/b/node/x`) is refused with `codes.PermissionDenied` | Negative | —    |
| U-44 | Object operations on a store the service just moved are deferred until the status shows a leader (first-I/O rule)                                      | Boundary | —    |

### `NetworkFence` in the driver (§10.2)

File: `csi-driver/internal/csi/controller/networkfence_test.go`

| #    | Scenario                                                                                                                                          | Type                | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-45 | CIDRs matching only site `a`'s nodes: one `POST .../sites/a/fence`; `200 done` answers success                                                    | Positive            | —    |
| U-46 | The fence route answers `202` with a task: the RPC returns `codes.Unavailable` so csi-addons retries, and a later call that finds `done` succeeds | Boundary            | —    |
| U-47 | The fence route answers `409` (proof refused): `codes.FailedPrecondition` quoting the reason, event `FenceRefused`                                | Negative            | —    |
| U-48 | CIDRs matching no node: `codes.InvalidArgument`, no backend call                                                                                  | Negative            | —    |
| U-49 | CIDRs matching nodes of both sites: `codes.InvalidArgument`, no backend call                                                                      | Negative            | —    |
| U-50 | A `/32` list and a covering `/24` resolve to the same site                                                                                        | Boundary            | —    |
| U-51 | `UnfenceClusterNetwork` while the site is still recorded: `codes.FailedPrecondition`; once the record is gone: success                            | Positive + Negative | —    |
| U-52 | Fence on a cluster that is not a sync cluster: `codes.FailedPrecondition` naming the cluster                                                      | Negative            | —    |
| U-53 | The fence never answers success before the control plane records `done` (the one failure that makes a failover unsafe)                            | Regression          | —    |

### The site link (§7, Appendix C)

File: `atlas-lib/link/relay_test.go`, `fleet/internal/forwarder/forwarder_test.go` (package to be created)

| #    | Scenario                                                                                                                                                          | Type                | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-54 | Two agents dial the relay as `site/a` and `site/b`; a stream from `site/a/operator` to `site/b/operator` carries a unary gRPC call end to end                     | Positive            | —    |
| U-55 | A sub-peer registering as `site/b/operator` through `site/a`'s agent is refused by the relay                                                                      | Negative            | —    |
| U-56 | A node peer (`site/a/node/x`) opening a stream to any remote target is refused; an operator peer is admitted                                                      | Negative            | —    |
| U-57 | The target is not linked: `ErrNoSession`, retryable, no stream left half open                                                                                     | Negative            | —    |
| U-58 | The relay session drops: the agent redials, the registry recovers, a stream opened after recovery succeeds                                                        | Boundary            | —    |
| U-59 | The agent's local hub refuses a local peer whose `TokenReview` fails; the relay refuses an agent whose hub token fails                                            | Negative            | —    |
| U-60 | The relay is not leader (`Accepting` false): agents are refused and redial with backoff                                                                           | Boundary            | —    |
| U-61 | A stream open while the relay loses leadership is closed with an error, not left hanging                                                                          | Boundary            | —    |
| U-62 | The HTTP relay to the control plane (§7.5) is off by default; enabled, a request from `site/b/operator` reaches the management API with the caller's bearer token | Positive + Negative | —    |

### Preflight (§5.4)

File: `operator/internal/controllers/cluster/storagecluster_preflight_test.go`

| #    | Scenario                                                                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-63 | The reachability read answers every worker reachable: the create proceeds                                                       | Positive | —    |
| U-64 | One worker unreachable: the cluster holds in `CheckingControlPlane` with the worker named                                       | Negative | —    |
| U-65 | The reachability route is absent (`404`): the preflight falls back to the node API probe and does not fail on the missing route | Boundary | —    |
| U-66 | The follower finds a node of its own site already added by another installation: refused with the node named                    | Negative | —    |

---

## 2. Integration Tests

Two operator managers in one `envtest` process, each with its own namespace and `ControlPlane` of `source.managed`, against one mock control plane that keeps a cluster record with `leader_site` and answers the sync routes. The hub's operator runs beside them with a fake `ManifestWork` and `ManagedClusterView` client, as the `StorageSiteDeployment` tests do.

### Leader creates, follower adopts (§5)

| #    | Scenario                                                                                                                                                | Type                | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| I-01 | Two `ClusterDeploymentConfig`s with the role fields: the leader's `StorageCluster` creates, the follower's adopts, both reach the same UUID and `Ready` | Positive            | —    |
| I-02 | Nodes added by both sites carry their site; the mock's node listing shows `a` and `b` populated                                                         | Positive            | —    |
| I-03 | The follower's config applied first: it holds in `CheckingControlPlane` with `PeerClusterMissing` until the leader's cluster exists, then adopts        | Boundary            | —    |
| I-04 | The leader's activation waits for the follower's nodes, then activates; one `activate` call total                                                       | Boundary            | —    |
| I-05 | A `StorageClusterOps` of `RollingRestart` on the follower is refused at admission; the same on the leader runs                                          | Negative + Positive | —    |
| I-06 | Role swap: both configs re-delivered with swapped roles; the old leader drops the role, the new one admits ops; no second create is posted              | Positive            | —    |
| I-07 | The follower's operator restarts mid-adoption: adoption resumes from the control plane's record, no duplicate Secret                                    | Negative            | —    |

### `StretchedCluster` to two works (§5.1)

| #    | Scenario                                                                                                                                                                                                  | Type                | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| I-08 | A `StretchedCluster` raises both discoveries, sizes both drafts, and on approval delivers the leader's first and the follower's once the leader's view reports a UUID                                     | Positive            | —    |
| I-09 | `StretchedCluster.status.sites[]` reflects each draft's phase and nodes, then each site's confirmed role and node count; `phase` walks `Discovering → Drafted → LeaderCreating → FollowerJoining → Ready` | Positive            | —    |
| I-10 | Deleting the `StretchedCluster` releases both works with their resources orphaned; the sites' `StorageCluster`s stay                                                                                      | Negative + Positive | —    |

### Switchover through the link (§7, §9)

| #    | Scenario                                                                                                                                                   | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-11 | A fake node peer on site `b` stages a volume led from `a`: the `b` operator demotes `a`, promotes `b`, the stage proceeds                                  | Positive | —    |
| I-12 | The `a` operator is stopped: the move still completes, because the demote is issued against the control plane, not the peer                                | Positive | —    |
| I-13 | The relay is stopped: a local stage on `b` still completes (local link); a cross-site `SiteService.Describe` returns `ErrNoSession`                        | Boundary | —    |
| I-14 | A PVC provisioned by site `b`'s driver: the mock's create body carries `site: b`; the volume's owner node is on `b`                                        | Positive | —    |
| I-15 | A PVC on `b` annotated with a site-`a` node: the mock answers `409`, the PVC stays `Pending` with the reason, no create succeeds                           | Negative | —    |
| I-16 | A protected PVC is deleted on `a` (PV retained by Ramen's rule): the mock backend sees no delete; the PV is `Released` with `Retain`                       | Positive | —    |
| I-17 | A PVC of the same name is then applied on `b`: the mock answers `409`, the driver returns the existing handle, one volume exists                           | Positive | —    |
| I-18 | After both sites add their nodes, each manager's client lists every node of the cluster, with `stretched.remote` true for the peer's and false for its own | Positive | —    |
| I-19 | On each site, the device listing holds every device of both sites, each `nodeRef` resolving to a node on that site, managed or remote                      | Positive | —    |

---

## 3. E2E Tests

The lab of design Appendix D: a hub cluster running OCM, `ramen-hub`, the fleet manager, and the control plane; two K3s clusters as sites `a` and `b`; MinIO on the hub for Ramen's metadata. Every row with data in flight runs the hashed writer of `design-csi-addons-replication.md` §12 and asserts the hash, not only that I/O continued. Pass criteria are Ramen's own conditions on the DRPC and the VRG plus the backend's site records, never a sleep.

### Bring-up (§5)

| #    | Scenario                                                                                                                                                                            | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Register both managed clusters, apply the `StretchedCluster`, approve: both discoveries run, the leader creates, the follower adopts, every node online on its site, cluster active | Positive | —    |
| E-02 | Apply one `StorageClassDeployment` and the metro `DRPolicy`: both sites' `StorageClass`es match by name and `storageid`; the `DRPolicy` validates                                   | Positive | —    |
| E-03 | Create a PVC on each site: both volumes belong to the one backend cluster; each is led from the site it was created on                                                              | Positive | —    |

### Planned relocate (§8.3, §9)

| #    | Scenario                                                                                                                                       | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-04 | Protect a workload on `a`, write, relocate to `b`: pods stage on `b`, volumes report `led_from: b`, hash intact, no `VolumeReplication` exists | Positive | —    |
| E-05 | Relocate back to `a`: same assertions, both moves planned (no `SyncPeerSiteOffline` event)                                                     | Positive | —    |
| E-06 | A consistency group of three PVCs relocates as one group move                                                                                  | Positive | —    |
| E-07 | Two workloads on one store, one relocated: the demote's `409 volumes` holds the stage and names the other workload's volumes                   | Negative | —    |

### Unplanned failover and site return (§8.3, §10)

| #    | Scenario                                                                                                                                                                     | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-08 | Power off site `a` (the leader): the monitor records `a` down; failover to `b`: `NetworkFence` fences `a`, stages promote forced, hash intact to the last acknowledged write | Positive | —    |
| E-09 | Power `a` back on: the site returns by activation, `UnfenceClusterNetwork` succeeds once the record is gone, the DRPC reaches `Unfenced`, relocate back succeeds             | Positive | —    |
| E-10 | Failover attempted while `a` is reachable (its nodes healthy): the fence's proof refuses, the DRPC holds, nothing is promoted                                                | Negative | —    |
| E-11 | Swap the roles while `a` is down: `b` becomes leader and runs a rolling restart of its own site; `a` returns as follower                                                     | Positive | —    |

### Birth site (§9.5)

| #    | Scenario                                                                                                                                                           | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-14 | A PVC created on `a` with `Immediate` binding: `storage_node_id` is a site-`a` node, `GET V/replication/status` says `led_from: a`                                 | Positive | —    |
| E-15 | A PVC created on `b` with `WaitForFirstConsumer` and `pod-affinity`: owner node is the storage node co-located with the pod's worker on `b`                        | Positive | —    |
| E-16 | A snapshot of an `a`-born volume restored on `b`: the clone's home is `a`; `b`'s pod stages after one planned move                                                 | Boundary | —    |
| E-17 | Relocate `a` to `b`: the backend volume UUID and the volume count are unchanged before and after; the PV on `a` is `Released` with `Retain` and no claim reference | Positive | —    |
| E-18 | Unprotect (delete the DRPC) then delete the PVC on the primary: the PV's policy is `Delete` again and the backend volume is gone                                   | Positive | —    |
| E-19 | Without Ramen: delete the PVC on `a` with the PV retained, apply the same PVC on `b`: the claim binds to the existing volume, its pod stages after one move        | Positive | —    |

### Hub loss (§13)

| #    | Scenario                                                                                                                                                | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-12 | Stop the hub: I/O on both sites continues, a new pod on the leading site stages (status read fails, volume already led here), a relocate does not start | Boundary | —    |
| E-13 | Hub returns: site records resume, the pending relocate completes                                                                                        | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01 — The sites and the hub are all partitioned from each other

**Design reference:** §13, Open Question Q2

**What to verify:** the backend's forced promote does not pass on a management-only partition (the peer site vetoes), and that on a full partition the design's stated limitation is what happens: no witness stops the isolated site.

**Test concept:** with the storage network up, cut the management network between the hub and `a`; fail over; expect the proof to refuse and the DRPC to hold. Then cut the storage network between the sites as well; expect the proof to pass and both sites to serve until `a` is fenced. Record both outcomes against the design's failure-mode table.

### M-02 — The relay loses leadership mid-switchover

**Design reference:** §7.2, §13

**What to verify:** a `Move` in flight on a local link completes regardless; a cross-site `RestartNodes` stream is closed with an error and the leader's rolling restart resumes from the control plane's restart state after the relay returns.

**Test concept:** scale the fleet manager to two replicas, delete the leader pod during a rolling restart driven from the leader site, assert the restart finishes with every node restarted once.

### M-03 — The follower is promoted while the old leader is still reconciling

**Design reference:** §11.1

**What to verify:** no window exists in which both operators admit cluster-scoped ops. The old leader refuses from the reconcile that observes `Follower`; an ops created on it in between is failed with `RoleLost`, not run.

**Test concept:** apply the swap and, within the same second, create a `RollingRestart` on the old leader; expect `RoleLost` and no restart calls from it.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 94        | 36      | 58          |
| Integration | 19        | 0       | 19          |
| E2E         | 19        | 0       | 19          |
| Manual      | 3         | 0       | 3           |

---

## 6. Dependencies Declined

| Dependency                                                     | Why not tested here                                                                          |
|----------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| The liveness proof, site records, and site return (P0-1)       | The control plane's, on the `sync-replication` branch; faked at the boundary in U-35 … U-53  |
| The site fence route (P0-2)                                    | The control plane's; its contract is pinned by U-45 … U-53 and exercised live in E-08 … E-10 |
| Ramen's sync-mode VRG, fencing order, and S3 round trip (P0-4) | Upstream; E-04 … E-09 exercise this repository's conformance to what Ramen drives            |
| OCM registration and `ManifestWork` delivery                   | Upstream; the reconciler's tests fake the work and view clients                              |

---

## 7. What Is Not Yet Covered

Phase 1 is built and its unit rows are covered, with these left open:

| Rows       | Why                                                                                                                                                          |
|------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-05, U-82 | The cluster record has no `leader_site` yet (P0-2), so neither a role conflict at adoption nor a role dropped on a record change can be observed             |
| U-63–U-66  | The preflight of §5.4 waits on the `reachability` route (P0-2); without it the only check would be a probe the operator cannot run from a site               |
| U-80       | A follower drives no cluster-wide operation by admission (U-14) and on a role swap (U-08); no test reads a cluster-wide task event on a follower directly    |
| U-87       | A remote node has no worker, and the workload's node set has always skipped one; the guard is structural, and no test drives the workload with a remote node |
| U-92       | A remote node's devices are owned by it like any node's, so the existing owner-reference cascade removes them; no test deletes a remote node with devices    |

The Phase 2 to 4 rows, and every integration, E2E, and manual row, are `—`. The rows that must be green before Phase 3 is called safe are U-53 (a fence never answers success early), U-29 (a stage never attaches on a stale status), and U-75 (a recreate never deletes a volume whose site is down), both of which are written red first against a mock that answers wrong, as the design's §15 requires.
