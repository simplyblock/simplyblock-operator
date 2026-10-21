// Every reason a discovery run leaves a worker or a device out of the drafted
// ClusterDeploymentConfig, fails, or has its CDC refused afterwards, with how to
// fix each. It lives with the runbooks because administrators read it while a
// deployment is stuck. Build: typst compile discovery-rejections.typ

#set document(title: "CDC generation rejections", author: "simplyblock")
#set page(
  paper: "a4",
  flipped: true,
  margin: (x: 15mm, top: 13mm, bottom: 15mm),
  footer: context [
    #set text(7pt, fill: gray)
    simplyblock operator: CDC generation rejections
    #h(1fr)
    page #counter(page).display()
  ],
)
#set text(font: ("Helvetica Neue", "Helvetica", "Arial"), size: 9pt, lang: "en", region: "us")
#set par(leading: 0.55em)
#set heading(numbering: "1.1")
#show heading.where(level: 1): set text(14pt)
#show heading.where(level: 2): set text(11pt)
#show raw: set text(font: ("Menlo", "DejaVu Sans Mono"), size: 0.92em)

#let navy = rgb("#27384a")
#let note(body) = block(
  fill: rgb("#fdf8e6"), stroke: 0.6pt + rgb("#c9a227"), inset: 6pt, radius: 2pt, width: 100%, body,
)
// grid renders a reference table: a dark header row and banded body rows.
#let grid(widths, header, ..cells) = {
  set text(7.6pt)
  set par(leading: 0.45em)
  table(
    columns: widths,
    stroke: 0.3pt + rgb("#b8c0c8"),
    inset: 4pt,
    fill: (_, y) => if y == 0 { navy } else if calc.even(y) { rgb("#f1f4f7") } else { white },
    table.header(..header.map(h => text(fill: white, weight: "bold", h))),
    ..cells,
  )
}

#align(center, text(18pt, weight: "bold")[ClusterDeploymentConfig generation: rejections and fixes])

Every reason a discovery run (`OperatorOps` with `action: Discover`) leaves a worker or a device out of
the drafted ClusterDeploymentConfig (CDC), fails the run, or a CDC is refused afterwards by validation or
admission. The source of truth is `atlas-lib/blockdev/candidate.go` (probe reasons),
`operator/internal/discovery` (rules), `operator/internal/controllers/deployment` (run and validation), and
`operator/internal/webhook` (admission). Update this file when any of those changes.

= Subsystems

A run goes through three steps: *Inspecting* (pick workers), *Probing* (one probe Job per worker writes a
report ConfigMap), and *Writing* (apply the rules, draft the CDC). The rejections below are grouped by the
subsystem that makes them, in the order a run meets them. A device or a worker carries the first refusal
that applies, except in the node probe, which records every reason it finds.

#grid((1.3fr, 1.9fr, 1.8fr, 1.6fr, 0.5fr), ([Subsystem], [Code], [Rejects], [Reported as], [Section]),
  [Discovery run controller], [`operator/internal/controllers/deployment/operatorops_controller.go`],
  [The whole run, and workers that are not usable], [OperatorOps `status.message`, events `OperationFailed`, `WorkerDeclined`, `DeviceInspectionFailed`], [3],
  [Node probe], [`atlas-lib/blockdev/candidate.go`, run by `operator/internal/nodeprobe`],
  [Devices that are not free on the machine], [probe ConfigMap `rejections[]`; events `DeviceDeclined` by rule `available`], [4],
  [Discovery rules], [`operator/internal/discovery` (`rules.go`, `simplyblock.go`, `iscsi.go`, `netalign.go`, `placement.go`, `template.go`)],
  [Devices this run does not want, workers left with no devices, workers off the shared networks, devices on a second NUMA node, drafts with no journal disk], [events `DeviceDeclined`, `WorkerDeclined`, `ConfigRefused`; the run's summary message], [5],
  [CDC validation controller], [`operator/internal/controllers/deployment/validation.go`, `topology.go`, `erasurecoding.go`, `expansion.go`],
  [Draft or approved documents that cannot be deployed], [CDC `status.phase` Draft or Failed, `status.message`, Warning events], [6],
  [Admission], [`operator/internal/webhook/clusterdeploymentconfig_validator.go`, `operatorops_validator.go`, CRD CEL rules],
  [API writes], [the `kubectl` request is denied], [7],
)

= Where to read the result

#grid((1.2fr, 2.2fr), ([What], [Command]),
  [Run phase and summary message],
  [`kubectl get operatorops -n NS`, then `kubectl get operatorops RUN -n NS -o jsonpath='{.status.message}'`],
  [Every refusal, one event each (reasons `DeviceDeclined`, `WorkerDeclined`, `DeviceInspectionFailed`, `ConfigRefused`, and so on)],
  [`kubectl describe operatorops RUN -n NS`, or `kubectl get events -n NS --field-selector involvedObject.kind=OperatorOps,involvedObject.name=RUN`],
  [Probe reports with the full detail of each rejection (`devices[].rejections[].reason` and `.detail`)],
  [`kubectl get cm -n NS -l storage.simplyblock.io/nodeprobe-run=RUN -o yaml`],
  [Probe Jobs and their logs (TTL 1 h, `backoffLimit` 2, deadline 300 s)],
  [`kubectl get jobs,pods -n NS -l storage.simplyblock.io/nodeprobe-run=RUN`, then `kubectl logs`],
  [CDC validation (`status.phase` Draft or Failed, `status.message`, Warning events)],
  [`kubectl get cdc -n NS` and `kubectl describe cdc NAME -n NS`],
)

#note[The `DeviceDeclined` event carries only the reason code, for example `declined by available because the
probe refused it: NotBlank`. The detail (which mountpoint, which holder, which signature at which offset) is
only in the probe report ConfigMap.]

#pagebreak()
= Discovery run controller

`operator/internal/controllers/deployment/operatorops_controller.go` drives the run. It fails the run when nothing can be drafted, and leaves out workers it cannot use before any probe runs.

== Run failures (OperatorOps goes to Failed)

#grid((1fr, 2fr, 1.6fr, 1.7fr), ([Failure], [Message], [When], [Fix]),
  [Unknown action], [`action %q is not one this operator runs`], [`spec.action` is not `Discover`.], [Set `action: Discover`.],
  [No probe image], [`no probe image: set <env> on the operator's deployment ...`],
  [The probe image environment variable is missing on the manager Deployment.], [Set it, normally through the Helm chart.],
  [Unreadable size range], [`spec.discover.deviceFilter.driveSizeRange is %q, which cannot be read` or `the size range %q counts backward`],
  [Admission webhook, and the controller before probing and before writing.],
  [Use `100G-2T`, `500G-`, `-2T`, or `1920G` (exact). Units T, G, M; a bare number is G.],
  [No free worker], [`no worker is free: every node either carries a StorageNode already, is unschedulable, is reserved for the control plane ... or does not match the run's selector`],
  [Nothing is left after `workers` or `nodeSelector`, after dropping nodes that already run a StorageNode, and after the usability check.],
  [Uncordon or untaint nodes, fix `workers` or `nodeSelector`, or set `enableControlPlaneNodes`. Tolerations do not bypass the taint check.],
  [All probes failed], [`no worker reported: every one of the %d probe Jobs failed`],
  [Every probe Job used up its retries without writing a report.],
  [Read the probe pod logs: image pull, ServiceAccount, privileges or SCC on OpenShift, host paths.],
  [Reports gone], [`the probe reports are gone, so there is nothing to write`],
  [The ConfigMaps were deleted between Probing and Writing.], [Run discovery again.],
  [No usable device anywhere], [`no worker has a device this run would use: ...` followed by counts per rule, for example `4 device(s) across 4 worker(s), none usable: 4 declined by available: the probe refused it: NotBlank`],
  [The plan produced no node set. The summary leaves out pre-filter refusals (`whole disk`, `device class`).],
  [Fix per rule, sections 4 and 5. The events and the probe reports name each device.],
  [Step deadline (`StepDeadlineExceeded`)], [`the cluster could not be read within 5m`, `the probes did not all finish within 15m ...`, or `the draft could not be written within 5m`],
  [A step ran past its budget.], [Check API health. For Probing, find the stuck probe pod (missing toleration, image pull, PSA or SCC).],
  [Run cannot resume], [`the run cannot be resumed: %v`, and graph errors],
  [The status is corrupted or comes from an incompatible operator.], [Delete the OperatorOps and run again.],
)

== Worker selection (the run continues without the worker)

#grid((1.1fr, 2fr, 1.6fr, 1.5fr), ([Refusal], [Message], [When], [Fix]),
  [Cordoned or tainted (`WorkerDeclined`)], [`%s is not used: it is cordoned`, `it carries the taint K=V:Effect`, or `it is not schedulable`],
  [`spec.unschedulable` is set, or the node has any NoSchedule or NoExecute taint.], [`kubectl uncordon`, remove the taint. Tolerations only let the probe run.],
  [Control-plane or infrastructure node (`WorkerDeclined`)], [`%s is not used: it is <role>; set spec.discover.enableControlPlaneNodes to include it`],
  [The node has a control-plane or infrastructure role.], [Set `enableControlPlaneNodes: true` (emits `ControlPlaneNodeIncluded`).],
  [Unknown worker name (`WorkerDeclined`, Warning)], [`%s is named in spec.discover.workers and there is no node by that name`],
  [Typo in `spec.discover.workers`.], [Fix the name.],
  [Already a StorageNode, or not selected], [(no event)],
  [A StorageNode already runs on the node, or it does not match `nodeSelector`.], [Intended. Remove the StorageNode or adjust the selector.],
  [Probe failed (`DeviceInspectionFailed`)], [`the probe on %s failed, so it is not in the draft`],
  [That worker's Job failed three times.], [`kubectl logs` of the probe pod, fix, run again.],
)

#pagebreak()
= Node probe

`atlas-lib/blockdev/candidate.go`, run on each worker by the probe Job (`operator/internal/nodeprobe`). It answers whether a device is free on the machine, independent of what the run asked for.

The probe checks, in order: kind, transport, size, removable, read-only, mounts, swap, holders, exclusive
open, and partitions. Only a device that passed all of these, or failed only on partitions, is opened and its
first bytes read. A device can carry several reasons. In events they show as
`declined by available because the probe refused it: Reason, Reason`. On OpenShift, run the host commands
below through `oc debug node/NODE` and then `chroot /host`.

#grid((0.9fr, 1.7fr, 1.3fr, 2.3fr), ([Reason], [When], [Detail in the report], [Fix]),
  [`NotAWholeDisk`], [Partition, device-mapper node, md array, loop, RAM disk, nbd.], [`the device is a <kind>`],
  [Nothing to fix; these are never candidates. Use the underlying whole disk.],
  [`FabricNamespace`], [An NVMe namespace reached over a fabric: a simplyblock or other remote volume attached to this node.],
  [`the namespace is reached over a fabric ...`], [Nothing; it is not local storage. `nvme disconnect` it if it is stale.],
  [`UnknownTransport`], [A whole disk on a bus the scan cannot name.], [`the device sits on no bus this scan could name`],
  [Not fixable by configuration. Report the device (driver, sysfs path) so support can be added.],
  [`NoCapacity`], [Size 0: unbacked loop, empty card reader, unused nbd.], [`the device reports a size of zero`], [Nothing.],
  [`Removable`], [The kernel flags it removable: optical drive, USB stick, card reader.], [`the kernel reports the device as removable ...`],
  [Not usable as backend storage. Some hypervisors expose disks as removable; change the virtual disk type.],
  [`ReadOnly`], [The kernel presents it read-only.], [`the kernel presents the device read-only`],
  [Make it writable in the hypervisor or controller, or `blockdev --setrw /dev/X`.],
  [`Mounted`], [The disk or one of its partitions is mounted.], [`mounted at [...]`],
  [`umount`, and remove it from `/etc/fstab` and systemd mount units. Never for the boot disk.],
  [`SwapArea`], [The disk or a partition is active swap.], [`the device is an active swap area`], [`swapoff /dev/X`, and remove it from `/etc/fstab`.],
  [`Stacked`], [Another device is built on it: an active LVM logical volume, an md RAID member, dm-crypt, a multipath map.], [`held by [dm-0]`],
  [Find the holder: `ls /sys/block/X/holders`, `dmsetup ls`. LVM: `vgchange -an VG`, `lvremove`, `vgremove`, `pvremove /dev/X`. md: `mdadm --stop`, `mdadm --zero-superblock /dev/X`. dm-crypt: `cryptsetup close`. Multipath: blacklist the disk.],
  [`Busy`], [The kernel refused an exclusive open, and mounts, swap, and holders do not explain it.], [`the kernel refused an exclusive open, and gives no reason`],
  [Find the opener: `fuser -vm /dev/X`, `lsof /dev/X`, multipathd, a VM, an iSCSI target. On the boot disk it appears together with `Mounted`.],
  [`Unreadable`], [The exclusive-open check could not be made, the content read failed, or the reading came back empty.],
  [`the kernel could not be asked: ...` or `the device could not be read: ...`],
  [Check the probe pod's privileges and logs, and `dmesg` for I/O errors. Run discovery again.],
  [`Partitioned`], [Partitions in sysfs, or a GPT or MBR table in the first bytes.], [`the device carries the partitions [...]` or `GPT header at ...`],
  [`wipefs -a /dev/X` (and `sgdisk --zap-all /dev/X` for the backup GPT). If the table is stale and this is the only reason: `spec.discover.enablePartitionedDevices: true`.],
  [`LVMPhysicalVolume`], [An LVM physical-volume label with no active logical volume, so `lsblk` shows a bare disk while `pvs` and `pvdisplay` list it. Before this reason existed it was reported as `NotBlank`.],
  [`LVM2 physical-volume label at 512`],
  [Confirm with `pvs /dev/X`. If it belongs to a volume group: `vgremove VG` (or `vgreduce VG /dev/X`). Then `pvremove /dev/X`. On RHEL-based hosts also drop it from the devices file: `lvmdevices --deldev /dev/X`.],
  [`NotBlank`], [A filesystem (ext2/3/4, XFS, Btrfs, FAT, exFAT), a swap signature, LUKS, md, ZFS, bcache, or unrecognized bytes.],
  [The signature and its offset, for example `XFS superblock at 0`],
  [After confirming the data is not needed: `wipefs -a /dev/X`. md: `mdadm --zero-superblock /dev/X`. ZFS: `zpool labelclear -f /dev/X`.],
)

#note[Not rejections: a disk whose signature `wipefs` erased is reported as released and admitted. A disk carrying
a simplyblock (alceml) superblock from an earlier deployment is admitted, and the report says so. Wipe it
first if the old data must not be picked up.]

#pagebreak()
= Discovery rules

`operator/internal/discovery` decides, from the probe reports, which free devices and which workers go into the draft. These refusals follow the run's own configuration rather than the state of the machine.

== Device rules

Applied in this order; a device carries only the first refusal. Pre-filter refusals are sent as events but
left out of the run's summary message.

#grid((0.9fr, 2fr, 1.8fr, 1.5fr), ([Rule], [Message], [When], [Fix]),
  [`whole disk` (pre-filter)], [`it is a <kind> rather than a whole disk`],
  [Not a whole disk. The block class also accepts partitions.], [Use whole disks.],
  [`simplyblock volume`], [`it is a simplyblock logical volume of cluster %s, which this fleet already serves`],
  [The device's subsystem NQN is a simplyblock volume.], [Nothing. `nvme disconnect` it if it is stale.],
  [`device class` (pre-filter)], [`this run scans NVMe devices and the device is on %s`, `... which is a volume something else exported ...`, or `it has no PCI address to name it by`],
  [NVMe class (the default) and the disk is virtio, SCSI, SATA, and so on; or block class and the disk is a fabric namespace.],
  [For non-NVMe disks set `spec.discover.enableLogicalBlockDevices: true`.],
  [`available`], [`the probe refused it: Reason, ...` or `the probe did not report it as available and gave no reason`],
  [The probe gave the device one or more reasons (section 4).], [Fix per reason. `enablePartitionedDevices` waives `Partitioned` only when it is the sole reason.],
  [`iSCSI`], [`it is an iSCSI LUN ... so a run takes one only where the allow list names it and this one names %s`],
  [An iSCSI LUN not named in the allow list.], [Add it to `deviceFilter.blockAllowList` (block class) or `pcieAllowList`.],
  [`allow and deny lists`], [`it is in the deny list` or `it is not in the allow list`],
  [`deviceFilter.pcieAllowList` and `pcieDenyList` (NVMe, PCI address), or `blockAllowList` and `blockDenyList` (block, kernel or `/dev/disk/...` path).], [Fix the lists.],
  [`model`], [`its model %q does not contain %q`], [`deviceFilter.pcieModel` is set (NVMe class); case-insensitive substring.], [Fix or remove `pcieModel`.],
  [`size range`], [`it is %s and the range %s starts above it` or `... ends below it`], [Outside `deviceFilter.driveSizeRange` (inclusive).], [Widen the range.],
)


== Worker rules (`rules.go`)

A worker is dropped when nothing of it is left after the device rules.

#grid((1.1fr, 2fr, 1.6fr, 1.5fr), ([Refusal], [Message], [When], [Fix]),
  [`has devices`], [`no device of it survived the device rules`],
  [Every device of the worker was refused.], [Read the device refusals of that worker (section 4).],
  [`has devices`, NVMe on vfio, in use], [`it presents no usable block device, and %d of its NVMe controllers (%s) are bound to a userspace driver and in use ...`],
  [The controllers are bound to `vfio-pci` or `uio_pci_generic` and something drives them (an old SPDK, a VM passthrough).], [Stop whatever drives them, then run again.],
  [`has devices`, NVMe on vfio, unknown], [`... whether anything is driving %d of its NVMe controllers (%s) could not be established, so they are not offered`],
  [The probe could not check the controllers.], [Check the probe logs and privileges; check the controllers by hand.],
  [`has devices`, NVMe on vfio, free], [`... are bound to a userspace driver and nothing is using them, so the disks are there to be reclaimed`],
  [Shown only when they were not picked up as claimable.], [Normally added automatically on the NVMe class; otherwise unbind them from `vfio-pci`.],
  [`fully readable` (off by default)], [`the probe could not read %d of its readings: ...`],
  [Only with the `WorkerWasReadable` rule enabled.], [Fix what the probe could not read (see the probe logs).],
)

== Network alignment (`netalign.go`)

Workers must share the management network and the data networks most of the fleet is on.

#grid((1.1fr, 2fr, 1.6fr, 1.5fr), ([Refusal], [Message], [When], [Fix]),
  [`management network`], [`%s is on %s, and the fleet's management network is %s, held by %d of its %d workers`],
  [The worker's management interface is not on the network most workers share.], [Move the worker onto that network, or leave it out of the run.],
  [`data network`], [`it holds no data interface on %s, and the fleet's data networks are %s, held by %d of its %d workers`],
  [The worker lacks an interface on one of the shared data networks.], [Configure the missing interface.],
)

== NUMA placement (`placement.go`)

One NUMA node per worker is drafted; devices on the others are declined.

#grid((1.1fr, 2fr, 1.6fr, 1.5fr), ([Refusal], [Message], [When], [Fix]),
  [`most available NUMA node`], [`no memory node of it carries an unclaimed device`; per device: `NUMA node %d carries %d unclaimed devices ... so it was chosen`],
  [Devices sit on more than one NUMA node, and only the best node's devices are drafted.], [By design. Edit the draft by hand to use the others.],
)

== Draft template (`template.go`)

Refusals while the document is written. The run fails with `no document could be drafted: ...` and a `ConfigRefused` event.

#grid((1fr, 2fr, 1.6fr, 1.7fr), ([Failure], [Message], [When], [Fix]),
  [Block class, no journal disk (`ConfigRefused`)], [`%s hands over no disk that could carry the journal, and a block cluster has no other journal layout ... Give the worker a second disk, or deploy it as nvme`],
  [Block class, and a worker has fewer than two disks or its disks report no size.], [Add a second disk, or run with the NVMe class.],
  [Block class, journal tie (`ConfigRefused`)], [`%s hands over no disk smaller than its others, so nothing says which one carries the journal ...`],
  [Block class, and the smallest disks of a worker are the same size.], [Set `spec.discover.forceJournalDevice: true`.],
)

#pagebreak()
= CDC validation controller

Checked on every reconcile. A draft stays `Draft` with the first finding in `status.message` (plus
`and N other problems`); an approved CDC moves to `Failed`. Every finding is also a Warning event with the
reason below.

#grid((1.2fr, 3fr, 1.6fr), ([Reason], [Message], [Fix]),
  [`WorkerNotFound`], [`%s is not a node of this Kubernetes cluster`], [Fix the group's `workers`.],
  [`WorkerNotFound`], [`%s appears in more than one group ...`], [Put each worker into one group.],
  [`WorkerNotFound`], [`the groups name different management interfaces (%s) ...` (the same for data interfaces)], [Use the same interface names in every group.],
  [`NoManagementInterface`], [`group(s) %s name no management interface ...`], [Set `mgmtInterface`.],
  [`DeviceNotFound`], [`group %s names no devices ...`], [Add devices to the group.],
  [`DeviceNotFound`], [`the cluster has failure domains enabled and group %s declares none ...`], [Set the group's failure domain.],
  [`DeviceClassMismatch`], [`the groups name %s devices and cluster %s is built out of %s ...`], [Use the class of the cluster named in `clusterRef`.],
  [`JournalDeviceRequired`], [`the groups name logical block devices, and the control plane runs those only with the journal on a device of its own ...`], [Set `spec.cluster.enableJournalDevice: true`.],
  [`TooManyFailureDomains`], [`the document would bring the cluster to %d failure domains, and a cluster maps at most %d`], [Use fewer failure domains.],
  [`MemberTooLarge`], [`node set %s makes %d storage nodes, and a cluster holds at most %d ...` or `the document adds %d storage nodes to cluster %s ...`], [Split into smaller node sets (supercluster members), or add fewer nodes.],
  [`MemberNameInvalid`], [`node set %s makes a member named like %s, which a StorageCluster cannot be named ...`], [Shorten the node set or cluster name.],
  [`MemberSizingUnstated`], [`spec.clusterRef names supercluster %s, and the members the document adds have no sizing ...`], [Add `spec.cluster`.],
  [`MemberSpansRegions`], [`node set %s lists workers in regions %s, and a member stays within one region`], [Group workers by region.],
  [`MemberMissingFailureDomain`], [`node set %s holds no worker in failure domain %s, which another node set of its region holds ...`], [Rebalance workers across node sets.],
  [`StripeUnsupported`], [`%s is %s, which is not a scheme simplyblock supports ...`], [Pick a supported erasure-coding scheme.],
  [`StripeBelowMinimumNodes`, `...Workers`, `...Devices`], [`%s is %s, which needs at least %d storage node(s)`, or the same for workers and devices], [Add nodes, workers, or devices, or choose a smaller stripe.],
  [`StripeSingleNodeCount`], [`the cluster is a single-node cluster (clusterMode Single), which runs exactly one storage node ...`], [Exactly one node in single mode.],
  [`ClusterExists`, `ClusterNotFound` (after approval)], [The cluster to create already exists, or the one in `clusterRef` is missing.], [Use `clusterRef` for an existing cluster; fix the name.],
)

= Admission (webhook and CRD CEL rules)

#grid((1fr, 3.4fr, 1.4fr), ([Rule], [Message], [Fix]),
  [Approval check], [`this document cannot be approved, and approving it is what makes it immutable: <problems>`. Problems: an unknown worker; another approved CDC creates the same cluster; neither `spec.cluster` nor `spec.clusterRef`; `clusterRef` does not exist; `spec.cluster.name` already exists; a class mismatch.],
  [Fix each listed problem, then approve.],
  [After approval], [`spec.approved cannot be withdrawn` or `spec is immutable once spec.approved is true ...`], [Write a second CDC that names the cluster in `spec.clusterRef`.],
  [CDC CEL rules], [NVMe addresses or block devices, not both; every group of a node set uses the same class; `enableArbitration` and `arbitration` only with `clusterMode: Dual`; node-set overrides need more than one node set or a `clusterRef`; `spec.cluster.openshift` needs `spec.environment: OpenShift`.],
  [Fix the document.],
  [OperatorOps CEL rules], [PCI filters cannot be combined with `enableLogicalBlockDevices`; `blockAllowList` and `blockDenyList` require it; `workers` and `nodeSelector` are exclusive.],
  [Use the filters of the chosen class; state one worker selection.],
)

= `spec.discover` reference

#grid((1.4fr, 4fr), ([Field], [Effect]),
  [`configName`], [Name of the CDC to write (default: generated).],
  [`workers`, `nodeSelector`], [Which nodes to probe; mutually exclusive.],
  [`tolerations`], [Lets the probe Job run on tainted nodes and is copied into the draft. Does not make a tainted node usable.],
  [`enableControlPlaneNodes`], [Includes control-plane and infrastructure nodes.],
  [`failureDomainLabel`], [Node label read as the failure domain.],
  [`enableLogicalBlockDevices`], [Block class: devices named by `/dev/disk/...` path instead of NVMe PCI address. Needed for virtio, SCSI, and SATA disks.],
  [`enablePartitionedDevices`], [Waives `Partitioned` when it is a device's only reason.],
  [`forceJournalDevice`], [Dedicates one of several equal smallest disks to the journal.],
  [`deviceFilter.pcieAllowList`, `pcieDenyList`, `pcieModel`], [NVMe class filters.],
  [`deviceFilter.blockAllowList`, `blockDenyList`], [Block class filters; match the kernel or the stable path.],
  [`deviceFilter.driveSizeRange`], [Inclusive size range, for example `500G-4T`.],
  [`clusterRef`], [The draft adds nodes to an existing cluster.],
)
