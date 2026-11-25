// Chapter 13: moving a volume to another storage node of the same cluster
// while its pod keeps running, with a PersistentVolumeOps of action Migrate.
// It covers what moves, the short pause at cutover, the object and its steps,
// the locks and pins around it, the controllers that raise moves by
// themselves, and the data realignment after them. Moves between clusters are
// the next chapter, and drains are covered from the node's side in
// node-removal.
TRAINING.register({
  id: 'volume-migration-nodes',
  title: 'Live migration of volumes between nodes',
  summary: 'Moving a volume to another storage node of the same cluster while its pod keeps running: a PersistentVolumeOps `Migrate`, what moves, the short pause at cutover, and the locks, pins, and rebalancer around it.',
  goal: 'You can move a volume to another node, follow the move step by step, stop it while it can still be stopped, explain why it waits or fails, and set up the rebalancer and the data realignment.',

  sections: [
    {
      heading: 'What moves, and what the cutover costs',
      body: [
        'A volume in simplyblock has two parts. Its **front end** lives on one storage node: the logical volume and its snapshots, which map the volume onto its data. Its **data** is spread over the drives of the whole cluster.',
        'A live migration moves the front end to another node. The data stays where it is. The pod keeps running on its worker, and its I/O switches path underneath it.',
        { steps: [
          '**Snapshots.** The volume\'s snapshots move to the target, oldest first.',
          '**Catch-up rounds.** While the volume keeps running, simplyblock takes up to three extra snapshots and moves each one. It stops once less than 500 MiB has changed since the last one.',
          '**Cutover.** The storage engine freezes the volume, so new I/O waits instead of failing. It moves the last changes and then sends I/O to the target.',
          '**Path switch.** The target\'s paths become ANA optimized and the source\'s inaccessible. The host\'s NVMe multipath follows, and the waiting I/O completes on the target.',
        ] },
        'The pause your application sees is the cutover. The storage engine gives it at most 12 seconds. The catch-up rounds keep the last changes small, so it is usually much shorter. If the cutover fails, the source paths reopen, one more catch-up round runs, and the cutover is tried again.',
        'Worked example for a shared subsystem: before the cutover, every path is made inaccessible, and simplyblock waits 2 seconds for I/O in flight. The pause is then at least 2 seconds, and at most about 2 + 12 = 14 seconds.',
        { table: { head: ['What', 'Moves?', 'Why'], rows: [
          ['Other volumes of the same NVMe-oF subsystem', 'yes', 'A subsystem moves as a whole. With the default `max_namespace_per_subsys: 1`, it holds one volume.'],
          ['The volume\'s snapshots', 'yes', 'They are part of the front end.'],
          ['The PersistentVolume', 'no', 'Its handle is `<cluster>:<pool>:<volume>` and names no node.'],
          ['The pod', 'no', 'It keeps running on its worker.'],
        ] } },
        'While a volume moves, you cannot delete it, resize it, or delete its snapshots. No volume on the source node can get a new snapshot until the move ends.',
        'The move stays inside one cluster. Moves between clusters are [the next chapter](chapter.html?c=volume-migration-clusters).',
      ],
      examples: [
        { title: 'A StorageClass that puts four volumes in one subsystem', file: 'examples/volume-migration-nodes/06-storageclass-shared-subsystem.yaml', notes: [
          { at: 'max_namespace_per_subsys: "4"', text: 'Up to four volumes share one subsystem, so a move of one moves all four.' },
          { at: 'provisioner: csi.simplyblock.io', text: 'The simplyblock CSI driver.' },
        ] },
      ],
    },
    {
      heading: 'Writing a migration',
      body: [
        'A PersistentVolumeOps (short name `pvops`) is cluster-scoped, like a PersistentVolume. It names the volume, not the claim. It names the target as a StorageNode object, not a UUID.',
        { steps: [
          'Find the volume behind the claim.',
          'Find the target node with `kubectl get sn -n simplyblock`.',
          'Write the PersistentVolumeOps and apply it.',
          'Watch it with `kubectl get pvops -w`.',
        ] },
        { code: '$ kubectl get pvc pg-data -n payments -o jsonpath=\'{.spec.volumeName}\'\npvc-5b8e2f31-7c4d-4a9e-8f12-3d6b0a1c9e47\n\n$ kubectl apply -f 01-persistentvolumeops-migrate.yaml\npersistentvolumeops.storage.simplyblock.io/pg-data-to-worker-4 created', title: 'Finding the volume and starting the move' },
        'The volume, the action, and the target cannot change later. Only `spec.abort` can. `Migrate` requires the `spec.migrate` block.',
        'The webhook refuses a move that can never work:',
        { list: [
          'no PersistentVolume of that name, or one of another CSI driver,',
          'a pNFS volume,',
          'a target StorageNode that does not exist,',
          'a target on the other site of a stretched cluster, or in another storage cluster.',
        ] },
        'A target that is not `Online` yet is accepted. The operation waits, retries every 15 seconds, and raises `TargetNodeNotReady` each time.',
      ],
      examples: [
        { title: 'A migration to another node', file: 'examples/volume-migration-nodes/01-persistentvolumeops-migrate.yaml', notes: [
          { at: 'kind: PersistentVolumeOps', text: 'Cluster-scoped, so there is no namespace. Its events land in `default`.' },
          { at: 'persistentVolumeName:', text: 'The PersistentVolume, not the claim.' },
          { at: 'action: Migrate', text: 'Moves the volume to another node of the same cluster.' },
          { at: 'targetNodeRef:', text: 'The target StorageNode. Immutable.' },
          { at: 'namespace: simplyblock', text: 'Required: the namespace of the StorageNode.' },
          { at: 'name: prod-worker-4-0', text: 'The StorageNode object name.' },
        ] },
      ],
    },
    {
      heading: 'Phases, steps, and the cutover',
      body: [
        'The phase is `Pending`, `Running`, then `Succeeded`, `Failed`, or `Aborted`. While it runs, `status.step.state` shows the step.',
        { table: { head: ['Step', 'What happens', 'Budget', 'Abort'], rows: [
          ['`Validating`', 'The control plane creates the move. A Job on each node that uses the volume checks the target\'s paths.', '15 min', 'yes'],
          ['`Migrating`', 'The operator starts the move once. The control plane moves the volume and cuts over.', '30 min + 10 min per volume in the subsystem', 'yes'],
          ['`Verifying`', 'The operator deletes the Jobs and cleans up dead connections.', '10 min', 'no'],
        ] } },
        'Worked example: the subsystem holds 3 volumes. `Migrating` then gets 30 + 3 × 10 = 60 minutes.',
        { code: '$ kubectl get pvops -w\nNAME                  VOLUME                                     ACTION    TARGET            PHASE       STEP         AGE\npg-data-to-worker-4   pvc-5b8e2f31-7c4d-4a9e-8f12-3d6b0a1c9e47   Migrate   prod-worker-4-0   Running     Validating   4s\npg-data-to-worker-4   pvc-5b8e2f31-7c4d-4a9e-8f12-3d6b0a1c9e47   Migrate   prod-worker-4-0   Running     Migrating    40s\npg-data-to-worker-4   pvc-5b8e2f31-7c4d-4a9e-8f12-3d6b0a1c9e47   Migrate   prod-worker-4-0   Running     Verifying    3m\npg-data-to-worker-4   pvc-5b8e2f31-7c4d-4a9e-8f12-3d6b0a1c9e47   Migrate   prod-worker-4-0   Succeeded   Completed    3m', title: 'Following a move' },
        'Why `Validating` comes first:',
        { list: [
          'Every pod that uses the volume must be `Running`. A pod still starting is waited for, with `WaitingForConsumer`.',
          'Each node\'s Job connects the target\'s paths and checks that they are there. The Job has 180 seconds and no retry. One failure fails the operation.',
          'So at cutover, every host already has a path to the target.',
        ] },
        'To stop a move, set `spec.abort: true` in `Validating` or `Migrating`. The operator cancels the move, disconnects the target paths, and ends `Aborted`. In `Verifying` the volume has already moved, so the abort is ignored and a delete is refused.',
        { code: 'kubectl patch pvops pg-data-to-worker-4 --type merge -p \'{"spec":{"abort":true}}\'', title: 'Stopping a move' },
      ],
      examples: [
        { title: 'Stopping a migration', file: 'examples/volume-migration-nodes/02-persistentvolumeops-abort.yaml', notes: [
          { at: '  abort: true', text: 'The only field you can change. Honored in `Validating` and `Migrating`.' },
          { at: 'action: Migrate', text: 'Unchanged.' },
        ] },
        { title: 'A target that already holds the volume', file: 'examples/volume-migration-nodes/03-persistentvolumeops-target-is-source.yaml', notes: [
          { at: 'name: pg-data-to-worker-2', text: 'Ends `Succeeded` with the event `TargetNodeIsSource`. Nothing moves.' },
          { at: 'name: prod-worker-2-0', text: 'The node the subsystem is already on.' },
        ] },
      ],
    },
    {
      heading: 'Two locks, pins, and what waits',
      body: [
        'Two locks keep moves from colliding. An operation that cannot take both waits in `Pending` with `OperationQueued`.',
        { table: { head: ['Lock', 'Where', 'Why'], rows: [
          ['Subsystem lock', 'Annotation `storage.simplyblock.io/active-ops` on every PersistentVolume of the subsystem', 'The subsystem moves as a whole.'],
          ['Cluster slot', 'Annotation `storage.simplyblock.io/active-volume-migration` on the StorageCluster', 'One volume migration runs per cluster at a time.'],
        ] } },
        'Worked example: a drain raises 20 moves. One leaves `Pending`. The other 19 wait, and each starts when the one before it ends.',
        { code: '$ kubectl get stc prod -n simplyblock -o jsonpath=\'{.metadata.annotations.storage\\.simplyblock\\.io/active-volume-migration}\'\npg-data-to-worker-4', title: 'Which operation holds the cluster slot' },
        'A **pin** is the claim annotation `storage.simplyblock.io/selected-storage-node`. Its value is a storage node\'s UUID. After the volume is created, only a change to the pin moves it:',
        { list: [
          'Change the pin, and the operator creates a move to the new node.',
          'Remove the pin, and the volume stays where it is.',
          'Any other move of a pinned volume fails with `VolumePinned`. So does a move of a volume that shares a subsystem with one pinned elsewhere.',
        ] },
      ],
      examples: [
        { title: 'A pinned claim', file: 'examples/volume-migration-nodes/04-pvc-pinned.yaml', notes: [
          { at: 'selected-storage-node:', text: 'The pin. The value is a StorageNode\'s `status.uuid`, not its name.' },
          { at: 'storageClassName: simplyblock-prod', text: 'The class does not pin. The annotation does.' },
        ] },
      ],
    },
    {
      heading: 'Who does what during the move',
      body: [
        { table: { head: ['Component', 'Role'], rows: [
          ['Control plane', 'Creates the move, publishes the target\'s paths, moves the volume, and switches the paths at cutover.'],
          ['Operator', 'Takes the locks, runs the Jobs, starts the move once, and cleans up.'],
          ['Jobs', 'Connect the target\'s paths before the move, or disconnect them after an abort.'],
          ['CSI driver', 'Nothing. A pod that restarts later gets the new paths as usual.'],
          ['Host kernel', 'NVMe multipath sends I/O to the path that is accessible.'],
        ] } },
        'A refusal that will never change fails the operation at once, with the reason in `status.message`. A refusal that clears by itself, such as a running rebalance, is retried inside `Validating`.',
        'Events land on the PersistentVolumeOps, in the namespace `default`:',
        { code: '$ kubectl get events -n default --field-selector involvedObject.kind=PersistentVolumeOps\nLAST SEEN   TYPE     REASON               OBJECT                                    MESSAGE\n3m          Normal   OperationStarted     persistentvolumeops/pg-data-to-worker-4   ...\n3m          Normal   MigrationCreated     persistentvolumeops/pg-data-to-worker-4   ...\n2m          Normal   MigrationStarted     persistentvolumeops/pg-data-to-worker-4   ...\n10s         Normal   OperationSucceeded   persistentvolumeops/pg-data-to-worker-4   ...', title: 'The events of one move' },
        { table: { head: ['Reason', 'Type', 'Meaning'], rows: [
          ['`OperationQueued`', 'Normal', 'Waiting for a lock'],
          ['`TargetNodeNotReady`', 'Warning', 'The target is not `Online`, retried'],
          ['`WaitingForConsumer`', 'Normal', 'A pod using the volume has not started'],
          ['`VolumePinned`', 'Warning', 'A pin is in the way, the operation fails'],
          ['`TargetNodeIsSource`', 'Normal', 'The volume is already there'],
          ['`StepDeadlineExceeded`', 'Warning', 'A step ran out of time, the operation fails'],
          ['`CleanupBlocked`', 'Warning', 'A delete waits until the cleanup can finish'],
        ] } },
        'The operator also exports metrics, such as `simplyblock_persistentvolume_operations_total` and `simplyblock_persistentvolume_operation_duration_seconds`.',
      ],
    },
    {
      heading: 'Moves nobody writes by hand, and the realignment after them',
      body: [
        'Three controllers create the same PersistentVolumeOps for you:',
        { list: [
          '**The rebalancer**, when `enableVolumeAutoPlacement: true`. Its operations are named `rebalance-<volume UUID>`.',
          '**A drain**, a StorageNodeOps `Remove`. See [the node removal chapter](chapter.html?c=node-removal).',
          '**The pin controller**, when a pin changes.',
        ] },
        'The rebalancer runs every 60 seconds by default. It looks for a node whose write latency is more than 80% above its baseline. It moves up to 10 volumes to a node at least 20 points cooler. A moved volume is left alone for 600 seconds.',
        'Worked example: node A\'s latency is 95% above baseline and node B\'s is 10%. A is above 80, and B is 85 points cooler, so volumes move from A to B.',
        'Start with `disableMigration: true`. The rebalancer then logs its choices and moves nothing. Set `prometheusURL` whenever you write a `volumeAutoPlacement` block, because the default applies only when the block is absent.',
        'After moves, the cluster\'s data no longer matches the new placement. A **data realignment** fixes fault tolerance and node affinity. The operator requests one after `minMoves` successful moves (default 1), at most once per `interval` (default 10 minutes).',
        'While a realignment runs, the control plane refuses new moves. A move waits in `Validating`, and fails after its 15 minutes. Raise `minMoves` to realign less often during a long drain.',
        { code: 'kubectl annotate stc prod -n simplyblock simplyblock.io/trigger-realignment="$(date +%s)" --overwrite', title: 'Requesting a realignment now' },
      ],
      examples: [
        { title: 'Rebalancing on, realignment batched', file: 'examples/volume-migration-nodes/05-storagecluster-autoplacement.yaml', notes: [
          { at: 'maxSubsystemCount: 50', text: 'A required sizing field, with `vcpuCount`.' },
          { at: 'enableVolumeAutoPlacement: true', text: 'Turns the rebalancer on. Unset means off.' },
          { at: 'disableMigration: false', text: 'Set `true` for a dry run.' },
          { at: 'imbalanceThreshold: 80', text: 'Latency above baseline, in percent, that makes a node a source.' },
          { at: 'defaultCoolDownSeconds: 600', text: 'Kept in memory, so an operator restart forgets it.' },
          { at: 'prometheusURL:', text: 'Required when the block is present.' },
          { at: 'minMoves: 5', text: 'Five moves before each realignment.' },
        ] },
        { title: 'A move created by the rebalancer', file: 'examples/volume-migration-nodes/07-persistentvolumeops-rebalancer.yaml', notes: [
          { at: 'name: rebalance-9a3c7e21-4b5d-4f60-8e12-7c3b1a9d0f54', text: 'Named after the backend volume UUID.' },
          { at: 'storage.simplyblock.io/rebalancer: "true"', text: 'Marks the rebalancer\'s moves. It deletes them once they end.' },
          { at: 'kind: StorageCluster', text: 'The creator. A drain\'s moves name a StorageNodeOps here.' },
        ] },
      ],
    },
    {
      heading: 'Limits to know',
      body: [
        { list: [
          'Status shows the step, not how far the move has got.',
          'Nothing deletes a finished operation you wrote by hand. Delete it yourself.',
          'Events appear on the PersistentVolumeOps only, not on the claim.',
          '`storageNodeCandidateCount` and `metricsBackend` are accepted but have no effect.',
        ] },
        { code: '$ kubectl get pvops\nNAME                  VOLUME                                     ACTION    TARGET            PHASE       STEP        AGE\npg-data-to-worker-4   pvc-5b8e2f31-7c4d-4a9e-8f12-3d6b0a1c9e47   Migrate   prod-worker-4-0   Succeeded   Completed   2d\n\n$ kubectl delete pvops pg-data-to-worker-4\npersistentvolumeops.storage.simplyblock.io "pg-data-to-worker-4" deleted', title: 'Cleaning up a finished move' },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The apply is refused: no PersistentVolume of that name.', cause: 'You used the claim\'s name.', fix: 'Use the claim\'s `spec.volumeName`.' },
          { symptom: 'No phase, and `TargetNodeNotReady` repeats.', cause: 'The target is not `Online`.', fix: 'Wait for the node, or delete the operation and pick another node.' },
          { symptom: '`Pending` with `OperationQueued`.', cause: 'Another move holds the cluster slot or the subsystem.', fix: 'Wait. The message names the holder.' },
          { symptom: 'Fails at once with `VolumePinned`.', cause: 'The volume or a subsystem sibling is pinned elsewhere.', fix: 'Change the pin to the target\'s UUID instead.' },
          { symptom: '`WaitingForConsumer`, then `StepDeadlineExceeded`.', cause: 'A pod using the volume is not `Running`.', fix: 'Fix or delete the pod.' },
          { symptom: 'Fails in `Validating`, naming a node.', cause: 'That node could not reach the target\'s paths.', fix: 'Read the Job\'s log in the StorageCluster\'s namespace and fix the network.' },
          { symptom: 'Moves fail in `Validating` right after other moves.', cause: 'A data realignment is running.', fix: 'Raise `dataRealignment.minMoves`.' },
          { symptom: '`Aborted`, but the volume is on the target.', cause: 'The abort arrived just after the move finished.', fix: 'Check where the volume is before you act.' },
          { symptom: 'The rebalancer never moves anything.', cause: 'No `prometheusURL` in the block, a node is offline, or `disableMigration` is true.', fix: 'Set `prometheusURL` and read the operator\'s log.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'What moves', cat: 'concept', text: 'What does a PersistentVolumeOps `Migrate` move to the target node?', options: ['The volume\'s front end: its logical volume and snapshots', 'Every volume of the same NVMe-oF subsystem', 'All of the volume\'s data, block by block', 'The consuming pod', 'The PersistentVolume object'], correct: [0, 1], explain: 'The front end moves and the data stays on the cluster\'s drives. The subsystem moves as a whole. The pod and the PersistentVolume do not change.' },
    { topic: 'What moves', cat: 'concept', text: 'What does your application see at cutover?', options: ['A short pause in I/O, with no errors', 'An I/O error that the pod must retry', 'A restart of the pod on the target node', 'An unmount and remount of the volume', 'Nothing, the cutover has no pause at all'], correct: [0], explain: 'The storage engine freezes the volume, so new I/O waits. The host\'s multipath then sends it to the target.' },
    { topic: 'What moves', cat: 'concept', text: 'How long can the cutover pause last for a volume with its own subsystem?', options: ['At most 12 seconds', 'Exactly 2 seconds', 'At most 15 minutes', 'Up to an hour', 'As long as the whole data copy'], correct: [0], explain: 'The storage engine gives the cutover at most 12 seconds. The catch-up rounds usually make it much shorter.' },
    { topic: 'What moves', cat: 'concept', text: 'What keeps the cutover short?', options: ['Up to three catch-up snapshots, until less than 500 MiB has changed', 'Moving the snapshots before the cutover', 'Stopping the pod during the move', 'Copying the data to the target first', 'Disabling the volume\'s QoS'], correct: [0, 1], explain: 'Most of the front end moves while the volume runs, so only the last changes move during the freeze.' },
    { topic: 'What moves', cat: 'concept', text: 'A StorageClass does not set `max_namespace_per_subsys`. How many volumes does a `Migrate` move?', options: ['Only the named volume', 'Every volume in its pool', 'Every volume on its source node', 'Every volume of the claim\'s namespace', 'Every volume of its consistency group'], correct: [0], explain: 'The default is 1, so each volume has its own subsystem.' },
    { topic: 'What moves', cat: 'action', text: 'Which of these are refused while a volume moves?', options: ['Deleting the volume', 'Resizing the volume', 'Deleting one of its snapshots', 'A new snapshot of another volume on the source node', 'Reads and writes from the pod'], correct: [0, 1, 2, 3], explain: 'The move protects the volume and the source node\'s snapshots. I/O keeps flowing.' },

    { topic: 'Writing the operation', cat: 'field', text: 'Which fields can change after the operation is created?', options: ['`spec.abort`', '`spec.migrate.targetNodeRef`', '`spec.persistentVolumeName`', '`spec.action`', '`spec.creatorRef`, once set'], correct: [0], explain: 'Only the abort can change.' },
    { topic: 'Writing the operation', cat: 'field', text: 'Why does `spec.migrate.targetNodeRef` carry a namespace?', options: ['The operation is cluster-scoped, so a bare name has no namespace', 'Two clusters in two namespaces can each have a node of the same name', 'The PersistentVolume lives in that namespace', 'The control plane looks nodes up by namespace', 'The namespace selects the storage pool'], correct: [0, 1], explain: 'A cluster-scoped object must say where a namespaced object lives.' },
    { topic: 'Writing the operation', cat: 'action', text: 'Which of these does the webhook refuse when you create the operation?', options: ['A volume of another CSI driver', 'A pNFS volume', 'A target StorageNode that does not exist', 'A target node that is offline', 'A target node of another storage cluster'], correct: [0, 1, 2, 4], explain: 'An offline node may come back, so the operation is accepted and waits with `TargetNodeNotReady`.' },
    { topic: 'Writing the operation', cat: 'concept', text: 'You move the claim `pg-data` in the namespace `payments`. Which of these are true of the operation?', options: ['It names the claim\'s PersistentVolume, not the claim', 'It has no `metadata.namespace`', 'It is created in the namespace `payments`', 'It names its storage cluster', 'It names the target node by UUID'], correct: [0, 1], explain: 'The kind is cluster-scoped and names the volume. The cluster comes from the volume handle.' },

    { topic: 'Steps', cat: 'concept', text: 'In which steps does `spec.abort` work?', options: ['`Validating`', '`Migrating`', '`Verifying`', '`Completed`', 'Any step while `Running`'], correct: [0, 1], explain: 'In `Verifying` the volume has already moved.' },
    { topic: 'Steps', cat: 'action', text: 'What happens in `Validating`?', options: ['The control plane creates the move and publishes the target\'s paths', 'The operator waits for every pod using the volume to be `Running`', 'A Job on each using node checks the target\'s paths', 'The cutover', 'The source\'s paths are disconnected'], correct: [0, 1, 2], explain: 'The cutover is in `Migrating`. The operator never disconnects the source\'s paths.' },
    { topic: 'Steps', cat: 'field', text: 'The subsystem holds 3 volumes. How long may `Migrating` take?', options: ['60 minutes', '40 minutes', '30 minutes', '15 minutes', 'No limit'], correct: [0], explain: '30 minutes plus 10 per volume: 30 + 3 × 10 = 60.' },
    { topic: 'Steps', cat: 'action', text: 'What does `Verifying` do?', options: ['Deletes the Jobs', 'Cleans up dead connections on the nodes that took part', 'Disconnects the target paths', 'Disconnects the source\'s paths', 'Honors a late abort'], correct: [0, 1], explain: 'After cutover the target\'s paths carry the I/O, so they stay.' },
    { topic: 'Steps', cat: 'concept', text: 'The Job fails on one of three nodes that use the volume. What happens?', options: ['The operation fails, and the move is canceled', 'The Job is retried', 'The move continues with the two good nodes', 'The operation waits for the deadline', 'The node\'s pod is restarted'], correct: [0], explain: 'The cutover affects every node, so one node without a path would lose access.' },
    { topic: 'Steps', cat: 'action', text: 'Which of these end the operation as `Succeeded`?', options: ['The move finishes and the cleanup is done', 'The volume is already on the target', 'An abort in `Validating`', 'A step runs out of time', 'A pin blocks the move'], correct: [0, 1], explain: 'An already-placed volume ends `Succeeded` with `TargetNodeIsSource`. An abort ends `Aborted`, and a deadline or a pin ends `Failed`.' },

    { topic: 'Locks and pins', cat: 'field', text: 'Which annotations does a running move hold?', options: ['`storage.simplyblock.io/active-ops` on every PersistentVolume of the subsystem', '`storage.simplyblock.io/active-volume-migration` on the StorageCluster', '`storage.simplyblock.io/selected-storage-node` on the claim', 'A lock in the PersistentVolume\'s status', '`storage.simplyblock.io/relocated-to` on the PersistentVolume'], correct: [0, 1], explain: 'The subsystem lock and the cluster slot. The pin belongs to you.' },
    { topic: 'Locks and pins', cat: 'concept', text: 'A drain raises 20 moves in one cluster. What happens?', options: ['One runs, and the others wait in `Pending` with `OperationQueued`', 'All 20 start at once', 'The second one fails', 'They run in parallel, one per target node', 'The drain raises only one'], correct: [0], explain: 'One volume migration runs per cluster at a time.' },
    { topic: 'Locks and pins', cat: 'action', text: 'A claim is pinned to node A. You write a `Migrate` of its volume to node B. What happens?', options: ['The operation fails with `VolumePinned`', 'The pin changes to B', 'The operation waits for the pin to go', 'The volume moves and the pin follows', 'The webhook refuses the object'], correct: [0], explain: 'Only a change to the pin moves a pinned volume.' },
    { topic: 'Locks and pins', cat: 'concept', text: 'Volume X shares a subsystem with volume Y, which is pinned to node A. You move X to node B. Which are true?', options: ['The operation fails with `VolumePinned`', 'X moves alone and Y stays', 'Both move and Y\'s pin is updated', 'The operation waits for Y to be unpinned', 'A move of X to node A would not be blocked by Y\'s pin'], correct: [0, 4], explain: 'Y would move with X. A pin that names the target is no conflict.' },
    { topic: 'Locks and pins', cat: 'field', text: 'What value does the pin annotation carry?', options: ['The StorageNode\'s `status.uuid`', 'The StorageNode\'s name', 'The worker\'s name', 'The claim\'s `spec.volumeName`', 'The subsystem NQN'], correct: [0], explain: 'The UUID. The webhook refuses a value that names no storage node.' },

    { topic: 'Roles and events', cat: 'concept', text: 'What does the CSI driver do during a move?', options: ['Connects the target\'s paths', 'Switches the ANA states', 'Restarts the pod on the target', 'Unstages and restages the volume', 'Reports progress'], correct: [], explain: 'Nothing. The Jobs connect the paths, the control plane switches them, and the kernel follows.' },
    { topic: 'Roles and events', cat: 'action', text: 'Where do you find the events of a PersistentVolumeOps?', options: ['In `kubectl describe pvops <name>`', 'In the namespace `default`', 'In `kubectl get events -n simplyblock`', 'On the claim', 'On the target StorageNode'], correct: [0, 1], explain: 'Events of a cluster-scoped object land in `default`.' },
    { topic: 'Roles and events', cat: 'concept', text: 'Which events mean the operation waits rather than fails?', options: ['`OperationQueued`', '`WaitingForConsumer`', '`TargetNodeNotReady`', '`StepDeadlineExceeded`', '`VolumePinned`'], correct: [0, 1, 2], explain: 'A deadline and a pin end the operation as `Failed`.' },
    { topic: 'Roles and events', cat: 'concept', text: 'Which of these does the control plane do in a move?', options: ['Creates the move and publishes the target\'s paths', 'Moves the volume\'s front end', 'Switches the ANA states at cutover', 'Refuses a move while a rebalance runs', 'Runs the path-check Jobs'], correct: [0, 1, 2, 3], explain: 'The operator runs the Jobs.' },

    { topic: 'Rebalancing and realignment', cat: 'field', text: 'A StorageCluster sets `enableVolumeAutoPlacement: true` and `disableMigration: true`. What happens?', options: ['Load is checked each cycle', 'The chosen moves are logged', 'No PersistentVolumeOps is created', 'Metrics are still exported', 'Volumes move at half the rate'], correct: [0, 1, 2, 3], explain: 'It is a dry run.' },
    { topic: 'Rebalancing and realignment', cat: 'concept', text: 'Which of these create a `Migrate` without you writing it?', options: ['The rebalancer', 'A StorageNodeOps `Remove`', 'A change to a claim\'s pin', 'The data realignment', 'The CSI driver, when a pod moves'], correct: [0, 1, 2], explain: 'A realignment moves no volume, and the CSI driver creates nothing.' },
    { topic: 'Rebalancing and realignment', cat: 'concept', text: 'Which statements about the data realignment are true?', options: ['It fixes fault tolerance and node affinity after moves', 'It is requested after `minMoves` successful moves, at most once per `interval`', 'While it runs, new moves wait in `Validating`', 'Failed moves count toward it', 'Turning it off turns migration off'], correct: [0, 1, 2], explain: 'Only successful moves count. Migration cannot be turned off.' },
    { topic: 'Rebalancing and realignment', cat: 'field', text: 'A StorageCluster says nothing about rebalancing or realignment. Which are true?', options: ['Realignment runs after moves', 'A realignment is requested after every successful move', 'Realignments are at least 10 minutes apart', 'The rebalancer is off', 'You can still move volumes by hand'], correct: [0, 1, 2, 3, 4], explain: 'Realignment is on by default with `minMoves: 1` and `interval: 10m`. The rebalancer is opt-in.' },
    { topic: 'Rebalancing and realignment', cat: 'action', text: 'The `volumeAutoPlacement` block holds only `imbalanceThreshold: 60`. What happens?', options: ['Every cycle is skipped, with an error in the operator\'s log', 'The chart\'s Prometheus address is used', 'The StorageCluster is refused', 'A warning event is raised on the cluster', 'The rebalancer uses the control plane\'s metrics'], correct: [0], explain: 'The default address applies only when the whole block is absent.' },
    { topic: 'Rebalancing and realignment', cat: 'concept', text: 'Node A is 95% above its latency baseline and node B is 10% above. With the defaults, does the rebalancer move volumes from A to B?', options: ['Yes: A is above 80 and B is 85 points cooler', 'No: B must be at least 50 points cooler', 'No: A must be 100% above baseline', 'No: the rebalancer moves only pinned volumes', 'Yes, but only one volume per hour'], correct: [0], explain: 'The defaults are `imbalanceThreshold: 80` and `minHotColdDifferencePct: 20`. Up to 10 volumes move per cycle.' },
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
