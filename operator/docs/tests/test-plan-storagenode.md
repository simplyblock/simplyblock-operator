# Test Plan: StorageNode and StorageNodeOps

Related design: [`designs/crd-redesign/design-storagenode.md`](../designs/crd-redesign/design-storagenode.md)

Supersedes `test-plan-storagenode-ops.md` and `test-plan-drain-remove.md`, both
removed in the same change. Their scenarios were prose rows without permanent
identifiers, and they are re-expressed here with IDs, keeping the original wording
wherever it survived the rework.

Scope is the operator, its webhooks, and the Kubernetes surface this repository
builds. The control plane (`sbcli`) and SPDK are dependencies, faked at the
boundary: what a row asserts is the operator's response to a control-plane
answer, never the control plane's own behavior.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Rows that are runnable against the shipped API name the shipped spelling, and
rows in a planned block name the target. The parent reference is
`spec.storageNodeSetRef` today and `spec.clusterRef` after the reparent in design
§3.1, the per-node block is `spec.overrides` today and `spec.config` after, the
operation's target is `spec.storageNodeRef` today and `spec.nodeRef` after, and
its position is `status.subPhase` today and `status.step` after, and the slot
ordinal is `spec.socketIndex` today and `spec.slot` after. Action values are
lowercase today (`remove`, `migrate`) and PascalCase after the rename in design
§6.3 (`Remove`, `Migrate`), so a row naming an action names the spelling its
harness would use.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster, real data path                               |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by a mock HTTP server. No Kubernetes API server is involved.

### Entity: Object Naming (design §3.1)

File: `operator/internal/controllers/node/storagenodeset_storagenode_unit_test.go`

| #    | Scenario                                                            | Type     | Test                                                |
|------|---------------------------------------------------------------------|----------|-----------------------------------------------------|
| U-01 | A generated name is non-empty, lowercase, and at most 63 characters | Positive | `TestStorageNodeCRName_SimpleCase`                  |
| U-02 | A parent name long enough to overflow is truncated to a valid label | Boundary | `TestStorageNodeCRName_TruncatesLongNames`          |
| U-03 | Two calls with the same parent name produce different names         | Positive | `TestStorageNodeCRName_IsRandomPerCall`             |
| U-04 | A generated name contains only DNS-label characters                 | Positive | `TestStorageNodeCRName_IsDNSLabelSafe`              |
| U-05 | Uppercase and underscores in a worker hostname are replaced         | Negative | `TestSanitiseDNSLabel_ReplacesInvalidChars`         |
| U-06 | Leading and trailing hyphens are stripped from a sanitized label    | Boundary | `TestSanitiseDNSLabel_StripsLeadingTrailingHyphens` |
| U-07 | A name collision on create is retried with a fresh identifier       | Negative | —                                                   |
| U-08 | The generated name encodes neither the worker nor the socket        | Negative | —                                                   |

### Entity: Provisioning Gates (design §4.2)

File: `operator/internal/controllers/node/storagenode_controller_unit_test.go`

| #    | Scenario                                                                             | Type     | Test                                                         |
|------|--------------------------------------------------------------------------------------|----------|--------------------------------------------------------------|
| U-09 | `enableFailureDomains` set and no fault group declared: provisioning is held         | Negative | `TestCheckFailureDomain_BlocksWhenEnabledAndNotSet`          |
| U-10 | `enableFailureDomains` set and a fault group present: provisioning proceeds          | Positive | `TestCheckFailureDomain_AllowsWhenFailureDomainSet`          |
| U-11 | `enableFailureDomains` unset: the fault group is not required                        | Negative | `TestCheckFailureDomain_SkipsWhenFeatureDisabled`            |
| U-12 | A fault group of 0 is a valid group and is not read as unset                         | Boundary | —                                                            |
| U-13 | Held provisioning emits `FailureDomainMissing` and issues no `POST`                  | Negative | —                                                            |
| U-14 | The worker's storage-node API answers: the host check passes                         | Positive | `TestCheckNodeInfoReachable`                                 |
| U-15 | The worker's storage-node API is unreachable: held, no `POST`                        | Negative | `TestStorageNodeSetReconcileUnreachableNodeInfoRequeues`     |
| U-16 | TLS is enabled and the CA is missing: the host check fails informatively             | Negative | `TestCheckNodeInfoReachableTLSMissingCA`                     |
| U-17 | The host check retries until the endpoint answers                                    | Positive | `TestWaitForNodeInfoReachable`                               |
| U-18 | No in-flight sibling: the in-flight count is zero                                    | Boundary | `TestCountInFlightNodes_ZeroWhenNonePosted`                  |
| U-19 | Siblings past `Posting` without a UUID are counted as in flight                      | Positive | `TestCountInFlightNodes_CountsSiblingsWithPostedAtAndNoUUID` |
| U-20 | The node counts every sibling but itself                                             | Boundary | `TestCountInFlightNodes_ExcludesSelf`                        |
| U-21 | Two nodes on one worker count as one in-flight worker, not two                       | Boundary | `TestCountInFlightNodes_DeduplicatesByWorker`                |
| U-22 | A worker already in flight does not block another worker under the limit             | Positive | `TestParallelNodeAddContinuesPastPendingWorker`              |
| U-23 | `maxParallelNodeAdds` reached: the node holds at `AwaitingSlot` and issues no `POST` | Negative | —                                                            |
| U-24 | Workers hosting a FoundationDB pod are identified                                    | Positive | `TestFDBWorkerSet`                                           |
| U-25 | A FoundationDB worker holds while another FoundationDB worker is in flight           | Negative | —                                                            |
| U-26 | A FoundationDB worker holds even when `maxParallelNodeAdds` allows more              | Boundary | —                                                            |
| U-27 | A non-FoundationDB worker is not held by a FoundationDB worker in flight             | Negative | —                                                            |

### Entity: The Provisioning Claim (design §4.2)

File: `operator/internal/controllers/node/storagenode_controller_unit_test.go`

| #    | Scenario                                                                              | Type     | Test |
|------|---------------------------------------------------------------------------------------|----------|------|
| U-28 | The transition into `Posting` is persisted before the `POST` is issued                | Positive | —    |
| U-29 | A second reconciler at the same `resourceVersion`: 409, backs off, issues no `POST`   | Negative | —    |
| U-30 | A sibling socket already at `Posting`: this object enters `Resolving` without posting | Negative | —    |
| U-31 | A sibling socket at `Resolving`: this object enters `Resolving` without posting       | Negative | —    |
| U-32 | The `POST` returns 5xx: the step stays `Posting` and is retried                       | Negative | —    |
| U-33 | The `POST` returns 4xx: the step stays `Posting`, and the body is in the event        | Negative | —    |
| U-34 | The `POST` times out: the step is not advanced and no second `POST` is issued         | Negative | —    |
| U-35 | The `POST` succeeds: the step advances to `Resolving`                                 | Positive | —    |

### Entity: UUID Resolution and Adoption (design §4.2, §4.3)

File: `operator/internal/controllers/node/storagenode_controller_unit_test.go`

| #    | Scenario                                                                           | Type     | Test                                     |
|------|------------------------------------------------------------------------------------|----------|------------------------------------------|
| U-36 | The worker's internal IP is resolved from the Kubernetes `Node`                    | Positive | `TestGetNodeInternalIP`                  |
| U-37 | The Kubernetes `Node` carries no internal address: held, not failed                | Negative | `TestGetNodeInternalIPNoAddress`         |
| U-38 | One backend node at the worker's IP: matched to `slot` 0                           | Positive | —                                        |
| U-39 | Two backend nodes at one IP: sorted by RPC port and matched by `slot`              | Positive | —                                        |
| U-40 | `slot` beyond the number of backend nodes at the IP: held, not matched             | Boundary | —                                        |
| U-41 | No backend node at the worker's IP: `Resolving` holds                              | Negative | `TestPollNodeOnlinePaths`                |
| U-42 | The control plane errors during resolution: held, and the step is not advanced     | Negative | `TestPollNodeOnlineErrorAndTimeoutPaths` |
| U-43 | The `Resolving` deadline expires: the node is marked failed rather than polling on | Boundary | —                                        |
| U-44 | An upgrade Secret is present: the node adopts without a `POST`                     | Positive | —                                        |
| U-45 | An upgrade Secret is present but empty: falls through to the normal path           | Negative | —                                        |
| U-46 | A backend node already exists at the worker's IP: adopted rather than added        | Positive | —                                        |
| U-47 | Adoption records the backend UUID and leaves `status.phase` at `Online`            | Positive | —                                        |

### Entity: Steady-State Sync (design §4.4)

File: `operator/internal/controllers/node/storagenode_controller_unit_test.go`

| #     | Scenario                                                                      | Type     | Test |
|-------|-------------------------------------------------------------------------------|----------|------|
| U-48  | A streamed node object writes `status.status`, `health`, and the resources    | Positive | —    |
| U-49  | Nothing changed since the last pass: the reconcile issues no status patch     | Negative | —    |
| U-50  | The control-plane assigned fault group differs from the requested one         | Positive | —    |
| U-51  | A malformed streamed object: an error rather than a nil dereference           | Negative | —    |
| U-52  | `status.observedGeneration` matches `metadata.generation` after a sync        | Positive | —    |
| U-236 | A node reporting 3 of 4 devices online: `devices.online` 3, `devices.total` 4 | Positive | —    |
| U-237 | A node whose devices have not been reported: `devices` absent, not `{0, 0}`   | Boundary | —    |
| U-238 | A node with zero devices online: `devices.online` is 0 and present            | Boundary | —    |

### Entity: Deletion (design §4.5)

File: `operator/internal/controllers/node/storagenode_controller_unit_test.go`

| #    | Scenario                                                                        | Type     | Test                                                      |
|------|---------------------------------------------------------------------------------|----------|-----------------------------------------------------------|
| U-53 | A node that never got a UUID: the finalizer is removed with no operation raised | Boundary | `TestHandleDeletion_RemovesFinalizerWhenNeverProvisioned` |
| U-54 | An online node deleted: a `Remove` operation is raised and owned by the node    | Positive | `TestEnsureRemoveOps_CreatesOpsWhenMissing`               |
| U-55 | A second reconcile while the drain runs: no second operation is created         | Negative | `TestEnsureRemoveOps_IdempotentWhenAlreadyExists`         |
| U-56 | `status.activeOpsRef` still set: the finalizer is held                          | Negative | —                                                         |
| U-57 | The raised operation failed: the finalizer is held rather than force-removed    | Negative | —                                                         |
| U-58 | A suspended node deleted: a `Remove` operation is still raised                  | Positive | —                                                         |
| U-59 | An offline node deleted: no operation is raised and the finalizer is removed    | Boundary | —                                                         |

### Entity: The workerNode Webhook (design §3.1)

File: `operator/internal/webhook/storagenode_validator_test.go`

| #     | Scenario                                                                    | Type     | Test                       |
|-------|-----------------------------------------------------------------------------|----------|----------------------------|
| U-60  | A user changing `spec.workerNode`: denied with the migration hint           | Negative | `TestStorageNodeValidator` |
| U-61  | The operator's service account changing `spec.workerNode`: allowed          | Positive | `TestStorageNodeValidator` |
| U-62  | An update that does not touch `spec.workerNode`: allowed without inspection | Negative | `TestStorageNodeValidator` |
| U-63  | A create rather than an update: allowed, since there is no old value        | Boundary | `TestStorageNodeValidator` |
| U-64  | A service account in another namespace named like the operator's: denied    | Negative | —                          |
| U-239 | A user changing `spec.config.pcieAllowList`: denied                         | Negative | —                          |
| U-240 | The operator merging `newSsdPcie` into `spec.config.pcieAllowList`: allowed | Positive | —                          |
| U-241 | A user changing `spec.config.sizing.vcpuCount`: denied                      | Negative | —                          |
| U-242 | The operator re-sizing `spec.config.sizing`: allowed                        | Positive | —                          |
| U-243 | An update touching none of the guarded fields: admitted without inspection  | Negative | —                          |

### Workload: DaemonSet, Services, and RBAC (design §5.1)

Files: `operator/internal/controllers/node/storagenodeset_controller_unit_test.go`,
`operator/internal/utils/storage_nodeset_ds_test.go`

| #    | Scenario                                                                         | Type     | Test                                                                     |
|------|----------------------------------------------------------------------------------|----------|--------------------------------------------------------------------------|
| U-65 | No DaemonSet present: one is created                                             | Positive | `TestStorageNodeSetDaemonSetReconcileCreatesWhenMissing`                 |
| U-66 | A DaemonSet present: it is updated in place rather than recreated                | Positive | `TestStorageNodeSetDaemonSetReconcileUpdatesExisting`                    |
| U-67 | TLS disabled: the pod template carries no serving-certificate mount              | Negative | `TestStorageNodeSetDaemonSetReconcileTLSDisabled`                        |
| U-68 | TLS enabled: the pod template mounts the serving certificate                     | Positive | `TestStorageNodeSetDaemonSetReconcileTLSEnabled`                         |
| U-69 | The cert-manager provider: the Certificate is created alongside                  | Positive | `TestStorageNodeSetDaemonSetReconcileTLSCertManagerProvider`             |
| U-70 | User-supplied container resources override the defaults                          | Positive | `TestBuildStorageNodeSetDaemonSetUserResourcesOverrideDefaults`          |
| U-71 | The ServiceAccount carries an owner reference to its parent                      | Positive | `TestStorageNodeSetReconcileServiceAccountHasOwnerReference`             |
| U-72 | ClusterRoleBinding names include the namespace, so two namespaces do not collide | Positive | `TestBuildStorageNodeSetClusterRoleBindingNameIncludesNamespace`         |
| U-73 | Namespace-specific ClusterRoleBindings are created                               | Positive | `TestStorageNodeSetReconcileCreatesNamespaceSpecificClusterRoleBindings` |
| U-74 | The SPDK proxy Service is created                                                | Positive | `TestReconcileSpdkProxyService`                                          |
| U-75 | Every workload object carries an owner reference to the `StorageCluster`         | Positive | —                                                                        |
| U-76 | Deleting the `StorageCluster` cascades to every workload object                  | Positive | —                                                                        |

### Workload: Storage-Plane Labels (design §5.2)

File: `operator/internal/controllers/node/storagenodeset_controller_unit_test.go`

| #    | Scenario                                                                        | Type     | Test                                |
|------|---------------------------------------------------------------------------------|----------|-------------------------------------|
| U-77 | Workers gain the node-type and node-set labels                                  | Positive | `TestStorageNodeSetLabelingHelpers` |
| U-78 | A node that has come online gets its per-slot storage-node-uuid label           | Positive | —                                   |
| U-79 | A node with no UUID yet contributes no slot label                               | Negative | —                                   |
| U-80 | The slot label key is stable across a UUID change, and only the value moves     | Positive | —                                   |
| U-81 | A worker hosting nodes of two clusters carries two non-colliding slot keys      | Positive | —                                   |
| U-82 | A stale slot label whose node is gone is removed                                | Positive | —                                   |
| U-83 | The node `List` fails: the reconcile aborts and deletes no label                | Negative | —                                   |
| U-84 | A worker with no storage node: its slot labels are removed, the others are kept | Boundary | —                                   |

### Workload: Per-Node Configuration (design §5.3)

File: `operator/internal/controllers/node/storagenodeset_storagenode_unit_test.go`

| #     | Scenario                                                                            | Type     | Test                                                                |
|-------|-------------------------------------------------------------------------------------|----------|---------------------------------------------------------------------|
| U-85  | Cluster sizing values appear in a worker's env file                                 | Positive | `TestBuildPerNodeEnvFile_UsesClusterSizingValues`                   |
| U-86  | Cluster sizing is identical in every worker's entry                                 | Positive | `TestBuildPerNodeEnvFile_ClusterSizingIdenticalAcrossWorkers`       |
| U-87  | Every key the init container reads is present, including the empty ones             | Boundary | `TestBuildPerNodeEnvFile_ContainsAllRequiredKeys`                   |
| U-88  | The cluster is missing its required sizing: the write is refused with a named error | Negative | `TestReconcilePerNodeConfigMap_RejectsClusterMissingRequiredSizing` |
| U-89  | Every worker gets an entry carrying the cluster sizing                              | Positive | `TestReconcilePerNodeConfigMap_WritesClusterSizingForEveryWorker`   |
| U-90  | Two nodes with different device filters get different entries                       | Positive | —                                                                   |
| U-91  | A node with no `spec.config`: its entry carries the empty values, not missing keys  | Boundary | —                                                                   |
| U-92  | A device list containing a shell metacharacter is quoted rather than interpolated   | Negative | —                                                                   |
| U-244 | A `deviceNames` entry that is a PCI address reaches the node as one                 | Positive | —                                                                   |
| U-245 | A `deviceNames` entry that is a device path reaches the node as one                 | Positive | —                                                                   |
| U-246 | A mixed `deviceNames` list: both forms reach the node, in the order given           | Boundary | —                                                                   |
| U-247 | `deviceNames` set alongside `pcieAllowList`: the explicit list wins (design §3.1)   | Boundary | —                                                                   |
| U-93  | The ConfigMap is written before the DaemonSet on a fresh reconcile                  | Positive | —                                                                   |
| U-94  | A node deleted: its entry is removed from the ConfigMap                             | Positive | —                                                                   |

### Workload: Endpoints and Certificate Rotation (design §5.4)

Files: `operator/internal/controllers/node/storagenodeset_controller_unit_test.go`,
`operator/internal/utils/storage_nodeset_ds_test.go`

| #     | Scenario                                                                           | Type     | Test                                                                 |
|-------|------------------------------------------------------------------------------------|----------|----------------------------------------------------------------------|
| U-95  | The per-pod address is built from the worker's hostname label and the namespace    | Positive | `TestStorageNodeSetAPIAddress`                                       |
| U-96  | The EndpointSlice check matches the address builder's output exactly               | Positive | `TestEndpointSliceHasWorker_MatchesBuilderOutput`                    |
| U-97  | A dotted worker hostname is truncated to a valid endpoint label                    | Boundary | `TestBuildSpdkProxyEndpointSlice_DottedNodeNameTruncates`            |
| U-98  | Two workers whose first label segment collides: the build fails rather than merges | Negative | `TestBuildSpdkProxyEndpointSlice_CollidingFirstLabelFails`           |
| U-99  | SPDK proxy EndpointSlices are built from the running pods                          | Positive | `TestReconcileSpdkProxyEndpointSlices`                               |
| U-100 | Two pods sharing a first label segment: reported rather than silently merged       | Negative | `TestReconcileSpdkProxyEndpointSlices_DuplicateFirstSegment`         |
| U-101 | The RPC port is read from the pod's environment, falling back to its name          | Boundary | `TestExtractSpdkProxyRpcPort_FallbackToPodName`                      |
| U-102 | The serving certificate's revision is stamped onto the pod template                | Positive | `TestStorageNodeSetDaemonSetTLSSecretRevisionAnnotation`             |
| U-103 | The certificate Secret rotates: the DaemonSet rolls                                | Positive | `TestStorageNodeSetDaemonSetReconcileRollsOnTLSSecretRevisionChange` |
| U-104 | The TLS serving environment reaches the container                                  | Positive | `TestStorageNodeSetDaemonSetSBTLSServeEnv`                           |
| U-105 | A Secret that is not the storage-node-api certificate: the predicate ignores it    | Negative | `TestIsStorageNodeSetTLSSecretPredicate`                             |
| U-106 | The certificate Secret changes: every affected object in the namespace is enqueued | Positive | `TestTLSSecretToStorageNodeSetRequestsEnqueuesAllInNamespace`        |
| U-107 | Certificates and Services are reconciled together for the cert-manager provider    | Positive | `TestReconcileServicesAndServingCertificatesForCertManagerProvider`  |

### Operation: Lifecycle and Lock (design §7.1, §11)

File: `operator/internal/controllers/node/storagenodeops_controller_unit_test.go`

| #     | Scenario                                                                           | Type     | Test                                                      |
|-------|------------------------------------------------------------------------------------|----------|-----------------------------------------------------------|
| U-108 | The lock is free: it is acquired and the phase becomes `Running`                   | Positive | `TestAcquireLock_SetsActiveOpsRefAndTransitionsToRunning` |
| U-109 | Another operation holds the lock: this one stays `Pending` and requeues            | Negative | `TestAcquireLock_RequeuesWhenAnotherOpsActive`            |
| U-110 | A `Remove` acquiring the lock enters its graph's initial step                      | Positive | `TestAcquireLock_RemoveDrainSetsValidatingSubPhase`       |
| U-111 | Success: the phase is `Succeeded` and the lock is cleared                          | Positive | `TestSucceedOps_SetsPhaseAndClearsLock`                   |
| U-112 | Failure: the phase is `Failed` with a message, and the lock is cleared             | Positive | `TestFailOps_SetsPhaseAndClearsLock`                      |
| U-113 | A release by a non-owner: the lock is left alone                                   | Negative | `TestReleaseLock_OnlyClearsIfOwner`                       |
| U-114 | Advancing the step persists it before the next side effect                         | Positive | `TestAdvanceSubPhase_UpdatesSubPhaseAndResetsTrigger`     |
| U-115 | An unknown action: the operation fails terminally with the action in the message   | Negative | `TestDispatch_UnknownActionFails`                         |
| U-116 | The target node does not exist: the operation fails with a not-found message       | Negative | —                                                         |
| U-117 | A terminal operation re-reconciled: no side effect, and the lock is released again | Negative | —                                                         |
| U-118 | Two reconcilers acquiring one free lock: the loser gets 409 and requeues           | Negative | —                                                         |
| U-119 | The operation is deleted while `Running`: the finalizer releases the lock          | Positive | —                                                         |
| U-120 | Operations on two different nodes run without contending                           | Positive | —                                                         |
| U-121 | The cluster is not active: the operation holds and emits `ClusterNotReady`         | Negative | —                                                         |
| U-122 | The cluster is rebalancing: the operation holds rather than proceeding             | Negative | —                                                         |
| U-123 | The cluster becomes active: the held operation resumes with no further input       | Positive | —                                                         |
| U-124 | A node event wakes a queued operation before its requeue interval elapses          | Positive | —                                                         |

### Operation: The Single-Step Actions (design §7.3)

File: `operator/internal/controllers/node/storagenodeops_controller_unit_test.go`

| #     | Scenario                                                                         | Type     | Test |
|-------|----------------------------------------------------------------------------------|----------|------|
| U-125 | `Suspend`: the call is issued, and the step completes when the node is suspended | Positive | —    |
| U-126 | `Resume`: the step completes when the node is online                             | Positive | —    |
| U-127 | `Shutdown`: the step completes when the node is offline                          | Positive | —    |
| U-128 | `Restart`: `reattachVolume` and `force` are passed through when set              | Positive | —    |
| U-129 | The node is already at the target state: the call is not issued at all           | Negative | —    |
| U-130 | The call returns 5xx: the step is retried and the phase does not advance         | Negative | —    |
| U-131 | The call returns 4xx: the step is retried, and the body reaches the event        | Negative | —    |
| U-132 | The call is retried after a timeout: the endpoint is called at most once more    | Negative | —    |
| U-133 | The node never reaches the target state: the step's deadline expires and fails   | Boundary | —    |
| U-134 | A late response after the deadline expired: ignored, no second commit            | Negative | —    |

### Operation: Volume Classification (design §8.1)

File: `operator/internal/controllers/node/drain_unit_test.go`

| #     | Scenario                                                                        | Type     | Test                                                          |
|-------|---------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-135 | A volume matching a `PersistentVolume`: classified as PV-managed                | Positive | `TestMatchVolumesToPVs_PVManaged`                             |
| U-136 | A volume whose claim carries the pin annotation: classified as pinned           | Negative | `TestMatchVolumesToPVs_Pinned`                                |
| U-137 | A volume matching no `PersistentVolume`: classified as unmanaged                | Negative | `TestMatchVolumesToPVs_Unmanaged`                             |
| U-138 | A volume matching the system filter: excluded from every bucket                 | Negative | `TestMatchVolumesToPVs_SystemVolumeSkipped`                   |
| U-139 | A node with no volumes: no drain work is produced and no error is returned      | Boundary | `TestMatchVolumesToPVs_EmptyNodeSkipsMigration`               |
| U-140 | A node holding only system volumes: nothing is migrated                         | Boundary | `TestMatchVolumesToPVs_OnlySystemVolumes`                     |
| U-141 | The default system filter matches the rebalancer's benchmark volume names       | Positive | `TestResolveOpsSystemVolumeFilter_UsesDefaultWhenNoDrain`     |
| U-142 | A custom `systemVolumeFilterRegex` replaces the default                         | Positive | `TestResolveOpsSystemVolumeFilter_UsesCustomPattern`          |
| U-143 | A malformed `systemVolumeFilterRegex`: the operation fails with the parse error | Negative | `TestResolveOpsSystemVolumeFilter_InvalidPatternReturnsError` |
| U-144 | A volume that is both pinned and unmanaged: both blockers are reported          | Boundary | —                                                             |
| U-145 | A claim in another namespace with the same name: not read as this one's pin     | Negative | —                                                             |
| U-146 | An empty `systemVolumeFilterRegex`: matches nothing rather than everything      | Boundary | —                                                             |

### Operation: The Remove Graph (design §8.2, §8.3)

File: `operator/internal/controllers/node/storagenodeops_controller_unit_test.go`

| #     | Scenario                                                                         | Type     | Test                                        |
|-------|----------------------------------------------------------------------------------|----------|---------------------------------------------|
| U-147 | Validation clear: the step advances to `Suspending`                              | Positive | —                                           |
| U-148 | Pinned volumes present: the step holds and no suspend is issued                  | Negative | —                                           |
| U-149 | Unmanaged volumes present: the step holds and no suspend is issued               | Negative | —                                           |
| U-150 | The pin is removed: the next reconcile advances without further input            | Positive | —                                           |
| U-151 | The node is already suspended: no suspend call is issued and the step advances   | Negative | —                                           |
| U-152 | Migration targets are spread evenly across the online peers                      | Positive | `TestRoundRobinDistributesEvenly`           |
| U-153 | No online peer: the step holds and emits `NoMigrationTarget`                     | Negative | `TestRoundRobinErrorsWhenNoTargetAvailable` |
| U-154 | Offline peers are excluded from target selection                                 | Negative | `TestRoundRobinSkipsOfflineNodes`           |
| U-155 | Exactly one online peer: every volume goes to it                                 | Boundary | —                                           |
| U-156 | Every migration completed: the step advances to `Verifying`                      | Positive | —                                           |
| U-157 | Verification finds non-system volumes: the step holds and retries                | Negative | —                                           |
| U-158 | Verification finds only system volumes: they are deleted, then the step advances | Positive | —                                           |
| U-159 | A system-volume delete returns 404: treated as success                           | Boundary | —                                           |
| U-160 | A system-volume delete is rejected: the node is resumed and the operation fails  | Negative | —                                           |
| U-161 | The node delete returns 200, 204, or 404: the operation succeeds                 | Boundary | —                                           |
| U-162 | The node delete returns 5xx: retried, and the operation does not fail            | Negative | —                                           |
| U-163 | The node delete is rejected: the node is resumed and the operation fails         | Negative | —                                           |
| U-164 | A resume that itself fails: the operation still reaches `Failed`, with an event  | Negative | —                                           |
| U-165 | `spec.abort` set during `Validating`: `Aborted` with no resume call issued       | Boundary | —                                           |
| U-166 | `spec.abort` set during `MigratingVolumes`: migrations deleted, node resumed     | Positive | —                                           |
| U-167 | A step's deadline expires mid-drain: the node is resumed and the operation fails | Boundary | —                                           |
| U-168 | `status.drain.volumesTotal` is written once and not recomputed on later passes   | Positive | —                                           |
| U-169 | A drain with zero volumes: `volumesTotal` is 0 and the step advances immediately | Boundary | —                                           |

### Operation: PersistentVolumeOps Lifecycle (design §8.4)

File: `operator/internal/controllers/node/drain_unit_test.go`

| #     | Scenario                                                                                                                 | Type     | Test                                             |
|-------|--------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------|
| U-170 | Generated migration names are valid DNS labels                                                                           | Positive | `TestDrainMigrationNameIsDNSValid`               |
| U-171 | Two long volume names sharing a prefix produce distinct migration names                                                  | Boundary | `TestDrainMigrationNameNoCollisionOnLongPVNames` |
| U-172 | An operator restart mid-drain does not recreate existing migration objects                                               | Negative | —                                                |
| U-173 | A completed migration is deleted and the counter is written first                                                        | Positive | —                                                |
| U-174 | A failed migration is deleted and replaced against a fresh target                                                        | Positive | —                                                |
| U-175 | A migration deleted out of band is recreated rather than counted as complete                                             | Negative | —                                                |
| U-176 | Every migration carries `spec.creatorRef` with the operation's UID                                                       | Positive | —                                                |
| U-177 | The same volume name in two namespaces produces two distinct objects                                                     | Boundary | —                                                |
| U-248 | Every migration carries `storage.simplyblock.io/managed-by: storagenodeops`, and a `List` on the label finds the fan-out | Positive | —                                                |

### Operation: The Migrate Graph (design §9)

File: `operator/internal/controllers/node/storagenodeops_migrate_config_unit_test.go`

| #     | Scenario                                                                         | Type     | Test                                              |
|-------|----------------------------------------------------------------------------------|----------|---------------------------------------------------|
| U-178 | The target worker's configuration is cloned from the source                      | Positive | `TestEnsureMigratedWorkerConfig`                  |
| U-179 | The topology re-point moves the node's configuration to the target               | Positive | `TestReconcileMigratedTopologyMigratesNodeConfig` |
| U-180 | `newSsdPcie` addresses are merged into the effective allow list                  | Positive | `TestMergePcieAllowedIntoEnvFile`                 |
| U-181 | Merging two PCI lists produces no duplicates                                     | Positive | `TestMergePcieList`                               |
| U-182 | A quoted shell list is parsed back into its elements                             | Positive | `TestParseShellCSV`                               |
| U-183 | An empty PCI list merged with additions yields just the additions                | Boundary | `TestMergePcieList`                               |
| U-184 | The target worker does not exist: the operation fails informatively              | Negative | —                                                 |
| U-185 | The target worker is the node's current worker: the operation fails immediately  | Negative | —                                                 |
| U-186 | `spec.migrate` absent for `action: Migrate`: rejected                            | Negative | —                                                 |
| U-187 | The target's pod is not `Ready`: `Preparing` holds and no restart is issued      | Negative | —                                                 |
| U-188 | The pod is `Ready` but not yet in the EndpointSlice: `Preparing` still holds     | Boundary | —                                                 |
| U-189 | The pod is `Ready` and in the EndpointSlice: the step advances                   | Positive | —                                                 |
| U-190 | The relocation restart is forced by default                                      | Positive | —                                                 |
| U-191 | An explicit `spec.force: false` is honored rather than overridden                | Negative | —                                                 |
| U-192 | The node is still online after the restart call: `Relocating` holds              | Negative | —                                                 |
| U-193 | The node has left online: the step advances to `AwaitingNode`                    | Positive | —                                                 |
| U-194 | The node returns online: the step advances to `Promoting`                        | Positive | —                                                 |
| U-195 | The promote is issued exactly once across several reconciles                     | Negative | —                                                 |
| U-196 | `spec.abort` during `Preparing`: `Aborted`, and no control-plane call was made   | Positive | —                                                 |
| U-197 | `spec.abort` during `Promoting`: refused by the graph, and the operation runs on | Negative | —                                                 |
| U-198 | The topology re-point happens after the promote, never before                    | Positive | —                                                 |
| U-199 | The source worker loses its storage-plane labels when no node remains on it      | Positive | —                                                 |
| U-200 | The source worker keeps its labels when another node still runs there            | Boundary | —                                                 |

### Operation: The Host Maintenance Graph (design §10)

File: `operator/internal/controllers/node/storagenodeops_controller_unit_test.go`

| #     | Scenario                                                                            | Type     | Test |
|-------|-------------------------------------------------------------------------------------|----------|------|
| U-201 | A worker becomes unschedulable: a `HostMaintenance` operation is raised             | Positive | —    |
| U-202 | The worker is uncordoned before the operation starts: it completes as a no-op       | Negative | —    |
| U-203 | A second reconcile while one is running: no second operation is created             | Negative | —    |
| U-204 | The concurrency limit is reached: the operation holds at `Holding`                  | Negative | —    |
| U-205 | Two nodes on one worker: the pair counts as one worker against the limit            | Boundary | —    |
| U-206 | The limit is the cluster's effective value, not `spec.maxConcurrentWorkerRestarts`  | Boundary | —    |
| U-207 | A blocking budget is created before the shutdown call                               | Positive | —    |
| U-208 | The node reports offline: the budget is relaxed                                     | Positive | —    |
| U-209 | The budget is relaxed only after the node is offline, never before                  | Negative | —    |
| U-210 | The storage pod is gone: the step advances to `AwaitingHost`                        | Positive | —    |
| U-211 | The worker's API answers again: the restart is issued                               | Positive | —    |
| U-212 | The node is already online when `Restarting` is entered: no restart call is issued  | Negative | —    |
| U-213 | The node comes back online: the budget and the drain label are removed              | Positive | —    |
| U-214 | The operation fails: the budget is still removed, so the worker stays drainable     | Negative | —    |
| U-215 | A stale operator self-budget from a previous crash is cleaned up                    | Negative | —    |
| U-216 | The `AwaitingHost` deadline expires: the operation fails and the node stays offline | Boundary | —    |

### Ops Shape: The Step Machine (design §6.3, not yet built)

File: `operator/internal/controllers/node/storagenodeops_machine_test.go`, planned.

Seven graphs declared as one `statemachine.MultiConfig`, none of which exists
yet.

| #     | Scenario                                                                           | Type     | Test |
|-------|------------------------------------------------------------------------------------|----------|------|
| U-217 | Every declared graph builds, including the ones the action under test does not use | Positive | —    |
| U-218 | An action with no declared graph: `ErrUnknownAction` rather than a stall           | Negative | —    |
| U-219 | `Remove` transitioning to `Promoting`: rejected as an illegal transition           | Negative | —    |
| U-220 | `Migrate` transitioning to `Removing`: rejected as an illegal transition           | Negative | —    |
| U-221 | `HostMaintenance` transitioning to `Suspending`: rejected                          | Negative | —    |
| U-222 | An empty `status.step`: restores to the action's declared initial state            | Boundary | —    |
| U-223 | A step value that belongs to a different action: restoration fails informatively   | Negative | —    |
| U-224 | A step value outside the enum: restoration fails rather than stalling              | Negative | —    |
| U-225 | The snapshot round-trips through `Snapshot` and `FromSnapshot` unchanged           | Positive | —    |
| U-226 | A deadline persisted and restored is the same absolute instant                     | Positive | —    |
| U-227 | A deadline that passed while the operator was down: restores as expired            | Boundary | —    |
| U-228 | A step with no deadline: restores with none rather than with a zero instant        | Boundary | —    |
| U-229 | A terminal step: `IsTerminal` is true and no transition is attempted               | Boundary | —    |
| U-230 | The outer phase machine is separate from the step machine                          | Positive | —    |
| U-231 | Every state each graph declares appears in the step `Enum` marker                  | Boundary | —    |
| U-232 | Every state each graph declares appears in the `status.step` CEL rule              | Boundary | —    |
| U-233 | The CEL rule names no value the graphs do not declare                              | Negative | —    |
| U-234 | A stored step from another action: `ErrUnknownState`, naming the declared set      | Negative | —    |
| U-235 | A restore that fails: the operation is `Failed` with the error, not requeued       | Negative | —    |

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`, driven by
`TestControllers` in `operator/internal/controllers/node/suite_test.go`, with the
control plane still mocked. These cover what a fake client cannot: real
admission, real `resourceVersion` semantics, and real watch delivery.

### Admission and Validation (design §3.1, §3.2, §3.4, §6.1)

| #    | Scenario                                                                                         | Type     | Test |
|------|--------------------------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.clusterRef` omitted at creation: rejected as `Required`                                    | Negative | —    |
| I-02 | `spec.clusterRef` changed after creation: rejected as immutable                                  | Negative | —    |
| I-46 | `spec.clusterRef` naming no `StorageCluster`: the create is rejected                             | Negative | —    |
| I-47 | `spec.clusterRef` naming a cluster with no `status.uuid`: admitted, holds with `ClusterNotReady` | Boundary | —    |
| I-48 | `spec.clusterRef` naming a cluster in another namespace: the create is rejected                  | Negative | —    |
| I-49 | `config.sizing.vcpuCount` differing from the cluster's: the create is rejected                   | Negative | —    |
| I-50 | `config.sizing.maxSubsystemCount` differing from the cluster's: rejected                         | Negative | —    |
| I-51 | `config.sizing` matching the cluster's exactly: admitted                                         | Positive | —    |
| I-52 | The operator re-sizing one node mid-roll: admitted, since the identity is exempt                 | Positive | —    |
| I-03 | `spec.socketId` omitted at creation, set later: accepted, then frozen                            | Boundary | —    |
| I-04 | `spec.socketId` set at creation, cleared later: rejected                                         | Boundary | —    |
| I-05 | `spec.config.failureDomain` of -1: rejected by the minimum                                       | Boundary | —    |
| I-06 | `spec.config.failureDomain` of 0: accepted                                                       | Boundary | —    |
| I-07 | `spec.config.spdkSystemMemory` of `"4X"`: rejected by the pattern                                | Negative | —    |
| I-08 | `spec.workerNode` changed by a non-operator identity: rejected by the webhook                    | Negative | —    |
| I-09 | The webhook is unavailable: the update is rejected rather than admitted                          | Negative | —    |
| I-10 | `spec.action` outside the enum: rejected by admission before the controller sees it              | Negative | —    |
| I-11 | `spec.nodeRef` changed after creation: rejected as immutable                                     | Negative | —    |
| I-12 | `spec.migrate.targetWorkerNode` changed after creation: rejected as immutable                    | Negative | —    |
| I-13 | `spec.abort` set on a `Running` operation: accepted, since it is the mutable field               | Positive | —    |
| I-14 | `spec.remove.systemVolumeFilterRegex` unset: defaulted by the API server                         | Boundary | —    |
| I-15 | Short names `sn` and `snops` resolve to the same lists as the full kinds                         | Positive | —    |
| I-29 | `spec.slot` of -1: rejected by the minimum                                                       | Boundary | —    |
| I-30 | `spec.config` omitted at creation: rejected as `Required`                                        | Negative | —    |
| I-31 | `spec.config.sizing` omitted at creation: rejected as `Required`                                 | Negative | —    |
| I-32 | `spec.config.deviceNames` changed after creation: rejected as immutable                          | Negative | —    |
| I-33 | `spec.config.journalManager` changed after creation: rejected as immutable                       | Negative | —    |
| I-34 | `spec.config.failureDomain` unset at creation, set later: accepted, then frozen                  | Boundary | —    |
| I-35 | `spec.config.expand` changed after creation: rejected as immutable                               | Negative | —    |
| I-36 | `spec.config.spdkImage` changed: accepted, since a phased rollout is why it is per node          | Positive | —    |
| I-37 | The deployment config is deleted: every node reconciles unchanged                                | Positive | —    |
| I-38 | The deployment config's device filter is edited: existing nodes are untouched                    | Negative | —    |
| I-39 | A node created after that edit carries the new filter                                            | Positive | —    |
| I-40 | `spec.config.deviceNames` holding a PCI address: accepted                                        | Positive | —    |
| I-41 | `spec.config.deviceNames` holding a device path: accepted                                        | Positive | —    |
| I-42 | `spec.config.deviceNames` holding a bare device name: accepted, as a path under `/dev`           | Boundary | —    |
| I-43 | `spec.config.deviceNames` holding both a PCI address and a path: accepted                        | Boundary | —    |
| I-44 | `spec.config.deviceNames` holding a malformed PCI address: rejected by the item pattern          | Negative | —    |
| I-45 | `spec.config.deviceNames` holding a path with a space: rejected by the item pattern              | Negative | —    |

### Controller Behavior Under a Real API Server (design §4, §7, §11)

| #    | Scenario                                                                        | Type     | Test              |
|------|---------------------------------------------------------------------------------|----------|-------------------|
| I-16 | Reconciling a not-found resource returns no requeue                             | Negative | `TestControllers` |
| I-17 | Two operations for one node: the second stays `Pending`, then runs              | Positive | —                 |
| I-18 | The lock is released: the queued operation wakes from the node watch            | Positive | —                 |
| I-19 | `kubectl delete` on a `Running` operation: `activeOpsRef` is cleared            | Positive | —                 |
| I-20 | Operations on two nodes of one cluster run concurrently without interference    | Positive | —                 |
| I-21 | Operations on nodes of two clusters run concurrently without interference       | Positive | —                 |
| I-22 | An operation targeting a node with no `status.uuid`: fails informatively        | Negative | —                 |
| I-23 | An operation in namespace A cannot lock a same-named node in namespace B        | Negative | —                 |
| I-24 | Deleting the `StorageCluster` cascades to its nodes and to the workload objects | Positive | —                 |
| I-25 | Two reconcilers racing the `Posting` claim: exactly one `POST` is issued        | Negative | —                 |
| I-26 | Two nodes with the same name in two namespaces: both workloads are independent  | Positive | —                 |
| I-27 | The namespace is deleted mid-drain: the operation terminates and releases       | Negative | —                 |
| I-28 | The controller's role covers every object the workload reconcile touches        | Positive | —                 |

---

## 3. End-to-End Tests

A live simplyblock cluster with a real control plane and a real data path. Every
row here changes cluster state, and the destructive ones say so.

### Provisioning and Adoption (design §4)

| #    | Scenario                                                                          | Type     | Test |
|------|-----------------------------------------------------------------------------------|----------|------|
| E-01 | A node object created: provisioned to `Ready` with a UUID within one cycle        | Positive | —    |
| E-02 | Five workers with `maxParallelNodeAdds` of 2: at most two are ever in flight      | Boundary | —    |
| E-03 | FoundationDB and non-FoundationDB workers together: the former stay sequential    | Positive | —    |
| E-04 | A two-socket worker: two nodes provisioned, each matched to its own backend node  | Positive | —    |
| E-05 | A pre-existing backend node with no object: adopted, keeping its UUID and volumes | Positive | —    |
| E-06 | Adoption of a Helm-deployed fleet through the upgrade Secret                      | Positive | —    |
| E-07 | The per-node ConfigMap on a fresh install: the init container finds a full entry  | Negative | —    |

### Operations (design §7, §8, §9, §10)

| #    | Scenario                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------|----------|------|
| E-08 | `Suspend` then `Resume`: the node returns to `online` and accepts new volumes            | Positive | —    |
| E-09 | `Restart` with `reattachVolume`: volumes are reattached and I/O resumes                  | Positive | —    |
| E-10 | Happy-path `Remove` on a node with volumes: drained, removed, `Succeeded`                | Positive | —    |
| E-11 | `Remove` on an empty node: no migrations created, and it completes directly              | Boundary | —    |
| E-12 | `Remove` blocked by a pinned claim: the node stays online until the pin is removed       | Negative | —    |
| E-13 | Sustained fio during a `Remove`: no I/O errors, and checksums match after                | Positive | —    |
| E-14 | A migration fails mid-drain: it is replaced and the drain completes                      | Negative | —    |
| E-15 | `Remove` aborted mid-drain: the node returns to `online` and accepts volumes             | Negative | —    |
| E-16 | `Migrate` to another worker: the node keeps its UUID and its volumes follow              | Positive | —    |
| E-17 | Sustained fio during a `Migrate`: no I/O errors, and checksums match after               | Positive | —    |
| E-18 | `Migrate` with `newSsdPcie`: the added devices appear and survive a later restart        | Positive | —    |
| E-19 | A worker cordoned and drained: the node is shut down, evicted, and returns online        | Positive | —    |
| E-20 | Two workers cordoned at once on a three-node cluster: the second holds for its slot      | Boundary | —    |
| E-21 | Sustained fio through a host maintenance window: no I/O errors on the peers              | Positive | —    |
| E-22 | Transient 5xx from the control plane mid-operation: retried, and the operation completes | Negative | —    |
| E-23 | A drain on a three-node cluster with one peer already offline: the operation blocks      | Boundary | —    |
| E-24 | A drain on a single-node cluster: blocked with `NoMigrationTarget`, node untouched       | Boundary | —    |
| E-25 | A drain of a node holding 100 or more volumes: completes, and the duration is recorded   | Boundary | —    |
| E-26 | A node added mid-drain: the drain's volume set is unaffected                             | Negative | —    |

---

## 4. Manual Scenarios

### M-01: The operator is killed between the write-ahead patch and the backend call

**Design reference:** §7.2.

**What to verify:** that the persisted step does what the removed `status.triggered`
flag used to, which no unit test can show because it requires the process to
actually die between two statements.

**Test concept:**

1. Create a `StorageNodeOps` with `action: Suspend` against an online node.
2. Kill the operator pod the moment the `Requesting` step is persisted, before the
   control plane records a suspend request.
3. Restart the operator and watch the operation resume.
4. Confirm in the control-plane audit log that exactly one suspend was requested,
   or none.
5. Repeat for the `Posting` step of a node's own provisioning, where a duplicate
   call adds a second backend node rather than repeating a no-op.

**Current behavior:** the flag is written after the call, so a crash between the
two leaves the flag false and the call made, which is precisely the case the flag
exists to prevent. Design §7.2 removes the failure mode by reading the target's
state instead.

### M-02: The drained node's host dies mid-drain

**Design reference:** §8.2, §8.3.

**What to verify:** that a drain whose subject disappears reaches a terminal state
rather than looping. If the node is genuinely gone, verification finds no volumes
and the removal succeeds. If it is unreachable but present, the step holds until
its deadline and then resumes and fails.

**Test concept:**

1. Start a `Remove` on node A of a three-node cluster.
2. Wait for `status.step.state` to reach `MigratingVolumes`.
3. Power off the host running node A.
4. Confirm the operation reaches `Succeeded` or `Failed` within the step deadline,
   and never sits in a loop.
5. Confirm the surviving nodes report no volumes stranded on A.

### M-03: Cutting the cluster to its fault-tolerance limit during a drain

**Design reference:** §8.3, §11.

**What to verify:** what the operator does when a drain would take the cluster
below its redundancy floor. The design's stance is that the cluster gate of §7.1
holds the operation, and this scenario is what proves the gate is reached before
the removal rather than after it.

**Current behavior:** the gate checks the cluster's status and its rebalancing
flag, not its fault-tolerance headroom, so a drain on a cluster at its limit
proceeds. Design §16, Q7 owns the question of where that check belongs.

**Test concept:**

1. A three-node cluster with a fault tolerance of one.
2. Take node C offline out of band, which puts the cluster at its limit.
3. Create a `Remove` for node B.
4. Confirm the operation holds rather than removing the last redundant node, and
   that the reason is on the object as an event.
5. Bring node C back and confirm the operation proceeds without further input.

### M-04: A relocation restart whose start is never observed

**Design reference:** §9, §12.

**What to verify:** the one negative completion predicate in the design. The
`Relocating` step completes when the node has left `online`, and a coalescing
stream can deliver `online` before and `online` after without ever delivering the
window in between.

**Test concept:**

1. Start a `Migrate` for a node onto another worker.
2. Throttle or suspend the operator's stream subscription across the restart
   window, so the departure from `online` is not delivered.
3. Confirm the operation either holds until its deadline or completes, and in
   particular that no promote is issued while the restart is in flight.
4. Confirm the relocated devices are not left in `new`.

**Open question:** a restart generation on the node object would make the
observation positive and remove the scenario. Design §16, Q4.

### M-05: An OS upgrade rolling across every worker

**Design reference:** §10.

**What to verify:** the whole maintenance flow under the conditions it exists for,
which is the only way to exercise the interaction between the operator, the
eviction, the kubelet, and the reboot.

**Test concept:**

1. A five-node cluster with a concurrency limit of one.
2. Trigger a rolling OS upgrade across every worker.
3. Confirm exactly one maintenance operation is `Running` at a time, and the rest
   hold at `Holding` with `MaintenanceQueued`.
4. Confirm each node returns to `online` before the next worker is cordoned.
5. Run fio throughout and confirm no I/O errors and matching checksums.
6. Confirm no budget and no drain label survives the run.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 248       | 87      | 161         |
| Integration | 45        | 1       | 44          |
| E2E         | 26        | 0       | 26          |
| Manual      | 5         | 0       | 5           |
| **Total**   | **324**   | **88**  | **236**     |

Eighty-seven of the eighty-eight covered scenarios are unit tests, and they
concentrate in three places: the workload builders, the drain's volume
classification, and the operation lock. That is the right distribution, because a
defect in any of the three corrupts state rather than failing safely. Every
covered scenario outside a unit test is `I-16`.

Eighty-four distinct test functions cover those eighty-eight scenarios, because a
table-driven test satisfies one identifier per subtest.

The ratio is what a target-model plan looks like against a registered API. Most
uncovered rows are not gaps in testing so much as scenarios for behavior that does
not exist yet, and §6 separates the two.

---

## 6. What Is Not Yet Covered

| #                          | Gap                                                                                           | Reason                                                                                                                                                                                                                                                                           |
|----------------------------|-----------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-07, U-08                 | Name collision retry, and the name carrying no worker                                         | The retry path exists and is unexercised. The negative assertion about the name has never been written down                                                                                                                                                                      |
| U-12                       | A fault group of 0 read as set rather than unset                                              | `effectiveFailureDomain` returns 0 for both today, which is the defect the row would catch                                                                                                                                                                                       |
| U-23, U-25 … U-27          | The parallel-add hold and the FoundationDB serialization rules                                | `isFDBWorkerBlocked` has no test reference, and it is the rule that keeps a control plane from losing quorum during an expansion                                                                                                                                                 |
| U-28 … U-35                | The whole `Posting` claim                                                                     | Planned, not built. The guard today is `status.postedAt` plus a `List`, which the optimistic-lock claim of design §4.2 replaces                                                                                                                                                  |
| U-38 … U-40, U-43          | Positional UUID resolution for multi-socket workers                                           | `pollUUIDFromBackend` has no test reference, and the RPC-port ordering it depends on is an assumption nothing asserts                                                                                                                                                            |
| U-44 … U-47                | Every adoption route                                                                          | No test reference at all, and adoption is the migration path off Helm and the path every pre-existing cluster takes                                                                                                                                                              |
| U-48 … U-52, U-236 … U-238 | Steady-state sync, and the device counts                                                      | `syncStatus` has no test reference. The early return is what keeps an idle cluster from writing, and the device rows pin an ordering the string form got backward (design §15.1)                                                                                                 |
| U-56 … U-59                | Deletion while an operation runs, and the non-online cases                                    | The finalizer hold has no test, and it is what stops a delete from orphaning a backend node                                                                                                                                                                                      |
| U-64                       | A look-alike service account in another namespace                                             | The prefix match is string-based, and nothing asserts it cannot be spoofed by a namespace name                                                                                                                                                                                   |
| U-75, U-76                 | Workload ownership by the `StorageCluster`                                                    | Planned, not built. The objects are owned by `StorageNodeSet` today, and the reparent is design §15.3                                                                                                                                                                            |
| U-78 … U-84                | The per-slot storage-node-uuid labels                                                         | The most consequential labels in the operator, and the only covered part is the node-type label. A wrong key here breaks CSI provisioning                                                                                                                                        |
| U-90 … U-94                | Per-node divergence in the ConfigMap                                                          | Only the cluster-uniform half is covered. The shell-quoting row matters because device names reach a sourced file                                                                                                                                                                |
| U-244 … U-247, I-40 … I-45 | The widened `deviceNames`                                                                     | Design §3.1 has the field take a PCI address and a device path in one list. Nothing implements either form yet, and `I-44` and `I-45` are what hold the item pattern                                                                                                             |
| U-116 … U-124              | Terminal re-reconcile, the acquire race, and the cluster gate                                 | The lock's happy paths are covered and its concurrent ones are not. The cluster gate applies to one action today, design §7.1 widens it                                                                                                                                          |
| U-125 … U-134              | Every single-step action                                                                      | `runSimpleAction` has no test reference, and its skip-if-already-there behavior is what design §7.2 replaces `status.triggered` with                                                                                                                                             |
| U-144 … U-146              | Classification boundaries                                                                     | The buckets are covered individually and their overlaps are not                                                                                                                                                                                                                  |
| U-147 … U-169              | The remove graph past classification                                                          | Only the helpers are covered. Every step, every hold, and the whole resume path are untested                                                                                                                                                                                     |
| U-172 … U-177, U-248       | `PersistentVolumeOps` lifecycle, and the reference and label that replace the owner reference | The name generation is covered and the lifecycle is not                                                                                                                                                                                                                          |
| U-184 … U-200              | The migrate graph                                                                             | Only the configuration merge is covered. The DNS gate, the two-step restart observation, and the abort refusal are untested                                                                                                                                                      |
| U-201 … U-216              | The whole host maintenance action                                                             | Planned, not built. It is a separate controller driving `StorageNodeSet.status.drainCoordination` today, which design §10 replaces                                                                                                                                               |
| U-217 … U-235              | The step machine, and the three lists that have to agree                                      | Planned, not built. `atlas-lib/statemachine` has no consumer in either component yet. `U-231` to `U-233` are what the shared `KubeSnapshot` makes necessary: the step values live in the graph, in the `Enum` marker, and in the CEL rule, and only a test keeps the three level |
| I-01 … I-15, I-46 … I-52   | Every admission rule                                                                          | Needs `envtest`, because CEL, `Required`, and defaulting are enforced by the API server and a fake client applies none of them                                                                                                                                                   |
| I-17 … I-28                | Lock behavior, cascade, and isolation under a real API server                                 | Needs `envtest` for real `resourceVersion` conflicts, real watch delivery, and real garbage collection                                                                                                                                                                           |
| I-37 … I-39                | The deployment config's ephemerality                                                          | Nothing to test yet: `ClusterDeploymentConfig` does not exist, and the registered model rewrites `spec.overrides` from the set on every reconcile, which is the behavior design §3.1 replaces                                                                                    |
| E-01 … E-26                | All end-to-end scenarios                                                                      | Needs a live cluster. The e2e harness under `test/` is not committed yet                                                                                                                                                                                                         |
| M-01 … M-05                | Crash consistency, host death, degraded clusters, and the roll                                | Need process kills, host power control, and a real OS upgrade                                                                                                                                                                                                                    |
| Metrics                    | The eleven metrics of design §13.2                                                            | Designed, not built. Nothing exports a metric for either kind today                                                                                                                                                                                                              |
| Events                     | The twenty reasons of design §13.1                                                            | Fourteen reasons exist under different names, and no test asserts any of them                                                                                                                                                                                                    |
| Retention                  | Nothing deletes a terminal `StorageNodeOps`                                                   | Feature does not exist. Design §16, Q6                                                                                                                                                                                                                                           |

### Axis coverage

The axes are the ones that actually break this operator. A blank cell is a
combination nothing exercises.

| Axis                      | Value                     | Scenarios                                                       |
|---------------------------|---------------------------|-----------------------------------------------------------------|
| Cluster node count        | Single node               | U-155, E-24                                                     |
|                           | Two nodes                 | U-155                                                           |
|                           | Three nodes               | E-13, E-20, E-23, M-02, M-03                                    |
|                           | Five or more              | E-02, E-25, M-05                                                |
|                           | Asymmetric node sizes     | —                                                               |
| Sockets per worker        | One                       | Every scenario except those below                               |
|                           | Two or more               | U-21, U-30, U-31, U-39, U-40, U-205, E-04                       |
| Namespace count           | Single namespace          | Every scenario except those below                               |
|                           | Multiple namespaces       | U-72, U-145, U-177, I-23, I-26                                  |
| simplyblock cluster count | One cluster               | Every scenario except those below                               |
|                           | Several in one Kubernetes | U-81, I-21                                                      |
|                           | Cross-cluster             | — (not applicable: no node operation spans Kubernetes clusters) |
| Failure domains           | Not enabled               | U-11                                                            |
|                           | Enabled and set           | U-10                                                            |
|                           | Enabled and unset         | U-09, U-13                                                      |
|                           | Partially set             | —                                                               |
| Object scale              | Zero volumes              | U-139, U-140, U-169, E-11                                       |
|                           | A handful                 | U-152, E-10, E-13                                               |
|                           | 100 or more               | E-25                                                            |
| Lifecycle and restart     | Mid-step restart          | U-172, M-01                                                     |
|                           | Terminal re-reconcile     | U-117                                                           |
|                           | Deletion mid-operation    | U-119, I-19, I-27                                               |
|                           | Host death mid-operation  | M-02                                                            |
| Actor                     | Operator-raised operation | U-54, U-201                                                     |
|                           | User-created operation    | Every operation scenario except those two                       |
|                           | Webhook path              | U-60 … U-64, I-08, I-09                                         |

**The asymmetric-node row is the significant blank.** Every drain and every
migration picks a target from the online peers by round-robin, which spreads by
count rather than by capacity, so a cluster whose nodes differ in size
concentrates on the smallest one exactly as readily as on the largest. Nothing
here exercises that, and design §8.2 does not claim otherwise.

**The partially set failure-domain row is the second.** A cluster with
`enableFailureDomains` set where some nodes declare a group and others do not is
the state a half-finished expansion leaves behind, and the gate of §4.2 is
per-node, so the cluster runs in a mixed state nothing reports on.

**The multi-namespace rows are covered thinly and matter more than the count
suggests.** Two namespaces are two independent deployments, and the workload
objects are namespaced while the storage-plane labels and the ClusterRoleBindings
are not. `U-72` covers the binding names and `U-81` covers the label keys, and
nothing covers what happens when two namespaces claim the same worker.
