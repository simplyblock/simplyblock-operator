// Chapter: relocating one storage node onto another Kubernetes worker with a
// StorageNodeOps of action Migrate. It sits beside the removal and expansion
// chapters because the three answer one question together, which tool changes
// the host a node runs on, and this one covers the case where one machine
// replaces another and the node keeps its identity. The steps, budgets, and
// events are those of operator/internal/controllers/node/migrate.go and
// graphs.go, and design-storagenode.md §9 is the specification behind them.
TRAINING.register({
  id: 'node-host-migration',
  title: 'Migrating a storage node to a different host',
  summary: 'Relocating one storage node onto another Kubernetes worker with a StorageNodeOps of action `Migrate`: the node keeps its UUID and its volumes, and its data is rebuilt onto the target host\'s own drives.',
  goal: 'after the chapter the reader can tell when a change of host calls for a migration, a maintenance window, or a removal followed by an add, prepare a target worker, write the `Migrate` operation, follow its four steps, and know which of them can still be called off.',

  sections: [
    {
      heading: 'Which tool changes the host a node runs on',
      body: [
        'A storage node is one SPDK process on one Kubernetes worker, and its StorageNode object names that worker in `spec.workerNode`. Moving the node to another worker is an operation of its own: a StorageNodeOps with `action: Migrate`. The node keeps its backend UUID, its partitions, its logical-volume assignments, and its slot, and what changes is the machine the SPDK process runs on. No PersistentVolumeOps is created, and the StorageNode is the same object before and after.',
        'The commissioned title says the node moves to a different host, and that is what the operation does. One point is adjusted to the code: the drives do not move with it. The operator asks the control plane to promote the node on the target, and according to the operator\'s design the promote activates the target host\'s drives, fails and migrates the origin host\'s, starts a rebalance, and re-homes the volumes. The data is rebuilt onto the target\'s own NVMe devices. Moving one drive together with its contents is StorageDeviceOps `Migrate`, which is declared and refused at admission.',
        { table: { head: ['Situation', 'Tool', 'What happens to the node'], rows: [
          ['A host is retired, or another machine replaces its hardware', 'StorageNodeOps `Migrate`', 'Same UUID and volumes, data rebuilt onto the target\'s drives, `spec.workerNode` re-pointed'],
          ['The same host is rebooted, upgraded, or repaired and comes back with its drives', 'StorageNodeOps `HostMaintenance`, raised by the operator when the worker is cordoned', 'Shut down, held while the host is away, restarted on the same worker'],
          ['The cluster shrinks, or the node is not coming back anywhere', '[Removing a node](chapter.html?c=node-removal), StorageNodeOps `Remove`', 'Drained, removed from the control plane, and `Removed` for good'],
          ['A new host joins as additional capacity', '[Expanding a cluster](chapter.html?c=cluster-expansion), a growth ClusterDeploymentConfig', 'A new StorageNode with a new UUID'],
        ] } },
        'A removal followed by an add reaches a similar end by a longer road. The removal moves every volume off the node, one NVMe-oF subsystem at a time, and rebuilds its devices onto the peers. The node ends `Removed`, and the new node joins with a new UUID that the cluster then rebalances onto. That is the right tool when capacity is not replaced one for one. When one machine replaces another, `Migrate` keeps the identity and skips the drain.',
      ],
    },
    {
      heading: 'Preparing the target worker',
      body: [
        'The operator checks the target in the first step, `Preparing`, and fails the operation when a check does not hold. These are the checks that end a migration early:',
        { list: [
          '**The target is a node of this Kubernetes cluster.** Otherwise the operation fails with `target worker <name> is not a node of this Kubernetes cluster`.',
          '**The target is `Ready`.** A worker that is not fails the operation with `target worker <name> is not Ready`. The operator does not wait for it.',
          '**The node exists in the control plane.** A StorageNode without `status.uuid` fails the operation as not provisioned yet. A StorageCluster without one does not fail it: the cluster gate holds it in `Pending`.',
          '**The node belongs to this site.** On a stretched cluster the webhook refuses a StorageNodeOps for the other site\'s nodes at creation, because that site\'s operator runs its operations.',
        ] },
        'Three more conditions come from reading the code. The operator checks none of them, and each one matters:',
        { list: [
          '**The drives.** The target boots from a copy of the source\'s entry in the per-node configuration, `<cluster>-per-node-config`, whose PCI allow list holds the source\'s addresses. A target of the same hardware model, with its drives at the same addresses, needs nothing more. Drives at other addresses are named in `spec.migrate.newSsdPcie`, and they are added to the copy before the target is labeled. What the node agent does with that list on the host is outside this repository.',
          '**No other node of the same cluster on the target.** The per-node configuration holds one entry per worker, which the copy overwrites, and the worker\'s topology label is keyed by slot number. `Preparing` does not refuse a target that already runs a node of this cluster, and nothing in the code handles that case, so choose a worker that runs none.',
          '**The source is not cordoned yet.** A cordoned worker makes the operator raise a `HostMaintenance` named `<node>-maintenance` for the node on it, which takes the node\'s lock and shuts the node down. The migration then waits behind it with `OperationQueued`, and the window waits for an uncordon. Migrate first, and cordon and drain the retired host once the node has left it.',
        ] },
        'The cluster also has to be `active` in the control plane and not rebalancing when the operation is admitted, which the section on admission covers. Migrations of several nodes therefore run one after another: the promote of one starts a rebalance, and the next is held until it settles. A migration adds no node, so it takes no provisioning slot and is not sequenced the way node adds on FoundationDB hosts are.',
      ],
    },
    {
      heading: 'Writing the operation',
      body: [
        'A migration is one object. `spec.nodeRef` names the StorageNode, `spec.action` is `Migrate`, and `spec.migrate.targetWorkerNode` names the worker. The API refuses a change to any of the three once the object exists, so a migration to a different worker is a new StorageNodeOps.',
        { table: { head: ['Field', 'Required', 'Changeable', 'What it does'], rows: [
          ['`spec.nodeRef`', 'yes', 'no', 'The StorageNode to move, in the same namespace. The operation never owns it.'],
          ['`spec.action`', 'yes', 'no', '`Migrate`.'],
          ['`spec.migrate.targetWorkerNode`', 'yes, for `Migrate`', 'no', 'The worker the node moves to. Left out, the operation fails with `spec.migrate.targetWorkerNode is empty`.'],
          ['`spec.migrate.newSsdPcie`', 'no', 'not enforced', 'PCI addresses to bind on the target: added to its copied configuration, sent with the restart as `new_ssd_pcie`, and merged into the node\'s `spec.config.pcieAllowList` at the promote.'],
          ['`spec.reattachVolume`', 'no', 'not enforced', 'Sent with the relocation restart as `reattach_volume`. What the control plane does with it is not described in this repository.'],
          ['`spec.force`', 'no', 'not enforced', 'True when unset, for `Migrate` alone. The node is online when it is relocated, and the control plane refuses an unforced restart of a node that is not offline.'],
          ['`spec.abort`', 'no', 'yes', 'Calls the operation off. Honored in `Preparing` only.'],
        ] } },
        'The type\'s own comment calls `spec.abort` the only mutable field, and the schema enforces that for `nodeRef`, `action`, and `targetWorkerNode` alone. An edit to `force`, `reattachVolume`, or `newSsdPcie` is admitted and read by whichever step runs next, so a list changed after `Preparing` reaches the restart and the allow list but not the configuration the target booted from. Treat all three as fixed.',
        { callout: 'Leave `spec.force` unset. Set to `false`, the relocation restart goes out unforced, the control plane refuses it for an online node, and the operation stays in `Relocating` until its 15-minute budget fails it.', kind: 'warn' },
        { code: 'kubectl apply -f 01-storagenodeops-migrate.yaml\nkubectl get snops -n simplyblock -w', title: 'Starting a migration and following it' },
      ],
      examples: [
        { title: 'A migration onto identical hardware', file: 'examples/node-host-migration/01-storagenodeops-migrate.yaml', notes: [
          { at: 'kind: StorageNodeOps', text: 'Short name `snops`. The record stays after the operation ends, as the audit of what was done to which node.' },
          { at: 'nodeRef: production-worker-3-0', text: 'The StorageNode by its object name. The name was derived from the cluster, the worker, and the slot when the node was created, and it keeps naming `worker-3` after the move.' },
          { at: 'action: Migrate', text: 'Relocation with the identity kept. `Remove` would drain the node and end it.' },
          { at: 'targetWorkerNode: worker-5', text: 'Required and immutable. A worker that does not exist or is not `Ready` fails the operation in `Preparing`.' },
        ] },
        { title: 'A migration that binds new drives', file: 'examples/node-host-migration/02-storagenodeops-migrate-new-drives.yaml', notes: [
          { at: 'name: move-production-worker-3-0-new-drives', text: 'A name of its own. Operation records are kept, so a second migration of the same node is a second object.' },
          { at: 'reattachVolume: true', text: 'Passed to the relocation restart as `reattach_volume`. The operator only forwards it.' },
          { at: 'newSsdPcie:', text: 'Drives the target has at addresses the source did not. Without them the target is told to bind the source\'s addresses alone.' },
          { at: '"0000:88:00.0"', text: 'Quoted, so that every YAML parser reads the address as a string. After the promote it is also in the StorageNode\'s allow list.' },
        ] },
      ],
    },
    {
      heading: 'Admission and the four steps',
      body: [
        'A new operation is `Pending` and holds nothing. Two things stand between it and its first step, and both leave it `Pending` with the reason in `status.message`.',
        { steps: [
          '**The cluster gate.** The operator reads the cluster from the control plane: it has to be `active` and not rebalancing. Otherwise, the operation stays `Pending`, and a `ClusterNotReady` event gives the reason, such as `cluster production is rebalancing; the operation resumes when it settles`. `Migrate` and `HostMaintenance` wait at this gate because they move data. `Remove`, `Shutdown`, `Restart`, `Suspend`, and `Resume` skip it.',
          '**The node lock.** The operation writes its own name into the StorageNode\'s `status.activeOpsRef` with an optimistic-lock patch. When another operation holds it, this one waits with an `OperationQueued` event naming the holder. With the lock taken, the phase becomes `Running`.',
        ] },
        'The gate is asked once, before the lock, and never again. A migration restarts its node, which leaves the cluster degraded until the node is back, and asking the gate on every pass would hold the operation on the result of its own work. From there on each step is bounded by its own budget instead.',
        { table: { head: ['Step', 'Budget', 'Abortable', 'What the operator does', 'Done when'], rows: [
          ['`Preparing`', '15 min', 'yes', 'Checks the target, copies the node\'s per-node configuration onto it with `newSsdPcie` merged in, and labels it into the storage plane', 'The target\'s storage-node pod is `Ready` and its address is published in the EndpointSlice'],
          ['`Relocating`', '15 min', 'no', '`POST /api/v2/clusters/{cluster}/storage-nodes/{node}/restart` with the target\'s `node_address`, `force`, `reattach_volume`, and `new_ssd_pcie`', 'The node has left `online`'],
          ['`AwaitingNode`', '45 min', 'no', 'Nothing: it watches the restart', 'The node is `online` again'],
          ['`Promoting`', '30 min', 'no', '`POST .../storage-nodes/{node}/promote`, then the re-point of the StorageNode and the release of the source worker', 'The re-point and the release are applied'],
        ] } },
        '`Preparing` waits on DNS and not only on the pod. The control plane resolves `node_address` itself, a name that does not resolve makes the restart fail inside the control plane, and the pod turns `Ready` before its EndpointSlice is published. With a managed control plane, which cannot resolve this cluster\'s Service names, the address is the target worker\'s internal IP on port 5000 instead.',
        '`Relocating` and `AwaitingNode` are two steps because the restart is asynchronous. Right after the call the node can still report `online` from before it, so the operation first waits for the node to leave `online` and then for it to return. `Promoting` reads the node again and waits while it is not `online`, so a promote into a restart still in flight is not issued.',
        'A step that outlives its budget fails the operation with a `StepDeadlineExceeded` event, and `status.step` keeps the step it stopped in. The four budgets add up to 105 minutes, which is roughly the longest an admitted migration runs before it succeeds or fails.',
      ],
    },
    {
      heading: 'Promoting: the control plane\'s half and the operator\'s',
      body: [
        '`Promoting` is the step with no way back, and two systems share its work.',
        { list: [
          '**The control plane** receives the promote. According to the operator\'s design and code comments, it activates the target host\'s drives, fails and migrates the origin host\'s drives, starts a rebalance, and re-homes the node\'s logical volumes. The drives are not carried across: the data is rebuilt onto the target\'s own NVMe. The control plane\'s implementation is not in this repository, and nothing in it states how long the rebuild takes.',
          '**The operator** re-points the Kubernetes view after the promote and never before it. `spec.workerNode` becomes the target, the `newSsdPcie` addresses are appended to `spec.config.pcieAllowList`, the target worker gets the node\'s slot label, and the source worker loses its storage-plane labels if no other node of the cluster is left on it. A two-socket source that still runs the other socket\'s node keeps them.',
        ] },
        'The promote and the re-point are one claimed call, and the re-point is the record that the promote landed: a pass that finds `spec.workerNode` already naming the target does not promote again. The step ends once the source is released. It does not wait for the rebalance, so a migration can read `Succeeded` while the control plane is still moving data, and the next migration waits at the cluster gate meanwhile.',
        'The labels are the ones the storage-node DaemonSet and the CSI driver read. `io.simplyblock.storagenodeset=<cluster>` selects the workers the DaemonSet runs on, and `simplyblock.io/storage-node-uuid.<cluster-uuid>.<slot>` carries the node\'s UUID for the CSI topology. The slot is immutable, so the key the target gets is the key the source carried.',
        { callout: '`spec.workerNode` cannot be edited by hand. The StorageNode webhook admits a change to it, to `spec.socketId`, to `spec.nodeIndex`, or to anything under `spec.config` only from a service account in the operator\'s namespace, and refuses everyone else with a message that says to relocate a node with a StorageNodeOps of action `Migrate`. An edit would describe a relocation the control plane never made, with the SPDK process still running on the old host.', kind: 'warn' },
      ],
      examples: [
        { title: 'The StorageNode after the promote', file: 'examples/node-host-migration/03-storagenode-after-promotion.yaml', notes: [
          { at: 'name: production-worker-3-0', text: 'Unchanged. The name records where the node was created, and `spec.workerNode` records where it runs.' },
          { at: 'workerNode: worker-5', text: 'Re-pointed by the promote. The webhook refuses the same change from anyone but the operator.' },
          { at: 'slot: 0', text: 'Immutable. The target\'s topology label is keyed by it, so the key is the one the source carried.' },
          { at: 'deviceNames:', text: 'Immutable and untouched by the migration. The per-node configuration gives the target these addresses together with the allow list.' },
          { at: 'pcieAllowList:', text: 'The `newSsdPcie` addresses, appended at the promote. A migration is the only writer of this list, which is why the webhook guards it rather than an immutability marker.' },
          { at: 'uuid: 4b1e8f0a', text: 'The backend UUID from before the move. A removal and an add would have produced a new one.' },
        ] },
      ],
    },
    {
      heading: 'Calling it off, and following it',
      body: [
        'Set `spec.abort: true` on the running operation. The machine honors it in `Preparing` only, where the operator has written the target\'s configuration and labels and has asked nothing of the control plane: the phase becomes `Aborted`, with an `OperationAborted` event. From `Relocating` on, the abort is refused with an `AbortRefused` event and the operation runs to its end, because the node is part-way through a restart that only this operation watches back.',
        'An abort in `Preparing` does not take back what the step did. The target keeps its storage-plane label and its copied configuration entry. The only code that strips a worker is a later migration\'s `Promoting`, which removes the labels from every labeled worker that runs no node of the cluster. A new `Migrate` to the same target finds the label and the entry in place.',
        'Deleting the record is no shortcut. The StorageNodeOps webhook refuses a delete while the operation is at `Relocating`, `AwaitingNode`, or `Promoting`, because the record is the only thing accounting for the work and the node would stay locked by an object that no longer exists. A `Pending` operation, one in `Preparing`, and a finished one can be deleted.',
        { table: { head: ['Event', 'Type', 'When'], rows: [
          ['`ClusterNotReady`', 'Warning', 'While `Pending`: the cluster is not `active`, is rebalancing, or has no UUID'],
          ['`OperationQueued`', 'Normal', 'While `Pending`: another operation holds the node\'s lock'],
          ['`OperationStarted`', 'Normal', 'The lock is taken and `Preparing` begins'],
          ['`AbortRefused`', 'Warning', '`spec.abort` arrived at `Relocating` or later'],
          ['`OperationAborted`', 'Normal', 'The abort was honored in `Preparing`'],
          ['`StepDeadlineExceeded`', 'Warning', 'A step outlived its budget, followed by `OperationFailed`'],
          ['`OperationSucceeded`, `OperationFailed`', 'Normal, Warning', 'The operation reached its terminal phase'],
        ] } },
        'Each event is recorded on the StorageNodeOps and again on the StorageNode, so `kubectl describe sn` shows the history of every operation that touched the node.',
        { code: 'kubectl get snops -n simplyblock\nkubectl get snops move-production-worker-3-0 -n simplyblock -o wide\nkubectl describe sn production-worker-3-0 -n simplyblock\nkubectl get sn production-worker-3-0 -n simplyblock', title: 'Watching a migration' },
        'The print columns of `kubectl get snops` are the node, the action, the phase, and the step, and `-o wide` adds `status.message`. On the StorageNode, the `Worker` column changes to the target at the promote.',
      ],
      examples: [
        { title: 'Calling the first migration off', file: 'examples/node-host-migration/04-storagenodeops-migrate-abort.yaml', notes: [
          { at: 'name: move-production-worker-3-0', text: 'The same object as the first example, edited in place. An abort is a change to a running operation, not a new one.' },
          { at: 'abort: true', text: 'Honored in `Preparing`. At any later step the operator emits `AbortRefused` and carries on.' },
          { at: 'targetWorkerNode: worker-5', text: 'Unchanged. The API refuses a new target on an existing object.' },
        ] },
      ],
    },
    {
      heading: 'A same-host reboot is a maintenance window',
      body: [
        'When the host comes back with the same drives, nothing has to move and `Migrate` is the wrong tool. The usual path is the Kubernetes one, `kubectl cordon` and `kubectl drain`. The operator sees the cordon and raises a `HostMaintenance` operation named `<node>-maintenance`. The window shuts the node down gracefully behind a PodDisruptionBudget that makes the drain wait for it, lets the drain through, and restarts the node once the worker is schedulable again and its API answers.',
        { table: { head: ['Step', 'Budget', 'Abortable', 'Done when'], rows: [
          ['`Holding`', '6 h', 'yes', 'Fewer workers are in maintenance than the cluster\'s `status.maxConcurrentWorkerRestarts`'],
          ['`ShuttingDown`', '15 min', 'no', 'The node is offline, with the disruption budget in place'],
          ['`Releasing`', '15 min', 'no', 'The SPDK pod has left the worker, after the budget is deleted'],
          ['`AwaitingHost`', '4 h', 'no', 'The worker is schedulable again and its storage-node API answers'],
          ['`Restarting`', '45 min', 'no', 'The node is `online`'],
          ['`Cleanup`', '5 min', 'no', 'The budget and the drain label are gone'],
        ] } },
        'A window written by hand is accepted and behaves the same. Give it a name other than `<node>-maintenance`: the operator calls off a window of that name while the worker is schedulable and the window has not left `Holding`. An `AwaitingHost` that outlives its four hours fails the operation and leaves the node offline, and a `Restart` operation brings it back.',
        'The two actions share their mechanism. A migration\'s relocation is the same control-plane restart a maintenance window brings the node back with, aimed at another host\'s address. Both wait at the cluster gate, both take the node\'s lock, and both can be called off in their first step only.',
      ],
      examples: [
        { title: 'A maintenance window written by hand', file: 'examples/node-host-migration/05-storagenodeops-hostmaintenance.yaml', notes: [
          { at: 'name: production-worker-3-0-firmware', text: 'Not `<node>-maintenance`, which is the name the operator gives the window it raises and calls off on a schedulable worker.' },
          { at: 'action: HostMaintenance', text: 'The node returns to the worker it left. No `spec.migrate` block, and no change to `spec.workerNode`.' },
          { at: 'reattachVolume: true', text: 'Applies to `Restart`, `Migrate`, and `HostMaintenance`, and is passed to the restart that ends the window.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong, and what is not verified',
      body: [
        { pitfalls: [
          { symptom: 'The operation stays `Pending` with `ClusterNotReady` events.', cause: 'The cluster is not `active` in the control plane, or it is rebalancing, often after the previous migration\'s promote.', fix: 'Wait. The gate is asked again on every retry, and the operation starts on its own once the cluster settles.' },
          { symptom: 'The operation stays `Pending` with `OperationQueued` naming `<node>-maintenance`.', cause: 'The source worker was cordoned before the migration, and the maintenance window the cordon raised holds the node\'s lock.', fix: 'Uncordon the worker. A window still in `Holding` is called off. One that has shut the node down restarts it on the source and finishes, and the migration takes the lock after it.' },
          { symptom: 'The operation fails in `Preparing` with `target worker worker-5 is not Ready` or `is not a node of this Kubernetes cluster`.', cause: 'The target is not `Ready`, or the name is wrong. The step refuses rather than waits.', fix: 'Bring the worker to `Ready` or correct the name, and create a new StorageNodeOps. A failed record is not resumed, and the target is immutable.' },
          { symptom: '`Preparing` fails with `StepDeadlineExceeded`.', cause: 'The target\'s storage-node pod was not `Ready` with its address published within 15 minutes.', fix: 'Look at the pod on the target. Nothing was asked of the control plane, and the label and copied configuration stay, so a new StorageNodeOps repeats `Preparing` against a pod that may be up by then.' },
          { symptom: 'An edit of `spec.workerNode` on the StorageNode is refused.', cause: 'The webhook admits that field from the operator\'s service accounts alone.', fix: 'Write a StorageNodeOps with `action: Migrate`.' },
          { symptom: '`Relocating` fails after 15 minutes with the node still `online`.', cause: '`spec.force: false`, so the control plane refused the unforced restart, or the operator never saw the node leave `online` (see below).', fix: 'Leave `spec.force` unset. No recovery procedure is written down for a migration that failed after its restart went out, so read the node\'s `status.status` and the control plane\'s view of it before acting.' },
          { symptom: 'A `Migrate` to the worker the node already runs on restarts the node.', cause: 'The code skips `Preparing` when the target is the current worker, and only that step. `Relocating` still sends a forced restart aimed at that worker, and `Promoting` skips the promote.', fix: 'Do not run a finished migration again. Use `Restart` when a restart is what is wanted.' },
          { symptom: '`spec.abort: true` is set and the migration keeps running, with an `AbortRefused` event.', cause: 'The abort arrived at `Relocating` or later, where the restart is under way.', fix: 'Let it finish. A delete of the record is refused at those steps as well.' },
        ] },
        { callout: 'Not validated end to end. The unit tests drive each step against a fake control plane. The end-to-end rows of the storage-node test plan, a migration that keeps its UUID with its volumes following, one under sustained I/O, and one with `newSsdPcie`, need a live cluster and none of them is automated. Three unit rows have no test: the abort in `Preparing`, and the source worker losing or keeping its labels. Rehearse a migration on a cluster that carries no production data first.', kind: 'warn' },
        'One question is open. `Relocating` completes on a negative observation, the node no longer reporting `online`, and the operator follows the control plane through a stream that can coalesce updates. A quick restart can then show `online` before and `online` after without the departure ever being delivered, and the step waits out its 15 minutes and fails. The design asks the control plane for a restart generation or another monotonic counter, which turns the signal into a positive one, and records the request as question 4 of `design-storagenode.md` §16.',
      ],
    },
  ],

  questions: [
    { topic: 'Choosing the tool', cat: 'concept', text: 'Which of these does a node keep through a `Migrate`?', options: ['Its backend UUID', 'Its logical-volume assignments', 'Its slot', 'Its `spec.workerNode`', 'The physical drives it used on the source host'], correct: [0, 1, 2], explain: 'A migration changes the machine the SPDK process runs on and nothing else about the node\'s identity. `spec.workerNode` is re-pointed at the promote, and the data is rebuilt onto the target\'s drives.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'A host is retired and an identical machine replaces it. Which operation moves the node and keeps its identity?', options: ['StorageNodeOps `Migrate`', 'StorageNodeOps `Remove`, then a growth ClusterDeploymentConfig', 'StorageNodeOps `HostMaintenance`', 'StorageDeviceOps `Migrate` for each drive', 'An edit of `spec.workerNode` on the StorageNode'], correct: [0], explain: 'Remove and add gives a new UUID, a maintenance window brings the node back to the same host, StorageDeviceOps `Migrate` is refused at admission, and the webhook refuses the edit.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'According to the operator\'s design, what happens to the data of a node that is migrated?', options: ['It is rebuilt onto the target host\'s own NVMe devices', 'The origin host\'s drives are failed and migrated', 'The drives are carried to the new host and attached there', 'A PersistentVolumeOps moves each volume', 'It stays on the origin host and is read over the network'], correct: [0, 1], explain: 'The promote activates the target\'s drives, fails and migrates the origin\'s, starts a rebalance, and re-homes the volumes. No PersistentVolumeOps is created, and the control plane\'s side of this is not in the repository.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'A worker is rebooted for a kernel upgrade and comes back with its drives. Which of these is the right tool?', options: ['Cordon and drain it, and let the operator raise `HostMaintenance`', '`Migrate` the node to another worker and back', '`Remove` the node and add it again', 'Edit `spec.workerNode` to a placeholder during the reboot', 'Delete the StorageNode and let it be created again'], correct: [0], explain: 'A same-host reboot is a maintenance window: nothing has to move. Deleting the StorageNode raises a `Remove`, which ends the node.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'Which of these are true of removing a node and adding another instead of migrating it?', options: ['The new node gets a new backend UUID', 'The removal moves the volumes off with PersistentVolumeOps', 'The removed StorageNode ends in phase `Removed`', 'It is the right tool when capacity is not replaced one for one', 'It keeps the slot label on the old worker'], correct: [0, 1, 2, 3], explain: 'A removal drains the node and ends it, and an add creates a new node with a new identity. It suits a shrink or a later growth rather than a one-for-one replacement.' },
    { topic: 'Choosing the tool', cat: 'concept', text: 'Which of these does a migration do to the Kubernetes objects of the node\'s volumes?', options: ['Creates a PersistentVolumeOps per volume', 'Rewrites each PersistentVolume\'s node affinity', 'Deletes and recreates the claims', 'Labels each claim with `storage.simplyblock.io/drain-node`', 'Pins each claim to the target with `storage.simplyblock.io/selected-storage-node`'], correct: [], explain: 'None of them. A migration touches the StorageNode, the per-node configuration, and the worker labels, and the volumes stay assigned to the same backend node. The drain label and the volume moves belong to a removal.' },

    { topic: 'The target worker', cat: 'field', text: 'Which conditions on the target does `Preparing` check, failing the operation when they do not hold?', options: ['It is a node of this Kubernetes cluster', 'It is `Ready`', 'It runs no other node of the same cluster', 'Its drives sit at the source\'s PCI addresses', 'It is not cordoned'], correct: [0, 1], explain: 'The step reads the worker and refuses one that does not exist or is not `Ready`. The other three are worth checking by hand, and the operator checks none of them.' },
    { topic: 'The target worker', cat: 'concept', text: 'The target\'s drives sit at PCI addresses the source never had. What makes the target bind them?', options: ['Listing them in `spec.migrate.newSsdPcie`', 'Editing `spec.config.deviceNames` on the StorageNode', 'Nothing, the target binds every NVMe device it finds', 'A StorageDeviceOps `Migrate` per drive', 'A new discovery run against the target'], correct: [0], explain: 'The target boots from a copy of the source\'s configuration, whose allow list holds the source\'s addresses. `newSsdPcie` adds to it. `deviceNames` is immutable, and the other answers do nothing for a migration.' },
    { topic: 'The target worker', cat: 'action', text: 'An administrator cordons the retiring worker first, then creates the `Migrate`. Which of these follow from the code?', options: ['The operator raises a `HostMaintenance` named `<node>-maintenance`', 'The migration waits with `OperationQueued` while the window holds the lock', 'The migration skips the lock because a user created it', 'The window is turned into the migration', 'The cordon is ignored for a node with a migration pending'], correct: [0, 1], explain: 'A cordon raises a maintenance window for every node on the worker, and the window takes the lock. Migrate first and cordon afterward.' },
    { topic: 'The target worker', cat: 'concept', text: 'Which of these does the operator check before it migrates a node onto a worker?', options: ['That the worker has as many CPU cores as the source', 'That the worker\'s NIC names match the source\'s', 'That the worker has enough huge pages', 'That the worker runs no FoundationDB process', 'That the worker\'s drives are empty'], correct: [], explain: 'None of them. `Preparing` checks that the worker exists and is `Ready`, and then waits for the storage-node pod on it to be `Ready` and published in DNS.' },

    { topic: 'The operation', cat: 'field', text: 'Which fields are required for a `Migrate` StorageNodeOps to run?', options: ['`spec.nodeRef`', '`spec.action`', '`spec.migrate.targetWorkerNode`', '`spec.migrate.newSsdPcie`', '`spec.force`'], correct: [0, 1, 2], explain: 'The node, the action, and the target. Without a target the operation fails with `spec.migrate.targetWorkerNode is empty`. The drive list is optional, and force defaults to true for a migration.' },
    { topic: 'The operation', cat: 'field', text: 'Which fields does the API server refuse to change once a StorageNodeOps exists?', options: ['`spec.nodeRef`', '`spec.action`', '`spec.migrate.targetWorkerNode`', '`spec.abort`', '`spec.reattachVolume`'], correct: [0, 1, 2], explain: 'Those three carry an immutability rule in the schema. `spec.abort` is meant to change, and `reattachVolume`, `force`, and `newSsdPcie` carry no rule even though the type\'s comment calls them fixed.' },
    { topic: 'The operation', cat: 'field', text: 'What does `spec.force` do on a `Migrate`?', options: ['It defaults to true when unset', 'Set to false, it makes the control plane refuse the restart of an online node', 'It skips the cluster gate', 'It lets an abort be honored after `Preparing`', 'It removes the node instead of moving it'], correct: [0, 1], explain: 'The relocated node is online, and the control plane refuses an unforced restart of a node that is not offline, so a migration forces it unless told otherwise.' },
    { topic: 'The operation', cat: 'field', text: 'Where do the `newSsdPcie` addresses go?', options: ['Into the target\'s copied per-node configuration, before the target is labeled', 'Into the relocation restart as `new_ssd_pcie`', 'Into the StorageNode\'s `spec.config.pcieAllowList` at the promote', 'Into the StorageNode\'s `spec.config.deviceNames`', 'Into a StorageDevice object the migration writes for each'], correct: [0, 1, 2], explain: 'They reach the host through the configuration, the control plane through the restart, and the StorageNode through the allow list, where a later rebuild finds them. `deviceNames` is immutable.' },
    { topic: 'The operation', cat: 'action', text: 'A migration was created with the wrong target and is still `Pending`. What is the fix?', options: ['Delete it, or set `spec.abort`, and create a new StorageNodeOps with the right target', 'Edit `spec.migrate.targetWorkerNode`', 'Edit the StorageNode\'s `spec.workerNode`', 'Patch the status step to `Preparing`', 'Wait for the operator to choose the nearest `Ready` worker'], correct: [0], explain: 'The target is immutable, and a `Pending` operation has done nothing, so it can be deleted or aborted and written again.' },

    { topic: 'Gate and lock', cat: 'concept', text: 'Which of these hold a new `Migrate` in `Pending`?', options: ['The cluster is not `active` in the control plane', 'The cluster is rebalancing', 'Another operation holds the node\'s lock', 'The target worker is not `Ready`', 'The cluster turns degraded because of the restart, after admission'], correct: [0, 1, 2], explain: 'The cluster gate and the lock hold an operation in `Pending`. A target that is not `Ready` fails it in `Preparing`, and the gate is never asked again once the operation is admitted.' },
    { topic: 'Gate and lock', cat: 'field', text: 'Where is the node lock kept?', options: ['`StorageNode.status.activeOpsRef`', 'A Lease in the operator\'s namespace', 'An annotation on the StorageNodeOps', '`StorageCluster.status.activeOpsRef`', 'A label on the worker'], correct: [0], explain: 'The operation writes its name into the node\'s status with an optimistic-lock patch, and clears it when it reaches a terminal phase.' },
    { topic: 'Gate and lock', cat: 'concept', text: 'Which of these actions skip the cluster gate?', options: ['`Remove`', '`Restart`', '`Shutdown`', '`Migrate`', '`HostMaintenance`'], correct: [0, 1, 2], explain: '`Remove`, `Shutdown`, `Restart`, `Suspend`, and `Resume` run whatever the cluster reports. `Migrate` and `HostMaintenance` move data and wait for an active cluster that is not rebalancing.' },
    { topic: 'Gate and lock', cat: 'concept', text: 'Why is the cluster gate asked only before the lock, and never of a running migration?', options: ['The migration restarts the node, which leaves the cluster degraded until the node is back', 'Each running step is bounded by its own budget instead', 'The control plane forbids reading the cluster during a restart', 'A running migration holds the cluster\'s lock', 'The gate is too slow to ask more than once'], correct: [0, 1], explain: 'Asking on every pass would hold the operation on the result of its own work. Deadlines bound the steps instead.' },

    { topic: 'Steps and abort', cat: 'concept', text: 'In which steps of a `Migrate` is `spec.abort` honored?', options: ['`Preparing`', '`Relocating`', '`AwaitingNode`', '`Promoting`', '`Holding`'], correct: [0], explain: 'Only `Preparing`, before anything is asked of the control plane. `Holding` belongs to `HostMaintenance`.' },
    { topic: 'Steps and abort', cat: 'concept', text: 'Which pairings of a migration step and its budget are right?', options: ['`Preparing`, 15 minutes', '`Relocating`, 15 minutes', '`AwaitingNode`, 45 minutes', '`Promoting`, 30 minutes', 'The whole operation, 24 hours'], correct: [0, 1, 2, 3], explain: 'The four budgets add up to 105 minutes. There is no budget for the operation as a whole.' },
    { topic: 'Steps and abort', cat: 'concept', text: 'Why are `Relocating` and `AwaitingNode` two steps?', options: ['The restart is asynchronous, and the node can still report `online` from before it', 'A promote issued while the restart is in flight leaves the relocated devices stuck in `new`', 'The control plane needs two calls for one restart', 'The first step drains the volumes and the second restarts the node', 'They run on two different workers'], correct: [0, 1], explain: 'The first waits for the node to leave `online` and the second for it to come back, so the promote is not issued into a restart that has not finished.' },
    { topic: 'Steps and abort', cat: 'action', text: 'An abort arrives while the migration is in `AwaitingNode`. Which of these happen?', options: ['An `AbortRefused` event', 'The operation runs on', 'The phase becomes `Aborted`', 'The node is restarted back on the source', 'A delete of the record is refused as well'], correct: [0, 1, 4], explain: 'The node is mid-restart and only this operation watches it back, so neither the abort nor the delete is honored.' },
    { topic: 'Steps and abort', cat: 'action', text: 'A migration is aborted in `Preparing`. Which of these are true afterward?', options: ['The phase is `Aborted`', 'Nothing was asked of the control plane', 'The target worker keeps its storage-plane label', 'The target\'s copied configuration entry is removed', 'The node is restarted on its source worker'], correct: [0, 1, 2], explain: 'The abort ends the operation without undoing the step. The label and the entry stay until a later migration\'s `Promoting` releases unoccupied workers, and the node was never restarted.' },

    { topic: 'Promoting', cat: 'concept', text: 'What does the operator do in `Promoting`, after the promote call?', options: ['Sets `spec.workerNode` to the target', 'Appends `newSsdPcie` to `spec.config.pcieAllowList`', 'Gives the target worker the node\'s slot label', 'Removes the source worker\'s storage-plane labels if no other node of the cluster is there', 'Waits for the rebalance to finish'], correct: [0, 1, 2, 3], explain: 'The re-point and the release are the Kubernetes half of the move. The step does not wait for the rebalance, which the control plane runs afterward.' },
    { topic: 'Promoting', cat: 'field', text: 'Who may change `spec.workerNode` on a StorageNode?', options: ['A service account in the operator\'s namespace', 'A cluster administrator with `kubectl edit`', 'Anyone, since the field is not immutable', 'The CSI driver', 'Nobody, the field is immutable'], correct: [0], explain: 'The field carries no immutability marker because a migration re-points it, and the webhook refuses the change from anyone outside the operator\'s namespace.' },
    { topic: 'Promoting', cat: 'concept', text: 'Which of these are true of the promote?', options: ['It is the step with no way back', 'The re-point happens after it and never before', 'A pass that finds `spec.workerNode` already naming the target does not promote again', 'It is issued only once the node is `online` again', 'Its implementation is in the control plane, not in this repository'], correct: [0, 1, 2, 3, 4], explain: 'All five. The re-point is the record that the promote landed, and `Promoting` reads the node before it calls.' },
    { topic: 'Promoting', cat: 'concept', text: 'The source worker runs a second storage node of the same cluster on its other socket. What happens to the worker\'s labels when the first node is migrated?', options: ['They stay, because a node of the cluster remains there', 'They are removed, and the second node\'s pod is unscheduled', 'The second node is migrated as well', 'The migration is refused', 'They move to the target'], correct: [0], explain: 'The release strips only workers that no node of the cluster occupies, which is what keeps the other socket\'s node scheduled.' },
    { topic: 'Promoting', cat: 'concept', text: 'A migration reads `Succeeded`. Which of these can still be true?', options: ['The control plane is still rebalancing after the promote', 'A migration of another node is held with `ClusterNotReady`', 'The StorageNode still names its old worker', 'The node lock is still held by the migration', 'The node\'s UUID has changed'], correct: [0, 1], explain: 'The step ends at the re-point and the release, not at the end of the rebalance, and the next data-moving operation waits for the cluster to settle. The re-point and the lock release happen before `Succeeded`, and the UUID never changes.' },

    { topic: 'Maintenance window', cat: 'concept', text: 'Which of these are steps of `HostMaintenance` and not of `Migrate`?', options: ['`Holding`', '`Releasing`', '`AwaitingHost`', '`Cleanup`', '`ShuttingDown`'], correct: [0, 1, 2, 3, 4], explain: 'All five. A migration runs `Preparing`, `Relocating`, `AwaitingNode`, and `Promoting`. `ShuttingDown` is shared with `Remove`, not with `Migrate`.' },
    { topic: 'Maintenance window', cat: 'action', text: 'Why should a hand-written `HostMaintenance` not be named `<node>-maintenance`?', options: ['The operator calls off a window of that name while the worker is schedulable and the window has not left `Holding`', 'The webhook reserves the name', 'The name is longer than 63 characters', 'The operator deletes any object of that name on sight', 'The name makes the window skip the cluster gate'], correct: [0], explain: 'That is the name of the window a cordon raises, and an uncordoned worker is the signal to call it off. The operator deletes such a window only once it is terminal and the worker is schedulable.' },
    { topic: 'Maintenance window', cat: 'concept', text: 'Which of these does a same-host maintenance window share with a migration?', options: ['It waits at the cluster gate', 'It takes the node\'s lock', 'It can be called off in its first step only', 'It re-points `spec.workerNode`', 'It brings the node up with a control-plane restart'], correct: [0, 1, 2, 4], explain: 'Both move data, so both wait at the gate, both lock the node, and both end with the same restart primitive. Only a migration re-points the node.' },
  ],

  tasks: [
    {
      title: 'Retiring a worker',
      scenario: 'Worker `worker-3` is being retired. An identical machine, `worker-5`, has joined the Kubernetes cluster and is `Ready`, and runs no storage node yet. Write the operation that moves the storage node `production-worker-3-0` onto it and keeps the node\'s UUID and volumes.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: retire-worker-3\n  namespace: simplyblock\nspec:\n  nodeRef: {{node}}\n  action: {{action}}\n  migrate:\n    targetWorkerNode: {{target}}\n',
      blanks: {
        kind: { accept: ['StorageNodeOps'], hint: 'the node-level operation kind, short name `snops`' },
        node: { accept: ['production-worker-3-0'], hint: 'the StorageNode by its object name' },
        action: { accept: ['Migrate'], hint: 'the action that keeps the node\'s identity' },
        target: { accept: ['worker-5'], hint: 'the Kubernetes worker the node moves to' },
      },
      explain: 'A `StorageNodeOps` with `action: Migrate` names the node and the target. Identical hardware needs no `newSsdPcie`, and `force` is left to its default.',
    },
    {
      title: 'A target with drives at new addresses',
      scenario: 'The replacement machine `worker-7` carries its drives at `0000:88:00.0` and `0000:89:00.0`, addresses the source never had. Move `production-worker-4-0` onto it, bind both drives, and ask the control plane to reattach the node\'s volumes as part of the restart. Leave the force flag at its default.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: StorageNodeOps\nmetadata:\n  name: move-production-worker-4-0\n  namespace: simplyblock\nspec:\n  nodeRef: production-worker-4-0\n  action: Migrate\n  reattachVolume: {{reattach}}\n  migrate:\n    targetWorkerNode: {{target}}\n    {{field}}:\n      - "0000:88:00.0"\n      - {{second}}\n',
      blanks: {
        reattach: { accept: ['true'], hint: 'passed through to the restart' },
        target: { accept: ['worker-7'], hint: 'the replacement machine' },
        field: { accept: ['newSsdPcie'], hint: 'the list of PCI addresses to bind on the target' },
        second: { accept: ['/"?0000:89:00\\.0"?/', '/\'0000:89:00\\.0\'/'], hint: 'the second drive\'s PCI address' },
      },
      explain: '`newSsdPcie` adds the target\'s drives to the configuration it boots from, to the restart, and to the node\'s allow list at the promote. `reattachVolume` is forwarded to the restart, and `force` stays unset so the restart of the online node is forced.',
    },
    {
      title: 'A firmware update on the same host',
      scenario: 'Worker `worker-3` needs a firmware update and comes back with the same drives. The administrator starts the window by hand for the node `production-worker-3-0` instead of cordoning the worker. Write the operation, with a name the operator will not call off.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: StorageNodeOps\nmetadata:\n  name: {{name}}\n  namespace: simplyblock\nspec:\n  nodeRef: {{node}}\n  action: {{action}}\n',
      blanks: {
        name: { accept: ['/(?!production-worker-3-0-maintenance$)[a-z0-9]([-a-z0-9]*[a-z0-9])?/'], hint: 'any valid name except the one the operator gives the window a cordon raises' },
        node: { accept: ['production-worker-3-0'], hint: 'the StorageNode on that worker' },
        action: { accept: ['HostMaintenance'], hint: 'the node returns to the host it left' },
      },
      explain: 'A same-host reboot is a maintenance window, not a migration. The name `<node>-maintenance` belongs to the window the operator raises, and it calls that one off while the worker is schedulable and the window is still in `Holding`.',
    },
  ],
});
