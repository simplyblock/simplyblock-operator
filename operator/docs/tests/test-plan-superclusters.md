# Test Plan: Superclusters

Related design: [`designs/design-superclusters.md`](../designs/design-superclusters.md)  
Harness: [`operator/internal/controllers/testsupport`](../../internal/controllers/testsupport)
for the operator, and the mock control plane in
[`csi-driver/internal/csi/controller/mock_controlplane_test.go`](../../../csi-driver/internal/csi/controller/mock_controlplane_test.go)
for the CSI driver

Scope: the operator, the CSI driver, and the Kubernetes surface of this
repository. Control-plane (`sbcli`) behavior is a dependency, faked at the
boundary. The design's Phase 0 prerequisites (P0-1 and P0-2) are not tested here.
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

| #        | Scenario                                                                                                                                                                | Type     | Test |
|----------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| ~~U-20~~ | ~~A standalone cluster with no owner of the shared Service uses the shared names~~ superseded by U-133: workload names derive from the cluster (design §6.1)            | Positive | —    |
| ~~U-21~~ | ~~A member uses `simplyblock-storage-node-api-<member>` and the matching TLS secret name~~ superseded by U-136: members share the supercluster's Services (design §6.2) | Positive | —    |
| ~~U-22~~ | ~~A member that owns the shared Service keeps the shared names~~ superseded by U-134: workload names derive from the cluster (design §6.1)                              | Positive | —    |
| ~~U-23~~ | ~~A standalone cluster when another cluster owns the shared Service uses per-cluster names~~ superseded by U-133: workload names derive from the cluster (design §6.1)  | Boundary | —    |
| U-24     | Node hostnames built by the operator and by the bootstrap agree for a member                                                                                            | Positive | —    |

### Admission (design §4.2, §4.4, §6.2)

File: `operator/internal/webhook/storagecluster_validator_test.go`

| #        | Scenario                                                                                                                                                                                          | Type                | Test                                                |
|----------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|-----------------------------------------------------|
| U-25     | The first cluster of a namespace is admitted                                                                                                                                                      | Positive            | `TestTheFirstClusterOfANamespaceIsAdmitted`         |
| ~~U-26~~ | ~~A second standalone cluster in one namespace is refused~~ superseded by U-138: the one-cluster-per-namespace rule is removed (design §4.4)                                                      | Negative            | —                                                   |
| U-27     | A cluster beside one in another namespace is admitted                                                                                                                                             | Positive            | `TestAClusterIsAdmittedBesideOneInAnotherNamespace` |
| U-28     | Updating the only cluster is admitted                                                                                                                                                             | Positive            | `TestUpdatingTheOnlyClusterIsAdmitted`              |
| ~~U-29~~ | ~~A supercluster whose members all resolve in its namespace is admitted~~ superseded by U-139: membership is declared by the member (design §4.1)                                                 | Positive            | —                                                   |
| ~~U-30~~ | ~~A supercluster naming a member that does not exist is refused, naming it~~ superseded by U-140: membership is declared by the member (design §4.1)                                              | Negative            | —                                                   |
| ~~U-31~~ | ~~A supercluster naming a member in another namespace is refused~~ superseded by U-140: membership is declared by the member (design §4.1)                                                        | Negative            | —                                                   |
| ~~U-32~~ | ~~A supercluster naming another supercluster as a member is refused~~ superseded by U-141: membership is declared by the member (design §4.1)                                                     | Negative            | —                                                   |
| ~~U-33~~ | ~~A supercluster naming a member another supercluster lists is refused~~ superseded by U-144: membership is declared by the member (design §4.1)                                                  | Negative            | —                                                   |
| U-34     | Members on different `ControlPlane` objects are refused                                                                                                                                           | Negative            | —                                                   |
| ~~U-35~~ | ~~An aggregate of one member is refused, of two is admitted~~ superseded by U-142: a supercluster has no member minimum (design §4.3)                                                             | Boundary            | —                                                   |
| U-36     | A supercluster carrying `vcpuCount` or `stripe` is refused, naming the field                                                                                                                      | Negative            | —                                                   |
| U-37     | A standalone cluster without `maxSubsystemCount` or `vcpuCount` is refused                                                                                                                        | Negative            | —                                                   |
| U-181    | A supercluster carrying every defaulted spec field at its schema default is admitted, and the defaults are read from the generated CRD schema                                                     | Positive            | —                                                   |
| U-182    | A supercluster carrying `nvmfBasePort: 4421` is refused, naming the field                                                                                                                         | Negative            | —                                                   |
| ~~U-38~~ | ~~`placement` on a standalone cluster is refused~~ superseded by U-143: placement moved into `spec.supercluster` (design §4.1)                                                                    | Negative            | —                                                   |
| ~~U-39~~ | ~~Adding `aggregate` to a standalone cluster, or removing it from a supercluster, is refused~~ superseded by U-144: membership is declared by the member (design §4.1)                            | Negative            | —                                                   |
| ~~U-40~~ | ~~Appending an aggregate entry is admitted~~ superseded by U-144: membership is declared by the member (design §4.1)                                                                              | Positive            | —                                                   |
| ~~U-41~~ | ~~Removing an entry whose member's pools report bound volumes is refused~~ superseded by U-145: a member leaves through `RemoveMember` (design §8.4)                                              | Negative            | —                                                   |
| ~~U-42~~ | ~~Removing an entry whose member holds no volumes is admitted~~ superseded by U-145: a member leaves through `RemoveMember` (design §8.4)                                                         | Positive            | —                                                   |
| ~~U-43~~ | ~~Deleting a cluster an aggregate lists is refused~~ superseded by U-145: a member leaves through `RemoveMember` (design §8.4)                                                                    | Negative            | —                                                   |
| ~~U-44~~ | ~~A standalone cluster created beside a supercluster is refused~~ superseded by U-138: the one-cluster-per-namespace rule is removed (design §4.4)                                                | Negative            | —                                                   |
| ~~U-45~~ | ~~A member created before its supercluster is admitted when an approved document names it, refused otherwise~~ superseded by U-146: the supercluster is created first (design §5.4)               | Positive + Negative | —                                                   |
| ~~U-46~~ | ~~A member created while a concurrent aggregate edit lists it under a second supercluster is refused on the member side~~ superseded by U-144: membership is declared by the member (design §4.1) | Negative            | —                                                   |

### Member Selection (design §7.2, §7.3, §9.3)

File: `csi-driver/internal/csi/controller/placement_aggregate_test.go`

| #        | Scenario                                                                                                                                                                   | Type     | Test |
|----------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-47     | `MostAvailableCapacity` picks the member with the most free capacity                                                                                                       | Positive | —    |
| U-48     | `LeastProvisioned` picks the member with the lowest provisioned ratio                                                                                                      | Positive | —    |
| U-170    | `LeastVolumes` picks the member with the fewest PersistentVolumes whose handle names it                                                                                    | Positive | —    |
| U-171    | Members that rank equal under any strategy are chosen between at random, and a retry of the same volume name returns the member it was placed in                           | Positive | —    |
| U-172    | Under `LeastVolumes`, a burst of creates counts the volumes placed whose PersistentVolume does not exist yet, so the burst spreads                                         | Boundary | —    |
| U-49     | Equal ranks break by cluster UUID, identically across calls                                                                                                                | Boundary | —    |
| U-50     | A member with `disablePlacement`, or with a `RemoveMember` running, is never chosen                                                                                        | Negative | —    |
| U-51     | A member whose free capacity equals the requested size is eligible, one byte less is not                                                                                   | Boundary | —    |
| U-52     | A member above its critical threshold is dropped                                                                                                                           | Negative | —    |
| U-53     | A member whose stale reading cannot be refreshed within two seconds is dropped, and the others are still ranked                                                            | Negative | —    |
| U-54     | No candidate fails with `ResourceExhausted`, listing each member's reason                                                                                                  | Negative | —    |
| ~~U-55~~ | ~~An unknown aggregate name fails with `InvalidArgument` naming the class~~ superseded by U-113: a class names a supercluster in `cluster_id`                              | Negative | —    |
| ~~U-56~~ | ~~A class carrying both `supercluster` and `cluster_id` is refused~~ superseded by U-112: a class names a supercluster in `cluster_id`                                     | Negative | —    |
| ~~U-57~~ | ~~`zone_cluster_map` beside `supercluster` restricts candidates to the zone's members~~ superseded by U-111: a class with a zone map never reaches placement (design §7.2) | Positive | —    |
| U-58     | A name existing in a member returns that member without ranking, including a member with `disablePlacement`                                                                | Positive | —    |
| U-59     | A name existing in no member is placed, and the mock receives exactly one create                                                                                           | Positive | —    |
| U-60     | A lookup failing with 5xx in one member fails the call rather than placing, so no duplicate can arise                                                                      | Negative | —    |
| U-61     | The handle's first component is the chosen member's UUID                                                                                                                   | Positive | —    |
| U-62     | A clone through a supercluster class lands in the source's member                                                                                                          | Positive | —    |
| U-63     | A clone whose source member does not belong to the class's supercluster is refused with `InvalidArgument`                                                                  | Negative | —    |
| U-64     | A group whose volumes span two members is refused, naming both members                                                                                                     | Negative | —    |
| U-165    | A labeled request whose group has live members in one member is placed there, whatever the capacity ranking says                                                           | Positive | —    |
| U-166    | A labeled request whose group name exists in two members fails with `FailedPrecondition` naming both                                                                       | Negative | —    |
| U-167    | Two concurrent first volumes of one group land in one member, because the second follows the operator's record of the first                                                | Boundary | —    |
| U-168    | A labeled request whose group lives in a member with `disablePlacement` or under removal fails with `FailedPrecondition` naming the member                                 | Negative | —    |
| U-169    | A label added later to a PVC whose volume is in another member than the group is refused with a Warning event naming the group's member                                    | Negative | —    |

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

| #        | Scenario                                                                                                                                                                               | Type     | Test                                               |
|----------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------|
| U-74     | One node set with several groups derives one standalone cluster named `spec.cluster.name`                                                                                              | Positive | —                                                  |
| U-75     | Two node sets derive two members, `<cluster>-<nodeset>`, and a supercluster named `spec.cluster.name`                                                                                  | Positive | —                                                  |
| U-76     | A node set whose groups name three failure domains derives one member with all three mapped                                                                                            | Positive | —                                                  |
| ~~U-77~~ | ~~A member name of exactly 63 characters is admitted, 64 is refused with `MemberNameInvalid` naming the node set~~ superseded by U-129: names are bounded by `MaxLength` (design §5.3) | Boundary | —                                                  |
| U-78     | A node set name that makes the member name an invalid DNS label is refused with `MemberNameInvalid`                                                                                    | Negative | —                                                  |
| ~~U-79~~ | ~~Thirty-two node sets are admitted, thirty-three are refused with `TooManyMembers`~~ superseded by U-154: there is no member limit, only the 32-storage-node limit (design §5.2)      | Boundary | —                                                  |
| U-80     | Two node sets with different interfaces are not a finding                                                                                                                              | Positive | —                                                  |
| U-81     | Two groups in one node set with different interfaces are a finding                                                                                                                     | Negative | `TestGroupsThatDisagreeAboutInterfacesAreReported` |
| U-82     | Two node sets with different device classes pass the narrowed CEL rule, and each member is stamped with its own class                                                                  | Positive | —                                                  |
| U-83     | With one slot per worker, a node set of 32 workers is admitted and 33 is refused with `MemberTooLarge`                                                                                 | Boundary | —                                                  |
| U-84     | With two slots per worker, a node set of 16 workers is admitted and 17 is refused                                                                                                      | Boundary | —                                                  |
| U-85     | A one-node-set document over 32 storage nodes is refused, so an ordinary cluster is held to the limit too                                                                              | Negative | —                                                  |
| U-86     | A node set below the erasure-coding minimum is refused even when the document's total meets it                                                                                         | Negative | —                                                  |
| U-87     | Growth adding eight workers to a member of thirty is refused with `MemberTooLarge` naming both counts                                                                                  | Boundary | —                                                  |
| U-155    | A worker listed twice within one node set is a finding naming the worker                                                                                                               | Negative | —                                                  |
| U-156    | A worker in two node sets with different sockets and disjoint devices is admitted, and derives one storage node per member on it                                                       | Positive | —                                                  |
| U-157    | Two node sets sharing a worker with an overlapping `socketsToUse` are a `WorkerOverlap` finding naming the worker and both node sets                                                   | Negative | —                                                  |
| U-158    | Two node sets sharing a worker whose groups resolve to one device are a `WorkerOverlap` finding                                                                                        | Negative | —                                                  |

### Balanced Split in the Discovery Generator (design §5.5)

File: `operator/internal/discovery/sizesplit_test.go`

| #    | Scenario                                                                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-88 | Twenty workers with one slot each are drafted as one node set keeping the tier's name, `discovered`                           | Positive | —    |
| U-89 | Thirty-two workers are one node set, thirty-three are two of 17 and 16                                                        | Boundary | —    |
| U-90 | Forty workers with one slot each are two node sets of 20                                                                      | Positive | —    |
| U-91 | Forty workers with two slots each are three node sets of 14, 13, and 13                                                       | Positive | —    |
| U-92 | Sixty-five workers with one slot each are three node sets of 22, 22, and 21                                                   | Positive | —    |
| U-93 | Workers in three failure domains are spread over every node set, with per-domain counts differing by at most one between sets | Positive | —    |
| U-94 | Workers whose management interface names differ are never drafted into one node set                                           | Negative | —    |
| U-95 | Group names are unique within every node set after the split                                                                  | Boundary | —    |
| U-96 | Two runs over an unchanged fleet produce identical node sets and names                                                        | Positive | —    |
| U-97 | A partition below the erasure-coding minimum is drafted without padding, and validation reports the minimum                   | Negative | —    |
| U-98 | An infra tier and a worker tier split independently, drafted as `infra`, `discovered-1`, and `discovered-2`                   | Positive | —    |
| U-99 | Two hundred workers with distinct disks produce no node set above 32 groups                                                   | Boundary | —    |

### Kubernetes Topology and Placement-Group Labels (design §5.7, §7.2)

File: `operator/internal/discovery/topology_test.go`, and
`csi-driver/internal/csi/controller/placement_aggregate_test.go` for U-109 to U-111

| #     | Scenario                                                                                                                                                | Type                | Test |
|-------|---------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-100 | A worker's `topology.kubernetes.io/zone` is copied verbatim into its group's `failureDomain`                                                            | Positive            | —    |
| U-101 | `failureDomainLabel` set to a rack label replaces the zone label as the source                                                                          | Positive            | —    |
| U-102 | A label value that does not fit the `failureDomain` pattern is a finding naming the worker, and is not rewritten                                        | Negative            | —    |
| U-103 | Every worker labeled sets `enableFailureDomains` on the template, no worker labeled leaves it unset                                                     | Positive + Negative | —    |
| U-104 | Some workers labeled is drafted with `TopologyIncomplete` naming the unlabeled ones                                                                     | Boundary            | —    |
| U-105 | Workers in two regions are never drafted into one node set, and node set names carry the region                                                         | Negative            | —    |
| U-106 | Thirty workers in zone `a` and ten in zone `b` are two members of fifteen and five each                                                                 | Positive            | —    |
| U-107 | A hand-written node set whose workers span two regions is refused at approval with `MemberSpansRegions`                                                 | Negative            | —    |
| U-108 | A worker without a region label matches any region, so an unlabeled fleet is never refused for it                                                       | Boundary            | —    |
| U-173 | Every worker of a partition labeled `storage.simplyblock.io/placement-group` forms one node set per value, named by the value, with no split            | Positive            | —    |
| U-174 | Some workers labeled: the unlabeled ones are split as usual and the draft carries `PlacementGroupLabelIncomplete` naming them                           | Boundary            | —    |
| U-175 | A label value whose workers span two regions or two hardware classes is a finding naming the value                                                      | Negative            | —    |
| U-176 | A label value that is not a DNS label of at most 23 characters is a finding naming the workers                                                          | Negative            | —    |
| U-177 | A labeled node set over the storage-node limit is drafted with `MemberTooLarge`                                                                         | Boundary            | —    |
| U-178 | With failure domains enabled, a node set lacking a domain another node set of its region holds is refused with `MemberMissingFailureDomain` naming both | Negative            | —    |
| U-179 | Node sets of different regions are compared only within their region                                                                                    | Positive            | —    |
| U-180 | With failure domains disabled, node sets with different domains are admitted                                                                            | Positive            | —    |
| U-109 | A preferred topology narrows candidates to members in its region                                                                                        | Positive            | —    |
| U-110 | No member in the preferred region with room falls back to the requisite regions, then to every eligible member                                          | Negative            | —    |
| U-111 | A class carrying `zone_cluster_map` restricts by the map and ignores member topology                                                                    | Positive            | —    |

### Aggregate Identifier (design §3, §7.1, §15)

File: `operator/internal/utils/objects_test.go`, and
`csi-driver/internal/csi/controller/placement_aggregate_test.go` for U-112 and U-113

| #     | Scenario                                                                                                                            | Type     | Test |
|-------|-------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-112 | A class whose `cluster_id` names no cluster in the secret calls `Place`, and one naming a member's UUID never calls it              | Positive | —    |
| U-113 | `Place` for an identifier naming no supercluster returns `NotFound`, which the driver returns as `InvalidArgument` naming the class | Negative | —    |
| U-114 | A supercluster's `status.uuid` is its `metadata.uid`                                                                                | Positive | —    |
| U-115 | `ResolveClusterUUID` on a supercluster returns `ErrAggregateCluster`, and the mock control plane receives no call                   | Negative | —    |

### Chain Resolution for Controller Calls (design §8.3)

File: `atlas-lib/controlplane/chain_test.go` and
`csi-driver/internal/csi/controller/chain_test.go`

| #     | Scenario                                                                                                                                        | Type     | Test |
|-------|-------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-116 | Expand, snapshot, clone from a volume, publish, and unpublish on a relocated volume's original handle reach the target through the relationship | Positive | —    |
| U-117 | A handle whose source volume still exists is not redirected, and the relationship endpoint is not called                                        | Negative | —    |
| U-118 | A chain longer than the hop limit fails with a named error instead of looping                                                                   | Boundary | —    |
| U-189 | DeleteVolume of a PV whose `relocated-to` names the chain's active volume deletes it with the retired members                                   | Positive | —    |
| U-190 | DeleteVolume of an unmarked PV with the same chain, the disaster-recovery case, deletes no active volume                                        | Negative | —    |
| U-191 | DeleteVolume of a PV whose mark names a volume other than the active one deletes no active volume                                               | Negative | —    |
| U-192 | A marked PV's resolution starts at the mark's handle and follows a later fail-over relationship of the destination                              | Positive | —    |
| U-193 | DeleteVolume of a marked PV whose source cluster is gone from the secret deletes the destination instead of reporting it deleted                | Negative | —    |

### Ceiling Shares (design §7.1, §7.5)

File: `operator/internal/controllers/pool/shares_test.go`

| #         | Scenario                                                                                                                                           | Type     | Test |
|-----------|----------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-119     | A ceiling of 100k IOPS over three eligible members is 33,334, 33,333, and 33,333, and sums to the ceiling                                          | Boundary | —    |
| U-120     | A member leaving placement has its share divided among the rest                                                                                    | Positive | —    |
| U-121     | Demand shares give every eligible member a floor of 10% of an even share and divide the rest by rate                                               | Positive | —    |
| ~~U-122~~ | ~~A share moving by less than 5% is not written, and one moving by more is~~ superseded by U-186: the hysteresis is the allocation's (design §7.5) | Boundary | —    |
| U-123     | Lowered shares are written before raised ones, so the sum never exceeds the ceiling                                                                | Positive | —    |
| U-186     | Moving from 10k/45k/45k to 11k/44.5k/44.5k writes all three shares, both decreases before the increase, and the applied sum never exceeds 100k     | Boundary | —    |
| U-187     | A decrease that fails ends the round with no increase sent, and the next interval writes a fresh allocation                                        | Negative | —    |
| U-188     | An allocation in which no share moved by more than 5% writes nothing                                                                               | Negative | —    |

### Per-Node-Set Overrides and Name Limits (design §5.3, §5.8)

File: `operator/internal/controllers/deployment/supercluster_members_test.go`

| #         | Scenario                                                                                                                                                                                           | Type                | Test |
|-----------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-124     | A node set's `vcpuCount` override reaches its member only, and the other members inherit the template                                                                                              | Positive            | —    |
| U-125     | Slots are counted per node set: 16 workers at two slots in one set and 32 at one slot in another are both admitted                                                                                 | Boundary            | —    |
| U-126     | Overrides on a document with one node set are refused                                                                                                                                              | Negative            | —    |
| U-127     | An NVMe node set beside a logical block node set is admitted, and a node set mixing both is refused                                                                                                | Positive + Negative | —    |
| U-128     | The erasure-coding minimum is checked against a node set's own `stripe` override                                                                                                                   | Boundary            | —    |
| ~~U-129~~ | ~~A cluster name of 35 characters is admitted and 36 is refused, a node set name of 26 is admitted and 27 is refused~~ superseded by U-154: node set names are at most 23 characters (design §5.3) | Boundary            | —    |
| ~~U-130~~ | ~~The generator cuts a node set base to 23 characters and appends `-1` to `-32`~~ superseded by U-151: members carry a random suffix (design §5.3)                                                 | Boundary            | —    |
| U-194     | A node set's `vcpuCount` override of 4 is admitted and 3 is refused by the API server, as the template and `StorageClusterSpec` refuse it                                                          | Boundary            | —    |

### Fan-Out Admission (design §9.6)

File: `operator/internal/webhook/storageclusterops_validator_test.go`

| #     | Scenario                                                                                                                                    | Type                | Test |
|-------|---------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-131 | `RollingRestart`, `Restart`, `Shutdown`, and `Start` on a supercluster are admitted, and `Activate`, `Expand`, and `CancelTask` are refused | Positive + Negative | —    |

### Encrypted Volumes Are Never Moved (design §9.4)

File: `operator/internal/controllers/volume/relocate_test.go`

| #     | Scenario                                                                                                                                                                                        | Type     | Test |
|-------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-132 | A volume the control plane reports with `crypto_bdev` set, through a class that does not enable encryption, fails in `Validating`, and the mock receives no `replication/start` and no `commit` | Negative | —    |

### Workload Names and the Shared Certificate (design §6)

File: `atlas-lib/kube/names_test.go` and
`operator/internal/controllers/cluster/workload_names_test.go`

| #     | Scenario                                                                                                                                                                                   | Type     | Test |
|-------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-133 | A new standalone cluster in a namespace where no cluster records the constant names records `simplyblock-storage-node-api`, `simplyblock-spdk-proxy`, and their `-tls` Secrets             | Positive | —    |
| U-134 | A second standalone cluster in that namespace records `sb-<name>-node-api` and `sb-<name>-spdk-proxy`, and a supercluster always does                                                      | Positive | —    |
| U-135 | A name over 63 characters is cut to 40 characters and an eight-character hash, is a valid Service name, and is the same on every call                                                      | Boundary | —    |
| U-160 | A cluster with `status.uuid` set and no `status.workloadNames` is recorded with the constant names and no suffix, and its Services keep their names                                        | Positive | —    |
| U-161 | `status.workloadNames` is written before any workload object and before the backend cluster is created, and a later reconcile never changes it                                             | Positive | —    |
| U-162 | A member records its supercluster's Services and Secrets and its own `-<suffix>`                                                                                                           | Positive | —    |
| U-163 | Two standalone clusters created at once that both record the constant names: the one that finds the Service owned by the other rewrites to derived names before its backend cluster exists | Boundary | —    |
| U-164 | Every workload builder reads its names from `status.workloadNames` and none derives them                                                                                                   | Positive | —    |
| U-136 | A member's hostnames are `<node>-<suffix>.sb-<supercluster>-node-api.<namespace>.svc`, and its EndpointSlice is named for the member and lists its agents' pod addresses                   | Positive | —    |
| U-159 | Two members with storage nodes on one worker get distinct hostnames in the shared Service, and a label over 63 characters is cut by the hash rule                                          | Positive | —    |
| U-137 | A member mounts the supercluster's certificate Secret, and no Certificate is created for the member                                                                                        | Positive | —    |

### Membership Admission (design §4.1, §4.4)

File: `operator/internal/webhook/storagecluster_validator_test.go`

| #     | Scenario                                                                                                                                                                        | Type                | Test |
|-------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-138 | A second standalone cluster in one namespace is admitted                                                                                                                        | Positive            | —    |
| U-139 | A member whose `superclusterRef` names a supercluster in its namespace is admitted                                                                                              | Positive            | —    |
| U-140 | A `superclusterRef` naming nothing, a standalone cluster, or a supercluster in another namespace is refused                                                                     | Negative            | —    |
| U-141 | A cluster carrying both `supercluster` and `superclusterRef` is refused                                                                                                         | Negative            | —    |
| U-142 | A supercluster with no members is admitted and reports `Provisioning`                                                                                                           | Boundary            | —    |
| U-143 | `disablePlacement` on a cluster that is not a member is refused                                                                                                                 | Negative            | —    |
| U-144 | Adding or removing `supercluster`, and adding, changing, or removing `superclusterRef`, after creation is refused                                                               | Negative            | —    |
| U-145 | Deleting a supercluster with members is refused, deleting a member whose pools report bound volumes is refused outside `RemoveMember`, and deleting an empty member is admitted | Positive + Negative | —    |
| U-146 | A member created before its supercluster is refused                                                                                                                             | Negative            | —    |

### Placement Service (design §7.2, §7.3)

File: `operator/internal/placement/placement_test.go`, and
`csi-driver/internal/csi/controller/placement_aggregate_test.go` for U-148 and U-149

| #     | Scenario                                                                                                                                     | Type     | Test |
|-------|----------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-147 | Sizes placed since a member's last reading are subtracted from its free capacity, so ten placements in a burst spread over two equal members | Positive | —    |
| U-148 | The link to the operator being down fails `CreateVolume` with `Unavailable`, and no create reaches the control plane                         | Negative | —    |
| U-149 | A class naming a member's UUID in `cluster_id` never calls `Place`                                                                           | Negative | —    |
| U-150 | Two concurrent `Place` calls for one supercluster each see the size the other placed                                                         | Boundary | —    |
| U-183 | A chosen member is written to the PVC annotation before `Place` answers, and a failed patch fails the call with `Unavailable`                | Positive | —    |
| U-184 | A retry for a PVC carrying a placement record returns that member while the name lookup finds nothing                                        | Negative | —    |
| U-185 | A request without the PVC name and namespace fails with `FailedPrecondition` and places nothing                                              | Negative | —    |

### Member Names (design §5.3, §5.5)

File: `operator/internal/controllers/deployment/supercluster_members_test.go`, and
`operator/internal/discovery/sizesplit_test.go` for U-151

| #     | Scenario                                                                                                                                                          | Type                | Test |
|-------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| U-151 | A generated node set name is cut to 23 characters, keeping its number                                                                                             | Boundary            | —    |
| U-152 | A member's suffix is three random lowercase letters, and a drawn name that already exists, or a suffix another member of the supercluster carries, is drawn again | Positive + Negative | —    |
| U-153 | Two growth documents with alike node set names produce two distinct members                                                                                       | Positive            | —    |
| U-154 | A cluster name of 35 characters is admitted and 36 is refused, a node set name of 23 is admitted and 24 is refused                                                | Boundary            | —    |

---

## 2. Integration Tests

Run the full controller reconciliation loop against a mock control plane HTTP
server and a real Kubernetes API through `envtest`.

### Supercluster Reconcile (design §10.1)

| #        | Scenario                                                                                                                                                                        | Type     | Test |
|----------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01     | A supercluster of two online members reports `Online`, both members eligible, and `observedGeneration`                                                                          | Positive | —    |
| I-02     | Each member reports `role: PlacementGroup` and `superclusterRef`, and an unrelated cluster `Standalone`                                                                         | Positive | —    |
| I-03     | A member going `Unavailable` makes the supercluster `Degraded`, drops the member from placement, and emits `MemberPlacementChanged`                                             | Positive | —    |
| ~~I-04~~ | ~~A missing member is reported `Pending` with `MemberNotFound`, and the other member is still read~~ superseded by U-140: membership is declared by the member (design §4.1)    | Negative | —    |
| ~~I-05~~ | ~~The secret gains an `aggregates` entry, and the `clusters` entries the cluster controller wrote are untouched~~ superseded by I-53: placement is the operator's (design §7.2) | Positive | —    |
| ~~I-06~~ | ~~Setting `disablePlacement` flips the member's `placement` in the secret~~ superseded by I-54: placement is the operator's (design §7.2)                                       | Positive | —    |
| I-07     | The capacity endpoint returning 5xx keeps the last reading and requeues                                                                                                         | Negative | —    |
| I-08     | The volume rebalancer and the arbitration controller never reconcile the supercluster                                                                                           | Negative | —    |
| ~~I-09~~ | ~~Two superclusters in two namespaces write two entries and never read each other's members~~ superseded by I-55: placement is the operator's (design §7.2)                     | Positive | —    |
| I-10     | Crossing the combined warning threshold emits `AggregateCapacityThreshold` once                                                                                                 | Boundary | —    |
| ~~I-11~~ | ~~A reconcile with nothing changed issues no status write and no secret write~~ superseded by I-56: placement is the operator's (design §7.2)                                   | Negative | —    |

### Expansion (design §5.3, §5.4)

| #        | Scenario                                                                                                                                                                                      | Type     | Test |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| ~~I-12~~ | ~~A two-group document creates two members, then the supercluster, then nodes per member, and one activation per member~~ superseded by I-58: the supercluster is created first (design §5.4) | Positive | —    |
| I-13     | A one-node-set document expands into one cluster with no supercluster                                                                                                                         | Positive | —    |
| ~~I-14~~ | ~~A restart between creating members and creating the supercluster creates the supercluster and no second member~~ superseded by I-59: member names are drawn and recorded (design §5.4)      | Negative | —    |
| I-15     | A member whose nodes are online is activated while another member's nodes are still coming up                                                                                                 | Positive | —    |
| I-16     | `status.clusterRef` names the supercluster and `status.nodeRefs` lists every member's nodes                                                                                                   | Positive | —    |
| ~~I-17~~ | ~~Growth with existing group names adds nodes to those members only~~ superseded by I-60: growth is decided by what `clusterRef` names (design §5.6)                                          | Positive | —    |
| ~~I-18~~ | ~~Growth with a new group name creates the member and appends it to the aggregate~~ superseded by I-61: growth is decided by what `clusterRef` names (design §5.6)                            | Positive | —    |
| ~~I-19~~ | ~~Growth with two group names against a standalone cluster is refused with `ClusterNotAggregate`~~ superseded by I-60: growth is decided by what `clusterRef` names (design §5.6)             | Negative | —    |
| ~~I-20~~ | ~~Appending to the aggregate retries on a conflicting concurrent edit and keeps the administrator's change~~ superseded by I-61: growth is decided by what `clusterRef` names (design §5.6)   | Negative | —    |

### Pools (design §7.1)

| #        | Scenario                                                                                                                                   | Type     | Test |
|----------|--------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-21     | A pool on a supercluster creates one backend pool per member under the pool's name                                                         | Positive | —    |
| I-22     | The pool is `Ready` only when every eligible member has it                                                                                 | Boundary | —    |
| I-23     | A 409 on an existing backend pool is success, and the mock sees no second create                                                           | Negative | —    |
| I-24     | One member's create failing leaves that member ineligible and the pool status naming it                                                    | Negative | —    |
| I-25     | Every member's backend pool admits every allowed node's NQN, including a consuming node that runs no storage node                          | Positive | —    |
| ~~I-26~~ | ~~Limits are applied unchanged to every member's backend pool~~ superseded by U-119: the ceiling is the supercluster's (design §7.1)       | Positive | —    |
| I-27     | A member added later receives every pool before it becomes eligible                                                                        | Positive | —    |
| ~~I-28~~ | ~~The generated StorageClass carries `supercluster` and no `cluster_id`~~ superseded by I-47: a class names a supercluster in `cluster_id` | Positive | —    |
| ~~I-29~~ | ~~Deleting the supercluster is held while a pool names it~~ superseded by U-145: deletion is decided at admission (design §4.4)            | Negative | —    |

### Relocate (design §8.2)

| #        | Scenario                                                                                                                                                                                                                                       | Type                | Test |
|----------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------|
| ~~I-30~~ | ~~The happy path calls start, polls status, runs the attach Jobs, commits once, and succeeds~~ superseded by I-44: the cutover follows the replication slot's order (design §8.2)                                                              | Positive            | —    |
| I-31     | Volumes sharing the subsystem are listed in `status.relocate.volumes` and moved together                                                                                                                                                       | Positive            | —    |
| I-32     | Abort before commit calls stop, deletes the Jobs, and ends `Aborted`                                                                                                                                                                           | Positive            | —    |
| I-33     | Abort after commit is ignored with `RelocationAbortIgnored`                                                                                                                                                                                    | Negative            | —    |
| ~~I-34~~ | ~~A restart in every step resumes there, and the mock sees one start and one commit~~ superseded by I-71 … I-73: the commit is resent until a task is recorded (design §8.2)                                                                   | Negative            | —    |
| I-35     | Start or commit returning 5xx is retried without advancing the step                                                                                                                                                                            | Negative            | —    |
| I-36     | Start returning 4xx fails the operation with the control plane's message                                                                                                                                                                       | Negative            | —    |
| ~~I-37~~ | ~~A failing attach Job fails the operation after stop~~ superseded by I-45: a failing Job holds instead of failing (design §8.2)                                                                                                               | Negative            | —    |
| I-38     | The original handle not resolving after commit fails with `HandleUnresolved`                                                                                                                                                                   | Negative            | —    |
| I-39     | A `Migrate` and a `Relocate` of one volume exclude each other                                                                                                                                                                                  | Negative            | —    |
| ~~I-40~~ | ~~`Evacuate` cordons the member, raises one `Relocate` per subsystem within the concurrency bound, and removes the entry when the member is empty~~ superseded by I-63: `RemoveMember` replaces `Evacuate` (design §8.4)                       | Positive            | —    |
| ~~I-41~~ | ~~A failing child holds the evacuation with `EvacuationHeld`~~ superseded by I-64: `RemoveMember` replaces `Evacuate` (design §8.4)                                                                                                            | Negative            | —    |
| I-42     | A draft with two node sets names both members in `status.message` before approval                                                                                                                                                              | Positive            | —    |
| ~~I-43~~ | ~~The aggregate entry in the secret carries each member's region and zones, read from its nodes' `Node` labels, and follows a relabeled node~~ superseded by I-57: placement is the operator's (design §7.2)                                   | Positive            | —    |
| I-44     | `Relocate` calls start, waits for sync, commits once, preconnects every consuming node with the paths `connect` returns at `cutover_pending`, sends `cutover-proceed` once, and verifies through the relationship                              | Positive            | —    |
| I-45     | A failing preconnect Job holds the step with `PathAttachHeld`, and the mock receives no `cutover-proceed`                                                                                                                                      | Negative            | —    |
| ~~I-46~~ | ~~A restart after `proceeded` is set sends no second `cutover-proceed`~~ superseded by I-74 and I-75: the proceed is resent while `cutover_pending` (design §8.2)                                                                              | Negative            | —    |
| I-47     | The generated StorageClass for a supercluster pool carries the supercluster's `status.uuid` in `cluster_id`                                                                                                                                    | Positive            | —    |
| I-48     | A `RollingRestart` on a supercluster raises one member operation at a time in order of member name, and records progress in `status.fanOut`                                                                                                    | Positive            | —    |
| I-49     | A failing member operation holds the walk with `FanOutHeld`, and no later member is touched                                                                                                                                                    | Negative            | —    |
| I-50     | A member already running its own operation holds the walk until that operation finishes                                                                                                                                                        | Negative            | —    |
| I-51     | A member's backend pool limits are written with its share, and a member leaving placement rewrites the others' shares                                                                                                                          | Positive            | —    |
| ~~I-52~~ | ~~`Evacuate` of a member holding an encrypted volume holds with `EvacuationHeld` on it, never raises a `commit` for it, and never removes the member from the aggregate~~ superseded by I-65: `RemoveMember` replaces `Evacuate` (design §8.4) | Negative            | —    |
| I-53     | `Place` answers from the supercluster reconciler's readings, and the chosen member is the handle's first component                                                                                                                             | Positive            | —    |
| I-54     | Setting a member's `disablePlacement` removes it from `Place` answers and emits `MemberPlacementChanged`                                                                                                                                       | Positive            | —    |
| I-55     | Two superclusters in two namespaces each list only the members naming them                                                                                                                                                                     | Positive            | —    |
| I-56     | A reconcile with nothing changed issues no status write                                                                                                                                                                                        | Negative            | —    |
| I-57     | Each member's region and zones are read from its nodes' `Node` labels, and follow a relabeled node                                                                                                                                             | Positive            | —    |
| I-58     | A two-node-set document creates the supercluster, records two drawn member names, creates the members naming it, their nodes, and one activation per member                                                                                    | Positive            | —    |
| I-59     | A restart after the names are recorded creates the recorded members and no others                                                                                                                                                              | Negative            | —    |
| I-60     | A document naming a member adds nodes to it, and one naming a standalone cluster adds nodes to that cluster                                                                                                                                    | Positive            | —    |
| I-61     | A document naming a supercluster creates one new member per node set, each naming the supercluster, and a document without `spec.cluster` is refused with `MemberSizingUnstated`                                                               | Positive + Negative | —    |
| I-62     | A new member holds before creating its backend cluster or any workload until the supercluster's shared Services and certificate are ready                                                                                                      | Negative            | —    |
| I-63     | `RemoveMember` takes the member out of placement, raises one `Relocate` per subsystem within the concurrency bound, deletes the member once its pools hold no volume, and emits `MemberRemoved`                                                | Positive            | —    |
| I-64     | A failing child holds the removal with `RemovalHeld`, and the member is not deleted                                                                                                                                                            | Negative            | —    |
| I-65     | `RemoveMember` of a member holding an encrypted volume fails in `Validating`, raises no `Relocate`, and deletes nothing                                                                                                                        | Negative            | —    |
| I-66     | Aborting during `Draining` raises no further children, lets running ones finish, and returns the member to placement                                                                                                                           | Positive            | —    |
| I-67     | The supercluster creates one Service per API and one serving certificate, and a member joining later creates no Certificate                                                                                                                    | Positive            | —    |
| I-68     | Creating a supercluster's member sends the recorded Services, Secret, and suffix in the `POST /api/v2/clusters/` body to a fake control plane                                                                                                  | Positive            | —    |
| I-69     | Upgrading the operator over a cluster without `status.workloadNames` leaves its Services, Secrets, and node hostnames unchanged                                                                                                                | Positive            | —    |
| I-70     | The §4.5 supercluster created through `envtest` reads back with every defaulted field set, and the validator admits the object as read                                                                                                         | Positive            | —    |
| I-71     | A restart after `committed` is written and before the commit is sent sends the commit, and one task is recorded                                                                                                                                | Negative            | —    |
| I-72     | A restart after the commit returned 202 and before `taskID` is written resends it, records the task the 409 names, and the mock runs one cutover                                                                                               | Negative            | —    |
| I-73     | A commit the mock accepts but answers only after the client timeout is resent, and the task the 409 names is recorded                                                                                                                          | Negative            | —    |
| I-74     | A restart after `proceeded` is written and before the proceed is sent sends it while the relationship is `cutover_pending`                                                                                                                     | Negative            | —    |
| I-75     | A restart after the relationship left `cutover_pending` sends no proceed and advances to `Verifying`                                                                                                                                           | Negative            | —    |
| I-76     | `RemoveMember` holds in `Draining` with `RemovalHeld` naming a moved PV without a mark, and deletes the member once the PV is marked                                                                                                           | Negative            | —    |
| I-77     | `RemoveMember` of a member holding a volume with a `failover`-mode relationship fails in `Validating`, naming the volume                                                                                                                       | Negative            | —    |

---

## 3. E2E Tests

Run against a live simplyblock control plane with three members of three nodes
each, using fio in verify mode for every data-path row. Rows E-06 to E-08 need a
member pair whose storage networks reach each other (P0-2).

| #    | Scenario                                                                                                                                            | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | A two-node-set deployment document produces a supercluster and two activated members                                                                | Positive | —    |
| E-02 | Thirty PVCs through a supercluster class spread across members by the strategy, and every volume passes fio verify                                  | Positive | —    |
| E-03 | `CreateVolume` whose create times out in member A and becomes visible only after the retry is retried in A, and one backend volume results          | Negative | —    |
| E-04 | A member shut down leaves the others' volumes serving and new volumes landing elsewhere                                                             | Negative | —    |
| E-05 | Snapshot, clone, and expansion of a volume stay in its member                                                                                       | Positive | —    |
| E-06 | A relocation under sustained fio completes with no I/O error and verified data                                                                      | Positive | —    |
| E-07 | Expand and snapshot after a relocation succeed through the original handle, and deleting the PVC leaves no volume in either member                  | Positive | —    |
| E-08 | A member removed under load is deleted with every volume moved out and intact                                                                       | Positive | —    |
| E-09 | Discovery over a fleet of more than 32 storage nodes (17 workers with two slots each) drafts two node sets that approve and expand into two members | Positive | —    |
| E-10 | Removing the source member of a relocated PV, then restarting its pod, remounts the volume from the destination with its data intact                | Positive | —    |

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

| Axis            | Values covered                                                                                         | IDs                                      | Not covered                                |
|-----------------|--------------------------------------------------------------------------------------------------------|------------------------------------------|--------------------------------------------|
| Cluster count   | standalone, two standalone in one namespace, supercluster of none, 2, and 3 members, two superclusters | U-74, U-138, U-142, E-02, I-55           | many members on a live control plane       |
| Member topology | one-node member, three-node member                                                                     | U-12, E-01                               | five-node members, members of unequal size |
| Member size     | 16 and 17 workers at two slots, 32 and 33 at one slot, 20 to 65 workers split                          | U-84, U-83, U-89, U-90, U-91, U-92, E-09 | —                                          |
| Topology        | zones labeled, unlabeled, partially labeled, rack label, two regions                                   | U-100, U-103, U-104, U-101, U-105, U-109 | a live cluster stretched over two regions  |
| Namespace scope | one namespace, two namespaces                                                                          | U-139, U-140, I-55                       | —                                          |
| Lifecycle       | restart mid-expansion, restart in every relocate step                                                  | I-59, I-71 … I-75                        | restart mid-removal                        |
| Control plane   | 4xx, 5xx, timeout, 409                                                                                 | I-36, I-35, U-53, I-23                   | —                                          |
| Data path       | spread, failure, relocation, member removal                                                            | E-02, E-04, E-06, E-08                   | —                                          |

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 157       | 4       | all others  |
| Integration | 55        | 0       | all of them |
| E2E         | 10        | 0       | E-01 … E-10 |
| Manual      | 2         | 0       | M-01, M-02  |

The four covered unit rows are existing tests of behavior this design keeps:
U-25, U-27, U-28, and U-81. The counts exclude struck rows, each superseded by
the row its strike names: U-10 … U-18, U-20 … U-23, U-26, U-29 … U-33, U-35, U-38 … U-46, U-55 … U-57, U-77, U-79, U-122, U-129 … U-130, and I-04 … I-06, I-09, I-11 … I-12, I-14, I-17 … I-20, I-26, I-28 … I-30, I-34, I-37, I-40 … I-41, I-43, I-46, I-52.

---

## 7. What Is Not Yet Covered

| #                                                                                                                                                        | Gap                                               | Reason                                                                                                   |
|----------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------|----------------------------------------------------------------------------------------------------------|
| U-01 … U-09, U-19, U-24, U-34, U-36 … U-37, U-47 … U-54, U-58 … U-76, U-78, U-80, U-82 … U-128, U-131 … U-194                                            | Every new unit scenario                           | Feature not implemented (Phases 1 to 5)                                                                  |
| I-01 … I-03, I-07 … I-08, I-10, I-13, I-15 … I-16, I-21 … I-25, I-27, I-31 … I-33, I-35 … I-36, I-38 … I-39, I-42, I-44 … I-45, I-47 … I-51, I-53 … I-77 | Every integration scenario                        | Feature not implemented (Phases 1 to 5)                                                                  |
| E-01 … E-05, E-09                                                                                                                                        | Live supercluster, placement, and discovery split | Feature not implemented (Phases 1 and 2)                                                                 |
| E-06 … E-08, E-10                                                                                                                                        | Live relocation and member removal                | Feature not implemented (Phases 3 and 4)                                                                 |
| M-01, M-02                                                                                                                                               | Failure injection                                 | Needs node power control and selective network blocking                                                  |
| —                                                                                                                                                        | Many members on a live control plane              | No lab of that size. Placement and roll-up are covered by fixtures                                       |
| —                                                                                                                                                        | A live cluster stretched over two regions         | No such lab. U-105, U-107, and U-109 cover the decisions                                                 |
| —                                                                                                                                                        | Five-node and unequal-size members                | Placement does not read node count, and capacity fixtures in U-47 and U-48 cover it                      |
| —                                                                                                                                                        | Restart mid-removal                               | Removal resumes through its children, each covered by I-71 … I-75. A dedicated row is added with Phase 4 |
