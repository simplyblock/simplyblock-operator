Good Cases:
  - Cluster create with device_mode=lblk (and confirm nvme mode is unaffected — regression check)
  - Device selection via each mode: no selector (auto-select all eligible whole disks), --blk-names, --blk-names-exclude, --blk-serials
  - Partition-based selection: pre-partitioned disk, journal auto-split via --jm-percent
  - Exactly the minimum device count (2) and above (3+, 4+, uneven sizes — smallest auto-becomes JM)
  - Single-node (non-HA) and multi-node HA lblk clusters
  - Full CSI E2E lifecycle: lvol lifecycle on an lblk cluster: create, snapshot, clone, resize, delete
  - Planned node restart — device re-identified by serial, cluster stays healthy
  - Cluster expansion — add a new lblk node to an already-active lblk cluster
  - --force/--force-format reuse of a previously-partitioned disk
  - /blockdevices inventory accuracy (eligibility flags, whole disks + partitions)

Error Cases:
  - Missing/typo'd device name or serial → clean rejection, no partial state
  - Requested device busy (mounted, LVM/md/dm-crypt held, backs root fs) → rejected with reason
  - Fewer than minimum devices selected → rejected
  - --lblk combined with any NVMe selection flag → rejected
  - Duplicate serials among selected devices (we hit this ourselves) → clean rejection
  - Disk and one of its own partitions both selected → rejected
  - Partitioned disk selected without --force/--force-format → rejected
  - Device disappears after node is online (unplugged / NVMe-oF path drops) → device_remove fires, node degrades gracefully
  - Hung IO on an AIO device (stalled backing store) → watchdog trips (qd-sampling/iostat), ≥2 stalled devices → node auto-restart
  - SPDK process crash/zombie mid add-node → cleanup + retry succeeds, doesn't squat hugepages
  - add-node retry leaves JM-mesh holes → fresh activation blocks, re-activation (recovery) still proceeds
  - Journal device (or journal partition) fails → node/cluster behavior on JM loss
  - Kernel renames a device across reboot (sdb→sdc) → serial-based resolution still finds it
  - Attempting to add an NVMe-selected node to an already-lblk-mode cluster, or vice versa → rejected
  - Operator-specific: editing blkNames on an already-provisioned StorageNodeSet/node → should be rejected, not silently reconfigure a live node
  (currently unenforced — a real gap I flagged, not yet fixed)
  - Selected device smaller than the journal floor (2 GiB) → clear rejection, not a broken journal

Load / Scale Cases:
  - The dual-node-outage churn soak sbcli already ran (graceful/forced/container-kill/host-reboot/network-partition) — worth re-running
  independently once the operator sits in front of it, since we've only verified the operator's wiring, not its behavior under churn
  - Many concurrent lvol create/delete cycles (20–50) on an lblk cluster
  - Sustained fio throughput/IOPS on lblk vs NVMe-backed lvols — quantify the AIO-vs-userspace-NVMe-driver overhead
  - High device count per node (8, 16 disks) and high partition count per disk
  - Large lblk fleet (10+ nodes), add-node concurrency tuning (maxParallelNodeAdds)
  - Repeated add/remove/re-add churn — watch for stale device-identity leaks in the DB
  - Long uptime soak — AIO bdevs lack some of bdev_nvme's lifecycle hooks, so fd/memory leak risk is real and undertested


Compatibility / Environment Cases:
  - Different backing media: local SATA/SAS SSD, virtio-blk, cloud EBS-like volume, iSCSI LUN, NVMe-oF volume (what we used)
  - GPT vs MBR partition tables

Operator/K8s-specific cases:
  - StorageCluster.spec.deviceMode immutability enforced at admission
  - Fleet-default + per-node-override merge correctness for blkNames/blkSerials/enableLblk
  - Mutual-exclusivity webhook (lblk selectors vs PCIe selectors) at CR-admission time instead of today's pod CrashLoopBackOff
