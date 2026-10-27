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
//
// discovery-symptoms.typ cites this document by section number: renumbering a
// section here means updating its Read column.

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
  [Probe Job on each worker], [Host OS, CPU, memory, huge pages, network interfaces, block devices, NVMe controllers, and the NetworkAttachmentDefinitions of the run's namespace], [Probe report ConfigMap, labeled `storage.simplyblock.io/nodeprobe-run=<run>`], [4 to 9],
  [Discovery planner], [Choices made from the readings: interfaces, NUMA node, groups, cluster settings], [The draft CDC and the notes in its status], [10],
)

#v(4pt)
The probe report carries a schema version (currently 11). A report from a probe image of another version is
refused, and the probe is run again. A reading the probe could not take is listed under `unreadable` in the
report, one sentence each. The rest of the report stays valid. Section 11 shows a complete report.

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
  [Schedulability], [`spec.unschedulable`, `spec.taints`], [Cordoned, or a taint with effect `NoSchedule` or `NoExecute` that `spec.discover.tolerations` does not tolerate.], [Such a node is skipped.],
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

== Kernel

The kernel release is read from `/proc/sys/kernel/osrelease` (what `uname -r` prints), and the report carries
the upstream version it opens with as `version.major`, `version.minor`, and `version.patch`: `6.1.155` of
`6.1.155-176.282.amzn2023.x86_64`, and `6.8.0` of `6.8-rc1`. A release that does not open with two numbers
leaves `version` out. Distributions backport fixes without changing the version, so it places a kernel in
upstream's history and does not say which fixes it carries.

Each fact below is read
from the first source that answers, in this order: the running kernel (`/sys/module`, `/sys/class/misc`), the
module index of the booted release (`/usr/lib/modules/<release>/modules.builtin` and `modules.dep`), and the
kernel build configuration (`/proc/config.gz`, or `/usr/lib/modules/<release>/config` on RHEL, Fedora, and Amazon
Linux). Ubuntu keeps its configuration only in `/boot`, which the probe does not read. Every fact carries a
`detail` naming the source that answered. The report holds them under `hostOS.kernel`.

#grid((1.1fr, 3fr, 2.6fr), ([Native NVMe multipath], [How it is recognized], [Meaning]),
  [`Enabled`], [`/sys/module/nvme_core/parameters/multipath` is `Y`, or the configuration sets `CONFIG_NVME_MULTIPATH=y`.], [A volume served over several paths is one disk, and the kernel fails over between the paths.],
  [`Disabled`], [The parameter is `N`, or the configuration sets the option and the kernel command line carries `nvme_core.multipath=N`.], [Each path is a disk of its own until the host reboots without the parameter.],
  [`NotBuilt`], [The configuration does not set `CONFIG_NVME_MULTIPATH`, or `nvme_core` is loaded without a `multipath` parameter and no configuration is readable. This is the default Amazon Linux 2023 EKS node image.], [Each path is a disk of its own, and only another kernel changes that.],
  [`Unknown`], [`nvme_core` is not loaded and no configuration is readable.], [Nothing was concluded.],
)

#v(4pt)
#grid((1fr, 2.2fr, 1.6fr, 1.6fr, 1.6fr), ([Capability], [`Available`], [`Loadable`], [`Absent`], [Module and option]),
  [`/dev/kvm`], [`/sys/class/misc/kvm` exists.], [The CPU reports `vmx` or `svm`, and `kvm_intel` or `kvm_amd` is a module that is not loaded.], [The CPU reports neither `vmx` nor `svm` (no hardware virtualization, or a VM without nested virtualization), KVM is loaded and registered no device, or the kernel lacks KVM.], [`kvm`, `kvm_intel`, `kvm_amd`, `CONFIG_KVM`],
  [`/dev/net/tun`], [`/sys/class/misc/tun` exists, or `tun` is loaded or built in.], [`tun` is a module that is not loaded. The kernel loads it on the first open of `/dev/net/tun`.], [Neither the module index nor the configuration has it.], [`tun`, `CONFIG_TUN`],
  [pNFS block layout], [`blocklayoutdriver` is loaded or built in.], [`blocklayoutdriver` is a module that is not loaded.], [Neither the module index nor the configuration has it.], [`blocklayoutdriver`, `CONFIG_PNFS_BLOCK`],
)

#v(4pt)
A capability nothing could answer for is `Unknown`. A worker whose native NVMe multipath is `NotBuilt` or `Disabled`
gets a Warning event `NVMeMultipathUnavailable` on the OperatorOps. It is not refused. The storage engine runs in
user space and does not use the kernel's NVMe initiator, so the worker hosts a storage node. It cannot mount a
volume over more than one path, so workloads with volumes need nodes on another kernel, for example, the Ubuntu
24.04 EKS image.

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

== Network attachments

The probe lists the Multus NetworkAttachmentDefinitions (`k8s.cni.cncf.io/v1`) in the run's namespace, and
the report carries them under `networkAttachments`. Whether the cluster serves the kind at all is asked of the
API server's discovery endpoint first.

#grid((1.1fr, 2fr, 3fr), ([Reading], [Values], [Meaning]),
  [`state`], [`Served`], [The cluster runs Multus. `definitions` holds the namespace's definitions, possibly none.],
  [`state`], [`NotServed`], [The cluster does not register the kind: no Multus.],
  [`state`], [`Unread`], [The list failed, and `detail` says why, for example, missing RBAC. Nothing is concluded about the cluster.],
  [`type`], [`ipvlan`, `macvlan`, `host-device`, `bridge`, `sriov`, and others], [The CNI plugin that creates the pod's interface. For a plugin list (`plugins`), the first plugin.],
  [`hostInterface`], [An interface name], [The `master` of an ipvlan, macvlan, or VLAN plugin, the `device` of a host-device, or the `bridge` of a bridge plugin.],
  [`pciAddress`], [A PCI address], [The `pciBusID` of a host-device named by slot.],
  [`mode`, `ipam`], [For example, `l2` and `whereabouts`], [The plugin's mode and its address plugin.],
  [`resourceName`], [A device plugin resource], [The annotation `k8s.v1.cni.cncf.io/resourceName`, which SR-IOV definitions carry.],
  [`problem`], [A sentence], [Why nothing could be read from the configuration: there is none (Multus then reads it from the node), or it does not parse.],
)

#v(4pt)
A run uses the definitions only when `spec.discover.networkAttachment` names them, which the chart values
`discovery.networkAttachment.management` and `discovery.networkAttachment.data` set. Without it, the draft uses
host networking as before.

- *Both named:* every group carries `networkAttachment` with `management` and `data`, and names no interfaces.
- *Only management named:* every group carries `networkAttachment.management` alone. Data traffic flows through the
  management attachment.
- *Cluster without Multus:* when no probe found the kind served and at least one found it not served, the draft
  uses host networking and the run raises the Warning `NetworkAttachmentsUnavailable`.
- *Definition missing:* a name the namespace does not hold fails the run before any worker is probed, with a
  Warning naming the missing definition and the definitions the namespace holds. A namespace with no
  definitions at all fails the same way: a run that asks for an attachment never falls back to host networking.
- *Host interface missing:* a worker without the `hostInterface` (or without a NIC at the `pciAddress`) of a
  named definition is refused under the same rule. A `bridge` definition and one naming no host interface are
  not checked against the worker.
- *Reading failed:* a worker whose probe reported `Unread` is refused under the same rule, with the reason.

A draft never mixes the two: a group names its interfaces or its attachment, and every group of a document
uses the same networking. Until storage nodes are built on attachments, a document with attachments is held
from approval with the finding `NetworkAttachmentNotDeployable`.

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

#note[Every value below is derived by the run. None can be stated ahead of it, by the Helm chart or otherwise:
a different value is an edit to the draft before it is approved (section 12). The draft's `ConfigWritten`
events say how each one was derived.]

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

#pagebreak(weak: true)
= Example probe report

One worker's report as the probe writes it, under the key `report.json` of the probe report ConfigMap: an
Ubuntu 22.04 worker with two blank NVMe disks, a SATA boot disk, and one NVMe controller a userspace driver
holds. Fields with no value are left out rather than written empty. Sizes are bytes, and a `numaNode` of `-1`
is a device the kernel places on no memory node. A reading that failed adds an `unreadable` list of sentences
at the end, one sentence for each, such as
`read the kernel: read the kernel release from /proc/sys/kernel/osrelease: no such file or directory`.

#v(4pt)
#block(fill: rgb("#f6f8fa"), stroke: 0.6pt + rgb("#d0d7de"), inset: 8pt, radius: 2pt, width: 100%)[
#set text(size: 7.2pt)
#set par(leading: 0.42em)
```json
{
  "version": 11,
  "node": "worker-01",
  "probedAt": "2026-09-18T09:00:00Z",
  "cpu": {
    "onlineCPUs": 32, "physicalCores": 16, "sockets": 1, "threadsPerCore": 2, "hyperThreading": true,
    "numaNodes": [
      { "node": 0, "onlineCPUs": [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
          21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31], "physicalCores": 16 }
    ]
  },
  "memory": {
    "totalBytes": 274877906944, "freeBytes": 64424509440, "availableBytes": 257698037760,
    "numaNodes": [
      { "node": 0, "totalBytes": 137438953472, "freeBytes": 68719476736 },
      { "node": 1, "totalBytes": 137438953472, "freeBytes": 68719476736 }
    ]
  },
  "hugePages": [
    { "sizeBytes": 1073741824, "total": 32, "free": 32,
      "numaNodes": [ { "node": 0, "total": 16, "free": 16 }, { "node": 1, "total": 16, "free": 16 } ] }
  ],
  "interfaces": [
    { "name": "eth0", "speedMbps": 25000, "mtu": 1500, "state": "up", "driver": "mlx5_core", "numaNode": 0,
      "kind": "physical", "addresses": ["10.10.10.1/24"] },
    { "name": "eth1", "speedMbps": 25000, "mtu": 1500, "state": "up", "driver": "mlx5_core", "numaNode": 1,
      "kind": "physical", "addresses": ["10.10.11.1/24"] }
  ],
  "devices": [
    { "name": "nvme0n1", "path": "/dev/nvme0n1", "pciAddress": "0000:5e:00.0", "sizeBytes": 3298534883328,
      "kind": "Disk", "transport": "NVMe", "model": "SAMSUNG MZQL23T8HCLS-00A07", "numaNode": 0,
      "available": true, "content": "Blank" },
    { "name": "nvme1n1", "path": "/dev/nvme1n1", "pciAddress": "0000:5f:00.0", "sizeBytes": 3298534883328,
      "kind": "Disk", "transport": "NVMe", "model": "SAMSUNG MZQL23T8HCLS-00A07", "numaNode": 0,
      "available": true, "content": "Blank" },
    { "name": "sda", "path": "/dev/sda", "stablePath": "/dev/disk/by-id/ata-SAMSUNG_MZ7L3480HCHQ-00A07_S6Q1NX0T",
      "sizeBytes": 480103981056, "kind": "Disk", "transport": "SATA", "model": "SAMSUNG MZ7L3480HCHQ-00A07",
      "numaNode": -1, "available": false,
      "rejections": [ { "reason": "Partitioned", "detail": "the device carries the partitions [sda1 sda2]" } ] },
    { "name": "sda1", "path": "/dev/sda1", "sizeBytes": 1127219200, "kind": "Partition", "transport": "SATA",
      "numaNode": -1, "available": false,
      "rejections": [ { "reason": "NotAWholeDisk", "detail": "the device is a Partition" },
                      { "reason": "Mounted", "detail": "mounted at [/boot/efi]" } ] }
  ],
  "hostOS": {
    "distro": "ubuntu", "family": "Debian", "version": "22.04", "prettyName": "Ubuntu 22.04.4 LTS",
    "architecture": "x86_64",
    "kernel": {
      "release": "5.15.0-122-generic",
      "version": { "major": 5, "minor": 15, "patch": 0 },
      "nvmeMultipath": { "state": "Enabled", "detail": "nvme_core.multipath is Y" },
      "kvm": { "state": "Available", "detail": "the kernel has registered /dev/kvm" },
      "tun": { "state": "Available", "detail": "the kernel has registered /dev/net/tun" },
      "pnfsBlock": { "state": "Loadable",
                     "detail": "blocklayoutdriver is a module of 5.15.0-122-generic and not loaded" }
    }
  },
  "nvmeControllers": [
    { "address": "0000:5e:00.0", "driver": "nvme", "vendor": "0x144d", "product": "0xa80a", "numaNode": 0 },
    { "address": "0000:5f:00.0", "driver": "nvme", "vendor": "0x144d", "product": "0xa80a", "numaNode": 0 },
    { "address": "0000:62:00.0", "driver": "uio_pci_generic", "vendor": "0x144d", "product": "0xa80a",
      "numaNode": 0, "inUse": true }
  ],
  "networkAttachments": {
    "state": "Served", "namespace": "simplyblock",
    "definitions": [
      { "namespace": "simplyblock", "name": "storage-ipvlan", "type": "ipvlan", "hostInterface": "enp1s0",
        "mode": "l2", "ipam": "static" }
    ]
  }
}
```
]

#pagebreak(weak: true)
= Changing what discovery chose

A choice is changed in one of two places:

- *Before a run*, through the node labels and taints the run reads and the run's own settings,
  `spec.discover` of the OperatorOps. The initial run of a Helm install takes them from the chart's
  `discovery` values.
- *In the draft*, the ClusterDeploymentConfig, after the run and before `spec.approved: true`. The draft is
  checked again after every edit, and a problem shows in its `status.message` and Warning events. Once
  approved, the document can no longer be changed.

A finished run is not run again. A new run is a new OperatorOps
with another name. Its draft is named after the run unless `spec.discover.configName` names it, and a draft
that already exists is left as it is (event `ConfigExists`). Delete the old draft first, or give the new run
another name. When a new run is needed, and how long a run's objects stay, is in section 10 of the
rejections document.

== Workers and disks

#grid((1.3fr, 2.6fr, 2.6fr), ([To], [Before the run], [In the draft]),
  [Leave a worker out], [`spec.discover.workers` lists the workers to inspect, or `nodeSelector` selects them by label. Use one or the other.], [Remove it from its group's `workers`. A group needs at least one worker, so delete a group whose last worker goes.],
  [Include a worker that was declined], [Resolve the reason first (rejections, section 3). Cordoned: `kubectl uncordon`. Control-plane node: `enableControlPlaneNodes: true`. A taint: a matching entry in `tolerations`, which the draft carries to the storage nodes.], [Add it to the `workers` of a group whose devices and interfaces it has. The draft check refuses a name that is not a node (`WorkerNotFound`).],
  [Leave a disk out], [NVMe mode: `deviceFilter.pcieDenyList` (PCI addresses), `pcieAllowList`, or `pcieModel`. Logical block device mode: `blockDenyList` or `blockAllowList` (`/dev/...` paths). Either mode: `driveSizeRange`, for example `500G-4T`.], [Remove it from the group's `devices.nvme` (PCI addresses) or `devices.block` (paths). The list holds for every worker of the group, so a disk only some workers should leave out needs a group of its own for those workers.],
  [Take a disk that was refused], [Resolve its reason (rejections, section 4). A leftover partition table alone: `enablePartitionedDevices: true`. An iSCSI disk: name it in `blockAllowList`.], [Add it to the group's devices. `enableDriveFormat` is on in every draft, so the disk is formatted when the cluster takes it, whatever the probe found on it. Add one only when its data is known to be disposable.],
  [Take the disks of the other NUMA node], [Nothing: the run takes the disks of the NUMA node with the most.], [Add their addresses to the group's devices.],
  [Use logical block devices instead of NVMe], [`enableLogicalBlockDevices: true`. Disks are then named by `/dev/disk/by-id` path, and partitions can be taken.], [Not by an edit: the device names, the journal, and the checks differ. Run discovery again.],
)

== Network

#grid((1.3fr, 2.6fr, 2.6fr), ([To], [Before the run], [In the draft]),
  [Use another management interface], [No setting. The run takes the interface holding the node's internal IP (section 7.2).], [Set the group's `mgmtInterface`. Every group of one cluster has to name the same interface.],
  [Use other data interfaces], [No setting. The ranking is fixed (section 7.3).], [Set the group's `dataInterfaces`. Every group of one cluster has to name the same interfaces.],
  [Attach through Multus], [Set `spec.discover.networkAttachment.management`, and `.data` for a separate data network. In the chart: `discovery.networkAttachment.management` and `.data`.], [Set the group's `networkAttachment` and remove its interfaces. Every group of one document has to name the same attachments.],
)

== Topology

#grid((1.3fr, 2.6fr, 2.6fr), ([To], [Before the run], [In the draft]),
  [Set failure domains], [Label every worker: `topology.kubernetes.io/zone`, or the label `spec.discover.failureDomainLabel` names. Failure domains are enabled only when every worker carries a valid value.], [Set each group's `failureDomain` and `spec.cluster.enableFailureDomains: true`. The domains have to fit the stripe (rejections, section 6).],
  [Choose which workers form one cluster], [Label them `storage.simplyblock.io/placement-group=<name>`. Each value becomes a node set, and each node set a cluster.], [Move the groups between node sets. A node set becomes a cluster of at most 32 storage nodes, within one region.],
  [Add workers to an existing cluster], [`spec.discover.clusterRef` names the cluster. The draft then proposes no cluster settings.], [Set `spec.clusterRef`.],
)

== Cluster settings

These are in the draft only, under `spec.cluster`. A node set whose hardware differs from the rest can carry
its own `vcpuCount`, `maxSubsystemCount`, `minHugePagesSize`, `socketsToUse`, `nodesPerSocket`, and `stripe`
under `spec.nodeSets[].cluster`, on a document with more than one node set or one naming `spec.clusterRef`.

#grid((1.3fr, 2.2fr, 3fr), ([Setting], [What the draft holds], [What to know before changing it]),
  [`vcpuCount`], [The physical cores of the smallest worker's chosen NUMA node, at least 4.], [One value for every storage node of the cluster, which is why the smallest worker sets it.],
  [`minHugePagesSize`], [The free huge pages of the smallest worker's chosen NUMA node.], [Unset when a worker has less than 1 GiB.],
  [`maxSubsystemCount`], [30, not a reading.], [How many subsystems a node serves follows from the workload, not from the hardware.],
  [`enableDriveFormat`], [On.], [With it off, a listed disk that carries anything is not usable.],
  [`stripe`, `clusterMode`], [Derived from the worker and disk counts (section 10).], [Cannot be changed once the cluster exists. The draft check refuses a stripe the fleet is too small for.],
  [`enableJournalDevice`], [On when every worker has exactly one disk smaller than all its others.], [`spec.discover.forceJournalDevice` before the run picks one where disks tie. Logical block device mode requires it.],
  [`enableFailureDomains`], [On when every worker carries a valid failure domain.], [See topology above.],
  [`enableChecksumValidation`, `enableAtomicity4K`, `socketsToUse`, `nodesPerSocket`], [Not set. Nothing the probe reads decides them.], [`enableChecksumValidation` is applied to each disk when the cluster is created and never afterward, so a cluster created without it cannot have it.],
  [`tolerations`], [The tolerations the run probed with.], [What the storage-node pods tolerate. Correct it when the run's tolerations are not the fleet's taints.],
  [`spec.hostOS`], [The workers' distribution, when they all run the same one.], [Set it when os-release was unreadable. One document states one host OS for all its workers, so workers on different distributions go into separate documents.],
  [`spec.environment`], [The run's `status.environment`.], [Correct it when the platform was misread, for example after `EnvironmentPartiallyRead`.],
)

#v(4pt)
#note[Two settings of a group are never derived and can be added in the draft: `spdkSystemMemory`, the memory
SPDK starts with, and `reservedSystemCPU`, the cores held back from SPDK, as a list such as `0,1`.]

