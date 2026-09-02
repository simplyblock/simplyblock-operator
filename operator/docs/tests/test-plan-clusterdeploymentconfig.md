# Test Plan: ClusterDeploymentConfig and OperatorOps

Related design: [`designs/crd-redesign/design-clusterdeploymentconfig.md`](../designs/crd-redesign/design-clusterdeploymentconfig.md)

Scope is the operator, its webhooks, and the Kubernetes surface this repository
builds. The control plane (`sbcli`) is a dependency, faked at the boundary: what
a row asserts is the operator's response to an answer, never the control plane's
own behavior.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Both kinds are new, so every row is a specification rather than a gap against
shipped behavior. Nothing here has a shipped spelling to name.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock deployment, real workers with real devices            |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client. Expansion is a
pure function of a document plus the cluster's current state, which is why most
of this document's behavior lands here.

### Draft Validation (design §4.1)

File: `operator/internal/controllers/deployment/clusterdeploymentconfig_validate_test.go`

| #    | Scenario                                                                     | Type     | Test |
|------|------------------------------------------------------------------------------|----------|------|
| U-01 | A valid draft: `AwaitingApproval` emitted, phase stays `Draft`               | Positive | —    |
| U-02 | A draft naming a worker that does not exist: `WorkerNotFound`, still `Draft` | Negative | —    |
| U-03 | A draft naming a device no node advertises: `DeviceNotFound`, still `Draft`  | Negative | —    |
| U-04 | A draft is never expanded, whatever it contains                              | Negative | —    |
| U-05 | Validation writes what it found to `status.message` and nothing else         | Positive | —    |
| U-06 | One worker in two groups of one node set: rejected as ambiguous              | Negative | —    |
| U-07 | One worker in two node sets: rejected for the same reason                    | Negative | —    |
| U-08 | A node set with no groups: rejected by `MinItems`                            | Boundary | —    |
| U-09 | A group with no workers: rejected by `MinItems`                              | Boundary | —    |
| U-10 | `AwaitingApproval` is emitted once, not on every reconcile                   | Boundary | —    |
| U-11 | A draft that becomes valid after an edit: the event is emitted then          | Positive | —    |

### Expansion (design §4.2, §6)

File: `operator/internal/controllers/deployment/clusterdeploymentconfig_expand_test.go`

| #    | Scenario                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------|----------|------|
| U-12 | One group of two workers, one socket, one node per socket: two `StorageNode` objects | Positive | —    |
| U-13 | The same on a two-socket layout: four objects, slots 0 and 1 per worker              | Positive | —    |
| U-14 | `nodesPerSocket` of 2 on two sockets: four slots per worker                          | Boundary | —    |
| U-15 | Each node carries its group's device selection                                       | Positive | —    |
| U-16 | Each node carries its node set's sizing in `spec.config.sizing`                      | Positive | —    |
| U-17 | Each node carries `spec.nodeSet` naming the set it was declared in                   | Positive | —    |
| U-18 | Each node carries a controller reference to the `StorageCluster`, not to the config  | Positive | —    |
| U-19 | Two node sets with different sizing: their nodes differ accordingly                  | Positive | —    |
| U-20 | Re-entering `CreatingNodes` with every node present: nothing is created              | Negative | —    |
| U-21 | Re-entering with half the nodes present: only the missing slots are created          | Positive | —    |
| U-22 | `status.clusterRef` and `status.nodeRefs` record what was produced                   | Positive | —    |
| U-23 | The expansion finishes without waiting for a node to be provisioned                  | Positive | —    |
| U-24 | A cluster whose creation fails: the phase becomes `Failed` with the reason           | Negative | —    |
| U-25 | `AwaitingCluster` holds until `status.uuid` is set                                   | Negative | —    |
| U-26 | A step's deadline expires: `StepDeadlineExceeded`, phase does not advance            | Boundary | —    |

### Create-Only Semantics (design §6)

| #    | Scenario                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------|----------|------|
| U-27 | No existing cluster, no `clusterRef`: the cluster and all nodes are created     | Positive | —    |
| U-28 | An existing cluster, no `clusterRef`: refused with `ClusterExists`, `Failed`    | Negative | —    |
| U-29 | An existing cluster named by `clusterRef`: only missing nodes are added         | Positive | —    |
| U-30 | No existing cluster but `clusterRef` set: refused with `ClusterNotFound`        | Negative | —    |
| U-31 | A second config adding a rack to an existing cluster: only the rack is created  | Positive | —    |
| U-32 | A node set omitted from a later config: nothing is drained or removed           | Negative | —    |
| U-33 | A device removed from a group in a later config: the existing node is untouched | Negative | —    |
| U-34 | A later config whose group changes a device list: no node is rewritten          | Negative | —    |

### Approval (design §5)

| #    | Scenario                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------|----------|------|
| U-35 | `spec.approved` false: expansion is never entered                               | Negative | —    |
| U-36 | `spec.approved` set true: expansion begins on the next reconcile                | Positive | —    |
| U-37 | The `ready-to-deploy` label is written on an approved config                    | Positive | —    |
| U-38 | The `ready-to-deploy` label is read from nowhere: setting it alone does nothing | Negative | —    |
| U-39 | Expansion is held while the `ControlPlane` is not `Ready`                       | Negative | —    |
| U-40 | The `ControlPlane` becomes `Ready`: the held expansion proceeds unattended      | Positive | —    |
| U-41 | No `ControlPlane` at all: held with a clear reason, not failed                  | Negative | —    |

### Deletion (design §4.3)

| #    | Scenario                                                                | Type     | Test |
|------|-------------------------------------------------------------------------|----------|------|
| U-42 | Deleting an `Expanded` config: the cluster and its nodes are untouched  | Negative | —    |
| U-43 | Deleting a `Draft`: nothing was created, nothing is removed             | Negative | —    |
| U-44 | The config carries no finalizer, so deletion is immediate               | Boundary | —    |
| U-45 | No object created by the expansion has an owner reference to the config | Negative | —    |

### Discovery (design §8)

File: `operator/internal/controllers/deployment/operatorops_discover_test.go`

| #        | Scenario                                                                                                     | Type     | Test |
|----------|--------------------------------------------------------------------------------------------------------------|----------|------|
| U-46     | Three workers with identical devices: one group of three                                                     | Positive | —    |
| U-47     | Two workers with differing devices: two groups                                                               | Positive | —    |
| U-48     | No schedulable worker: an empty draft with a message saying so                                               | Boundary | —    |
| U-49     | One worker: one group of one                                                                                 | Boundary | —    |
| U-50     | A `nodeSelector` that matches a subset: only those workers are inspected                                     | Positive | —    |
| U-51     | A worker whose devices cannot be read: `DeviceInspectionFailed`, the run continues                           | Negative | —    |
| U-52     | Nodes carrying `topology.kubernetes.io/zone`: it seeds `failureDomain`                                       | Positive | —    |
| U-53     | Nodes carrying no topology label: `failureDomain` is left unset, not guessed                                 | Negative | —    |
| U-54     | An OpenShift cluster: `spec.environment` is `OpenShift`                                                      | Positive | —    |
| U-55     | An unrecognized distribution: `spec.environment` is `Vanilla`                                                | Boundary | —    |
| U-56     | No `StorageCluster` in the namespace: a create-shaped draft with no `clusterRef`                             | Positive | —    |
| ~~U-57~~ | A reachable control plane with an existing cluster. Withdrawn: discovery makes no backend call (design §8.1) | —        | —    |
| U-58     | The output always has `spec.approved` false                                                                  | Positive | —    |
| U-59     | A second run writes a second document, never editing the first                                               | Negative | —    |
| U-60     | `spec.discover.configName` set: that name is used                                                            | Positive | —    |
| U-61     | `configName` unset: a generated name that cannot collide with the first run                                  | Boundary | —    |
| U-62     | Discovery changes nothing: no cluster, no node, no control-plane write                                       | Negative | —    |

### OperatorOps Lifecycle (design §7)

File: `operator/internal/controllers/deployment/operatorops_controller_unit_test.go`

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-63 | The spec carries no target reference, and none is required                    | Positive | —    |
| U-64 | An unknown action: terminal failure with the action in the message            | Negative | —    |
| U-72 | A step value the running code does not declare: `ErrUnknownState`, `Failed`   | Negative | —    |
| U-74 | `spec.discover` absent for `action: Discover`: accepted, since it is optional | Boundary | —    |
| U-75 | Every declared state appears in the step `Enum` and in the CEL rule           | Boundary | —    |

`U-65` through `U-71` and `U-73` covered an `Upgrade` action that design §7.1
removed. Their IDs are retired rather than reused.

### Device Selection and the Discovery Filter (design §3.1, §8.1)

Files: `operator/internal/controllers/deployment/clusterdeploymentconfig_expand_test.go`
and `operator/internal/controllers/deployment/operatorops_discover_test.go`

| #    | Scenario                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------|----------|------|
| U-76 | A group's `devices.nvme`: the node's `pcieAllowList` carries exactly those addresses | Positive | —    |
| U-77 | A group's `devices.block`: the node's `deviceNames` carries exactly those names      | Positive | —    |
| U-78 | A group naming both: both node fields are set, and neither is widened                | Positive | —    |
| U-79 | A group whose `devices` is absent: the node inherits no device fields                | Boundary | —    |
| U-80 | A `deviceFilter` allow list: only those addresses reach the draft                    | Positive | —    |
| U-81 | A `deviceFilter` deny list: the denied address reaches no group of the draft         | Negative | —    |
| U-82 | A `deviceFilter` model and size range: only matching devices reach the draft         | Positive | —    |
| U-83 | A `deviceFilter` matching nothing: an empty draft with a message saying so           | Boundary | —    |
| U-84 | `deviceFilter` absent: every advertised device reaches the draft, boot device too    | Boundary | —    |
| U-85 | The draft carries the resolved list and no filter anywhere in its spec               | Negative | —    |

`U-85` is the row that keeps design §8.1's rule true, since a discovery run that
copied its own filter into the document would make the document mean something
different on hardware that changed.

### Approval Webhook (design §5.1, §5.2)

File: `operator/internal/webhook/clusterdeploymentconfig_validator_test.go`

The handler is a pure function of an admission request and a lister, so every
decision it makes is a unit test. Only its wiring needs `envtest`, which is
`I-24` onward.

| #     | Scenario                                                                                              | Type     | Test |
|-------|-------------------------------------------------------------------------------------------------------|----------|------|
| U-86  | Creating a draft naming a worker that does not exist: admitted                                        | Positive | —    |
| U-87  | Creating a draft naming a device no node advertises: admitted                                         | Positive | —    |
| U-88  | Editing an unapproved draft into a worse state: admitted                                              | Positive | —    |
| U-89  | Approving a document whose workers all exist: admitted                                                | Positive | —    |
| U-90  | Approving a document naming a worker that does not exist: denied, naming the worker                   | Negative | —    |
| U-91  | Approving a document naming an unavailable device: admitted, since devices are not an admission check | Boundary | —    |
| U-92  | Approving with `clusterRef` set to a cluster that does not exist: denied                              | Negative | —    |
| U-93  | Approving with no `clusterRef` while the named cluster exists: denied                                 | Negative | —    |
| U-94  | Approving while another approved config already owns the cluster: denied                              | Negative | —    |
| U-95  | Editing an approved document: denied, naming the field and pointing at `clusterRef`                   | Negative | —    |
| U-96  | Withdrawing approval: denied                                                                          | Negative | —    |
| U-97  | Editing only `metadata` on an approved document: admitted                                             | Boundary | —    |
| U-98  | A draft created by the operator's own service account: admitted on the same path                      | Boundary | —    |
| U-99  | An approving edit by the operator's own service account: validated, not exempted                      | Negative | —    |
| U-100 | A request whose object does not decode: `Errored`, not `Allowed`                                      | Negative | —    |

`U-98` and `U-99` are the pair that keeps design §5.2's rule true: discovery
writes drafts and needs no exemption, and an exemption for approvals would be a
hole in the gate rather than a convenience.

### Environment, Mixing, and Re-Discovery (design §3.1, §6, §8.1)

Files: `operator/internal/controllers/deployment/clusterdeploymentconfig_expand_test.go`
and `operator/internal/controllers/deployment/operatorops_discover_test.go`

| #     | Scenario                                                                               | Type     | Test |
|-------|----------------------------------------------------------------------------------------|----------|------|
| U-116 | `environment: OpenShift`: every node gets the four flags that distribution implies     | Positive | —    |
| U-117 | `environment: Vanilla`: every node gets that distribution's flags, not OpenShift's     | Positive | —    |
| U-118 | `environment` absent: no distribution flag is stamped, and none is invented            | Boundary | —    |
| U-119 | A node edited after expansion to flip one flag: nothing re-stamps it                   | Negative | —    |
| U-120 | A group naming both `nvme` and `block`: expanded, with `MixedDeviceClasses` emitted    | Boundary | —    |
| U-121 | A group naming both: `config.deviceNames` carries the addresses and the paths together | Positive | —    |
| U-122 | `MixedDeviceClasses` does not hold approval or expansion                               | Negative | —    |
| U-123 | Discovery against a cluster this operator deployed: claimed workers are not candidates | Negative | —    |
| U-124 | The same run: an unclaimed worker beside claimed ones is a candidate                   | Positive | —    |
| U-125 | The same run: a device an existing node owns is not a candidate                        | Negative | —    |
| U-126 | The same run writes a growth document, with `clusterRef` set and only new node sets    | Positive | —    |
| U-127 | Every worker and device already claimed: an empty draft, not a duplicate of the first  | Boundary | —    |
| U-128 | `spec.environment` from node labels alone                                              | Positive | —    |
| U-129 | `spec.environment` from a service only that distribution registers                     | Positive | —    |
| U-130 | Conflicting evidence: one distribution is concluded and the reason is in the message   | Boundary | —    |
| U-131 | Two runs against one Kubernetes cluster: the same `spec.environment` both times        | Positive | —    |

`U-126` and `U-127` are the pair design §8.1 turns on. Re-running discovery after
an expansion is how a fleet grows, so the run has to produce a document that adds
and one that adds nothing when there is nothing to add.

`U-131` is what makes design §12's second question a non-question. Two documents
written against one Kubernetes cluster read the same evidence, so they agree on
the distribution without anybody coordinating.

### Device Validation Before Expansion (design §4.1, §5.1)

File: `operator/internal/controllers/deployment/clusterdeploymentconfig_validate_test.go`

Admission does not look at devices, so these are the only two moments that do.

| #     | Scenario                                                                                | Type     | Test |
|-------|-----------------------------------------------------------------------------------------|----------|------|
| U-132 | A hand-written draft naming an unavailable device: `DeviceNotFound` while still `Draft` | Negative | —    |
| U-133 | `Validating` rejects a device that stopped being available after approval: `Failed`     | Negative | —    |
| U-134 | `Validating` accepts a device the draft validation had already cleared                  | Positive | —    |

`U-132` and `U-133` are the two halves of what replaced the admission-time check.
Design §4.1 tells a reviewer while the document is still editable, and design §5.1
leaves the last word to `Validating`, so a device mistake is caught twice and
never by the webhook.

### Device Availability (design §8.2)

File: `operator/internal/controllers/deployment/operatorops_discover_test.go`

| #     | Scenario                                                                             | Type     | Test |
|-------|--------------------------------------------------------------------------------------|----------|------|
| U-109 | An unmounted, unpartitioned, idle device: reported                                   | Positive | —    |
| U-110 | A mounted device: excluded                                                           | Negative | —    |
| U-111 | A device busy without being mounted, held open or in a device-mapper stack: excluded | Negative | —    |
| U-112 | A device carrying a partition table, `enablePartitionedDevices` unset: excluded      | Negative | —    |
| U-113 | The same device with `enablePartitionedDevices` set: reported                        | Positive | —    |
| U-114 | A mounted device with `enablePartitionedDevices` set: still excluded                 | Boundary | —    |
| U-115 | A group naming a partitioned device: the expansion passes it through unchanged       | Positive | —    |

`U-114` is the row that states which conditions are waivable. Partitions are, and
mounted and busy are not, so a flag that waived them would be a flag for handing
simplyblock a disk another subsystem is writing to.

### Backend Device Classes (design §3.1, §8.1)

File: `operator/internal/controllers/deployment/operatorops_discover_test.go`

| #     | Scenario                                                                        | Type     | Test |
|-------|---------------------------------------------------------------------------------|----------|------|
| U-101 | `enableLogicalBlockDevices` unset: only NVMe devices reach the draft            | Boundary | —    |
| U-102 | `enableLogicalBlockDevices` true: both classes reach the draft                  | Positive | —    |
| U-103 | A worker with block devices and no NVMe device, flag unset: no devices reported | Negative | —    |
| U-104 | The same worker with the flag set: its block devices reach `devices.block`      | Positive | —    |
| U-105 | NVMe candidates land in `devices.nvme` and block candidates in `devices.block`  | Positive | —    |
| U-106 | A PCI deny list with the flag set: block devices are unaffected by it           | Boundary | —    |
| U-107 | A `driveSizeRange` with the flag set: it narrows both classes                   | Positive | —    |
| U-108 | A mounted block device: excluded whatever the class flag says                   | Negative | —    |

`U-108` is the row that keeps the two flags from being read as one thing.
Enabling a class widens which devices are considered, and it never waives the
availability rule that decides which of them are reported.

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`. The
immutability rules are CEL and cannot be exercised any other way.

| #    | Scenario                                                                                              | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | An approved config edited: rejected by the immutability rule                                          | Negative | —    |
| I-02 | An unapproved config edited: accepted                                                                 | Positive | —    |
| I-03 | `spec.approved` set true, then false: the withdrawal is rejected                                      | Negative | —    |
| I-04 | `spec.approved` false, then true: accepted                                                            | Positive | —    |
| I-05 | An approved config edited only in `metadata`: accepted, since the rule is on spec                     | Boundary | —    |
| I-06 | `spec.nodeSets` omitted: rejected as `Required`                                                       | Negative | —    |
| I-07 | `spec.nodeSets` empty: rejected by `MinItems`                                                         | Boundary | —    |
| I-08 | `spec.environment` outside the enum: rejected                                                         | Negative | —    |
| I-09 | A group with 201 workers: rejected by `MaxItems`                                                      | Boundary | —    |
| I-10 | A group with duplicate workers: rejected by `listType=set`                                            | Negative | —    |
| I-11 | `spec.nodeSets[].sizing` omitted: rejected as `Required`                                              | Negative | —    |
| I-12 | `OperatorOps.spec.action` outside the enum: rejected                                                  | Negative | —    |
| I-13 | `OperatorOps.spec.action` changed after creation: rejected as immutable                               | Negative | —    |
| I-14 | Short names `cdc` and `oops` resolve to the same lists as the full kinds                              | Positive | —    |
| I-15 | A full expansion against a real API server: cluster and nodes exist afterward                         | Positive | —    |
| I-16 | Deleting the config afterward: the cluster and nodes survive                                          | Positive | —    |
| I-17 | Two configs in two namespaces with the same name: neither reads the other                             | Negative | —    |
| I-18 | Two configs in one namespace naming one cluster: the second is refused                                | Negative | —    |
| I-19 | The controller's role covers every object the expansion creates                                       | Positive | —    |
| I-20 | A `devices` block with neither `nvme` nor `block`: rejected by the CEL rule                           | Negative | —    |
| I-21 | A `devices` block with only `nvme`: accepted                                                          | Boundary | —    |
| I-22 | A `devices` block with duplicate `nvme` entries: rejected by `listType=set`                           | Negative | —    |
| I-23 | A `devices` block carrying a filter field such as `pcieDenyList`: rejected                            | Negative | —    |
| I-34 | `devices.nvme` holding a well-formed PCI address: accepted                                            | Positive | —    |
| I-35 | `devices.nvme` holding a device path: rejected by the item pattern                                    | Negative | —    |
| I-36 | `devices.nvme` holding a truncated PCI address: rejected by the item pattern                          | Negative | —    |
| I-37 | `devices.block` holding a path under `/dev`: accepted                                                 | Positive | —    |
| I-38 | `devices.block` holding a bare device name: rejected by the item pattern                              | Negative | —    |
| I-39 | `devices.block` holding a path outside `/dev`: rejected by the item pattern                           | Negative | —    |
| I-24 | The webhook is registered for `create` and `update` on the kind                                       | Positive | —    |
| I-25 | An approving apply naming a missing worker: rejected by the API server                                | Negative | —    |
| I-26 | The same document with the worker created first: accepted                                             | Positive | —    |
| I-27 | An invalid draft applied unapproved: accepted, and `status.message` reports it                        | Positive | —    |
| I-28 | An edit to an approved document: rejected, and the stored object is unchanged                         | Negative | —    |
| I-29 | `enableLogicalBlockDevices` outside a boolean: rejected by the schema                                 | Negative | —    |
| I-30 | A group whose `devices.block` names a path: accepted                                                  | Positive | —    |
| I-31 | `enablePartitionedDevices` outside a boolean: rejected by the schema                                  | Negative | —    |
| I-32 | Approving a config naming a mounted device: accepted by the API server, then `Failed` at `Validating` | Negative | —    |
| I-33 | Approving a config naming a partitioned device: accepted and expanded                                 | Positive | —    |

---

## 3. End-to-End Tests

A live deployment with real workers carrying real NVMe devices, which is the only
place discovery's central question can be answered.

| #        | Scenario                                                                                              | Type     | Test |
|----------|-------------------------------------------------------------------------------------------------------|----------|------|
| E-01     | Discovery on a fresh cluster: a draft naming every worker and its devices                             | Positive | —    |
| E-02     | A worker booting from NVMe: the boot device is excluded as mounted                                    | Negative | —    |
| E-03     | A reviewer corrects the device list, approves, and the fleet comes up                                 | Positive | —    |
| ~~E-04~~ | Discovery against an existing deployment. Withdrawn with the adoption path (design §8.1)              | —        | —    |
| ~~E-05~~ | Approving that draft adopts the running cluster. Withdrawn: adoption is design-storagecluster.md §4.3 | —        | —    |
| E-06     | A second config adds a rack to the running cluster                                                    | Positive | —    |
| E-07     | Deleting every config: the cluster keeps serving I/O                                                  | Positive | —    |
| E-08     | An expansion of twenty workers: `maxParallelNodeAdds` still bounds the adds                           | Boundary | —    |
| E-11     | Sustained I/O during an expansion that adds nodes: no interruption                                    | Positive | —    |
| E-12     | Discovery with the flag set on a worker with SATA disks: they reach the draft                         | Positive | —    |
| E-13     | A cluster built from logical block devices serves I/O                                                 | Positive | —    |
| E-14     | A partitioned device forced into a group: the node comes up using it                                  | Positive | —    |
| E-15     | A device mounted on the worker: no draft and no expansion ever claims it                              | Negative | —    |

`E-09` and `E-10` covered the same removed action, and their IDs are retired too.

---

## 4. Manual Scenarios

`M-01` was the operator upgrading itself, which design §7.1 removed. Its ID is
retired.

### M-02: Availability, its override, and what a reviewer still has to correct

**Design reference:** §8.2.

**What to verify:** both halves of the availability rule on a real machine. The
inspection is what keeps a device in use out of every draft, and the approval gate
is what catches the devices it lets through that this cluster should not own.

**Test concept:**

1. Prepare a worker that boots from one device, has a second device mounted at a
   path, a third carrying a stale partition table, and a fourth idle and clean.
2. Run discovery with no `deviceFilter`, and confirm the draft lists the fourth
   device and none of the first three.
3. Re-run with `enablePartitionedDevices` set, and confirm the third device now
   appears while the first two still do not.
4. Approve that draft and confirm the node comes up using both devices, which is
   the forced-use path of design §8.2.
5. Repeat step 2 on a worker whose idle device is a scratch disk this cluster
   should not own, and confirm the only thing that stops it being consumed is a
   reviewer deleting the entry before approving.
6. Confirm the worker still boots throughout.

### M-03: A config that names an already-managed cluster

**Design reference:** §6.

**What to verify:** that the refusal is legible rather than a silent no-op, since
this is the mistake an administrator makes when growing a fleet by copying the
first config.

**Test concept:**

1. Expand a config that creates cluster `production`.
2. Copy it, change nothing, and approve the copy.
3. Confirm the second config reaches `Failed` with `ClusterExists`, and that the
   running cluster and its nodes are untouched.
4. Set `clusterRef: production` on a third copy carrying only a new node set, and
   confirm it adds those nodes and nothing else.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered | Withdrawn |
|-------------|-----------|---------|-------------|-----------|
| Unit        | 125       | 0       | 125         | 1         |
| Integration | 39        | 0       | 39          | 0         |
| E2E         | 11        | 0       | 11          | 2         |
| Manual      | 2         | 0       | 2           | 0         |
| **Total**   | **177**   | **0**   | **177**     | **3**     |

A withdrawn row is one whose behavior the design removed. Its identifier stays in
the matrix, struck through, because identifiers are never reused. It counts as
neither a scenario nor a gap. All three are the adoption path design §8.1 no longer
has: discovery reads the cluster it runs in and makes no backend call.

Nothing is covered, and nothing can be: neither kind exists. Every row is a
specification, and the plan's value before implementation is that it says what
the kinds have to do rather than what somebody remembers deciding.

The distribution is worth reading, though. Seventy per cent of the scenarios
are unit tests, which is higher than any other plan in this repository, and that
is a property of the design rather than an accident: expansion is a pure function
of a document plus the cluster's current state, so almost all of it is provable
against a fake client.

---

## 6. What Is Not Yet Covered

| #             | Gap                                                          | Reason                                                                                                                                      |
|---------------|--------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-11   | Draft validation                                             | The kind does not exist                                                                                                                     |
| U-12 … U-26   | Expansion                                                    | The kind does not exist. These are the rows to write first, because they are pure and they are what the document is for                     |
| U-27 … U-34   | Create-only semantics                                        | The kind does not exist. `U-32` to `U-34` are the rows that keep a typo from becoming a data-loss event (design §6)                         |
| U-35 … U-45   | Approval and deletion                                        | The kind does not exist                                                                                                                     |
| U-46 … U-62   | Discovery                                                    | `OperatorOps` does not exist, and there is no discovery code of any kind in the repository                                                  |
| U-63 … U-75   | `OperatorOps` lifecycle                                      | The kind does not exist                                                                                                                     |
| U-76 … U-85   | Device selection and the discovery filter                    | Neither kind exists. `U-85` is the row that holds design §8.1's rule that a filter is an input and never a stored field                     |
| U-86 … U-100  | The approval webhook                                         | Neither the kind nor the webhook exists. `U-98` and `U-99` are the rows that keep discovery unexempted, which design §5.2 requires          |
| U-101 … U-108 | Backend device classes                                       | Neither kind exists, and design §12 Q4 leaves open which `StorageNode` field a `devices.block` entry expands into                           |
| U-109 … U-115 | Device availability                                          | Neither kind exists. Mount and busy state is the node's own answer, so these rows fake the inventory and `E-15` is what proves the real one |
| U-116 … U-131 | Environment, mixing, and re-discovery                        | Neither kind exists. These are the rows the resolved questions of design §12 turned from undecided into testable                            |
| U-132 … U-134 | Device validation before expansion                           | Neither kind exists. These replace the admission-time device check that design §5.1 hands to discovery                                      |
| I-01 … I-39   | Every admission rule and the real-API-server expansion       | Needs `envtest`, because CEL and `Required` are enforced by the API server and a fake client applies neither                                |
| E-01 … E-15   | All end-to-end scenarios                                     | Needs a live deployment with real devices. The e2e harness under `test/` is not committed yet                                               |
| E-02          | Distinguishing the boot device                               | Design §8.2 says discovery cannot do this, so the row asserts that it reports rather than chooses. It needs real hardware                   |
| M-02, M-03    | Device availability and its override, and a duplicate config | Need a worker with a mounted, a partitioned, and an idle device, and a running cluster                                                      |
| Metrics       | The eight metrics of design §9.2                             | Designed, not built                                                                                                                         |
| Q1            | The `KubernetesEnvironment` to flag mapping                  | Design §12 states it only for `OpenShift`, so `U-117` asserts that `Vanilla` differs from it without asserting what it is                   |

### Axis coverage

| Axis               | Value                                  | Scenarios                  |
|--------------------|----------------------------------------|----------------------------|
| Cluster topology   | One worker                             | U-49                       |
|                    | Two or three workers                   | U-12, U-46, U-47           |
|                    | Twenty or more                         | E-08                       |
|                    | None schedulable                       | U-48                       |
| Sockets per worker | One                                    | U-12                       |
|                    | Two                                    | U-13                       |
|                    | Two with two nodes per socket          | U-14                       |
| Deployment shape   | Create a new cluster                   | U-27, E-01, E-03           |
|                    | Add to an existing cluster             | U-29, U-31, E-06, M-03     |
|                    | Refused conflict                       | U-28, U-30, I-18, M-03     |
| Namespace count    | Single                                 | Every scenario except I-17 |
|                    | Multiple                               | I-17                       |
| Approval state     | Draft, valid                           | U-01, U-35                 |
|                    | Draft, invalid                         | U-02, U-03, U-06, U-07     |
|                    | Approved                               | U-36, I-01, I-03           |
|                    | Being approved, valid                  | U-89, I-26                 |
|                    | Being approved, invalid                | U-90, U-92, U-94, I-25     |
| Dependency health  | Control plane ready                    | U-27                       |
|                    | Not ready                              | U-39, U-41                 |
|                    | Becomes ready                          | U-40                       |
| Lifecycle          | Re-expansion after a crash             | U-20, U-21                 |
|                    | Deletion after expansion               | U-42, I-16, E-07           |
| Object scale       | Zero workers                           | U-48                       |
|                    | One group, one worker                  | U-49                       |
|                    | Twenty workers                         | E-08                       |
| Device selection   | PCI addresses (`nvme`)                 | U-76, U-80, I-21, I-34     |
|                    | Block devices (`block`)                | U-77, I-37                 |
|                    | A shape in the wrong member            | I-35, I-38                 |
|                    | A malformed shape                      | I-36, I-39                 |
|                    | Both in one group                      | U-78                       |
|                    | Neither declared                       | U-79, I-20                 |
| Availability       | Idle, unpartitioned                    | U-109, U-115               |
|                    | Mounted                                | U-110, U-114, E-02, E-15   |
|                    | Busy without a mount                   | U-111                      |
|                    | Partitioned, not waived                | U-112                      |
|                    | Partitioned, waived                    | U-113, E-14                |
| Backend class      | NVMe only                              | U-101, U-103               |
|                    | Logical block only                     | U-104, E-12, E-13          |
|                    | Both on one worker                     | U-102, U-105               |
| Discovery filter   | Absent                                 | U-84                       |
|                    | Allow, deny, model, size               | U-80, U-81, U-82           |
|                    | Matches nothing                        | U-83                       |
| Distribution       | OpenShift                              | U-54, U-116                |
|                    | Vanilla or unrecognized                | U-55, U-117                |
|                    | Unset                                  | U-118                      |
| Distribution proof | Node labels                            | U-128                      |
|                    | A cluster-scoped service               | U-129                      |
|                    | Conflicting evidence                   | U-130                      |
| Discovery run      | First, against nothing                 | U-56, E-01                 |
|                    | Against workers a cluster already uses | U-123, U-127               |
|                    | Against a cluster it deployed          | U-123, U-126               |
|                    | Nothing left unclaimed                 | U-127                      |

**The deployment-shape axis is the one this document exists for**, and all four
of its values have rows. The refused-conflict value matters most: design §6
chooses refusal over merging, and `U-28`, `U-30`, and `M-03` are what would catch
a later implementation quietly deciding to merge instead.

**The device-inspection axis is still hardware-only.** Whether a device is
mounted, busy, or partitioned is the node's own answer, so `E-02`, `E-12`, `E-14`,
`E-15`, and `M-02` are the only rows that exercise it. `U-101` to `U-115` run
against a faked inventory and prove a weaker claim: that discovery applies the
rule it was given to the answers it was handed, not that the answers were right.
That gap is the reason design §8.2 puts the reviewer after the inspection rather
than trusting it.
