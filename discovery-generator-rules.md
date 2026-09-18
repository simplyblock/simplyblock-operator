<!-- Every rule the discovery generator applies between a fleet's probe reports and the
     ClusterDeploymentConfig a reviewer approves: the precedence between them, the exact
     condition each turns on, and where each rule came from. Written for validation, because
     the generator's recorded expectations were produced by running the generator and
     therefore prove consistency rather than correctness. -->

# The rules the discovery generator applies

**What this is.** Every rule between a fleet's probe reports and the drafted
`ClusterDeploymentConfig`, with its precedence, its exact condition, and its
provenance.

**Why it exists.** The 155 recorded expectations under
`operator/internal/controllers/deployment/testdata/discovery/` were produced by
running the generator over the fixtures. That proves the rules were applied
consistently and proves nothing about whether they are the right rules. Several
were never specified anywhere: they were settled in code, and the expectations
now carry them as though they were agreed. This is the list to ratify, correct,
or overrule.

**How to read it.** Every rule carries a provenance marker, and the markers are
the point.

| Marker      | Meaning                                                                                                                                                      |
|-------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SPECIFIED` | A design document states the rule, and the code implements what was written                                                                                  |
| `GROUNDED`  | No design states it, and the code cites a constraint outside itself: a control-plane refusal, an API server limit, an addressing requirement, or determinism |
| `ASSERTED`  | No design states it, and the reason the code gives is a preference rather than a constraint                                                                  |
| `INVENTED`  | No design states it and no reason is given. It was settled while the code was written                                                                        |
| `UNBUILT`   | A design states the rule and the code does not implement it                                                                                                  |

A `SPECIFIED` or `GROUNDED` rule needs checking for accuracy. An `ASSERTED` or
`INVENTED` rule needs a decision. §10 collects the second kind.

## 1. The pipeline, in order

A run is three steps, and only the third produces a document.

1. **Inspecting** settles which workers the run is about and which distribution
   the cluster runs. Nothing after it re-derives either.
2. **Probing** starts one Job per worker, each writing a report into a
   ConfigMap. The probe decides nothing: it reports every block device, refused
   ones included, with the grounds for each.
3. **Writing** turns the reports into a document.

Writing is itself ordered, and the order is load-bearing.

```
reports, sorted by worker name
  └─ per worker:
       1. synthesize a device for every idle userspace NVMe controller   §4.4
       2. apply the device rules, first refusal wins                     §4.1
       3. apply the worker rules, first refusal wins                     §4.3
       4. choose which memory node to place on                           §5
       5. record every admitted device the placement did not take        §5.5
       6. choose the management interface                                §7
  └─ over the workers that survived:
       7. group them by what they hand over                              §6.1
       8. split the groups into node sets by role                        §6.4
  └─ over the plan:
       9. derive the cluster block                                       §8
      10. assemble, name, and write the document                         §9
```

Devices are filtered before workers because whether a worker is worth including
is mostly a question about what is left of it. `GROUNDED`.

## 2. Which workers the run is about

Decided once, in Inspecting, and persisted. `GROUNDED`: a worker that joins the
cluster while the probes run is not in this run's draft, and re-listing in
Probing would put it there with no report to describe it.

A node is a candidate when all four hold:

| #   | Condition                                                        | Provenance  |
|-----|------------------------------------------------------------------|-------------|
| 1   | It matches `spec.discover.nodeSelector`, when one is set         | `SPECIFIED` |
| 2   | No `StorageNode` in the namespace already names it               | `SPECIFIED` |
| 3   | It is schedulable                                                | `GROUNDED`  |
| 4   | Its role holds storage nodes, or the control-plane opt-in is set | `GROUNDED`  |

**Schedulable** means not cordoned and carrying no `NoSchedule` or `NoExecute`
taint. `PreferNoSchedule` does not exclude. `GROUNDED`: a probe Job is pinned
with `spec.nodeName` and would run on either, and a worker the cluster is not
scheduling to is not one to hand to a storage cluster.

**Role** is read from `node-role.kubernetes.io/<role>` labels, and the most
restrictive wins: control-plane, then infra, then worker. Both `master` and
`control-plane` are read, neither preferred. An unrecognized role is ignored
rather than refused. Infra nodes are used **without** an opt-in, control-plane
nodes only with one. `GROUNDED`, and the asymmetry is argued: an OpenShift infra
node is the tier a cluster's own infrastructure runs on, and simplyblock storage
is infrastructure.

A declined worker gets an event naming the reason. A node already carrying a
`StorageNode` is the one exclusion that stays **silent**, against the adjacent
comment claiming both exclusions are otherwise explained.

**Environment** is concluded once from the whole node list, not per probe.
`SPECIFIED`. Signals, strongest first: registered API groups, then the kubelet
version string, then the OS image prefix, then node labels and annotations. When
several distributions match, precedence is OpenShift, Talos, K3s, Rancher.
`GROUNDED`: a Rancher-managed K3s cluster carries both sets of markers, and what
decides the host's shape is the distribution that installed the kubelet. Nothing
matching yields `Vanilla`. A failure to list API groups is an event, not a
failure, since half the evidence still yields a conclusion.
## 3. What the probe decides, and what it refuses to decide

The probe decides nothing about what a cluster should use. `SPECIFIED`, and the
rule is load-bearing for everything below it: every block device is reported,
refused ones included, each with the grounds it was refused on, because the run's
device filter belongs to the operator that holds the fleet's reports and an
administrator has to be able to be told why the disk they expected is not a
candidate.

Three consequences the generator depends on:

- **The probe accumulates grounds, the generator stops at the first.** A device
  arrives carrying every reason it is unusable, and is refused for one of them.
  That is what makes the partition waiver safe: it admits a device whose **only**
  ground is a partition table, so a boot disk stays out on its mount.
- **The report is a wire format with a version, and a reader refuses a version it
  does not know.** `GROUNDED`: a Job keeps the image it started with, so an
  operator upgraded mid-run reads reports from the previous probe, and a field
  that changed meaning would be read as the current one.
- **The report is the only place a node's name survives exactly.** Object names
  and labels are sanitized and truncated to fit a 63-byte budget, so the node a
  report is about is the field inside it.

What the probe reads and the generator never consults is itself a list worth
reviewing: per-node memory, online CPUs, swap, MTU, MAC address, driver, PCI
address of an interface, huge-page totals as distinct from free pages, and every
huge-page reading the kubelet publishes. None of them enters any rule.

## 4. Which devices reach the draft

Four stages, and a device dropped at one never reaches the next. Two of them are
the probe's and two are the generator's.

| Stage | Owner     | Decides                                           | Short-circuits          |
|-------|-----------|---------------------------------------------------|-------------------------|
| A     | probe     | Which sysfs entries exist, and their kind and bus | no                      |
| B     | probe     | Every ground a device is refused on               | accumulates all grounds |
| C     | generator | Admit or refuse each device                       | **first refusal wins**  |
| D     | generator | Admit or refuse the whole worker                  | **first refusal wins**  |

The probe accumulates every ground, and the generator stops at the first. A device
therefore carries all the reasons it is unusable and is refused for one of them.

### 4.1 The device rules, in evaluation order

| #   | Rule                 | Present when                                  | Pre-filter | Provenance |
|-----|----------------------|-----------------------------------------------|------------|------------|
| 1   | Whole disk           | always                                        | **yes**    | `GROUNDED` |
| 2   | Device class         | always                                        | **yes**    | `GROUNDED` |
| 3   | Available            | always                                        | no         | `GROUNDED` |
| 4   | Allow and deny lists | the scanned class's allow or deny list is set | no         | `ASSERTED` |
| 5   | Model                | NVMe run and `pcieModel` is set               | no         | `GROUNDED` |
| 6   | Size range           | `driveSizeRange` is set **and parses**        | no         | `ASSERTED` |

The order is stated and justified: what the device is, then whether it is free,
then whether this run wants it. "A partition refused as 'not in the allow list'
would be a true statement and the wrong one."

**Pre-filter** marks a rule whose refusal says nothing about the fleet's
storage. Pre-filtered refusals are emitted as events and are **excluded** from
the per-worker explanation the run's status carries. Only rules 1 and 2 set it.

### 4.2 Each rule's exact condition

**1. Whole disk**: refuses unless `kind == "Disk"`, exact and case-sensitive.
An empty kind refuses. `GROUNDED`: it is redundant against the probe, and it is
the one rule whose absence would be silent, because a partition admitted by a
waiver would be handed to a cluster as a disk.

**2. Device class**: symmetric, and it has to be, because a cluster is built out
of one class.

- On an **NVMe** run: refuses unless `transport == "NVMe"` exactly, then refuses
  unless the PCI address is non-empty.
- On a **block** run: refuses `NVMe` and `NVMeFabric` alike, then refuses unless
  the path is non-empty.

The block side used to check only the path, so an NVMe disk reached a block draft
by its path and one document could name both classes. Naming devices by path is
what the block class does rather than what makes a device one, so the bus is what
both sides read.

**2a. Simplyblock volume**: refuses a device whose subsystem NQN parses as a
simplyblock logical-volume subsystem, naming the cluster it belongs to.
`GROUNDED`: a volume this product exported and a worker attached is a namespace
like any other, and handing one back to a cluster as backend storage would give
a volume's own bytes away as free space.

It sits ahead of the class rule deliberately. The class rule refuses every
fabric namespace too, but as a pre-filter, so its answer never reaches the
explanation: a reviewer asking why a machine full of disks proposed none would be
told the disks are on a bus the run does not scan, rather than that they are the
fleet's own volumes. It is also the check that survives the class rule being
relaxed, which is the one way a cluster could be told to take its own bytes.

Parsing rather than matching a prefix is what makes it specific. A subsystem of
this product that is not a logical volume does not read as one, and a namespace
another product exported falls through to the class rule.

**3. Available**: admits a device the probe found free. One waiver, and it is
narrow: with `enablePartitionedDevices`, a device whose **only** ground is a
partition table is admitted. A device also mounted, held, or unreadable is not.
`GROUNDED`, and the narrowness is enforced on the probe's side too.

The refusal quotes the ground *names* joined by commas, not the details. A
device marked unavailable with no grounds at all, which is inconsistent data the
probe does not write, reads "the probe did not report it as available and gave no
reason."

**5. iSCSI**: refuses an iSCSI LUN the run's allow list does not name.
`GROUNDED`: every other bus a run scans is a cable inside the chassis, and a LUN
is a disk on the other side of a network, so a cluster built on one runs every
write of its data path over that network. Whether that is wanted is a question
about the deployment rather than about the hardware.

The default is refusal rather than proposal-and-review, because a draft
proposing a LUN is one a reviewer has to notice and strike, and a fifty-worker
document is not one anybody reads that closely. Naming it in the allow list is
the decision.

It reads the allow list itself rather than leaving it to the rule below, because
that rule is only built when a list was given: with no filter there would be
nothing to refuse a LUN, and with a list given for another reason a LUN would be
admitted by being named alongside everything else.

It also sits ahead of that rule so that an unnamed LUN says it is a LUN rather
than that it is not in the allow list, which is the same true-but-wrong-answer
the whole-disk ordering exists to avoid.

**6. Allow and deny lists**: deny is evaluated first and wins, and an **empty allow
list means allow everything** rather than allow nothing. Matching is case-folded.

Two things asserted and not justified: deny-before-allow, and the case folding.
The folding is defensible for a PCI address and is **wrong for a block path**:
`/dev/SDB` matches `/dev/sdb`, and Linux paths are case-sensitive.

**7. Model**: case-folded **substring** match. `GROUNDED`: a model string is
padded, versioned, and vendor-formatted, so `MZQL2` is what an administrator
writes. A device with an empty model is refused by any non-empty filter.

**8. Size range**: both bounds are **inclusive**, and a zero maximum means **no upper
bound**. Applies to whichever class is scanned.

### 4.3 The worker rules

**Has devices**: refuses a worker with nothing admitted, and says one of three
things. `GROUNDED` throughout, with the distinction spelled out: a machine whose
disks are on a userspace driver has no block devices at all, so "no device
survived the rules" is true and useless.

| Situation                                           | What the reviewer is told                 |
|-----------------------------------------------------|-------------------------------------------|
| Userspace-bound controllers, something driving them | something is driving its disks            |
| Userspace-bound controllers, nothing driving them   | the disks are there to be reclaimed       |
| Neither                                             | no device of it survived the device rules |

**Fully readable**: refuses a worker whose probe could not read everything. Off
by default, and **nothing in the operator ever turns it on**. `GROUNDED`: a
worker whose CPU tree could not be read still has disks worth reviewing.

### 4.4 Devices the generator invents

An NVMe controller bound to a userspace driver presents no block device, so it
cannot reach a draft through the device reading at all. The generator synthesizes
one for each such controller that **nothing is using** and whose address no
reported device already carries. `GROUNDED`, and the trade is stated: everything
the disk would have said about itself is lost, so it reaches the draft unsized
and uninspected, and the group it lands in is named for a count rather than a
capacity.

**Two interactions worth deciding on**, neither documented:

- A **model filter refuses every synthesized controller**, because its model is
  empty and the match is a substring.
- A **size range with any lower bound refuses every synthesized controller**,
  because its size is zero.

So a run that filters by model or by size silently excludes exactly the disks the
synthesis exists to offer.

### 4.5 Findings

**F-4.1. An unparsable size range silently admits everything, and the code says
otherwise.** `plan.go:281-283` states: "ParseSizeRange is called by the caller
that validates the spec, and this one skips what it cannot read rather than
silently widening the filter, and the run reports the parse failure separately."

No such caller exists. `ParseSizeRange` is called from exactly one place outside
its own tests. `driveSizeRange` carries no schema pattern and no CEL rule. So an
unparsable range drops the rule entirely, every size passes, and **nothing
reports it**: not a refusal, not an event, not the summary. This is `G-9`, and
it is worse than recorded: the comment asserts a safeguard that was never built.

**F-4.2. Eleven places drop a device or a worker with no record.** The ones that
matter:

- A userspace controller that is **in use** is skipped silently. The only trace
  is the worker-level message, and only if the worker ends up with nothing.
- A worker whose report cannot be decoded raises an event but produces **no
  refusal**, so it is absent from the explanation the status carries.
- A worker whose report names a node the run is not about is dropped in silence.
- ~~`blockAllowList` and `blockDenyList` are silently ignored when the
  planner's class and the filter disagree.~~ **Fixed.** The planner carried a
  class field beside the filter's own `enableLogicalBlockDevices`, so one fact
  had two statements and they could disagree: a planner told nothing scanned
  NVMe, read the PCI lists, and dropped the block lists on a branch that never
  ran. The field is gone, and the class is read from the filter, where it was
  always written.

**F-4.3. `InUse: false` is not distinguished from "never read."** The report's
own comment says a reader deciding whether to reclaim a controller "has to find
this report free of such an entry [in `unreadable`] first." Nothing performs that
check, and the rule that would, Fully readable, is off by default. A probe that
could not read the process table therefore yields controllers that look idle.

**F-4.4. Fixed: the explanation counts by rule.** It used to group on the
rendered sentence, and the allow-and-deny refusal embedded the address in its
sentence, so a hundred declined devices produced a hundred clauses. It now
groups on the rule the refusal already carries, and the allow-and-deny reason
names the list rather than the device, which the refusal holds separately. Where
one rule's reasons genuinely differ, a mounted disk against a partitioned one,
each is counted rather than dropped.

**F-4.5. Fixed: the bound is quoted as written, and a size is never rounded
up.** The bound used to be re-rendered from the parsed number, so a filter
naming `1920G` was quoted back as `1.875T`. The rule now carries the range as
the filter wrote it.

The renderer was worse than lossy. Four significant digits rounded, so a byte
under a tebibyte printed as `1024G`, which reads as more than the value, and its
own parser refuses every decimal it produced. A whole number of units is now
written exactly and parses back to the byte it came from, and anything else is
truncated to two decimals.
## 5. Placement: which part of a worker is used

A worker's admitted devices are grouped by the memory node they hang off, and
one of those groups is taken. The rest are left behind and recorded as refused.

**The buckets exist only where an admitted device is.** A memory node with 64
cores, 128 GiB of huge pages, and a 100 GbE port but no admitted device does not
appear in the ranking at all, and cannot be chosen. The CPU, huge-page, and NIC
readings are attached to buckets that already exist and are otherwise discarded.
`placement.go:139-171`. `INVENTED`, asserted by construction with no comment.

### 5.1 The ranking, in order

Applied only when two or more buckets exist. Every key is compared in turn until
one of them separates the two nodes.

| #   | Key                                       | Direction  | Provenance | Stated reason                                                                                                             |
|-----|-------------------------------------------|------------|------------|---------------------------------------------------------------------------------------------------------------------------|
| 1   | Admitted device **count**                 | descending | `GROUNDED` | Usable space is bounded by the erasure-coding stripe, which is a count of devices, so four small disks beat one large one |
| 2   | Combined **capacity** in bytes            | descending | `ASSERTED` | "Capacity breaks a tie in the count"                                                                                      |
| 3   | **Physical cores** of the node            | descending | `ASSERTED` | "Cores break a tie in capacity"                                                                                           |
| 4   | A real node before the **unknown bucket** | real first | `GROUNDED` | The bucket is numbered -1, so comparing ids alone would rank it above node 0                                              |
| 5   | **Node id**                               | ascending  | `GROUNDED` | Two runs against one worker have to choose the same node                                                                  |

`placement.go:101-120`.

Key 4 is the one worth reading twice. The unknown bucket's id is `-1`, and key 5
is ascending, so removing key 4 would make "no memory node in particular" win
every full tie against node 0. It is not merely theoretical: a worker whose CPU
topology could not be read has zero cores against every bucket, so keys 1 to 3
can all tie, and the comparison falls through to it.

**Two questions this ranking does not ask**, both recorded in the code as known
and both with a cost:

- Where the data NIC is. A node with four disks and no fast NIC is preferred
  over one with three and a 100 GbE port.
- Whether huge pages are reserved on the node. A node with the disks and no
  reserved memory cannot start SPDK at all, and this ranks it first.

### 5.2 What is read and not ranked on

| Reading                  | Carried as                            | In the key? |
|--------------------------|---------------------------------------|-------------|
| Online CPUs per node     | `NUMANodeResources.OnlineCPUs`        | No          |
| Free huge pages per node | `NUMANodeResources.FreeHugePageBytes` | No          |
| Fastest NIC per node     | `NUMANodeResources.FastestNICMbps`    | No          |
| Memory per node          | nothing, the struct has no field      | No          |

The per-node memory reading is collected by the probe for a stated purpose, that a
storage node pinned to a socket draws its memory from that socket, and the
placement never consults it.

### 5.3 The NIC reading, and what a bond does to it

The fastest NIC per node is computed from the **raw** `speedMbps` against the
**raw** `numaNode` of every interface in the report, with no stack traversal and
no kind filter. `placement.go:167-171`.

A bond, a team, a VLAN, a macvlan, a bridge, and a veth all sit under
`devices/virtual`, and the interface reader returns early for those before it
reads a memory node, so every one of them arrives carrying `numaNode: -1`. The
consequence is exact: an aggregate's speed is credited to the unknown bucket,
and only when that bucket happens to exist because some admitted device also
reported no memory node. Otherwise, the reading is dropped.

No real memory node is ever credited with a bonded or tagged link as such. What
saves the reading in practice is that the report carries the bond's members too,
each with a real node and a real speed, so the members are counted individually.

This is the one place the stack resolution added for the management interface
was not applied: `netstack.go` resolves an aggregate through its members and
reports a memory node only where they agree, and `NUMANodeBreakdown` does none
of it.

### 5.4 The sentence the placement produces

Every chosen worker carries a sentence saying what was chosen and why, and every
device left behind is recorded as refused with that same sentence as its reason.

Three defects in it, all observable in the recorded expectations:

1. It always says "devices" plural, so a one-device winner reads `carries 1
   unclaimed devices`.
2. It names only the runner-up. A three-node machine's third node is never
   mentioned.
3. **It asserts the count and capacity comparison even when neither decided.** A
   tie settled by key 3, 4, or 5 renders as `NUMA node 0 carries 2 unclaimed
   devices (2T) against 2 (2T) on no memory node in particular, so it was
   chosen`, which is equal numbers either side of a "so."

`Worker.PlacementReason` holds the sentence and **nothing in the repository
reads it**. Its only path to a reviewer is as the reason on the per-device
refusals, which become `DeviceDeclined` events.

### 5.5 Leftover devices

A device that survived every rule and was not taken by the placement is recorded
as a refusal whose rule is the placement's name and whose reason is the whole
winner sentence, identical on every leftover device of that worker.

Membership is tested by **device name equality**, not by identity or address.

These refusals are not pre-filters, so they would reach the per-worker
explanation, but the explanation is only assembled when the run produced no
node sets at all, and a worker with leftovers is by construction in the plan. So
they surface as events, inflate the refusal count in the run's summary, and do
not count toward the device count.

**A knock-on worth checking:** leftover devices are absent from the worker's
address list, which is what the grouping signature is computed over. Two
identical machines placed on different memory nodes therefore hand over
different address sets and land in different groups.

### 5.6 The alternative placement

`AllDevices` takes every admitted device regardless of memory node. It is not
used by the run, because the controller always builds the default, and it is a seam
for a fleet whose machines have one memory node.

Its downstream effect is worth stating because it is not local: a worker whose
devices span nodes has no single chosen node, which makes the cluster block's
vCPU count fall to the API floor and its huge-page size go unset for the whole
fleet, and the note the reviewer reads then says the worker "has no huge pages
reserved on the memory node it was placed on," which is not what happened.
## 6. Grouping and node sets

### 6.1 What makes two workers one group

A group's device selection is shared by every worker in it, so grouping is a
claim about the machines rather than a presentation choice. Two workers share a
group when three things match exactly:

1. The device class of the run.
2. The **name** of the management interface.
3. The sorted, deduplicated list of device addresses.

Hashed together, in that order. `SPECIFIED` for grouping by identical hardware,
which the design calls a guess at intent that a reviewer regroups. The
management interface is in the key with a `GROUNDED` reason: a `NodeGroup` names
one interface for every worker it lists.

**What is deliberately not in the key**, and what each omission costs:

| Not in the key                                     | Consequence                                                                                                 |
|----------------------------------------------------|-------------------------------------------------------------------------------------------------------------|
| Device **sizes**                                   | A 1.92 TB fleet and a 3.84 TB fleet at the same slots are one group, named for the first worker's capacity  |
| Device **count**, as distinct from the address set | One controller with two namespaces and one with a single namespace group together                           |
| NUMA topology                                      | No document field expresses it, so nothing is lost at the group level                                       |
| Model, vendor, serial, rotational                  | A mixed-model fleet at identical slots is one group. Model can only exclude devices, never split groups     |
| Kube role                                          | Deliberate: the split by role happens after grouping, so identical infra machines stay one group            |
| Memory, CPU, huge pages                            | Two machines with identical disks and very different RAM are one group, and `spdkSystemMemory` is never set |
| Interface kind, speed, members                     | Two workers whose `eth0` is a bare NIC on one and a bond on the other are one group                         |

**Address handling.** Addresses are deduplicated (`GROUNDED`: a controller with
two namespaces reports two devices at one address) and sorted ascending. The sort
normalizes probe enumeration order, so two workers whose kernels enumerated the
same disks differently still group together.

Case is **not** normalized. A probe reporting `0000:5E:00.0` and another
reporting `0000:5e:00.0` produce different signatures and therefore different
groups, while the allow-and-deny rule *does* fold case. `INVENTED`, and
inconsistent with the filter's treatment of the same string.

### 6.2 Ordering and numbering

Groups are ordered ascending by their first worker's name, and numbered from one
in that order. `GROUNDED`: two runs over one fleet produce the groups in the same
order and their generated names are stable.

The comparison is lexicographic, so `worker-10` sorts before `worker-2`. A
stability limit the comment does not state: numbering is stable across re-runs
over an **unchanged** fleet only. Adding a machine that sorts earliest shifts
every later group's number.

### 6.3 The group name

The pattern is `group-<n>-<class>-<count>x<size>`, for example
`group-1-nvme-4x3.492T`. `GROUNDED`: it is positional rather than derived from
the hardware, because the name is what a reviewer edits and `group-1` invites
that where a hash does not.

The size is the **first worker's** total device bytes divided by the number of
deduplicated addresses. Consequences:

| Fleet                                | Rendered     | Note                                                                              |
|--------------------------------------|--------------|-----------------------------------------------------------------------------------|
| 4 × 3.84 TB at four slots            | `4x3.492T`   | Binary divisor under an SI-looking letter                                         |
| One controller, two 1 TiB namespaces | `1x2T`       | States 2T for something the draft names once                                      |
| One 2 TiB disk and one 1 TiB disk    | `2x1.5T`     | An average no disk has                                                            |
| Claimed userspace controllers        | `8xunsized`  | `GROUNDED`: naming it `0 B` would state a capacity where there is only an absence |
| 10 240 TiB                           | `1.024e+04T` | Exponent notation inside a group name                                             |

**A false claim in the code.** The comment on the size computation says it is
"the same for every worker in it by construction." The signature hashes addresses
and not sizes, so it is not. This is the one place in that file where a stated
rationale asserts more than the code guarantees.

### 6.4 Node sets

One node set per role, ordered infra, then workers, then control-plane.
`GROUNDED`: infra nodes are almost always the ones somebody meant to be the
storage, and on OpenShift they do not count against a subscription's core limit,
so proposing them first in a block of their own lets a reviewer take that
placement by deleting the other block.

Names are `infra`, `discovered`, and `control-plane`. Splitting happens **after**
grouping, `GROUNDED`, so two infra nodes with identical disks stay one group.

**A consequence of that order:** a hardware group whose members span two roles
produces two `NodeGroup`s **with the identical name** in two different node sets,
each still stating the address count and size computed before the split. Names
are unique within a set and not across the document.

## 7. The management interface

### 7.1 The ladder

Applied per interface, in report order, which is ascending by name.

| #   | Rung                                                                    | Provenance                         |
|-----|-------------------------------------------------------------------------|------------------------------------|
| 1   | The kind must be bindable                                               | `INVENTED`                         |
| 2   | The state must be `up`, `unknown`, or empty                             | `ASSERTED`                         |
| 3   | A bridge or an overlay is refused unless it holds the node's address    | `ASSERTED`                         |
| 4   | It must hold at least one reachable address                             | `GROUNDED`                         |
| 5   | The interface holding the node's `InternalIP` wins, and returns at once | `GROUNDED`                         |
| 6   | Otherwise: resolved speed descending, kind simplicity, then name        | `ASSERTED`, `INVENTED`, `GROUNDED` |

**Bindable** is exactly physical, bond, team, VLAN, VXLAN, macvlan, ipvlan, and
bridge. Loopback, an unidentified virtual device, and any unrecognized kind are
refused. Refusing loopback and unidentified devices is argued. Admitting the rest is an
enumeration with no stated reason.

**Reachable** refuses an unparsable address, link-local unicast and multicast,
loopback, and the unspecified address. `GROUNDED`: the control plane refuses a
node whose management interface it cannot find an IP on, and it refuses it inside
the `node_add` task rather than at the request.

**Simplicity tiers** are physical 0, bond and team 1, VLAN and macvlan and ipvlan
2, everything else 3. The *role* of the key is argued, the tiers themselves are
not, and the stated metric ("the fewest layers between the address and the wire")
does not produce them: a VLAN over a bond is two layers and a bare VLAN is one,
and both are tier 2. Tier 3 is unreachable given the bindable set.

### 7.2 Resolving through the stack

An aggregate reports no speed, no slot, and no memory node of its own, so those
are resolved downward through its members.

- **Speed:** an interface's own reading wins if it is above zero, and the walk
  stops there. Otherwise, an aggregate sums its members and a derived interface
  takes its first parent with a speed. `GROUNDED` for the split, `ASSERTED` for
  the self-report short circuit.
- **Members:** the physical interfaces reachable downward, sorted and
  deduplicated. Empty for an interface that is itself physical.
- **Memory node:** the node every member agrees on, and unknown when they do not.
  `GROUNDED`: a bond whose members are in two sockets has no affinity, and
  reporting one of them would claim an affinity the interface does not have.

### 7.3 The interaction that matters most

**The eligibility gate runs before the node-address win.** An interface holding
the cluster's own address is skipped entirely, not even kept as a candidate, when
it is down, of an unidentified kind, or holding no otherwise-reachable address.

The adjacent comment states the opposite: "when it is known the interface holding
it wins outright. That is not a preference among equals." `INVENTED`, and no test
covers it.

What happens instead: the draft names some other interface on a different
network, with a reason reading "it is the fastest interface holding a reachable
address," which does not mention that the cluster's own address was elsewhere.
That is precisely the outcome the rule was written to prevent.

### 7.4 What the choice records, and what is read

The chosen interface carries its kind, its members, its resolved speed, its
memory node, and a sentence explaining the choice. **Only the name is ever read**
downstream, for the group signature and the document's `mgmtInterface`.

The sentence's stated purpose is "the record a reviewer reads," and it is
rendered in no template, event, or status. In the one case a reviewer most needs
explained, a worker with no serving interface, the sentence is **empty** and no
refusal is recorded.
## 8. The cluster block

Two of the cluster's fields are required by the API and cannot be read off a
worker. The file that derives them states its own standard: nothing there has to
be right, it has to be plausible, stated, and easy to correct.

| Field                  | Value                                                          | Provenance |
|------------------------|----------------------------------------------------------------|------------|
| `name`                 | the document's name plus `-cluster`                            | `ASSERTED` |
| `maxSubsystemCount`    | always 30                                                      | `GROUNDED` |
| `enableDriveFormat`    | always true                                                    | `GROUNDED` |
| `vcpuCount`            | the smallest chosen memory node's physical cores, floored at 4 | `GROUNDED` |
| `minHugePagesSize`     | the smallest free huge pages on any chosen node, or unset      | `ASSERTED` |
| `socketsToUse`         | never set                                                      | `INVENTED` |
| `nodesPerSocket`       | never set                                                      | `INVENTED` |
| `stripe`               | never set                                                      | `INVENTED` |
| `fabricType`           | never set                                                      | `INVENTED` |
| `enableFailureDomains` | never set                                                      | `INVENTED` |

**`maxSubsystemCount`** is the middle of the API's range, chosen so that a
reviewer who has not thought about it gets a working cluster and one who has can
see the number was not derived. Nothing a probe reports bears on it.

**`enableDriveFormat`** is on the document because it is destructive and the
document is what somebody approves. The cluster's own field is immutable once the
cluster exists, so a default nobody saw could not be undone.

**`vcpuCount`** takes the smallest worker because the control plane assumes the
count uniform across a cluster's nodes. The floor of 4 is the best-grounded
number in the generator: below it, sbcli's core layout assigns no NVMe-oF poller
core at all. Where the smallest worker cannot meet the floor, the floor is used
anyway and the note says the cluster asks for more than that worker has.

A worker contributes a reading only when **every** chosen device sits on one
memory node. A worker whose devices span nodes contributes nothing, and if no
worker contributes, the value is the floor.

**`minHugePagesSize`** reads the **free** pages on the chosen node, summed across
page sizes, and proposes the smallest across the fleet. It returns unset as soon
as **any** worker has none, and it truncates to whole gibibytes.

**The five fields never set** each have a cost. The most expensive is
`socketsToUse`: empty means socket 0 alone, so a two-socket fleet is drafted as a
single-socket deployment and the disks the placement chose on socket 1 are in the
document while the cluster is laid out for socket 0. The layout is immutable on
the cluster it lands on.

## 9. Assembling and naming the document

| Rule                                  | Value                                               | Provenance  |
|---------------------------------------|-----------------------------------------------------|-------------|
| Document name                         | `spec.discover.configName`, else `discovered-<run>` | `ASSERTED`  |
| `spec.approved`                       | always false, including on a re-run                 | `GROUNDED`  |
| `spec.environment`                    | copied from what Inspecting concluded               | `SPECIFIED` |
| Growth branch                         | `clusterRef` set means no cluster block is proposed | `GROUNDED`  |
| The filter                            | never written into the document                     | `SPECIFIED` |
| An existing document of the same name | adopted, left as it is, with an event               | `GROUNDED`  |
| Owner reference                       | none. The document outlives the run                 | `GROUNDED`  |

**The report objects** are named by a formula holding to 63 bytes rather than the
253 a ConfigMap may have. `GROUNDED` in an observed API server rejection: the Job
that writes a report is named the same way, and a Job's name reaches its pods as
a label, where 63 is the limit. The digest is unconditional because the run and
the node join on a separator both may contain.

Neither the name nor the labels can be read back as the values that produced
them. The node a report is about is the field inside the report, which is the only
place it appears as the cluster spells it.

## 10. Rules that need a decision

Every entry is `ASSERTED` or `INVENTED`: no design states it, and the code gives
no constraint for it. Ordered by what a wrong answer costs.

| #   | Rule                                                                            | Where | The question                                                                      |
|-----|---------------------------------------------------------------------------------|-------|-----------------------------------------------------------------------------------|
| 1   | The eligibility gate runs before the node-address win                           | §7.3  | Should a down link holding the cluster's own address be passed over in silence?   |
| 2   | Resolved speed descending is the first sort key for an interface                | §7.1  | Is the fastest link the management interface, or the data one?                    |
| 3   | A bridge or overlay is admitted only with the node's address                    | §7.1  | Is a bridge holding that address a management interface, or a machine to look at? |
| 4   | An aggregate's speed is the sum of its members                                  | §7.2  | Is a bridge an aggregate for this purpose? Its ports are not one flow's bandwidth |
| 5   | The simplicity tiers                                                            | §7.1  | Why does a bond outrank a VLAN, and what is tier 3 for?                           |
| 6   | Capacity, then cores, as the placement's tie-breaks                             | §5.1  | Is more capacity the right second key, given the stripe argument for the first?   |
| 7   | The placement ignores huge pages and NIC locality                               | §5.1  | Both are recorded as known. Which should enter the ranking?                       |
| 8   | Buckets exist only where an admitted device is                                  | §5    | A node with cores, pages, and a fast NIC but no free disk is invisible            |
| 9   | `minHugePagesSize` is the smallest **free** pages, unset if any worker has none | §8    | simplyblock allocates its own pages, so this reads a baseline as a requirement    |
| 10  | The five cluster fields never set                                               | §8    | `socketsToUse` above all: the second socket of every worker is left out           |
| 11  | Deny before allow, and an empty allow list means allow everything               | §4.2  | Both are conventions worth stating rather than discovering                        |
| 12  | Allow and deny fold case, including for block paths                             | §4.2  | `/dev/SDB` matches `/dev/sdb`, and Linux paths are case-sensitive                 |
| 13  | Device addresses are grouped case-sensitively                                   | §6.1  | The opposite convention to the filter, on the same strings                        |
| 14  | A model or size filter silently excludes every claimed controller               | §4.4  | They are unsized and unmodeled by construction                                    |
| 15  | The document name is the run's name, not a timestamp                            | §9    | The field's own documentation says timestamp                                      |

## 11. Defects found while cataloging

Each is a contradiction between the code and its own stated intent, or a path
that cannot do what it claims. None of them is a matter of taste.

| #   | Finding                                                                                                                                                                                                                          | Where  |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------|
| 1   | ~~An unparsable `driveSizeRange` silently admits every device.~~ **Fixed.** An admission webhook on `OperatorOps` refuses the run at the request, and both steps that read the filter refuse it again                            | §4.5   |
| 2   | **Both "huge pages unset" notes are computed and thrown away.** A draft that omits the field never explains why, against the file's own standard that every derived number carries a sentence                                    | §8     |
| 3   | **The derived cluster name is unbounded against a 63-character limit.** `configName` has no maximum and a run name may be 253, so the create fails and the step retries until its deadline                                       | §9     |
| 4   | **The draft's run label is the raw run name**, where every other use of that label is sanitized. An uppercase letter or 64 characters produces a create that can never succeed                                                   | §9     |
| 5   | **The group size comment claims every worker in a group has the same disks.** The signature hashes addresses, not sizes                                                                                                          | §6.3   |
| 6   | **The placement's sentence asserts a comparison that did not decide.** A tie settled by cores or node id still reads "carries 2 devices (2T) against 2 (2T), so it was chosen," and says "1 unclaimed devices" for a single disk | §5.4   |
| 7   | **The no-worker failure message says infra nodes are excluded.** They are admitted without an opt-in                                                                                                                             | §2     |
| 8   | **`InUse: false` is not distinguished from never read.** The report's own comment says a reader must check for an unreadable entry first. Nothing does, and the rule that would is off by default                                | §4.5   |
| 9   | **A worker whose report cannot be decoded produces no refusal**, only an event, so it is absent from the explanation the status carries                                                                                          | §4.5   |
| 10  | **`Reason` is empty in the one case a reviewer most needs explained**: a worker with no serving interface                                                                                                                        | §7.4   |
| 11  | **Five of six recorded interface facts have no consumer**, including the sentence whose stated purpose is a record a reviewer reads                                                                                              | §7.4   |
| 12  | **`Upper` is collected, shipped, and read by nothing.** The documented use, a NIC holding no address with the address on a VLAN above it, is never implemented                                                                   | §7.2   |
| 13  | **A wireless interface is a tier-0 management candidate.** `DEVTYPE=wlan` falls through the kind switch to physical                                                                                                              | §7.1   |
| 14  | **A team or a VLAN on a kernel publishing no `DEVTYPE` becomes unbindable.** Bonds and bridges have a directory fallback, these do not                                                                                           | §7.1   |
| 15  | **Carrier and duplex are dropped from the report**, against a header claiming no filtering and no judgment. The rule admits `unknown` state, which is exactly the case carrier would settle                                      | §7.1   |
| 16  | **Five schema limits are unchecked before writing**: groups per node set, workers per group, devices per selection, and the two address patterns. Each surfaces as a create rejection rather than a finding                      | §6, §9 |
| 17  | **The unknown memory-node bucket sorts first here and last in atlas-lib.** Same sentinel, opposite convention                                                                                                                    | §5     |
| 18  | **Two constants share one event wire value** in a package whose own premise is that a reason is an API                                                                                                                           | §9     |

## 12. Rules that are specified and not built

| Rule                                                                                       | Stated in                                                             | State     |
|--------------------------------------------------------------------------------------------|-----------------------------------------------------------------------|-----------|
| `failureDomain` is seeded from `topology.kubernetes.io/zone` and left unset where no label | `design-clusterdeploymentconfig.md` §8.2, and the API field's own doc | `UNBUILT` |

Nothing in the operator reads that label, and `failureDomain` is never written.
The test plan's `U-52` and `U-53` are the rows for it, both unimplemented.

It is not cosmetic. A **growth** document against a cluster with
`enableFailureDomains` set is generated already failing its own validation,
because every group is required to carry a domain and none does. A creating
document escapes only because the generator never sets `enableFailureDomains`
either.
