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
//
// discovery-symptoms.typ cites this document by section number: renumbering a
// section here means updating its Read column.

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
  [Warnings], [Nothing. The draft is written, and parts of it need a decision before approval], [Warning events on the OperatorOps and on the CDC], [8],
)

#v(4pt)
#note[`curl -s -L https://install.simplyblock.io/scripts/discovery-bundler | bash` collects all of the following
into one archive. The symptom index shows how to name the namespace and the run, and what to do without the
script.]

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
  [Collect the probe pod logs. Usual causes: the image cannot be pulled (private registry, air gap), the probe is not allowed to run privileged (OpenShift SCC, Pod Security admission), or a taint added to the worker after the run selected it.],
  [Probes timed out], [`the probes did not all finish within 15m; ...`],
  [A probe pod did not finish within 15 minutes.],
  [Find the pod that is still `Pending` or `Running` and ask for its `kubectl describe` output. Usually, it is an image pull, or a taint the worker received after the run selected it.],
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
  [Cordoned or tainted], [`<worker> is not used: it is cordoned`, `... it carries the taint <key>=<value>:<effect>, which spec.discover.tolerations does not tolerate`, or `... it is not schedulable`],
  [The node is cordoned, or it has a `NoSchedule` or `NoExecute` taint the run does not tolerate.],
  [Ask whether the node is meant for storage. If so, `kubectl uncordon <node>`. For a taint that dedicates the node to storage, add a matching entry to `spec.discover.tolerations` and run discovery again: the draft carries it to the storage nodes. Any other taint has to be removed.],
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

Logical block device mode refuses no worker for its journal. By default the configure of every storage node
carves a journal partition out of the node's smallest disk (the journal split), so a worker with a single
disk, such as one RAID-protected storage-array LUN, is drafted too, and every disk carries data. The draft
leaves `enableJournalDevice` unset and says so in its notes.

`spec.discover.forceJournalDevice: true` dedicates the smallest disk of every node to the journal instead. It is
honoured only when every worker hands over at least two disks of known size; otherwise the draft keeps the
journal split and names the worker that could not give up a disk.

= Draft document checks

The draft CDC is checked continuously. While a problem exists, the CDC stays in phase `Draft` with the
first problem in its status message; an approved CDC moves to `Failed`. Every problem is also a Warning
event on the CDC, under the reason shown below each rejection's name. One reason can stand for several
problems, so read the message as well. A valid draft reports `the document is valid and is waiting for
spec.approved`.

#grid((1.2fr, 2.6fr, 2.2fr), ([Rejection], [What the customer sees], [How to resolve]),
  [Unknown worker \ `WorkerNotFound`], [`<worker> is not a node of this Kubernetes cluster`], [A worker was renamed or removed after discovery, or the draft was edited. Fix the worker names, or run discovery again.],
  [Worker listed twice \ `WorkerNotFound`], [`<worker> appears in more than one group ...`], [Each worker may appear in one group only.],
  [Different network interfaces \ `WorkerNotFound`], [`the groups name different management interfaces (<names>) ...` (or data interfaces)], [All workers of one cluster must use the same interface names. Rename the interfaces on the hosts, or split the workers into separate clusters.],
  [No management interface \ `NoManagementInterface`], [`group(s) <names> name no management interface ...`], [Set the management interface in the group.],
  [Group without disks \ `DeviceNotFound`], [`group <name> names no devices ...`], [Add disks or remove the group.],
  [Failure domain missing \ `DeviceNotFound`], [`the cluster has failure domains enabled and group <name> declares none ...`], [Label the workers with their failure domain, or set it in the group.],
  [Too many failure domains \ `TooManyFailureDomains`], [`the document would bring the cluster to <n> failure domains, and a cluster maps at most <max>`], [Merge failure domains.],
  [Failure domains do not fit the stripe \ `FailureDomainLayout`], [`failure-domain mode with <scheme> needs at least <n> distinct failure domains (the stripe's parity plus two), and the workers are in <n> (...)` or `failure-domain mode needs an equal number of workers in every failure domain when the cluster is activated, and the domains hold <domain>: <n>, ...`], [Add workers in further failure domains, move workers so every domain holds the same number, choose a stripe with less parity, or set `spec.cluster.enableFailureDomains: false`.],
  [Mixed disk types \ `DeviceClassMismatch`], [`the groups name <class> devices and cluster <name> is built out of <class> ...`], [When adding to an existing cluster, use the same mode (NVMe or logical block devices) the cluster was built with.],
  [Cluster too large \ `MemberTooLarge`], [`node set <name> makes <n> storage nodes, and a cluster holds at most <max> ...` or `the document adds <n> storage nodes to cluster <name> ...`], [Split the workers into several node sets (each becomes its own cluster), or add fewer nodes.],
  [Name too long \ `MemberNameInvalid`], [`node set <name> makes a member named like <name>, which a StorageCluster cannot be named ...`], [Shorten the cluster or node set name.],
  [Sizing missing \ `MemberSizingUnstated`], [`spec.clusterRef names supercluster <name>, and the members the document adds have no sizing ...`], [Add the cluster settings (`spec.cluster`).],
  [Spans regions \ `MemberSpansRegions`], [`node set <name> lists workers in regions <names>, and a member stays within one region`], [Group the workers by region.],
  [Uneven failure domains \ `MemberMissingFailureDomain`], [`node set <name> holds no worker in failure domain <name>, which another node set of its region holds ...`], [Rebalance the workers so each node set covers every failure domain of its region.],
  [Unsupported erasure coding \ `StripeUnsupported`], [`<field> is <scheme>, which is not a scheme simplyblock supports ...`], [Pick a supported scheme.],
  [Too few nodes, workers, or disks for the erasure coding \ `StripeBelowMinimumNodes`, `StripeBelowMinimumWorkers`, `StripeBelowMinimumDevices`], [`<field> is <scheme>, which needs at least <n> storage node(s) ...` (or workers, or devices)], [Add nodes, workers, or disks, or choose a smaller scheme.],
  [Single-node mode \ `StripeSingleNodeCount`], [`the cluster is a single-node cluster (clusterMode Single), which runs exactly one storage node ...`], [Use exactly one worker, or a different cluster mode.],
  [Cluster exists or is missing (after approval) \ `ClusterExists`, `ClusterNotFound`], [The CDC moves to `Failed` because the cluster to create already exists, or the cluster to extend does not.], [To extend an existing cluster, name it in `spec.clusterRef`. Check the name otherwise.],
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

= Warnings

A warning leaves the run or the document going. The draft is written, or the approved CDC keeps working,
but part of it is not what the customer probably intended. Each one is a Warning event on the object named
in the first column. Collect them with `kubectl get events -n <ns> --field-selector type=Warning`, or with
`kubectl describe` on the OperatorOps and the CDC.

== On the discovery run

These are raised when the draft is written. The run still succeeds. Read them before approving, because
the draft is what gets deployed and a later fix means a new CDC.

#grid((1.25fr, 2.2fr, 1.7fr, 2.4fr), ([Warning], [What the customer sees], [What it changes in the draft], [How to resolve]),
  [`TopologyIncomplete`], [`<workers> carry no <label> label while other workers do, so the draft gives them no failure domain and does not enable failure domains: label them and run discovery again`],
  [The listed workers have no failure domain, and `enableFailureDomains` is off for the whole cluster. The labeled workers keep their value, which then has no effect.],
  [Label the listed workers with the label named in the message (`topology.kubernetes.io/zone` unless the run sets `spec.discover.failureDomainLabel`): `kubectl label node <worker> <label>=<domain>`. Then run discovery again. A cluster without failure domains is valid; if that is intended, nothing needs doing.],
  [`FailureDomainInvalid`], [`worker <worker> carries <label>="<value>", which is not a failure domain (at most 63 letters, digits, '-', '_', or '.', starting and ending with a letter or digit), so it has none in the draft`],
  [The worker has no failure domain, so failure domains are not enabled for the cluster.],
  [Relabel the worker with a value of that form, or point `spec.discover.failureDomainLabel` at a label that carries one. Then run discovery again.],
  [`PlacementGroupLabelIncomplete`], [`<workers> carry no storage.simplyblock.io/placement-group label while other workers on the same network do, so they are split by size: label them to choose their member`],
  [The labeled workers form node sets named after their value. The unlabeled ones are distributed by size into additional node sets, each of which becomes a cluster of its own.],
  [Ask which cluster each listed worker belongs to and label it: `kubectl label node <worker> storage.simplyblock.io/placement-group=<name>`. Run discovery again.],
  [`PlacementGroupInvalid`], [`<workers> carry storage.simplyblock.io/placement-group="<value>", which is not a node set name (a DNS label of at most 23 characters), so they are drafted as unlabeled workers are`],
  [The workers are treated as if they had no label.],
  [Relabel them with a lowercase name of letters, digits, and `-`, at most 23 characters, and run discovery again.],
  [`PlacementGroupInvalid`], [`storage.simplyblock.io/placement-group=<value> is carried by workers in different regions or on different networks, which cannot share a member, so its workers are drafted into <n> node sets`],
  [One placement group becomes several node sets, so several clusters, numbered after the value.],
  [A cluster stays within one region and one network. Give the workers of each region or network their own value, or move them onto one network. If the split is acceptable, nothing needs doing.],
  [`NVMeMultipathUnavailable`], [`<worker> boots kernel <release>, which is built without CONFIG_NVME_MULTIPATH (<evidence>): it hosts a storage node, and it cannot mount a volume over more than one path ...` or `<worker> boots kernel <release> with native NVMe multipath switched off (<evidence>): ... only after a reboot without nvme_core.multipath=N`],
  [Nothing. The worker hosts a storage node. Volumes mounted on it get one disk per path and no failover, which is the default kernel of Amazon Linux 2023 EKS node images.],
  [Ask whether workloads with volumes will run on the worker. If so: built without it, move the worker to a kernel that has it (for EKS, the Ubuntu 24.04 node image). Switched off, remove `nvme_core.multipath=N` from the kernel command line and reboot. The probe report's `hostOS.kernel.nvmeMultipath` names the evidence.],
  [`ControlPlaneNodeIncluded`], [`<worker> is <role> and is in the draft because spec.discover.enableControlPlaneNodes is set`],
  [The control-plane node hosts a storage node.],
  [Confirm with the customer that storage on control-plane nodes is intended. That is typical for three-node compact clusters. Otherwise, remove the setting and run discovery again.],
  [`EnvironmentPartiallyRead`], [`the API groups could not be listed, so the distribution was concluded from the nodes alone: <error>`],
  [`status.environment` (OpenShift, Talos, EKS, and so on) may be wrong, and with it the settings the draft derives from it.],
  [Check `status.environment` of the OperatorOps against the actual platform. If it is wrong, set `spec.environment` in the draft CDC before approving it. Look into the error, usually RBAC or an API server problem.],
  [`ReportUnreadable`], [`<ns>/<configmap>: the probe report is version <n> and this operator reads version <m>, so the probe has to be re-run`, `parse the probe report: ...`, or `the ConfigMap <ns>/<configmap> carries no report.json, so the probe did not finish writing it`],
  [That worker is left out of the draft.],
  [A version mismatch means the operator was upgraded while the run was in progress: run discovery again. Any other message: collect the ConfigMap and the probe pod's logs.],
)

#note[`DeviceInspectionFailed` (the probe on a worker failed) and `WorkerDeclined` for a name that does not exist
are Warning events too. They leave a worker out, so they are listed under worker selection in section 3.]

== On the CDC after approval

#grid((1.25fr, 2.2fr, 1.7fr, 2.4fr), ([Event], [What the customer sees], [What it means], [How to resolve]),
  [`ControlPlaneNotReady` (Warning)], [`ControlPlane <name> is <phase>, so it is not answering yet` or `ControlPlane <name> cannot be read: ...`. The CDC stays in phase `Expanding`.],
  [The CDC is approved and valid, and waits for the simplyblock control plane before it creates the cluster. It retries on its own.],
  [Check `kubectl get controlplane -n <ns>` and the control-plane pods. The CDC continues once the control plane is ready.],
  [`AwaitingNodes` (Normal)], [`StorageNode <name> is <phase> rather than Online`, or, for a document with several clusters, `StorageNode <name> of member <cluster> is not Online yet, so the next member's nodes are not created`],
  [The CDC waits for its storage nodes to come online before activating the cluster, or before creating the next cluster's nodes.],
  [Expected for several minutes. If it stays, look at the named StorageNode: `kubectl describe storagenode <name> -n <ns>` and the storage-node pods on that worker.],
  [`StepDeadlineExceeded` (Warning)], [`Step <step> outlived its deadline`. The CDC moves to `Failed`.],
  [A deployment step did not finish in its time limit. The CDC does not continue.],
  [Find out why the step stalled from the StorageNode and StorageCluster events. A `Failed` CDC cannot be resumed. Fix the cause and create a new CDC for what is missing, naming the cluster in `spec.clusterRef` if it was created.],
)

= Discovery settings reference

All under `spec.discover` of the OperatorOps.

#grid((1.5fr, 4fr), ([Setting], [Effect]),
  [`enableLogicalBlockDevices`], [Logical block device mode: virtio, SCSI, SAS, SATA, and NVMe disks through the kernel, named by `/dev/disk/by-id` path. Required for virtual machines and most cloud instances. The default is NVMe mode, which takes only NVMe disks, named by PCI address.],
  [`enablePartitionedDevices`], [Takes a disk whose only problem is a leftover partition table.],
  [`forceJournalDevice`], [Logical block device mode: dedicates the smallest disk of every node to the journal instead of the default journal split.],
  [`enableControlPlaneNodes`], [Includes control-plane nodes. Infrastructure nodes are always included.],
  [`workers` or `nodeSelector`], [Which workers to inspect; use one or the other.],
  [`tolerations`], [Includes workers whose `NoSchedule` and `NoExecute` taints these tolerate, lets the probe run there, and carries over to the deployment.],
  [`failureDomainLabel`], [The node label that names each worker's failure domain.],
  [`deviceFilter.pcieAllowList`, `pcieDenyList`, `pcieModel`], [NVMe mode disk filters, by PCI address and model.],
  [`deviceFilter.blockAllowList`, `blockDenyList`], [Logical block device mode disk filters, by `/dev/...` path.],
  [`deviceFilter.driveSizeRange`], [Size range, for example `500G-4T`.],
  [`clusterRef`], [The draft adds workers to an existing cluster.],
  [`configName`], [Name of the draft CDC.],
)

#pagebreak(weak: true)
= Running discovery again

A run reads the fleet once. Its draft describes the fleet as it was then and is never updated, and a finished
run never runs again: editing its OperatorOps changes nothing. Whatever changed since needs a new run.

== When a new run is needed

#grid((1.6fr, 2.4fr, 2.6fr), ([After], [Why the existing draft is wrong], [What to do]),
  [Resolving a rejection: a wiped disk, an uncordoned worker, a label, a taint toleration, a filter, the device mode], [The draft was written from what the run saw, and leaves out what it refused.], [A new run. For a small change, editing the draft instead is often quicker (detection document, section 12).],
  [Adding workers to the Kubernetes cluster], [The draft does not name them.], [A new run. To add them to a cluster that is already deployed, set `spec.discover.clusterRef` to that cluster.],
  [Removing or renaming a worker], [The draft check reports `WorkerNotFound` and holds the draft.], [Remove the worker from the draft, or run again.],
  [A disk changing after the run: data written to it, mounted, replaced, or removed], [Nothing notices. The draft checks look at the nodes, not at the disks.], [Run again before approving. `enableDriveFormat` is on in every draft unless it was edited, so approving formats every listed disk, including one that received data after the run.],
  [Upgrading the operator while a run is in progress], [When the new operator reads another report version, reports written by the previous probe image are refused with `ReportUnreadable`, and their workers are left out.], [A new run. A run that finished before the upgrade is unaffected, and so is its draft.],
  [A failed or aborted run], [It wrote no draft, or a partial one, and never resumes.], [Fix the cause from its message, then start a new run.],
  [`helm upgrade` with changed `discovery` values], [The chart's run keeps its name, and a finished run is not run again, so an upgrade does not discover again.], [Write the OperatorOps by hand.],
)

== Starting one

A new run is a new OperatorOps with a name not used before. Its draft is called `discovered-<run>` unless
`spec.discover.configName` names it, and a run whose draft name is taken leaves the existing draft as it is
(event `ConfigExists`) instead of overwriting a document someone may have reviewed.

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: OperatorOps
metadata:
  name: discovery-2
  namespace: <ns>
spec:
  action: Discover
  discover: {}        # the settings of section 9, as the earlier run had them plus the change
```

A worker that already runs a storage node is never inspected again. A worker named in another draft is, and
nothing refuses approving two documents that name the same worker, so delete a draft that will not be used.

== What a run leaves behind, and for how long

#grid((1.6fr, 2.4fr, 2.6fr), ([Object], [Lifetime], [Notes]),
  [OperatorOps], [Until deleted.], [The run's record: its phase, status message, and environment. Deleting a running one aborts it first. `spec.abort: true` stops it without deleting it.],
  [Probe report ConfigMaps (label `storage.simplyblock.io/nodeprobe-run=<run>`)], [As long as the OperatorOps exists.], [Deleted with the run. Collect them first if the case is open.],
  [Probe Jobs and pods], [One hour after they finish.], [Their logs go with them. A failed probe is retried twice.],
  [Events on the OperatorOps], [About one hour, the Kubernetes default.], [The status message and the probe reports keep the essentials after the events are gone.],
  [The draft ClusterDeploymentConfig], [Until deleted. Deleting the run does not delete it.], [It carries the run's name in the label `storage.simplyblock.io/nodeprobe-run`.],
  [NVMe controllers a probe returned to the kernel driver], [Permanently.], [Listed under `reclaimed` in that worker's report.],
)

