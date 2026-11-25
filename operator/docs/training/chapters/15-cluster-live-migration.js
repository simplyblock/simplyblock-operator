// Moving every volume of one storage cluster onto another while the workloads
// run: a ReplicationOps with action migration at scope cluster, as the code
// implements it. The chapter follows the objects in the order they are
// written, the target cluster, the pair, the migration policy, the dry run,
// and the real run, then what the operator and the slots do until the plan
// ends, and what is left behind. It is the last of the three chapters on
// moving data. volume-migration-nodes moves a volume inside one cluster, and
// volume-migration-clusters uses the same machinery as this chapter for one
// volume, one policy, or one pair.
TRAINING.register({
  id: 'cluster-live-migration',
  title: 'Live-migrating an entire storage cluster',
  summary: 'Every bound claim of one storage cluster moved onto a second cluster of the same control plane while its workloads run: the preparation, the dry run and its forecast, one migration plan, the cutovers, and what remains of the source afterward.',
  goal: 'after the chapter the reader can prepare a target cluster, forecast a whole-cluster move, run it with `deleteSource`, read its progress and outcome, and say what the operator leaves to a person before the source cluster can be retired.',

  sections: [
    {
      heading: 'What moves, and what the framing means in the code',
      body: [
        'The chapter was commissioned as "live-migrating an entire storage cluster." In the code a cluster is not moved. Its **volumes** are. The source StorageCluster, its storage nodes, its pools, and its StorageClasses stay where they are, and the operator moves the volume behind every bound claim of that cluster onto a second StorageCluster that has to exist already. "Live" means what the code does for each volume: the data is copied while the workload keeps writing, and then the paths switch to the target cluster under one short I/O freeze, without the pod being restarted or the volume remounted. How long that freeze lasts is decided by the control plane, and the repository does not state a figure for this path.',
        'The move is a `ReplicationOps` with `action: migration` and `scope: cluster`. Its `ref` names the source StorageCluster, and `spec.policyRef` names a `ReplicationPolicy` in `migration` mode whose pair runs from that cluster to the target. The replication kinds are the `v1alpha1` family the [asynchronous DR chapter](chapter.html?c=async-dr) teaches. This chapter assumes that one and covers what is specific to moving a whole cluster.',
        { table: { head: ['Object', 'Who writes it', 'Its part in the move'], rows: [
          ['`ClusterDeploymentConfig`', 'the administrator', 'creates the target StorageCluster on new workers'],
          ['`ReplicationPair`', 'the administrator', 'the direction: source cluster to target cluster'],
          ['`ReplicationPolicy`, `mode: migration`', 'the administrator', 'what unreplicated claims are enrolled under'],
          ['`ReplicationOps`, `dryRun: true`', 'the administrator', 'the forecast, with nothing changed'],
          ['`ReplicationOps`', 'the administrator', 'the move, with or without `deleteSource`'],
          ['`ReplicationSlot`', 'the operator', 'one per enrolled claim, through which each cutover runs'],
        ] } },
        'Both clusters are clusters of **one control plane**. Replication, and therefore migration, works between clusters the same control plane addresses, and nothing in the API names a second ControlPlane. The common case is a hardware refresh inside one Kubernetes cluster: new workers join, a second StorageCluster is deployed on them, the volumes move, and the old cluster is retired afterward by a person.',
        { callout: 'Removing a member from a supercluster, the `StorageClusterOps` action `RemoveMember` that would move a member\'s volumes into the others and then delete the member, is planned in the supercluster design (Phase 4) and not built. It is not in the action enum. A cluster-scoped `ReplicationOps` is the only whole-cluster move the operator implements today.', kind: 'note' },
      ],
    },
    {
      heading: 'Preparing the target, the pair, and the policy',
      body: [
        'Three objects come first, in this order. The **target cluster** is an ordinary StorageCluster, created by an approved ClusterDeploymentConfig on workers of its own, and it has to reach `Online` with a `status.uuid`, because every later step resolves it by that UUID. Once it is registered, the StorageCluster reconciler writes its entry into `simplyblock-csi-secret-v2`, together with an entry for every other cluster the control plane lists, so the CSI driver can reach both clusters, a target whose StorageCluster object lives on another Kubernetes cluster included. Such a target is named in the pair by its UUID. A stage that follows a moved volume and finds no entry for its cluster fails with a log line saying the target cluster is not in the secret file.',
        'The **pair** names `sourceCluster` and `targetCluster`. A name is looked up among the StorageClusters of the pair\'s namespace, a UUID is passed through unresolved, and a supercluster aggregate is refused on either side. The pair reconciler creates the backend replication target and sets `status.ready`. The **policy** names the pair and sets `mode: migration`. Its `interval` and `snapshotRetention` govern the replication that runs between enrollment and the plan, and the retention may not go below 2.',
        'Then the conditions no object states, which the reader checks:',
        { list: [
          '**Capacity on the target** for every volume of the source. The operator checks none. The control plane places the volumes on target nodes when it plans, may refuse the plan, and reports the risks it did not refuse for in `status.warnings`, such as too little free capacity on the source for the snapshots the copy pins. Read the warnings of the dry run before the real run.',
          '**Reachability.** The storage networks of the two clusters must reach each other for the copy, and every consuming host must reach the target\'s storage nodes, because the cutover connects the target\'s paths on the consumer node before the switch.',
          '**No encrypted volumes.** Nothing on this path refuses an encrypted volume. The supercluster design records that the volume replication creates on the target has no crypto layer and that `delete_source` deletes the only key. That is not confirmed in the control plane\'s code here, and an encrypted cluster move is not to be treated as working.',
          '**Claims under DR replication.** A claim follows one policy at a time. A claim already enrolled under a `failover` policy is skipped by the cluster migration and stays on the source, as the fourth section shows.',
        ] },
      ],
      examples: [
        { title: 'The target cluster', file: 'examples/cluster-live-migration/01-target-clusterdeploymentconfig.yaml', notes: [
          { at: 'name: gen2', text: 'The StorageCluster the document creates, and the name the pair uses for the target. The default pool becomes `gen2-default` with its own StorageClass.' },
          { at: 'maxSubsystemCount: 50', text: 'One of the two required sizing values, with `vcpuCount`. Everything else in `cluster` has a default.' },
          { at: 'gen2-worker-0', text: 'New workers. The source cluster gen1 keeps its own workers until somebody retires it.' },
          { at: 'approved: true', text: 'The review gate. The document creates nothing until it is set, and the move cannot start until the cluster it creates is `Online`.' },
        ] },
        { title: 'The pair', file: 'examples/cluster-live-migration/02-replicationpair.yaml', notes: [
          { at: 'sourceCluster: gen1', text: 'The cluster to empty, a StorageCluster of this namespace. The ReplicationOps names the same cluster in `ref`, and the operation fails if the two do not resolve to one UUID.' },
          { at: 'targetCluster: gen2', text: 'Resolved to gen2\'s `status.uuid`. Immutable once the pair exists.' },
        ] },
        { title: 'The migration policy', file: 'examples/cluster-live-migration/03-replicationpolicy-migration.yaml', notes: [
          { at: 'pairRef: gen1-to-gen2', text: 'The pair in the same namespace.' },
          { at: 'mode: migration', text: 'Required for a cluster-scoped migration. A policy in `failover` mode is refused at admission.' },
          { at: 'snapshotRetention: 2', text: 'The CRD\'s minimum. The default is 3.' },
        ] },
      ],
    },
    {
      heading: 'Admission, the dry run, and reading the forecast',
      body: [
        'An admission webhook checks every `ReplicationOps` on create. For a cluster-scoped migration it refuses:',
        { list: [
          '`scope: cluster` with any action other than `migration`.',
          '`scope: cluster` without `spec.policyRef`, and `spec.policyRef` with any other scope.',
          'A `policyRef` that names no ReplicationPolicy in the operation\'s namespace, or one that is not in `migration` mode.',
          'A `ref` that resolves to no StorageCluster of the namespace with a `status.uuid`. A UUID passes as it is.',
          '`spec.dryRun` on any action other than `migration`.',
        ] },
        'The operation lives in the policy\'s namespace, because the reconciler looks the policy up there, and the source cluster named in `ref` is looked up in the same namespace. Admission does not check that the policy\'s pair starts at the cluster in `ref`. The reconciler does, and fails the operation with a message saying which cluster the policy replicates from.',
        'The **dry run** comes first. It resolves the scope exactly as the real run will, annotates nothing, and sends one plan request with `dry_run` set. The control plane answers with the plan it would run, and the operation ends `Succeeded` with the event `MigrationForecast`. The forecast and the per-volume plan are left in the status, and the dry run records no `planID`.',
        { table: { head: ['Field', 'What it says'], rows: [
          ['`status.results[]`', 'one entry per volume, with `pvcRef`, the `targetNodeID` the plan chose, `bytesRemaining`, and `etaSeconds`, plus the claims the scope skipped and why'],
          ['`status.forecast.bytesRemaining`', 'the data to copy across all volumes'],
          ['`status.forecast.etaSeconds`, `completesAt`', 'the forecast time until the last volume has cut over'],
          ['`status.forecast.throughputBytesPerSec`, `throughputMeasured`', 'the rate the forecast assumes, and whether it was measured or a default'],
          ['`status.forecast.converging`, `dirtyBytesPerSec`, `notConvergingLVS`', 'whether replication outruns the writes, the write rate, and the volume stores that it does not outrun'],
          ['`status.warnings`', 'the risks the control plane found and did not refuse the plan for'],
        ] } },
        { code: 'kubectl get replops forecast-gen1-move -n simplyblock -o wide\nkubectl get replops forecast-gen1-move -n simplyblock -o jsonpath=\'{.status.forecast}\'\nkubectl get replops forecast-gen1-move -n simplyblock -o jsonpath=\'{.status.warnings}\'', title: 'Reading the forecast' },
        'Against a control plane that has no migration plans, the plan endpoint answers 404, and the dry run fails with a message saying the control plane cannot forecast a migration. The real run on such a control plane still works through an older path, covered in the sixth section, but without a forecast.',
        '`dryRun` cannot be taken off an operation once set, and a finished operation is not run again. The real run is a second `ReplicationOps`.',
      ],
      examples: [
        { title: 'The dry run', file: 'examples/cluster-live-migration/04-replicationops-dryrun.yaml', notes: [
          { at: 'namespace: simplyblock', text: 'The policy\'s namespace, which is also where the StorageCluster gen1 is looked up.' },
          { at: 'scope: cluster', text: 'Every bound claim whose volume lives on gen1, in every namespace. Allowed with `action: migration` only.' },
          { at: 'ref: gen1', text: 'The source StorageCluster by name, or its UUID. It must match the source of the policy\'s pair.' },
          { at: 'policyRef: gen1-move', text: 'Required with scope `cluster`, refused with any other scope, and immutable.' },
          { at: 'dryRun: true', text: 'The plan and the forecast only. Nothing is enrolled, replicated, or cut over.' },
        ] },
      ],
    },
    {
      heading: 'The real run: the lock, scope resolution, and enrollment',
      body: [
        'The real run takes the **lock** first: `ReplicationPolicy.status.activeOpsRef` on the policy named by `policyRef`. A second operation on the same policy does not start, keeps an empty phase, and is checked again every 10 seconds. The lock is released when the operation ends `Succeeded` or `Failed`, or is deleted. The operation then moves to `Running` and works through its subphases, deciding on every pass from `status.subphase` and `status.planID` alone.',
        { table: { head: ['Subphase', 'What the operation waits for'], rows: [
          ['`Enrolling`', 'a claim it annotated has no slot yet'],
          ['`AwaitingReplication`', 'every slot of the scope reports `replicating`'],
          ['`CreatingPlan`', 'written just before the plan request'],
          ['`TrackingPlan`', 'the plan, read back every 30 seconds until it ends'],
          ['`AwaitingCutover`', 'only on a control plane without migration plans: each slot reaching `cutover_done`'],
        ] } },
        '**Scope resolution** lists the PersistentVolumeClaims of every namespace, because workloads keep their claims in their own namespaces while the replication objects live in the storage namespace. A claim that is not `Bound`, is being deleted, has no CSI volume, or carries a handle that is not simplyblock\'s is not considered. For the rest, "on the source" means where the volume lives now, not what the PersistentVolume\'s handle says: the handle is fixed at provisioning and names the cluster a moved volume left, so the operator follows the control plane\'s replication relationships from the handle to the current volume. A claim whose slot is already attached is located through the slot.',
        { table: { head: ['The claim', 'What the scope does with it'], rows: [
          ['volume lives on another cluster, and never lived on the source', 'leaves it out of the results'],
          ['volume started on the source and has moved elsewhere', 'reports it `skipped`, "moved to cluster ..."'],
          ['live slot under another policy', 'reports it `skipped`, "follows replication policy ..."'],
          ['no slot, and its own or its StorageClass\'s annotation names another policy', 'reports it `skipped`, "assigned replication policy ..."'],
          ['slot under `policyRef`', 'includes it as it is'],
          ['no slot and no other policy assigned, a slot that never attached, or a slot that finished an earlier migration', 'enrolls it'],
        ] } },
        '**Enrolling** sets the annotation `storage.simplyblock.io/replication-policy` on the claim, written `simplyblock/gen1-move` for a claim outside the policy\'s namespace. The operation never overwrites an annotation that names another policy. The claim watcher then creates the slot beside the claim, named `<policy>-<claim>` and owned by it, exactly as for a claim a person annotated, and the slot reconciler attaches the volume and reports `replicating`. A slot left behind by an earlier migration under the same policy is deleted so that the watcher can create a fresh one under its name.',
        'While it waits, the operation writes each volume into `status.results` as `pending` with a reason, and checks again every 15 seconds. A slot in `error` becomes a `failed` result, a slot in `cutover_done` is `skipped` as already cut over, and a slot in `failed_over` is `skipped` with the advice to fail it back before migrating. Claims bound on the source after the plan has been created are not added to it.',
        { callout: 'Every `skipped` claim stays on the source, and a skip does not fail the operation. A run can end `Succeeded` with claims still on the cluster that is about to be retired. Read the skipped results before retiring anything.', kind: 'warn' },
      ],
      examples: [
        { title: 'The move', file: 'examples/cluster-live-migration/05-replicationops-migration.yaml', notes: [
          { at: 'name: move-gen1', text: 'Its namespace, name, and UID become the plan\'s name, which is how a restarted operator finds the plan again.' },
          { at: 'policyRef: gen1-move', text: 'The lock holder and the policy unreplicated claims are enrolled under.' },
          { at: 'deleteSource: true', text: 'The control plane deletes each source volume after its cutover, and the operator marks the PersistentVolume with where the data lives now.' },
        ] },
      ],
    },
    {
      heading: 'One plan, the cutovers, and convergence',
      body: [
        'Once every volume of the scope replicates, the operator writes `subphase: CreatingPlan` and then sends one request to `POST /api/v2/clusters/<source>/migration-plans/` with every volume\'s id, the target cluster\'s UUID when it resolves locally, `delete_source`, and the name `replicationops/<namespace>/<name>/<uid>`. The order matters. A pass that dies after the request but before `status.planID` is written finds `CreatingPlan` on its next run, lists the plans, and adopts the one carrying its name rather than creating a second plan. Once the id is recorded the subphase is `TrackingPlan`, and the event `MigrationPlanCreated` carries the volume count and the first forecast.',
        'From then on the operator reads the plan every 30 seconds and mirrors it: each volume\'s `state`, `targetNodeID`, `bytesRemaining`, and `etaSeconds` into `status.results`, and the plan\'s forecast into `status.forecast`. A read that fails is retried and is not a failure, because the plan keeps running in the control plane. A plan that answers 404 has been deleted, and the operation fails.',
        { table: { head: ['Per-volume `state`', 'Meaning', '`status`'], rows: [
          ['`replicating`', 'initial copy or catch-up', '`pending`'],
          ['`queued`', 'caught up, waiting for its turn to cut over', '`pending`'],
          ['`converging`', 'shrinking the final delta', '`pending`'],
          ['`cutting_over`', 'I/O frozen, paths switching', '`pending`'],
          ['`done`', 'cut over', '`succeeded`'],
          ['`failed`, `canceled`', 'did not move', '`failed`'],
          ['`skipped`', 'left out by the control plane', '`skipped`'],
        ] } },
        'The plan runs the cutovers **one at a time per source volume store**, so that each freeze stays short. The operator does not order them. Each cutover runs through the volume\'s slot, which polls the backend every 15 seconds while it is `replicating` and, on seeing `cutover_pending`, does this:',
        { steps: [
          'Reads the target paths from `GET .../volumes/<volume>/connect` on the source cluster.',
          'Finds the node of a running pod that mounts the claim, the first such node by name.',
          'Runs a preconnect Job (`--mode=replication-preconnect`) pinned to that node, which connects the target\'s paths, and emits `PreconnectStarted` on the slot.',
          'Once the Job has succeeded, sends `POST .../replication/cutover-proceed`. The control plane then switches the ANA states, and the kernel\'s multipath follows them to the target.',
        ] },
        'A failed preconnect Job is deleted and created again, with the warning `PreconnectFailed`, and proceed is not signaled, because proceeding would take the source paths away from a consumer that has no target path. A volume with no running consumer is signaled at once. The control plane does not wait forever: after its safety timeout, `REPL_CUTOVER_PROCEED_TIMEOUT_SEC`, it proceeds alone. The value lives in the control plane and the repository does not settle it: the supercluster design quotes 120 seconds and a comment in the slot reconciler about 30.',
        '**Convergence** is the control plane\'s verdict, mirrored into `status.forecast.converging`. `false` means clients write some source volume store at least about as fast as it replicates, so its lag never closes and no cutover can start there. The forecast\'s `etaSeconds` and `completesAt` are then left unset, `notConvergingLVS` names the stores, and `dirtyBytesPerSec` gives the write rate. The operator emits one `NotConverging` warning when the verdict turns false and one `Converging` event when it turns true again, not one per poll. It does nothing else. It does not throttle writers, reorder volumes, or stop the plan. Lowering the writes is the reader\'s decision.',
        { code: 'kubectl get replops move-gen1 -n simplyblock -o wide -w\nkubectl get events -n simplyblock --field-selector involvedObject.name=move-gen1\nkubectl get replicationslots -A', title: 'Following the move' },
      ],
    },
    {
      heading: 'How it ends: success, partial failure, and the relocated-to mark',
      body: [
        'A plan that reports `done` ends the operation `Succeeded`, with the event `MigrationComplete`, as long as no result is `failed`. A plan that ends `partial`, `failed`, or `canceled` ends the operation `Failed`, with the warning `Failed`, a message counting the volumes that did not migrate, and every result kept. A volume whose cutover the control plane abandoned goes back to `replicating` on its slot, with the warning `CutoverAbandoned`, so a later migration can pick it up. Either way the lock is released and the operation is finished: nothing retries it.',
        'With **`deleteSource: true`** the control plane deletes each source volume after its cutover. The PersistentVolume\'s `volumeHandle` is immutable and still names that deleted volume, so on every poll the operator marks each PV whose result is `succeeded` with the annotation `storage.simplyblock.io/relocated-to`, holding the handle of the volume that serves it now. The same mark is written by a supercluster Relocate.',
        { code: 'apiVersion: v1\nkind: PersistentVolume\nmetadata:\n  name: pvc-5b0e2c71-8d4a-4f3e-9a61-2c7d0e9b4f18\n  annotations:\n    storage.simplyblock.io/relocated-to: 9d3f6a2e-1b4c-4e7d-8f50-3a2b1c0d9e87:4e8a1f2b-6c3d-4a5e-9b70-1d2c3e4f5a60:c2a7e9f1-3d4b-4c8a-9e10-5f6a7b8c9d01\nspec:\n  csi:\n    driver: csi.simplyblock.io\n    volumeHandle: 3a9c7e15-6b2d-4f80-a1c4-8e5d2b7f0c93:b7d1e4a2-5c8f-4e3b-9a06-2f1d8c7e4b55:f0e1d2c3-b4a5-4968-8776-5a4b3c2d1e0f', title: 'An excerpt of a migrated PersistentVolume' },
        'Both values are handles of the form `<cluster>:<pool>:<volume>`. The handle names gen1 and the volume that was deleted, and the mark names gen2 and the volume that serves the claim. The CSI driver uses the mark twice. A stage starts resolving the volume from the mark, and when a handle\'s volume answers 404 the driver walks the replication chain from the mark, or from the handle when there is none. `DeleteVolume` deletes the volume the mark names, when that is still the active one. An unmarked handle never has its active volume deleted, because after a DR failover a PV on another Kubernetes cluster can carry the same handle.',
        'Without `deleteSource` the source volumes stay and no mark is written. Whether a later stage through the old handle reaches the target then depends on how the control plane answers a connect request for a source volume that has cut over, which the repository does not show and no test covers end to end.',
        'On a control plane **without migration plans**, the 404 on the plan request sends the real run down an older path. It commits each volume with `POST .../replication/commit`, `delete_source` included when set, puts each slot into `cutover_pending`, emits `MigrationCommitQueued`, and follows the slots in subphase `AwaitingCutover` until each is `cutover_done`, which is a `succeeded` result, or in `error` or deleted, which is a `failed` one. The cutovers still run through the slots as above. There is no forecast and no per-volume state, and the operator writes the relocated-to mark only from a tracked plan, so this path leaves the PVs unmarked even with `deleteSource`. How the control plane orders those commits is not shown in the repository.',
      ],
    },
    {
      heading: 'What is left afterward, and the limits',
      body: [
        'A finished migration leaves more behind than the moved volumes:',
        { list: [
          '**The source StorageCluster**, with its nodes, pools, and StorageClasses. The operator does not delete it, and its StorageClass still provisions new claims on the source.',
          '**The slots**, one per moved claim in the claim\'s namespace, in `cutover_done` and still following `gen1-move`. The policy cannot be deleted while they do.',
          '**The annotation** `storage.simplyblock.io/replication-policy` on every claim the run enrolled.',
          '**The PersistentVolumes** with their original handles, marked with `relocated-to` when `deleteSource` was set on the plan path.',
        ] },
        'Retiring the source is a separate, manual step, and the order is the reader\'s to keep. Deleting a StorageCluster deletes its backend cluster and removes its entry from `simplyblock-csi-secret-v2`. A PV that still names that cluster and carries no mark then has nothing to resolve its handle through: the cluster\'s records, which the chain walk starts from, are gone, and so is the driver\'s way of reaching them. Every migrated PV has to carry its mark before the source goes. **Nothing enforces this.** The StorageCluster webhook refuses a deletion only while a supercluster member names the cluster, and no finalizer, admission rule, or reconciler check counts unmarked PVs.',
        { code: 'kubectl get replops move-gen1 -n simplyblock -o jsonpath=\'{range .status.results[*]}{.pvcRef}{"\\t"}{.status}{"\\t"}{.detail}{"\\n"}{end}\'\nkubectl get pv -o custom-columns=NAME:.metadata.name,HANDLE:.spec.csi.volumeHandle,MARK:.metadata.annotations.storage\\.simplyblock\\.io/relocated-to', title: 'Checking before the source is retired' },
        'The limits of this path, as the code stands:',
        { list: [
          '**Cancellation releases the lock and nothing else.** There is no `spec.abort`. Deleting a running operation clears `activeOpsRef`, and the plan keeps running in the control plane, which can cancel a plan through its own API. The design lists a mapping from Kubernetes to that call as an open question.',
          '**Preconnect covers one consumer node.** The slot resolves the first node, by name, of the running pods that mount the claim. A volume mounted on several nodes gets the target\'s paths connected on that node only before the switch.',
          '**Encrypted volumes are unconfirmed.** No guard refuses them on this path, and the design records a key-loss risk with `delete_source`.',
          '**No live two-cluster end-to-end test.** The design\'s scenarios are unit tests with a fake client and a fake control plane. Comments in the code cite fixes found on a test bed, which are not tests in the repository.',
          '**Other scopes stay in one namespace.** Only scope `cluster` looks across namespaces. Scopes `policy`, `volume`, and `target`, which the [chapter on moving volumes between clusters](chapter.html?c=volume-migration-clusters) covers, collect the slots of the operation\'s namespace only.',
          '**Removing a supercluster member** (`RemoveMember`) is planned, not built.',
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The operation is refused at admission over `ref`.', cause: 'No StorageCluster of that name exists in the operation\'s namespace, or it has no `status.uuid` yet.', fix: 'Create the operation in the namespace of the policy and the source cluster, and wait for the cluster to report its UUID.' },
          { symptom: 'The operation is refused because `policyRef` is in mode `failover`.', cause: 'The DR policy was named instead of a migration policy.', fix: 'Write a separate policy on the same pair with `mode: migration`.' },
          { symptom: 'The operation fails at once with "replicates from cluster ..., not from ...".', cause: 'The policy\'s pair starts at another cluster than the one in `ref`.', fix: 'Name the pair\'s source in `ref`, or write a pair from the cluster being emptied.' },
          { symptom: 'The operation never starts, and its phase stays empty.', cause: 'Another operation holds `activeOpsRef` on the same policy.', fix: 'Wait for it to end, or delete it. Deleting a running migration leaves its plan running in the control plane.' },
          { symptom: 'The run sits in `Enrolling` or `AwaitingReplication`.', cause: 'A slot was not created yet, or has not reached `replicating`, for example because the policy or the pair is not ready.', fix: 'Read the `pending` results, which give each volume\'s reason, and the slot\'s `status.message`.' },
          { symptom: 'The run ends `Succeeded` and some claims are still on the source.', cause: 'They were skipped, because another policy holds them by slot or annotation or their slot is failed over, or they were bound after the plan was created and never entered it.', fix: 'Read the skipped results and list the claims bound on the source, and decide for each before the source is retired. A second run picks up the claims bound since, and skips the ones another policy holds again, because the operation never takes a claim from another policy.' },
          { symptom: '`TrackingPlan` with `converging: false` and no ETA.', cause: 'Writes to the stores in `notConvergingLVS` outrun replication, so their cutovers cannot start.', fix: 'Lower the writes to those volumes. The operator reports the condition and does not throttle anything.' },
          { symptom: 'A slot emits `PreconnectFailed` repeatedly.', cause: 'The Job on the consumer node cannot connect the target\'s paths: the host cannot reach the target\'s storage nodes, or its pod is refused. The Job runs privileged on the host network in the claim\'s namespace, so a namespace whose Pod Security level or SCC binding refuses that refuses the Job\'s pod.', fix: 'Check the network path from the consumer node to the target and the Job\'s events. The control plane proceeds alone after its timeout, and a consumer without target paths then loses its I/O.' },
          { symptom: 'A dry run fails with a message that the control plane cannot forecast a migration.', cause: 'The control plane predates migration plans and answers 404.', fix: 'Run without `dryRun`, which falls back to one commit per volume, or upgrade the control plane. The fallback writes no relocated-to mark.' },
          { symptom: 'After the source cluster was deleted, pods of moved claims fail to stage.', cause: 'Their PVs carried no relocated-to mark, and the source\'s records and CSI entry are gone.', fix: 'There is no repair path in the operator. Retire the source only after every moved PV is marked.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Framing and preparation', cat: 'concept', text: 'What does a cluster-scoped migration move?', options: ['The volumes behind the bound claims whose data lives on the source cluster', 'The StorageCluster object and its StorageNodes', 'The source cluster\'s StoragePools and StorageClasses', 'The control plane', 'The storage nodes\' SPDK pods onto new workers'], correct: [0], explain: 'The cluster itself stays. Its volumes are copied to a second cluster of the same control plane and cut over, and the source StorageCluster, its nodes, pools, and classes remain until somebody retires them.' },
    { topic: 'Framing and preparation', cat: 'concept', text: 'Which of these have to exist before the migration\'s ReplicationOps is written?', options: ['A target StorageCluster of the same control plane', 'A ReplicationPair from the source to the target', 'A ReplicationPolicy in mode `migration` naming that pair', 'A ReplicationSlot per claim, written by hand', 'A second ControlPlane for the target'], correct: [0, 1, 2], explain: 'The target, the pair, and the migration policy come first. Slots are created by the operator when claims are enrolled, and replication works between clusters of one control plane, so there is no second ControlPlane.' },
    { topic: 'Framing and preparation', cat: 'field', text: 'Which values of `ReplicationPair.spec.targetCluster` resolve for this chapter\'s target gen2?', options: ['`gen2`, the StorageCluster\'s name in the pair\'s namespace', 'The backend UUID of gen2', 'The name of the ClusterDeploymentConfig, when it differs from the cluster\'s', '`simplyblock/gen2`, a namespaced reference', 'The name of a supercluster gen2 belongs to'], correct: [0, 1], explain: 'A name is looked up among the StorageClusters of the namespace and a UUID passes through. There is no namespaced form, a document\'s name is not a cluster\'s, and a supercluster aggregate is refused.' },
    { topic: 'Framing and preparation', cat: 'concept', text: 'The target is a StorageCluster of the same Kubernetes cluster. Who writes its entry into `simplyblock-csi-secret-v2`?', options: ['The StorageCluster reconciler, which writes one entry for its own cluster and one for every other cluster the control plane lists', 'The administrator, by hand, before the move', 'The ReplicationPair reconciler', 'The migration plan', 'The preconnect Job'], correct: [0], explain: 'Every StorageCluster reconciler registers its own cluster and the peers its control plane lists, so the driver can reach the target without a hand edit of the Secret.' },
    { topic: 'Framing and preparation', cat: 'concept', text: 'Which of these does the operator check about capacity before it creates the plan?', options: ['Free capacity in the target cluster\'s pools', 'Free capacity on the source for the snapshots the copy pins', 'The target\'s node count against the source\'s', 'Huge pages on the target workers', 'Whether the target\'s StorageClass exists'], correct: [], explain: 'The operator checks none of them. The control plane plans the placement, may refuse the plan, and reports the risks it did not refuse for in `status.warnings`.' },
    { topic: 'Framing and preparation', cat: 'field', text: 'What does `status.warnings` on a migration hold?', options: ['The risks the control plane found when it planned and did not refuse the plan for', 'The reasons admission gave', 'The claims the scope skipped', 'The slot reconciler\'s events', 'The volumes that failed'], correct: [0], explain: 'Warnings come with the plan, the dry run\'s included, for example too little free capacity on the source for the snapshots the copy pins. Skipped claims and failures are in `status.results`.' },
    { topic: 'Admission and dry run', cat: 'field', text: 'Which of these does the ReplicationOps admission webhook refuse?', options: ['`scope: cluster` with `action: failover`', '`scope: cluster` without `policyRef`', '`policyRef` with `scope: policy`', '`dryRun: true` with `action: trigger`', '`deleteSource: true` together with `dryRun: true`'], correct: [0, 1, 2, 3], explain: 'Scope `cluster` belongs to migration and needs a policy, `policyRef` belongs to scope `cluster`, and `dryRun` belongs to migration. Nothing refuses `deleteSource` on a dry run, which changes nothing anyway.' },
    { topic: 'Admission and dry run', cat: 'action', text: 'A cluster-scoped migration names `policyRef: dr-policy`, a policy in `failover` mode. What happens?', options: ['Admission refuses it, naming the mode it needs', 'It runs and enrolls the claims under dr-policy', 'The operator switches the policy to `migration` mode', 'It waits for the policy lock', 'It runs as a failover'], correct: [0], explain: 'The webhook reads the policy and refuses any mode but `migration`. The reconciler repeats the check and fails such an operation if one gets past admission.' },
    { topic: 'Admission and dry run', cat: 'concept', text: 'In which namespace does the cluster-scoped ReplicationOps belong?', options: ['The policy\'s, which is also where the StorageCluster named in `ref` is looked up', 'The namespace of the claims it moves', 'Any namespace, since the scope covers all of them', '`kube-system`', 'The target cluster\'s namespace on another Kubernetes cluster'], correct: [0], explain: 'The reconciler looks the policy up in the operation\'s namespace and resolves `ref` there. The claims can be anywhere, which the scope handles by listing every namespace.' },
    { topic: 'Admission and dry run', cat: 'action', text: 'Which of these does a dry run do?', options: ['Sends one plan request with `dry_run` set', 'Leaves the forecast and the per-volume plan in its status', 'Annotates the claims that are not replicated yet', 'Ends `Succeeded`, with the event `MigrationForecast`', 'Records a `planID` for the real run to reuse'], correct: [0, 1, 3], explain: 'A dry run resolves the scope, enrolls nothing, and publishes what the control plane would do. The control plane returns no plan id for it, and the real run creates its own plan.' },
    { topic: 'Admission and dry run', cat: 'concept', text: 'A dry run fails with a message that the control plane cannot forecast a migration. What is true?', options: ['The plan endpoint answered 404', 'A real run on the same control plane falls back to one commit per volume', 'The real run on that control plane fails the same way', 'The policy is in the wrong mode', 'The dry run succeeds when repeated with `deleteSource`'], correct: [0, 1], explain: 'A 404 on the plan request means the control plane predates migration plans. Nothing can forecast there, but the real run still commits each volume and follows the slots.' },
    { topic: 'Admission and dry run', cat: 'action', text: 'The dry run looks right. How is the real move started?', options: ['A second ReplicationOps without `dryRun`', 'Editing the dry run to remove `dryRun`', 'Setting `spec.approved: true` on the dry run', 'Annotating the dry run to resume it', 'Deleting the dry run, which turns its forecast into a plan'], correct: [0], explain: '`dryRun` cannot be taken off once set, and a finished operation is not run again. The real run is a new object with the same scope, ref, and policy.' },
    { topic: 'Scope resolution', cat: 'concept', text: 'Which claims does a cluster migration\'s scope resolution consider?', options: ['Bound claims in every namespace', 'Claims whose volume lives on the source now, located through the control plane\'s replication relationships', 'Claims still `Pending`', 'Claims being deleted', 'Only the claims of the operation\'s namespace'], correct: [0, 1], explain: 'Workloads keep their claims in their own namespaces, so every namespace is listed. The PV\'s handle is fixed at provisioning, so the current location is found by following relationships. Unbound and deleting claims are left out.' },
    { topic: 'Scope resolution', cat: 'action', text: 'A claim on gen1 has a live slot following `simplyblock/dr-policy`. What does the cluster migration do with it?', options: ['Reports it `skipped`, naming the policy it follows', 'Re-annotates it with `gen1-move`', 'Deletes the DR slot and enrolls the claim', 'Fails the operation', 'Moves it under the DR policy'], correct: [0], explain: 'A claim follows one policy at a time, and the operation never takes a claim from another policy. The claim stays on the source.' },
    { topic: 'Scope resolution', cat: 'field', text: 'How does the operation enroll a claim in namespace `shop` under the policy `gen1-move` of namespace `simplyblock`?', options: ['It sets `storage.simplyblock.io/replication-policy: simplyblock/gen1-move` on the claim', 'It writes a ReplicationSlot itself', 'It annotates the claim\'s StorageClass', 'It labels the PersistentVolume', 'It adds an owner reference to the policy'], correct: [0], explain: 'The operation only annotates. The claim watcher creates the slot beside the claim, named `gen1-move-<claim>`, as for a claim a person annotated.' },
    { topic: 'Scope resolution', cat: 'concept', text: 'Which of these claims appear in `status.results` as `skipped`?', options: ['A claim with no slot that its StorageClass\'s annotation assigns to another policy', 'A claim whose slot under another policy is live', 'A claim whose volume started on gen1 and has since moved to another cluster', 'A claim whose slot under `gen1-move` is `failed_over`', 'A claim on an unrelated cluster'], correct: [0, 1, 2, 3], explain: 'The first three are left alone by scope resolution, and a failed-over slot is skipped with the advice to fail it back first. A claim that never lived on the source is not listed at all.' },
    { topic: 'Scope resolution', cat: 'concept', text: 'A claim\'s slot under another policy was created but never attached, so its status is empty. What does the cluster migration do?', options: ['Enrolls the claim under the migration policy, and the watcher replaces that slot', 'Skips it as following the other policy', 'Fails the operation', 'Waits in `Enrolling` until the slot attaches', 'Deletes the claim'], correct: [0], explain: 'A slot that never attached does not hold the claim. The operation re-points the annotation, on which the watcher replaces the slot. The code comment cites a failed round whose leftover annotations made the next round skip every volume.' },
    { topic: 'Scope resolution', cat: 'field', text: 'Which subphases can a cluster migration pass through on a control plane that has migration plans?', options: ['`Enrolling`', '`AwaitingReplication`', '`CreatingPlan`', '`TrackingPlan`', '`AwaitingCutover`'], correct: [0, 1, 2, 3], explain: '`AwaitingCutover` belongs to the fallback for a control plane without migration plans, where each volume is committed on its own.' },
    { topic: 'Plan and cutover', cat: 'concept', text: 'Why is `subphase: CreatingPlan` written before the request that creates the plan?', options: ['So a pass that dies after the request looks the plan up by name instead of creating a second one', 'Because the control plane reads the subphase', 'To take the policy lock', 'To enroll the claims', 'To stop the slots from polling'], correct: [0], explain: 'A pass that finds `CreatingPlan` and no `planID` lists the plans and adopts the one carrying its name. That is how a restarted operator avoids a second plan.' },
    { topic: 'Plan and cutover', cat: 'field', text: 'Which name does the operator give the plan?', options: ['`replicationops/<namespace>/<name>/<uid>`', 'The operation\'s name alone', 'The policy\'s name', '`<source>-to-<target>`', 'None, the control plane names it'], correct: [0], explain: 'The UID makes the name unique to this operation, so a later operation of the same name never adopts an older plan.' },
    { topic: 'Plan and cutover', cat: 'action', text: 'What does a slot do when the backend reports `cutover_pending` for its volume?', options: ['Reads the target paths from the volume\'s connect endpoint', 'Runs a preconnect Job on the node of a running pod that mounts the claim', 'Sends `cutover-proceed` once the Job has succeeded', 'Switches the ANA states itself', 'Restarts the consuming pod'], correct: [0, 1, 2], explain: 'The slot prepares the consumer and releases the cutover. The control plane switches ANA, and the pod is neither restarted nor remounted.' },
    { topic: 'Plan and cutover', cat: 'concept', text: 'How are the cutovers of a cluster migration ordered?', options: ['One at a time per source volume store, as the plan runs them', 'All at once, under one freeze', 'One at a time across the whole cluster, by claim name', 'Namespace by namespace', 'By the operator, in the order the claims were enrolled'], correct: [0], explain: 'The control plane\'s plan queues every cutover and runs one per source volume store at a time, so each freeze stays short. The operator does not order them.' },
    { topic: 'Plan and cutover', cat: 'action', text: 'A preconnect Job fails. What does the slot reconciler do?', options: ['Deletes the Job and creates it again, without signaling proceed', 'Emits the warning `PreconnectFailed`', 'Signals proceed anyway', 'Fails the ReplicationOps', 'Moves the slot to `error`'], correct: [0, 1], explain: 'Proceeding would take the source paths from a consumer that has no target path. The control plane\'s own timeout still bounds how long the cutover waits.' },
    { topic: 'Plan and cutover', cat: 'concept', text: 'The operator is down while a cutover is pending. What happens to that cutover?', options: ['The control plane proceeds alone after `REPL_CUTOVER_PROCEED_TIMEOUT_SEC`', 'It waits until the operator returns', 'The plan is canceled', 'The volume fails over to the target', 'The source volume is deleted without a cutover'], correct: [0], explain: 'The timeout is a control-plane safety value. The repository quotes it as 120 seconds in the supercluster design and about 30 in a code comment, so the exact figure is the control plane\'s.' },
    { topic: 'Outcome and convergence', cat: 'concept', text: 'Which statements about `status.forecast.converging: false` are true?', options: ['Clients write some source volume store at least about as fast as it replicates, so no cutover can start there', 'The operator throttles the writers', 'The operator emits one `NotConverging` warning when the verdict changes', 'The forecast\'s `etaSeconds` and `completesAt` are left unset', 'The operation fails'], correct: [0, 2, 3], explain: 'The operator reports the verdict and changes nothing. The plan keeps running, and lowering the writes to the stores in `notConvergingLVS` is up to the reader.' },
    { topic: 'Outcome and convergence', cat: 'field', text: 'Which plan statuses end the operation `Failed`?', options: ['`partial`', '`failed`', '`canceled`', '`done`', '`running`'], correct: [0, 1, 2], explain: 'Those three end it `Failed` with the results kept. `done` ends it `Succeeded` when no result failed, and `running` keeps it tracking.' },
    { topic: 'Outcome and convergence', cat: 'concept', text: 'A cluster migration ends `Succeeded`. Which statements are true?', options: ['No result in `status.results` is `failed`', 'Claims skipped before the plan may still be on the source', 'The source StorageCluster has been deleted', 'Claims bound on the source after the plan was created were moved as well', 'The policy lock has been released'], correct: [0, 1, 4], explain: 'Success counts failures only. Skipped claims and claims that arrived after the plan stay on the source, and the operator never deletes the source cluster.' },
    { topic: 'Outcome and convergence', cat: 'field', text: 'Where does a running migration report its progress?', options: ['`status.results[].state`', '`status.results[].bytesRemaining`', '`status.results[].etaSeconds`', '`status.results[].targetNodeID`', '`status.forecast.etaSeconds`, also the `ETA` column of `kubectl get -o wide`'], correct: [0, 1, 2, 3, 4], explain: 'Every poll mirrors the plan\'s per-volume values into the results and its forecast into `status.forecast`. The `ETA` and `Converging` columns are shown at `-o wide`.' },
    { topic: 'Outcome and convergence', cat: 'action', text: 'A real run meets a control plane without migration plans. Which statements are true?', options: ['Each volume is committed with its own request', 'The operator puts each slot into `cutover_pending`', 'The subphase is `AwaitingCutover`', 'The event `MigrationCommitQueued` is emitted', 'The status carries no forecast'], correct: [0, 1, 2, 3, 4], explain: 'That is the older commit path. It follows the slots until each is `cutover_done` or in error, and it never writes the relocated-to mark.' },
    { topic: 'Afterward and limits', cat: 'concept', text: 'What does `deleteSource: true` change on the plan path?', options: ['The control plane deletes each source volume after its cutover', 'The operator marks each migrated PV with `storage.simplyblock.io/relocated-to`', 'The source StorageCluster is deleted at the end', 'The PV\'s `volumeHandle` is rewritten to the target', 'The claims are re-created on the target'], correct: [0, 1], explain: 'The handle is immutable, so the mark is how the CSI driver finds the target volume and deletes it with the claim later. The source cluster stays.' },
    { topic: 'Afterward and limits', cat: 'concept', text: 'Which of these remain after a successful cluster migration?', options: ['The source StorageCluster', 'The source cluster\'s StorageClass, which still provisions on the source', 'The ReplicationSlots, in `cutover_done`', 'The policy annotation on the enrolled claims', 'The PVs\' original volume handles'], correct: [0, 1, 2, 3, 4], explain: 'The operation moves volumes and cleans up nothing else. Retiring the source, its class, and the slots is left to a person.' },
    { topic: 'Afterward and limits', cat: 'concept', text: 'What stops the source StorageCluster from being deleted before every migrated PV carries its relocated-to mark?', options: ['An admission rule that counts unmarked PVs', 'A finalizer the migration puts on the source cluster', 'A check in the StorageCluster reconciler\'s teardown', 'The CSI driver keeping the cluster\'s secret entry', 'The ReplicationOps holding a lock on the cluster'], correct: [], explain: 'None of these exists. The StorageCluster webhook refuses a deletion only while a supercluster member names the cluster, and teardown deletes the backend cluster and the CSI entry. The order is the reader\'s to keep.' },
    { topic: 'Afterward and limits', cat: 'action', text: 'A running cluster-scoped ReplicationOps is deleted. What happens?', options: ['The policy lock is released', 'The plan keeps running in the control plane', 'The plan is canceled', 'The volumes already cut over are moved back', 'The enrollment annotations are removed'], correct: [0, 1], explain: 'Deletion releases `activeOpsRef` and nothing else. There is no `spec.abort`, and mapping it to the control plane\'s cancel call is an open question in the design.' },
    { topic: 'Afterward and limits', cat: 'concept', text: 'Which of these are limits of the cluster migration as implemented?', options: ['Preconnect connects the target\'s paths on one consumer node per volume', 'Nothing refuses encrypted volumes, and moving them is unconfirmed', 'No automated test runs it against two live clusters', 'A supercluster member drain, `RemoveMember`, is planned and not built', 'The plan refuses claims from more than one namespace'], correct: [0, 1, 2, 3], explain: 'Scope `cluster` is the one scope that collects claims from every namespace. The other four are limits the chapter states.' },
  ],

  tasks: [
    {
      title: 'The pair and the migration policy',
      scenario: 'Write the pair from the StorageCluster gen1 to the StorageCluster gen2, both in `simplyblock`, and the policy a cluster-scoped migration can enroll claims under, keeping as few snapshots as the CRD allows.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha1\nkind: ReplicationPair\nmetadata:\n  name: gen1-to-gen2\n  namespace: simplyblock\nspec:\n  sourceCluster: {{source}}\n  targetCluster: {{target}}\n---\napiVersion: storage.simplyblock.io/v1alpha1\nkind: ReplicationPolicy\nmetadata:\n  name: gen1-move\n  namespace: simplyblock\nspec:\n  pairRef: {{pair}}\n  mode: {{mode}}\n  interval: 5m\n  snapshotRetention: {{retention}}\n',
      blanks: {
        source: { accept: ['gen1'], hint: 'the cluster to empty' },
        target: { accept: ['gen2'], hint: 'the cluster that receives the volumes, by name' },
        pair: { accept: ['gen1-to-gen2'], hint: 'the pair above' },
        mode: { accept: ['migration'], hint: 'the only mode a cluster-scoped migration accepts' },
        retention: { accept: ['2'], hint: 'the CRD\'s minimum' },
      },
      explain: 'The pair names both clusters by their StorageCluster names in the namespace. The policy must be in `migration` mode, and `snapshotRetention` may not go below 2.',
    },
    {
      title: 'The forecast',
      scenario: 'Write the operation that forecasts moving every claim of gen1 under `gen1-move` and changes nothing.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha1\nkind: {{kind}}\nmetadata:\n  name: forecast-gen1-move\n  namespace: simplyblock\nspec:\n  action: {{action}}\n  scope: {{scope}}\n  ref: {{ref}}\n  policyRef: gen1-move\n  dryRun: {{dry}}\n',
      blanks: {
        kind: { accept: ['ReplicationOps'], hint: 'the one-shot replication operation, short name `replops`' },
        action: { accept: ['migration'], hint: 'the only action a dry run is allowed on' },
        scope: { accept: ['cluster'], hint: 'every bound claim on the source, in every namespace' },
        ref: { accept: ['gen1', '/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/'], hint: 'the source StorageCluster, by name or UUID' },
        dry: { accept: ['true'], hint: 'plan and forecast only' },
      },
      explain: 'Scope `cluster` takes `action: migration`, the source cluster in `ref`, and a migration policy in `policyRef`. `dryRun: true` publishes the forecast and changes nothing.',
    },
    {
      title: 'The move, deleting the source volumes',
      scenario: 'Write the real move of every claim on gen1, so that each source volume is deleted after its cutover and every moved PersistentVolume is marked with where its data lives now.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha1\nkind: ReplicationOps\nmetadata:\n  name: move-gen1\n  namespace: {{ns}}\nspec:\n  action: migration\n  scope: {{scope}}\n  ref: gen1\n  policyRef: {{policy}}\n  deleteSource: {{delete}}\n',
      blanks: {
        ns: { accept: ['simplyblock'], hint: 'the policy\'s namespace' },
        scope: { accept: ['cluster'], hint: 'the scope that crosses namespaces' },
        policy: { accept: ['gen1-move'], hint: 'the migration policy on the pair from gen1' },
        delete: { accept: ['true'], hint: 'the field that leads to the relocated-to mark' },
      },
      explain: 'The operation lives with the policy. With `deleteSource: true` the control plane deletes each source volume after its cutover, and the operator writes `storage.simplyblock.io/relocated-to` on each moved PV.',
    },
  ],
});
