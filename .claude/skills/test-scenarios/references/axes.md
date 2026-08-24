# Coverage Axes

Each axis has an applicability test, its values, **what actually breaks** at each
value, and the rows it obliges. Walk all of them; state which you excluded and
why.

---

## A. Storage cluster topology (node count)

**Applies when** the feature selects, places, migrates, drains, distributes, or
counts storage nodes — or reads per-node metrics. In practice: nearly everything
in the operator.

| Value                | Why it is not redundant with the others                                                                                                                                                                                                                    |
|----------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Single node**      | Every "pick another node" path has no answer. Degenerate: the only node is simultaneously hottest and coolest; `k > len(nodes)` must clamp; round-robin has no peer; drain has no migration target; FTT is 0 so redundancy checks must not divide by zero. |
| **Two nodes**        | Tie-breaks and even splits; "picks the only available target"; one node offline leaves a single-node cluster mid-operation.                                                                                                                                |
| **Three nodes**      | The FTT=1 minimum. One node offline puts the cluster *at* its fault-tolerance limit, so operations that reduce redundancy further must block. Draining the third node is the interesting case, not the first.                                              |
| **Five or more**     | Distribution becomes observable: round-robin evenness, budget caps (a 10 % migration cap has no effect below 10 nodes), candidate ranking beyond the top two, concurrent operations on two nodes, partial subsets offline.                                 |
| **Asymmetric nodes** | Different capacity, vCPU count, huge-page allocation, or `max_lvol` per node: size normalization, minimum-vCPU gates, and "the biggest node is not the emptiest" ranking.                                                                                  |

**Obliged rows**

- Single node: the feature no-ops cleanly, or fails with a clear reason and
  leaves the cluster untouched — never a nil deref, never a silent success.
- Three nodes with one offline: at the FTT limit, the operation blocks or fails
  safely rather than removing the last redundancy.
- Five or more: the selection spreads instead of concentrating; caps apply.
- Scaling mid-operation: a node joins or leaves while the operation runs.

---

## B. Namespace scope

**Applies when** the feature reads, writes, watches, or generates names from
namespaced objects — PVCs, PVs, Pods, Secrets, ConfigMaps, Jobs, or any
namespaced CR. A cluster-scoped controller still needs this axis if it touches
PVCs anywhere in its flow.

| Value                                        | Why it is not redundant                                                                                                                                                                                                                                                                                                                     |
|----------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Single namespace**                         | The base path.                                                                                                                                                                                                                                                                                                                              |
| **Multiple namespaces, one storage backend** | Two namespaces sharing a StorageClass or StoragePool: **the same object name in both** must produce distinct, DNS-valid generated names (this is the classic collision — a generated CR name built from a PVC or PV name must include or hash the namespace). Watchers must not cross-wire; per-PVC annotations must resolve per namespace. |
| **Operator namespace vs workload namespace** | A cluster-scoped CR referencing namespaced objects; the operator's own ConfigMaps/Secrets/Jobs living somewhere the workload cannot see; RBAC that happens to work only in the operator's namespace.                                                                                                                                        |
| **Cross-namespace reference**                | A policy or CR in namespace A naming an object in namespace B: must be explicitly rejected (or explicitly supported), never silently ignored and never silently resolved against the wrong namespace.                                                                                                                                       |
| **Namespace lifecycle**                      | Namespace deleted while an operation on its objects is running: finalizers must release, volumes must not be orphaned, the operation must reach a terminal state.                                                                                                                                                                           |

**Obliged rows**

- Same PVC name in two namespaces → two distinct generated resources, both
  DNS-label valid, ≤63 chars, no collision (`Boundary` when the names are long
  enough to be truncated).
- Multi-tenant isolation: an operation scoped to namespace A does not read,
  migrate, or delete anything belonging to namespace B (`Negative`, asserted by
  absence).
- A namespaced watcher sees an event in an unwatched namespace → ignored, no
  reconcile.
- RBAC: the controller's role covers every namespace it must reach; a forbidden
  verb surfaces as a clear failure, not a stuck reconcile.

---

## C. Cluster count

**Applies when** more than one `StorageCluster` can exist, or the feature spans
Kubernetes clusters. The first is almost always true; the second is true for
replication, failover, backup, and migration features.

| Value                                               | Why it is not redundant                                                                                                                                                                                                                                                               |
|-----------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **One StorageCluster**                              | The base path.                                                                                                                                                                                                                                                                        |
| **Several StorageClusters, one Kubernetes cluster** | Per-cluster locks (`activeOpsRef`) must not interfere; per-cluster state maps must be keyed correctly (**closure and loop-variable capture bugs live here**); metrics and events must carry the cluster label; an operation in cluster A must not touch cluster B's nodes or volumes. |
| **Two Kubernetes clusters (cross-cluster feature)** | Source/target pairing, which side owns the CR, peer unreachable, peer CR deleted, network partition, both sides believing they are primary, asymmetric configuration or component versions, clock skew in anything time-based.                                                        |
| **Cross-cluster failover / failback**               | The reverse direction is a separate scenario from the forward one, and the second failover after a failback is a separate scenario again.                                                                                                                                             |

**Obliged rows**

- Isolation: concurrent operations in two `StorageCluster`s complete
  independently, neither blocking nor mutating the other.
- Per-cluster keying: state recorded for cluster A is not read back for cluster B
  (`Negative`).
- Cross-cluster: peer unreachable mid-operation → defined terminal state on the
  local side, no partial commit.
- Cross-cluster: both directions, plus the repeat.

---

## D. Failure domains and placement topology

**Applies when** the feature places, migrates, or removes anything, and the
cluster can carry failure domains or node labels.

Values: none set · all set · **partially set** (the neglected one) · every domain
holding exactly one node · one domain holding the majority.

**Obliged rows:** the feature respects domains when enabled; blocks or warns when
`enableFailureDomains=true` and a domain is unset; and a placement that would
concentrate a volume's replicas in one domain is rejected.

---

## E. Object scale

**Applies always.** Values: **zero** (empty list — the most-skipped row), one, a
handful (3–5), many (100+), and more candidates than the operation's cap.

**Obliged rows:** empty input returns empty output without error; one object
exercises the no-tie-break path; 100+ hits no CR-count, name-length, or API
request-rate limit and its time-to-complete is recorded.

---

## F. Lifecycle and timing

**Applies always.** Each value is a distinct point at which state can be
inconsistent:

- Object exists but is not yet provisioned (no backend UUID).
- Mid-operation, each sub-phase separately.
- **After an operator restart in each sub-phase:** the write-ahead / `Triggered`
  guard must prevent a duplicated side effect.
- After a control-plane restart or a brief API-server outage (a 3-second blip is
  enough to make a liveness probe lie).
- While another operation holds the lock.
- Terminal (`Succeeded` / `Failed`) — re-reconcile must be a no-op.
- During deletion — finalizer released on every path, including failures.
- Cool-down or hysteresis window: inside it, exactly at its expiry, after it.

---

## G. Trigger and actor

**Applies always.** The same state change reached by a different actor takes a
different code path: operator-driven reconcile · user `kubectl apply` / `patch` /
`delete` (including deleting the CR mid-operation and editing an immutable field)
· control-plane-side change the operator only learns about by polling · CSI
driver request · admission webhook path (and the webhook being down).

---

## H. Component version skew

**Applies when** the feature spans operator, CSI driver, and control plane.
Values: matched versions · operator newer than CSI · CSI newer than operator ·
control plane missing an endpoint the operator calls. At minimum, one row for
"the endpoint this design needs does not exist yet" when the design flags it as
an external dependency.

---

## Choosing combinations

Full cartesian expansion is never the answer. Cover:

1. **Every value of every applicable axis, at least once.** Non-negotiable.
2. **Interacting pairs, explicitly.** Two axes interact when they reach the same
   decision function. Recurring ones in this codebase:
   - topology × eligibility (single node × all volumes pinned → no target)
   - topology × failure domain (three nodes, three domains, one offline)
   - namespace count × name generation (same name, long names, truncation)
   - cluster count × per-cluster state (cool-downs, locks, metric labels)
   - scale × cap (more candidates than the budget allows)
   - lifecycle × actor (restart during a user-initiated cancellation)
3. **Declare the rest.** Untested combinations go in the gap table with a reason.
