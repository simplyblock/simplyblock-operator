// Chapter 8: a two-node edge cluster with an arbiter. Two storage nodes on an
// edge site, mirroring each other with a 1+1 stripe in clusterMode Dual, and
// the central control plane as the witness that decides which node continues
// when the two lose each other. The chapter follows the same sequence as
// chapter 1 and differs in three places: the edge cluster points at a control
// plane it does not host, the draft declares Dual and turns on arbitration,
// and a NodeHealthCheck with the operator's remediation template lets
// Kubernetes follow a storage fence.
//
// Two words the commissioner's framing uses are adjusted to the code in the
// lesson: the arbiter is the central control plane and not an OpenShift
// arbiter node, and edgeCluster is a statement no controller reads.
TRAINING.register({
  id: 'two-node-edge',
  title: 'Two-node edge clusters with an arbiter',
  summary: 'Two storage nodes on an edge site in `Dual` mode, registered with a central control plane that acts as their witness, and the fencing that lets Kubernetes follow the witness\'s verdict.',
  goal: 'you can deploy a two-node cluster that points at a central control plane, read and tune its arbitration settings, wire a NodeHealthCheck to the operator\'s remediation template, and tell from the status what the witness decided.',

  sections: [
    {
      heading: 'What a two-node edge cluster asks of simplyblock',
      body: [
        'An edge site has two machines with NVMe devices and nothing else worth a storage node. simplyblock runs one storage node on each, in `clusterMode: Dual`, with a `1+1` stripe: one copy of every chunk on each node, so the cluster survives the loss of either. Outside `Dual` every redundant scheme needs at least three storage nodes, because a stripe needs a spare node to rebuild onto. A `Dual` cluster has no spare, and the control plane activates it on exactly its two nodes.',
        'Two nodes cannot tell a dead peer from a cut cable. When they lose each other, each would continue alone and the data would fork. The arbiter is what prevents that: the **central control plane**, on another site, acts as the witness. With `enableArbitration: true` a node that loses its peer holds writes and asks the witness, and only the node the witness grants continues. The other is fenced. The witness is the same control plane the edge cluster registers with through a ControlPlane whose `spec.source.managed` names it, so no third machine on the edge site is involved.',
        { table: { head: ['Machine', 'Where', 'simplyblock on it'], rows: [
          ['edge-0', 'the edge site', 'a storage node, one copy of every chunk'],
          ['edge-1', 'the edge site', 'a storage node, the other copy'],
          ['an OpenShift arbiter node', 'the edge site', 'nothing: it is OpenShift\'s third control-plane node, not the storage witness'],
          ['the central control plane', 'the central site', 'FoundationDB, the management API, and the two-node arbiter'],
        ] } },
        'Two things in the framing of this chapter are adjusted to what the code does. First, OpenShift has its own notion of an **arbiter node**, a third, small control-plane node that a two-node OpenShift cluster adds to keep its own quorum. It is not the storage witness, carries no storage node, and the only place it shows up in this chapter is as a node the discovery run and the NodeHealthCheck must leave out. Second, a ClusterDeploymentConfig has a field `spec.edgeCluster`. It is declared as a statement that the deployment is an edge one, and no controller under `operator/internal` reads it. It documents the deployment and switches nothing on.',
        { callout: 'The two-node design the types cite, `docs/design/two-node-arbitration.md` in the sbcli repository, is not part of this repository. What the arbiter does with holds, leases, and verdicts is the control plane\'s side and cannot be verified here. The chapter teaches the operator\'s side: what it sends, what it reads, and what it puts on the Kubernetes nodes.', kind: 'info' },
      ],
    },
    {
      heading: 'The edge cluster points at the central control plane',
      body: [
        'The Helm chart\'s `managed` profile installs the operator and the CSI driver on the edge cluster and no control plane. It renders a ControlPlane with `spec.source.managed` into the bootstrap ConfigMap, and the operator applies it after it starts. The source is immutable: a control plane the operator installed and one it did not are different deployments, and the clusters and their volumes live in the FoundationDB behind whichever was chosen first.',
        { list: [
          '`endpoint` is the central management API\'s base URL. It goes through the same outbound-URL guard as every other endpoint in the group, so a loopback or link-local address is refused.',
          '`credentialsSecretRef` names a Secret in the operator\'s namespace holding the bearer token under the key `token` or `secret`, read in that order. Naming a Secret that does not exist, or one with neither key, is an error rather than a fallback, because naming one states that the control plane needs it.',
          '`caBundleSecretRef` names a Secret with the CA under `ca.crt` or `tls.crt`, and absent means the system trust store. It verifies the readiness probe and nothing else: the edge operator\'s own calls to the management API trust the system store and the CA bundle mounted from the edge\'s own certificates, and its CSI driver trusts only that mounted bundle. So the central endpoint needs a certificate the edge already trusts, which the [management cluster chapter](chapter.html?c=edge-management-cluster) sets up.',
          '`storageNodeImage` is what the storage nodes run when the cluster\'s own `spec.storageNodes.image` is unset. A local control plane\'s image doubles as that default, a managed one carries nothing to fall back to, so a `managed` deployment without it fails the node workload with `spec.storageNodes.image is unset and ControlPlane simplyblock states no managed image`.',
        ] },
        'The token is the link between the two sites. On the central site, the control plane\'s `spec.source.local.adminTokenSecretRef` (chart value `controlplane.local.adminTokenSecretRef`) names a Secret with a static admin token under `token`, projected into the management API as an environment variable. The edge cluster presents the same value. A Kubernetes TokenReview never crosses a cluster boundary, which is why a static token exists at all.',
        'The operator installs nothing for a managed source. It resolves the endpoint, reads the token, probes the API, and reports: the ControlPlane is `Available` when the probe passes and `Unavailable` when it fails, with `status.endpoint` and `status.version` filled in. A missing or empty Secret raises a `CredentialsError` warning event, an unreachable endpoint an `EndpointUnreachable` one, and the probe repeats on the steady-state interval.',
        { code: 'kubectl get controlplane simplyblock -n simplyblock\nkubectl get events -n simplyblock --field-selector involvedObject.kind=ControlPlane', title: 'Checking the link to the central site' },
      ],
      examples: [
        { title: 'Helm values for the managed profile', file: 'examples/two-node-edge/01-helm-values.yaml', notes: [
          { at: 'profile: managed', text: 'Operator and CSI driver here, control plane elsewhere. The chart renders no discovery run for this profile.' },
          { at: 'endpoint:', text: 'Required for the profile. The chart refuses to render without it.' },
          { at: 'credentialsSecretRef: central-control-plane', text: 'The Secret of the next example. Naming it and not creating it is a `CredentialsError`, and leaving the value empty sends no token at all.' },
          { at: 'storageNodeImage:', text: 'Required in practice: every StorageCluster on a managed deployment gets its storage-node image from here or from its own spec.' },
        ] },
        { title: 'The credentials Secret', file: 'examples/two-node-edge/02-credentials-secret.yaml', notes: [
          { at: 'token:', text: 'The key the operator reads first. `secret` is the other accepted key. Whitespace is trimmed, and an empty value counts as no key.' },
          { at: 'namespace: simplyblock', text: 'The operator\'s namespace. The reference on the ControlPlane is a local one and names no namespace.' },
        ] },
        { title: 'The ControlPlane with a managed source', file: 'examples/two-node-edge/03-controlplane.yaml', notes: [
          { at: 'managed:', text: 'Exactly one of `local` and `managed` is set, and which one cannot change afterward.' },
          { at: 'endpoint: https://control.central.example.com', text: 'Validated against the pattern of an HTTP or HTTPS URL with a host, an optional port, and an optional path.' },
          { at: 'name: central-control-plane', text: 'A LocalObjectReference. The Secret lives beside the ControlPlane.' },
        ] },
      ],
    },
    {
      heading: 'Discovery: a fleet of two is proposed as Dual',
      body: [
        'The chart renders no discovery run for the `managed` profile, so the OperatorOps is written by hand, as it is after an OperatorHub install. The run inspects what `spec.discover.workers` names, and only that: naming `edge-0` and `edge-1` is what keeps every other machine on the site out of the draft, an OpenShift arbiter node included. A named worker that is a control-plane node is still declined unless `enableControlPlaneNodes` is set, so on a two-node OpenShift cluster, where the two nodes carry the control-plane role, the run needs both.',
        'The draft a fleet of exactly two produces is a two-node cluster: `clusterMode: Dual` and a `1+1` stripe, with a note in `status.message` saying so and saying why, that one copy on each worker survives the loss of either and that data migration after a failure runs in relaxed placement, so it needs no third node. A fleet of three would be proposed as a `Standard` cluster, and a fleet of two with a third node let in by mistake is exactly that.',
        'The run proposes the mode and the stripe. It does not propose arbitration: `enableArbitration` and the `arbitration` block are a reviewer\'s decision, because they depend on a witness the run cannot see.',
        { code: 'kubectl get oops edge -n simplyblock -w\nkubectl get cdc discovered-edge -n simplyblock -o jsonpath=\'{.status.message}\'', title: 'Following the run' },
      ],
      examples: [
        { title: 'The discovery run', file: 'examples/two-node-edge/04-operatorops-discover.yaml', notes: [
          { at: 'workers:', text: 'The two storage nodes by their Kubernetes names. The run inspects nothing it does not name, which is how the arbiter node stays out.' },
          { at: 'enableControlPlaneNodes: true', text: 'Named or not, a control-plane node is declined without this. The run then reports `ControlPlaneNodeIncluded` for each one it took.' },
        ] },
      ],
    },
    {
      heading: 'Reviewing the draft: the mode and the arbitration settings',
      body: [
        'The reviewer reads `status.message`, checks that exactly the two workers are listed with their devices, and then writes the part the run could not: arbitration. The three fields sit on `spec.cluster`, the StorageCluster template, and the expansion carries them to the cluster\'s `spec.clusterMode`, `spec.enableArbitration`, and `spec.arbitration` unchanged.',
        { table: { head: ['Field', 'Values', 'What it does'], rows: [
          ['`clusterMode`', '`Single`, `Dual`, `Standard` (default)', 'How many storage nodes the cluster is built for. `Dual` is exactly two, running `1+1`.'],
          ['`enableArbitration`', '`true`, `false`', 'Turns on the control plane\'s two-node arbitration. Without it a `Dual` cluster tolerates the loss of either node without a witness.'],
          ['`arbitration.holdMs`', '500 to 4500', 'How long a node holds writes after losing its peer before it acts on its own. It must stay below the hosts\' NVMe keep-alive timeout of 5000 ms, or clients fail I/O before the verdict arrives.'],
          ['`arbitration.leaseTtlMs`', '500 to 10000, and below `holdMs`', 'How long a lease from the arbiter stays valid. It only matters while a node is holding.'],
          ['`arbitration.hostFencing`', '`true`, `false`', 'Lets Kubernetes follow the arbiter\'s fence of a node: the loser\'s NVMe host identities are revoked on the peer, the operator sets a node condition, and a NodeHealthCheck taints the node out of service.'],
        ] } },
        'Three rules on the API hold the fields together. `enableArbitration` and `arbitration` are admitted only with `clusterMode: Dual`, on the template and on the StorageCluster alike. `leaseTtlMs` must be shorter than `holdMs` when both are set, or the object is refused with `leaseTtlMs must be shorter than holdMs`. And `clusterMode` can never be changed to or from `Single` after the cluster exists, because the control plane fixes that shape at creation. `Dual` carries no such rule on the API, and the arbitration reconciler reads the mode on every pass.',
        'Omitted `holdMs` and `leaseTtlMs` take the arbiter\'s defaults, which live in the control plane and are not stated in this repository. The example states both so that the relation between them is visible.',
        { steps: [
          'Read `status.message`. The stripe note should say `Dual`, and validation should name no problem.',
          'Check that `workers` lists exactly `edge-0` and `edge-1`. A third worker makes the proposal `Standard`.',
          'Set `cluster.clusterMode: Dual` if it is not already, `enableArbitration: true`, and an `arbitration` block with `hostFencing: true`.',
          'Set `cluster.name`, and `edgeCluster: true` if the record should say so.',
          'Set `approved: true`.',
        ] },
      ],
      examples: [
        { title: 'The reviewed draft', file: 'examples/two-node-edge/05-clusterdeploymentconfig.yaml', notes: [
          { at: 'edgeCluster: true', text: 'A statement about the deployment. No controller reads it, so it is written for the record and switches nothing.' },
          { at: 'clusterMode: Dual', text: 'Proposed by the run from the fleet of two. The expansion carries it to the cluster, where the arbitration reconciler and the activation gate read it.' },
          { at: 'enableArbitration: true', text: 'The reviewer\'s line. Admitted only beside `clusterMode: Dual`, and carried to the cluster as is.' },
          { at: 'holdMs: 3000', text: 'Within 500 to 4500, and below the 5000 ms NVMe keep-alive the hosts run.' },
          { at: 'leaseTtlMs: 1500', text: 'Within 500 to 10000 and shorter than `holdMs`, which the CEL rule on the block demands.' },
          { at: 'hostFencing: true', text: 'What makes the operator set `StorageFenced` on a fenced node. Without it the arbiter still decides, and the loser\'s workloads are not moved.' },
          { at: 'parityChunks: 1', text: '`1+1`: one copy per node. The only scheme a `Dual` cluster activates on its two nodes.' },
          { at: 'edge-0', text: 'Exactly two workers. The arbitration reconciler reports `two-node arbitration needs exactly two storage nodes` for any other count and does nothing else.' },
        ] },
      ],
    },
    {
      heading: 'What the operator does after approval',
      body: [
        'The expansion creates the StorageCluster and two StorageNodes, and the phases are the ones chapter 1 showed. The layout check at approval holds a `Dual` cluster to its two nodes: `1+1` on a two-node cluster needs both, and the activation gate refuses to activate with one, with the message `the cluster is a two-node cluster with stripe 1+1, which needs both of its 2 storage nodes`. The cluster is created on the central control plane, under the token from the Secret.',
        'Once the cluster has a `status.uuid` the arbitration reconciler, which runs for StorageClusters in `Dual` mode with `enableArbitration` on, starts following the witness. On every pass it:',
        { list: [
          'reads the arbiter\'s record from the control plane: its state, its epoch, the nodes it holds fenced, and the host fences,',
          'pushes `two_node_arbitration` and `two_node_host_fencing` to the control plane when they differ from the spec, so changing `hostFencing` on the cluster is enough,',
          'reports each node\'s NVMe host NQNs when host fencing is on, and records them in `status.remediation.reportedHostNQNs` on the StorageNode,',
          'writes what Kubernetes knows about each node onto its StorageNode as `status.remediation`, and reports an out-of-service taint to the arbiter as fencing evidence,',
          'writes `status.arbitration` on the cluster: `state`, `epoch`, `fencedNodes`, `hostFences`, and `observedAt`.',
        ] },
        'The pass repeats every 15 seconds while the arbiter is `steady` and every 2 seconds while it is acting. When the record cannot be read, nothing changes on the nodes and `status.arbitration.message` says `cannot read the arbitration record`, followed by the error. The reconciler never decides a verdict and emits no events: its log says what it did.',
        { code: 'kubectl -n simplyblock get storagecluster edge -o jsonpath=\'{.status.arbitration}\'\nkubectl -n simplyblock get storagenode -o jsonpath=\'{range .items[*]}{.spec.workerNode}{"\\t"}{.status.remediation.reportedHostNQNs}{"\\n"}{end}\'', title: 'Reading the witness\'s record' },
        'Each node should list two NQNs: the kernel default the CSI node plugin publishes as the annotation `storage.simplyblock.io/nvme-host-nqn`, and the simplyblock form derived from the node\'s UID. A node with none is one whose host the control plane cannot fence, and the host fence for it reads `unavailable`.',
        'Two limits are worth knowing. A supercluster is left alone by the arbitration reconciler, since its `status.uuid` names no backend cluster, while a member in `Dual` mode is arbitrated on its own like any other cluster. And `Dual` mode is covered by unit tests of the arbitration, host fence, and remediation reconcilers only, with no end-to-end test in the repository, so a fence on a real cluster is something to rehearse rather than assume.',
      ],
    },
    {
      heading: 'A fence, step by step',
      body: [
        'With host fencing on, a partition ends with a winner instead of stopping I/O on both nodes. The sequence below is what the code does, from the record the control plane holds to the taint on the node and back.',
        { steps: [
          'The arbiter grants one node and fences the other. The operator reads the record and puts the taint `storage.simplyblock.io/fenced=<epoch>:NoExecute` on the loser\'s Kubernetes node, and keeps it through healing until the arbiter is `steady` again.',
          'The control plane revokes every NVMe host identity of the loser on the winner\'s volumes. Its host fence reads `revoking`, then `revoked`.',
          'On `revoked`, the operator sets the node condition `StorageFenced=True` with reason `WitnessVerdict` and a message naming the epoch and the winner. A `revoking` fence sets no condition yet.',
          'NodeHealthCheck sees the condition and creates a StorageFenceRemediation for the node from the template.',
          'The remediation reconciler confirms the fence with the control plane, not only with the condition, and applies `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute` with the annotation `storage.simplyblock.io/out-of-service-by: storage-fence-remediation`. Pods are force-deleted, VolumeAttachments force-detached, and the workloads restart on the winner. The remediation reads `Tainted`.',
          'The storage heals, and the host fence reads `releasable`. The operator waits until the node is `Ready` and no pod from before the fence is left on it, except the ones tolerating the out-of-service taint, then asks the control plane to give the host its access back. The condition turns `False` with reason `NotFenced`.',
          'NodeHealthCheck deletes the remediation, and the remediation reconciler removes the taint it set. A taint an administrator set, or one without the annotation, stays.',
        ] },
        { table: { head: ['Host fence state', 'Condition', 'Meaning'], rows: [
          ['`revoking`', 'none yet', 'The control plane is revoking the node\'s hosts on the winner.'],
          ['`revoked`', '`StorageFenced=True`', 'Revoked. The remediation may taint the node.'],
          ['`releasable`', '`StorageFenced=True`', 'The storage has healed. The operator releases once the node is Ready and clean.'],
          ['`restoring`', '`StorageFenced=True`', 'The release is running.'],
          ['`unavailable`', 'none', 'No host NQN is known for the node: nothing revoked, no condition, no remediation.'],
        ] } },
        { table: { head: ['Remediation phase', 'Meaning'], rows: [
          ['`Pending`', 'The fence is not confirmed by the control plane, or host fencing is off for the cluster. `status.message` says which.'],
          ['`Blocked`', 'A volume of another CSI driver is attached, and `allowForeignVolumes` is false.'],
          ['`Tainted`', 'The out-of-service taint is on the node, with the epoch in `status.epoch`.'],
        ] } },
        'The host is not fenced, and nothing is restarted, when only the storage service is affected: an SPDK crash, a planned restart of the storage node, or the node losing the control plane while it still reaches its peer. Its workloads keep running through the peer\'s path. And the operator reports an out-of-service taint to the arbiter as fencing evidence only when it did not set that taint itself: its own remediation follows the arbiter\'s verdict and is not new evidence about the node.',
        { callout: 'Every stateful workload on the two nodes should use simplyblock volumes. The out-of-service taint force-detaches every volume of the node, and only simplyblock\'s are fenced, so a node with another driver\'s volume attached is left in `Blocked` unless the template sets `allowForeignVolumes: true`.', kind: 'warn' },
      ],
      examples: [
        { title: 'The remediation template', file: 'examples/two-node-edge/06-storagefenceremediationtemplate.yaml', notes: [
          { at: 'kind: StorageFenceRemediationTemplate', text: 'The operator\'s CRD, short name `sfrt`. NodeHealthCheck creates a StorageFenceRemediation, short name `sfr`, from it per fenced node.' },
          { at: 'allowForeignVolumes: false', text: 'The one field a remediation has. `true` lets the taint go on although a volume of another driver is attached and would be force-detached unfenced.' },
        ] },
        { title: 'The NodeHealthCheck', file: 'examples/two-node-edge/07-nodehealthcheck.yaml', notes: [
          { at: 'kind: NodeHealthCheck', text: 'A medik8s object. The operator neither installs the NodeHealthCheck operator nor validates this manifest. On OpenShift it comes from OperatorHub.' },
          { at: 'kubernetes.io/hostname', text: 'The two storage nodes by name. On a two-node OpenShift cluster with an arbiter node the selector must match the two workers and not the arbiter, and naming them is the plainest way.' },
          { at: 'maxUnhealthy: 1', text: 'Required. With the default of 51% healthy, one node out of two is never remediated.' },
          { at: 'type: StorageFenced', text: 'The condition the operator sets. Its `duration` is how long it must hold before the check acts.' },
          { at: 'name: storage-fence', text: 'The template of the previous example, in the operator\'s namespace.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The ControlPlane reports `Unavailable` with a `CredentialsError` event.', cause: 'The Secret `credentialsSecretRef` names does not exist in the operator\'s namespace, or carries neither a `token` nor a `secret` key.', fix: 'Create the Secret with the admin token under `token`, the same value the central side\'s `adminTokenSecretRef` holds. The probe repeats on its own.' },
          { symptom: 'The ControlPlane reports `Unavailable` with `EndpointUnreachable`.', cause: 'The endpoint is a loopback or link-local address, which the URL guard refuses, or the central API is not reachable from the edge site, or its certificate is not trusted.', fix: 'Use the central API\'s routable URL. Naming a private CA in `caBundleSecretRef` lets the probe pass and nothing more, because the operator\'s and the CSI driver\'s calls do not read it. Serve the central API with a certificate the edge already trusts, as the management cluster chapter describes.' },
          { symptom: 'The draft proposes `Standard` with three workers.', cause: 'The run inspected a third machine, such as an OpenShift arbiter node with a spare disk.', fix: 'Name the two storage nodes in `spec.discover.workers` on a new run. A finished run is not repeated.' },
          { symptom: 'The draft is refused with `enableArbitration and arbitration are only valid when clusterMode is Dual`.', cause: 'The reviewer added arbitration to a template whose mode was left at the default `Standard`.', fix: 'Set `cluster.clusterMode: Dual`. The same rule holds on the StorageCluster.' },
          { symptom: 'The draft is refused with `leaseTtlMs must be shorter than holdMs`.', cause: 'Both were set and the lease is as long as the hold or longer.', fix: 'Lower `leaseTtlMs` or raise `holdMs`, keeping `holdMs` at most 4500.' },
          { symptom: 'The storage-node pods never start on a managed deployment, and the error names `spec.storageNodes.image`.', cause: 'A managed control plane carries no image to fall back to, and neither `storageNodeImage` on the ControlPlane nor the cluster\'s own image is set.', fix: 'Set `controlplane.managed.storageNodeImage` in the chart, or `spec.storageNodes.image` on the cluster.' },
          { symptom: '`status.arbitration.message` says `two-node arbitration needs exactly two storage nodes`.', cause: 'The cluster has one node or three, so the reconciler reports and does nothing.', fix: 'A `Dual` cluster is built for exactly two. Check the draft\'s workers before approval, since the stripe cannot be changed afterward.' },
          { symptom: '`StorageFenced=True` holds and no StorageFenceRemediation appears.', cause: 'The NodeHealthCheck operator is not installed, `maxUnhealthy` was left at its default, or the selector does not match the node.', fix: 'Install the operator, set `maxUnhealthy: 1`, and make the selector match the two workers and not the arbiter node.' },
          { symptom: 'The remediation stays `Blocked`.', cause: 'A VolumeAttachment of another CSI driver is on the node, and the template refuses to force-detach it unfenced.', fix: 'Detach the other driver\'s volume, or set `allowForeignVolumes: true` on the template if losing it is acceptable.' },
          { symptom: 'The host fence stays `releasable`, and the log says `waiting for the node to be clean`.', cause: 'The node is not `Ready`, or pods from before the fence are still bound to it, which happens when the out-of-service taint was never applied.', fix: 'Fix what keeps the node from `Ready`, or get the stale pods off the node. The release then runs by itself.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Witness and modes', cat: 'concept', text: 'What is the arbiter of a two-node simplyblock cluster?', options: ['The central control plane, acting as the witness', 'An OpenShift arbiter node on the edge site', 'A third storage node with no devices', 'The operator on the edge cluster', 'The CSI controller plugin'], correct: [0], explain: 'The witness is the central control plane the edge cluster registers with. An OpenShift arbiter node is a control-plane node for OpenShift\'s own quorum and plays no part in storage arbitration, and the operator only follows the verdict.' },
    { topic: 'Witness and modes', cat: 'concept', text: 'Which of these are true of `clusterMode: Dual`?', options: ['It is exactly two storage nodes', 'It runs a `1+1` stripe, one copy on each node', 'It is proposed by discovery for a fleet of two', 'It is the default mode', 'It can be changed to `Single` later'], correct: [0, 1, 2], explain: '`Dual` is two nodes mirroring each other, and a fleet of exactly two is proposed as one. The default is `Standard`, and a change to or from `Single` is refused after creation.' },
    { topic: 'Witness and modes', cat: 'concept', text: 'Why can a cluster outside `Dual` mode not run `1+1` on two nodes?', options: ['Every redundant scheme needs a spare node to rebuild onto, so `1+1` needs three', 'Two nodes cannot hold two chunks', 'FoundationDB needs three coordinators', 'The CSI driver needs three nodes', 'The witness needs a third node'], correct: [0], explain: 'Each scheme\'s minimum is its chunks plus one spare per tolerated failure. `Dual` is the declared exception: `1+1` on exactly two, with no spare, and migration after a failure in relaxed placement.' },
    { topic: 'Witness and modes', cat: 'field', text: 'What does `spec.edgeCluster` on a ClusterDeploymentConfig do?', options: ['Turns on arbitration', 'Selects the managed profile', 'Makes the draft propose `Dual`', 'Shortens the provisioning budget', 'Registers the cluster with the central control plane'], correct: [], explain: 'None of these. The field states that the deployment is an edge one, and no controller reads it. It documents the deployment and switches nothing.' },
    { topic: 'Witness and modes', cat: 'concept', text: 'On a two-node OpenShift cluster with an arbiter node, which of these does the arbiter node do for simplyblock?', options: ['Hosts a storage node', 'Decides which storage node continues on a partition', 'Holds the credentials Secret', 'Is proposed as a worker by default', 'Must be left out by the discovery run and the NodeHealthCheck'], correct: [4], explain: 'It is OpenShift\'s third control-plane node and nothing simplyblock uses. The run names the two storage nodes so it is never inspected, and the NodeHealthCheck selector matches the two workers and not the arbiter.' },
    { topic: 'Managed control plane', cat: 'field', text: 'Which chart values does an edge install with the `managed` profile state?', options: ['`deployment.profile: managed`', '`controlplane.managed.endpoint`', '`controlplane.managed.credentialsSecretRef`', '`controlplane.managed.storageNodeImage`', '`discovery.enableControlPlaneNodes`'], correct: [0, 1, 2, 3], explain: 'The profile, the endpoint, the token Secret, and the storage-node image, which a managed control plane cannot otherwise provide. The chart renders no discovery run for this profile, so its discovery values are unused.' },
    { topic: 'Managed control plane', cat: 'field', text: 'Under which keys does the operator read the bearer token from the Secret `credentialsSecretRef` names?', options: ['`token`', '`secret`', '`password`', '`bearer`', '`admin-token`'], correct: [0, 1], explain: '`token` is read first and `secret` second. Whitespace is trimmed, and a Secret with neither key is a `CredentialsError`.' },
    { topic: 'Managed control plane', cat: 'concept', text: 'What does the operator do with a ControlPlane whose `spec.source.managed` is set?', options: ['Installs FoundationDB on the edge cluster', 'Resolves the endpoint, probes it, and reports the phase', 'Creates the StorageCluster on the central control plane', 'Copies the central control plane\'s image to the storage nodes', 'Opens a tunnel to the central site'], correct: [1], explain: 'It installs nothing and owns nothing: the probe is the only signal, and the phase is `Available` or `Unavailable`. The cluster is created later, by the StorageCluster reconciler, through that endpoint.' },
    { topic: 'Managed control plane', cat: 'concept', text: 'Why does the central control plane need `adminTokenSecretRef` for an edge cluster to register with it?', options: ['A Kubernetes TokenReview never crosses a cluster boundary', 'The management API refuses every unauthenticated call', 'FoundationDB requires a token per cluster', 'The operator cannot read ServiceAccount tokens', 'TLS needs a shared secret'], correct: [0], explain: 'The central control plane accepts its own cluster\'s Kubernetes identities through TokenReview, which cannot verify a token issued elsewhere. A static admin token is what the edge operator can present instead.' },
    { topic: 'Managed control plane', cat: 'field', text: 'Which statements about `spec.source` of a ControlPlane are true?', options: ['Exactly one of `local` and `managed` is set', 'Which one is set cannot change afterward', 'The fields inside the chosen member stay editable', 'Both can be set to run a control plane with a backup', 'A ControlPlane by any name is reconciled'], correct: [0, 1, 2], explain: 'The two modes are siblings, one is chosen at creation, and switching is refused because the clusters live in the FoundationDB behind the old one. Only a ControlPlane named `simplyblock` in the operator\'s namespace is reconciled.' },
    { topic: 'Managed control plane', cat: 'action', text: 'A storage-node workload on a managed deployment fails with `spec.storageNodes.image is unset and ControlPlane simplyblock states no managed image`. What fixes it?', options: ['Set `storageNodeImage` on the managed source', 'Set `spec.storageNodes.image` on the StorageCluster', 'Set `spec.source.local.image` as well', 'Re-create the ControlPlane', 'Install the control plane on the edge cluster'], correct: [0, 1], explain: 'A managed control plane is another cluster\'s install and carries no image to fall back to, so either the ControlPlane states one or the cluster does. Setting `local` beside `managed` is refused.' },
    { topic: 'Managed control plane', cat: 'field', text: 'Which of these values of `spec.source.managed.endpoint` are refused, at admission or by the operator\'s URL guard?', options: ['`https://control.central.example.com`', '`http://127.0.0.1:8080`', '`https://control.central.example.com:8443/api`', '`https://169.254.1.1`', '`ftp://control.central.example.com`'], correct: [1, 3, 4], explain: 'The pattern on the field admits HTTP and HTTPS URLs with a host, an optional port, and a path, so `ftp` fails at admission. A loopback or link-local address passes the pattern and is refused by the outbound-URL guard when the operator resolves it, reported as `EndpointUnreachable`.' },
    { topic: 'Discovery and the draft', cat: 'concept', text: 'Why is the discovery run of an edge cluster with the `managed` profile written by hand?', options: ['The chart renders no discovery run for that profile', 'The central control plane runs discovery', 'Discovery needs the control plane to be local', 'The OperatorOps CRD is not installed by the managed profile', 'A managed cluster has no draft'], correct: [0], explain: 'The chart raises its one run on a fresh `standalone` install only. On the edge cluster the operator runs discovery as it does anywhere, from an OperatorOps somebody writes.' },
    { topic: 'Discovery and the draft', cat: 'field', text: 'What does naming `edge-0` and `edge-1` in `spec.discover.workers` do?', options: ['Limits the run to those two nodes', 'Allows control-plane nodes to be used', 'Keeps an OpenShift arbiter node out of the draft', 'Sets `clusterMode: Dual` directly', 'Approves the draft for those nodes'], correct: [0, 2], explain: 'The run inspects what it is told and nothing else, which is what keeps every other machine out. A named control-plane node still needs `enableControlPlaneNodes`, and the mode follows from the fleet size rather than from the list.' },
    { topic: 'Discovery and the draft', cat: 'concept', text: 'A fleet of exactly two is discovered. Which of these does the draft carry?', options: ['`clusterMode: Dual`', 'A `1+1` stripe', '`enableArbitration: true`', 'An `arbitration` block', '`edgeCluster: true`'], correct: [0, 1], explain: 'The run proposes the mode and the stripe, with a note saying why. Arbitration is a reviewer\'s decision, because it depends on a witness the run cannot see, and `edgeCluster` is read by nothing.' },
    { topic: 'Discovery and the draft', cat: 'concept', text: 'The two storage nodes of a two-node OpenShift cluster carry the control-plane role. The run names them in `workers` and nothing else. What happens?', options: ['Both are inspected, because they are named', 'Both are declined as control-plane nodes, and the run fails with no free worker', 'One of them is inspected', 'The arbiter node is inspected instead', 'The draft proposes `Single`'], correct: [1], explain: 'Named workers are filtered first, and the control-plane check still applies. Without `enableControlPlaneNodes: true` each is declined with `is not used`, and a run with no worker left fails.' },
    { topic: 'Discovery and the draft', cat: 'concept', text: 'A third machine on the site was inspected by mistake. What does the draft propose?', options: ['A `Standard` cluster', 'A `Dual` cluster with a spare', 'A `Single` cluster', 'An error, because three nodes cannot run `1+1`', 'A `Dual` cluster, with the third node declined'], correct: [0], explain: 'Three is a `Standard` fleet, and the run proposes the scheme three nodes carry. The fix is a new run naming the two storage nodes, since a finished run is not repeated.' },
    { topic: 'Arbitration settings', cat: 'field', text: 'Which rules does the API enforce on the arbitration fields?', options: ['`enableArbitration` and `arbitration` need `clusterMode: Dual`', '`leaseTtlMs` must be shorter than `holdMs` when both are set', '`holdMs` is between 500 and 4500', '`leaseTtlMs` is between 500 and 10000', '`hostFencing` needs `enableArbitration: true`'], correct: [0, 1, 2, 3], explain: 'The first four are CEL rules and bounds on the types. `hostFencing` without `enableArbitration` is admitted and simply has no effect: the operator asks for host fencing only when the mode is `Dual`, arbitration is on, and `hostFencing` is true.' },
    { topic: 'Arbitration settings', cat: 'concept', text: 'Why is `holdMs` capped at 4500?', options: ['It must stay below the hosts\' NVMe keep-alive timeout of 5000 ms', 'FoundationDB leases expire at 5 seconds', 'The reconciler requeues every 5 seconds', 'Kubernetes evicts pods after 5 seconds', 'The lease is renewed every 5 seconds'], correct: [0], explain: 'A node holds writes while it waits for the verdict. Held longer than the keep-alive, clients fail I/O before the verdict arrives.' },
    { topic: 'Arbitration settings', cat: 'field', text: 'A reviewer sets `enableArbitration: true` and `arbitration.hostFencing: true` on a template left at `clusterMode: Standard`. What happens?', options: ['The document is refused at admission', 'The expansion sets the mode to `Dual`', 'The settings are ignored', 'The cluster is created and arbitration turned on later', 'The run is repeated'], correct: [0], explain: 'The CEL rule on the template refuses it with `enableArbitration and arbitration are only valid when clusterMode is Dual`. The same rule holds on the StorageCluster.' },
    { topic: 'Arbitration settings', cat: 'concept', text: 'What does leaving `holdMs` and `leaseTtlMs` out of the `arbitration` block mean?', options: ['Arbitration is off', 'The arbiter\'s defaults apply, which live in the control plane', 'The operator picks 4500 and 500', 'The draft is refused', 'The values are read from the peer node'], correct: [1], explain: 'All arbitration fields are optional, and omitted values take the arbiter\'s defaults. Those defaults are not stated in this repository.' },
    { topic: 'Arbitration settings', cat: 'action', text: 'Host fencing was turned on by editing `spec.arbitration.hostFencing` on a running `Dual` cluster. What does the operator do?', options: ['Pushes the setting to the control plane on its next pass', 'Starts reporting each node\'s host NQNs', 'Re-creates the cluster', 'Reboots both nodes', 'Refuses the edit, because arbitration is immutable'], correct: [0, 1], explain: 'The reconciler pushes `two_node_arbitration` and `two_node_host_fencing` whenever they differ from the spec, and with host fencing on it reports the host identities the control plane would revoke. The settings only change what the next loss does.' },
    { topic: 'Fencing and remediation', cat: 'concept', text: 'Which objects does the operator write on the Kubernetes node the arbiter fenced?', options: ['The taint `storage.simplyblock.io/fenced` with the epoch as value', 'The condition `StorageFenced=True`, once the host fence is `revoked`', 'The taint `node.kubernetes.io/out-of-service`, from the arbitration reconciler', 'A StorageFenceRemediation', 'A NodeHealthCheck'], correct: [0, 1], explain: 'The arbitration reconciler sets the fenced taint and, with host fencing, the condition. The out-of-service taint is set only by the remediation reconciler, on a StorageFenceRemediation that NodeHealthCheck creates.' },
    { topic: 'Fencing and remediation', cat: 'concept', text: 'Which of these happen when a node\'s host fence reads `revoking`?', options: ['The condition `StorageFenced=True` is set', 'A StorageFenceRemediation is created', 'The out-of-service taint is applied', 'The node\'s pods are restarted on the peer', 'The cluster is marked `Degraded`'], correct: [], explain: 'None yet. The control plane is still revoking the node\'s hosts on the winner, and the condition, the remediation, and the taint follow once the fence reads `revoked`.' },
    { topic: 'Fencing and remediation', cat: 'field', text: 'What does the remediation reconciler check before it applies the out-of-service taint?', options: ['The node carries `StorageFenced=True`', 'A storage node with a control-plane id runs on the node', 'Its cluster asks for host fencing', 'The control plane holds an effective host fence for the node', 'No volume of another CSI driver is attached, unless `allowForeignVolumes` is set'], correct: [0, 1, 2, 3, 4], explain: 'All five. The condition is the operator\'s copy and the taint must not outlive the fence it relies on, so the fence is confirmed with the control plane on every pass before the taint goes on.' },
    { topic: 'Fencing and remediation', cat: 'field', text: 'Which phases can a StorageFenceRemediation report?', options: ['`Pending`', '`Blocked`', '`Tainted`', '`Fenced`', '`Released`'], correct: [0, 1, 2], explain: '`Pending` while the fence is not confirmed or host fencing is off, `Blocked` when a foreign volume refuses the taint, and `Tainted` once the taint is on. The release is reported on the cluster\'s `status.arbitration.hostFences`, not on the remediation.' },
    { topic: 'Fencing and remediation', cat: 'concept', text: 'When does the operator ask the control plane to give a fenced host its access back?', options: ['When the host fence reads `releasable`, the node is `Ready`, and no pod from before the fence is left on it', 'As soon as the node is `Ready`', 'When NodeHealthCheck deletes the remediation', 'When the administrator removes the taint', 'Every 15 seconds while the arbiter is `steady`'], correct: [0], explain: 'A pod from before the fence may be the first instance of a workload that runs on the peer now, and giving the host its access back while it lives would let both write. Pods tolerating the out-of-service taint, the storage pods, are not counted.' },
    { topic: 'Fencing and remediation', cat: 'concept', text: 'Which out-of-service taints does the operator report to the arbiter as positive fencing evidence?', options: ['One set by an administrator', 'One set by another remediation', 'One its own StorageFenceRemediation set', 'A `NotReady` condition', 'The `storage.simplyblock.io/fenced` taint'], correct: [0, 1], explain: 'A taint the storage fence remediation set follows the arbiter\'s own verdict and is not new evidence, so it carries an annotation and is left out. `NotReady` is also what a partition looks like, and the fenced taint is the verdict itself.' },
    { topic: 'Fencing and remediation', cat: 'action', text: 'The remediation stays `Blocked`. Which of these resolve it?', options: ['Detach the other driver\'s volume', 'Set `allowForeignVolumes: true` on the template', 'Delete the NodeHealthCheck', 'Remove the `StorageFenced` condition by hand', 'Restart the operator'], correct: [0, 1], explain: 'The taint would force-detach a volume nothing fenced, so the remediation waits for the volume to go or for the template to allow it. The condition is the operator\'s to set, and the check and the operator are not at fault.' },
    { topic: 'Fencing and remediation', cat: 'field', text: 'Why does the NodeHealthCheck for a two-node cluster need `maxUnhealthy: 1`?', options: ['With the default of 51% healthy, one node out of two is never remediated', 'NodeHealthCheck refuses a check without it', 'The operator reads it to decide the winner', 'It limits the fence to one epoch', 'It is what names the template'], correct: [0], explain: 'One fenced node of two is half the pool, below the default healthy threshold, so the check would never act. The operator never reads the NodeHealthCheck.' },
    { topic: 'Operations and limits', cat: 'concept', text: 'What does the arbitration reconciler do when the control plane cannot be read?', options: ['Leaves the taints and conditions as they are', 'Writes `cannot read the arbitration record` into `status.arbitration.message`', 'Fences both nodes to be safe', 'Removes every taint', 'Marks the cluster `Degraded`'], correct: [0, 1], explain: 'No verdict to follow means nothing changes on the nodes. The message is recorded and the pass repeats after 5 seconds.' },
    { topic: 'Operations and limits', cat: 'field', text: 'Which fields does `status.arbitration` on a StorageCluster carry?', options: ['`state`', '`epoch`', '`fencedNodes`', '`hostFences`', '`winner`'], correct: [0, 1, 2, 3], explain: 'The record as last read, with `observedAt` and a `message`. A winner is named per host fence, under `hostFences[].winner`, and not at the top level.' },
    { topic: 'Operations and limits', cat: 'concept', text: 'How is `Dual` mode verified in this repository?', options: ['By unit tests of the arbitration, host fence, and remediation reconcilers', 'By an end-to-end test on a two-node cluster', 'By the support tool\'s admission test of the training examples', 'By a chaos test that cuts the link between the nodes', 'It is not tested'], correct: [0, 2], explain: 'The reconcilers have unit tests, and the training manifests are admitted against the CRDs in CI. There is no end-to-end test of a fence, so one on a real cluster is something to rehearse.' },
    { topic: 'Operations and limits', cat: 'concept', text: 'A `Dual` cluster has three StorageNodes. Which of these does the operator do?', options: ['Reports `two-node arbitration needs exactly two storage nodes` and follows no verdict', 'Refuses the third StorageNode at admission', 'Fences the third node', 'Converts the cluster to `Standard`', 'Deletes the newest node'], correct: [0], explain: 'The arbitration reconciler records the message and does nothing else for any count but two. The mode and the stripe are fixed at creation, so the layout is checked before approval rather than repaired after it.' },
    { topic: 'Operations and limits', cat: 'concept', text: 'How does arbitration relate to a supercluster?', options: ['A supercluster is left alone by the arbitration reconciler', 'A member in `Dual` mode is arbitrated on its own', 'A supercluster of `Dual` members is refused', 'The supercluster\'s status carries the members\' verdicts', 'Arbitration is pushed to the supercluster\'s uuid'], correct: [0, 1], explain: 'A supercluster has no nodes and its uuid names no backend cluster, so there is nothing to arbitrate. Arbitration is per cluster and unchanged for the members, and a supercluster of `Dual` members is admitted.' },
    { topic: 'Operations and limits', cat: 'action', text: '`status.arbitration.hostFences` shows a node as `unavailable`. What does it mean, and what is checked?', options: ['No host NQN is known for the node, so nothing can be revoked', 'The CSI node plugin is not running on the node, or the StorageNode has no `reportedHostNQNs`', 'The node is powered off', 'The central control plane is unreachable', 'The NodeHealthCheck is missing'], correct: [0, 1], explain: 'Without a host identity the control plane has nothing to revoke, so no condition and no remediation follow. The plugin publishes the kernel NQN as an annotation, and the operator reports it along with the UID-derived one.' },
  ],

  tasks: [
    {
      title: 'The ControlPlane of the edge cluster',
      scenario: 'Write the control plane the edge operator reconciles: a control plane it does not host, reached at the central management API with a bearer token from a Secret.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: ControlPlane\nmetadata:\n  name: {{name}}\n  namespace: simplyblock\nspec:\n  source:\n    {{source}}:\n      endpoint: {{endpoint}}\n      {{ref}}:\n        name: central-control-plane\n      storageNodeImage: quay.io/simplyblock-io/simplyblock:26.4.0-beta4\n',
      blanks: {
        name: { accept: ['simplyblock'], hint: 'the only name the operator reconciles' },
        source: { accept: ['managed'], hint: 'pointed at, not hosted' },
        endpoint: { accept: ['/https?:\\/\\/[a-zA-Z0-9.-]+(:[0-9]{1,5})?(\\/.*)?/'], hint: 'an HTTP or HTTPS URL with a host, not a loopback address' },
        ref: { accept: ['credentialsSecretRef'], hint: 'the field naming the Secret with the token' },
      },
      explain: 'Only `simplyblock` is reconciled. `source.managed` installs nothing and probes the endpoint, which must be an HTTP or HTTPS URL, and `credentialsSecretRef` names the Secret whose `token` or `secret` key holds the bearer token.',
    },
    {
      title: 'The cluster template of the reviewed draft',
      scenario: 'Complete the cluster template so that the two nodes form a two-node cluster under the central control plane\'s arbitration, with host fencing, a hold of 3000 ms, and a lease the API admits beside it.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: ClusterDeploymentConfig\nmetadata:\n  name: discovered-edge\n  namespace: simplyblock\nspec:\n  approved: true\n  environment: OpenShift\n  cluster:\n    name: edge\n    maxSubsystemCount: 50\n    vcpuCount: 8\n    clusterMode: {{mode}}\n    enableArbitration: {{arb}}\n    arbitration:\n      holdMs: 3000\n      leaseTtlMs: {{lease}}\n      hostFencing: {{fencing}}\n    stripe:\n      dataChunks: 1\n      parityChunks: {{parity}}\n  nodeSets:\n    - name: edge\n      groups:\n        - name: default\n          workers: [edge-0, edge-1]\n          mgmtInterface: br-ex\n          dataInterfaces: [br-ex]\n          devices:\n            nvme: ["0000:c1:00.0"]\n',
      blanks: {
        mode: { accept: ['Dual'], hint: 'the mode built for exactly two storage nodes' },
        arb: { accept: ['true'], hint: 'the witness is consulted only when this is on' },
        lease: { accept: ['/([5-9][0-9]{2}|1[0-9]{3}|2[0-9]{3})/'], hint: 'at least 500 and shorter than the hold' },
        fencing: { accept: ['true'], hint: 'lets Kubernetes follow the verdict' },
        parity: { accept: ['1'], hint: 'one copy on each node' },
      },
      explain: '`Dual` is the mode arbitration is valid with, `enableArbitration` turns the witness on, `leaseTtlMs` has to be between 500 and 10000 and shorter than `holdMs`, `hostFencing` makes the operator set `StorageFenced`, and `1+1` is the one scheme a two-node cluster activates.',
    },
    {
      title: 'The NodeHealthCheck',
      scenario: 'Write the medik8s check that remediates a storage-fenced node of the two with the operator\'s template, so that one fenced node out of two is acted on.',
      yaml: 'apiVersion: remediation.medik8s.io/v1alpha1\nkind: NodeHealthCheck\nmetadata:\n  name: simplyblock-storage-fenced\nspec:\n  selector:\n    matchExpressions:\n      - key: kubernetes.io/hostname\n        operator: In\n        values: [edge-0, edge-1]\n  maxUnhealthy: {{max}}\n  unhealthyConditions:\n    - type: {{condition}}\n      status: "True"\n      duration: 2s\n  remediationTemplate:\n    apiVersion: {{group}}\n    kind: {{kind}}\n    namespace: simplyblock\n    name: storage-fence\n',
      blanks: {
        max: { accept: ['1'], hint: 'the default of 51% healthy never acts on one node of two' },
        condition: { accept: ['StorageFenced'], hint: 'the node condition the operator sets' },
        group: { accept: ['storage.simplyblock.io/v1alpha2'], hint: 'the operator\'s API group and version' },
        kind: { accept: ['StorageFenceRemediationTemplate'], hint: 'the template kind, short name `sfrt`' },
      },
      explain: '`maxUnhealthy: 1` is what lets the check act on one node of two, `StorageFenced` is the condition the arbitration reconciler sets, and the template is the operator\'s `StorageFenceRemediationTemplate` at `storage.simplyblock.io/v1alpha2`.',
    },
  ],
});
