// Support runbook: the entry point for a customer's discovery problem. It maps what
// the customer reports ("no disks were offered", "the wrong interface was picked")
// to the place to look first, the usual causes, and the section of the two
// reference documents that explains each: discovery-rejections.typ (why something
// was left out or refused) and discovery-detection.typ (what discovery reads and
// how it derives the draft). It lives with them because it indexes them, and it
// names their sections by number, so a renumbering there is an edit here.
//
// Build: typst compile discovery-symptoms.typ
//
// The document is written for support staff, so it names no code locations.

#set document(title: "Deployment discovery symptoms", author: "simplyblock")
#set page(
  paper: "a4",
  flipped: true,
  margin: (x: 15mm, top: 13mm, bottom: 15mm),
  footer: context [
    #set text(7pt, fill: gray)
    simplyblock support: deployment discovery symptoms
    #h(1fr)
    page #counter(page).display()
  ],
)
#set text(font: ("Helvetica Neue", "Helvetica", "Arial"), size: 9pt, lang: "en", region: "us")
#set par(leading: 0.55em)
#set heading(numbering: "1.1")
#show heading.where(level: 1): set text(14pt)
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
#let cols = (1.5fr, 2fr, 2.9fr, 0.9fr)
#let head = ([The customer reports], [Look at first], [Usual causes], [Read])

#align(center, text(18pt, weight: "bold")[Deployment discovery: from symptom to cause])

A customer rarely reports a reason code. They report what they see: no disks, the wrong interface, a draft
that does not match the hardware. This index starts from that and leads to the reason, and from the reason to
the two reference documents:

- *Rejections:* _Deployment discovery: rejections and how to resolve them_. Why a worker or a disk was left
  out, why a run failed, why a document was refused, and the warnings.
- *Detection:* _Deployment discovery: what is detected about the workers_. What discovery reads on each worker, and how the
  draft's settings are derived from it.

The *Read* column names the section, for example _R 4_ for section 4 of the rejections document and _D 7.3_
for section 7.3 of the detection document. Placeholders are written in angle brackets, for example,
`<worker>`.

= Before anything else

Ask for these four things first. Together they answer most cases without a second round trip.

#grid((1.6fr, 3.4fr), ([What], [Command]),
  [The run and its events], [`kubectl describe operatorops <run> -n <ns>`],
  [The probe reports (a complete example is in D 11)], [`kubectl get cm -n <ns> -l storage.simplyblock.io/nodeprobe-run=<run> -o yaml`],
  [The draft and its events], [`kubectl get cdc <name> -n <ns> -o yaml` and `kubectl describe cdc <name> -n <ns>`],
  [Every warning], [`kubectl get events -n <ns> --field-selector type=Warning`],
)

#v(4pt)
#note[An event line names a reason only, for example,
`sdb: declined by available because the probe refused it: NotBlank`. What was found on the disk is in the probe report under `devices[].rejections[].detail`. Events
expire after an hour by default. The probe reports and the run's `status.message` stay.]

= The run

#grid(cols, head,
  [The run does not finish], [`kubectl get jobs,pods -n <ns> -l storage.simplyblock.io/nodeprobe-run=<run>`],
  [A probe pod is `Pending` (image pull, a taint without a matching `spec.discover.tolerations` entry) or refused (OpenShift SCC, Pod Security admission). After 15 minutes the run fails with "Probes timed out".],
  [R 2],
  [The run failed: "no worker is free"], [`WorkerDeclined` events],
  [Every worker is cordoned, tainted, a control-plane node, already a storage node, or outside `spec.discover.workers` or `nodeSelector`.],
  [R 2, R 3],
  [The run failed: "no worker has a device this run would use"], [The counts in the message, then `DeviceDeclined` events],
  [Virtual machines or cloud instances in the default NVMe mode: the disks are virtio, SCSI, or SATA and need `enableLogicalBlockDevices`. Disks with old data (`NotBlank`, `Partitioned`, `LVMPhysicalVolume`). NVMe disks held by an earlier SPDK or simplyblock installation.],
  [R 5.1, R 4, R 5.2],
  [The run failed: "no document could be drafted"], [`ConfigRefused` event],
  [Logical block device mode with a worker that has one disk, or several disks tied for smallest, so no journal disk can be chosen.],
  [R 5.3],
  [The run failed with any other message], [`status.message` of the OperatorOps],
  [Look the message up in the run failures.],
  [R 2],
  [The run succeeded, with warnings], [Warning events on the OperatorOps],
  [The draft is written, and part of it needs a decision: failure domains, placement groups, the environment, a control-plane node.],
  [R 8.1],
)

= Workers and disks

#grid(cols, head,
  [A worker is missing from the draft], [Events that name the worker: `WorkerDeclined`, `DeviceInspectionFailed`, `ReportUnreadable`, `DeviceDeclined`],
  [Not selected (cordoned, tainted, control plane, selector). Its probe failed. Every disk of it was refused. It is off the fleet's management or data network. It already runs a storage node, which is reported nowhere.],
  [R 3, R 5.2],
  [No disks offered on a worker that has disks], [The worker's probe report: `devices[]` with `available` and `rejections`, and `nvmeControllers[]`],
  [NVMe disks absent from `devices` and their controllers on `vfio-pci` or `uio_pci_generic` with `inUse: true`: SPDK, an earlier installation, or a virtual machine holds them. Disks refused for content or use. In NVMe mode, disks on another bus.],
  [R 4, R 5.2, D 9],
  [Some disks of a worker are missing], [`DeviceDeclined` events for those disks],
  [The disks sit on the other NUMA node, and only the node with the most disks is taken. A size, model, allow, or deny filter. Content on the disk.],
  [R 5.1, R 5.2, D 8.7],
  [A disk is in no event and not in the report], [`lsblk` on the worker, and `nvmeControllers[]` in the report],
  [The kernel does not present it as a block device: its NVMe controller is bound to a userspace driver, or the disk is not attached.],
  [D 9],
  [A disk with data from an earlier simplyblock deployment is offered], [The report's `content` for the disk: `Simplyblock` or `Released`],
  [Expected. A disk carrying an earlier deployment's data (`Simplyblock`) and one erased with `wipefs` (`Released`) are both taken. Wipe the disk completely before discovery if the old data must not be reused.],
  [R 4, D 8.5],
  [Volumes on a worker show one disk per path, or do not fail over], [`NVMeMultipathUnavailable` warning, and `hostOS.kernel.nvmeMultipath` in the worker's report],
  [The worker's kernel is built without native NVMe multipath (Amazon Linux 2023 EKS images), or it was booted with `nvme_core.multipath=N`.],
  [R 8.1, D 4.1],
  [A partition is not used], [The run's mode],
  [Partitions are used only in logical block device mode, and only empty ones.],
  [R 5.1],
)

= The draft's settings

Each row below says why the draft holds what it does. How to change it, before a run or in the draft, is
D 12.

#grid(cols, head,
  [The wrong management interface], [`interfaces[]` in the worker's probe report],
  [The interface holding the node's internal IP wins whatever its speed, unless it is a loopback or a peered virtual device. Check which address Kubernetes reports for the node.],
  [D 7.2],
  [No data interfaces, or the wrong ones], [`interfaces[]`: `mtu`, `speedMbps`, `kind`, `driver`, `addresses`],
  [Below 10000 Mbps, IPv6 only, or not physical, bond, or team. A jumbo-frame (MTU 9000) interface outranks a faster one. One worker without an interface on a common data network turns data interfaces off for the whole draft.],
  [D 7.3, D 7.4],
  [A worker was refused for its network], [`WorkerDeclined` with "management network" or "data network"],
  [Its addresses are on a different prefix than most workers.],
  [R 5.2, D 7.4],
  [The workers are split into several groups], [Compare the workers' reports],
  [Workers share a group only with the same device names, management interface, data interfaces, and failure domain. In NVMe mode the device names are PCI addresses, so different slots make different groups.],
  [D 10],
  [The draft has several node sets, so several clusters], [The node labels and the worker count],
  [More than 32 storage nodes for one role, workers in different regions, or `storage.simplyblock.io/placement-group` labels, including the `PlacementGroup` warnings.],
  [D 10, R 8.1],
  [Failure domains are not enabled], [Warning events, and the workers' failure-domain label],
  [Some workers carry no label (`TopologyIncomplete`), a value is invalid (`FailureDomainInvalid`), or the stripe does not fit the domains.],
  [R 8.1, D 10],
  [`hostOS` is missing from the draft], [The `ConfigWritten` events, and `hostOS` in the reports],
  [The workers run different distributions, or a worker's os-release could not be read.],
  [D 4],
  [`vcpuCount` or `minHugePagesSize` is lower than expected], [`cpu` and `hugePages` in the reports],
  [Both come from the smallest worker, counted on the NUMA node the disks are on. Huge pages under 1 GiB leave the setting unset.],
  [D 5, D 6, D 10],
  [The stripe or cluster mode is unexpected], [Worker count and disk count per worker],
  [Derived from them: 2+1 from three nodes, 1+1 and `Dual` for two, `Single` for one. The stripe cannot change after the cluster is created, so correct it in the draft.],
  [D 10],
  [A journal device is used, or is not], [Disk sizes per worker],
  [Set when every worker has exactly one disk smaller than all its others. Logical block device mode needs one.],
  [D 10, R 5.3],
  [The wrong platform (OpenShift, Talos, EKS, ...)], [`status.environment` of the OperatorOps, and an `EnvironmentPartiallyRead` warning],
  [The markers that identify each platform were missing, or the API groups could not be listed. Set `spec.environment` in the draft.],
  [D 2, R 8.1],
)

= Review and deployment

#grid(cols, head,
  [The draft no longer matches the hardware], [When the run happened (`metadata.creationTimestamp` of the OperatorOps)],
  [The draft is never updated after the run. Workers added, removed, or relabeled since, and disks that changed since, are not in it.],
  [R 10],
  [The draft stays in `Draft`], [`status.message` of the CDC and its Warning events],
  [A document check fails. Without a problem, the message is `the document is valid and is waiting for spec.approved`, which means it is waiting for approval.],
  [R 6],
  [Approving is denied], [The message of the `kubectl` command],
  [A listed problem, another approved CDC for the same cluster, or a change to an approved CDC.],
  [R 7],
  [Approved, and nothing happens], [The CDC's phase and events],
  [`ControlPlaneNotReady`: waiting for the control plane. `AwaitingNodes`: waiting for a storage node to come online.],
  [R 8.2],
  [The approved CDC is `Failed`], [`status.message` of the CDC],
  [A document check failed after approval, the cluster exists already or is missing (`ClusterExists`, `ClusterNotFound`), or a step outlived its deadline (`StepDeadlineExceeded`).],
  [R 6, R 8.2],
)
