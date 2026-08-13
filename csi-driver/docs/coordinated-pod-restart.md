# Coordinated Pod Restart on Shared NVMe-oF Subsystems

## Problem

The Guardian daemon watches for broken logical volumes (lvols) and automatically
restarts the pod that owns the affected volume. This works correctly when each pod
has its own private NVMe-oF subsystem (its own NQN).

The problem arises when **multiple pods share a single NVMe-oF subsystem**. In
simplyblock, a subsystem is identified by its NQN (NVMe Qualified Name). If two
pods — call them A and B — both have volumes on the same NQN, they share one
physical connection to the storage node.

Restarting pod A alone tears down that shared NVMe-oF connection. Pod B, which
was healthy, loses its storage mid-flight. This is worse than doing nothing.

## What "Shared Subsystem" Means

When a volume is mounted on a node, the CSI driver establishes an NVMe-oF
connection to the storage node. Simplyblock identifies each subsystem by an NQN
of the form:

```
nqn.2023-01.io.simplyblock:<clusterID>:lvol:<masterLvolID>
```

Multiple volumes (and therefore multiple pods) can be placed on lvols that belong
to the same subsystem. When that happens, they all use the same single NVMe-oF
connection — the one whose NQN is derived from the master lvol.

## The NQN Index

`initiator.go` maintains two package-level maps that are updated every time a
volume is connected or disconnected:

```
nqnByLvolID  map[string]string    // lvolID  →  NQN
lvolIDsByNQN map[string][]string  // NQN     →  []lvolID
```

`SubsystemLvolIDs(lvolID)` looks up both maps and returns all lvol IDs that share
the same NQN as the given lvolID, **including itself**. Three outcomes are possible:

| Return value | Meaning |
|---|---|
| `nil` | lvolID not yet in the index — connection not established yet, or node just restarted |
| `[]string{lvolID}` | sole member — private subsystem, safe to restart individually |
| `[]string{lvolID, siblingA, ...}` | shared subsystem — coordinated restart required |

## Decision Flow

Every Guardian tick, broken lvols are routed through `restartBrokenLvols`:

```
for each broken lvolID:
    siblings = SubsystemLvolIDs(lvolID)

    if siblings == nil          → individual restart (with shared-subsystem guard*)
    if len(siblings) == 1       → individual restart
    if len(siblings) > 1        → coordinatedSubsystemRestart(all siblings)
        mark all siblings as handled to skip on next loop iteration
```

\* If the index is not yet populated and the pod's StorageClass indicates a shared
subsystem, the restart is **suppressed for this tick**. Once `reconnectSubsystems`
runs and fills the index (typically within one poll interval), the pod is routed
correctly on the next tick.

## The All-or-Nothing Gate

`coordinatedSubsystemRestart` enforces a strict gate: **every candidate pod must
pass every eligibility check before any pod is deleted**.

Eligibility checks (applied to each candidate):

1. **Controller-managed** — pod must be owned by a Deployment, StatefulSet, or
   similar controller so it will be recreated automatically after deletion.
2. **Opted in** — pod must carry the `simplyblock.io/auto-restart-on-pathloss: "true"`
   label, or use a StorageClass that carries that annotation.
3. **Not in backoff** — at least `RestartBackoff` (default 10 min) must have
   elapsed since the last restart of this pod.

If any candidate fails any check, **the entire group is suppressed** for this
tick. No pod is deleted. The Guardian will re-evaluate on the next tick, giving
the blocking pod time to clear its backoff or opt in.

Only when all checks pass does the Guardian delete all candidate pods in one loop,
recording each deletion in the restart backoff tracker and removing the pod→lvol
mapping.

```
Gate pass?  YES for all  →  delete pod A, delete pod B, delete pod C  (together)
Gate pass?  NO  for any  →  delete nobody, retry next tick
```

This prevents the "partial teardown" failure mode where pod A is restarted,
the shared NVMe connection drops, and pod B crashes with I/O errors even though
it was healthy.

## Edge Cases

**Index not yet populated (`siblings == nil`).**
This happens at node startup before `reconnectSubsystems` has run, or for a
volume whose Connect path hasn't completed. The Guardian suppresses the restart
if the StorageClass signals a shared subsystem, and emits a Kubernetes Event so
the situation is observable. It resolves itself within one poll interval.

**Only some pods in the group are broken.**
`coordinatedSubsystemRestart` receives the full sibling list from the NQN index.
It then filters to candidates that actually have a broken pod. If only pod A is
broken and pod B is healthy (no broken-lvol record), pod B is not included as a
candidate — the restart proceeds for pod A alone via the coordinated path, which
in this case produces a one-element candidate list and degrades safely to a
single-pod delete.

**Backoff prevents the group from restarting.**
If pod A was restarted 3 minutes ago and the backoff is 10 minutes, the whole
group is held back. This is intentional: deleting pod B would still disconnect
the shared NVMe subsystem and could interrupt pod A mid-recovery.

**Pod not opted in.**
A single non-opted-in pod blocks the entire group. Operators must opt in all pods
that share a subsystem, or none of them will receive automatic restarts.

## Observability

- Every suppression and every restart is logged at `klog.Warning` level.
- When the NQN index is not yet populated, a Kubernetes Event is emitted on the
  pod (`reason: SharedSubsystemRestartSuppressed`) so it appears in `kubectl
  describe pod`.
- Dry-run mode (`DryRun: true`) runs all checks and logs what would have been
  deleted without actually deleting anything.
