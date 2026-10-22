// Support runbook: every reason a discovery run leaves a worker or a disk out of
// the drafted ClusterDeploymentConfig, fails, or has its document refused, with
// when it happens and how to get past it. It lives with the runbooks because the
// support team reads it while a customer's deployment is stuck.
//
// Build: typst compile discovery-rejections.typ (the first build downloads the
// fletcher package from Typst Universe, which draws the flowchart).
//
// Keep it in step with the code that produces these messages:
// atlas-lib/blockdev/candidate.go (disk inspection), operator/internal/discovery
// (filters, network, NUMA, journal), operator/internal/controllers/deployment
// (the run and document checks), and operator/internal/webhook (admission).
// The document is written for support staff, so it names no code locations.

#import "@preview/fletcher:0.5.8" as fletcher: diagram, node, edge

#set document(title: "Deployment discovery rejections", author: "simplyblock")
#set page(
  paper: "a4",
  flipped: true,
  margin: (x: 15mm, top: 13mm, bottom: 15mm),
  footer: context [
    #set text(7pt, fill: gray)
    simplyblock support: deployment discovery rejections
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
#let cols = (1.1fr, 1.9fr, 1.7fr, 2.2fr)
#let head = ([Rejection], [What the customer sees], [When it happens], [How to resolve])

#align(center, text(18pt, weight: "bold")[Deployment discovery: rejections and how to resolve them])

A customer deploys simplyblock by running *discovery*: an `OperatorOps` resource with `action: Discover`.
Discovery inspects the Kubernetes workers and their disks and writes a draft *ClusterDeploymentConfig*
(CDC). The customer reviews the draft and approves it. This document lists every reason discovery leaves a
worker or a disk out of the draft, fails altogether, or why a CDC is refused afterward, and what to do
about each.

Placeholders in messages are written in angle brackets, for example, `<worker>`. Commands on a host run
through `oc debug node/<node>` followed by `chroot /host` on OpenShift, or through SSH elsewhere.

= Where the rejections show up

Discovery runs in three steps: it *selects workers*, *inspects* each one with a short-lived probe Job,
and *writes* the draft. The rejections are grouped by the stage that produces them:

#grid((1.4fr, 2.6fr, 2.6fr, 0.5fr), ([Stage], [Rejects], [Where the customer sees it], [Section]),
  [Discovery run], [The whole run, so no draft is written], [OperatorOps `status.message`], [2],
  [Worker selection], [Workers that cannot be used at all], [OperatorOps events `WorkerDeclined`, `DeviceInspectionFailed`], [3],
  [Disk inspection on the worker], [Disks that are in use, formatted, or not suitable hardware], [OperatorOps events `DeviceDeclined` (rule `available`), and the probe report], [4],
  [Discovery filters], [Disks the run was told not to take, workers left with no disks, workers off the shared networks], [OperatorOps events `DeviceDeclined`, `WorkerDeclined`, `ConfigRefused`], [5],
  [Draft document checks], [A draft or approved CDC that cannot be deployed], [CDC `status.message`, phase `Draft` or `Failed`, Warning events], [6],
  [Approval and admission], [Changes to a CDC or OperatorOps that are not allowed], [The `kubectl apply` or `kubectl edit` is denied with the message], [7],
)

#v(4pt)
#grid((1.5fr, 3fr), ([What to collect], [Command]),
  [Run status and summary], [`kubectl get operatorops -n <ns>` and `kubectl get operatorops <run> -n <ns> -o jsonpath='{.status.message}'`],
  [Every refusal, one event each], [`kubectl describe operatorops <run> -n <ns>`],
  [Probe reports, with the full detail of each disk refusal], [`kubectl get cm -n <ns> -l storage.simplyblock.io/nodeprobe-run=<run> -o yaml`],
  [Probe Jobs and logs (kept for one hour)], [`kubectl get jobs,pods -n <ns> -l storage.simplyblock.io/nodeprobe-run=<run>`, then `kubectl logs <pod> -n <ns>`],
  [Draft document status], [`kubectl get cdc -n <ns>` and `kubectl describe cdc <name> -n <ns>`],
)

#note[A disk refusal event shows only the reason name, for example, `sdb: declined by available because the
probe refused it: NotBlank`. *What* was found (the mountpoint, the filesystem type, the holding device) is in
the probe report ConfigMap under `devices[].rejections[].detail`. Ask the customer for it whenever the
reason name alone does not settle the case.]

#pagebreak(weak: true)
== The order of the checks

Discovery runs its checks in the order below. To find out why a worker or a disk is missing from the draft,
follow it from the top until the first check it fails; the red box names the rejection and the section that
explains it. Every disk refusal from the disk inspection (section 4) is one box here, because the probe
records all of them at once rather than one after another.

#let ok = rgb("#e8eef5")
#let bad = rgb("#fbeaea")
#let red = rgb("#b03a2e")
#let check(pos, body) = node(pos, body, fill: ok, stroke: 0.6pt + navy, corner-radius: 2pt, width: 56mm)
#let exit(pos, body) = node(pos, body, fill: bad, stroke: 0.6pt + red, corner-radius: 2pt, width: 60mm)
#let terminal(pos, body) = node(pos, text(fill: white, weight: "bold", body), fill: navy, shape: fletcher.shapes.pill, width: 40mm)
#let connector(pos) = node(pos, text(weight: "bold")[A], shape: circle, stroke: 0.6pt + navy, fill: ok)
#let no(from, to) = edge(from, to, "-|>", stroke: 0.6pt + red, label: text(fill: red)[no], label-side: left)
#let next(from, to) = edge(from, to, "-|>", stroke: 0.6pt + navy)

#align(center, {
  set text(7.4pt)
  set par(leading: 0.4em)
  diagram(
    spacing: (7mm, 3.4mm),
    node-inset: 4pt,

    terminal((0, 0), [Discovery starts]),
    check((0, 1), [*Each worker:* usable? \ not cordoned, tainted, control-plane, or misnamed]),
    exit((1, 1), [Worker skipped (§3). None left: *No free worker* (§2)]),
    check((0, 2), [Did the worker's probe report?]),
    exit((1, 2), [*Probe failed* (§3). On every worker: *All probes failed* (§2)]),
    check((0, 3), [*Each disk:* a whole disk?]),
    exit((1, 3), [*Not a whole disk* (§5.1). Partitions only in logical block device mode]),
    check((0, 4), [A local disk, not a simplyblock volume?]),
    exit((1, 4), [*simplyblock volume* (§5.1)]),
    check((0, 5), [Matches the run's mode? \ NVMe (default) or logical block devices]),
    exit((1, 5), [*Not NVMe* (§5.1). Ask whether logical block devices are meant]),
    check((0, 6), [Free on the worker? \ not mounted, held, formatted, partitioned, ...]),
    exit((1, 6), [Inspection reason: `Mounted`, `NotBlank`, `LVMPhysicalVolume`, ... (§4)]),
    check((0, 7), [Passes the run's filters? \ iSCSI, allow and deny lists, model, size]),
    exit((1, 7), [*iSCSI disk*, *allow or deny list*, *model*, or *size filter* (§5.1)]),
    connector((0, 8)),

    connector((2, 0)),
    check((2, 1), [*Each worker:* any disk left?]),
    exit((3, 1), [*No disk left*, or NVMe disks held by another driver (§5.2)]),
    check((2, 2), [All disks on one NUMA node?]),
    exit((3, 2), [*Second NUMA node*: the other socket's disks are left out (§5.2)]),
    check((2, 3), [On the shared management and data networks?]),
    exit((3, 3), [*Management network* or *data network* (§5.2)]),
    check((2, 4), [Any worker left with disks?]),
    exit((3, 4), [*No usable disk* (§2). The run fails]),
    check((2, 5), [Journal disk available? \ logical block device mode only]),
    exit((3, 5), [*Only one disk* or *tied smallest disks* (§5.3). The run fails]),
    check((2, 6), [Draft CDC written. \ Document checks pass?]),
    exit((3, 6), [Draft held in `Draft`, approved CDC `Failed` (§6)]),
    check((2, 7), [Approval accepted?]),
    exit((3, 7), [Request denied (§7)]),
    terminal((2, 8), [Cluster deployed]),

    next((0, 0), (0, 1)), next((0, 1), (0, 2)), next((0, 2), (0, 3)), next((0, 3), (0, 4)),
    next((0, 4), (0, 5)), next((0, 5), (0, 6)), next((0, 6), (0, 7)), next((0, 7), (0, 8)),
    next((2, 0), (2, 1)), next((2, 1), (2, 2)), next((2, 2), (2, 3)), next((2, 3), (2, 4)),
    next((2, 4), (2, 5)), next((2, 5), (2, 6)), next((2, 6), (2, 7)), next((2, 7), (2, 8)),
    no((0, 1), (1, 1)), no((0, 2), (1, 2)), no((0, 3), (1, 3)), no((0, 4), (1, 4)),
    no((0, 5), (1, 5)), no((0, 6), (1, 6)), no((0, 7), (1, 7)),
    no((2, 1), (3, 1)), no((2, 2), (3, 2)), no((2, 3), (3, 3)), no((2, 4), (3, 4)),
    no((2, 5), (3, 5)), no((2, 6), (3, 6)), no((2, 7), (3, 7)),
  )
})

A worker that fails a check on the left is left out, and so is a disk; the run continues with the rest.
Disks that pass every check on the left continue at *A*. The "Not a whole disk" and "Not NVMe" refusals
are left out of the run's summary message, because every worker has many such devices; look for them in
the events.

= Discovery run

The run itself fails and no draft is written. The OperatorOps shows phase `Failed` and the message below.

#grid(cols, head,
  [No free worker], [`no worker is free: every node either carries a StorageNode already, is unschedulable, is reserved for the control plane or for infrastructure, or does not match the run's selector`],
  [No worker is left after the selection in section 3.],
  [Check the `WorkerDeclined` events for the individual reasons. Common causes: all workers cordoned or tainted; the run names `workers` or a `nodeSelector` that matches nothing; storage is meant to go on control-plane nodes (set `spec.discover.enableControlPlaneNodes: true`); every worker already runs a storage node.],
  [No usable disk], [`no worker has a device this run would use: <count> device(s) across <count> worker(s), none usable: <count> declined by <rule>: <reason>; ...`],
  [Every disk on every worker was refused.],
  [The counts name the dominant reason. Look it up in sections 4 and 5. The two most common: the disks are virtio, SCSI, or SATA and the run is in the default NVMe mode (section 5, "Not NVMe"), or the disks carry old data (section 4).],
  [All probes failed], [`no worker reported: every one of the <count> probe Jobs failed`],
  [The probe Job failed three times on every worker.],
  [Collect the probe pod logs. Usual causes: the image cannot be pulled (private registry, air gap), the probe is not allowed to run privileged (OpenShift SCC, Pod Security admission), or a taint keeps it off the nodes.],
  [Probes timed out], [`the probes did not all finish within 15m; ...`],
  [A probe pod did not finish within 15 minutes.],
  [Find the pod that is still `Pending` or `Running` and ask for its `kubectl describe` output. Usually, it is an image pull or a taint without a matching `spec.discover.tolerations` entry.],
  [Cluster not readable], [`the cluster could not be read within 5m` or `the draft could not be written within 5m`],
  [The Kubernetes API did not answer in time.],
  [Check API server health and the operator logs, then run discovery again.],
  [Invalid size range], [`spec.discover.deviceFilter.driveSizeRange is "<value>", which cannot be read` or `the size range "<value>" counts backward`],
  [The customer's size filter is not understood. Usually refused right at `kubectl apply`.],
  [Valid forms: `100G-2T`, `500G-` (at least), `-2T` (at most), `1920G` (exactly). Units T, G, M; a plain number means G.],
  [Probe image missing], [`no probe image: set <variable> on the operator's deployment ...`],
  [The operator was installed without the probe image setting.],
  [Reinstall or upgrade the operator with the Helm chart, which sets it.],
  [Reports deleted], [`the probe reports are gone, so there is nothing to write`],
  [Someone deleted the probe ConfigMaps while the run was in progress.],
  [Run discovery again.],
  [Run cannot resume], [`the run cannot be resumed: ...`],
  [The run's status was damaged, or it was started by a different operator version.],
  [Delete the OperatorOps and run discovery again.],
)

= Worker selection

Before anything is inspected, workers that cannot host storage are left out. The run continues with the
rest. Each one is reported as a `WorkerDeclined` event.

#grid(cols, head,
  [Cordoned or tainted], [`<worker> is not used: it is cordoned`, `... it carries the taint <key>=<value>:<effect>`, or `... it is not schedulable`],
  [The node is cordoned, or it has a `NoSchedule` or `NoExecute` taint.],
  [Ask whether the node is meant for storage. If so, `kubectl uncordon <node>` or remove the taint. Adding tolerations to the run is not enough: a tainted node is still skipped.],
  [Control-plane node], [`<worker> is not used: it is <role>; set spec.discover.enableControlPlaneNodes to include it`],
  [The node is a control-plane node. Infrastructure nodes are used without this setting.],
  [Ask whether storage is meant to run there (typical for three-node compact clusters). If so, set `spec.discover.enableControlPlaneNodes: true`.],
  [Unknown worker name], [`<worker> is named in spec.discover.workers and there is no node by that name`],
  [A name in the run's worker list does not exist.],
  [Compare with `kubectl get nodes` and fix the name.],
  [Already a storage node], [(nothing is reported)],
  [The worker already runs a simplyblock storage node.],
  [Expected. Discovery only proposes new workers.],
  [Not selected], [(nothing is reported)],
  [The worker does not match the run's `nodeSelector`.],
  [Check the node labels against the selector.],
  [Probe failed on this worker], [`the probe on <worker> failed, so it is not in the draft`],
  [The probe Job failed three times on this worker only.],
  [Collect that probe pod's logs and `kubectl describe` output.],
)

= Disk inspection on the worker

The probe looks at every block device on the worker and records every reason it cannot be handed to
simplyblock. In the events these show as `declined by available because the probe refused it: <reason>,
<reason>`. A disk can carry several reasons; resolve all of them.

Before wiping anything, confirm with the customer that the data on the disk is not needed. Every command
below destroys what is on the disk.

#grid((1fr, 1.6fr, 1.7fr, 2.6fr), ([Reason], [When it happens], [Detail in the probe report], [How to resolve]),
  [`Mounted`], [The disk or one of its partitions is mounted. Always the case for the boot disk.], [`mounted at [<paths>]`],
  [If it is the boot or a system disk: expected, nothing to do. Otherwise unmount it and remove it from `/etc/fstab` or the systemd mount unit, so it is not mounted again after a reboot.],
  [`SwapArea`], [The disk or a partition of it is in use as swap.], [`the device is an active swap area`],
  [`swapoff /dev/<disk>` and remove it from `/etc/fstab`.],
  [`Stacked`], [Another device is built on top of the disk: an active LVM volume, a software RAID array, an encrypted volume, or a multipath map. `lsblk` shows children under the disk.], [`held by [<device>]`],
  [Ask what the disk is used for. To release it: LVM, check what else the group holds with `pvs -o pv_name,vg_name,pv_used`. If the volume group lies only on this disk: `vgchange -an <vg>`, then `vgremove <vg>`. If it spans other disks, `vgremove` would delete their volumes too: move the data off this disk with `pvmove /dev/<disk>` (or remove the volumes on it), then `vgreduce <vg> /dev/<disk>`. Then `pvremove /dev/<disk>`. Software RAID, `mdadm --stop /dev/<md>`, then `mdadm --zero-superblock /dev/<disk>`. Encryption, `cryptsetup close <name>`. Multipath, exclude the disk in the multipath configuration.],
  [`LVMPhysicalVolume`], [The disk is an LVM physical volume, but no LVM volume on it is active. `lsblk` shows a bare disk; `pvs` or `pvdisplay` lists it.], [`LVM2 physical-volume label at 512`],
  [Check what else the group holds with `pvs -o pv_name,vg_name,pv_used`. If the volume group lies only on this disk: `vgchange -an <vg>`, then `vgremove <vg>`. If it spans other disks, `vgremove` would delete their volumes too: move the data off this disk with `pvmove /dev/<disk>` (or remove the volumes on it), then `vgreduce <vg> /dev/<disk>`. Then `pvremove /dev/<disk>`. On RHEL-based hosts also `lvmdevices --deldev /dev/<disk>`. Operator versions before this reason existed report the same disk as `NotBlank`.],
  [`NotBlank`], [The disk carries a filesystem, swap signature, encryption header, RAID or ZFS label, or other data that is not recognized as empty.], [What was found, for example `XFS superblock at 0`],
  [`wipefs -a /dev/<disk>`. For an old software RAID member, `mdadm --zero-superblock /dev/<disk>`. For ZFS, `zpool labelclear -f /dev/<disk>`.],
  [`Partitioned`], [The disk has a partition table.], [`the device carries the partitions [<names>]` or `GPT header at <offset>`],
  [If the partitions are not needed: `wipefs -a /dev/<disk>` (and `sgdisk --zap-all /dev/<disk>` to remove the backup GPT). If the table is left over and partitioning is the only reason on the disk, the customer can set `spec.discover.enablePartitionedDevices: true` instead of wiping.],
  [`NotAWholeDisk`], [The device is a partition, an LVM volume, a RAID array, a loop device, a RAM disk, or a network block device (`nbd`). Every worker has many of these; they are expected.], [`the device is a <kind>`],
  [Usually nothing: these are never proposed. If the customer expected a *partition* to be used: partitions can only be used in logical block device mode. Ask whether logical block devices are meant to be used (see section 5, "Not NVMe"). Otherwise use the whole disk. In logical block device mode a partition is taken when this is its only reason and it is empty; any other reason on it is resolved like the same reason on a disk.],
  [`Busy`], [The kernel would not hand the disk over, and nothing visible (mount, swap, stacked device) explains why.], [`the kernel refused an exclusive open, and gives no reason`],
  [Usually seen together with `Mounted` on the boot disk, which explains it. Alone: find out what holds the disk with `fuser -vm /dev/<disk>` or `lsof /dev/<disk>`. Typical holders: multipathd, a virtual machine, an iSCSI target, a backup agent.],
  [`Unreadable`], [The probe could not establish whether the disk is in use or what is on it.], [`the kernel could not be asked: ...` or `the device could not be read: ...`],
  [Check `dmesg` on the worker for I/O errors (failing disk) and the probe logs for permission errors. Run discovery again.],
  [`ReadOnly`], [The disk is read-only.], [`the kernel presents the device read-only`],
  [Change the setting in the hypervisor or the storage controller, or `blockdev --setrw /dev/<disk>` if it was set on the host.],
  [`Removable`], [The device is removable: CD/DVD drive, USB stick, card reader.], [`the kernel reports the device as removable ...`],
  [Not usable for storage. If a real disk shows up as removable (some hypervisors do this), change the virtual disk type.],
  [`NoCapacity`], [The device reports a size of zero: an unused loop or network block device, an empty card reader.], [`the device reports a size of zero`],
  [Nothing; expected.],
  [`FabricNamespace`], [The disk is a remote NVMe volume attached over the network, for example a simplyblock volume in use on this worker.], [`the namespace is reached over a fabric ...`],
  [Nothing; it is not a local disk. If the attachment is stale, `nvme disconnect` it.],
  [`UnknownTransport`], [The disk sits on a controller type the probe does not recognize.], [`the device sits on no bus this scan could name`],
  [Cannot be worked around by configuration. Escalate to engineering with the disk's driver and `/sys/block/<disk>` path.],
)

#note[Not refused: a disk wiped with `wipefs` is taken, and the report notes the old format. A disk that still
carries data from an earlier simplyblock deployment is also taken, and the report says so. If the old data
must not be reused, wipe the disk before running discovery.]

= Discovery filters

After inspection, discovery applies the customer's own settings and the deployment's requirements. A disk
gets only the first refusal that applies, in the order listed. Each one is reported as a `DeviceDeclined` or
`WorkerDeclined` event.

== Disks

#grid(cols, head,
  [Not a whole disk], [`<worker>/<device>: declined by whole disk because it is a <kind> rather than a whole disk`],
  [A partition in NVMe mode, or any loop, RAID, or LVM device. Left out of the run's summary because every worker has these.],
  [Same as `NotAWholeDisk` in section 4.],
  [simplyblock volume], [`... because it is a simplyblock logical volume of cluster <id>, which this fleet already serves`],
  [The disk is a simplyblock volume attached to this worker.],
  [Nothing; it belongs to an existing cluster.],
  [Not NVMe], [`... because this run scans NVMe devices and the device is on <bus>`],
  [The run is in the default NVMe mode and the disk is virtio, SCSI, SAS, SATA, or similar. Left out of the run's summary, so a run that only finds such disks reports "no usable disk" with few details.],
  [Ask whether logical block devices are meant to be used. Typical for virtual machines and cloud instances. If so, run discovery again with `spec.discover.enableLogicalBlockDevices: true`. In that mode disks are named by their `/dev/disk/by-id` path, and partitions are possible.],
  [Remote volume in block mode], [`... because this run scans logical block devices and the device is on nvme-fabric ...`],
  [Logical block device mode, and the disk is a remote NVMe volume.],
  [Nothing; it is not a local disk.],
  [Refused by inspection], [`... because the probe refused it: <reason>, ...`],
  [The disk inspection gave one or more reasons.],
  [Look up each reason in section 4.],
  [iSCSI disk], [`... because it is an iSCSI LUN, which is storage on the other side of a network, so a run takes one only where the allow list names it ...`],
  [The disk is attached over iSCSI and the customer did not list it explicitly.],
  [Ask whether the iSCSI disk is meant to back simplyblock. If so, add it to `spec.discover.deviceFilter.blockAllowList`.],
  [Deny or allow list], [`... because it is in the deny list` or `... because it is not in the allow list`],
  [The customer's disk filters exclude it.],
  [Check `spec.discover.deviceFilter`: `pcieAllowList` and `pcieDenyList` take PCI addresses (NVMe mode); `blockAllowList` and `blockDenyList` take `/dev/...` paths (block mode).],
  [Model filter], [`... because its model "<model>" does not contain "<text>"`],
  [The customer set `pcieModel` and this disk's model does not match.],
  [Fix or remove `spec.discover.deviceFilter.pcieModel`.],
  [Size filter], [`... because it is <size> and the range <range> starts above it` (or `ends below it`)],
  [The disk is outside `spec.discover.deviceFilter.driveSizeRange`.],
  [Widen or remove the range.],
)

== Workers

#grid(cols, head,
  [No disk left], [`<worker>: declined by has devices because no device of it survived the device rules`],
  [Every disk of this worker was refused.],
  [Look at that worker's disk refusals.],
  [NVMe disks in use by another driver], [`... it presents no usable block device, and <count> of its NVMe controllers (<addresses>) are bound to a userspace driver and in use, so something is driving its disks`],
  [The NVMe disks are held by a userspace driver such as `vfio-pci`, and something is using them: a previous SPDK or simplyblock installation, or a virtual machine with the disks passed through.],
  [Ask what runs on the worker. Stop the old installation or the virtual machine, then run discovery again.],
  [NVMe disks, state unknown], [`... whether anything is driving <count> of its NVMe controllers (<addresses>) could not be established, so they are not offered`],
  [The NVMe disks are on a userspace driver, and the probe could not check whether they are in use.],
  [Collect the probe logs. Check on the host whether anything uses the controllers.],
  [NVMe disks free to reclaim], [`... are bound to a userspace driver and nothing is using them, so the disks are there to be reclaimed`],
  [Left over from an earlier installation and not in use.],
  [Normally taken automatically. If not, unbind the controllers from `vfio-pci` and run again.],
  [Management network], [`<worker>: declined by management network because <worker> is on <network>, and the fleet's management network is <network>, held by <n> of its <m> workers`],
  [The worker's management address is on a different network than most workers.],
  [Ask whether the worker is meant to be part of the cluster. Either connect it to the common management network or leave it out of the run.],
  [Data network], [`... because it holds no data interface on <network>, and the fleet's data networks are <networks>, held by <n> of its <m> workers`],
  [The worker is missing an interface on one of the networks most workers use for storage traffic.],
  [Configure the missing interface on the worker, or leave it out of the run.],
  [Second NUMA node], [`... declined by most available NUMA node because NUMA node <n> carries <count> unclaimed devices ... so it was chosen`],
  [The worker has disks on more than one CPU socket. Only the disks of the socket with the most disks are proposed.],
  [Expected. If the customer wants the other disks, they can edit the draft before approving it.],
  [Not fully readable (optional)], [`... the probe could not read <count> of its readings: ...`],
  [Only when this check is enabled; it is off by default.],
  [Collect the probe logs.],
)

== Journal disk (logical block device mode)

In logical block device mode every worker needs one disk set aside for the journal. The run fails with
`no document could be drafted: ...` and a `ConfigRefused` event.

#grid(cols, head,
  [Only one disk], [`<worker> hands over no disk that could carry the journal, and a block cluster has no other journal layout ... Give the worker a second disk, or deploy it as nvme`],
  [A worker contributes fewer than two disks.],
  [Add a second disk to the worker, leave the worker out, or deploy in NVMe mode if the disks are NVMe.],
  [Tied smallest disks], [`<worker> hands over no disk smaller than its others, so nothing says which one carries the journal ...`],
  [Two or more of the worker's disks share the smallest size, so none is obviously the journal disk. The larger disks do not matter: 1 TB, 1 TB, and 4 TB is refused too.],
  [Set `spec.discover.forceJournalDevice: true`. One of the disks then becomes the journal; the message states how much capacity that costs.],
)

= Draft document checks

The draft CDC is checked continuously. While a problem exists, the CDC stays in phase `Draft` with the
first problem in its status message; an approved CDC moves to `Failed`. Every problem is also a Warning
event on the CDC. A valid draft reports `the document is valid and is waiting for spec.approved`.

#grid((1.2fr, 2.6fr, 2.2fr), ([Rejection], [What the customer sees], [How to resolve]),
  [Unknown worker], [`<worker> is not a node of this Kubernetes cluster`], [A worker was renamed or removed after discovery, or the draft was edited. Fix the worker names, or run discovery again.],
  [Worker listed twice], [`<worker> appears in more than one group ...`], [Each worker may appear in one group only.],
  [Different network interfaces], [`the groups name different management interfaces (<names>) ...` (or data interfaces)], [All workers of one cluster must use the same interface names. Rename the interfaces on the hosts, or split the workers into separate clusters.],
  [No management interface], [`group(s) <names> name no management interface ...`], [Set the management interface in the group.],
  [Group without disks], [`group <name> names no devices ...`], [Add disks or remove the group.],
  [Failure domain missing], [`the cluster has failure domains enabled and group <name> declares none ...`], [Label the workers with their failure domain, or set it in the group.],
  [Too many failure domains], [`the document would bring the cluster to <n> failure domains, and a cluster maps at most <max>`], [Merge failure domains.],
  [Mixed disk types], [`the groups name <class> devices and cluster <name> is built out of <class> ...`], [When adding to an existing cluster, use the same mode (NVMe or logical block devices) the cluster was built with.],
  [Journal required], [`the groups name logical block devices, and the control plane runs those only with the journal on a device of its own ...`], [Set `spec.cluster.enableJournalDevice: true`.],
  [Cluster too large], [`node set <name> makes <n> storage nodes, and a cluster holds at most <max> ...` or `the document adds <n> storage nodes to cluster <name> ...`], [Split the workers into several node sets (each becomes its own cluster), or add fewer nodes.],
  [Name too long], [`node set <name> makes a member named like <name>, which a StorageCluster cannot be named ...`], [Shorten the cluster or node set name.],
  [Sizing missing], [`spec.clusterRef names supercluster <name>, and the members the document adds have no sizing ...`], [Add the cluster settings (`spec.cluster`).],
  [Spans regions], [`node set <name> lists workers in regions <names>, and a member stays within one region`], [Group the workers by region.],
  [Uneven failure domains], [`node set <name> holds no worker in failure domain <name>, which another node set of its region holds ...`], [Rebalance the workers so each node set covers every failure domain of its region.],
  [Unsupported erasure coding], [`<field> is <scheme>, which is not a scheme simplyblock supports ...`], [Pick a supported scheme.],
  [Too few nodes, workers, or disks for the erasure coding], [`<field> is <scheme>, which needs at least <n> storage node(s) ...` (or workers, or devices)], [Add nodes, workers, or disks, or choose a smaller scheme.],
  [Single-node mode], [`the cluster is a single-node cluster (clusterMode Single), which runs exactly one storage node ...`], [Use exactly one worker, or a different cluster mode.],
  [Cluster exists or is missing (after approval)], [The CDC moves to `Failed` because the cluster to create already exists, or the cluster to extend does not.], [To extend an existing cluster, name it in `spec.clusterRef`. Check the name otherwise.],
)

= Approval and admission

These are refused immediately: the `kubectl apply` or `kubectl edit` fails with the message.

#grid((1.2fr, 2.8fr, 2fr), ([Rejection], [What the customer sees], [How to resolve]),
  [Cannot approve], [`this document cannot be approved, and approving it is what makes it immutable: <problems>`. Problems include: a worker that does not exist; another approved CDC creating the same cluster; neither a cluster to create nor one to extend; the cluster to extend does not exist; a cluster with that name already exists; mixed disk types.],
  [Fix each listed problem, then approve again.],
  [Change after approval], [`spec.approved cannot be withdrawn` or `spec is immutable once spec.approved is true ...`],
  [An approved CDC cannot be changed. To add workers, create a new CDC that names the cluster in `spec.clusterRef`, or run discovery again with `spec.discover.clusterRef`.],
  [Invalid CDC contents], [For example: `a device selection names NVMe addresses or block devices, not both`, `every group of a node set must name the same device class`, `enableArbitration and arbitration are only valid when clusterMode is Dual`, `spec.cluster.openshift ... spec.environment has to be OpenShift`.],
  [Fix the document as the message says.],
  [Invalid discovery settings], [`the PCI filters select NVMe devices and cannot be combined with enableLogicalBlockDevices ...`, `blockAllowList and blockDenyList ... require enableLogicalBlockDevices`, `spec.discover names workers and also carries a nodeSelector ...`],
  [Use the filters that match the chosen mode, and either `workers` or `nodeSelector`, not both.],
)

= Discovery settings reference

All under `spec.discover` of the OperatorOps.

#grid((1.5fr, 4fr), ([Setting], [Effect]),
  [`enableLogicalBlockDevices`], [Logical block device mode: virtio, SCSI, SAS, SATA, and NVMe disks through the kernel, named by `/dev/disk/by-id` path. Required for virtual machines and most cloud instances. The default is NVMe mode, which takes only NVMe disks, named by PCI address.],
  [`enablePartitionedDevices`], [Takes a disk whose only problem is a leftover partition table.],
  [`forceJournalDevice`], [Logical block device mode: picks a journal disk when several disks tie for the smallest size.],
  [`enableControlPlaneNodes`], [Includes control-plane nodes. Infrastructure nodes are always included.],
  [`workers` or `nodeSelector`], [Which workers to inspect; use one or the other.],
  [`tolerations`], [Lets the probe run on tainted nodes and carries over to the deployment. Does not make a tainted node usable.],
  [`failureDomainLabel`], [The node label that names each worker's failure domain.],
  [`deviceFilter.pcieAllowList`, `pcieDenyList`, `pcieModel`], [NVMe mode disk filters, by PCI address and model.],
  [`deviceFilter.blockAllowList`, `blockDenyList`], [Logical block device mode disk filters, by `/dev/...` path.],
  [`deviceFilter.driveSizeRange`], [Size range, for example `500G-4T`.],
  [`clusterRef`], [The draft adds workers to an existing cluster.],
  [`configName`], [Name of the draft CDC.],
)
