// Chapter 11: removing a storage node from a running cluster. It teaches the
// one operation that does it, a StorageNodeOps with action Remove, the two ways
// it is started, the census and admission that run before anything is touched,
// the six steps after the point of no return, and what is left behind once the
// node is Removed. Removing one drive is the StorageDeviceOps Fail action, and
// the chapter closes with it because it is the narrower answer to the same
// question.
//
// The steps, budgets, and messages are read from
// operator/internal/controllers/node/remove.go, classify.go, and graphs.go.
TRAINING.register({
  id: 'node-removal',
  title: 'Removing a storage node',
  summary: 'Taking one storage node out of a running cluster with a StorageNodeOps `Remove`: the volume census and the admission checks that run first, the drain that moves the node\'s devices and volumes onto its peers, the removal, and what is left afterward. Removing a single drive with StorageDeviceOps `Fail` closes the chapter.',
  goal: 'the learner can start a node removal either way, read why it holds or fails, follow it step by step until the node is `Removed`, clean up the StorageNode and the worker in the right order, and take a single drive out of service instead when that is all that is needed.',

  sections: [
    {
      heading: 'What a removal is, and where the framing was adjusted',
      body: [
        'The chapter was commissioned under the title "Removing a node," and in the code that is not one call. A removal is a drain followed by the control plane\'s node removal, both run by one StorageNodeOps with `action: Remove`. The drain is a sequence of steps inside that operation and not an action of its own: there is no `Drain` action, and the `spec.drain` block of `v1alpha1` is `spec.remove` in `v1alpha2`. Removing a single drive is a different operation on a different kind, StorageDeviceOps `Fail`, and the last section covers it. Moving a node to another worker while keeping it is `Migrate`, the subject of [the host-migration chapter](chapter.html?c=node-host-migration), and is not a removal at all.',
        'The operation splits the work between the operator and the control plane. The operator decides whether the removal may start, moves the volumes through Kubernetes objects, and drives the control plane\'s calls in order. The control plane rebuilds the node\'s data onto its peers, copies each volume, and finally takes the node apart.',
        { table: { head: ['Part of the removal', 'Done by', 'How'], rows: [
          ['Sorting the node\'s volumes and checking that the removal is allowed', 'operator', 'reads the control plane\'s volumes, the PersistentVolumes, and the claims, and computes the failure-domain rule from the StorageNodes'],
          ['Shutting the node down', 'control plane, asked by the operator', '`POST .../storage-nodes/{node}/shutdown`'],
          ['Rebuilding the node\'s devices onto its peers', 'control plane', '`POST .../storage-nodes/{node}/prepare-removal`'],
          ['Moving the volumes', 'operator, with the control plane copying the data', 'one PersistentVolumeOps per NVMe-oF subsystem'],
          ['Confirming the node is empty', 'operator and control plane', 'deletes system volumes, then `POST .../verify-drained`'],
          ['Removing the node', 'control plane, asked by the operator', '`DELETE .../storage-nodes/{node}?force_remove=false`'],
        ] } },
        { callout: 'From the shutdown onward there is no way back. Only the first step, `Validating`, can be aborted, and it changes nothing. A node that reaches `Removed` is never brought back into service.', kind: 'warn' },
        'How the control plane rebuilds devices, copies data, and decides its own admission is not part of this repository, and the chapter describes only what the operator asks for and how it reads the answers.',
      ],
    },
    {
      heading: 'Starting a removal: an operation or a deletion',
      body: [
        'There are two ways to start one, and both end in the same kind of object.',
        { list: [
          '**A StorageNodeOps written by hand**, with `spec.nodeRef` naming the StorageNode and `action: Remove`. `spec.remove.systemVolumeFilterRegex` is optional. The operation is a record and stays after it finishes.',
          '**Deleting the StorageNode**, with `kubectl delete storagenode <node>`. The node\'s finalizer raises a StorageNodeOps named `<node>-remove`, owned by the StorageNode through a controller reference, and holds the object until that removal reaches `Succeeded`. A StorageNode with no backend UUID has nothing behind it, and its finalizer is released at once without a removal. So is the finalizer of a node whose StorageCluster is already gone.',
        ] },
        'A deletion-raised removal that ends `Failed` or `Aborted` does not release the StorageNode. The object stays, with its finalizer, and the operator repeats an `OperationFailed` event carrying the removal\'s message on it. The finalizer raises a removal only when no record of that name exists, so retrying means deleting the terminal `<node>-remove` record, after which the next pass raises a new one.',
        'A Remove is not held by the cluster gate that keeps `Migrate` and `HostMaintenance` off a cluster that is not active or is rebalancing. It runs on a degraded or rebalancing cluster, because removing a node is sometimes what makes the cluster healthy again. It does take the node\'s lock, `StorageNode.status.activeOpsRef`, so a second operation on the same node waits in `Pending` with an `OperationQueued` event. An operation against the remote node of a stretched cluster is refused at admission: that node\'s own site runs its operations.',
        { code: 'kubectl apply -f 01-storagenodeops-remove.yaml\nkubectl get snops -n simplyblock -w\nkubectl get sn production-worker-5-0 -n simplyblock -w', title: 'Starting and following a removal' },
      ],
      examples: [
        { title: 'A removal written by hand', file: 'examples/node-removal/01-storagenodeops-remove.yaml', notes: [
          { at: 'name: production-worker-5-0-remove', text: 'The name the finalizer would give it. When the StorageNode is deleted later, the finalizer looks for exactly this name, finds a `Succeeded` record, and releases the object without raising a second removal.' },
          { at: 'nodeRef: production-worker-5-0', text: 'The StorageNode, by its object name, in the operation\'s namespace. Immutable, like `action`.' },
          { at: 'action: Remove', text: 'Drain, then remove. The other actions of the kind are `Shutdown`, `Restart`, `Suspend`, `Resume`, `Migrate`, and `HostMaintenance`.' },
        ] },
        { title: 'The same removal with its own system-volume pattern', file: 'examples/node-removal/02-storagenodeops-remove-regex.yaml', notes: [
          { at: '  remove:', text: 'The parameter block of the `Remove` action, ignored by every other action. Backend volume names matching it are deleted in `Verifying` rather than migrated. Without the field, the operator uses `^sb-fio-baseline-.*`, the rebalancer\'s per-node benchmark volumes.' },
          { at: 'sb-fio-baseline-|perf-baseline-', text: 'The field replaces the default, so the default pattern is repeated in the alternation. A pattern that does not compile fails the operation in `Validating`.' },
        ] },
      ],
    },
    {
      heading: 'Validating: the volume census and the admission',
      body: [
        '`Validating` performs no side effect. It sorts every volume whose primary is on the node into one of four buckets, then asks whether the removal is admitted. Its budget is 24 hours.',
        { table: { head: ['Bucket', 'What puts a volume in it', 'What the removal does with it'], rows: [
          ['System', 'its backend name matches `systemVolumeFilterRegex`', 'skips it now and deletes it in `Verifying`'],
          ['Unmanaged', 'no simplyblock PersistentVolume names it', 'holds `Validating` with `DrainBlocked`, naming the volumes, until somebody removes them by hand'],
          ['Pinned', 'its claim carries `storage.simplyblock.io/selected-storage-node`', 'fails the operation at once, naming the volumes'],
          ['Managed', 'a PersistentVolume names it, and nothing pins it', 'counts it into `status.drain.volumesTotal` and moves it in `MigratingVolumes`'],
        ] } },
        'A pinned volume fails the removal rather than holding it, because the operator does not remove a pin on the user\'s behalf and a removal that reports `Running` for hours looks like progress. The fix is to remove or change the annotation on the claim and start a new removal. A claim that cannot be read for a moment does not block the drain: the census is marked incomplete and the pass is retried.',
        { callout: 'The pattern is checked before the PersistentVolume is looked up. A volume that has a PersistentVolume and a claim, and whose backend name happens to match the pattern, is treated as a system volume and deleted in `Verifying`. Keep the pattern narrow.', kind: 'warn' },
        'With no pinned and no unmanaged volume left, two admission answers are asked, the operator\'s own first.',
        { steps: [
          '**The failure-domain rule**, only on a cluster with `spec.enableFailureDomains: true`. The operator counts hosts, that is workers, per failure domain from the StorageNodes, and takes one host away from the drained node\'s domain unless another storage node of the cluster stays on the same worker. After the removal, the largest and the smallest domain may differ by at most 1 host, and every domain has to keep at least 2 hosts.',
          '**The control plane\'s read-only check**, `GET .../storage-nodes/{node}/removal-admission`, which the operator\'s code describes as covering fault-tolerance headroom, replica relocation, and active tasks on the node. A control plane that answers 404 does not offer the check, and its admission then runs only inside `prepare-removal`, after the node is already shut down.',
        ] },
        { table: { head: ['Hosts per domain before', 'Node removed from', 'After', 'Operator\'s answer'], rows: [
          ['3, 3, 3', 'domain 0', '2, 3, 3', 'admitted'],
          ['2, 3, 3', 'domain 0', '1, 3, 3', 'refused: unbalanced by 2, and domain 0 below 2'],
          ['2, 3, 3', 'domain 1', '2, 2, 3', 'admitted'],
          ['2, 2, 2', 'any', '1, 2, 2', 'refused: a domain below 2'],
          ['4, 4, 3', 'domain 2', '4, 4, 2', 'refused: unbalanced by 2'],
        ] } },
        'A refusal from either answer holds the step with a `RemovalNotAdmitted` event and this message, with the reason in the operator\'s or the control plane\'s words:',
        { code: 'the removal of node production-worker-5-0 is not admitted: failure domain 0 would drop to 1 host(s); at least 2 are required per domain; nothing has been done', title: 'status.message of a held removal' },
        'The hold is re-checked on every pass, and the step\'s 24-hour deadline keeps running. When the condition clears, for example, after a growth document has added a host to the short domain (see [Expanding a cluster](chapter.html?c=cluster-expansion)), the removal continues by itself. When the deadline passes first, the operation fails with `StepDeadlineExceeded` and the node is untouched. [Failure domains](chapter.html?c=failure-domains) explains why a cluster of three domains of two cannot shrink until it has grown.',
        'Once admitted, the step writes `status.drain.volumesTotal`, the number of managed volumes, and moves to `ShuttingDown`.',
      ],
    },
    {
      heading: 'The steps after the point of no return',
      body: [
        'Each step has its own budget, read on every pass. A step that outlives it fails the operation with a `StepDeadlineExceeded` event. Two budgets count time without progress: every change of the node\'s control-plane status or of one of its devices\' statuses moves the deadline a whole budget out again. Every step first asks the control plane for the node, and a node the control plane does not have counts as that step done.',
        { table: { head: ['Step', 'Budget', 'What is sent', 'Done when', 'Holds or fails'], rows: [
          ['`ShuttingDown`', '15 minutes', 'a graceful shutdown, only to a node that is `online` or `suspended`', 'the control plane accepted it, or the node is no longer running', '409: holds with `RemovalDeferred`, asked again at most once a minute. Other 4xx: fails, node still serving'],
          ['`MigratingDevices`', '7 hours without progress', '`prepare-removal` once the node is `offline`, then again while the rebuild runs, to keep it going or restart it', 'the control plane reports the device rebuild done', 'a refusal that passes by itself: `RemovalDeferred`. A final refusal: fails, node left offline'],
          ['`MigratingVolumes`', '12 hours', 'one PersistentVolumeOps per NVMe-oF subsystem', 'no managed volume is left on the node and no move is outstanding', 'no eligible peer: holds with `NoMigrationTarget`'],
          ['`Verifying`', '30 minutes', 'deletes the system volumes, then `verify-drained`', 'the node holds no volume and `verify-drained` agrees', 'anything left: holds with `DrainBlocked`. A refused delete: fails'],
          ['`Removing`', '2 hours', '`DELETE ...?force_remove=false`', 'the call is accepted, answers 404, or the node is `in_removal` or later', 'a refusal that passes by itself: `RemovalDeferred`. Other 4xx: fails'],
          ['`AwaitingRemoval`', '7 hours without progress', 'nothing', 'the node reports `removed`', '`removed_failed`: fails'],
        ] } },
        '**MigratingDevices.** The control plane rebuilds the data the node\'s devices held onto the peers, and the operator records its progress in `status.removal`: the node\'s status in the control plane\'s spelling (`pending_removal`, `migrating_devices`, `migrating_lvols`, `in_removal`, `removed`, `removed_failed`), each StorageDevice\'s status, and when either last changed. The refusals that pass by themselves are a cluster still rebalancing, an active task on the node, and peers that are not online. A final refusal fails the operation with the node offline, and the message says that a Restart operation brings it back. A node stuck in `pending_removal` gets `prepare-removal` again up to 3 times, counted in `status.removal.prepareAttempts`, before the operation fails.',
        '**MigratingVolumes.** The control plane moves an NVMe-oF subsystem with all its volumes at once, so the operator raises one PersistentVolumeOps per subsystem, named after the subsystem\'s first PersistentVolume. The cluster has one migration slot, an annotation on the StorageCluster, because the control plane runs one data migration per cluster, so the moves run one after another and the others wait in `Pending`. Completed moves are deleted as soon as they are counted into `status.drain.volumesMigrated`. A failed move is kept until the step ends, as the record of where that subsystem could not go, and a new move to a different peer replaces it with a `MigrationRetried` event. Once every move has finished the step emits `DrainCompleted`.',
        'The target of each move is chosen from the peers in this order of rules:',
        { list: [
          'only peers the control plane reports `online`, and on a stretched cluster only those of the drained node\'s site',
          'never a peer holding a replica of any volume of the subsystem, and never the drained node\'s own secondary or tertiary',
          'never a peer an earlier failed move of the same subsystem already tried',
          'peers whose secondary or tertiary is the drained node only when no other peer is left',
          'round-robin over the rest, sorted by UUID, so the node\'s volumes spread across the peers',
        ] },
        'When a move has no eligible peer, the step holds with `NoMigrationTarget`, naming what was tried. That is a stall and not a failure: it resolves when a node comes back or is added, and the 12-hour budget bounds the wait.',
        '**Verifying and Removing.** The census sees no snapshots, and the node DELETE refuses a node that still holds volumes or snapshots, so `Verifying` closes only on the control plane\'s `verify-drained`. `Removing` sends the DELETE without force, and `AwaitingRemoval` waits for the control plane to report the node `removed`.',
      ],
      examples: [
        { title: 'One of the moves the drain raises', file: 'examples/node-removal/03-persistentvolumeops-drain.yaml', notes: [
          { at: 'name: drain-', text: '`drain-`, the drained node\'s backend UUID, and the PersistentVolume\'s name. The name is derived, so a pass that runs again finds the object instead of creating a second one.' },
          { at: 'storage.simplyblock.io/drain-node', text: 'The backend UUID of the node being drained, not the StorageNode\'s name. `kubectl get pvops -l storage.simplyblock.io/drain-node=<uuid>` lists one drain\'s moves.' },
          { at: 'storage.simplyblock.io/target-node', text: 'The backend UUID of the peer the operator chose. A failed move is read back by it, so that the retry goes elsewhere.' },
          { at: 'managed-by: StorageNodeOps', text: 'The creator\'s kind, as a label because a reference in the spec cannot be selected on.' },
          { at: 'name: production-worker-2-0', text: 'The target as a StorageNode, which the operator resolved from the UUID it chose. It carries a namespace because the operation itself is cluster-scoped.' },
          { at: 'creatorRef:', text: 'The removal that raised the move, with its UID. A cluster-scoped object cannot have a namespaced owner, so the removal\'s finalizer aborts and deletes its moves instead of the garbage collector.' },
        ] },
      ],
    },
    {
      heading: 'Following a removal: phases, status, and events',
      body: [
        'Three objects show a removal, each from its own side.',
        { table: { head: ['Object', 'What it shows during a removal'], rows: [
          ['StorageNodeOps', '`Pending` until it holds the node\'s lock, `Running` with the step in the `Step` column, then `Succeeded`, `Failed`, or `Aborted`'],
          ['StorageNode', '`Offline` once the shutdown lands, `Removing` while the control plane reports `pending_removal` through `in_removal`, then `Removed`'],
          ['StorageCluster', '`Shrinking` for as long as the control plane reports a removal in progress, in place of `Online`, `Rebalancing`, or `Degraded`. It never replaces a phase in which the cluster is not serving'],
        ] } },
        '`Removed` is the last phase a StorageNode reaches. Nothing in the operator, a Restart included, brings that node back into service. Capacity comes back by adding a node.',
        { table: { head: ['Event', 'Meaning', 'What to do'], rows: [
          ['`OperationStarted`', 'the removal took the node\'s lock', 'nothing'],
          ['`OperationQueued`', 'another operation holds the node\'s lock', 'wait for it, or abort it'],
          ['`DrainBlocked`', 'unmanaged volumes in `Validating`, or volumes and snapshots still on the node in `Verifying`', 'remove what the message names'],
          ['`RemovalNotAdmitted`', 'the failure-domain rule or the control plane refused the removal, and nothing was done', 'change the condition, or abort'],
          ['`RemovalDeferred`', 'a shutdown, `prepare-removal`, or DELETE refused for a reason that passes by itself', 'nothing, it is asked again'],
          ['`NoMigrationTarget`', 'a subsystem has no eligible peer', 'bring a peer back or add a node'],
          ['`MigrationRetried`', 'a move failed or was aborted and is raised again', 'read the message for the failed target'],
          ['`DrainCompleted`', 'every volume has moved', 'nothing'],
          ['`AbortRefused`', '`spec.abort` arrived after `Validating`', 'nothing, the removal runs on'],
          ['`StepDeadlineExceeded`', 'a step outlived its budget, and the operation fails', 'read `status.message`'],
        ] } },
        'The operator emits each event on the StorageNodeOps and repeats it on the StorageNode, so `kubectl describe sn <node>` shows the removal\'s history too.',
        { code: 'kubectl get snops production-worker-5-0-remove -n simplyblock -o wide\nkubectl get snops production-worker-5-0-remove -n simplyblock -o jsonpath=\'{.status.drain}{"\\n"}{.status.removal}{"\\n"}\'\nkubectl get pvops -l storage.simplyblock.io/managed-by=StorageNodeOps\nkubectl get stc -n simplyblock', title: 'Reading the progress' },
      ],
    },
    {
      heading: 'Aborting, and what is left afterward',
      body: [
        'Setting `spec.abort: true` is the only change a StorageNodeOps accepts after creation. In `Validating` it ends the operation `Aborted` with nothing done, which makes it the way to call off a removal held with `RemovalNotAdmitted` or `DrainBlocked`. From `ShuttingDown` onward the abort is refused with an `AbortRefused` event and the removal runs on. Deleting the record is refused by the admission webhook at every step after `Validating` until the operation is terminal, because the record is the only thing following the removal to its end.',
        { code: 'kubectl patch snops production-worker-5-0-remove -n simplyblock --type merge -p \'{"spec":{"abort":true}}\'', title: 'Calling off a held removal' },
        'Aborting a removal that a deletion raised does not cancel the deletion. The StorageNode keeps its deletion timestamp and its finalizer, the node keeps serving, and the finalizer holds because the removal did not succeed. Deleting the `Aborted` record makes the next pass raise a new removal.',
        'A removal written by hand leaves two things behind when it succeeds. The StorageNode object stays, in phase `Removed`. The worker keeps its storage labels, because the operator strips them only at the end of a `Migrate`. Neither is cleaned up by the operator.',
        { steps: [
          'Wait for the StorageNodeOps to reach `Succeeded` and the StorageNode `Removed`.',
          'Delete the StorageNode. When the removal was named `<node>-remove`, the finalizer finds that `Succeeded` record and releases the object. A removal under any other name leads the finalizer to raise a new `<node>-remove` against a node the control plane reports removed, and what that operation then does has not been traced.',
          'Only then cordon and drain the worker, and remove its storage labels if it is leaving the storage plane.',
        ] },
        { callout: 'The order in the last two steps is inferred from the code rather than observed. The StorageNode reconcile raises a `HostMaintenance` operation, `<node>-maintenance`, for any node with a backend UUID whose worker is cordoned, and nothing on that path checks for the `Removed` phase. Deleting the Removed StorageNode first keeps that from happening.', kind: 'warn' },
      ],
      examples: [
        { title: 'The removal after the abort', file: 'examples/node-removal/04-storagenodeops-remove-abort.yaml', notes: [
          { at: 'abort: true', text: 'Honored in `Validating` and refused afterward. The operation ends `Aborted`, which is a phase of its own and not `Failed`, because nothing went wrong.' },
          { at: 'action: Remove', text: 'Unchanged. `nodeRef` and `action` are immutable, so the edit touches only `abort`.' },
        ] },
      ],
    },
    {
      heading: 'Removing one drive instead: StorageDeviceOps Fail',
      body: [
        'A drive that cannot be trusted any more does not need its whole node removed. A StorageDeviceOps with `action: Fail` takes one StorageDevice out of service: it removes the device from the data path, then fails it, and the cluster rebuilds the redundancy the device held. The StorageDevice is named after its StorageNode and the first eight characters of the control plane\'s device UUID.',
        { table: { head: ['Step', 'Budget', 'What happens', 'Abortable'], rows: [
          ['`Removing`', '10 minutes', 'the device is taken out of the data path, with a `DeviceRemovalRequested` event', 'yes, until the call is made'],
          ['`Failing`', '2 minutes', 'the removed device is failed, with a `DeviceFailRequested` event', 'no'],
          ['`Awaiting`', '5 minutes', 'waits for the device to report `failed` or `failed_and_migrated`', 'no'],
        ] } },
        'The failure cannot be undone, and the rebuild it starts outlives the operation: `Awaiting` waits for the failure to be reported, not for the rebuild to finish. A device that already reports failed is refused rather than failed again. To check a suspicion first, `Restart` recycles the device and `CheckHealth` reads its SMART log into `status.health.smart` without changing anything.',
        'There is no bare `Remove` action on a device, and `Replace`, `Migrate`, and `SelfTest` are refused at admission because the control plane offers no verb for them yet. The operator mirrors whatever devices the control plane reports as StorageDevices. How a drive inserted into a free slot reaches the control plane is host-side behavior that cannot be verified in this repository.',
      ],
      examples: [
        { title: 'Failing one drive', file: 'examples/node-removal/05-storagedeviceops-fail.yaml', notes: [
          { at: 'deviceRef: production-worker-5-0-3f9c2a1b', text: 'The StorageDevice, in the operation\'s namespace: `<node>-<first eight characters of the device UUID>`. `kubectl get sd -l storage.simplyblock.io/node=production-worker-5-0` lists a node\'s devices.' },
          { at: 'action: Fail', text: 'One of three admitted actions, with `Restart` and `CheckHealth`. Abortable only while it is still in `Removing`.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The removal is `Failed` in `Validating` with a message naming pinned volumes.', cause: 'Claims on the node carry `storage.simplyblock.io/selected-storage-node`, and a pinned volume cannot be moved.', fix: 'Remove or change the annotation on those claims, then start a new removal. A deletion-raised one is restarted by deleting the failed `<node>-remove` record.' },
          { symptom: '`Validating` holds with `DrainBlocked` and a list of volume UUIDs.', cause: 'Backend volumes on the node have no PersistentVolume, and the operator neither moves nor deletes data nothing in Kubernetes tracks.', fix: 'Remove those volumes through the control plane by hand. The removal continues on its next pass.' },
          { symptom: '`Validating` holds with `RemovalNotAdmitted` and a failure-domain message.', cause: 'The removal would leave the domains unbalanced by more than 1 host, or a domain with fewer than 2.', fix: 'Grow the short domain first with a growth document, or abort the removal and remove a node from a larger domain.' },
          { symptom: 'The removal failed with `StepDeadlineExceeded` at `Validating`, a day after it was created.', cause: 'A hold was never resolved, and `Validating` has a 24-hour budget.', fix: 'Nothing was done to the node. Resolve the hold and start a new removal.' },
          { symptom: '`MigratingVolumes` holds with `NoMigrationTarget`.', cause: 'No online peer holds none of the subsystem\'s replicas, or every eligible peer has already failed a move of it.', fix: 'Bring an offline peer back or add a node. The drain resumes by itself, within the 12-hour budget.' },
          { symptom: 'The removal failed in `MigratingDevices` and the node is offline.', cause: 'The control plane refused `prepare-removal` for a reason that does not pass by itself.', fix: 'A StorageNodeOps `Restart` brings the node back. Read the refusal in `status.message` before trying again.' },
          { symptom: 'A user-written volume disappeared during a removal.', cause: 'Its backend name matched `systemVolumeFilterRegex`, which is checked before the PersistentVolume, so it was deleted in `Verifying` as a system volume.', fix: 'Keep the pattern anchored and specific to benchmark volumes. A deleted volume is not recovered by the operator.' },
          { symptom: '`kubectl delete storagenode` returned, and the object is still there days later.', cause: 'The finalizer holds until `<node>-remove` succeeds, and that removal failed or was aborted.', fix: 'Read the record\'s `status.message`, fix the cause, and delete the terminal record so that a new removal is raised.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Starting a removal', cat: 'action', text: 'Which of these start the removal of a storage node?', options: ['A StorageNodeOps with `action: Remove`', '`kubectl delete storagenode <node>`', 'A StorageNodeOps with `action: Drain`', 'Deleting the worker from the approved ClusterDeploymentConfig', 'A StorageClusterOps with action `Shrink`'], correct: [0, 1], explain: 'The deletion\'s finalizer raises a Remove, so both paths end in the same operation. There is no `Drain` action, an approved document is immutable and never removes anything, and StorageClusterOps has no `Shrink` action.' },
    { topic: 'Starting a removal', cat: 'field', text: 'The StorageNode `production-worker-5-0`, which has a backend UUID, is deleted. What does its finalizer raise?', options: ['A StorageNodeOps named `production-worker-5-0-remove`, owned by the StorageNode', 'A StorageNodeOps named `production-worker-5-0-maintenance`', 'One PersistentVolumeOps per volume, directly', 'Nothing, the object goes at once', 'A StorageClusterOps `Expand`'], correct: [0], explain: 'The finalizer raises `<node>-remove` with a controller reference to the node and holds the object until it succeeds. `-maintenance` is the cordon\'s operation, the moves come later from the removal, and only a node without a backend UUID goes at once.' },
    { topic: 'Starting a removal', cat: 'concept', text: 'A removal raised by deleting a StorageNode ended `Failed`. Which statements are true?', options: ['The StorageNode object stays, with its finalizer', 'The finalizer is released, because the removal is terminal', 'An `OperationFailed` event with the removal\'s message is repeated on the StorageNode', 'Deleting the failed `<node>-remove` record makes the finalizer raise a new removal', 'The operator retries the same record by itself'], correct: [0, 2, 3], explain: 'Only `Succeeded` releases the finalizer. The finalizer raises a removal only when no record of that name exists, so a retry is deleting the terminal record. A terminal record is never run again.' },
    { topic: 'Starting a removal', cat: 'concept', text: 'The cluster is degraded and rebalancing when a Remove is created. What happens?', options: ['It goes ahead, because Remove is exempt from the cluster gate', 'It holds in `Pending` with `ClusterNotReady`', 'It fails at once', 'It waits until the cluster is `Online`', 'It is refused at admission'], correct: [0], explain: 'The gate holds only `Migrate` and `HostMaintenance`. Removing a node can be what makes the cluster healthy again, so holding a removal on an unhealthy cluster could wait forever.' },
    { topic: 'Starting a removal', cat: 'field', text: 'Which of these are fields a Remove StorageNodeOps accepts?', options: ['`spec.nodeRef`', '`spec.remove.systemVolumeFilterRegex`', '`spec.abort`', '`spec.drain.targetNodes`', '`spec.remove.force`'], correct: [0, 1, 2], explain: '`nodeRef` and `action` are required, `remove.systemVolumeFilterRegex` is the one parameter of the action, and `abort` calls it off. There is no `spec.drain` in `v1alpha2` and no force flag under `remove`.' },
    { topic: 'Starting a removal', cat: 'concept', text: 'Which of these keep a newly created Remove from reaching `Validating`?', options: ['Another operation holding the node\'s lock', 'The node being the remote node of a stretched cluster', 'The cluster rebalancing', 'The cluster being degraded', 'The cluster being `Shrinking` from the removal of another node'], correct: [0, 1], explain: 'A held lock queues the operation in `Pending` with `OperationQueued`, and the webhook refuses an operation on a remote node. The cluster\'s state does not hold a Remove before it starts, although the control plane\'s admission may refuse it in `Validating`.' },
    { topic: 'Census and admission', cat: 'concept', text: 'A claim of a volume on the node carries `storage.simplyblock.io/selected-storage-node`. What does `Validating` do?', options: ['Fails the operation, naming the volume', 'Holds with `DrainBlocked` until the annotation is removed', 'Removes the annotation and moves the volume', 'Moves the volume and keeps the annotation', 'Deletes the volume in `Verifying`'], correct: [0], explain: 'A pinned volume cannot be moved, and the operator does not remove a pin for the user. It fails before the shutdown rather than holding, so the removal does not look like it is making progress.' },
    { topic: 'Census and admission', cat: 'concept', text: 'A backend volume on the node has no PersistentVolume. What does `Validating` do?', options: ['Holds with `DrainBlocked`, naming the volume', 'Moves it like any other volume', 'Deletes it in `Verifying`', 'Fails the operation', 'Ignores it'], correct: [0], explain: 'Moving it would move data nothing in Kubernetes tracks, and deleting it would destroy that data, so the removal waits for somebody to remove it by hand.' },
    { topic: 'Census and admission', cat: 'field', text: 'A Remove sets `systemVolumeFilterRegex: \'^perf-baseline-.*\'`. Which statements are true?', options: ['Matching volumes are deleted in `Verifying`', 'The default `^sb-fio-baseline-.*` no longer applies', 'A volume with a PersistentVolume whose backend name matches is deleted too', 'Matching volumes are migrated first', 'A pattern that does not compile fails the operation'], correct: [0, 1, 2, 4], explain: 'The pattern replaces the default and is checked before the PersistentVolume lookup, so any matching volume is a system volume. A pattern that does not parse is a terminal failure, because no retry makes it parse.' },
    { topic: 'Census and admission', cat: 'concept', text: 'A cluster in failure-domain mode has one storage node per worker. Which removals does the operator\'s failure-domain rule admit?', options: ['Hosts 3, 3, 3: a node from domain 0', 'Hosts 2, 3, 3: a node from domain 0', 'Hosts 2, 3, 3: a node from domain 1', 'Hosts 2, 2, 2: any node', 'Hosts 4, 4, 3: a node from domain 2'], correct: [0, 2], explain: 'After the removal the domains may differ by at most 1 host and each must keep 2. The second leaves 1, 3, 3, the fourth leaves a domain at 1, and the fifth leaves 4, 4, 2, unbalanced by 2.' },
    { topic: 'Census and admission', cat: 'concept', text: 'A worker runs two storage nodes of the cluster, and one of them is removed. How does the host count of the worker\'s failure domain change for the rule?', options: ['It does not change, because another storage node of the cluster stays on the worker', 'It drops by one', 'It drops by two', 'The domain is left out of the check', 'The removal is refused for a shared worker'], correct: [0], explain: 'The rule counts hosts, which are workers. A worker leaves its domain only when no other storage node of the cluster stays on it.' },
    { topic: 'Census and admission', cat: 'concept', text: 'What does the control plane\'s read-only `removal-admission` check cover, as the operator\'s code describes it?', options: ['Fault-tolerance headroom', 'Replica relocation', 'Active tasks on the node', 'The annotations on the volumes\' claims', 'Whether the worker is cordoned'], correct: [0, 1, 2], explain: 'Those three are what the code names. The claims are the operator\'s census, and a cordon is a Kubernetes fact the control plane does not see.' },
    { topic: 'Census and admission', cat: 'concept', text: 'A removal holds in `Validating` with `RemovalNotAdmitted`. Which statements are true?', options: ['Nothing has been done to the node', 'The 24-hour budget of the step keeps running', 'Setting `spec.abort` ends it `Aborted`', 'The node is already offline', 'The admission is asked again on later passes'], correct: [0, 1, 2, 4], explain: '`Validating` has no side effect, a hold does not stop the deadline, and the step is abortable. The node is still serving, and the removal continues by itself once the condition clears.' },
    { topic: 'Census and admission', cat: 'action', text: 'The control plane answers 404 to `GET .../removal-admission`. What does the operator do?', options: ['Treats the check as not offered, so the control plane\'s admission runs only inside `prepare-removal`', 'Fails the removal', 'Holds with `RemovalNotAdmitted`', 'Removes the node without any admission', 'Retries the check until the step\'s deadline'], correct: [0], explain: 'A control plane without the endpoint still admits the removal in `prepare-removal`, which runs after the shutdown. The operator\'s own failure-domain rule is asked either way.' },
    { topic: 'The drain steps', cat: 'concept', text: 'What does the control plane do during `MigratingDevices`?', options: ['Rebuilds the data of the node\'s devices onto its peers', 'Copies each volume to a target the operator chose', 'Deletes the node\'s system volumes', 'Removes the storage labels from the worker', 'Restarts the node on another worker'], correct: [0], explain: '`prepare-removal` starts the rebuild. The volume copies come in `MigratingVolumes`, the system volumes are deleted in `Verifying`, the labels stay, and a restart elsewhere is `Migrate`.' },
    { topic: 'The drain steps', cat: 'concept', text: 'Which rules does the operator apply when choosing the target of a volume move?', options: ['Only peers the control plane reports online', 'Never a peer holding a replica of any volume of the subsystem', 'Peers whose secondary or tertiary is the drained node only when no other is left', 'The peer with the most free capacity first', 'On a stretched cluster, peers of the other site first'], correct: [0, 1, 2], explain: 'The choice is round-robin over eligible peers sorted by UUID, not by capacity. On a stretched cluster only peers of the drained node\'s own site qualify.' },
    { topic: 'The drain steps', cat: 'concept', text: 'No online peer is eligible for one of the subsystems. Which statements are true?', options: ['The step holds with `NoMigrationTarget`', 'The operation fails at once', 'The move goes to a node that holds a replica', 'The volume is skipped and the drain goes on to `Verifying`', 'The wait counts against the 12-hour budget of the step'], correct: [0, 4], explain: 'It is a stall that resolves when a node comes back or is added. Failing would only mean starting again later, and a replica holder is never a target.' },
    { topic: 'The drain steps', cat: 'field', text: 'Which labels does a PersistentVolumeOps raised by a drain carry?', options: ['`storage.simplyblock.io/drain-node` with the drained node\'s backend UUID', '`storage.simplyblock.io/target-node` with the target\'s backend UUID', '`storage.simplyblock.io/managed-by: StorageNodeOps`', '`storage.simplyblock.io/drain-node` with the StorageNode\'s object name', 'An owner reference to the StorageNodeOps'], correct: [0, 1, 2], explain: 'The node labels carry backend UUIDs. The kind is cluster-scoped, so it cannot be owned by the namespaced StorageNodeOps and names it in `spec.creatorRef` instead.' },
    { topic: 'The drain steps', cat: 'concept', text: 'The drained node holds twenty volumes in twenty NVMe-oF subsystems. Which statements about `MigratingVolumes` are true?', options: ['It raises one PersistentVolumeOps per subsystem', 'It raises one per volume, whatever the subsystems', 'The moves run one at a time, because the cluster has one migration slot', 'All twenty copy at once', 'It raises one per target peer'], correct: [0, 2], explain: 'The control plane moves a subsystem as a whole and runs one data migration per cluster, which the slot annotation on the StorageCluster enforces. Here every subsystem has one volume, so the counts coincide.' },
    { topic: 'The drain steps', cat: 'action', text: 'What does `Verifying` do?', options: ['Deletes the system volumes', 'Asks `verify-drained`, which also covers snapshots', 'Holds with `DrainBlocked` while anything is left on the node', 'Migrates the snapshots', 'Deletes the unmanaged volumes'], correct: [0, 1, 2], explain: 'The census sees no snapshots, so the control plane\'s answer closes the step. Nothing in a removal migrates snapshots or deletes unmanaged volumes.' },
    { topic: 'The drain steps', cat: 'concept', text: 'The graceful shutdown in `ShuttingDown` is answered with 409. Which statements are true?', options: ['The step holds with `RemovalDeferred` and asks again at most once a minute', 'The node keeps serving while it waits', 'The operation fails', 'The operator forces the shutdown', 'The step is skipped'], correct: [0, 1], explain: 'A 409 names a condition that clears by itself, such as a peer restarting. Forcing past it could take two nodes down at once before the admission has judged it.' },
    { topic: 'The drain steps', cat: 'concept', text: 'The control plane refuses `prepare-removal` for a reason that does not pass by itself. Which statements are true?', options: ['The operation fails, with the node left offline', 'A StorageNodeOps `Restart` brings the node back', 'The operator restarts the node by itself', 'The step holds with `RemovalDeferred`', 'The node is removed anyway'], correct: [0, 1], explain: 'Nothing in a removal brings a node back, and the failure message says a Restart does. Only refusals that pass by themselves hold with `RemovalDeferred`.' },
    { topic: 'The drain steps', cat: 'field', text: 'Which step budgets are right?', options: ['`Validating`: 24 hours', '`ShuttingDown`: 15 minutes', '`MigratingVolumes`: 12 hours', '`Verifying`: 30 minutes', '`Removing`: 2 hours'], correct: [0, 1, 2, 3, 4], explain: 'All five. `MigratingDevices` and `AwaitingRemoval` have 7 hours each, counted without progress.' },
    { topic: 'The drain steps', cat: 'concept', text: 'Which statements about the budgets of `MigratingDevices` and `AwaitingRemoval` are true?', options: ['Each change of the node\'s or a device\'s status moves the deadline a whole budget out', 'The budget is 7 hours without progress', 'They are total limits that never move', 'Both steps are abortable', 'A `removed_failed` status in `AwaitingRemoval` fails the operation'], correct: [0, 1, 4], explain: 'A rebuild that keeps moving is waited on for as long as it moves. Only `Validating` is abortable, and `removed_failed` is the control plane giving up.' },
    { topic: 'Phases and events', cat: 'concept', text: 'Which StorageNode phases does a node pass through during a removal?', options: ['`Offline`', '`Removing`', '`Removed`', '`Shrinking`', '`Draining`'], correct: [0, 1, 2], explain: 'The shutdown makes it `Offline`, the control plane\'s removal statuses map to `Removing`, and `removed` maps to `Removed`. `Shrinking` is the cluster\'s phase, and there is no `Draining`.' },
    { topic: 'Phases and events', cat: 'concept', text: 'Which statement about a StorageNode in phase `Removed` is true?', options: ['It is the last phase, and nothing brings the node back into service', 'A StorageNodeOps `Restart` returns it to `Online`', 'A StorageNodeOps `Resume` returns it to `Online`', 'The operator deletes the object', 'The operator strips the worker\'s labels'], correct: [0], explain: 'The type documents `Removed` as terminal. The object and the labels stay until somebody removes them.' },
    { topic: 'Phases and events', cat: 'concept', text: 'Which phase does the StorageCluster show while the control plane reports a removal in progress and the cluster is serving?', options: ['`Shrinking`', '`Rebalancing`', '`Degraded`', '`Removing`', '`Provisioning`'], correct: [0], explain: '`Shrinking` replaces `Online`, `Rebalancing`, and `Degraded` while a removal runs, because the removal is the cause of both the rebalance and the degradation. `Removing` is a StorageNode phase.' },
    { topic: 'Phases and events', cat: 'concept', text: 'Which events say the removal is waiting rather than failed?', options: ['`DrainBlocked`', '`NoMigrationTarget`', '`RemovalDeferred`', '`RemovalNotAdmitted`', '`StepDeadlineExceeded`'], correct: [0, 1, 2, 3], explain: 'The first four are holds, and the step\'s budget keeps running behind each of them. `StepDeadlineExceeded` comes with the failure.' },
    { topic: 'Phases and events', cat: 'field', text: 'Where on the StorageNodeOps is a removal\'s progress recorded?', options: ['`status.drain.volumesMigrated` against `status.drain.volumesTotal`', '`status.removal.devices`', '`status.removal.prepareAttempts`', '`status.step.state`', '`spec.nodeRef`'], correct: [0, 1, 2, 3], explain: 'The drain counters, the control plane\'s statuses for the node and its devices, the retry count, and the step are all status. `spec.nodeRef` names the node and never changes.' },
    { topic: 'Abort and aftermath', cat: 'concept', text: 'From which steps can a Remove be aborted?', options: ['`Validating`', '`ShuttingDown`', '`MigratingDevices`', '`MigratingVolumes`', '`Verifying`'], correct: [0], explain: '`Validating` changes nothing, so an abort there ends `Aborted` directly. From the shutdown onward the node is on its way out and nothing in the removal brings it back.' },
    { topic: 'Abort and aftermath', cat: 'action', text: '`spec.abort: true` is set on a removal in `MigratingVolumes`. What happens?', options: ['An `AbortRefused` event, and the removal runs on', 'The node comes back online', 'The moves already made are reversed', 'The operation ends `Aborted`', 'The volumes move back to the node'], correct: [0], explain: 'The step has no abort edge. The refusal is an event, and the operation keeps running to its end.' },
    { topic: 'Abort and aftermath', cat: 'concept', text: 'A removal written by hand has succeeded. Which of these does the operator do by itself afterward?', options: ['Deletes the StorageNode object', 'Strips the storage labels from the worker', 'Cordons the worker', 'Deletes the StorageNodeOps record', 'Removes the worker from the approved ClusterDeploymentConfig'], correct: [], explain: 'None of them. The StorageNode stays in `Removed`, the labels are stripped only by a `Migrate`, the record stays as an audit record, and an approved document is immutable.' },
    { topic: 'Abort and aftermath', cat: 'concept', text: 'Why is the Removed StorageNode deleted before its worker is cordoned?', options: ['The StorageNode reconcile raises a `HostMaintenance` operation for a node with a backend UUID whose worker is cordoned, and nothing on that path checks for `Removed`', 'Cordoning a worker deletes its StorageNode', 'Kubernetes refuses to cordon a worker with a StorageNode on it', 'The finalizer needs the worker schedulable to run', 'The control plane re-adds a node whose worker is cordoned'], correct: [0], explain: 'This is read from the code rather than observed: the cordon raises `<node>-maintenance`, and no guard for the `Removed` phase was found before it. The other options are invented.' },
    { topic: 'Abort and aftermath', cat: 'concept', text: 'A removal written by hand as `production-worker-5-0-remove` has succeeded, and the StorageNode is then deleted. What happens?', options: ['The finalizer finds the `Succeeded` record and releases the object without raising another removal', 'A second removal is raised', 'The deletion is refused', 'The node is added back', 'The record is deleted along with the StorageNode'], correct: [0], explain: 'The finalizer looks for `<node>-remove` by name and releases on `Succeeded`. A hand-written record has no owner reference to the node, so it stays.' },
    { topic: 'Abort and aftermath', cat: 'action', text: 'Somebody runs `kubectl delete snops` on a removal in `MigratingVolumes`. Which of these happen?', options: ['The drain stops', 'The node comes back online', 'The lock is released and the moves keep running unwatched', 'The moves are aborted and the volumes restored', 'The delete is admitted because the operation is `Running`'], correct: [], explain: 'None of them: the admission webhook refuses the delete at every step after `Validating` until the operation is terminal, because the record is what follows the removal to its end.' },
    { topic: 'Abort and aftermath', cat: 'concept', text: 'A removal raised by deleting a StorageNode is held in `Validating`, and `spec.abort` is set on it. Which statements are true?', options: ['It ends `Aborted` with nothing done', 'The StorageNode keeps its deletion timestamp and its finalizer', 'The node keeps serving', 'The deletion of the StorageNode is withdrawn', 'Deleting the `Aborted` record makes the finalizer raise a new removal'], correct: [0, 1, 2, 4], explain: 'An abort ends the operation, not the deletion. The finalizer holds because the removal did not succeed, and it raises a new one once no record of that name exists.' },
    { topic: 'Removing one drive', cat: 'action', text: 'Which operation takes one drive out of service without removing its node?', options: ['StorageDeviceOps with `action: Fail`', 'StorageDeviceOps with `action: Remove`', 'StorageDeviceOps with `action: Replace`', 'StorageNodeOps `Remove` with a list of devices', 'Editing the node\'s `pcieAllowList`'], correct: [0], explain: 'There is no bare `Remove` on a device, `Replace` is refused at admission, a node removal takes every device, and the allow list is not edited by hand.' },
    { topic: 'Removing one drive', cat: 'concept', text: 'Which statements about the steps of StorageDeviceOps `Fail` are true?', options: ['`Removing` is abortable', '`Failing` is not abortable', '`Awaiting` is not abortable', '`Failing` is abortable', 'The first step is `Requesting`'], correct: [0, 1, 2], explain: 'Nothing is issued while the operation sits in `Removing`. Once the device is out of the data path there is no call that puts it back. `Requesting` is the first step of `Restart` and `CheckHealth`.' },
    { topic: 'Removing one drive', cat: 'concept', text: 'Which statements about StorageDeviceOps `Fail` are true?', options: ['The failure cannot be undone', 'The cluster rebuilds the redundancy the device held', 'The operation waits for the rebuild to finish', 'A device that already reports failed is refused', 'The device\'s node is shut down'], correct: [0, 1, 3], explain: 'The rebuild outlives the operation, which waits only for the failure to be reported. One device is the narrowest thing the cluster can take out, and its node keeps serving.' },
    { topic: 'Removing one drive', cat: 'field', text: 'Which of these StorageDeviceOps actions are admitted today?', options: ['`Replace`', '`Migrate`', '`SelfTest`', '`Remove`', '`Add`'], correct: [], explain: 'None of them. The admitted actions are `Restart`, `Fail`, and `CheckHealth`. `Replace`, `Migrate`, and `SelfTest` wait on control-plane verbs, there is no bare `Remove`, and a new device is discovered rather than added.' },
  ],

  tasks: [
    {
      title: 'A removal that keeps the site\'s benchmark volumes out of the drain',
      scenario: 'Remove the storage node `edge-worker-3-0`. The site\'s own benchmark tool creates per-node volumes named `bench-...`, which should be deleted rather than migrated, and the rebalancer\'s `sb-fio-baseline-` volumes should still be treated as system volumes.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: edge-worker-3-0-remove\n  namespace: simplyblock\nspec:\n  nodeRef: {{node}}\n  action: {{action}}\n  remove:\n    systemVolumeFilterRegex: \'{{regex}}\'\n',
      blanks: {
        kind: { accept: ['StorageNodeOps'], hint: 'the node operation kind, short name `snops`' },
        node: { accept: ['edge-worker-3-0'], hint: 'the StorageNode by its object name' },
        action: { accept: ['Remove'], hint: 'drain, then remove' },
        regex: { accept: ['^(sb-fio-baseline-|bench-).*', '^(bench-|sb-fio-baseline-).*', '^(sb-fio-baseline-|bench-)', '^(bench-|sb-fio-baseline-)', '^sb-fio-baseline-.*|^bench-.*', '^bench-.*|^sb-fio-baseline-.*'], hint: 'the field replaces the default, so both prefixes are needed' },
      },
      explain: 'A `StorageNodeOps` with `action: Remove` names the node in `nodeRef`. Setting `systemVolumeFilterRegex` replaces `^sb-fio-baseline-.*`, so the pattern has to carry both prefixes, anchored so that no user volume matches by accident.',
    },
    {
      title: 'Calling off a held removal',
      scenario: 'The removal `production-worker-5-0-remove` is held in `Validating` with `RemovalNotAdmitted`, because its failure domain would drop below two hosts. Write the manifest as it reads after the one edit that calls it off with nothing done.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: StorageNodeOps\nmetadata:\n  name: production-worker-5-0-remove\n  namespace: simplyblock\nspec:\n  nodeRef: production-worker-5-0\n  action: {{action}}\n  {{field}}: {{value}}\n',
      blanks: {
        action: { accept: ['Remove'], hint: 'immutable, so it stays what it was' },
        field: { accept: ['abort'], hint: 'the only field of the spec that can change after creation' },
        value: { accept: ['true'], hint: 'honored in `Validating` only' },
      },
      explain: '`spec.abort: true` in `Validating` ends the operation `Aborted`, and the node keeps serving. `nodeRef` and `action` cannot change. After `Validating`, the same edit is refused with `AbortRefused`.',
    },
    {
      title: 'Failing one drive',
      scenario: 'One NVMe drive of the storage node `production-worker-5-0` keeps reporting media errors. The control plane\'s UUID for it begins `3f9c2a1b-`. Take only that drive out of service for good.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: production-worker-5-0-3f9c2a1b-fail\n  namespace: simplyblock\nspec:\n  deviceRef: {{device}}\n  action: {{action}}\n',
      blanks: {
        kind: { accept: ['StorageDeviceOps'], hint: 'the device operation kind, short name `sdops`' },
        device: { accept: ['production-worker-5-0-3f9c2a1b'], hint: 'the node\'s name and the first eight characters of the device UUID' },
        action: { accept: ['Fail'], hint: 'removes the device from the data path, then fails it' },
      },
      explain: 'A `StorageDeviceOps` names the StorageDevice, `<node>-<first eight characters of the UUID>`. `Fail` is irreversible once past `Removing`, and the cluster rebuilds the redundancy the drive held while the node keeps serving.',
    },
  ],
});
