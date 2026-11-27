// Chapter 11: removing a storage node from a running cluster. It teaches the
// one operation that does it, a StorageNodeOps with action Remove: the two ways
// to start it, the checks the operator and the control plane make, the steps
// after the point of no return, how to follow and clean up a removal, and what
// goes wrong. Failing one drive with StorageDeviceOps Fail closes the chapter.
TRAINING.register({
  id: 'node-removal',
  title: 'Removing a storage node',
  summary: 'Taking one storage node out of a running cluster with a StorageNodeOps `Remove`: the checks that run first, the drain that rebuilds the node\'s data and moves its volumes, the removal, and the cleanup. Failing one drive with StorageDeviceOps `Fail` closes the chapter.',
  goal: 'You can start a node removal, read why it holds or fails, follow it to `Removed`, clean up in the right order, and fail a single drive instead when that is enough.',

  sections: [
    {
      heading: 'What a removal does',
      body: [
        'You remove a storage node when its worker failed for good, when you retire hardware, or when you shrink the cluster.',
        'A removal moves everything off the node first. Only an empty node leaves the cluster.',
        'One StorageNodeOps with `action: Remove` does all of it. There is no separate `Drain` action.',
        { table: { head: ['Part of the removal', 'Done by'], rows: [
          ['Sort the node\'s volumes and check the zone balance', 'operator'],
          ['Shut the node down. Each volume fails over to its standby node', 'control plane, asked by the operator'],
          ['Check that the cluster can lose the node', 'control plane'],
          ['Rebuild the node\'s data from parity onto other drives', 'control plane'],
          ['Move the volumes, one PersistentVolumeOps per NVMe-oF subsystem', 'operator, with the control plane copying data'],
          ['Confirm the node is empty', 'operator and control plane'],
          ['Take the node apart', 'control plane, asked by the operator'],
        ] } },
        { callout: 'From the shutdown on, there is no way back. Only the first step, `Validating`, can be aborted. A `Removed` node never comes back.', kind: 'warn' },
        'To replace a node, add the new one first with a growth document, as [Expanding a cluster](chapter.html?c=cluster-expansion) shows. Then remove the old one.',
        { table: { head: ['Order', 'Nodes during the work', 'Risk'], rows: [
          ['Add first, then remove', '6, then 7, then 6', 'Capacity and redundancy stay full'],
          ['Remove first, then add', '6, then 5, then 6', 'Less capacity, and a smaller cluster to fail into'],
        ] } },
        'Other jobs use other operations. Fail one drive with StorageDeviceOps `Fail`, at the end of this chapter. Move a node to another worker with `Migrate`, in [the host-migration chapter](chapter.html?c=node-host-migration).',
      ],
    },
    {
      heading: 'Starting a removal: an operation or a deletion',
      body: [
        'You can start a removal in two ways.',
        { list: [
          '**Write a StorageNodeOps** with `spec.nodeRef` and `action: Remove`. It stays as a record after it finishes.',
          '**Delete the StorageNode.** Its finalizer creates a StorageNodeOps named `<node>-remove`. The object stays until that removal reaches `Succeeded`.',
        ] },
        'A StorageNode without a backend UUID goes at once, without a removal. So does one whose StorageCluster is gone.',
        { code: '$ kubectl apply -f 01-storagenodeops-remove.yaml\nstoragenodeops.storage.simplyblock.io/production-worker-5-0-remove created\n\n$ kubectl get snops -n simplyblock\nNAME                           NODE                    ACTION   PHASE     STEP         AGE\nproduction-worker-5-0-remove   production-worker-5-0   Remove   Running   Validating   10s', title: 'Starting a removal' },
        'If a removal from a deletion ends `Failed` or `Aborted`, the StorageNode stays. To retry, delete the finished `<node>-remove` record.',
        { code: '$ kubectl delete snops production-worker-5-0-remove -n simplyblock\n$ kubectl get snops -n simplyblock\nNAME                           NODE                    ACTION   PHASE     STEP         AGE\nproduction-worker-5-0-remove   production-worker-5-0   Remove   Running   Validating   3s', title: 'Retrying a removal raised by a deletion' },
        'A removal runs on a degraded or rebalancing cluster. Removing a node can be what makes the cluster healthy again.',
        'A removal takes the node\'s lock. A second operation on the same node waits in `Pending` with `OperationQueued`.',
      ],
      examples: [
        { title: 'A removal written by hand', file: 'examples/node-removal/01-storagenodeops-remove.yaml', notes: [
          { at: 'name: production-worker-5-0-remove', text: 'The name the finalizer uses. A later delete of the StorageNode finds this record and raises no second removal.' },
          { at: 'nodeRef: production-worker-5-0', text: 'The StorageNode, by name. It cannot change.' },
          { at: 'action: Remove', text: 'Drain, then remove.' },
        ] },
        { title: 'The same removal with its own system-volume pattern', file: 'examples/node-removal/02-storagenodeops-remove-regex.yaml', notes: [
          { at: '  remove:', text: 'The settings of `Remove`. Matching volumes are deleted in `Verifying`, not moved.' },
          { at: 'sb-fio-baseline-|perf-baseline-', text: 'The pattern replaces the default, so it repeats `sb-fio-baseline-`. A pattern that does not compile fails the operation.' },
        ] },
      ],
    },
    {
      heading: 'Validating: what the operator checks before anything changes',
      body: [
        '`Validating` changes nothing. Its budget is 24 hours.',
        'First, it sorts each volume on the node into one of four groups.',
        { table: { head: ['Group', 'What puts a volume in it', 'What happens'], rows: [
          ['System', 'its name matches `systemVolumeFilterRegex`', 'deleted in `Verifying`'],
          ['Unmanaged', 'no PersistentVolume names it', 'the step holds with `DrainBlocked` until you remove it'],
          ['Pinned', 'its claim has `storage.simplyblock.io/selected-storage-node`', 'the operation fails at once'],
          ['Managed', 'a PersistentVolume names it, with no pin', 'moved in `MigratingVolumes`'],
        ] } },
        { code: 'node production-worker-5-0 holds 1 pinned volume (pvc-8e2f61d4-0b7a-4c3e-9f52-a1d6e7c40b93), and a pinned\nvolume cannot be moved; remove or change the storage.simplyblock.io/selected-storage-node annotation\non their claims, then remove the node again', title: 'status.message of a removal failed by a pin' },
        { callout: 'The pattern is checked first. A real volume whose name matches it is deleted as a system volume. Keep the pattern narrow and anchored with `^`.', kind: 'warn' },
        'Then, on a cluster with failure domains, the operator checks the zone balance. It counts hosts per zone, after the node is gone.',
        { list: [
          'The largest and smallest zone may differ by at most 1 host.',
          'Every zone must keep at least 2 hosts.',
          'A worker with another storage node of the cluster still counts as a host.',
        ] },
        { table: { head: ['Hosts per zone', 'Remove from', 'After', 'Answer'], rows: [
          ['3, 3, 3', 'zone 0', '2, 3, 3', 'admitted'],
          ['2, 3, 3', 'zone 0', '1, 3, 3', 'refused'],
          ['2, 3, 3', 'zone 1', '2, 2, 3', 'admitted'],
          ['2, 2, 2', 'any', '1, 2, 2', 'refused'],
          ['4, 4, 3', 'zone 2', '4, 4, 2', 'refused'],
        ] } },
        { code: '$ kubectl get snops production-worker-5-0-remove -n simplyblock -o wide\nNAME                           NODE                    ACTION   PHASE     STEP         MESSAGE\nproduction-worker-5-0-remove   production-worker-5-0   Remove   Running   Validating   the removal of node production-worker-5-0\nis not admitted: failure domain 0 would drop to 1 host(s); at least 2 are required per domain; nothing has been done', title: 'A removal held with RemovalNotAdmitted' },
        'The check repeats on every pass. If you add a host to the short zone, the removal continues by itself.',
        'If 24 hours pass first, the operation fails with `StepDeadlineExceeded`. The node is untouched.',
        'Once the removal is allowed, the operator writes the number of managed volumes into `status.drain.volumesTotal`.',
      ],
    },
    {
      heading: 'What the control plane checks, and when',
      body: [
        'The control plane checks the removal twice: at `prepare-removal` in `MigratingDevices`, and at the node DELETE in `Removing`.',
        'By then the node is shut down. A refusal leaves it offline.',
        { table: { head: ['Check', 'Example'], rows: [
          ['The node\'s status allows removal', '`online`, `suspended`, `down`, `offline`, or `unreachable`'],
          ['The cluster keeps its fault tolerance', 'With one parity chunk, no other node may be offline'],
          ['The zones stay balanced', 'The same rule the operator checked'],
          ['Standby copies on the node can move', 'Each needs a host apart from the copies it protects'],
          ['No task runs on the node', 'A running restart task refuses the removal'],
          ['No snapshot is left, at the DELETE', 'Volumes and their snapshots must have moved'],
          ['Synchronous replication: no volume on the node', 'Volumes cannot be moved on such a cluster'],
        ] } },
        'You can remove a node that is already `offline`. The rebuild reads its data from parity on the other nodes.',
        { table: { head: ['Parity chunks', 'Other nodes offline', 'Removal'], rows: [
          ['1', '0', 'accepted'],
          ['1', '1', 'deferred until it is back'],
          ['2', '1', 'accepted'],
          ['2', '2', 'deferred'],
        ] } },
        'Some refusals pass by themselves: a rebalance, a task on the node, or a peer that is down. The step holds with `RemovalDeferred` and asks again.',
        { code: 'Warning  RemovalDeferred  storagenodeops/production-worker-5-0-remove  the control plane deferred the removal of node\nproduction-worker-5-0: Can not remove node 5b1e7c42-...: FTT=1 (npcs=1): cannot remove node, cluster already has\n1 not-online node(s); it is asked again', title: 'A deferred removal' },
        'Any other refusal fails the operation. A StorageNodeOps `Restart` brings the node back.',
      ],
    },
    {
      heading: 'The steps after the point of no return',
      body: [
        'Each step has a budget. A step that runs out fails the operation with `StepDeadlineExceeded`.',
        { table: { head: ['Step', 'Budget', 'What happens'], rows: [
          ['`ShuttingDown`', '15 minutes', 'Graceful shutdown. A 409 holds with `RemovalDeferred`'],
          ['`MigratingDevices`', '7 hours without progress', 'The control plane rebuilds the node\'s data onto other drives'],
          ['`MigratingVolumes`', '12 hours', 'The operator moves each NVMe-oF subsystem to a peer'],
          ['`Verifying`', '30 minutes', 'System volumes are deleted, and `verify-drained` must say empty'],
          ['`Removing`', '2 hours', 'The operator sends the node DELETE'],
          ['`AwaitingRemoval`', '7 hours without progress', 'The control plane takes the node apart'],
        ] } },
        '"Without progress" means each status change of the node or a drive resets the budget.',
        '**MigratingDevices.** The control plane marks each data drive removed, then failed. Failing a drive starts a rebuild on every other node. The drive\'s data is recreated from parity onto the other drives.',
        { code: '$ kubectl get snops production-worker-5-0-remove -n simplyblock -o jsonpath=\'{.status.removal}\'\n{"nodeStatus":"migrating_devices","devices":{"production-worker-5-0-3f9c2a1b":"failed_and_migrated",\n"production-worker-5-0-a03b9e44":"failed"},"lastProgressTime":"2026-11-25T10:42:17Z"}', title: 'One drive rebuilt, one still rebuilding' },
        'A drive shows `failed_and_migrated` when its data is rebuilt. When all are done, the node moves to `migrating_lvols`.',
        'The node\'s statuses run in this order: `pending_removal`, `migrating_devices`, `migrating_lvols`, `in_removal`, `removed`.',
        '**MigratingVolumes.** The operator creates one PersistentVolumeOps per subsystem. The cluster runs one data migration at a time, so the moves run one after another.',
        { code: '$ kubectl get pvops -l storage.simplyblock.io/drain-node=5b1e7c42-9d3a-4f08-a6e2-1c7d9b0f4e21\nNAME                                   VOLUME           ACTION    TARGET                  PHASE     STEP\ndrain-5b1e7c42-...-pvc-8e2f61d4-...    pvc-8e2f61d4-... Migrate   production-worker-2-0   Running   Migrating\ndrain-5b1e7c42-...-pvc-1a77c0e2-...    pvc-1a77c0e2-... Migrate   production-worker-3-0   Pending\n\n$ kubectl get snops production-worker-5-0-remove -n simplyblock -o jsonpath=\'{.status.drain}\'\n{"volumesTotal":12,"volumesMigrated":7}', title: 'Following the volume moves: 7 of 12 done' },
        'A finished move is deleted. A failed move stays, and a new move to another peer is created with `MigrationRetried`.',
        'The operator picks each target by these rules:',
        { list: [
          'only online peers, and on a stretched cluster only peers of the same site',
          'never a peer that holds a copy of a volume in the subsystem',
          'never a peer that already failed this subsystem',
          'peers whose standby is the drained node only as a last choice',
          'round-robin over the rest',
        ] },
        'With no eligible peer, the step holds with `NoMigrationTarget`. It continues when a node comes back or is added.',
        '**Verifying.** The operator\'s volume count does not see snapshots. So the step waits for `verify-drained` to report no volume and no snapshot.',
        '**AwaitingRemoval.** The control plane removes the standby copies of the node\'s store and its journal copies on other nodes. It moves the standby roles the node held to other hosts. Then it marks the node `removed`.',
        'If the control plane gives up, it reports `removed_failed`, and the operation fails. Fix the cause and start a new removal.',
      ],
      examples: [
        { title: 'One of the moves the drain raises', file: 'examples/node-removal/03-persistentvolumeops-drain.yaml', notes: [
          { at: 'name: drain-', text: '`drain-`, the drained node\'s UUID, and the PersistentVolume name.' },
          { at: 'storage.simplyblock.io/drain-node', text: 'The drained node\'s backend UUID. Select on it to list one drain\'s moves.' },
          { at: 'storage.simplyblock.io/target-node', text: 'The backend UUID of the chosen peer.' },
          { at: 'managed-by: StorageNodeOps', text: 'The kind that created the move.' },
          { at: 'name: production-worker-2-0', text: 'The target StorageNode.' },
          { at: 'creatorRef:', text: 'The removal that created the move.' },
        ] },
      ],
    },
    {
      heading: 'Following a removal: phases, status, and events',
      body: [
        'Three objects show a removal.',
        { table: { head: ['Object', 'Phases during a removal'], rows: [
          ['StorageNodeOps', '`Pending`, `Running`, then `Succeeded`, `Failed`, or `Aborted`'],
          ['StorageNode', '`Offline`, then `Removing`, then `Removed`'],
          ['StorageCluster', '`Shrinking` while a removal runs'],
        ] } },
        { code: '$ kubectl get sn production-worker-5-0 -n simplyblock\nNAME                    CLUSTER      WORKER     SOCKET   SLOT   PHASE      STEP   STATUS              HEALTH   AGE\nproduction-worker-5-0   production   worker-5   0        0      Removing          migrating_lvols     false    94d\n\n$ kubectl get stc production -n simplyblock\nNAME         PHASE       ROLE   STEP   STATUS   EC    UUID       AGE\nproduction   Shrinking                 active   4+2   9a0f...    120d', title: 'The node and the cluster during a removal' },
        '`Removed` is final. To get capacity back, add a node.',
        { table: { head: ['Event', 'Meaning', 'What to do'], rows: [
          ['`OperationQueued`', 'another operation holds the node\'s lock', 'wait, or abort it'],
          ['`DrainBlocked`', 'unmanaged volumes, or something left at `Verifying`', 'remove what the message names'],
          ['`RemovalNotAdmitted`', 'the zone rule refused the removal', 'fix the zones, or abort'],
          ['`RemovalDeferred`', 'a refusal that passes by itself', 'nothing'],
          ['`NoMigrationTarget`', 'no peer can take a subsystem', 'bring a peer back or add one'],
          ['`MigrationRetried`', 'a move failed and is retried elsewhere', 'read the failed target'],
          ['`DrainCompleted`', 'all volumes moved', 'nothing'],
          ['`AbortRefused`', 'abort after `Validating`', 'nothing'],
          ['`StepDeadlineExceeded`', 'a step ran out of time', 'read `status.message`'],
        ] } },
        'Each event also appears on the StorageNode. `kubectl describe sn <node>` shows the history.',
      ],
    },
    {
      heading: 'Aborting, and cleaning up afterward',
      body: [
        '`spec.abort: true` is the only change a StorageNodeOps accepts. It works only in `Validating`.',
        { code: '$ kubectl patch snops production-worker-5-0-remove -n simplyblock --type merge -p \'{"spec":{"abort":true}}\'\n$ kubectl get snops production-worker-5-0-remove -n simplyblock\nNAME                           NODE                    ACTION   PHASE     STEP         AGE\nproduction-worker-5-0-remove   production-worker-5-0   Remove   Aborted   Validating   2h', title: 'Calling off a held removal' },
        'After `Validating`, the abort is refused with `AbortRefused`. You also cannot delete the record until it finishes.',
        'Aborting a removal from a deletion does not cancel the deletion. The StorageNode keeps its finalizer and keeps serving.',
        'A hand-written removal leaves two things behind:',
        { list: [
          'the StorageNode, in phase `Removed`, which still counts toward the 32-node cap',
          'the storage labels on the worker',
        ] },
        { steps: [
          'Wait for `Succeeded` on the StorageNodeOps and `Removed` on the StorageNode.',
          'Delete the StorageNode. If the removal is named `<node>-remove`, the object goes at once.',
          'Then cordon and drain the worker, and remove its storage labels.',
        ] },
        { code: '$ kubectl delete sn production-worker-5-0 -n simplyblock\nstoragenode.storage.simplyblock.io "production-worker-5-0" deleted\n$ kubectl cordon worker-5\n$ kubectl drain worker-5 --ignore-daemonsets --delete-emptydir-data', title: 'Cleaning up in order' },
        { callout: 'Delete the StorageNode before you cordon. A cordon makes the operator create `<node>-maintenance` for every StorageNode on the worker, a `Removed` one too.', kind: 'warn' },
      ],
      examples: [
        { title: 'The removal after the abort', file: 'examples/node-removal/04-storagenodeops-remove-abort.yaml', notes: [
          { at: 'abort: true', text: 'Works in `Validating`. The operation ends `Aborted`, not `Failed`.' },
          { at: 'action: Remove', text: 'Unchanged. Only `abort` can change.' },
        ] },
      ],
    },
    {
      heading: 'Removing one drive instead: StorageDeviceOps Fail',
      body: [
        'A bad drive does not need its node removed. A StorageDeviceOps with `action: Fail` takes one drive out of service.',
        'The other nodes rebuild its data from parity, as in a node removal.',
        { table: { head: ['Step', 'Budget', 'What happens', 'Abortable'], rows: [
          ['`Removing`', '10 minutes', 'the drive leaves the data path', 'yes, until the call is made'],
          ['`Failing`', '2 minutes', 'the drive is failed', 'no'],
          ['`Awaiting`', '5 minutes', 'waits for `failed` or `failed_and_migrated`', 'no'],
        ] } },
        { code: '$ kubectl get sd -n simplyblock -l storage.simplyblock.io/node=production-worker-5-0\n$ kubectl apply -f 05-storagedeviceops-fail.yaml\n$ kubectl get sdops -n simplyblock\nNAME                                  DEVICE                           ACTION   PHASE       STEP\nproduction-worker-5-0-3f9c2a1b-fail   production-worker-5-0-3f9c2a1b   Fail     Succeeded   Completed', title: 'Failing a drive' },
        'A failure cannot be undone. The rebuild continues after the operation ends.',
        'Before you fail a drive, check that other drives have room, and that no other drive or node is down.',
        'The control plane refuses the failure while that drive restarts, or while any volume migration runs.',
        'To check a drive first, use `CheckHealth`. It reads the SMART log into `status.health.smart`.',
        'There is no `Remove`, `Replace`, or `Add` action on a drive. simplyblock does not document how to add a drive to a running node on Kubernetes.',
      ],
      examples: [
        { title: 'Failing one drive', file: 'examples/node-removal/05-storagedeviceops-fail.yaml', notes: [
          { at: 'deviceRef: production-worker-5-0-3f9c2a1b', text: '`<node>-<first eight characters of the device UUID>`.' },
          { at: 'action: Fail', text: 'Abortable only in `Removing`.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The removal fails in `Validating`, naming pinned volumes.', cause: 'Their claims have `storage.simplyblock.io/selected-storage-node`.', fix: 'Remove or change the annotation, then start a new removal.' },
          { symptom: '`Validating` holds with `DrainBlocked`.', cause: 'Volumes on the node have no PersistentVolume.', fix: 'Remove them through the control plane by hand.' },
          { symptom: '`Validating` holds with `RemovalNotAdmitted`.', cause: 'A zone would be short or out of balance.', fix: 'Add a host to the short zone, or remove a node from a larger zone.' },
          { symptom: '`MigratingDevices` holds with `RemovalDeferred`, naming offline nodes.', cause: 'The cluster cannot lose two nodes at once.', fix: 'Bring the other node back. The drained node stays offline meanwhile.' },
          { symptom: 'The removal fails in `MigratingDevices`, and the node is offline.', cause: 'The control plane refused for good, for example no host for a standby copy.', fix: 'Run a StorageNodeOps `Restart` to bring the node back.' },
          { symptom: '`MigratingVolumes` holds with `NoMigrationTarget`.', cause: 'No online peer can take the subsystem.', fix: 'Bring a peer back or add a node.' },
          { symptom: 'A user volume disappeared during a removal.', cause: 'Its name matched `systemVolumeFilterRegex`.', fix: 'Anchor the pattern to benchmark prefixes. A deleted volume cannot be recovered.' },
          { symptom: 'A deleted StorageNode stays for days.', cause: 'Its `<node>-remove` failed or was aborted.', fix: 'Fix the cause, then delete the finished record.' },
          { symptom: 'A `<node>-maintenance` appears for a `Removed` node.', cause: 'You cordoned before deleting the StorageNode.', fix: 'Delete the StorageNode first next time.' },
        ] },
      ],
      examples: [
        { title: 'A claim pinned to a node', file: 'examples/node-removal/06-pvc-pinned.yaml', notes: [
          { at: 'selected-storage-node', text: 'Pins the volume to the node with this UUID. A removal of that node fails until you remove or change it.' },
          { at: 'storageClassName: simplyblock-csi-sc', text: 'A simplyblock StorageClass, so the volume lives on a storage node.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Starting a removal', cat: 'action', text: 'Which of these start a node removal?', options: ['A StorageNodeOps with `action: Remove`', '`kubectl delete storagenode <node>`', 'A StorageNodeOps with `action: Drain`', 'Deleting the worker from the approved ClusterDeploymentConfig', 'A StorageClusterOps with action `Shrink`'], correct: [0, 1], explain: 'A delete raises a `Remove`. There is no `Drain` or `Shrink`, and an approved document cannot change.' },
    { topic: 'Starting a removal', cat: 'field', text: 'You delete the StorageNode `production-worker-5-0`. What does its finalizer create?', options: ['A StorageNodeOps named `production-worker-5-0-remove`', 'A StorageNodeOps named `production-worker-5-0-maintenance`', 'One PersistentVolumeOps per volume', 'Nothing, the object goes at once', 'A StorageClusterOps `Expand`'], correct: [0], explain: 'It creates `<node>-remove` and holds the object until it succeeds.' },
    { topic: 'Starting a removal', cat: 'concept', text: 'A removal from a deletion ended `Failed`. What is true?', options: ['The StorageNode stays, with its finalizer', 'The finalizer is released', 'An `OperationFailed` event repeats on the StorageNode', 'Deleting the failed record raises a new removal', 'The operator reruns the same record'], correct: [0, 2, 3], explain: 'Only `Succeeded` releases the node. You retry by deleting the finished record.' },
    { topic: 'Starting a removal', cat: 'concept', text: 'The cluster is degraded and rebalancing. You create a `Remove`. What happens?', options: ['It starts', 'It waits in `Pending` with `ClusterNotReady`', 'It fails', 'It waits for `Online`', 'It is refused at admission'], correct: [0], explain: 'A removal can make the cluster healthy again, so it is not held back.' },
    { topic: 'Starting a removal', cat: 'field', text: 'Which fields does a `Remove` StorageNodeOps accept?', options: ['`spec.nodeRef`', '`spec.remove.systemVolumeFilterRegex`', '`spec.abort`', '`spec.drain.targetNodes`', '`spec.remove.force`'], correct: [0, 1, 2], explain: 'There is no `spec.drain` and no force flag.' },
    { topic: 'Starting a removal', cat: 'concept', text: 'What keeps a new `Remove` from reaching `Validating`?', options: ['Another operation holds the node\'s lock', 'The node is the remote node of a stretched cluster', 'The cluster is rebalancing', 'The cluster is degraded', 'Another node is being removed'], correct: [0, 1], explain: 'A held lock queues it. An operation on a remote node is refused.' },
    { topic: 'Starting a removal', cat: 'action', text: 'You retire a node and want to keep capacity. In which order do you work?', options: ['Add the new node, wait for `Online`, then remove the old one', 'Remove, then add', 'Delete the worker, then remove the node', 'Fail every drive, then remove', 'Cordon the worker, then remove'], correct: [0], explain: 'Adding first keeps capacity and redundancy full.' },

    { topic: 'Operator checks', cat: 'concept', text: 'A volume\'s claim has `storage.simplyblock.io/selected-storage-node`. What does `Validating` do?', options: ['Fails the operation', 'Holds with `DrainBlocked`', 'Removes the pin and moves the volume', 'Moves the volume and keeps the pin', 'Deletes the volume'], correct: [0], explain: 'The operator never moves a pinned volume. It fails before the shutdown.' },
    { topic: 'Operator checks', cat: 'concept', text: 'A volume on the node has no PersistentVolume. What does `Validating` do?', options: ['Holds with `DrainBlocked`', 'Moves it', 'Deletes it', 'Fails the operation', 'Ignores it'], correct: [0], explain: 'It waits for you to remove the volume by hand.' },
    { topic: 'Operator checks', cat: 'field', text: 'A `Remove` sets `systemVolumeFilterRegex: \'^perf-baseline-.*\'`. What is true?', options: ['Matching volumes are deleted in `Verifying`', 'The default no longer applies', 'A matching volume with a PersistentVolume is deleted too', 'Matching volumes are moved first', 'A bad pattern fails the operation'], correct: [0, 1, 2, 4], explain: 'The pattern replaces the default and is checked first.' },
    { topic: 'Operator checks', cat: 'concept', text: 'One storage node per worker. Which removals pass the zone rule?', options: ['3, 3, 3: from zone 0', '2, 3, 3: from zone 0', '2, 3, 3: from zone 1', '2, 2, 2: any', '4, 4, 3: from zone 2'], correct: [0, 2], explain: 'Zones must stay within 1 host and keep 2. The others leave 1, 3, 3, then 1, 2, 2, then 4, 4, 2.' },
    { topic: 'Operator checks', cat: 'concept', text: 'A worker runs two storage nodes. You remove one. What happens to its zone\'s host count?', options: ['It stays the same', 'It drops by one', 'It drops by two', 'The zone is skipped', 'The removal is refused'], correct: [0], explain: 'The worker still hosts the other node, so it still counts.' },
    { topic: 'Operator checks', cat: 'concept', text: 'A removal holds with `RemovalNotAdmitted`. What is true?', options: ['Nothing has changed on the node', 'The 24-hour budget keeps running', '`spec.abort` ends it `Aborted`', 'The node is offline', 'The check repeats'], correct: [0, 1, 2, 4], explain: 'The node still serves. The removal continues when the zones allow it.' },

    { topic: 'Control plane checks', cat: 'concept', text: 'When does the control plane check a removal?', options: ['At `prepare-removal` in `MigratingDevices`', 'At the node DELETE in `Removing`', 'In `Validating`', 'Only in `AwaitingRemoval`', 'Never'], correct: [0, 1], explain: 'Both happen after the shutdown. That is why the operator checks zones first.' },
    { topic: 'Control plane checks', cat: 'concept', text: 'What does the control plane check before a removal?', options: ['The cluster keeps its fault tolerance', 'Standby copies on the node can move to another host', 'No task runs on the node', 'Claims have no annotations', 'The worker is cordoned'], correct: [0, 1, 2], explain: 'It does not see claims or cordons.' },
    { topic: 'Control plane checks', cat: 'concept', text: 'One parity chunk. Another node is offline at `prepare-removal`. What happens?', options: ['The step holds with `RemovalDeferred`', 'The drained node stays offline while it waits', 'The operation fails', 'The drained node comes back', 'The rebuild is skipped'], correct: [0, 1], explain: 'With one parity chunk, the cluster cannot lose a second node. The wait passes when the peer returns.' },
    { topic: 'Control plane checks', cat: 'action', text: 'A node is `offline` because its worker died. Can you remove it?', options: ['Yes', 'No, it must be `online`', 'Only with `spec.remove.force`', 'Only after deleting the worker', 'Only by failing each drive'], correct: [0], explain: 'Offline and unreachable nodes are removable. Their data is rebuilt from parity.' },

    { topic: 'The drain steps', cat: 'concept', text: 'How does the control plane rebuild a removed node\'s data?', options: ['It fails each drive, and the other nodes rebuild the data from parity', 'It copies each drive to a spare node', 'It waits for a new node', 'It moves the journal drive first', 'The operator copies it'], correct: [0], explain: 'A drive shows `failed_and_migrated` once rebuilt.' },
    { topic: 'The drain steps', cat: 'concept', text: 'How does the operator pick a volume move\'s target?', options: ['Only online peers', 'Never a peer with a copy of the subsystem\'s volumes', 'Peers whose standby is the drained node only as a last choice', 'The peer with most free space', 'Peers of the other site first'], correct: [0, 1, 2], explain: 'It picks round-robin, not by space. On a stretched cluster, only the same site.' },
    { topic: 'The drain steps', cat: 'concept', text: 'No peer can take a subsystem. What is true?', options: ['The step holds with `NoMigrationTarget`', 'The operation fails', 'A peer with a copy is used', 'The volume is skipped', 'The wait counts against the 12-hour budget'], correct: [0, 4], explain: 'It continues when a node comes back or is added.' },
    { topic: 'The drain steps', cat: 'field', text: 'Which labels does a drain\'s PersistentVolumeOps have?', options: ['`storage.simplyblock.io/drain-node` with the node\'s UUID', '`storage.simplyblock.io/target-node` with the target\'s UUID', '`storage.simplyblock.io/managed-by: StorageNodeOps`', '`storage.simplyblock.io/drain-node` with the StorageNode name', 'An owner reference to the StorageNodeOps'], correct: [0, 1, 2], explain: 'The labels hold backend UUIDs. `spec.creatorRef` names the removal.' },
    { topic: 'The drain steps', cat: 'concept', text: 'The node holds 20 volumes in 20 subsystems. What is true of `MigratingVolumes`?', options: ['It creates one PersistentVolumeOps per subsystem', 'It creates one per volume whatever the subsystems', 'The moves run one at a time', 'All 20 copy at once', 'It creates one per peer'], correct: [0, 2], explain: 'The cluster runs one data migration at a time.' },
    { topic: 'The drain steps', cat: 'concept', text: '`status.drain` shows `volumesTotal: 12` and `volumesMigrated: 7`. How many volumes are left?', options: ['5', '7', '12', '19', '0'], correct: [0], explain: '12 − 7 = 5.' },
    { topic: 'The drain steps', cat: 'action', text: 'What does `Verifying` do?', options: ['Deletes system volumes', 'Asks `verify-drained`, which covers snapshots', 'Holds with `DrainBlocked` while anything is left', 'Moves snapshots', 'Deletes unmanaged volumes'], correct: [0, 1, 2], explain: 'The operator does not count snapshots, so the control plane decides.' },
    { topic: 'The drain steps', cat: 'concept', text: 'The shutdown gets a 409. What is true?', options: ['The step holds with `RemovalDeferred`', 'The node keeps serving', 'The operation fails', 'The operator forces it', 'The step is skipped'], correct: [0, 1], explain: 'Forcing could put two nodes down at once.' },
    { topic: 'The drain steps', cat: 'concept', text: 'What does the control plane do after the node DELETE?', options: ['Removes the standby copies of the node\'s store', 'Replaces the node\'s journal copies', 'Moves the node\'s standby roles to other hosts', 'Moves the node\'s volumes', 'Rebuilds the node\'s drives'], correct: [0, 1, 2], explain: 'Volumes and drives were done in earlier steps.' },
    { topic: 'The drain steps', cat: 'field', text: 'Which budgets are right?', options: ['`Validating`: 24 hours', '`ShuttingDown`: 15 minutes', '`MigratingVolumes`: 12 hours', '`Verifying`: 30 minutes', '`Removing`: 2 hours'], correct: [0, 1, 2, 3, 4], explain: 'All five. The two control-plane waits have 7 hours without progress.' },

    { topic: 'Phases and events', cat: 'concept', text: 'Which phases does a StorageNode pass during a removal?', options: ['`Offline`', '`Removing`', '`Removed`', '`Shrinking`', '`Draining`'], correct: [0, 1, 2], explain: '`Shrinking` is the cluster\'s phase.' },
    { topic: 'Phases and events', cat: 'concept', text: 'Which is true of a `Removed` StorageNode?', options: ['Nothing brings it back', 'A `Restart` brings it back', 'A `Resume` brings it back', 'The operator deletes it', 'The operator strips the worker\'s labels'], correct: [0], explain: 'You delete the object and the labels yourself.' },
    { topic: 'Phases and events', cat: 'concept', text: 'Which StorageCluster phase shows a removal in progress?', options: ['`Shrinking`', '`Rebalancing`', '`Degraded`', '`Removing`', '`Provisioning`'], correct: [0], explain: '`Removing` is a StorageNode phase.' },
    { topic: 'Phases and events', cat: 'concept', text: 'Which events mean the removal waits rather than failed?', options: ['`DrainBlocked`', '`NoMigrationTarget`', '`RemovalDeferred`', '`RemovalNotAdmitted`', '`StepDeadlineExceeded`'], correct: [0, 1, 2, 3], explain: 'The budget keeps running during each wait.' },

    { topic: 'Abort and cleanup', cat: 'concept', text: 'From which steps can you abort a `Remove`?', options: ['`Validating`', '`ShuttingDown`', '`MigratingDevices`', '`MigratingVolumes`', '`Verifying`'], correct: [0], explain: 'Only `Validating` has changed nothing.' },
    { topic: 'Abort and cleanup', cat: 'action', text: 'You set `spec.abort: true` in `MigratingVolumes`. What happens?', options: ['`AbortRefused`, and the removal continues', 'The node comes back', 'Moves are reversed', 'It ends `Aborted`', 'Volumes move back'], correct: [0], explain: 'The step cannot be aborted.' },
    { topic: 'Abort and cleanup', cat: 'concept', text: 'A hand-written removal succeeded. What does the operator do next by itself?', options: ['Deletes the StorageNode', 'Strips the worker\'s labels', 'Cordons the worker', 'Deletes the StorageNodeOps', 'Edits the approved document'], correct: [], explain: 'None. You clean up yourself.' },
    { topic: 'Abort and cleanup', cat: 'concept', text: 'Why delete the `Removed` StorageNode before you cordon its worker?', options: ['A cordon creates `<node>-maintenance` for every StorageNode on the worker', 'A cordon deletes the StorageNode', 'Kubernetes refuses the cordon', 'The finalizer needs a schedulable worker', 'The control plane re-adds the node'], correct: [0], explain: 'A `Removed` node is not skipped.' },
    { topic: 'Abort and cleanup', cat: 'action', text: 'Somebody runs `kubectl delete snops` during `MigratingVolumes`. What happens?', options: ['The drain stops', 'The node comes back', 'The moves run unwatched', 'The moves are undone', 'The delete is admitted'], correct: [], explain: 'None. The webhook refuses the delete until the removal finishes.' },

    { topic: 'Removing one drive', cat: 'action', text: 'Which operation takes one drive out of service?', options: ['StorageDeviceOps `Fail`', 'StorageDeviceOps `Remove`', 'StorageDeviceOps `Replace`', 'StorageNodeOps `Remove` with a drive list', 'Editing `pcieAllowList`'], correct: [0], explain: 'There is no `Remove` or `Replace` on a drive.' },
    { topic: 'Removing one drive', cat: 'concept', text: 'Which `Fail` steps can be aborted?', options: ['`Removing`', '`Failing`', '`Awaiting`', '`Requesting`', '`Completed`'], correct: [0], explain: 'Once the drive leaves the data path, nothing puts it back.' },
    { topic: 'Removing one drive', cat: 'concept', text: 'Which statements about `Fail` are true?', options: ['It cannot be undone', 'Other nodes rebuild the drive\'s data', 'The operation waits for the rebuild', 'It is refused while any volume migration runs', 'The node shuts down'], correct: [0, 1, 3], explain: 'The rebuild continues after the operation ends. The node keeps serving.' },
    { topic: 'Removing one drive', cat: 'field', text: 'Which StorageDeviceOps actions are admitted?', options: ['`Replace`', '`Migrate`', '`SelfTest`', '`Remove`', '`Add`'], correct: [], explain: 'None. The admitted actions are `Restart`, `Fail`, and `CheckHealth`.' },
  ],

  tasks: [
    {
      title: 'A removal that keeps the site\'s benchmark volumes out of the drain',
      scenario: 'Remove the node `edge-worker-3-0`. Delete its `bench-...` volumes instead of moving them, and keep `sb-fio-baseline-` volumes as system volumes.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: edge-worker-3-0-remove\n  namespace: simplyblock\nspec:\n  nodeRef: {{node}}\n  action: {{action}}\n  remove:\n    systemVolumeFilterRegex: \'{{regex}}\'\n',
      blanks: {
        kind: { accept: ['StorageNodeOps'], hint: 'short name `snops`' },
        node: { accept: ['edge-worker-3-0'], hint: 'the StorageNode name' },
        action: { accept: ['Remove'], hint: 'drain, then remove' },
        regex: { accept: ['^(sb-fio-baseline-|bench-).*', '^(bench-|sb-fio-baseline-).*', '^(sb-fio-baseline-|bench-)', '^(bench-|sb-fio-baseline-)', '^sb-fio-baseline-.*|^bench-.*', '^bench-.*|^sb-fio-baseline-.*'], hint: 'both prefixes, anchored' },
      },
      explain: 'The pattern replaces the default, so it needs both prefixes. Anchor it so no user volume matches.',
    },
    {
      title: 'Calling off a held removal',
      scenario: '`production-worker-5-0-remove` holds in `Validating` with `RemovalNotAdmitted`. Write it after the one edit that calls it off.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: StorageNodeOps\nmetadata:\n  name: production-worker-5-0-remove\n  namespace: simplyblock\nspec:\n  nodeRef: production-worker-5-0\n  action: {{action}}\n  {{field}}: {{value}}\n',
      blanks: {
        action: { accept: ['Remove'], hint: 'unchanged' },
        field: { accept: ['abort'], hint: 'the only field that can change' },
        value: { accept: ['true'], hint: 'works in `Validating` only' },
      },
      explain: '`abort: true` in `Validating` ends the removal `Aborted`. The node keeps serving.',
    },
    {
      title: 'Failing one drive',
      scenario: 'A drive of `production-worker-5-0` keeps reporting media errors. Its UUID begins `3f9c2a1b-`. Take only that drive out of service.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: production-worker-5-0-3f9c2a1b-fail\n  namespace: simplyblock\nspec:\n  deviceRef: {{device}}\n  action: {{action}}\n',
      blanks: {
        kind: { accept: ['StorageDeviceOps'], hint: 'short name `sdops`' },
        device: { accept: ['production-worker-5-0-3f9c2a1b'], hint: 'node name plus eight UUID characters' },
        action: { accept: ['Fail'], hint: 'removes the drive, then fails it' },
      },
      explain: 'The StorageDevice is `<node>-<eight UUID characters>`. `Fail` cannot be undone, and the node keeps serving.',
    },
  ],
});
