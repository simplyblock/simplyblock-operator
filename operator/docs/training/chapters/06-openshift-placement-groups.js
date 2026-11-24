// Chapter 6: a 96-node cluster on OpenShift through placement groups. The
// chapter where the fleet outgrows one cluster: a simplyblock cluster holds at
// most 32 storage nodes, so 96 workers are three clusters, and a supercluster
// is what makes them one StorageCluster to Kubernetes. A placement group is a
// member of that supercluster, and the node label of the same name is how a
// worker is assigned to one. The deployment sequence is chapter 1's, with one
// draft that carries three node sets instead of one, and what is new is what
// the operator writes from it and where a volume lands afterward.
TRAINING.register({
  id: 'openshift-placement-groups',
  title: 'A 96-node cluster on OpenShift through placement groups',
  summary: 'Ninety-six OpenShift workers as one StorageCluster: three members of 32 storage nodes each, drafted from the placement-group label, expanded into a supercluster, and fed by one StorageClass whose volumes the operator places.',
  goal: 'you can label a fleet into placement groups, read a draft with several node sets and the findings it can carry, name the objects the expansion writes and the order it writes them in, say where a volume lands and why, and add a fourth member later.',

  sections: [
    {
      heading: 'Placement groups are the members of a supercluster',
      body: [
        'The framing this chapter was commissioned under is a large cluster deployed through placement groups. In the operator the phrase names one thing exactly. A simplyblock cluster holds at most 32 storage nodes, and a fleet larger than that is several clusters, which a **supercluster** presents to Kubernetes as one `StorageCluster`. Each cluster inside it is a **member**, reported as `status.role: PlacementGroup` because it is one of the groups a new volume is placed into, and the node label `storage.simplyblock.io/placement-group` is how a worker is assigned to one. The chapter uses member and placement group for the same object.',
        'The limit is about exposure rather than capacity. A cluster\'s fault tolerance is fixed by its erasure-coding scheme and does not grow with its node count, while the chance that more nodes fail at once than the scheme tolerates grows with every node added. Ninety-six workers with one storage node each are therefore three members of 32, and the chapter deploys exactly that. The control plane does not enforce the limit: the operator does, at approval.',
        { table: { head: ['Object', 'What marks it', '`status.role`', 'What it has'], rows: [
          ['Supercluster', '`spec.supercluster`', '`Supercluster`', 'No nodes and no sizing. One name for pools, classes, backup policies, and dashboards, a status rolled up from the members, and the placement decision'],
          ['Member', '`spec.superclusterRef`', '`PlacementGroup`', 'A complete cluster: its own nodes, stripe, failure domains, and storage-node workload'],
          ['Ordinary cluster', 'neither', '`Standalone`', 'What chapters 1 and 5 deployed'],
        ] } },
        'A supercluster adds no data path. A volume lives in exactly one member for its whole life unless it is moved, nothing stripes or mirrors across members, and a member\'s failure is that member\'s volumes\' failure. Every member is a cluster of the same control plane, in the same namespace as its supercluster, and a supercluster cannot be a member of another.',
        { callout: 'The superclusters design is still marked Draft, and its phases are not all built. Built: superclusters and their expansion from a deployment document, pools fanned out to members with placement at volume creation, and `PersistentVolumeOps` `Relocate` for moving one volume to another member. Planned and not taught here as present: `StorageClusterOps` `RemoveMember`, which would move a member\'s volumes out and delete it, and pool QoS ceilings shared across members by demand.', kind: 'info' },
        'What differs from the [sixteen-node chapter](chapter.html?c=openshift-sixteen-nodes) is the count, and the count is of storage nodes rather than workers. A worker runs one storage node per slot, and the slots come from `socketsToUse` and `nodesPerSocket`: 32 workers of one slot are exactly the limit, and a fleet that uses two sockets per worker gets 16 workers per member and needs six members for 96 workers.',
      ],
    },
    {
      heading: 'Before discovery: the control plane and the labels',
      body: [
        'The SCC bindings, the namespace labels, the operator install, and the SimplyblockDriver are chapter 1\'s and are not repeated. Two things are done differently before the run, and both are about scale.',
        'The first is where FoundationDB sits. Adding a storage node reboots its worker, and the node controller adds workers `nodeProvisioningBudget` at a time, except that a worker hosting a FoundationDB pod is always added on its own, whatever the budget says. Left to land anywhere, the three coordinators would put three of the 96 workers on a serial path. `spec.source.local.foundationDB.nodeSelector` confines the database\'s own processes, and nothing else the control plane runs, to the infrastructure nodes, which carry no NVMe devices and are never drafted as storage nodes. The selector has to match at least as many workers as `replicas`, because the worker is the database\'s fault domain.',
        'The second is the label. Workers carrying one value of `storage.simplyblock.io/placement-group` form one node set named by the value, and the discovery generator neither counts, sizes, nor deals those workers. The value has to be a node set name: a DNS label of at most 23 characters, so that the member name built from it stays a DNS label. A value that is not one is reported as `PlacementGroupInvalid`, naming the workers, and they are drafted as unlabeled workers are.',
        { code: 'for n in $(seq -f "worker-%03g" 1 32); do oc label node "$n" storage.simplyblock.io/placement-group=pg-a; done\nfor n in $(seq -f "worker-%03g" 33 64); do oc label node "$n" storage.simplyblock.io/placement-group=pg-b; done\nfor n in $(seq -f "worker-%03g" 65 96); do oc label node "$n" storage.simplyblock.io/placement-group=pg-c; done', title: 'Assigning the workers to three placement groups' },
        'The label is optional, and without it the generator splits the fleet itself. With `s` slots per worker a member holds at most `floor(32 / s)` workers, `n` workers need `ceil(n / c)` members, and they are sized within one worker of each other. Workers are sorted by failure domain, then by name, and dealt round-robin, so every zone present is spread over every member. The node sets are numbered, `discovered-1`, `discovered-2`, and so on, and a tier spanning regions carries the region before the number. The label exists for the administrator who wants to choose which workers share a member, by rack or by purchase batch, rather than accept the deal.',
        { table: { head: ['Workers', 'Slots per worker', 'Members', 'Workers per member'], rows: [
          ['32', '1', '1', '32'],
          ['33', '1', '2', '17, 16'],
          ['96', '1', '3', '32, 32, 32'],
          ['97', '1', '4', '25, 24, 24, 24'],
          ['40', '2', '3', '14, 13, 13'],
        ] } },
        'Labeling only some workers of a partition is not refused. The labeled ones form their node sets, the unlabeled ones are split by size, and the draft carries `PlacementGroupLabelIncomplete` naming them, because the draft is a proposal for a reviewer to correct and labeling the rest and running discovery again is the correction.',
      ],
      examples: [
        { title: 'The ControlPlane, with FoundationDB confined', file: 'examples/openshift-placement-groups/01-controlplane.yaml', notes: [
          { at: 'replicas: 3', text: 'Three coordinators, the default. The selector below has to match at least this many workers.' },
          { at: 'node-role.kubernetes.io/infra', text: 'The database\'s processes land on the infrastructure nodes alone. The management API and the object store follow the control plane\'s own placement, not this selector.' },
          { at: 'operator: Exists', text: 'Tolerations here replace the control plane\'s for the database rather than adding to them, so the infra taint is tolerated by the database alone.' },
        ] },
      ],
    },
    {
      heading: 'Discovery: one run, three node sets',
      body: [
        'The run is the `OperatorOps` of chapter 1 with nothing added for superclusters: it reads the labels off the workers it inspects. Control-plane nodes are left out by default, as there, and the infrastructure nodes are inspected and refused because they hold no NVMe device, so neither reaches the draft. `configName` names the draft, and leaving it out names one after the run\'s timestamp so that a second run never overwrites a reviewed first one.',
        'Before the labels are read, the workers are partitioned by what a member must share: the `topology.kubernetes.io/region` label, the management interface name, the data interfaces, and the device class. A member never spans two regions, because it stripes every write synchronously across its own nodes and a member spanning regions puts inter-region latency in every write. A label value whose workers fall in two partitions cannot be one member, so each partition\'s share becomes a numbered node set and the draft carries `PlacementGroupInvalid` saying so. A worker without the region label matches any region, so an on-premises fleet that sets no topology labels is one region.',
        'The zone label is read as the failure domain and, without placement-group labels, is what the dealing spreads over every member. Failure domains are the [next chapter](chapter.html?c=failure-domains), and this fleet carries no zone labels, so the draft leaves `enableFailureDomains` unset.',
        { code: 'oc get oops prod -n simplyblock -w\noc get cdc discovered-prod -n simplyblock -o jsonpath=\'{.status.message}\'', title: 'Following the run' },
        'The run\'s status summarizes what it found in one sentence, ending in the node set count: `96 workers with 192 NVMe devices, in 3 group(s) across 3 node set(s)`, followed by the refusals. Three node sets is the line to look for, because three node sets is what makes the draft a supercluster document.',
      ],
      examples: [
        { title: 'The discovery run', file: 'examples/openshift-placement-groups/02-operatorops-discover.yaml', notes: [
          { at: 'action: Discover', text: 'The same run as in chapter 1. The placement groups come from the node labels, and nothing in the run names them.' },
          { at: 'configName: discovered-prod', text: 'The draft\'s name. Unstated, the run names it after its own timestamp.' },
          { at: 'enablePartitionedDevices: true', text: 'Reports disks with a stale partition table, which reused machines carry. The infra nodes have no NVMe devices at all and are refused regardless.' },
        ] },
      ],
    },
    {
      heading: 'Reviewing the draft: one template, three node sets',
      body: [
        'The draft is one document: `spec.cluster`, the template, and `spec.nodeSets`, one per label value. The template applies to every member, because sizing is uniform within a cluster and stays uniform within each member, and `spec.cluster.name` becomes the supercluster\'s name. A node set may override the template for its own member in `nodeSets[].cluster`: `vcpuCount`, `maxSubsystemCount`, `minHugePagesSize`, `socketsToUse`, `nodesPerSocket`, and `stripe`, for a node set whose hardware differs from the rest. An unset field inherits the template\'s value, and overrides are admitted only on a document with more than one node set or one naming a cluster in `clusterRef`, because a single standalone cluster\'s template already states its sizing.',
        'What used to span the whole document now spans one node set, since each node set becomes a cluster of its own. Two node sets may differ in `mgmtInterface`, `dataInterfaces`, and device class. Two groups within one node set that disagree remain a finding, and a node set mixing NVMe and logical block groups is refused by the schema.',
        { table: { head: ['Finding', 'Raised when', 'Fix'], rows: [
          ['`MemberTooLarge`', 'A node set\'s workers times its slots exceed 32', 'Split the node set, or use fewer sockets per worker'],
          ['`MemberNameInvalid`', '`<cluster>-<nodeset>-<suffix>` is not a DNS label of at most 63 characters', 'Shorten the node set\'s name or the cluster\'s'],
          ['`MemberSpansRegions`', 'A node set lists workers in two values of `topology.kubernetes.io/region`', 'Move the workers so each node set is in one region'],
          ['The stripe minimum', 'A node set holds fewer storage nodes than its scheme needs, eight for `4+2`', 'Add workers to the node set or narrow the stripe, per member'],
          ['`WorkerNotFound`', 'Two groups of one node set disagree on interfaces', 'Make the groups of a node set agree, or split them into node sets'],
        ] } },
        'These checks run on a hand-written document as much as on a draft, so a document authored without labels, three node sets typed by hand, is held to the same rules.',
        { steps: [
          'Read `status.message`. A valid draft says `the document is valid and is waiting for spec.approved`, and the `AwaitingApproval` event is raised once on that transition. Anything else is the first of the findings, each of which was also raised as a Warning event naming its reason.',
          'Count the node sets and their workers. Three node sets of 32 is the layout the labels asked for, and a worker missing from all of them is one the run refused, with the reason on the run.',
          'Set `cluster.name`. It becomes the supercluster\'s name, and every member is named after it: `prod` and `pg-a` make a member `prod-pg-a-<suffix>`.',
          'Choose `nodeProvisioningBudget`. It bounds how many workers of one member are in the node-add process at once, and the chapter sets four.',
          'Set `approved: true`. The document is immutable from that moment.',
        ] },
      ],
      examples: [
        { title: 'The reviewed draft', file: 'examples/openshift-placement-groups/03-clusterdeploymentconfig.yaml', notes: [
          { at: 'name: prod', text: 'The supercluster\'s name. The members are `prod-pg-a-<suffix>`, `prod-pg-b-<suffix>`, and `prod-pg-c-<suffix>`, and the default pool and class are `prod-default` and `simplyblock-prod`.' },
          { at: 'parityChunks: 2', text: '`4+2` needs eight storage nodes, checked per node set. Every member here has 32, and a node set may override the stripe for its own member.' },
          { at: 'nodesPerSocket: 1', text: 'One socket, one node per socket: one storage node per worker, so 32 workers are 32 storage nodes, the limit exactly.' },
          { at: 'nodeProvisioningBudget: 4', text: 'Four workers of a member in node-add at once. Members start their first adds one member at a time, so four is also the fleet-wide figure during expansion.' },
          { at: 'name: pg-a', text: 'A node set per label value, named by the value. It is copied into every node\'s `spec.nodeSet`, so a node names the part of the document it came from.' },
          { at: 'through worker-032', text: 'The real document lists every worker and every device by PCI address. A document whose meaning depends on what the hardware turns out to be is not one a reviewer can approve.' },
          { at: 'name: pg-b', text: 'The second member. It may differ from `pg-a` in interfaces and device class, and may carry a `cluster` block overriding the template for its own hardware.' },
        ] },
      ],
    },
    {
      heading: 'What the operator writes: the supercluster and its members',
      body: [
        'The document moves `Draft`, `Expanding`, `Expanded` through the same steps as chapter 1, and each step does more than it did there.',
        { table: { head: ['Step', 'With three node sets'], rows: [
          ['`Validating`', 'Every check of the table above, per node set'],
          ['`CreatingCluster`', 'Creates the supercluster `prod`, with `spec.supercluster` empty, then draws a name for every member and records all three in the document\'s `status.members` before creating any, then creates the members, each with `superclusterRef: prod`'],
          ['`AwaitingCluster`', 'Waits for every member\'s `status.uuid`. A member holds until the supercluster reports the shared Services and certificate ready'],
          ['`CreatingNodes`', 'One StorageNode per worker per slot, per member, in node-set order: the next member\'s nodes are not created until every node of the earlier member is `Online`'],
          ['`Activating`', 'One `StorageClusterOps` `Activate` per member, named after the document and the member'],
        ] } },
        'The supercluster is created first because a member names it in an immutable field that admission resolves. The names are recorded before the members exist so that a crash between two creates resumes with the same names and creates no second member. A member\'s suffix is three random lowercase letters, drawn afresh, never reused, and distinct from every other member\'s, because it tells members apart in node hostnames. Each creation is a `ClusterCreated` event on the document: `Created supercluster prod`, then `Created member prod-pg-a-kqv of supercluster prod from node set pg-a`.',
        'Members start their first node adds one at a time for a reason specific to OpenShift. The API\'s description of `openshift.machineConfigPool` says that adding a node creates a machine-config pool of the cluster\'s own and moves the worker into it, and the first add on a worker reboots it to apply the CPU topology. A pool limits how many of its own workers reboot at once and knows nothing of the other pools, so three members adding their first nodes together would reboot workers of all three at once. The expansion therefore creates a member\'s nodes only once every earlier member\'s nodes are online, raising `AwaitingNodes` on the document while it waits, and `CreatingNodes` is given an activation\'s worth of time for each member before the last. What the node agent does on the worker during an add is host-side behavior outside this repository and is not described here.',
        'Within a member, `nodeProvisioningBudget` is the bound, and this chapter\'s four means at most four workers in node-add at any moment through all 96 adds. The expansion does not wait for the last member\'s nodes to come up: it created the objects and is `Expanded`, and the supercluster\'s own phase is what reports the fleet\'s progress.',
        { table: { head: ['Members', 'Supercluster phase'], rows: [
          ['No member yet, or any member `Pending`, `Creating`, `Provisioning`, or `Activating`', '`Provisioning`'],
          ['Every member `Online`', '`Online`'],
          ['At least one member serving, at least one not `Online`', '`Degraded`'],
          ['No member serving, at least one `Suspended`, none `Unavailable`', '`Suspended`'],
          ['No member serving, otherwise', '`Unavailable`'],
        ] } },
        'Serving is `Online`, `Degraded`, `Rebalancing`, or `Shrinking`. The supercluster\'s status also carries `members[]` with each member\'s name, UUID, phase, whether placement may choose it, and its capacity reading, `memberCount`, `capacity` as the sums of the members\' total, used, and provisioned bytes, and `maxFaultTolerance` as the lowest among the members. Its `status.uuid` is its own `metadata.uid`, because no backend cluster exists for it. The supercluster raises `MemberAdded` when a member first appears in its status and `MemberPlacementChanged` when a member leaves placement or returns, with the reason: it is `Suspended`, it does not hold a pool yet, or placement is disabled on it.',
        { code: 'oc get stc -n simplyblock\noc get stc prod -n simplyblock -o jsonpath=\'{.status.members[*].name}\'\noc get cdc discovered-prod -n simplyblock -o jsonpath=\'{.status.members}\'', title: 'The clusters after expansion' },
        'The members share two Services and one serving certificate, owned by the supercluster: `sb-prod-node-api` and `sb-prod-spdk-proxy`, with their `-tls` Secrets. Each member\'s own workload, its DaemonSet, ConfigMaps, and EndpointSlices, is named for the member, and a node\'s hostname carries the member\'s suffix so that one worker could serve several members. Workers shared between members are not built today: a worker appears in one node set across the whole document.',
      ],
      examples: [
        { title: 'The supercluster, as it reads after an administrator tuned it', file: 'examples/openshift-placement-groups/04-storagecluster-supercluster.yaml', notes: [
          { at: 'supercluster:', text: 'The operator wrote this block empty. Its presence is fixed at creation, and the webhook refuses every spec field beside it and the two thresholds, naming the field.' },
          { at: 'strategy: MostAvailableCapacity', text: 'The default when unset, and mutable afterward. `LeastProvisioned` ranks by the ratio of provisioned to physical capacity, `LeastVolumes` by volume count.' },
          { at: 'warningThreshold:', text: 'Applies to the combined capacity and raises `AggregateCapacityThreshold`. Each member keeps its own thresholds, and a member above its own critical threshold is skipped at placement.' },
        ] },
        { title: 'One member, as the expansion writes it', file: 'examples/openshift-placement-groups/05-storagecluster-member.yaml', notes: [
          { at: 'name: prod-pg-a-kqv', text: '`<cluster>-<nodeset>-<suffix>`. The suffix was drawn, recorded in the document\'s `status.members`, and is never reused by a later member of the same node set name.' },
          { at: 'superclusterRef: prod', text: 'Membership, set at creation and immutable. The supercluster had to exist first, and a reference to a standalone cluster is refused.' },
          { at: 'maxSubsystemCount: 50', text: 'The template\'s sizing, or the node set\'s override. A member carries a full spec, and the supercluster carries none.' },
          { at: 'nodeProvisioningBudget: 4', text: 'Per member. The node controller of this member adds four of its workers at once, after the earlier member\'s nodes are online.' },
          { at: 'machineConfigPool: worker', text: 'Resolved from `environment: OpenShift`, with the kubelet configuration and the CPU topology. The member\'s own pool inherits from this role.' },
        ] },
      ],
    },
    {
      heading: 'Pools, classes, and where a volume lands',
      body: [
        'Once the supercluster reconciles it writes a default pool `prod-default`, and the pool controller writes the class `simplyblock-prod` for it, naming the supercluster\'s `status.uuid` in `cluster_id` as it names a cluster\'s UUID for an ordinary cluster. Every member is an ordinary cluster and writes its own default pool and class as well, `prod-pg-a-kqv-default` and `simplyblock-prod-pg-a-kqv`. A claim against a member\'s class lands in that member and never reaches placement. A claim against the supercluster\'s class is placed.',
        'A pool whose `clusterRef` names the supercluster is created in every member under the pool\'s name. Its `status.uuid` stays empty and `status.members[]` records each member\'s backend pool UUID and phase, and the pool is `Ready` once every placement-eligible member holds it. `limits.capacity` is the pool\'s ceiling across all members: the operator writes each member\'s backend pool an even share of it, with the remainder going one unit at a time to the first members in name order, and a member out of placement keeps the share its backend pool enforces, because its volumes stay and may grow. `limits.iops` and `limits.throughput` are refused on every pool today, supercluster or not. `allowedNodes` reaches every member\'s backend pool whole, since a volume of the pool may be placed in any member and consumed from any allowed node.',
        'At `CreateVolume`, the CSI controller finds that the class\'s `cluster_id` names no cluster it knows and asks the operator over the link it already holds. The operator answers in this order.',
        { steps: [
          'A placement already recorded on the PVC, in the annotation `storage.simplyblock.io/placement-member`, is the answer, so a retry goes back to the member the create was sent to and nowhere else. Without a record, the volume\'s name is looked up in every member\'s pool, and a hit is recorded and returned.',
          'A PVC labeled with a consistency group is placed in the member holding that group, because a group lives on one logical volume store of one node.',
          'The candidates are the placement-eligible members: serving, not `disablePlacement`, and holding every pool of the supercluster.',
          'When the request carries a preferred topology, members in the preferred segment\'s region are kept, then the requisite segments\' regions, and every eligible member only when neither leaves one with room.',
          'Members without room for the requested size, counting the sizes placed since the last capacity reading, and members above their own `criticalThreshold` are dropped.',
          'The rest are ranked by the strategy, and ties are broken at random. The chosen member is recorded on the PVC before the answer goes out.',
        ] },
        { table: { head: ['Strategy', 'Picks the member with'], rows: [
          ['`MostAvailableCapacity`', 'the most free physical capacity, the default'],
          ['`LeastProvisioned`', 'the lowest ratio of provisioned to physical capacity'],
          ['`LeastVolumes`', 'the fewest volumes, counted from the PersistentVolumes whose handle names it plus the placements not yet backed by one'],
        ] } },
        'No candidate left fails the create with `ResourceExhausted`, naming the reason each member was dropped, and the supercluster raises `AggregateCapacityExhausted`, at most once a minute. The driver writes the chosen member\'s UUID into the volume handle, and from then on the volume is the member\'s: expansion, snapshots, and clones stay inside it, and a clone lands in its source\'s member whatever the class says. Moving one volume to another member is `PersistentVolumeOps` with action `Relocate`, refused for an encrypted volume or one in a consistency group. Setting `disablePlacement: true` on a member stops new volumes landing in it and leaves everything already there where it is.',
        { callout: 'A `StorageClusterOps` naming the supercluster fails before any control-plane call, because the supercluster has no backend cluster. Operations are written against a member: `prod-pg-a-kqv`, not `prod`.', kind: 'warn' },
      ],
      examples: [
        { title: 'A pool on the supercluster', file: 'examples/openshift-placement-groups/06-storagepool.yaml', notes: [
          { at: 'clusterRef: prod', text: 'Naming the supercluster creates the pool in every member under this name. The class the pool controller writes for it names the supercluster, so its claims are placed.' },
          { at: 'capacity: 300T', text: 'The ceiling across all members: 100T is written to each of three members accepting volumes. A fourth member changes the shares to 75T each.' },
          { at: 'filesystem: xfs', text: 'Volume defaults are the same in every member. The block is immutable once set, because the class\'s parameters are.' },
        ] },
      ],
    },
    {
      heading: 'Growing: a fourth placement group',
      body: [
        'Growth is a second document naming an existing cluster in `spec.clusterRef`, and what it names decides what it grows.',
        { table: { head: ['`clusterRef` names', 'The expansion'], rows: [
          ['A standalone cluster', 'Adds every node set\'s nodes to it, within 32 storage nodes'],
          ['The supercluster', 'Creates one new member per node set, each named `<supercluster>-<nodeset>-<suffix>` and naming the supercluster'],
          ['A member', 'Adds every node set\'s nodes to that member, within 32 storage nodes counting the ones it has'],
        ] } },
        'A document adding members states their sizing in `spec.cluster` as a creating document does, and a document naming the supercluster without it is refused with `MemberSizingUnstated`. The schema requires `cluster.name` and the expansion does not use it, because the supercluster already has one. Node sets may carry overrides, since the document names a cluster in `clusterRef`. The new member is checked as a creation: its own storage-node limit, its own stripe minimum, and a fresh suffix distinct from every suffix the supercluster\'s members already carry, including members another document added.',
        'A document naming a member instead counts the nodes the member already holds: eight workers added to a member of thirty are refused with `MemberTooLarge`, naming both numbers.',
        'The new member runs through the same steps alone, and once it reconciles the supercluster raises `MemberAdded`. It joins the shared Services and the existing certificate with no reissue, because the certificate names the Service and not the members, and it receives every pool of the supercluster before it is placement-eligible, so no volume lands in a member whose pool is not there yet. The capacity shares of every pool are rewritten over the members that now accept volumes.',
        'Removing a member is the part that is planned and not built. `RemoveMember` would take the member out of placement, relocate its volumes into the others, and delete it, and no operation does that today. What exists is `disablePlacement: true` to stop new volumes and `Relocate` to move them one at a time. Deleting the supercluster while members name it is refused.',
      ],
      examples: [
        { title: 'The growth document', file: 'examples/openshift-placement-groups/07-clusterdeploymentconfig-growth.yaml', notes: [
          { at: 'clusterRef: prod', text: 'Naming the supercluster makes every node set of this document a new member. Naming `prod-pg-a-kqv` instead would add the workers to that member, within its 32.' },
          { at: 'name: prod', text: 'Required by the schema and not used: the supercluster has a name. The rest of the block is the new member\'s sizing, without which the document is refused with `MemberSizingUnstated`.' },
          { at: 'name: pg-d', text: 'The fourth member is `prod-pg-d-<suffix>`, with a suffix drawn apart from `kqv` and the other two.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The draft reports `MemberTooLarge` for a node set that lists 32 workers.', cause: 'The template, or the node set\'s override, uses two sockets per worker, so 32 workers are 64 storage nodes.', fix: 'Split the node set into two of 16, or use one socket. The count is of storage nodes, and the finding names the workers a node set may list.' },
          { symptom: 'The draft has four node sets where three were intended, and `PlacementGroupLabelIncomplete` names some workers.', cause: 'Those workers carry no placement-group label while others on the same network do, so they were split by size into a node set of their own.', fix: 'Label them and run discovery again, or move them into the intended node sets by hand before approving.' },
          { symptom: '`PlacementGroupInvalid` names a value and the draft has two node sets named `pg-a-1` and `pg-a-2`.', cause: 'Workers carrying `pg-a` are in two regions or on two networks, which cannot share a member.', fix: 'Relabel so each value stays within one region and one network. The split into numbered node sets is what the draft can do with the labels as they are.' },
          { symptom: 'Approval is refused with `MemberSpansRegions`.', cause: 'A node set lists workers in two values of `topology.kubernetes.io/region`, on a draft edited by hand or on a hand-written document.', fix: 'Move the workers so each node set is in one region. A worker without the label matches any region.' },
          { symptom: 'The document stays in `CreatingNodes` with `AwaitingNodes` naming a node of the first member.', cause: 'The next member\'s nodes are not created until every node of the earlier member is `Online`, and one of them is stuck in `Provisioning`.', fix: 'Fix that node, as in chapter 1: SCCs, the device, or the reboot. The step\'s deadline includes an activation\'s worth of time per earlier member.' },
          { symptom: 'The supercluster is `Degraded` while every volume works.', cause: 'One member is not `Online`: rebalancing, degraded, or unavailable, while the others serve.', fix: 'Read `status.members[]` for the member and its phase. Placement skips a member that is not serving, and the others take new volumes.' },
          { symptom: 'A claim against `simplyblock-prod` fails with `ResourceExhausted` although a member has room.', cause: 'That member is not placement-eligible: `disablePlacement` is set, it is not serving, or it does not hold a pool of the supercluster yet, which `MemberPlacementChanged` on the supercluster said.', fix: 'Wait for the pool, or turn placement back on. The event names the reason.' },
          { symptom: 'A `StorageCluster` written with `superclusterRef` is refused at admission.', cause: 'The supercluster does not exist yet, or the name refers to a standalone cluster.', fix: 'Create the supercluster first, or let a deployment document create both. A cluster becomes a supercluster only when created with `spec.supercluster`.' },
          { symptom: 'A growth document naming `prod` is refused with `MemberSizingUnstated`.', cause: 'It carries no `spec.cluster`, and a new member needs sizing.', fix: 'State `spec.cluster` with the member\'s sizing. Its `name` is required by the schema and ignored.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'Placement groups', cat: 'concept', text: 'What is a placement group in this operator?', options: ['A member StorageCluster of a supercluster', 'A Kubernetes topology zone', 'A group of workers within a node set', 'A parameter of the generated StorageClass', 'What `status.role` reports on a member'], correct: [0, 4], explain: 'A member is one of the groups a supercluster\'s volumes are placed in, which is why its role reads `PlacementGroup`. The node label of the same name assigns a worker to one. Zones are failure domains, and groups within a node set share a configuration.' },
    { topic: 'Placement groups', cat: 'concept', text: 'Why is a cluster held to 32 storage nodes?', options: ['Its fault tolerance is fixed by the stripe while the chance of concurrent failures grows with every node', 'FoundationDB holds at most 32 nodes per cluster', 'The volume handle has room for 32 nodes', 'The control plane refuses the 33rd node', 'An OpenShift machine-config pool holds 32 workers'], correct: [0], explain: 'The limit bounds exposure, not capacity. The control plane does not enforce it: the operator does, at approval, with `MemberTooLarge`.' },
    { topic: 'Placement groups', cat: 'concept', text: 'Which statements about a supercluster are true?', options: ['It is a StorageCluster carrying `spec.supercluster`', 'It has no storage nodes of its own', 'It stripes each volume across its members', 'Its `status.uuid` is a backend cluster\'s UUID', 'Pools, StorageClasses, and backup policies may name it'], correct: [0, 1, 4], explain: 'A supercluster is one name and one status with no data path. A volume lives in one member, and the supercluster\'s `status.uuid` is its own `metadata.uid`, because no backend cluster exists for it.' },
    { topic: 'Placement groups', cat: 'concept', text: 'Ninety-six workers use two sockets each with one storage node per socket. How many members does the fleet need at least?', options: ['Three', 'Six', 'Two', 'One', 'Twelve'], correct: [1], explain: 'The count is of storage nodes. Two slots per worker make 192 nodes, and a member holds 32, so six members of 16 workers.' },
    { topic: 'Placement groups', cat: 'concept', text: 'Which of these are built today?', options: ['Expanding a document with several node sets into a supercluster', 'Pools fanned out to the members and placement at volume creation', '`PersistentVolumeOps` `Relocate` to another member', '`StorageClusterOps` `RemoveMember`', 'Pool QoS ceilings shared across members by demand'], correct: [0, 1, 2], explain: 'Phases 1 to 3 of the design are built. Removing a member and demand-driven QoS shares are planned, and the chapter does not teach them as present.' },
    { topic: 'Placement groups', cat: 'field', text: 'Which values can `status.role` on a StorageCluster take?', options: ['`Standalone`', '`Supercluster`', '`PlacementGroup`', '`Member`', '`Aggregate`'], correct: [0, 1, 2], explain: 'The enum has three values, and a member reads `PlacementGroup`. `oc get stc` shows it in the `Role` column after `Phase`.' },
    { topic: 'Labels and discovery', cat: 'field', text: 'What does the label `storage.simplyblock.io/placement-group` do on a worker?', options: ['Workers with one value become one node set named by the value', 'It names the member StorageCluster exactly', 'It restricts which workers the run inspects', 'It assigns the worker a failure domain', 'It takes those workers out of the size split'], correct: [0, 4], explain: 'The generator neither counts, sizes, nor deals a labeled partition. The member is named `<cluster>-<value>-<suffix>`, the run still inspects every schedulable worker, and the failure domain comes from the zone label or `failureDomainLabel`.' },
    { topic: 'Labels and discovery', cat: 'concept', text: 'Some workers of a partition carry the placement-group label and others do not. What happens?', options: ['The draft is refused', 'The unlabeled workers are split by size and the draft carries `PlacementGroupLabelIncomplete` naming them', 'The unlabeled workers are left out of the draft', 'The labeled workers are dealt as unlabeled ones are', 'The run fails'], correct: [1], explain: 'A partial labeling is a finding rather than a refusal: the draft is a proposal for a reviewer to correct, and labeling the rest and running discovery again is the correction.' },
    { topic: 'Labels and discovery', cat: 'field', text: 'Which of these placement-group values are reported as `PlacementGroupInvalid`?', options: ['`pg-a`', '`PG_A`', 'A value of 30 characters', '`rack7`', '`east-1`'], correct: [1, 2], explain: 'A value has to be a node set name: a DNS label of at most 23 characters, so that the member name built from it stays a DNS label. Uppercase and underscores are not a DNS label, and 30 characters are too many.' },
    { topic: 'Labels and discovery', cat: 'concept', text: 'Without labels, how does discovery draft 97 workers of one slot each?', options: ['One node set of 97', 'Four node sets of 25, 24, 24, and 24 workers', 'Three node sets of 32 and one of 1', 'It refuses the run', 'Two node sets of 48 and 49'], correct: [1], explain: '`ceil(97 / 32)` is four members, sized within one worker of each other. A node set over the limit is never drafted, and the fleet is not refused for its size.' },
    { topic: 'Labels and discovery', cat: 'concept', text: 'Which of these partition workers that cannot share a member?', options: ['The `topology.kubernetes.io/region` label', 'The management interface name', 'The data interfaces', 'The device class', 'The `topology.kubernetes.io/zone` label'], correct: [0, 1, 2, 3], explain: 'A member stays in one region and on one network with one device class. The zone is the failure domain, which the dealing spreads over every member rather than splitting by.' },
    { topic: 'Labels and discovery', cat: 'concept', text: 'Without labels, in what order are workers dealt into members?', options: ['Sorted by failure domain, then by name, round-robin', 'By name alone, even where zones are labeled', 'By free capacity', 'By rack count', 'At random'], correct: [0], explain: 'Sorting by failure domain first and dealing round-robin is what spreads every zone over every member, so a zone outage degrades each member a little rather than taking one down.' },
    { topic: 'Labels and discovery', cat: 'action', text: 'What does a discovery run need in order to produce a supercluster draft?', options: ['A field naming the supercluster', '`enableControlPlaneNodes: true`', 'One run per placement group', 'More than one node set after the split', 'An action other than `Discover`'], correct: [3], explain: 'Nothing in the run names superclusters. A draft with two or more node sets is a supercluster document, whether the node sets came from labels or from the size split.' },
    { topic: 'Labels and discovery', cat: 'field', text: 'Why does this chapter set `foundationDB.nodeSelector` on the ControlPlane?', options: ['A worker hosting a FoundationDB pod is added to its cluster on its own, whatever the budget says', 'FoundationDB cannot run on a storage worker', 'The discovery run refuses FoundationDB hosts', 'The selector has to match at least as many workers as `replicas`', 'It makes the database faster'], correct: [0, 3], explain: 'Confining the database to the infra nodes keeps the storage workers off the serial path. The selector is held to the replica count because the worker is the database\'s fault domain, and the run reads devices, not database pods.' },
    { topic: 'The draft', cat: 'field', text: 'Which fields can a node set override in `nodeSets[].cluster`?', options: ['`vcpuCount`', '`maxSubsystemCount`', '`stripe`', '`mgmtInterface`', '`nodeProvisioningBudget`'], correct: [0, 1, 2], explain: 'The overrides are `vcpuCount`, `maxSubsystemCount`, `minHugePagesSize`, `socketsToUse`, `nodesPerSocket`, and `stripe`: the sizing that varies with hardware. Interfaces are a group\'s field, and the budget is the template\'s.' },
    { topic: 'The draft', cat: 'field', text: 'When may a node set carry a `cluster` override block?', options: ['When the document has more than one node set', 'When the document names a cluster in `clusterRef`', 'On any document', 'Never on a draft the run wrote', 'Only when `environment` is `OpenShift`'], correct: [0, 1], explain: 'A single standalone cluster\'s template already states that cluster\'s sizing, so the schema admits overrides only where a node set becomes a member or grows a cluster.' },
    { topic: 'The draft', cat: 'concept', text: 'A node set lists 33 workers of one slot each. What does validation report?', options: ['`MemberTooLarge`, naming the node set and how many workers it may list', '`MemberNameInvalid`', '`MemberSpansRegions`', 'Nothing, the limit is the control plane\'s', 'The node set is split automatically'], correct: [0], explain: 'Validation refuses a node set over 32 storage nodes and says how to split it. The draft is where the split happens, and approval only checks.' },
    { topic: 'The draft', cat: 'concept', text: 'Which checks run per node set on a supercluster document?', options: ['The erasure-coding minimum', 'Groups agreeing on interfaces', 'The storage-node limit', 'The member name being a DNS label', 'The approval gate'], correct: [0, 1, 2, 3], explain: 'Each node set becomes a cluster, so what spans a cluster spans a node set. Approval is one field for the whole document.' },
    { topic: 'The draft', cat: 'field', text: 'The template names the cluster `prod`, a node set is `pg-a`, and the drawn suffix is `kqv`. What is the member called?', options: ['`prod-pg-a-kqv`', '`pg-a`', '`prod-1`', '`prod-pg-a`', '`kqv-pg-a-prod`'], correct: [0], explain: '`<cluster>-<nodeset>-<suffix>`. The suffix tells members apart in node hostnames, which is why it is drawn apart from every other member\'s.' },
    { topic: 'The draft', cat: 'concept', text: 'Which of these refuse a document at approval?', options: ['A node set listing workers in two regions', 'A worker without the region label', 'A `4+2` stripe on a node set of six workers', 'Two node sets with different management interfaces', 'Every worker carrying a zone label'], correct: [0, 2], explain: '`MemberSpansRegions` and the per-member stripe minimum refuse. An unlabeled worker matches any region, two node sets may differ in interfaces, and zone labels are what failure domains are made from.' },
    { topic: 'The draft', cat: 'field', text: 'What does `nodeProvisioningBudget` bound on a supercluster document?', options: ['How many workers of one member are in node-add at once', 'How many members are activated at once', 'How many members start their first node adds at once', 'The number of storage nodes in the supercluster', 'How many reboots a machine-config pool performs'], correct: [0], explain: 'The budget is per cluster, so per member. Members start their first adds one at a time regardless, so during expansion the budget is also the fleet-wide figure.' },
    { topic: 'Expansion', cat: 'concept', text: 'Which statements about `CreatingCluster` on a three-node-set document are true?', options: ['The supercluster is created before any member', 'Every member\'s name is recorded in `status.members` before any member is created', 'The supercluster is written with the template\'s sizing', 'A removed member\'s name is reused by the next member of that node set', 'A crash between two creates draws new names on the retry'], correct: [0, 1], explain: 'A member names its supercluster in an immutable field admission resolves, so the supercluster comes first. Names are recorded ahead so a retry creates the member it named. The supercluster carries `spec.supercluster` empty and no sizing.' },
    { topic: 'Expansion', cat: 'concept', text: 'Why are members\' first node adds run one member at a time?', options: ['Each member\'s machine-config pool reboots its workers without knowing the other pools', 'FoundationDB admits one cluster at a time', 'The control plane serializes cluster creation', 'The CSI driver needs one member online first', 'The budget is counted over the whole supercluster'], correct: [0], explain: 'Three members adding their first nodes together would reboot workers of all three at once. The expansion starts a member\'s nodes only once every earlier member\'s nodes are online.' },
    { topic: 'Expansion', cat: 'concept', text: 'Two members are `Online` and the third is still `Provisioning`. What is the supercluster\'s phase?', options: ['`Provisioning`', '`Degraded`', '`Online`', '`Pending`', '`Unavailable`'], correct: [0], explain: 'Any member not yet activated keeps the supercluster `Provisioning`. `Degraded` is for a member that was activated and is not `Online` now.' },
    { topic: 'Expansion', cat: 'concept', text: 'Two members are `Online` and the third is `Unavailable`. What is the supercluster\'s phase?', options: ['`Degraded`', '`Unavailable`', '`Online`', '`Suspended`', '`Provisioning`'], correct: [0], explain: 'At least one member serving and at least one not `Online` is `Degraded`. The unavailable member\'s volumes are unavailable, and the others are unaffected.' },
    { topic: 'Expansion', cat: 'field', text: 'Which of these does a supercluster\'s status carry?', options: ['`members[]` with each member\'s phase and eligibility', '`memberCount`', '`capacity` as sums over the members', '`maxFaultTolerance` as the lowest among the members', '`uuid` equal to its own `metadata.uid`'], correct: [0, 1, 2, 3, 4], explain: 'Every field is reconstructable from the members and the control plane. The UUID is the aggregate\'s identifier, which the generated class names in `cluster_id`.' },
    { topic: 'Expansion', cat: 'concept', text: 'Which objects do the members of `prod` share?', options: ['The Services `sb-prod-node-api` and `sb-prod-spdk-proxy`', 'One serving certificate', 'One DaemonSet', 'One machine-config pool', 'One default pool'], correct: [0, 1], explain: 'The Services and the certificate are the supercluster\'s. Each member has its own DaemonSet, its own machine-config pool on OpenShift, and its own default pool.' },
    { topic: 'Expansion', cat: 'concept', text: 'Which events land on the supercluster object?', options: ['`MemberAdded`', '`MemberPlacementChanged`', '`AggregateCapacityThreshold`', '`AggregateCapacityExhausted`', '`MemberRemoved`'], correct: [0, 1, 2, 3], explain: '`MemberRemoved` belongs to `RemoveMember`, which is planned. The other four exist, and `MemberPlacementChanged` says why a member left placement.' },
    { topic: 'Pools and placement', cat: 'concept', text: 'A StoragePool names the supercluster in `clusterRef`. Which statements are true?', options: ['It is created in every member under its name', 'Its `status.uuid` stays empty and `status.members[]` records each member\'s pool', 'It is `Ready` once every placement-eligible member holds it', 'Its capacity limit is split evenly among the members accepting volumes', 'Its IOPS limit is split among the members'], correct: [0, 1, 2, 3], explain: 'The pool fans out and the ceiling is the pool\'s across all members. `limits.iops` and `limits.throughput` are refused on every pool today.' },
    { topic: 'Pools and placement', cat: 'field', text: 'Which placement strategies exist?', options: ['`MostAvailableCapacity`', '`LeastProvisioned`', '`LeastVolumes`', '`RoundRobin`', '`Nearest`'], correct: [0, 1, 2], explain: 'Three strategies, with `MostAvailableCapacity` the default. Ties are broken at random, and a request\'s topology narrows the candidates before the strategy ranks them.' },
    { topic: 'Pools and placement', cat: 'concept', text: 'Which of these make a member ineligible for placement?', options: ['`disablePlacement: true`', 'Not holding a pool of the supercluster yet', 'Phase `Suspended`', 'Phase `Degraded`', 'Holding fewer volumes than the others'], correct: [0, 1, 2], explain: 'Eligible is serving, not `disablePlacement`, and holding every pool. `Degraded` is a serving phase, and the volume count is what `LeastVolumes` ranks by, not a filter.' },
    { topic: 'Pools and placement', cat: 'concept', text: 'The external-provisioner retries a `CreateVolume` whose first attempt timed out. Where does the retry go?', options: ['The member recorded on the PVC', 'Wherever a fresh ranking points', 'The first member by name', 'A member chosen at random', 'Nowhere, the retry fails'], correct: [0], explain: 'The chosen member is written to the PVC\'s `storage.simplyblock.io/placement-member` annotation before the first answer, so a retry never places the volume a second time elsewhere.' },
    { topic: 'Pools and placement', cat: 'concept', text: 'A claim names `simplyblock-prod-pg-a-kqv`, a member\'s own class. What happens?', options: ['The volume lands in that member without placement', 'The claim is refused', 'The volume is placed by the strategy', 'The volume spreads across members', 'The volume is relocated afterward'], correct: [0], explain: 'Every member is an ordinary cluster with a default pool and class of its own. Only a class naming the supercluster reaches placement.' },
    { topic: 'Pools and placement', cat: 'concept', text: 'Which operations stay inside the member a volume was placed in?', options: ['Expansion', 'Snapshots', 'Clones', '`Relocate`', 'None of them'], correct: [0, 1, 2], explain: 'The handle names the member and every later call reaches it directly. A clone lands in its source\'s member whatever the class says. `Relocate` is the one operation that crosses members.' },
    { topic: 'Pools and placement', cat: 'action', text: 'Every member of `prod` is above its critical threshold and a claim arrives. Which of these does the operator do?', options: ['Places the volume in the member with the most volumes', 'Creates a fourth member', 'Expands a member\'s capacity', 'Moves volumes between members to make room', 'Answers from the last capacity reading'], correct: [], explain: 'None of them. The create fails with `ResourceExhausted`, naming the reason each member was dropped, and the supercluster raises `AggregateCapacityExhausted`.' },
    { topic: 'Pools and placement', cat: 'action', text: 'A `StorageClusterOps` with action `RollingRestart` names `prod`, the supercluster. Which of these does it do today?', options: ['Restarts the members one at a time', 'Restarts the members in parallel', 'Activates every member', 'Expands every member', 'Moves volumes between members'], correct: [], explain: 'None of them. It fails before any control-plane call, because the supercluster has no backend cluster. The walk over members is in the design and not built, and operations target a member.' },
    { topic: 'Growth', cat: 'concept', text: 'A growth document names the supercluster in `clusterRef`. What does the expansion do?', options: ['Creates one new member per node set', 'Adds the nodes to the largest member', 'Refuses the document', 'Merges the node sets into the existing members', 'Re-creates the supercluster'], correct: [0], explain: 'What `clusterRef` names decides what grows: a standalone cluster or a member takes nodes, and a supercluster takes members.' },
    { topic: 'Growth', cat: 'field', text: 'The growth document naming `prod` has no `spec.cluster`. What happens?', options: ['It is refused with `MemberSizingUnstated`', 'The sizing is inherited from the supercluster', 'The sizing is inherited from the first member', 'The sizing is read from the original document', 'The schema refuses it before the operator sees it'], correct: [0], explain: 'A new member needs sizing, and the supercluster has none to inherit. The schema admits the document, and validation refuses it naming the field.' },
    { topic: 'Growth', cat: 'concept', text: 'Which statements about the fourth member are true once it is created?', options: ['Its suffix is drawn apart from every suffix the other members carry', 'It receives every pool of the supercluster before it is placement-eligible', 'It joins the shared certificate without a reissue', 'The supercluster raises `MemberAdded` for it', 'It takes new volumes before its pools exist'], correct: [0, 1, 2, 3], explain: 'A member lacking a pool is not eligible, so no volume lands where its pool is not. The certificate names the Service and not the members.' },
    { topic: 'Growth', cat: 'action', text: 'Which of these exist today to take a member out of use?', options: ['`disablePlacement: true`, which stops new volumes landing in it', '`PersistentVolumeOps` `Relocate`, which moves one volume to another member', '`StorageClusterOps` `RemoveMember`, which moves every volume out and deletes the member', 'Deleting the supercluster with its members', 'Clearing `superclusterRef` on the member'], correct: [0, 1], explain: '`RemoveMember` is planned. Deleting a supercluster its members name is refused, and `superclusterRef` is immutable.' },
    { topic: 'Growth', cat: 'concept', text: 'A document names member `prod-pg-a-kqv` in `clusterRef` and adds eight workers of one slot. The member holds thirty storage nodes. What happens?', options: ['It is refused with `MemberTooLarge`, naming both numbers', 'Two workers are added and six refused', 'A new member is created for the eight', 'The workers are added and the member exceeds 32', 'The limit does not apply to growth'], correct: [0], explain: 'Growth counts the nodes a cluster already has, and a document that would take it past 32 is refused whole. The remedy is a document naming the supercluster, which makes a new member.' },
  ],

  tasks: [
    {
      title: 'The supercluster, tuned',
      scenario: 'Write the supercluster `prod` so that a new volume goes to the member holding the fewest volumes, and so that the combined capacity warns at 80 percent used.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: prod\n  namespace: simplyblock\nspec:\n  {{marker}}:\n    placement:\n      strategy: {{strategy}}\n  warningThreshold:\n    capacity: {{warn}}\n',
      blanks: {
        kind: { accept: ['StorageCluster'], hint: 'a supercluster is the same kind as its members' },
        marker: { accept: ['supercluster'], hint: 'the block whose presence makes the cluster a supercluster' },
        strategy: { accept: ['LeastVolumes'], hint: 'ranks by volume count' },
        warn: { accept: ['80'], hint: 'the threshold applies to the combined capacity' },
      },
      explain: 'A supercluster is a `StorageCluster` carrying `spec.supercluster`, which the webhook admits beside the two thresholds and nothing else. `LeastVolumes` picks the member holding the fewest volumes, and `warningThreshold` applies to the members\' combined capacity.',
    },
    {
      title: 'A member out of placement',
      scenario: 'Member `prod-pg-c-hwd` is being emptied. Edit it so that no new volume lands in it while the volumes already there keep working.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: StorageCluster\nmetadata:\n  name: {{name}}\n  namespace: simplyblock\nspec:\n  superclusterRef: {{ref}}\n  disablePlacement: {{disable}}\n  maxSubsystemCount: 50\n  vcpuCount: 8\n',
      blanks: {
        name: { accept: ['prod-pg-c-hwd'], hint: 'the member, `<cluster>-<nodeset>-<suffix>`' },
        ref: { accept: ['prod'], hint: 'the supercluster, immutable since creation' },
        disable: { accept: ['true'], hint: 'valid only on a member' },
      },
      explain: 'Placement is turned off on the member, never on the supercluster: `disablePlacement` is admitted only beside `superclusterRef`. The member keeps its volumes, its pools, and its share of every pool\'s ceiling, and `RemoveMember`, which would empty it, is not built.',
    },
    {
      title: 'A fourth placement group',
      scenario: 'Thirty-two more workers were labeled `pg-d`. Write the document that adds them to the supercluster `prod` as a new member with the same sizing as the others.',
      yaml: 'apiVersion: storage.simplyblock.io/v1alpha2\nkind: {{kind}}\nmetadata:\n  name: grow-prod-pg-d\n  namespace: simplyblock\nspec:\n  approved: {{approved}}\n  environment: OpenShift\n  clusterRef: {{ref}}\n  cluster:\n    name: prod\n    maxSubsystemCount: 50\n    vcpuCount: 8\n    stripe:\n      dataChunks: 4\n      parityChunks: 2\n  nodeSets:\n    - name: {{nodeset}}\n      groups:\n        - name: default\n          workers: [worker-097, worker-098, worker-099]\n          mgmtInterface: br-ex\n          dataInterfaces: [ens2f0]\n          devices:\n            nvme: ["0000:c1:00.0"]\n',
      blanks: {
        kind: { accept: ['ClusterDeploymentConfig'], hint: 'growth is a second document, short name `cdc`' },
        approved: { accept: ['true'], hint: 'the review gate' },
        ref: { accept: ['prod'], hint: 'naming the supercluster makes every node set a new member' },
        nodeset: { accept: ['pg-d'], hint: 'the member is named after it' },
      },
      explain: 'A `ClusterDeploymentConfig` naming the supercluster in `clusterRef` creates one member per node set, sized by `spec.cluster`, whose `name` the schema requires and the expansion ignores. The node set `pg-d` makes the member `prod-pg-d-<suffix>`.',
    },
  ],
});
