# Test Plan: Storage Node Network Attachment

Related design: [`designs/design-storage-network.md`](../designs/design-storage-network.md)

Scope is the operator, its admission rules, and the Kubernetes surface this repository builds. The control plane (`sbcli`) and SPDK are dependencies, faked at the boundary: a row asserts the operator's response to a control-plane answer, never the control plane's own behavior. The sbcli changes of P0-1 and P0-2 are tested in the sbcli repository.

Scenario IDs are permanent: `U-` unit (pure functions, fake `client.Client`), `I-` integration (reconcile loop and admission against `envtest` and a mock control plane), `E-` end-to-end (live cluster), `M-` manual (needs failure injection or hardware). Types are `Positive`, `Negative`, `Boundary`, and `Regression`.

**Axes selected:** cluster topology (§5), namespace scope, cluster count, and three specific to this feature, the attachment mechanism (`host-device`, `macvlan`, `ipvlan`, `hostInterface`), the address source (pool, adopted NIC address), and the initiator location (on the data subnet, routed from another subnet, on the pod's own worker). **Excluded:** asymmetric node sizes, because the design reads no per-node capacity.

---

## 1. Unit Tests

Pure functions and a fake `client.Client`, with no API server.

### Attachment Builder (§5.1)

File: `network_attachments_test.go`

| #    | Scenario                                                                                                                       | Type     |
|------|--------------------------------------------------------------------------------------------------------------------------------|----------|
| U-01 | One management and two data networks produce elements named `mgmt0`, `data0`, and `data1`, in the order the cluster lists them | Positive |
| U-02 | Each element carries `ips` with the node's assigned address, prefix length included, and none sets `default-route`             | Positive |
| U-03 | A cluster with no management network produces `data0` onward and no `mgmt0`                                                    | Boundary |
| U-04 | A `hostInterface` attachment produces no element and no annotation                                                             | Negative |
| U-05 | A node with no assigned address for a network that is attached through a definition returns an error naming the network        | Negative |
| U-06 | The maximum of eight data networks produces `data0` to `data7`, and a ninth is not built                                       | Boundary |
| U-07 | The same input produces byte-identical output on every call, so a retried node-add is idempotent                               | Positive |

### `network-status` Verifier (§5.4)

File: `network_verify_test.go`

| #    | Scenario                                                                                                            | Type       |
|------|---------------------------------------------------------------------------------------------------------------------|------------|
| U-13 | Every declared interface present with the declared address gives `Attached` for each network                        | Positive   |
| U-14 | An interface whose address differs from the declaration gives `Mismatched` and names both addresses                 | Negative   |
| U-15 | A declared interface absent from the annotation gives `Pending` before the pod is `Running` and `Unavailable` after | Negative   |
| U-16 | An annotation that is missing, empty, or not valid JSON is reported as such and never panics                        | Negative   |
| U-17 | The annotation's `sbnet-test/data-nic` style name is matched to the definition by namespace and name, not by prefix | Positive   |
| U-18 | An address with and without a prefix length compares equal when the host part is equal                              | Boundary   |
| U-19 | The `eth0` default-network entry is ignored, and an extra undeclared interface does not fail the match              | Positive   |
| U-20 | The pod is matched by its RPC-port label, and two SPDK pods of one worker are never confused                        | Regression |

### Duplicate-Address Result (§8.2)

File: `netprobe_test.go`

| #        | Scenario                                                                                                                                                                     | Type     |
|----------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|
| ~~U-27~~ | ~~The check's `arping -D` result with one reply classifies as `NetworkAddressInUse`, and no reply as free~~ Withdrawn: duplicate-address detection is removed with the pool. | Positive |
| ~~U-42~~ | ~~Both probes carry a termination grace period of at most two seconds~~ Superseded by U-52.                                                                                  | Positive |
| ~~U-43~~ | ~~The holder container execs `sleep`, so the container's own process receives the stop signal~~ Withdrawn: no check pod holds a network namespace.                           | Positive |

The operator reads no pod events, so U-21 to U-26 (classifying a sandbox event) are withdrawn. I-62 covers an SPDK pod that does not start.

### Address Pool (§4.3)

File: `network_pool_test.go`

| #        | Scenario                                                                                                                                                                                                                                | Type     |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|
| U-28     | The first address of the pool is taken when nothing is held, written with the subnet's prefix length                                                                                                                                    | Positive |
| U-29     | An address another node holds is skipped, whatever its prefix length                                                                                                                                                                    | Negative |
| ~~U-30~~ | ~~An address the node was turned away from is not taken again~~ Withdrawn: a node is never turned away from an address.                                                                                                                 | Negative |
| U-31     | A pool with nothing left is exhausted, and a pool of one address is taken once                                                                                                                                                          | Boundary |
| U-32     | The subnet's network and broadcast addresses are never handed out, and a pool that is not a range inside its subnet is invalid                                                                                                          | Boundary |
| U-33     | A pool of a subnet alone is the whole subnet                                                                                                                                                                                            | Positive |
| U-34     | `first` and `last` still narrow the range, and only one of them is an invalid pool                                                                                                                                                      | Boundary |
| ~~U-35~~ | ~~Excluded addresses and ranges are never handed out, and a pool whose exclude list covers the subnet is exhausted~~ Withdrawn: the pool has no exclude list.                                                                           | Negative |
| ~~U-36~~ | ~~An exclude entry outside the subnet has no effect~~ Withdrawn: the pool has no exclude list.                                                                                                                                          | Boundary |
| ~~U-37~~ | ~~An exclude entry that is not an address or a range is an invalid pool~~ Withdrawn: the pool has no exclude list.                                                                                                                      | Negative |
| ~~U-38~~ | ~~The batch is in order, bounded, and skips held, turned-away, and excluded addresses~~ Withdrawn: the allocator returns one address, not a batch.                                                                                      | Positive |
| ~~U-39~~ | ~~The batch is shorter than the limit when the pool runs out, and an empty batch is exhaustion~~ Withdrawn: the allocator returns one address, not a batch.                                                                             | Boundary |
| U-40     | A `/30` hands out two addresses, and a `/31` and a `/32` hand out theirs with no network or broadcast address to keep back                                                                                                              | Boundary |
| ~~U-41~~ | ~~The guest probe's duplicate results name the addresses that answered, by interface, and the probe asks about its candidates~~ Withdrawn with the guest probe. A scenario for the check Appendix A proposes is added when Q10 settles. | Positive |

### Address Adoption (§4.3)

File: `network_adopt_test.go`

| #        | Scenario                                                                                                                                                                                             | Type       |
|----------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| U-44     | The first IPv4 address of the interface the plugin reports, with its prefix length, is the node's address                                                                                            | Positive   |
| U-45     | An interface with no address, with only IPv6 addresses, or whose first IPv4 address is link-local has no usable address                                                                              | Negative   |
| U-46     | The interface is found by the name the definition gives or by the PCI address it names, and an interface the worker lacks is reported absent                                                         | Positive   |
| ~~U-47~~ | ~~A definition whose plugin is not `host-device`, or whose IPAM is not `static`, is unsupported, and the result names the setting~~ Withdrawn: the operator does not inspect the plugin or the IPAM. | Negative   |
| ~~U-48~~ | ~~A `host-device` network that declares a pool draws from it and reads no interface~~ Superseded by I-43.                                                                                            | Boundary   |
| U-49     | An address already recorded in `status.network` is kept, and the interface report is not read again                                                                                                  | Regression |
| U-50     | An address the report marks `dynamic` is adopted as it stands                                                                                                                                        | Positive   |

### Reachability Result (§8.1)

File: `netcheck_test.go`

| #        | Scenario                                                                                                                             | Type     |
|----------|--------------------------------------------------------------------------------------------------------------------------------------|----------|
| U-51     | An answer of connected is reached, and an answer of refused, timed out, or no answer is not reached                                  | Positive |
| ~~U-52~~ | ~~Check pods carry a termination grace period of at most two seconds and a deadline of ten minutes~~ Withdrawn: no check pod exists. | Positive |
| U-53     | The workers to test are the workers with a linked CSI node plugin, and a worker without one is skipped                               | Boundary |
| U-54     | A plugin that does not advertise the host check is unavailable and is never counted as passed                                        | Negative |

---

## 2. Integration Tests

The full reconcile loop and admission against `envtest` and a mock control plane. The mock records every call, so a row can assert both that a call happened and that it did not.

### Admission and Immutability (§4.1, §4.4, §4.5)

File: `network_admission_test.go`

| #        | Scenario                                                                                                                                | Type       |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------|------------|
| I-01     | A `StorageCluster` with `network` and no legacy fields is accepted                                                                      | Positive   |
| I-02     | `network` together with `mgmtInterface` or `dataInterfaces` is rejected with the exclusivity message                                    | Negative   |
| I-03     | An attachment with both `networkAttachmentRef` and `hostInterface`, or with neither, is rejected                                        | Negative   |
| I-04     | A management network attached through a definition and a data network as `hostInterface` is rejected as a mixed cluster                 | Negative   |
| I-05     | `network` is accepted at creation, and cannot be added to an existing cluster, changed, or removed                                      | Boundary   |
| I-06     | A `fabricType` that includes RDMA with a `networkAttachmentRef` data network is rejected, and `hostInterface` is accepted               | Negative   |
| I-07     | More than eight data networks is rejected, and eight is accepted                                                                        | Boundary   |
| I-08     | Two data networks with the same name are rejected, and a name that is not a DNS label is rejected                                       | Negative   |
| ~~I-09~~ | ~~A network attached through a definition requires an address pool, and a host interface network refuses one~~ Superseded by I-56.      | Negative   |
| I-10     | A node of a cluster on definition networks is admitted without an address                                                               | Positive   |
| I-11     | A node's assigned address is kept across reconcile passes                                                                               | Positive   |
| I-12     | A pool whose subnet, first, or last is not an IPv4 address, or whose first or last lies outside the subnet, is rejected                 | Negative   |
| I-47     | A pool of a subnet alone is accepted, and `first` without `last`, or `last` without `first`, is rejected                                | Negative   |
| ~~I-48~~ | ~~The exclude list takes addresses and ranges, at most thirty-two, and rejects anything else~~ Withdrawn: the pool has no exclude list. | Negative   |
| I-13     | A cluster without `network` behaves exactly as before: every existing admission test passes unchanged                                   | Regression |

### Checks Before Node-Add (§5.4, §8.1)

File: `network_preflight_test.go`

| #        | Scenario                                                                                                                                                                                                        | Type       |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| I-14     | A valid declaration chooses the node's address, the checks pass, and the node-add call is made with the attachment list                                                                                         | Positive   |
| I-15     | A cluster with no `NetworkAttachmentDefinition` kind holds the node in `Pending`, emits `NetworkAttachmentUnsupported`, and makes no node-add call                                                              | Negative   |
| I-16     | A reference to a definition that does not exist holds the node and emits `NetworkAttachmentNotFound`, with zero node-add calls                                                                                  | Negative   |
| ~~I-17~~ | ~~A probe pod that has not started after the timeout holds the node and emits `NetworkAttachmentFailed` naming the pod~~ Superseded by I-62.                                                                    | Negative   |
| ~~I-18~~ | ~~A probe that holds a different address holds the node and emits `NetworkAddressMismatch`~~ Superseded by I-33.                                                                                                | Negative   |
| ~~I-19~~ | ~~A check whose `arping -D` is answered turns the address away, emits `NetworkAddressInUse`, and the node takes the next address of the pool without being held~~ Withdrawn: no duplicate-address check exists. | Negative   |
| ~~I-20~~ | ~~A probe whose peer check fails holds the node and emits `DataNetworkUnreachable`~~ Withdrawn: the peer check ran in the guest probe.                                                                          | Negative   |
| ~~I-21~~ | ~~A failed host-network probe lets the node proceed, sets `hostReachable` false, and emits `DataNetworkHostUnreachable`~~ Superseded by I-65.                                                                   | Negative   |
| ~~I-22~~ | ~~The probe pod is deleted before the node-add call, so it and the SPDK pod never hold an address together~~ Superseded by I-63.                                                                                | Positive   |
| I-23     | A held node is retried on the next reconcile once the definition is created, and the hold clears                                                                                                                | Positive   |
| ~~I-24~~ | ~~An operator restart while the probe runs resumes it and never creates a second probe pod~~ Superseded by I-63.                                                                                                | Regression |
| ~~I-49~~ | ~~Every candidate that answers is turned away in one pass, and the next pass takes the first address that did not~~ Withdrawn: no address is turned away.                                                       | Negative   |
| ~~I-50~~ | ~~A candidate that answers while the assigned address is free is remembered, and the node is not held~~ Withdrawn: no address is turned away.                                                                   | Positive   |
| ~~I-51~~ | ~~A node that turned away the most it keeps is held for `NetworkAddressPoolExhausted` and does not choose again~~ Withdrawn: no address is turned away.                                                         | Boundary   |
| ~~I-52~~ | ~~A subnet with an exclude list is all the pool needs, and the node takes the first address past the excluded range~~ Withdrawn: the pool has no exclude list.                                                  | Positive   |
| ~~I-53~~ | ~~A guest probe that is being deleted is not judged against the node's new address: the node waits without being held and creates a fresh probe once the pod is gone~~ Superseded by I-67.                      | Regression |
| ~~I-54~~ | ~~A host probe that is being deleted is waited for and records no reachability~~ Superseded by I-67.                                                                                                            | Negative   |
| ~~I-55~~ | ~~A candidate that answers because another node has it assigned is not recorded in `rejected`, in the turn-away path and in the free-address path~~ Withdrawn: no address is turned away.                       | Negative   |

### Node-Add and Restart (§6, §10)

File: `network_nodeadd_test.go`

| #    | Scenario                                                                                                                                             | Type       |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| I-26 | The node-add body carries the attachment list, and for a `hostInterface` cluster it carries the legacy `data_nics` and `interface_name`              | Positive   |
| I-27 | A node-add that returns 5xx is retried with an identical body, and the mock sees the same attachments each time                                      | Negative   |
| I-28 | A node-add that returns 4xx for the attachments surfaces a reported failure and does not advance state                                               | Negative   |
| I-29 | A control plane that does not accept attachments, which is the endpoint not existing yet, holds the node with an event naming the missing capability | Negative   |
| I-30 | A restart passes the recorded attachments again and does not recompute them from a changed cluster spec                                              | Positive   |
| I-31 | A node-add that times out and is retried creates one node, asserted by the mock's recorded count                                                     | Regression |

### Verification After Start (§5.4)

File: `network_verify_reconcile_test.go`

| #    | Scenario                                                                                                                               | Type     |
|------|----------------------------------------------------------------------------------------------------------------------------------------|----------|
| I-32 | An SPDK pod that reports the declared addresses sets `status.network` to `Attached` and emits `NetworkAttached` once                   | Positive |
| I-33 | An SPDK pod that reports a different address sets `Mismatched`, emits `NetworkAddressMismatch`, and calls the control plane's shutdown | Negative |
| I-34 | A node taken out of service for a mismatch is not restarted until the declaration or the definition changes                            | Negative |
| I-35 | A pod that loses its interface after attach is reported `Unavailable` with `NetworkAttachmentLost` from the proxy's link report        | Negative |
| I-36 | A re-reconcile of an `Attached` node makes no control-plane call and emits no event                                                    | Boundary |
| I-37 | A `hostInterface` node has an empty `status.network` and no verification call                                                          | Negative |
| I-38 | Metrics `attached` and `host_reachable` follow the phases, and the failures counter increments by reason                               | Positive |

### Pods Off the Host Network (§5.2)

File: `network_workloads_test.go`

| #    | Scenario                                                                                        | Type       |
|------|-------------------------------------------------------------------------------------------------|------------|
| I-39 | A cluster with `network` renders the node agent DaemonSet without `hostNetwork`                 | Positive   |
| I-40 | A cluster without `network` renders every pod with `hostNetwork: true`, byte-identical to today | Regression |
| I-41 | The CSI node plugin and the mds-proxy keep `hostNetwork: true` in both modes                    | Negative   |
| I-42 | A node-preparation job keeps `hostNetwork: true` in both modes                                  | Negative   |

### Pool Optional (§4.1)

File: `network_admission_test.go`

| #    | Scenario                                                                                                                                    | Type     |
|------|---------------------------------------------------------------------------------------------------------------------------------------------|----------|
| I-56 | A network attached through a definition is accepted with an address pool and without one, and a host interface network still refuses a pool | Boundary |

### Adopted Address (§4.3)

File: `network_adopt_reconcile_test.go`

| #        | Scenario                                                                                                                                                                          | Type       |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| I-57     | A network with no pool records the NIC's address, as the CSI node plugin reports it, in `status.network` before the node-add call, and the call carries it                        | Positive   |
| I-58     | An interface without a usable address holds the node, emits `NetworkInterfaceNoAddress` naming the interface, and makes no node-add call                                          | Negative   |
| ~~I-59~~ | ~~A network with no pool whose plugin is `macvlan` or `ipvlan` holds the node and emits `NetworkAddressPoolMissing`~~ Withdrawn: a network with no pool adopts the NIC's address. | Negative   |
| I-61     | With the SPDK pod of an earlier attempt still holding the NIC, the step keeps the recorded address and creates no pod that attaches the network                                   | Regression |

### Reachability Gate (§5.4, §8.1)

File: `network_reachability_test.go`

| #        | Scenario                                                                                                                                                              | Type       |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| I-62     | An SPDK pod that has not started after a minute leaves the node `Pending` and emits `NetworkAttachmentFailed` naming the pod                                          | Negative   |
| I-63     | No pod that attaches the data network exists before the node-add call, and none is created while the SPDK pod holds it                                                | Regression |
| I-64     | Every linked CSI node plugin that reaches the listener records `Attached` and emits `NetworkAttached` once                                                            | Positive   |
| I-65     | A worker that cannot reach the listener leaves the network `Unavailable`, emits `DataNetworkHostUnreachable` naming the worker, and the node is not reported `Online` | Negative   |
| I-66     | The gate runs again after a node restart and when a plugin links on a worker that had none, and a pass clears the `Unavailable` phase                                 | Positive   |
| ~~I-67~~ | ~~A check pod that is being deleted is not judged, and the step creates a fresh one once it is gone~~ Withdrawn: no check pod exists.                                 | Regression |
| I-68     | A connection refused before the control plane reports the node online emits no event                                                                                  | Boundary   |

### Pooled Address (§4.3)

File: `network_pool_assign_test.go`

| #    | Scenario                                                                                                                                                   | Type       |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| I-43 | The first pass of a node on a pooled network records the first free address of the pool in `status.network`, and reads no interface                        | Positive   |
| I-44 | An address another node already holds is not chosen                                                                                                        | Negative   |
| I-45 | A pool with every address held holds the node and emits `NetworkAddressPoolExhausted` naming the network                                                   | Negative   |
| I-46 | Two nodes that chose the same address settle on two, and the node whose name sorts later gives its choice up                                               | Boundary   |
| I-60 | The address source follows the pool and not the plugin: a `macvlan` network with no pool adopts the NIC's address, and one with a pool draws from the pool | Boundary   |
| I-75 | A pooled network needs no host check: the node proceeds with no CSI node plugin linked                                                                     | Positive   |
| I-76 | An address a node holds is kept across passes, including through a pool that changed under it                                                              | Regression |
| I-77 | An address inside the pool that another node holds under another network name is not chosen                                                                | Negative   |
| I-78 | A network with no pool whose definition names no device or PCI address holds the node with `NetworkAttachmentFailed` and asks the worker nothing           | Negative   |

### Definition and Host Check (§5.3, §8.1)

File: `network_definition_test.go`

| #        | Scenario                                                                                                                                                                                                                                     | Type       |
|----------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| ~~I-69~~ | ~~A definition whose plugin is `macvlan`, `ipvlan`, `bridge`, or `sriov` holds the node, emits `NetworkDefinitionUnsupported` naming the type, and makes no node-add call~~ Withdrawn: the operator does not inspect the plugin or the IPAM. | Negative   |
| ~~I-70~~ | ~~A definition whose IPAM is `host-local`, `whereabouts`, or `dhcp` holds the node, emits `NetworkDefinitionUnsupported` naming the IPAM, and makes no node-add call~~ Withdrawn: the operator does not inspect the plugin or the IPAM.      | Negative   |
| I-71     | A worker whose CSI node plugin is not linked holds the node with `NetworkHostCheckUnavailable`, and the next reconcile retries                                                                                                               | Negative   |
| I-72     | A plugin that does not advertise the host check is treated as unavailable and never as passed                                                                                                                                                | Negative   |
| I-73     | No pod is created for the address read or for the gate                                                                                                                                                                                       | Regression |
| I-74     | A recorded address is not read again once the NIC is in the SPDK pod, and the plugin's answer is not asked for                                                                                                                               | Regression |

---

## 3. E2E Tests

Run against a live cluster with a real control plane. Anything touching the data path asserts I/O correctness with fio verify, not that I/O continued.

### Attachment on a Live Cluster (§5, §7)

| #    | Scenario                                                                                                                                                     | Type       |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|
| E-01 | A node added on a `macvlan` data network with a pool holds the assigned address, and its listeners are created on it                                         | Positive   |
| E-02 | A volume attached from a worker with no storage node passes fio verify over the attached network                                                             | Positive   |
| E-03 | The SPDK pod restarted by the control plane keeps the same address, and the volume's I/O resumes without error                                               | Positive   |
| E-04 | Two storage nodes on one worker each take their own NIC through their own definition, or share one `macvlan` parent with a pool address each                 | Positive   |
| E-05 | A cluster on `hostInterface` is unchanged by an upgrade to the build that adds this feature                                                                  | Regression |
| E-06 | The control plane dials the SPDK pod's attached management address and completes node-add                                                                    | Positive   |
| E-07 | A volume attached from the storage worker itself passes fio verify with the host-side `macvlan` sibling in place                                             | Positive   |
| E-08 | The same attach without the host-side sibling fails, and the gate had held the network `Unavailable` with `DataNetworkHostUnreachable`                       | Negative   |
| E-09 | Per-interface `container_network_*` metrics for `data0` increase with the fio workload                                                                       | Positive   |
| E-10 | A node on a `host-device` data network holds the NIC's own address, and its listeners are created on it                                                      | Positive   |
| E-11 | A volume attached from a worker on another subnet passes fio verify, with a gateway route to that subnet in the definition                                   | Positive   |
| E-12 | The SPDK pod restarted by the control plane keeps the address and the route, and a volume attach from another subnet still passes                            | Regression |
| E-13 | The same declaration without a route to the initiators' subnet leaves the network `Unavailable` with `DataNetworkHostUnreachable`, and no volume is attached | Negative   |
| E-14 | A volume attached from the worker that hosts the node passes fio verify over a `host-device` network, through the fabric                                     | Positive   |

---

## 4. Manual Scenarios and Test Concepts

### M-01: The Multus daemon is down on a worker

**Design reference:** §8.2

**What to verify:** The SPDK pod of a node on that worker stays in `ContainerCreating`, and the node stays `Pending` with `NetworkAttachmentFailed`.

**Test concept:**
1. Scale the Multus DaemonSet pod on one worker down, or stop its service.
2. Add a node on that worker with a valid declaration.
3. Read the `StorageNode` events: one `NetworkAttachmentFailed` naming the SPDK pod.
4. Restore Multus and confirm the pod starts and the hold clears.

### ~~M-02~~: An address is already held by another host

Withdrawn: the pool has no duplicate-address check, so a pooled address that another host uses is the administrator's to leave out of the range.

### ~~M-04~~: Host bridge topology

Withdrawn: the `bridge` plugin is not supported.

### M-05: Multus upgrade during node-add

**Design reference:** §8.3

**What to verify:** Pods that start during the Multus outage fail their attach and retry, and nothing records a false address.

**Test concept:**
1. Start a node-add and restart the Multus DaemonSet while the SPDK pod is being created.
2. The SPDK pod's attach retries and succeeds after Multus returns.
3. The recorded addresses equal the assigned ones.

### M-06: Primary CNI matrix

**Design reference:** §9, §12

**What to verify:** The same declaration works on Flannel, Cilium, and Calico, and on K3s, OpenShift, and Talos.

**Test concept:**
1. For each primary CNI, install Multus and the plugins as §9 requires, including `cni.exclusive: false` for Cilium.
2. Run E-10 to E-12 and record the result per cell.

### M-07: An interface with no host address

**Design reference:** §4.3, §8.2

**What to verify:** A `host-device` network on a NIC that carries no address holds the node, and giving the NIC a static address lets the node proceed.

**Test concept:**
1. Create a `host-device` definition naming a NIC with no address, and a cluster whose network names it.
2. Add the node. It stays `Pending` with `NetworkInterfaceNoAddress` naming the interface, and no node-add call is made.
3. Give the NIC a static address. The next reconcile records it in `status.network` and the node-add proceeds.

### M-08: An adopted lease that lapses

**Design reference:** §4.3, §5.3

**What to verify:** An adopted address that the DHCP service leased without a reservation can be given to another host once the NIC is in the pod, and a reserved one cannot.

**Test concept:**
1. Shorten the lease on the data segment and add a node whose NIC holds a leased address.
2. Let the lease expire and request an address for a second host.
3. Record whether the second host receives the node's address. Repeat with a reservation for the NIC, which the second host must not receive.

---

## 5. Axis Coverage

| Axis                  | Values covered                                                          | IDs                                | Not covered                                                   |
|-----------------------|-------------------------------------------------------------------------|------------------------------------|---------------------------------------------------------------|
| Cluster topology      | 1 node, 3 nodes, 5+ nodes, several nodes per worker                     | I-14, I-10, E-10, E-04             | 5+ nodes                                                      |
| Namespace scope       | single, definition in another namespace                                 | I-16, I-39                         | namespace deleted mid-check                                   |
| Cluster count         | 1 `StorageCluster`, 2 in one Kubernetes cluster                         | I-10, I-40                         | two clusters sharing one definition name across namespaces    |
| Attachment mechanism  | `host-device`, `macvlan`, `hostInterface`                               | E-01, E-10, E-11, I-04, I-37       | `ipvlan` live, and a `host-device` NIC bound to a DPDK driver |
| Address source        | pool, adopted NIC address                                               | I-43, I-57, I-60, E-01, E-10       | a pool on a cloud network until run                           |
| Lifecycle and restart | restart, retry, resume after operator restart                           | I-27, I-30, I-31, I-63, I-66, E-03 | control plane restart mid-node-add                            |
| Primary CNI           | Flannel                                                                 | M-06                               | Cilium, Calico, OpenShift, and Talos until run                |
| Initiator location    | on the data subnet, routed from another subnet, on the pod's own worker | E-10, E-11, E-14                   | a fabric without a route between the subnets until run        |
