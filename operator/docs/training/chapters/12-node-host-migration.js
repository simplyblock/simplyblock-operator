// Chapter 12: moving a storage node onto another Kubernetes worker with a
// StorageNodeOps of action Migrate. It sits beside the removal and expansion
// chapters because the three answer one question together, which tool changes
// the host a node runs on. It closes with the maintenance window, the tool for
// a host that comes back.
TRAINING.register({
  id: 'node-host-migration',
  title: 'Migrating a storage node to a different host',
  summary: 'Moving one storage node onto another Kubernetes worker with a StorageNodeOps `Migrate`. The node keeps its UUID and its volumes. The target\'s drives join the cluster, and the data on the old drives is rebuilt from parity.',
  goal: 'You can choose between a migration, a maintenance window, and a removal, prepare the target worker, write the `Migrate` operation, follow its four steps, and know which steps you can still call off.',

  sections: [
    {
      heading: 'Which tool changes the host a node runs on',
      body: [
        'A **storage node** is one storage-engine (SPDK) process on one Kubernetes worker. Its StorageNode object names that worker in `spec.workerNode`.',
        'To move the node to another worker, create a StorageNodeOps with `action: Migrate`. The node keeps its UUID, its volumes, and its slot. Only the machine changes. No volume moves to another node, so no PersistentVolumeOps is created.',
        { table: { head: ['Situation', 'Tool', 'Result'], rows: [
          ['New hardware replaces a host', 'StorageNodeOps `Migrate`', 'Same UUID and volumes, on the target worker'],
          ['The same host reboots and comes back with its drives', '`HostMaintenance`, raised when you cordon the worker', 'Node shut down, then restarted on the same worker'],
          ['The node leaves the cluster for good', '[Removing a node](chapter.html?c=node-removal), `Remove`', 'Volumes drained, node `Removed`'],
          ['A host is added as extra capacity', '[Expanding a cluster](chapter.html?c=cluster-expansion)', 'A new StorageNode with a new UUID'],
        ] } },
        'Here is the node before a move. The `Worker` column is what a migration changes:',
        { code: '$ kubectl get sn -n simplyblock\nNAME                    CLUSTER      WORKER     SOCKET   SLOT   PHASE    STEP   STATUS   HEALTH   AGE\nproduction-worker-3-0   production   worker-3   0        0      Online          online   true     41d', title: 'A storage node and its worker' },
        { callout: 'Do not edit `spec.workerNode` by hand. The webhook refuses it and tells you to use a StorageNodeOps with `action: Migrate`.', kind: 'warn' },
      ],
    },
    {
      heading: 'Preparing the target worker',
      body: [
        'The operator checks the target in the first step, `Preparing`. The operation fails when:',
        { list: [
          'the target is not a node of this Kubernetes cluster,',
          'the target is not `Ready` (the operator does not wait for it),',
          'the StorageNode has no `status.uuid` yet.',
        ] },
        'Check three more things yourself:',
        { list: [
          '**Drives.** The target binds drives at the source\'s PCI addresses. List drives at other addresses in `spec.migrate.newSsdPcie`. With none of the drives present, the restart fails.',
          '**One node per worker.** Pick a worker that runs no storage node of this cluster.',
          '**No cordon yet.** A cordon on the source starts a maintenance window, and your migration waits behind it. Migrate first, then cordon the old host.',
        ] },
        { code: '$ kubectl get node worker-5\nNAME       STATUS   ROLES    AGE   VERSION\nworker-5   Ready    <none>   2h    v1.31.4\n\n$ kubectl get sn -n simplyblock -o wide | grep worker-5\n(no output: no storage node runs there yet)', title: 'Checking a target before you migrate' },
        'The cluster must also be `active` and not rebalancing. So you migrate several nodes one after another, never in parallel.',
        { callout: 'Rehearse your first migration on a cluster without production data.', kind: 'tip' },
      ],
    },
    {
      heading: 'Writing the operation',
      body: [
        'A migration is one object with three required fields. You cannot change them after you create the object.',
        { table: { head: ['Field', 'Required', 'What it does'], rows: [
          ['`spec.nodeRef`', 'yes', 'The StorageNode to move, in the same namespace.'],
          ['`spec.action`', 'yes', '`Migrate`.'],
          ['`spec.migrate.targetWorkerNode`', 'yes', 'The worker the node moves to.'],
          ['`spec.migrate.newSsdPcie`', 'no', 'PCI addresses of extra drives to bind on the target. NVMe clusters only.'],
          ['`spec.reattachVolume`', 'no', 'Moves the node\'s EBS volumes to the new EC2 instance. AWS only.'],
          ['`spec.force`', 'no', 'Defaults to true for `Migrate`. Leave it unset.'],
          ['`spec.abort`', 'no', 'Calls the operation off, in `Preparing` only.'],
        ] } },
        { code: '$ kubectl apply -f 01-storagenodeops-migrate.yaml\nstoragenodeops.storage.simplyblock.io/move-production-worker-3-0 created', title: 'Starting a migration' },
        { callout: 'With `force: false` the control plane refuses to restart an online node, and `Relocating` fails after 15 minutes. On a `LogicalBlock` cluster, leave `newSsdPcie` and `reattachVolume` unset: the restart fails with them.', kind: 'warn' },
      ],
      examples: [
        { title: 'A migration onto identical hardware', file: 'examples/node-host-migration/01-storagenodeops-migrate.yaml', notes: [
          { at: 'kind: StorageNodeOps', text: 'Short name `snops`. The object stays after the operation ends, as a record.' },
          { at: 'nodeRef: production-worker-3-0', text: 'The StorageNode by name. The name keeps saying `worker-3` after the move.' },
          { at: 'action: Migrate', text: 'Moves the node and keeps its identity.' },
          { at: 'targetWorkerNode: worker-5', text: 'Required and immutable.' },
        ] },
        { title: 'A migration that binds drives at new addresses', file: 'examples/node-host-migration/02-storagenodeops-migrate-new-drives.yaml', notes: [
          { at: 'name: move-production-worker-3-0-new-drives', text: 'Each migration is its own object, with its own name.' },
          { at: 'newSsdPcie:', text: 'Drives the target has at addresses the source did not.' },
          { at: '"0000:88:00.0"', text: 'Quoted, so YAML reads it as a string.' },
        ] },
        { title: 'A migration on AWS that moves the EBS drives', file: 'examples/node-host-migration/06-storagenodeops-migrate-ebs.yaml', notes: [
          { at: 'reattachVolume: true', text: 'Moves the tagged EBS volumes to the new instance. Nothing is rebuilt.' },
          { at: 'targetWorkerNode: ip-10-0-2-17.ec2.internal', text: 'The new EC2 instance, as a Kubernetes worker.' },
        ] },
      ],
    },
    {
      heading: 'Admission and the four steps',
      body: [
        'A new operation starts as `Pending`. It waits there for two things:',
        { list: [
          '**The cluster gate.** The cluster must be `active` and not rebalancing. Otherwise you see a `ClusterNotReady` event.',
          '**The node lock.** The operation writes its name into the StorageNode\'s `status.activeOpsRef`. If another operation holds it, you see `OperationQueued`.',
        ] },
        'Then the phase becomes `Running`, and four steps follow. Each step has a time budget.',
        { table: { head: ['Step', 'Budget', 'Abort?', 'What happens', 'Done when'], rows: [
          ['`Preparing`', '15 min', 'yes', 'The operator copies the node\'s configuration to the target and labels it', 'The target\'s pod is `Ready` and in DNS'],
          ['`Relocating`', '15 min', 'no', 'The operator asks the control plane to restart the node on the target', 'The node leaves `online`'],
          ['`AwaitingNode`', '45 min', 'no', 'The node restarts on the target', 'The node is `online` again'],
          ['`Promoting`', '30 min', 'no', 'The control plane promotes the node, then the operator points the StorageNode at the target', 'Both are done'],
        ] } },
        'The budgets add up: 15 + 15 + 45 + 30 = 105 minutes. That is the longest a migration runs before it succeeds or fails. A step that runs out fails the operation with `StepDeadlineExceeded`.',
        { code: '$ kubectl get snops -n simplyblock -w\nNAME                        NODE                    ACTION    PHASE       STEP           AGE\nmove-production-worker-3-0  production-worker-3-0   Migrate   Running     Preparing      5s\nmove-production-worker-3-0  production-worker-3-0   Migrate   Running     Relocating     2m\nmove-production-worker-3-0  production-worker-3-0   Migrate   Running     AwaitingNode   2m\nmove-production-worker-3-0  production-worker-3-0   Migrate   Running     Promoting      6m\nmove-production-worker-3-0  production-worker-3-0   Migrate   Succeeded   Completed      7m', title: 'Following a migration' },
        '`Preparing` waits until the target\'s address is in DNS, because the control plane looks it up. With a managed control plane, the operator uses the worker\'s internal IP on port 5000 instead.',
      ],
    },
    {
      heading: 'Promoting: the control plane\'s half and the operator\'s',
      body: [
        'The drives do not travel with the node. In simplyblock, the data of every volume is spread over the drives of the whole cluster. So the old drives can be replaced, and their data rebuilt from parity, while the volumes stay readable.',
        { steps: [
          '**Restart.** The node starts on the target. A drive the node has not seen before becomes **new**. A drive of the node that is missing on the target becomes **removed**.',
          '**Promote.** The control plane erases each new drive and adds it to the cluster. Then it fails each removed drive, which starts a rebuild of its data from parity.',
          '**Re-point.** The operator sets `spec.workerNode` to the target, appends `newSsdPcie` to `spec.config.pcieAllowList`, and labels the target. It removes the labels from the source if no other node of the cluster runs there.',
        ] },
        'Worked example: the old host had 4 drives and the target has 4 different ones. After the restart, the node lists 4 new and 4 removed drives. The promote adds the 4 new ones and fails the 4 removed ones. The cluster then rebuilds the data of 4 drives.',
        'The migration reads `Succeeded` before the rebuild ends. The next migration waits until it does:',
        { code: '$ kubectl get stc production -n simplyblock -o jsonpath=\'{.status.phase}{" rebalancing="}{.status.rebalancing}{"\\n"}\'\nRebalancing rebalancing=true', title: 'The rebuild after a promote' },
        'If a drive is still new or removed after the promote, the control plane answers 409. The operator calls the promote again about once a minute, for up to 30 minutes. Failing a drive is refused while any volume migration runs in the cluster.',
        '**On AWS with EBS drives**, set `reattachVolume: true`. The control plane moves the EBS volumes tagged `simplyblock-storage` or `simplyblock-jm` to the new instance. The node finds its own drives, so nothing is rebuilt. Elsewhere, leave it unset, or the restart can fail.',
        { callout: 'Use empty, dedicated drives on the target. The promote erases them.', kind: 'warn' },
      ],
      examples: [
        { title: 'The StorageNode after the promote', file: 'examples/node-host-migration/03-storagenode-after-promotion.yaml', notes: [
          { at: 'name: production-worker-3-0', text: 'Unchanged. The name says where the node was created.' },
          { at: 'workerNode: worker-5', text: 'Set to the target after the promote.' },
          { at: 'slot: 0', text: 'Unchanged. The target\'s topology label uses it.' },
          { at: 'deviceNames:', text: 'Unchanged by the migration.' },
          { at: 'pcieAllowList:', text: 'The `newSsdPcie` addresses, added at the promote.' },
          { at: 'uuid: 4b1e8f0a', text: 'The same UUID as before the move.' },
        ] },
      ],
    },
    {
      heading: 'Calling it off, and following it',
      body: [
        'Set `spec.abort: true` to call a migration off. It works only in `Preparing`, before the control plane is asked for anything. The phase then becomes `Aborted`.',
        'From `Relocating` on, you see `AbortRefused` and the migration runs to its end. The webhook also refuses to delete the operation in those steps.',
        { code: '$ kubectl patch snops move-production-worker-3-0 -n simplyblock --type merge -p \'{"spec":{"abort":true}}\'\nstoragenodeops.storage.simplyblock.io/move-production-worker-3-0 patched\n\n$ kubectl get events -n simplyblock --field-selector involvedObject.name=move-production-worker-3-0\nLAST SEEN   TYPE      REASON         OBJECT                                      MESSAGE\n10s         Warning   AbortRefused   storagenodeops/move-production-worker-3-0   ...', title: 'An abort that came too late' },
        'An abort in `Preparing` leaves the target\'s label and copied configuration in place. A new `Migrate` to the same target reuses them.',
        { table: { head: ['Event', 'Type', 'When'], rows: [
          ['`ClusterNotReady`', 'Warning', '`Pending`: the cluster is not `active`, or is rebalancing'],
          ['`OperationQueued`', 'Normal', '`Pending`: another operation holds the node\'s lock'],
          ['`OperationStarted`', 'Normal', '`Preparing` begins'],
          ['`AbortRefused`', 'Warning', 'The abort arrived at `Relocating` or later'],
          ['`OperationAborted`', 'Normal', 'The abort was honored'],
          ['`StepDeadlineExceeded`', 'Warning', 'A step ran out of time'],
          ['`OperationSucceeded`, `OperationFailed`', 'Normal, Warning', 'The operation ended'],
        ] } },
        'Every event also appears on the StorageNode, so `kubectl describe sn` shows the node\'s whole history. `kubectl get snops -o wide` adds a `Message` column, such as `the Migrate completed on node production-worker-3-0`.',
      ],
      examples: [
        { title: 'Calling the first migration off', file: 'examples/node-host-migration/04-storagenodeops-migrate-abort.yaml', notes: [
          { at: 'name: move-production-worker-3-0', text: 'The same object as the first example, edited in place.' },
          { at: 'abort: true', text: 'Honored in `Preparing` only.' },
          { at: 'targetWorkerNode: worker-5', text: 'Unchanged. You cannot change the target.' },
        ] },
      ],
    },
    {
      heading: 'A same-host reboot is a maintenance window',
      body: [
        'When the host comes back with the same drives, nothing has to move. Cordon and drain the worker. The operator then creates a `HostMaintenance` operation named `<node>-maintenance`.',
        { code: '$ kubectl cordon worker-3\nnode/worker-3 cordoned\n\n$ kubectl get snops -n simplyblock\nNAME                                NODE                    ACTION            PHASE     STEP           AGE\nproduction-worker-3-0-maintenance   production-worker-3-0   HostMaintenance   Running   ShuttingDown   20s', title: 'A cordon starts a maintenance window' },
        { table: { head: ['Step', 'Budget', 'Abort?', 'Done when'], rows: [
          ['`Holding`', '6 h', 'yes', 'A maintenance slot is free'],
          ['`ShuttingDown`', '15 min', 'no', 'The node is offline'],
          ['`Releasing`', '15 min', 'no', 'The storage pod has left the worker'],
          ['`AwaitingHost`', '4 h', 'no', 'The worker is schedulable and its API answers'],
          ['`Restarting`', '45 min', 'no', 'The node is `online`'],
          ['`Cleanup`', '5 min', 'no', 'The disruption budget is gone'],
        ] } },
        'When you uncordon, the operator calls off a window still in `Holding` and deletes a finished one. To start a window by hand, give it any other name than `<node>-maintenance`.',
      ],
      examples: [
        { title: 'A maintenance window written by hand', file: 'examples/node-host-migration/05-storagenodeops-hostmaintenance.yaml', notes: [
          { at: 'name: production-worker-3-0-firmware', text: 'Not `<node>-maintenance`, which the operator calls off on a schedulable worker.' },
          { at: 'action: HostMaintenance', text: 'The node returns to the same worker.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { code: '$ kubectl get snops move-production-worker-3-0 -n simplyblock -o wide\nNAME                        NODE                    ACTION    PHASE     STEP   MESSAGE\nmove-production-worker-3-0  production-worker-3-0   Migrate   Pending          cluster production is rebalancing; the operation resumes when it settles', title: 'A migration held by the cluster gate' },
        { pitfalls: [
          { symptom: '`Pending` with `ClusterNotReady`.', cause: 'The cluster is rebalancing, often after the previous migration.', fix: 'Wait. The operation starts on its own.' },
          { symptom: '`Pending` with `OperationQueued` naming `<node>-maintenance`.', cause: 'You cordoned the source first.', fix: 'Uncordon the worker. The window ends, and the migration takes the lock.' },
          { symptom: 'Fails in `Preparing` with `target worker worker-5 is not Ready`.', cause: 'The target is not `Ready`, or the name is wrong.', fix: 'Fix the worker, then create a new StorageNodeOps.' },
          { symptom: '`Preparing` fails with `StepDeadlineExceeded`.', cause: 'The target\'s pod was not `Ready` and in DNS within 15 minutes.', fix: 'Check the pod on the target, then create a new StorageNodeOps.' },
          { symptom: '`AwaitingNode` fails, and the node is offline.', cause: 'The restart failed, for example because the target has none of the node\'s drives.', fix: 'simplyblock does not document a recovery for this case. Contact simplyblock support before you act on the node.' },
          { symptom: '`Relocating` fails after 15 minutes, node still `online`.', cause: '`force: false`, or the restart was too quick to be seen.', fix: 'Check `kubectl get sn`. Then create a new `Migrate` to the same target, with `force` unset.' },
          { symptom: '`Promoting` repeats with a 409 that says the promote did not finish.', cause: 'A volume migration runs in the cluster, so failing a drive is refused.', fix: 'Let it finish. If 30 minutes pass, create a new `Migrate` to the same target.' },
          { symptom: 'A `Migrate` to the node\'s current worker restarts the node.', cause: 'The restart still runs when the target is the current worker.', fix: 'Do not run a finished migration again. Use `Restart` for a restart.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Choosing the tool', cat: 'concept', text: 'Which of these does a node keep through a `Migrate`?', options: ['Its backend UUID', 'Its logical volumes', 'Its slot', 'Its `spec.workerNode`', 'The physical drives it used on the source host, on any platform'], correct: [0, 1, 2], explain: 'Only the machine changes. `spec.workerNode` is pointed at the target at the promote, and outside AWS EBS the target\'s drives replace the old host\'s.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'A host is retired and an identical machine replaces it. Which operation moves the node and keeps its identity?', options: ['StorageNodeOps `Migrate`', 'StorageNodeOps `Remove`, then a growth ClusterDeploymentConfig', 'StorageNodeOps `HostMaintenance`', 'StorageDeviceOps `Migrate` for each drive', 'An edit of `spec.workerNode` on the StorageNode'], correct: [0], explain: 'Remove and add gives a new UUID, a maintenance window brings the node back to the same host, StorageDeviceOps `Migrate` is refused at admission, and the webhook refuses the edit.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'A worker is rebooted for a kernel upgrade and comes back with its drives. What is the right tool?', options: ['Cordon and drain it, and let the operator raise `HostMaintenance`', '`Migrate` the node to another worker and back', '`Remove` the node and add it again', 'Edit `spec.workerNode` to a placeholder during the reboot', 'Delete the StorageNode and let it be created again'], correct: [0], explain: 'A same-host reboot is a maintenance window: nothing has to move. Deleting the StorageNode raises a `Remove`, which ends the node.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'Which of these are true of removing a node and adding another instead of migrating it?', options: ['The new node gets a new backend UUID', 'The removal moves the volumes off with PersistentVolumeOps', 'The removed StorageNode ends in phase `Removed`', 'It is the right tool when capacity is not replaced one for one', 'It keeps the slot label on the old worker'], correct: [0, 1, 2, 3], explain: 'A removal drains the node and ends it, and an add creates a new node with a new identity. It suits a shrink or a later growth rather than a one-for-one replacement.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'Which of these does a migration do to the Kubernetes objects of the node\'s volumes?', options: ['Creates a PersistentVolumeOps per volume', 'Rewrites each PersistentVolume\'s node affinity', 'Deletes and recreates the claims', 'Labels each claim with `storage.simplyblock.io/drain-node`', 'Pins each claim to the target with `storage.simplyblock.io/selected-storage-node`'], correct: [], explain: 'None of them. The volumes stay on the same storage node, so nothing in Kubernetes about them changes. Volume moves belong to a removal.' },

    { topic: 'Drives and data', cat: 'concept', text: 'On a cluster of servers with local NVMe drives, what happens to the data that was on the old host\'s drives?', options: ['It is rebuilt from the cluster\'s parity after the promote fails those drives', 'It is copied off the old host before the restart', 'The drives are carried to the new host and attached there', 'A PersistentVolumeOps moves each volume off them', 'It stays on the old host and is read over the network for good'], correct: [0], explain: 'The restart marks the drives the target lacks as removed, and the promote fails them, which starts a rebuild from parity onto the cluster\'s drives.' },
    { topic: 'Drives and data', cat: 'concept', text: 'Which of these does the promote do?', options: ['Activates the target\'s new drives and adds them to the cluster map', 'Erases the target\'s new drives as it initializes them', 'Fails the node\'s drives that the restart did not find on the target', 'Records the target as the host of the node and its volumes', 'Copies every volume to another node first'], correct: [0, 1, 2, 3], explain: 'The promote turns new devices into cluster devices, fails the removed ones, which starts the rebuild, and re-points the node. No volume is copied.' },
    { topic: 'Drives and data', cat: 'action', text: 'A node runs on EC2 with EBS volumes as its drives, and it moves to a new instance. What does `spec.reattachVolume: true` do?', options: ['Detaches the tagged EBS volumes from the old instance and attaches them to the new one', 'Lets the restart find the node\'s own drives, so nothing has to be rebuilt', 'Re-attaches the PersistentVolumes to their pods', 'Moves every logical volume to another storage node', 'Restarts the consuming pods on the new instance'], correct: [0, 1], explain: 'The control plane moves the EBS volumes tagged `simplyblock-storage` or `simplyblock-jm`. The restart finds them by serial number, so no device is new or removed.' },
    { topic: 'Drives and data', cat: 'concept', text: 'A migration reads `Succeeded`. Which of these can still be true?', options: ['The cluster is `Rebalancing` while the old drives\' data is rebuilt', 'A migration of another node waits with `ClusterNotReady`', 'The StorageNode still names its old worker', 'The node lock is still held by the migration', 'The node\'s UUID has changed'], correct: [0, 1], explain: 'The step ends at the re-point and the release, not at the end of the rebuild. The re-point and the lock release happen before `Succeeded`, and the UUID never changes.' },
    { topic: 'Drives and data', cat: 'action', text: '`Promoting` repeats with a 409 that says the promote did not finish. What helps?', options: ['Letting running volume migrations in the cluster finish', 'Waiting, since the operator calls the promote again about once a minute', 'Deleting the StorageNodeOps', 'Setting `spec.abort: true`', 'Editing `spec.workerNode` to the target'], correct: [0, 1], explain: 'Failing the old drives is refused while any volume migration runs. The delete and the abort are refused in `Promoting`, and the webhook refuses the edit.' },

    { topic: 'The target worker', cat: 'field', text: 'Which conditions on the target does `Preparing` check, failing the operation when they do not hold?', options: ['It is a node of this Kubernetes cluster', 'It is `Ready`', 'It runs no other node of the same cluster', 'Its drives sit at the source\'s PCI addresses', 'It is not cordoned'], correct: [0, 1], explain: 'The step refuses a worker that does not exist or is not `Ready`. The other three are worth checking yourself.' },
    { topic: 'The target worker', cat: 'concept', text: 'The target\'s drives sit at PCI addresses the source never had. What makes the target bind them?', options: ['Listing them in `spec.migrate.newSsdPcie`', 'Editing `spec.config.deviceNames` on the StorageNode', 'Nothing, the target binds every NVMe device it finds', 'A StorageDeviceOps `Migrate` per drive', 'A new discovery run against the target'], correct: [0], explain: 'The target boots from a copy of the source\'s configuration, whose allow list holds the source\'s addresses. `newSsdPcie` adds to it. `deviceNames` is immutable.' },
    { topic: 'The target worker', cat: 'action', text: 'An administrator cordons the retiring worker first, then creates the `Migrate`. What follows?', options: ['The operator raises a `HostMaintenance` named `<node>-maintenance`', 'The migration waits with `OperationQueued` while the window holds the lock', 'The migration skips the lock because a user created it', 'The window turns into the migration', 'The cordon is ignored for a node with a migration pending'], correct: [0, 1], explain: 'A cordon raises a maintenance window, and the window takes the lock. Migrate first and cordon afterward.' },
    { topic: 'The target worker', cat: 'concept', text: 'Which of these does the operator check before it migrates a node onto a worker?', options: ['That the worker has as many CPU cores as the source', 'That the worker\'s NIC names match the source\'s', 'That the worker has enough huge pages', 'That the worker runs no FoundationDB process', 'That the worker\'s drives are empty'], correct: [], explain: 'None of them. `Preparing` checks that the worker exists and is `Ready`, then waits for its storage-node pod and its DNS name. The promote erases the target\'s drives, so use dedicated ones.' },

    { topic: 'The operation', cat: 'field', text: 'Which fields are required for a `Migrate` StorageNodeOps to run?', options: ['`spec.nodeRef`', '`spec.action`', '`spec.migrate.targetWorkerNode`', '`spec.migrate.newSsdPcie`', '`spec.force`'], correct: [0, 1, 2], explain: 'The node, the action, and the target. The drive list is optional, and `force` defaults to true for a migration.' },
    { topic: 'The operation', cat: 'field', text: 'Which fields does the API server refuse to change after a StorageNodeOps exists?', options: ['`spec.nodeRef`', '`spec.action`', '`spec.migrate.targetWorkerNode`', '`spec.abort`', '`spec.reattachVolume`'], correct: [0, 1, 2], explain: 'Those three are immutable. `spec.abort` is meant to change, and `reattachVolume`, `force`, and `newSsdPcie` are not enforced, though you should treat them as fixed.' },
    { topic: 'The operation', cat: 'field', text: 'What does `spec.force` do on a `Migrate`?', options: ['It defaults to true when unset', 'Set to false, it makes the control plane refuse the restart of an online node', 'It skips the cluster gate', 'It lets an abort be honored after `Preparing`', 'It removes the node instead of moving it'], correct: [0, 1], explain: 'The node is online when it is moved, and the control plane refuses an unforced restart of a node that is not offline.' },
    { topic: 'The operation', cat: 'field', text: 'Where do the `newSsdPcie` addresses go?', options: ['Into the target\'s copied per-node configuration, before the target is labeled', 'Into the relocation restart', 'Into the StorageNode\'s `spec.config.pcieAllowList` at the promote', 'Into the StorageNode\'s `spec.config.deviceNames`', 'Into a StorageDevice object for each drive'], correct: [0, 1, 2], explain: 'They reach the host through the configuration, the control plane through the restart, and the StorageNode through the allow list, where a later rebuild of the node finds them.' },
    { topic: 'The operation', cat: 'action', text: 'A migration was created with the wrong target and is still `Pending`. What is the fix?', options: ['Delete it, or set `spec.abort`, and create a new StorageNodeOps with the right target', 'Edit `spec.migrate.targetWorkerNode`', 'Edit the StorageNode\'s `spec.workerNode`', 'Patch the status step to `Preparing`', 'Wait for the operator to choose the nearest `Ready` worker'], correct: [0], explain: 'The target is immutable, and a `Pending` operation has done nothing, so it can be deleted or aborted and written again.' },
    { topic: 'The operation', cat: 'action', text: 'On which clusters should `newSsdPcie` and `reattachVolume` stay unset?', options: ['A `LogicalBlock` cluster, for both fields', 'A cluster outside AWS, for `reattachVolume`', 'Any cluster whose target has the same hardware as the source, for `newSsdPcie`', 'Every NVMe cluster, for `newSsdPcie`', 'An AWS cluster whose nodes use EBS drives, for `reattachVolume`'], correct: [0, 1, 2], explain: 'The control plane does not support either field on logical block devices, `reattachVolume` only moves EBS volumes, and identical hardware needs no extra addresses.' },

    { topic: 'Gate, lock, and steps', cat: 'concept', text: 'Which of these hold a new `Migrate` in `Pending`?', options: ['The cluster is not `active` in the control plane', 'The cluster is rebalancing', 'Another operation holds the node\'s lock', 'The target worker is not `Ready`', 'The cluster turns degraded because of the restart, after admission'], correct: [0, 1, 2], explain: 'The gate and the lock hold an operation in `Pending`. A target that is not `Ready` fails it in `Preparing`, and the gate is not checked again once the operation runs.' },
    { topic: 'Gate, lock, and steps', cat: 'field', text: 'Where is the node lock kept?', options: ['`StorageNode.status.activeOpsRef`', 'A Lease in the operator\'s namespace', 'An annotation on the StorageNodeOps', '`StorageCluster.status.activeOpsRef`', 'A label on the worker'], correct: [0], explain: 'The operation writes its name into the node\'s status, and clears it when it reaches a final phase.' },
    { topic: 'Gate, lock, and steps', cat: 'concept', text: 'In which steps of a `Migrate` is `spec.abort` honored?', options: ['`Preparing`', '`Relocating`', '`AwaitingNode`', '`Promoting`', '`Holding`'], correct: [0], explain: 'Only `Preparing`, before anything is asked of the control plane. `Holding` belongs to `HostMaintenance`.' },
    { topic: 'Gate, lock, and steps', cat: 'concept', text: 'Which pairings of a migration step and its budget are right?', options: ['`Preparing`, 15 minutes', '`Relocating`, 15 minutes', '`AwaitingNode`, 45 minutes', '`Promoting`, 30 minutes', 'The whole operation, 24 hours'], correct: [0, 1, 2, 3], explain: 'The four budgets add up to 105 minutes. There is no separate budget for the whole operation.' },
    { topic: 'Gate, lock, and steps', cat: 'concept', text: 'Why are `Relocating` and `AwaitingNode` two steps?', options: ['The restart runs in the background, and the node can still report `online` from before it', 'A promote issued during the restart leaves the new devices unactivated', 'The control plane needs two calls for one restart', 'The first step drains the volumes and the second restarts the node', 'They run on two different workers'], correct: [0, 1], explain: 'The first waits for the node to leave `online` and the second for it to come back, so the promote is not issued into an unfinished restart.' },
    { topic: 'Gate, lock, and steps', cat: 'action', text: 'An abort arrives while the migration is in `AwaitingNode`. What happens?', options: ['An `AbortRefused` event', 'The operation runs on', 'The phase becomes `Aborted`', 'The node is restarted back on the source', 'A delete of the operation is refused as well'], correct: [0, 1, 4], explain: 'The node is in the middle of a restart that only this operation watches, so neither the abort nor the delete is honored.' },
    { topic: 'Gate, lock, and steps', cat: 'action', text: 'A migration is aborted in `Preparing`. Which of these are true afterward?', options: ['The phase is `Aborted`', 'Nothing was asked of the control plane', 'The target worker keeps its storage-plane label', 'The target\'s copied configuration entry is removed', 'The node is restarted on its source worker'], correct: [0, 1, 2], explain: 'The abort ends the operation without undoing the step. The label and the entry stay, and the node was never restarted.' },

    { topic: 'Promoting', cat: 'concept', text: 'What does the operator do in `Promoting`, after the promote succeeds?', options: ['Sets `spec.workerNode` to the target', 'Appends `newSsdPcie` to `spec.config.pcieAllowList`', 'Gives the target worker the node\'s slot label', 'Removes the source worker\'s storage-plane labels if no other node of the cluster is there', 'Waits for the rebuild to finish'], correct: [0, 1, 2, 3], explain: 'The re-point and the release are the Kubernetes half of the move. The step does not wait for the rebuild.' },
    { topic: 'Promoting', cat: 'field', text: 'Who may change `spec.workerNode` on a StorageNode?', options: ['The operator', 'A cluster administrator with `kubectl edit`', 'Anyone, since the field is not immutable', 'The CSI driver', 'Nobody, not even the operator'], correct: [0], explain: 'A migration re-points the field, so it is not immutable, and the webhook refuses the change from anyone but the operator.' },
    { topic: 'Promoting', cat: 'concept', text: 'The source worker runs a second storage node of the same cluster on its other socket. What happens to the worker\'s labels when the first node is migrated?', options: ['They stay, because a node of the cluster remains there', 'They are removed, and the second node\'s pod is unscheduled', 'The second node is migrated as well', 'The migration is refused', 'They move to the target'], correct: [0], explain: 'The release removes labels only from workers that no node of the cluster occupies, which keeps the other socket\'s node scheduled.' },

    { topic: 'Maintenance window', cat: 'concept', text: 'Which of these are steps of `HostMaintenance` and not of `Migrate`?', options: ['`Holding`', '`Releasing`', '`AwaitingHost`', '`Cleanup`', '`ShuttingDown`'], correct: [0, 1, 2, 3, 4], explain: 'All five. A migration runs `Preparing`, `Relocating`, `AwaitingNode`, and `Promoting`.' },
    { topic: 'Maintenance window', cat: 'action', text: 'Why should a hand-written `HostMaintenance` not be named `<node>-maintenance`?', options: ['The operator calls off a window of that name while the worker is schedulable and the window is still in `Holding`', 'The webhook reserves the name', 'The name is longer than 63 characters', 'The operator deletes a running object of that name on sight', 'The name makes the window skip the cluster gate'], correct: [0], explain: 'That is the name of the window a cordon raises, and a schedulable worker is the signal to call it off. A finished window of that name is deleted.' },
    { topic: 'Maintenance window', cat: 'concept', text: 'Which of these does a maintenance window share with a migration?', options: ['It waits at the cluster gate', 'It takes the node\'s lock', 'It can be called off in its first step only', 'It re-points `spec.workerNode`', 'It brings the node up with a control-plane restart'], correct: [0, 1, 2, 4], explain: 'Both move data, so both wait at the gate, both lock the node, and both end with the same restart. Only a migration re-points the node.' },
  ],

  tasks: [
    {
      title: 'Retiring a worker',
      scenario: 'Worker `worker-3` is being retired. An identical machine, `worker-5`, has joined the Kubernetes cluster, is `Ready`, and runs no storage node yet. Write the operation that moves the storage node `production-worker-3-0` onto it and keeps the node\'s UUID and volumes.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: retire-worker-3\n  namespace: simplyblock\nspec:\n  nodeRef: {{node}}\n  action: {{action}}\n  migrate:\n    targetWorkerNode: {{target}}\n',
      blanks: {
        kind: { accept: ['StorageNodeOps'], hint: 'the node-level operation kind, short name `snops`' },
        node: { accept: ['production-worker-3-0'], hint: 'the StorageNode by its object name' },
        action: { accept: ['Migrate'], hint: 'the action that keeps the node\'s identity' },
        target: { accept: ['worker-5'], hint: 'the Kubernetes worker the node moves to' },
      },
      explain: 'A `StorageNodeOps` with `action: Migrate` names the node and the target. Identical hardware needs no `newSsdPcie`, and `force` stays at its default.',
    },
    {
      title: 'A target with drives at new addresses',
      scenario: 'The replacement machine `worker-7` carries its drives at `0000:88:00.0` and `0000:89:00.0`, addresses the source never had. The cluster is an NVMe cluster on bare metal. Move `production-worker-4-0` onto it and bind both drives. Leave the force flag at its default.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: StorageNodeOps\nmetadata:\n  name: move-production-worker-4-0\n  namespace: simplyblock\nspec:\n  nodeRef: {{node}}\n  action: Migrate\n  migrate:\n    targetWorkerNode: {{target}}\n    {{field}}:\n      - "0000:88:00.0"\n      - {{second}}\n',
      blanks: {
        node: { accept: ['production-worker-4-0'], hint: 'the StorageNode to move' },
        target: { accept: ['worker-7'], hint: 'the replacement machine' },
        field: { accept: ['newSsdPcie'], hint: 'the list of PCI addresses to bind on the target' },
        second: { accept: ['/"?0000:89:00\\.0"?/', '/\'0000:89:00\\.0\'/'], hint: 'the second drive\'s PCI address' },
      },
      explain: '`newSsdPcie` adds the target\'s drives to the configuration it boots from, to the restart, and to the node\'s allow list at the promote. `reattachVolume` stays unset because there are no EBS volumes to move, and `force` stays unset so the restart of the online node is forced.',
    },
    {
      title: 'A firmware update on the same host',
      scenario: 'Worker `worker-3` needs a firmware update and comes back with the same drives. You start the window by hand for the node `production-worker-3-0` instead of cordoning the worker. Write the operation, with a name the operator will not call off.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: StorageNodeOps\nmetadata:\n  name: {{name}}\n  namespace: simplyblock\nspec:\n  nodeRef: {{node}}\n  action: {{action}}\n',
      blanks: {
        name: { accept: ['/(?!production-worker-3-0-maintenance$)[a-z0-9]([-a-z0-9]*[a-z0-9])?/'], hint: 'any valid name except the one the operator gives the window a cordon raises' },
        node: { accept: ['production-worker-3-0'], hint: 'the StorageNode on that worker' },
        action: { accept: ['HostMaintenance'], hint: 'the node returns to the host it left' },
      },
      explain: 'A same-host reboot is a maintenance window, not a migration. The name `<node>-maintenance` belongs to the window the operator raises, and the operator calls that one off while the worker is schedulable and the window is still in `Holding`.',
    },
  ],
});
