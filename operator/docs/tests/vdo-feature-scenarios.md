# VDO Client-Side Compression/Dedup — Scenario Matrix

Companion to the detailed test plan at
[`test-plan-issue-277-client-side-compression.md`](../../../.claude/worktrees/design-issue-277-client-side-compression/operator/docs/tests/test-plan-issue-277-client-side-compression.md)
(design branch) — that file has per-mechanism unit/integration/E2E breakdowns. This file
organizes the same feature into Good/Error/Load buckets (mirroring the Backup/Restore
scenario style) and tracks live-cluster status specifically for branch
`issue-277-client-side-compression-impl` on the `config-israel` cluster.

Status legend: **Tested** (verified with evidence, cited) · **Not tested** (identified gap,
not yet exercised) · **N/A** (doesn't apply to this feature).

---

## Good Cases (positive)

| # | Scenario | Status | Evidence |
|---|---|---|---|
| 1 | Pool with `clientCompression`+`clientDeduplication` → StorageClass → PVC → Pod → VDO device created with both flags on | **Tested** (this session) | CSI node log: `lvcreate --type vdo ... --compression y --deduplication y`; pod mounted `/dev/mapper/vdo--...` XFS successfully |
| 2 | Compression/dedup produce measurable savings on compressible/duplicate data | **Tested** (prior session, PR #402) | ~104MB (1 original + 2 exact duplicates) → 89% `vdostats` savings. Not yet re-run on this cluster/session |
| 3 | Delete + recreate pod (same PVC) → VDO device reattached, not recreated; data survives | **Tested** (prior session, PR #402) | Checksum match confirmed before/after; `CreateOrAttachVDO`'s reactivate path (`vgchange -ay`) taken, not `lvcreate` |
| 4 | Online PVC expansion → VDO pool + logical volume grow, filesystem resizes, data intact | **Tested** (prior session, PR #402) | 6Gi → 10Gi, `GrowVDO` ran before filesystem resize |
| 5 | PVC clone / VolumeSnapshot restore scheduled onto the same node as a still-live source | **Tested** (prior session, PR #402) | `ResolveClonedVDO` → `vgimportclone`+`lvrename`; 3 independent VG identities coexisted, no cross-contamination |
| 6 | Multiple VDO instances on one node survive a node reboot | **Tested** (prior session, PR #402) | 2 PVCs, distinct checksums, real reboot, `kvdo` usage count exactly 2 post-reboot, both checksums matched |
| 7 | XFS filesystem on top of a VDO device | **Tested** (prior session, PR #402) | `mkfs.xfs` ran with no stripe-alignment flags (confirms VDO-aware skip), `nouuid` intact, data round-tripped |
| 8 | `clientCompression=true` alone (dedup off) and `clientDeduplication=true` alone (compression off) as independently-toggled pools | **Tested — found a real bug** (this session) | Both pools created correctly (`client_compression`/`client_deduplication` params correct on each StorageClass), but **both volumes failed to stage** — see Error #9 below. The independent-flag `lvcreate` args themselves were never reached; the failure is upstream, in device resolution, not specific to either flag |
| 9 | Pod naturally reschedules (not just delete+recreate on the same node) onto a **different** vdo-capable node, and `PersistentVolume.spec.nodeAffinity` correctly restricts it to vdo-capable nodes only | **Not tested** | The `vdoCapableSegment`/nodeAffinity fix (this session, commit `4ac2ff0`) is unit-tested only; never exercised against a real multi-vdo-capable-node reschedule |
| 10 | Non-vdo-capable node correctly excluded from scheduling a VDO-requesting PVC | **Tested** (this session) | `vm01` (control-plane, `vdo-capable=false`) never selected; pod landed on `vm15` (vdo-capable, non-storage) per `allowedTopologies` |

## Error Cases (negative)

| # | Scenario | Status | Evidence |
|---|---|---|---|
| 1 | Storage-side disconnects while the node/pod stay up (stale VDO/LVM state after unclean unstage) | **Tested** (prior session, PR #402) | Forced NVMe-oF disconnect; found + fixed 2 real bugs (`DeactivateVDO` needed a `dmsetup remove` fallback; dash-escaping in device-name matching); confirmed fully automatic cleanup on repeat |
| 2 | Node genuinely crashes (not a graceful reboot) mid-write under `vdo_write_policy=async` | **Tested** (prior session, PR #402) | Real `sysrq` crash (verified via new boot timestamp); `fsync()`'d file survived, non-`fsync()`'d file lost — correct POSIX outcome |
| 3 | Node dies / pod is killed **during** initial VDO creation (mid-`lvcreate`), leaving a partially-created PV/VG/pool | **Not tested** | Every prior test let `CreateOrAttachVDO` complete; an interrupted first-time creation (as opposed to the already-covered "backing device disappears after creation" case) is a different code path and untested |
| 4 | Underlying physical pool device fills up (VDO ENOSPC / write failure when the pool has no free physical blocks left) | **Not tested** | Not in the design doc's original scope either — flagging as a genuine gap |
| 9 | **NEW BUG FOUND** — `NodeStageVolume` fails when LVM sees the volume's two redundant NVMe-oF (HA) paths as separate local block devices with an identical on-disk PV signature | **Confirmed, not fixed** (this session) | Two independently-created pools (Error #8's compression-only and dedup-only volumes) both failed `mkfs.xfs` with `No such file or directory` on the VDO device path. Root cause found in `csi-driver` CSI-node logs: `pvscan[...] PV /dev/nvme2n1 ... is duplicate for PVID <x> on /dev/nvme3n1`. Each volume's HA connection creates *two* separate local device nodes (not kernel-native ANA multipath merged into one), both exposing byte-identical backend data. `pvscan --cache <by-id-path>` does a broader-than-scoped scan, hits this duplicate PVID, and `vgs`/`vgchange -ay` then behave non-deterministically depending on which of the two device nodes LVM's cache happens to resolve first — sometimes correct (the original `vdo-pool` test happened to work), sometimes not (both of these new pools failed). This likely also explains the earlier `vm03` "Bad address" (`EFAULT`) failures earlier this session, previously (and only partially correctly) attributed to stale on-disk partition data from a prior tenant. **Not fixed in this session** — a real fix needs LVM commands scoped to exactly one device (e.g. `--devices`/`--devicesfile`, or an explicit filter excluding the standby HA path) across `CreateOrAttachVDO`/`ResolveClonedVDO`/`GrowVDO`/`RemoveVDO`, which is a real design/implementation task, not a quick patch |
| 5 | Node's VDO capability regresses *after* a VDO-backed PVC is already bound and running there (e.g. `kvdo` becomes unloadable) | **Not tested** | Design doc lists this as a required "hard-fail loudly, don't silently mount raw" behavior; never actually broken a running node's kvdo module to check |
| 6 | Genuinely concurrent (overlapping, not just closely-timed) `vgchange`/`pvscan` calls racing at the LVM level | **Not tested** | The multi-instance-reboot test's two `NodeStageVolume` sequences happened to run sequentially, not truly overlapping — flagged as an explicit caveat in PR #402 |
| 7 | Backend NVMe-oF device disappears *permanently* (not transient) — confirm no orphaned dm-vdo/LVM state is left after the pod/PVC is deleted | **Tested** (prior session, PR #402) | Same test as Error #1 — deletion afterward confirmed fully automatic, no manual cleanup needed |
| 8 | This session's own cluster-bootstrap failures (FDB sidecar permission bug, stale control-plane image tag, forced `redundancyMode=triple` misconfig, stale NVMe partition data on `vm03`/`vm15` from prior tenants, memory-exhaustion from FDB process co-location) | **N/A to VDO itself** | All infra/environment issues unrelated to the VDO code path; documented and worked around live during this session, not filed as VDO bugs |

## Load Cases

| # | Scenario | Status | Evidence |
|---|---|---|---|
| 1 | Many VDO-backed volumes on a single node (dedup-engine index/memory scaling at realistic fleet density) | **Not tested** | Only 2-instance parallel scaling has ever been measured (PR #402); no stress test beyond that |
| 2 | 10-30+ VDO-backed PVCs provisioned concurrently (mirrors the Backup feature's parallel-backup load case) | **Not tested** | Not attempted in any session |
| 3 | Sustained high-throughput writes to a VDO volume — measure real CPU/memory overhead of inline compression+dedup under load | **Not tested** | Only small (~104MB), non-sustained writes have been used to demonstrate savings |
| 4 | Large volume (10s of GB) with a realistic mix of compressible/duplicate/random data at scale | **Not tested** | All prior data-savings verification used small (~100MB), synthetic, highly-duplicate data |

---

## What was tested live this session

Given the cluster (`config-israel`, branch `issue-277-client-side-compression-impl`) is
freshly bootstrapped and active, prioritized the cheapest, highest-value **Not tested**
gaps above rather than re-proving already-covered ground:

1. Good #2 (re-run) — compression/dedup savings via `vdostats` on this cluster's volume:
   **99% space saving** on ~96MB of highly duplicate/compressible data (1 original + 2 exact
   copies).
2. Good #8 — independent `clientCompression`-only and `clientDeduplication`-only pools:
   **surfaced Error #9**, a real, previously-unknown bug (LVM duplicate-PV ambiguity across
   a volume's two HA paths). Both test volumes failed to stage.
3. Good #9 (pod reschedule onto a different vdo-capable node, confirm `nodeAffinity` holds)
   — **not reached**; deprioritized once Error #9 surfaced, since it's a more urgent,
   concrete correctness bug worth surfacing immediately rather than continuing down the
   scenario list.

Error #3, #4, #5, #6 and all Load cases are deliberately deferred — they either require
destructive/long-running setups (filling a real pool to capacity, breaking a live node's
kernel module, sustained load generation) disproportionate to what's left of this session,
or genuine multi-agent/multi-hour infrastructure I don't have on hand right now.
