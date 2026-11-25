// Moving volumes to another storage cluster while the workload keeps running,
// on the two paths the operator implements: a ReplicationOps migration over a
// pair and a migration-mode policy, which works between any two clusters of one
// control plane, and a PersistentVolumeOps Relocate, which works between the
// members of one supercluster. The chapter teaches the general path first and
// the supercluster path second, then the constraints that hold for both and how
// a migration differs from a DR failover and from Ramen's Relocate. The DR use
// of the same replication kinds is async-dr, and moving a whole cluster with
// scope cluster is cluster-live-migration.
TRAINING.register({
  id: 'volume-migration-clusters',
  title: 'Live-migrating volumes between storage clusters and sites',
  summary: 'Moving mounted volumes to another storage cluster of the same control plane: a ReplicationOps migration over a pair and a migration-mode policy, with its dry run, plan, forecast, and per-volume cutover, and a PersistentVolumeOps Relocate between the members of a supercluster.',
  goal: 'after the chapter the reader can enroll volumes into a migration policy, forecast and run their move to a second cluster, follow each cutover through the slot and its preconnect Job, relocate a volume inside a supercluster, and say which prerequisites a move across clusters or sites depends on and what has not been validated.',

  sections: [
    {
      heading: 'Two paths, and where the framing was adjusted',
      body: [
        'The commissioned framing was "live-migrating volumes between storage clusters and sites." The code supports it with two adjustments. First, a **site is a storage cluster that the same control plane serves**. A ReplicationPair names its two clusters by StorageCluster name or by backend UUID, and nothing in the API refers to a second control plane, so a volume moves to another site only when that site\'s cluster is registered with the control plane that serves the source. Second, **live means copy, then a short cutover**. The data is copied while the workload keeps writing, and then each volume\'s paths switch to the target while its I/O is frozen. The control plane reports that step as the volume state `cutting_over`, "I/O frozen, paths switching." How long the freeze lasts on this path is decided by the control plane, and the repository states no figure for it.',
        'The operator offers two ways to do it. They share the control plane\'s replication in migration mode and the same cutover protocol, and differ in what a user writes and between which clusters they work.',
        { table: { head: ['Path', 'What a user writes', 'Between', 'State in the code'], rows: [
          ['Replication migration', '`ReplicationPair`, `ReplicationPolicy` with `mode: migration`, claims enrolled by annotation, a `ReplicationOps` with `action: migration`', 'any two clusters of one control plane', 'Implemented. Tested by unit tests only'],
          ['Supercluster Relocate', 'one `PersistentVolumeOps` with `action: Relocate`', 'members of one supercluster, in one Kubernetes namespace', 'Built (design Draft, Phase 3). Its end-to-end and manual tests have not been run'],
        ] } },
        'A third operation, `PersistentVolumeOps` with `action: Migrate`, moves a volume between storage nodes inside one cluster, and admission refuses a target node in another cluster. It is the subject of the [chapter on moving volumes between nodes](chapter.html?c=volume-migration-nodes).',
        { callout: 'Neither path has been validated end to end on two live clusters. The migration-plan design is marked Implemented with every scenario of its test plan covered by unit tests, and no live two-cluster test exists. The supercluster test plan\'s end-to-end rows for a relocation (E-06, E-07, and E-10) and its manual scenarios are listed and have not been run. Encrypted volumes are refused by Relocate, and the replication path has no guard for them and no confirmation that they survive a move. Treat both paths as implemented and unproven.', kind: 'warn' },
      ],
    },
    {
      heading: 'The pair, the migration policy, and the volumes it carries',
      body: [
        'The general path uses the `v1alpha1` replication kinds that the [DR chapter](chapter.html?c=async-dr) teaches in full. This section repeats only what a move needs.',
        'A **ReplicationPair** names `spec.sourceCluster`, a StorageCluster in the pair\'s namespace, and `spec.targetCluster`, a StorageCluster name in the same namespace or the backend UUID of the target. A UUID is passed through unresolved, which is how a target whose StorageCluster object lives on another Kubernetes cluster is named. `targetCluster` is immutable. A supercluster, which is an aggregate, is refused as either side when the operator resolves it. The pair reconciler creates or reuses the backend replication target and records `status.backendTargetID` and `status.ready: true`.',
        'A **ReplicationPolicy** with `mode: migration` names the pair in `spec.pairRef` and sets `interval` (default `5m`, rounded to whole minutes) and `snapshotRetention` (at least `2`, default `3`). The mode is sent to the control plane with the backend policy. Only a cluster-scoped migration checks it: admission refuses a `policyRef` that is not in `migration` mode. A migration at policy, volume, or target scope does not check the mode, and what the control plane does with a migration plan over volumes that replicate in `failover` mode is not shown in the repository, so write a separate policy in `migration` mode for the move.',
        'A claim is **enrolled** by the annotation `storage.simplyblock.io/replication-policy` on the claim or on its StorageClass, and the claim\'s annotation wins when both are set. The value is the policy\'s name, or `<namespace>/<name>` for a policy in another namespace. Once the claim is `Bound`, the operator creates a `ReplicationSlot` named `<policy>-<claim>` in the claim\'s namespace, owned by the claim. The slot attaches the volume to the backend policy and from then on reports the volume\'s state.',
        { table: { head: ['Slot state', 'Meaning for a move'], rows: [
          ['`attaching`', 'the policy is being attached to the volume'],
          ['`replicating`', 'replication runs. A migration waits until every slot of its scope is here'],
          ['`cutover_pending`', 'the control plane has prepared this volume\'s cutover and waits for the operator to proceed'],
          ['`cutover_done`', 'the target serves the volume. A later migration skips it as already cut over'],
          ['`failed_over`', 'the volume was failed over for DR. A migration skips it until it is failed back'],
          ['`error`', 'a backend call failed. A migration reports the volume as failed'],
        ] } },
        { callout: 'A migration at scope `policy`, `volume`, or `target` sees only the slots in its own namespace, and it has to live in the policy\'s namespace, because that is where admission and the reconciler look the policy up. So these scopes move only claims that live beside the policy. A claim in a workload namespace is moved by scope `cluster`, which lists claims in every namespace and enrolls them itself. That scope moves a whole cluster and is the subject of the [chapter on live-migrating a cluster](chapter.html?c=cluster-live-migration).', kind: 'note' },
        { code: 'kubectl get relpair,repl -n simplyblock\nkubectl get relslot -n simplyblock', title: 'The pair, the policy, and the slots' },
      ],
      examples: [
        { title: 'The pair', file: 'examples/volume-migration-clusters/01-replicationpair.yaml', notes: [
          { at: 'sourceCluster: site-a', text: 'A StorageCluster of this namespace. Its `status.uuid` is the cluster the plan is created on.' },
          { at: 'targetCluster: site-b', text: 'A StorageCluster name here, resolved to its UUID. A backend UUID is passed through for a target whose object lives on another Kubernetes cluster. Immutable.' },
        ] },
        { title: 'The migration policy', file: 'examples/volume-migration-clusters/02-replicationpolicy-migration.yaml', notes: [
          { at: 'pairRef', text: 'The pair in the same namespace.' },
          { at: 'mode: migration', text: 'The planned move. The other mode, `failover`, keeps the target as a DR standby.' },
          { at: 'interval: 1m', text: 'How often a replication snapshot is shipped until the cutover. Sent to the control plane in whole minutes.' },
          { at: 'snapshotRetention: 2', text: 'The CRD\'s minimum. A value of `1` is refused.' },
        ] },
        { title: 'An enrolled claim', file: 'examples/volume-migration-clusters/03-pvc.yaml', notes: [
          { at: 'namespace: simplyblock', text: 'Beside the policy, so that a policy-scoped migration in this namespace finds its slot.' },
          { at: 'replication-policy: move-to-site-b', text: 'The bare name, because the policy is in the claim\'s namespace. From another namespace it would read `simplyblock/move-to-site-b`.' },
          { at: 'storageClassName: simplyblock-site-a', text: 'The class site-a wrote for its default pool. The volume is provisioned on the source and moved later.' },
        ] },
      ],
    },
    {
      heading: 'The migration operation: dry run, plan, and forecast',
      body: [
        'A move is one `ReplicationOps` with `action: migration`. The scope says which volumes, and `ref` names the object the scope takes. `dryRun` and `policyRef` are immutable by a CEL rule. `action`, `scope`, and `ref` are documented as immutable and no rule enforces it, so a changed move is a new operation, not an edit.',
        { table: { head: ['Scope', '`ref` names', 'Volumes moved', 'Lock'], rows: [
          ['`policy`', 'a ReplicationPolicy', 'every slot of the policy in the operation\'s namespace', '`ReplicationPolicy.status.activeOpsRef`'],
          ['`volume`', 'a ReplicationSlot', 'that one slot', 'the slot\'s policy'],
          ['`target`', 'a ReplicationPair', 'every slot of every policy on the pair, in the namespace', '`ReplicationPair.status.activeOpsRef`'],
          ['`cluster`', 'a StorageCluster name or UUID', 'every bound claim on that cluster, in any namespace, enrolled under `spec.policyRef`', 'the policy named by `policyRef`'],
        ] } },
        'An admission webhook refuses an operation whose `ref` names nothing of the scope\'s kind in the namespace, `dryRun` on any action but `migration`, `policyRef` outside scope `cluster`, and scope `cluster` without `policyRef` or with any action but `migration`. A second operation on a locked policy or pair stays `Pending` and is checked again every 10 seconds. Deleting an operation releases its lock and nothing else, and there is no `abort` field, so a plan that exists keeps running in the control plane.',
        '**Run the dry run first.** With `dryRun: true` the operator puts every volume of the scope into a plan request with `dry_run` set, whatever state its slot is in, and the control plane answers with the plan it would run. The operation ends `Succeeded` once the answer is in its status and emits `MigrationForecast`. Against a control plane that has no migration plans, the dry run fails, because nothing can forecast.',
        'The **real run** goes through four persisted subphases:',
        { steps: [
          '`AwaitingReplication`: the operator checks every slot every 15 seconds until each is `replicating`. A slot in `cutover_done` is reported `skipped` as already cut over, one in `failed_over` is `skipped` with the reason that it has to be failed back before migrating, and one in `error` is `failed`. (`Enrolling` comes first only at scope `cluster`.)',
          '`CreatingPlan`: written before the call, then `POST /api/v2/clusters/<source>/migration-plans/` with the volume IDs, the target cluster, `delete_source`, and the name `replicationops/<namespace>/<name>/<uid>`. A pass that died after the call finds the subphase and adopts the plan by that name instead of creating a second one.',
          '`TrackingPlan`: the plan\'s ID is kept in `status.planID` and the plan is read every 30 seconds. Each volume\'s state, target node, bytes remaining, and ETA go into `status.results`, and the plan-wide estimate into `status.forecast`. The event `MigrationPlanCreated` marks the start.',
          'The end: a plan that is `done` ends the operation `Succeeded` with `MigrationComplete`. One that is `partial`, `failed`, or `canceled` ends it `Failed` with the results kept and the event `Failed`.',
        ] },
        'A control plane that answers the plan request with 404 has no migration plans. The operator then commits each volume on its own, puts each slot in `cutover_pending`, emits `MigrationCommitQueued`, and follows the slots in the subphase `AwaitingCutover` until each reaches `cutover_done`.',
        { table: { head: ['Plan state of a volume', 'Result `status`'], rows: [
          ['`replicating`, `queued`, `converging`, `cutting_over`', '`pending`'],
          ['`done`', '`succeeded`'],
          ['`failed`, `canceled`', '`failed`'],
          ['`skipped`', '`skipped`'],
        ] } },
        'The **forecast** carries `bytesRemaining`, `etaSeconds`, `completesAt`, and `throughputBytesPerSec` with `throughputMeasured`, which is false while no transfer has completed and the rate is a default. `converging` is the verdict that matters: **`converging: false` means some source volume store is written at least as fast as it replicates**. Its lag never closes, no cutover starts on it, and `etaSeconds` and `completesAt` are left unset. `dirtyBytesPerSec` is the clients\' write rate and `notConvergingLVS` names the stores. The operator emits `NotConverging` as a warning when the verdict turns false and `Converging` when it turns true again, once per change. It reports and does not throttle. `status.warnings` carries the risks the control plane found and did not refuse the plan for, such as too little free capacity on the source for the snapshots the copy pins.',
        { code: 'kubectl get replops -n simplyblock -o wide\nkubectl get replops move-to-site-b -n simplyblock -o jsonpath=\'{.status.forecast}\'', title: 'Following a migration (the wide output adds ETA and Converging)' },
      ],
      examples: [
        { title: 'The dry run', file: 'examples/volume-migration-clusters/04-replicationops-dryrun.yaml', notes: [
          { at: 'action: migration', text: 'The planned move, and the only action that accepts `dryRun`.' },
          { at: 'scope: policy', text: 'Every slot of the policy in this namespace.' },
          { at: 'ref: move-to-site-b', text: 'The policy, which admission requires to exist in this namespace.' },
          { at: 'dryRun: true', text: 'The plan and the forecast only. Immutable, so the real run is a second object.' },
        ] },
        { title: 'The migration', file: 'examples/volume-migration-clusters/05-replicationops-migration.yaml', notes: [
          { at: 'name: move-to-site-b', text: 'Part of the plan\'s name with the namespace and the UID, which is how a restarted operator finds the plan again.' },
          { at: 'scope: policy', text: 'The same scope as the dry run, so the forecast describes this move.' },
          { at: 'deleteSource: true', text: 'The control plane deletes each source volume after its cutover, and the operator marks each migrated PersistentVolume with `relocated-to`.' },
        ] },
      ],
    },
    {
      heading: 'The cutover, one volume store at a time',
      body: [
        'The plan copies every volume, then cuts the volumes over one at a time per source volume store, so that each I/O freeze stays short. The plan decides the order. Each cutover is carried out by the **ReplicationSlot reconciler**, which polls its volume every 15 seconds while it replicates and reacts when the control plane reports `cutover_pending`:',
        { steps: [
          'It reads the volume\'s `connect` endpoint, which during `cutover_pending` answers with the target\'s paths beside the source\'s.',
          'It finds the node running a pod that has the volume mounted and creates the Job `replslot-preconnect-<volume>` in the slot\'s namespace, pinned to that node, in mode `replication-preconnect`, with a deadline of 120 seconds. The event `PreconnectStarted` names the node.',
          'Once the Job has succeeded, it sends `POST .../replication/cutover-proceed`. The control plane then flips ANA: the target\'s paths become optimized and the source\'s inaccessible.',
          'The slot follows the control plane to `cutover_done`.',
        ] },
        'A failed preconnect Job is deleted and created again, with the warning `PreconnectFailed`, and the proceed is not sent: proceeding while a consumer holds no target path is what cut a consumer off from its volume on a test bed. A volume with no running consumer gets the proceed at once. If the operator never proceeds, the control plane proceeds alone after its own timeout, `REPL_CUTOVER_PROCEED_TIMEOUT_SEC`. The repository gives two values for it, 120 seconds in the supercluster design and Relocate\'s deadlines and about 30 seconds in a comment of the slot reconciler, and it is set in the control plane, where it cannot be checked from here. When the slot first sees `cutover_done` without having preconnected, it runs a late preconnect so that the node gets the target\'s paths after the flip. When the control plane abandons a cutover and puts the volume back to `replicating`, the slot follows it there and discards its preconnect Job.',
        'The slot resolves **one** consuming node, so a volume mounted on several nodes has the target\'s paths connected ahead of the flip on one of them only. Relocate, in the next section, runs a Job on every consuming node.',
        'The copy keeps the source volume\'s NQN and namespace ID, according to the supercluster design, so the host sees the same namespace on new paths and nothing is remounted. That is control-plane behavior the repository describes and does not show.',
        '**Afterward**, the PersistentVolume\'s `volumeHandle` still names the source volume, because a handle is fixed at provisioning. With `deleteSource: true` the source volume is deleted after its cutover, and on every poll the operator writes the annotation `storage.simplyblock.io/relocated-to` on each migrated volume\'s PersistentVolume, with the handle the control plane\'s relationships resolve to. The CSI driver stages the volume from that mark and deletes it through the mark when the claim goes. Without `deleteSource` no mark is written and the source volume stays. A pod staged later then reaches the target only if the control plane answers the old handle with the target\'s paths, which has not been verified end to end. The source StorageCluster is never deleted by the operator. Deleting it before every migrated PersistentVolume carries its mark strands the handles, and nothing prevents it.',
        { code: 'kubectl get relslot -n simplyblock -w\nkubectl get jobs -n simplyblock | grep replslot-preconnect\nkubectl get pv -o custom-columns=NAME:.metadata.name,MOVED:.metadata.annotations.storage\\.simplyblock\\.io/relocated-to', title: 'Watching the cutovers' },
      ],
    },
    {
      heading: 'Between members of a supercluster: PersistentVolumeOps Relocate',
      body: [
        'A supercluster is one StorageCluster that aggregates several member StorageClusters in the same namespace, each carrying `spec.superclusterRef`. Between its members a volume is moved by one object, a `PersistentVolumeOps` (`storage.simplyblock.io/v1alpha2`, cluster-scoped, short name `pvops`) with `action: Relocate` and `spec.relocate.targetClusterRef` naming the target member. No pair, policy, or annotation is written: the operator starts the replication in migration mode itself, with a one-minute interval, into the target member\'s pool of the same supercluster pool.',
        'Besides the checks every PersistentVolumeOps passes (the PersistentVolume exists, has a CSI source of this operator\'s driver, is not a pNFS volume, and has a parsable handle), admission refuses a Relocate when:',
        { list: [
          'the volume is encrypted, with `EncryptedVolumeNotMovable`, because the move deletes the source and with it the only copy of the key, which replication does not carry',
          'its claim carries a consistency-group label, because a group\'s members stay in one member',
          'no StorageCluster reports the volume\'s cluster, or that cluster is in no supercluster',
          'the target is the volume\'s current member, does not exist in that member\'s namespace, or belongs to another supercluster',
          'the target sets `disablePlacement`',
        ] },
        'A target that exists and is not yet accepting volumes is not refused. The operation holds in `Validating` with the event `TargetMemberNotReady` until it is. The type\'s own CEL rule requires `spec.relocate` with `Relocate` and refuses it with any other action, and `targetClusterRef` is immutable.',
        'The volume moves together with every volume sharing its NVMe-oF subsystem, all listed in `status.relocate.volumes`. It takes the same lock as `Migrate`, the annotation `storage.simplyblock.io/active-ops` on every PersistentVolume of the subsystem, and does not take the per-cluster migration slot. The phases are `Pending`, `Running`, `Succeeded`, `Failed`, and `Aborted`, and the steps are these:',
        { table: { head: ['Step', 'What happens', 'Deadline'], rows: [
          ['`Validating`', 'resolves the target member and pool and lists the subsystem\'s volumes. Holds with `TargetMemberNotReady`', '15 min'],
          ['`Replicating`', 'starts each volume\'s replication into the target (`ReplicationStarted`) and waits until all are in sync', '2 h plus 30 min per volume'],
          ['`Committing`', 'writes `status.relocate.committed`, then commits each volume with its source deletion and records the task', '15 min'],
          ['`AwaitingCutover`', 'waits until the control plane has prepared the next volume\'s cutover', '30 min per volume'],
          ['`AttachingPaths`', 'one path Job per consuming node. A failed Job is replaced with `PathAttachHeld` and holds the cutover', '10 min per volume'],
          ['`Proceeding`', 'releases the cutover and waits until the target serves the volume, then returns to `AwaitingCutover` for the next volume', '10 min per volume'],
          ['`Verifying`', 'checks every cutover task, writes `relocated-to` on each PersistentVolume, and deletes the path Jobs', '10 min'],
        ] } },
        '**`Committing` is the point of no return.** Up to and including `Replicating`, `spec.abort: true` stops the replication of every volume and the operation ends `Aborted`, with the source serving as before. From `Committing` on, the control plane cuts the volumes over and deletes their sources whatever the operation does: an abort is answered with `RelocationAbortIgnored` and the move carries on, and a delete of the object is refused at admission in `Committing`, `AwaitingCutover`, `AttachingPaths`, `Proceeding`, and `Verifying`. A step past its deadline fails the operation with `StepDeadlineExceeded`.',
        'If the control plane proceeds on its own timeout before a consuming node held the target\'s paths, the node is recorded in `status.relocate.nodesMissingPaths` with `CutoverWithoutPaths`. `Verifying` still marks the PersistentVolume, so a restaged pod reaches the target, and the operation then ends `Failed`, naming the nodes whose pods lost the volume until they are restarted. A handle that does not resolve into the target fails it with `HandleUnresolved`. Success is reported with the event `OperationSucceeded`, which names the volume and the member it moved to. The events land in the namespace `default`, because the object is cluster-scoped, so `kubectl describe pvops` is where they are read.',
        { code: 'kubectl get pvops relocate-orders-db -w\nkubectl describe pvops relocate-orders-db', title: 'Following a Relocate' },
      ],
      examples: [
        { title: 'A Relocate', file: 'examples/volume-migration-clusters/06-persistentvolumeops-relocate.yaml', notes: [
          { at: 'apiVersion: storage.simplyblock.io/v1alpha2', text: 'PersistentVolumeOps is a `v1alpha2` kind, unlike the replication kinds.' },
          { at: 'name: relocate-orders-db', text: 'No namespace: the operation is cluster-scoped, like the PersistentVolume.' },
          { at: 'persistentVolumeName', text: 'The volume, not the claim. Every volume sharing its subsystem moves with it.' },
          { at: 'action: Relocate', text: 'Requires `spec.relocate` and refuses `spec.migrate` and `spec.joinConsistencyGroup`.' },
          { at: 'targetClusterRef: prod-fast-mzt', text: 'Another member of the same supercluster, in the namespace of the volume\'s current member, that does not set `disablePlacement`. Immutable.' },
        ] },
      ],
    },
    {
      heading: 'Crossing clusters and sites, and what a migration is not',
      body: [
        'Both paths depend on the same conditions, and the operator checks only some of them:',
        { list: [
          '**One control plane.** Replication works between clusters the same control plane serves. A Relocate additionally needs both members in one supercluster, and therefore in one Kubernetes cluster and namespace.',
          '**Data-plane reachability between the storage networks.** The target node attaches the copy over NVMe-oF, so the two clusters\' storage networks have to reach each other. The supercluster design lists this as prerequisite P0-2, and no step of either path tests it before the copy starts.',
          '**Hosts that reach the target\'s storage nodes.** The preconnect Job (or Relocate\'s path Jobs) connects the target\'s paths from each consuming node. A node that cannot reach them fails the Job, which holds the cutover until the control plane\'s timeout.',
          '**The target in the CSI secret.** The CSI driver reads every cluster it talks to from `simplyblock-csi-secret-v2`. The operator writes an entry for each StorageCluster it manages, and a target whose StorageCluster lives on another Kubernetes cluster is listed through `spec.remoteClusters` of the SimplyblockDriver. Without the entry the driver cannot address the target, and logs "target cluster ... not in secret file."',
          '**The NQN kept, so no remount.** The switch is an ANA flip under the same subsystem NQN, so pods keep running. Kernel multipath has to follow ANA, and the CSI driver connects with a controller-loss timeout of 60 seconds.',
        ] },
        'Inside one **stretched cluster** the two sites are one storage cluster, and a `Migrate` to a node of the other site is refused with `RemoteNode`. A stretched cluster replicates synchronously and is not moved between its sites by either path here.',
        { table: { head: ['', 'ReplicationOps `failover`', 'ReplicationOps `migration`', 'PersistentVolumeOps `Relocate`', 'Ramen DRPC `Relocate`'], rows: [
          ['Planned', 'no, written when the source is lost', 'yes', 'yes', 'yes'],
          ['Source cluster', 'may be down', 'up throughout', 'up throughout', 'up, demoted first'],
          ['What the target serves', 'the copy it holds', 'the volume with its final delta, after one freeze per volume', 'the same', 'the volume, after a final sync'],
          ['Workload', 'taught in the DR chapter', 'keeps running, paths switch by ANA', 'keeps running, paths switch by ANA', 'moves to the other Kubernetes cluster'],
          ['Slot or record afterward', '`failed_over`', '`cutover_done`', '`status.relocate` with `cutOver` per volume', 'Ramen\'s own objects'],
        ] } },
        'A **DR failover** is the [async-dr](chapter.html?c=async-dr) operation: unplanned, written by a person after deciding the source is lost, and run against a copy that is as recent as the last shipped snapshot. A migration is planned, runs with both clusters up, and catches the target up with the final delta before each cutover. **Ramen\'s `Relocate`** on a DRPlacementControl is an application move between two Kubernetes clusters: the source-side VolumeReplicationGroup demotes, the target-side one promotes, and the workload starts on the other cluster. It is described in `design-ramen-integration.md`, a Draft whose live validation is outstanding, and taught with its third site in the [arbiter chapter](chapter.html?c=async-dr-arbiter). It has the name in common with a PersistentVolumeOps `Relocate` and nothing else: that one keeps the workload where it runs and changes which cluster serves its volume.',
      ],
    },
    {
      heading: 'What goes wrong, and what is not verified',
      body: [
        { pitfalls: [
          { symptom: 'A policy-scoped migration ends with no volume moved, or with fewer than the policy has.', cause: 'Its claims live in other namespaces. Scopes `policy`, `volume`, and `target` see only slots in the operation\'s namespace.', fix: 'Move claims in workload namespaces with scope `cluster` and a `policyRef`, or keep the claims to be moved beside the policy.' },
          { symptom: 'The migration stays in `AwaitingReplication`.', cause: 'A slot is still `attaching`, or the pair or policy is not ready, so replication has not started.', fix: 'Read `status.results` for the slot named as waiting and the slot\'s own `status.message`, and fix the pair or policy it points at.' },
          { symptom: 'The migration sits in `TrackingPlan` with `converging: false` and no ETA.', cause: 'A source volume store is written faster than it replicates, and no cutover can start on it.', fix: 'Reduce the writes to the stores in `notConvergingLVS`, or move fewer volumes at once. The plan keeps running and the operator does not throttle.' },
          { symptom: 'A slot stays in `cutover_pending` and `PreconnectFailed` repeats.', cause: 'The consuming node cannot reach the target\'s storage nodes, so the preconnect Job fails and the proceed is held.', fix: 'Fix the route from the node to the target\'s storage network. The control plane proceeds alone after its timeout, after which that node\'s pod has to be restarted to get the target\'s paths.' },
          { symptom: 'A pod restarted after the move cannot stage its volume.', cause: 'The target is not listed in `simplyblock-csi-secret-v2`, or the migration ran without `deleteSource` and the old handle does not lead to the target.', fix: 'List the target cluster (as a StorageCluster or in `spec.remoteClusters`), and check that the PersistentVolume carries `relocated-to`.' },
          { symptom: 'A Relocate is refused with `EncryptedVolumeNotMovable`.', cause: 'The volume is encrypted and the move would delete the only copy of its key.', fix: 'None today. The replication path has no guard and must not be used for such a volume either, until the control plane carries the key (P0-1).' },
          { symptom: 'A Relocate waits in `Validating` with `TargetMemberNotReady`.', cause: 'The target member exists and does not accept volumes yet.', fix: 'Bring the member online. If it accepts volumes within the 15 minutes of the step, the operation proceeds by itself, and otherwise it fails with `StepDeadlineExceeded` and is written again.' },
          { symptom: 'Setting `spec.abort` on a Relocate has no effect, and deleting it is refused.', cause: 'It is past `Committing`, the point of no return.', fix: 'Let it finish. The control plane completes the cutover and deletes the source either way.' },
        ] },
        'What this chapter could not verify in the repository, and therefore states as unverified:',
        { list: [
          'No live two-cluster end-to-end test of either path exists. The replication migration is covered by unit tests, and The supercluster test plan\'s end-to-end rows for a relocation (E-06, E-07, and E-10) and its manual scenarios have not been run.',
          'Encrypted volumes are refused by Relocate. On the replication path no guard was found, and the supercluster design says the landing volume has no crypto layer and `delete_source` deletes the key. Do not move encrypted volumes by either path.',
          'The length of the I/O freeze at cutover, the value of `REPL_CUTOVER_PROCEED_TIMEOUT_SEC`, and that the copy keeps the NQN and namespace ID are control-plane behavior the repository describes and does not show.',
          'Without `deleteSource`, whether a pod staged after the move reaches the target depends on the control plane\'s answer for the old handle, which has not been tested end to end.',
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Paths and framing', cat: 'concept', text: 'In the code, what is the "site" a volume migrates to?', options: ['A storage cluster that the same control plane serves', 'A second ControlPlane object that the pair references', 'A Kubernetes cluster registered with OCM', 'A failure domain inside one storage cluster', 'One site of a stretched cluster'], correct: [0], explain: 'A pair names two clusters by StorageCluster name or backend UUID, and nothing in the API refers to another control plane. A stretched cluster\'s sites are one storage cluster, which neither path moves between.' },
    { topic: 'Paths and framing', cat: 'concept', text: 'Which operations move a mounted volume to another storage cluster?', options: ['A ReplicationOps with `action: migration`', 'A PersistentVolumeOps with `action: Relocate`', 'A PersistentVolumeOps with `action: Migrate`', 'A ReplicationOps with `action: failover`', 'A VolumeMigration'], correct: [0, 1], explain: 'The replication migration works between any two clusters of one control plane, and Relocate between members of a supercluster. `Migrate` and its legacy form `VolumeMigration` stay inside one cluster, and a failover is the unplanned DR operation.' },
    { topic: 'Paths and framing', cat: 'concept', text: 'What does "live" mean for a cross-cluster migration as the code implements it?', options: ['The data is copied while the workload keeps writing', 'Each volume\'s paths switch to the target during a short I/O freeze', 'The volume keeps its NQN, so the host does not remount it', 'No I/O is ever paused', 'Only the primary role moves and no data is copied'], correct: [0, 1, 2], explain: 'A copy runs first, then each volume is cut over with its I/O frozen while ANA flips. Moving only the role exists in a UI prototype on a mock backend, not in the operator.' },
    { topic: 'Paths and framing', cat: 'concept', text: 'Which of these have been validated end to end on two live clusters?', options: ['A ReplicationOps migration at scope `policy`', 'A ReplicationOps migration at scope `cluster`', 'A PersistentVolumeOps Relocate', 'The dry-run forecast against a real control plane', 'Moving an encrypted volume by replication'], correct: [], explain: 'None. The migration-plan test plan is unit tests only, Relocate\'s end-to-end and manual scenarios have not been run, and encrypted volumes are refused by Relocate and unconfirmed on the replication path.' },
    { topic: 'Paths and framing', cat: 'field', text: 'Which kind and version pairs does this chapter use?', options: ['`ReplicationPair` in `storage.simplyblock.io/v1alpha1`', '`ReplicationPolicy` in `storage.simplyblock.io/v1alpha1`', '`ReplicationOps` in `storage.simplyblock.io/v1alpha1`', '`ReplicationOps` in `storage.simplyblock.io/v1alpha2`', '`PersistentVolumeOps` in `storage.simplyblock.io/v1alpha1`'], correct: [0, 1, 2], explain: 'The replication kinds are the `v1alpha1` family. PersistentVolumeOps is a `v1alpha2` kind.' },

    { topic: 'Pair, policy, and enrollment', cat: 'field', text: 'Which statements about `spec.targetCluster` of a ReplicationPair are true?', options: ['It can be a StorageCluster name in the pair\'s namespace', 'It can be the backend UUID of the cluster', 'It is immutable after creation', 'It may name a supercluster', 'It names a ControlPlane'], correct: [0, 1, 2], explain: 'A name is resolved to the cluster\'s UUID and a UUID is passed through. A CEL rule makes it immutable. A supercluster is an aggregate and is refused as either side.' },
    { topic: 'Pair, policy, and enrollment', cat: 'field', text: 'Which values of `spec.snapshotRetention` does the ReplicationPolicy CRD accept?', options: ['`1`', '`2`', '`3`', '`0`', '`10`'], correct: [1, 2, 4], explain: 'The minimum is two and the default three. Zero and one are refused.' },
    { topic: 'Pair, policy, and enrollment', cat: 'concept', text: 'How is a claim enrolled into a migration policy?', options: ['The annotation `storage.simplyblock.io/replication-policy` on the claim', 'The same annotation on the claim\'s StorageClass', 'A ReplicationSlot written by hand', 'A label on the PersistentVolume', 'A list of claims in the policy\'s spec'], correct: [0, 1], explain: 'The annotation on the claim or its class enrolls it, and the operator writes the slot. The policy has no list of claims.' },
    { topic: 'Pair, policy, and enrollment', cat: 'concept', text: 'A claim\'s annotation and its StorageClass\'s annotation name different policies. Which one does the claim follow?', options: ['The claim\'s', 'The StorageClass\'s', 'Both, with one slot each', 'Neither, and the slot goes to `error`', 'Whichever was written first'], correct: [0], explain: 'The claim\'s annotation wins over the class\'s. A claim has one slot.' },
    { topic: 'Pair, policy, and enrollment', cat: 'concept', text: 'A migration at scope `policy` runs in the `simplyblock` namespace, beside its policy. Which claims does it move?', options: ['Claims in `simplyblock` that follow the policy', 'Claims in every namespace that follow the policy', 'Every claim on the source cluster', 'Claims whose StorageClass belongs to the source cluster', 'Only claims labeled for migration'], correct: [0], explain: 'The policy, volume, and target scopes list slots in the operation\'s namespace, and a slot lives beside its claim. Claims elsewhere are moved by scope `cluster`.' },

    { topic: 'The migration operation', cat: 'field', text: 'Which pairings of scope and `ref` are right for a migration?', options: ['`policy`: a ReplicationPolicy', '`volume`: a ReplicationSlot', '`target`: a ReplicationPair', '`cluster`: a StorageCluster name or UUID', '`volume`: a PersistentVolumeClaim'], correct: [0, 1, 2, 3], explain: 'Scope `volume` names the slot, not the claim, and admission refuses a `ref` that names nothing of the scope\'s kind in the namespace.' },
    { topic: 'The migration operation', cat: 'field', text: 'Which ReplicationOps are refused at admission?', options: ['`dryRun: true` with `action: failover`', '`policyRef` with `scope: policy`', '`scope: cluster` without `policyRef`', '`scope: policy` whose `ref` names no ReplicationPolicy in the namespace', '`deleteSource: true` with `action: migration`'], correct: [0, 1, 2, 3], explain: '`dryRun` is for migration only, `policyRef` belongs to scope `cluster` and is required there, and the `ref` must resolve. `deleteSource` is allowed on a migration.' },
    { topic: 'The migration operation', cat: 'action', text: 'What does a migration with `dryRun: true` do?', options: ['Asks the control plane for the plan and ends `Succeeded` once the forecast is in status', 'Copies the data and skips only the cutover', 'Reserves capacity on the target', 'Fails against a control plane without migration plans', 'Emits the event `MigrationForecast`'], correct: [0, 3, 4], explain: 'A dry run sends the plan request with `dry_run` set and records the answer. It copies nothing beyond what the policy already replicates and reserves nothing. Without migration plans nothing can forecast.' },
    { topic: 'The migration operation', cat: 'concept', text: 'A running migration reports `status.forecast.converging: false`. Which follow?', options: ['Some source volume store is written at least as fast as it replicates', 'No cutover starts on that store', '`etaSeconds` and `completesAt` are left unset', 'The operator throttles the workload\'s writes', 'The operation fails at once'], correct: [0, 1, 2], explain: 'The lag of that store cannot close. The operator emits `NotConverging` and keeps tracking the plan. It reports and does not throttle.' },
    { topic: 'The migration operation', cat: 'concept', text: 'Why does the operator record the subphase `CreatingPlan` before it posts the plan, and name the plan after the operation\'s namespace, name, and UID?', options: ['So that a pass that died after the call adopts the plan instead of creating a second one', 'So that the control plane can authenticate the operator', 'So that two operations can share one plan', 'Because the control plane refuses a plan without a name', 'So that deleting the operation cancels the plan'], correct: [0], explain: 'A later pass that finds `CreatingPlan` looks the plan up by name before creating one. Deleting the operation cancels nothing.' },
    { topic: 'The migration operation', cat: 'action', text: 'Which of these stop a migration plan that is running in the control plane?', options: ['Setting `spec.abort: true` on the ReplicationOps', 'Deleting the ReplicationOps', 'Changing `spec.dryRun` to `true`', 'Deleting the ReplicationPolicy', 'Setting `spec.deleteSource: false`'], correct: [], explain: 'None. ReplicationOps has no `abort` field, deleting it releases the lock and leaves the plan running, `dryRun` is immutable, `deleteSource` was sent with the plan when it was created, and a policy\'s deletion is held while slots follow it.' },
    { topic: 'The migration operation', cat: 'field', text: 'Which fields does each entry of `status.results` of a running migration carry?', options: ['`state`', '`targetNodeID`', '`bytesRemaining`', '`etaSeconds`', '`pvcRef`'], correct: [0, 1, 2, 3, 4], explain: 'Each plan volume is mirrored with its plan state, target node, bytes left, and ETA, named by its claim and slot where the operator knows them.' },

    { topic: 'The cutover', cat: 'action', text: 'Which steps does the slot reconciler take when its volume reaches `cutover_pending`?', options: ['Reads the target\'s paths from the volume\'s `connect` endpoint', 'Runs a preconnect Job on the node consuming the volume', 'Sends `cutover-proceed` once the Job has succeeded', 'Restarts the consuming pod', 'Deletes the PersistentVolume and creates a new one'], correct: [0, 1, 2], explain: 'The target\'s paths are connected before the ANA flip, and the proceed releases the flip. Nothing is restarted or re-created.' },
    { topic: 'The cutover', cat: 'action', text: 'The preconnect Job for a volume fails. What does the slot reconciler do?', options: ['Deletes the Job and retries it, without sending `cutover-proceed`', 'Sends `cutover-proceed` anyway', 'Fails the ReplicationOps', 'Puts the slot in `error`', 'Cancels the migration plan'], correct: [0], explain: 'It emits `PreconnectFailed` and holds the proceed, because proceeding with a consumer that has no target path cuts it off from its volume.' },
    { topic: 'The cutover', cat: 'concept', text: 'The operator never sends `cutover-proceed` for a volume. What happens?', options: ['The control plane proceeds on its own after `REPL_CUTOVER_PROCEED_TIMEOUT_SEC`', 'The cutover waits forever', 'The plan is canceled', 'The slot returns to `attaching`', 'The volume is failed over'], correct: [0], explain: 'The control plane has a safety timeout. The repository gives it as 120 seconds in one place and about 30 in another, and the value lives in the control plane.' },
    { topic: 'The cutover', cat: 'field', text: 'After a migration with `deleteSource: true`, which are true of a migrated claim\'s PersistentVolume?', options: ['Its `volumeHandle` still names the source volume', 'It carries `storage.simplyblock.io/relocated-to` with the current handle', 'The CSI driver stages the volume through that annotation', 'Its `volumeHandle` is rewritten to the target', 'It is deleted and created again, bound to the same claim'], correct: [0, 1, 2], explain: 'A handle is fixed at provisioning, so the operator records where the volume lives now in the mark, which the driver stages and deletes through.' },
    { topic: 'The cutover', cat: 'concept', text: 'A migration ran without `deleteSource`. Which are true?', options: ['No `relocated-to` mark is written', 'The source volumes are kept', 'The operator deletes the source StorageCluster', 'The PersistentVolumes are bound again', 'The claims lose their replication annotation'], correct: [0, 1], explain: 'Without `deleteSource` the operator writes no mark, and whether a pod staged later reaches the target depends on the control plane\'s answer for the old handle, which is unverified. The operator never deletes the source cluster.' },

    { topic: 'Relocate', cat: 'concept', text: 'Which Relocate requests does admission refuse?', options: ['One for an encrypted volume', 'One whose claim carries a consistency-group label', 'One whose target member sets `disablePlacement`', 'One whose target member is not yet accepting volumes', 'One whose target is the volume\'s current member'], correct: [0, 1, 2, 4], explain: 'Each refusal is fixed for the request\'s life. Readiness is not: the operation holds in `Validating` with `TargetMemberNotReady`.' },
    { topic: 'Relocate', cat: 'concept', text: 'From which step on is `spec.abort` on a Relocate no longer honored?', options: ['`Committing`', '`Replicating`', '`Validating`', 'Only `Verifying`', 'An abort is honored at every step'], correct: [0], explain: 'From the commit on, the control plane cuts the volumes over and deletes their sources whatever the operation does, so the abort is answered with `RelocationAbortIgnored`.' },
    { topic: 'Relocate', cat: 'field', text: 'Which statements about the Relocate parameters are true?', options: ['`spec.relocate` is required with `action: Relocate` and refused with any other action', '`targetClusterRef` is immutable', '`targetClusterRef` names a StorageCluster in the namespace of the volume\'s current member', '`targetClusterRef` is the target\'s backend UUID', '`spec.migrate.targetNodeRef` is required as well'], correct: [0, 1, 2], explain: 'A CEL rule pairs each action with its block, and a Relocate refuses `spec.migrate`. The target is named by its object in the member\'s namespace.' },
    { topic: 'Relocate', cat: 'concept', text: 'Which volumes does one Relocate move?', options: ['The named volume', 'Every volume sharing its NVMe-oF subsystem', 'Every volume in the claim\'s namespace', 'Every volume of the source member', 'Only the named volume, while its siblings stay'], correct: [0, 1], explain: 'The control plane requires the volumes of one subsystem to replicate to the same target, so they move together and are listed in `status.relocate.volumes`.' },
    { topic: 'Relocate', cat: 'action', text: 'The control plane cut a volume over on its own timeout before a consuming node held the target\'s paths. What does the Relocate do?', options: ['Records the node in `status.relocate.nodesMissingPaths` with `CutoverWithoutPaths`', 'Still marks the PersistentVolume with `relocated-to` in `Verifying`', 'Ends `Failed`, naming the nodes whose pods lost the volume until restarted', 'Moves the volume back to the source member', 'Ends `Succeeded`'], correct: [0, 1, 2], explain: 'The volume did move, so the mark is written for later stages, and the operation fails to make the affected pods visible.' },
    { topic: 'Relocate', cat: 'concept', text: 'Which steps of a Relocate take the per-cluster migration slot that serializes `Migrate` operations?', options: ['`Validating`', '`Replicating`', '`Committing`', '`Proceeding`', '`Verifying`'], correct: [], explain: 'None. A Relocate replicates between clusters and takes only the volume lock it shares with `Migrate`.' },

    { topic: 'Constraints and comparisons', cat: 'concept', text: 'Which must hold for a volume to move to another cluster by replication?', options: ['One control plane serves both clusters', 'The two clusters\' storage networks reach each other', 'The consuming hosts reach the target\'s storage nodes', 'The CSI secret `simplyblock-csi-secret-v2` lists the target cluster', 'Both clusters have the same number of storage nodes'], correct: [0, 1, 2, 3], explain: 'Replication is between clusters of one control plane, the copy crosses the storage networks over NVMe-oF, the preconnect runs on each consumer, and the driver addresses the target through the secret. Node counts need not match.' },
    { topic: 'Constraints and comparisons', cat: 'concept', text: 'Which of these set a migration apart from a DR failover?', options: ['A migration is planned and runs with both clusters up', 'A migration ends with slots in `cutover_done`, a failover with slots in `failed_over`', 'A migration catches the target up with the final delta before each cutover', 'A failover can run while the source cluster is down', 'A migration requires a policy in `failover` mode'], correct: [0, 1, 2, 3], explain: 'A failover is the unplanned DR operation against the copy the target holds. A migration needs both sides up and ends in `cutover_done`. Scope `cluster` requires a `migration`-mode policy.' },
    { topic: 'Constraints and comparisons', cat: 'concept', text: 'How does Ramen\'s `Relocate` differ from a PersistentVolumeOps `Relocate`?', options: ['Ramen moves the application to another Kubernetes cluster by demoting one side and promoting the other', 'A PersistentVolumeOps Relocate keeps the workload where it runs', 'They are one operation under two names', 'Ramen\'s Relocate requires a supercluster', 'A PersistentVolumeOps Relocate restarts the pods on the target cluster'], correct: [0, 1], explain: 'They share a name. Ramen moves the workload between Kubernetes clusters, and the PersistentVolumeOps Relocate changes which member serves the volume under running pods.' },
    { topic: 'Constraints and comparisons', cat: 'concept', text: 'A volume lives in a cluster that belongs to no supercluster. What moves it to another cluster?', options: ['A ReplicationOps migration', 'A PersistentVolumeOps Relocate', 'A PersistentVolumeOps Migrate', 'Editing the PersistentVolume\'s handle', 'Changing the claim\'s StorageClass'], correct: [0], explain: 'Relocate refuses a volume outside a supercluster, `Migrate` refuses a target node in another cluster, and a handle and a bound claim\'s class cannot be changed.' },
    { topic: 'Constraints and comparisons', cat: 'field', text: 'The target cluster\'s StorageCluster object lives on another Kubernetes cluster. How does the CSI driver here get the target\'s endpoint and credentials?', options: ['From `spec.remoteClusters` of the SimplyblockDriver, which the operator writes into `simplyblock-csi-secret-v2`', 'From the ReplicationPair', 'From the backend policy the ReplicationPolicy created', 'It needs none, the source cluster forwards the calls', 'From the target\'s ControlPlane object'], correct: [0], explain: 'The driver reads every cluster from the secret. Local StorageClusters get their entries from the operator, and a cluster served from outside is declared in `spec.remoteClusters`.' },
  ],

  tasks: [
    {
      title: 'The migration policy',
      scenario: 'Write the policy that carries volumes from site-a to site-b over the pair `site-a-to-site-b`, as a planned move rather than a DR standby, keeping at least as many snapshots as the CRD requires.',
      yaml: [
        'apiVersion: storage.simplyblock.io/v1alpha1',
        'kind: {{kind}}',
        'metadata:',
        '  name: move-to-site-b',
        '  namespace: simplyblock',
        'spec:',
        '  pairRef: {{pair}}',
        '  mode: {{mode}}',
        '  interval: 1m',
        '  snapshotRetention: {{retention}}',
      ].join('\n') + '\n',
      blanks: {
        kind: { accept: ['ReplicationPolicy'], hint: 'the kind with the cadence and the mode, short name `repl`' },
        pair: { accept: ['site-a-to-site-b'], hint: 'the pair by name, in the same namespace' },
        mode: { accept: ['migration'], hint: 'the planned move, not the standby' },
        retention: { accept: ['/([2-9]|[1-9][0-9]+)/'], hint: 'the CRD\'s minimum or more' },
      },
      explain: 'A `ReplicationPolicy` names its pair, `mode: migration` makes the replication end in a planned cutover, and `snapshotRetention` is at least two.',
    },
    {
      title: 'The forecast before the move',
      scenario: 'Write the operation that asks the control plane what moving every volume of the policy `move-to-site-b` would take, without moving anything.',
      yaml: [
        'apiVersion: storage.simplyblock.io/v1alpha1',
        'kind: ReplicationOps',
        'metadata:',
        '  name: forecast-move-to-site-b',
        '  namespace: simplyblock',
        'spec:',
        '  action: {{action}}',
        '  scope: {{scope}}',
        '  ref: {{ref}}',
        '  dryRun: {{dry}}',
      ].join('\n') + '\n',
      blanks: {
        action: { accept: ['migration'], hint: 'the only action a dry run is allowed on' },
        scope: { accept: ['policy'], hint: 'every slot of one policy' },
        ref: { accept: ['move-to-site-b'], hint: 'what that scope names' },
        dry: { accept: ['true'], hint: 'plan and forecast only' },
      },
      explain: 'A migration at scope `policy` names the policy in `ref`. `dryRun: true` sends the plan with `dry_run` set and ends `Succeeded` with the forecast in status.',
    },
    {
      title: 'A Relocate inside a supercluster',
      scenario: 'Move the volume `pvc-4b1d2e7a-9c3f-4a85-b6d0-2e1f7c9a3b58` to the member `prod-fast-mzt` of its supercluster.',
      yaml: [
        'apiVersion: storage.simplyblock.io/{{version}}',
        'kind: PersistentVolumeOps',
        'metadata:',
        '  name: relocate-orders-db',
        'spec:',
        '  persistentVolumeName: pvc-4b1d2e7a-9c3f-4a85-b6d0-2e1f7c9a3b58',
        '  action: {{action}}',
        '  {{block}}:',
        '    targetClusterRef: {{target}}',
      ].join('\n') + '\n',
      blanks: {
        version: { accept: ['v1alpha2'], hint: 'not the replication kinds\' version' },
        action: { accept: ['Relocate'], hint: 'between members, not between nodes' },
        block: { accept: ['relocate'], hint: 'the parameter block the CEL rule pairs with the action' },
        target: { accept: ['prod-fast-mzt'], hint: 'the member, by its StorageCluster name' },
      },
      explain: 'PersistentVolumeOps is `v1alpha2`. `action: Relocate` requires the `relocate` block, whose `targetClusterRef` names another member of the same supercluster.',
    },
  ],
});
