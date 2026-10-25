# Design Document: Storage Node Network Attachment

**Status:** Accepted  
**Author:** Manohar Reddy  
**Date:** 2026-10-24  

A storage node's management and data networks are declared as references to `NetworkAttachmentDefinition` objects, and the node's pods attach to them through Multus. This is an alternative to binding the pods to the host network, which stays supported and is what a cluster gets when it declares no network. 

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                         | Kind                    | Blocks                          | Status      |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------------------------------|-------------|
| P0-1 | Node-add accepts a list of network attachments and passes it to the agent's `spdk_process_start`, which renders the pod's networks annotation with explicit interface names, and omits `hostNetwork` | Control plane (`sbcli`) | A node on a network             | Not shipped |
| P0-2 | The SPDK pod reports for itself: `spdk_process_start` returns the pod's `network-status`, node-add and restart read interfaces from it, and the SPDK proxy binds the address it holds (§6)           | Control plane (`sbcli`) | A node on a network             | Not shipped |
| P0-3 | Multus 4 or later, the CNI plugin each definition names, and the `static` IPAM plugin on every storage worker.                                                                                       | Kubernetes              | A node on a network             | Deployer    |
| P0-4 | Workers that run a host-network pod needing a storage network have a path to that subnet, and on a worker that also hosts a storage node, to the SPDK pod's own address (§7)                         | Node OS                 | Volume attach on shared workers | Deployer    |

Without P0-1 and P0-2 a node declared on a network is held in `Pending` with an event naming the missing capability. Without P0-3 the same hold applies. Without P0-4 the node starts, and a volume attach from a worker without the path fails, which the preflight reports beforehand (§8).

---

## Overview

Today, `StorageCluster.spec.storageNodes.mgmtInterface` and `dataInterfaces` are interface names such as `eth0` and `eth1`, and the node's pods run on the host network so those names resolve. Kubernetes holds no record of which pod uses which interface or address, so nothing that works from pod network namespaces sees the storage network.

`spec.storageNodes.network` replaces the names with references. Each network points at a `NetworkAttachmentDefinition` that the administrator owns, and the SPDK pod attaches to it through Multus at an address the operator takes from the network's pool. The control plane's node record keeps the fields it has today, an interface name and an address per network. Only the source changes: the SPDK pod reports its own interfaces, because they exist in no other pod's network namespace.

---

## 1. Background

### 1.1 How a storage node uses the network today

A storage node is two kinds of pod. The operator's DaemonSet runs one node agent per worker, a Flask API on port 5000, on the host network (`operator/internal/utils/storage_node_workload.go`). When the control plane adds or restarts a node, it asks the agent to start the node's SPDK pod from the control plane's `storage_deploy_spdk.yaml.j2`. That template also uses the host network, has a restart policy of `Never`, and is named for the node's RPC port, so a worker with several sockets runs several SPDK pods behind one agent.

The agent reports the host's interfaces, read with `ip -j address show`. The control plane takes the management address from the interface named by `interface_name` and dials it later as SPDK's RPC address (`mgmt_ip`). It takes data addresses from the interfaces in `data_nics` and creates an NVMe-oF listener on each. Both values come from `StorageCluster.spec.storageNodes` and are immutable once set, because the control plane spends them at node-add and never revisits them.

The management API pods also run on the host network, because they dial the nodes' management addresses directly (`operator/internal/controllers/controlplane/managementapi.go`). A control plane outside the cluster is given the worker's `InternalIP` as the node address (`controllers/node/workload.go`, `NodeAddress`). The operator publishes a per-port DNS name for each SPDK pod (`controllers/node/spdkproxydns.go`), which the control plane uses when TLS is on.

### 1.2 What is missing

- A NIC assignment is a string, not a reference to an object the administrator owns.
- The mapping from a pod to its interfaces and addresses is not in the Kubernetes API, so per-interface traffic cannot be attributed to a pod.
- Interface names are cluster-wide. A fleet whose data NIC is `eth1` on most workers and `ens4f0` on a few needs a node group per name.

---

## 2. Goals and Non-Goals

### Goals

- Management and data networks are references in the API, and the SPDK pod attaches to them.
- The node agent and the control plane's pods leave the host network whenever a network is declared. Host networking remains a mode a cluster selects.
- The declaration is the same on any primary CNI.
- Each node's address on each network is stable across pod restarts, because the control plane and every initiator hold it, and it is chosen by the operator from a pool the cluster declares, so no address is typed per node.
- The pod-to-network mapping is readable from the Kubernetes API, and per-interface traffic is attributable to a pod.
- A cluster that declares no network behaves exactly as today.
- A network that cannot be attached is reported before the node is added and is never replaced by another network or by the host network.

### Non-Goals

- **SR-IOV.** It is not supported yet.
- **Replacing the primary CNI.** The pod network and Service routing stay with the cluster's CNI. Interfaces are added beside the default one.
- **Creating the `NetworkAttachmentDefinition`, the host path, or the CNI plugins.** The administrator owns them (§4.2). Multus and the plugins are a prerequisite (P0-3).
- **RDMA on an attached network.** macvlan and ipvlan cannot carry RoCE, so an RDMA fabric states its data networks as host interfaces (§4.4).
- **Pods that must stay on the host network.** The CSI node plugin drives the kernel NVMe/TCP initiator, which opens connections from the host's initial network namespace. The mds-proxy opens connections from the host because SPDK refuses a pod's connection to its own node. Node-preparation jobs configure hugepages and kernel modules and carry no storage traffic (Q2). All of them reach the networks through §7.
- **DHCP and pool addressing by the CNI.** Addresses that can change across a restart cannot serve a storage node. The operator draws each address from a pool the cluster declares, records it, and passes it to the `static` plugin.
- **Changing a node's network after it is added** (§4.4).

---

## 3. Architecture Overview

```
┌────────────────────────────────────────────────────────────────────┐
│                      Kubernetes Control Plane                      │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │                    StorageNodeReconciler                     │  │
│  │  1. validate spec.storageNodes.network and assign addresses  │  │
│  │  2. probe the networks on the worker before node-add         │  │
│  │  3. pass the node's attachments in the node-add call         │  │
│  │  4. verify the SPDK pod's network-status after it starts     │  │
│  └──────────────────────────────────────────────────────────────┘  │
│  StorageCluster  spec.storageNodes.network                         │
│  StorageNode     status.network (assigned address, phase)          │
│  Pod (SPDK)      k8s.v1.cni.cncf.io/networks, …/network-status     │
└─────────────────────────────┬──────────────────────────────────────┘
                              │ node-add: network attachments
┌─────────────────────────────▼──────────────────────────────────────┐
│                      simplyblock control plane                     │
│  creates the SPDK pod through the agent, reads its interfaces      │
│  from the pod, dials its RPC address, creates NVMe-oF listeners    │
└─────────────────────────────┬──────────────────────────────────────┘
                              │ spdk_process_start (networks annotation)
        worker                ▼
  ┌───────────────────┐   ┌───────────────────┐
  │ node agent pod    │──►│ SPDK pod          │ eth0 pod network
  │                   │   │   mgmt0 management│ (optional)
  └───────────────────┘   │   data0 data      │ data subnet
                          └───────────────────┘
```

The operator never talks to a CNI. It decides what the SPDK pod attaches to and reads the `network-status` annotation Multus writes on that pod. The cluster's CNI owns `eth0`, Multus owns the secondary interfaces, the operator owns the declaration, the probe, and the verification, and the control plane owns the SPDK pod's creation and the listeners.

---

## 4. API Design

### 4.1 `spec.storageNodes.network`

The group sits beside `mgmtInterface` and `dataInterfaces` on `StorageNodesSpec`. The types are in `operator/api/v1alpha2/storagecluster_types.go`.

```go
Management *StorageNetwork  `json:"management,omitempty"`
Data       []StorageNetwork `json:"data,omitempty"`
```

A `StorageNetwork` has a name, an attachment, and for a network attached through a definition an address pool (§4.3). The attachment holds exactly one of two cases.

| Attachment             | Meaning                                                                                                           |
|------------------------|-------------------------------------------------------------------------------------------------------------------|
| `networkAttachmentRef` | The SPDK pod attaches to the named `NetworkAttachmentDefinition` and takes the interface the plugin creates       |
| `hostInterface`        | The SPDK pod runs on the host network and binds the named host interface, which is what the legacy fields express |

```yaml
spec:
  storageNodes:
    network:
      management:
        name: management
        attachment:
          networkAttachmentRef: {name: storage-mgmt}
        addressPool: {subnet: 10.0.0.0/24, exclude: [10.0.0.1-10.0.0.100]}
      data:
        - name: storage-a
          attachment:
            networkAttachmentRef: {name: storage-data}
          addressPool: {subnet: 10.10.10.0/24, exclude: [10.10.10.1-10.10.10.100]}
```

The definition is looked up in the storage cluster's namespace, because Multus rejects a reference to another namespace when its namespace isolation is on. An omitted `management` leaves SPDK's RPC on the pod network (Q6).

### 4.2 The administrator owns the definition

The definition holds the plugin, the parent interface, VLAN, MTU, routes, and IPAM. Those depend on the site's NICs, switches, and CNI, which the operator cannot know, and an operator that generated definitions would need a parameter for every plugin option and would tie itself to the plugins it knows. A reference keeps the operator out of the plugin's fields, so macvlan, ipvlan, and host-device all work unchanged. The same definition can serve other workloads, and the operator never rewrites an object the administrator or another team depends on. The operator checks the result of an attachment (§5.4, §8.1) and does not write the definition.

### 4.3 Addressing

A network attached through a definition declares an `addressPool`, and the one value it must carry is the subnet, such as `10.10.10.0/24` (`storagecluster_types.go`). The subnet is the fabric's address plan. Nothing in the cluster holds it: the definition's `master` names an interface and not its subnet, a `host-device` interface may carry no host address at all, and a routed fabric gives each worker its own subnet. The operator therefore cannot derive it, and a CNI that allocates by itself would need the same range in its own configuration.

Everything else about the pool is narrowing. `first` and `last` are set together and bound the range, and without them the range is every host address of the subnet. `exclude` lists addresses and inclusive ranges, written `10.10.10.1-10.10.10.50`, that the operator never hands out. It is for what the network cannot show to ARP: a gateway, a switch, or a machine that is powered off. A worker or any machine that is up answers the duplicate-address check and is skipped without being listed. An entry outside the subnet has no effect. A network on a host interface has no pool, because the pods bind the host's own address, and the schema refuses a pool there and requires one on a definition network.

The operator gives each node one address on each pooled network, as an IPv4 address with the prefix length of the subnet, such as `10.10.10.101/24`, and the definition uses the `static` IPAM plugin. The prefix length is part of the address because the plugin takes the address in that form. The choice is the first address of the range that no other node of the cluster holds, that the node was not turned away from, and that is not excluded. The subnet's network and broadcast addresses are never chosen, except in a `/31` or a `/32`, which have none to keep back. Two addresses clash when their host parts are equal, whatever their prefix lengths.

The address has to be known before the pod exists, because the control plane records data addresses at node-add, creates listeners on them, and every initiator holds them. It has to be the same after every restart. So the operator writes it to `status.network[].assigned` before anything attaches, as the write ahead of the side effect that uses it, and never chooses again for that node. The control plane renders it into the `ips` field of the pod's network selection element (§5.1), and the verification compares it with what the pod reports (§5.4).

Two nodes that choose in the same moment, each unaware of the other, can pick the same address. Each pass of a node that has not attached yet compares its address with the addresses of the nodes whose names sort before its own, and gives its choice up on a clash, so the pair settles on two addresses without a lock.

### 4.4 Immutability and exclusivity

- `network` is present from the cluster's creation or never. It cannot be added to an existing cluster, changed, or removed, because the control plane resolves the networks at node-add and nothing re-reads the cluster field, and because a cluster that exists on the host network stays there.
- A node's assigned address is fixed once chosen, because the listeners and every initiator hold it. Changing one is a remove and a re-add.
- `network` and the legacy fields are mutually exclusive. The legacy fields stay supported and mean `hostInterface` attachments.
- A cluster's networks are all `networkAttachmentRef` or all `hostInterface`, because a pod is either on the host network or not.
- A cluster whose `fabricType` includes RDMA cannot declare a `networkAttachmentRef` data network.

### 4.5 Per-node status

A node states no address. `status.network` records, per network, the address the operator assigned from the pool, the addresses the pool turned away for this node (`rejected`, at most 64), the interface name inside the pod, the address the pod reported, whether the host can reach it, and a phase of `Pending`, `Attached`, `Mismatched`, or `Unavailable`. A node on the host network has no entries. The types are in `operator/api/v1alpha2/storagenode_types.go`.

An assigned address lives in the node's status only. A `StorageNode` object deleted and created again while its SPDK pod still holds the old address assigns again, and the pod's report then differs from the new choice (Q8).

### 4.6 In a ClusterDeploymentConfig

A `ClusterDeploymentConfig` carries the network the way it carries every other cluster setting. `spec.cluster.network` is the same block as `spec.storageNodes.network`, address pools included, and is copied onto the cluster the document creates, which is the only moment it can be set (§4.4). The document names no node address: the operator assigns each from the pool when the node is added.

```yaml
spec:
  cluster:
    name: production
    network:
      data:
        - name: fabric
          attachment:
            networkAttachmentRef:
              name: fabric-data
          addressPool: {subnet: 10.10.10.0/24, exclude: [10.10.10.1-10.10.10.150]}
  nodeSets:
    - name: rack-a
      groups:
        - name: group-1
          workers: [worker-1, worker-2]
```

A document that states no network expands exactly as it always did, and discovery never proposes one: which definition serves the storage network and which subnet its pool covers are the administrator's decisions. A group on a network names no `mgmtInterface` or `dataInterfaces`, and the schema refuses the combination.

---

## 5. Attaching a Node

### 5.1 The attachment list and the annotation

The operator passes the node's attachments in the node-add call as a list: the definition's name, an interface name, and the node's address. Interface names are explicit so none depends on Multus's ordering: `mgmt0` for the management network and `data0` onward for data networks. The control plane keeps the list with the node record and passes it to the agent at every start, and the agent renders it into the SPDK pod's `k8s.v1.cni.cncf.io/networks` annotation. Each element carries `ips` with a prefix length. No element sets `default-route`, so the pod's default route stays on `eth0`.

### 5.2 The pods that attach

| Pod                              | With a declared network                                           | Otherwise | Why                                                                                           |
|----------------------------------|-------------------------------------------------------------------|-----------|-----------------------------------------------------------------------------------------------|
| SPDK                             | `mgmt0` if declared, and `data0` onward                           | Host      | It serves RPC on the management address and listeners on the data ones                        |
| Node agent                       | Pod network, plus `mgmt0` when the control plane is external (Q1) | Host      | The control plane reaches it, and it reads no host interface once the SPDK pod reports        |
| Control plane pods               | Pod network, plus the management network (Q1)                     | Host      | They dial the SPDK pod's management address and the agent                                     |
| Operator-created jobs and probes | Pod network                                                       | Host      | A job with no storage traffic and no host interface read needs no host network                |
| CSI node plugin                  | Host                                                              | Host      | The kernel NVMe/TCP initiator opens its connections from the host's initial network namespace |
| mds-proxy                        | Host                                                              | Host      | SPDK refuses a pod's connection to its own node                                               |

### 5.3 Requirements on the definition

A definition that serves a storage node meets these requirements, and the preflight checks the ones it can observe.

- **`static` IPAM, and the `ips` capability declared.** Multus 4.2.2 passed the assigned address with and without the declaration, but the declaration is the documented contract. A definition with another IPAM plugin ignores the assigned address, and only the resulting address shows it.

```json
{
  "cniVersion": "0.3.1",
  "name": "storage-data",
  "type": "macvlan",
  "master": "ens4",
  "mode": "bridge",
  "mtu": 9000,
  "capabilities": { "ips": true },
  "ipam": { "type": "static" }
}
```

- **The pool excludes any DHCP scope or IPAM range** that another definition uses on the same segment, because nothing checks a static address against them. The probe's `arping -D` finds a host that answers on an address, and not one that is merely reserved or powered off, so those go in `exclude`.
- **The definition owns the MTU and the routes.** A route to a peer subnet belongs in the static IPAM block. `default-route` is never set on a storage network, because Multus would replace the pod's default route and break its reach to the cluster.
- **Two interfaces of a pod do not share a subnet,** because that causes ARP flux and reverse-path filtering drops. A definition that must share one adds a `tuning` plugin to set `arp_ignore` and `arp_announce`.
- **One parent NIC carries one plugin kind.** macvlan and ipvlan children cannot share a parent (§7).
- **The `master` is one name for the whole cluster.** A fleet with different NIC names needs a definition per name (Q3).

### 5.4 Verification in two stages

The SPDK pod does not exist when the operator decides whether to add a node, so only the first stage can gate the add.

**Before node-add**, the probe (§8.1) runs a pod attached the way the SPDK pod will be. It proves the definition, the plugin, the declared address, the MTU, that no other host holds the address, and that a peer answers. A failed probe holds the node in `Pending` and the control plane is not called.

**After the SPDK pod is `Running`**, the control plane and the operator read the pod's `network-status`, matching the pod by its RPC-port label, and compare each interface and address with the declaration. A match records `Attached`. A difference records `Mismatched`, emits `NetworkAddressMismatch`, and takes the node out of service through the control plane's shutdown, because its listeners were created on an address nothing can be assumed to answer. The node is not restarted until the declaration or the definition is corrected.

`network-status` is written once, when Multus finishes the attach. It is evidence of the attach and says nothing about the interface's later state, which the SPDK proxy reports (§6).

### 5.5 RBAC

The operator needs `get` and `list` on `network-attachment-definitions` in `k8s.cni.cncf.io`, `get` and `list` on `pods`, and `create` and `delete` on `pods` in the storage namespace. RBAC cannot narrow the last to the probe's label. The probe pods run unprivileged apart from the `NET_RAW` capability `arping` needs.

---

## 6. What the Control Plane Is Given

For a `hostInterface` attachment every call carries the legacy values unchanged. For a node on a network, the SPDK pod's interfaces exist only in its own network namespace, while every sbcli path that touches a node's NICs reads the agent's report from another. Node-add looks `data_nics` up in it and asks the agent whether the interface is TCP or RoCE capable, restart repeats that, the health check pings through the agent, and the management address comes from it.

The SPDK pod therefore reports for itself, and the node agent reports its own host. The sbcli changes (P0-2) are these:

1. **The start reports the pod's interfaces.** `spdk_process_start` waits for the pod to be `Running` and to hold every declared interface, and returns the pod's address and the address and MAC of each interface from `network-status`. Node-add takes the node's data NICs from that response only, and the node records the address SPDK's RPC is served on as `spdk_ip`.
2. **`mgmt_ip` stays the worker's address.** Every placement rule that asks whether two nodes share a worker, the failure-domain maps, and the agent's own endpoint read it, so it keeps meaning the host. `spdk_ip` is the one address that moves: the control plane dials it for SPDK RPC, and under TLS it dials the per-worker DNS name instead.
3. **The agent reports the host.** The agent runs in the pod network, so `hostname -s` and its interfaces are the pod's. The DaemonSet hands it `HOSTNAME` (the worker's name), `HOST_IP` (the worker's address), and `POD_IP`, and `/info` reports the worker's name and address. Node-add takes the worker's address from `host_ip` when the cluster declares no host interface. The node-preparation job, which stays on the host network, calls the agent at `POD_IP`.
4. **The proxy binds the address it holds.** For a node on a network the proxy binds the management interface's address inside the pod, or the pod's own address (`POD_IP`) when no management network is declared.
5. **Restart reads the interfaces every time,** so the reported addresses are confirmed on every start, and a pod recreated on another address is followed.
6. **The capability probes answer from the attachment.** An attached interface is TCP capable and not RoCE capable, so the agent's probes are not called.
7. **Live checks go through the SPDK proxy,** which runs in the pod's namespace. It answers a link-state check and a ping sent out of a named interface as local methods, without asking SPDK. The health check uses them in place of the agent's `ping_ip`, which reads the agent's namespace.

The DaemonSet adds `HOST_IP` and `POD_IP` only for a cluster that declares a network, so an upgrade does not roll the agents of a cluster on the host network.

`bind_to_device` needs no change, because the interface name only has to exist in SPDK's own namespace.

When no management network is declared, SPDK's RPC stays on the pod network. The control plane has to dial a name that follows the pod across restarts, which is the per-port DNS name, because a pod address changes when the pod is recreated (Q6).

---

## 7. Reachability

Three host-network processes reach an SPDK pod's attached addresses: the kernel NVMe/TCP initiator behind every volume attach, a control plane pod dialing the management address, and the mds-proxy.

**On another worker** the path is ordinary routing on the network's subnet.

**On the same worker** a macvlan child is not reachable from its parent's host namespace, and an ipvlan child behaves the same way. A macvlan and an ipvlan child cannot share a parent, and the second kind fails the pod's sandbox with `failed to create ipvlan: device or resource busy`.

| Topology                                                           | Supported            | Requirement                                                                                              |
|--------------------------------------------------------------------|----------------------|----------------------------------------------------------------------------------------------------------|
| Storage workers run no host-network pod that needs the network     | Yes, with any plugin | None. The preflight records `hostReachable: false` and warns                                             |
| A storage worker also attaches volumes or runs a control plane pod | With a host path     | A host-side macvlan child on the same parent with a free address and a route to the pod's address (P0-4) |
| `hostInterface` attachment                                         | Yes                  | None. The host is the pod's namespace                                                                    |

The host-side child needs one interface per worker that shares a node with a storage pod, created by NetworkManager, systemd-networkd, or an NMState policy, and it adds no data-path overhead. Setting `arp_ignore` and `arp_announce` on the parent is a hardening for a shared subnet.

The host's own address on the parent defeats the child. A worker whose NIC carries an address in the data subnet routes that subnet out of the NIC, and the NIC cannot reach its macvlan children, so the host path has to carry the worker's address on that subnet: move it from the NIC to the child.

A host bridge holding the NIC, with the `bridge` plugin attaching pods to it, reached pods on its own worker but failed off-node on the test fabric. The bridge topology is unsupported until the live matrix (§12) passes it. The `bridge` plugin with `isGateway` on a bridge not attached to the NIC reaches pods on its own worker only.

An initiator cannot run in a pod namespace to avoid this, because a kernel NVMe/TCP controller can reconnect from the host's initial namespace and its connection would then move or fail.

**Clients outside the cluster.** `clientDataIfname` is unchanged.

**Several data networks.** A volume's initiator opens one path per data network, so each worker that attaches volumes needs a route to every declared data subnet.

---

## 8. Preflight, Failure Modes, and Operations

### 8.1 Preflight

Before a node is added, the operator runs two probes on the worker, as `nodeprobe` does for devices. They run in the provisioning step `CheckingNetwork`, which sits between the configuration gate and the queue for a node-add slot, so a node holds no slot while it waits on a definition. A cluster that attaches no network through a definition skips the step.

The first is a pod attached to the declared networks the way the SPDK pod will be. It checks that the definition exists, that its plugin is installed on the worker, that the pod got the assigned address and MTU, that no other host answers an `arping -D` for the address, and that up to three peers on the data network answer. The same `arping -D` is sent for the next sixteen addresses of the pool that the node could take, without attaching them. A busy start of the subnet then costs one probe and not one per address: every address that answers is recorded in `status.network[].rejected`, and the node takes the first one that did not. When the assigned address is free, the candidates that answered are still recorded, so that no later choice tries them. The checks run one after another and take a few seconds each, so a probe with a full batch runs for up to a minute. A second container keeps the pod running after the checks end, so its network namespace outlives them. Its result gates node-add.

The second is a host-network pod that pings the first probe's address, which is the path a host process needs on a shared worker (§7). Its result is recorded as `hostReachable` and warned on. It does not gate node-add, and a host probe that has not answered after two minutes is dropped without a result.

Both probes are deleted before node-add, so neither holds an address together with the SPDK pod. Node-add waits for them to be gone. Both probes carry a deadline of ten minutes, so a probe the operator lost track of ends on its own. They also carry a one-second termination grace period, and the holder runs `exec sleep` so that it receives the stop signal itself, so a lingering probe does not delay the node's next one.

A probe pod that is being deleted is never judged. A node whose address changed finds the earlier attempt's pod under the same name until the kubelet has finished tearing it down, and that pod's `network-status` belongs to the old address. The step waits for it to go and then creates a fresh probe for the new address. The same holds for the host probe.

Probes of different nodes run at the same time, and each holds the address it was given. A candidate that answers because another node of the cluster has it assigned is not a foreign host, so it is not recorded in `rejected`. The owning node's own pass settles a real clash through the earlier-name rule of §4.3.

### 8.2 Failure modes

| Failure                                                                                     | Detection                                              | Behavior                                                                                                                                                           |
|---------------------------------------------------------------------------------------------|--------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| The cluster has no `NetworkAttachmentDefinition` kind                                       | Discovery on the kind                                  | The node stays `Pending`. `NetworkAttachmentUnsupported` on the node                                                                                               |
| A referenced definition does not exist                                                      | Get on the reference                                   | The node stays `Pending`. `NetworkAttachmentNotFound` on the node                                                                                                  |
| The definition's plugin is not installed on the worker                                      | The probe pod stays in `ContainerCreating`             | The node stays `Pending`. `NetworkAttachmentFailed` on the node, naming the pod                                                                                    |
| The Multus daemon is down on the worker                                                     | The probe pod stays in `ContainerCreating`             | The node stays `Pending`. `NetworkAttachmentFailed` on the node. Every new pod on that worker is affected                                                          |
| The probe holds an address other than the assigned one                                      | `network-status` of the probe pod                      | The node stays `Pending`. `NetworkAddressMismatch` on the node                                                                                                     |
| Another host answers for the assigned address or a candidate                                | The probe's `arping -D`                                | The node does not stay held. It turns away every address that answered, removes the probe, and takes the first one that did not. `NetworkAddressInUse` on the node |
| The pool has no address left for the node, or the node turned away the 64 it keeps          | The assignment                                         | The node stays `Pending`. `NetworkAddressPoolExhausted` on the node, naming the network and the range                                                              |
| The pool is not a range inside its subnet, or an `exclude` entry is not an address or range | The assignment                                         | The node stays `Pending`. `NetworkAddressPoolInvalid` on the node                                                                                                  |
| A started SPDK pod's address differs from the declared one                                  | `network-status` of the SPDK pod after it is `Running` | Phase `Mismatched`. The node is taken out of service. `NetworkAddressMismatch` on the node                                                                         |
| A peer on the data network does not answer the probe                                        | The probe pod's peer check                             | The node stays `Pending`. `DataNetworkUnreachable` on the node                                                                                                     |
| The host cannot reach the SPDK address on its own worker                                    | The host-network probe                                 | The node proceeds. `hostReachable` is false and `DataNetworkHostUnreachable` is emitted on the node                                                                |
| An attached SPDK pod loses its interface                                                    | The SPDK proxy's link report, read by the health check | Phase `Unavailable`. `NetworkAttachmentLost` on the node                                                                                                           |

A pod whose attach fails never starts, because kubelet keeps retrying the sandbox, so the detectable failure is a pod that has not started after a minute. The operator names the pod and does not read its events, because that read needs access to every event in the cluster, so the administrator reads the cause with `kubectl describe`. A missing definition leaves the pod in `ContainerCreating` with a `NoNetworkFound` event from Multus, and `static` IPAM accepts an address another host holds, which is why the probe runs `arping -D`.

No failure falls back to the host network, because a node listening on a different network than declared would be unreachable by the initiators that were told otherwise.

### 8.3 Operations

| Situation                                 | What the administrator does                                                                                                                                              |
|-------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A node is held in `Pending` for a network | Read the event on the `StorageNode`, fix the definition or the worker, and the next reconcile retries the probe                                                          |
| A definition is edited                    | The edit applies when the SPDK pod next starts, because Multus reads the definition at attach. A restart of the node applies it                                          |
| A node's address has to change            | Remove the node and add it again (§4.4)                                                                                                                                  |
| A node has to return to the host network  | Remove the node and add it again in a cluster whose networks are `hostInterface`. `network` is fixed at the cluster's creation                                           |
| A worker is replaced                      | Migrate the node. The new worker needs the definition's plugin, the parent interface the definition names, and a host path if it hosts both a storage node and attachers |
| Multus is upgraded or its daemon restarts | Every pod that starts during the outage fails its attach and retries. Schedule the upgrade where no node is being added or restarted                                     |

---

## 9. Portability Across CNIs

The attachment path uses two standard objects, the `NetworkAttachmentDefinition` and the `network-status` annotation, and neither belongs to a primary CNI. No Cilium CRD, Calico IP pool, or Flannel option is read or written, and the address is verified from `network-status`, not from `ip addr` or the declaration. The only per-CNI work is keeping Multus's configuration in place:

| Platform                  | Behavior to configure for                                                                                                                                                                                                                                                                                                                                                                                                                  |
|---------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Cilium as the primary CNI | `cni.exclusive` is true by default, and Cilium then takes ownership of the CNI configuration directory. Set it to false. Set `devices` explicitly so the eBPF datapath does not attach to a storage NIC. The reference plugins are not shipped                                                                                                                                                                                             |
| K3s                       | The CNI configuration directory is `/var/lib/rancher/k3s/agent/etc/cni/net.d` and the plugin directory is `/var/lib/rancher/k3s/data/cni`. Only `bridge` and `host-local` are bundled, so `macvlan` and `static` are installed. Multus needs `--multus-kubeconfig-file-host` set to the host path of its kubeconfig, or every pod start on the node fails with `stat /etc/cni/net.d/multus.d/multus.kubeconfig: no such file or directory` |
| OpenShift                 | Multus runs with namespace isolation on, so a definition has to be in the pod's namespace or a global one (§4.1). A macvlan definition needs a host parent interface                                                                                                                                                                                                                                                                       |
| Any distribution          | The Multus configuration must sort ahead of the primary CNI's, and with the thick plugin its daemon is a dependency of every pod start (§8.2)                                                                                                                                                                                                                                                                                              |

---

## 10. Backend API Requirements

| Method | Endpoint                                                      | Notes                                                                                                                                                                                           |
|--------|---------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `/api/v2/clusters/{id}/storage-nodes` (node-add)              | Exists. P0-1 adds `networks`, a list of `name`, `namespace`, `interface`, and `ips`, and records it on the node. P0-2 takes `data_nics` and the management address from the SPDK pod's response |
| POST   | `/api/v2/clusters/{id}/storage-nodes/{id}/{action}` (restart) | Exists. P0-1 and P0-2: it passes the recorded attachments again and reads the pod's interfaces on every restart                                                                                 |
| POST   | Node agent `/spdk_process_start`                              | Exists. P0-1 renders the attachments as the annotation and drops `hostNetwork`. P0-2 returns the pod's `network-status` once the pod is `Running`                                               |
| GET    | Node agent `/info`, `/ifc_is_tcp`, `/ifc_is_roce`             | Exist, unchanged. Not called for a node on a network                                                                                                                                            |
| GET    | SPDK proxy link report and ping                               | New (P0-2). The ping is sent out of a named interface in the SPDK pod's namespace. The health check uses it for an attached data interface                                                      |

P0-1 and P0-2 are the design's external blockers, and P0-2 reaches the health check and the restart flow as well as node-add. Node-add and restart must be idempotent on their attachment parameters, because the reconciler retries them.

---

## 11. Observability

Per-interface traffic already exists: kubelet and cAdvisor report `container_network_*` per interface for a pod with its own network namespace. A host-network pod reports the host's interfaces with no mapping to the pod. Moving the SPDK pod onto an attached network is what makes `interface="data0"` a statement about that pod.

### Kubernetes Events

Events land on the `StorageNode`, which the administrator owns and which outlives the pods.

| Event                                                                                         | Type    | Reason                         |
|-----------------------------------------------------------------------------------------------|---------|--------------------------------|
| The cluster has no `NetworkAttachmentDefinition` kind, so the node is held                    | Warning | `NetworkAttachmentUnsupported` |
| A referenced definition does not exist, so the node is held                                   | Warning | `NetworkAttachmentNotFound`    |
| A pod could not attach, so the node is held                                                   | Warning | `NetworkAttachmentFailed`      |
| A pod's address differs from the declared one, so the node is held or taken out of service    | Warning | `NetworkAddressMismatch`       |
| Another host answers for an address of the pool, so the node takes the next one               | Warning | `NetworkAddressInUse`          |
| The pool has no address left, so the node is held                                             | Warning | `NetworkAddressPoolExhausted`  |
| The pool is not a range inside its subnet, so the node is held                                | Warning | `NetworkAddressPoolInvalid`    |
| A peer on the data network does not answer, so the node is held                               | Warning | `DataNetworkUnreachable`       |
| The host cannot reach the SPDK address on its own worker, so volumes cannot be attached there | Warning | `DataNetworkHostUnreachable`   |
| An attached SPDK pod lost its interface                                                       | Warning | `NetworkAttachmentLost`        |
| Every declared network is attached and verified                                               | Normal  | `NetworkAttached`              |

### Prometheus Metrics

| Metric                                                  | Labels                       | Description                                                                        |
|---------------------------------------------------------|------------------------------|------------------------------------------------------------------------------------|
| `simplyblock_storagenode_network_attached_state`        | `cluster`, `node`, `network` | 1 while the network is attached and verified, 0 otherwise                          |
| `simplyblock_storagenode_network_host_reachable_state`  | `cluster`, `node`, `network` | 1 while the host can reach the node's SPDK address on its own worker, 0 otherwise  |
| `simplyblock_storagenode_network_attach_failures_total` | `cluster`, `reason`          | Attach attempts that ended in each reason of the table above                       |
| `simplyblock_storagenode_network_preflight_seconds`     | `cluster`                    | Histogram of the preflight's duration, from the probe pod's creation to its result |

The attached gauge reading 0 for a node whose pods are running alerts on a lost interface. The host-reachable gauge reading 0 on a worker that attaches volumes alerts on a volume attach that will fail. The failures counter separates a cluster missing Multus from a mistyped definition name, which look identical from the node's phase.

---

## 12. Testing Strategy

The scenario matrix is in [`test-plan-storage-network.md`](../tests/test-plan-storage-network.md). The sbcli changes of P0-1 and P0-2 are tested in sbcli, and this repository tests the operator's behavior at that boundary, faked.

The risk concentrates where no unit test reaches: the two verification stages, reachability on a shared worker (§7), and the CNI portability claim (§9). A live run is required on each cell of primary CNI against secondary plugin: Flannel on K3s, Cilium, and Calico against macvlan, ipvlan, `bridge`, and host-device, plus OpenShift and Talos, with the host-path variants of §7.

---

## 13. Migration

The legacy fields and the network group are mutually exclusive, and `network` can only be present from the cluster's creation, so no existing cluster changes mode in place. A cluster that states no network is unaffected by every section above: an upgrade leaves its pods on the host network, its restarts render the same pod as before, and a pool is accepted only on a network attached through a definition, which such a cluster has none of.

A node already added keeps the networks the control plane recorded at node-add, and a node's `status.network` is empty exactly when it is on the host network. Moving a node between networks is a remove and a re-add, or a node migration (Q3).

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Owner                   |
|-----|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|
| Q1  | How the agent and the control plane's pods are addressed off the host network. The agent is one per worker and serves several SPDK pods, so its address is per worker and cannot come from a node's `spec.config.network`. A control plane inside the cluster can reach it at its pod address, and one outside needs an address routed on the management network. Each control plane replica dials the SPDK pod's management address, so each needs a stated management address | Operator, control plane |
| Q2  | Whether the node-preparation jobs can keep the host network, or any of them reads a network the node's pods now leave                                                                                                                                                                                                                                                                                                                                                           | Control plane (`sbcli`) |
| Q3  | How a node moves between networks without removal, and how a fleet with different NIC names per worker selects among definitions                                                                                                                                                                                                                                                                                                                                                | Operator                |
| Q4  | Whether the operator creates the host-side interface a shared worker needs, through a privileged DaemonSet, or leaves it to the administrator                                                                                                                                                                                                                                                                                                                                   | Operator                |
| Q6  | How the control plane dials SPDK's RPC when no management network is declared and TLS is off. It dials `mgmt_ip` directly today and the per-port DNS name only under TLS                                                                                                                                                                                                                                                                                                        | Control plane (`sbcli`) |
| Q7  | IPv6 and dual-stack data networks                                                                                                                                                                                                                                                                                                                                                                                                                                               | Operator                |
| Q8  | Whether a `StorageNode` recreated after its status was lost can recover the address its SPDK pod still holds. The assigned address is kept in status only, so a recreated object assigns again, and verification then records the pod's address as a mismatch                                                                                                                                                                                                                   | Operator                |

---
