// Chapter 13: moving a volume to another storage node of the same cluster while
// it stays in use. The commissioned framing was an instant move, and the first
// section says why the chapter teaches a live copy and a short cutover instead:
// that is what PersistentVolumeOps Migrate does, and the move without a copy
// exists only on the Control Center prototype's mock backend.
//
// The rest follows the operation as the operator runs it: the object, its
// admission, the three steps and their budgets, the two locks, pins, the roles
// of the control plane, the operator, and the CSI driver, the three controllers
// that raise moves without a person, and the realignment that follows them.
// Moves between clusters are the next chapter, and drains are node-removal's.
TRAINING.register({
  id: 'volume-migration-nodes',
  title: 'Live migration of volumes between nodes',
  summary: 'Moving a volume and its NVMe-oF subsystem to another storage node of the same cluster while its pod keeps running: a PersistentVolumeOps Migrate, the live copy, the cutover under one short ANA freeze, and the locks, pins, and rebalancer around it.',
  goal: 'the reader can write a PersistentVolumeOps that moves a volume to another node, follow it through its steps and events, stop it while it can still be stopped, explain why it waits or fails, and configure the rebalancer and the realignment that raise and follow such moves.',

  sections: [
    {
      heading: 'What moves, and what the cutover costs',
      body: [
        'The commissioned title of this chapter was "instant migration of volumes between nodes." The operator does something else, and the chapter teaches the code. A `PersistentVolumeOps` with `action: Migrate` copies the volume to the target storage node while the volume stays in use, and then cuts over. The copy takes as long as the data takes: the step that waits for it is given 30 minutes plus 10 minutes per volume moved (`operator/internal/controllers/volume/graphs.go`).',
        'The cutover is the short part. The operator\'s comments describe the control plane driving every path of the subsystem inaccessible for about two seconds while it moves the volume, after which the target serves it (`operator/internal/volumemigration/release.go`). The test plan requires exactly one such freeze per cutover, because more than one has lost writes in the field (`operator/docs/tests/test-plan-persistentvolumeops.md`, rows M-02, E-05, and E-06). Neither the duration nor the count is measured by any automated test in the repository.',
        'A move that hands only the primary role to another node and copies nothing exists in one place: the Control Center prototype. Its "Migrate instantly" dialog posts to `/lvols/{id}/migrate`, which its mock backend answers (`control-center/mock-api.jsx`) and which no operator or atlas-lib code calls. The prototype\'s volume that follows its pod to another node has no operator code behind it either.',
        { table: { head: ['What', 'Moves with the named volume', 'Why'], rows: [
          ['Every other volume of its NVMe-oF subsystem', 'yes', 'the control plane migrates a subsystem as a whole. The StorageClass parameter `max_namespace_per_subsys` decides how many volumes share one, and the CSI driver\'s default is 1'],
          ['Its snapshots', 'yes, as the control plane reports it', 'a single-volume migration counts its progress in snapshots copied (`SnapsMigrated` of `SnapsTotal` in atlas-lib\'s client)'],
          ['The PersistentVolume and its handle', 'no', 'the handle is `<cluster>:<pool>:<volume>` and names no node, so nothing in Kubernetes changes'],
          ['The consuming pod', 'no', 'it keeps running on its worker, and its I/O changes path underneath it'],
        ] } },
        'The move stays inside one storage cluster. A move to another cluster, or to another member of a supercluster, is [the next chapter](chapter.html?c=volume-migration-clusters). A drain moves volumes through this same operation, and [the node removal chapter](chapter.html?c=node-removal) covers it from the node\'s side.',
        { callout: 'What the control plane does inside the copy, and how the host kernel holds I/O while no path is accessible, are not visible in this repository. The chapter says what the operator sends, what it reads back, and what its comments and its test plan say about the rest.', kind: 'note' },
      ],
    },
    {
      heading: 'Writing a migration',
      body: [
        'A person writes one object. It is cluster-scoped, short name `pvops`, because a PersistentVolume is. It names the volume rather than the claim, because a claim can be deleted while its volume is retained, and it names the target as a StorageNode object rather than as a backend UUID. The cluster, the pool, and the volume UUIDs are read from the volume\'s CSI handle.',
        { steps: [
          'Find the volume behind the claim: `kubectl get pvc pg-data -n payments -o jsonpath=\'{.spec.volumeName}\'`.',
          'Find the target among the cluster\'s nodes: `kubectl get sn -n simplyblock`. A node created from a deployment config is named `<cluster>-<worker>-<slot>`.',
          'Write the PersistentVolumeOps with `action: Migrate` and `spec.migrate.targetNodeRef`, and apply it.',
          'Follow it with `kubectl get pvops -w`, whose columns show the phase and the step.',
        ] },
        'The action and its block have to agree, which a CEL rule on the type checks: `Migrate` requires `spec.migrate`, and `Relocate` and `JoinConsistencyGroup` refuse it. The volume, the action, and the target are immutable. `spec.abort` is the one field that changes afterward. `spec.creatorRef` is set by the controllers that raise moves and is absent on a hand-written operation.',
        'A validating webhook refuses at create what no reconcile could make work:',
        { list: [
          'no PersistentVolume of that name, or one without a CSI source',
          'a volume of another CSI driver than the ones the SimplyblockDriver objects name',
          'a pNFS volume, whose metadata server and clients hold paths the migration does not account for',
          'a volume handle that is not `<cluster>:<pool>:<volume>`',
          'a target StorageNode that does not exist',
          'a target that is the other site\'s node of a stretched cluster, refused as `RemoteNode`',
          'a target node of another storage cluster than the volume\'s, when that cluster has a backend UUID',
        ] },
        'Conditions that are true now and may be false in a minute are admitted and handled by the controller. A target node that is not `Online`, or has no `status.uuid` yet, makes the operation wait and retry every 15 seconds. It stays without a phase, its message says what it waits for, a node that is not `Online` raises a `TargetNodeNotReady` warning on every retry, and no step deadline runs yet, because no step has started.',
      ],
      examples: [
        { title: 'A migration to another node', file: 'examples/volume-migration-nodes/01-persistentvolumeops-migrate.yaml', notes: [
          { at: 'kind: PersistentVolumeOps', text: 'Cluster-scoped, so `metadata` carries no namespace. Its events land in `default` for that reason.' },
          { at: 'persistentVolumeName:', text: 'The PersistentVolume, not the claim. Immutable.' },
          { at: 'action: Migrate', text: 'Another storage node of the same cluster. `Relocate` and `JoinConsistencyGroup` are the kind\'s other two actions.' },
          { at: 'targetNodeRef:', text: 'Immutable as a pair, so a target cannot be half-changed into a name in one cluster and a namespace in another.' },
          { at: 'namespace: simplyblock', text: 'Required, because a cluster-scoped object has no namespace for a bare name to mean, and two clusters in two namespaces may each hold a node of the same name.' },
          { at: 'name: prod-worker-4-0', text: 'The StorageNode object. The controller resolves its backend UUID from `status.uuid`.' },
        ] },
      ],
    },
    {
      heading: 'Phases, steps, and the cutover',
      body: [
        'The phase is the operation\'s own progress: `Pending`, `Running`, then one of `Succeeded`, `Failed`, and `Aborted`. The step, `status.step.state`, is where a running migration is. A succeeded operation reads `Completed`, and a failed or aborted one keeps the step it stopped in.',
        { table: { head: ['Step', 'What happens', 'Budget', 'Abort'], rows: [
          ['`Validating`', 'the backend migration is created, consuming pods are waited for, and one Job per consuming node checks the target\'s paths', '15 minutes', 'honored'],
          ['`Migrating`', 'the operator continues the migration once, and the control plane copies and cuts over', '30 minutes plus 10 per volume of the subsystem', 'honored'],
          ['`Verifying`', 'the validation Jobs are deleted, and controllers that carry no namespace are cleared on the nodes that took part', '10 minutes', 'not honored, and a delete is refused'],
        ] } },
        'The ordering is what makes the cutover safe for every host that consumes the subsystem:',
        { steps: [
          '`Validating` asks the control plane to create the migration. The control plane publishes the target\'s paths for the same subsystem and reports how many volumes it holds, which the operator records as `status.migration.memberCount`.',
          'Every pod that mounts a claim of the subsystem has to be `Running`. A pod that exists and has not started is waited for with `WaitingForConsumer`, because a pod that stages against the source during the copy is stranded at cutover.',
          'One Job per consuming node, in the StorageCluster\'s namespace with the rebalancer image, connects the target\'s paths with a controller-loss timeout of 60 seconds, waits up to 15 seconds for the namespaces to appear, and requires every path to be present, live, and not yet accessible. The Job has 180 seconds and no retry, and the first failure fails the operation.',
          'Before leaving `Validating`, the consumers and the pins are read again, because a volume can join the subsystem and a pod can move while the checks run.',
          '`Migrating` writes `status.migration.continuedAt` and then calls continue, once. The order makes the call at-most-once: a continue retried after a timeout has copied nothing while the source was unfrozen, and lost writes. The operator then reads the migration until the control plane reports it `done`, `failed`, or `canceled`.',
          'The control plane copies and cuts over: every path inaccessible for about two seconds, then the target\'s paths serve. The host\'s NVMe multipath follows the ANA states, and nothing in Kubernetes moves.',
          '`Verifying` deletes the Jobs and clears dead controllers. It does not release the target\'s paths, because they are now the volume\'s data path. The operation ends `Succeeded` with `OperationSucceeded`.',
        ] },
        'An abort is honored in `Validating` and `Migrating`: the operator cancels the backend migration, runs a release Job on every node that took part to disconnect the target paths that carry no I/O, and ends `Aborted` with `OperationAborted`. Deleting an operation in those steps runs the same unwind through its finalizer, and a cleanup that cannot finish holds the delete open with `CleanupBlocked`. A step that outlives its budget raises `StepDeadlineExceeded`, is unwound the same way, and ends `Failed`.',
        { callout: 'The abort is checked before the step reads the migration. An abort that arrives in the seconds after the control plane finished the copy, and before the operator read it, ends the operation `Aborted` while the volume has moved. This follows from the code and has no test. Check where the volume is before acting on such an `Aborted`.', kind: 'warn' },
        { code: 'kubectl get pvops pg-data-to-worker-4 -o jsonpath=\'{.status.phase} {.status.step.state} {.status.message}\'\nkubectl patch pvops pg-data-to-worker-4 --type merge -p \'{"spec":{"abort":true}}\'', title: 'Reading and stopping an operation' },
      ],
      examples: [
        { title: 'Stopping a migration', file: 'examples/volume-migration-nodes/02-persistentvolumeops-abort.yaml', notes: [
          { at: '  abort: true', text: 'The only mutable field. Read at the next pass, and honored only from `Validating` and `Migrating`.' },
          { at: 'action: Migrate', text: 'Unchanged. A spec that changed anything else would be refused as immutable.' },
        ] },
        { title: 'A target that already holds the volume', file: 'examples/volume-migration-nodes/03-persistentvolumeops-target-is-source.yaml', notes: [
          { at: 'name: pg-data-to-worker-2', text: 'Ends `Succeeded`, step `Completed`, with the event `TargetNodeIsSource`. Nothing is copied.' },
          { at: 'name: prod-worker-2-0', text: 'The node the whole subsystem is on. The check reads every member, so a subsystem split between two nodes is waited on as a cutover in progress.' },
        ] },
      ],
    },
    {
      heading: 'Two locks, pins, and what waits',
      body: [
        'Each pass checks four things in order before it advances a step: the target node is resolvable and `Online`, no pin stands in the way, the cluster\'s migration slot is free, and the subsystem\'s lock can be taken.',
        { table: { head: ['Lock', 'Where it is kept', 'Why'], rows: [
          ['the subsystem lock', 'annotation `storage.simplyblock.io/active-ops` on every PersistentVolume of the subsystem, naming the operation', 'the control plane moves the subsystem as a whole, so two operations naming two siblings would each ask for the same move. The volumes are taken in name order, and either all of them or none'],
          ['the migration slot', 'annotation `storage.simplyblock.io/active-volume-migration` on the StorageCluster', 'the control plane runs one data migration per cluster and refuses the rest'],
        ] } },
        'An operation that cannot take either holds nothing and waits in `Pending`. Its `status.deferredSince` records since when, its message names the operation in the way, and an `OperationQueued` event says which lock it waits for. A release wakes the waiting operations through a watch, with a 15-second retry behind it. Both locks are released on every terminal phase and on deletion, and a lock whose holder is terminal or gone is broken by the next operation that finds it.',
        { code: 'kubectl get pv -o custom-columns=NAME:.metadata.name,HELD_BY:.metadata.annotations.storage\\.simplyblock\\.io/active-ops\nkubectl get stc prod -n simplyblock -o jsonpath=\'{.metadata.annotations.storage\\.simplyblock\\.io/active-volume-migration}\'', title: 'Who holds what' },
        'A pin is the claim annotation `storage.simplyblock.io/selected-storage-node`, whose value is the backend UUID of a storage node. A validating webhook refuses a value that names no storage node the control plane knows. At creation the CSI controller places the volume on that node. After that, only the pin controller moves a pinned volume: a changed value raises a PersistentVolumeOps to the new node, labeled `storage.simplyblock.io/pinned-volume-pv`, and a removed annotation leaves the volume where it is.',
        'Every other move of a pinned volume ends `Failed` with `VolumePinned`, and so does a move of any volume whose subsystem holds a sibling pinned to another node than the target. The pins are read while the operation is `Pending` and again at the end of `Validating`. A pin-driven move whose pin was removed or changed before it started ends `Aborted`, because the request was withdrawn. A drain is held while its node carries a pinned volume, and the rebalancer leaves pinned volumes out.',
      ],
      examples: [
        { title: 'A pinned claim', file: 'examples/volume-migration-nodes/04-pvc-pinned.yaml', notes: [
          { at: 'selected-storage-node:', text: 'The pin. The value is `status.uuid` of a StorageNode, not its object name.' },
          { at: 'storageClassName: simplyblock-prod', text: 'Nothing in the class pins. Placement at creation and every later move read the claim\'s annotation.' },
        ] },
      ],
    },
    {
      heading: 'Who does what during the move',
      body: [
        { table: { head: ['Component', 'During a Migrate'], rows: [
          ['control plane', 'creates the migration and publishes the target\'s paths, copies the data, and drives the ANA states at cutover. It refuses a create it will never accept, and answers one it cannot accept yet, during a rebalance, a node\'s data migration, or another migration of the subsystem'],
          ['operator', 'resolves the volume and the target, takes the slot and the lock, runs the validation Jobs, continues the migration once, cleans up, and keeps the finalizer'],
          ['validation and release Jobs', 'connect and check the target\'s paths on each consuming node before the copy, or disconnect them on an abort, a failure, or a delete'],
          ['CSI driver', 'nothing that the move asks of it. The operation makes no CSI call'],
          ['host kernel', 'its NVMe multipath sends I/O to whichever path the ANA states make accessible'],
        ] } },
        'A refusal the control plane will never change, an answer of 400 with its own reason, fails the operation at once with that reason in `status.message`, so whatever raised it can choose another target. A refusal that clears by itself is retried inside `Validating`, against that step\'s 15 minutes.',
        'The target\'s paths are connected with a controller-loss timeout of 60 seconds rather than the hour the control plane answers with. They become the volume\'s data path at cutover, and 60 is the CSI driver\'s `DefaultCtrlLossTmo` (`csi-driver/internal/initiator/initiator.go`), so every path of the volume ends on one timeout. A pod restarted after the move stages through the driver\'s usual refresh, which asks the control plane for the volume\'s current paths.',
        'The events land on the PersistentVolumeOps. An event takes its namespace from the object it is about, and for a cluster-scoped object that is `default`, so a namespaced `kubectl get events -n simplyblock` finds none of them.',
        { code: 'kubectl describe pvops pg-data-to-worker-4\nkubectl get events -n default --field-selector involvedObject.kind=PersistentVolumeOps', title: 'Finding the events' },
        { table: { head: ['Reason', 'Type', 'Meaning'], rows: [
          ['`OperationQueued`', 'Normal', 'waiting for the slot or for a volume of the subsystem'],
          ['`OperationStarted`', 'Normal', 'both locks taken, `Validating` entered'],
          ['`TargetNodeNotReady`', 'Warning', 'the target is not `Online`, retried'],
          ['`VolumePinned`', 'Warning', 'a pin stands in the way, the operation fails'],
          ['`MigrationCreated`, `MigrationStarted`', 'Normal', 'the backend migration exists, and the copy was continued'],
          ['`WaitingForConsumer`', 'Normal', 'a consuming pod has not started'],
          ['`ValidationStarted`, `ValidationSkipped`', 'Normal', 'Jobs started, or no node consumes the subsystem'],
          ['`TargetNodeIsSource`', 'Normal', 'the subsystem is already there, `Succeeded` with nothing copied'],
          ['`ReleasingPaths`, `CleanupBlocked`', 'Normal, Warning', 'the unwind disconnects paths, or a delete is held until it can'],
          ['`StepDeadlineExceeded`', 'Warning', 'a step outlived its budget, the operation fails'],
          ['`OperationSucceeded`, `OperationFailed`, `OperationAborted`', 'Normal, Warning, Normal', 'the terminal phase'],
        ] } },
        'Five metrics are exported: `simplyblock_persistentvolume_operations_total`, `simplyblock_persistentvolume_operation_duration_seconds`, `simplyblock_persistentvolume_operation_step_duration_seconds`, `simplyblock_persistentvolume_operation_step_deadline_exceeded_total`, and `simplyblock_persistentvolume_operation_queued_seconds`.',
      ],
    },
    {
      heading: 'Moves nobody writes by hand, and the realignment after them',
      body: [
        'Three controllers raise the same PersistentVolumeOps Migrate through one mover, and each is held to the same locks, pins, and steps:',
        { list: [
          '**The auto-rebalancer**, when the StorageCluster sets `enableVolumeAutoPlacement: true`. Its operations are named `rebalance-<volume UUID>`, carry `spec.creatorRef` naming the StorageCluster and the label `storage.simplyblock.io/rebalancer: "true"`, and are deleted by the rebalancer once terminal.',
          '**A drain**, a StorageNodeOps with action `Remove`, which raises one move per subsystem on the node, deletes each move once it succeeds, and replaces a failed move with one to another target. [The node removal chapter](chapter.html?c=node-removal) covers it.',
          '**The pin controller**, when a claim\'s pin changes.',
        ] },
        'The Helm value `volumeMigration.legacy: true` makes the same three raise the older namespaced `VolumeMigration` instead. It exists for migrations that started on that kind to finish after an upgrade, and the two kinds do not convert into each other.',
        'The rebalancer runs a cycle every `evaluationInterval`, 60 seconds by default. It skips the cycle while any storage node is offline, in restart, or unreachable, and while its own moves from an earlier cycle are not finished. A node whose write latency deviates from its baseline by more than `imbalanceThreshold` percent, 80 by default, is a source. The target is the coolest node at least `minHotColdDifferencePct` points cooler, 20 by default. Volumes are ranked by `iopsWeight` times IOPS plus `throughputWeight` times MB/s, and at most `maxVolumeMigrationsPerCycle`, 10 by default, are moved. A moved volume is left alone for `defaultCoolDownSeconds`, 600 by default, a window held in memory that an operator restart forgets. `disableMigration: true` is the dry run: everything runs and the candidates are logged, and nothing is created.',
        'The rebalancer reads latency through the Prometheus at `prometheusURL`. When the cluster has no `volumeAutoPlacement` block at all, that is the chart\'s `http://simplyblock-prometheus:9090`. A block that is present and leaves `prometheusURL` out gets no default, and every cycle is skipped with an error in the operator\'s log and no event. `storageNodeCandidateCount` and `metricsBackend` are accepted by the API and read by no code today.',
        'After a move lands, the control plane\'s internal structures still reflect the placement before it. A **data realignment** restores the fault-tolerance and node-affinity guarantees the move invalidated. Every `Succeeded` Migrate increments `status.volumeMoveGeneration` on the StorageCluster. A realignment is requested once `minMoves` moves are outstanding, 1 by default, and at most once per `interval`, 10 minutes by default, both under `spec.volumeMigrationSettings.dataRealignment`. The annotation `simplyblock.io/trigger-realignment` with any value forces one at once. `spec.disableDataRealignment: true` turns realignment off. Nothing turns migration off, because drains, rebalancing, and device replacement are all done by moving volumes.',
        'A realignment delays the next migration. The control plane refuses new migrations while one runs, which the type documentation measures at tens of minutes on a loaded cluster, and the operation holding the cluster\'s slot waits for it inside `Validating`, against that step\'s 15 minutes. A realignment longer than that fails the operation with `StepDeadlineExceeded`. With `minMoves: 1` migrations and realignments alternate. Raising `minMoves` batches the realignments, and turning them off is for a cluster that migrates continuously.',
        'Before an untriggered realignment the controller waits until no volume is moving, and it reads only the legacy `VolumeMigration` kind for that. A PersistentVolumeOps that is copying does not hold the realignment off, and what the control plane does with a realignment request during a running copy is not visible in the repository.',
      ],
      examples: [
        { title: 'Rebalancing on, realignment batched', file: 'examples/volume-migration-nodes/05-storagecluster-autoplacement.yaml', notes: [
          { at: 'maxSubsystemCount: 50', text: 'One of the two sizing fields the API requires unless the cluster is a supercluster, with `vcpuCount`.' },
          { at: 'enableVolumeAutoPlacement: true', text: 'The switch. It is a field of the spec rather than of the block it governs, and unset means off.' },
          { at: 'disableMigration: false', text: 'Set to `true` for a dry run: candidates are logged and no operation is created.' },
          { at: 'imbalanceThreshold: 80', text: 'The latency deviation from baseline, in percent, above which a node is a source.' },
          { at: 'defaultCoolDownSeconds: 600', text: 'Kept in the operator\'s memory, so a restart forgets it and a volume can be chosen again sooner.' },
          { at: 'prometheusURL:', text: 'Required once the block is present. The chart\'s address is the default only when the whole block is absent.' },
          { at: 'minMoves: 5', text: 'Five moves accumulate before a realignment is requested, so a drain alternates less often between copying and realigning.' },
        ] },
      ],
    },
    {
      heading: 'What is not built, and what is not tested',
      body: [
        { list: [
          'No retention: nothing deletes a terminal PersistentVolumeOps written by hand. The design\'s `opsRetention` and `opsHistoryLimit` are not on the StorageCluster.',
          'No events on the claim. The design mirrors the operation\'s events onto the PersistentVolumeClaim, and the code emits them on the operation alone.',
          'No copy progress in status. The control plane reports snapshots copied for a single-volume migration, and PersistentVolumeOps does not surface it.',
          'Two designed metrics, the active-operation gauge and the stale-path counter, and two designed events, `ValidationFailed` and `StalePathCleaned`, are not built. A failed validation reports `OperationFailed` with the node in its message.',
          'Two rebalancer fields, `storageNodeCandidateCount` and `metricsBackend`, are read by nothing.',
          'Not tested end to end: rows E-01 to E-15 and M-01 to M-04 of the test plan, which include data integrity under load, the freeze count, stale paths, a migration during a realignment, and twenty concurrent migrations to one target. The plan says the end-to-end harness is not committed.',
        ] },
        { callout: 'The test plan notes that every field failure of this kind was invisible to status: a cutover that froze twice, a transfer that skipped its drain, and a retried transfer all reported success. A `Succeeded` phase says the steps finished, not that every write arrived.', kind: 'warn' },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The apply is refused: the volume was provisioned by another driver, or there is no PersistentVolume of that name.', cause: 'The claim\'s name was used instead of its volume\'s, or the volume is not a simplyblock volume.', fix: 'Read `spec.volumeName` from the claim and put that in `spec.persistentVolumeName`.' },
          { symptom: 'The operation has no phase, and `TargetNodeNotReady` repeats.', cause: 'The target StorageNode is not `Online`. A node with no backend UUID yet waits the same way, with the reason in `status.message` and no event.', fix: 'Wait for the node, or delete the operation and write one to another node. No step budget runs while it waits.' },
          { symptom: 'The operation sits in `Pending` with `OperationQueued`.', cause: 'Another operation holds the cluster\'s slot or a volume of the subsystem.', fix: 'Nothing, it starts when the holder ends. The message names the holder, and `status.deferredSince` says since when.' },
          { symptom: 'The operation fails at once with `VolumePinned`.', cause: 'The volume or a sibling on its subsystem is pinned to another node.', fix: 'Change the pin on the claim to the target\'s UUID, which raises the move, or remove the pin and write the operation again.' },
          { symptom: '`WaitingForConsumer` repeats, then `StepDeadlineExceeded` in `Validating`.', cause: 'A pod mounting a claim of the subsystem is not `Running`, for example unschedulable.', fix: 'Fix or delete the pod. The operation does not skip a consumer that exists.' },
          { symptom: 'The operation fails in `Validating` naming a node whose paths could not be established.', cause: 'That node could not reach the target over the published paths, or the Job could not run there within 180 seconds.', fix: 'Read the Job\'s log in the StorageCluster\'s namespace and fix the node\'s network or the rebalancer image, then write the operation again.' },
          { symptom: 'Migrations fail with `StepDeadlineExceeded` in `Validating` in the minutes after other moves finished.', cause: 'A data realignment is running, and the control plane refuses new migrations until it ends.', fix: 'Raise `dataRealignment.minMoves` to batch realignments, or set `disableDataRealignment` on a cluster that migrates continuously.' },
          { symptom: 'Deleting the operation is refused.', cause: 'It is in `Verifying`, where the volume has moved and the cleanup is what keeps the next migration possible.', fix: 'Wait for the terminal phase, then delete.' },
          { symptom: 'The rebalancer never moves anything and raises no event.', cause: 'A `volumeAutoPlacement` block without `prometheusURL` skips every cycle, or a node is offline, or `disableMigration` is true.', fix: 'Set `prometheusURL`, read the operator\'s log for the skip reason, and set `disableMigration: false` once the dry run looks right.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Scope and framing', cat: 'concept', text: 'Which of these describe what a PersistentVolumeOps `Migrate` does to the volume\'s data?', options: ['It copies the data to the target node while the volume stays in use, then cuts over', 'It hands the primary role to the target and copies nothing', 'It cuts over under one ANA freeze, which the code describes as about two seconds', 'It requires the consuming pod to be restarted on the target node', 'It moves every volume that shares the named volume\'s NVMe-oF subsystem'], correct: [0, 2, 4], explain: 'The operation is a live copy followed by a cutover under one short freeze, and the control plane migrates the whole subsystem. The move without a copy exists only on the Control Center mock, and the pod keeps running.' },
    { topic: 'Scope and framing', cat: 'concept', text: 'Where does a move that hands only the primary role to another node, copying nothing, exist in this repository?', options: ['In the Control Center prototype, answered by its mock backend', 'In the PersistentVolumeOps controller, for a subsystem of one volume', 'In the CSI node plugin, when a pod is rescheduled', 'In the auto-rebalancer, when `disableMigration` is true', 'In the drain of a StorageNodeOps `Remove`'], correct: [0], explain: 'The prototype\'s dialog posts to `/lvols/{id}/migrate`, which `control-center/mock-api.jsx` answers and no operator or atlas-lib code calls. Every operator path copies.' },
    { topic: 'Scope and framing', cat: 'concept', text: 'A claim was provisioned from a StorageClass that does not set `max_namespace_per_subsys`. How many volumes does a `Migrate` of its volume move?', options: ['Only that volume, because the CSI driver gives each volume its own subsystem by default', 'Every volume in its pool', 'Every volume on its source node', 'Every volume of the claim\'s namespace', 'Every volume of its consistency group'], correct: [0], explain: 'The control plane moves a subsystem, and the CSI driver\'s default of `max_namespace_per_subsys` is 1, so the subsystem holds the one volume.' },
    { topic: 'Scope and framing', cat: 'concept', text: 'Which of these moves is a `Migrate` as this chapter teaches it?', options: ['To another storage node of the volume\'s own cluster', 'To another member of the volume\'s supercluster', 'To the other site\'s node of a stretched cluster', 'Of a pNFS volume to another node', 'To another cluster of the same control plane'], correct: [0], explain: 'A `Migrate` stays inside one cluster. A supercluster member is `Relocate`, a cluster-to-cluster move is replication-based, both in the next chapter, and the stretched remote node and pNFS volumes are refused.' },
    { topic: 'Scope and framing', cat: 'concept', text: 'What do the repository\'s sources say about the freeze at cutover?', options: ['The operator\'s comments describe every path inaccessible for about two seconds', 'The test plan expects exactly one freeze per cutover', 'An automated test counts the freezes on every CI run', '`status.migration` reports the freeze count', 'The test plan records that more than one freeze has lost writes in the field'], correct: [0, 1, 4], explain: 'The duration is in `release.go`, and rows M-02, E-05, and E-06 of the test plan require one freeze because two lost writes. No automated test counts them, and status does not show them.' },

    { topic: 'Writing the operation', cat: 'field', text: 'Which fields of a `Migrate` operation can be changed after it is created?', options: ['`spec.abort`', '`spec.migrate.targetNodeRef`', '`spec.persistentVolumeName`', '`spec.action`', '`spec.creatorRef`, once set'], correct: [0], explain: 'The abort is the only mutable field. The volume, the action, and the target are immutable, and `creatorRef` is immutable once set.' },
    { topic: 'Writing the operation', cat: 'field', text: 'Why does `spec.migrate.targetNodeRef` carry a namespace as well as a name?', options: ['The operation is cluster-scoped, so a bare name has no namespace to mean', 'Two clusters in two namespaces may each hold a node of the same name', 'The PersistentVolume lives in that namespace', 'The control plane looks nodes up by namespace', 'The namespace selects the storage pool'], correct: [0, 1], explain: 'A cluster-scoped object referring to a namespaced one has to say where, for the same reason `pv.spec.claimRef` does. A PersistentVolume has no namespace, and the control plane uses the node\'s UUID.' },
    { topic: 'Writing the operation', cat: 'action', text: 'Which of these does the admission webhook refuse when the operation is created?', options: ['A volume provisioned by another CSI driver', 'A pNFS volume', 'A target StorageNode that does not exist', 'A target node that is offline', 'A target node of another storage cluster that has a backend UUID'], correct: [0, 1, 2, 4], explain: 'The webhook refuses what no reconcile could fix. An offline node is a fact about now, so it is admitted and the operation waits with `TargetNodeNotReady`.' },
    { topic: 'Writing the operation', cat: 'action', text: 'Which of these are admitted and then handled by the controller instead of being refused?', options: ['A target node that is not `Online`', 'A target node that already holds the volume\'s subsystem', 'A target StorageNode with no `status.uuid` yet', 'A volume handle that is not `<cluster>:<pool>:<volume>`', 'A target that is a stretched cluster\'s remote node'], correct: [0, 1, 2], explain: 'The first three can change, so they are waits or an outcome: two waits and a `Succeeded` with `TargetNodeIsSource`. A malformed handle and a remote node never change and are refused.' },
    { topic: 'Writing the operation', cat: 'concept', text: 'A migration is written for the claim `pg-data` in the namespace `payments`. Which of these are true of the operation?', options: ['It names the claim\'s bound PersistentVolume, not the claim', 'It carries no `metadata.namespace`', 'It is created in the namespace `payments`', 'It names its storage cluster in the spec', 'It names the target node by its backend UUID'], correct: [0, 1], explain: 'The kind is cluster-scoped and names the volume. The cluster comes from the volume\'s handle, and the target is a StorageNode object.' },

    { topic: 'Steps and cutover', cat: 'concept', text: 'At which steps is `spec.abort` honored?', options: ['`Validating`', '`Migrating`', '`Verifying`', '`Completed`', 'Any step, as long as the phase is `Running`'], correct: [0, 1], explain: 'The graph marks `Validating` and `Migrating` abortable. In `Verifying` the copy has finished and the volume has moved, so the abort is noted and the cleanup runs.' },
    { topic: 'Steps and cutover', cat: 'action', text: 'Which of these happen in `Validating`?', options: ['The operator creates the backend migration, and the control plane publishes the target\'s paths', 'The operator waits for every pod mounting a claim of the subsystem to be `Running`', 'One Job per consuming node connects the target\'s paths and requires them present, live, and not yet accessible', 'The data copy starts', 'The source node\'s paths are disconnected'], correct: [0, 1, 2], explain: 'Validating creates the migration and proves every consumer can reach the target. The copy starts in `Migrating`, and the operator never disconnects the source\'s paths.' },
    { topic: 'Steps and cutover', cat: 'concept', text: 'Why is `status.migration.continuedAt` written before the continue call?', options: ['It makes the continue at-most-once, so a timed-out call is never followed by a second transfer', 'A crash between the write and the call leaves the step to time out instead of retrying', 'The auto-rebalancer computes its cool-down from it', 'The control plane requires it in the request', 'It is what `kubectl get pvops` shows as progress'], correct: [0, 1], explain: 'A retried continue after a timeout once copied nothing while the source was unfrozen. A rare stall is accepted against a silent loss of writes.' },
    { topic: 'Steps and cutover', cat: 'field', text: 'The control plane reports `memberCount: 3` for a migration. How long may `Migrating` take before `StepDeadlineExceeded`?', options: ['60 minutes', '40 minutes', '30 minutes', '15 minutes', 'There is no limit'], correct: [0], explain: 'The copy is given 30 minutes plus 10 per volume of the subsystem: 30 + 3 × 10. There is no ceiling, so a large subsystem gets a long bound.' },
    { topic: 'Steps and cutover', cat: 'action', text: 'What does `Verifying` do?', options: ['Deletes the validation Jobs', 'Clears controllers that carry no namespace on the nodes that took part', 'Disconnects the target paths the Jobs connected', 'Disconnects the source node\'s paths', 'Honors a late `spec.abort`'], correct: [0, 1], explain: 'After the cutover the target\'s paths are the volume\'s data path, so releasing them would be an outage. What is cleared is the dead controller a lost path leaves, which would block the next migration.' },
    { topic: 'Steps and cutover', cat: 'concept', text: 'The validation Job fails on one of three consuming nodes. What happens?', options: ['The operation fails at once, and the migration is taken back', 'The Job is retried on that node', 'The migration continues with the two nodes that passed', 'The operation waits for the `Validating` deadline', 'The node is excluded and its pod restarted'], correct: [0], explain: 'The cutover is subsystem-wide, so continuing without one node is an outage there. The Job has no retry, and the first failure is fatal.' },

    { topic: 'Locks and pins', cat: 'field', text: 'Which annotations does a running `Migrate` operation hold?', options: ['`storage.simplyblock.io/active-ops` on every PersistentVolume of its subsystem', '`storage.simplyblock.io/active-volume-migration` on the StorageCluster', '`storage.simplyblock.io/selected-storage-node` on the claim', '`status.activeOpsRef` on the PersistentVolume', '`storage.simplyblock.io/relocated-to` on the PersistentVolume'], correct: [0, 1], explain: 'The subsystem lock and the cluster slot. The pin is the user\'s, a PersistentVolume has no field for the operator to add, and the relocation mark belongs to moves between clusters.' },
    { topic: 'Locks and pins', cat: 'concept', text: 'A drain raises twenty moves in one cluster at once. What happens?', options: ['One leaves `Pending` at a time, and the others wait there with `status.deferredSince` and `OperationQueued`', 'All twenty start, and the control plane serializes them', 'The second one fails with a refusal', 'They run in parallel, one per target node', 'The drain refuses to raise more than one'], correct: [0], explain: 'The cluster\'s migration slot lets one operation past `Pending`, because the control plane runs one data migration per cluster. The others hold nothing and wait.' },
    { topic: 'Locks and pins', cat: 'action', text: 'A claim is pinned to node A, and a person writes a `Migrate` of its volume to node B. What happens?', options: ['The operation fails with `VolumePinned`', 'The pin is changed to B', 'The operation waits until the pin is removed', 'The volume moves and the pin follows', 'The admission webhook refuses the object'], correct: [0], explain: 'A pinned volume is moved by its pin and nothing else. The check runs in the controller while the operation is `Pending`, not at admission.' },
    { topic: 'Locks and pins', cat: 'concept', text: 'Volume X is not pinned and shares its subsystem with volume Y, pinned to node A. A `Migrate` names X with target B. Which statements are true?', options: ['The operation fails with `VolumePinned`', 'X moves to B alone and Y stays', 'Both move to B and Y\'s pin is updated', 'The operation waits until Y is unpinned', 'Y\'s pin would not stand in the way if the target were node A'], correct: [0, 4], explain: 'The subsystem moves as a whole, so Y would move with X. A pin that names the target is no conflict.' },
    { topic: 'Locks and pins', cat: 'field', text: 'What value does the pin annotation `storage.simplyblock.io/selected-storage-node` carry?', options: ['The `status.uuid` of a StorageNode', 'The StorageNode object\'s name', 'The Kubernetes worker\'s name', 'The claim\'s `spec.volumeName`', 'The subsystem\'s NQN'], correct: [0], explain: 'The backend UUID. A webhook refuses a value that names no storage node the control plane knows.' },

    { topic: 'Roles and events', cat: 'concept', text: 'Which of these does the CSI driver do during a `Migrate`?', options: ['Connects the target\'s paths before the cutover', 'Flips the ANA states of the paths', 'Restarts the consuming pod on the target node', 'Unstages and restages the volume', 'Reports the copy\'s progress to the operator'], correct: [], explain: 'None. The Jobs connect the paths, the control plane drives ANA, and the kernel\'s multipath follows. The driver only gets the new paths when a pod is later restarted and stages again.' },
    { topic: 'Roles and events', cat: 'action', text: 'Where are the events of a PersistentVolumeOps found?', options: ['In `kubectl describe pvops <name>`', 'In the namespace `default`', 'In `kubectl get events -n simplyblock`', 'On the claim the volume is bound to', 'On the target StorageNode'], correct: [0, 1], explain: 'An event of a cluster-scoped object lands in `default`. The design\'s mirror onto the claim is not built.' },
    { topic: 'Roles and events', cat: 'concept', text: 'Which of these does the control plane do in a node migration?', options: ['Creates the migration and publishes the target\'s paths', 'Copies the data', 'Drives the ANA states at cutover', 'Refuses a create it will never accept, and answers one it cannot accept yet', 'Runs the validation Jobs on the consuming nodes'], correct: [0, 1, 2, 3], explain: 'The operator runs the Jobs. A 400 with a lasting reason fails the operation at once, and a refusal that clears by itself is retried inside `Validating`.' },
    { topic: 'Roles and events', cat: 'concept', text: 'Why are the target\'s paths connected with a controller-loss timeout of 60 seconds?', options: ['It is the CSI driver\'s `DefaultCtrlLossTmo`', 'The target\'s paths become the volume\'s data path, so every path of the volume ends on one timeout', 'The control plane answers with an hour, and that value is not used', 'The kernel refuses a longer timeout', 'It is the length of the cutover freeze'], correct: [0, 1, 2], explain: 'The operator overrides the control plane\'s answer so that what `status.migration.connections` shows is the connect that is made. The freeze is about two seconds.' },
    { topic: 'Roles and events', cat: 'concept', text: 'Which events report an operation that waits rather than fails?', options: ['`OperationQueued`', '`WaitingForConsumer`', '`TargetNodeNotReady`', '`StepDeadlineExceeded`', '`VolumePinned`'], correct: [0, 1, 2], explain: '`TargetNodeNotReady` is a warning, and the operation retries. A deadline and a pin end the operation as `Failed`.' },

    { topic: 'Rebalancing and realignment', cat: 'field', text: 'A StorageCluster sets `enableVolumeAutoPlacement: true` and `volumeAutoPlacement.disableMigration: true`. Which of these happen?', options: ['Load is evaluated each cycle', 'The candidates are logged', 'No PersistentVolumeOps is created', 'Deviation metrics are emitted', 'Volumes move at half the rate'], correct: [0, 1, 2, 3], explain: 'It is the dry run: the whole cycle runs and the moves are discarded.' },
    { topic: 'Rebalancing and realignment', cat: 'concept', text: 'Which of these raise a PersistentVolumeOps `Migrate` without a person writing it?', options: ['The auto-rebalancer', 'A StorageNodeOps `Remove` draining its node', 'The pin controller, when a claim\'s pin changes', 'The data realignment', 'The CSI driver, when a pod is rescheduled'], correct: [0, 1, 2], explain: 'The three go through one mover and are held to the same locks and pins. A realignment moves no volume, and the CSI driver raises nothing.' },
    { topic: 'Rebalancing and realignment', cat: 'concept', text: 'Which statements about the post-migration data realignment are true?', options: ['It restores the fault-tolerance and node-affinity guarantees a move invalidated', 'It is requested after `minMoves` successful moves, at most once per `interval`', 'While it runs the control plane refuses new migrations, and a waiting operation spends its `Validating` budget', 'Failed and aborted operations count toward it too', 'Turning it off turns volume migration off'], correct: [0, 1, 2], explain: 'Only a `Succeeded` move increments `status.volumeMoveGeneration`, because a failed one left the volume where it was. Migration has no switch.' },
    { topic: 'Rebalancing and realignment', cat: 'field', text: 'Which `volumeAutoPlacement` fields does the rebalancer read?', options: ['`imbalanceThreshold`', '`minHotColdDifferencePct`', '`defaultCoolDownSeconds`', '`maxVolumeMigrationsPerCycle`', '`evaluationInterval`'], correct: [0, 1, 2, 3, 4], explain: 'All five are read, with defaults of 80, 20, 600, 10, and 60 seconds. `storageNodeCandidateCount` and `metricsBackend` are the two that are not.' },
    { topic: 'Rebalancing and realignment', cat: 'field', text: 'A StorageCluster says nothing about rebalancing, migration, or realignment. Which of these are true?', options: ['Data realignment runs after moves', 'A realignment is requested after every successful move', 'Realignments are spaced at least 10 minutes apart', 'Auto-rebalancing is off', 'Volumes can still be migrated by hand and by drains'], correct: [0, 1, 2, 3, 4], explain: 'Realignment is on by default with `minMoves` 1 and an interval of 10 minutes, rebalancing is opt-in, and migration cannot be turned off.' },
    { topic: 'Rebalancing and realignment', cat: 'field', text: 'Which of these stop a hand-written PersistentVolumeOps `Migrate` from moving a volume?', options: ['`spec.disableDataRealignment: true`', '`spec.volumeAutoPlacement.disableMigration: true`', '`spec.enableVolumeAutoPlacement: false`', 'An empty `spec.volumeMigrationSettings.rebalancerImage`', '`spec.volumeMigrationSettings.dataRealignment.minMoves: 100`'], correct: [], explain: 'None. `disableMigration` only keeps the rebalancer from creating moves, and an empty image field falls back to the deployment\'s or the operator\'s own rebalancer image.' },
    { topic: 'Rebalancing and realignment', cat: 'field', text: 'What does setting `storageNodeCandidateCount` or `metricsBackend` change today?', options: ['How many hot nodes are evaluated per cycle', 'Where node load is read from', 'The latency percentile', 'The baseline window', 'The Prometheus address'], correct: [], explain: 'Both are accepted by the API and read by no code. The percentile is an operator flag, and the window and the address have their own fields.' },
    { topic: 'Rebalancing and realignment', cat: 'action', text: 'A StorageCluster sets `enableVolumeAutoPlacement: true` and a `volumeAutoPlacement` block holding only `imbalanceThreshold: 60`. What happens?', options: ['Every cycle is skipped, with an error in the operator\'s log', 'The chart\'s Prometheus address is used', 'Admission refuses the StorageCluster', 'A `VolumeRebalancingFailed` event is raised on the cluster', 'The rebalancer falls back to the control plane\'s metrics'], correct: [0], explain: 'The chart\'s address is the default only when the whole block is absent. A present block without `prometheusURL` fails configuration on every cycle, which is logged and counted and raises no event.' },
  ],

  tasks: [
    {
      title: 'Move a volume to another node',
      scenario: 'The claim `orders-db` is bound to the PersistentVolume `pvc-0c7e4b19-2f6a-4d83-9e51-7a2b8c3d4f60`. Move it to the StorageNode `prod-worker-5-0` of the cluster `prod` in the namespace `simplyblock`.',
      [`yaml`]: [
        'apiVersion: storage.simplyblock.io/v1alpha2',
        'kind: {{kind}}',
        'metadata:',
        '  name: orders-db-to-worker-5',
        'spec:',
        '  {{field}}: pvc-0c7e4b19-2f6a-4d83-9e51-7a2b8c3d4f60',
        '  action: {{action}}',
        '  {{block}}:',
        '    targetNodeRef:',
        '      namespace: {{namespace}}',
        '      name: prod-worker-5-0',
      ].join('\n') + '\n',
      blanks: {
        kind: { accept: ['PersistentVolumeOps'], hint: 'the cluster-scoped operation on one volume, short name `pvops`' },
        field: { accept: ['persistentVolumeName'], hint: 'names the volume, not the claim' },
        action: { accept: ['Migrate'], hint: 'another node of the same cluster' },
        block: { accept: ['migrate'], hint: 'the parameter block the action requires' },
        namespace: { accept: ['simplyblock'], hint: 'where the StorageNode lives' },
      },
      explain: 'A `PersistentVolumeOps` names the volume in `persistentVolumeName`, and `action: Migrate` requires the `migrate` block, whose `targetNodeRef` carries the node\'s namespace because the operation has none.',
    },
    {
      title: 'Pin a claim to a node',
      scenario: 'The StorageNode `prod-worker-5-0` reports `status.uuid: c41f0d2e-8a7b-4e55-b0c3-9f2a6d1e7b84`. Pin the claim `orders-db` to it, so that the pin controller moves the volume there and nothing else moves it afterward.',
      [`yaml`]: [
        'apiVersion: v1',
        'kind: {{kind}}',
        'metadata:',
        '  name: orders-db',
        '  namespace: shop',
        '  annotations:',
        '    {{key}}: {{value}}',
        'spec:',
        '  accessModes:',
        '    - ReadWriteOnce',
        '  storageClassName: simplyblock-prod',
        '  resources:',
        '    requests:',
        '      storage: 50Gi',
      ].join('\n') + '\n',
      blanks: {
        kind: { accept: ['PersistentVolumeClaim'], hint: 'the pin lives on the claim' },
        key: { accept: ['storage.simplyblock.io/selected-storage-node'], hint: 'the canonical pin annotation' },
        value: { accept: ['c41f0d2e-8a7b-4e55-b0c3-9f2a6d1e7b84'], hint: 'the backend UUID, not the object name' },
      },
      explain: 'The pin is the claim annotation `storage.simplyblock.io/selected-storage-node` with the node\'s backend UUID. A changed value raises a move, and every other move of the volume is refused with `VolumePinned`.',
    },
    {
      title: 'Rebalancing as a dry run, realignment batched',
      scenario: 'On the cluster `prod`, turn on latency-driven rebalancing as a dry run first, reading latency from the chart\'s Prometheus at `http://simplyblock-prometheus:9090`, and request a data realignment only after four moves have accumulated.',
      [`yaml`]: [
        'apiVersion: storage.simplyblock.io/v1alpha2',
        'kind: StorageCluster',
        'metadata:',
        '  name: prod',
        '  namespace: simplyblock',
        'spec:',
        '  maxSubsystemCount: 50',
        '  vcpuCount: 8',
        '  {{enable}}: true',
        '  volumeAutoPlacement:',
        '    disableMigration: {{dryrun}}',
        '    prometheusURL: {{address}}',
        '  volumeMigrationSettings:',
        '    dataRealignment:',
        '      minMoves: {{moves}}',
      ].join('\n') + '\n',
      blanks: {
        enable: { accept: ['enableVolumeAutoPlacement'], hint: 'the switch, a field of the spec rather than of the block' },
        dryrun: { accept: ['true'], hint: 'candidates logged, nothing created' },
        address: { accept: ['http://simplyblock-prometheus:9090'], hint: 'required once the block is present' },
        moves: { accept: ['4'], hint: 'how many moves accumulate before a realignment' },
      },
      explain: '`enableVolumeAutoPlacement` runs the cycle, `disableMigration: true` makes it a dry run, `prometheusURL` has no default inside a present block, and `minMoves: 4` batches the realignment.',
    },
  ],
});
