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
- ✅ `--force`/`--force-format` reuse of a previously-partitioned disk — exercised at
  the `node_configure.py` level: a partitioned disk (`sdd`) selected with `--force`
  succeeds cleanly and writes config, vs. clean rejection without `--force` (see Error
  Cases). The `BlkForceFormat` CRD field/wiring (PR #437) still hasn't been driven
  through the full K8s CR → operator → add-node path end-to-end, only the underlying
  CLI behavior it wires up to.
- 🟡 `/blockdevices` inventory accuracy — eligibility flags observed correctly in
  `node_configure.py` output logs on every run, but no dedicated systematic check across
  all eligibility-rule branches (mounted, held, root-disk, read-only, etc.)

## Error Cases

- ✅ Missing/typo'd device name or serial → clean rejection — **resolved & confirmed**.
  The earlier "blocked" note was solvable: reused the removed 4th GCP node (labeled
  for DaemonSet scheduling but deliberately kept out of `workerNodes`, so the operator
  never attempts add-node and the `_is_pod_present_for_node()` guard never trips) to
  get an indefinite, stable window for manual `kubectl exec` testing.
  `{'sdz': 'not present'}` — clean, no partial state.
- ✅ Requested device busy (mounted / backs root fs) → `{'sda': 'mounted (busy)'}` —
  clean rejection with reason.
- ✅ Fewer than minimum devices selected → `"lblk mode requires at least 2 partitions
  or SSDs per node; only 1 eligible unit(s) selected: ['sdb']"` — clean rejection.
- ✅ `--lblk` combined with any NVMe selection flag → was a crash (CrashLoopBackOff)
  instead of a clean rejection; **fixed** at the CRD level (see the Operator/K8s-specific
  entry below) — now rejected at admission time before any pod is ever created
- ✅ Duplicate serials among selected devices → clean rejection (hit this ourselves via
  the donor-cluster NVMe-oF workaround; confirmed working as intended)
- ✅ Disk and one of its own partitions both selected → distinct, clear rejection:
  `"selection contains partition(s) ['sdd1'] of a disk that is itself selected; select
  either the whole disk or its partitions"` (only surfaces once `--force` gets past the
  more basic "disk is partitioned" check first — see next item)
- ✅ Partitioned disk selected without `--force`/`--force-format` → `{'sdd': 'partitioned
  (pass --force to format at add-node)'}` — clean rejection; **and** confirmed the
  Good-Case flip side: the same disk selected *with* `--force` succeeds cleanly
  (config written) — validates the previously-untested `--force-format` reuse Good Case
  too.
- ✅ Device disappears after node is online (unplugged / NVMe-oF path drop) →
  live `gcloud compute instances detach-disk` on a GCP lblk storage node's data disk
  while online. Confirmed graceful: device flipped `Status: removed`, node stayed
  `online` (no crash/restart) with `Health: False` and `Dev: 1/0` — correctly reflects
  the degraded state without any cascading cluster-level failure. `device_remove`
  fired as expected.
- ⚠️ Kernel renames a device across reboot (sdb→sdc) → **gap found, see below** —
  discovered as a side effect of the device-disappears test's recovery step
- ⬜ Hung IO on an AIO device (stalled backing store) → watchdog trips, node auto-restarts
- ⬜ SPDK process crash/zombie mid add-node → cleanup + retry succeeds, no hugepage squat
- ⬜ add-node retry leaves JM-mesh holes → fresh activation blocks, recovery still proceeds
- ⚠️ Journal device (or journal partition) fails → **gap found, see below** — unlike a
  storage-device failure, a JM-device failure is NOT proactively detected
- 🟡 Adding an NVMe-selected node to an lblk-mode cluster, or vice versa → rejected —
  attempted on GCP, not meaningfully testable in this environment: GCP `pd-balanced`
  disks aren't real NVMe PCIe hardware at all, so NVMe-mode `node_configure.py` (no
  `--lblk`) just fails at "no NVMe devices with class 0108 found" — a client-side
  hardware-absence error, not the cluster-level device-mode mismatch check we actually
  want to exercise. Indirect confirmation only: we organically hit the reverse case
  earlier this session (a genuinely lblk-configured node against a cluster still in
  `nvme` mode) and got the expected clean rejection — `"The node config carries
  'lblk_devices' but this cluster runs in nvme device mode; re-run 'sn configure'
  without --lblk or create the cluster with --device-mode lblk."` The check is
  symmetric in its wording, so this is reasonably strong (if indirect) evidence the
  reverse direction works too, but not a direct repro. Would need a machine with real
  NVMe hardware (e.g. `config-israel`) to test directly.
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
- ✅ Selected device smaller than the journal floor (2 GiB) → precise, clear rejection
  with actual byte-size math: `"journal needs 3253406269 bytes (3% of 108446875648, min
  2147483648) but the smallest selected partition sdc1 (1072693248 bytes) may
  contribute at most 536346624; provide a larger partition"`


## Compatibility / Environment Cases

- Different backing media:
  - ✅ real local NVMe disk, used as a plain lblk block device (`config-israel`)
  - ✅ cloud network-attached block volume, GCP `pd-balanced` (covers the
    "cloud EBS-like volume" case — genuinely non-atomic-write backing media)
  - 🟡 NVMe-oF volume — attempted via a donor-cluster workaround, abandoned after hitting
    the duplicate-serial-number blocker (architectural, see findings doc)
  - ⏭️ local SATA/SAS SSD, virtio-blk, iSCSI LUN — excluded per instruction
- ⚠️ GPT vs MBR partition tables — exercised on `config-israel` vm15's `nvme3n1`
  (spare 48.5GB QEMU NVMe test disk, isolated from the cluster's real 580GB storage
  NVMes). **Two real gaps found, see below**: (1) partition-table-type detection
  silently breaks in any container that doesn't bind-mount `/run/udev` — which
  includes the actual production `snode-spdk-pod`/DaemonSet, confirmed by inspecting
  its real volume mounts; (2) once that's worked around, the GPT journal-split path
  has a sector-size unit bug that corrupts partition boundaries on any 4Kn
  (4096-byte-native-sector) disk. MBR itself is correctly and cleanly rejected once
  detection works. Disk fully restored to its original two-partition GPT layout
  (same PARTUUIDs/labels) afterward.

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

- **Real gap, not yet fixed, most severe finding of this round**: the migration task
  scheduler has a genuine **circular-wait deadlock** between the `device_migration`
  (outage recovery) and `new_device_migration` (expansion/role-rebalance) task
  families when both target the same node+distrib. Reproduced twice independently:
  - `tasks_runner_migration.py` (~line 285) treats a `SUSPENDED` `new_device_migration`
    task on the same node+`distr_name` as a reason to defer a `device_migration` task
    ("task found on same node, retry").
  - `tasks_runner_new_dev_migration.py` (~line 52) treats *any* non-`DONE`
    `device_migration`/`failed_device_migration` task anywhere in the cluster as a
    reason to defer a `new_device_migration` task ("deferring: recovery migration
    ... is open") — by design, since recovery is meant to have priority over
    expansion.
  - Put together: task A (`device_migration`) won't run because it sees task B
    (`new_device_migration`, `suspended`) on the same node/distrib; task B won't run
    because task A exists at all. Neither side has a tie-breaker, so once both
    conditions are true simultaneously, **neither task can ever proceed** — confirmed
    by direct DB inspection (`e5bef9b1-...`, `function: device_migration, status: new,
    retry: 0`, completely un-retried for the ~50 minutes it sat blocked, while
    `9e59dc34-...`/`new_device_migration` on the identical node+`distrib_5` sat
    `suspended` deferring right back to it by UUID). This left the whole cluster
    stuck `ACTIVE - REBALANCING` with all 3 nodes `Health: False` for 2+ hours with
    zero progress, and would have stayed that way indefinitely without manual
    intervention. Recurred a second time immediately afterward (`b83c6418-...` on a
    different node/distrib) as soon as another recovery migration was queued,
    confirming this isn't a one-off — it's a structural gap that will hit *any*
    device-recovery event that happens to race with an in-flight expansion/rebalance.
  - Workaround applied both times: `sbcli-dev cluster cancel-task <new_device_migration
    task id>` on the specific task(s) sharing the node+distrib with the stuck
    `device_migration` task, which immediately unblocked it (it ran to completion
    within seconds of the conflicting task being canceled).
  - Fix direction: the `device_migration` side's same-node-distrib check should not
    treat a merely-`SUSPENDED` (i.e., not actually running) `new_device_migration`
    task as a blocker — only a genuinely `RUNNING` one should count as "active", matching
    what `new_device_migration`'s own `get_active_node_mig_task` already does
    correctly (it only counts `STATUS_RUNNING`, not `NEW`/`SUSPENDED`). Flagging for
    the backend (`sbcli`) team — this is core task-scheduler logic, not something the
    operator controls.
  - Side-effect noted along the way: resolving this triggered the system's own
    coordinated sequential node-restart mechanism (nodes visibly went `down`→
    restarted one at a time, never in parallel), which completed cleanly and is
    consistent with the intended, correct HA behavior (see
    `feedback_sequential_node_restart` memory) — no new issue there.

- **Not a product bug, own testing oversight, noted for completeness**: node-2's JM
  device (`5909eaa1-...`) was left stuck `Status: removed` for roughly two hours after
  the earlier "Device disappears" test, because that test's storage-device and JM
  device were both carved from the *same* physical GCP PD (`...-storage-node-2-pd-1`)
  — detaching it took out both devices at once, but only the storage device
  (`4a92b3aa-...`) was recovered via `restart-device` at the time; the JM device was
  missed. JM devices aren't in `snode.nvme_devices`, so `restart-device` correctly
  reports `"device not found"` for one — the right command is `storage-node
  restart-jm-device`. This stale "removed" JM device is what caused node-1's
  `port_allow`/reconnect task to loop forever ("Node unable to connect to remote
  JMs, retry task") once the scheduler deadlock above was cleared and the cluster
  tried to actually converge. Fixed via `restart-jm-device --force`.

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

- **Real gap, not yet fixed**: `restart-device` fails to recover an lblk/AIO device
  after the kernel reassigns its backing block-device name (e.g. GCP detach/reattach:
  `/dev/sdc` → `/dev/sdd`), even though `sbcli`'s `restart_device()` already contains
  serial-first re-resolution logic explicitly written for this ("lblk mode: ... kernel
  names shift, recreate if gone" —
  `simplyblock_core/controllers/device_controller.py:497`). Root cause: that
  re-resolution is gated behind `if not snode.rpc_client().get_bdevs(device_obj.nvme_bdev):`
  — but the preceding teardown step only deletes the PT/alceml layers, never the base
  `bdev_aio` itself, so a **stale** aio bdev object (still registered in SPDK, still
  pointing at the now-gone `/dev/sdc` file descriptor) satisfies `get_bdevs()` and the
  whole serial-resolution/recreate block gets skipped. The subsequent
  `_def_create_device_stack()` then tries to layer a fresh alceml bdev on top of that
  dead aio bdev and fails outright (`Failed to create alceml bdev`,
  `bdev.c:8748:bdev_open_ext: *NOTICE*: Currently unable to find bdev with name:
  alceml_...`), leaving the device permanently stuck in `unavailable`/`removed` status
  — no automatic recovery path once this happens.
  - Repro: on GCP lblk cluster, live-detached a storage node's data disk (confirmed
    `device_remove` fires correctly, node degrades gracefully — see Error Cases above),
    then reattached the same GCP PD. Kernel remapped it from `/dev/sdc` to `/dev/sdd`
    (confirmed via `lsblk` + `/dev/disk/by-id/google-persistent-disk-N` symlinks).
    `sbcli-dev storage-node restart-device <uuid>` then failed with "Failed to create
    alceml bdev" / "Failed to create device stack", and the SPDK RPC log showed
    `bdev_alceml_create` called against `cntr_path: aio_SYN_<serial>_<id>` with no
    preceding fresh `bdev_aio_create` call for that name at all — i.e. the
    serial-resolution branch never ran.
  - Fix direction: either (a) have the teardown step also delete the base `bdev_aio`
    bdev unconditionally before the type check, so `get_bdevs()` correctly reports it
    missing and the recreate path always runs for `bdev_type == "aio"`, or (b) make the
    check itself verify the aio bdev's backing file is still openable (not just that a
    bdev object with that name exists) before deciding recreation isn't needed. Not
    fixed live in this session — flagging as a finding for the backend (`sbcli`) team,
    since `simplyblock-operator` doesn't own this code path.
  - Confirmed the workaround: manually deleting the stale base bdev
    (`bdev_aio_delete` on `aio_SYN_<serial>_<id>`) via direct RPC, then re-running
    `restart-device --force`, recovered the device cleanly (`online`, rebalance task
    created, cluster went `DEGRADED` → `ACTIVE - REBALANCING`). So the underlying
    per-layer recreate logic is otherwise sound — it's specifically the stale-aio-bdev
    detection gap above that blocks the automatic path.

- **Real gap, not yet fixed**: a JM (journal) device's backing block device disappearing
  is not proactively detected the way a storage device's is. Repro: live-detached the
  GCP PD backing node-3's JM device (`gcloud compute instances detach-disk`, same
  technique as the passing "Device disappears" storage-device test above). Polled
  `sn list-devices` every 20s for 4+ minutes — the JM device stayed reported
  `Status: online, Health: True` the entire time (contrast: the storage-device case
  flipped to `removed`/`Health: False` within ~20s). Then wrote real data through a
  mounted CSI volume on a client node (`dd ... oflag=direct` into the k8s CSI
  globalmount path) — the write immediately failed with `Input/output error` at the
  application layer, proving the failure *is* real and *is* reaching the data path,
  it's just invisible in the node/device health reporting. So there is no graceful
  degradation for JM loss the way there is for a storage-device loss: no proactive
  flip to unhealthy/removed, no automatic failover of the journal role to a healthy
  mesh peer, and no re-balance triggered — the operator/monitoring stack simply
  doesn't know anything is wrong until/unless something else notices the I/O errors.
  Disk reattached afterward to restore node-3. Fix direction: the periodic
  device-health-check should probe the JM bdev the same way it probes storage bdevs
  (e.g. a lightweight read/write liveness check), not rely on it only being exercised
  incidentally by real journal traffic. Flagging for the backend (`sbcli`) team — this
  is core cluster/HA behavior, not something the operator controls.

- **Real gap, found incidentally (not yet fixed)**: leftover `failed_device_migration`
  tasks from the earlier cluster-expansion incident (see above — the removed node-4
  and its devices) were still present in `cluster list-tasks`, `suspended`, each
  retried 75-77 times, all failing with `'Device <uuid> not found'` (confirmed via
  direct DB lookup — the referenced devices genuinely no longer exist, they belonged
  to the node we `sn remove`'d during that recovery). These can never succeed since
  their target device is gone, yet nothing auto-cancels or garbage-collects them —
  they'd have retried forever, and appear to have been holding the whole cluster's
  health/status reporting hostage (`cluster list` showed `DEGRADED`/stuck
  `REBALANCING` and all 3 nodes `Health: False` well past when the actual, unrelated
  device-recovery work should have converged). Manually canceled all 12 via
  `sbcli-dev cluster cancel-task <id>`, after which the cluster's task queue cleared
  down to only genuine in-flight recovery-migration tasks. Fix direction: task
  scheduler should detect a "device not found" failure as terminal (not retry-forever)
  and auto-cancel, or at least surface it distinctly from a transient/retryable
  failure. Flagging for the backend (`sbcli`) team.

- **Real gap, not yet fixed**: `node_utils.py`'s partition-table-type detection
  (used before splitting a GPT partition for the journal in lblk-with-partitions
  mode) relies on `lsblk -ndo PTTYPE /dev/{parent}`, which silently returns an
  **empty string** — not an error, not "unknown", just empty — inside any container
  that doesn't have the host's `/run/udev` bind-mounted (`lsblk`'s PTTYPE column
  needs the udev database; without it, it doesn't fall back to a raw probe the way
  `blkid -p` does). Confirmed the real production `snode-spdk-pod`/DaemonSet
  (`simplyblock-storage-node-ds-gcp-lblk-node` on the GCP lblk cluster) mounts
  `dev-vol:/dev`, `host-sys:/sys`, `etc-simplyblock`, `host-mnt`, `host-modules`,
  `var-run-simplyblock` — **no `/run/udev`** — so this isn't a debug-pod artifact,
  it's the actual shipped configuration. Net effect: `node_configure.py --lblk` with
  `--jm-percent`/partition-based journal splitting will always hit
  `"disk X has partition table 'unknown'; splitting a partition for the journal
  requires GPT"` in production, even against a disk that genuinely is GPT — a
  real, always-on feature is unconditionally broken.
  - Repro: on `config-israel` vm15, ran the `feat-lblk-rebased-onto-main` image as a
    privileged debug pod with `/dev`+`/sys` (matching prod) against the spare
    `nvme3n1` disk (confirmed via `blkid -p`: genuinely GPT, two labeled partitions).
    `lsblk -ndo PTTYPE /dev/nvme3n1` returned empty; adding a `/run/udev` hostPath
    mount fixed it instantly (`gpt` reported correctly, `/run/udev/data` populated).
  - Fix direction: use `blkid -p -o value -s PTTYPE /dev/{parent}` (or equivalent
    direct-probe call) instead of `lsblk`'s udev-dependent column, so detection works
    regardless of whether `/run/udev` happens to be mounted.

- **Real gap, not yet fixed**: once partition-table detection is fixed (see above),
  `split_partition_for_journal()` in `node_utils.py` (~line 383) has a **sector-size
  unit mismatch**. It reads `start`/`size` from `/sys/block/{parent}/{part}/{start,
  size}` — which the kernel always expresses in fixed 512-byte units regardless of
  the device's actual sector size — then passes those raw numbers directly as sector
  arguments to `sgdisk`, which interprets them in the **disk's own native sector
  size**. On a classic 512-byte-sector disk the two units coincide and it works by
  accident; on any 4Kn (4096-byte native sector) disk, every boundary is inflated by
  exactly `native_sector_size / 512` = 8x.
  - Repro: requested a 2 GiB (`2147483648`-byte) journal split via `--jm-percent` on
    `nvme3n1` (a QEMU NVMe emulated disk with 4096-byte logical/physical sectors,
    confirmed via `sgdisk -p`: "Sector size (logical/physical): 4096/4096 bytes").
    The resulting `sb_jm` partition came out as **16.0 GiB** — exactly 8x the
    requested size — and the subsequent data-partition `sgdisk -n` call then failed
    outright (`Could not create partition 3 ... Error encountered; not saving
    changes`) because the inflated journal partition's end sector overran into
    already-allocated space.
  - Fix direction: convert the sysfs `start`/`size` values (always 512-byte units) to
    the disk's actual logical sector size before building the `sgdisk` sector
    arguments — e.g. multiply/divide by `(sysfs 512B units) / (blockdev --getss
    /dev/{parent})` — or pass byte offsets to a tool that accepts them directly
    instead of raw sector counts.
  - Confirmed MBR itself is handled correctly once detection is fixed: the same
    disk relabeled as `dos` (MBR) via `sfdisk` was cleanly and correctly rejected
    with `"disk nvme3n1 has partition table 'dos'; splitting a partition for the
    journal requires GPT"` — no crash, no partial mutation, clear error.
  - Test hygiene: `nvme3n1` was fully restored afterward to its original state (GPT,
    two 20GiB partitions, same PARTUUIDs `c1987f59-...`/`61be8e65-...` and disk GUID
    `1f2c3b58-...`, labels `data1`/`data2`) via `sgdisk --zap-all` + recreate, so the
    disk is unchanged for whoever uses it next.
