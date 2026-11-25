// Chapter 19: DHCHAP, the in-band authentication of NVMe-oF hosts, as the
// operator implements it. It sits in the security part because it decides which
// hosts may connect to a pool's volumes. What a user writes is one flag on a
// StoragePool at creation and the list of nodes allowed to connect, and the
// chapter follows that list into node labels, the control plane's allowed hosts,
// the StorageClass parameter that turns it into node affinity, and the connect
// on the node, where the keys exist only in memory.
TRAINING.register({
  id: 'dhchap',
  title: 'Using DHCHAP to authenticate NVMe-oF hosts',
  summary: 'A StoragePool created with DHCHAP admits connections only from the Kubernetes nodes it lists. The chapter covers the pool, the class written for it, the keys the control plane holds, and changing the list on a running pool.',
  goal: 'the learner can create a DHCHAP pool and its StorageClass, say what the operator does with the allowed nodes and where the keys live, tell the DHCHAP host NQN apart from the kernel default, and diagnose a host the control plane refuses.',

  sections: [
    {
      heading: 'What DHCHAP protects, and where the framing changed',
      body: [
        'Any host that can reach a storage node\'s NVMe-oF listener can ask to connect to a subsystem. DHCHAP, the DH-HMAC-CHAP authentication of the NVMe specification, puts a challenge in front of that connection: the host has to prove it holds the secret the target expects for its host NQN. When the control plane also issues a controller secret, the target proves its identity back to the host, and the authentication is bidirectional.',
        'In simplyblock, DHCHAP belongs to a StoragePool. A pool created with it has a list of allowed hosts in the control plane, and the control plane hands out a connection, with its secrets, only to a host on that list. The CSI node plugin presents those secrets when it connects. A user writes two things: the flag on the pool, and the Kubernetes nodes that may connect.',
        { callout: 'The commissioned title was "Using DHCHAP Security", which reads like a security setting for the whole cluster. The code has no such setting: no field of StorageCluster, SimplyblockDriver, or ClusterDeploymentConfig mentions DHCHAP. It is a decision made per pool, at the pool\'s creation, with a list of nodes that stays editable, and no key ever passes through a Kubernetes object. The chapter teaches that shape.', kind: 'info' },
        { table: { head: ['Question', 'Answer in the code'], rows: [
          ['Which hosts may connect to a pool\'s volumes', 'Only the nodes in `spec.allowedNodes`, by their UID-derived host NQN'],
          ['Does the target prove its identity to the host', 'When the control plane issues a controller secret, yes'],
          ['Is the traffic encrypted', 'Not by DHCHAP. NVMe/TCP TLS needs a pre-shared key that nothing in the repository provisions'],
          ['Where are the keys', 'In the control plane, and in the node plugin\'s memory during a connect'],
        ] } },
      ],
    },
    {
      heading: 'A pool decision, made at creation',
      body: [
        'A DHCHAP pool is a StoragePool with `spec.volumeDefaults.enableDHCHAP: true` and the hosts in `spec.allowedNodes`, by Kubernetes Node name. Two rules of the CRD give it its shape.',
        { list: [
          '**`volumeDefaults` is immutable once set.** Every field in it is written into the pool\'s StorageClass, and Kubernetes does not let anyone edit a class\'s parameters, so the whole block is fixed. DHCHAP cannot be turned on or off on an existing pool. A pool that needs it is a new pool.',
          '**`allowedNodes` requires DHCHAP.** A pool listing nodes without `enableDHCHAP: true` is refused with `allowedNodes requires volumeDefaults.enableDHCHAP to be true`. The list itself stays mutable, and it is a set, so a name appears once.',
        ] },
        'The reverse is admitted: a DHCHAP pool with an empty list. The type\'s documentation says authentication is enforced only once the list is non-empty, and such a pool\'s class gets no node selector. What the control plane does for a DHCHAP pool with no allowed hosts is not visible in the repository.',
        'Under `v1alpha1` the rule ran the other way: `spec.dhchap` required a non-empty `allowedNodes`. The conversion maps `spec.dhchap` to `spec.volumeDefaults.enableDHCHAP`, so a pool written at the older version arrives with the flag in its new place.',
        { callout: 'The cluster\'s default pool cannot become a DHCHAP pool. The cluster writes it with an empty `volumeDefaults` block, the API server fills in `filesystem: xfs`, and from then on the block is fixed with DHCHAP off. A DHCHAP pool is always an authored pool.', kind: 'warn' },
        'There is no sample to start from: `operator/config/samples` has no DHCHAP pool, and the Helm chart\'s `operator_customresources.yaml` shows the two fields only as commented lines. The manifest below is the whole of what a DHCHAP pool needs.',
      ],
      examples: [
        { title: 'A DHCHAP pool with two allowed nodes', file: 'examples/dhchap/01-storagepool.yaml', notes: [
          { at: 'clusterRef: production', text: 'The cluster the pool is carved out of, immutable like the flag below. The admission webhook refuses a name with no StorageCluster behind it.' },
          { at: 'allowedNodes:', text: 'The hosts that may connect, by `kubectl get nodes` name. The list is left as written: a name that resolves to no node is skipped, not pruned.' },
          { at: '- worker-1', text: 'Each entry becomes a node label and an allowed host in the control plane, keyed by the node\'s UID rather than this name.' },
          { at: 'capacity: 20T', text: 'Limits are mutable and unrelated to DHCHAP. They are here because a real pool has them.' },
          { at: 'enableDHCHAP: true', text: 'The decision. Sent to the control plane once, in the call that creates the pool, and fixed from then on.' },
          { at: 'filesystem: xfs', text: 'Also a volume default, and the CRD\'s default when omitted. The class written below repeats it so it agrees with the pool.' },
        ] },
      ],
    },
    {
      heading: 'What the operator does with the list',
      body: [
        'The pool controller acts on the list on every reconcile once the pool exists in the control plane. In order:',
        { steps: [
          '**Create.** The pool is created in the control plane with `dhchap: true`, and the UUID that comes back is recorded in `status.uuid`. The event is `PoolCreated`.',
          '**Resolve.** Each name in `allowedNodes` is looked up as a Node. A name that resolves to nothing is skipped and reported once, as a Warning event `AllowedNodeMissing`, and it stays in the spec, so a node that returns under the same name rejoins the pool.',
          '**Label.** Every resolved node gets the label `storage.simplyblock.io/storage-pool.<pool UUID>=allowed`, and the label comes off every node that carries it and is no longer resolved.',
          '**Register the hosts.** For each resolved node the operator computes the host NQN `nqn.2014-08.io.simplyblock:uuid:<Node UID>` and adds it to the pool\'s hosts in the control plane, with a `POST` to `/api/v2/clusters/<cluster>/storage-pools/<pool>/host`. A host the control plane has and the list no longer wants is removed with a `DELETE` to the same path.',
          '**Report.** `status.allowedNodes` lists the names that resolved. A list that names nodes of which none resolves leaves the pool `Pending`, with the message `every node in spec.allowedNodes resolves to nothing, so the pool can place no volume`.',
        ] },
        'The NQN comes from the node\'s UID, not its name. A node deleted and registered again under the same name is a different host to Kubernetes, with a new UID, and the authorization of the old one does not carry over to it.',
        'The pool controller watches pools, the classes assigned to them, and member clusters, and it does not watch Nodes. A node that joins after a pool named it, or one rebuilt with a new UID, is picked up at the pool\'s next reconcile, which any update to the pool or to one of its classes triggers.',
        'The operator emits no DHCHAP event or condition of its own. What it did is read from the node labels, from `status.allowedNodes`, and from its log, which records `added the pool label to a node` and `added a host to the pool`. On a pool whose `clusterRef` names a supercluster, the label carries the pool\'s object UID instead of a backend UUID, because such a pool has a backend pool in every member and no UUID of its own.',
        { code: 'kubectl get sp payments -n simplyblock -o wide\nkubectl get sp payments -n simplyblock -o jsonpath=\'{.status.uuid}{"\\n"}{.status.allowedNodes}{"\\n"}\'\nkubectl get nodes -L storage.simplyblock.io/storage-pool.3f9a1c2e-7b4d-4e8a-9c61-2d5f0a7b8e14', title: 'Reading what the operator did' },
      ],
    },
    {
      heading: 'The class, and how scheduling follows',
      body: [
        'The control plane refuses a host that is not on the list, and Kubernetes scheduling keeps pods from being sent to one in the first place. The link between the two is the class parameter `dhchap_node_selector`, whose value is the exact label key the operator writes on the allowed nodes. At CreateVolume the CSI controller adds that key, with the value `allowed`, to the volume\'s accessible topology, and the PersistentVolume is created with a `nodeAffinity` that requires it.',
        'The operator writes the parameter into one class only: the class it writes for a pool named `<cluster>-default`, and only when that pool has DHCHAP on, a non-empty `allowedNodes`, and a UUID at the moment the class is written. The default pool the cluster writes for itself never has DHCHAP, as the previous section said, so in practice the class of a DHCHAP pool is written by hand. It carries the three assignment labels, `cluster_id` and `pool_name`, and `dhchap_node_selector`.',
        { steps: [
          'Apply the pool and wait for `Ready`.',
          'Read `status.uuid` from the pool and `status.uuid` from its cluster.',
          'Write the class with `dhchap_node_selector: storage.simplyblock.io/storage-pool.<pool UUID>` and `cluster_id: <cluster UUID>`.',
          'Write the claims against the class.',
        ] },
        'A class without the parameter is admitted. The StorageClass webhook refuses an assigned class only when it reaches another cluster or pool, or stripes otherwise than the pool. The pool reports the missing parameter as a Normal event `StorageClassOverridesPoolDefault`, whose message names `dhchap_node_selector`. Volumes from such a class carry no DHCHAP node affinity, and a pod sent to a host outside the list fails to mount.',
        'Leave `allowedTopologies` off the class. The external provisioner matches it against the topology keys a node registered in its CSINode object when the node plugin started, so a pool label written afterward fails every claim with `is not in requisite`. The parameter avoids that, because the CSI controller builds the topology from the class and not from the node\'s registration.',
        'The class uses `WaitForFirstConsumer`, and before the volume exists the scheduler has nothing that tells it about the restriction. The end-to-end test pins its first pod to an allowed node, and the example below does the same with a `nodeSelector` on the pool\'s label. Once the PersistentVolume exists, its `nodeAffinity` keeps every later pod on an allowed host, which the same test checks by recreating the pod with no pin. Where the scheduler places an unpinned first pod is Kubernetes behavior the repository does not test.',
      ],
      examples: [
        { title: 'The class for the DHCHAP pool', file: 'examples/dhchap/02-storageclass.yaml', notes: [
          { at: 'storage.simplyblock.io/pool: payments', text: 'With the namespace and cluster labels above it, the assignment. The pool lists the class in `status.storageClassNames` and holds its own deletion while the class exists.' },
          { at: 'provisioner: csi.simplyblock.io', text: 'The SimplyblockDriver\'s `driverName`.' },
          { at: 'cluster_id:', text: 'The cluster\'s `status.uuid`. A class naming another cluster is refused by the webhook.' },
          { at: 'pool_name: payments', text: 'The pool by name. The webhook refuses a class whose labels and this parameter name different pools.' },
          { at: 'csi.storage.k8s.io/fstype: xfs', text: 'Agrees with the pool\'s `filesystem`. Leaving it out is admitted and reported as an override.' },
          { at: 'dhchap_node_selector:', text: 'The exact label key the operator writes on allowed nodes, with the pool\'s `status.uuid` in it. The CSI controller turns it into the PV\'s node affinity.' },
          { at: 'WaitForFirstConsumer', text: 'Binding waits for a pod, so the first pod\'s placement matters. The next two examples cover that.' },
        ] },
        { title: 'A claim against it', file: 'examples/dhchap/03-pvc.yaml', notes: [
          { at: 'storageClassName: simplyblock-production-payments', text: 'The only line that ties the claim to DHCHAP. A claim has no DHCHAP field.' },
        ] },
        { title: 'The first pod, pinned to an allowed host', file: 'examples/dhchap/04-pod.yaml', notes: [
          { at: 'storage.simplyblock.io/storage-pool.', text: 'The same key as the class parameter, with the value the operator writes. Any node in the list qualifies.' },
          { at: 'claimName: ledger', text: 'Mounting stages the volume on the node, which is where the connect and its authentication happen.' },
        ] },
      ],
    },
    {
      heading: 'Where the keys live, and which host NQN is used',
      body: [
        'The control plane generates and stores the DHCHAP keys. Whether one key serves the whole pool or each host has its own is its decision, and the secret is resolved for one host and one subsystem at a time. No Kubernetes Secret holds a key, and the operator never sees one: its only DHCHAP calls create the pool with the flag and add or remove hosts.',
        'At stage time the node plugin asks the control plane for the volume\'s connection and names its host NQN. The answer carries a prebuilt `nvme connect` line, and the plugin reads `--dhchap-secret` from it and, when present, `--dhchap-ctrl-secret`, and passes what it found to nvme-cli on its command line. The secrets are re-read at every stage, held in memory, redacted from the plugin\'s log, and never written to the stashed volume context or to the stack record on the node.',
        'Bidirectional authentication is the control plane\'s choice as well. When the answer carries a controller secret, both flags are passed and the target authenticates back to the host. When it does not, only the host authenticates. No field of the pool or the class selects one or the other.',
        { list: [
          '**Rotation** of DHCHAP keys is not implemented in the operator or the CSI driver, and a user cannot supply keys of their own.',
          '**TLS** is passed through as `--tls` when the control plane\'s connect line asks for it, but nothing provisions the pre-shared key the kernel needs in its keyring, so treat TLS as not supported by the operator.',
          '**A control plane that does not answer** leaves the plugin with its stashed context, which carries no secrets, so a DHCHAP volume cannot be attached until the control plane answers. No test in the repository covers that case.',
        ] },
        'A node has two host NQNs, and only one of them is the DHCHAP identity.',
        { table: { head: ['', 'DHCHAP identity', 'Kernel default'], rows: [
          ['Form', '`nqn.2014-08.io.simplyblock:uuid:<Node UID>`', '`nqn.2014-08.org.nvmexpress:uuid:<hostid>`'],
          ['Where it comes from', 'Derived from the Node UID by the operator and the node plugin, from the same atlas-lib function', 'Written to `/etc/nvme/hostnqn` by the node plugin\'s postStart hook, from a hostid persisted on the node'],
          ['Published as', 'Nothing. Both sides compute it', 'The Node annotation `storage.simplyblock.io/nvme-host-nqn`'],
          ['Used for', 'The pool\'s allowed hosts, and every connect to a DHCHAP pool\'s volume', 'Plain volumes, and the operator\'s two-node fencing and consistency-group moves'],
        ] } },
        'A DHCHAP connect also passes `--hostid`, derived from the UUID in the DHCHAP NQN. Without it nvme-cli would use the node\'s `/etc/nvme/hostid`, which the kernel already associates with the default NQN once any plain volume is connected, and the kernel refuses one hostid with two different host NQNs. Deriving the hostid keeps a node with plain and DHCHAP volumes able to connect both.',
      ],
    },
    {
      heading: 'Changing allowedNodes on a running pool',
      body: [
        'The list is the part of a DHCHAP pool that changes. Apply the pool with the list edited and `volumeDefaults` exactly as before.',
        { list: [
          '**Adding a node** labels it and registers its host NQN at the next reconcile. The class needs no change, because its selector names the pool\'s label and not a node, and the pool\'s existing volumes can be consumed on the new node too, since their PersistentVolumes require the same label.',
          '**Removing a node** takes the label off it and removes its host NQN from the control plane. New pods using the pool\'s volumes can no longer be scheduled there, and a new stage from that node is refused by the control plane.',
        ] },
        'Removing a node does not move a volume, does not evict a running pod, and does not make the operator disconnect an existing attachment. Whether the control plane tears down a connection that is already open when its host leaves the list is not visible in the repository, so drain the pool\'s workloads from a node before removing it.',
        { callout: 'Moving the pool\'s volumes off removed nodes is declared and not implemented. `StoragePoolOps` has a `Rebalance` action for it, and the operator ends every such operation `Failed` with a message saying the action is declared but not implemented.', kind: 'warn' },
        'Emptying the list removes the label from every node and every host from the control plane. A class carrying `dhchap_node_selector` then names a label no node has, so no pod using the pool\'s volumes can be scheduled anywhere.',
      ],
      examples: [
        { title: 'The same pool with a third node', file: 'examples/dhchap/05-storagepool-node-added.yaml', notes: [
          { at: '- worker-3', text: 'The one change. The next reconcile labels worker-3 and adds its host NQN to the pool\'s allowed hosts.' },
          { at: 'enableDHCHAP: true', text: 'Repeated unchanged. Any change under `volumeDefaults` is refused with `field is immutable`.' },
        ] },
      ],
    },
    {
      heading: 'What the nodes need',
      body: [
        'The repository states no kernel or nvme-cli minimum for DHCHAP. What it relies on, read from the commands the node plugin runs, is:',
        { list: [
          'nvme-cli whose `nvme connect` accepts `--dhchap-secret`, `--dhchap-ctrl-secret`, and `--hostid`.',
          'A kernel whose NVMe-oF host supports in-band DHCHAP authentication.',
          'The `nvme-tcp` module, which the node plugin\'s postStart hook loads with `modprobe`, together with `nvme-rdma`.',
          'The CSI node plugin running on every node in the list, since it is what stages the volume and connects.',
        ] },
        'Check the distribution\'s kernel and nvme-cli against those flags before relying on DHCHAP, because nothing in the operator checks them. The repository\'s end-to-end test (`csi-driver/e2e/dhchap.go`) is the evidence that the path works: an allowed host mounts and writes, an unregistered host NQN is refused by the control plane with `not found in allowed hosts`, a dropped path reconnects, and the PersistentVolume carries the node affinity.',
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The pool is refused with `allowedNodes requires volumeDefaults.enableDHCHAP to be true`.', cause: 'The pool lists nodes without `enableDHCHAP: true`.', fix: 'Add `enableDHCHAP: true` to a new pool. An existing pool with a `volumeDefaults` block cannot have it added.' },
          { symptom: 'Turning DHCHAP on or off is refused with `field is immutable`.', cause: '`volumeDefaults` is fixed once set, because its fields are in the class.', fix: 'Create a new pool with the setting it needs, and a class for it.' },
          { symptom: 'A pod stays in `ContainerCreating` with a `FailedMount` event whose message contains `the control plane refused to publish the volume` and `not found in allowed hosts`.', cause: 'The pod runs on a node whose host NQN is not in the pool\'s allowed hosts. Usually the class lacks `dhchap_node_selector`, so the volume has no node affinity, or the node is not in `allowedNodes`.', fix: 'Add the node to `allowedNodes`, or move the pod to a node that carries the pool\'s label. For a class without the parameter, write a new class with it: class parameters cannot be edited, and volumes already provisioned keep the affinity they were given.' },
          { symptom: 'The pool is `Pending` with `every node in spec.allowedNodes resolves to nothing`, and `AllowedNodeMissing` events name the entries.', cause: 'The names do not match any Node object, for example a fully qualified name where the node is registered under a short one.', fix: 'Use the names `kubectl get nodes` prints.' },
          { symptom: 'After a node was rebuilt under its old name, pods using the pool\'s volumes no longer schedule there, or its mounts are refused.', cause: 'The new Node object has a new UID, so a new host NQN, and none of the pool\'s label. The pool controller does not watch Nodes.', fix: 'Trigger a reconcile of the pool with any update, such as an annotation. It labels the node and registers its new NQN.' },
          { symptom: 'The pool reports `StorageClassOverridesPoolDefault` with `dhchap_node_selector` in the message.', cause: 'The class was written without the parameter, or with a value that is not the pool\'s label key.', fix: 'Write a new class with `dhchap_node_selector: storage.simplyblock.io/storage-pool.<pool UUID>`, and delete the old one once no claim needs it.' },
          { symptom: 'Every claim from the class fails with `is not in requisite`.', cause: 'The class has `allowedTopologies` naming the pool\'s label, which the node plugin did not register when it started.', fix: 'Write the class without `allowedTopologies`. `dhchap_node_selector` is the whole of the restriction.' },
          { symptom: 'A pool created without a `volumeDefaults` block was given `enableDHCHAP: true` later, and nothing authenticates.', cause: 'The CRD admits adding the block to a pool that had none, but the operator sends DHCHAP to the control plane only in the call that creates the pool.', fix: 'Create a new pool with DHCHAP from the start.' },
          { symptom: 'DHCHAP volumes fail to stage while the control plane is down.', cause: 'The secrets come only from the control plane, and the stashed context the plugin falls back to has none.', fix: 'Restore the control plane. The stage is retried and succeeds once it answers.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'What DHCHAP protects', cat: 'concept', text: 'What does DHCHAP add to a connection to a DHCHAP pool\'s volume?', options: ['The host proves it holds the secret expected for its host NQN before the connection is admitted', 'When the control plane issues a controller secret, the target proves its identity back to the host', 'Encryption of the data on the wire', 'A check that the pod\'s service account may use the volume', 'A Kubernetes Secret per volume holding its key'], correct: [0, 1], explain: 'DHCHAP authenticates the host, and the target too when a controller secret is issued. It does not encrypt, it knows nothing of service accounts, and no Secret holds a key.' },
    { topic: 'What DHCHAP protects', cat: 'field', text: 'Where is DHCHAP turned on?', options: ['`StorageCluster.spec`', '`SimplyblockDriver.spec`', '`StoragePool.spec.volumeDefaults.enableDHCHAP`', '`ClusterDeploymentConfig.spec.cluster`', 'A StorageClass parameter `dhchap: true`'], correct: [2], explain: 'The only DHCHAP field of the API is on the pool. The cluster, the driver, and the deployment document have none, and the class parameter is `dhchap_node_selector`, which restricts placement.' },
    { topic: 'What DHCHAP protects', cat: 'concept', text: 'Which of these do the operator or the CSI driver provide today?', options: ['Rotation of DHCHAP keys', 'Importing DHCHAP keys a user supplies', 'Provisioning the pre-shared key NVMe/TCP TLS needs', 'A field selecting one-way or two-way authentication', 'A condition on the StoragePool reporting DHCHAP failures'], correct: [], explain: 'None of them exists. Keys are generated by the control plane and not rotated by either component, nothing provisions TLS keys, the direction is the control plane\'s choice, and the operator emits no DHCHAP condition.' },
    { topic: 'What DHCHAP protects', cat: 'concept', text: 'The commissioned title framed DHCHAP as a security setting. Which statements describe the code?', options: ['It is decided per pool', 'It is decided when the pool is created and cannot be changed on that pool', 'No key passes through a Kubernetes object', 'Once one pool has it, every pool of the cluster gets it', 'The cluster\'s default pool can be switched to it'], correct: [0, 1, 2], explain: 'The flag is a pool\'s, inside the immutable `volumeDefaults`, and the keys stay in the control plane and the node plugin\'s memory. Pools are independent, and the default pool is written with DHCHAP off and a fixed block.' },
    { topic: 'What DHCHAP protects', cat: 'concept', text: 'Who decides whether the authentication is one-way or bidirectional?', options: ['The control plane, by issuing a controller secret or not', 'The pool\'s `volumeDefaults`', 'A StorageClass parameter', 'The CSI node plugin, from the kernel version', 'The SimplyblockDriver'], correct: [0], explain: 'The plugin passes `--dhchap-ctrl-secret` when the control plane\'s connect line carries one and omits it otherwise. No Kubernetes field selects it.' },

    { topic: 'The pool and its list', cat: 'field', text: 'A StoragePool has `allowedNodes: [worker-1]` and no `volumeDefaults`. What happens when it is applied?', options: ['It is refused with `allowedNodes requires volumeDefaults.enableDHCHAP to be true`', 'It is admitted and the operator turns DHCHAP on', 'It is admitted and the nodes are labeled without authentication', 'It is admitted and stays `Pending`', 'The webhook refuses it because the cluster has no DHCHAP setting'], correct: [0], explain: 'A CEL rule on the spec refuses a non-empty list without `enableDHCHAP: true`. The cluster has no DHCHAP setting to consult.' },
    { topic: 'The pool and its list', cat: 'field', text: 'Which of these pools are admitted?', options: ['`enableDHCHAP: true` with `allowedNodes: [a, b]`', '`enableDHCHAP: true` with no `allowedNodes`', '`enableDHCHAP: false` with `allowedNodes: [a]`', 'No `volumeDefaults` and `allowedNodes: []`', '`enableDHCHAP: false` with no `allowedNodes`'], correct: [0, 1, 3, 4], explain: 'The rule only refuses a non-empty list without DHCHAP. An empty list passes, and DHCHAP without a list is admitted, though its class gets no selector.' },
    { topic: 'The pool and its list', cat: 'action', text: 'Pool `payments` has `volumeDefaults: {enableDHCHAP: true, filesystem: xfs}`. Which edits are admitted?', options: ['Adding a node to `allowedNodes`', 'Removing a node from `allowedNodes`', 'Raising `limits.capacity`', 'Setting `enableDHCHAP: false`', 'Changing `filesystem` to `ext4`'], correct: [0, 1, 2], explain: 'The list and the limits are mutable. Anything under `volumeDefaults` is refused with `field is immutable`.' },
    { topic: 'The pool and its list', cat: 'concept', text: 'Why is `volumeDefaults` immutable once set?', options: ['Its fields are written into the pool\'s class, and Kubernetes does not let a class\'s parameters be edited', 'The control plane refuses every update to a pool', 'DHCHAP keys are derived from the filesystem type', 'The CSI driver reads it once when it starts', 'The admission webhook cannot read it after creation'], correct: [0], explain: 'A pool whose defaults changed would have a class the operator cannot update and a spec that no longer describes it. The control plane accepts limit updates, so the second option is false.' },
    { topic: 'The pool and its list', cat: 'concept', text: 'Why can the cluster\'s default pool not become a DHCHAP pool?', options: ['The cluster writes it with an empty `volumeDefaults` block, the API server fills in `filesystem: xfs`, and the block is then fixed with DHCHAP off', 'A CEL rule refuses DHCHAP on a pool whose name ends in `-default`', 'The default class has no room for another parameter', 'The operator strips `allowedNodes` from the default pool', 'The control plane refuses DHCHAP on a cluster\'s first pool'], correct: [0], explain: 'The empty block is present, so it is immutable from the first write. No rule names the default pool, and the others are invented.' },

    { topic: 'What the operator does', cat: 'concept', text: 'A DHCHAP pool listing two existing nodes is `Ready`. What has the operator done?', options: ['Labeled both nodes `storage.simplyblock.io/storage-pool.<pool UUID>=allowed`', 'Added each node\'s host NQN to the pool\'s allowed hosts in the control plane', 'Created a Secret per node with its key', 'Written a StorageClass for the pool', 'Listed both names in `status.allowedNodes`'], correct: [0, 1, 4], explain: 'Labels, allowed hosts, and the status are the operator\'s. It writes no Secret, and it writes a class only for a pool named `<cluster>-default`.' },
    { topic: 'What the operator does', cat: 'field', text: 'Which host NQN does the operator register for a node whose UID is `6b1e0c4a-...`?', options: ['`nqn.2014-08.io.simplyblock:uuid:6b1e0c4a-...`', 'The same string the CSI node plugin presents when it connects a DHCHAP volume', '`nqn.2014-08.org.nvmexpress:uuid:<hostid>`', 'The value of the annotation `storage.simplyblock.io/nvme-host-nqn`', '`nqn.2014-08.io.simplyblock:worker-1`'], correct: [0, 1], explain: 'Both sides compute `nqn.2014-08.io.simplyblock:uuid:<Node UID>` with the same function, which is why no NQN is managed by hand. The kernel default and the annotation are the other NQN.' },
    { topic: 'What the operator does', cat: 'concept', text: '`allowedNodes` lists `worker-1` and `worker-9`, and there is no node `worker-9`. What happens?', options: ['A Warning event `AllowedNodeMissing` names `worker-9`, once', '`worker-9` is removed from the spec', '`status.allowedNodes` lists only `worker-1`', 'The pool is `Pending`', '`worker-1` is labeled and registered'], correct: [0, 2, 4], explain: 'The missing name is announced once and left in the spec. The pool is `Pending` only when no listed name resolves.' },
    { topic: 'What the operator does', cat: 'concept', text: 'Why is the host NQN derived from the Node UID and not from the node name?', options: ['A node rebuilt under the same name is a different host, and its old authorization should not carry over', 'Node names cannot appear in an NQN', 'The control plane limits NQNs to 36 characters', 'The kernel writes the UID into `/etc/nvme/hostnqn`', 'The node plugin cannot read its node\'s name'], correct: [0], explain: 'The UID changes when the node object is recreated, so a rebuilt machine has to be registered again. The kernel file holds the other NQN.' },
    { topic: 'What the operator does', cat: 'concept', text: 'A node joins the cluster after a pool already named it. Which of these make the pool controller pick it up?', options: ['An update to the StoragePool', 'A change to a StorageClass assigned to the pool', 'The Node object being created', 'The node plugin starting on the node', 'A new claim against the pool\'s class'], correct: [0, 1], explain: 'The controller watches pools, their assigned classes, and member clusters. It does not watch Nodes, claims, or the node plugin.' },

    { topic: 'Class and scheduling', cat: 'field', text: 'Pool `payments` has `status.uuid` `3f9a1c2e-7b4d-4e8a-9c61-2d5f0a7b8e14`. What is the value of `dhchap_node_selector` in its class?', options: ['`storage.simplyblock.io/storage-pool.3f9a1c2e-7b4d-4e8a-9c61-2d5f0a7b8e14`', '`storage.simplyblock.io/storage-pool.3f9a1c2e-7b4d-4e8a-9c61-2d5f0a7b8e14=allowed`', '`payments`', '`storage.simplyblock.io/pool`', '`true`'], correct: [0], explain: 'The parameter carries the exact label key. The CSI controller supplies the value `allowed` itself.' },
    { topic: 'Class and scheduling', cat: 'concept', text: 'Into which classes does the operator write `dhchap_node_selector`?', options: ['Every class assigned to a DHCHAP pool', 'The class it writes for a pool named `<cluster>-default`, when that pool has DHCHAP on, a non-empty list, and a UUID at that moment', 'Every class carrying the three assignment labels, at the next reconcile', 'The class of every pool whose `allowedNodes` changes', 'Classes that carry `allowedTopologies`'], correct: [1], explain: 'The operator writes one class, the default pool\'s, once. It never edits an authored class, and class parameters cannot be edited anyway.' },
    { topic: 'Class and scheduling', cat: 'concept', text: 'A class assigned to a DHCHAP pool omits `dhchap_node_selector`. What follows?', options: ['The StorageClass webhook admits it', 'The pool emits a Normal event `StorageClassOverridesPoolDefault` naming the parameter', 'Volumes provisioned from it carry no DHCHAP node affinity', 'A pod sent to a host outside the list fails to mount', 'The operator patches the class to add the parameter'], correct: [0, 1, 2, 3], explain: 'The webhook refuses only another cluster, pool, or stripe. The omission is reported as an override, the volumes are unpinned, and the control plane refuses the wrong host. Nothing patches the class.' },
    { topic: 'Class and scheduling', cat: 'concept', text: 'Why is the restriction not written as `allowedTopologies` on the class?', options: ['The external provisioner matches it against keys the node registered when its plugin started, so a pool label written later fails claims with `is not in requisite`', '`allowedTopologies` cannot be combined with `WaitForFirstConsumer`', 'The StorageClass webhook refuses `allowedTopologies`', '`allowedTopologies` may only name zone keys', 'The CRD of the pool refuses it'], correct: [0], explain: 'The registration timing is the reason. The CSI controller builds the topology from the class parameter, which does not depend on registration.' },
    { topic: 'Class and scheduling', cat: 'concept', text: 'Which statements about scheduling the pool\'s volumes are true?', options: ['The PersistentVolume gets a node affinity requiring the pool\'s label with the value `allowed`', 'Before the volume exists, the class tells the scheduler nothing about the restriction', 'After the volume exists, its node affinity keeps later pods on allowed hosts', 'The end-to-end test pins its first pod to an allowed node', 'The CSI controller reads the pod\'s `nodeSelector` to choose a host'], correct: [0, 1, 2, 3], explain: 'The affinity is built from the class parameter at CreateVolume, which is why the first pod is pinned. The controller never reads the pod.' },

    { topic: 'Keys and identities', cat: 'concept', text: 'Where do DHCHAP keys exist?', options: ['In the control plane, which generates and stores them', 'In a Secret in the `simplyblock` namespace', 'In the node plugin\'s memory while it connects', 'In the stashed volume context on the node', 'In the StoragePool\'s status'], correct: [0, 2], explain: 'The control plane holds them and hands them over per connect. They are never written to a Kubernetes object or to the stash.' },
    { topic: 'Keys and identities', cat: 'concept', text: 'How does the node plugin obtain the secrets for a connect?', options: ['From the prebuilt `nvme connect` line in the control plane\'s answer to a request naming the node\'s host NQN', 'From an annotation the operator writes on the Node', 'From a projected Secret volume', 'By deriving them from the Node UID', 'From the class parameters'], correct: [0], explain: 'The connect line is the only channel. The operator never sees a key, and nothing derives one locally.' },
    { topic: 'Keys and identities', cat: 'concept', text: 'Which statements about a node\'s two host NQNs are true?', options: ['DHCHAP connects use `nqn.2014-08.io.simplyblock:uuid:<Node UID>`', 'The annotation `storage.simplyblock.io/nvme-host-nqn` holds the kernel default `nqn.2014-08.org.nvmexpress:uuid:<hostid>`', 'Plain volumes connect with the kernel default', 'The operator reads the annotation for two-node fencing and consistency-group moves', 'The annotation is what the operator registers in a DHCHAP pool\'s allowed hosts'], correct: [0, 1, 2, 3], explain: 'The annotation is the kernel default, used by plain volumes and read for fencing and moves. The allowed hosts get the UID-derived NQN.' },
    { topic: 'Keys and identities', cat: 'concept', text: 'Why does a DHCHAP connect pass a `--hostid` derived from the DHCHAP NQN?', options: ['The kernel refuses one hostid with two different host NQNs, so a node with plain volumes connected would otherwise fail every DHCHAP connect', 'The secrets are keyed by hostid', 'nvme-cli refuses `--dhchap-secret` without `--hostid`', 'The control plane checks the hostid against the allowed hosts', 'It is how keys are rotated'], correct: [0], explain: 'Without it nvme-cli uses `/etc/nvme/hostid`, which the kernel already pairs with the default NQN.' },
    { topic: 'Keys and identities', cat: 'concept', text: 'Which are true of the DHCHAP secrets on the node?', options: ['They are redacted from the plugin\'s log', 'They are not written to the stashed volume context', 'They are not written to the stack record', 'They are passed to nvme-cli on its command line', 'They are read again from the control plane at every stage'], correct: [0, 1, 2, 3, 4], explain: 'All five hold. The stash outlives the pod that wrote it, which is why the secrets are re-read and kept in memory.' },

    { topic: 'Changing the list and diagnosing', cat: 'action', text: '`worker-3` is added to `allowedNodes`. What happens?', options: ['`worker-3` gets the pool\'s label', 'Its host NQN is added to the pool\'s allowed hosts', 'The class has to be replaced by one naming `worker-3`', 'The pool\'s existing volumes may be consumed on `worker-3`', 'The existing PersistentVolumes are rewritten'], correct: [0, 1, 3], explain: 'The PVs and the class name the pool\'s label, not nodes, so a newly labeled node qualifies for existing volumes with nothing rewritten.' },
    { topic: 'Changing the list and diagnosing', cat: 'action', text: '`worker-2` is removed from `allowedNodes`. Which of these happen?', options: ['The pool\'s label comes off `worker-2`', 'Its host NQN is removed from the pool\'s allowed hosts', 'The volumes consumed on `worker-2` are moved', 'The operator evicts the pods on `worker-2` using the pool\'s volumes', 'A `Rebalance` StoragePoolOps runs'], correct: [0, 1], explain: 'Only the label and the host change. Nothing moves or evicts, and `Rebalance` is declared and not implemented.' },
    { topic: 'Changing the list and diagnosing', cat: 'concept', text: 'A pod stays in `ContainerCreating` with `FailedMount` and `not found in allowed hosts` in the message. What are likely causes?', options: ['The pod runs on a node not in `allowedNodes`', 'The class lacks `dhchap_node_selector`, so the volume has no node affinity', 'The node was rebuilt with a new UID and the pool has not reconciled since', 'The pool\'s filesystem is `xfs`', 'The claim is `ReadWriteOnce`'], correct: [0, 1, 2], explain: 'All three put a host NQN the control plane does not have in front of it. The filesystem and the access mode have nothing to do with authentication.' },
    { topic: 'Changing the list and diagnosing', cat: 'concept', text: 'Which minimums does the repository state for DHCHAP on a node?', options: ['A kernel version', 'An nvme-cli version', 'A list of supported distributions', 'A TLS pre-shared key in the keyring', 'A hardware security module'], correct: [], explain: 'None. The repository relies on nvme-cli flags and kernel support without naming versions, and TLS is not part of DHCHAP here.' },
    { topic: 'Changing the list and diagnosing', cat: 'action', text: 'The control plane is unreachable and a DHCHAP volume has to be staged on an allowed node. What happens?', options: ['The plugin falls back to the stashed context, which has no secrets, so the volume is not attached until the control plane answers', 'The operator supplies the keys from a Secret', 'The plugin derives the secrets from the Node UID', 'The attach succeeds with secrets cached on disk', 'The pod is rescheduled to the control plane\'s node'], correct: [0], explain: 'The secrets have one source. Nothing caches them on disk by design.' },
    { topic: 'Changing the list and diagnosing', cat: 'concept', text: 'The pool is `Pending` with `every node in spec.allowedNodes resolves to nothing`. Which statements are right?', options: ['None of the listed names matches a Node object', 'The operator has pruned the names from the spec', 'Naming the nodes as `kubectl get nodes` prints them fixes it', 'DHCHAP is off on the pool', 'It is the same state as a pool with an empty `allowedNodes`'], correct: [0, 2], explain: 'The spec is never pruned, and a pool with no list at all is not held `Pending` for it. The list cannot be non-empty with DHCHAP off.' },
  ],

  tasks: [
    {
      title: 'A DHCHAP pool',
      scenario: 'Write the pool `payments` in cluster `production` so that only `worker-1` and `worker-2` may connect to its volumes.',
      [`yaml`]: [
        'apiVersion: storage.simplyblock.io/v1alpha2',
        'kind: StoragePool',
        'metadata:',
        '  name: payments',
        '  namespace: simplyblock',
        'spec:',
        '  clusterRef: production',
        '  {{list}}:',
        '    - worker-1',
        '    - worker-2',
        '  {{group}}:',
        '    {{field}}: {{value}}',
      ].join('\n') + '\n',
      blanks: {
        list: { accept: ['allowedNodes'], hint: 'the hosts, by Kubernetes Node name' },
        group: { accept: ['volumeDefaults'], hint: 'the immutable block every volume is created with' },
        field: { accept: ['enableDHCHAP'], hint: 'the one DHCHAP field of the API' },
        value: { accept: ['true'], hint: 'the list is refused without it' },
      },
      explain: '`allowedNodes` names the hosts, and the CEL rule requires `volumeDefaults.enableDHCHAP: true` beside it. The flag is fixed from creation.',
    },
    {
      title: 'The class for the pool',
      scenario: 'The pool `payments` is `Ready` with `status.uuid` `3f9a1c2e-7b4d-4e8a-9c61-2d5f0a7b8e14`. Write its class so that its volumes are pinned to the allowed nodes.',
      [`yaml`]: [
        'apiVersion: storage.k8s.io/v1',
        'kind: StorageClass',
        'metadata:',
        '  name: simplyblock-production-payments',
        '  labels:',
        '    storage.simplyblock.io/namespace: simplyblock',
        '    storage.simplyblock.io/cluster: production',
        '    storage.simplyblock.io/pool: {{poolLabel}}',
        'provisioner: {{provisioner}}',
        'parameters:',
        '  cluster_id: b2d4e6f8-1a3c-4e5f-8a9b-0c1d2e3f4a5b',
        '  pool_name: payments',
        '  {{key}}: {{selector}}',
        'volumeBindingMode: {{binding}}',
      ].join('\n') + '\n',
      blanks: {
        poolLabel: { accept: ['payments'], hint: 'the third assignment label' },
        provisioner: { accept: ['csi.simplyblock.io'], hint: 'the driver name' },
        key: { accept: ['dhchap_node_selector'], hint: 'the parameter the CSI controller turns into node affinity' },
        selector: { accept: ['storage.simplyblock.io/storage-pool.3f9a1c2e-7b4d-4e8a-9c61-2d5f0a7b8e14'], hint: 'the exact label key the operator writes on allowed nodes' },
        binding: { accept: ['WaitForFirstConsumer'], hint: 'binding waits for the pod' },
      },
      explain: 'The three labels assign the class to the pool, and `dhchap_node_selector` carries the label key with the pool\'s UUID. The value `allowed` is not part of it.',
    },
    {
      title: 'The first pod',
      scenario: 'Write the first pod using claim `ledger` from that class, placed on an allowed node.',
      [`yaml`]: [
        'apiVersion: v1',
        'kind: Pod',
        'metadata:',
        '  name: ledger',
        '  namespace: payments',
        'spec:',
        '  nodeSelector:',
        '    {{labelKey}}: {{labelValue}}',
        '  containers:',
        '    - name: app',
        '      image: registry.access.redhat.com/ubi9/ubi-minimal:9.4',
        '      command:',
        '        - sleep',
        '        - infinity',
        '      volumeMounts:',
        '        - name: data',
        '          mountPath: /data',
        '  volumes:',
        '    - name: data',
        '      {{source}}:',
        '        claimName: {{claim}}',
      ].join('\n') + '\n',
      blanks: {
        labelKey: { accept: ['storage.simplyblock.io/storage-pool.3f9a1c2e-7b4d-4e8a-9c61-2d5f0a7b8e14'], hint: 'the same key as the class parameter' },
        labelValue: { accept: ['allowed', '"allowed"'], hint: 'what the operator writes on each allowed node' },
        source: { accept: ['persistentVolumeClaim'], hint: 'the volume source naming a claim' },
        claim: { accept: ['ledger'], hint: 'the claim from the scenario' },
      },
      explain: 'Before the volume exists nothing in the class steers the scheduler, so the first pod selects the pool\'s label. Later pods are held by the PersistentVolume\'s node affinity.',
    },
  ],
});
