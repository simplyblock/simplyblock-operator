# sb-mock — the Control Center's test backend

One process that impersonates every upstream the console proxies, for one or
several Kubernetes contexts, so the **real, unmodified UI** can be exercised
against generated data:

```
console ──► sb-mock ──┬── Kubernetes API      /k8s/<ctx>/   (simplyblock CRDs, Ramen CRDs, core objects, CRDs)
                      ├── control plane v2    /cp/<ctx>/    (clusters, nodes, devices, pools, volumes, snapshots,
                      │                                      tasks, event log, capacity, iostats, SSE watch)
                      ├── operator API        /operator/    (fixture-driven stub)
                      └── Prometheus          /prometheus/  (deterministic generated series)
```

Reads come from generated worlds. **Writes persist** — create/update/patch/
delete land in the in-memory store, bump `resourceVersion`, and fire watch
events — but perform no underlying change. A simulator per world plays the
part of every reconciler and of the control plane's task runners: Ops objects
move through `Pending → Running → Succeeded/Failed`, `spec.abort` is honored,
nodes, devices, and clusters change status as the Ops act on them, control
plane tasks advance, DRPC failovers progress, replication slots cut over, and
every transition lands both as a Kubernetes Event and as a control plane
event-log record.

> **Hub/agent, multi-tenant, drift-managed paradigm.** The world is shaped to
> the CRD-redesign + multi-cluster RBAC design (namespaces `sb-<tenant>` /
> `sb-mc-*`, hierarchy labels, the `sb:*` aggregated roles, `AccessGrant`
> / `NodePoolAllocation` / `ManagedCluster`, actor-stamp annotations, a hub→
> transport StorageCluster projection with agent-authored status, and
> `observedGeneration` applied/pending drift). **Almost none of this is on
> `main`** — see [PARADIGM.md](PARADIGM.md) for what the mock reproduces
> faithfully and where it necessarily stands in.

## Running it

```sh
go run . --dataset dr-failover --seed 42          # reproducible single-context world
go run . --serve-ui .. --dataset fleet-edge-sites  # + serve the console itself against eight edge contexts:
                                                   #   open http://localhost:8080 — no nginx needed
go test ./...
```

| Flag                  | Default       | Meaning                                                                                                                                                                                                                            |
|-----------------------|---------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--listen`            | `:8080`       | listen address                                                                                                                                                                                                                     |
| `--dataset`           | `auto`        | a scenario (`small-healthy`, `medium-degraded`, `large-scale`, `dr-failover`, `chaos`), a fleet (`fleet-stages`, `fleet-tenants`, `fleet-namespaces`, `fleet-edge-shared`, `fleet-edge-sites`), or `auto` (seeded random scenario) |
| `--seed`              | random        | same seed + dataset = byte-identical worlds; every context of a fleet derives its own seed from this one and its name                                                                                                              |
| `--namespace`         | `simplyblock` | the control-plane (system) namespace; storage objects live in the tenant namespaces, not here                                                                                                                                      |
| `--sim-interval`      | `4s`          | simulator tick; `0` disables it (drive ticks via `/mockctl/advance`)                                                                                                                                                               |
| `--fail-rate`         | `0.1`         | probability a simulated operation or task ends `Failed`                                                                                                                                                                            |
| `--viewer`            | `global`      | authorization stand-in for the impersonated user: `global`, `none`, or `<sb:role>@<namespace>[,...]` — see PARADIGM.md                                                                                                             |
| `--serve-ui`          | —             | directory with the console's `index.html`; also mounts `/k8s`, `/cp`, `/operator`, `/prometheus` and generates `/config.js`                                                                                                        |
| `--crd-dir`           | auto          | directory with the operator's CRD YAML; by default `operator/config/crd/bases` is found relative to the working directory or the source tree                                                                                       |
| `--operator-fixtures` | —             | JSON fixtures for the operator API (below)                                                                                                                                                                                         |

### Access / authorization endpoints

The console's access screen and read/write path are answered from the `--viewer`
identity (a stand-in for impersonation — see PARADIGM.md §"Real identity"):

- `POST /apis/authorization.k8s.io/v1/selfsubjectrulesreviews` — the viewer's rules in a namespace
- `POST /apis/authorization.k8s.io/v1/subjectaccessreviews` (and `selfsubjectaccessreviews`) — allow/deny
- `GET /operator/v1/access/scopes` — the scope tree the viewer may see
- `GET /operator/v1/access/self` — aggregated self-rules per namespace

### Against the console pod

The console's env vars already support pointing its proxy anywhere, so no UI
or image change is needed:

```
SB_K8S_API=http://sb-mock:8080
SB_CP_URL=http://sb-mock:8080
SB_OPERATOR_URL=http://sb-mock:8080
SB_PROMETHEUS_URL=http://sb-mock:8080
```

With the chart: `--set controlCenter.enabled=true --set controlCenter.mock.enabled=true`
deploys the mock next to the console and wires those overrides automatically.

## Contexts: the two route families

A world is one Kubernetes context. The root routes (`/api`, `/apis`,
`/version`, `/api/v2/`, `/api/v1/query`) serve the **local** world: the context
named `local`, else the first context of the fleet. Every world is also served
under its own prefixes, which is what the console configured with several
contexts uses:

| Route                        | Serves                                                                        |
|------------------------------|-------------------------------------------------------------------------------|
| `/k8s/`                      | the Kubernetes API of the local world (`/k8s/apis/...`)                       |
| `/k8s/<ctx>/`                | the Kubernetes API of context `<ctx>` (`/k8s/edge-03/apis/...`)               |
| `/cp/<ctx>/api/v2/...`       | the control plane v2 API of context `<ctx>` (`/cp/local/` is the local world) |
| `/operator/`, `/prometheus/` | the operator stub and Prometheus of the local world                           |

`api`, `apis`, `version`, `openapi`, `healthz`, `livez`, `readyz`, `metrics`,
and `local` cannot name a context.

When `--serve-ui` is set, `/config.js` carries what the console needs:
`k8sBase: "/k8s"`, `cpBase: "/cp/local"`, one `contexts[]` entry per world
(`name`, `labels`, `namespaces` that hold StorageClusters, `k8sBase`
`/k8s/<name>` or `/k8s` for `local`, `cpBase` `/cp/<name>`, `local`), and
`features` with every fixture-only view switched on (`zones`, `buckets`,
`cgroups`, `migrationPaths`, `fileStorage`, `objectStorage`, `fdbBackups`,
`accessRoles`), so the design preview keeps those views.

## What the Kubernetes side implements

- discovery (`/api`, `/apis`, `/apis/{g}/{v}`) for every registered type
- list / get / create / update / delete, with proper `Status` errors
- patches: JSON merge patch (RFC 7386), JSON patch (RFC 6902 —
  add/remove/replace/test), apply patch (create-or-merge); strategic merge is
  treated as a JSON merge — list-merge semantics are not reproduced
- `?watch=true` streaming (newline-delimited events, heartbeats, 410 on
  overflow so clients relist), label selectors (equality, existence, set-based)
  and dotted-path field selectors
- `/status` subresources (writes apply to the object) and `pods/{name}/log`
- `metadata.generation` bumps only on spec changes; UI-created objects get
  fresh `uid`/`creationTimestamp`
- `customresourcedefinitions` served from the operator's generated CRD YAML
  (`operator/config/crd/bases`), so the console reads the real
  `x-kubernetes-validations` (without the directory the list is empty)

Registered types are the union of the console's ClusterRole: all shipped
simplyblock CRDs, the **proposed** kinds with no CRD yet (they serve empty
lists — an empty list beats a 404 for a UI), the Ramen and OCM kinds, the
snapshot kinds, and the core/workload objects. The list lives in `registry.go`.

### Real-world object formats

The generator writes the formats the operator and the CSI driver produce,
because the console joins on them:

- PV `spec.csi.volumeHandle` = `<clusterUUID>:<poolUUID>:<lvolUUID>`, and
  VolumeSnapshotContent `status.snapshotHandle` = `<clusterUUID>:<poolUUID>:<snapshotUUID>`
- StorageClass `simplyblock-<ns>-<cluster>-<pool>` with labels
  `storage.simplyblock.io/namespace|cluster|pool` and parameters `cluster_id`, `pool_name`
- worker Nodes labeled `io.simplyblock.node-type`, `io.simplyblock.storagenodeset`,
  `simplyblock.io/storage-node-uuid.<clusterUUID>.<socket>=<nodeUUID>`, and
  `topology.kubernetes.io/zone|region`. StorageNodes are labeled
  `storage.simplyblock.io/storagenodeset|worker` and carry `status.ports.rpc`
- the node API pod (StorageNodeSet DaemonSet, cluster namespace): labels
  `app=storage-node`, `simplyblock-cluster=<cluster>`, `io.simplyblock.storagenodeset=<set>`,
  container `s-node-api-container`. The SPDK pod (control plane namespace) is
  `snode-spdk-pod-<rpcPort>-<clusterUUID>`, labels `role=simplyblock-storage-node`,
  `app=spdk-app-<rpcPort>`, containers `spdk-container` and `spdk-proxy-container`,
  host networked and pinned by `kubernetes.io/hostname`
- control plane Deployments `simplyblock-admin-control`, `simplyblock-webappapi`,
  `simplyblock-monitoring`, `simplyblock-tasks`, `simplyblock-fdb-controller-manager`,
  `simplyblock-operator` (selector `app=<name>`), FDB pods labeled
  `foundationdb.org/fdb-cluster-name`, every pod annotated `log-collector/enabled: "true"`
- StorageClusters labeled `storage.simplyblock.io/site|stage|tenant` when the
  scenario sets them. StorageDevice `spec.deviceID` is the backend device UUID

## What the control plane side implements

`/api/v2` is the read half of `shared/openapi.json`, projected from the store
and shaped like its DTOs (snake_case fields). `GET` only. A bearer is accepted
and never checked (logged once when absent). Unknown routes answer
FastAPI-style `{"detail": ...}`.

| Route                                                                       | Source                                                                                                                                                                                                   |
|-----------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/clusters/`, `/clusters/{id}/`                                             | user-facing StorageClusters (`status.uuid` is the id), capacity summed from the nodes                                                                                                                    |
| `/clusters/{c}/storage-nodes/`, `.../{n}/`                                  | StorageNodes of the cluster (via its StorageNodeSet)                                                                                                                                                     |
| `.../storage-nodes/{n}/devices/`, `.../{d}/`                                | StorageDevices by `status.nodeID`, and `.../{d}/health-info` is a SMART page, deterministic per device                                                                                                   |
| `/clusters/{c}/storage-pools/`, `.../{p}/`                                  | StoragePools by `spec.clusterName`                                                                                                                                                                       |
| `.../storage-pools/{p}/volumes/`, `.../{v}/`                                | PVs whose handle starts with `<c>:<p>:`, joined with their PVC (`pvc_name` = `<ns>/<name>`)                                                                                                              |
| `.../storage-pools/{p}/snapshots/`, `.../{s}/`, `.../volumes/{v}/snapshots` | VolumeSnapshotContents by handle, with the source lvol                                                                                                                                                   |
| `/clusters/{c}/tasks/`, `.../{t}/`                                          | control plane tasks: masters `node_restart`, `device_restart`, `node_add` and sub-tasks naming their master in `function_params.parent_task_id`, with statuses `new`, `running`, `suspended`, and `done` |
| `/clusters/{c}/logs?limit=`                                                 | the event log, newest first: `Date`, `NodeId`, `Event`, `Level`, `Message`, `Storage_ID`, `VUID`, `Status`                                                                                               |
| `.../capacity?history=`, `.../iostats?history=`                             | for cluster, node, device, pool, and volume: one latest record, or a seeded series over `history` (`30m`, `2h`, `1d`)                                                                                    |
| `/management-nodes/`                                                        | the control-plane Kubernetes nodes                                                                                                                                                                       |
| `/_meta/health`, `/_meta/ready`                                             | `204`                                                                                                                                                                                                    |

`?watch=true` on every list and detail route streams Server-Sent Events:
`retry: 3000`, `event: snapshot` first (the array, or the object), then
`created` / `updated` / `deleted` mapped from the store's watch, a `: ping`
comment every 15 s, and a close after one hour.

The event log carries `STATUS_CHANGE` records (`Storage node status changed
from: X to: Y`, `Device status changed from: X to: Y`, `Cluster status changed
from: X to: Y`), `node_status`, `device_status`, `CAPACITY` (`Cluster capacity
utilization reached: NN%`), and journal manager (`JM_*`) events. The generator
seeds the cluster's history, and the simulator appends every transition it makes.
The iostats and capacity record shapes are not typed in the OpenAPI document,
so they follow the control plane's stat objects (`date`, `size_total`,
`size_used`, `size_free`, `size_util`, `size_prov`, `read_bytes_ps`,
`write_io_ps`, `read_latency_ps`, and so on).

## Datasets

A dataset is a parameterized topology (`datasets.go`) rendered into a coherent
object graph (`gen.go`): devices reference their nodes, slots their PVCs,
DRPCs their DRPolicy, PVs their PVCs and their pool, and the Helm release
Secret is a structurally real `helm.sh/release.v1` payload (base64-in-base64,
gzipped JSON). Names, NVMe models, capacities and statuses come from seeded
pools — `--dataset auto` picks a scenario at random, and any run is
reproducible from its logged `dataset=… seed=…` line.

A scenario can hold several storage clusters (`Clusters`, or one per
`ClusterNamespaces` entry). Clusters sharing a namespace get a name prefix so
their nodes, pools, and ops never collide.

A **fleet** is one world per context, with the labels the console falls back
to when neither the StorageCluster nor its Namespace carries site, stage, or
tenant:

| Fleet               | Contexts              | Layout                                                                                      |
|---------------------|-----------------------|---------------------------------------------------------------------------------------------|
| `fleet-stages`      | `dev`, `uat`, `prd`   | one cluster each, context label `stage`                                                     |
| `fleet-tenants`     | `hub`                 | one cluster per `sb-acme`, `sb-globex`, `sb-initech` namespace, tenant label on the cluster |
| `fleet-namespaces`  | `shared`              | clusters in `sb-blue` and `sb-green`                                                        |
| `fleet-edge-shared` | `edge`                | twelve two-worker clusters, each with its own site label and worker zone                    |
| `fleet-edge-sites`  | `edge-01` … `edge-08` | one small cluster per context in the `simplyblock` namespace, context label `site`          |

A plain scenario is a one-context fleet named `local`.

## The admin API

| Endpoint                                          | Purpose                                                                                                                                                  |
|---------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `GET /mockctl/info`                               | dataset, seed, object counts of the local world, every context (labels, scenario, seed, object counts, clusters), and **unmocked operator-API requests** |
| `POST /mockctl/reset?dataset=fleet-stages&seed=7` | rebuild every world (watches are closed so clients relist, and contexts that come or go are added or removed)                                            |
| `POST /mockctl/advance?ticks=5&ctx=prd`           | run simulator ticks on demand, in every world or one (run with `--sim-interval=0` for fully deterministic e2e tests)                                     |

## Operator-API fixtures

The `/operator/v1/*` surface is still settling, so it is fixture-driven: a
request for `GET /v1/foo/bar` serves `<fixtures>/v1/foo/bar.json` (then
`bar/index.json`; writes first try `bar.post.json` etc.). Anything unmatched
answers `{"status":"success","data":[]}` and is recorded — check
`/mockctl/info`'s `unmockedOperatorRequests` after clicking through the UI to
see exactly which endpoints still need fixture files. That list is the to-do
list.

## What is deliberately not implemented

- authentication/authorization — every request succeeds; RBAC is not simulated
- control plane writes (`POST`, `PUT`, and `DELETE` under `/api/v2` answer `405`)
- strategic-merge list semantics, server-side-apply field ownership
- `resourceVersion` preconditions on writes (no optimistic-concurrency 409s)
- protobuf negotiation; responses are always JSON
