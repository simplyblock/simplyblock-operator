// Support reference: everything a discovery run reads about the cluster, its
// workers, and their disks, with the exact values each reading recognizes and what
// the drafted ClusterDeploymentConfig derives from it. It is the companion of
// discovery-rejections.typ and lives with the runbooks for the same reason: the
// support team reads it while reviewing a customer's draft.
//
// Build: typst compile discovery-detection.typ (from the repository root:
// typst compile operator/docs/runbooks/discovery-detection.typ discovery-detection.pdf).
//
// Keep it in step with the code that produces these readings:
// atlas-lib/blockdev (device kinds, transports, signatures, usage, stable names),
// atlas-lib/inventory (CPU, memory, huge pages, interfaces, host OS, environment),
// atlas-lib/pci (NVMe controllers), operator/internal/nodeprobe (the report), and
// operator/internal/discovery (interface choice, network alignment, NUMA placement,
// node roles, failure domains, and the derived cluster settings).
// The document is written for support staff, so it names no code locations.

#set document(title: "Deployment discovery detection", author: "simplyblock")
#set page(
  paper: "a4",
  flipped: true,
  margin: (x: 15mm, top: 13mm, bottom: 15mm),
  footer: context [
    #set text(7pt, fill: gray)
    simplyblock support: what deployment discovery detects
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
// Reading tables: what is read, where from, the values recognized, and what uses it.
#let cols = (1.2fr, 1.6fr, 2.6fr, 2.2fr)
#let head = ([Reading], [Source], [Values recognized], [Used for])

#align(center, text(18pt, weight: "bold")[Deployment discovery: what is detected about the workers])

Discovery (an `OperatorOps` resource with `action: Discover`) reads the cluster and every candidate worker,
then drafts a *ClusterDeploymentConfig* (CDC). This document lists every reading discovery takes, the exact
values it recognizes, and which setting of the draft each reading decides. The refusals that follow from these
readings are in the companion document, _Deployment discovery: rejections and how to resolve them_.

Placeholders are written in angle brackets, for example, `<worker>`. Paths such as `/sys/class/net` are the
host's, read by the probe through a host mount.

= Where the readings come from

#grid((1.3fr, 2.4fr, 2.6fr, 0.5fr), ([Source], [What it reads], [Where the customer sees it], [Section]),
  [Kubernetes API, once per run], [The cluster's distribution, from API groups and node metadata], [OperatorOps `status.environment`], [2],
  [Kubernetes Node objects], [Roles, taints, schedulability, internal IP, CPU, memory, and huge-page capacity, topology labels], [`kubectl get node <worker> -o yaml`], [3],
  [Probe Job on each worker], [Host OS, CPU, memory, huge pages, network interfaces, block devices, NVMe controllers], [Probe report ConfigMap, labeled `storage.simplyblock.io/nodeprobe-run=<run>`], [4 to 9],
  [Discovery planner], [Choices made from the readings: interfaces, NUMA node, groups, cluster settings], [The draft CDC and the notes in its status], [10],
)

#v(4pt)
The probe report carries a schema version (currently 9). A report from a probe image of another version is
refused, and the probe is run again. A reading the probe could not take is listed under `unreadable` in the
report, one sentence each. The rest of the report stays valid.

= Cluster environment

The distribution is concluded once per run from the API server and the node objects. Every marker found is
recorded as evidence. When markers of several distributions are present, the first in the order OpenShift,
Talos, EKS, GKE, AKS, OKE, K3s, Rancher wins. Without any marker, the environment is `Vanilla`.

#grid((1fr, 1.3fr, 3.6fr), ([Distribution], [Evidence source], [Markers]),
  [OpenShift], [API group], [`config.openshift.io`, `operator.openshift.io`, `route.openshift.io`, `security.openshift.io`, `machineconfiguration.openshift.io`],
  [OpenShift], [Node OS image], [Starts with `Red Hat Enterprise Linux CoreOS`],
  [OpenShift], [Node label or annotation key], [Prefix `node.openshift.io/` or `machineconfiguration.openshift.io/`],
  [Talos], [API group], [`talos.dev`],
  [Talos], [Node OS image], [Starts with `Talos`],
  [Talos], [Node label or annotation key], [Prefix `talos.dev/`],
  [EKS], [Kubelet version], [Contains `-eks-`, for example `v1.30.8-eks-2d5f260`],
  [EKS], [Node label or annotation key], [Prefix `eks.amazonaws.com/`],
  [GKE], [Kubelet version], [Contains `-gke.`, for example `v1.30.5-gke.1014001`],
  [GKE], [Node label or annotation key], [Prefix `cloud.google.com/gke-`],
  [AKS], [Node label or annotation key], [Exactly `kubernetes.azure.com/agentpool` or `kubernetes.azure.com/cluster`. A longer key that starts with either, such as `kubernetes.azure.com/cluster-name`, is not a marker.],
  [OKE], [Node label or annotation key], [Prefix `oke.oraclecloud.com/`],
  [K3s], [API group], [`k3s.cattle.io`],
  [K3s], [Kubelet version], [Contains `+k3s`],
  [K3s], [Node label or annotation key], [Prefix `k3s.io/`],
  [Rancher], [API group], [`management.cattle.io`, `provisioning.cattle.io`, `rke.cattle.io`],
  [Rancher], [Kubelet version], [Contains `+rke2` or `+rke`],
  [Rancher], [Node label or annotation key], [Prefix `rke2.io/`, `rke.cattle.io/`, `management.cattle.io/`, or `cattle.io/`],
)

#v(4pt)
The labels a cloud provider's controller sets (`oci.oraclecloud.com/`, `topology.k8s.aws/`,
`kubernetes.azure.com/managed`) are not markers, because self-managed clusters on the same cloud carry them too.
EKS Anywhere carries the `-eks-` suffix and Google Distributed Cloud the `-gke.` suffix, so both read as the managed
service. On EKS, GKE, AKS, OKE, and Talos the storage node leaves the kubelet configuration alone.

= Kubernetes node facts

#grid(cols, head,
  [Role], [Labels `node-role.kubernetes.io/<role>`], [`control-plane` and `master` mean ControlPlane, `infra` means Infra, `worker` means Worker. A node with no role label is a Worker. A node with several roles counts as ControlPlane first, then Infra.], [Worker and Infra nodes are used. ControlPlane nodes only with `spec.discover.enableControlPlaneNodes`. The draft gets one node set per role, named `discovered`, `infra`, or `control-plane`.],
  [Schedulability], [`spec.unschedulable`, `spec.taints`], [Cordoned, or any taint with effect `NoSchedule` or `NoExecute`.], [Such a node is skipped.],
  [Internal IP], [`status.addresses` of type `InternalIP`], [The first such address.], [Pins the management interface (section 7.2).],
  [CPU and memory], [`status.capacity` and `status.allocatable`], [`cpu`, `memory`. Reserved memory is capacity minus allocatable.], [Shown for review next to the probe's own readings.],
  [Huge pages], [`status.capacity` and `status.allocatable`], [Every resource named `hugepages-<size>`, for example `hugepages-2Mi`, `hugepages-1Gi`.], [Shown for review.],
  [Failure domain], [The label named by `spec.discover.failureDomainLabel`, default `topology.kubernetes.io/zone`], [At most 63 characters, letters, digits, `-`, `_`, and `.`, starting and ending with a letter or digit.], [When every worker carries a valid value, the draft sets `enableFailureDomains` and names each group's domain. Invalid or partly missing labels are reported as findings `FailureDomainInvalid` and `TopologyIncomplete`.],
  [Region], [Label `topology.kubernetes.io/region`], [Any value.], [Workers of different regions never share a member cluster.],
  [Placement group], [Label `storage.simplyblock.io/placement-group`], [A DNS label of at most 23 characters.], [One node set per value, instead of the automatic size split. Findings `PlacementGroupLabelIncomplete` and `PlacementGroupInvalid`.],
)

= Host operating system

Read from the host's `/etc/os-release` (`ID`, `ID_LIKE`, `VERSION_ID`, `PRETTY_NAME`) and from `uname`
(machine field, for example `x86_64` or `aarch64`).

#grid((1.4fr, 4fr), ([Family], [Distributions (`ID` values)]),
  [`Debian`], [`ubuntu`, `debian`],
  [`RedHat`], [`rhel`, `centos`, `rocky`, `almalinux`, `fedora`, `amzn` (Amazon Linux), `ol` (Oracle Linux)],
  [`SUSE`], [`sles`, `opensuse`, `opensuse-leap`, `opensuse-tumbleweed`],
  [`Alpine`], [`alpine`],
  [`Arch`], [`arch`],
  [(no family)], [`flatcar`, `talos`: hosts without a package manager. Any other `ID` gets the family of the first distribution named in its `ID_LIKE`, or none.],
)

#v(4pt)
The draft sets `hostOS` (distribution and family) only when every worker reports the same distribution.
Workers on different distributions, or a worker whose os-release could not be read, leave it unset with a note
naming the workers.

= CPU

#grid(cols, head,
  [Online and offline CPUs], [`/sys/devices/system/cpu/online` and the CPU list], [Count of logical CPUs in use, and of those the kernel knows but does not use.], [Review. Offline CPUs on a worker are worth asking about.],
  [Physical cores, sockets], [`topology/core_id` and `topology/physical_package_id` of each CPU], [A core is counted once per socket.], [`vcpuCount` (below).],
  [Threads per core, hyperthreading], [`topology/thread_siblings_list`], [1 without simultaneous multithreading, 2 with it. Hyperthreading is on when above 1.], [Review.],
  [Per NUMA node], [`/sys/devices/system/node/node<n>`], [Online CPUs and physical cores of each memory node.], [NUMA placement and `vcpuCount`.],
)

#v(4pt)
`vcpuCount` in the draft is the number of physical cores of the NUMA node chosen on each worker (section
8.7), taken from the worker with the fewest. It is never below 4, the API's minimum. A worker whose chosen disks
span NUMA nodes contributes nothing to it.

= Memory and huge pages

#grid(cols, head,
  [Memory], [`/proc/meminfo`], [`MemTotal`, `MemFree`, `MemAvailable`, `Hugetlb`, `SwapTotal`, `SwapFree`.], [Review. Size against available memory, not free memory.],
  [Memory per NUMA node], [`/sys/devices/system/node/node<n>/meminfo`], [Total and free bytes per node.], [Review.],
  [Huge-page pools], [`/sys/kernel/mm/hugepages/hugepages-<size>kB`], [`nr_hugepages` (total) and `free_hugepages` (free) for every page size, for example 2 MiB and 1 GiB.], [`minHugePagesSize`.],
  [Huge pages per NUMA node], [`/sys/devices/system/node/node<n>/hugepages`], [The same per memory node.], [`minHugePagesSize` and NUMA placement.],
)

#v(4pt)
`minHugePagesSize` is the free huge-page memory on the chosen NUMA node of the worker with the least, rounded
down to whole GiB (for example `4G`). It is left unset when any worker has no free huge pages on that node, or
when the smallest amount is under 1 GiB. Each node then computes its own minimum.

= Network interfaces

== Readings per interface

The probe reports every interface, virtual ones included.

#grid(cols, head,
  [Name, MAC address], [`/sys/class/net/<if>/address`], [As reported.], [The draft names interfaces by name.],
  [MTU], [`/sys/class/net/<if>/mtu`], [Any value. 9000 and above counts as jumbo frames.], [Data interface ranking.],
  [Link speed], [`/sys/class/net/<if>/speed`], [Megabits per second. 0 means unknown (link down or virtual device). A bond, team, or bridge without a speed of its own gets the sum of its members' speeds. A VLAN or macvlan gets its parent's.], [Data interface minimum and ranking, management ranking.],
  [State], [`/sys/class/net/<if>/operstate`], [`up`, `down`, `unknown`, and the other kernel states. Only `up` and `unknown` are usable.], [Both interface choices.],
  [Driver, PCI address], [`/sys/class/net/<if>/device`], [Empty for an interface without hardware. A bond or team takes its members' driver when all members share one.], [Data interface ranking: drivers whose name starts with `mlx` (NVIDIA Mellanox: `mlx4_core`, `mlx4_en`, `mlx5_core`) rank first. No other driver is singled out.],
  [NUMA node], [`device/numa_node`], [The memory node of the NIC, or unknown. For a bond, the node of its members when they agree.], [Review and NUMA breakdown.],
  [Peered], [`iflink` compared to `ifindex`], [True for one end of a pair, which is what a pod's veth is.], [Excludes pod links from the management interface.],
  [Lower and upper devices], [`lower_<if>` and `upper_<if>` links], [Members of a bond or bridge, the parent of a VLAN or macvlan, and what is built on top.], [Speed, driver, and NUMA node through the stack.],
  [VLAN, VXLAN], [Netlink], [VLAN ID 0 to 4094 with protocol `802.1Q` or `802.1ad`. VXLAN network identifier 0 to 16777215.], [Review.],
  [Addresses], [Netlink], [Every IPv4 and IPv6 address in CIDR notation, for example `10.10.0.1/24`. Link-local, loopback, and unspecified addresses do not count as reachable.], [Interface choice and network alignment.],
)

#v(4pt)
#grid((1fr, 3fr, 2.2fr), ([Kind], [How it is recognized], [Usable for]),
  [`loopback`], [The loopback flag.], [Nothing.],
  [`bridge`], [`DEVTYPE=bridge`, or a `bridge` directory.], [Management only when it holds the node's internal IP.],
  [`bond`], [`DEVTYPE=bond`, or a `bonding` directory.], [Management and data.],
  [`team`], [`DEVTYPE=team`.], [Management and data.],
  [`vlan`], [`DEVTYPE=vlan`.], [Management.],
  [`vxlan`], [`DEVTYPE=vxlan`.], [Management only when it holds the node's internal IP.],
  [`macvlan`, `ipvlan`], [`DEVTYPE=macvlan` or `DEVTYPE=ipvlan`.], [Management.],
  [`virtual`], [Under `/sys/devices/virtual` and none of the above: veth, dummy, tunnel, CNI devices.], [Management only when it holds the node's internal IP and is not peered.],
  [`physical`], [Everything else: a NIC with hardware behind it.], [Management and data.],
)

== Management interface

The interface holding the node's internal IP is the management interface, unless it is a loopback or a
peered virtual device. Otherwise the candidates are interfaces that are `up` or `unknown`, of a kind listed
above as usable for management, and holding a reachable address. Bridges and VXLAN interfaces are not
candidates. The fastest candidate wins, then the simpler kind (physical, then bond or team, then VLAN, macvlan,
or ipvlan), then the name in alphabetical order.

== Data interfaces

A data interface is a `physical`, `bond`, or `team` interface that is `up` or `unknown`, whose first IPv4
address is reachable, and whose speed is at least 10000 Mbps. An interface with only IPv6 addresses does not
qualify. The candidates are ranked by:

+ MTU of 9000 or more.
+ Higher speed.
+ Bond or team before a single NIC.
+ A Mellanox driver (`mlx*`) before any other.
+ Name.

The draft takes the best candidate and every other candidate with the same MTU, speed, driver, and
aggregation. Data interfaces never share hardware with the management interface. When the only qualifying
link is the management link, the draft names no data interfaces and data runs on the management interface.

== Network alignment across workers

The networks are compared by address prefix, not by interface name. A host route (`/32`) names no network and
is not compared.

- *Management network:* the prefix held by most workers' management interfaces. A worker on another network is
  refused.
- *Data networks:* the largest set of prefixes held by more than half of the workers, compared over at most 8
  data networks per worker. A worker missing one of them is refused. The others keep only the data interfaces
  on those networks.
- If any remaining worker is left without data interfaces, the draft names data interfaces for none of them.

= Block devices

== Readings per device

The probe lists every block device in `/sys/class/block`. Hidden NVMe multipath path devices are skipped.

#grid(cols, head,
  [Name, path], [The kernel name and `/dev/<name>`], [For example `sdb`, `nvme0n1`.], [Fallback name in logical block device mode.],
  [Stable path], [`/dev/disk/by-partuuid`, `/dev/disk/by-id`], [Preferred in this order: `by-partuuid`, then by-id links built from the device's own identifier (`wwn-`, `nvme-eui.`, `nvme-uuid.`), then other by-id links (model and serial). `by-uuid`, `by-label`, and `by-path` are never used.], [The device name in logical block device mode.],
  [PCI address], [The controller of the device], [For example `0000:5e:00.0`.], [The device name in NVMe mode.],
  [Size], [`size` (512-byte sectors)], [Bytes. 0 is refused as `NoCapacity`.], [Size filter, journal disk, grouping.],
  [Block sizes], [`queue/logical_block_size`, `queue/physical_block_size`], [Bytes.], [Content reading.],
  [Atomic write unit], [`queue/atomic_write_unit_max_bytes` and `_min_bytes`], [Bytes. Absent on kernels before 6.11, which is not the same as 0.], [Review against `enableAtomicity4K` and checksum validation.],
  [Vendor, model, serial], [`device/vendor`, `device/model`, `device/serial`], [As reported.], [`pcieModel` filter, review.],
  [Rotational], [`queue/rotational`], [True for a spinning disk.], [Review.],
  [Read-only, removable], [`ro`, `removable`], [Either one refuses the device (`ReadOnly`, `Removable`).], [Availability.],
  [NUMA node], [The device's PCI parent], [The memory node, or unknown.], [NUMA placement.],
  [Subsystem NQN], [`subsysnqn` of the NVMe subsystem], [Empty for non-NVMe devices.], [Recognizes simplyblock volumes (section 8.6).],
)

== Device kinds

#grid((1fr, 3fr, 2.2fr), ([Kind], [How it is recognized], [Treatment]),
  [`Disk`], [Everything not listed below.], [The only kind a cluster is built from.],
  [`Partition`], [The device has a `partition` attribute.], [Refused in NVMe mode. In logical block device mode taken when empty.],
  [`DeviceMapper`], [A `dm` directory, or the name starts with `dm-`: LVM volume, dm-crypt, multipath.], [Refused (`NotAWholeDisk`).],
  [`MDRaid`], [An `md` directory, or the name starts with `md`.], [Refused (`NotAWholeDisk`).],
  [`Loop`], [A `loop` directory, or the name starts with `loop`.], [Refused (`NotAWholeDisk`).],
  [`Memory`], [The name starts with `ram` or `zram`.], [Refused (`NotAWholeDisk`).],
  [`Network`], [The name starts with `nbd`, `rbd` (Ceph), or `drbd`.], [Refused (`NotAWholeDisk`).],
)

== Transports

The bus is read from the directories the device's sysfs path passes through. The more specific markers win.

#grid((1fr, 3fr, 2.2fr), ([Transport], [Marker in the device path], [Treatment]),
  [`NVMe`], [An `nvme` directory, or an NVMe multipath subsystem whose controllers report transport `pcie`.], [The only transport of NVMe mode (the default).],
  [`NVMeFabric`], [An NVMe multipath subsystem with a controller on `tcp`, `rdma`, or `fc`, or with no readable controller.], [Always refused (`FabricNamespace`). A remote volume, not a local disk.],
  [`Virtio`], [`virtio<n>`.], [Logical block device mode.],
  [`MMC`], [`mmc_host` or `mmc<n>`: SD or eMMC card.], [Logical block device mode.],
  [`USB`], [`usb<n>`.], [Logical block device mode. Usually also `Removable`.],
  [`SATA`], [`ata<n>`.], [Logical block device mode.],
  [`SAS`], [`end_device-`, `sas_`, or `expander-`.], [Logical block device mode.],
  [`iSCSI`], [`session<n>`.], [Logical block device mode, and only when named in `blockAllowList`.],
  [`SCSI`], [`host<n>` and none of the above.], [Logical block device mode.],
  [(unknown)], [None of the above.], [Refused (`UnknownTransport`).],
)

== Usage on the host

#grid(cols, head,
  [Mounts], [`/proc/self/mountinfo` (host)], [The device or any of its partitions mounted anywhere.], [`Mounted`.],
  [Swap], [`/proc/swaps`], [The device or a partition is active swap.], [`SwapArea`.],
  [Holders], [`/sys/class/block/<dev>/holders`], [Device-mapper, RAID, or multipath devices built on top.], [`Stacked`.],
  [Exclusive open], [Opening the device with `O_EXCL`], [The kernel refuses with nothing above to explain it.], [`Busy`.],
)

== Content on the device

The probe reads the first and last 1 MiB of every device (each read limited to 30 seconds) and matches them
against the catalog below. It never writes. A device on which nothing matches is `Blank` when its first bytes
are all zero, and `Foreign` otherwise.

#grid((1.2fr, 1fr, 2.4fr, 2.4fr), ([Format], [Content class], [Signature], [Detail in the report]),
  [`ext2`, `ext3`, `ext4`], [Filesystem], [Magic `0xEF53` at byte 1080. ext4 by its feature flags, ext3 by its journal flag.], [`ext4 superblock at 1024`],
  [`xfs`], [Filesystem], [`XFSB` at 0.], [`XFS superblock at 0`],
  [`LVM2_member`], [StackLayer], [`LABELONE` with type `LVM2` in one of the first four 512-byte sectors.], [`LVM2 physical-volume label at 512`. Refused as `LVMPhysicalVolume`.],
  [`crypto_LUKS`], [Foreign], [`LUKS\xba\xbe` at 0.], [`LUKS2 header at 0`],
  [`gpt`], [Foreign], [`EFI PART` at LBA 1, or the backup header at the last LBA.], [`GPT header at 512 (LBA 1)`. Refused as `Partitioned`.],
  [`dos`], [Foreign], [`0x55AA` at 510 with at least one used MBR entry. GPT wins when both are present.], [`MBR partition table at 446 ...`. Refused as `Partitioned`.],
  [`vfat`], [Foreign], [A valid FAT boot sector with type `FAT12`, `FAT16`, `FAT32`, or `FAT`.], [`FAT32 boot sector, type at 82`],
  [`exfat`], [Foreign], [`EXFAT` at 3.], [`exFAT boot sector, type at 3`],
  [`btrfs`], [Foreign], [`_BHRfS_M` at 65600.], [`btrfs superblock at 65600`],
  [`swap`], [Foreign], [`SWAPSPACE2` or `SWAP-SPACE` at the end of the first 4, 8, 16, or 64 KiB page.], [`SWAPSPACE2 signature at 4086, ...`],
  [`linux_raid_member`], [Foreign], [md magic at metadata 1.1 (0), 1.2 (4096), 1.0, or 0.90 (both near the end).], [`Linux software-RAID member, metadata 1.2, at 4096`],
  [`zfs_member`], [Foreign], [Uberblock magic `0x00bab10c` in any of the four vdev labels.], [`ZFS uberblock at ..., in the vdev label at ...`],
  [`bcache`], [Foreign], [bcache magic at 4120.], [`a bcache superblock at 4096`],
  [`simplyblock_alceml`], [Simplyblock], [`ALCEML_STORAGE` at 0, or an `ALCEML_PAGEv2` or `_UNMAPPED_` page header on a 4 KiB boundary.], [`a simplyblock storage superblock at 0`],
)

#v(4pt)
#grid((1.2fr, 4.6fr), ([Content class], [Meaning and treatment]),
  [`Blank`], [The first bytes are zero and nothing matched. Available.],
  [`Released`], [A known format whose signature was erased, which is what `wipefs` does. The format is still recognizable from its structure: ext, xfs, btrfs, bcache, swap, LVM2, LUKS, exFAT, FAT, GPT, MBR, md RAID. Available, and the report names the old format.],
  [`Simplyblock`], [Data of an earlier simplyblock deployment that no storage node holds. Available, and the report says so.],
  [`Filesystem`, `StackLayer`, `Foreign`], [Refused as `NotBlank`, `LVMPhysicalVolume`, or `Partitioned`.],
)

== simplyblock volumes

An NVMe namespace whose subsystem NQN parses as a simplyblock logical volume is refused, and the refusal
names the cluster ID the NQN carries. This keeps a volume that an existing cluster exports to the worker out of
the draft.

== NUMA placement

Discovery groups each worker's usable disks by NUMA node and keeps one node. The node with the most disks wins,
then the most total capacity, then the most physical cores, then a known node over an unknown one, then the
lower number. The other node's disks are left out. The breakdown also records, per node, the online CPUs, the
free huge-page memory, and the fastest NIC.

= NVMe controllers

The probe lists every PCI device with class `0x0108` (mass storage, NVM) from `/sys/bus/pci/devices`, including
controllers that present no block device.

#grid(cols, head,
  [Address], [PCI address], [For example `0000:5e:00.0`.], [The NVMe device name.],
  [Vendor, product], [`vendor`, `device`], [Raw PCI IDs, for example `0x144d`. Not resolved to names.], [Review.],
  [Driver], [`driver` link], [`nvme` (the kernel presents its disks), `uio_pci_generic` or `vfio-pci` (a userspace driver holds it), or none.], [Explains a worker with NVMe hardware and no NVMe disks.],
  [In use], [The host's process table, for the controller's `/dev/uio*` or `/dev/vfio/*` files], [Held, free, or unknown when the process table could not be read.], [Whether the controller may be offered.],
  [NUMA node], [`numa_node`], [The memory node, or unknown.], [NUMA placement.],
)

#v(4pt)
A controller on a userspace driver that nothing holds is a leftover of an earlier deployment. The probe binds
it back to the `nvme` driver, waits for its disks to appear, and lists it under `reclaimed` in the report.
In NVMe mode, a controller with no driver at all, or on a userspace driver and free, is offered in the draft
even when it presents no block device. A held controller, or one that could not be checked, is never offered.

#pagebreak(weak: true)
= What the draft derives

#note[The values `enableDriveFormat`, `maxSubsystemCount`, `stripe`, `enableChecksumValidation`, `socketsToUse`,
`nodesPerSocket`, `enableFailureDomains`, and `enableAtomicity4K` can be stated by the installation instead. A
stated value replaces the derived one, and the draft's notes say which values were stated and which were
derived.]

#v(4pt)
#grid((1.4fr, 2.2fr, 3.2fr), ([Draft setting], [From], [Rule]),
  [Device names], [Run mode, PCI address, stable path], [NVMe mode (default): PCI address. Logical block device mode (`enableLogicalBlockDevices`): stable path, or the kernel path when udev published none.],
  [Node groups], [Disks, interfaces, failure domain], [Workers with the same device names, management interface, data interfaces, and failure domain share one group.],
  [Node sets], [Role, region, placement-group label], [One node set per role. A role with more workers than one cluster holds (32 storage nodes) is split into member clusters of near-equal size, dealt in failure-domain order.],
  [Management and data interfaces], [Section 7], [Named per group.],
  [`enableDriveFormat`], [Not a reading], [On, unless the installation states otherwise.],
  [`vcpuCount`], [Physical cores of the chosen NUMA node], [Smallest across the workers, at least 4.],
  [`minHugePagesSize`], [Free huge pages on the chosen NUMA node], [Smallest across the workers, whole GiB. Unset when a worker has none or under 1 GiB.],
  [`maxSubsystemCount`], [Not a reading], [30, unless the installation states otherwise.],
  [`enableJournalDevice`], [Disk sizes per worker], [Set when every worker has exactly one disk smaller than all its others. Disks tied for smallest are taken only with `forceJournalDevice`. In logical block device mode a worker without such a disk fails the run.],
  [`stripe`, `clusterMode`], [Worker count, disk count], [3 or more nodes: 2+1 (1+1 when the scheme needs more nodes). 2 nodes: 1+1 and `clusterMode: Dual`. 1 node: 2+1 with 3 or more data disks, 1+1 with 2, 1+0 with 1, and `clusterMode: Single`.],
  [`enableFailureDomains`], [Failure-domain label], [On when every worker carries a valid value and the stripe fits the domains.],
  [`hostOS`], [os-release], [Set when every worker runs the same distribution.],
)
