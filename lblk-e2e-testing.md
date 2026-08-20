# lblk (non-NVMe block device) e2e test tracking

Legend: ✅ done/passed · ⚠️ done — gap/bug found · 🟡 partially covered · ⬜ not tested yet · ⏭️ excluded (per instruction)

Tested on: `config-israel` (real local NVMe disks used as lblk block devices) and GCP
`manohar-lblk-atomicity` (genuine non-atomic `pd-balanced` Persistent Disks).

## Good Cases

- ✅ Cluster create with `device_mode=lblk` — created repeatedly on both clusters; nvme
  mode untouched by this work (no regression risk introduced)
- Device selection modes:
  - ✅ no selector (auto-select all eligible whole disks) — used on both clusters
  - ⬜ `--blk-names`
  - ⬜ `--blk-names-exclude`
  - ⬜ `--blk-serials`
- ⬜ Partition-based selection (pre-partitioned disk) + journal auto-split via `--jm-percent`
- Device count:
  - ✅ exactly the minimum (2) — both clusters ran with 2 devices/node
  - ✅ 3+ devices with uneven sizes (2x100GB + 1x30GB) on the added GCP node — the
    30GB (smallest) disk correctly auto-became the JM device, confirmed via
    `sn list-devices` before the node was removed again (see cluster-expansion gap
    below — this device-topology behavior itself worked correctly; the cluster-level
    orchestration around adding the node is what's broken)
- HA topology: ✅ multi-node HA (both clusters)
- ✅ Full CSI E2E lifecycle (create/snapshot/clone/resize/delete) — 31/32 passed on both
  `config-israel` and GCP runs (1 skip = SPDKCSI-MULTICLUSTER, needs multi-cluster config,
  not applicable to a single-cluster setup)
- ✅ Planned node restart — device re-identified correctly, cluster stayed healthy
  (`config-israel`: DaemonSet pod delete; GCP: 10x abrupt SPDK force-kill while `fio`
  wrote continuously — node auto-recovered every time, lvol/filesystem stayed intact)
- ⚠️ Cluster expansion (add a new lblk node to an already-active cluster) —
  **real, significant gap found** on GCP: adding exactly **one** new node to an
  already-active 3-node cluster leaves the cluster permanently stuck. Root cause,
  confirmed by reading `sbcli`'s `cluster_expand()`: it explicitly requires a
  **minimum of 2 new nodes** at once (`"A minimum of 2 new nodes are required to
  expand cluster"`) — new nodes get paired with *each other* as HA secondaries, not
  with already-paired existing nodes, so a lone new node has no valid secondary
  candidate. Separately, **the operator never calls `cluster complete-expand`** after
  its automatic add-node flow brings a new node online — there's no code path in the
  operator repo invoking it at all, so even a *supported* (2+ node) expansion would
  need a manual step today. With one new node: the new node itself comes up healthy
  at the device/SPDK level, but the overall cluster gets stuck oscillating between
  `IN_ACTIVATION` and `SUSPENDED` forever (auto-retry loop, never self-heals) since
  `cluster_activate()`'s generic re-pairing logic hits the exact same "no secondary
  available" wall. Recovery required manually running `sn remove` + `sn delete` on
  the new node, then `cluster activate` — not something the operator or sbcli surfaces
  as guidance; a user hitting this cold would see a cluster stuck offline with no clear
  next step. Confirmed unaffected: the pre-existing lvol (from the earlier
  crash-consistency test) survived fully intact throughout. Not tested: whether a
  2-new-nodes-at-once expansion actually works cleanly (didn't spend the extra
  infra-provisioning time to confirm the supported path, given the unsupported path was
  already conclusively and reproducibly broken twice).
- ⬜ `--force`/`--force-format` reuse of a previously-partitioned disk — the
  `BlkForceFormat` field/wiring is merged (PR #437) but never exercised end-to-end
- 🟡 `/blockdevices` inventory accuracy — eligibility flags observed correctly in
  `node_configure.py` output logs on every run, but no dedicated systematic check across
  all eligibility-rule branches (mounted, held, root-disk, read-only, etc.)

## Error Cases

- 🟡 Missing/typo'd device name or serial → clean rejection — attempted on GCP, blocked:
  `node_configure.py` refuses to regenerate config once the node's own DaemonSet pod is
  `Running` (`_is_pod_present_for_node()` checks for a `snode-spdk-pod-*` on the same
  k8s node), and the operator races to add-node within seconds of the pod becoming ready
  — the window to test manually via `kubectl exec` is too narrow to reliably hit. Would
  need a standalone test harness (e.g. running `node_configure.py` in a plain `docker run`
  outside the k8s pod-management flow) to properly isolate.
- 🟡 Requested device busy (mounted / backs root fs) → rejected with reason — same blocker
- 🟡 Fewer than minimum devices selected → rejected — same blocker
- ✅ `--lblk` combined with any NVMe selection flag → was a crash (CrashLoopBackOff)
  instead of a clean rejection; **fixed** at the CRD level (see the Operator/K8s-specific
  entry below) — now rejected at admission time before any pod is ever created
- ✅ Duplicate serials among selected devices → clean rejection (hit this ourselves via
  the donor-cluster NVMe-oF workaround; confirmed working as intended)
- ⬜ Disk and one of its own partitions both selected → rejected
- ⬜ Partitioned disk selected without `--force`/`--force-format` → rejected
- ⬜ Device disappears after node is online (unplugged / NVMe-oF path drop) →
  `device_remove` fires, node degrades gracefully
- ⬜ Hung IO on an AIO device (stalled backing store) → watchdog trips, node auto-restarts
- ⬜ SPDK process crash/zombie mid add-node → cleanup + retry succeeds, no hugepage squat
- ⬜ add-node retry leaves JM-mesh holes → fresh activation blocks, recovery still proceeds
- ⬜ Journal device (or journal partition) fails → node/cluster behavior on JM loss
- ⬜ Kernel renames a device across reboot (sdb→sdc) → serial-based resolution still finds it
- ⬜ Adding an NVMe-selected node to an lblk-mode cluster, or vice versa → rejected
- ✅ Operator: editing `blkNames` on an already-provisioned StorageNodeSet/node →
  **fixed and verified**: `BlkNames`/`BlkNamesExclude`/`BlkSerials` now carry a CEL
  immutability rule on both `StorageNodeSetSpec` and `StorageNodeOverrides`. Root-caused
  a real subtlety along the way: a plain `self == oldSelf` rule (matching the existing
  `EnableLblk` pattern) does *not* catch the unset→set transition — Kubernetes skips CEL
  validation on optional fields when the old value was absent, by design — so the
  original bug report's exact scenario (a fleet created with no selector, then edited to
  add one) slipped through untested. Fixed using `optionalOldSelf: true` +
  `oldSelf.hasValue() && self == oldSelf.value()`. Empirically verified all 3 transitions
  on the live GCP cluster: unset→set rejected, set→different-value rejected, set→same-
  value (no-op) allowed.
- ⬜ Selected device smaller than the journal floor (2 GiB) → clear rejection


## Compatibility / Environment Cases

- Different backing media:
  - ✅ real local NVMe disk, used as a plain lblk block device (`config-israel`)
  - ✅ cloud network-attached block volume, GCP `pd-balanced` (covers the
    "cloud EBS-like volume" case — genuinely non-atomic-write backing media)
  - 🟡 NVMe-oF volume — attempted via a donor-cluster workaround, abandoned after hitting
    the duplicate-serial-number blocker (architectural, see findings doc)
  - ⏭️ local SATA/SAS SSD, virtio-blk, iSCSI LUN — excluded per instruction
- ⬜ GPT vs MBR partition tables — partitions were prepped on `config-israel` vm15's
  `nvme3n1` for this earlier but never actually exercised

## Operator/K8s-specific Cases

- 🟡 `StorageCluster.spec.deviceMode` immutability — CRD schema carries the
  `x-kubernetes-validations: self == oldSelf` enforcement rule (confirmed present), but
  never empirically tried a mutation against a live cluster to see it rejected
- 🟡 Fleet-default + per-node-override merge correctness (blkNames/blkSerials/enableLblk) —
  covered by Go unit tests (`storagenode_controller_unit_test.go`), not exercised live
- ✅ Mutual-exclusivity (lblk selectors vs PCIe selectors) at CR-admission time →
  **fixed and verified**: added an object-level CEL rule to both `StorageNodeSetSpec`
  and `StorageNodeOverrides` (not a webhook — a cross-field CEL rule is simpler here and
  runs on both create and update automatically, no separate wiring needed) rejecting any
  spec where an lblk selector (`enableLblk`/`blkNames`/`blkNamesExclude`/`blkSerials`) and
  an NVMe PCIe selector (`pcieAllowList`/`pcieDenyList`/`pcieModel`/`driveSizeRange`/
  `deviceNames`) are both set. Empirically verified on the live GCP cluster: the exact
  combo that used to crashloop the pod is now rejected at admission time with a clear
  message, while lblk-only and pcie-only configs still create successfully.

## Extra validation beyond this checklist

Driven by direct feedback from Michael (not originally in this file) — the actual
motivating question was write-atomicity/crash-consistency on non-atomic block storage,
not just "is it literally NVMe":

- ✅ Low-level raw-disk 4K write-atomicity probe on GCP `pd-balanced` (O_DIRECT, generation
  counter, `sysrq b` hard crash mid-write) — 6/6 trials clean, no torn write detected at
  the raw device level in this sample size
- ✅ LVS-metadata crash-consistency test: `fio` (no `--verify`, per Michael's explicit
  guidance — data-block torn writes are expected/out of scope) running continuously
  against a real PVC/lvol on the GCP lblk cluster, SPDK pod abruptly force-killed
  (`--grace-period=0 --force`) mid-write **10 times** — lvol/filesystem survived
  structurally intact every time (clean remount + full-file readback after the run)
- ⬜ Same test with an actual VM-level power-off (`sysrq b`) instead of container-kill —
  the other trigger scenario Michael named, not yet tested
- ⬜ Same test killing the HA secondary/replica instead of the primary
- ⬜ Repeated/overlapping kills (crash again before previous recovery completes)
- ⬜ Kill during a metadata-heavy operation specifically (lvol create/delete, snapshot
  create) rather than steady-state writes

## Bugs/gaps found along the way (not on the original checklist)

- **Real gap, not yet fixed**: the operator has no code path that calls `sbcli cluster
  complete-expand` after a node-add completes. Combined with `sbcli`'s own
  `cluster_expand()` requiring 2+ new nodes at once, adding storage nodes to an
  already-active cluster one at a time (which is what the operator's `StorageNodeSet`
  reconcile naturally does — nodes get added to `workerNodes` and reconciled
  individually) is fundamentally incompatible with how cluster expansion actually
  works on the backend today. See the Good Cases "Cluster expansion" entry above for
  the full repro and recovery steps.

- **Fixed**: operator `StorageNode` reconcile order bug — `Reconcile` fetched the parent
  `StorageNodeSet` before checking `DeletionTimestamp`, so a node whose parent was already
  deleted got stuck forever ("parent StorageNodeSet not found, requeuing"). Fixed in
  commit `d308677`, regression test added, pushed to PR #437.
- **Documented** in `lblk-testing-findings.md`: sbcli hugepage additive-stacking bug
  (reproduced on both `config-israel` and GCP — a generic, portable defect, not
  environment-specific), a Helm CRD-upgrade gotcha (`helm upgrade` never touches
  already-installed CRDs — must `kubectl apply -f crds/` directly), the QEMU NVMe
  emulation defect, the NVMe-oF serial-uniqueness architectural note, and test-hygiene
  notes.
- **Fixed**: `simplyblock-storage-node-sa`'s projected token used Kubernetes' default
  ~1hr expiration, so any already-running pod that predates a ServiceAccount
  recreation (exact trigger not conclusively root-caused — ruled out cascading GC,
  operator-pod restart, and `helm-sync`; most likely an informer-cache resync race
  from a CRD schema update, but unconfirmed) stayed stuck with 401s for up to ~48min
  before kubelet's next refresh, in practice requiring manual pod deletion to recover.
  Reduced to 600s (10min) in commit `83b2ece` so this class of failure self-heals in
  minutes regardless of root cause. Independent of whatever causes the SA recreation
  itself, which remains an open question.
