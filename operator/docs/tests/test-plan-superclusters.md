# Test Plan: Superclusters

Related design: [`designs/design-superclusters.md`](../designs/design-superclusters.md)  
Harness: [`operator/internal/controllers/testsupport`](../../internal/controllers/testsupport)
for the operator, and the mock control plane in
[`csi-driver/internal/csi/controller/mock_controlplane_test.go`](../../../csi-driver/internal/csi/controller/mock_controlplane_test.go)
for the CSI driver

Scope: the operator, the CSI driver, and the Kubernetes surface of this
repository. Control-plane (`sbcli`) behavior is a dependency, faked at the
boundary. The design's Phase 0 prerequisites (P0-1 to P0-6) are not tested here.
What is tested is this repository's response to each of them being present,
absent, or failing.

Scenario IDs are permanent: `U-` unit (no cluster: pure functions, fake
`client.Client`, mock HTTP), `I-` integration (full reconcile loop against
`envtest` and a mock backend), `E-` end-to-end (live cluster, real data path),
`M-` manual (needs failure injection or orchestration not yet automated). Types
are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column names
the implementing function, or `—` when the scenario is not yet covered. Every `—`
also appears in §7.

Section references written as `design §n` point at the design document. A bare
`§n` is this plan's own.

**Selected axes.** Cluster count (one standalone cluster, a supercluster of two,
of three, and of thirty-two members, and two superclusters in one Kubernetes
cluster). Member topology (one-node, three-node, and five-node members). Namespace
scope (one namespace, and superclusters in two namespaces). Lifecycle (operator
restart in every persisted step). **Excluded axis:** two Kubernetes clusters,
because every member is in the same Kubernetes cluster and namespace by design
(design §2, Non-Goals).

---

## 1. Unit Tests

All pure functions and webhook rules, with a fake `client.Client` and, for the
CSI driver, the mock control plane. Numbering runs continuously across the
groups below.

### Phase Rollup (design §4.3)

File: `operator/internal/controllers/cluster/supercluster_rollup_test.go`

| #    | Scenario                                                                                                          | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------|----------|------|
| U-01 | Every member `Online` rolls up to `Online`                                                                        | Positive | —    |
| U-02 | One member `Degraded`, the rest `Online`, rolls up to `Degraded`                                                  | Positive | —    |
| U-03 | One member `Rebalancing` rolls up to `Degraded`, not `Rebalancing`                                                | Positive | —    |
| U-04 | One member `Unavailable` while another serves rolls up to `Degraded`                                              | Positive | —    |
| U-05 | No member serving, at least one `Suspended`, with none `Unavailable`, rolls up to `Suspended`                     | Positive | —    |
| U-06 | No member serving, one `Unavailable`, rolls up to `Unavailable`                                                   | Negative | —    |
| U-07 | One member unresolved, or still `Pending` or `Creating`, rolls up to `Provisioning`                               | Boundary | —    |
| U-08 | `maxFaultTolerance` is the lowest among members, and unset when any member reports none                           | Boundary | —    |
| U-09 | Capacity sums the members' readings, and a member with no reading contributes nothing rather than zeroing the sum | Boundary | —    |

### Member Derivation and Naming (design §5.1, §5.2)

File: `operator/internal/controllers/deployment/supercluster_members_test.go`

| #        | Scenario                                                                                                                                                                                 | Type     | Test |
|----------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| ~~U-10~~ | ~~One distinct group name across two node sets derives one standalone cluster named `spec.cluster.name`~~ superseded by U-74: members are node sets, not group names (design §5.1)       | Positive | —    |
| ~~U-11~~ | ~~Two distinct group names derive two members, `<cluster>-<group>`, and a supercluster `spec.cluster.name`~~ superseded by U-75: members are node sets, not group names (design §5.1)    | Positive | —    |
| ~~U-12~~ | ~~One group name in two node sets derives one member whose nodes span both node sets~~ superseded by U-76: members are node sets, not group names (design §5.1)                          | Positive | —    |
| ~~U-13~~ | ~~A member name of exactly 63 characters is admitted, 64 is refused with `MemberNameInvalid` naming the group~~ superseded by U-77: members are node sets, not group names (design §5.1) | Boundary | —    |
| ~~U-14~~ | ~~A group name that makes the member name an invalid DNS label is refused with `MemberNameInvalid`~~ superseded by U-78: members are node sets, not group names (design §5.1)            | Negative | —    |
| ~~U-15~~ | ~~Thirty-two distinct groups are admitted, thirty-three are refused~~ superseded by U-79: members are node sets, not group names (design §5.1)                                           | Boundary | —    |
| ~~U-16~~ | ~~Two groups with different names and different interfaces are not a finding~~ superseded by U-80: members are node sets, not group names (design §5.1)                                  | Positive | —    |
| ~~U-17~~ | ~~Two node sets using one group name with different interfaces are a finding~~ superseded by U-81: members are node sets, not group names (design §5.1)                                  | Negative | —    |
| ~~U-18~~ | ~~Two groups with different names and different device classes are not a finding~~ superseded by U-82: members are node sets, not group names (design §5.1)                              | Positive | —    |
| U-19     | One worker in two groups of different names is still a finding                                                                                                                           | Negative | —    |

### Workload Naming (design §6.1)

File: `atlas-lib/kube/names_test.go` and
`operator/internal/controllers/cluster/workload_names_test.go`

| #    | Scenario                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------|----------|------|
| U-20 | A standalone cluster with no owner of the shared Service uses the shared names           | Positive | —    |
| U-21 | A member uses `simplyblock-storage-node-api-<member>` and the matching TLS secret name   | Positive | —    |
| U-22 | A member that owns the shared Service keeps the shared names                             | Positive | —    |
| U-23 | A standalone cluster when another cluster owns the shared Service uses per-cluster names | Boundary | —    |
| U-24 | Node hostnames built by the operator and by the bootstrap agree for a member             | Positive | —    |

### Admission (design §4.2, §4.4, §6.2)

File: `operator/internal/webhook/storagecluster_validator_test.go`

| #    | Scenario                                                                                                              | Type                | Test                                                |
|------|-----------------------------------------------------------------------------------------------------------------------|---------------------|-----------------------------------------------------|
| U-25 | The first cluster of a namespace is admitted                                                                          | Positive            | `TestTheFirstClusterOfANamespaceIsAdmitted`         |
| U-26 | A second standalone cluster in one namespace is refused                                                               | Negative            | `TestASecondClusterInOneNamespaceIsRefused`         |
| U-27 | A cluster beside one in another namespace is admitted                                                                 | Positive            | `TestAClusterIsAdmittedBesideOneInAnotherNamespace` |
| U-28 | Updating the only cluster is admitted                                                                                 | Positive            | `TestUpdatingTheOnlyClusterIsAdmitted`              |
| U-29 | A supercluster whose members all resolve in its namespace is admitted                                                 | Positive            | —                                                   |
| U-30 | A supercluster naming a member that does not exist is refused, naming it                                              | Negative            | —                                                   |
| U-31 | A supercluster naming a member in another namespace is refused                                                        | Negative            | —                                                   |
| U-32 | A supercluster naming another supercluster as a member is refused                                                     | Negative            | —                                                   |
| U-33 | A supercluster naming a member another supercluster lists is refused                                                  | Negative            | —                                                   |
| U-34 | Members on different `ControlPlane` objects are refused                                                               | Negative            | —                                                   |
| U-35 | An aggregate of one member is refused, of two is admitted                                                             | Boundary            | —                                                   |
| U-36 | A supercluster carrying `vcpuCount` or `stripe` is refused, naming the field                                          | Negative            | —                                                   |
| U-37 | A standalone cluster without `maxSubsystemCount` or `vcpuCount` is refused, as today                                  | Negative            | —                                                   |
| U-38 | `placement` on a standalone cluster is refused                                                                        | Negative            | —                                                   |
| U-39 | Adding `aggregate` to a standalone cluster, or removing it from a supercluster, is refused                            | Negative            | —                                                   |
| U-40 | Appending an aggregate entry is admitted                                                                              | Positive            | —                                                   |
| U-41 | Removing an entry whose member's pools report bound volumes is refused                                                | Negative            | —                                                   |
| U-42 | Removing an entry whose member holds no volumes is admitted                                                           | Positive            | —                                                   |
| U-43 | Deleting a cluster an aggregate lists is refused                                                                      | Negative            | —                                                   |
| U-44 | A standalone cluster created beside a supercluster is refused                                                         | Negative            | —                                                   |
| U-45 | A member created before its supercluster is admitted when an approved document names it, refused otherwise            | Positive + Negative | —                                                   |
| U-46 | A member created while a concurrent aggregate edit lists it under a second supercluster is refused on the member side | Negative            | —                                                   |

### Member Selection in the CSI Driver (design §7.2, §7.3)

File: `csi-driver/internal/csi/controller/placement_aggregate_test.go`

| #    | Scenario                                                                                              | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------|----------|------|
| U-47 | `MostAvailableCapacity` picks the member with the most free capacity                                  | Positive | —    |
| U-48 | `LeastProvisioned` picks the member with the lowest provisioned ratio                                 | Positive | —    |
| U-49 | Equal ranks break by cluster UUID, identically across calls                                           | Boundary | —    |
| U-50 | A member with `placement: false` is never chosen                                                      | Negative | —    |
| U-51 | A member whose free capacity equals the requested size is eligible, one byte less is not              | Boundary | —    |
| U-52 | A member above its critical threshold is dropped                                                      | Negative | —    |
| U-53 | A member whose capacity read exceeds the deadline is dropped, and the others are still ranked         | Negative | —    |
| U-54 | No candidate fails with `ResourceExhausted`, listing each member's reason                             | Negative | —    |
| U-55 | An unknown aggregate name fails with `InvalidArgument` naming the class                               | Negative | —    |
| U-56 | A class carrying both `supercluster` and `cluster_id` is refused                                      | Negative | —    |
| U-57 | `zone_cluster_map` beside `supercluster` restricts candidates to the zone's members                   | Positive | —    |
| U-58 | A name existing in a member returns that volume without ranking, including a cordoned member          | Positive | —    |
| U-59 | A name existing in no member is placed, and the mock receives exactly one create                      | Positive | —    |
| U-60 | A lookup failing with 5xx in one member fails the call rather than placing, so no duplicate can arise | Negative | —    |
| U-61 | The handle's first component is the chosen member's UUID                                              | Positive | —    |
| U-62 | A clone through a supercluster class lands in the source's member                                     | Positive | —    |
| U-63 | A clone whose source member is not in the class's aggregate is refused with `InvalidArgument`         | Negative | —    |
| U-64 | A group whose volumes span two members is refused, naming both members                                | Negative | —    |

### Relocate Validation (design §8.1, §9.3, §9.4)

File: `operator/internal/webhook/persistentvolumeops_validator_test.go`

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-65 | A target that is another eligible member of the same supercluster is admitted | Positive | —    |
| U-66 | The volume's own member as target is refused                                  | Negative | —    |
| U-67 | A member of a different supercluster as target is refused                     | Negative | —    |
| U-68 | A volume in a standalone cluster is refused                                   | Negative | —    |
| U-69 | A target with `disablePlacement` is refused                                   | Negative | —    |
| U-70 | A volume in a consistency group is refused                                    | Negative | —    |
| U-71 | An encrypted volume is refused with `EncryptedVolumeNotMovable`               | Negative | —    |
| U-72 | Editing `targetClusterRef` after creation is refused                          | Negative | —    |
| U-73 | `Relocate` without `relocate` is refused                                      | Negative | —    |

### Node Sets, Limits, and Naming (design §5.1, §5.2, §5.3, §5.6)

File: `operator/internal/controllers/deployment/supercluster_members_test.go`

| #    | Scenario                                                                                                              | Type     | Test                                               |
|------|-----------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------|
| U-74 | One node set with several groups derives one standalone cluster named `spec.cluster.name`                             | Positive | —                                                  |
| U-75 | Two node sets derive two members, `<cluster>-<nodeset>`, and a supercluster named `spec.cluster.name`                 | Positive | —                                                  |
| U-76 | A node set whose groups name three failure domains derives one member with all three mapped                           | Positive | —                                                  |
| U-77 | A member name of exactly 63 characters is admitted, 64 is refused with `MemberNameInvalid` naming the node set        | Boundary | —                                                  |
| U-78 | A node set name that makes the member name an invalid DNS label is refused with `MemberNameInvalid`                   | Negative | —                                                  |
| U-79 | Thirty-two node sets are admitted, thirty-three are refused with `TooManyMembers`                                     | Boundary | —                                                  |
| U-80 | Two node sets with different interfaces are not a finding                                                             | Positive | —                                                  |
| U-81 | Two groups in one node set with different interfaces are a finding                                                    | Negative | `TestGroupsThatDisagreeAboutInterfacesAreReported` |
| U-82 | Two node sets with different device classes pass the narrowed CEL rule, and each member is stamped with its own class | Positive | —                                                  |
| U-83 | With one slot per worker, a node set of 32 workers is admitted and 33 is refused with `MemberTooLarge`                | Boundary | —                                                  |
| U-84 | With two slots per worker, a node set of 16 workers is admitted and 17 is refused                                     | Boundary | —                                                  |
| U-85 | A one-node-set document over 32 storage nodes is refused, so an ordinary cluster is held to the limit too             | Negative | —                                                  |
| U-86 | A node set below the erasure-coding minimum is refused even when the document's total meets it                        | Negative | —                                                  |
| U-87 | Growth adding eight workers to a member of thirty is refused with `MemberTooLarge` naming both counts                 | Boundary | —                                                  |

### Balanced Split in the Discovery Generator (design §5.5)

File: `operator/internal/discovery/sizesplit_test.go`

| #    | Scenario                                                                                                                      | Type       | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-88 | Twenty workers with one slot each are drafted as one node set keeping the tier's name, `discovered`                           | Positive   | —    |
| U-89 | Thirty-two workers are one node set, thirty-three are two of 17 and 16                                                        | Boundary   | —    |
| U-90 | Forty workers with one slot each are two node sets of 20                                                                      | Positive   | —    |
| U-91 | Forty workers with two slots each are three node sets of 14, 13, and 13                                                       | Positive   | —    |
| U-92 | Sixty-five workers with one slot each are three node sets of 22, 22, and 21                                                   | Positive   | —    |
| U-93 | Workers in three failure domains are spread over every node set, with per-domain counts differing by at most one between sets | Positive   | —    |
| U-94 | Workers whose management interface names differ are never drafted into one node set                                           | Negative   | —    |
| U-95 | Group names are unique within every node set after the split                                                                  | Boundary   | —    |
| U-96 | Two runs over an unchanged fleet produce identical node sets and names                                                        | Positive   | —    |
| U-97 | A partition below the erasure-coding minimum is drafted without padding, and validation reports the minimum                   | Negative   | —    |
| U-98 | An infra tier and a worker tier split independently, drafted as `infra`, `discovered-1`, and `discovered-2`                   | Positive   | —    |
| U-99 | Two hundred workers with distinct disks produce no node set above 32 groups, which replaces the overflow split                | Regression | —    |

### Kubernetes Topology (design §5.7, §7.2)

File: `operator/internal/discovery/topology_test.go`, and
`csi-driver/internal/csi/controller/placement_aggregate_test.go` for U-109 to U-111

| #     | Scenario                                                                                                         | Type                | Test |
|-------|------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-100 | A worker's `topology.kubernetes.io/zone` is copied verbatim into its group's `failureDomain`                     | Positive            | —    |
| U-101 | `failureDomainLabel` set to a rack label replaces the zone label as the source                                   | Positive            | —    |
| U-102 | A label value that does not fit the `failureDomain` pattern is a finding naming the worker, and is not rewritten | Negative            | —    |
| U-103 | Every worker labeled sets `enableFailureDomains` on the template, no worker labeled leaves it unset              | Positive + Negative | —    |
| U-104 | Some workers labeled is drafted with `TopologyIncomplete` naming the unlabeled ones                              | Boundary            | —    |
| U-105 | Workers in two regions are never drafted into one node set, and node set names carry the region                  | Negative            | —    |
| U-106 | Thirty workers in zone `a` and ten in zone `b` are two members of fifteen and five each                          | Positive            | —    |
| U-107 | A hand-written node set whose workers span two regions is refused at approval with `MemberSpansRegions`          | Negative            | —    |
| U-108 | A worker without a region label matches any region, so an unlabeled fleet is never refused for it                | Boundary            | —    |
| U-109 | A preferred topology narrows candidates to members in its region                                                 | Positive            | —    |
| U-110 | No member in the preferred region with room falls back to the requisite regions, then to every eligible member   | Negative            | —    |
| U-111 | A class carrying `zone_cluster_map` restricts by the map and ignores member topology                             | Positive            | —    |

---

## 2. Integration Tests

Run the full controller reconciliation loop against a mock control plane HTTP
server and a real Kubernetes API through `envtest`.

### Supercluster Reconcile (design §10.1)

| #    | Scenario                                                                                                                            | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | A supercluster of two online members reports `Online`, both members eligible, and `observedGeneration`                              | Positive | —    |
| I-02 | Each member reports `role: Member` and `superclusterRef`, and an unrelated cluster `Standalone`                                     | Positive | —    |
| I-03 | A member going `Unavailable` makes the supercluster `Degraded`, drops the member from placement, and emits `MemberPlacementChanged` | Positive | —    |
| I-04 | A missing member is reported `Pending` with `MemberNotFound`, and the other member is still read                                    | Negative | —    |
| I-05 | The secret gains an `aggregates` entry, and the `clusters` entries the cluster controller wrote are untouched                       | Positive | —    |
| I-06 | Setting `disablePlacement` flips the member's `placement` in the secret                                                             | Positive | —    |
| I-07 | The capacity endpoint returning 5xx keeps the last reading and requeues                                                             | Negative | —    |
| I-08 | The volume rebalancer and the arbitration controller never reconcile the supercluster                                               | Negative | —    |
| I-09 | Two superclusters in two namespaces write two entries and never read each other's members                                           | Positive | —    |
| I-10 | Crossing the combined warning threshold emits `AggregateCapacityThreshold` once                                                     | Boundary | —    |
| I-11 | A reconcile with nothing changed issues no status write and no secret write                                                         | Negative | —    |

### Expansion (design §5.3, §5.4)

| #    | Scenario                                                                                                              | Type       | Test |
|------|-----------------------------------------------------------------------------------------------------------------------|------------|------|
| I-12 | A two-group document creates two members, then the supercluster, then nodes per member, and one activation per member | Positive   | —    |
| I-13 | A one-group document expands exactly as before                                                                        | Regression | —    |
| I-14 | A restart between creating members and creating the supercluster creates the supercluster and no second member        | Negative   | —    |
| I-15 | A member whose nodes are online is activated while another member's nodes are still coming up                         | Positive   | —    |
| I-16 | `status.clusterRef` names the supercluster and `status.nodeRefs` lists every member's nodes                           | Positive   | —    |
| I-17 | Growth with existing group names adds nodes to those members only                                                     | Positive   | —    |
| I-18 | Growth with a new group name creates the member and appends it to the aggregate                                       | Positive   | —    |
| I-19 | Growth with two group names against a standalone cluster is refused with `ClusterNotAggregate`                        | Negative   | —    |
| I-20 | Appending to the aggregate retries on a conflicting concurrent edit and keeps the administrator's change              | Negative   | —    |

### Pools (design §7.1)

| #    | Scenario                                                                                | Type     | Test |
|------|-----------------------------------------------------------------------------------------|----------|------|
| I-21 | A pool on a supercluster creates one backend pool per member under the pool's name      | Positive | —    |
| I-22 | The pool is `Ready` only when every eligible member has it                              | Boundary | —    |
| I-23 | A 409 on an existing backend pool is success, and the mock sees no second create        | Negative | —    |
| I-24 | One member's create failing leaves that member ineligible and the pool status naming it | Negative | —    |
| I-25 | `allowedNodes` is split by member                                                       | Positive | —    |
| I-26 | Limits are applied unchanged to every member's backend pool                             | Positive | —    |
| I-27 | A member appended later receives every pool before it becomes eligible                  | Positive | —    |
| I-28 | The generated StorageClass carries `supercluster` and no `cluster_id`                   | Positive | —    |
| I-29 | Deleting the supercluster is held while a pool names it                                 | Negative | —    |

### Relocate (design §8.2)

| #    | Scenario                                                                                                                                        | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-30 | The happy path calls start, polls status, runs the attach Jobs, commits once, and succeeds                                                      | Positive | —    |
| I-31 | Volumes sharing the subsystem are listed in `status.relocate.volumes` and moved together                                                        | Positive | —    |
| I-32 | Abort before commit calls stop, deletes the Jobs, and ends `Aborted`                                                                            | Positive | —    |
| I-33 | Abort after commit is ignored with `RelocationAbortIgnored`                                                                                     | Negative | —    |
| I-34 | A restart in every step resumes there, and the mock sees one start and one commit                                                               | Negative | —    |
| I-35 | Start or commit returning 5xx is retried without advancing the step                                                                             | Negative | —    |
| I-36 | Start returning 4xx fails the operation with the control plane's message                                                                        | Negative | —    |
| I-37 | A failing attach Job fails the operation after stop                                                                                             | Negative | —    |
| I-38 | The original handle not resolving after commit fails with `HandleUnresolved`                                                                    | Negative | —    |
| I-39 | A `Migrate` and a `Relocate` of one volume exclude each other                                                                                   | Negative | —    |
| I-40 | `Evacuate` cordons the member, raises one `Relocate` per subsystem within the concurrency bound, and removes the entry when the member is empty | Positive | —    |
| I-41 | A failing child holds the evacuation with `EvacuationHeld`                                                                                      | Negative | —    |
| I-42 | A draft with two node sets names both members in `status.message` before approval                                                               | Positive | —    |
| I-43 | The aggregate entry in the secret carries each member's region and zones, read from its nodes' `Node` labels, and follows a relabeled node      | Positive | —    |

---

## 3. E2E Tests

Run against a live simplyblock control plane with three members of three nodes
each, using fio in verify mode for every data-path row. Rows E-06 to E-08 need
P0-3 and P0-4.

| #    | Scenario                                                                                                                                            | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | A two-node-set deployment document produces a supercluster and two activated members                                                                | Positive | —    |
| E-02 | Thirty PVCs through a supercluster class spread across members by the strategy, and every volume passes fio verify                                  | Positive | —    |
| E-03 | `CreateVolume` retried after an injected timeout yields one backend volume                                                                          | Negative | —    |
| E-04 | A member shut down leaves the others' volumes serving and new volumes landing elsewhere                                                             | Negative | —    |
| E-05 | Snapshot, clone, and expansion of a volume stay in its member                                                                                       | Positive | —    |
| E-06 | A relocation under sustained fio completes with no I/O error and verified data                                                                      | Positive | —    |
| E-07 | Expand, snapshot, and delete after a relocation succeed through the original handle                                                                 | Positive | —    |
| E-08 | A member evacuated under load is removed from the aggregate with every volume intact                                                                | Positive | —    |
| E-09 | Discovery over a fleet of more than 32 storage nodes (17 workers with two slots each) drafts two node sets that approve and expand into two members | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01: The source member fails between commit and verification

**Design reference:** design §8.2

**What to verify:** a cutover that committed while the source member was
reachable completes or fails visibly, and the volume is served by exactly one
member afterward.

**Test concept:**

1. Start a relocation of a volume under fio verify.
2. Once `status.relocate.committed` is set, power off one source-member node
   carrying the volume.
3. Assert the operation reaches `Succeeded` or `Failed` with the relationship
   state reported, and that fio reports no data mismatch.

### M-02: A member partitioned from the CSI controller during a provisioning burst

**Design reference:** design §7.2, §7.3

**What to verify:** a member that does not answer within the deadline is skipped,
and a later retry cannot create a second copy in another member.

**Test concept:**

1. Block traffic from the CSI controller to one member's control-plane endpoint
   for capacity calls only.
2. Create twenty PVCs.
3. Restore the traffic and assert every PVC has exactly one backend volume across
   all members.

**Open question:** the lookup by name (design §7.3) uses the same endpoint, so
the partition has to be selective to exercise U-53 and U-60 separately.

---

## 5. Axis Coverage

| Axis            | Values covered                                                                  | IDs                                      | Not covered                                |
|-----------------|---------------------------------------------------------------------------------|------------------------------------------|--------------------------------------------|
| Cluster count   | standalone, 2 members, 3 members, 32 members, 33 members, and two superclusters | U-74, U-35, E-02, U-79, I-09             | 32 members on a live control plane         |
| Member topology | one-node member, three-node member                                              | U-12, E-01                               | five-node members, members of unequal size |
| Member size     | 16 and 17 workers at two slots, 32 and 33 at one slot, 20 to 65 workers split   | U-84, U-83, U-89, U-90, U-91, U-92, E-09 | —                                          |
| Topology        | zones labeled, unlabeled, partially labeled, rack label, two regions            | U-100, U-103, U-104, U-101, U-105, U-109 | a live cluster stretched over two regions  |
| Namespace scope | one namespace, two namespaces                                                   | U-29, U-31, I-09                         | —                                          |
| Lifecycle       | restart mid-expansion, restart in every relocate step                           | I-14, I-34                               | restart mid-evacuation                     |
| Control plane   | 4xx, 5xx, timeout, 409                                                          | I-36, I-35, U-53, I-23                   | —                                          |
| Data path       | spread, failure, relocation, evacuation                                         | E-02, E-04, E-06, E-08                   | —                                          |

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered               |
|-------------|-----------|---------|---------------------------|
| Unit        | 102       | 5       | every other active U- row |
| Integration | 43        | 0       | I-01 … I-43               |
| E2E         | 9         | 0       | E-01 … E-09               |
| Manual      | 2         | 0       | M-01, M-02                |

The five covered unit rows are existing tests of behavior this design keeps:
U-25 to U-28 and U-81. The unit count excludes the nine rows U-10 to U-18,
which are struck through and superseded by U-74 to U-82.

---

## 7. What Is Not Yet Covered

| #                                                   | Gap                                                    | Reason                                                                                               |
|-----------------------------------------------------|--------------------------------------------------------|------------------------------------------------------------------------------------------------------|
| U-01 … U-09, U-19 … U-24, U-29 … U-80, U-82 … U-111 | Every new unit scenario                                | Feature not implemented (Phases 1 to 4)                                                              |
| I-01 … I-29, I-42, I-43                             | Supercluster reconcile, expansion, pools, and topology | Feature not implemented (Phases 1 and 2)                                                             |
| I-30 … I-41                                         | Relocation and evacuation                              | Feature not implemented (Phases 3 and 4)                                                             |
| E-01 … E-05, E-09                                   | Live supercluster, placement, and discovery split      | Feature not implemented (Phases 1 and 2)                                                             |
| E-06 … E-08                                         | Live relocation and evacuation                         | Blocked by P0-3 and P0-4                                                                             |
| M-01, M-02                                          | Failure injection                                      | Needs node power control and selective network blocking                                              |
| —                                                   | A live cluster stretched over two regions              | No such lab. U-105, U-107, and U-109 cover the decisions                                             |
| —                                                   | 32 members on a live control plane                     | No lab of that size. U-79 covers the limit                                                           |
| —                                                   | Five-node and unequal-size members                     | Placement does not read node count, and capacity fixtures in U-47 and U-48 cover it                  |
| —                                                   | Restart mid-evacuation                                 | Evacuation resumes through its children, each covered by I-34. A dedicated row is added with Phase 4 |
