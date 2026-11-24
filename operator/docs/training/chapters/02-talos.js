// Chapter 2: a three-node cluster on Talos Linux. The sequence is the one the
// OpenShift chapter taught, so it is repeated here only as far as the Talos
// install states it differently: cert-manager instead of the service CA, Pod
// Security labels instead of SCCs. The depth goes to what Talos changes about
// the nodes themselves: a host with no shell, the machine config that holds the
// huge-page pool and the kubelet's CPU topology, the two modes that decide who
// writes it, and the talosctl image the node agent reaches the Talos API with.
TRAINING.register({
  id: 'talos',
  title: 'A three-node cluster on Talos Linux',
  summary: 'The deployment sequence on an immutable host: three Talos workers with their own NVMe devices, a control plane that stays separate, and the machine config that a storage node cannot write for itself.',
  goal: 'you can deploy simplyblock on a Talos cluster, know which of the draft\'s fields Talos adds and what the expansion makes of them, and can choose between the two machine-config modes and tell from a failed node add which one you are in.',

  sections: [
    {
      heading: 'What Talos changes, and what it does not',
      body: [
        'Talos Linux is a Kubernetes host with no shell, no package manager, and an immutable root filesystem. Everything about the machine, the kernel modules it loads, the sysfs values it sets, and the kubelet\'s configuration, is one document, the machine config, written through the Talos API with `talosctl`. The operator stores that in one word: `environment: Talos` on the draft, and `talos: true` on the cluster the draft expands into.',
        'What it changes for a storage node is where the host-side work goes. On every other distribution the node agent enters the host to reserve huge pages and configure the kubelet. On Talos it cannot, so it reserves the huge pages through the Talos API, with `talosctl` run as a Job, and skips the host-configuration Jobs that would enter the host. The agent itself is not part of this repository, so the chapter says what the operator hands it and what the API documents about its two modes, and no more.',
        'The three nodes of this chapter are three Talos **workers**, each with its own NVMe devices. Talos keeps its control plane on machines of its own, labeled and tainted as control-plane nodes, and nothing in this chapter changes that. The commissioned framing said a simple three-node cluster, and three storage nodes is what the default `1+1` stripe needs, so the layout below has a control plane beside the three. A Talos cluster whose three machines are all control-plane nodes with scheduling allowed on them is the compact case of chapter 1, and takes the same discovery flag.',
        { table: { head: ['Machine', 'Kubernetes role', 'simplyblock on it'], rows: [
          ['controlplane-0', '`control-plane`, tainted', 'nothing, the run leaves it out'],
          ['worker-0', 'worker', 'a storage node, a FoundationDB coordinator'],
          ['worker-1', 'worker', 'a storage node, a FoundationDB coordinator'],
          ['worker-2', 'worker', 'a storage node, a FoundationDB coordinator'],
        ] } },
        'The control plane of simplyblock, FoundationDB, the management API, and the object store, is hosted in the cluster with `source.local`, and its pods land on the schedulable nodes, which are the three workers. A worker hosting a FoundationDB pod is added to the cluster sequentially whatever `nodeProvisioningBudget` says, so the three workers are provisioned one after another here as they were on the compact cluster.',
      ],
    },
    {
      heading: 'Before the operator: cert-manager and the namespace',
      body: [
        'Talos has no security context constraints, so the two SCC bindings of chapter 1 have no counterpart. What it has is Pod Security Admission, which Talos ships enabled with the `baseline` level enforced on every namespace, a property of Talos rather than of the operator. The storage-node pods are privileged, so the `simplyblock` namespace needs the `privileged` labels, and neither the operator nor the chart writes them.',
        { code: 'kubectl create namespace simplyblock\nkubectl label namespace simplyblock \\\n  pod-security.kubernetes.io/enforce=privileged \\\n  pod-security.kubernetes.io/audit=privileged \\\n  pod-security.kubernetes.io/warn=privileged', title: 'The namespace' },
        'The control plane serves over TLS by default and needs an issuer. Off OpenShift that issuer is cert-manager: `provider: cert-manager` in the chart, which is also the CRD\'s default, and the chart creates a self-signed ClusterIssuer named `selfsigned` to mint its CA from. The chart refuses to render where `cert-manager.io/v1` is not served, so cert-manager is installed before the chart, or `tls.enabled` is set to false on a cluster that will not have it.',
        'cert-manager issues client certificates as well as serving ones, which the OpenShift service CA does not, so mutual TLS stays at its default of on: the chart issues a certificate to the operator, the two CSI plugins, Prometheus, and the FoundationDB peers.',
      ],
      examples: [
        { title: 'Helm values for a standalone install', file: 'examples/talos/01-helm-values.yaml', notes: [
          { at: 'profile: standalone', text: 'The operator hosts its own control plane, as on the compact cluster.' },
          { at: 'provider: cert-manager', text: 'The default provider and the only one off OpenShift. The render fails where the cert-manager API is not served.' },
          { at: 'createSelfSignedIssuer: true', text: 'The chart creates the ClusterIssuer named below. A deployment with an issuer of its own sets this false and names it.' },
          { at: 'mutual_enabled: true', text: 'The default, allowed only with cert-manager. Client certificates for every caller are issued by the chart.' },
          { at: 'name: talos', text: 'The run\'s name, at most 18 characters. The draft is `discovered-talos` and the proposed cluster `discovered-talos-cluster`.' },
          { at: 'enableControlPlaneNodes: false', text: 'The default, written out because it is the line chapter 1 set the other way. The control-plane machines are not storage machines here.' },
        ] },
      ],
    },
    {
      heading: 'The ControlPlane and the driver',
      body: [
        'The chart\'s `standalone` profile renders the ControlPlane, the SimplyblockDriver, and the discovery OperatorOps into the `simplyblock-bootstrap-objects` ConfigMap, and the operator applies them after it starts. An install without the chart writes the same three objects by hand, in that order. Only a ControlPlane named `simplyblock` in the operator\'s namespace is reconciled.',
        'The driver needs nothing Talos-specific. The init container that installs the NVMe-oF initiator modules enters only an Ubuntu host, because the install needs its shell and `apt`, and leaves any other host alone with an exit code of zero. On Talos the initiator modules are built into the kernel, so the node plugin\'s own start-up check finds them, and a mixed fleet is not held in `Init` by the workers it cannot enter.',
      ],
      examples: [
        { title: 'The ControlPlane', file: 'examples/talos/02-controlplane.yaml', notes: [
          { at: 'name: simplyblock', text: 'The only name the operator reconciles.' },
          { at: 'replicas: 3', text: 'Three coordinators, the default. They land on the schedulable nodes, which on this layout are the three storage workers.' },
          { at: 'enableMutualTLS: true', text: 'On, and the default. cert-manager can issue the client certificates the OpenShift provider could not.' },
          { at: 'provider: cert-manager', text: 'The CRD spells the two providers as the products do, `cert-manager` and `OpenShift`, and defaults to this one.' },
        ] },
        { title: 'The SimplyblockDriver', file: 'examples/talos/03-simplyblockdriver.yaml', notes: [
          { at: 'driverName', text: 'The provisioner the default pool\'s StorageClass is written with.' },
          { at: 'enableVolumeSnapshots', text: 'Installs the snapshotter sidecar. Unchanged from the compact cluster.' },
        ] },
      ],
    },
    {
      heading: 'Discovery on Talos',
      body: [
        'The run concludes the distribution from markers, strongest first: an API group only one installer registers, the kubelet\'s version suffix, the OS image the kubelet reports, and the labels and annotations on the nodes. Talos registers the `talos.dev` API group, labels its nodes under `talos.dev/`, and reports an OS image beginning with `Talos`. Any of them concludes `Talos`, and in the precedence among distributions only OpenShift ranks ahead of it. The conclusion is `status.environment` on the run and `spec.environment` on the draft, with the evidence in the run\'s status for the reviewer.',
        'The run also reads each worker\'s os-release and states `hostOS` on the draft when every worker agrees. On Talos that is `distro: talos` and no `family`, because a host with no package manager belongs to no packaging family, and the draft\'s note says so. The expansion spends the distro on one flag, `ubuntuHost`, which is false for everything but Ubuntu.',
        'Which machines the run inspects is decided twice, and the two decisions are separate. The **role** is read from the `node-role.kubernetes.io/<role>` labels, and a machine with the `control-plane` or the `master` role is not used unless `spec.discover.enableControlPlaneNodes` is true. The **taints** are honored on their own: a probe is pinned to its worker with `nodeName`, which bypasses the scheduler and not the taints, so a machine carrying a `NoSchedule` or `NoExecute` taint the run does not tolerate is not inspected, and the run\'s status names the taint. Talos labels and taints its control-plane machines, so on this layout they are left out twice over and the three workers are what the draft holds.',
        { callout: 'A compact Talos cluster, three control-plane machines with `cluster.allowSchedulingOnControlPlanes: true` in their machine config, is what the integration suite\'s single-node cluster is built like. Scheduling is allowed there, but the role label stays, so the run still needs `enableControlPlaneNodes: true`, as on the compact OpenShift cluster, and the draft then names the machines under a node set called `control-plane`.', kind: 'info' },
        { code: 'kubectl get oops -n simplyblock -o jsonpath=\'{range .items[*]}{.status.environment}{"\\n"}{.status.message}{"\\n"}{end}\'\nkubectl get cdc -n simplyblock', title: 'Following the run' },
      ],
      examples: [
        { title: 'The discovery run', file: 'examples/talos/04-operatorops-discover.yaml', notes: [
          { at: 'action: Discover', text: 'The only OperatorOps action. The run inspects and writes a draft, and changes nothing else.' },
          { at: 'enablePartitionedDevices: true', text: 'Reports disks that carry a stale partition table. A mounted or busy device is still never reported.' },
        ] },
      ],
    },
    {
      heading: 'The machine config: who writes it',
      body: [
        'A storage node on Talos needs two things in the worker\'s machine config: the huge-page pool, under `machine.sysfs`, and the kubelet\'s CPU topology, under `machine.kubelet.extraConfig`. `talosMachineConfig` on the draft\'s cluster template says who writes them, and lands on `StorageCluster.spec.storageNodes.talosMachineConfig`. The CRD refuses the field unless `spec.environment` is `Talos`, because the question exists on no other distribution.',
        { table: { head: ['Mode', 'Who writes the machine config', 'What the node does', 'What the namespace holds'], rows: [
          ['`Preprovisioned`, the default', 'The cluster\'s operator, before the draft is approved', 'Verifies both settings, applies nothing, and fails its add naming the patch to apply', 'No Talos API credentials'],
          ['`Managed`', 'The node itself, through the Talos API', 'Patches the machine config with `talosctl` Jobs', 'The `os:admin` credentials Secret the Talos API access feature issues'],
        ] } },
        'Unstated is `Preprovisioned`, and the expansion leaves it unstated on the cluster so that the cluster reads as the document did. The node agent gets it as `TALOS_MACHINE_CONFIG`, `preprovisioned` or `managed`, and the environment is only set when the cluster states Talos. The draft of this chapter writes the default out, so a reviewer sees who is expected to have written the machine config before approving.',
        'The image the node runs those Jobs with is `spec.images.talosctl`, an image this product does not build: Sidero publishes it on `ghcr.io`, and a mirror on an air-gapped site names its own registry, so any registry is admitted and only the shape of the reference is checked. It is the fleet\'s image, written to `StorageCluster.spec.storageNodes.talosctlImage` and handed to the DaemonSet as `TALOSCTL_IMAGE`, and unstated keeps the agent\'s own default. A deployment pins it to the cluster\'s Talos minor.',
        'The shape of the `os:admin` Secret, that is its name and keys, is the agent\'s contract with the Talos API access feature and is not written down in this repository, so this chapter cannot show it. What it can show is the patch a `Preprovisioned` deployment applies, to the extent the API documents it.',
      ],
      examples: [
        { title: 'The machine-config patch of a Preprovisioned deployment', file: 'examples/talos/05-machine-config-patch.yaml', notes: [
          { at: 'machine:', text: 'A Talos machine-config patch, applied with `talosctl patch machineconfig` to every worker before the draft is approved. It is not an object of the operator.' },
          { at: '# sysfs:', text: 'The huge-page pool. The agent verifies it under `machine.sysfs` and names the entry it expects when it fails the add. The entry is the agent\'s to name, so it is a comment here.' },
          { at: 'cpuManagerPolicy: static', text: 'CPU pinning holds only on a kubelet running the static CPU manager, and on Talos the kubelet is configured through `extraConfig` and nowhere else.' },
        ] },
      ],
    },
    {
      heading: 'Reviewing and approving the draft',
      body: [
        'The review is the one chapter 1 taught: read `status.message`, check that all three workers are listed with every device named by PCI address, set `cluster.name`, and set `approved: true`, after which the document is immutable. On a Talos draft the reviewer also reads four fields the run wrote or left at their defaults.',
        { steps: [
          '`environment: Talos`. Detected by the run, and correctable in the draft. It is what the expansion spends on `talos`, the kubelet configuration, and the CPU topology.',
          '`hostOS.distro: talos` with no family. The fleet is uniform, and nothing on it is Ubuntu.',
          '`cluster.talosMachineConfig`. Left out or `Preprovisioned` means the patches have to be on the workers before approval. `Managed` means the Secret has to be in the namespace before the nodes are added.',
          '`images.talosctl`. Unstated keeps the agent\'s default. Stated, it has to be reachable from the workers, which on an air-gapped site means a mirror.',
        ] },
        'What `environment: Talos` resolves to on the cluster is three flags the storage nodes then carry: `talos: true`, `enableKubeletConfiguration: true`, and `enableCpuTopology: true`. The kubelet flag is on because Talos has a kubelet to configure, through `machine.kubelet.extraConfig`, and the CPU topology is on because it is what that configuration is for. The design document of the redesign still shows the Talos row with the kubelet configuration not applied and the CPU topology unset, which was the plan before the Talos API path existed, and the code is what this chapter teaches. A document that states either flag on its cluster template replaces what the environment decided.',
        { table: { head: ['On the draft', 'On the StorageCluster', 'On the storage-node DaemonSet'], rows: [
          ['`environment: Talos`', '`storageNodes.talos: true`', '`TALOS_CLUSTER=true`'],
          ['`environment: Talos`', '`storageNodes.enableKubeletConfiguration: true`', '`SKIP_KUBELET_CONFIGURATION=false`'],
          ['`environment: Talos`', '`storageNodes.enableCpuTopology: true`', '`CPU_TOPOLOGY_ENABLED=true`'],
          ['`cluster.talosMachineConfig`', '`storageNodes.talosMachineConfig`', '`TALOS_MACHINE_CONFIG=preprovisioned` or `managed`'],
          ['`images.talosctl.image`', '`storageNodes.talosctlImage`', '`TALOSCTL_IMAGE`, only when stated'],
          ['`hostOS.distro: talos`', '`storageNodes.ubuntuHost: false`', '`UBUNTU_HOST=false`'],
        ] } },
      ],
      examples: [
        { title: 'The reviewed draft', file: 'examples/talos/06-clusterdeploymentconfig.yaml', notes: [
          { at: 'approved: true', text: 'The review gate. Immutable afterward, and never withdrawn.' },
          { at: 'environment: Talos', text: 'Detected from the `talos.dev` API group and the node labels. It resolves to `talos`, the kubelet configuration, and the CPU topology on the cluster, after which nothing reads it again.' },
          { at: 'distro: talos', text: 'Read from every worker\'s os-release. No `family`, because Talos belongs to no packaging tradition, and `ubuntuHost` resolves to false.' },
          { at: 'ghcr.io/siderolabs/talosctl', text: 'The image the node agent runs as a Job to reach the Talos API. Any registry is admitted because simplyblock does not build it. Pinned here to the Talos minor the integration suite boots.' },
          { at: 'talosMachineConfig: Preprovisioned', text: 'The default, written out. The patches are on the workers already, and no Talos API credentials are in the namespace. Admitted only because `environment` is `Talos`.' },
          { at: 'nodeProvisioningBudget: 1', text: 'Serial, the default. A worker hosting a FoundationDB pod is sequential whatever this says, and on three workers with three coordinators that is likely every worker.' },
          { at: 'worker-0', text: 'The three workers, by their Kubernetes names. The control-plane machine is not among them and was not inspected.' },
          { at: 'mgmtInterface: eth0', text: 'The interface holding the node\'s InternalIP, which is what the draft names so that the control plane reaches the worker on the network Kubernetes does. Talos names interfaces as the kernel does.' },
          { at: '0000:01:00.0', text: 'Every device the group hands to simplyblock, by PCI address, with no filter left in the document.' },
        ] },
      ],
    },
    {
      heading: 'What the operator does after approval',
      body: [
        'The phases are the ones chapter 1 listed: the document moves `Draft`, `Expanding`, `Expanded`, the StorageCluster `Pending`, `Creating`, `Provisioning`, `Activating`, `Online`, and each StorageNode `Pending`, `Provisioning`, `Online`. The storage-node DaemonSet is rendered with the Talos variables above, and the node agent takes it from there: it reserves the huge pages through the Talos API with a `talosctl` Job, verifies or patches the machine config according to its mode, and the control plane adds the node. Whether a Talos worker reboots during its add is the agent\'s behavior and is not readable from this repository.',
        'In `Preprovisioned` mode a worker whose machine config lacks either setting fails its add, and the failure names the patch to apply. That is the mode\'s design rather than a defect: the operator of the cluster is the one with the Talos credentials, and the node tells them what it needs instead of holding credentials of its own.',
        'Once the cluster is `Online` its reconciler writes the default pool `talos-default` and the StorageClass `simplyblock-talos`, and a claim against that class is the first volume. The node plugin attaches it over NVMe-oF TCP with the initiator modules the Talos kernel builds in.',
        { code: 'kubectl get cdc,stc,sn,sp -n simplyblock\nkubectl get storageclass simplyblock-talos\nkubectl get ds -n simplyblock -o jsonpath=\'{range .items[*].spec.template.spec.containers[*].env[?(@.name=="TALOS_CLUSTER")]}{.value}{"\\n"}{end}\'', title: 'The objects after expansion' },
        'The CSI driver\'s integration suite boots a Talos cluster in QEMU for every run, pinned to one `talosctl` release, so the driver\'s path on a Talos kernel is exercised in CI. The operator\'s own Talos branch, what the expansion resolves and what the DaemonSet carries, is covered by its unit tests. Neither is a procedure a deployment follows.',
      ],
      examples: [
        { title: 'The first claim', file: 'examples/talos/07-pvc.yaml', notes: [
          { at: 'storageClassName: simplyblock-talos', text: 'The class the cluster wrote for its default pool, named after the cluster with the leading `simplyblock` collapsed.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'A StorageNode stays in `Provisioning`, and the node\'s add failed with a message naming a machine-config patch.', cause: 'The deployment is in `Preprovisioned` mode, the default, and the worker\'s machine config lacks the huge-page pool or the kubelet\'s CPU topology.', fix: 'Apply the patch the message names to the worker with `talosctl patch machineconfig`. For a fleet that would rather have the node do it, write the next document with `talosMachineConfig: Managed` and the `os:admin` Secret in the namespace.' },
          { symptom: 'The draft has no workers, and the run reports every machine as a control-plane node to include with `enableControlPlaneNodes`.', cause: 'The cluster is compact: every machine carries the `control-plane` role label, and scheduling allowed on it does not remove the label.', fix: 'Set `spec.discover.enableControlPlaneNodes: true` on a new OperatorOps. A finished run is not repeated.' },
          { symptom: 'The run reports a machine as not used because it carries a taint `spec.discover.tolerations` does not tolerate.', cause: 'A control-plane taint on a machine meant to be storage, or a dedicated-storage taint the fleet put on its workers.', fix: 'Add the toleration to the run. It is copied into the draft, so the storage-node DaemonSet tolerates the same taint.' },
          { symptom: '`helm install` refuses to render, naming the cert-manager API.', cause: '`tls.provider: cert-manager` with `cert-manager.io/v1` not served on the cluster.', fix: 'Install cert-manager first. A cluster that will not have it sets `tls.enabled: false`.' },
          { symptom: 'Storage-node pods are refused at admission as violating the `baseline` Pod Security level.', cause: 'The namespace carries Talos\'s default enforcement and not the `privileged` labels.', fix: 'Label the namespace `pod-security.kubernetes.io/enforce=privileged`, with `audit` and `warn` to match.' },
          { symptom: 'The draft is refused: `spec.cluster.talosMachineConfig` is what a deployment onto Talos states.', cause: 'The mode was set while `spec.environment` is something other than `Talos`, or unset.', fix: 'Set `environment: Talos`, or remove the mode from a document that does not target Talos.' },
          { symptom: 'The draft states no `hostOS`, and its note says the workers do not run one operating system.', cause: 'A worker in the run is not Talos, which on a mixed fleet is the one that is being replaced.', fix: 'Narrow the run with `workers` or `nodeSelector`, or correct the fleet. A document builds one DaemonSet, which carries one host OS.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Talos and the layout', cat: 'concept', text: 'Which of these are true of a Talos host, as the operator\'s API describes it?', options: ['It has no shell', 'It has no package manager', 'Its kubelet is configured only through the machine config', 'It is reached through the Talos API', 'Its root filesystem is immutable'], correct: [0, 1, 2, 3, 4], explain: 'All five. Talos is an immutable host with no shell, no package manager, and a kubelet configured only through its machine config, which is written through the Talos API. The node agent never enters the host.' },
    { topic: 'Talos and the layout', cat: 'concept', text: 'Why does this chapter\'s layout keep the Talos control plane off the storage machines?', options: ['Talos labels and taints its control-plane machines, and the run leaves them out by default', 'A storage node cannot run on a Talos control-plane machine', 'The operator refuses a Talos draft with control-plane workers', 'FoundationDB cannot share a machine with etcd', 'The CSI driver needs a dedicated control plane'], correct: [0], explain: 'Nothing refuses the placement. The role label keeps a control-plane machine out of a draft unless the run is told to include it, and the taint keeps the probe off it unless tolerated, so the usual Talos layout has the storage on the workers.' },
    { topic: 'Talos and the layout', cat: 'concept', text: 'What does `environment: Talos` resolve to on the StorageCluster the draft expands into?', options: ['`storageNodes.talos: true`', '`storageNodes.enableKubeletConfiguration: true`', '`storageNodes.enableCpuTopology: true`', '`storageNodes.openshift` with an empty block', '`storageNodes.ubuntuHost: false`'], correct: [0, 1, 2], explain: 'The environment is spent on the three flags. The OpenShift block is what an OpenShift document states, and `ubuntuHost` is decided by `hostOS.distro`, not by the environment.' },
    { topic: 'Talos and the layout', cat: 'concept', text: 'The design document of the CRD redesign shows the Talos row with the kubelet configuration not applied and the CPU topology unset. What does the code do?', options: ['Exactly that, the design document is current', 'Turns both on, because the kubelet is configured through the Talos API', 'Turns the kubelet configuration on and leaves the CPU topology unset', 'Leaves both to the cluster\'s defaults', 'Refuses a Talos document until the flags are stated'], correct: [1], explain: 'The expansion sets `enableKubeletConfiguration` and `enableCpuTopology` true for Talos, through `machine.kubelet.extraConfig`. The design document predates that path, and the chapter teaches the code.' },
    { topic: 'Talos and the layout', cat: 'concept', text: 'With `source.local` and three FoundationDB replicas on a Talos cluster with one control-plane machine and three workers, which of these follow?', options: ['The coordinators land on the three workers', 'The workers hosting a FoundationDB pod are added sequentially whatever the budget says', 'The control-plane machine hosts a coordinator', 'The storage nodes wait for the control-plane machine to reboot', 'The cluster needs a fourth worker for the object store'], correct: [0, 1], explain: 'The control-plane machine is tainted, so the control plane\'s pods land on the schedulable workers, and a worker hosting a FoundationDB pod is sequential by the budget\'s own rule. Nothing waits on the control-plane machine, and the object store runs beside the rest.' },
    { topic: 'Before the operator', cat: 'concept', text: 'Which of these does a Talos install do before the operator that the OpenShift install did not?', options: ['Install cert-manager', 'Bind the `privileged` SCC', 'Bind the `anyuid` SCC', 'Label the namespace with the `privileged` Pod Security level', 'Create a self-signed ClusterIssuer by hand'], correct: [0], explain: 'cert-manager has to be installed before the chart renders, and the chart creates the ClusterIssuer itself. Talos has no SCCs, and the Pod Security labels are needed on both platforms.' },
    { topic: 'Before the operator', cat: 'concept', text: 'Why does the `simplyblock` namespace need the `privileged` Pod Security labels on Talos?', options: ['Talos ships Pod Security Admission enforcing `baseline` on every namespace', 'The operator refuses to start in a namespace without them', 'The chart reads the labels to select the Talos path', 'The Talos API checks them before issuing credentials', 'FoundationDB requires a privileged namespace'], correct: [0], explain: 'The storage-node pods are privileged and the default enforcement refuses them. It is a property of Talos, and neither the operator nor the chart writes the labels.' },
    { topic: 'Before the operator', cat: 'field', text: 'Which chart values does a Talos install state as this chapter does?', options: ['`tls.provider: cert-manager`', '`tls.mutual_enabled: true`', '`tls.provider: talos`', '`deployment.profile: talos`', '`discovery.enableControlPlaneNodes: false`'], correct: [0, 1, 4], explain: 'cert-manager with mutual TLS on is the chart\'s default off OpenShift, and the control-plane flag stays off because the storage machines are workers. There is no Talos provider and no Talos profile: the chart carries nothing Talos-specific.' },
    { topic: 'Before the operator', cat: 'field', text: 'Which of these does the Helm chart render specifically for a Talos cluster?', options: ['A `talos` deployment profile', 'A `talos` TLS provider', 'The `privileged` Pod Security labels on the namespace', 'The `os:admin` credentials Secret', 'The machine-config patch for the workers'], correct: [], explain: 'None of them. The chart carries no Talos value at all: the profiles are `standalone`, `managed`, and `empty`, the providers `cert-manager` and `openshift`, and the labels, the Secret, and the patch are the administrator\'s to write.' },
    { topic: 'Before the operator', cat: 'concept', text: '`helm install` refuses to render with an error naming the cert-manager API. Which of these fix it?', options: ['Install cert-manager and install again', 'Set `tls.enabled: false`', 'Set `tls.provider: openshift`', 'Set `tls.mutual_enabled: false`', 'Set `tls.cert-manager.createSelfSignedIssuer: false`'], correct: [0, 1], explain: 'The chart refuses the cert-manager provider where `cert-manager.io/v1` is not served. The OpenShift provider is refused off OpenShift, and the other two values do not change which API the render needs.' },
    { topic: 'Install', cat: 'field', text: 'Which of these are true of mutual TLS on the ControlPlane of this chapter?', options: ['It is on by default', 'cert-manager issues the client certificates', 'It has to be off, as on OpenShift', 'It is ignored when `enableTLS` is false', 'It needs a client certificate the deployment provisions by hand'], correct: [0, 1, 3], explain: 'Both TLS fields default to on. The chart issues a client certificate to every caller it deploys, so nothing is provisioned by hand, and the OpenShift restriction comes from the service CA, which is not in play here.' },
    { topic: 'Install', cat: 'field', text: 'What does the init container that installs the NVMe-oF initiator modules do on a Talos worker?', options: ['Installs them through the Talos API', 'Leaves the host alone and exits with zero', 'Fails, holding the node plugin in `Init`', 'Loads them with a machine-config patch', 'Runs `apt` inside the host'], correct: [1], explain: 'Only an Ubuntu host is entered, because the install needs its shell and `apt`. Any other host is left alone so that a mixed fleet is not held in `Init`, and on Talos the initiator modules are built into the kernel anyway.' },
    { topic: 'Install', cat: 'concept', text: 'Which objects does the chart\'s `standalone` profile hand to the operator through the `simplyblock-bootstrap-objects` ConfigMap on Talos?', options: ['The ControlPlane', 'The SimplyblockDriver', 'The discovery OperatorOps', 'The machine-config patch', 'The `os:admin` Secret'], correct: [0, 1, 2], explain: 'The same three as on any platform. The machine-config patch is applied with `talosctl`, and the Secret, when a deployment is `Managed`, is the administrator\'s to create.' },
    { topic: 'Discovery', cat: 'concept', text: 'Which markers conclude `Talos` in a discovery run?', options: ['The `talos.dev` API group', 'Node labels under `talos.dev/`', 'An OS image beginning with `Talos`', 'A kubelet version suffix `+talos`', 'The `machineconfiguration.openshift.io` API group'], correct: [0, 1, 2], explain: 'The API group is the strongest marker, the labels the weakest, and the OS image is matched as a prefix. Talos builds no suffix into the kubelet version, and the OpenShift group concludes OpenShift.' },
    { topic: 'Discovery', cat: 'field', text: 'What does the run state as `hostOS` on a draft whose three workers all run Talos?', options: ['`distro: talos` and no `family`', '`distro: talos` and `family: Alpine`', '`distro: Talos` and `family: Talos`', 'Nothing, because Talos has no os-release', 'Nothing, because `environment: Talos` already says it'], correct: [0], explain: 'The distro is the os-release ID, lowercase and verbatim, and a host with no package manager belongs to no packaging family. The environment is a different fact about the cluster rather than the machines.' },
    { topic: 'Discovery', cat: 'concept', text: 'A Talos control-plane machine carries the `control-plane` role label and the control-plane taint. Which of these keep it out of the draft?', options: ['The role label, unless `enableControlPlaneNodes` is true', 'The taint, unless `spec.discover.tolerations` tolerates it', 'The `talos.dev/` labels', 'The absence of NVMe devices on it', 'The `Talos` environment'], correct: [0, 1], explain: 'The role and the taints are two separate decisions, and this machine is excluded by both. The Talos markers conclude the environment and exclude nothing.' },
    { topic: 'Discovery', cat: 'concept', text: 'A Talos cluster has three control-plane machines with `cluster.allowSchedulingOnControlPlanes: true` and nothing else. What does a run with the default settings write?', options: ['A draft with three workers', 'A draft with no workers, reporting each machine as a control-plane node to include with `enableControlPlaneNodes`', 'A draft with a node set named `control-plane`', 'No draft, the run fails on the taints', 'A draft with `environment: Vanilla`'], correct: [1], explain: 'Allowing scheduling removes the obstacle the taint was, but the role label stays and the run is not allowed to use the machines. With the flag set the same cluster yields a `control-plane` node set, as the compact cluster did.' },
    { topic: 'Discovery', cat: 'field', text: 'Which of these does the run record about a machine it did not use?', options: ['A `WorkerDeclined` event naming the untolerated taint', 'A line in `status.message` saying why it is not used', 'A worker entry in the draft with `enabled: false`', 'A node set the reviewer can enable', 'Nothing, unused machines are silent'], correct: [0, 1], explain: 'The run says why in its status and in an event, and the draft holds only the workers it used. There is no disabled entry in a document.' },
    { topic: 'Machine config', cat: 'field', text: 'Which two settings does a storage node need in a Talos worker\'s machine config?', options: ['The huge-page pool under `machine.sysfs`', 'The kubelet\'s CPU topology under `machine.kubelet.extraConfig`', 'The NVMe-oF initiator modules under `machine.kernel.modules`', 'The storage-node image under `machine.install`', 'The control-plane taint under `machine.nodeTaints`'], correct: [0, 1], explain: 'The API names the two. The initiator modules are built into the Talos kernel, and the other two are invented.' },
    { topic: 'Machine config', cat: 'concept', text: 'Which of these are true of `Preprovisioned` mode?', options: ['It is the default', 'The node verifies the machine config and applies nothing', 'A node whose machine config lacks a setting fails its add naming the patch to apply', 'No Talos API credentials exist in the namespace', 'The operator applies the patch through the Talos API'], correct: [0, 1, 2, 3], explain: 'In the default mode the cluster\'s operator writes the machine config before the nodes are added, and the node only checks. Nothing in the namespace holds Talos credentials, which is the point of the mode.' },
    { topic: 'Machine config', cat: 'concept', text: 'Which of these are true of `Managed` mode?', options: ['The node patches the machine config itself, through the Talos API', 'It needs the `os:admin` credentials Secret in the namespace', 'The node runs `talosctl` as Jobs', 'It is the default', 'The operator writes the Secret from the ControlPlane'], correct: [0, 1, 2], explain: 'The node does the writing, with the image `images.talosctl` names, and the Secret is the one the Talos API access feature issues. `Preprovisioned` is the default, and nothing in the operator creates the Secret.' },
    { topic: 'Machine config', cat: 'field', text: 'A document states `cluster.talosMachineConfig: Managed` with `environment: Vanilla`. What happens?', options: ['The CRD refuses it, because the mode is what a deployment onto Talos states', 'The mode is ignored', 'The environment is corrected to `Talos`', 'The cluster is created with `talos: true`', 'The draft is marked `Failed` at expansion'], correct: [0], explain: 'A CEL rule ties the field to the environment at admission, so the document is never stored. The companion rule ties `cluster.openshift` to `OpenShift`.' },
    { topic: 'Machine config', cat: 'field', text: 'Which of these are true of `spec.images.talosctl`?', options: ['Any registry is admitted, because simplyblock does not build the image', 'Unstated keeps the node agent\'s own default', 'It is written per node, like the SPDK image', 'It lands on `StorageCluster.spec.storageNodes.talosctlImage`', 'It reaches the DaemonSet as `TALOSCTL_IMAGE`'], correct: [0, 1, 3, 4], explain: 'It is the fleet\'s image, one DaemonSet and one Job image, and the only image slot that admits a registry other than simplyblock\'s. The SPDK images are the ones written per node.' },
    { topic: 'Machine config', cat: 'field', text: 'Which environment variables does the storage-node DaemonSet carry only on a cluster that states Talos?', options: ['`TALOS_MACHINE_CONFIG`', '`TALOS_CLUSTER`', '`TALOSCTL_IMAGE`', '`UBUNTU_HOST`', '`CPU_TOPOLOGY_ENABLED`'], correct: [0], explain: '`TALOS_CLUSTER` is always present and reads `false` elsewhere, `TALOSCTL_IMAGE` is present whenever an image is stated, and the other two are on every DaemonSet. Only the mode is added on the condition that Talos is stated.' },
    { topic: 'Machine config', cat: 'concept', text: 'What does this chapter show for the `os:admin` Secret of `Managed` mode?', options: ['Its name and keys, as the operator documents them', 'A `talosctl` command that creates it', 'Nothing, because its shape is the node agent\'s contract and is not written down in this repository', 'The Secret the chart renders for it', 'The Secret the ControlPlane writes'], correct: [2], explain: 'The API says the Secret exists and what issues it, and no more. The chapter says so rather than inventing a shape.' },
    { topic: 'Deployment config', cat: 'field', text: 'Which fields of this chapter\'s draft are there because the cluster is Talos?', options: ['`environment: Talos`', '`hostOS.distro: talos`', '`cluster.talosMachineConfig`', '`images.talosctl`', '`cluster.stripe`'], correct: [0, 1, 2, 3], explain: 'The four are what the Talos review reads. The stripe is on every draft.' },
    { topic: 'Deployment config', cat: 'concept', text: 'The reviewer writes `cluster.talosMachineConfig: Preprovisioned` although it is the default. Which of these change?', options: ['The cluster is created with the mode stated', 'The DaemonSet\'s `TALOS_MACHINE_CONFIG`', 'What the node agent does', 'Whether the draft is admitted', 'Whether the patches have to be on the workers before approval'], correct: [0], explain: 'Stated, the expansion copies it to the cluster. Unstated, the cluster reads unstated and the DaemonSet still says `preprovisioned`, so the agent does the same either way, and the draft is admitted either way because the environment is Talos.' },
    { topic: 'Deployment config', cat: 'field', text: 'The draft names `mgmtInterface: eth0` on every worker. Where did the run take that from?', options: ['The interface holding the node\'s InternalIP', 'The first interface in `talosctl get links`', 'The `br-ex` bridge', 'The interface with the most bandwidth', 'A value `images.talosctl` provides'], correct: [0], explain: 'The operator addresses a worker by its InternalIP everywhere else, so the draft names the interface that holds it, and Talos names interfaces as the kernel does. `br-ex` is OpenShift\'s bridge.' },
    { topic: 'Expansion', cat: 'concept', text: 'A StorageNode on a `Preprovisioned` deployment stays in `Provisioning` and its add failed naming a patch. Which of these are reasons to act?', options: ['Apply the patch the message names with `talosctl`', 'Switch the live cluster to `Managed` by editing the StorageCluster', 'Re-create the draft', 'Grant the `privileged` SCC', 'Delete the StorageNode'], correct: [0], explain: 'The mode\'s design is that the node tells the operator what the machine config lacks. The fix is the patch. The document is immutable, and the SCC does not exist on Talos.' },
    { topic: 'Expansion', cat: 'concept', text: 'After the cluster `talos` in the namespace `simplyblock` is `Online`, which objects does its reconciler write?', options: ['A StoragePool named `talos-default`', 'A StorageClass named `simplyblock-talos`', 'A StorageClass named `simplyblock-simplyblock-talos`', 'A machine-config patch per worker', 'A `talosctl` Job per worker'], correct: [0, 1], explain: 'The default pool is `<cluster>-default` and its class collapses the leading `simplyblock`. The Jobs are the node agent\'s, run during provisioning, not the cluster reconciler\'s.' },
    { topic: 'Expansion', cat: 'concept', text: 'What does the integration suite\'s Talos cluster prove about this chapter?', options: ['That the CSI driver\'s path on a Talos kernel is exercised in CI', 'That a deployment follows the suite\'s procedure', 'That the operator\'s Talos expansion is covered by it', 'That the machine-config patch is the suite\'s `nvmet` patch', 'That QEMU is a supported deployment target'], correct: [0], explain: 'The suite boots Talos in QEMU to run the driver\'s volume stack on a real kernel. The operator\'s Talos branch is covered by its unit tests, and nothing about the suite is a user procedure.' },
  ],

  tasks: [
    {
      title: 'The reviewed Talos draft',
      scenario: 'Complete the draft so that it is expanded into a cluster named `talos` on Talos, with the machine config written by the cluster\'s operator before approval, and with the `talosctl` image pinned to a mirror.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: ClusterDeploymentConfig\nmetadata:\n  name: discovered-talos\n  namespace: simplyblock\nspec:\n  approved: true\n  environment: {{environment}}\n  hostOS:\n    distro: {{distro}}\n  images:\n    talosctl:\n      image: {{image}}\n  cluster:\n    name: {{name}}\n    maxSubsystemCount: 50\n    vcpuCount: 8\n    talosMachineConfig: {{mode}}\n  nodeSets:\n    - name: default\n      groups:\n        - name: default\n          workers: [worker-0, worker-1, worker-2]\n          mgmtInterface: eth0\n          dataInterfaces: [eth0]\n          devices:\n            nvme: ["0000:01:00.0"]\n',
      blanks: {
        environment: { accept: ['Talos'], hint: 'the enum\'s spelling, and what the mode below requires' },
        distro: { accept: ['talos'], hint: 'the os-release ID, lowercase and verbatim' },
        image: { accept: ['/^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?\\/[a-z0-9][a-z0-9._\\/-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?$/'], hint: 'any registry, repository, and tag, such as `registry.example.com/siderolabs/talosctl:v1.13.8`' },
        name: { accept: ['talos'], hint: 'the default pool and the class are named after it' },
        mode: { accept: ['Preprovisioned'], hint: 'the mode in which the node verifies and applies nothing' },
      },
      explain: '`Talos` is the environment the mode requires, `talos` is the distro as os-release spells it, the image may name any registry because simplyblock does not build it, the cluster name decides `talos-default` and `simplyblock-talos`, and `Preprovisioned` is the mode in which the cluster\'s operator writes the machine config first.',
    },
    {
      title: 'The ControlPlane off OpenShift',
      scenario: 'Write the control plane the operator hosts on a Talos cluster, with certificates from cert-manager, client certificates required, and three coordinators.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: ControlPlane\nmetadata:\n  name: {{name}}\n  namespace: simplyblock\nspec:\n  source:\n    {{source}}:\n      image: quay.io/simplyblock-io/simplyblock:26.4.0-beta4\n      foundationDB:\n        replicas: {{replicas}}\n      tls:\n        enableMutualTLS: {{mutual}}\n        provider: {{provider}}\n',
      blanks: {
        name: { accept: ['simplyblock'], hint: 'the only name the operator reconciles' },
        source: { accept: ['local'], hint: 'hosted here, not pointed at' },
        replicas: { accept: ['3'], hint: 'the smallest count that survives one loss' },
        mutual: { accept: ['true'], hint: 'this issuer can issue client certificates' },
        provider: { accept: ['cert-manager'], hint: 'spelled as the product spells it, and the CRD\'s default' },
      },
      explain: 'Only `simplyblock` is reconciled, `source.local` hosts the control plane, three coordinators survive one loss, and cert-manager issues client certificates, so mutual TLS stays on where the OpenShift provider had to turn it off.',
    },
    {
      title: 'A discovery run on a compact Talos cluster',
      scenario: 'Write the run for a Talos cluster whose three machines are all control-plane nodes with scheduling allowed on them, and which reports disks that still carry an old partition table.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: compact-talos\n  namespace: simplyblock\nspec:\n  action: {{action}}\n  discover:\n    enableControlPlaneNodes: {{cp}}\n    enablePartitionedDevices: {{partitioned}}\n',
      blanks: {
        kind: { accept: ['OperatorOps'], hint: 'the operator-level operation kind, short name `oops`' },
        action: { accept: ['Discover'], hint: 'its only action today' },
        cp: { accept: ['true'], hint: 'scheduling allowed on them removes the taint and not the role label' },
        partitioned: { accept: ['true'], hint: 'the one availability condition that can be waived' },
      },
      explain: 'An `OperatorOps` with `action: Discover` writes the draft. The three machines keep the `control-plane` role label whatever their taints, so the run has to be allowed to use them, and `enablePartitionedDevices` reports the disks a reused machine carries.',
    },
  ],
});
