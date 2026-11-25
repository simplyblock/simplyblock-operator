// Chapter 18: disaster recovery across two sites with asynchronous replication
// and a third site as the arbiter. The third site runs the Open Cluster
// Management hub with Ramen's hub operator and, in this chapter's layout, the
// shared simplyblock control plane. It holds the DR intent (DRCluster,
// DRPolicy, DRPlacementControl) and drives the csi-addons VolumeReplication
// path on the two sites when a person sets an action.
//
// The commissioned word "arbiter" is adjusted to the code in the first section:
// nothing in the repository judges a site lost for asynchronous replication, and
// nothing fences it. The chapter builds on chapter async-dr, which teaches the
// same two sites without the hub, and is a lab procedure on a preview path
// because the Ramen-driven asynchronous flow has not been validated end to end.
TRAINING.register({
  id: 'async-dr-arbiter',
  title: 'DR with an arbiter across two sites with asynchronous replication',
  summary: 'Two sites replicating asynchronously and a third site that runs the OCM hub, Ramen\'s hub operator, and the shared control plane: the classes on each site, the DRCluster, DRPolicy, and DRPlacementControl on the hub, and the relocate and failover a person starts from there.',
  goal: 'after the chapter the reader can set up the three sites, write the StorageClass and VolumeReplicationClass Ramen pairs across the two sites and the DR objects on the hub, start a relocate or a failover, and say what the third site decides, what it does not, and which parts of the path are not yet validated.',

  sections: [
    {
      heading: 'What the arbiter is here, and what it is not',
      body: [
        'The commission asked for DR "with an arbiter across two sites with asynchronous replication." In the code, the only third party that takes part in asynchronous replication between two sites is an Open Cluster Management (OCM) hub running Ramen\'s hub operator. This chapter calls that third site the arbiter and adjusts the word to what it does: it **orchestrates and holds the DR intent**, and it does not judge. It keeps the DRClusters, the DRPolicy, and one DRPlacementControl per application, and when a person sets an action it drives the demote, the promote, and the restore of the application\'s claims on the two sites. In the topology of the Ramen integration design the two sites share one simplyblock control plane, and this chapter places that control plane on the third site as well, for the reason the sixth section gives.',
        { table: { head: ['Site', 'What runs there', 'What is written there'], rows: [
          ['site-a', 'the operator (`managed` profile), the CSI driver with its csi-addons sidecar, the csi-addons controller-manager, Ramen\'s DR cluster operator, a StorageCluster', 'the ReplicationPair and ReplicationPolicy toward site-b, the StorageClass, the VolumeReplicationClass, and the application with its claims'],
          ['site-b', 'the same', 'the reverse pair and policy, a StorageClass and a VolumeReplicationClass of the same names'],
          ['the hub, the third site', 'the OCM hub, Ramen\'s hub operator, the simplyblock control plane', 'two DRClusters, the DRPolicy, the OCM Placement, and the DRPlacementControl'],
        ] } },
        'Four statements bound everything else in the chapter:',
        { list: [
          '**A person decides failover.** Ramen never fails over by itself. It acts when a person sets `spec.action` on a DRPlacementControl to `Failover` or `Relocate`, and nothing in this repository writes that field.',
          '**No witness and no quorum.** For asynchronous replication there is no two-site witness and no quorum. Nothing on the hub or on the sites decides which site is alive. The hub reaches both sites through OCM and carries out what it is told.',
          '**No fencing of the lost site.** The fence a DRCluster can ask for is meant for sync (Metro) DR, and the CSI driver\'s NetworkFence service fences only one site of a stretched sync cluster. A lost asynchronous site is not fenced by anything in this repository.',
          '**A preview path.** The Ramen-driven asynchronous path has not been validated end to end in this repository. The csi-addons replication design records the validation against a Ramen VolumeReplicationGroup in async mode as outstanding, and the Ramen test plan lists its five manual scenarios as blocked on a live hub. Comments in the driver record live relocate and fail-back runs that found and fixed defects, which shows the path has been exercised, not that it has passed. Read this chapter as a lab procedure.',
        ] },
        'The chapter builds on [Disaster recovery with asynchronous replication across two sites](chapter.html?c=async-dr), which teaches the same two sites without a hub: the ReplicationPair, the ReplicationPolicy, slots enrolled by annotation, and a ReplicationOps a person writes per volume or per policy. Here the pair and the policy keep their job of authoring the backend relationship, and the per-volume control moves to the upstream `VolumeReplication`, which Ramen creates. The slot and the ReplicationOps are not used for a volume Ramen protects.',
        { callout: 'Ramen is vendored as CRDs and deployment manifests at `v0.1.0-rc1`, not as code. What Ramen does between a DRPlacementControl and a VolumeReplication (the VolumeReplicationGroup, the metadata in S3, the change of placement) is upstream behavior the repository does not show. The chapter teaches what the repository does show: the CRD fields, the chart, the operator\'s part, and the driver\'s side of every call.', kind: 'info' },
      ],
    },
    {
      heading: 'Two other arbiters, and why neither is this one',
      body: [
        'The two-node `Dual` cluster of [Two-node edge clusters with an arbiter](chapter.html?c=two-node-edge) has a witness: the central control plane decides which of two storage nodes on one edge site continues when they lose each other. It decides inside one site, between two nodes of one cluster, and it is not wired to replication. Nothing forbids a `Dual` cluster from being one side of a ReplicationPair, but its witness then still judges only its own two nodes, never between the sites.',
        'A StretchedCluster has a judge on its hub: its design calls the section "The Hub Is the Arbiter," and the control plane there decides that a site is lost and allows a forced promote. It is **synchronous only**: one backend cluster across two sites, a DRPolicy with an empty `schedulingInterval`, both DRClusters in one region, and a NetworkFence for the lost site. Asynchronous replication between stretched clusters is out of its scope, and the operator\'s VolumeReplicationGroup follower returns early for an asynchronous group.',
        { table: { head: ['Arbiter', 'Topology', 'What it decides', 'Asynchronous'], rows: [
          ['the `Dual` witness', 'two storage nodes on one site, a central control plane', 'which node continues after a split', 'no'],
          ['the stretched hub', 'one cluster across two sites, the control plane on the OCM hub', 'that a site is lost, then fences it and allows a forced promote', 'no, sync only'],
          ['this chapter\'s hub', 'two clusters on two sites, the OCM hub with Ramen', 'nothing: it carries out the action a person sets', 'yes'],
        ] } },
      ],
    },
    {
      heading: 'Installing the three sites',
      body: [
        'The order follows the dependencies. The OCM hub comes first, with both sites registered as ManagedClusters, which is OCM\'s own installation and not part of the chart or the repository. Then the operator release on the hub, which brings up the control plane. Then one release on each site, which registers the site\'s storage with that control plane, and on each site a discovery run and an approved draft produce its StorageCluster, as for any site whose control plane runs elsewhere (chapter [two-node-edge](chapter.html?c=two-node-edge) shows that sequence).',
        { list: [
          '**On each DR site**, `deployment.profile: managed` points the operator at the hub\'s management API. `csiaddons.create: true` installs the `VolumeReplication` and `VolumeReplicationClass` CRDs where the cluster does not serve them, the stock csi-addons controller-manager and its RBAC, and sets `enableCSIAddons` on the SimplyblockDriver the chart renders, which adds the csi-addons sidecar. It is off by default. `ramen.drCluster.enabled: true` runs Ramen\'s DR cluster operator in `ramen-system` and installs its CRDs, `VolumeReplicationGroup` among them.',
          '**On the hub**, `deployment.profile: standalone` hosts the control plane, and `discovery.enabled: false` keeps the chart from proposing storage nodes there. `ramen.hub.enabled: true` runs Ramen\'s hub operator, whose manager fails on the missing OCM kinds where OCM is not installed.',
          '**Everywhere**, `ramen.installCRDs` stays `true` unless Ramen already exists on the cluster, for example, from Red Hat Advanced Cluster Management or OpenShift Data Foundation, where the chart must not redefine the CRDs. The vendored CRDs carry `helm.sh/resource-policy: keep`, so an uninstall leaves them and every DR object in place.',
        ] },
        '`ramen.s3StoreProfiles` is shared by both Ramen operators and follows Ramen\'s own `s3StoreProfiles` format. Ramen keeps the PV-related cluster state there so that an application\'s claims can be restored on the other site, and each DRCluster names the profile its site restores from. The chart passes the profiles into Ramen\'s configuration and renders no Secret for them: the Secret each `s3SecretRef` names is created by hand.',
        'The operator\'s part on each site is the CSI driver\'s cluster secret. Each site\'s operator writes its own cluster as `local`, and one more entry for every other cluster the shared control plane lists. That second entry is what lets site-b\'s driver act on a volume whose handle names site-a\'s cluster after a failover, and it exists only because the control plane is shared: a site with its own control plane would not list the other site\'s cluster.',
        { code: 'kubectl get pods -n ramen-system\nkubectl get csiaddonsnodes -A', title: 'The Ramen operator and the csi-addons sidecar on a site' },
      ],
      examples: [
        { title: 'Helm values on a DR site', file: 'examples/async-dr-arbiter/01-helm-values-dr-site.yaml', notes: [
          { at: 'profile: managed', text: 'No control plane on the site. Both sites register with the one on the hub, which is what lets either site promote while the other is down.' },
          { at: 'endpoint: https://control.dr-hub.example.com', text: 'The hub\'s management API. The same URL on both sites.' },
          { at: 'csiaddons:', text: 'Off by default. On, it installs the replication CRDs and the csi-addons controller-manager and turns on the driver\'s sidecar, without which Ramen has nothing to drive.' },
          { at: 'drCluster:', text: 'Ramen\'s execution half: it reconciles the VolumeReplicationGroup the hub places on this site.' },
          { at: 's3ProfileName: site-a-s3', text: 'The name a DRCluster refers to. Both profiles are listed on both sites and on the hub.' },
          { at: 'name: ramen-s3-site-a', text: 'A Secret the chart does not render. It is created by hand on every cluster whose values list the profile.' },
        ] },
        { title: 'Helm values on the hub', file: 'examples/async-dr-arbiter/02-helm-values-hub.yaml', notes: [
          { at: 'profile: standalone', text: 'The hub hosts the control plane both sites register with.' },
          { at: 'enabled: false', text: 'The first discovery run is not rendered. The hub runs no storage nodes.' },
          { at: 'hub:', text: 'Ramen\'s hub operator: DRCluster, DRPolicy, and DRPlacementControl. It needs OCM on this cluster before it can start.' },
        ] },
      ],
    },
    {
      heading: 'The backend relationship and the classes on each site',
      body: [
        'Ramen creates no storage relationship. The backend target and policy are authored by a ReplicationPair and a ReplicationPolicy exactly as in [chapter async-dr](chapter.html?c=async-dr): on site-a, a pair from site-a to site-b and a policy in `failover` mode. The target is written as site-b\'s backend UUID, because site-b\'s StorageCluster lives on another Kubernetes cluster and a name would not resolve. Following the csi-addons design\'s convention, site-b has the reverse pair and policy toward site-a. The id a class needs is in the policy\'s status:',
        { code: 'kubectl get repl dr-policy -n simplyblock -o jsonpath=\'{.status.backendPolicyID}\'', title: 'The backend policy id, on site-a' },
        'The **StorageClass** has the same name on both sites, because a failover restores each claim with its original `storageClassName` and Ramen joins the two sites\' classes by that name. Each carries `ramendr.openshift.io/storageid`, with a different value per site because each site has its own storage cluster, and differing values mark the pair as asynchronous. It carries no `storage.simplyblock.io/replication-policy` annotation, which would enroll its claims through slots.',
        'The **VolumeReplicationClass** binds a `VolumeReplication` to a backend policy. Its provisioner is `csi.simplyblock.io`, and the driver reads two parameters from it: `replicationPolicyID`, which is the backend policy\'s id from `status.backendPolicyID` and not the policy\'s name, and `sourceClusterID`, optional, for a resync from a cluster other than the one the backend has on record. The design\'s spelling `replicationPolicy`, a name, is not read. `schedulingInterval` is not read by the driver either: Ramen selects the class by it, together with the DRPolicy\'s selector and the provisioner, so it equals the DRPolicy\'s interval. The two sites\' classes carry an equal `ramendr.openshift.io/replicationid`. The CRD makes the parameters immutable, so a different policy id is a new class.',
        { table: { head: ['Interval', 'Where', 'What reads it'], rows: [
          ['`spec.interval`', 'ReplicationPolicy', 'the operator, which sends it as whole minutes. This is the cadence the backend replicates at.'],
          ['`parameters.schedulingInterval`', 'VolumeReplicationClass', 'Ramen, to select the class'],
          ['`spec.schedulingInterval`', 'DRPolicy', 'Ramen. Non-empty is asynchronous. The pattern is `^(|\\d+[mhd])$`.'],
        ] } },
        'Nothing compares the ReplicationPolicy\'s interval with the other two, so the three are kept equal by hand. Choose minutes or hours: `1d` is a valid DRPolicy interval and not a duration the operator can parse, and `24h` is valid in both.',
        'Three limits of the driver apply to every class. A striped volume is refused by every Replication verb with `FAILED_PRECONDITION`. An empty `replicationPolicyID` is accepted on purpose: on the site about to be promoted there is nothing to attach yet, so `EnableVolumeReplication` returns success without attaching, and the promote does the work. That is why site-b\'s class may name the reverse policy or none. And the design\'s rule of one owner per volume, a slot or a `VolumeReplication` and never both, is not enforced: the design says the driver refuses a volume a slot manages and the annotation watcher skips claims that have a `VolumeReplication`, and neither check is in the code today. To move a claim from the annotation path, remove the annotation, wait for the slot to detach, and only then select the claim for Ramen.',
      ],
      examples: [
        { title: 'The StorageClass on site-a', file: 'examples/async-dr-arbiter/03-storageclass.yaml', notes: [
          { at: 'name: simplyblock-dr', text: 'The same name on site-b. A restored claim refers to its class by name, and Ramen pairs classes across the sites by name.' },
          { at: 'ramendr.openshift.io/storageid', text: 'Different on each site, here the site\'s cluster UUID. The repository\'s designs spell the key in lower case, and the vendored CRD descriptions write `storageID`. The code that reads it is Ramen\'s.' },
          { at: 'cluster_id', text: 'With `pool_name`, what the driver needs to reach site-a\'s cluster. Site-b\'s class carries its own cluster and pool.' },
        ] },
        { title: 'The VolumeReplicationClass on site-a', file: 'examples/async-dr-arbiter/04-volumereplicationclass.yaml', notes: [
          { at: 'ramendr.openshift.io/replicationid', text: 'Equal on the two sites\' classes. It names the relationship Ramen pairs.' },
          { at: 'dr.example.com/class', text: 'The label the DRPolicy\'s `replicationClassSelector` matches. Any label works, as long as the policy selects it.' },
          { at: 'provisioner: csi.simplyblock.io', text: 'Ramen matches it against the claims\' provisioner.' },
          { at: 'replicationPolicyID: 4f1c', text: 'The backend policy\'s id from `ReplicationPolicy.status.backendPolicyID`. A name here, or the key `replicationPolicy`, leaves the id empty, and the driver then attaches nothing.' },
          { at: 'schedulingInterval: 5m', text: 'Equal to the DRPolicy\'s. Ramen selects by it, the driver ignores it, and the backend replicates at the ReplicationPolicy\'s interval.' },
        ] },
      ],
    },
    {
      heading: 'The DR intent on the hub',
      body: [
        'A **DRCluster** is written per site and is cluster-scoped. `s3ProfileName` is required and immutable: the profile the site restores PV-related cluster state from when an application moves to it. `region` is immutable, and managed clusters in one region form a sync group, so an asynchronous pair has two regions. `cidrs` and `clusterFence` feed the fence of sync DR and are left out here.',
        'The **DRPolicy** is cluster-scoped too. `drClusters` lists exactly two DRClusters, which a CEL rule enforces, and is immutable. `schedulingInterval` is required and immutable, and a non-empty value makes the policy asynchronous. `replicationClassSelector` is immutable and is matched against the VolumeReplicationClasses\' labels. Ramen\'s hub then writes the class pairs it found into `status.async.peerClasses`: the StorageClass name, the two storage ids, and the replication id. Nothing in this repository checks that pairing. An earlier same-cluster preflight was removed because it could see whether a label was present, never whether the two sites agreed. An empty `peerClasses` means the pairing failed.',
        { code: 'kubectl get drpolicy dr-5m -o jsonpath=\'{.status.async.peerClasses}\'', title: 'What Ramen paired' },
        'The **Placement** is an OCM kind in the application\'s namespace that places the application on a managed cluster. Its schema is not in this repository, so the chapter does not show one. The DRPlacementControl refers to it through `placementRef`, whose description in the vendored CRD still says PlacementRule.',
        'The **DRPlacementControl** is namespaced, in the application\'s namespace on the hub. `drPolicyRef`, `placementRef`, and `pvcSelector` are required and immutable, and `preferredCluster` names the site the application runs on. With no `action`, the application runs on its preferred site and is protected. Ramen places a VolumeReplicationGroup on site-a, which creates one `VolumeReplication` per claim the selector matches. The csi-addons controller-manager calls `EnableVolumeReplication`, and the driver attaches the volume to the backend policy the class names with one `PUT` of `replication_policy_id`. From then on `GetVolumeReplicationInfo` reports `lastSyncTime`, the creation time of the newest fully replicated snapshot. The `VolumeReplication` conditions `Completed`, `Degraded`, and `Resyncing` are what Ramen aggregates into the group\'s `DataProtected` and the DRPlacementControl\'s `PeerReady`, which gates a relocate.',
        { code: '# on the hub\nkubectl get drpc -n shop -o wide\n# on site-a\nkubectl get volumereplication -n shop', title: 'Following protection' },
      ],
      examples: [
        { title: 'The two DRClusters', file: 'examples/async-dr-arbiter/05-drclusters.yaml', notes: [
          { at: 'name: site-a', text: 'Named after the managed cluster it describes, the name the DRPolicy and the DRPlacementControl use.' },
          { at: 'region: east', text: 'Different on the two DRClusters. One region would make the two sites a sync group.' },
          { at: 's3ProfileName: site-a-s3', text: 'Required and immutable. A profile of the Helm values, here the one site-a restores from.' },
        ] },
        { title: 'The DRPolicy', file: 'examples/async-dr-arbiter/06-drpolicy.yaml', notes: [
          { at: 'drClusters:', text: 'Exactly two, and immutable.' },
          { at: 'schedulingInterval: 5m', text: 'Non-empty, so asynchronous. Equal to the classes\' `schedulingInterval` and, by hand, to the ReplicationPolicy\'s interval.' },
          { at: 'replicationClassSelector:', text: 'Selects the VolumeReplicationClasses by label. Immutable.' },
        ] },
        { title: 'The DRPlacementControl', file: 'examples/async-dr-arbiter/07-drplacementcontrol.yaml', notes: [
          { at: 'namespace: shop', text: 'The application\'s namespace, with its Placement, on the hub.' },
          { at: 'name: dr-5m', text: 'The DRPolicy. Immutable on the DRPlacementControl.' },
          { at: 'kind: Placement', text: 'The OCM Placement of the application. Immutable.' },
          { at: 'preferredCluster: site-a', text: 'Where the application runs while no action is set.' },
          { at: 'app: orders', text: 'The claims to protect. Each matching claim gets one VolumeReplication on the site the application runs on.' },
        ] },
      ],
    },
    {
      heading: 'Relocate and failover: what a person writes and what follows',
      body: [
        'A **relocate** is the planned move, with both sites up. A person sets `action: Relocate`, and the move goes toward `preferredCluster`, the field the CRD describes as where the user prefers the application to run. The csi-addons design describes the sequence the driver serves: the source side is demoted first, which fences the source (its paths become ANA inaccessible) and then takes and confirms one final snapshot, answering `ABORTED` while it converges so the controller calls again. Then the target is promoted with `force=false`, which the control plane refuses until the demote has completed. The design calls this lossless, because the final snapshot holds every acknowledged write.',
        'A **failover** is the unplanned one. A person who has decided that site-a is lost sets `action: Failover` and `failoverCluster: site-b`. When `failoverCluster` is left out the DRPlacementControl picks the surviving cluster of the DRPolicy, which chooses the target and still does not decide that a site is lost. On site-b the `VolumeReplication` turns primary. csi-addons calls `EnableVolumeReplication` before every promote, and the driver is written to let it succeed on the site about to be promoted, where there is nothing to attach yet. The csi-addons design describes the rest: `PromoteVolume` is tried planned first, and with no demote and a source that is not online the control plane answers `FAILED_PRECONDITION`, which the csi-addons controller escalates to a forced promote at once, with no grace period. The forced promote clones the last fully replicated snapshot on site-b, so the writes since it are lost: up to one interval, plus whatever was still being shipped.',
        { table: { head: ['', 'Relocate', 'Failover'], rows: [
          ['Who sets it', 'a person, with both sites up', 'a person, after deciding the source is lost'],
          ['Promote', '`force=false`, after a completed demote', 'planned, escalated to forced on `FAILED_PRECONDITION`'],
          ['Data', 'every acknowledged write, according to the design', 'the last fully replicated snapshot'],
        ] } },
        'A promote is a call to the control plane. That is why the control plane belongs on the third site: if it ran on site-a, the loss of site-a would take it along, and site-b could not be promoted at all. The design also names a race. A source node that died within the last health-check interval still reads online, so the planned promote is treated as a no-op for one reconcile, and the next attempt, once the node\'s status has caught up, takes the forced path.',
        'After the promote, the new primary on site-b is **not replicated anywhere**. The driver deliberately does not attach the class\'s policy to a promoted volume, because attaching at promote time was found harmful live during a relocate. When site-a returns, Ramen drives `ResyncVolume`, which reverses the shipping direction, resyncs by delta, and reports ready once caught up. It never merges and never cuts over. Moving the application back to site-a is then a relocate.',
        { code: 'kubectl patch drpc orders-drpc -n shop --type merge \\\n  -p \'{"spec":{"action":"Failover","failoverCluster":"site-b"}}\'', title: 'The failover, on the hub' },
        'A rehearsal does not need either action. The TestFailover drill of [chapter async-dr](chapter.html?c=async-dr) recovers a clone on a bubble cluster and promotes nothing.',
      ],
      examples: [
        { title: 'The DRPlacementControl, set to fail over', file: 'examples/async-dr-arbiter/08-drplacementcontrol-failover.yaml', notes: [
          { at: 'action: Failover', text: 'The decision, written by a person. The CRD allows `Failover` and `Relocate`.' },
          { at: 'failoverCluster: site-b', text: 'The site to run on. Left out, the surviving cluster of the DRPolicy is chosen.' },
          { at: 'preferredCluster: site-a', text: 'Unchanged. It is the site a later relocate moves the application back to.' },
        ] },
      ],
    },
    {
      heading: 'What goes wrong',
      body: [
        { pitfalls: [
          { symptom: 'The `VolumeReplication` reports `Completed`, and `lastSyncTime` never appears.', cause: 'The class carries the policy\'s name, or the key `replicationPolicy`. The driver reads only `replicationPolicyID`, treats an empty value as the site about to be promoted, and succeeds without attaching.', fix: 'Write a new class with `replicationPolicyID` set to `ReplicationPolicy.status.backendPolicyID`. The parameters of a class are immutable.' },
          { symptom: 'No `VolumeReplication` appears on site-a after the DRPlacementControl is created.', cause: 'No class matches: its `schedulingInterval` differs from the DRPolicy\'s, its labels miss the `replicationClassSelector`, or its provisioner differs from the claims\'.', fix: 'Align the class. The DRPolicy\'s interval and selector are immutable, so changing them means a new DRPolicy.' },
          { symptom: '`status.async.peerClasses` on the DRPolicy is empty.', cause: 'The StorageClass names differ between the sites, a `storageid` label is missing, or the two classes\' `replicationid` values differ.', fix: 'Give the classes one name and one `replicationid`, and each StorageClass its own `storageid`. Nothing in this repository checks this for you.' },
          { symptom: 'Snapshots arrive at another cadence than the DRPolicy states.', cause: 'The ReplicationPolicy\'s `spec.interval` differs. Ramen\'s intervals select a class and set no cadence.', fix: 'Set the ReplicationPolicy\'s interval to the DRPolicy\'s, in minutes or hours.' },
          { symptom: 'Ramen\'s hub operator does not start.', cause: 'OCM is not installed on the hub cluster.', fix: 'Install the OCM hub and register both sites before the release with `ramen.hub.enabled: true`.' },
          { symptom: 'The release conflicts over Ramen CRDs on a cluster with Red Hat Advanced Cluster Management or OpenShift Data Foundation.', cause: 'Ramen is already installed there.', fix: 'Set `ramen.installCRDs: false`.' },
          { symptom: 'A DRCluster is set to `clusterFence: Fenced` to isolate a lost asynchronous site.', cause: 'Nothing in this repository can carry that fence out. The driver\'s fence service fences one site of a stretched sync cluster and refuses addresses that match nodes with no site.', fix: 'Leave `clusterFence` unset for an asynchronous pair, and keep the lost site from returning by other means until it has been resynced.' },
          { symptom: '`EnableVolumeReplication` fails with `FAILED_PRECONDITION`, saying the volume is striped.', cause: 'The volume is striped, which no Replication verb supports.', fix: 'Protect volumes of a class without `stripe_width`.' },
          { symptom: 'A volume re-syncs in full again and again.', cause: 'It is enrolled by the annotation and selected by Ramen at once, so two owners attach it in turn, and the code does not stop the second.', fix: 'Remove the annotation, let the slot detach, then protect the claim through Ramen only.' },
          { symptom: 'After `action: Failover`, every promote on site-b fails with `UNAVAILABLE`.', cause: 'Site-b cannot reach the control plane, typically because it ran on site-a.', fix: 'Host the control plane on the third site, where neither site\'s loss takes it along.' },
        ] },
      ],
    },
  ],

  questions: [
    { topic: 'The arbiter', cat: 'concept', text: 'In this chapter\'s layout, what does the third site do?', options: ['Runs the OCM hub', 'Runs Ramen\'s hub operator, which holds the DRClusters, the DRPolicy, and the DRPlacementControls', 'Judges that a site is lost and starts the failover', 'Hosts the shared simplyblock control plane', 'Acts as the quorum member between the two sites'], correct: [0, 1, 3], explain: 'The third site orchestrates and holds the DR intent, and in this layout it also hosts the control plane both sites register with. It judges nothing, and for asynchronous replication there is no quorum.' },
    { topic: 'The arbiter', cat: 'concept', text: 'Who decides that an asynchronously replicated application fails over?', options: ['Ramen\'s hub, once a DRCluster is unreachable', 'A person, by setting `spec.action: Failover` on the DRPlacementControl', 'The csi-addons controller, once a VolumeReplication is degraded', 'The operator\'s VolumeReplicationGroup follower', 'The control plane on the hub'], correct: [1], explain: 'Ramen never fails over by itself, and nothing in the repository writes the action. The follower serves stretched sync clusters only.' },
    { topic: 'The arbiter', cat: 'concept', text: 'Which of these are true of asynchronous replication between two sites in this repository?', options: ['There is no two-site witness and no quorum', 'Nothing in the repository fences the lost site', 'The Ramen-driven path has been validated end to end', '`DRCluster.spec.clusterFence` fences the lost asynchronous site', 'The hub fails over after a timeout'], correct: [0, 1], explain: 'No witness, no quorum, and no fence for async. The fence the DRCluster drives is for sync DR, and the driver fences only a stretched site. The validation is outstanding.' },
    { topic: 'The arbiter', cat: 'concept', text: 'Which statements describe the state of the Ramen-driven asynchronous path?', options: ['The csi-addons design records the validation against a Ramen VolumeReplicationGroup in async mode as outstanding', 'The Ramen test plan lists its manual scenarios as blocked on a live hub', 'Comments in the driver record live relocate runs that found and fixed defects', 'It is the generally available way to protect volumes', 'The operator has a controller of its own for asynchronous Ramen groups'], correct: [0, 1, 2], explain: 'The path has been exercised and has not passed a validation, which is why the chapter is a lab procedure. No operator controller handles asynchronous Ramen groups.' },
    { topic: 'The arbiter', cat: 'concept', text: 'Which parts of the async-dr chapter remain in use for a volume Ramen protects?', options: ['The ReplicationPair', 'The ReplicationPolicy', 'A ReplicationSlot created by the annotation', 'A ReplicationOps with `action: failover`', 'The `storage.simplyblock.io/replication-policy` annotation on the class'], correct: [0, 1], explain: 'The pair and the policy author the backend relationship the class points at. The slot, the annotation, and the ReplicationOps belong to the other path, and a volume has one owner.' },
    { topic: 'The arbiter', cat: 'action', text: 'Which of these does Ramen\'s hub write on its own initiative?', options: ['`action: Failover` on a DRPlacementControl', '`failoverCluster` on a DRPlacementControl', 'A DRCluster for a newly registered managed cluster', 'A DRPolicy with a shorter interval', 'A ReplicationOps'], correct: [], explain: 'None of them. The DR objects are written by a person, and the hub carries out the action a person sets.' },

    { topic: 'Other arbiters', cat: 'concept', text: 'Which statements about the witness of a `Dual` two-node cluster are true?', options: ['It decides which of two storage nodes on one site continues', 'It is the central control plane', 'It decides between the two sites of an asynchronous pair', 'It is turned on with `enableArbitration`', 'It requires Ramen'], correct: [0, 1, 3], explain: 'The `Dual` witness judges inside one site, between two nodes of one cluster, and is not wired to replication.' },
    { topic: 'Other arbiters', cat: 'concept', text: 'Which statements about a StretchedCluster\'s hub are true?', options: ['Its judgment applies to synchronous replication only', 'The control plane there judges a lost site and allows a forced promote', 'It judges a lost site of an asynchronous pair of two clusters', 'Its DRPolicy has an empty `schedulingInterval`', 'It takes its cadence from a ReplicationPolicy'], correct: [0, 1, 3], explain: 'The stretched design makes the hub the judge for one cluster across two sites, synchronously. An empty interval is a sync policy, and asynchronous replication is out of its scope.' },
    { topic: 'Other arbiters', cat: 'concept', text: 'What does the operator\'s VolumeReplicationGroup follower do with an asynchronous group?', options: ['Returns early and does nothing', 'Moves the volumes\' leadership to the site', 'Calls `PromoteVolume`', 'Fences the other site', 'Writes the DRPlacementControl\'s action'], correct: [0], explain: 'The follower serves stretched sync clusters, and an asynchronous group\'s volumes are moved by its VolumeReplications through the driver.' },
    { topic: 'Other arbiters', cat: 'concept', text: 'Which of these decides, for asynchronous replication between two sites, that a site is lost?', options: ['The `Dual` witness', 'The stretched hub', 'Ramen\'s hub', 'The csi-addons controller-manager', 'The OCM Placement'], correct: [], explain: 'None of them. A person decides, and the hub carries out the action.' },

    { topic: 'Installation', cat: 'field', text: 'Which values does this chapter set on a DR site?', options: ['`csiaddons.create: true`', '`ramen.drCluster.enabled: true`', '`ramen.hub.enabled: true`', '`deployment.profile: managed`', '`multiCluster.enable: true`'], correct: [0, 1, 3], explain: 'A site runs the csi-addons machinery and Ramen\'s DR cluster operator, and registers with the hub\'s control plane. The hub operator runs on the hub, and `multiCluster` lists storage clusters for the driver, not DR.' },
    { topic: 'Installation', cat: 'field', text: 'What does `csiaddons.create: true` do?', options: ['Installs the `VolumeReplication` and `VolumeReplicationClass` CRDs where the cluster does not serve them', 'Installs the stock csi-addons controller-manager', 'Sets `enableCSIAddons` on the SimplyblockDriver the chart renders, which adds the sidecar', 'Installs Ramen\'s DR cluster operator', 'Installs a fence service for asynchronous sites'], correct: [0, 1, 2], explain: 'The CRDs, the controller-manager, and the sidecar. Ramen is its own value, and no fence serves asynchronous sites.' },
    { topic: 'Installation', cat: 'concept', text: 'What does Ramen\'s hub operator need on its cluster before it starts?', options: ['Open Cluster Management', 'The csi-addons controller-manager', 'Storage nodes', 'A VolumeReplicationClass', 'Velero'], correct: [0], explain: 'The hub operator drives placement through OCM, and its manager fails on the missing OCM kinds without it.' },
    { topic: 'Installation', cat: 'field', text: 'Ramen is already installed on a cluster by Red Hat Advanced Cluster Management. What does the release set?', options: ['`ramen.installCRDs: false`', '`ramen.createNamespace: false` and nothing else', '`csiaddons.create: false`', '`ramen.hub.enabled: false`', 'Nothing, the chart replaces the existing CRDs'], correct: [0], explain: 'The CRDs must not be redefined where Ramen exists, and `installCRDs` is the switch. The vendored CRDs are kept on uninstall.' },
    { topic: 'Installation', cat: 'concept', text: 'Why does this layout share one control plane between the two sites?', options: ['Each operator writes driver secret entries for every cluster the control plane lists, so site-b\'s driver resolves a handle naming site-a\'s cluster', 'A ReplicationPair names both clusters as one control plane knows them', 'Ramen requires both sites to share a control plane', 'A promote is a call to the control plane', 'OCM requires one control plane per hub'], correct: [0, 1, 3], explain: 'The driver secret, the pair, and the promote all depend on one control plane addressing both clusters. Ramen and OCM know nothing of the simplyblock control plane.' },
    { topic: 'Installation', cat: 'field', text: 'Which statements about `ramen.s3StoreProfiles` are true?', options: ['Ramen keeps PV-related cluster state there for the restore on the other site', 'Each DRCluster names one profile', 'The chart creates the Secret each profile refers to', 'It follows Ramen\'s own `s3StoreProfiles` format', 'It holds one profile per StorageClass'], correct: [0, 1, 3], explain: 'The profiles are passed into Ramen\'s configuration, and the DRCluster names the one its site restores from. The Secret is created by hand.' },

    { topic: 'Classes', cat: 'field', text: 'What value does `replicationPolicyID` on a VolumeReplicationClass carry?', options: ['`ReplicationPolicy.status.backendPolicyID`', 'The ReplicationPolicy\'s name', 'The ReplicationPair\'s `status.backendTargetID`', 'The StorageCluster\'s UUID', 'The DRPolicy\'s name'], correct: [0], explain: 'The driver passes it to the control plane as the policy to attach, so it is the backend id. Resolving a name is not implemented.' },
    { topic: 'Classes', cat: 'field', text: 'A class carries `replicationPolicy: dr-policy` and no `replicationPolicyID`. What happens on `EnableVolumeReplication`?', options: ['It succeeds without attaching anything', 'The VolumeReplication is refused at admission', 'The driver resolves the name to the policy', 'It fails with `FAILED_PRECONDITION`', 'The CRD refuses the class'], correct: [0], explain: 'The driver reads only `replicationPolicyID` and treats an empty value as the site about to be promoted, so it returns success and the volume is not replicated.' },
    { topic: 'Classes', cat: 'field', text: 'Which of these are equal on the two sites?', options: ['The StorageClass name', 'The `ramendr.openshift.io/replicationid` value on the VolumeReplicationClass', 'The `ramendr.openshift.io/storageid` value', 'The StorageClass\'s `cluster_id`', 'The VolumeReplicationClass\'s `schedulingInterval`'], correct: [0, 1, 4], explain: 'One class name, one replication id, and one interval. The storage id and the cluster differ, because each site has its own storage cluster.' },
    { topic: 'Classes', cat: 'concept', text: 'Which statements about the intervals are true?', options: ['`ReplicationPolicy.spec.interval` is the cadence the backend replicates at', 'The driver reads the class\'s `schedulingInterval`', 'The DRPolicy\'s `schedulingInterval` equals the class\'s, which is how Ramen selects it', 'Nothing compares the ReplicationPolicy\'s interval with the other two', '`1d` is valid in both the ReplicationPolicy and the DRPolicy'], correct: [0, 2, 3], explain: 'Ramen\'s two intervals select a class, the policy\'s sets the cadence, and the three are kept equal by hand. `1d` matches Ramen\'s pattern and is not a duration the operator parses.' },
    { topic: 'Classes', cat: 'action', text: 'The backend policy of an existing VolumeReplicationClass has to change. What is done?', options: ['A new class with the new `replicationPolicyID`', 'An edit of the class\'s parameters', 'An annotation on the class', 'Deleting the DRPolicy', 'Restarting the driver'], correct: [0], explain: 'The CRD makes the parameters immutable, so a different policy id is a new class.' },
    { topic: 'Classes', cat: 'concept', text: 'Which statements about protecting a volume through a VolumeReplication are true?', options: ['A striped volume is refused by every Replication verb', 'The driver refuses a volume a ReplicationSlot manages', 'An empty `replicationPolicyID` makes `EnableVolumeReplication` succeed without attaching', 'Moving a claim off the annotation path means removing the annotation and waiting for the slot to detach first', 'Enabling against a different policy re-attaches without refusing, which is a full re-sync'], correct: [0, 2, 3, 4], explain: 'The one-owner refusal is in the design and not in the code, so the move between paths is done by hand. A different-policy attach is not refused either.' },

    { topic: 'Hub objects', cat: 'field', text: 'Which rules does the DRPolicy CRD enforce?', options: ['`drClusters` lists exactly two clusters', '`drClusters` is immutable', '`schedulingInterval` is required', '`schedulingInterval` can be changed later', '`replicationClassSelector` is immutable'], correct: [0, 1, 2, 4], explain: 'CEL rules make the cluster list two and immutable, and the interval and the selector immutable. A different cadence is a new DRPolicy.' },
    { topic: 'Hub objects', cat: 'field', text: 'Which statements about the DRClusters of an asynchronous pair are true?', options: ['Their regions differ', '`s3ProfileName` is required and immutable', '`cidrs` is needed for the asynchronous pair', '`clusterFence: Fenced` fences the lost asynchronous site', '`region` is immutable'], correct: [0, 1, 4], explain: 'Clusters in one region form a sync group, so an asynchronous pair has two. The CIDRs and the fence belong to sync DR.' },
    { topic: 'Hub objects', cat: 'field', text: 'Which fields of a DRPlacementControl are required?', options: ['`drPolicyRef`', '`placementRef`', '`pvcSelector`', '`action`', '`failoverCluster`'], correct: [0, 1, 2], explain: 'The three references are required and immutable. The action and the failover cluster are written when a person decides to move the application.' },
    { topic: 'Hub objects', cat: 'concept', text: 'A DRPlacementControl without `action` is created. Which of these follow?', options: ['A VolumeReplicationGroup on the preferred cluster', 'One VolumeReplication per claim the `pvcSelector` matches', 'The driver attaches each volume to the backend policy the class names', '`lastSyncTime` reports the creation time of the newest fully replicated snapshot', 'One ReplicationSlot per claim'], correct: [0, 1, 2, 3], explain: 'Ramen creates the group and the VolumeReplications, and the driver attaches and reports. Slots belong to the annotation path.' },
    { topic: 'Hub objects', cat: 'concept', text: 'Which statements about `status.async.peerClasses` on the DRPolicy are true?', options: ['Ramen\'s hub computes it', 'Distinct storage ids mark the pair as asynchronous', 'Nothing in this repository checks the pairing', 'The operator records `PeerClassesVerified` once it agrees', 'The classes are joined by StorageClass name'], correct: [0, 1, 2, 4], explain: 'The pairing is Ramen\'s, across the two sites. The operator\'s same-cluster preflight was removed because it could check presence and never agreement.' },

    { topic: 'Failover and relocate', cat: 'action', text: 'Which of these describe a relocate as the csi-addons design describes it?', options: ['The source is fenced first, then one final snapshot is taken and confirmed', 'The target is promoted with `force=false`', 'The planned promote is refused until the demote has completed', 'The writes of the last interval are lost', 'The design calls it lossless'], correct: [0, 1, 2, 4], explain: 'The demote leaves every acknowledged write in the final snapshot, and the planned promote waits for it. Losing an interval is the failover\'s cost.' },
    { topic: 'Failover and relocate', cat: 'action', text: 'Which statements about a failover are true?', options: ['A person sets `action: Failover`', 'Without `failoverCluster`, the surviving cluster of the DRPolicy is chosen', 'A planned promote answered with `FAILED_PRECONDITION` is escalated to a forced one', 'The forced promote clones the last fully replicated snapshot', 'The writes since that snapshot are lost'], correct: [0, 1, 2, 3, 4], explain: 'All five. Choosing the surviving cluster picks the target and still does not decide that a site is lost.' },
    { topic: 'Failover and relocate', cat: 'concept', text: 'A failover promoted the volumes on site-b. Which of these are true?', options: ['The driver attached the class\'s policy to each promoted volume', 'The new primary is not replicated until site-a returns and is resynced', '`ResyncVolume` reverses the direction and resyncs by delta', '`ResyncVolume` cuts the volume back over to site-a', 'Moving back to site-a is a relocate'], correct: [1, 2, 4], explain: 'The driver deliberately attaches no policy at promote time. Resync reconciles and never cuts over, and the move home is a planned relocate.' },
    { topic: 'Failover and relocate', cat: 'concept', text: 'The control plane runs on site-a, and site-a is lost. Which of these are true?', options: ['Site-b cannot be promoted', 'Ramen moves the control plane to site-b', 'The driver promotes locally without the control plane', 'The hub fences site-a', 'It is the reason the chapter hosts the control plane on the third site'], correct: [0, 4], explain: 'A promote is a call to the control plane. Nothing moves the control plane, and nothing fences an asynchronous site.' },
    { topic: 'Failover and relocate', cat: 'action', text: 'A DRPlacementControl is set to `action: Failover`. Which of these happen?', options: ['The operator writes a ReplicationOps', 'The ReplicationSlots go to `failed_over`', 'Site-a is fenced', 'The ReplicationPolicy is deleted', 'The DRPolicy\'s interval is shortened'], correct: [], explain: 'None of them. Ramen drives the VolumeReplications and the driver promotes. No slot, ReplicationOps, or fence is involved, and the policies are untouched.' },
    { topic: 'Failover and relocate', cat: 'field', text: 'Which fields of the DRPlacementControl change when it is set to fail over?', options: ['`action`', '`failoverCluster`', '`preferredCluster`', '`pvcSelector`', '`drPolicyRef`'], correct: [0, 1], explain: 'The action and the target. The references are immutable, and the preferred cluster stays as the site a later relocate goes back to.' },
  ],

  tasks: [
    {
      title: 'The class on site-a',
      scenario: 'The DRPolicy replicates every ten minutes, and the ReplicationPolicy toward site-b reports `status.backendPolicyID: 4f1c8a2e-9b3d-4e7f-a1c5-6d2e8f0b3a71`. Write site-a\'s VolumeReplicationClass so that Ramen pairs it with site-b\'s and the driver attaches the volumes to that policy.',
      yaml: 'apiVersion: replication.storage.openshift.io/v1alpha1\nkind: {{kind}}\nmetadata:\n  name: simplyblock-dr-10m\n  labels:\n    ramendr.openshift.io/{{label}}: simplyblock-site-a-site-b\n    dr.example.com/class: simplyblock-dr\nspec:\n  provisioner: {{provisioner}}\n  parameters:\n    {{param}}: 4f1c8a2e-9b3d-4e7f-a1c5-6d2e8f0b3a71\n    schedulingInterval: {{interval}}\n',
      blanks: {
        kind: { accept: ['VolumeReplicationClass'], hint: 'the csi-addons class kind' },
        label: { accept: ['replicationid'], hint: 'the label equal on both sites\' classes' },
        provisioner: { accept: ['csi.simplyblock.io'], hint: 'the driver\'s name' },
        param: { accept: ['replicationPolicyID'], hint: 'the one key the driver reads for the policy' },
        interval: { accept: ['10m'], hint: 'equal to the DRPolicy\'s' },
      },
      explain: 'The `replicationid` label pairs the two classes, the provisioner is the driver, `replicationPolicyID` carries the backend id, and `schedulingInterval` equals the DRPolicy\'s so that Ramen selects the class.',
    },
    {
      title: 'The DRPolicy',
      scenario: 'Write the hub\'s policy for the pair site-a and site-b, replicating asynchronously every ten minutes, and selecting the classes labeled `dr.example.com/class: simplyblock-dr`.',
      yaml: 'apiVersion: ramendr.openshift.io/v1alpha1\nkind: {{kind}}\nmetadata:\n  name: dr-10m\nspec:\n  drClusters:\n    - site-a\n    - {{peer}}\n  schedulingInterval: {{interval}}\n  {{selector}}:\n    matchLabels:\n      dr.example.com/class: simplyblock-dr\n',
      blanks: {
        kind: { accept: ['DRPolicy'], hint: 'the cluster-scoped hub kind with the interval' },
        peer: { accept: ['site-b'], hint: 'exactly two DRClusters' },
        interval: { accept: ['10m'], hint: 'non-empty is asynchronous, in minutes, hours, or days' },
        selector: { accept: ['replicationClassSelector'], hint: 'the selector over VolumeReplicationClasses' },
      },
      explain: 'A `DRPolicy` names exactly two DRClusters. A non-empty `schedulingInterval` makes it asynchronous, and `replicationClassSelector` selects the classes by label. All three are immutable.',
    },
    {
      title: 'The failover',
      scenario: 'A person has decided that site-a is lost. Complete the DRPlacementControl of the `orders` application so that it runs on site-b.',
      yaml: 'apiVersion: ramendr.openshift.io/v1alpha1\nkind: {{kind}}\nmetadata:\n  name: orders-drpc\n  namespace: shop\nspec:\n  action: {{action}}\n  {{target}}: site-b\n  drPolicyRef:\n    name: {{policy}}\n  placementRef:\n    kind: Placement\n    name: orders-placement\n  preferredCluster: site-a\n  pvcSelector:\n    matchLabels:\n      app: orders\n',
      blanks: {
        kind: { accept: ['DRPlacementControl'], hint: 'the per-application hub kind, short name `drpc`' },
        action: { accept: ['Failover'], hint: 'the unplanned action of the enum' },
        target: { accept: ['failoverCluster'], hint: 'the field naming the site to run on' },
        policy: { accept: ['dr-5m'], hint: 'the DRPolicy of the chapter\'s examples, unchanged because it is immutable' },
      },
      explain: 'The failover is the same `DRPlacementControl` with `action: Failover` and `failoverCluster`. The policy reference is immutable, so it stays `dr-5m`.',
    },
  ],
});
