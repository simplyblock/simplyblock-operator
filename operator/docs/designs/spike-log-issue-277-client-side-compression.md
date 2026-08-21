# Spike Log: Client-Side Compression (VDO) — Issue #277

Companion to [`design-issue-277-client-side-compression.md`](design-issue-277-client-side-compression.md).
This is a record of hands-on experiments actually run against the live test
cluster (`~/.kube/config`, 4-node k3s, Rocky Linux 9.5), not a test plan — every
command below was actually executed, with real output. Kept separate from the
design doc so the design stays readable while this stays a faithful transcript
to reproduce or extend from.

All experiments used `kubectl debug node` (read-only checks, mounts host `/` at
`/host`) or a scratch privileged+`hostPID` pod running `nsenter -t 1 -m -u -n -i
-- <cmd>` (for actions needing real host mutation: package installs, module
loads, reboots). Scratch Kubernetes objects (pods, PVCs, StorageClasses, VG/LV/
loop devices) were cleaned up after each experiment unless noted otherwise.

---

## 1. Baseline feasibility spike (original, before this log started)

- Cluster: all 4 nodes on `5.14.0-503.14.1.el9_5.x86_64`.
- `kmod-kvdo`/`vdo` available via `dnf` (BaseOS repo) but not installed by default.
- `modprobe kvdo` failed: no matching prebuilt `kvdo.ko` for the running kernel
  (RHEL's weak-modules mechanism only had builds for `el9_8`-era kernels).
- A newer kernel (`5.14.0-687.33.1.el9_8`) was available in-repo with a
  matching module, but no reboot was performed at this stage.

## 2. RBAC delta check (`vm04`)

Verified a ClusterRole scoped to exactly `get`/`patch`/`update` on `nodes` is
sufficient to label a node (nothing broader needed):

```bash
kubectl auth can-i patch nodes --as=system:serviceaccount:default:vdo-label-test-sa   # yes
kubectl auth can-i update nodes --as=system:serviceaccount:default:vdo-label-test-sa  # yes
kubectl auth can-i list persistentvolumeclaims --as=system:serviceaccount:default:vdo-label-test-sa  # no
kubectl patch node vm04... --as=system:serviceaccount:default:vdo-label-test-sa \
  --type=merge -p '{"metadata":{"labels":{"simplyblock.io/vdo-capable":"true"}}}'  # succeeded
```

Cleaned up (label removed, scratch SA/ClusterRole/ClusterRoleBinding deleted).

## 3. Topology gate dry run (`vm04`)

Used the driver's *existing* zone-based topology plumbing (no new code) as a
faithful analog for the not-yet-implemented `vdo-capable` key — same
`buildAccessibleTopology`/`AllowedTopologies` code path, different label.

```bash
kubectl label node vm04... topology.kubernetes.io/zone=vdo-test-zone --overwrite
# scratch StorageClass (copy of a real pool's params) with:
#   allowedTopologies: [{matchLabelExpressions: [{key: topology.kubernetes.io/zone, values: [vdo-test-zone]}]}]
#   volumeBindingMode: WaitForFirstConsumer
```

- **Positive case**: PVC/pod scheduled → `selected-node` annotation = `vm04`
  (the only matching node), pod nominated to `vm04`.
- **Negative case**: StorageClass zone value changed to `zone-that-does-not-exist`
  → pod stayed fully unscheduled: `0/4 nodes are available: 4 node(s) didn't
  find available persistent volumes to bind`.

Cleaned up (pod/PVC/StorageClass deleted, zone label reverted to `default`).

## 4. nsenter install mechanism check (`vm03`, untouched node)

```bash
# From a pod with hostPID: true, privileged: true, capabilities: [SYS_ADMIN, SYS_MODULE]:
nsenter -t 1 -m -u -n -i -- dnf install -y kmod-kvdo vdo
```

Installed cleanly. **Finding**: this pulled in a full alternate kernel
(`kernel-core`, `kernel-modules`, `kernel-modules-core`, `5.14.0-687.33.1.el9_8`,
~88MB) as a transitive dependency of `kmod-kvdo`, **and silently changed the
node's default boot entry** to that new kernel (`grubby --default-kernel`
confirmed before/after). Reverted:

```bash
nsenter -t 1 -m -u -n -i -- grubby --set-default /boot/vmlinuz-5.14.0-503.14.1.el9_5.x86_64
```

vm02 (from the original spike) was checked too — its default was untouched,
apparently because those kernel packages were already present from an
unrelated prior state.

## 5. DKMS/source-build prerequisite check (`vm02`) — ruled out, no build attempted

```bash
dnf list kernel-devel --showduplicates | grep "503.14.1.el9_5"   # not found — only 687.x builds listed
curl -sI https://github.com                                       # HTTP/2 200 — internet access is fine
dnf list --showduplicates "kvdo*" "dm-vdo*"                        # no matching packages
dnf provides "*/kvdo.c"                                            # no matching packages
```

No `kernel-devel` for the exact running kernel → no headers to build against.
**This was a prerequisite check only** — no source was cloned, no `make` was
run, no module was loaded. Confirmed via `github.com/dm-vdo/kvdo`'s own README
that the source targets whole EL-release kernel families generically (not
NVR-pinned like the prebuilt RPM), so the blocker is vendor header retention,
not source versioning — but the practical outcome is identical: no currently
buildable path on this kernel.

## 6. Real kernel upgrade + `kvdo` load + real VDO device (`vm04`)

**Explicitly approved by the user**, after checking:
- Pool redundancy: 3 storage nodes (vm02/vm03/vm04), `distr_npcs=1` (tolerates
  one node loss).
- Workload footprint on vm04 vs vm02/vm03: comparable across all three (each
  runs one storage-node shard + 1-2 FDB shards + a couple of single-instance
  pods that reschedule freely) — vm04 was not meaningfully riskier.

```bash
kubectl cordon vm04.simplyblock4.localdomain

# from a hostPID + privileged pod on vm04:
nsenter -t 1 -m -u -n -i -- dnf install -y kmod-kvdo vdo
nsenter -t 1 -m -u -n -i -- grubby --set-default /boot/vmlinuz-5.14.0-687.33.1.el9_8.x86_64
nsenter -t 1 -m -u -n -i -- systemctl reboot
```

Node came back `Ready` on `5.14.0-687.33.1.el9_8.x86_64`.

```bash
nsenter -t 1 -m -u -n -i -- modprobe kvdo      # exit 0 — first success in this whole investigation
nsenter -t 1 -m -u -n -i -- lsmod | grep vdo   # kvdo 876544 0; dm_bufio, dm_mod depend on it
```

**Standalone `vdo` CLI does not exist**:

```bash
nsenter -t 1 -m -u -n -i -- rpm -ql vdo | grep bin
# /usr/bin/vdodmeventd /usr/bin/vdodumpconfig /usr/bin/vdoforcerebuild
# /usr/bin/vdoformat /usr/bin/vdosetuuid /usr/bin/vdostats
# (no /usr/bin/vdo)
```

`lvm2` has VDO support built in instead:

```bash
nsenter -t 1 -m -u -n -i -- lvm segtypes | grep vdo   # vdo, vdo-pool
nsenter -t 1 -m -u -n -i -- lvm version               # --with-vdo=internal
```

**Minimum size, discovered by hitting it**:

```bash
nsenter -t 1 -m -u -n -i -- lvcreate --type vdo -n vdotestlv -L 1900M -V 4G vdotestvg/vdopool --yes
# Minimum required size for VDO volume: 5063921664 bytes (~4.72GiB)
# vdoformat: formatVDO failed ... Out of space
```

**Successful create, at a size above the floor**:

```bash
nsenter -t 1 -m -u -n -i -- dd if=/dev/zero of=/tmp/vdo-scratch.img bs=1M count=6144
nsenter -t 1 -m -u -n -i -- losetup -f --show /tmp/vdo-scratch.img   # /dev/loop0
nsenter -t 1 -m -u -n -i -- pvcreate /dev/loop0
nsenter -t 1 -m -u -n -i -- vgcreate vdotestvg /dev/loop0
nsenter -t 1 -m -u -n -i -- lvcreate --type vdo --config "activation{checks=0}" \
  -n vdotestlv -L 5.5G -V 4G vdotestvg/vdopool --yes
# Logical volume "vdotestlv" created.
nsenter -t 1 -m -u -n -i -- mkfs.ext4 -q /dev/vdotestvg/vdotestlv
nsenter -t 1 -m -u -n -i -- mount /dev/vdotestvg/vdotestlv /mnt/vdotest
```

**Write + measure**:

```bash
# 100MB of a trivially repetitive pattern, copied 5x (best-case synthetic dedup/compression test)
yes ABCDEFGHIJ | head -c 100000000 > /mnt/vdotest/base.bin
for i in 1 2 3 4 5; do cp /mnt/vdotest/base.bin /mnt/vdotest/copy$i.bin; done
sync

nsenter -t 1 -m -u -n -i -- vdostats --human-readable vdotestvg-vdopool-vpool
# Device                       Size      Used Available Use% Space saving%
# vdotestvg-vdopool-vpool      5.5G      3.5G      2.0G  64%           99%

nsenter -t 1 -m -u -n -i -- vdostats --verbose vdotestvg-vdopool-vpool
# data blocks used      : 642        (~2.6MB — actual unique data after dedup+compression)
# overhead blocks used  : 918272     (~3.5GB — VDO's own bookkeeping)
# physical blocks       : 1441792    (the 5.5G pool)
# KVDO module bytes used: 408960848  (~390MB kernel RAM for this one instance)
```

**Interpretation**: 600MB of highly-duplicate/compressible input compressed
to ~2.6MB of real physical data — an excellent ratio, but for this synthetic
best-case input, not representative of real workloads. The number that
matters architecturally: **~3.5GB (64%) of the minimum-size pool is fixed VDO
overhead, present before any data is written, plus ~390MB kernel RAM per
instance.** See the design doc's Section 7 and Open Questions for what this
means for "one VDO instance per PVC."

**Cleanup** (scratch VG/LV/loop device only):

```bash
nsenter -t 1 -m -u -n -i -- umount /mnt/vdotest
nsenter -t 1 -m -u -n -i -- lvremove -f vdotestvg
nsenter -t 1 -m -u -n -i -- vgremove -f vdotestvg
nsenter -t 1 -m -u -n -i -- pvremove /dev/loop0
nsenter -t 1 -m -u -n -i -- losetup -d /dev/loop0
nsenter -t 1 -m -u -n -i -- rm -rf /tmp/vdo-scratch.img /mnt/vdotest

kubectl uncordon vm04.simplyblock4.localdomain
```

**State intentionally left behind**: at the user's explicit request, `vm04`'s
kernel and `kvdo` module were **not reverted** — it stays on
`5.14.0-687.33.1.el9_8.x86_64` with `kvdo` loaded, so further spikes (reconnect/
reactivate, `lvextend` growth) can build on it directly without repeating the
upgrade+reboot.

---

## 7. Reconnect/reactivate after simulated device loss (`vm04`)

Fresh VDO volume, separate from the one in §6 (same recipe: 6GB loop file,
5.5G physical pool, 4G logical LV, `vgdorecvg`/`vdoreclv`), with a canary file
written and checksummed before the simulated failure:

```bash
echo canary-data-before-disconnect > /mnt/vdorec/canary.txt
sha256sum /mnt/vdorec/canary.txt
# 857093e99ca7a530c9b76b4d8a5c59df332378a354b92d0d6aef0ae186587118
```

**Simulate total device loss** (a plain `losetup -d` while mounted turned out
to be insufficient — the dm-vdo layer still held the loop device open, so it
didn't actually disappear; the DM stack has to come down first, same as a real
forced-removal path):

```bash
losetup -d /dev/loop0        # returns immediately, but...
cat /mnt/vdorec/canary.txt   # ...still succeeds — device wasn't really gone
dmsetup ls                   # ...vdo devices still present

umount -l /mnt/vdorec
vgchange -an vdorecvg
losetup -a                   # now empty — device is genuinely gone
```

**Reconnect** (new loop device from the same backing file — deliberately not
assuming the same device node, matching real NVMe-oF ANA reconnect behavior):

```bash
losetup -f --show /tmp/vdo-scratch2.img   # /dev/loop0 again, but could differ
pvscan --cache                             # PV /dev/loop0 online.
vgchange -ay vdorecvg                      # 1 logical volume(s) ... now active
mount /dev/vdorecvg/vdoreclv /mnt/vdorec
sha256sum /mnt/vdorec/canary.txt
# 857093e99ca7a530c9b76b4d8a5c59df332378a354b92d0d6aef0ae186587118  (unchanged)
```

**Result**: `pvscan --cache` + `vgchange -ay` reattaches the existing VDO
volume with zero recreate/reformat, regardless of device path. Data verified
byte-identical via checksum. This confirms the mechanism Section 9 of the
design doc relies on.

## 8. Growth: `lvextend` on pool + LV, online (`vm04`)

Using the same volume from §7, after reconnect:

```bash
df -h /  /tmp                              # 5.4G available on host root before growing

truncate -s 8G /tmp/vdo-scratch2.img       # backing file 6G -> 8G
losetup -c /dev/loop0                      # refresh loop device to new file size
pvresize /dev/loop0                        # PV: <8.00g, <2.50g free

lvextend -L +2G vdorecvg/vdopool           # physical: 5.5G -> 7.5G, ONLINE (fs still mounted)
# Data% dropped from 63.68% to 46.71% -- same absolute overhead, smaller % of a bigger pool

lvextend -L +2G vdorecvg/vdoreclv          # logical: 4G -> 6G

resize2fs /dev/vdorecvg/vdoreclv           # online resize
df -h /mnt/vdorec                          # 3.9G -> 5.9G

sha256sum /mnt/vdorec/canary.txt           # unchanged throughout

dd if=/dev/urandom of=/mnt/vdorec/postgrowth.bin bs=1M count=500   # succeeds
df -h /mnt/vdorec                          # 501M used -- space is genuinely usable, not just reported
```

**Result**: the full growth chain (`pvresize` → `lvextend` pool → `lvextend`
LV → `resize2fs`) works end-to-end, entirely online, with no data loss and no
unmount required. Not yet tested: growth behavior specifically *at* the
~4.72GiB minimum-size floor (this spike started from an already-above-floor
5.5G pool).

Cleaned up: unmounted, `vgremove`/`pvremove`/`losetup -d`, scratch files
removed, test pod deleted. `vm04` itself left as before — new kernel, `kvdo`
loaded, uncordoned.

---

## 9. Multiple concurrent VDO instances (`vm04`)

Only 12G free on vm04's single 30G disk (no spare capacity elsewhere on this
VM) — scoped down to **2** concurrent instances rather than 3-5, at ~5.1GB
each. This is a test-VM disk-size constraint, not a design limitation.

```bash
pvcreate /dev/loop0 /dev/loop1
vgcreate vdomulti1 /dev/loop0 && vgcreate vdomulti2 /dev/loop1
lvcreate --type vdo -n lv1 -L 5.1G -V 4G vdomulti1/vdopool1 --yes
lvcreate --type vdo -n lv2 -L 5.1G -V 4G vdomulti2/vdopool2 --yes

dmsetup ls
# vdomulti1-lv1, vdomulti1-vdopool1-vpool, vdomulti1-vdopool1_vdata
# vdomulti2-lv2, vdomulti2-vdopool2-vpool, vdomulti2-vdopool2_vdata  -- no collisions

lsmod | grep vdo
# kvdo 876544 2  -- one module instance, shared, serving both targets (usage count 2)

mkfs.ext4 ... && mount ... (both)
echo instance-1-data > /mnt/multi1/marker.txt
echo instance-2-data > /mnt/multi2/marker.txt
cat /mnt/multi1/marker.txt   # instance-1-data -- correct, no crossover
cat /mnt/multi2/marker.txt   # instance-2-data -- correct, no crossover

vdostats --verbose vdomulti1-vdopool1-vpool | grep "KVDO module bytes used"   # 817919568
vdostats --verbose vdomulti2-vdopool2-vpool | grep "KVDO module bytes used"   # 817919568 (identical -- global, not per-instance)
```

**Result**: no naming/dm collisions between simultaneous instances; data stays
correctly isolated. `KVDO module bytes used` is a **module-wide total**, not
per-instance — confirmed by both instances reporting the identical number.
Math: single-instance baseline from §6 was `408960848` bytes;
`408960848 × 2 = 817921696`; measured 2-instance total was `817919568` —
within 0.0003% of exactly double. **RAM cost scales linearly per instance, no
shared savings beyond the module's own fixed code/data.**

Cleaned up: unmounted, `vgremove` both, `pvremove`/`losetup -d` both, scratch
files removed.

## 10. Performance overhead: raw vs. VDO-backed (`vm04`, `fio` 3.35)

Raw 2G loop device vs. a 5.1G-pool/4G-logical VDO device, both `direct=1`,
`ioengine=libaio`, `iodepth=16`, `bs=1M`, tested against the **block device
directly** (no filesystem, to isolate VDO's own overhead from fs noise).

```bash
# RAW, sequential write, incompressible (random, refill_buffers)
fio --name=rawwrite --filename=/dev/loop0 --rw=write --bs=1M --size=1500M \
  --direct=1 --ioengine=libaio --iodepth=16 --refill_buffers --randrepeat=0
# WRITE: bw=1076MiB/s

# RAW, sequential read
fio --name=rawread --filename=/dev/loop0 --rw=read --bs=1M --size=1500M --direct=1 --ioengine=libaio --iodepth=16
# READ: bw=2538MiB/s

# VDO, sequential write, incompressible (random)
fio --name=vdowriteinc --filename=/dev/vdoperf/perflv --rw=write --bs=1M --size=1500M \
  --direct=1 --ioengine=libaio --iodepth=16 --refill_buffers --randrepeat=0
# WRITE: bw=99.6MiB/s   -- ~10.8x slower than raw

# VDO, sequential read (of the incompressible data just written)
fio --name=vdoreadinc --filename=/dev/vdoperf/perflv --rw=read --bs=1M --size=1500M --direct=1 --ioengine=libaio --iodepth=16
# READ: bw=957MiB/s     -- ~2.65x slower than raw

# VDO, sequential write, zero-filled ("compressible") -- MISLEADING, see below
fio --name=vdowritecomp --filename=/dev/vdoperf/perflv --rw=write --bs=1M --size=1500M \
  --direct=1 --ioengine=libaio --iodepth=16 --zero_buffers
# WRITE: bw=1045MiB/s   -- looks great, but...

# VDO, sequential write, REALISTIC ~50% compressible (buffer_compress_percentage, not literal zeros)
fio --name=vdowritemid --filename=/dev/vdoperf/perflv --rw=write --bs=1M --size=1500M \
  --direct=1 --ioengine=libaio --iodepth=16 --buffer_compress_percentage=50 --refill_buffers
# WRITE: bw=135MiB/s    -- ~8x slower than raw -- THIS is the representative number
```

**Important methodology note**: the `--zero_buffers` "compressible" test
(1045 MiB/s, nearly matching raw) is **misleading** — all-zero blocks hit
VDO's dedicated zero-block fast path (not even written, just mapped), which
isn't representative of real compressible data (logs, text, JSON — compress
well via LZ4 but aren't literally all-zero). The `buffer_compress_percentage`
test exercises the actual compression pipeline and is much closer to the
incompressible result (135 MiB/s vs. 99.6 MiB/s) — both around **8-11x
slower than raw for writes**, while reads are a more modest **~2.65x slower**.

**Caveats — this is a bound, not a production number**: (1) 12 vCPUs were
available, so the write penalty isn't simple CPU starvation; (2)
`lvs -o+vdo_write_policy` showed `auto`, which likely resolves to `sync` for a
loop device (no volatile write-cache reporting) — a real NVMe-oF device might
let VDO run `async` and close much of this gap; (3) this is a loop-device/VM
environment, not real NVMe hardware. **This needs to be re-measured against
real NVMe-oF-backed storage before drawing production conclusions** — but the
order of magnitude (multi-x write overhead) is a real signal, not test noise,
and should inform whether client-side compression is opt-in-only for
throughput-sensitive workloads.

Cleaned up: `vgremove`/`pvremove`/`losetup -d` both devices, scratch files
removed.

## 11. Crash/interrupt during `lvcreate --type vdo` (`vm04`) — inconclusive

Three attempts to catch `lvcreate --type vdo`/`vdoformat` mid-transaction, all
failed to produce a genuinely interrupted state:

1. Fixed `sleep 0.4` then `pkill -9 -f vdoformat` — the LV had already been
   fully created by the time the kill landed.
2. Tight polling loop (`pgrep -f vdoformat` every iteration, kill on first
   detection, effectively catching it at "iteration 1") — LV still completed
   successfully. `time lvcreate ...` on an 8.5G pool measured **0.563s
   wall-clock for the entire operation** — too fast to reliably race with
   simple signals in this environment.
3. `kill -STOP` on detection (to freeze rather than race a kill) — same
   outcome; the operation had effectively finished by the time the freeze
   took hold, or the exec session's own overhead exceeded the operation's
   runtime.
4. Attempted a deterministic slowdown via a `dm-delay` wrapper device
   (`dmsetup create ... delay ... 200 ...`) to force multi-second I/O latency
   — abandoned due to LVM duplicate-PV detection between the raw loop device
   and the delay device, and `device-or-resource-busy` errors removing the
   delay device afterward. Not worth further time given this is the
   lowest-priority of the four spikes in this round.

**Bounded conclusion**: `lvcreate --type vdo` completes very fast (sub-second
even at 8-9GB) in this environment, which narrows the practical window for a
`NodeStageVolume` interruption (e.g. kubelet killing the CSI pod) to catch it
truly mid-transaction. This doesn't prove it's impossible — real production
volumes, real (slower, network-attached) NVMe-oF storage, or a loaded node
could all widen the window — but it wasn't reproducible with straightforward
signal-based testing here. **Still an open item**, not a confirmed-safe result.

## 12. Node reboot with an active VDO volume (`vm04`)

Fresh VDO volume + canary file (checksum `5681eca8...`), backing file
deliberately placed in `/tmp` after confirming `/tmp` is NOT tmpfs on this
node (`findmnt /tmp` empty, `tmp.mount` disabled — i.e. part of the persistent
root disk, so it survives a reboot):

```bash
kubectl cordon vm04...
systemctl reboot   # via nsenter from a privileged+hostPID pod

# k8s API node-Ready polling never caught the NotReady window (same as the
# earlier kernel-upgrade reboot) -- confirmed the reboot actually happened via
# host uptime instead:
uptime   # up 3 min  -- genuine recent reboot confirmed

# State immediately after boot, BEFORE recreating the loop device:
vgs   # only "rl" (root VG) -- our test VG is invisible, no ghost/stale entries
pvs   # only /dev/sda2
dmsetup ls   # only rl-root, rl-swap -- no leftover VDO dm entries
lsmod | grep vdo   # empty -- kvdo NOT auto-loaded at boot
modprobe kvdo; echo $?   # 0 -- still loadable fine post-reboot

# Reconnect (identical sequence to §7, now after a REAL reboot not a simulated one):
losetup -f --show /tmp/vdo-reboot.img
pvscan --cache        # PV /dev/loop0 online.
vgchange -ay vdoreboot   # 1 logical volume(s) ... now active
mount /dev/vdoreboot/rebootlv /mnt/vdoreboot
sha256sum /mnt/vdoreboot/canary.txt
# 5681eca8c7c01e4d5ddce3765d968d237f034fbe89df83a61ff15425468bf5bd  -- unchanged
```

**Result**: a full node reboot with a VDO-backed VG present causes **no
auto-activation conflicts, no ghost VG state, no stale dm entries** — because
the backing device (loop file, standing in for the NVMe-oF path) simply isn't
present at boot time, so stock LVM boot-time scanning has nothing to act on.
The exact same reconnect mechanism from §7 works identically after a real
reboot. Data integrity confirmed via checksum match.

Cleaned up: unmounted, `vgremove`/`pvremove`/`losetup -d`, scratch files
removed, both test pods deleted, node uncordoned.

---

## 13. Encryption + client-side compression interaction (`vm04` + `sbcli` research)

Two-part spike: first an architecture question (does the NVMe-oF client
receive plaintext or ciphertext for an "encrypted" volume?), then an
empirical data test.

**Architecture** (research over `~/simplyblock/sbcli`,
`simplyblock_core/controllers/lvol_controller.py`): when encryption is
enabled, a crypto vbdev is layered on top of the base lvol and
`lvol.top_bdev` is reassigned to point at it (line 671); the nvmf
namespace-add RPC always exposes `lvol.top_bdev` (lines 1180-1181,
1299-1301). The crypto bdev sits **below** the nvmf attach point — every
NVMe-oF read is decrypted server-side before bytes reach the wire. The DEK
comes from a server-side KMS and is installed entirely on the storage node
(`_create_crypto_lvol`, lines 31-73); no key material transits the CSI/client
path (`csi-driver/pkg/util/nvmf.go:206`, `controllerserver.go:767,833` only
pass a boolean flag). **Conclusion: the client always receives plaintext.**

**Empirical data test** (`vm04`, fresh 5.1G-pool VDO volume, same recipe as
earlier spikes):

```bash
# Realistic (not degenerate-zero) compressible dataset: real journal logs, repeated
for i in $(seq 1 2000); do journalctl --no-pager 2>/dev/null; done > /tmp/plaintext-source.txt
ls -lh /tmp/plaintext-source.txt   # 536M

cp /tmp/plaintext-source.txt /mnt/vdocrypt/plain.bin
sync
vdostats --verbose vdocrypt-vdopool-vpool | grep -E "data blocks used|logical blocks used"
# data blocks used    : 11072     (~43MB physical for 536MB logical -- ~12.4x reduction)
# logical blocks used : 170496

openssl enc -aes-256-ctr -pbkdf2 -pass pass:test123 -in /tmp/plaintext-source.txt -out /mnt/vdocrypt/cipher.bin
sync
vdostats --verbose vdocrypt-vdopool-vpool | grep -E "data blocks used|logical blocks used"
# data blocks used    : 148258    (148258 - 11072 = 137186 new blocks for ~536MB of ciphertext -- ~1:1, no savings)
# logical blocks used : 307669    (307669 - 170496 = 137173 new logical blocks, matches the ciphertext's size)
```

**Result**: the mechanical claim (ciphertext doesn't compress/dedup) is
correct in isolation — plaintext got a ~12.4x reduction, the AES-256-CTR
ciphertext of the *same* source data got essentially zero savings (~1:1).
But since the architecture confirms the client never receives ciphertext for
this feature, this failure mode doesn't occur in practice.
`encryption=true` + `client_compression=true` is a fully compatible
combination — an earlier draft of the design doc's compatibility review had
this wrong (assumed the client sees ciphertext) and has been corrected.

Cleaned up: unmounted, `vgremove`/`pvremove`/`losetup -d`, scratch files
removed, test pod deleted.

---

## 14. Clone/snapshot UUID collision, on a real simplyblock cluster (`vm04`)

Everything up to this point used loop devices standing in for "a connected
block device." This spike used a real simplyblock cluster: a real lvol, real
`sbctl` snapshot/clone, real NVMe-oF connect — no simulation.

**Connect to the real lvol** (commands provided directly, not derived):

```bash
nvme connect --reconnect-delay=2 --ctrl-loss-tmo=3600 --fast_io_fail_tmo=8 --nr-io-queues=3 \
  --keep-alive-tmo=4 --transport=tcp --traddr=192.168.10.114 --trsvcid=4428 \
  --nqn=nqn.2023-02.io.simplyblock:ee1c4f35-d232-4450-bcb0-28ba2faffacb:lvol:b7d433b7-fd53-4f73-b3d6-726c224b30e5
nvme connect --reconnect-delay=2 --ctrl-loss-tmo=3600 --fast_io_fail_tmo=8 --nr-io-queues=3 \
  --keep-alive-tmo=4 --transport=tcp --traddr=192.168.10.113 --trsvcid=4428 \
  --nqn=nqn.2023-02.io.simplyblock:ee1c4f35-d232-4450-bcb0-28ba2faffacb:lvol:b7d433b7-fd53-4f73-b3d6-726c224b30e5

nvme list-subsys
# nvme-subsys0 - NQN=nqn...:lvol:b7d433b7-fd53-4f73-b3d6-726c224b30e5
#  +- nvme0 tcp traddr=192.168.10.114,trsvcid=4428 live
#  +- nvme1 tcp traddr=192.168.10.113,trsvcid=4428 live

ls -la /dev/disk/by-id/ | grep nvme
# nvme-uuid.b7d433b7-fd53-4f73-b3d6-726c224b30e5 -> ../../nvme0n1   -- the stable path, exactly as designed
```

**Real VDO on the real device** (10GB lvol, comfortably above the ~4.72GiB floor):

```bash
DEV=/dev/disk/by-id/nvme-uuid.b7d433b7-fd53-4f73-b3d6-726c224b30e5
pvcreate $DEV
vgcreate vdo-b7d433b7-fd53-4f73-b3d6-726c224b30e5 $DEV
lvcreate --type vdo --config "activation{checks=0}" -n b7d433b7-fd53-4f73-b3d6-726c224b30e5 \
  -L 9.2G -V 8G vdo-b7d433b7-fd53-4f73-b3d6-726c224b30e5/vdopool --yes
mkfs.ext4 -q /dev/vdo-b7d433b7.../b7d433b7...
mount /dev/vdo-b7d433b7.../b7d433b7... /mnt/real-vdo
echo real-simplyblock-cluster-canary > /mnt/real-vdo/canary.txt
sha256sum /mnt/real-vdo/canary.txt
# 182140c83391eaa3b3cfbdf7fc9270e330d7fc44f4c506e2b783d8ab57ac379c

vdostats --human-readable
# Device                                        Size   Used  Available  Use%  Space saving%
# vdo-...-vdopool-vpool                         9.2G   3.2G       6.0G   35%            99%
```

Real, mountable, working VDO on an actual simplyblock NVMe-oF volume — the
entire loop-device recipe carried over with zero changes.

**The clone, and the architectural surprise**: expected each CSI volume to
get its own NQN/subsystem. Reality is different:

```bash
sbctl lvol clone-lvol b7d433b7-fd53-4f73-b3d6-726c224b30e5 lvol-clone1
# ... "Add BDev to subsystem" ... nqn: nqn...:lvol:b7d433b7-fd53-4f73-b3d6-726c224b30e5  (SAME NQN as the source!)
# result: clone id 63a83b95-87e5-4fb0-9bcf-75a6b7e0ad49

sbctl lvol connect 63a83b95-87e5-4fb0-9bcf-75a6b7e0ad49
# returns connect strings for nqn...:lvol:b7d433b7-fd53-4f73-b3d6-726c224b30e5 -- the ORIGINAL's NQN, not a new one
```

The clone is namespace 2 of the *same* subsystem, not a separate connection.
Since the subsystem's already connected, no new `nvme connect` is even
needed — just a rescan:

```bash
nvme ns-rescan /dev/nvme0
nvme list
# /dev/nvme0n1  ... b7d433b7-fd53-4f73-b3d6-726c224b30e5  0x1
# /dev/nvme0n2  ... b7d433b7-fd53-4f73-b3d6-726c224b30e5  0x2   -- the clone, same subsystem serial

ls -la /dev/disk/by-id/ | grep nvme
# lvm-pv-uuid-lb1F4P-r4dZ-72oc-Vze4-C5d7-ta50-5FVgKF -> ../../nvme0n2   -- udev ALREADY found an LVM
#   signature on the clone's raw bytes, before any LVM command was run on it
```

**The collision, live**:

```bash
blkid /dev/nvme0n1 /dev/nvme0n2
# /dev/nvme0n1: UUID="lb1F4P-r4dZ-72oc-Vze4-C5d7-ta50-5FVgKF" TYPE="LVM2_member"
# /dev/nvme0n2: UUID="lb1F4P-r4dZ-72oc-Vze4-C5d7-ta50-5FVgKF" TYPE="LVM2_member"   -- IDENTICAL

pvs -o+uuid
# only /dev/nvme0n1 listed -- nvme0n2 silently absent

pvscan --cache -vvv 2>&1 | grep -iE "duplicate|nvme0n2"
#   Found dev 259:4 /dev/nvme0n2 - new alias.
#   /dev/nvme0n2: Skipping (deviceid)

mount /dev/vdo-b7d433b7.../b7d433b7... /mnt/clone-attempt
ls /mnt/clone-attempt
# canary.txt  lost+found    -- the ORIGINAL's data, not independent clone content. No error. No warning.
```

Confirmed exactly as predicted: the clone is silently unreachable, and
anything using "the volume's own name" transparently gets the original's live
data instead — a silent-wrong-data risk, not a loud failure. Unmounted the
ambiguous mount (it was just the original, mounted twice).

**The fix**:

```bash
lvmdevices --adddev /dev/nvme0n2
#   WARNING: adding device /dev/nvme0n2 with PVID ... which is already used for /dev/nvme0n1 ...
#   Add device with duplicate PV to devices file?[n]
#   Device not added.                                    -- refuses by default

lvmdevices --adddev /dev/nvme0n2 -y                       -- force past the refusal

vgimportclone --basevgname vdo-63a83b95-87e5-4fb0-9bcf-75a6b7e0ad49 /dev/nvme0n2 -y
# (first attempt, before lvmdevices --adddev, failed: "Failed to find device /dev/nvme0n2")

pvs -o+uuid
# /dev/nvme0n1 vdo-b7d433b7-...  lb1F4P-r4dZ-72oc-Vze4-C5d7-ta50-5FVgKF
# /dev/nvme0n2 vdo-63a83b95-...  carIqJ-YEg5-G65L-lQRa-axSb-Az3X-vDJw28   -- distinct now
vgs -o+uuid
# vdo-b7d433b7-...  xiGYvM-q9gM-FbQC-mjCy-3mhT-FFud-vB09H3
# vdo-63a83b95-...  ASL30Y-CQPu-xs4M-7504-BmNN-b2dN-4gi2Gy                -- distinct VG names + UUIDs
```

**Caveat found**: `vgimportclone` only renames the VG — `lvs` afterward still
showed the LV itself named `b7d433b7-fd53-4f73-b3d6-726c224b30e5` (the
source's ID) inside the renamed VG. A real `ResolveClonedVDO` needs an
explicit `lvrename` too.

**Verify independence**:

```bash
vgchange -ay vdo-63a83b95-87e5-4fb0-9bcf-75a6b7e0ad49
mount /dev/vdo-63a83b95.../b7d433b7-fd53-4f73-b3d6-726c224b30e5 /mnt/clone-real
cat /mnt/clone-real/canary.txt
# real-simplyblock-cluster-canary   -- correctly inherited from the source at snapshot time

echo clone-only-data > /mnt/clone-real/clone-marker.txt
ls /mnt/real-vdo/     # canary.txt, lost+found                    -- original: unaffected
ls /mnt/clone-real/   # canary.txt, clone-marker.txt, lost+found  -- clone: genuinely independent now
```

Cleaned up: unmounted both, `vgremove` both VGs, `pvremove` both devices,
`lvmdevices --deldev` both entries. Real `sbctl` lvol/clone/snapshot objects
left in place at the time (deleted later, see §15/§16, once no longer needed).

---

## 15. Volume migration, on the real cluster (`vm04`)

Same real lvol from §14 (VDO rebuilt fresh, new canary). Goal: verify
migration is actually transparent to the client-side VDO/LVM layer, not just
assumed so from reading the operator's migration controller code.

**Non-batch migration is rejected** because the subsystem has 2 members (the
original + the clone from §14, still present at this point):

```bash
sbctl lvol migrate b7d433b7-fd53-4f73-b3d6-726c224b30e5 6ec0dc8a-454d-43bc-95dc-927bcc484f8d
# Error: LVol b7d433b7-... belongs to a shared NVMe-oF subsystem with 2 member(s)
#   (NQN=nqn...:lvol:b7d433b7-...). Use --batch to migrate the whole subsystem together.
```

**Batch migration**:

```bash
sbctl lvol migrate --batch b7d433b7-fd53-4f73-b3d6-726c224b30e5 6ec0dc8a-454d-43bc-95dc-927bcc484f8d
# Migration Group ID: ec12a9c4-f513-4010-9ee6-06d1622ab5aa
# new connect strings -- SAME NQN as before, new traddr=192.168.10.112:4430 and traddr=192.168.10.114:4430

nvme connect ... --traddr=192.168.10.112 --trsvcid=4430 --nqn=nqn...:lvol:b7d433b7-...
nvme connect ... --traddr=192.168.10.114 --trsvcid=4430 --nqn=nqn...:lvol:b7d433b7-...

nvme list-subsys
# nvme-subsys0 - NQN=nqn...:lvol:b7d433b7-...   (unchanged)
#  +- nvme0 traddr=192.168.10.114:4428 live   (original)
#  +- nvme1 traddr=192.168.10.113:4428 live   (original)
#  +- nvme2 traddr=192.168.10.112:4430 live   (new, migration target)
#  +- nvme3 traddr=192.168.10.114:4430 live   (new, migration target)

cat /mnt/migrate-test/migrate-canary.txt
# canary-before-migration   -- still intact, device identity unaffected by adding new paths
```

Confirmed: migration reuses the exact same NQN and just adds multipath legs
— identical mechanism to the reconnect/reactivate spike in §7, no new
VDO-side logic needed.

**Cutover stalled**: `sbctl lvol migrate-continue --batch ec12a9c4-...`
started the copy (`snap_copy` phase), but after ~10+ minutes of polling
`sbctl lvol migrate-list`, the snap counters (`0/1`, `0/0`) never advanced.
Not dead, though — `sbctl cluster list-tasks` showed the `lvol_migration`
tasks as `running` with an advancing `Updated At` timestamp, and
`sbctl cluster list` showed the cluster in `ACTIVE - REBALANCING`, plausibly
contending for the same task-worker/I/O resources on this small 3-node test
cluster. Decision made to stop waiting rather than keep polling — the
architectural questions that mattered (same NQN, additive multipath legs,
atomic batch enforcement) were already answered regardless of copy duration.
**Not observed**: the actual cutover moment (old paths going inaccessible,
new paths becoming primary).

Cleaned up: `sbctl lvol migrate-cancel --batch ec12a9c4-...` to stop the
stalled copy; unmounted the test filesystem; later (in §16, for unrelated
disk-space reasons) the clone, snapshot, and original lvol were all deleted
via `sbctl` — migration group, snapshot, clone, and lvol no longer exist on
the cluster.

---

## 16. Isolating compression vs. deduplication cost (`vm04` + real cluster)

Every prior spike in this log used plain `lvcreate --type vdo` with no flags,
meaning **both compression and dedup were on by default in all of them** — the
two costs were never actually separated until this round.

**Prerequisite hiccup**: `vm04` had only 4.6G free (below the ~4.72GiB VDO
floor) at the start of this round. Root cause was NOT leftover scratch
files (`/tmp` was clean) or the real lvol/clone/snapshot from §"real cluster"
testing (deleting those via `sbctl` freed zero space — their actual physical
footprint was tiny/thin-provisioned) — it was `/var/lib/rancher/k3s/agent`
(19G of containerd image/snapshot data). `k3s crictl rmi --prune` freed ~10G
of genuinely dangling/untagged image layers safely, without touching anything
currently running.

**Flag syntax confirmed** (`man lvmvdo`, `lvcreate --help`):
```bash
lvcreate --type vdo --compression y|n --deduplication y|n -n LV -L Size -V VSize VG/vdopool
lvchange --compression y|n --deduplication y|n VG/vdopool   # live toggle, no recreate needed
```

**Four-way isolation** (fresh loop device reused sequentially, ~1GB test data
each time — 500MB of real repeated journal-log text plus an exact duplicate
copy, to exercise both internal compressibility and cross-block duplication):

```bash
# Variant A: compression=y deduplication=n
lvcreate --type vdo --compression y --deduplication n -n testlv -L 5.1G -V 4G vdotest/vdopool --yes
# ... mkfs, mount, cp testdata-source.txt base.bin, cp testdata-source.txt copy.bin, sync
vdostats --verbose vdotest-vdopool-vpool
#   data blocks used: 74017   overhead blocks used: 813959   KVDO module bytes used: 182285032

# Variant B: compression=n deduplication=y
#   data blocks used: 116733  overhead blocks used: 813943   KVDO module bytes used: 408960872

# Variant C: compression=y deduplication=y (both)
#   data blocks used: 34014   overhead blocks used: 813949   KVDO module bytes used: 408960872

# Variant D: compression=n deduplication=n (neither)
#   data blocks used: 254299  overhead blocks used: 813944   KVDO module bytes used: 182285032
```

**First run of Variant A was contaminated** — measured 592358216 bytes
(~565MB), higher than expected. `dmsetup ls`/`lsmod` (`kvdo` usage count 2)
revealed a leftover orphaned VDO stack from the earlier real-cluster
migration spike (`vdo-b7d433b7...`, VG metadata already gone since the
underlying lvol had since been deleted server-side, but the dm-mapper
entries were still active in the kernel). `vgremove` failed ("VG not found");
cleared with direct `dmsetup remove` on the three orphaned devices in
dependency order. Re-measured cleanly afterward. **Lesson**: `KVDO module
bytes used` being module-wide (confirmed in §9) means any leftover VDO
instance anywhere on the node silently pollutes every subsequent
measurement — always verify `lsmod | grep vdo`'s usage count matches
expectations before trusting a reading.

**Result**: RAM cost tracks deduplication only (182MB dedup-off regardless of
compression, 390MB dedup-on regardless of compression) — compression adds no
measurable memory overhead. Best space savings need both together (34,014
data blocks vs. 74,017 compression-only vs. 116,733 dedup-only).

**Re-verified against the real cluster**: created a fresh real lvol
(`sbctl lvol add`), connected via NVMe-oF, `lvcreate --type vdo --compression
y --deduplication n` on the real device → 182,285,032-range reading, then
`lvchange --deduplication y vdo-.../vdopool` **live, without recreating** →
410,075,376 bytes, matching the loop-device dedup figure almost exactly.
Confirms both the independent-flags mechanism and the live-toggle mechanism
work identically on real NVMe-oF-backed storage.

Cleaned up: loop-device VGs/PVs removed between each variant; real lvol
deleted via `sbctl lvol delete` at the end; test pod deleted.

---

## Open items for future spikes

1. **Growth at/near the minimum-size floor**: does `lvextend` behave
   differently when starting right at the ~4.72GiB floor rather than
   comfortably above it?
2. **Overhead-ratio scaling**: does the fixed VDO overhead (Section 7 of the
   design doc: ~390MB RAM, ~3.5GB disk at minimum size) improve meaningfully
   as a percentage at realistic platform PVC sizes (only two data points
   gathered so far — 5.5G and 7.5G)?
3. **Performance re-measurement on real NVMe-oF hardware**: §10's ~8-11x write
   overhead was measured on a loop device in a VM with `vdo_write_policy=auto`
   (likely resolving to `sync`) — needs re-validation against real
   NVMe-oF-backed storage, where `async` write policy may substantially
   change the picture, before treating the magnitude as production-representative.
4. **Genuinely reproducing an interrupted `lvcreate --type vdo`**: §11 was
   inconclusive — worth a different technique (e.g. throttling at the
   loop-file/filesystem layer instead of a dm-delay wrapper, or testing on
   slower/real storage where the operation naturally takes longer).
5. **Full CSI-integrated e2e**: once the code in the design doc exists, a real
   `client_compression=true` PVC through the actual StorageClass → topology
   gate → `NodeStageVolume` path, including a real (not synthetic-loop-device)
   NVMe-oF path-loss/reconnect exercise.
