# Work Plan: FoundationDB Host Maintenance

Related design: [`designs/design-fdb-host-maintenance.md`](../designs/design-fdb-host-maintenance.md)
Test plan: [`tests/test-plan-fdb-host-maintenance.md`](../tests/test-plan-fdb-host-maintenance.md)

**Author:** geoffrey1330
**Date:** 2026-11-11
**Design status at split:** Accepted. W-12 to W-14 were added on 2026-11-11 from the design's §5.12, at status Partially Implemented
**Issues:** not yet filed

Work item IDs (`W-01`) are permanent and are cited from issue bodies and commit
messages, so they are never renumbered. Each item below is one GitHub issue: the
description and acceptance criteria are the issue body verbatim.

`Depends on` is ordering the work requires to be correct. `Conflicts with` is
ordering the mechanics require: two items that rewrite the same file, or that
both regenerate the same manifests, are serialized even when neither needs the
other's result. §1 is derived from both.

---

## 1. Execution Order

<!-- Generated. Run `.claude/skills/work-plan/scripts/check-work-plan.py
     <this file> --write` to refresh this section, and never edit the
     three tables by hand. -->

### Waves

| Wave | Items                  | Runs after |
|------|------------------------|------------|
| 1    | W-01, W-03, W-14       | —          |
| 2    | W-02, W-04, W-08, W-12 | wave 1     |
| 3    | W-05                   | wave 2     |
| 4    | W-06                   | wave 3     |
| 5    | W-07, W-13             | wave 4     |
| 6    | W-09, W-10, W-11       | wave 5     |

### Critical path

W-01 → W-04 → W-05 → W-06 → W-10 (5 items, weight 14)

### Serialization notes

| Items      | Reason                                            |
|------------|---------------------------------------------------|
| W-05, W-06 | both write `hostmaintenance.go` and its test file |
| W-06, W-07 | both write `hostmaintenance.go` and its test file |

---

## 2. External Prerequisites

The design's Phase 0 table names five prerequisites, all available: the
FoundationDB operator's coordinator rules (P0-1, P0-2), `fdbcli` in the monitor
image (P0-3), FoundationDB's single maintenance zone (P0-4), and the
`DisruptionTarget` pod condition (P0-5). No item is blocked on them. P0-5 is
the secondary drain signal only, and a cluster without it runs on the sentinel.

| Prerequisite     | Blocks | Design           |
|------------------|--------|------------------|
| none outstanding | —      | design § Phase 0 |

---

## 3. Work Items

### W-01 — Add the HostMaintenance action to ControlPlaneOps

**Depends on:** —
**Labels:** operator, enhancement, high
**Design:** §4, §13
**Scenarios:** U-63, U-64, U-65
**Size:** M

Extend `operator/api/v1alpha2/controlplaneops_types.go` to the type in the
design's Appendix A: the action value `HostMaintenance`, the parameter block
`HostMaintenanceSpec` with `workerNode`, the seven step values `Holding`,
`EnteringMaintenance`, `MovingCoordinators`, `Releasing`, `AwaitingHost`,
`LeavingMaintenance`, and `Cleanup`, the status block `HostMaintenanceStatus`
with its two enums `HostMaintenanceCoordinatorPlacement` and
`HostMaintenanceDrainSignal`, the two new `XValidation` rules on the spec, the
widened CEL rule on `status.step.state`, and the `Worker` print column. Add the
new cases to `operator/internal/controllers/controlplane/cel_validation_test.go`
beside the existing parameter-block immutability cases. The validating webhook
in `operator/internal/webhook/controlplaneops_validator.go` needs no change,
and U-65 asserts that it already rejects the new action against a managed
control plane.

**Acceptance criteria**

- [ ] The type matches the design's Appendix A field for field, and
      `.claude/skills/api-design/scripts/check-crds.py --changed` reports no error
- [ ] `make -C operator manifests generate` and `make helm-sync` produce no
      further diff
- [ ] U-63, U-64, and U-65 pass

### W-02 — Add the coordinator choice as a pure function

**Depends on:** W-01
**Labels:** operator, enhancement, high
**Design:** §5.5
**Scenarios:** U-01, U-02, U-03, U-04, U-05, U-06, U-07, U-08, U-09, U-10, U-11, U-12, U-13, U-14, U-15, U-16, U-85
**Size:** M

Write `operator/internal/controllers/controlplane/coordinators.go`: a function
that takes the current cluster file, the `FoundationDBCluster` process groups
with their class, fault domain, conditions, and removal timestamp, the window's
zone, and the set of other leaving zones, and returns the coordinator set to
write, the placement, and whether anything is to be written. It implements the
three steps and the algorithm summary of design §5.5: keep the entries already
off the zone, fill from stateful classes in the order `log`, `storage`,
`transaction` and by process group ID, one host at a time first, then hosts
already holding one, and `Unchanged` when the list cannot be filled. Addresses
are built from the cluster name, the process group ID, the namespace, the DNS
domain, and the port read from the current file, and a file whose entries carry
different ports is an error. The function reads nothing from the API and is
tested as a table.

**Acceptance criteria**

- [ ] U-01 through U-16 and U-85 pass as table tests in `coordinators_test.go`
- [ ] The function has no dependency on `client.Client`

### W-03 — Build the fdbcli command Jobs and the sentinel Job

**Depends on:** —
**Labels:** operator, enhancement, high
**Design:** §5.3, §5.4
**Scenarios:** U-17, U-18, U-19, U-20, U-21, U-22, U-75, U-89, U-90
**Size:** S

Write `operator/internal/controllers/controlplane/fdbcommand.go`: a builder for
the command Job of design §5.4, named `<operation>-<command>`, running
`fdbMonitorImage` with `fdbcli -C /var/fdb/fdb.cluster --timeout 30 --exec
"<command>"`, mounting the cluster-file ConfigMap key, carrying the FoundationDB
node selector and tolerations plus a toleration of
`node.kubernetes.io/unschedulable`, a required node affinity away from the
window's worker that is dropped when that worker is the only node matching the
selector, `restartPolicy: Never`, `backoffLimit: 3`,
`activeDeadlineSeconds: 120`, `ttlSecondsAfterFinished: 3600`, and an owner
reference to the operation. Beside it, a builder for the sentinel Job of design
§5.3, named `<operation>-sentinel`, running the same image with
`sleep infinity`, pinned with `spec.nodeName`, with `backoffLimit: 0` and no
label the budget selects. Also a reader that classifies an existing Job as
running, `Complete`, or `Failed`, and that reads the pod log of a `Complete`
`maintenance-status` Job.

**Acceptance criteria**

- [ ] U-17 through U-22, U-75, U-89, and U-90 pass in `fdbcommand_test.go`
- [ ] Both builders are deterministic for the same inputs, so a replay produces
      the same object

### W-04 — Add the HostMaintenance graph and the lock held per step

**Depends on:** W-01
**Labels:** operator, enhancement, high
**Design:** §6, §7.3
**Scenarios:** U-61, U-62, U-83, U-84, U-93
**Size:** M

Add the `HostMaintenance` graph to `opsGraphs()` in
`operator/internal/controllers/controlplane/graphs.go` as the line of design
§6, with `Holding` abortable and the deadlines of its table, and let a graph
declare which of its steps hold the control plane's lock. Change
`controlplaneops_controller.go` so that the lock is acquired on the transition
into the first lock-holding step and released on the transition out of the last
one, with the phase `Running` from the first step, while the three existing
actions declare every step as lock-holding and keep their behavior. A window
that fails or is deleted while holding the lock releases it from its terminal
transition, and one that fails outside the lock-holding steps touches the lock
not at all. The `Holding` deadline is set from the latest `drainObservedAt`
across the namespace's windows by W-07; this item gives the step its static
six-hour budget.

**Acceptance criteria**

- [ ] U-61, U-62, U-83, U-84, and U-93 pass
- [ ] `TestEveryActionsGraphIsALine` still passes with the fourth graph
- [ ] Every existing test in `controlplaneops_test.go` passes unchanged

### W-05 — Implement Holding, Releasing, Cleanup, and the teardown

**Depends on:** W-01, W-03, W-04
**Conflicts with:** W-06
**Labels:** operator, enhancement, high
**Design:** §5.2, §5.8, §5.9, §5.10
**Scenarios:** U-23, U-24, U-25, U-26, U-27, U-28, U-29, U-30, U-31, U-32, U-33, U-91
**Size:** M

Create `operator/internal/controllers/controlplane/hostmaintenance.go` with the
step dispatch for the action and the three steps that touch only Kubernetes
objects. `Holding` records the zone from the Node's `kubernetes.io/hostname`
label and the process groups whose fault domain it is, labels the worker's
FoundationDB pods with `storage.simplyblock.io/fdb-maintenance-worker`, creates
the budget `sb-fdb-maintenance-<hash>` at `maxUnavailable: 0`, creates the
sentinel Job of W-03, and completes when the budget reports zero allowed
disruptions and `drainObservedAt` is set. `Releasing` deletes the budget and
the label and completes when the budget is gone. `Cleanup` removes a label left
behind. The terminal teardown runs from every failing and aborted transition,
best effort, deleting the budget and the label and emitting
`MaintenanceMarkersLeft` when it cannot. The event reasons of design §11 that
these steps emit go into `events.go`. The observation of the drain that
completes `Holding` is W-07; until it lands, the step's completion condition is
satisfied only by a `drainObservedAt` a test writes.

**Acceptance criteria**

- [ ] U-23 through U-33 and U-91 pass in `hostmaintenance_test.go`
- [ ] The storage node's label and budget on a converged worker are untouched by
      every step and by the teardown (U-27)
- [ ] The test plan's `Test` column names the implementing functions for these
      rows

### W-06 — Implement the database steps of the window

**Depends on:** W-01, W-02, W-03, W-04, W-05
**Conflicts with:** W-07
**Labels:** operator, enhancement, high
**Design:** §5.5, §5.6, §5.7, §5.9
**Scenarios:** U-34, U-35, U-36, U-37, U-38, U-39, U-40, U-41, U-42, U-43, U-44, U-45, U-46, U-48, U-50, U-51, U-68, U-69, U-70, U-71, U-86, U-87, U-88, U-92, U-95
**Size:** L

The item stays whole because its four steps share one write-ahead pattern, one
Job reader, and one status block, and splitting it leaves a window that enters
maintenance and never leaves it.

Add to `hostmaintenance.go` the steps that issue commands and wait for the
database. `EnteringMaintenance` issues `maintenance on <zone> <seconds>` with
the `AwaitingHost` deadline, records `maintenanceUntil`, emits
`MaintenanceZoneTakenOver` on any other window whose zone was active, and
completes on `Complete` or on `Failed` with `MaintenanceModeNotSet`.
`MovingCoordinators` calls W-02 with the leaving zones, writes the set and the
placement to status before creating the `coordinators` Job, holds while the
cluster-file ConfigMap still names a process on the zone, completes when it
does not or when the placement is `Unchanged`, and fails on its deadline with
`CoordinatorsReverted` when a `Complete` Job did not change the file.
`AwaitingHost` completes when no recorded process group carries
`MissingProcesses` or remains in the status. `LeavingMaintenance` issues
`maintenance status`, reads its log, issues `maintenance off` only when the
zone is the window's own, and completes with `MaintenanceModeLeft` on any
failure. A finished window reports `Completed` and its placement.

**Acceptance criteria**

- [ ] Every scenario listed above passes in `hostmaintenance_test.go`
- [ ] No step holds the budget on a `Failed` Job (U-36, U-44)
- [ ] An operator restart in `MovingCoordinators` reads the existing Job rather
      than creating a second (U-46)

### W-07 — Observe the drain and slide the Holding deadline

**Depends on:** W-05
**Labels:** operator, enhancement, high
**Design:** §5.3, §6, §7.2
**Scenarios:** U-72, U-76, U-77, U-78, U-79, U-80, U-81, U-82, U-94, U-96, U-97, U-98
**Size:** M

Add to the operations controller a watch on pods in the namespace that maps a
pod on the worker of a `Holding` window to that window, and in `Holding` the
two observations of design §5.3: the sentinel pod's deletion timestamp or its
absence after the Job recorded one writes `drainObservedBy: Sentinel`, and a
`DisruptionTarget` condition with reason `EvictionByEvictionAPI` on any other
pod of the worker writes `drainObservedBy: PodDisruption`. A recorded
observation is never overwritten. Make the `Holding` deadline run from the
latest `drainObservedAt` across the namespace's windows, from the window's
creation while none exists, and have a change to one window's `drainObservedAt`
enqueue every other window in `Holding`. A window whose drain is observed while
another window holds the lock waits in `Holding` with `MoveQueued`.

**Acceptance criteria**

- [ ] Every scenario listed above passes
- [ ] A window older than the deadline keeps holding while another window
      observed a drain inside it (U-97)
- [ ] The budget is gone after a `Holding` deadline expiry (U-82)

### W-08 — Raise a window on the cordon of a FoundationDB host

**Depends on:** W-01
**Labels:** operator, enhancement, high
**Design:** §5.1, §7.2
**Scenarios:** U-52, U-53, U-54, U-55, U-56, U-57, U-58, U-59, U-60, U-74
**Size:** M

Add to `operator/internal/controllers/controlplane/controlplane_controller.go`
a `Watches(&corev1.Node{}, …)` mapping a Node to the `ControlPlane` of the
namespace that holds a FoundationDB pod on it, found by the label
`foundationdb.org/fdb-cluster-name` and `spec.nodeName`, and the raise of
design §5.1: a cordoned Node with such a pod ensures the `ControlPlaneOps`
`<controlplane>-maintenance-<hash>` with the worker in `spec.hostMaintenance`,
where the hash is the first eight hex digits of the SHA-256 of the worker's
name. An uncordon aborts a window still `Holding`, leaves a later one running,
and deletes a terminal one, and a deleted Node is treated as an uncordon for
the terminal record. Emit `HostMaintenanceRaised` on the `ControlPlane`.

**Acceptance criteria**

- [ ] U-52 through U-60 and U-74 pass in `controlplane_controller_test.go`
- [ ] A schedulable Node and a cordoned Node without FoundationDB pods raise
      nothing (U-53, U-54)

### W-09 — Export the HostMaintenance metrics

**Depends on:** W-06, W-07
**Labels:** operator, enhancement, medium
**Design:** §11
**Scenarios:** —
**Size:** S

Add to `operator/internal/controllers/controlplane/metrics.go` the four series
of design §11: `simplyblock_controlplane_fdb_maintenance_zone_state`,
`simplyblock_controlplane_fdb_eviction_hold_seconds` with its `signal` label,
`simplyblock_controlplane_fdb_holding_windows_count`,
`simplyblock_controlplane_fdb_command_duration_seconds`, and
`simplyblock_controlplane_fdb_coordinator_moves_total`, and set them from the
steps: the zone gauge from `EnteringMaintenance` to `LeavingMaintenance` and on
every terminal path, the hold histogram from `drainObservedAt` to the budget's
deletion, the holding gauge from the count of windows in `Holding`, the command
histogram from each Job's start and end, and the moves counter from the
placement at `MovingCoordinators`.

**Acceptance criteria**

- [ ] The five series are registered and appear on the metrics endpoint with the
      labels of design §11
- [ ] The zone gauge reads zero after a window fails in `MovingCoordinators`

### W-10 — Add the envtest suite with the FoundationDB CRDs

**Depends on:** W-06, W-07, W-08
**Labels:** operator, enhancement, medium
**Design:** §6, §12
**Scenarios:** I-01, I-02, I-03, I-04, I-05, I-06, I-07, I-08, I-09, I-10
**Size:** L

The size is the suite's bootstrap, which cannot be split from the first
scenario that needs it, and the ten scenarios share one fixture.

Add an `envtest` suite under `operator/internal/controllers/controlplane/` that
installs the FoundationDB CRDs from
`helm-charts/charts/simplyblock-operator/templates/`, runs the ControlPlane and
ControlPlaneOps controllers, writes a `FoundationDBCluster` status by hand, sets
Job conditions by hand, rewrites the cluster-file ConfigMap, and drives the
window with real `Eviction` requests. No suite with these CRDs exists today, so
the suite's bootstrap is part of the item.

**Acceptance criteria**

- [ ] I-01 through I-10 pass under `make -C operator test`
- [ ] The suite runs without network access

### W-11 — Add the end-to-end drain suite

**Depends on:** W-06, W-07, W-08
**Labels:** operator, enhancement, medium
**Design:** §5.11, §12
**Scenarios:** E-01, E-02, E-03, E-04, E-05, E-06, E-07, E-08, E-10, E-11, E-12, E-13, E-14
**Size:** L

The size is the live harness, shared by every scenario, and one lab run covers
them all.

Add a live-cluster suite that drains FoundationDB hosts under a management-API
transaction load and asserts the cluster file, `fdbcli status json`, the
maintenance zone, the commit error count, and the control plane's volume
records, including the pool-wide cordon with serial drains of design §5.11.
E-08 needs a lab with exactly three FoundationDB hosts and E-14 one on cloud
volumes whose drained node is replaced; the item lands the suite and runs the
rest on the four-worker lab, and records those two as pending the labs.

**Acceptance criteria**

- [ ] Every listed scenario except E-08 and E-14 has run green on a live cluster,
      with the run's evidence attached to the issue
- [ ] E-08 and E-14 run green once their labs exist, or stay in the test plan's
      gap table with the lab named

### W-12 — Add the hand-off steps and the exclusion record to ControlPlaneOps

**Depends on:** W-01
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §4.2, §4.3, §5.12, §6
**Scenarios:** U-111, U-113
**Size:** S

Add the steps `ExcludingProcesses` and `IncludingProcesses` to the
`ControlPlaneOpsStep` enum and the CEL rule on `status.step.state`, the field
`status.hostMaintenance.excludedProcesses`, the two steps to the window's graph
between `MovingCoordinators` and `Releasing` and between `AwaitingHost` and
`LeavingMaintenance`, `ExcludingProcesses` to the lock-holding steps, and both
to the delete guard. Regenerate the manifests and the chart.

**Acceptance criteria**

- [ ] The step enum, the CEL rule, and Appendix A agree on the two new values
- [ ] `ExcludingProcesses` holds the lock and `IncludingProcesses` does not (U-111)
- [ ] The delete guard refuses both steps, and the guard-and-graph agreement
      test passes (U-113)
- [ ] `make -C operator manifests generate` and `make helm-sync` produce no
      further diff

### W-13 — Hand the transaction roles off before the release

**Depends on:** W-06, W-12
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §5.12, §8, §10, §11
**Scenarios:** U-102, U-103, U-104, U-105, U-106, U-107, U-108, U-109, U-110
**Size:** M

Implement `ExcludingProcesses` and `IncludingProcesses` in
`hostmaintenance_database.go`: read the recorded groups' classes from the
`FoundationDBCluster` status, write the non-storage IDs to
`excludedProcesses` before the Job, issue the waiting `exclude` over their
localities with a longer `fdbcli` timeout, complete on `Complete` with
`ProcessesHandedOff` and on `Failed` with `ProcessesNotHandedOff`, issue the
`include` after `AwaitingHost` with `ProcessesIncluded` or
`ProcessesLeftExcluded`, and create the include Job from the terminal teardown
when the window ends early with an exclusion recorded. Add the four events.

**Acceptance criteria**

- [ ] Every scenario listed above passes in `hostmaintenance_database_test.go`
- [ ] No storage-class process group is ever named in an `exclude` command (U-102)
- [ ] A `Failed` exclude does not hold the budget (U-104)
- [ ] A window that fails after excluding leaves an `include` Job behind (U-110)

### W-14 — Apply dedicated stateless processes

**Depends on:** —
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §1, §5.12, §13
**Scenarios:** U-112
**Size:** S

Change `foundationDBCluster` in `foundationdb.go` to apply
`processCounts.stateless` equal to the log count instead of `-1`, so the
transaction system is recruited on stateless processes and the hand-off of
W-13 leaves nothing on the storage process. The item is independent of W-13
and is what makes W-13 complete on a drain.

**Acceptance criteria**

- [ ] `processCounts.stateless` equals `processCounts.log` for every replica
      count the install test covers (U-112)
- [ ] The design's §1 and §13 describe the applied layout

---

## 4. Not in This Plan

| Deferred                                                                                                                          | Why                                                                                                        | Where it goes                                                    |
|-----------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------|
| The sbcli transaction retry spike                                                                                                 | Design §14 Q3 is a control-plane question, and its answer changes the design's claim rather than its items | A spike in the sbcli repository before W-11 runs E-03            |
| M-01 through M-05                                                                                                                 | They need failure injection or orchestration nothing automates yet                                         | The test plan's manual section, run by hand against W-11's suite |
| The maintenance mode length, the restamp on a planned move, the zone takeover wait, and the raise on an Unavailable control plane | Design §14 Q1, Q2, Q4, and Q5 change a constant or a condition, not an item                                | Answered in the design and folded into W-06 or W-08              |
| Whether the control plane accepts a graceful node shutdown during a node add, and whether the storage window's gate counts an add | Behavior of the `StorageNodeOps` window, not of this design                                                | A spike against the storage node's design                        |
