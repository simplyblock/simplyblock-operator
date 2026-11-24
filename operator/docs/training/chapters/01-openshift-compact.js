// Chapter 1: a three-node compact cluster on OpenShift. The foundation chapter,
// because every later deployment repeats its sequence: the security context
// constraints, the operator, the ControlPlane and the driver, a discovery run,
// the draft that is reviewed and approved, and the pool and class that appear.
// What is specific to a compact cluster is one flag on the discovery run and
// the consequence of every node also hosting the control plane.
//
// It is the model chapter: chapter-schema.js says what it has to contain, and
// the next chapter is written by copying this one.
TRAINING.register({
  id: 'openshift-compact',
  title: 'A three-node compact cluster on OpenShift',
  summary: 'The whole deployment sequence on the smallest production layout: three OpenShift nodes that are control plane and workers at once, each with its own NVMe devices.',
  goal: 'you can take a compact OpenShift cluster from an empty namespace to a bound volume, know which objects you write and which the operator writes, and can read a discovery draft before approving it.',

  sections: [
    {
      heading: 'What a compact cluster asks of simplyblock',
      body: [
        'A compact OpenShift cluster has three nodes and nothing else: each carries the `master` and the `worker` role, runs etcd and the API server, and schedules workloads. simplyblock puts two more things on them. The control plane, that is FoundationDB with three coordinators, the management API, and the object store, and one storage node per node, driving the node\'s NVMe devices through SPDK.',
        'Three is also the smallest node count the default erasure-coding scheme allows. A stripe of `1+1` needs two nodes to place its chunks on and one spare to rebuild onto, so a `1+1` cluster is refused below three storage nodes, and the three nodes of a compact cluster are exactly enough.',
        { table: { head: ['Node', 'Kubernetes roles', 'simplyblock on it'], rows: [
          ['master-0', '`master`, `worker`', 'a FoundationDB coordinator, a storage node'],
          ['master-1', '`master`, `worker`', 'a FoundationDB coordinator, a storage node'],
          ['master-2', '`master`, `worker`', 'a FoundationDB coordinator, a storage node'],
        ] } },
        'Two consequences follow from every node hosting a coordinator, and both show up later in the chapter. The discovery run has to be told that control-plane nodes are allowed, because by default it leaves them out. And the nodes are added to the cluster one at a time, whatever the provisioning budget says, because adding a storage node reboots its host and two coordinators rebooting together would leave the database below its fault tolerance.',
      ],
    },
    {
      heading: 'Before the operator: SCCs and the namespace',
      body: [
        'The operator and everything it runs live in the `simplyblock` namespace. Its service accounts need the `privileged` and `anyuid` security context constraints, and the namespace needs the `privileged` Pod Security level. The operator grants none of this itself: it is the one thing an OpenShift administrator does by hand before anything is installed, and the install guide in the operator\'s OperatorHub entry says the same.',
        { callout: 'A storage-node pod that cannot get the `privileged` SCC stays in `CreateContainerConfigError` or is refused at admission. The operator reports the DaemonSet as not ready, and the node\'s phase never leaves `Provisioning`. Check the bindings first.', kind: 'warn' },
      ],
      examples: [
        { title: 'The two SCC bindings', file: 'examples/openshift-compact/01-scc-bindings.yaml', notes: [
          { at: 'system:openshift:scc:privileged', text: 'The ClusterRole OpenShift generates for each SCC. Binding it to a group is what grants the SCC.' },
          { at: 'system:serviceaccounts:simplyblock', text: 'Every service account of the namespace, so the operator, the control plane, the storage nodes, and the CSI plugins are all covered by one binding.' },
          { at: 'pod-security.kubernetes.io/enforce', text: 'The namespace label that lets privileged pods in at all. Without it the SCC is granted and the pod is still refused.' },
        ] },
      ],
    },
    {
      heading: 'Installing the operator: OperatorHub or Helm',
      body: [
        'On OpenShift the operator installs either from OperatorHub, through the OLM bundle the release workflow publishes, or with the Helm chart. The objects the operator reconciles are the same on both paths. What differs is who writes the first three of them.',
        { list: [
          '**OperatorHub** installs the operator alone. The administrator then creates the ControlPlane, the SimplyblockDriver, and an OperatorOps from the operator\'s tabs in the console, in that order.',
          '**Helm** with `deployment.profile: standalone` renders those three into the ConfigMap `simplyblock-bootstrap-objects`, and the operator applies them after it starts. The chart does not render them as release objects, because Helm refuses an object whose CRD it is installing in the same release.',
        ] },
        'TLS is the one value an OpenShift install states differently from everywhere else. The control plane serves over TLS by default and needs an issuer for its certificate. On OpenShift that is the service CA, `provider: openshift` in the chart and `OpenShift` on the ControlPlane, and the service CA issues serving certificates only, so mutual TLS is turned off.',
      ],
      examples: [
        { title: 'Helm values for a standalone install', file: 'examples/openshift-compact/02-helm-values.yaml', notes: [
          { at: 'profile: standalone', text: 'The operator hosts its own control plane. `managed` points at a control plane elsewhere, `empty` installs the operator alone.' },
          { at: 'provider: openshift', text: 'Serving certificates from the OpenShift service CA. The chart refuses this provider off OpenShift.' },
          { at: 'mutual_enabled: false', text: 'Only the cert-manager provider can issue client certificates, so the OpenShift provider requires this off.' },
          { at: 'name: compact', text: 'What the discovery run is called, at most 18 characters. The draft is named `discovered-compact` and the proposed cluster `discovered-compact-cluster`.' },
          { at: 'enableControlPlaneNodes: true', text: 'The compact-cluster line. The run otherwise inspects no node carrying the control-plane role, and on a compact cluster that is every node.' },
        ] },
        { title: 'The ControlPlane', file: 'examples/openshift-compact/03-controlplane.yaml', notes: [
          { at: 'name: simplyblock', text: 'The only name the operator reconciles, in its own namespace. A ControlPlane by any other name is ignored.' },
          { at: 'local:', text: '`source.local` hosts the control plane here. `source.managed` would point at one elsewhere, which a later chapter uses.' },
          { at: 'replicas: 3', text: 'Three coordinators is the smallest count that survives one loss, and the default. On a compact cluster they land one per node.' },
          { at: 'provider: OpenShift', text: 'The CRD spells the two providers as the products do: `OpenShift` and `cert-manager`.' },
        ] },
        { title: 'The SimplyblockDriver', file: 'examples/openshift-compact/04-simplyblockdriver.yaml', notes: [
          { at: 'driverName', text: 'The name volumes and StorageClasses refer to. The default pool\'s class is written with it as its provisioner.' },
          { at: 'enableVolumeSnapshots', text: 'Installs the snapshotter sidecar. The chapter on backups depends on it.' },
        ] },
      ],
    },
    {
      heading: 'Discovery: the run and its draft',
      body: [
        'Nobody writes StorageNode objects. A discovery run, an OperatorOps with the `Discover` action, probes every schedulable worker, reads its interfaces, NVMe devices, huge pages, and cores, and writes a ClusterDeploymentConfig in `Draft`. The run changes nothing else, and the draft is inert until somebody approves it.',
        'On a compact cluster the run needs `spec.discover.enableControlPlaneNodes: true`. The operator leaves control-plane nodes out by default because a storage node on an etcd host is a placement almost nobody intends, and the approval gate is a poor place to catch it. The combined three-node deployment is the case the flag exists for.',
        { code: 'oc get oops compact -n simplyblock -w\noc get cdc -n simplyblock', title: 'Following the run' },
        'The run\'s `status.environment` records the distribution it detected, `OpenShift` here, from the API groups and the node labels. The draft carries it as `spec.environment`, and the expansion spends it: on OpenShift it turns on the kubelet configuration and the CPU topology for every node the document produces.',
      ],
      examples: [
        { title: 'The discovery run', file: 'examples/openshift-compact/05-operatorops-discover.yaml', notes: [
          { at: 'action: Discover', text: 'The only OperatorOps action today. The run is an operation, so it is kept afterward as a record of what it found.' },
          { at: 'enableControlPlaneNodes: true', text: 'Without it the run reports each node as `<worker> is not used: it is control-plane` and writes a draft with no workers.' },
          { at: 'enablePartitionedDevices: true', text: 'Reports disks that carry a stale partition table, which a machine that held data before has on every disk. A mounted or busy device is never reported.' },
        ] },
      ],
    },
    {
      heading: 'Reviewing and approving the draft',
      body: [
        'The draft is the whole deployment as one document: the cluster to create, and under `nodeSets` the workers grouped by configuration, each group with its interfaces and its explicit device list. A reviewer reads `status.message`, which holds what validation found, checks the workers and devices, edits what needs editing, and sets `spec.approved: true`. From that moment the document is immutable, and approval cannot be withdrawn.',
        { steps: [
          'Read `status.message` on the draft. Validation runs on every pass and the message names the first problem, such as a worker with no devices.',
          'Check that all three nodes are listed. A node missing from `workers` is one the run refused, and the run\'s own status says why.',
          'Check the devices. The draft names every PCI address explicitly, so a boot disk that slipped in is visible, and the run\'s `deviceFilter` is how it is kept out next time.',
          'Set `cluster.name` to the name the cluster should carry. The default pool and the StorageClass are named after it.',
          'Set `approved: true`.',
        ] },
        'Approval creates the StorageCluster and one StorageNode per worker. Applying a StorageCluster of the same name alongside the document is refused with `ClusterExists`: a document creates or adds and never reconciles a difference.',
      ],
      examples: [
        { title: 'The reviewed draft', file: 'examples/openshift-compact/06-clusterdeploymentconfig.yaml', notes: [
          { at: 'approved: true', text: 'The review gate. A document is validated on every pass and expanded on none until this is set, and it is immutable afterward.' },
          { at: 'environment: OpenShift', text: 'Detected by the run. It sets the kubelet and CPU-topology flags on every node the document produces, after which nothing reads it again.' },
          { at: 'maxSubsystemCount: 50', text: 'One of the two required sizing values, with `vcpuCount`. Stated once for the deployment because the control plane assumes them uniform across the nodes.' },
          { at: 'parityChunks: 1', text: '`1+1`, the default when the stripe is left out. It needs three storage nodes, which the layout check at approval confirms.' },
          { at: 'nodeProvisioningBudget: 1', text: 'How many workers may be in the node-add process at once. On a compact cluster it makes no difference: every node hosts a coordinator, and those are always added one at a time.' },
          { at: 'master-0', text: 'The three nodes, by their Kubernetes names. On a compact cluster they are the control-plane nodes.' },
          { at: 'mgmtInterface: br-ex', text: 'The interface the control plane reaches the node on. On OpenShift the host network sits behind the `br-ex` bridge.' },
          { at: '0000:c1:00.0', text: 'Every device the group hands to simplyblock, by PCI address. There is no allow list and no size range in a document, so a reviewer sees exactly what is taken.' },
        ] },
      ],
    },
    {
      heading: 'What the operator does after approval',
      body: [
        'The document moves `Draft`, `Expanding`, `Expanded`. Behind it the StorageCluster moves `Pending`, `Creating`, `Provisioning`, `Activating`, `Online`, and each StorageNode `Pending`, `Provisioning`, `Online`. Provisioning a node is where the time goes: the node agent configures the host, which on OpenShift means a machine config and a reboot, and the control plane adds the node once it is back.',
        { table: { head: ['Object', 'Phases, in order', 'Where it waits'], rows: [
          ['ClusterDeploymentConfig', '`Draft`, `Expanding`, `Expanded`', '`Expanding` until the cluster is `Online`'],
          ['StorageCluster', '`Pending`, `Creating`, `Provisioning`, `Activating`, `Online`', '`Provisioning` until every node is added'],
          ['StorageNode', '`Pending`, `Provisioning`, `Online`', '`Provisioning` across the host\'s reboot'],
        ] } },
        'On a compact cluster the three nodes are provisioned strictly one after another, so the cluster is `Online` after three reboots, not one. That is the control plane protecting its own database: a coordinator\'s host is never rebooted while another coordinator\'s host is.',
        'Once the cluster is `Online` its reconciler writes a default StoragePool, named `<cluster>-default`, and a StorageClass for it, named `simplyblock-<namespace>-<cluster>` with a leading repetition of `simplyblock` collapsed. For a cluster `compact` in the namespace `simplyblock` that is the pool `compact-default` and the class `simplyblock-compact`. A claim against that class is the first volume.',
        { code: 'oc get cdc,stc,sn,sp -n simplyblock\noc get storageclass simplyblock-compact', title: 'The objects after expansion' },
      ],
      examples: [
        { title: 'The first claim', file: 'examples/openshift-compact/07-pvc.yaml', notes: [
          { at: 'storageClassName: simplyblock-compact', text: 'The class the cluster wrote for its default pool. Its labels name the pool, and its parameters carry the cluster id and the pool name for the driver.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The draft has no workers, and the run reports every node as `not used: it is control-plane`.', cause: 'The run was not allowed to inspect control-plane nodes, and on a compact cluster there are no others.', fix: 'Set `spec.discover.enableControlPlaneNodes: true` on a new OperatorOps, or `discovery.enableControlPlaneNodes` in the chart. A finished run is not repeated.' },
          { symptom: 'Storage-node pods never start, and the StorageNodes stay in `Provisioning`.', cause: 'The SCC bindings or the namespace\'s Pod Security labels are missing.', fix: 'Apply the two ClusterRoleBindings and label the namespace, then the DaemonSet schedules on its own.' },
          { symptom: '`helm install` refuses to render with a TLS error.', cause: '`tls.provider: openshift` was combined with mutual TLS, or used off OpenShift.', fix: 'Set `tls.mutual_enabled: false` with the OpenShift provider, and use cert-manager anywhere else.' },
          { symptom: 'A StorageCluster applied by hand is refused with `ClusterExists`.', cause: 'An approved document already owns a cluster of that name.', fix: 'Let the document create the cluster. To add nodes later, write a new document with `clusterRef` instead of `cluster`.' },
          { symptom: 'Validation refuses the draft over the stripe.', cause: 'A `1+1` stripe with fewer than three workers, or a wider stripe than three nodes can hold.', fix: 'Three nodes carry `1+1` and nothing wider. `2+1` needs four and `4+1` six.' },
          { symptom: 'The Helm install fails on the discovery name.', cause: '`discovery.name` is longer than 18 characters, so the proposed cluster name would exceed the 37-character limit.', fix: 'Choose a shorter run name. The draft and the cluster are named after it.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Compact cluster', cat: 'concept', text: 'What does simplyblock run on each of the three nodes of a compact cluster, after this chapter\'s deployment?', options: ['A storage node driving the node\'s NVMe devices', 'A FoundationDB coordinator', 'A second API server for the control plane', 'The CSI node plugin', 'A dedicated witness for the other two nodes'], correct: [0, 1, 3], explain: 'Every node hosts a storage node and one of the three coordinators, and the CSI node plugin runs as a DaemonSet everywhere volumes are mounted. The management API is simplyblock\'s own and nothing stands in for the Kubernetes API server, and a witness belongs to two-node clusters.' },
    { topic: 'Compact cluster', cat: 'concept', text: 'Why is three the smallest node count for the default stripe?', options: ['A `1+1` stripe places two chunks and needs one spare node to rebuild onto', 'FoundationDB refuses fewer than three coordinators', 'OpenShift refuses a compact cluster with fewer nodes', 'The control plane needs one node per erasure-coding scheme', 'The CSI driver needs three nodes for its controller replicas'], correct: [0], explain: 'Each scheme has a minimum node count: the chunks of one stripe plus one spare per tolerated failure. For `1+1` that is three. The coordinator default is also three, but it is a default rather than a refusal, and the other options are invented.' },
    { topic: 'Compact cluster', cat: 'concept', text: 'Which of these are consequences of every node also hosting a FoundationDB coordinator?', options: ['The nodes are added to the cluster one at a time', 'The discovery run has to be allowed to use control-plane nodes', 'The cluster cannot use erasure coding', 'The default pool is created later', 'Mutual TLS has to be turned off'], correct: [0, 1], explain: 'A node add reboots the host, and the operator never reboots two coordinator hosts at once. The discovery flag is needed because the nodes carry the control-plane role. Erasure coding, the default pool, and TLS are unaffected.' },
    { topic: 'SCC and namespace', cat: 'concept', text: 'Which security context constraints does the simplyblock namespace need bound to its service accounts?', options: ['`privileged`', '`anyuid`', '`restricted-v2`', '`hostnetwork`', '`nonroot`'], correct: [0, 1], explain: 'The OperatorHub install guide binds `privileged` and `anyuid` to the group of all service accounts in the namespace. The storage-node pods drive host devices, which `restricted-v2` does not allow, and the other two are not needed on their own.' },
    { topic: 'SCC and namespace', cat: 'concept', text: 'Who grants the SCCs and labels the namespace?', options: ['The operator, when the ControlPlane is created', 'The Helm chart, as part of the release', 'An OpenShift administrator, by hand, before the install', 'The discovery run, as part of probing the nodes', 'OLM, when the bundle is subscribed'], correct: [2], explain: 'The operator grants nothing of this itself, and only removes a leftover binding an older version made. The OperatorHub description tells the administrator to apply the bindings and the labels before installing.' },
    { topic: 'SCC and namespace', cat: 'field', text: 'What is the subject of the two SCC ClusterRoleBindings?', options: ['The operator\'s ServiceAccount alone', 'The group `system:serviceaccounts:simplyblock`', 'The `default` ServiceAccount of every namespace', 'The user who installs the operator', 'The group `system:authenticated`'], correct: [1], explain: 'One binding per SCC to the group of all service accounts of the namespace covers the operator, the control plane, the storage nodes, and the CSI plugins at once.' },
    { topic: 'SCC and namespace', cat: 'concept', text: 'A storage-node pod is refused at admission although both SCC bindings exist. What is the likeliest cause?', options: ['The namespace lacks the `privileged` Pod Security labels', 'The pod is missing a toleration', 'The ControlPlane is not yet `Available`', 'The node has no NVMe devices', 'The SCC bindings have to be RoleBindings'], correct: [0], explain: 'Pod Security admission is a second gate beside the SCCs: the namespace has to enforce the `privileged` level, or a privileged pod is refused even with the SCC granted.' },
    { topic: 'Install paths', cat: 'concept', text: 'With the Helm chart\'s `standalone` profile, which objects does the operator apply from the `simplyblock-bootstrap-objects` ConfigMap?', options: ['The ControlPlane', 'The SimplyblockDriver', 'The discovery OperatorOps', 'The StorageCluster', 'The StorageNodes'], correct: [0, 1, 2], explain: 'The chart renders the three bootstrap objects into a ConfigMap and the operator applies them, because Helm refuses an object whose CRD the same release installs. The cluster and its nodes come from the approved draft.' },
    { topic: 'Install paths', cat: 'concept', text: 'After an OperatorHub install, which objects does the administrator still create by hand?', options: ['The operator Deployment and its CRDs', 'A ControlPlane named `simplyblock`', 'A SimplyblockDriver', 'A discovery run', 'The ClusterDeploymentConfig'], correct: [1, 2, 3], explain: 'OLM installs the operator and its CRDs. The administrator creates the ControlPlane, the driver, and the discovery run from the operator\'s tabs, and the run writes the ClusterDeploymentConfig.' },
    { topic: 'Install paths', cat: 'field', text: 'Which chart values does an OpenShift install state differently from a vanilla Kubernetes one?', options: ['`tls.provider: openshift`', '`tls.mutual_enabled: false`', '`deployment.profile: openshift`', '`discovery.enabled: false`', '`controlplane.local.eksRoleARN`'], correct: [0, 1], explain: 'The service CA issues serving certificates only, so the OpenShift provider goes with mutual TLS off. The profiles are `standalone`, `managed`, and `empty`, discovery stays on, and the role ARN is for EKS.' },
    { topic: 'Install paths', cat: 'field', text: 'Which ControlPlane does the operator reconcile?', options: ['Any ControlPlane in any namespace', 'Only one named `simplyblock` in the operator\'s namespace', 'The first one created', 'One per StorageCluster', 'Only one created by the Helm chart'], correct: [1], explain: 'The ControlPlane is a singleton by name and namespace. One by another name is ignored.' },
    { topic: 'Install paths', cat: 'field', text: 'Which of these does `spec.source.local` on the ControlPlane install in this cluster?', options: ['FoundationDB', 'The management API', 'The object store', 'The storage nodes', 'The CSI driver'], correct: [0, 1, 2], explain: '`source.local` installs FoundationDB, the object store, and the management API here, in that order. The storage nodes come from the approved draft and the CSI driver from the SimplyblockDriver.' },
    { topic: 'Install paths', cat: 'field', text: 'A SimplyblockDriver was created right after the ControlPlane and reports `Unavailable`. Which of these are reasons to act?', options: ['The driver is broken and has to be re-created', 'The ControlPlane has to be deleted and written again', 'The SCC bindings are missing', 'The storage cluster is still being provisioned', 'The driver name is wrong'], correct: [], explain: 'None of them. The driver reports `Unavailable` until a storage cluster exists, which on a fresh install is expected and needs nothing done.' },
    { topic: 'Discovery', cat: 'action', text: 'Which of these does an OperatorOps with action `Discover` do?', options: ['Probes the schedulable workers', 'Writes a ClusterDeploymentConfig in `Draft`', 'Creates one StorageNode per worker', 'Formats the NVMe devices it finds', 'Reboots the workers to apply machine configs'], correct: [0, 1], explain: 'The run inspects and writes a draft, and changes nothing else. The cluster and the nodes are created when the draft is approved, and the formatting and reboots happen during node provisioning.' },
    { topic: 'Discovery', cat: 'field', text: 'What does `spec.discover.enableControlPlaneNodes` change?', options: ['Whether nodes carrying the control-plane role are inspected and proposed', 'Whether the simplyblock control plane runs on the nodes', 'Whether infrastructure nodes are inspected', 'Whether the draft is approved automatically', 'Whether etcd is moved off the nodes'], correct: [0], explain: 'It lets the run consider machines that run the API server and etcd. Infrastructure nodes are used without asking, and nothing approves a draft but a person.' },
    { topic: 'Discovery', cat: 'field', text: 'Which fields of `spec.discover` narrow which workers a run inspects?', options: ['`workers`', '`nodeSelector`', '`deviceFilter`', '`enablePartitionedDevices`', '`configName`'], correct: [0, 1], explain: '`workers` names nodes and `nodeSelector` selects them by label. `deviceFilter` narrows devices, `enablePartitionedDevices` widens what is reported, and `configName` names the draft.' },
    { topic: 'Discovery', cat: 'concept', text: 'A fresh Helm install with `discovery.enabled: true` has finished its run. The administrator changes `discovery.nodeSelector` and runs `helm upgrade`. What happens?', options: ['A second run inspects the newly selected workers', 'Nothing, a finished run is not repeated on upgrade', 'The existing draft is rewritten', 'The cluster is re-created', 'The upgrade is refused'], correct: [1], explain: 'The chart raises the run once, on a fresh install. Another run needs a new OperatorOps.' },
    { topic: 'Discovery', cat: 'field', text: 'Which devices does a run with `enablePartitionedDevices: true` still leave out?', options: ['Devices carrying a stale partition table', 'Mounted devices', 'Devices holding swap', 'Devices the kernel holds', 'Devices outside the run\'s `driveSizeRange`'], correct: [1, 2, 3, 4], explain: 'The partition table is the only availability condition that can be waived, because a machine that held data before has one on every disk. A mounted, busy, or swap device is never reported, and the size range is a filter the run applies.' },
    { topic: 'Deployment config', cat: 'field', text: 'Which of these does `spec.environment: OpenShift` do on a ClusterDeploymentConfig?', options: ['Turns on the kubelet configuration for the nodes the document produces', 'Turns on the CPU topology for them', 'Grants the SCCs to the namespace', 'Selects the OLM install path', 'Makes the operator detect OpenShift'], correct: [0, 1], explain: 'The environment is a shorthand the expansion spends: it sets the per-distribution flags on the cluster and nothing reads it again. The run detected it, and a reviewer can correct it in the draft.' },
    { topic: 'Deployment config', cat: 'field', text: 'Which statements about `spec.approved` are true?', options: ['The document is expanded only once it is true', 'It cannot be set back to false', 'Everything else in the document is immutable once it is true', 'It is set by the discovery run when validation passes', 'It must be set within an hour of the draft being written'], correct: [0, 1, 2], explain: 'Approval is the review gate, immutable once given, and it freezes the document as the record of what was deployed. Only a person sets it, and a draft can wait as long as a review takes.' },
    { topic: 'Deployment config', cat: 'field', text: 'Which sizing values are required on the cluster of a deployment config?', options: ['`maxSubsystemCount`', '`vcpuCount`', '`minHugePagesSize`', '`stripe`', '`nodeProvisioningBudget`'], correct: [0, 1], explain: 'The two counts are required and stated once for the deployment. The huge pages floor, the stripe, and the budget have defaults.' },
    { topic: 'Deployment config', cat: 'concept', text: 'Which of these can a node group\'s `devices` in a deployment config hold?', options: ['Every device the group\'s workers hand to simplyblock, by explicit address', 'A size range the run filters by', 'A model name to match', 'A count of devices per worker', 'A deny list of PCI addresses'], correct: [0], explain: 'A document whose meaning depends on what the hardware turns out to be is not one a reviewer can approve, so every filter, the size range and the deny list included, belongs to the run, and the draft names every device.' },
    { topic: 'Deployment config', cat: 'concept', text: 'A StorageCluster is applied by hand with the same name as the cluster of an approved document. Which of these happen?', options: ['The apply is refused with `ClusterExists`', 'The document adopts it', 'The operator merges the two specs', 'The document is marked `Failed`', 'The hand-written cluster replaces the document\'s'], correct: [0], explain: 'A document creates or adds and never reconciles a difference, so a second cluster of the same name is refused rather than reconciled.' },
    { topic: 'Deployment config', cat: 'field', text: 'How does a later document add nodes to the cluster this chapter created?', options: ['With `spec.clusterRef` naming the cluster, and no `spec.cluster`', 'By editing the approved document', 'With `spec.cluster` of the same name', 'By creating StorageNodes by hand', 'By re-running discovery with `approved: true`'], correct: [0], explain: '`clusterRef` names an existing cluster the document adds to. The approved document is immutable, and StorageNodes are never hand-written.' },
    { topic: 'Expansion', cat: 'concept', text: 'Which of these are phases of a ClusterDeploymentConfig?', options: ['`Draft`', '`Expanding`', '`Expanded`', '`Approved`', '`Online`'], correct: [0, 1, 2], explain: 'The document is a `Draft` until approved, `Expanding` while its objects are being made, `Expanded` once they exist, and `Failed` when the expansion refused or could not finish. Approval is a spec field, and `Online` is the cluster\'s phase.' },
    { topic: 'Expansion', cat: 'concept', text: 'Why does a compact cluster reach `Online` only after three reboots in a row?', options: ['Every node hosts a coordinator, and coordinator hosts are provisioned one at a time', 'OpenShift reboots the whole cluster per machine config', 'The default `nodeProvisioningBudget` is 3', 'Each node has to be drained of etcd first', 'The stripe is written after each node'], correct: [0], explain: 'A node add reboots the host, and workers hosting a FoundationDB pod are always sequential whatever the budget says, so the database never has two coordinators down at once.' },
    { topic: 'Pools and classes', cat: 'concept', text: 'After the cluster `compact` in the namespace `simplyblock` is `Online`, which objects does its reconciler write?', options: ['A StoragePool named `compact-default`', 'A StorageClass named `simplyblock-compact`', 'A StorageClass named `simplyblock-simplyblock-compact`', 'One StoragePool per node', 'A PersistentVolumeClaim to test the cluster'], correct: [0, 1], explain: 'The default pool is `<cluster>-default`, and its class is `simplyblock-<namespace>-<cluster>` with the leading repetition of `simplyblock` collapsed. Nothing else is written for the user.' },
    { topic: 'Pools and classes', cat: 'field', text: 'How is a StorageClass tied to the pool it draws from?', options: ['By the label `storage.simplyblock.io/pool`', 'By `parameters.pool_name` for the driver', 'By an owner reference to the pool', 'By being created from the pool\'s spec', 'By its name alone'], correct: [0, 1], explain: 'The labels are the assignment, and `parameters` carries what the driver needs to reach the backend. The operator validates that the two agree, and no owner reference is involved.' },
    { topic: 'Pools and classes', cat: 'concept', text: 'Which of these happen when the default pool is deleted?', options: ['It deletes like any other pool, unless a volume still draws from it', 'The cluster writes it again on its next pass', 'The cluster is marked `Degraded`', 'Every volume in it is deleted', 'The StorageCluster is deleted with it'], correct: [0], explain: 'The default pool is an ordinary pool in every respect, deletion included. An annotation on the cluster records that it was written, so it is not written a second time.' },
  ],

  tasks: [
    {
      title: 'A discovery run for a compact cluster',
      scenario: 'Write the run that inspects all three nodes of a compact OpenShift cluster and reports disks that still carry an old partition table.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: compact\n  namespace: simplyblock\nspec:\n  action: {{action}}\n  discover:\n    enableControlPlaneNodes: {{cp}}\n    enablePartitionedDevices: {{partitioned}}\n',
      blanks: {
        kind: { accept: ['OperatorOps'], hint: 'the operator-level operation kind, short name `oops`' },
        action: { accept: ['Discover'], hint: 'its only action today' },
        cp: { accept: ['true'], hint: 'on a compact cluster every node carries the control-plane role' },
        partitioned: { accept: ['true'], hint: 'the one availability condition that can be waived' },
      },
      explain: 'An `OperatorOps` with `action: Discover` writes the draft. `enableControlPlaneNodes` is the compact-cluster line, and `enablePartitionedDevices` reports the disks a reused machine carries.',
    },
    {
      title: 'The ControlPlane on OpenShift',
      scenario: 'Write the control plane the operator hosts on an OpenShift cluster, with serving certificates from the service CA and three coordinators.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: ControlPlane\nmetadata:\n  name: {{name}}\n  namespace: simplyblock\nspec:\n  source:\n    {{source}}:\n      image: quay.io/simplyblock-io/simplyblock:26.4.0-beta4\n      foundationDB:\n        replicas: {{replicas}}\n      tls:\n        enableMutualTLS: {{mutual}}\n        provider: {{provider}}\n',
      blanks: {
        name: { accept: ['simplyblock'], hint: 'the only name the operator reconciles' },
        source: { accept: ['local'], hint: 'hosted here, not pointed at' },
        replicas: { accept: ['3'], hint: 'the smallest count that survives one loss' },
        mutual: { accept: ['false'], hint: 'the service CA issues serving certificates only' },
        provider: { accept: ['OpenShift'], hint: 'spelled as the product spells it' },
      },
      explain: 'Only `simplyblock` is reconciled. `source.local` hosts the control plane, three coordinators survive one loss, and the `OpenShift` provider cannot issue client certificates, so mutual TLS is off.',
    },
    {
      title: 'The reviewed draft',
      scenario: 'Complete the draft so that it is expanded into a cluster named `compact` with the default stripe, on OpenShift, and so that the operator starts.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: ClusterDeploymentConfig\nmetadata:\n  name: discovered-compact\n  namespace: simplyblock\nspec:\n  approved: {{approved}}\n  environment: {{environment}}\n  cluster:\n    name: {{name}}\n    maxSubsystemCount: 50\n    vcpuCount: 8\n    stripe:\n      dataChunks: {{data}}\n      parityChunks: {{parity}}\n  nodeSets:\n    - name: compact\n      groups:\n        - name: default\n          workers: [master-0, master-1, master-2]\n          mgmtInterface: br-ex\n          dataInterfaces: [br-ex]\n          devices:\n            nvme: ["0000:c1:00.0"]\n',
      blanks: {
        approved: { accept: ['true'], hint: 'the review gate' },
        environment: { accept: ['OpenShift'], hint: 'one of the enum\'s spellings' },
        name: { accept: ['compact'], hint: 'the default pool and the class are named after it' },
        data: { accept: ['1'], hint: 'the default scheme, which three nodes can hold' },
        parity: { accept: ['1'], hint: 'one chunk loss survived' },
      },
      explain: 'The document is expanded once `approved` is true. `OpenShift` is the enum\'s spelling, the cluster name decides `compact-default` and `simplyblock-compact`, and `1+1` is the scheme three nodes carry.',
    },
  ],
});
