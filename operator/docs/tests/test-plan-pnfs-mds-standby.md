# Test Plan: Warm-Standby pNFS Metadata Server

Related design: [`designs/design-pnfs-mds-standby.md`](../designs/design-pnfs-mds-standby.md)
Builds on: [`test-plan-pnfs-rwx.md`](test-plan-pnfs-rwx.md), whose restart scenarios (F-11 … F-20) this plan does not repeat.

Harness: `atlas-lib` (`make -C atlas-lib test`), `csi-driver` (`make -C csi-driver unit-test`) for the runner and guest agent, `operator` (`make -C operator test`), the on-node integration job for two guests against a real target, and the sbtest framework (`make -C test/framework run SUITE=pnfs-fio`) against a live cluster.

Scope: the operator, the runner, the guest agent, and the Kubernetes surface of this repository. The control plane (`sbcli`) and SPDK are dependencies, faked at the boundary. The guest kernel patch of design P0-1 is built in the pnfs-os repository, and this plan asserts its behavior as a guest sees it (§3).

Scenario IDs are permanent. A scenario the design no longer describes keeps its row, struck through, with what supersedes it.

| Prefix | Class             | Harness                                                                                                      |
|--------|-------------------|--------------------------------------------------------------------------------------------------------------|
| `U-`   | Unit              | no cluster: pure functions, a fake reservation device, a fake guest system (sysfs, systemd, mounts)          |
| `O-`   | Operator unit     | fake client, fake link registry, fake `ExportService` and `MDSService`, mock control plane                   |
| `I-`   | Guest integration | two MDS guests from the MDS image and one NVMe-oF target with reservations, no Kubernetes                    |
| `E-`   | End-to-end        | live cluster with at least two KVM-capable nodes                                                             |
| `F-`   | Failure injection | live cluster plus a fault: a node powered off, a network cut, an API server stop, a refused reservation call |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column names the implementing function, e2e spec, or sbtest suite and detector, or `—` when the scenario is not covered yet. Nothing in this design is implemented, so every row is `—`.

---

## Table of Contents

1. [Unit Tests](#1-unit-tests)
2. [Operator Unit Tests](#2-operator-unit-tests)
3. [Guest Integration Tests](#3-guest-integration-tests)
4. [End-to-End Tests](#4-end-to-end-tests)
5. [Failure-Injection Tests](#5-failure-injection-tests)
6. [Axis Coverage](#6-axis-coverage)
7. [Coverage Summary](#7-coverage-summary)
8. [What Is Not Yet Covered](#8-what-is-not-yet-covered)

---

## 1. Unit Tests

Pure functions and the agent's state machine, against a fake reservation device that records every register, acquire, and preempt and answers reports from that record. Numbering runs continuously across the groups.

### Reservation key (§6.2)

File: `atlas-lib/nfsexport/mdskey_test.go`

| #    | Scenario                                                                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-01 | `MDSReservationKey`: top byte `0x01`, bits 55 to 32 the StatefulSet UID's tag, low 32 bits the epoch                            | Positive | —    |
| U-02 | Two StatefulSet UIDs at the same epoch give different keys. One UID at two epochs gives different keys                          | Positive | —    |
| U-03 | No key is a possible client key: for every boot time from 2000 to the end of `u32`, `(cl_boot << 32)` never has top byte `0x01` | Boundary | —    |
| U-04 | Epoch 0 is refused, and epoch `2^32 - 1` is the largest accepted                                                                | Boundary | —    |

### Fence sequence (§6.4)

File: `atlas-lib/nvme/reservation_test.go` and the agent's fence in `csi-driver/internal/mds/agent/fence_test.go`

| #    | Scenario                                                                                                                            | Type       | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-05 | An older MDS key holds the reservation: the agent registers, preempts it, and the second report shows its own key holding           | Positive   | —    |
| U-06 | Client keys are registered beside the holder: none is preempted, and all are still registered afterward                             | Positive   | —    |
| U-07 | Two stale MDS keys, one holding and one only registered: both are gone afterward                                                    | Positive   | —    |
| U-08 | No reservation and no registrant (a new namespace): register, then acquire without preempt                                          | Boundary   | —    |
| U-09 | The agent's own key already holds and nothing else is in the MDS range: no call is made                                             | Boundary   | —    |
| U-10 | A preempt fails: the fence returns the device and the step, and the remaining devices are not touched                               | Negative   | —    |
| U-11 | Another host is registered with the agent's key: refused before registering, as a collision the caller can tell from other failures | Negative   | —    |
| U-12 | A client key holds the reservation: refused, nothing preempted                                                                      | Negative   | —    |
| U-13 | The report after fencing still shows another MDS-range key (the target ignored the preempt): refused                                | Negative   | —    |
| U-14 | The target returns Invalid Field for Preempt and Abort: the fence never issues it, only the plain Preempt                           | Regression | —    |

### Guest agent roles (§5.3, §8)

File: `csi-driver/internal/mds/agent/promote_test.go`

| #    | Scenario                                                                                                                                                               | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-15 | `Promote` on a standby: connect all, fence all, mount the state volume, write `pnfs_mds_pr_key`, start the NFS server, in that order. `Describe` then reports `Active` | Positive | —    |
| U-16 | `Promote` whose fence fails on the last of five devices: nothing mounted, the key parameter unwritten, nfsd not started, role `Standby`                                | Negative | —    |
| U-17 | `Promote` twice at the same epoch: the second returns success and repeats no side effect                                                                               | Boundary | —    |
| U-18 | `Promote` below the accepted epoch is refused. `Promote` at a higher epoch on an `Active` guest is refused                                                             | Negative | —    |
| U-19 | `CreateExport` on a standby is refused with no connect and no mount                                                                                                    | Negative | —    |
| U-20 | `CreateExport` on an active guest for a namespace not yet fenced fences it before `mkfs`                                                                               | Positive | —    |
| U-21 | Health: a booted standby is `SERVING` with no state volume. An active guest is `SERVING` only with the state volume mounted and nfsd threads                           | Positive | —    |
| U-22 | The state volume carries no filesystem: formatted ext4 once. It carries one: mounted without formatting                                                                | Boundary | —    |

### Runner and identity (§5.2, §8)

File: `csi-driver/internal/mds/qemu/qemu_test.go`, `csi-driver/internal/mds/relay/relay_test.go`

| #    | Scenario                                                                                                              | Type                | Test |
|------|-----------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-23 | Both ordinals' kernel command lines carry the StatefulSet's name as the hostname, not the pod name                    | Positive            | —    |
| U-24 | The host NQN per ordinal: distinct for 0 and 1, the same for the same UID and ordinal, and accepted by `nqn.HostUUID` | Positive            | —    |
| U-25 | The relay resolves `Promote`'s connections for the pod's own host NQN, and drops connections the operator sent        | Positive + Negative | —    |

---

## 2. Operator Unit Tests

The `NFSExport` reconciler and its MDS helpers against a fake client, a fake link registry, and fake `ExportService` and `MDSService` peers that count calls. File: `operator/internal/controller/nfsexport_standby_unit_test.go` unless a group names another.

### Workload (§5.1, §5.4, §4.1)

| #    | Scenario                                                                                                                                  | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| O-01 | The StatefulSet has 2 replicas, `Parallel`, `OnDelete`, a required hostname anti-affinity on its own pods, and the `kvm-capable` selector | Positive | —    |
| O-02 | `disableStandby: true` builds 1 replica                                                                                                   | Positive | —    |
| O-03 | No claim template, no state StorageClass, and no admission policy is created                                                              | Positive | —    |
| O-04 | The state volume is created once in the first export's pool and its handle recorded. A second reconcile makes no create call              | Positive | —    |
| O-05 | `CreateVolume` returns 500: requeued, nothing recorded, no promotion, `MDSStateUnavailable`                                               | Negative | —    |
| O-06 | StatefulSet deletion: the finalizer deletes the state volume, a 404 counts as done, and the finalizer is removed                          | Positive | —    |

### Promotion and the record (§4.3, §7.1, §7.3)

| #    | Scenario                                                                                                                                                                                             | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| O-07 | First export, no record: the `Ready` standby with a session is recorded at epoch 1 and promoted with `key(1)`                                                                                        | Positive | —    |
| O-08 | The record write conflicts: no `Promote` is sent, and the pass requeues                                                                                                                              | Negative | —    |
| O-09 | Two reconciles decide a takeover from the same cached `resourceVersion`: exactly one record write succeeds and exactly one `Promote` is sent                                                         | Negative | —    |
| O-10 | The full sequence: record, withdraw (EndpointSlices empty, conntrack forget for the old IP), `Promote`, then per export `mdsPodName`, `CreateExport`, `assembledBy`, EndpointSlice, conntrack forget | Positive | —    |
| O-11 | Restart after the record write and before `Promote`: the next pass sends `Promote` at the recorded epoch, not a new one                                                                              | Positive | —    |
| O-12 | Restart in the middle of reassembly: only exports whose `assembledBy` is not the new instance are reassembled                                                                                        | Positive | —    |
| O-13 | `Promote` fails with a fence error: `MDSFenceFailed`, no `CreateExport`, EndpointSlices stay empty, retried with backoff at the same epoch                                                           | Negative | —    |
| O-14 | `Promote` fails with a collision: the epoch is incremented through a conditioned write and `Promote` retried with the new key                                                                        | Negative | —    |
| O-15 | Reassembly runs at most eight exports at once with 20 bound                                                                                                                                          | Boundary | —    |
| O-16 | An export bound while the record's target is not `Active` waits in `Pending`                                                                                                                         | Boundary | —    |
| O-17 | Promotion with zero exports bound: the state volume alone is fenced and the guest becomes active                                                                                                     | Boundary | —    |

### Detection (§7.2)

| #    | Scenario                                                                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| O-18 | No session and pod not `Ready` for less than the window: held, `MDSFailoverHeld`, no record write                                  | Negative | —    |
| O-19 | The same at exactly the window: still held. One tick past it: the takeover starts                                                  | Boundary | —    |
| O-20 | Pod and node `Ready` stale (API server blip) while the session is up: no takeover                                                  | Negative | —    |
| O-21 | Session down while pod and node are `Ready`: no takeover                                                                           | Negative | —    |
| O-22 | A new leader observes a lost active: the window starts at leadership, not at the loss                                              | Negative | —    |
| O-23 | The active pod gets a deletion timestamp: the takeover starts at once, without the window                                          | Positive | —    |
| O-24 | Loss observed but the standby has no session: held, `MDSStandbyMissing`, and the first guest to report `Standby` is promoted later | Boundary | —    |

### Roles over time (§7.4, §7.5)

| #    | Scenario                                                                                                                         | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------|----------|------|
| O-25 | A session reports `Active` with an instance that is not the record's: its pod is deleted, `MDSDemoted`                           | Positive | —    |
| O-26 | The old active's pod stays `Terminating` on a lost node: the takeover completes and `MDSStandbyMissing` repeats until it is gone | Positive | —    |
| O-27 | A template change: the standby is deleted first, and the active only after the new pod reports `Standby`                         | Positive | —    |
| O-28 | `disableStandby` set while ordinal 1 is active: switchover to 0 before the scale to 1 replica                                    | Boundary | —    |
| O-29 | `disableStandby` cleared: scaled to 2, the new pod is not promoted                                                               | Positive | —    |

### Observability and RBAC (§10, §8)

| #    | Scenario                                                                                                                                  | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| O-30 | Each event of design §10 is emitted with its reason and on the objects its row names                                                      | Positive | —    |
| O-31 | `failovers_total` counts by cause and result, and `takeover_seconds` observes each phase once per takeover                                | Positive | —    |
| O-32 | The ClusterRole has no `persistentvolumeclaims` `create`, and the record and finalizer writes need nothing beyond `statefulsets` `update` | Negative | —    |

---

## 3. Guest Integration Tests

Two guests booted from the MDS image, each with its own host NQN, against one NVMe-oF target with reservations, driven through the agent's gRPC API with no Kubernetes. The harness does not exist yet (§8). Rows assert the behavior of the kernel patch (design §6.3) and the agent together.

| #    | Scenario                                                                                                                                                        | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | With `pnfs_mds_pr_key` set and the agent's fence done, a client's GETDEVICEINFO succeeds, and the report shows no registration made by nfsd                     | Positive | —    |
| I-02 | Guest A is fenced by B: A's GETDEVICEINFO fails, and the report before and after is identical (A did not register again)                                        | Negative | —    |
| I-03 | A's stale-key fence runs while B holds: B's key stays registered and holding                                                                                    | Negative | —    |
| I-04 | Writing `pnfs_mds_pr_key` while nfsd has threads returns `EBUSY`                                                                                                | Negative | —    |
| I-05 | B promotes while A serves: A's next metadata write fails, and A's filesystem shuts down (Open Question 3 records what is observed)                              | Positive | —    |
| I-06 | A and B are promoted concurrently at different epochs over three namespaces: at no instant can both write one namespace, and at most one agent reports `Active` | Negative | —    |
| I-07 | The state volume is fenced and mounted like an export namespace, and A cannot write it after B's promotion                                                      | Positive | —    |
| I-08 | A client registered before the takeover is still registered after it and its direct write succeeds                                                              | Positive | —    |
| I-09 | Writes queued from A at the target when B's preempt lands: record whether they reach the device, and that B's mount follows the preempt (design §6.6)           | Boundary | —    |

---

## 4. End-to-End Tests

A live cluster with at least two KVM-capable nodes, under the CSI e2e suite or `pnfs-fio`. Data-path rows are judged by `fio.checksum` and by the NFS WRITE-count check the CSI probe uses.

| #    | Scenario                                                                                                                                                       | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Default `spec.pnfs.mds`: two pods on two nodes. One reports `Active`, the other `Standby`, exports are `Ready`, and the standby's guest has no NVMe controller | Positive | —    |
| E-02 | One KVM-capable node: the standby is `Pending`, `MDSStandbyMissing` is emitted, and exports are served                                                         | Boundary | —    |
| E-03 | `disableStandby: true`: one pod, exports served, a pod delete recovers in place as `test-plan-pnfs-rwx.md` F-11                                                | Positive | —    |
| E-04 | `disableStandby` toggled on and off with exports `Ready` under `pnfs-fio`: no fio error, checksums clean                                                       | Positive | —    |
| E-05 | Both host NQNs are authorized on every export's volume and the state volume, and the standby can connect each once promoted                                    | Positive | —    |

---

## 5. Failure-Injection Tests

A live cluster under `pnfs-fio` with checksum verification, plus the fault named in the row. Destructive: these power off nodes and cut networks, and run only on lab clusters.

| #    | Scenario                                                                                                                                                                                    | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| F-01 | The active pod is deleted: the standby is promoted, no fio error, checksums clean, every client back on the direct path (WRITE count zero after the re-probe)                               | Positive | —    |
| F-02 | The active's node is powered off: the standby serves within the measured budget while the old pod is `Terminating`, no fio error, checksums clean                                           | Positive | —    |
| F-03 | The active's node is cut from the API server and the other nodes, with its storage path left up: the old guest is fenced (report shows only the new key), its clients move, checksums clean | Negative | —    |
| F-04 | The standby's `Promote` meets a refused reservation call on one export (the standby's NQN revoked on that volume): nothing mounted, `MDSFenceFailed`, recovery once the fault is removed    | Negative | —    |
| F-05 | The API server is stopped for less than the detection window: no takeover, `failovers_total` unchanged, no fio error                                                                        | Negative | —    |
| F-06 | The operator leader is deleted while both guests are healthy: no takeover                                                                                                                   | Negative | —    |
| F-07 | Two operator processes are made to decide the same takeover (leader election disabled in a test build, or the record hand-edited to repeat an epoch): one active, no double mount           | Negative | —    |
| F-08 | The partition of F-03 heals: the old active is demoted, boots as the standby, and the report never shows two MDS keys registered                                                            | Positive | —    |
| F-09 | A second takeover before the first one's grace ends                                                                                                                                         | Boundary | —    |
| F-10 | A takeover with 32 exports: every export is back inside `pnfs_export_restore`'s 60 seconds and no client sees `ESTALE`                                                                      | Boundary | —    |
| F-11 | Both pods lost, then one node returns: its guest is promoted at a new epoch and clients reclaim                                                                                             | Boundary | —    |
| F-12 | A client holds a lock across a takeover: the lock is reclaimed in grace                                                                                                                     | Positive | —    |
| F-13 | One client node is down during a takeover: grace runs its full length and the other clients recover                                                                                         | Boundary | —    |
| F-14 | An export grown just before a takeover: clients keep the direct path on the new active (device IDs stable, pnfs-os 0006)                                                                    | Positive | —    |
| F-15 | A takeover with zeroed layouts on: data written through a layout before it reads back intact on another node                                                                                | Positive | —    |

---

## 6. Axis Coverage

| Axis              | Values covered                                                    | IDs                                | Not covered                                                       |
|-------------------|-------------------------------------------------------------------|------------------------------------|-------------------------------------------------------------------|
| KVM-capable nodes | one, two, more than two                                           | E-02, E-01, F-02                   | —                                                                 |
| Standby toggle    | on, off, toggled at runtime                                       | E-01, E-03, E-04, O-28, O-29       | —                                                                 |
| Loss kind         | pod delete, node off, partition, both lost, false (API, operator) | F-01, F-02, F-03, F-11, F-05, F-06 | the active's guest crashing inside a running pod (runner restart) |
| Active ordinal    | 0 and 1                                                           | O-27, O-28                         | a live takeover from ordinal 1 to 0                               |
| Export count      | 0, 1, 20, 32                                                      | O-17, O-10, O-15, F-10             | —                                                                 |
| Writers           | one operator, two racing reconciles, two operator processes       | O-08, O-09, F-07                   | —                                                                 |
| Operator restart  | before record, after record, mid-reassembly, leadership move      | O-11, O-12, O-22, F-06             | restart between fence and nfsd start inside the guest             |
| Cluster count     | one storage cluster                                               | all                                | two storage clusters each with a pair, in one run                 |
| Client kernel     | Talos 6.18                                                        | F-01 … F-15                        | RHEL and Debian families                                          |

---

## 7. Coverage Summary

| Class                    | Scenarios | Covered | Not covered |
|--------------------------|-----------|---------|-------------|
| Unit (`U-`)              | 25        | 0       | U-01 … U-25 |
| Operator (`O-`)          | 32        | 0       | O-01 … O-32 |
| Guest integration (`I-`) | 9         | 0       | I-01 … I-09 |
| End-to-end (`E-`)        | 5         | 0       | E-01 … E-05 |
| Failure injection (`F-`) | 15        | 0       | F-01 … F-15 |
| **Total**                | **86**    | **0**   | **86**      |

The design is a draft and nothing is implemented. Unit and operator rows become coverable as the code lands. The `I-` and `F-` rows carry the safety claim and need harness work first.

---

## 8. What Is Not Yet Covered

| #           | Gap                                                        | Reason                                                                                                                                                                    |
|-------------|------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-25 | Every unit scenario                                        | Not implemented                                                                                                                                                           |
| O-01 … O-32 | Every operator scenario                                    | Not implemented                                                                                                                                                           |
| I-01 … I-09 | Two guests against a real target                           | The guest kernel patch (design P0-1) is not written, and no harness boots two guests against a target with reservations. The on-node job boots none                       |
| I-09        | Queued writes at the moment of the preempt                 | Needs a target that can hold a host's commands at the instant of the preempt, which neither SPDK nor nvmet offers without instrumentation                                 |
| E-01 … E-05 | Every end-to-end scenario                                  | Not implemented. E-05 also depends on design P0-2                                                                                                                         |
| F-01 … F-15 | Every failure-injection scenario                           | Not implemented. sbtest has no component that powers off a node, cuts a node's network selectively, or stops the API server at a point in the timed run                   |
| F-07        | Two operator processes deciding one takeover               | Needs a test build without leader election, or a hand edit of the record, and neither is a supported configuration                                                        |
| —           | The active's guest crashing inside a running pod           | `test-plan-pnfs-rwx.md` F-15 covers the restart in place. Whether the standby or the restarted guest is promoted depends on the detection window (design Open Question 1) |
| —           | A live takeover from ordinal 1 to ordinal 0                | Covered only in the operator unit tests. The live rows do not choose which ordinal is active                                                                              |
| —           | Operator restart between fence and nfsd start in the guest | The agent's `Promote` is one call. A restart of the runner during it is a guest reboot, which U-16 and O-11 cover from each side                                          |
| —           | Two storage clusters each with a pair                      | No live run has two storage clusters with exports                                                                                                                         |
| —           | RHEL- and Debian-family clients                            | The lab cluster is Talos                                                                                                                                                  |
| —           | SPDK's reservation behavior itself                         | Out of scope for this repository (design P0-3). I-01 … I-09 assert what a guest sees of it                                                                                |
