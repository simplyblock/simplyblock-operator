# Design Document: FoundationDB Host Maintenance

**Status:** Partially Implemented  
**Author:** geoffrey1330  
**Date:** 2026-11-11 (last updated 2026-11-11)  
**Test Plan:** [`tests/test-plan-fdb-host-maintenance.md`](../tests/test-plan-fdb-host-maintenance.md)  
**Work Plan:** [`tasks/work-plan-fdb-host-maintenance.md`](../tasks/work-plan-fdb-host-maintenance.md)

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                 | Kind       | Blocks                            | Status                         |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|-----------------------------------|--------------------------------|
| P0-1 | The FoundationDB operator keeps a coordinator set it did not choose while the set is valid: stateful classes, DNS addresses, the desired count, and at most one coordinator per fault domain                 | Ecosystem  | `MovingCoordinators` (§5.5)       | Available, verified in v2.13.0 |
| P0-2 | The FoundationDB operator never selects a process whose zone is under maintenance as a coordinator candidate, and fails its own re-election when the remaining zones cannot satisfy its one-per-domain limit | Ecosystem  | The shared-host fallback (§5.5)   | Available, verified in v2.13.0 |
| P0-3 | `fdbcli` in the `fdb-kubernetes-monitor` image the processes run, at `/usr/bin/fdbcli`, with the `coordinators`, `maintenance on`, bare `maintenance`, and `maintenance off` commands of FoundationDB 7.3    | Ecosystem  | Every command step (§5.4)         | Available                      |
| P0-4 | FoundationDB holds one maintenance zone at a time, and a zone's processes report `under_maintenance` in the machine-readable status                                                                          | Ecosystem  | `EnteringMaintenance` (§5.7)      | Available                      |
| P0-5 | An evicted pod carries the `DisruptionTarget` condition with reason `EvictionByEvictionAPI`                                                                                                                  | Kubernetes | The secondary drain signal (§5.3) | Available from Kubernetes 1.26 |

P0-1 is what makes a move stick: an upstream operator that re-elects on every
pass reverts the set within one reconcile, and the window releases with the
coordinators back on the draining host. P0-2 is what makes the fallback for a
cluster with too few hosts (§5.5) stick in the same way. Without P0-3 there is
no way to issue a command, and the window degrades to holding and releasing
with nothing moved, which §10 names. P0-4 is why one window moves at a time
(§7.3). Without P0-5 the window has the sentinel as its only drain signal,
and the Holding deadline as its only fallback. The first four were
read from the `fdb-kubernetes-operator` v2.13.0 source that
`operator/internal/controllers/controlplane/foundationdb.go` pins.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [Data Model Changes](#4-data-model-changes)
5. [The Maintenance Window](#5-the-maintenance-window)
6. [State Machine](#6-state-machine)
7. [Controller Design](#7-controller-design)
8. [FoundationDB Interface](#8-foundationdb-interface)
9. [Configuration](#9-configuration)
10. [Failure Modes and Fallback](#10-failure-modes-and-fallback)
11. [Observability](#11-observability)
12. [Testing Strategy](#12-testing-strategy)
13. [Migration Strategy](#13-migration-strategy)
14. [Open Questions](#14-open-questions)

Appendices:

- [Appendix A: `controlplaneops_types.go`](#appendix-a-controlplaneops_typesgo)

---

## Overview

A Kubernetes worker that runs FoundationDB processes is drained like any other,
and today the drain evicts those processes with whatever roles they hold. When
one of them is a coordinator, the database runs the rest of the maintenance one
coordinator short, and the operator that would re-elect it may be the pod being
evicted. When one of them is a storage server, data distribution starts copying
its data to other hosts a minute into an absence that lasts two.

The `HostMaintenance` action on `ControlPlaneOps` is the window in which the
operator takes those two roles off a host before the drain is allowed to evict
anything. It is built for the way node pools are actually maintained: every
worker is cordoned at once, and the workers are drained one at a time, each
replaced rather than uncordoned. The ControlPlane controller raises one window
per cordoned worker that hosts FoundationDB pods. Each window holds its pods at
once with a PodDisruptionBudget that allows no disruption, and then waits. When
a drain reaches that worker, which the window sees as the eviction of a
sentinel pod it placed beside them, the window sets FoundationDB's maintenance mode for the host's zone, moves every
coordinator the host holds onto processes on other hosts, verifies the move in
the cluster file, and deletes the budget so the drain proceeds. After the host's
processes report back, on the same node or a replacement, the window clears the
maintenance mode and ends.

The window never refuses a drain. Every command it issues can fail, and every
failure before `Releasing` is reported and stepped over, because a budget that
outlives its window makes the worker undrainable by anything with nothing left
saying why. What the window guarantees is ordering: nothing is evicted before
the coordinators have been given the chance to move, the move takes seconds,
and the budget is gone within one step of that chance being spent.

Commits and reads continue throughout, with one qualification §2 states as a
non-goal. FoundationDB loses no committed transaction while a log replica
survives, and the window keeps the coordinator quorum whole, but every process
that holds a transaction-system role causes a recovery of a few seconds when it
dies. The window cannot remove that recovery. It removes the quorum risk, the
data movement, and the dependency on the upstream operator being alive at the
moment of eviction.

---

## 1. Background

FoundationDB is installed by the ControlPlane controller as a
`FoundationDBCluster` custom resource, reconciled by the upstream
`fdb-kubernetes-operator` ([`design-controlplane.md`](crd-redesign/design-controlplane.md)
§5.1). The upstream operator creates one pod per process group, each with its own
PersistentVolumeClaim, places them with a *preferred* anti-affinity per process
class, and writes the cluster file into the ConfigMap
`simplyblock-fdb-cluster-config` under the key `cluster-file`. The fault domain
key is `kubernetes.io/hostname`, so a zone is a worker. The coordinators are DNS
names of the form `<pod>.simplyblock-fdb-cluster.<namespace>.svc.<dnsDomain>`,
because the cluster is applied with `routing.defineDNSLocalityFields`.

The operator reads back `status.health` and the process-group counts
(`readFoundationDB` in `foundationdb.go`) and nothing else. No code in the
operator issues a FoundationDB command, and no code in the control plane
(`sbcli`) does on Kubernetes either.

Three properties of that deployment decide what a drain does to the database.

**Every host holds a transaction-system role.** The cluster is applied with
`processCounts.stateless: -1`, so the commit proxies, the GRV proxies, the
resolvers, and the master are recruited on the storage processes, beside the log
processes. A drain of any FoundationDB host therefore kills a process the
transaction system depends on, which triggers a recovery.

**The coordinators sit on three hosts and move only after a loss.** With
`redundancy_mode: double` the database wants three coordinators, one per zone.
The upstream operator re-elects a coordinator only after its process has left
the status, and the operator's own pod may be the one being evicted.

**Where an evicted pod comes back depends on the volume.** The claims take the
cluster's default StorageClass. On a cloud class such as EBS the upstream
operator recreates the pod under the same process group and it lands on another
node with its data. On a node-bound class, which K3s and most on-premise
clusters default to, the recreated pod stays `Pending` until the same node
returns. Either way the database runs without that process for the length of
the absence.

How a node pool is maintained decides what the window sees. An EKS managed node
group update, and every rolling update of its kind, cordons every worker in the
pool at once, drains them one at a time, and terminates each drained node in
favor of a replacement. The cordon therefore says nothing about which host goes
next, no host is ever uncordoned, and a drain that a PodDisruptionBudget holds
for longer than fifteen minutes fails the update.

The drain is already caught for storage workers. The `StorageNode` controller
watches `Node` objects and raises a `StorageNodeOps` `HostMaintenance` window
when a worker that carries a `StorageNode` is cordoned
([`design-storagenode.md`](crd-redesign/design-storagenode.md) §10). That
window's budget covers the FoundationDB pods on the worker as well, so that the
SPDK shutdown keeps the control plane it is talking to, and `Releasing` deletes
the budget once the backend node is offline. Nothing between those two steps
touches FoundationDB. A worker that hosts FoundationDB and carries no
`StorageNode` gets no window at all, and the lab this design was written against
has one.

Two earlier mechanisms turned the eviction into a process-group replacement:
taint-based replacement options on the `FoundationDBCluster`, and a
`pods/eviction` admission webhook. A replacement needs a spare worker to land
on, a hyper-converged cluster has none, and the drain was refused for as long as
the replacement could not land. Both were removed. The constraint they leave
behind is the one this design is built around: a drain is never refused.

---

## 2. Goals and Non-Goals

### Goals

- A cordoned worker that hosts FoundationDB pods gets a maintenance window
  whether or not it carries a `StorageNode`.
- No FoundationDB pod on a cordoned worker is evicted before the window has set
  maintenance mode and attempted the coordinator move for that worker.
- Workers are protected in the order they are drained, not the order they were
  cordoned, and a drain is held for seconds rather than for the length of
  another host's absence.
- The coordinator quorum is whole for the length of a host's absence whenever
  enough processes exist off the host to hold it.
- Data distribution does not copy the host's storage data away during the
  absence.
- The budget is removed within one step of the move being attempted, whatever
  the attempt's outcome, and on every terminal path of the window.
- A host that is replaced rather than uncordoned ends its window cleanly.
- The window is restart-safe: every side effect is written ahead of being
  performed and is idempotent on replay.
- Every hold, skip, and degraded outcome is an event on the operation, and the
  operation is the audit record of what was moved where.

### Non-Goals

- **A recovery-free drain.** Every FoundationDB host holds a transaction-system
  role (§1), and the coordinator move is itself a recovery. Clients retry
  through a recovery of a few seconds, and whether the control plane's own
  transactions do so correctly is §14 Q3.
- **Replacing process groups.** The window moves roles, never pods. A
  replacement needs a spare worker (§1), and the upstream operator's own
  replacement logic remains in charge of a process that stays down past its
  failure-detection window.
- **Moving log or stateless roles off the host.** FoundationDB recruits those
  roles itself, and the only lever that moves them is an exclusion, which is a
  replacement.
- **Protecting hosts drained together.** Two workers drained at the same moment
  get two moves in sequence, each taking seconds, and the second host's pods are
  held for that long. When every FoundationDB host is drained at once the
  database is down until hosts return, and no ordering of roles changes that.
- **Unplanned host loss.** A worker that fails without a cordon is the upstream
  operator's case, and the window does nothing for it.
- **Evictions that bypass the eviction API.** A `kubectl drain
  --disable-eviction` or a direct pod delete is not held by any budget and is
  not seen by the window, which is today's behavior.
- **The storage node's own window.** `StorageNodeOps` `HostMaintenance` keeps
  its steps, its budget, and its FoundationDB entry in `evictionTargets`
  (§5.10).

---

## 3. Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────────┐
│                         Kubernetes Control Plane                           │
│                                                                            │
│   ┌──────────────────────────────────────────────────────────────────┐     │
│   │  ControlPlaneReconciler (entity)                                 │     │
│   │  1. watches Node; each cordoned worker hosting FDB pods raises   │     │
│   │     ControlPlaneOps <cp>-maintenance-<hash(worker)>              │     │
│   │  2. an uncordon aborts a window still Holding, deletes a         │     │
│   │     finished one                                                 │     │
│   └──────────────────────────────────────────────────────────────────┘     │
│   ┌──────────────────────────────────────────────────────────────────┐     │
│   │  Sentinel Job per held worker (pause pod, no budget)             │     │
│   │  3. its eviction is the drain, seen by the operations controller │     │
│   │     as status.hostMaintenance.drainObservedAt                    │     │
│   └──────────────────────────────────────────────────────────────────┘     │
│   ┌──────────────────────────────────────────────────────────────────┐     │
│   │  ControlPlaneOpsReconciler, action HostMaintenance               │     │
│   │  4. Holding: labels the FDB pods, creates the budget, waits      │     │
│   │     for the drain signal                                         │     │
│   │  5. takes the lock, runs fdbcli through a Job per command        │     │
│   │  6. chooses coordinators from FoundationDBCluster.status         │     │
│   │  7. verifies the move in ConfigMap cluster-file, releases,       │     │
│   │     gives the lock back, waits for the processes, clears the zone│     │
│   └──────────────────────────────────────────────────────────────────┘     │
│                                                                            │
│   ControlPlane.status.activeOpsRef    ControlPlaneOps.spec.hostMaintenance │
│   FoundationDBCluster.status.processGroups    ConfigMap cluster-file       │
│   Pods foundationdb.org/fdb-cluster-name    PodDisruptionBudget    Job     │
└──────────────────────────────┬─────────────────────────────────────────────┘
                               │ Job: fdb-kubernetes-monitor image, cluster file mounted
┌──────────────────────────────▼─────────────────────────────────────────────┐
│  FoundationDB 7.3 (fdbcli)                                                 │
│  maintenance on <zone> <seconds>    coordinators <dns:port> ...            │
│  maintenance (status)               maintenance off                        │
└────────────────────────────────────────────────────────────────────────────┘
```

**The window is an operation on the control plane.** The ControlPlane owns the
`FoundationDBCluster`, and a FoundationDB host need not be a storage host.
Modeling it as a `ControlPlaneOps` action gives it what the other actions have:
the control plane's lock, a persisted step, and an audit record afterward.

**The cordon raises and holds, and the drain moves.** A cordon is a statement
of intent about a pool, and a drain is the act on one host. The budget goes up
on the first, because it is cheap and reversible and because a pool-wide cordon
is followed by drains in an order nobody announces. The move waits for the
second, because it is the move for the host that is leaving now, and because
its targets are the hosts that are not.

**The operator issues commands through Jobs.** The Job runs the
`fdb-kubernetes-monitor` image the database's own pods run, pinned by the same
constant that builds the `FoundationDBCluster`, so `fdbcli` is the server's
version and the operator's own image and role stay as they are. It mounts the
cluster-file ConfigMap and reaches the coordinators over the pod network as any
client does.

**The upstream operator stays in charge of coordinator selection.** The window
writes one coordinator set under the rules the upstream operator enforces
(§5.5), so the upstream operator leaves it alone, and when the host is back and
the maintenance zone cleared the upstream operator re-elects as it would after
any change.

---

## 4. Data Model Changes

`ControlPlaneOps` gains one action, one parameter block, seven steps, and one
status block. No other kind changes. The whole type is Appendix A, and the
numbered sections below quote the fields their argument turns on.

### 4.1 The action and its parameters

```go
// +kubebuilder:validation:Enum=Restart;Upgrade;Backup;HostMaintenance
type ControlPlaneOpsAction string

// WorkerNode is the Kubernetes Node whose FoundationDB processes the window
// takes roles off. It is the Node's name, and the zone the window maintains
// is the Node's kubernetes.io/hostname label.
// +kubebuilder:validation:Required
// +kubebuilder:validation:MinLength=1
WorkerNode string `json:"workerNode"`
```

`spec.hostMaintenance` is immutable like the other parameter blocks, by the
same `XValidation` rule, and an operation whose action is `HostMaintenance`
must carry it, which a second rule enforces at admission. The worker is a Node
name and not a FoundationDB zone, because the Node is what the cordon lands on.
The zone is read from the Node's `kubernetes.io/hostname` label when the window
starts, since the two differ on clouds whose node names are not hostnames, and
it is recorded in status so that a later step clears the zone the window set
rather than the one the label says now.

The validating webhook that already rejects an operation naming a control plane
the operator did not install applies unchanged. A `HostMaintenance` on a managed
control plane has no `FoundationDBCluster` to act on and is rejected at
creation.

### 4.2 The steps

```go
// +kubebuilder:validation:Enum=Draining;Restarting;Awaiting;Preflight;Applying;Verifying;Requesting;Holding;EnteringMaintenance;MovingCoordinators;Releasing;AwaitingHost;LeavingMaintenance;Cleanup;Completed
type ControlPlaneOpsStep string
```

The seven new values are the window's (§6). The CEL rule on `status.step.state`
lists them as well. The enum stays flat, because which steps belong to which
action is the graph's to declare.

### 4.3 The status block

```go
// HostMaintenance is what a HostMaintenance window did, written as it goes.
// +optional
HostMaintenance *HostMaintenanceStatus `json:"hostMaintenance,omitempty"`
```

`HostMaintenanceStatus` carries the zone the window maintains, the process
groups that were on the host when the window started, when and how the drain
was observed, the coordinator set the window wrote, how that set was placed
(`Spread`, `SharedHost`, or `Unchanged`), and when the maintenance mode it set
expires. The zone, the process groups, and the set are what a later step acts
on, and the drain observation and the placement are the two facts about the
window that `Succeeded` does not say.

A `Worker` print column at priority 1 shows `spec.hostMaintenance.workerNode`,
so `kubectl get cpops -o wide` answers which host a window is for.

### 4.4 Example

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: ControlPlaneOps
metadata:
  name: simplyblock-maintenance-3f9a1c2e
  namespace: simplyblock
spec:
  controlPlaneRef: simplyblock
  action: HostMaintenance
  hostMaintenance:
    workerNode: ip-10-0-12-71.eu-central-1.compute.internal
status:
  phase: Running
  step:
    state: AwaitingHost
  message: waiting for 2 FoundationDB process groups of ip-10-0-12-71.eu-central-1.compute.internal to report back
  hostMaintenance:
    zone: ip-10-0-12-71.eu-central-1.compute.internal
    processGroups:
      - log-16954
      - storage-54325
    drainObservedAt: "2026-11-11T14:02:09Z"
    drainObservedBy: Sentinel
    coordinators:
      - simplyblock-fdb-cluster-log-9617.simplyblock-fdb-cluster.simplyblock.svc.cluster.local:4501
      - simplyblock-fdb-cluster-log-80289.simplyblock-fdb-cluster.simplyblock.svc.cluster.local:4501
      - simplyblock-fdb-cluster-storage-73729.simplyblock-fdb-cluster.simplyblock.svc.cluster.local:4501
    coordinatorPlacement: Spread
    maintenanceUntil: "2026-11-11T18:02:31Z"
```

---

## 5. The Maintenance Window

### 5.1 The trigger

The ControlPlane controller watches `Node` objects. A Node whose
`spec.unschedulable` is true, and on which at least one pod in the control
plane's namespace carries the label `foundationdb.org/fdb-cluster-name`, gets a
window: the controller ensures a `ControlPlaneOps` named
`<controlplane>-maintenance-<hash>` exists, where `hash` is the first eight hex
digits of the SHA-256 of the worker's name. The hash is what keeps the name a
DNS label under a worker whose name is a fully qualified domain name, and it is
the same construction the storage node's budget uses. A pool-wide cordon raises
one window per FoundationDB host, and every one of them starts at once.

A user creating one by hand is accepted and behaves identically, which is what
makes the flow testable without cordoning anything.

An uncordon is answered the way the storage node's window answers it. A window
still `Holding` has done nothing to the database and is aborted through
`spec.abort`. From `EnteringMaintenance` onward the window runs on, because
something has to clear what it set. A window in a terminal phase is deleted on
the uncordon, so the next cordon of the same worker finds no record under the
derived name and raises a fresh one. A worker that is replaced rather than
uncordoned has its Node deleted, and the controller treats a deleted Node as an
uncordon for the purpose of the terminal record.

### 5.2 Holding the eviction

`Holding` records the window's zone and the process groups whose fault domain
it is, labels every FoundationDB pod on the worker with
`storage.simplyblock.io/fdb-maintenance-worker=<worker>`, and creates the
PodDisruptionBudget `sb-fdb-maintenance-<hash>` with `maxUnavailable: 0` over
that label. The label key and the budget name are the window's own and not the
storage node's, so that one window releasing cannot strip the other's hold
(§5.10). A pod the upstream operator recreates during the window carries no
label, which is correct: it is a pod the drain has already evicted.

Holding takes no lock. Several windows hold at once after a pool-wide cordon,
and holding excludes nothing: the budget is a Kubernetes object the window owns,
and putting it up touches neither the database nor another window's host.

`kubectl drain` blocks on the budget and retries the eviction every few
seconds. The budget's job is to hold the eviction until the move for this host
has been attempted, and `Releasing` is what lets it go.

The step completes when the budget reports zero allowed disruptions and a
drain of the worker has been observed (§5.3). Until then the window waits with
the budget up, for as long as the pool's maintenance takes to reach this host.

### 5.3 Observing the drain

A drain is a series of eviction requests, and a request a budget refuses leaves
no record in the API. The window therefore observes the drain through two
signals, either of which completes the wait, and records which one did in
`status.hostMaintenance.drainObservedBy`.

**The eviction of the window's sentinel.** `Holding` places one sentinel pod on
the worker beside the held pods: a Job named `<operation>-sentinel` whose pod
runs `fdbMonitorImage` with the command `sleep infinity`, which is an image
every FoundationDB host already has. The pod is pinned to the worker with
`spec.nodeName`, which places it without the scheduler and so on a cordoned
node, and no budget selects it. The Job carries `backoffLimit: 0`, so nothing
recreates the pod once it is gone. A drain evicts every evictable pod on the
worker, the sentinel among the first because nothing holds it, and the
operations controller sees the sentinel's deletion timestamp or its
disappearance. It writes `drainObservedAt` and `drainObservedBy: Sentinel`. The
sentinel is the drain made visible on a worker whose every other evictable pod
is one the budget holds, and it refuses nothing: a drain evicts it on the first
request.

**The eviction of a neighbor.** A drain evicts the pods no budget holds at
once, each carrying the condition `DisruptionTarget` with reason
`EvictionByEvictionAPI` as it is deleted. The operations controller watches
pods on the workers of its `Holding` windows, and such a condition on any pod
of the worker, including one that is not FoundationDB's, is the drain seen from
the side. It writes `drainObservedBy: PodDisruption`. This is the signal a
sentinel that could not be created leaves, and the one that arrives first on a
worker with other workloads.

A window that is `Holding` with neither signal waits to its deadline, fails,
and runs the terminal teardown, which deletes the budget. That bound is the
drain-never-refused rule applied to a window nothing reached, and it is the
one case in which the hold is long (§10).

The deadline is measured from the last drain any window in the namespace
observed, and from the window's own creation while none has. A rolling update
that is progressing therefore never expires a window: every drain it performs
restarts the clock for the windows still waiting their turn, however many hosts
the pool has. A pool in which nothing has been drained for the length of the
deadline is a cordon with no update behind it, and its windows release as a
window nothing reached does.

### 5.4 Issuing a command

Every FoundationDB command is one Job, named `<operation>-<command>` for one of
`maintenance-on`, `coordinators`, `maintenance-status`, and `maintenance-off`,
created by the step that needs it and read back on the passes that follow. The
Job is built the same way for every command.

- It runs `fdbMonitorImage` with a shell line around
  `fdbcli -C /etc/foundationdb/fdb.cluster --timeout 30 --exec "<command>"`
  that prints the command's output, copies it to the container's termination
  message, and exits with the command's code.
- It mounts the ConfigMap `simplyblock-fdb-cluster-config`, key `cluster-file`,
  at `/etc/foundationdb/fdb.cluster`, which is where every other FoundationDB
  consumer the operator deploys mounts it.
- It takes the node selector and tolerations the FoundationDB processes take
  (`foundationDBNodeSelector` in `foundationdb.go`), plus a toleration of
  `node.kubernetes.io/unschedulable` with effect `NoSchedule`, which is what
  lets it run while every worker in the pool is cordoned.
- It carries a required node affinity away from the window's worker, so the
  drain does not evict the command that is clearing the way for it. On a
  cluster where that worker is the only node matching the selector the
  affinity is dropped, because a Job with nowhere to run is worse than one the
  drain restarts.
- It has `restartPolicy: Never`, `backoffLimit: 3`,
  `activeDeadlineSeconds: 120`, and `ttlSecondsAfterFinished: 3600`.
- It is owned by the `ControlPlaneOps`, so a deleted record takes its Jobs with
  it.

The Job's name is derived from the operation and the command, so a step that
finds its Job already present reads it rather than creating a second, which is
the write-ahead guard for every command. A Job that reports `Complete` is a
command that returned success, and `fdbcli --exec` exits non-zero when the
command fails, which the Job reports as `Failed` after its retries. The one
command whose output matters, the bare `maintenance`, which prints the zone
under maintenance, is read from the pod's termination message, so no log access
is needed. What a step does with `Failed` is that step's decision (§6), and no
step before `Releasing` holds the budget on it.

### 5.5 Choosing the coordinators

`MovingCoordinators` reads two things: the current coordinator list from the
cluster file in the ConfigMap, and the process groups from
`FoundationDBCluster.status.processGroups`, each with its `processGroupID`,
`processClass`, and `faultDomain`. It writes a new list only when a current
coordinator's process group has the window's zone as its fault domain.

The candidates are the process groups whose class is stateful (`storage`,
`log`, or `transaction`), whose fault domain is neither the window's zone nor
the zone of another window that has observed its drain, and that carry no
`processGroupConditions` and no `removalTimestamp`. A process group the
upstream operator already considers unhealthy or is removing does not take a
coordinator, and neither does one on a host that is about to leave. A cordoned
host whose drain has not been observed is a candidate, because its processes
are running and the pool's maintenance may not reach it for hours.

The count to fill is the desired coordinator count, which is the count in the
current cluster file. Coordinators already in the list that are candidates are
kept, so the move changes as few entries as it can. The vacated entries are
filled in this order:

#### Step 1 — Spread

Candidates on hosts that hold no coordinator yet, stateful classes in the
order `log`, `storage`, `transaction`, and within a class by process group ID.
When the list is full the placement is `Spread`, and the set satisfies every
rule the upstream operator checks: stateful classes, DNS addresses, the desired
count, and one coordinator per zone. The upstream operator keeps it (P0-1).

#### Step 2 — Shared host

When Step 1 runs out of hosts, the remaining entries are filled from candidates
on hosts that already hold one, in the same class order. The placement is
`SharedHost`. A three-worker cluster with `double` redundancy always lands here,
because three coordinators cannot sit one per host on the two hosts not being
drained.

The upstream operator judges such a set invalid and attempts a re-election.
Its candidates exclude every process whose zone is under maintenance (P0-2),
which is why `EnteringMaintenance` precedes this step. With the draining zone
excluded it cannot build a one-per-zone set either, fails with its
not-enough-processes error, and requeues. The window's set stays. When
`LeavingMaintenance` clears the zone, the upstream operator's next pass finds a
valid one-per-zone set available again and elects it, which is one more
recovery, after the host is back.

#### Step 3 — Unchanged

When Steps 1 and 2 together cannot fill the list, because fewer stateful
processes exist off the leaving hosts than coordinators are wanted, the window
writes nothing. The placement is `Unchanged`, the event `CoordinatorsNotMoved`
names the counts, and the step completes. A single-worker cluster is this case
on every drain.

#### Algorithm Summary

```
current   := coordinators(configmap cluster-file)
groups    := FoundationDBCluster.status.processGroups
zone      := status.hostMaintenance.zone
leaving   := {zone} ∪ {w.zone for w in windows if w.drainObservedAt != nil}
if no entry of current maps to a group with faultDomain == zone:
    placement := Unchanged (nothing on this host); done
keep      := [c in current if group(c).faultDomain != zone]
candidates:= [g in groups if g.class is stateful
                        and g.faultDomain not in leaving
                        and g.conditions == [] and g.removalTimestamp == nil
                        and address(g) not in keep]
usedHosts := {group(c).faultDomain for c in keep}
want      := len(current) - len(keep)
spread    := take(want, [g in candidates if g.faultDomain not in usedHosts],
                  by class order log, storage, transaction, then by id,
                  marking each host used as it is taken)
shared    := take(want - len(spread), [g in candidates not in spread])
if len(spread) + len(shared) < want:
    placement := Unchanged; event CoordinatorsNotMoved; done
set       := keep + address(spread) + address(shared)
placement := Spread if shared == [] else SharedHost
write status.hostMaintenance.coordinators = set, then Job "coordinators <set>"
```

`address(g)` is
`<cluster>-<processGroupID>.<cluster>.<namespace>.svc.<dnsDomain>:<port>`, with
the port taken from the entries of the current cluster file, which all carry the
same one.

### 5.6 Verifying the move

The upstream operator rewrites the ConfigMap's `cluster-file` after a
coordinator change, and the ControlPlane controller already watches that
ConfigMap. `MovingCoordinators` completes when the file names none of the
process groups whose fault domain is the window's zone, or when the Job failed
and the step recorded the placement `Unchanged`. It holds while the Job runs and
while the file still names a process on the host, up to its deadline.

Verification reads the file rather than trusting the Job's exit code, because
the file is what every client reads and what the step's successors act on. A
file that still names the host after a `Complete` Job is the upstream operator
having reverted the set, which §10 names as the signal that P0-1 or P0-2 has
changed under the window.

### 5.7 Maintenance mode

`EnteringMaintenance` issues `maintenance on <zone> <seconds>`, where the
seconds are the `AwaitingHost` deadline. It runs before the coordinator move for
the reason §5.5 Step 2 states, and it is what stops data distribution from
copying the host's storage data away while the host is absent. The step records
`status.hostMaintenance.maintenanceUntil` and completes when the Job completes.
A Job that fails completes the step as well, with the event
`MaintenanceModeNotSet`, because a window that cannot set maintenance mode can
still move the coordinators and must still release the drain.

FoundationDB holds one maintenance zone, and `maintenance on` for a second zone
replaces the first. In a rolling update the host drained next enters
maintenance while the host drained before it is still absent, so the earlier
window's zone is taken over. The earlier host's storage servers then count as
failed for data distribution, which is the window's protection running out one
host early. The replacement is deliberate: the host leaving now needs the zone
more than the host that left minutes ago, whose processes are already coming
back. The event `MaintenanceZoneTakenOver` on the earlier window says so.

`LeavingMaintenance` runs once `AwaitingHost` has completed. It issues the
bare `maintenance` command and reads its answer: when the active zone is the
window's own, it issues `maintenance off` and emits `MaintenanceModeCleared`,
and when the zone is another's or none it completes with nothing issued. A
window never clears a zone it did not set. A failed `maintenance off` completes
the step with the event `MaintenanceModeLeft`: FoundationDB expires the zone
itself when the seconds run out, so a leftover is bounded.

The `FoundationDBCluster` is applied without `maintenanceModeOptions`, so the
upstream operator neither sets nor resets a maintenance zone on its own. The
zone is the windows' for the length of the pool's maintenance.

### 5.8 Releasing

`Releasing` deletes the budget and removes the label from the pods, and
completes when the budget is gone. The control plane's lock is given back on
completion (§7.3). From here the drain's next retry evicts the pods, the
upstream operator recreates each under its process group, and the recreated
pods land on another node or wait for this one, as the volume class decides.

### 5.9 Awaiting the processes, and ending

`AwaitingHost` completes when no process group recorded at `Holding` carries a
`MissingProcesses` condition in `FoundationDBCluster.status.processGroups`. It
reads nothing from the Node: a node-bound volume brings the processes back on
the same node after its reboot, a cloud volume brings them back on a
replacement, and a Node that was terminated no longer exists to be read. A
process group that has left the status altogether, because the upstream
operator replaced it past its failure-detection window, counts as returned. Its
deadline is the one length nobody controls, and an expiry fails the window
rather than recovering anything: the budget is already gone, the maintenance
zone expires on its own, and the event says the processes did not come back.

`Cleanup` removes any label the release left behind and completes. It is on
the success path only. A window that fails or is aborted runs the same teardown
from its terminal transition, best effort, and emits `MaintenanceMarkersLeft`
when the budget or a label could not be removed, because a budget at
`maxUnavailable: 0` outliving its window is the one outcome this design must
not leave silent.

### 5.10 A worker that is also a storage host

On a converged worker both windows are raised by the same cordon, and both hold
the FoundationDB pods: the storage node's through its `evictionTargets` entry
and the label `storage.simplyblock.io/maintenance-worker`, this one through its
own label. The drain proceeds when both budgets are gone. The storage node's
window keeps FoundationDB on the host until the SPDK shutdown is done, which is
what its entry is for, and this window keeps it until the coordinators have had
their chance to move.

Neither window reads the other. The storage node's `Releasing` deletes
`sb-maintenance-<hash>` and clears its own label, and this window's deletes
`sb-fdb-maintenance-<hash>` and clears its own. A pod carrying both labels is
held until both are gone. The storage node's window begins its shutdown at the
cordon, so on a converged worker the drain's first eviction usually finds the
SPDK node already offline, and this window's move is what the drain
then waits the few seconds for.

### 5.11 A rolling update of a node pool

The sequence the design is built for, on a four-worker pool with FoundationDB
on every worker and cloud volumes, drained by a managed node group update.

1. The pool is cordoned. Four windows are raised and reach `Holding` within
   seconds, each with its budget up. No lock is taken and no command is issued.
   The database is untouched.
2. The update drains the first worker. Its unheld pods go at once, the
   sentinel among them, and the sentinel's eviction stamps that worker's window.
   The window takes the lock, sets maintenance mode for its zone, moves the
   coordinators it held onto the other three workers, which are cordoned and
   healthy, verifies the cluster file, deletes its budget, and gives the lock
   back. The drain's next retry evicts the pods. The hold lasted about thirty
   seconds.
3. The update terminates the first worker and brings up a replacement. The
   upstream operator recreates the evicted pods, which land on the replacement
   with their volumes. The first window sits in `AwaitingHost` until their
   process groups report, then clears its zone if it is still the active one.
4. The update drains the second worker. Its window observes the drain, takes
   the lock, and enters maintenance, which takes the zone over from the first
   window if that window has not cleared it yet. The second window's candidates
   exclude the first worker only while the first window's drain is observed
   and its processes have not returned, so a replacement whose processes are
   back is a target again.
5. Steps 2 to 4 repeat for the third and fourth workers. Each drain is held for
   the length of one move. Each window ends when its processes are back on a
   replacement, and the deleted Node is what deletes the terminal record.

At every point the coordinator quorum is whole, no storage data is copied for a
host whose processes are minutes from returning, and no drain waits on another
host's absence.

---

## 6. State Machine

The window is a line. Its position is `status.step`, persisted before the side
effect each step performs, and every step requeues rather than waits.

```
  worker cordoned, FDB pods on it
    │
    ▼
  Holding              ← record zone and process groups, label the FDB pods,
    │                    create the budget at maxUnavailable: 0
    │  budget reports 0 allowed disruptions, and a drain of the worker is observed
    │  (takes the lock)
    ▼
  EnteringMaintenance  ← Job: maintenance on <zone> <awaitingHost seconds>
    │  Job Complete, or Job Failed with MaintenanceModeNotSet
    ▼
  MovingCoordinators   ← choose the set (§5.5), write it to status, Job: coordinators
    │  cluster file names no process on the host, or placement Unchanged
    ▼
  Releasing            ← delete the budget, remove the label
    │  budget gone (gives the lock back)
    ▼
  AwaitingHost         ← nothing
    │  no recorded process group carries MissingProcesses
    ▼
  LeavingMaintenance   ← Job: maintenance (the status), then Job: maintenance off when the zone is its own
    │  Job Complete, or Job Failed with MaintenanceModeLeft
    ▼
  Cleanup              ← remove any label left, then Completed
```

| Step                  | Deadline                                               | Abortable | Holds the lock |
|-----------------------|--------------------------------------------------------|-----------|----------------|
| `Holding`             | 6 hours since the last drain observed in the namespace | yes       | no             |
| `EnteringMaintenance` | 5 minutes                                              | no        | yes            |
| `MovingCoordinators`  | 10 minutes                                             | no        | yes            |
| `Releasing`           | 5 minutes                                              | no        | yes            |
| `AwaitingHost`        | 4 hours                                                | no        | no             |
| `LeavingMaintenance`  | 5 minutes                                              | no        | no             |
| `Cleanup`             | 5 minutes                                              | no        | no             |

`Holding` is the one abortable step, because it is the one from which nothing
has been done to the database. Its deadline runs from the last drain observed
by any window in the namespace (§5.3), so a pool-wide update of any length
keeps every window until its drain arrives, and a pool in which no drain has
happened for six hours releases. `AwaitingHost` carries the same deadline as
the storage node's wait for a host, since the two wait for the same reboot.

| Condition                                                   | Step                  | Result                                                                                                                                                                                                |
|-------------------------------------------------------------|-----------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| The worker is uncordoned                                    | `Holding`             | Aborted by the ControlPlane controller through `spec.abort`, markers taken down                                                                                                                       |
| The worker is uncordoned, or its Node is deleted            | any later step        | The window runs on. `AwaitingHost` reads the process groups, not the Node                                                                                                                             |
| The budget cannot be created                                | `Holding`             | Requeue. On the deadline the window fails with no markers up                                                                                                                                          |
| No drain is observed                                        | `Holding`             | Waits with the budget up while other windows observe drains, fails six hours after the last one, and the teardown deletes the budget                                                                  |
| The lock is held by another window's move                   | `Holding`             | Waits with the budget up and `MoveQueued`, and takes the lock when it frees, which is seconds later                                                                                                   |
| `maintenance on` fails                                      | `EnteringMaintenance` | `MaintenanceModeNotSet`, step completes, `maintenanceUntil` empty                                                                                                                                     |
| `maintenance on` takes the zone over from an earlier window | `EnteringMaintenance` | The earlier window gets `MaintenanceZoneTakenOver`, and its `LeavingMaintenance` later finds the zone is not its own                                                                                  |
| No coordinator is on the host                               | `MovingCoordinators`  | Placement `Unchanged`, no Job, step completes                                                                                                                                                         |
| Fewer candidates than entries to fill                       | `MovingCoordinators`  | Placement `Unchanged`, `CoordinatorsNotMoved`, step completes                                                                                                                                         |
| Only hosts already holding a coordinator remain             | `MovingCoordinators`  | Placement `SharedHost`, `CoordinatorsMoved` names the shared host, step completes                                                                                                                     |
| `coordinators` Job fails                                    | `MovingCoordinators`  | Placement `Unchanged`, `CoordinatorsNotMoved`, step completes                                                                                                                                         |
| Cluster file still names the host after a `Complete` Job    | `MovingCoordinators`  | Holds to the deadline, then the window fails with `CoordinatorsReverted`, and the terminal teardown releases                                                                                          |
| The budget cannot be deleted                                | `Releasing`           | Requeue. On the deadline the window fails, and the terminal teardown retries once, then `MaintenanceMarkersLeft`                                                                                      |
| The processes do not return                                 | `AwaitingHost`        | Window fails on the deadline, and the maintenance zone expires on its own                                                                                                                             |
| The status read or `maintenance off` fails                  | `LeavingMaintenance`  | `MaintenanceModeLeft`, step completes                                                                                                                                                                 |
| Operator restart                                            | any                   | The step is persisted and every Job is named, so the pass after the restart reads rather than redoes                                                                                                  |
| The operation is deleted while running                      | any                   | The delete guard refuses it from `EnteringMaintenance` on, as it refuses an abort there. A delete that reaches the controller runs the terminal teardown and releases the lock if the window holds it |
| The `ControlPlane` is deleted                               | any                   | Missing-target failure, as for every `Ops` kind                                                                                                                                                       |

---

## 7. Controller Design

### 7.1 Location

`operator/internal/controllers/controlplane/hostmaintenance.go` carries the
steps, beside `controlplaneops_controller.go` whose `perform` dispatches to
them. The coordinator choice of §5.5 is a pure function over the cluster file,
the process groups, and the leaving zones, in `coordinators.go` in the same
package, and the Job construction is in `fdbcommand.go`. The raise and the
uncordon handling live in `controlplane_controller.go` beside the entity's
other watches. The sentinel is built beside the command Jobs in
`fdbcommand.go`.

### 7.2 Reconciliation Trigger

The ControlPlane controller adds `Watches(&corev1.Node{}, …)` mapping a Node to
the `ControlPlane` objects whose namespace holds a FoundationDB pod on that
Node, which is a list of pods by the label `foundationdb.org/fdb-cluster-name`
filtered on `spec.nodeName`. A Node deletion maps the same way, from the pods
that still name it.

The operations controller already reconciles on its own object and on the Jobs
the ControlPlane controller owns. It adds two triggers: the ConfigMap
`simplyblock-fdb-cluster-config`, for operations in `MovingCoordinators`, so
that verification runs when the file changes rather than on the next requeue,
and pods on the workers of `Holding` operations, so that the sentinel's
deletion and a neighbor's `DisruptionTarget` condition are seen when they
land. A change to one window's
`drainObservedAt` enqueues every other window in `Holding`, which is how the
Holding deadline of §5.3 is re-armed.

### 7.3 Concurrency and Mutual Exclusion

The window takes `ControlPlane.status.activeOpsRef` for `EnteringMaintenance`,
`MovingCoordinators`, and `Releasing`, and for nothing else. Those are the
steps that touch the database and choose among hosts, and two of them running
at once would pick each other's host as a target. `Holding` and everything
after `Releasing` exclude nothing: a budget is the window's own object, and a
wait for processes to return is a wait.

This is a narrower lock than the other actions take, and it changes the shared
machinery in `controlplaneops_controller.go`: the lock is acquired on the
transition out of `Holding` rather than before the first step, and released on
the transition out of `Releasing` rather than at the terminal phase. The phase
is `Running` from `Holding` onward, because the window is doing something from
its first step. An action's graph declares the steps that hold the lock, and
the controller reads it from there, so the three existing actions keep holding
it from their first step to their last.

The lock still answers two questions. Two workers drained together get two
moves in sequence, and the second window waits in `Holding` with its budget up
and the event `MoveQueued`, for the seconds the first move takes. And a
`Restart` or `Upgrade`, which rolls the management API, cannot start while a
move is in progress, and a move cannot start while one of those holds the lock.
A `Restart` created while a window is in `AwaitingHost` runs, because nothing
in that wait is disturbed by it.

A window that fails or is deleted while holding the lock releases it from its
terminal transition, as every action does.

### 7.4 Interaction with Existing Controllers

- **The `StorageNode` controller** raises its own window on the same cordon.
  §5.10 states how the two coexist, and neither reads the other.
- **The ControlPlane entity reconciler** restamps the FoundationDB consumers
  when the cluster-file ConfigMap changes (`clusterfile.go`). A coordinator move
  therefore rolls the management API pods. §14 Q2 holds whether a planned move
  should.
- **The upstream FoundationDB operator** sees a coordinator set it did not
  choose and keeps or reverts it by the rules P0-1 and P0-2 state. It sees the
  evicted pod as a missing process and recreates it, and past its
  failure-detection window it replaces the process group, which `AwaitingHost`
  counts as returned.
- **The `ControlPlaneOps` validating webhook** applies with one addition: its
  delete guard refuses a window from `EnteringMaintenance` on, which keeps the
  guard and the graph's abort line one rule (§4.1). The design adds no
  webhook of its own: the sentinel is the drain signal, and it holds nothing.

### 7.5 RBAC

No new rule for the controller. The manager's role already carries `nodes`
(`get`, `list`, `watch`), `pods` (`list`, `watch`, `patch`),
`poddisruptionbudgets` (full), `batch/jobs` (full),
`configmaps` (`get`, `watch`), and `foundationdbclusters` (`get`, `watch`),
each already granted to an existing controller. The window issues no
`pods/exec`: a command run inside a database pod needs that privilege on every
pod in the namespace, and the role does not carry it. The sentinel is a Job
like the commands, under the same grant.

---

## 8. FoundationDB Interface

The window has no control-plane (`sbcli`) dependency. Its external calls are
four `fdbcli` commands, each issued through the Job of §5.4.

| Command                             | Step                  | Idempotency                                                                                     |
|-------------------------------------|-----------------------|-------------------------------------------------------------------------------------------------|
| `maintenance on <zone> <seconds>`   | `EnteringMaintenance` | Replaces the zone and its remaining time. Replaying it extends the window, which is harmless    |
| `coordinators <addr> <addr> <addr>` | `MovingCoordinators`  | Writing the set already in place is a no-op in FoundationDB. The Job's name is the replay guard |
| `maintenance` (the status)          | `LeavingMaintenance`  | A read                                                                                          |
| `maintenance off`                   | `LeavingMaintenance`  | Clearing no zone succeeds                                                                       |

Every command causes at most one recovery: `coordinators` always, and the three
maintenance commands never. The sliding `Holding` deadline of §5.3 is re-armed
on every pass of a `Holding` window, which runs at least every fifteen seconds,
and on the pass a drain observed elsewhere enqueues. The Job carries `--timeout 30` so that a command
against a database that cannot answer fails inside the Job's own deadline.

---

## 9. Configuration

Nothing is configurable. The window is raised for every cordon of a FoundationDB
host, one move runs at a time by FoundationDB's maintenance model and the lock,
and its constants are the deadlines of §6 and the Job settings of §5.4. The
maintenance mode's length is the `AwaitingHost` deadline, which §14 Q1 holds.

---

## 10. Failure Modes and Fallback

| Failure                                                         | Detection                                              | Behavior                                                                                                                                                                                                               |
|-----------------------------------------------------------------|--------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| The database cannot answer a command                            | The Job fails after its retries                        | The step completes degraded and says so. The budget is never held on it                                                                                                                                                |
| Every worker in the pool is cordoned                            | Nothing special                                        | The Job tolerates the cordon taint and runs on a cordoned worker other than the window's. Every window holds, and each moves when its drain arrives                                                                    |
| The Job's pod cannot be scheduled                               | The Job stays active to `activeDeadlineSeconds`        | As a failed command. A cluster with no host for the database's selector is one whose drain still proceeds                                                                                                              |
| The sentinel cannot be created or never starts                  | Nothing                                                | A Pending sentinel is still evicted, and its deletion is still the signal. A sentinel that was never created leaves the neighbor's `DisruptionTarget` as the signal, or the deadline when the worker has no unheld pod |
| The drain bypasses the eviction API                             | Nothing                                                | No budget holds a direct delete, and no signal reaches the window. The pods are gone with their roles, which is today's behavior. The window waits in `Holding` to its deadline, then fails and tears down             |
| The upstream operator reverts the set                           | The cluster file names the host after a `Complete` Job | The step holds to its deadline, the window fails with `CoordinatorsReverted`, and the teardown releases the drain. The signal that P0-1 or P0-2 changed                                                                |
| Too few processes off the leaving hosts                         | §5.5 Step 3                                            | Placement `Unchanged`, the absence runs one coordinator short, which is today's behavior, named in an event                                                                                                            |
| Two workers drained at the same moment                          | The lock                                               | The second window holds with `MoveQueued` for the seconds of the first move, then moves with the first host excluded from its candidates                                                                               |
| The worker is cordoned and never drained                        | Nothing                                                | The window holds with the budget up and no command issued, to the uncordon, or to six hours after the last drain any window observed. Nothing in the database changes                                                  |
| The host's processes return while a later window holds the zone | `LeavingMaintenance` reads the maintenance status      | The earlier window clears nothing and completes. The zone belongs to the host that left last                                                                                                                           |
| The operator pod is on the worker                               | Nothing special                                        | The step is persisted and the Jobs are named, so a manager rescheduled elsewhere resumes. The operator's own budget is the storage node's concern and not this window's                                                |
| The processes never return                                      | `AwaitingHost` deadline                                | The window fails. The maintenance zone expires on its own, and the upstream operator replaces the process groups past its own window                                                                                   |
| A terminal teardown cannot remove the budget                    | Delete returns an error twice                          | `MaintenanceMarkersLeft` on the operation, which is the one event a worker that will not drain is explained by                                                                                                         |
| The node name is not the hostname                               | Nothing                                                | The zone is read from the Node's `kubernetes.io/hostname` label at `Holding`, so the command names the zone FoundationDB knows                                                                                         |

---

## 11. Observability

The `ControlPlaneOps` kind already emits the lifecycle reasons of
[`design-controlplane.md`](crd-redesign/design-controlplane.md) §9.1 and
publishes `simplyblock_controlplane_operations_total` and
`simplyblock_controlplane_operation_duration_seconds`, which the new action
joins under `action=HostMaintenance`. The events below are additions to that
surface, and the metrics are new series in the same registry.

### Kubernetes Events

The window's events land on the `ControlPlaneOps`, which outlives the window as
its audit record. The raise lands on the `ControlPlane`, because it is the
object that exists before the window does.

| Event                                                                                          | Type      | Reason                     | On                |
|------------------------------------------------------------------------------------------------|-----------|----------------------------|-------------------|
| A cordoned worker hosts FoundationDB pods and a window was raised for it                       | `Normal`  | `HostMaintenanceRaised`    | `ControlPlane`    |
| The budget is up and the eviction of the worker's FoundationDB pods is held                    | `Normal`  | `EvictionHeld`             | `ControlPlaneOps` |
| A drain of the worker was observed, and the message names the signal                           | `Normal`  | `DrainObserved`            | `ControlPlaneOps` |
| The move is waiting for another window's move to finish                                        | `Normal`  | `MoveQueued`               | `ControlPlaneOps` |
| Maintenance mode was set for the worker's zone until the recorded time                         | `Normal`  | `MaintenanceModeSet`       | `ControlPlaneOps` |
| Maintenance mode could not be set and the window continues without it                          | `Warning` | `MaintenanceModeNotSet`    | `ControlPlaneOps` |
| A later window set maintenance mode for its zone, which ended this window's                    | `Normal`  | `MaintenanceZoneTakenOver` | `ControlPlaneOps` |
| The coordinators were moved, and the message names the set and whether two share a host        | `Normal`  | `CoordinatorsMoved`        | `ControlPlaneOps` |
| The coordinators were not moved, and the message names the counts or the failed command        | `Warning` | `CoordinatorsNotMoved`     | `ControlPlaneOps` |
| The cluster file still names the host after the move, so the set was reverted under the window | `Warning` | `CoordinatorsReverted`     | `ControlPlaneOps` |
| The budget is gone and the drain proceeds                                                      | `Normal`  | `EvictionReleased`         | `ControlPlaneOps` |
| The window is waiting for the host's process groups to report back                             | `Normal`  | `AwaitingHostReturn`       | `ControlPlaneOps` |
| Maintenance mode was cleared                                                                   | `Normal`  | `MaintenanceModeCleared`   | `ControlPlaneOps` |
| Maintenance mode could not be cleared and expires on its own                                   | `Warning` | `MaintenanceModeLeft`      | `ControlPlaneOps` |
| A terminal teardown left the budget or a label behind                                          | `Warning` | `MaintenanceMarkersLeft`   | `ControlPlaneOps` |

`DrainObserved` is the event that explains a rolling update after the fact: its
timestamps across the windows are the order the pool was drained in, and its
message says whether the sentinel or a neighbor's eviction was the signal.
`CoordinatorsNotMoved` and `CoordinatorsReverted` are the two that matter to a
reader during one. The first is a cluster whose shape cannot hold the quorum
off the leaving hosts, which is a sizing fact the event turns into a sentence.
The second is the upstream operator behaving differently from what this design
verified, and it is the event to look for after an upgrade of that operator.

### Prometheus Metrics

| Metric                                                  | Labels                           | Description                                                                                      |
|---------------------------------------------------------|----------------------------------|--------------------------------------------------------------------------------------------------|
| `simplyblock_controlplane_fdb_maintenance_zone_active`  | `namespace`, `worker`            | Gauge, 1 while a window believes it holds the maintenance zone for the worker                    |
| `simplyblock_controlplane_fdb_eviction_hold_seconds`    | `namespace`, `signal`            | Histogram of the time between the drain being observed and the budget going down                 |
| `simplyblock_controlplane_fdb_holding_windows`          | `namespace`                      | Gauge of windows in `Holding`, which after a pool-wide cordon is the number of hosts still to go |
| `simplyblock_controlplane_fdb_command_duration_seconds` | `namespace`, `command`, `result` | Histogram of each `fdbcli` Job's duration by command and `Complete` or `Failed`                  |
| `simplyblock_controlplane_fdb_coordinator_moves_total`  | `namespace`, `placement`         | Windows by placement: `Spread`, `SharedHost`, or `Unchanged`                                     |

**`simplyblock_controlplane_fdb_eviction_hold_seconds` is what the node pool
update feels.** It measures from the drain being observed to the budget's
deletion, which is the only interval in which the window stands in a
drain's way, and an update that fails on a PodDisruptionBudget timeout is one
whose upper buckets grew first. The `signal` label separates a slow move from a
slow observation.

**`simplyblock_controlplane_fdb_maintenance_zone_active` is the leak detector.**
A gauge at one with no window `Running` is a zone the teardown did not clear,
and it is the alert that says data distribution is ignoring a zone nobody is
maintaining.

**`simplyblock_controlplane_fdb_coordinator_moves_total{placement="Unchanged"}`
growing on a cluster larger than one host** is a cluster whose stateful
processes are fewer than it has hosts, which is a `FoundationDBCluster` sizing
fact worth a ticket rather than an alert.

---

## 12. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-fdb-host-maintenance.md`](../tests/test-plan-fdb-host-maintenance.md).

- **Unit:** the coordinator choice of §5.5 is a pure function and carries the
  risk: every placement, every exclusion including the leaving hosts, the
  address construction, the minimal-change property, and the one-, two-,
  three-, and five-host shapes. The steps run against a fake client with Jobs
  whose conditions the test sets by hand, which proves the two drain signals,
  the lock's scope, every degraded completion, the zone takeover, and the
  terminal teardown. The raise, the uncordon, and the Node deletion are tested
  the way the storage node's are, with a fake Node. The sentinel is a Job the
  fake client holds, and its deletion is what the test performs.
- **Integration:** the whole line against `envtest` with the FoundationDB CRDs
  installed and a `FoundationDBCluster` status written by the test, proving
  that a cordon raises and holds, that an `Eviction` request is refused and
  stamps the window, that `Releasing` lets the next one through, and that two
  windows on two workers move in sequence.
- **E2E:** a live cluster with `kubectl drain` on a FoundationDB host under a
  transaction load from the management API, asserting the cluster file, the
  quorum, the maintenance zone, no data movement, and the commit error rate
  through the window. A cordon of every worker followed by serial drains
  asserts the rolling-update sequence of §5.11, and the same on a three-worker
  cluster asserts the shared-host placement and the upstream re-election after
  the processes return.
- **What does not exist yet:** an `envtest` suite with the FoundationDB CRDs,
  a lab with exactly three FoundationDB hosts, and a lab on cloud volumes where
  a drained node is replaced rather than rebooted.
- **Implemented:** the API (§4), the window's steps (§5.2 to §5.9), the
  coordinator choice (§5.5), the drain signals and the sliding deadline
  (§5.3), the lock scope (§7.3), the raise on the cordon (§5.1), and the
  metrics (§11), each with its unit scenarios. **Not yet:** the integration
  suite, the end-to-end suite, and a live run.

The scenarios that must not be cut are the drain signals of §5.3, the three
placements of §5.5, the lock scope of §7.3, the budget being gone on every
terminal path (§5.9), and the reverted-set detection (§5.6).

---

## 13. Migration Strategy

The change is additive: one enum value on `spec.action`, one optional parameter
block, seven enum values on the step, and one optional status block. Existing
operations are unaffected. The CEL rule on
`status.step.state` is widened, never narrowed. The lock machinery's change
(§7.3) keeps the three existing actions holding the lock from their first step
to their last. `make -C operator manifests generate` and `make helm-sync`
regenerate the CRD and the chart.

Nothing migrates on the storage node's side. Its `HostMaintenance` keeps the
FoundationDB entry in `evictionTargets`, which §5.10 relies on.

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                            | Owner              |
|-----|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------|
| 1   | **Maintenance mode length:** the window sets it to the `AwaitingHost` deadline of four hours, which is the storage node's as well. A host that is drained for a replacement rather than a reboot leaves data under-replicated for that long before data distribution reacts. Whether the length should be shorter and refreshed by `AwaitingHost` on each pass depends on how long a replacement takes in the field | Operator team      |
| 2   | **The restamp on a planned move:** the cluster-file hash annotation rolls every FoundationDB consumer when the coordinators change ([#466](https://github.com/simplyblock/simplyblock-operator/issues/466)). A move the operator itself made is a case the restamp could skip, since a running client follows a coordinator change on its own. Whether it should is #466's                                          | Operator team      |
| 3   | **Control-plane transactions through a recovery:** whether `sbcli` retries `commit_unknown_result` and `not_committed` through the recovery the move and the eviction each cause. The window's claim that commits continue rests on the client doing so                                                                                                                                                             | Control plane team |
| 4   | **A FoundationDB host with no window at all:** a worker cordoned while the control plane is `Unavailable` cannot run a Job against the database. The design raises the window and lets the commands fail (§10), which is the drain-never-refused rule applied. Whether the raise should be skipped instead, so the record does not say a window ran when nothing could                                              | Operator team      |
| 5   | **The zone takeover:** a later window's `maintenance on` ends the earlier window's protection while its processes may still be returning (§5.7). Whether `EnteringMaintenance` should instead wait a bounded time for the earlier zone to clear depends on how long a replacement's processes take to report, against the fifteen minutes a drain may be held                                                       | Operator team      |

---

## Appendix A: `controlplaneops_types.go`

```go
// ControlPlaneOps: one imperative operation performed against the control plane.
//
// Most of what an administrator does to a control plane is expressible as
// desired state, because the entity re-applies what it installed on every pass:
// changing the image is an edit, scaling FoundationDB is an edit, and an object
// somebody deleted by hand is put back. What is left is what this kind carries,
// and it is four things: restarting a workload, moving it to a new version,
// asking FoundationDB for a backup, and taking a FoundationDB host through a
// Kubernetes node drain.
//
// Every action requires a control plane this cluster hosts, since each acts on
// something the operator installed. An operation naming a remote one is rejected
// at admission rather than created and failed, which is what
// ControlPlaneOpsValidator is for.
//
// The kind is introduced by the redesign, so it has no v1alpha1 spelling, no
// spoke to convert from, and no Hub method: its CRD declares one version.
//
// design-controlplane.md §6 and Appendix B, and design-fdb-host-maintenance.md
// §4 and Appendix A, are the specification.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/crdtypes"
	"github.com/simplyblock/atlas/statemachine"
)

// ControlPlaneOpsAction is the operation a ControlPlaneOps performs. Every
// action acts on what the operator installed, so every action requires a managed
// control plane. An operation naming a control plane the operator did not
// install is rejected at creation rather than created and failed.
// +kubebuilder:validation:Enum=Restart;Upgrade;Backup;HostMaintenance
type ControlPlaneOpsAction string

const (
	// ControlPlaneOpsActionRestart restarts a wedged workload. Its scope is
	// spec.restart.components, and an empty list restarts the whole control
	// plane.
	ControlPlaneOpsActionRestart ControlPlaneOpsAction = "Restart"
	// ControlPlaneOpsActionUpgrade moves the control plane to a new version and
	// verifies afterward that the version it reports is the one asked for.
	ControlPlaneOpsActionUpgrade ControlPlaneOpsAction = "Upgrade"
	// ControlPlaneOpsActionBackup asks FoundationDB for a backup outside
	// whatever schedule exists. It asks rather than implements: the snapshot is
	// the FoundationDB operator's to take.
	ControlPlaneOpsActionBackup ControlPlaneOpsAction = "Backup"
	// ControlPlaneOpsActionHostMaintenance takes a Kubernetes worker that runs
	// FoundationDB processes through a node drain: it holds the eviction of
	// those pods from the cordon on, and when the drain reaches the worker it
	// sets FoundationDB's maintenance mode for the worker's zone, moves the
	// coordinators the worker holds onto other hosts, and releases the
	// eviction. It clears the maintenance mode once the worker's processes are
	// back. The operator raises it when such a worker is cordoned.
	ControlPlaneOpsActionHostMaintenance ControlPlaneOpsAction = "HostMaintenance"
)

// ControlPlaneOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type ControlPlaneOpsPhase string

const (
	// ControlPlaneOpsPhasePending is an operation waiting for its target's lock.
	ControlPlaneOpsPhasePending ControlPlaneOpsPhase = "Pending"
	// ControlPlaneOpsPhaseRunning is an operation holding the lock and working.
	ControlPlaneOpsPhaseRunning ControlPlaneOpsPhase = "Running"
	// ControlPlaneOpsPhaseSucceeded is a finished operation that did what it
	// said.
	ControlPlaneOpsPhaseSucceeded ControlPlaneOpsPhase = "Succeeded"
	// ControlPlaneOpsPhaseFailed is a finished operation that did not.
	ControlPlaneOpsPhaseFailed ControlPlaneOpsPhase = "Failed"
	// ControlPlaneOpsPhaseAborted is an operation stopped on request, whose
	// unwind has finished.
	ControlPlaneOpsPhaseAborted ControlPlaneOpsPhase = "Aborted"
)

// ControlPlaneOpsStep is one step of a running control-plane operation. Which
// steps belong to which action is declared by that action's graph rather than by
// this type, which is why the enum stays flat as actions are added.
// +kubebuilder:validation:Enum=Draining;Restarting;Awaiting;Preflight;Applying;Verifying;Requesting;Holding;EnteringMaintenance;MovingCoordinators;Releasing;AwaitingHost;LeavingMaintenance;Cleanup;Completed
type ControlPlaneOpsStep string

const (
	// ControlPlaneOpsStepDraining holds while another operation in the namespace
	// is still running. Restart and Upgrade both restart the management API, so
	// both drain.
	ControlPlaneOpsStepDraining ControlPlaneOpsStep = "Draining"

	// ControlPlaneOpsStepRestarting rolls the workloads the action named.
	ControlPlaneOpsStepRestarting ControlPlaneOpsStep = "Restarting"
	// ControlPlaneOpsStepAwaiting waits for what the previous step asked for:
	// the restarted pods coming back, or the backup reporting a snapshot.
	ControlPlaneOpsStepAwaiting ControlPlaneOpsStep = "Awaiting"

	// ControlPlaneOpsStepPreflight reads live state, which admission cannot: it
	// holds until the control plane is Available, and fails when the requested
	// image is the one already running.
	ControlPlaneOpsStepPreflight ControlPlaneOpsStep = "Preflight"
	// ControlPlaneOpsStepApplying writes the new image onto the entity, which is
	// what rolls the Deployment.
	ControlPlaneOpsStepApplying ControlPlaneOpsStep = "Applying"
	// ControlPlaneOpsStepVerifying re-probes and compares the reported version
	// against the one asked for, which is what makes an upgrade more than an
	// image bump.
	ControlPlaneOpsStepVerifying ControlPlaneOpsStep = "Verifying"

	// ControlPlaneOpsStepRequesting creates or triggers the FoundationDBBackup.
	// Awaiting is shared with Restart.
	ControlPlaneOpsStepRequesting ControlPlaneOpsStep = "Requesting"

	// ControlPlaneOpsStepHolding labels the worker's FoundationDB pods, puts up
	// the budget that holds their eviction, and waits for a drain of the worker
	// to be observed. It is the one step of a HostMaintenance window from which
	// an abort is possible, because nothing has been done to the database yet.
	ControlPlaneOpsStepHolding ControlPlaneOpsStep = "Holding"
	// ControlPlaneOpsStepEnteringMaintenance sets FoundationDB's maintenance
	// mode for the worker's zone, so that data distribution leaves the host's
	// storage data alone while the host is away.
	ControlPlaneOpsStepEnteringMaintenance ControlPlaneOpsStep = "EnteringMaintenance"
	// ControlPlaneOpsStepMovingCoordinators writes a coordinator set that names
	// no process on the worker, and completes once the cluster file agrees.
	ControlPlaneOpsStepMovingCoordinators ControlPlaneOpsStep = "MovingCoordinators"
	// ControlPlaneOpsStepReleasing takes the budget down, which is what lets
	// the drain proceed.
	ControlPlaneOpsStepReleasing ControlPlaneOpsStep = "Releasing"
	// ControlPlaneOpsStepAwaitingHost waits for the worker's FoundationDB
	// process groups to report back, on the same node or a replacement.
	ControlPlaneOpsStepAwaitingHost ControlPlaneOpsStep = "AwaitingHost"
	// ControlPlaneOpsStepLeavingMaintenance clears the maintenance mode the
	// window set, when the active zone is still the window's own.
	ControlPlaneOpsStepLeavingMaintenance ControlPlaneOpsStep = "LeavingMaintenance"
	// ControlPlaneOpsStepCleanup removes whatever marker the release left
	// behind. It is the last step of a window that succeeded.
	ControlPlaneOpsStepCleanup ControlPlaneOpsStep = "Cleanup"

	// ControlPlaneOpsStepCompleted is the terminal step of an operation that succeeded.
	ControlPlaneOpsStepCompleted ControlPlaneOpsStep = "Completed"
)

// HostMaintenanceCoordinatorPlacement is how a HostMaintenance window placed
// the coordinators it moved.
// +kubebuilder:validation:Enum=Spread;SharedHost;Unchanged
type HostMaintenanceCoordinatorPlacement string

const (
	// HostMaintenanceCoordinatorPlacementSpread is a set with one coordinator
	// per host, which the FoundationDB operator keeps as it is.
	HostMaintenanceCoordinatorPlacementSpread HostMaintenanceCoordinatorPlacement = "Spread"
	// HostMaintenanceCoordinatorPlacementSharedHost is a set in which two
	// coordinators share a host, written when fewer hosts than coordinators
	// remain off the leaving workers. The FoundationDB operator re-elects a
	// spread set once the worker's processes are back.
	HostMaintenanceCoordinatorPlacementSharedHost HostMaintenanceCoordinatorPlacement = "SharedHost"
	// HostMaintenanceCoordinatorPlacementUnchanged is a window that wrote no
	// set: the worker held no coordinator, too few processes exist off the
	// leaving workers, or the command failed. The operation's events say which.
	HostMaintenanceCoordinatorPlacementUnchanged HostMaintenanceCoordinatorPlacement = "Unchanged"
)

// HostMaintenanceDrainSignal is how a HostMaintenance window observed the
// drain of its worker.
// +kubebuilder:validation:Enum=Sentinel;PodDisruption
type HostMaintenanceDrainSignal string

const (
	// HostMaintenanceDrainSignalSentinel is the eviction of the sentinel pod the
	// window placed on the worker beside the held FoundationDB pods.
	HostMaintenanceDrainSignalSentinel HostMaintenanceDrainSignal = "Sentinel"
	// HostMaintenanceDrainSignalPodDisruption is the eviction of another pod on
	// the worker, seen as that pod's DisruptionTarget condition.
	HostMaintenanceDrainSignalPodDisruption HostMaintenanceDrainSignal = "PodDisruption"
)

// UpgradeSpec parameterizes the Upgrade action and is ignored by the others.
type UpgradeSpec struct {
	// Image is the version to move to. It replaces
	// ControlPlane.spec.source.managed.image when the operation succeeds, so the
	// entity keeps describing what is running.
	// +kubebuilder:validation:Required
	Image crdtypes.ImageReference `json:"image"`
}

// RestartSpec parameterizes the Restart action and is ignored by the others.
type RestartSpec struct {
	// Components names the workloads to restart. Empty restarts the whole
	// control plane. Naming only components that status.components reports as
	// non-essential skips the drain, because restarting them interrupts nothing.
	// +listType=set
	// +optional
	Components []string `json:"components,omitempty"`
}

// BackupSpec parameterizes the Backup action and is ignored by the others.
type BackupSpec struct {
	// BlobStore is the destination, in the form the FoundationDBBackup CRD takes
	// it. The operator copies it through rather than interpreting it, since the
	// backup is the FoundationDB operator's to perform.
	// +kubebuilder:validation:Required
	BlobStore string `json:"blobStore"`

	// BackupName is the FoundationDBBackup to create or trigger. Absent uses the
	// one already configured for the cluster, and fails when there is none and
	// no name to create.
	// +optional
	BackupName string `json:"backupName,omitempty"`
}

// HostMaintenanceSpec parameterizes the HostMaintenance action and is ignored
// by the others.
type HostMaintenanceSpec struct {
	// WorkerNode is the Kubernetes Node whose FoundationDB processes the window
	// takes roles off. It is the Node's name, and the zone the window maintains
	// is the Node's kubernetes.io/hostname label.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	WorkerNode string `json:"workerNode"`
}

// ControlPlaneOpsSpec is one operation to perform against the control plane.
//
// Everything except spec.abort is frozen once the object is admitted, which is
// what makes the status an audit of the request that ran rather than of whatever
// the object says now. The parameters are consumed several steps apart:
// Preflight reads spec.upgrade.image and Applying writes it, Draining reads
// spec.restart.components before Restarting rolls them, and Holding labels the
// pods of spec.hostMaintenance.workerNode that Releasing unlabels. An edit in
// between produces an operation that checked one thing and did another.
//
// What is frozen is each parameter block's presence together with its
// contents.
// +kubebuilder:validation:XValidation:rule="has(self.upgrade) == has(oldSelf.upgrade) && (!has(self.upgrade) || self.upgrade == oldSelf.upgrade)",message="spec.upgrade is immutable: Preflight checked the image the operation was admitted with, and Applying writes it several steps later"
// +kubebuilder:validation:XValidation:rule="has(self.restart) == has(oldSelf.restart) && (!has(self.restart) || self.restart == oldSelf.restart)",message="spec.restart is immutable: the drain is decided from the component list, so widening it afterward skips a drain the wider list would have required"
// +kubebuilder:validation:XValidation:rule="has(self.backup) == has(oldSelf.backup) && (!has(self.backup) || self.backup == oldSelf.backup)",message="spec.backup is immutable: the destination is what Requesting created the FoundationDBBackup against"
// +kubebuilder:validation:XValidation:rule="has(self.hostMaintenance) == has(oldSelf.hostMaintenance) && (!has(self.hostMaintenance) || self.hostMaintenance == oldSelf.hostMaintenance)",message="spec.hostMaintenance is immutable: the budget and the maintenance zone were raised for the worker the operation was admitted with"
// +kubebuilder:validation:XValidation:rule="self.action != 'HostMaintenance' || has(self.hostMaintenance)",message="action HostMaintenance needs spec.hostMaintenance, which names the worker"
type ControlPlaneOpsSpec struct {
	// ControlPlaneRef names the ControlPlane this operation acts on, in this
	// object's own namespace. The operation never owns its target, because
	// deleting the record of an operation must not delete the control plane it
	// operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ControlPlaneRef string `json:"controlPlaneRef"`

	// Action is the operation to perform. Immutable, so that the status describes
	// the operation that ran.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action ControlPlaneOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. It is
	// the one field of this spec an update may change, because it is the one that
	// is meant to be set after the operation started. Whether an abort is
	// expressible from the current step is declared by that action's graph rather
	// than checked here.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Upgrade parameterizes action Upgrade and is ignored by the others.
	// +optional
	Upgrade *UpgradeSpec `json:"upgrade,omitempty"`

	// Restart parameterizes action Restart and is ignored by the others.
	// +optional
	Restart *RestartSpec `json:"restart,omitempty"`

	// Backup parameterizes action Backup and is ignored by the others.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// HostMaintenance parameterizes action HostMaintenance and is ignored by
	// the others.
	// +optional
	HostMaintenance *HostMaintenanceSpec `json:"hostMaintenance,omitempty"`
}

// HostMaintenanceStatus is what a HostMaintenance window did, written as it
// goes so that a later step acts on what an earlier one recorded.
type HostMaintenanceStatus struct {
	// Zone is the FoundationDB fault domain the window maintains, read from
	// the worker's kubernetes.io/hostname label when the window started.
	// +optional
	Zone string `json:"zone,omitempty"`

	// ProcessGroups are the FoundationDB process groups whose fault domain was
	// the zone when the window started. They are what AwaitingHost waits for,
	// wherever they come back.
	// +listType=set
	// +optional
	ProcessGroups []string `json:"processGroups,omitempty"`

	// DrainObservedAt is when a drain of the worker was first seen. Empty while
	// the window holds without one.
	// +optional
	DrainObservedAt *metav1.Time `json:"drainObservedAt,omitempty"`

	// DrainObservedBy is which signal saw the drain: the eviction of the
	// window's sentinel pod, or the eviction of another pod on the worker.
	// +optional
	DrainObservedBy HostMaintenanceDrainSignal `json:"drainObservedBy,omitempty"`

	// Coordinators is the set the window wrote, as the addresses the cluster
	// file carries. Empty when the window wrote none.
	// +listType=set
	// +optional
	Coordinators []string `json:"coordinators,omitempty"`

	// CoordinatorPlacement is how the set was placed: one per host, two on
	// one host, or not written at all.
	// +optional
	CoordinatorPlacement HostMaintenanceCoordinatorPlacement `json:"coordinatorPlacement,omitempty"`

	// MaintenanceUntil is when the maintenance mode the window set expires on
	// its own. Empty when the mode could not be set.
	// +optional
	MaintenanceUntil *metav1.Time `json:"maintenanceUntil,omitempty"`
}

// ControlPlaneOpsStatus is the observed state of one control-plane operation.
type ControlPlaneOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase ControlPlaneOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs, so a step reported
	// here is a step that started.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Draining','Restarting','Awaiting','Preflight','Applying','Verifying','Requesting','Holding','EnteringMaintenance','MovingCoordinators','Releasing','AwaitingHost','LeavingMaintenance','Cleanup','Completed']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// BackupRef names the FoundationDBBackup a Backup run created or triggered.
	// The operation does not own it, because deleting the record of a backup
	// must not delete the backup's configuration.
	// +optional
	BackupRef string `json:"backupRef,omitempty"`

	// HostMaintenance is what a HostMaintenance window did, written as it goes.
	// +optional
	HostMaintenance *HostMaintenanceStatus `json:"hostMaintenance,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation acquired its target's lock.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cpops
// +kubebuilder:printcolumn:name="ControlPlane",type=string,JSONPath=".spec.controlPlaneRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=".spec.hostMaintenance.workerNode",priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ControlPlaneOps is a single operation performed against the control plane. It
// runs to a terminal phase and stays afterward as the audit record of what was
// done, with which parameters, and how it ended. Only one may act on the
// control plane at a time, which the entity's status.activeOpsRef enforces.
type ControlPlaneOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneOpsSpec   `json:"spec,omitempty"`
	Status ControlPlaneOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneOpsList contains a list of ControlPlaneOps.
type ControlPlaneOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlaneOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControlPlaneOps{}, &ControlPlaneOpsList{})
}
```
