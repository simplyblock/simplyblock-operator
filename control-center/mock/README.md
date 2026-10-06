# sb-mock — the Control Center's test backend

One process that impersonates all three upstreams the console proxies, so the
**real, unmodified UI** can be exercised against generated data:

```
console ──► sb-mock ──┬── Kubernetes API   (simplyblock CRDs, Ramen CRDs, core objects)
                      ├── operator API     (fixture-driven stub)
                      └── Prometheus       (deterministic generated series)
```

Reads come from a generated world. **Writes persist** — create/update/patch/
delete land in the in-memory store, bump `resourceVersion`, and fire watch
events — but perform no underlying change. A simulator plays the part of every
reconciler: Ops objects move through `Pending → Running → Succeeded/Failed`,
`spec.abort` is honored, DRPC failovers progress, replication slots cut over,
and each transition emits an Event.

> **Hub/agent, multi-tenant, drift-managed paradigm.** The world is shaped to
> the CRD-redesign + multi-cluster RBAC design (namespaces `sb-sc-*` / `sb-mc-*`
> / `sb-dr-system`, hierarchy labels, the `sb:*` aggregated roles, `AccessGrant`
> / `NodePoolAllocation` / `ManagedCluster`, actor-stamp annotations, a hub→
> transport StorageCluster projection with agent-authored status, and
> `observedGeneration` applied/pending drift). **Almost none of this is on
> `main`** — see [PARADIGM.md](PARADIGM.md) for what the mock reproduces
> faithfully and where it necessarily stands in.

## Running it

```sh
go run . --dataset dr-failover --seed 42          # reproducible world
go run . --serve-ui ..                             # + serve the console itself:
                                                   #   open http://localhost:8080 — no nginx needed
go test ./...
```

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8080` | listen address |
| `--dataset` | `auto` | `small-healthy`, `medium-degraded`, `large-scale`, `dr-failover`, `chaos`, or `auto` (seeded random pick) |
| `--seed` | random | same seed + dataset = byte-identical world |
| `--namespace` | `simplyblock` | the control-plane (system) namespace; storage objects live in `sb-sc-<cluster>`, not here (design name: `simplyblock-system`) |
| `--sim-interval` | `4s` | simulator tick; `0` disables it (drive ticks via `/mockctl/advance`) |
| `--fail-rate` | `0.1` | probability a simulated operation ends `Failed` |
| `--viewer` | `global` | authorization stand-in for the impersonated user: `global`, `none`, or `<sb:role>@<namespace>[,...]` — see PARADIGM.md |
| `--serve-ui` | — | directory with the console's `index.html`; also mounts `/k8s`, `/operator`, `/prometheus` and generates `/config.js` |
| `--operator-fixtures` | — | JSON fixtures for the operator API (below) |

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
SB_OPERATOR_URL=http://sb-mock:8080
SB_PROMETHEUS_URL=http://sb-mock:8080
```

With the chart: `--set controlCenter.enabled=true --set controlCenter.mock.enabled=true`
deploys the mock next to the console and wires those overrides automatically.

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

Registered types are the union of the console's ClusterRole: all shipped
simplyblock CRDs, the **proposed** kinds with no CRD yet (they serve empty
lists — an empty list beats a 404 for a UI), the Ramen and OCM kinds, and the
core/workload objects. The list lives in `registry.go`.

## Datasets

A dataset is a parameterized topology (`datasets.go`) rendered into a coherent
object graph (`gen.go`): devices reference their nodes, slots their PVCs,
DRPCs their DRPolicy, PVs their PVCs, and the Helm release Secret is a
structurally real `helm.sh/release.v1` payload (base64-in-base64, gzipped
JSON). Names, NVMe models, capacities and statuses come from seeded pools —
`--dataset auto` picks a scenario at random, and any run is reproducible from
its logged `dataset=… seed=…` line.

## The admin API

| Endpoint | Purpose |
|---|---|
| `GET /mockctl/info` | dataset, seed, object counts, and **unmocked operator-API requests** |
| `POST /mockctl/reset?dataset=chaos&seed=7` | rebuild the world (watches are closed so clients relist) |
| `POST /mockctl/advance?ticks=5` | run simulator ticks on demand — run with `--sim-interval=0` for fully deterministic e2e tests |

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
- strategic-merge list semantics, server-side-apply field ownership
- `resourceVersion` preconditions on writes (no optimistic-concurrency 409s)
- protobuf negotiation; responses are always JSON
