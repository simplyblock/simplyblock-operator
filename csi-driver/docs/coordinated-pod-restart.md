# Coordinated Pod Restart on Shared NVMe-oF Subsystems

## Problem

The Guardian daemon watches for broken logical volumes (lvols) and automatically
restarts the pod that owns the affected volume. This works correctly when each pod
has its own private NVMe-oF subsystem (its own NQN).

The problem arises when **multiple pods share a single NVMe-oF subsystem**. In
simplyblock, a subsystem is identified by its NQN (NVMe Qualified Name). If two
pods — call them A and B — both have volumes on the same NQN, they share one
physical connection to the storage node.

When the NVMe-oF path breaks, **both pod A and pod B are broken** — they share
the same physical connection, so neither can do I/O. The Guardian would only
consider restarting pod A because pod A's lvol is marked broken; pod B may not
yet appear broken in the Guardian's view.

Restarting pod A alone triggers NodeUnpublish → NodeUnstage → NodeStage →
NodePublish for the replacement pod. The NodeStage step reconnects the shared
NVMe-oF subsystem. From the kernel's perspective the NQN is now live again —
but **pod B's mount was established before the disconnect/reconnect cycle**. Pod
B's kernel mount is stale: the NVMe path looks connected but the mount context
is no longer valid. The Guardian sees the NQN as healthy and never restarts pod
B, leaving it silently broken.

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

This ensures a **clean disconnect → reconnect cycle** for the shared NVMe-oF
subsystem. The reason is mechanical, not protective: `Disconnect` in
`initiator.go` uses `selectDisconnectTarget`, which returns `shared=true` and
issues **no `nvme disconnect`** when more than one namespace of the subsystem is
still present on the node. As long as pod B holds its namespace symlink, deleting
pod A alone can never tear down the broken subsystem. Pod A's replacement then
tries to `Connect` on top of a still-broken kernel subsystem object, which may
fail or leave the connection in an inconsistent state. Deleting all pods together
lets the namespace count drop to one before the last `Disconnect` fires,
producing a genuine teardown followed by a clean `nvme connect` from the
replacement pods.

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
- Dry-run mode (`DryRun: true`) runs all checks and logs what would have been
  deleted without actually deleting anything.

Kubernetes Events are emitted so problems are visible via `kubectl describe pod`
without needing to dig through CSI node logs:

| Situation | Event reason | Emitted on |
|---|---|---|
| NQN index not yet populated | `AutoRestartSuppressed` | the suppressed pod |
| Non-opted-in pod blocks the group | `CoordinatedRestartBlocked` | the **blocking** pod |
| Opted-in pod held back by a peer | `CoordinatedRestartPending` | each **blocked** pod |

**Example — non-opted-in pod blocking the group:**

```
kubectl describe pod <opted-in-pod>
...
Events:
  Warning  CoordinatedRestartPending  guardian  Auto-restart of this pod is pending.
           It shares an NVMe-oF subsystem with pod simplyblock/worker-job-xyz,
           which did not pass restartability checks (not opted in, no owner
           controller, or in backoff). All pods in the shared-subsystem group
           must be restartable before any are restarted.

kubectl describe pod <blocking-pod>
...
Events:
  Warning  CoordinatedRestartBlocked  guardian  This pod is blocking coordinated
           auto-restart of a shared NVMe-oF subsystem group (3 pods). It did not
           pass restartability checks. Add label
           simplyblock.io/auto-restart-on-pathloss=true to this pod's controller,
           or set it on its StorageClass, to unblock the group.
```
