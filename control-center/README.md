# simplyblock Control Center — web UI prototype

Single-page UI for the simplyblock control plane. Runs as a static container in
the Kubernetes control plane and reads the Kubernetes API, the operator API and
Helm — nothing else.

> **Deploying this in the simplyblock-operator monorepo?** See
> [INTEGRATION.md](INTEGRATION.md): it is shipped by the operator Helm chart
> (`--set controlCenter.enabled=true`), tested with the `sb-mock` backend in
> [`mock/`](mock/) (`--set controlCenter.mock.enabled=true`), and built from
> [`deploy/`](deploy/). This file describes the console itself.

**The control plane is cross-cluster.** One deployment manages every storage
cluster, so its services, container logs and FoundationDB state database live in
a top-level **Control plane** section, not under any single cluster. The four
sections are Clusters, Kubernetes, Disaster recovery and Control plane.

## Recent additions

- **DR hub client** — the Disaster recovery section now reads and writes the
  `dr.simplyblock.io` CRs of [simplyblock-dr](https://github.com/simplyblock/simplyblock-dr)
  (plans, DR paths, protected applications, recovery plans, actions, tests,
  schedules, restores, site profiles, DHCP servers, DRConfig; site-mapper
  findings, guest reservations and the resolution inbox) through the Kubernetes API
  (`drhub-api.jsx`, `drhub.jsx`); the `/operator/v1/proposed` DR endpoints
  described further down are superseded for the top layer. `SB_MODE=dr` is the
  stripped-down DR-only console for a hub without a simplyblock control plane
  (preview: `?mode=dr`). See [INTEGRATION.md](INTEGRATION.md#disaster-recovery-and-the-dr-only-console).

- **Cluster discovery and deployment** — `Kubernetes → cluster → Discovery & deployment`: filtered hardware discovery, then a deployment document (draft → approve) that runs node configuration, storage-node addition and activation asynchronously, with a per-step log.
- **Migration paths** — online site migration by asynchronous replication plus KubeVirt live migration; application groups, convergence backlog, sequenced queue.
- **Ransomware recovery** — group-consistent backup policies as a recovery basis for protected applications, with point-in-time failover by generation or timestamp.
- **Ramen recipes** — per-application boot sequences (ordered groups, readiness gates, hooks, failure policy) driving capture and recovery workflows.
- **File and object storage** — pNFS-backed RWX PVCs and S3 buckets as first-class objects, both replicable and backupable.

## Files

| File | Role |
|---|---|
| `index.html` | Shell, design tokens, script load order |
| `contexts.jsx` | **Contexts and feature flags.** The Kubernetes clusters the console reaches (`/k8s/<name>`), their control planes (`/cp/<name>`), their site/stage/tenant labels, and the flags that gate fixture-only views |
| `k8s-client.jsx` | Kubernetes API transport per context: resource registry, paths, list/get/create/patch/delete, watch, pod logs, access reviews, the Ops object builders |
| `cp-client.jsx` | **Control plane v2 client** per context: clusters, nodes, devices, pools, volumes, snapshots, tasks, the event log, capacity and I/O statistics, device health, SSE watch |
| `store.jsx` | Cached collections per (context, kind): list once, follow a watch or poll, shared by every view; `useCollection`, `useV2` |
| `fleet-model.jsx` | Identity across contexts (`objKey`), site/stage/tenant precedence, the joins between kinds (node set → nodes → devices, PV handle, Node labels) |
| `k8s-sources.jsx` | The storage view models' inputs, assembled from CRDs, core objects, and the v2 DTOs |
| `api.jsx` | **View models.** Normalizers, the `REG` registry, the `api.*` reads and actions (Ops objects, PVC and policy edits), `useResource` |
| `agent.jsx` | Control plane pods and their logs, storage plane pod logs, device health (SMART) from the control plane, SPDK thread utilization from Prometheus |
| `dashboard.jsx` | **Fleet dashboard:** roll-up, grouping by site / stage / tenant / context, the cluster matrix, sites |
| `alerts.jsx` | **Alert engine and views:** rules derived from CR status, the event log, pods, operations; open and resolved history; silences; the bell and badges |
| `logs.jsx` | **Consolidated logs** per cluster: storage plane and control plane pod logs, the event log, Kubernetes events, merged and filtered |
| `tasks.jsx` | **Task and operation monitor:** Ops kinds, control plane tasks, subsystem migrations as one tree, with cancel where an API exists |
| `cr-view.jsx` / `cr-headlines.jsx` | **Two-tier custom resource views:** essentials (editable spec, status, conditions), everything (trees, YAML), the Kubernetes side; the per-kind headline tables |
| `panels.jsx` | Event log, control plane pods, SPDK threads and logs, SMART, the control plane section |
| `mock-backend.jsx` | Fixture store shaped like v2 payloads (snake_case, uuid FKs). Design/validation only |
| `mock-api.jsx` | `fetch` interceptor serving the fixtures, including every mutation route |
| `ui.jsx` | Primitives: traffic light, capacity bar, sparkline, metric block, icons, formatters |
| `actions.jsx` | Command registry per object kind + kebab menus + parameterised confirm dialogs |
| `tiles.jsx` / `tiles-data.jsx` | Tiles: cluster, host, node, device / pool, volume, snapshot, backup, policy |
| `details.jsx` / `details-data.jsx` | Detail pages, same split |
| `app.jsx` | Breadcrumb router, overview + detail views, sorting/filtering/scoping, cluster switcher; `SB_MODE` (full / dr) |
| `k8s-client.jsx` | Kubernetes API transport: resource registry per API group (storage, DR hub, Ramen, OCM), paths, access reviews |
| `drhub-api.jsx` / `drhub.jsx` | **DR hub client.** View models over the `dr.simplyblock.io` CRs, the mutations the console performs (RecoveryAction, TestBubble, RestoreAction, TestSchedule, plan / path / application / recovery plan), dashboard, tiles, details, dialogs |

## Object model & navigation

```
Clusters
└── Cluster
    ├── Hosts ─────── Host ── (its Storage nodes)
    ├── Storage nodes ─ Node ── Devices ── Device
    ├── Storage pools ─ Pool ── Logical volumes ─ Volume ─┬─ Snapshots ─ Snapshot
    │                     ├─ Snapshots (pool-scoped)      └─ Backups   ─ Backup
    │                     └─ Backups   (pool-scoped)
    ├── Logical volumes (cluster-wide)
    ├── Snapshots  (cluster-wide)
    ├── Backups    (cluster-wide)
    └── Backup policies ─ Policy
```

Snapshots and backups are one page reached from three scopes; the parent
breadcrumb segment *is* the filter, and the toolbar shows a scope pill.

**Hosts** are physical/virtual machines that have been prepared and labelled;
they appear in the list automatically. A host carries NUMA socket count, its
PCIe-NVMe devices per socket (assigned to a storage node or free), unused
unpartitioned Linux block devices, vCPU, system RAM, hugepages
allocated/reserved, and links to the 0–2 storage nodes it runs. It may also run
control plane services (both, on Kubernetes).

## Deploying a cluster

The operator model has four stages, and the first two are deliberately outside
this console:

```
1  helm install the control plane          outside the UI
2  install the operator on each cluster    outside the UI
3  discover the hardware                   OperatorOps{action: Discover}
4  draft -> review -> approve -> deploy    ClusterDeploymentConfig
```

**Discovery** (Kubernetes > a cluster > *Discovery & deployment*) deploys an
inspection pod on every matching worker node and reports its NUMA topology,
vCPU, memory, NICs and every unmounted, unused device — block device name,
serial, capacity, model, and PCIe address for NVMe. Filters apply *during*
discovery, so an excluded device is never reported and a boot disk cannot end up
in a node set by accident:

| Filter | Field |
|---|---|
| smallest / largest device | `deviceFilter.driveSizeRange{min,max}` |
| PCIe allow / deny | `deviceFilter.pcieAllowList`, `pcieDenyList` |
| block device names | `deviceFilter.blockDeviceNames` |
| unpartitioned Linux block devices | `deviceFilter.enableLogicalBlockDevices` |
| which workers to inspect | `nodeSelector.matchLabels` |

Discovery reports every worker node that carries no storage node yet; deciding
which of them to *use* happens in the wizard, not in discovery.

**The wizard** (*Draft a cluster deployment*) writes a `ClusterDeploymentConfig`
with `spec.approved: false`. Hosts are chosen canonically or by node label;
devices per host can be dropped individually; NUMA sockets are picked per host —
**one storage node runs per socket**, so the socket selection is what decides the
node count. Sizing is either `maxSubsystemCount` with the hugepage reservation
derived from it, or the reservation set directly:

```
minHugePagesSize = ceil((2 + 0.25 × maxSubsystemCount) / 2) × 2 GB
```

Core isolation is a per-group flag; CPU topology is enforced either way.

**Approval is one-way** and is the only mutable field on the document
(`PATCH spec.approved`; anything else is refused 422). It starts four
asynchronous, observable steps:

| Step | What changes |
|---|---|
| `CreateCluster` | the cluster exists, status `unready` |
| `PrepareNodes` | persistent hugepage reservation + core isolation on each host, hosts become `available` |
| `AddStorageNodes` | one storage node pod per group, devices claimed, cluster `in_activation` |
| `ActivateCluster` | nodes and devices online, default pool created, cluster `online` |

A document in flight cannot be deleted (403 from the webhook — it would strand
half-configured nodes). A document naming `spec.clusterRef` instead of
`spec.cluster` is a **growth** document against an existing cluster.

## S3 buckets

One bucket is one filesystem is one logical volume, so a bucket inherits
everything a volume can do. The bucket list (Clusters > a cluster > Buckets)
supports:

- **Create** — name (S3 naming rules enforced), pool, size, quota, versioning,
  object lock (one-way; a locked bucket cannot be deleted), KMS encryption,
  default storage class, owner and up to 50 **bucket tags**.
- **Delete** — refused while the bucket holds objects or object lock is on,
  matching S3 semantics. The backing volume goes with it.
- **Replication** — *Replicate → attach to a policy* joins the bucket's volume
  to a replication policy whose source is the bucket's cluster (sync across
  zones or async to a paired cluster); *Detach* leaves it. The tile shows the
  mode, the detail links to the policy.
- **Search and browse by S3 metadata** — the toolbar filters by tag (`key`
  matches any bucket carrying it, `key=value` exact), by region, and by
  configuration (versioned, object lock, encrypted, public, replicated / not
  replicated, has lifecycle rules). Free-text search also matches tags, owner
  and storage class. The detail shows tags, region, storage class, owner, CORS
  and the lifecycle rules.

```
POST   /clusters/{uuid}/buckets            create (tags, storage_class, owner)
PUT    /buckets/{uuid}                     versioning · object_lock · quota · storage_class
PUT    /buckets/{uuid}/tags                replace the tag set (≤ 50)
PUT    /buckets/{uuid}/access              service account · policy · public · rotate key
POST   /buckets/{uuid}/resize              grow only
POST   /buckets/{uuid}/replicate           {policy_id} — source must be this cluster
POST   /buckets/{uuid}/unreplicate
DELETE /buckets/{uuid}                     409 while non-empty or locked
```

## Hosts and preparing Kubernetes worker nodes

The Hosts page of a Kubernetes cluster lists **both** prepared storage hosts and
the cluster's worker nodes that are not prepared yet. A host moves through:

```
discovered  →  inspecting  →  ready to configure  →  available
(k8s worker)   (inspection    (inventory known)      (labelled, can
                pod running)                          run storage nodes)
```

- **discovered** — the worker node is visible but simplyblock knows nothing about
  its hardware. Candidate tiles are hatched, carry a checkbox, and show only what
  the Kubernetes node object provides (kubelet, instance type, zone, vCPU, RAM).
- Select any number of them and hit **Prepare n nodes** — `POST
  /clusters/{uuid}/hosts/prepare {host_ids[]}` schedules the inspection pod.
- **inspecting** — the pod collects the NUMA topology, the NVMe/block devices and
  the NICs. The tile shows the pod name and a live indicator.
- **ready to configure** — the inventory is in. **Configure** opens the form:
  NUMA socket(s) to use, system memory per storage-plane pod, hugepages per pod,
  the devices to assign, the management NIC and the data NIC(s) — all picked from
  what was actually discovered. `POST /hosts/{uuid}/configure`.
- **available** — labelled `simplyblock.io/storage-node=true`; a storage node can
  now be started on it, or a node migrated onto it.

## Edge clusters

Edge is a materially different deployment, and the UI reflects that rather than
showing dead controls:

- **no rebalancing** — the tile shows `rebalancing n/a`, the detail page says so.
- **no task engine** — the Tasks tab is not rendered, and
  `GET /clusters/{uuid}/tasks` answers **501** with an explanation.
- **block device class only** — NVMe is not offered; the create-cluster form drops
  the option as soon as you pick Edge, and the API rejects it.
- **managed through the Kubernetes API** — the endpoint row is labelled
  *Kubernetes API*, not *Management endpoint*.

There is no separate optimization field; the device class alone describes the
cluster. Capabilities are carried on the cluster object:
`{snapshot_replication, async_replication, rebalancing, tasks}`.

## Contexts: several Kubernetes clusters

The console manages storage across several Kubernetes clusters. Each is a named
**context** in `SB_CONFIG.contexts`: the browser reaches its API server through
the same-origin proxy at `k8sBase` (`/k8s` for the local cluster, `/k8s/<name>`
for a remote one) and its control plane at `cpBase` (`/cp/<name>`). The console
pod's nginx renders those locations from a contexts file and injects the right
credential per context; the browser never holds one. A context without `cpBase`
has no control plane reachable from the console: CRD views still work, and
tasks, the event log, and live I/O say so instead of failing.

```js
window.SB_CONFIG = {
  k8sBase: "/k8s", cpBase: "/cp/local",
  contexts: [
    {name: "local", labels: {site: "fra1", stage: "prd", tenant: ""}, k8sBase: "/k8s", cpBase: "/cp/local", namespaces: "all"},
    {name: "edge-07", labels: {site: "edge-07", stage: "prd"}, k8sBase: "/k8s/edge-07", cpBase: "", namespaces: ["simplyblock"]}
  ],
  features: {zones: false, buckets: false, cgroups: false, migrationPaths: false, fileStorage: false, objectStorage: false, fdbBackups: false, accessRoles: false}
};
```

The local context reads every namespace its role grants (a tenant-per-namespace
layout keeps each tenant's clusters apart); the console's own namespace
(`SB_CONFIG.namespace`) is only where it creates objects that name no other home.

## Fleet labels: site, stage, tenant

The dashboard groups clusters by site, lifecycle stage, tenant, or Kubernetes
context. The console reads those from, in order of precedence:

1. StorageCluster labels `storage.simplyblock.io/site`, `storage.simplyblock.io/stage`,
   `storage.simplyblock.io/tenant` (site may be comma-separated for a stretched cluster)
2. the same labels on the cluster's Namespace (tenant defaults to the namespace name)
3. the context's configured labels
4. for site only: the `topology.kubernetes.io/zone` (then `region`) labels of the
   cluster's worker Nodes, so **sites are tagged nodes**; a cluster whose workers
   span several zones is stretched and listed under each

Every derived value shows where it came from. Stages sort `DEV < UAT < STAGING < PRD`;
unknown names follow alphabetically.

## The surfaces

| Surface | `SB_CONFIG` key | Serves |
|---|---|---|
| Kubernetes API, per context | `contexts[].k8sBase` | CRDs in `storage.simplyblock.io`; core objects (Node, Pod, PV, PVC, StorageClass, VolumeSnapshot, Event, Namespace, CustomResourceDefinition); pod logs; watches |
| Control plane v2 API, per context | `contexts[].cpBase` | tasks, the cluster event log, capacity and I/O statistics, device health (SMART), live state (`?watch=true`, SSE); the one write is canceling a migration |
| Prometheus | `promBase` | SPDK thread utilization |
| Helm | `helmBase` | release, revision, chart, values |
| Operator `/proposed/*` | `operatorBase` | nothing in a deployment; the fixture backend still serves the views behind the feature flags |

What each view reads:

| Data | Source |
|---|---|
| Clusters, nodes, devices, pools, node sets, backups, policies, operations | the CRDs, joined by `spec.clusterName`, `spec.storageNodeSetRef`, `spec.nodeRef` |
| Hosts | Kubernetes Nodes, joined to storage nodes by the `simplyblock.io/storage-node-uuid.<cluster>.<socket>` labels and `spec.workerNode` |
| Logical volumes | PersistentVolumes of `csi.simplyblock.io`, their claims, their StorageClass, and the v2 volume by handle `<cluster>:<pool>:<volume>` |
| Snapshots | VolumeSnapshots and their VolumeSnapshotContents, joined by the snapshot handle |
| Storage classes, PVCs, Kubernetes clusters | the real objects; a "Kubernetes cluster" is a context |
| Tasks, the event log, live I/O, SMART | the context's control plane |
| Control plane services and their logs, storage plane logs | the pods (`simplyblock-*`, the SPDK pod `snode-spdk-pod-<rpc>-<cluster>`, the node API DaemonSet pod) and the pod-log subresource |
| Alerts | derived in the console from all of the above (`alerts.jsx`) |

Anywhere data comes from a container's stdout or from Prometheus the UI carries a
dashed **source tag**, because it has a different availability guarantee than an
object the API server stores.

### Going live

1. Drop the `mock-k8s.jsx`, `mock-k8s-server.jsx` and `mock-extras.jsx` script tags.
2. Set config at container start (nginx `envsubst` on the inline `SB_CONFIG` block):

```js
window.SB_CONFIG = {
  k8sBase: "",                       // same-origin through the API server proxy
  operatorBase: "/operator/v1",
  helmBase: "/helm/v1",
  namespace: "simplyblock",
  promBase: "/prometheus/api/v1",
  token: "<serviceaccount token>", mock: false
}
```

The service account needs `get/list/watch` on the CRDs, on core Node/Pod/PV/PVC/
StorageClass/Event, on `pods/log`, and `create/patch/delete` on the `*Ops` kinds.

### CRD reads

```
GET /apis/storage.simplyblock.io/v1alpha1/namespaces/{ns}/storageclusters
                                                        /storagenodes
                                                        /storagedevices
                                                        /storagepools
                                                        /storagebackups
                                                        /controlplanes
                                                        /simplyblockdrivers
                                                        /clusterdeploymentconfigs
```

Children are filtered with label selectors rather than nested paths:

```
?labelSelector=storage.simplyblock.io/cluster=<name>
?labelSelector=storage.simplyblock.io/owner-kind=StorageNode,…/owner-name=<name>
```

Core objects: `/api/v1/nodes`, `/api/v1/persistentvolumes`,
`/api/v1/namespaces/{ns}/persistentvolumeclaims`, `/apis/storage.k8s.io/v1/storageclasses`,
`/api/v1/namespaces/{ns}/pods/{name}/log`.

A logical volume **is** a core `PersistentVolume`. Everything simplyblock-specific
about it travels in `spec.csi.volumeAttributes`.

### Actions are objects, not verbs

Every long-running action creates an `<Entity>Ops` object:

```
POST /apis/.../storagenodeops     {spec: {action: "Restart", targetRef: {name}}}
```

| Kind | Actions |
|---|---|
| `StorageClusterOps` | Suspend · Activate · Expand · Rebalance |
| `StorageNodeOps` | Restart · Shutdown · Migrate · Remove · AddDevice |
| `StorageDeviceOps` | Restart · Fail · Remove · HealthCheck |
| `StoragePoolOps` | Enable · Disable · UpdateQos |
| `StorageBackupOps` | Merge · Restore · Export · Delete |
| `PersistentVolumeOps` | Resize · Snapshot · Clone · Migrate · Backup |
| `ControlPlaneOps` | Restart · RestoreStateDatabase |
| `OperatorOps` | Discover |
| `ClusterDeploymentConfig` | drafted by discovery, deployed by `spec.approved: true` |

Rules the console honours:

- `status.phase` and `status.step.state` carry progress; `status.events` the history.
- **`spec.abort` is the only stop.** A cancel is `PATCH {spec:{abort:true}}`, never a
  DELETE. Every other spec field is immutable and a patch touching one is refused.
- `status.activeOpsRef` on the entity is a lock — a second operation against the
  same entity is refused with **409**.
- DELETE follows the admission webhook: terminal admitted, abortable step unwound,
  a step with no abort edge **refused** rather than stranding the entity.

### Kinds with no CRD yet

These run on `operator /proposed/<collection>`, scoped with
`?scope=<parent-collection>&scopeId=<uuid>`:

replication policies · cluster pairs · DR clusters · application DR policies ·
protected apps · consistency groups and their snapshots · migrations · buckets ·
zones · backup policies · snapshots · alerts · tasks · cluster event log

That list is the set of CRDs the model still owes the console, which is why the
prefix is explicit rather than hidden. Two are worth challenging before they become
new kinds: a **task** is very nearly an `Ops` object, and the **cluster event log**
overlaps core `Event`. See `CRD-CROSSCHECK.md`.

Auth: `Authorization: Bearer <token>`. Failures are Kubernetes `Status` objects, and
the console shows `.message` verbatim. Actions that only make sense in certain states
are hidden or disabled with a reason, so the API is never called in a state it would
reject.

### Field mapping — verify against your OpenAPI spec

`api.jsx` → `normCluster / normHost / normNode / normDevice / normPool / normVolume /
normSnapshot / normBackup / normPolicy` is the single place where wire field names
appear. Names were inferred from the public docs and CLI vocabulary; adjust there
if v2 differs. Some responses need **expansions** so tiles render without N+1 calls:

- `lvol.pool_name`, `lvol.nodes = {primary|secondary|tertiary: {uuid, hostname}}`,
  `lvol.base_snapshot`, `lvol.backup_policy`, `lvol.replication_group`
- `host.devices[] = {id, kind, numa_socket, pcie_address, device_name, size, assigned_node_id}`
- rollup counters on parents: cluster `hosts_count / storage_nodes_count /
  storage_nodes_online / devices_count / devices_online / pools_count / lvols_count /
  snapshots_count / backups_count / backup_policies_count`; node `devices_count /
  devices_online`; pool `lvols_count / snapshots_count / backups_count`; lvol
  `snapshots_count / backups_count`
- `io_stats` (`read_io_ps`, `write_io_ps`, `read_bytes_ps`, `write_bytes_ps`) and a
  short `io_history` window per object for the sparklines

If rollups or history are not available on the list endpoints, they can be dropped
to a second `/iostats` call — the view models already isolate them.

## Behaviour

- **Polling** — the active view refetches every 6 s without a skeleton flash; every
  successful action forces an immediate refresh.
- **Breadcrumb** is the only navigation state; persisted to `localStorage` and
  restored on reload. Ancestor labels resolve through a UUID registry, so a deep
  link works cold.
- **Sorting** defaults to *unhealthy first* (status severity rank) for
  infrastructure and *newest first* for snapshots and backups; plus name,
  utilization, capacity, IOPS, throughput, size. Status filter chips carry counts.
- Keyboard: `/` focuses search, `Esc` goes up one breadcrumb level.

## Mock controls (design + validation)

The **mock api** menu in the header injects slow network, offline, empty
collections, random 503s, and a single failure — loading, empty and error states
are reachable without a real cluster. Mutations write to the fixture store, so
shutting a node down really does turn its devices amber.

## Disaster recovery


> **Superseded for the top layer (2026-09-29).** Protection plans, sites and
> protected applications are no longer read from `/operator/v1/proposed/*`:
> the console is a client of the DR hub's CRDs (`dr.simplyblock.io/v1alpha1`),
> which model sites inside the ProtectionPlan, declare directions as `DRPath`
> objects, and run Failover / Relocate / Restart as `RecoveryAction`s and
> rehearsals as `TestBubble`s. The section below documents the earlier
> proposal and stays for the storage-side replication kinds it also covers.

Two objects are authored. Everything under them is derived, and shown read-only
because its fields are immutable.

```
ProtectionPlan        cluster-scoped   sites[] · storageProfile · methods[]
ProtectedApplication  namespaced       planRef · pvcSelector · consistencyGroup
                                       preferredSite · orchestratedMethod
```

A **method** is one declared protection relationship. All three types are
configured identically and travel the same control path down to the CSI driver;
what differs is the parameter map the replication class carries, and therefore
what the driver does with the data.

| Method | Class mode | RPO | RTO | Generation select | Failback | Driver-side target |
|---|---|---|---|---|---|---|
| synchronous | `sync` | 0 | seconds | no | yes | peer cluster, inline write mirror |
| asynchronous | `async` | = interval | seconds | no | yes | peer cluster, block delta per epoch |
| generation vault | `snapshot-s3` | = interval | minutes | **yes** | **no** | S3 bucket, immutable objects + manifest |

Navigation: `Disaster recovery › Protection plans › Plan › {Applications, Sites}`
and `… › Applications › Application`. A site is one managed cluster; its name
must equal the OCM ManagedCluster name, and its **region** is how sync versus
async is declared — equal region on both sides of a pair permits a synchronous
mirror, distinct region does not.

### Derived objects

The plan detail shows what the orchestrator produced, with a cardinality box:

| Kind | Cardinality | Why |
|---|---|---|
| `DRCluster` | one per site | created once at onboarding |
| `DRPolicy` | one per pair × interval | every field immutable, so every pair a method could ever use is created up front |
| `DRPlacementControl` | one per application | the only object rewritten during operation; a rebind is a delete plus a create |
| `VRClass` / `VGRClass` | one per method × interval | not a Ramen object, but the switch Ramen reads |

**The one derivation that fails silently.** `methods[].interval` is written to
both `DRPolicy.spec.schedulingInterval` and the class's
`parameters.schedulingInterval`. If they differ by even formatting, no class
resolves, the policy still validates cleanly, **no peerClass appears**, and the
application is protected by nothing. The console treats an absent peerClass as
the authoritative signal, banners it on the plan and on the DR landing page, and
offers a fix that emits one value to both places. The `bronze` fixture plan is
deliberately broken so the state is visible.

### One placement control per application

A DRPC selects PVCs by label, so two over the same PVCs would both claim them —
the documented outcome is data corruption. `orchestratedMethod` therefore names
the single method Ramen drives; the rest keep replicating in the data plane with
identical parameters. Switching it rebinds the placement control and touches no
data. The vault method never becomes the orchestrated one except for the
duration of a restore.

### The side channel

Everything expressible in a Ramen resource goes through Ramen. Six payloads have
no field anywhere in its API, and two of them are reads the console cannot be
truthful without — read from Kubernetes alone, a plan with three declared
methods shows a healthy single-leg posture and no generations at all.

| # | Payload | Direction | Consumed by |
|---|---|---|---|
| 1 | peer / topology binding | down | driver, at site onboarding |
| 2 | generation selection | down | `PromoteVolume` — it has no point-in-time argument |
| 3 | retention and lock enforcement | both | object store, which owns the state |
| 4 | **generation catalogue** | up | **this console** — no CR represents a generation |
| 5 | **per-leg lag** | up | **this console** — only the orchestrated leg has a VRG |
| 6 | arbitration token | both | quorum across three or more sites, which `clusterFence` cannot express |

### Restoring from a generation

The ransomware path, and the only flow that is not ordinary Ramen failover:

```
select generation → pin out of band → rebind DRPC to the vault policy
  → VRG primary at the restore target → driver materialises volumes from the
  pinned objects → re-protect as a new plan with a full baseline
```

The pin is cleared once the promote resolves; a stale pin would make the next
ordinary failover resolve to an old generation. Afterwards failback is gone and
the application is flagged for re-protection: the original site's pre-compromise
state cannot be reconstructed, because the source volume is gone or untrusted
and the vault holds generations rather than a live peer.

Generations carry an integrity state and an Object Lock expiry. An unverified
generation is refused for restore until it is verified, and a locked generation
cannot be deleted before its lock expires — the guarantee the vault exists for.

```
GET    /protection-plans                          plan list
GET    /protection-plans/{uuid}                   + derived policies and classes
GET    /protection-plans/{uuid}/protected-apps | /sites
POST   /protection-plans                          {name, site_names[], storage_profile}
POST   /protection-plans/{uuid}/methods           {name, type, target, interval, retention*, immutable, bucket}
PUT    /protection-plans/{uuid}/methods/{name}/interval   {interval}   both places at once
DELETE /protection-plans/{uuid}/methods/{name}
DELETE /protection-plans/{uuid}                   refused while applications are bound
GET    /dr-sites | /dr-sites/{uuid}               DRCluster projection
GET    /dr-sites/{uuid}/protected-apps | /protection-plans
POST   /dr-sites/{uuid}/fence | /unfence
GET    /dr-arbitration                            payload 6
GET    /protected-apps/{uuid}/generations         payload 4
POST   /protected-apps                            {plan_id, preferred_site, orchestrated_method, …}
PUT    /protected-apps/{uuid}/orchestrated-method {method}             rebind
POST   /protected-apps/{uuid}/restore             {generation}         pin, then rebind
POST   /protected-apps/{uuid}/verify-generation   {generation}
POST   /protected-apps/{uuid}/failover | /relocate | /cleanup
PUT    /protected-apps/{uuid}/recipe
```

Storage-level replication policies and cluster pairs still exist below this
layer — they are what the driver implements, and a volume's `replication` block
points at one — but they are no longer the top-level DR object.

## Open

- Cluster-level *site migration* is expressed today as "put every volume in a
  migration group and cut over". If you want a single "migrate this cluster"
  wizard that creates the group, tracks all volumes and reports one progress
  number, say so and I will add it.
- Replication is cluster-scoped in the navigation, as you chose. An all-clusters
  replication overview would be a small addition if operators want one pane.
- Cluster and node **creation** is in: `New cluster` on the Clusters overview builds
  on prepared, unassigned hosts (`GET /hosts/unassigned`); storage nodes are added
  from the cluster tile (*Expand*), the host tile, or automatically with the cluster.
