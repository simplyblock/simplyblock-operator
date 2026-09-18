<!-- Enumerated input mutations for the discovery generator: the probe-report ConfigMaps a
     test feeds in, and the ClusterDeploymentConfig each one should produce. Written before
     the fixtures so that the axes are settled once rather than per file.
     Working document at the repo root: the fixtures it describes live under
     operator/internal/controllers/deployment/testdata/discovery/. -->

# Discovery generator: test case mutations

**Scope.** The generator is the path from probe reports to a drafted
`ClusterDeploymentConfig`: `nodeprobe.ReportFromConfigMap` →
`discovery.Planner.Plan` → `discovery.ClusterTemplateFor` →
`OperatorOpsReconciler.draftFor`. Everything after approval, meaning validation
and expansion into a `StorageCluster` and `StorageNode` objects, is the
`ClusterDeploymentConfig` controller's other half, covered by
`operator/docs/tests/test-plan-clusterdeploymentconfig.md` §1 (`U-01` onward).
Nothing here duplicates those IDs.

**What one case is.** A directory of input objects and one expected output:

```text
operator/internal/controllers/deployment/testdata/discovery/
  net/
    net-13-bond-holds-node-address/
      case.md               # the row this directory is, copied from this document
      ops.yaml              # the OperatorOps, carrying spec.discover and its deviceFilter
      nodes.yaml            # corev1.Node objects: labels, taints, addresses, capacity
      reports/<node>.yaml   # one ConfigMap per worker, the report JSON under report.json
      expected.yaml         # the ClusterDeploymentConfig the run should write
      expected-refusals.txt # Plan.RefusalLines(), one per line, absent when there are none
    net-14-.../
  numa/
  size/
```

One directory per case, under its family. The family level is for reading rather
than for the harness, which walks the tree and takes every directory holding a
`case.md`: a hundred and seventy directories in one listing is a set nobody
reviews, and eleven families of a dozen is.

The directory name carries the case identifier and a slug of the mutation, so
that a failure names the case without anybody looking it up.

A case with no `expected.yaml` is a refusal case: `expected-error.txt` holds the
message the run fails with, which is what `kubectl get operatorops` shows.

**Two harnesses.** The `Harness` column names which one a row belongs to.

| Harness | Where                                                            | Covers                                                          |
|---------|------------------------------------------------------------------|-----------------------------------------------------------------|
| `CM`    | `deployment` package, table-driven over the testdata directories | The whole path, report decoding and the draft's YAML included   |
| `GO`    | `discovery` package, built reports                               | Cases needing a `Planner` seam the controller never substitutes |

A `GO` row exists because the controller always builds the default `Planner`:
`AllDevices`, `SingleNodeSet`, and `WorkerWasReadable` are reachable only by
constructing one directly.

---

## 1. Mutation axes

| Axis                         | Values exercised                                                                                                                                                                      |
|------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Device class                 | NVMe only, logical block only, both on one worker, neither                                                                                                                            |
| Device count per node        | 0, 1, 4, 10, and one worker at 128 (the schema's `MaxItems`)                                                                                                                          |
| Device presentation          | Kernel-presented disk, partition, loopback, multi-namespace controller, idle userspace controller, held userspace controller, attached simplyblock volume, iSCSI LUN                  |
| Device state                 | Available, mounted, held open, in a device-mapper stack, partitioned, unreadable                                                                                                      |
| NUMA topology                | 1, 2, 4, and 8 memory nodes. Devices on one node, split evenly, split unevenly, and on none                                                                                           |
| vCPU count                   | 2, 8, 32, and 196 logical CPUs, with and without simultaneous multithreading                                                                                                          |
| Memory                       | 4 GiB, 64 GiB, 256 GiB, and 1 TiB total, with and without a per-node breakdown                                                                                                        |
| Huge pages already allocated | None at all, no hugetlbfs, a pool with no free pages, 256 GiB free per node, a total without a per-node breakdown, a pool only on the node not chosen, 2 MiB and 1 GiB pools together |
| Huge-page headroom           | Room for simplyblock's allocation on top of the existing pool, exactly enough, and short of it                                                                                        |
| Reuse instruction            | A run told to take a set number of the pre-allocated pages, and a run told nothing                                                                                                    |
| PCI addressing               | Identical across the fleet, identical across a subset of it, one odd worker out, differing per worker, one address set a subset of another, unsorted, two buses, a five-digit domain  |
| Interface count              | One interface, one that is only loopback, two physical, and a dozen of mixed kinds                                                                                                    |
| Interface kind               | Physical, bridge, bond, VLAN, VLAN over bond, VXLAN, macvlan, veth, and loopback                                                                                                      |
| Interface link               | Up, down, dormant, unknown state, 1G, 10G, 100G, unknown speed, MTU 1500 and 9000                                                                                                     |
| Interface address            | A routable v4 address, a global v6 address, link-local only, none, and the node's own `InternalIP`                                                                                    |
| Interface instruction        | A run told which interface is management, told which are data NICs, and told neither                                                                                                  |
| Stack resolution             | An aggregate reporting no speed of its own, a derived interface over one, members on one memory node and on two, and a stack that points at itself                                    |
| Fleet size                   | 0, 1, 3, and 32 workers                                                                                                                                                               |
| Fleet homogeneity            | Uniform, two layouts, a majority layout with stragglers, one odd worker out, every worker distinct, a mix of NUMA topologies                                                          |
| Node role                    | Unlabeled, `worker`, `infra`, `control-plane`, `master`, and machines carrying two                                                                                                    |
| Filter                       | Each of the seven `DeviceFilter` fields, singly and in the two combinations the CEL rule permits                                                                                      |
| Document shape               | Creating (a cluster template) and growing (`spec.discover.clusterRef`)                                                                                                                |
| Report validity              | Current version, an older version, absent key, malformed JSON, no node name, a foreign node                                                                                           |

---

## 2. Device class and inventory shape

| ID     | Mutation                                                    | Expected                                                                                  | Harness |
|--------|-------------------------------------------------------------|-------------------------------------------------------------------------------------------|---------|
| DEV-01 | 4 NVMe disks, one memory node, NVMe run                     | 1 group, `devices.nvme` holds the 4 addresses ascending, name `group-1-nvme-4x3T`         | `CM`    |
| DEV-02 | 4 virtio disks, block run                                   | 1 group, `devices.block` holds the 4 paths, class `block`                                 | `CM`    |
| DEV-03 | 4 virtio disks, NVMe run                                    | No node sets. Every device refused by `device class`, the worker by `has devices`         | `CM`    |
| DEV-04 | 2 NVMe and 2 virtio disks on one worker, NVMe run           | Only the 2 PCI addresses reach the group                                                  | `CM`    |
| DEV-05 | The same worker, block run                                  | Only the two virtio disks. The NVMe pair is refused for being the other class             | `CM`    |
| DEV-06 | One controller presenting two namespaces at one PCI address | The address is named once. `DeviceCount` is 1 for that controller                         | `CM`    |
| DEV-07 | A SATA disk beside an NVMe one, block run                   | Only the SATA disk. The NVMe run takes only the NVMe disk                                 | `CM`    |
| DEV-08 | A rotational HDD beside an SSD, both free                   | Both admitted: no rule reads `Rotational`. See §14, gap G-2                               | `CM`    |
| DEV-09 | 10 NVMe disks, one memory node                              | All 10 in one group, name `group-1-nvme-10x3T`                                            | `CM`    |
| DEV-10 | A partition and a loopback device beside 4 disks            | Both pre-filtered: absent from `Plan.Explain`, present in `RefusalLines`                  | `CM`    |
| DEV-11 | 128 NVMe disks on one worker                                | One group at the selection's `MaxItems`. The document still applies                       | `CM`    |
| DEV-12 | A disk whose `Kind` is `disk` and whose `SizeBytes` is 0    | Admitted. The group is named `unsized`                                                    | `CM`    |
| DEV-13 | A simplyblock volume attached to the worker, NVMe run       | Refused as this fleet's own volume, naming the cluster it belongs to                      | `CM`    |
| DEV-14 | The same volume on a block run                              | Refused the same way, since the rule is in both pipelines and reads no filter             | `CM`    |
| DEV-15 | A worker whose every disk is an attached volume             | No draft, and the explanation counts them together rather than listing each               | `CM`    |
| DEV-16 | A fabric namespace another product exported                 | Refused for being on a fabric, not as a simplyblock volume: the NQN does not parse as one | `CM`    |
| DEV-17 | An iSCSI LUN beside a virtio disk, block run, no allow list | Only the virtio disk. A LUN is storage across a network and is never taken by default     | `CM`    |
| DEV-18 | The same worker with the allow list naming the LUN          | Both disks. Naming it is the decision a run cannot make for a fleet                       | `CM`    |
| DEV-19 | An iSCSI LUN on an NVMe run                                 | Refused for being the other class, before the iSCSI rule is reached                       | `CM`    |

## 3. NUMA topology

The placement ranks a worker's memory nodes by unclaimed device count, then
capacity, then physical cores, then a real node ahead of the unknown bucket,
then the node id. Each row below pins one of those tie-breaks.

| ID      | Mutation                                                                     | Expected                                                                                  | Harness |
|---------|------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------|---------|
| NUMA-01 | 1 memory node, 4 disks on it                                                 | All 4 used, reason "every unclaimed device is on NUMA node 0"                             | `CM`    |
| NUMA-02 | 2 memory nodes, 2 disks each, equal size, equal cores                        | Node 0 chosen on the id tie-break. 2 disks refused by the placement                       | `CM`    |
| NUMA-03 | 2 memory nodes, 1 disk on node 0 and 3 on node 1                             | Node 1 chosen on count. The single disk refused                                           | `CM`    |
| NUMA-04 | 2 memory nodes, 2 × 8 TiB on node 0 and 3 × 1 TiB on node 1                  | Node 1 chosen: count beats capacity                                                       | `CM`    |
| NUMA-05 | 2 memory nodes, 2 disks each, 1 TiB on node 0 and 2 TiB on node 1            | Node 1 chosen on the capacity tie-break                                                   | `CM`    |
| NUMA-06 | 2 memory nodes, 2 disks each of one size, 8 cores on node 0 and 24 on node 1 | Node 1 chosen on the core tie-break                                                       | `CM`    |
| NUMA-07 | 4 memory nodes, 2 disks each (NPS4)                                          | Node 0 chosen. 6 disks refused. `vcpuCount` is node 0's physical cores                    | `CM`    |
| NUMA-08 | 8 memory nodes, 1 disk each but 3 on node 5                                  | Node 5 chosen on count                                                                    | `CM`    |
| NUMA-09 | Every device reports `numaNode: -1`                                          | The unknown bucket is used. `vcpuCount` floors at 4, `minHugePagesSize` unset, both noted | `CM`    |
| NUMA-10 | 2 disks on node 0 and 2 on `-1`, node 0 carrying cores                       | Node 0 chosen on cores. The unknown bucket is never preferred                             | `CM`    |
| NUMA-11 | The same worker with `Placement: AllDevices`                                 | Every disk used. No single chosen node, so `vcpuCount` floors and huge pages go unset     | `GO`    |
| NUMA-12 | A 1-node worker and a 2-node worker whose chosen addresses coincide          | One group: grouping reads addresses and the interface, never the topology                 | `CM`    |
| NUMA-13 | 2 memory nodes, all 10 disks on node 1 and none on node 0                    | Node 1 chosen with nothing to choose. The CPU note names node 1's cores                   | `CM`    |
| NUMA-14 | A large worker: 2 nodes × 49 cores, 5 disks each, huge pages on both         | 5 disks in the draft, `vcpuCount` 49, `minHugePagesSize` from node 0's free pages alone   | `CM`    |
| NUMA-15 | `cpu.numaNodes` empty while devices report nodes 0 and 1                     | Placement falls through to the id tie-break. `vcpuCount` floors at 4 with its note        | `CM`    |

## 4. Node size: vCPU, memory, and huge pages

simplyblock allocates its own huge pages: the node writes the pool it needs, and
`cluster.minHugePagesSize` is the floor on what it writes, rendered into the
per-node configuration as `MAX_HUGE_PAGES_SIZE`. What discovery reads a host's
pool for is therefore not whether SPDK will find pages to consume. It is the
baseline the new total is added to, which is four questions:

1. Are pages allocated already, and how many?
2. Are there none, on a kernel that has hugetlbfs and on one that has not?
3. Does the machine have memory for simplyblock's allocation on top of the
   existing pool?
4. Is the run told to take a stated number of the pre-allocated pages instead of
   adding its own?

The generator answers none of them. It reads the free pages of the chosen memory
node and proposes them as the cluster's floor, and it proposes nothing at all
when a worker has none, on the stated grounds that SPDK consumes pages rather
than reserving them. Every row below records that output, and §14 carries the
seven findings it produces, `G-14` through `G-20`.

| ID      | Mutation                                                               | Expected                                                                                                                                                                       | Harness |
|---------|------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| SIZE-01 | 8 logical CPUs, 4 physical cores on the chosen node                    | `vcpuCount: 4`, with the ordinary derivation note, not the floor note                                                                                                          | `CM`    |
| SIZE-02 | 2 logical CPUs, 2 physical cores                                       | `vcpuCount: 4` with the note saying the cluster asks for more than the worker has. Not a refusal                                                                               | `CM`    |
| SIZE-03 | 196 logical CPUs, 98 physical cores, 1 memory node, 1 TiB RAM          | `vcpuCount: 98`, the whole machine. See §14, gap G-3                                                                                                                           | `CM`    |
| SIZE-04 | 196 logical CPUs across 2 nodes, 1 TiB RAM, 512 GiB per node           | `vcpuCount: 49`, the chosen node's cores                                                                                                                                       | `CM`    |
| SIZE-05 | A fleet whose chosen nodes carry 4, 16, and 49 cores                   | `vcpuCount: 4`, the note naming the smallest worker                                                                                                                            | `CM`    |
| SIZE-06 | 4 GiB total, 900 MiB available, 4 free disks                           | Admitted unchanged: no rule reads memory. See §14, gap G-4                                                                                                                     | `CM`    |
| SIZE-07 | `memory` zero and an `unreadable` entry for it                         | Admitted by default. The draft is written from the disks                                                                                                                       | `CM`    |
| SIZE-08 | The same report with `WorkerRules: WorkerWasReadable`                  | The worker is refused, the message quoting the unreadable readings                                                                                                             | `GO`    |
| SIZE-09 | 512 × 1 GiB pages pre-allocated, 256 free per node                     | **Contested.** `minHugePagesSize: 256G`: another workload's reservation becomes simplyblock's own floor. See §14, gap G-14                                                     | `CM`    |
| SIZE-10 | One worker of three with no pre-allocated pages on its chosen node     | **Contested.** Unset for the whole cluster, the note naming that worker. Nothing pre-allocated is the ordinary case, not a reason to propose nothing. See §14, gap G-15        | `CM`    |
| SIZE-11 | 256 × 2 MiB pages free on the chosen node, 512 MiB in all              | Unset, the note saying the smallest reservation is under a gigabyte. A 1.5 GiB reading truncates to `1G`                                                                       | `CM`    |
| SIZE-12 | A pool with a total and an empty `numaNodes`                           | Unset: the reservation exists and its distribution is unknown                                                                                                                  | `CM`    |
| SIZE-13 | Pages pre-allocated on node 1 while the placement chooses node 0       | Unset, the note naming the worker. The placement does not read huge pages, and the pool on the other node is neither counted nor reported                                      | `CM`    |
| SIZE-14 | Swap total 8 GiB, swap free 0                                          | No effect on the draft. See §14, gap G-5                                                                                                                                       | `CM`    |
| SIZE-15 | `nodes.yaml` allocatable memory far under capacity                     | Recorded on the `KubeNode`, no effect on the draft                                                                                                                             | `CM`    |
| SIZE-16 | 256 GiB RAM, 200 GiB already in huge pages, 40 GiB available           | **Contested.** `minHugePagesSize: 200G` and no arithmetic against the 40 GiB left. See §14, gap G-16                                                                           | `CM`    |
| SIZE-17 | 1 TiB RAM, nothing pre-allocated, the whole machine available          | **Contested.** Nothing proposed, though the room for an allocation is there and reported. See §14, gaps G-15 and G-16                                                          | `CM`    |
| SIZE-18 | A run meant to take 128 of the host's 256 pre-allocated pages          | **Contested.** No field expresses it. `spec.discover` carries no huge-page input, and neither `minHugePagesSize` nor `spdkSystemMemory` is written by a run. See §14, gap G-17 | `CM`    |
| SIZE-19 | A pool of 256 pages, all promised to a mapping: `resv` 256, `free` 0   | Indistinguishable from an untouched pool of 256, because the report drops `resv_hugepages`. See §14, gap G-18                                                                  | `CM`    |
| SIZE-20 | A 1 GiB pool and a 2 MiB pool, both with free pages on the chosen node | The two sizes are summed into one figure, and nothing says which size the node should take                                                                                     | `CM`    |
| SIZE-21 | No hugetlbfs at all, `hugePages` absent from the report                | The same unset output as a machine whose pools are full, so the two are not told apart. See §14, gap G-19                                                                      | `CM`    |

## 5. PCI addressing and disk count

| ID     | Mutation                                                           | Expected                                                                                                   | Harness |
|--------|--------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------|---------|
| PCI-01 | 32 workers, identical addresses                                    | 1 group of 32 workers                                                                                      | `CM`    |
| PCI-02 | 2 workers, 4 disks each, different slots                           | 2 groups of 1 worker each                                                                                  | `CM`    |
| PCI-03 | 32 workers: 16 on layout A and 16 on layout B                      | 2 groups of 16, ordered by their first worker's name                                                       | `CM`    |
| PCI-04 | 32 workers, every layout distinct                                  | 32 groups in one node set, under the 64-group ceiling                                                      | `CM`    |
| PCI-05 | Addresses reported in descending order                             | `devices.nvme` ascending: the draft sorts                                                                  | `CM`    |
| PCI-06 | 10 disks across two buses, `0000:5e:*` and `0000:af:*`             | One group, addresses ascending across both buses                                                           | `CM`    |
| PCI-07 | A five-digit PCI domain, `10000:01:00.0`                           | **Contested.** The generator names it and the schema's item pattern refuses the document. See §14, gap G-6 | `CM`    |
| PCI-08 | Uppercase hex in the report, `0000:5E:00.0`                        | Named as reported. Grouping is byte-exact, so a fleet mixing cases splits                                  | `CM`    |
| PCI-09 | Workers named `worker-1` … `worker-32` with two layouts            | Group numbering follows lexicographic worker order: `worker-1`, `worker-10`, `worker-11`                   | `CM`    |
| PCI-10 | 5 workers: 3 on layout A, the other 2 each distinct                | 3 groups, of 3, 1, and 1 workers: partial homogeneity still groups what it can                             | `CM`    |
| PCI-11 | 32 workers: 20 on layout A, 6 on layout B, 6 each distinct         | 8 groups, of 20, 6, and six of 1, numbered by their first worker's name                                    | `CM`    |
| PCI-12 | 8 workers uniform but for one whose fourth disk is in another slot | 2 groups, of 7 and 1. One slot moved is a second group, which is the guess a reviewer regroups             | `CM`    |
| PCI-13 | 2 workers on one layout carrying 2 TiB and 4 TiB disks             | **Contested.** One group, named `group-1-nvme-2x2T` for the first worker's capacity. See §14, gap G-13     | `CM`    |
| PCI-14 | 3 workers, the third holding 3 of the 4 slots the others hold      | 2 groups: the address set matches whole or not at all, never as a subset                                   | `CM`    |

## 6. Fleet size and grouping

| ID       | Mutation                                                    | Expected                                                                   | Harness |
|----------|-------------------------------------------------------------|----------------------------------------------------------------------------|---------|
| FLEET-01 | 1 worker, 4 disks                                           | 1 group of 1                                                               | `CM`    |
| FLEET-02 | 3 uniform workers                                           | 1 group of 3. The summary reads 3 workers, 12 devices, 1 group, 1 node set | `CM`    |
| FLEET-03 | 32 uniform workers                                          | 1 group of 32, under the 200-worker ceiling                                | `CM`    |
| FLEET-04 | No reports at all                                           | The run fails: the probe reports are gone                                  | `CM`    |
| FLEET-05 | 32 workers, 3 with every disk mounted                       | 29 workers drafted. 3 worker refusals with their device reasons folded in  | `CM`    |
| FLEET-06 | The same 32 reports, ConfigMaps listed in a different order | A byte-identical document                                                  | `CM`    |
| FLEET-07 | Two ConfigMaps carrying a report for one node               | One report per node. The draft names the worker once                       | `CM`    |
| FLEET-08 | A report for a node absent from `status.workers`            | Skipped without an event: it is not this run's evidence                    | `CM`    |

## 7. Network interfaces

The draft names one management interface per group, and the rule is a ladder
rather than a match, because a fleet's machines do not agree on what their NICs
are called. What `ManagementInterface` applies today, in order:

1. Refuse a kind nothing can be bound to: loopback, and a virtual device the
   kernel does not identify, which is every veth and dummy on the machine.
2. Refuse anything whose state is neither `up` nor `unknown`.
3. Refuse anything holding no reachable address, which is to say link-local,
   loopback, and unspecified addresses do not count.
4. Refuse a bridge or an overlay that does not hold the node's own address.
   Both kinds can be bound and both are ordinarily somebody else's network: a
   CNI bridge and a flannel overlay carry the pod network, and a hypervisor
   host's bridge carries the address the cluster reaches the machine on. Which
   of the two a given device is can be read from nothing but the address on it.
5. Of what survives, the interface holding the node's `InternalIP` wins
   outright, whatever kind it is.
6. Failing that, the fastest link, with the simpler kind breaking that tie and
   the lowest name breaking the rest. A bond and a VLAN report no speed of
   their own, so the speed is resolved down the stack: an aggregate carries the
   sum of its members, and a derived interface carries what its parent carries.

Rungs 1, 4, and 6 are new, and they are what the interface kind in report
version 4 bought. Before it, every software interface read as "virtual" and was
refused, so a bonded or tagged management network, which is the ordinary
enterprise host, yielded no interface on any worker.

Two things the ladder does not cover, and both are inputs rather than readings:
which interface a fleet means for management, and which interfaces it means for
the data plane. Neither is expressible today (see `G-23` and `G-24`), so the
rows for them record what a run does in their absence.

### Where each rung came from

No design document specifies any of this. `design-clusterdeploymentconfig.md`
shows `mgmtInterface: eth1` in an example and states no rule for arriving at it,
so the ladder was assembled from a control-plane failure and from judgment. The
two are not the same thing, and a fixture whose outcome turns on the second is
recording a proposal rather than an expectation.

| Rung                                                        | Where it came from                                                                                                               | Status             |
|-------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------|--------------------|
| A node with no management address is refused                | The control plane's own refusal, "No management interface with IP found in provided interfaces" (commit 69e9f3f2)                | Grounded           |
| The interface holding the node's `InternalIP` wins outright | The operator addresses the worker by that address everywhere else, so any other choice splits one deployment across two networks | Grounded           |
| A link that is down is passed over                          | A link that is down keeps its address and carries nothing                                                                        | Grounded           |
| A link-local address does not count                         | It is configured without anybody assigning it and routes nowhere                                                                 | Grounded           |
| The cluster's own plumbing never wins                       | Stated as intent in commit 69e9f3f2. It was implemented as "anything virtual," which is what `G-21` records as wrong             | Grounded as intent |
| The lowest name breaks a tie                                | Two runs over one fleet have to produce the same document. Which tiebreak is arbitrary; that there is one is not                 | Grounded           |
| **The fastest link wins when no node address matches**      | Asserted in commit 69e9f3f2 with no reason given                                                                                 | **Unratified**     |
| **Which kinds can be bound at all**                         | Added with the kind reading. Loopback and an unidentified virtual device are safe to refuse; admitting the rest is a choice      | **Unratified**     |
| **A bridge or an overlay only with the node's own address** | Added with the kind reading                                                                                                      | **Unratified**     |
| **An aggregate carries the sum of its members**             | Added with the stack resolution                                                                                                  | **Unratified**     |
| **A simpler kind breaks a speed tie**                       | Added with the stack resolution                                                                                                  | **Unratified**     |

Twenty of the forty-two cases in this section carry no node address on a
candidate interface, so an unratified rung decides them. Their recorded
documents are evidence that the rule was applied consistently and no evidence
that the rule is right.

The question each unratified rung is really asking:

1. **Is the fastest link the management interface?** Management traffic is
   light, and the fastest link is what a data path wants. Naming it for
   management may be taking the wrong NIC for the wrong plane, which is what
   `NET-24` records. The alternatives are the slowest addressed link, the lowest
   name outright, and refusing to choose at all where a fleet has not said.
2. **Should a run choose at all when nothing identifies the interface?** A draft
   naming the wrong NIC is approved as readily as one naming the right one. The
   alternative is to name none and make the reviewer say, which trades a wrong
   document for one that cannot be approved unedited.
3. **Is a bridge holding the node's address the management interface, or a
   machine a reviewer should look at?** It is an ordinary hypervisor host and it
   is also what a misconfigured worker looks like.

| ID     | Mutation                                                                              | Expected                                                                                                                                                                | Harness |
|--------|---------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| NET-01 | One 10G interface, up, holding a routable address                                     | `mgmtInterface` names it                                                                                                                                                | `CM`    |
| NET-02 | A 1G and a 10G interface, both up and addressed, no node address on either            | The 10G is named                                                                                                                                                        | `CM`    |
| NET-03 | The node's `InternalIP` on the 1G while the 10G is faster                             | The 1G is named: the node address wins outright                                                                                                                         | `CM`    |
| NET-04 | Four interfaces, 1G and 10G, all unusable: down, bridged, or link-local only          | `mgmtInterface` empty and the draft still written. See §14, gap G-7                                                                                                     | `CM`    |
| NET-05 | `interfaces` absent from the report                                                   | The same: an empty interface and a written draft                                                                                                                        | `CM`    |
| NET-06 | Two workers with identical disks calling their NIC `eth0` and `ens5f0`                | 2 groups, which the draft validation then reports as unbuildable. See §14, gap G-25                                                                                     | `CM`    |
| NET-07 | Two 10G interfaces, `eth1` and `eth0`, both addressed                                 | `eth0` is named. A second run names it again                                                                                                                            | `CM`    |
| NET-08 | An addressed physical interface reporting speed 0 beside a 10G interface that is down | The addressed one is named                                                                                                                                              | `CM`    |
| NET-09 | A worker whose `InternalIP` sits on a bridge, as a hypervisor host's does             | `br0` named. A bridge carrying the cluster's own address is the management network                                                                                      | `CM`    |
| NET-10 | One interface holding only a global IPv6 address                                      | Named: reachability is not address-family dependent                                                                                                                     | `CM`    |
| NET-11 | An interface holding both a link-local and a routable address                         | Named on the routable one                                                                                                                                               | `CM`    |
| NET-12 | One interface and it is the loopback                                                  | `mgmtInterface` empty: loopback is refused by its ARPHRD type, not its name                                                                                             | `CM`    |
| NET-13 | `bond0` holding the node address over two unaddressed physical slaves                 | `bond0` named, with `eth0` and `eth1` reported as the hardware under it                                                                                                 | `CM`    |
| NET-14 | `eth0.100` holding the node address, the parent `eth0` unaddressed                    | `eth0.100` named, resolving to `eth0` for its slot and memory node                                                                                                      | `CM`    |
| NET-15 | `vxlan.calico` holding an overlay address beside `eth0` holding the node address      | `eth0` named. The overlay is refused for holding an address the cluster does not reach the machine on                                                                   | `CM`    |
| NET-16 | `bond0.100` over `bond0` over two slaves, the node address on the VLAN                | `bond0.100` named, resolving through the bond to both NICs                                                                                                              | `CM`    |
| NET-17 | A `macvlan` and an `ipvlan` over `eth0`, all three addressed                          | `eth0` named: the three carry the same traffic and the physical one is the simplest                                                                                     | `CM`    |
| NET-18 | Twelve `veth` interfaces holding pod-CIDR addresses beside one physical NIC           | The physical NIC named, whatever the veth count                                                                                                                         | `CM`    |
| NET-19 | `cni0` and `docker0` both addressed, one physical NIC with no address                 | `mgmtInterface` empty: a bridge is refused and an unaddressed NIC is not a candidate                                                                                    | `CM`    |
| NET-20 | An interface whose state is `dormant`, and one whose state is `lowerlayerdown`        | Both passed over: only `up` and `unknown` are admitted                                                                                                                  | `CM`    |
| NET-21 | An interface whose state is `unknown` holding a routable address                      | Named. A driver that does not track carrier is not a reason to refuse it                                                                                                | `CM`    |
| NET-22 | A run told that `ens5f0` is the management interface                                  | **Contested.** No input expresses it, so the ranking decides regardless. See §14, gap G-23                                                                              | `CM`    |
| NET-23 | A run told that `ens5f1` and `ens5f2` are the data NICs                               | **Contested.** `dataInterfaces` is never written, so the draft leaves the data plane unnamed. See §14, gap G-24                                                         | `CM`    |
| NET-24 | Two physical NICs, a 10G and a 100G, both up and addressed                            | The 100G named for management and no data NIC named at all, so the fast link is proposed for the wrong plane                                                            | `CM`    |
| NET-25 | Two otherwise equal 10G NICs at MTU 9000 and MTU 1500                                 | The lower name wins. MTU is reported and not ranked on                                                                                                                  | `CM`    |
| NET-26 | A 100G bridge beside a 10G physical NIC, both addressed                               | The 10G named: the kind ladder is decided before the speed                                                                                                              | `CM`    |
| NET-27 | An interface reporting state `up` with no link partner                                | Named: the report drops `carrier`, so a link with no partner reads as a working one. See §14, gap G-26                                                                  | `CM`    |
| NET-28 | A bond reporting no speed over two 25G NICs, beside an addressed 10G NIC              | The bond named: an aggregate carries the sum of its members                                                                                                             | `CM`    |
| NET-29 | A VLAN reporting no speed over that bond, beside the same 10G NIC                     | The VLAN named: a derived interface carries what its parent carries                                                                                                     | `CM`    |
| NET-30 | The chosen bond's members in sockets 0 and 1                                          | The memory node is reported as unknown rather than as one of the two                                                                                                    | `CM`    |
| NET-31 | A veth holding the node's own address                                                 | Nothing named: an unidentified virtual device is refused at the first rung, node address or not                                                                         | `CM`    |
| NET-32 | A bond whose `lower` names a second bond whose `lower` names the first                | The bond is named and the resolution terminates                                                                                                                         | `GO`    |
| NET-33 | The node's `InternalIP` on a link whose state is `down`                               | Nothing named: the state rung is applied before the address wins, so a link carrying nothing is refused however right its address is                                    | `CM`    |
| NET-34 | The node's `InternalIP` on both `bond0` and `bond0.100`                               | The first by the report's own order, which is the kernel's and is ascending by name, so two runs agree                                                                  | `CM`    |
| NET-35 | A bond reporting 50000 Mbps of its own over two 25G members                           | Its own reading is taken, and the members are not summed on top of it                                                                                                   | `CM`    |
| NET-36 | A bond whose `lower` names an interface the report does not carry                     | Named, with no members and no resolved speed: a member nothing describes contributes nothing                                                                            | `CM`    |
| NET-37 | A `team` interface over two 25G NICs, holding the node address                        | Named and resolved as a bond is, both being aggregates                                                                                                                  | `CM`    |
| NET-38 | A bridge over a bond over two NICs, the node address on the bridge                    | Named, and the members resolve two levels down to the two NICs                                                                                                          | `CM`    |
| NET-39 | A `macvlan` over `eth0` holding the node address                                      | Named, with `eth0` as its member and `eth0`'s speed as its own                                                                                                          | `CM`    |
| NET-40 | An interface carrying no `kind`, and nothing marking it virtual                       | Read as physical, which is what the fields before the kind said about it                                                                                                | `CM`    |
| NET-41 | A worker whose only fast NIC is a 2x25G bond, ranked for placement                    | **Contested.** The memory node's fastest NIC reads as 0 Mbps: the placement reads the raw speed and the raw memory node, neither of which a bond has. See §14, gap G-27 | `CM`    |
| NET-42 | Any successful run on a bonded host                                                   | **Contested.** The draft names the interface and says nothing about the slots, the speed, or the memory node resolved under it. See §14, gap G-28                       | `CM`    |

## 8. Node roles and node sets

| ID      | Mutation                                                 | Expected                                                              | Harness |
|---------|----------------------------------------------------------|-----------------------------------------------------------------------|---------|
| ROLE-01 | 3 workers, no role labels                                | One node set, `discovered`                                            | `CM`    |
| ROLE-02 | 3 `infra` and 3 `worker` nodes, identical hardware       | 2 node sets, `infra` first, one hardware group split across them      | `CM`    |
| ROLE-03 | A `control-plane` node with free disks beside 2 workers  | A third node set, `control-plane`, ordered last                       | `CM`    |
| ROLE-04 | A node labeled `infra` and `worker`                      | `infra`                                                               | `CM`    |
| ROLE-05 | A node labeled `control-plane` and `worker`              | `control-plane`: the most restrictive role wins                       | `CM`    |
| ROLE-06 | A node labeled `master`                                  | `control-plane`: both spellings are read                              | `CM`    |
| ROLE-07 | No `nodes.yaml` at all                                   | Every worker a `Worker`. The interface is chosen with no address hint | `GO`    |
| ROLE-08 | A cordoned node and a tainted node, both with free disks | Both reach the draft. See §14, gap G-8                                | `CM`    |
| ROLE-09 | A node carrying an unrecognized role label               | Treated as a worker: an unknown role is not a refusal                 | `CM`    |
| ROLE-10 | 32 workers across `infra`, `worker`, and `control-plane` | 3 node sets, each holding its role's share of every hardware group    | `CM`    |

## 9. Filters

| ID      | Mutation                                                       | Expected                                                                                              | Harness |
|---------|----------------------------------------------------------------|-------------------------------------------------------------------------------------------------------|---------|
| FILT-01 | `pcieDenyList` naming the boot slot on a uniform fleet         | That address in no group. The refusal appears in `Explain`, not pre-filtered                          | `CM`    |
| FILT-02 | `pcieAllowList` of 2 addresses against 10 disks                | Only those 2 reach the group                                                                          | `CM`    |
| FILT-03 | `pcieModel: MZQL2` against a mixed-model worker                | Only the matching disks. The refusal quotes both strings                                              | `CM`    |
| FILT-04 | `driveSizeRange: 1T-4T` with a 512 GiB disk present            | The small disk refused, the range quoted in the reason                                                | `CM`    |
| FILT-05 | `driveSizeRange: 2T` against 2 TiB and 1.92 TB disks           | Only the exact 2 TiB disks: a bare size is both bounds                                                | `CM`    |
| FILT-06 | `driveSizeRange: 2T-1T`                                        | The run is refused, naming the field and what a range looks like. Admission refuses it at the request | `CM`    |
| FILT-07 | `blockDenyList: /dev/sda` on a block run                       | The root disk in no group                                                                             | `CM`    |
| FILT-08 | `blockAllowList` of 2 paths against 6 block devices            | Only those 2                                                                                          | `CM`    |
| FILT-09 | `enablePartitionedDevices` with a GPT-only refusal             | Admitted. A disk also mounted stays refused                                                           | `CM`    |
| FILT-10 | A filter that matches nothing on any worker                    | No node sets. The run fails naming the filter rule per worker                                         | `CM`    |
| FILT-11 | An allow list written in uppercase against lowercase addresses | Matched: the comparison folds case                                                                    | `CM`    |
| FILT-12 | One address in both the allow and the deny list                | Refused: deny is evaluated first                                                                      | `CM`    |
| FILT-13 | `driveSizeRange` on a block run                                | It narrows the block class, which is the class being scanned                                          | `CM`    |
| FILT-14 | No `deviceFilter` at all                                       | Every free whole NVMe disk reaches the draft                                                          | `CM`    |

## 10. Probe-refused devices and userspace controllers

| ID      | Mutation                                                           | Expected                                                                                                                           | Harness |
|---------|--------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------|---------|
| HELD-01 | Every disk mounted                                                 | The worker refused. Its device reasons folded into one line, counted                                                               | `CM`    |
| HELD-02 | Every disk held in a device-mapper stack                           | The same shape with the other reason                                                                                               | `CM`    |
| HELD-03 | 4 NVMe controllers on `uio_pci_generic`, idle, no block devices    | 4 synthesized devices, group `group-1-nvme-4xunsized`                                                                              | `CM`    |
| HELD-04 | The same controllers with `inUse: true`                            | No devices. The worker refused with the message saying something is driving its disks                                              | `CM`    |
| HELD-05 | 2 idle userspace controllers beside 2 kernel-presented disks       | 4 addresses in the group, the kernel-bound pair counted once                                                                       | `CM`    |
| HELD-06 | An idle controller on `vfio-pci`                                   | Claimable on the same terms as `uio_pci_generic`                                                                                   | `CM`    |
| HELD-07 | A kernel-bound controller whose block device is already reported   | Named once, not twice                                                                                                              | `CM`    |
| HELD-08 | Idle userspace controllers on a block run                          | No devices at all: a controller with no block device has no path. Worker refused                                                   | `CM`    |
| HELD-09 | 16 loopback devices and 4 free disks                               | The 4 disks drafted. The loopbacks absent from `Explain` and present in `RefusalLines`                                             | `CM`    |
| HELD-10 | Half the controllers idle and half in use, no block devices        | Only the idle half is offered                                                                                                      | `CM`    |
| HELD-11 | Userspace controllers whose process table the probe could not read | Refused, and the message says whether anything is driving them could not be established. An unchecked controller is not a free one | `CM`    |

## 11. Refusal cases

| ID      | Mutation                                                                   | Expected                                                                          | Harness |
|---------|----------------------------------------------------------------------------|-----------------------------------------------------------------------------------|---------|
| FAIL-01 | Every worker reports no devices and no controllers                         | The run fails: no worker has a device this run would use, one line per worker     | `CM`    |
| FAIL-02 | Every disk in the fleet mounted                                            | The same, each line carrying the device reasons and their counts                  | `CM`    |
| FAIL-03 | Every report unreadable                                                    | `ReportUnreadable` per ConfigMap, then the run fails for want of reports          | `CM`    |
| FAIL-04 | A filter that excludes every disk                                          | The run fails. The refusal lines name the filter rule                             | `CM`    |
| FAIL-05 | Disks fine, no usable interface anywhere                                   | **Contested.** A draft is written with an empty `mgmtInterface`. See §14, gap G-7 | `CM`    |
| FAIL-06 | Every worker under the vCPU floor                                          | **Contested.** A draft is written with `vcpuCount: 4`. See §14, gap G-10          | `CM`    |
| FAIL-07 | Every worker at 4 GiB of RAM                                               | **Contested.** A draft is written unchanged. See §14, gap G-4                     | `CM`    |
| FAIL-08 | A single worker whose CPU tree is unreadable, disks fine                   | A draft with `vcpuCount: 4` and the note saying no worker reported its cores      | `CM`    |
| FAIL-09 | One worker of 32 refused, the rest fine                                    | A draft of 31. The refusal is an event, never a failure                           | `CM`    |
| FAIL-10 | A worker whose only disks are partitions, `enablePartitionedDevices` unset | Refused by `whole disk`, pre-filtered, so `Explain` gives the worker line alone   | `CM`    |

## 12. Report and ConfigMap validity

| ID    | Mutation                                                                 | Expected                                                                  | Harness |
|-------|--------------------------------------------------------------------------|---------------------------------------------------------------------------|---------|
| CM-01 | `version: 4` in one report of three, the schema before the subsystem NQN | `ReportUnreadable` naming both versions. The other two are drafted        | `CM`    |
| CM-02 | A ConfigMap with no `report.json` key                                    | `ReportUnreadable` saying the probe did not finish writing it             | `CM`    |
| CM-03 | `report.json` holding malformed JSON                                     | `ReportUnreadable` quoting the parse failure                              | `CM`    |
| CM-04 | A report with an empty `node`                                            | Refused: nothing can be attributed to it                                  | `CM`    |
| CM-05 | A ConfigMap without the run label                                        | Not listed, so not read                                                   | `CM`    |
| CM-06 | A report from a node whose name exceeds 63 characters                    | Drafted under its full name: the label is truncated and the report is not | `CM`    |
| CM-07 | A report of a 128-device worker near the ConfigMap ceiling               | Decoded and drafted                                                       | `CM`    |

## 13. Document shape and the cluster template

| ID      | Mutation                                                      | Expected                                                                                          | Harness |
|---------|---------------------------------------------------------------|---------------------------------------------------------------------------------------------------|---------|
| TMPL-01 | Any successful run                                            | `spec.approved` false, `enableDriveFormat` true, and the note about formatting                    | `CM`    |
| TMPL-02 | Any successful run                                            | `maxSubsystemCount: 30` with the note saying it is not a reading                                  | `CM`    |
| TMPL-03 | `spec.discover.configName` unset                              | The name is `discovered-<ops>` and the cluster `discovered-<ops>-cluster`                         | `CM`    |
| TMPL-04 | An `OperatorOps` name long enough to push the cluster past 63 | **Contested.** The document is written and `CreatingCluster` can never succeed. See §14, gap G-11 | `CM`    |
| TMPL-05 | `spec.discover.clusterRef` set                                | No `spec.cluster`, and `clusterRef` carried through with the growth note                          | `CM`    |
| TMPL-06 | Any run on a two-socket fleet                                 | `socketsToUse` and `nodesPerSocket` unset, so one node per worker on socket 0. See §14, gap G-12  | `CM`    |
| TMPL-07 | `spec.environment` from the run's status                      | Copied verbatim into the document                                                                 | `CM`    |
| TMPL-08 | Any run                                                       | No `deviceFilter` anywhere in the document: the resolved list is the record                       | `CM`    |

---

## 14. Gaps and contested expectations

Each is a case above whose expected value is what the code does rather than what
the design says, or a behavior no case can assert because nothing implements it.
A fixture is still written for every one. The row records today's output so that
a later change to it is visible as a diff rather than a surprise.

| #    | Finding                                                                                                                                                                                                                                                                                                                                         | Cases                  |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------|
| G-1  | **Fixed.** `ClassRule` checked the transport on the NVMe side only, so a block run admitted an NVMe disk by its path and a draft could name both classes. A cluster is built out of one class, so the check is symmetric now: a block run refuses `NVMe` and `NVMeFabric` alike                                                                 | DEV-05, DEV-07         |
| G-2  | Nothing reads `Rotational`, so a spinning disk is offered to a cluster on the same terms as an SSD                                                                                                                                                                                                                                              | DEV-08                 |
| G-3  | `vcpuCount` is the chosen node's whole physical core count, leaving nothing for the system on a large worker                                                                                                                                                                                                                                    | SIZE-03                |
| G-4  | No worker rule reads memory: a 4 GiB machine is drafted as a storage node                                                                                                                                                                                                                                                                       | SIZE-06, FAIL-07       |
| G-5  | Swap is reported and unread, though a host swapping is already oversubscribed                                                                                                                                                                                                                                                                   | SIZE-14                |
| G-6  | The generator does not hold an address to the schema's PCI pattern, so a five-digit domain produces a document the API server refuses                                                                                                                                                                                                           | PCI-07                 |
| G-7  | No worker rule requires a management interface, so the generator writes a group it already knows cannot deploy. The `ClusterDeploymentConfig` controller's draft validation does report it as `NoManagementInterface`, one step and one object later than the run that produced it                                                              | NET-04, FAIL-05        |
| G-8  | A cordoned or tainted node reaches the draft, because the role labels are read and `Unschedulable` is not                                                                                                                                                                                                                                       | ROLE-08                |
| G-9  | **Fixed.** An unreadable `driveSizeRange` dropped the rule and admitted every disk, and the comment claimed a validating caller reported it. There is one now: an admission webhook on `OperatorOps` refuses the run at the request, and both steps that read the filter refuse it again for a cluster whose webhook is not installed           | FILT-06                |
| G-10 | A fleet under the vCPU floor is drafted at the floor, so approval produces nodes the workers cannot host                                                                                                                                                                                                                                        | SIZE-02, FAIL-06       |
| G-11 | The cluster name is not checked against its own 63-character limit when derived                                                                                                                                                                                                                                                                 | TMPL-04                |
| G-12 | The generator chooses one memory node per worker and proposes no `socketsToUse`, so the second socket of every dual-socket worker is left out of the cluster entirely                                                                                                                                                                           | TMPL-06, NUMA-14       |
| G-13 | A group's signature is its addresses and its interface, so workers of differing capacity share a group and the group is named for whichever of them sorts first. Verified against the code                                                                                                                                                      | PCI-13                 |
| G-14 | `minHugePagesSize` is the floor on the pool simplyblock allocates for itself, and the generator sets it to the free pages already on the chosen node, so another workload's reservation becomes this cluster's allocation floor                                                                                                                 | SIZE-09, SIZE-16       |
| G-15 | A worker with nothing pre-allocated leaves the field unset, on the stated grounds that SPDK consumes pages and does not reserve them. simplyblock allocates its own, so an empty pool is the ordinary starting state. The same premise heads `atlas-lib/inventory/hugepages.go`                                                                 | SIZE-10, SIZE-17       |
| G-16 | Nothing checks that the host has memory for the allocation on top of the existing pool. `memory.availableBytes`, `memory.hugePagesBytes`, and the per-node `freeBytes` are all reported and all unread                                                                                                                                          | SIZE-16, SIZE-17       |
| G-17 | No input says to take a stated number of the pre-allocated pages. `spec.discover` carries no huge-page field, and the only two knobs that exist are never written by a run: the cluster's `minHugePagesSize` and a group's `spdkSystemMemory`                                                                                                   | SIZE-18                |
| G-18 | `hugePagesOf` drops `resv_hugepages`, which `inventory.HugePagePool` reads, so a draft cannot tell pages promised to a mapping from pages genuinely free                                                                                                                                                                                        | SIZE-19                |
| G-19 | A kernel with no hugetlbfs and a host whose pools are fully taken produce the same unset field and the same note, so a reviewer cannot tell a machine that cannot hold huge pages from one that has none left                                                                                                                                   | SIZE-21                |
| G-20 | The cluster's field is documented as a floor and rendered into the per-node configuration as `MAX_HUGE_PAGES_SIZE`. Which of the two the node honors decides what a run should propose                                                                                                                                                          | SIZE-09, SIZE-16       |
| G-21 | **Fixed.** `servesManagement` refused every virtual interface, and a bond, a VLAN, a VXLAN, and a macvlan are all virtual, so a fleet whose management address sat on a bond or a VLAN yielded no interface on any worker. The ladder now refuses a kind rather than the absence of hardware                                                    | NET-13, NET-14, NET-16 |
| G-22 | **Fixed.** The report carried `virtual`, `loopback`, and `bridge` and nothing else, so a bond, a VLAN, a VXLAN, and a veth were indistinguishable in it. `inventory.Interface` now reads the device type the kernel publishes and the `lower_*` and `upper_*` links around it, and the report carries `kind`, `lower`, and `upper` at version 4 | NET-13, NET-14, NET-17 |
| G-23 | No input predefines the management interface. `DiscoverSpec` carries no interface field, so the ranking cannot be overridden for a fleet that knows which NIC it means                                                                                                                                                                          | NET-22                 |
| G-24 | No run writes `dataInterfaces`. Every drafted document leaves the data plane unnamed, and the fastest link is proposed for management instead                                                                                                                                                                                                   | NET-23, NET-24         |
| G-25 | The grouper splits workers on their management interface while the expansion binds one interface per cluster, so a fleet whose machines name their NICs differently produces exactly the document `conflictingInterfaces` reports as unbuildable                                                                                                | NET-06                 |
| G-26 | `interfacesOf` drops `carrier` and `duplex`, which `inventory.Interface` reads, so a link that is up with no partner is not distinguishable from one carrying traffic                                                                                                                                                                           | NET-27                 |
| G-27 | `NUMANodeBreakdown` ranks a memory node's NICs by the raw `speedMbps` and the raw `numaNode`, and a bond has neither. A worker whose data path is bonded or tagged therefore counts as having no NIC at all on every memory node, though the resolution that would answer both now exists                                                       | NET-41                 |
| G-28 | `Management` carries the members, the resolved speed, and the memory node onto `Worker.Mgmt`, and nothing reads them. No note, field, or event says which slots a bonded management network lands in, so the evidence is gathered and not reported                                                                                              | NET-42                 |

## 15. Generation order

The fixtures come first and whole. Every case in this document is a directory of
input objects, and the inputs are a statement of what the fleet was, which is
settled by the row rather than by anything the harness does with it. Writing the
complete set before any code reads it keeps the two apart: what is being tested
is reviewable as a tree of hosts, and the harness is then written against a set
that is already fixed rather than growing to fit the cases it happens to load.

1. **Every case directory, all of them, inputs only.** `ops.yaml`, `nodes.yaml`,
   `reports/*.yaml`, and the `case.md` naming the row. This is the bulk of the
   work and none of it depends on a harness existing.
2. **Review the tree.** A family at a time, against its section here. A wrong
   fixture is a test that passes for the wrong reason, and it is far cheaper to
   catch in a directory of YAML than in a golden file.
3. **The harness**, walking the tree and driving each case through the run. It
   is written once, against the whole set, and a case it cannot load is a gap in
   the harness rather than a case to drop.
4. **The expected outputs.** For a row stating a value, written by hand from the
   row. For the rest, recorded from a run and then read against the row before
   being committed, because a golden file nobody read is a record of what the
   code did rather than of what it should do.
5. **The `GO` rows**, in the `discovery` package, since they substitute a
   `Planner` seam the controller never does and have no directory.

The contested rows are written like any other and keep their gap number in
`case.md`. They record today's output deliberately, so that the day one of them
changes, the diff is the finding rather than a surprise.
