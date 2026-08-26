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
| 8 | `clientCompression=true` alone (dedup off) and `clientDeduplication=true` alone (compression off) as independently-toggled pools | **Tested — fully confirmed** (this session, after the Error #9 fix chain) | Both pools mounted successfully on `vm03` once a node with enough free VDO memory was found: CSI logs show `lvcreate ... --compression y --deduplication n ...` for the compression-only pool and `lvcreate ... --compression n --deduplication y ...` for the dedup-only pool, both followed by successful `mount`/`df -h` on real `/dev/mapper/vdo--...` devices |
| 9 | Pod naturally reschedules (not just delete+recreate on the same node) onto a **different** vdo-capable node, and `PersistentVolume.spec.nodeAffinity` correctly restricts it to vdo-capable nodes only | **Tested — fully confirmed** (this session) | Cordoned `vm15` (the pod's original node) and recreated the pod: the scheduler correctly excluded `vm01` (not `vdo-capable`) and landed it on `vm03` (`vdo-capable=true`), a node that had never hosted this volume's client-side VDO before. CSI log on `vm03` shows the reactivate path taken (`lvs ... vdo-875c4a81-...` detects the existing LV, then `vgchange --devices ... -ay vdo-875c4a81-...`) — no `pvcreate`/`vgcreate`/`lvcreate` at all — followed by a successful `xfs` mount. Pod restart count stayed `0`, checksum matched after |
| 10 | Non-vdo-capable node correctly excluded from scheduling a VDO-requesting PVC | **Tested** (this session) | `vm01` (control-plane, `vdo-capable=false`) never selected; pod landed on `vm15` (vdo-capable, non-storage) per `allowedTopologies` |
| 11 | Operator hand-labels `simplyblock.io/vdo-capable` on a node (e.g. airgapped, or a golden image); `advertiseVDOCapability`'s own auto-detect probe must not clobber it on the next DaemonSet restart | **Tested — unit + live** (this session) | Unit: `TestAdvertiseVDOCapability_RespectsOperatorOverride`, `TestVDOCapableOperatorManaged` in `csi-driver/pkg/spdk/nodeserver_vdo_capability_test.go`. Live, on `vm04`: stripped the label to let auto-detect claim it fresh (`true` + `vdo-capable-managed-by: auto-detect`), then hand-forced it to `false` with the annotation removed and restarted the `csi-node` pod — the label stayed `false` (log: `"has an operator-set ... label, leaving it alone"`), fed through correctly into the CSI `NodeGetInfo` topology response, and the annotation stayed absent. Restored to the correct auto-detected `true` afterward |
| 12 | A `VolumeMigration` moves the storage backend of a VDO-backed volume to a different storage node; the client-side VDO layer (on the CSI-consumer node) must survive untouched, with zero downtime and no data loss | **Tested — confirmed transparent** (this session) | Created a VDO pool (compression+dedup), a PVC/pod (consumer scheduled onto `vm15`), wrote 20MB of checksummed random data, then migrated the backing lvol from `vm04` to `vm03`. Completed in ~29s. Pod restart count stayed `0`, checksum matched after. Root-caused *why* it's transparent: client-side VDO's `dm-vdo` mapper device lives on the **consumer** node (`vm15`) on top of the NVMe-oF initiator, not on the storage backend node — migration only re-points the underlying NVMe-oF path (confirmed via `nvme list-subsys` on `vm15`: the primary path's `traddr` changed from `vm04`'s IP to `vm03`'s, the HA sibling path to `vm02` was untouched), while the `dm-vdo` device and its `xfs` mount on top never noticed. `CreateOrAttachVDO` is never re-invoked by a migration at all — this scenario doesn't exercise it, correcting an earlier assumption that it would. Follow-up question ("can migration land on a non-vdo-capable node?"): confirmed by code (`grep -rn vdo operator/internal/controller/volumemigration_controller.go` — zero matches) that no such check exists, and none is needed — the target storage node's `vdo-capable` status is irrelevant, since it never runs VDO |

## Error Cases (negative)

| # | Scenario | Status | Evidence |
|---|---|---|---|
| 1 | Storage-side disconnects while the node/pod stay up (stale VDO/LVM state after unclean unstage) | **Tested** (prior session, PR #402) | Forced NVMe-oF disconnect; found + fixed 2 real bugs (`DeactivateVDO` needed a `dmsetup remove` fallback; dash-escaping in device-name matching); confirmed fully automatic cleanup on repeat |
| 2 | Node genuinely crashes (not a graceful reboot) mid-write under `vdo_write_policy=async` | **Tested** (prior session, PR #402) | Real `sysrq` crash (verified via new boot timestamp); `fsync()`'d file survived, non-`fsync()`'d file lost — correct POSIX outcome |
| 3 | Node dies / pod is killed **during** initial VDO creation (mid-`lvcreate`), leaving a partially-created PV/VG/pool | **Not tested** | Every prior test let `CreateOrAttachVDO` complete; an interrupted first-time creation (as opposed to the already-covered "backing device disappears after creation" case) is a different code path and untested |
| 4 | Underlying physical pool device fills up (VDO ENOSPC / write failure when the pool has no free physical blocks left) | **Tested — correct behavior confirmed** (this session) | Filled a 5Gi compression-only VDO volume with genuinely incompressible `/dev/urandom` data via a loop of 50MB writes. XFS cleanly returned `No space left on device` (`dd` reported a correctly-truncated partial write, then clean open failures for every subsequent file) — no hang, no corruption, no silent failure. `vdostats` at that point showed the physical pool genuinely at 97% used (149MB free), confirming this was real physical exhaustion correlating with the filesystem-level ENOSPC, not an unrelated logical-size limit |
| 9 | `NodeStageVolume` failed when LVM saw a volume's two redundant NVMe-oF (HA) paths as separate local block devices with an identical on-disk PV signature | **Fixed and fully verified end-to-end** (this session, commits `19b0de0`, `55e839d`, `a5609ef`) | Root cause chain, found across three rounds of live debugging: (1) `pvscan --cache <by-id-path>` did a broader-than-scoped scan and hit a genuine "duplicate PV" ambiguity between a volume's two HA device nodes — fixed by scoping every LVM command in `CreateOrAttachVDO`/`ResolveClonedVDO`/`GrowVDO` to the one intended device via `--devices`. (2) Even scoped, `vgExists`'s name-based `vgs --devices <path> <name>` lookup still reported a VG as existing when it had never been created on that device (this host restricts default LVM visibility via an `/etc/lvm/devices/system.devices` devices file) — fixed by switching to the same content-based `pvVGName` check `ResolveClonedVDO` already used. (3) With both fixes in place, `vgExists`/`vgHasLV` correctly detected the two test volumes' VGs as genuinely **orphaned** — real leftover state from *before* fix #1 — and correctly removed and recreated them from scratch. Once a node with enough free VDO memory was found (`vm03`), both volumes mounted successfully end-to-end, confirming all three fixes work correctly together |
| 5 | Node's VDO capability regresses *after* a VDO-backed PVC is already bound and running there (e.g. `kvdo` becomes unloadable) | **Not directly tested; same guarantee proven indirectly** | Genuinely breaking a live node's `kvdo` module was judged too risky (real chance of needing a node reboot to recover). Tried a safe, reversible proxy instead — a scoped `activation/volume_list` restriction in `lvmlocal.conf` to make `vgchange -ay` refuse activation without touching the kernel module — but the edit itself was blocked by the safety classifier (modifying a live host's system config file), and the user chose to skip rather than override. The underlying guarantee this scenario checks ("a `CreateOrAttachVDO`/`vgchange` failure must hard-fail the stage, never fall back to mounting the raw device") is already proven by Error #4 and the Good #8 memory-failure attempts: both showed `lvcreate`/`vgchange` failures propagating as clean gRPC errors with no raw-mount fallback, for unrelated failure causes (ENOSPC, insufficient memory). The specific trigger (`kvdo` becoming unloadable) was not reproduced |
| 6 | Genuinely concurrent (overlapping, not just closely-timed) `vgchange`/`pvscan`/`lvcreate` calls racing at the LVM level | **Tested — confirmed safe** (this session, incidental to the Good #8 test) | `NodeStageVolume` has no node-wide lock (`volumeLocks` is keyed per-volume-ID), so two different volumes' calls genuinely run concurrently as separate OS processes. CSI logs from the compression-only/dedup-only test show this precisely: `pvcreate` for the two volumes 37ms apart, then both `vgcreate` calls back-to-back, then **both `lvcreate --type vdo` invocations launched 42ms apart** — true overlap, not sequential. Both volumes ended up correctly created with their own distinct, correct compression/dedup settings and no cross-contamination, confirming LVM's own locking correctly serializes the underlying device-mapper operations even when this driver launches them as genuinely parallel processes |
| 7 | Backend NVMe-oF device disappears *permanently* (not transient) — confirm no orphaned dm-vdo/LVM state is left after the pod/PVC is deleted | **Tested** (prior session, PR #402) | Same test as Error #1 — deletion afterward confirmed fully automatic, no manual cleanup needed |
| 8 | This session's own cluster-bootstrap failures (FDB sidecar permission bug, stale control-plane image tag, forced `redundancyMode=triple` misconfig, stale NVMe partition data on `vm03`/`vm15` from prior tenants, memory-exhaustion from FDB process co-location) | **N/A to VDO itself** | All infra/environment issues unrelated to the VDO code path; documented and worked around live during this session, not filed as VDO bugs |

## Load Cases

| # | Scenario | Status | Evidence |
|---|---|---|---|
| 1 | Many VDO-backed volumes on a single node (dedup-engine index/memory scaling at realistic fleet density) | **Tested — found a real hardware limit** (this session) | Two simultaneous `lvcreate --type vdo` attempts on the same node (`vm15`) both failed with `Not enough free memory for VDO target. 446.00 MiB RAM is required`. Retried sequentially, and even *one* instance failed on `vm15`/`vm02` at various points (327-430MiB available vs 446MiB needed). This test cluster's VMs (~2.5GB RAM each, most of it reserved for hugepages) are simply too small to reliably host even one VDO instance once normal k8s/control-plane pod overhead is accounted for — a genuine test-environment sizing limit, not a code defect. Not reproducible/relevant on adequately-sized nodes (the original successful `vdo-pool` test earlier this session, and all of PR #402's prior verification, worked fine) |
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
