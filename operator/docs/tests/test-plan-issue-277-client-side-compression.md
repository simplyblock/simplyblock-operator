# Test Plan: Client-Side Compression / Deduplication (Issue #277)

Related design: [`designs/design-issue-277-client-side-compression.md`](../designs/design-issue-277-client-side-compression.md)
Spike transcript: [`designs/spike-log-issue-277-client-side-compression.md`](../designs/spike-log-issue-277-client-side-compression.md)

None of the CSI-side code in this plan exists yet (`csi-driver/pkg/util/vdo.go` and the
`nodeserver.go` wiring are still design, not implementation) — this is a forward test plan
to build against while implementing, not a report of what's already covered.

Each scenario is classified two ways:

- **Type**: **Positive** (the mechanism works as intended) or **Negative** (an error,
  edge case, or adverse condition is handled correctly rather than corrupting data,
  crashing, or silently degrading).
- **Level**: **Unit** (no cluster; mocked `lvm`/`vdo`/exec calls, mirroring
  `initiator_test.go`/`nodeserver_test.go` patterns), **Integration** (live cluster, a real
  NVMe-oF-backed lvol, non-destructive), or **E2E** (full path: StorageClass → scheduler →
  `NodeStageVolume`, requires the CSI-integrated code to exist).

---

## 1. `CreateOrAttachVDO` (`csi-driver/pkg/util/vdo.go`)

| Test | Scenario | Type | Level |
|---|---|---|---|
| Fresh device, no existing VG | Creates a new PV/VG/vdo-pool/LV stack named after `volumeID` | Positive | Unit + Integration |
| VG for `volumeID` already exists but inactive | Reactivates (`vgchange -ay`/`lvchange -ay`) instead of recreating | Positive | Unit + Integration |
| Called twice in a row on an already-active VG | Idempotent no-op, no duplicate `lvcreate`/`vgcreate` error | Positive | Unit |
| `compression=true, deduplication=false` | `lvcreate --type vdo --compression y --deduplication n` | Positive | Unit (assert exact args) |
| `compression=false, deduplication=true` | `lvcreate --type vdo --compression n --deduplication y` | Positive | Unit |
| `compression=true, deduplication=true` | `lvcreate --type vdo --compression y --deduplication y` | Positive | Unit |
| Device smaller than the ~4.72GiB VDO floor | Returns an explicit error before ever invoking `lvcreate` | Negative | Unit |
| `lvcreate` itself fails (e.g. host reports insufficient space) | Error propagated; no partial PV/VG left registered | Negative | Unit + Integration |
| `devicePath` doesn't exist / isn't a block device | Returns error before invoking any LVM command | Negative | Unit |
| `vgs`/`lvs` probe itself errors (e.g. LVM lock contention) | Error surfaced, not misread as "VG doesn't exist" (which would trigger a destructive `lvcreate` over live data) | Negative | Unit |

## 2. `ResolveClonedVDO`

| Test | Scenario | Type | Level |
|---|---|---|---|
| Clone with byte-duplicate PV/VG UUID vs. its source | UUIDs regenerated, VG renamed to `volumeID`, before `CreateOrAttachVDO`'s own `vgs` check runs | Positive | Unit + Integration (real `sbctl` clone) |
| Genuinely fresh, non-clone device | No-op — does not misfire and rename a legitimately new device | Negative (guards against a false positive) | Unit |
| `VolumeContentSource` unset, but device-level `blkid`/`pvs` shows an LVM/VDO signature under a foreign VG name | Defensive fallback still triggers `ResolveClonedVDO` | Positive | Unit + Integration |
| UUID-regeneration step fails mid-way (`vgimportclone`-equivalent partially applied) | Returns error; does not leave the VG in a half-renamed, half-old-UUID state that a later `CreateOrAttachVDO` could misinterpret | Negative | Unit |
| Two clones of the same source, scheduled to the same node in the same reconcile batch | Both resolve to independent identities without racing each other's `vgimportclone`/rename step | Negative (concurrency) | Integration/E2E |
| Clone scheduled to the **same node as its still-live source** | Both devices visible to the same node's LVM scan simultaneously; source's own VG/PV must not be affected by the clone's resolution | Negative | Integration |

## 3. `RemoveVDO`

| Test | Scenario | Type | Level |
|---|---|---|---|
| Normal teardown, PV reachable | `vgchange -an` + `vgremove` succeeds, device left inactive | Positive | Unit |
| Called twice / already removed | Idempotent no-op, not an error | Positive | Unit |
| PV unreachable — backing device already gone without a clean unstage (crash, force-detach; see design doc Section 8) | `vgremove` fails ("VG not found"); must fall back to direct `dmsetup remove` in dependency order (VDO target first) — this is a real gap the design doc flags as not yet implemented | Negative | Unit + Integration |
| `RemoveVDO` fails for any reason during `NodeUnstageVolume` | Caller must NOT proceed to `initiator.Disconnect` — disconnecting the raw device while VDO/dm nodes are still mapped on top of it creates exactly the orphaned-stack state above | Negative | Unit |

## 4. `GrowVDO`

| Test | Scenario | Type | Level |
|---|---|---|---|
| Grow while the volume is mounted and in use | `lvextend` (physical, pool LV) then `lvextend` (logical, VDO LV), filesystem stays intact and newly-available space is genuinely usable | Positive | Unit + Integration |
| `growPhysical`-equivalent (`lvextend` on the pool LV) succeeds, logical `lvextend` fails | Filesystem resize must NOT proceed against an inconsistent physical/logical state | Negative | Unit |
| Growth starting exactly at/near the ~4.72GiB minimum-size floor | Not yet spiked (existing spike started from an already-above-floor 5.5G pool) — needs its own pass | Negative (edge case) | Integration |
| Requested `newSize` smaller than current size | Rejected/no-op — VDO/LVM don't support online shrink | Negative | Unit |

## 5. Node Capability Detection & Advertisement (design doc Section 4)

| Test | Scenario | Type | Level |
|---|---|---|---|
| `kmod-kvdo`/`vdo` already installed | `postStart` hook's `rpm -q` check short-circuits; no repo hit, no reinstall on pod restart | Positive | Integration |
| Not installed, BaseOS repo reachable | Installs successfully via `nsenter`+`dnf`; marker file written; Node label patched `simplyblock.io/vdo-capable=true` | Positive | Integration |
| `dnf install` fails (airgapped / no repo access) | Marker reflects failure; label `vdo-capable=false` or absent; node excluded from VDO-requesting scheduling | Negative | Integration |
| `modprobe kvdo` fails (kernel/`kmod-kvdo` NVR mismatch — already reproduced once against the original spike cluster) | Same failure/labeling path as above | Negative | Integration |
| `buildAccessibleTopology` with `vdo-capable=true` node label present | Surfaced as a CSI topology segment via `NodeGetInfo` | Positive | Unit |
| `buildAccessibleTopology` with the label absent/false | Segment omitted — node not advertised as VDO-capable | Negative | Unit |
| Node's capability regresses **after** a VDO-backed PVC is already bound and running there (e.g. an OS update removes `kvdo` compatibility) | `NodeStageVolume`/`restageVolume` must hard-fail loudly on the next stage/restage rather than silently mounting the raw device (which could already carry a VDO container on-disk) | Negative | Unit + Integration |

## 6. StorageClass Parameters / CRD (design doc Section 6)

| Test | Scenario | Type | Level |
|---|---|---|---|
| `clientCompression=true`, `clientDeduplication=false` | Generated StorageClass: `client_compression="True"`, `client_deduplication="False"` | Positive | Unit |
| `clientCompression=false`, `clientDeduplication=true` | Inverse of above | Positive | Unit |
| Both `true` | Both params `"True"` | Positive | Unit |
| Both omitted | Both default to `false` per `+kubebuilder:default=false`; VDO never attempted | Negative (default-safe) | Unit |
| Either parameter `true` | `AllowedTopologies` includes the `vdo-capable=true` segment (per Section 4/5's "needed whenever *either* is true" rule) | Positive | Unit |
| Neither parameter `true` | `AllowedTopologies` NOT constrained by `vdo-capable` — any node schedulable | Negative | Unit |
| DHCHAP's existing `AllowedTopologies`/`allowedNodes` gate also applies to the same pool | Both constraints compose as AND in the generated `TopologySelectorTerm`, not overwritten by one another | Negative (regression risk) | Unit |

## 7. `nodeserver.go` Wiring

| Test | Scenario | Type | Level |
|---|---|---|---|
| `client_compression=="true"` OR `client_deduplication=="true"` in volume context | `CreateOrAttachVDO` called between `initiator.Connect` and `stageVolume`; VDO device path passed to `stageVolume`, not the raw NVMe-oF path | Positive | Unit (mocked VDO layer) |
| Neither flag true | `CreateOrAttachVDO` never called; raw device path used directly, unchanged from today's behavior | Negative (no-regression) | Unit |
| Volume created from `VolumeContentSource` (clone/snapshot restore) | `ResolveClonedVDO` runs before `CreateOrAttachVDO`'s own `vgs` check | Positive | Unit |
| `NodeUnstageVolume`, VDO was in use | `RemoveVDO` called **before** `initiator.Disconnect` (order asserted, not just both-called) | Positive | Unit |
| `restageVolume`/`ensureDeviceConnected` reconnect path, VDO was in use | Existing VDO device reattached (`pvscan --cache`, `vgchange -ay`) — never recreated/reformatted | Positive | Unit + Integration |
| Reconnect path encounters a device that looks blank/uninitialized where VDO was expected | Must fail loudly, not silently `lvcreate` a fresh VDO container over what should already hold data | Negative | Unit |
| `NodeExpandVolume`, VDO in use | `GrowVDO` called before the existing filesystem-resize logic | Positive | Unit |
| `NodeExpandVolume`, VDO in use, `GrowVDO` fails | Filesystem resize NOT attempted against a not-actually-grown device | Negative | Unit |
| `stageVolume` with `fsType=="xfs"` and either client flag true | `xfsStripeOptions` skipped entirely (stripe hints computed for the raw device are meaningless/misleading once VDO virtualizes blocks) | Positive | Unit — **not yet exercised beyond unit level; every hands-on spike so far used ext4, see design doc Section 12** |
| `stageVolume` with `fsType=="ext4"` and either client flag true | Unaffected — `xfsStripeOptions` path never applies to ext4 regardless | Negative (no-regression) | Unit |

## 8. Re-Provisioning / Failure Handling (design doc Section 8)

| Test | Scenario | Type | Level |
|---|---|---|---|
| Single VDO instance survives a full node reboot | Already verified in the spike log (§12) — port into a repeatable Integration test | Positive | Integration |
| Multiple VDO instances in parallel on one node | Already verified (§9 of the spike log) — no naming/dm collision, linear memory scaling — port into an Integration test | Positive | Integration |
| Multiple VDO instances present, then the node reboots together | **Verified** against the real implementation and a real reboot: two real PVCs on one node, distinct checksummed data, node rebooted — both reattached cleanly (`kvdo` usage count exactly 2, both `VDOOperatingMode: normal`, both checksums matched). Caveat: the two `NodeStageVolume` LVM sequences happened not to overlap in execution, so genuinely concurrent `vgchange`/`pvscan` locking remains unexercised (see design doc Notes) | Positive | Integration (done) |
| Genuinely concurrent (overlapping, not just closely-timed) `vgchange -ay`/`pvscan --cache` calls racing at the LVM level | Not yet forced — the reboot test above happened to serialize naturally | Negative | Integration |
| Backing device disappears without a clean unstage while the node stays up (crash / storage-side force-disconnect, pod rescheduled elsewhere) | Orphaned dm-vdo stack must be recoverable via `RemoveVDO`'s `dmsetup remove` fallback (see §3 above); reproduced once by accident in the spike log, not yet deliberately reproduced end-to-end | Negative | E2E (forced-failure simulation) |
| Original node rejoins as Ready after the scenario above | kubelet's volume reconciler re-invokes `NodeUnstageVolume`, giving the `dmsetup` fallback a chance to run without manual intervention — standard kubelet/CSI mechanics, not yet verified against this cluster's actual failure/reschedule path | Negative | E2E |
| `system.devices` bookkeeping after repeated node-failure cycles | Stale entries pruned (`lvmdevices --deldev`) rather than accumulating indefinitely across a node's lifetime | Negative (hygiene, not correctness) | Integration |

## 9. Compatibility Gaps (design doc Section 12)

| Test | Scenario | Type | Level |
|---|---|---|---|
| Clone/snapshot-restored volume scheduled onto the same node as its still-live source | Byte-duplicate PV/VG UUID resolved by `ResolveClonedVDO` before either device's LVM activation collides node-wide | Negative | E2E |
| XFS requested as `fsType` with either client flag true | End-to-end pass with XFS on top of a real VDO device — every existing spike used ext4 only (see design doc Section 12's added note) | Negative (untested combination) | Integration |
| Server-side `compression=true` + `client_compression=true` on the same volume | Redundant but not harmful — confirm no double-compression correctness issue, just wasted CPU | Negative | Integration |
| Server-side `encryption=true` + `client_compression=true` | Confirmed compatible via architecture research (crypto bdev sits below the nvmf attach point) — port the plaintext-vs-ciphertext compression-ratio check into a repeatable test | Positive | Integration |
| Placement webhook / volume placement injector co-locating a workload with a non-`vdo-capable` storage node | Confirm the two placement constraints compose as AND rather than conflicting | Negative | Integration |

---

## What Is Not Yet Covered

| Gap | Reason |
|---|---|
| Full CSI-integrated E2E (`client_compression=true` PVC through the real StorageClass → topology gate → `NodeStageVolume` path) | Blocked on the implementation itself existing — design only today |
| `lvm2`/VDO segtype availability detection as part of node capability checks | Open question in design doc Section 13, not yet decided |
| Standalone `vdo` CLI vs. LVM-integrated VDO as the long-term management interface | Open risk noted in design doc Non-Goals; affects `vdo.go`'s shape if it changes |
| `atlas-lib` placement of VDO/LVM primitives (vs. `csi-driver/pkg/util/vdo.go`) | Design suggestion under discussion, not yet decided |
| Multi-socket / multi-VDO-instance memory pressure at fleet scale (many volumes, one node) | Only 2-instance parallel scaling has been measured; no stress test at realistic fleet density |
| VolumeMigration's actual cutover moment (old paths going inaccessible, new paths becoming primary) | Spike observation window ended before cutover completed (see design doc Section 12) |
