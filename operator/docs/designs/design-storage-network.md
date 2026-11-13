# Design Document: Storage Node Network Attachment

**Status:** Accepted  
**Author:** Manohar Reddy  
**Date:** 2026-10-24 (last updated 2026-10-28)  

A storage node's management and data networks are declared as references to `NetworkAttachmentDefinition` objects, and the node's pods attach to them through Multus. The plugin is `host-device`, `macvlan`, or `ipvlan`: the pod takes a whole NIC from the worker, or a virtual interface on one, with an address from a pool the administrator declares or, with no pool, the address the NIC holds. This is an alternative to binding the pods to the host network, which stays supported and is what a cluster gets when it declares no network. 

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                         | Kind                    | Blocks                            | Status      |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|-----------------------------------|-------------|
| P0-1 | Node-add accepts a list of network attachments and passes it to the agent's `spdk_process_start`, which renders the pod's networks annotation with explicit interface names, and omits `hostNetwork` | Control plane (`sbcli`) | A node on a network               | Not shipped |
| P0-2 | The SPDK pod reports for itself: `spdk_process_start` returns the pod's `network-status`, node-add and restart read interfaces from it, and the SPDK proxy binds the address it holds (§6)           | Control plane (`sbcli`) | A node on a network               | Not shipped |
| P0-3 | Multus 4 or later, the plugin the definition names (`host-device`, `macvlan`, or `ipvlan`), and the `static` IPAM plugin on every storage worker.                                                    | Kubernetes              | A node on a network               | Deployer    |
| P0-4 | Workers that attach volumes reach the data subnet through the fabric, including a worker that runs an SPDK pod on the data subnet (§7)                                                               | Node OS                 | Volume attach on shared workers   | Deployer    |
| P0-5 | A data interface whose initiators are on other subnets has a route to those subnets through the fabric's gateway, set in its definition (§7)                                                         | Kubernetes              | Volume attach from another subnet | Deployer    |

Without P0-1 and P0-2 a node declared on a network is held in `Pending` with an event naming the missing capability. Without P0-3 the same hold applies. Without P0-4 or P0-5 the node starts, and a volume attach from a worker without the path fails, which the reachability gate reports (§8.1).

---

## Overview

Today, `StorageCluster.spec.storageNodes.mgmtInterface` and `dataInterfaces` are interface names such as `eth0` and `eth1`, and the node's pods run on the host network so those names resolve. Kubernetes holds no record of which pod uses which interface or address, so nothing that works from pod network namespaces sees the storage network.

`spec.storageNodes.network` replaces the names with references. Each network points at a `NetworkAttachmentDefinition` that the administrator owns, and the SPDK pod attaches to it through Multus. The definition moves a NIC of the worker into the pod, or creates an interface on it, and the node's address is drawn from the network's address pool or, with no pool, is the address the NIC holds, which the operator reads before the attach. The control plane's node record keeps the fields it has today, an interface name and an address per network. Only the source changes: the SPDK pod reports its own interfaces, because they exist in no other pod's network namespace.

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
- Each node's address on each network is stable across pod restarts, because the control plane and every initiator hold it, and the operator chooses it, from the network's pool or from the NIC, so no address is typed per node.
- The pod-to-network mapping is readable from the Kubernetes API, and per-interface traffic is attributable to a pod.
- A cluster that declares no network behaves exactly as today.
- A network that cannot be attached is reported before the node is added and is never replaced by another network or by the host network.

### Non-Goals

- **Other plugins.** `bridge` is not qualified. The operator does not inspect the plugin, so a definition of another type attaches only if it takes its address from the `ips` of the network selection element. Devices a device plugin hands out (an SR-IOV VF pool) are covered in §5.3.
- **Replacing the primary CNI.** The pod network and Service routing stay with the cluster's CNI. Interfaces are added beside the default one.
- **Creating the `NetworkAttachmentDefinition`, the routes, or the CNI plugins.** The administrator owns them (§4.2). Multus and the plugins are a prerequisite (P0-3).
- **RDMA on an attached network.** An RDMA fabric states its data networks as host interfaces (§4.4).
- **Pods that must stay on the host network.** The CSI node plugin drives the kernel NVMe/TCP initiator, which opens connections from the host's initial network namespace. The mds-proxy opens connections from the host because SPDK refuses a pod's connection to its own node. Node-preparation jobs configure hugepages and kernel modules and carry no storage traffic (Q2). All of them reach the networks through §7.
- **Addressing by the CNI.** DHCP, `host-local`, and Whereabouts allocate at attach time, so the address is not known before the pod exists and can differ after a restart. The operator chooses the address, records it, and passes it to the `static` plugin. A host address that a DHCP service leased is adopted as it stands, and the service has to keep it for that NIC (§5.3).
- **Changing a node's network after it is added** (§4.4).

---

## 3. Architecture Overview

```
┌────────────────────────────────────────────────────────────────────┐
│                      Kubernetes Control Plane                      │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │                    StorageNodeReconciler                     │  │
│  │  1. validate spec.storageNodes.network and assign addresses  │  │
│  │  2. check the networks on the worker before node-add         │  │
│  │  3. pass the node's attachments in the node-add call         │  │
│  │  4. verify network-status and the host path after it starts  │  │
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

The operator never talks to a CNI. It decides what the SPDK pod attaches to and reads the `network-status` annotation Multus writes on that pod. The cluster's CNI owns `eth0`, Multus owns the secondary interfaces, the operator owns the declaration, the address, and the verification, and the control plane owns the SPDK pod's creation and the listeners.

---

## 4. API Design

### 4.1 `spec.storageNodes.network`

The group sits beside `mgmtInterface` and `dataInterfaces` on `StorageNodesSpec`. The types are in `operator/api/v1alpha2/storagecluster_types.go`.

```go
Management *StorageNetwork  `json:"management,omitempty"`
Data       []StorageNetwork `json:"data,omitempty"`
```

A `StorageNetwork` has a name and an attachment. The attachment holds exactly one of two cases.

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
      data:
        - name: storage-a
          attachment:
            networkAttachmentRef: {name: storage-data}
```

The definition is looked up in the storage cluster's namespace, because Multus rejects a reference to another namespace when its namespace isolation is on. An omitted `management` leaves SPDK's RPC on the pod network (Q6).

### 4.1a Serving on the pod network

A third attachment, `podNetwork: {}`, puts the SPDK pod on the cluster CNI's network with nothing attached beside it. The pod's one interface carries the RPC and the data, so the CRD admits it only as the single data network, with no management network and no address pool.

```yaml
spec:
  storageNodes:
    network:
      data:
        - name: cni
          attachment:
            podNetwork: {}
```

The pod's own address changes on every restart, and every peer and client holds the address a volume is served on. The node agent therefore keeps a ClusterIP Service in front of each SPDK pod, created before the pod and deleted only with the node, and the control plane records the Service's address as the node's data address while the target binds the pod's own. A Service forwards only the ports it names, so the control plane publishes the node's ports to it whenever they change: at the add, on every restart, and for every lvstore the node starts or stops serving. The kernel initiator on a client reconnects to the same address after a restart, which is what keeps failover and restart behaviour as they are on a host interface.

The operator does none of the attaching: there is no definition to require, no address to assign and no probe to run, so a pod-network node passes the network check at once. Node-add carries `pod_network: true` and no `networks`. The node's status reports the one network with interface `eth0`, the Service's address, and a phase of `Attached` once the pod runs behind the Service, `Unavailable` while it does not, and `Pending` until the Service exists.

What the pod network costs: the traffic goes through the CNI's datapath instead of a NIC. Measured on the Talos lab (Cilium 1.20, four nodes 2+1, node NIC at 1500, fio 4k random read at queue depth 32 through the CSI), a client on the primary's node reached 36k IOPS against 48-50k on a VF, and a cross-node client 31k with native routing against 48k on a VF; in tunnel mode the cross-node figure drops by a further third and large-block bandwidth to a third. Native routing on a jumbo node network is the configuration this mode is meant for.

### 4.2 The administrator owns the definition

The definition holds the plugin, the NIC it moves, IPAM, and routes. Those depend on the site's NICs, switches, and CNI, which the operator cannot know, and an operator that generated definitions would need a parameter for every plugin option and would tie itself to the plugins it knows. A reference keeps the operator out of the plugin's other fields. The same definition can serve other workloads, and the operator never rewrites an object the administrator or another team depends on. The operator checks the plugin, the IPAM, and the result of an attachment (§5.4, §8.1) and does not write the definition.

### 4.3 Addressing

A node's address on a network attached through a definition comes from one of two sources, chosen by whether the network declares an address pool. Either way the operator chooses it before anything attaches, writes it to `status.network[].assigned` as the write ahead of the side effect that uses it, and never chooses it again for that node. The control plane renders it into the `ips` field of the pod's network selection element (§5.1), and the verification compares it with what the pod reports (§5.4).

**With a pool, the address is drawn from it.** The pool is the subnet of the fabric, and optionally a first and a last address inside it:

```yaml
data:
  - name: storage-a
    attachment:
      networkAttachmentRef: {name: storage-data}
    addressPool:
      subnet: 10.10.10.0/24
      first: 10.10.10.101
      last: 10.10.10.140
```

A node takes the first address of the range that no other storage node of the namespace holds, written with the subnet's prefix length. The network and broadcast addresses are never taken, except in a `/31` or a `/32`. Addresses compare by host part, so a node holding `10.10.10.101/16` holds the address whatever the prefix length. The nodes of a namespace share the pool, including the member clusters of a supercluster, which copy one network block from the document, so a node counts what every storage node of the namespace holds on that network name or inside that subnet. Two nodes that choose at the same moment settle without a lock: a node whose choice is still `Pending` gives it up when a node whose name sorts earlier holds the same address, and chooses again. A pool with no address left holds the node in `Pending` with `NetworkAddressPoolExhausted`. The operator does not check the wire for a host that already uses a pooled address, so the range has to leave out the addresses of the gateway, the switches, and any machine that is not a storage node.

**Without a pool, the address is the NIC's.** The operator asks the CSI node plugin of the worker, which runs on the host network, for the addresses of the interface the definition names, by name (`device` or `master`) or by PCI address (§8.1). The node's address is the interface's first IPv4 address with its prefix length, which is the address the control plane would take from the interface on the host (§1.1). A fabric that delivers the address to the NIC delivers it to the pod, which is what a cloud network that routes only the addresses assigned to an interface requires. A definition that names neither a device nor a PCI address leaves nothing to read, and holds the node with `NetworkAttachmentFailed`.

An interface with no IPv4 address, or whose first one is link-local, has no usable address, and the node is held in `Pending` with `NetworkInterfaceNoAddress`, naming the interface. The interface report marks a leased address `dynamic`, as it marks an interface's primary address on a cloud network, so the mark cannot decide whether an address is safe to adopt. A `host-device` NIC moves into the pod, so no DHCP client on the host renews the lease and the service has to keep that address for that NIC (§5.3). An address that two workers hold on one segment is a fault of the fabric, and the operator does not check for it.

The pool is not allowed with `hostInterface`, which carries the host's own address. A cloud network that delivers only the addresses assigned to an interface has to have each pooled address assigned to the node's data ENI, which is the administrator's step (§9).

### 4.4 Immutability and exclusivity

- `network` is present from the cluster's creation or never. It cannot be added to an existing cluster, changed, or removed, because the control plane resolves the networks at node-add and nothing re-reads the cluster field, and because a cluster that exists on the host network stays there.
- A node's assigned address is fixed once recorded, because the listeners and every initiator hold it. Changing one is a remove and a re-add.
- `network` and the legacy fields are mutually exclusive. The legacy fields stay supported and mean `hostInterface` attachments.
- A cluster's networks are all `networkAttachmentRef` or all `hostInterface`, because a pod is either on the host network or not; or the one `podNetwork` data network alone (§4.1a).
- A cluster whose `fabricType` includes RDMA cannot declare a `networkAttachmentRef` data network.

### 4.5 Per-node status

A node states no address. `status.network` records, per network, the address the operator chose, the interface name inside the pod, the address the pod reported, whether the workers' initiators can reach it, and a phase of `Pending`, `Attached`, `Mismatched`, or `Unavailable`. A node on the host network has no entries. The types are in `operator/api/v1alpha2/storagenode_types.go`.

An assigned address lives in the node's status only. A `StorageNode` object deleted and created again while its SPDK pod still holds the old address assigns again, and the pod's report then differs from the new choice (Q8).

### 4.6 In a ClusterDeploymentConfig

A `ClusterDeploymentConfig` carries the network the way it carries every other cluster setting. `spec.cluster.network` is the same block as `spec.storageNodes.network`, and is copied onto the cluster the document creates, which is the only moment it can be set (§4.4). The document names no node address: the operator chooses each from the network's pool, or reads it from the NIC, when the node is added.

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
  nodeSets:
    - name: rack-a
      groups:
        - name: group-1
          workers: [worker-1, worker-2]
```

A document that states no network expands exactly as it always did, and discovery never proposes one: which definition serves the storage network is the administrator's decision. A group on a network names no `mgmtInterface` or `dataInterfaces`, and the schema refuses the combination.

---

## 5. Attaching a Node

### 5.1 The attachment list and the annotation

The operator passes the node's attachments in the node-add call as a list: the definition's name, an interface name, and the node's address. Interface names are explicit so none depends on Multus's ordering: `mgmt0` for the management network and `data0` onward for data networks. The control plane keeps the list with the node record and passes it to the agent at every start, and the agent renders it into the SPDK pod's `k8s.v1.cni.cncf.io/networks` annotation. Each element carries `ips` with a prefix length. No element sets `default-route`, so the pod's default route stays on `eth0`.

### 5.2 The pods that attach

| Pod                   | With a declared network                                           | Otherwise | Why                                                                                                                                                                                              |
|-----------------------|-------------------------------------------------------------------|-----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| SPDK                  | `mgmt0` if declared, and `data0` onward                           | Host      | It serves RPC on the management address and listeners on the data ones                                                                                                                           |
| Node agent            | Pod network, plus `mgmt0` when the control plane is external (Q1) | Host      | The control plane reaches it, and it reads no host interface once the SPDK pod reports                                                                                                           |
| Control plane pods    | Pod network, plus the management network (Q1)                     | Host      | They dial the SPDK pod's management address and the agent                                                                                                                                        |
| Operator-created jobs | Pod network                                                       | Host      | A job with no storage traffic and no host interface read needs no host network                                                                                                                   |
| CSI node plugin       | Host                                                              | Host      | The kernel NVMe/TCP initiator opens its connections from the host's initial network namespace. The operator asks it to read a NIC's address and to test a connection to the SPDK listener (§8.1) |
| mds-proxy             | Host                                                              | Host      | SPDK refuses a pod's connection to its own node                                                                                                                                                  |

### 5.3 Requirements on the definition

A definition that serves a storage node meets these requirements, and the operator checks the ones it can observe.

- **`host-device`, `macvlan`, or `ipvlan`.** `host-device` moves the NIC into the pod and names it by `device` (an interface name) or by `pciBusID` (a PCI address). `macvlan` and `ipvlan` create an interface in the pod on the NIC named by `master`, which stays on the host.
- **`static` IPAM, and the `ips` capability declared.** The pod takes the address from the network selection element's `ips`. An IPAM plugin that allocates at attach time returns an address nothing recorded, and the verification reports it as a mismatch (§5.4).

```json
{
  "cniVersion": "0.3.1",
  "name": "storage-data",
  "type": "host-device",
  "device": "ens4",
  "capabilities": { "ips": true },
  "ipam": { "type": "static" }
}
```

- **A NIC that keeps its address,** when the network declares no pool. The pod takes the address the NIC holds, so a leased address needs a reservation for that NIC, because the host's client no longer renews it (§4.3).
- **The definition owns the routes.** A route to the initiators' subnets goes in the static IPAM block (§7). `default-route` is never set on a storage network, because Multus would replace the pod's default route and break its reach to the cluster.
- **Two interfaces of a pod do not share a subnet,** because that causes ARP flux and reverse-path filtering drops. A definition that must share one adds a `tuning` plugin to set `arp_ignore` and `arp_announce`.
- **One NIC per pod for `host-device`.** The plugin moves the whole interface, so a worker that runs several SPDK pods needs a NIC and a definition for each. `macvlan` and `ipvlan` leave the parent on the host, so several pods share it, and a pool gives each its own address.
- **The `device` or `master` is one name for the whole cluster.** A fleet with different NIC names needs a definition per name, or a PCI address when the slots agree (Q3).
- **A device a device plugin hands out.** A definition annotated `k8s.v1.cni.cncf.io/resourceName: <vendor>/<pool>` takes its device from a device plugin at attach time: the SR-IOV Network Device Plugin's VF pools, with the `sriov` CNI, or with `host-device` and the `deviceID` capability, which takes the PCI address Multus passes in. The preflight reads the annotation with the definition and records the resource on the node's status beside its address; node-add sends it with the network as the `resource` field of its element, and the SPDK container requests one of it in both requests and limits, as Kubernetes requires of an extended resource; the QoS class is unchanged. Without the request the plugin allocates nothing and Multus has no device to attach. The definition names no device, so the node's address comes from an `addressPool`: there is no NIC to adopt an address from before the device is allocated, and the node is held until a pool is declared.

```json
{
  "cniVersion": "0.3.1",
  "name": "storage-vf",
  "type": "sriov",
  "capabilities": { "ips": true },
  "ipam": { "type": "static" }
}
```

### 5.4 Verification in two stages

The SPDK pod does not exist when the operator decides whether to add a node, so only the first stage can gate the add.

**Before node-add**, the operator settles the node's address and checks what the definition and the worker can show without attaching anything (§8.1): that the definition exists, and that the node has an address: drawn from the pool, or read from an interface that is on the worker with a usable address. A failed check holds the node in `Pending` and the control plane is not called.

**After the SPDK pod is `Running`**, the control plane and the operator read the pod's `network-status`, matching the pod by its RPC-port label, and compare each interface and address with the declaration. A difference records `Mismatched`, emits `NetworkAddressMismatch`, and takes the node out of service through the control plane's shutdown, because its listeners were created on an address nothing can be assumed to answer. The node is not restarted until the declaration or the definition is corrected. A match leads to the reachability gate (§8.1): the CSI node plugins connect to the SPDK listener at the node's data address, and the network is recorded `Attached` only when every connection succeeds.

`network-status` is written once, when Multus finishes the attach. It is evidence of the attach and says nothing about the interface's later state, which the SPDK proxy reports (§6).

### 5.5 RBAC

The operator needs `get` and `list` on `network-attachment-definitions` in `k8s.cni.cncf.io` and on `pods`. It creates no pod for the address read or the gate, which run in the CSI node plugins over the CSI link (§8.1).

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

`bind_to_device` needs no change for an attachment, because the interface name only has to exist in SPDK's own namespace. On the pod network it is not set: the Service's traffic is delivered on the pod's interface by the CNI, and a socket pinned to a device refuses it. sbcli's `IFace` carries the two addresses apart, `ip4_address` for what peers and clients connect to and `listen_address` for what the target binds, and the two differ only on the pod network.

When no management network is declared, SPDK's RPC stays on the pod network. The control plane has to dial a name that follows the pod across restarts, which is the per-port DNS name, because a pod address changes when the pod is recreated (Q6).

---

## 7. Reachability

Three host-network processes reach an SPDK pod's attached addresses: the kernel NVMe/TCP initiator behind every volume attach, a control plane pod dialing the management address, and the mds-proxy.

A `host-device` pod holds the whole NIC, so no host-side interface can sit beside it, and a host never reaches the pod's address on its own. The packet leaves another NIC of the worker and takes the fabric to the data NIC. That holds on another worker and on the pod's own worker: from the pod's own worker the packet leaves one NIC and returns through the fabric to the data NIC. A cloud network delivers it, and a switched fabric delivers it when the worker's subnet routes to the data subnet.

A `macvlan` or `ipvlan` pod leaves the parent NIC on the host, and the kernel does not deliver a packet from the parent to a child interface on the same parent: the host cannot reach the pod's address through the parent itself. A worker that attaches volumes through the parent needs a host-side sibling interface of the same kind on the parent, with an address on the data subnet, or a route through the fabric. The sibling is the administrator's, and the reachability gate reports its absence (§8.1).

| Initiator                                                              | Path                        | Requirement                                                                                                          |
|------------------------------------------------------------------------|-----------------------------|----------------------------------------------------------------------------------------------------------------------|
| A worker whose host holds a NIC on the data subnet                     | On-link                     | None                                                                                                                 |
| A worker without one, or whose data NIC is in a `host-device` SPDK pod | Routed through the fabric   | A route to the initiators' subnets in the definition's IPAM block, and a fabric that routes between them             |
| A worker with the parent of a `macvlan` or `ipvlan` pod                | Through a host-side sibling | A sibling interface of the same kind on the parent with an address on the data subnet, or a route through the fabric |
| `hostInterface` attachment                                             | Direct                      | None. The host is the pod's namespace                                                                                |

**Return path.** An initiator on another subnet than the pod's data interface sends its request through the fabric to the data address, and the pod answers by its own routes. The pod's default route is `eth0`, so without a route for the initiator's subnet the reply leaves the pod network with the data interface's address as its source, which the fabric does not deliver, and a strict reverse-path filter on the data interface drops the request before it is answered. The definition owns the routes (§5.3): a route to the initiators' subnets through the fabric's gateway, in the `static` IPAM block, gives the pod a return path. The operator neither writes nor checks it, and the reachability gate (§8.1) reports a missing one as `DataNetworkHostUnreachable`. A route must not cover the pod network, because it would also send the replies to pod addresses out of the data interface, which matters wherever pods take addresses from the workers' own subnets. An initiator on the data interface's own subnet replies on-link and needs none of this.

An initiator cannot run in a pod namespace to avoid this, because a kernel NVMe/TCP controller can reconnect from the host's initial namespace and its connection would then move or fail.

**Clients outside the cluster.** `clientDataIfname` is unchanged.

**Several data networks.** A volume's initiator opens one path per data network, so each worker that attaches volumes needs a route to every declared data subnet.

---

## 8. Preflight, Failure Modes, and Operations

### 8.1 Preflight

Before a node is added, the operator settles its address and checks the definition and the worker without attaching anything. The checks run in the provisioning step `CheckingNetwork`, which sits between the configuration gate and the queue for a node-add slot, so a node holds no slot while it waits on a definition. A cluster that attaches no network through a definition skips the step.

The step reads each definition, which has to exist. A network that declares a pool takes the first free address of it (§4.3) and reads nothing from the worker. A network with no pool asks the CSI node plugin of the node's worker for the addresses of the interface the definition names, and adopts the first IPv4 address. The plugin runs on the host network, so it sees the NIC before the SPDK pod takes it. The operator issues the call down the connection the plugin holds to it (`csilink`), the way it asks the plugins to forget NFS flows. A node that has to read a NIC and has a worker whose plugin is not linked, or does not advertise the host check, is held with `NetworkHostCheckUnavailable`, and the step retries. Nothing in the step attaches a network, so no pod holds the NIC before the control plane starts the SPDK pod.

Once the control plane reports the node online and the SPDK pod's `network-status` matches the declaration (§5.4), the reachability gate runs. The operator asks the CSI node plugin of each worker that has one linked to open a TCP connection to the SPDK listener at the node's data address, on the port the control plane reports for it, and to answer whether it connected. A network is recorded `Attached`, and `NetworkAttached` is emitted, when every answer is yes. An answer of no, or one that does not arrive within the call's deadline of ten seconds, leaves the network `Unavailable` and emits `DataNetworkHostUnreachable`, naming the worker and pointing at the routes of the definition, the security groups of the fabric, and, for `macvlan` or `ipvlan`, the host-side sibling interface on the parent. The node is not reported `Online`. A worker with no linked plugin has no initiator to test and is skipped. When no plugin is linked on any worker, the node waits with `NetworkHostCheckUnavailable`. The gate repeats on the next reconcile until it passes. It runs again on every restart of the node and when a plugin links on a worker that had none, and between runs the host-reachable gauge (§11) holds the last result.

### 8.2 Failure modes

| Failure                                                                                                      | Detection                                              | Behavior                                                                                                                                           |
|--------------------------------------------------------------------------------------------------------------|--------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| The cluster has no `NetworkAttachmentDefinition` kind                                                        | Discovery on the kind                                  | The node stays `Pending`. `NetworkAttachmentUnsupported` on the node                                                                               |
| A referenced definition does not exist                                                                       | Get on the reference                                   | The node stays `Pending`. `NetworkAttachmentNotFound` on the node                                                                                  |
| A pool has no address left                                                                                   | Every address of the range is held by another node     | The node stays `Pending`. `NetworkAddressPoolExhausted` on the node, naming the network and the subnet                                             |
| A network with no pool: the CSI node plugin of the worker is not linked or does not advertise the host check | The CSI link                                           | The node stays `Pending` and the step retries. `NetworkHostCheckUnavailable` on the node                                                           |
| A network with no pool: the interface a definition names is not on the worker, or the definition names none  | The plugin's answer                                    | The node stays `Pending`. `NetworkAttachmentFailed` on the node, naming the interface                                                              |
| A network with no pool: the interface has no usable address                                                  | The plugin's answer                                    | The node stays `Pending`. `NetworkInterfaceNoAddress` on the node, naming the interface                                                            |
| The definition's plugin is not installed on the worker, or the Multus daemon is down                         | The SPDK pod stays in `ContainerCreating` for a minute | The node stays `Pending`. `NetworkAttachmentFailed` on the node, naming the pod. Every new pod on that worker is affected when Multus is the cause |
| A started SPDK pod's address differs from the declared one                                                   | `network-status` of the SPDK pod after it is `Running` | Phase `Mismatched`. The node is taken out of service. `NetworkAddressMismatch` on the node                                                         |
| A worker cannot reach the node's listener at its data address                                                | The reachability gate                                  | The network is `Unavailable` and the node is not reported `Online`. `DataNetworkHostUnreachable` on the node, naming the worker                    |
| An attached SPDK pod loses its interface                                                                     | The SPDK proxy's link report, read by the health check | Phase `Unavailable`. `NetworkAttachmentLost` on the node                                                                                           |

A pod whose attach fails never starts, because kubelet keeps retrying the sandbox, so the detectable failure is a pod that has not started after a minute. The operator names the pod and does not read its events, because that read needs access to every event in the cluster, so the administrator reads the cause with `kubectl describe`. A missing definition leaves the pod in `ContainerCreating` with a `NoNetworkFound` event from Multus.

No failure falls back to the host network, because a node listening on a different network than declared would be unreachable by the initiators that were told otherwise.

### 8.3 Operations

| Situation                                 | What the administrator does                                                                                                          |
|-------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------|
| A node is held in `Pending` for a network | Read the event on the `StorageNode`, fix the definition or the worker, and the next reconcile retries the checks                     |
| A definition is edited                    | The edit applies when the SPDK pod next starts, because Multus reads the definition at attach. A restart of the node applies it      |
| A node's address has to change            | Remove the node and add it again (§4.4)                                                                                              |
| A node has to return to the host network  | Remove the node and add it again in a cluster whose networks are `hostInterface`. `network` is fixed at the cluster's creation       |
| A worker is replaced                      | Migrate the node. The new worker needs the `host-device` plugin, the NIC the definition names, and a linked CSI node plugin          |
| Multus is upgraded or its daemon restarts | Every pod that starts during the outage fails its attach and retries. Schedule the upgrade where no node is being added or restarted |

---

## 9. Portability Across CNIs

The attachment path uses two standard objects, the `NetworkAttachmentDefinition` and the `network-status` annotation, and neither belongs to a primary CNI. No Cilium CRD, Calico IP pool, or Flannel option is read or written, and the address is verified from `network-status`, not from `ip addr` or the declaration. The only per-CNI work is keeping Multus's configuration in place:

| Platform                  | Behavior to configure for                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
|---------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Cilium as the primary CNI | `cni.exclusive` is true by default, and Cilium then takes ownership of the CNI configuration directory. Set it to false. Set `devices` explicitly so the eBPF datapath does not attach to a storage NIC. The reference plugins are not shipped                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| K3s                       | The CNI configuration directory is `/var/lib/rancher/k3s/agent/etc/cni/net.d` and the plugin directory is `/var/lib/rancher/k3s/data/cni`. Only `bridge` and `host-local` are bundled, so `host-device` and `static` are installed. Multus needs `--multus-kubeconfig-file-host` set to the host path of its kubeconfig, or every pod start on the node fails with `stat /etc/cni/net.d/multus.d/multus.kubeconfig: no such file or directory`                                                                                                                                                                                                                                              |
| OpenShift                 | Multus runs with namespace isolation on, so a definition has to be in the pod's namespace or a global one (§4.1).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| Any distribution          | The Multus configuration must sort ahead of the primary CNI's, and with the thick plugin its daemon is a dependency of every pod start (§8.2)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| Amazon EKS                | AWS documents Multus with the VPC CNI as the primary CNI. The VPC CNI manages every ENI it finds on a node, so the data ENI carries the tag `node.k8s.amazonaws.com/no_manage: true`. With Cilium in ENI mode, the exclude-tag setting (`eni-exclude-interface-tags`, Cilium 1.19 or later) keeps the data ENI out of pod addressing, and `devices` excludes it from the datapath. The VPC delivers only the addresses assigned to an ENI: the adopted address is one, and a pooled address has to be assigned to the node's data ENI as a secondary address. AWS documents `ipvlan` for this, and `macvlan` is unverified because its interfaces carry MAC addresses the VPC does not know |

---

## 10. Backend API Requirements

| Method | Endpoint                                                            | Notes                                                                                                                                                                                                                                                                  |
|--------|---------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `/api/v2/clusters/{id}/storage-nodes` (node-add)                    | Exists. P0-1 adds `networks`, a list of `name`, `namespace`, `interface`, and `ips`, and records it on the node. P0-2 takes `data_nics` and the management address from the SPDK pod's response                                                                        |
| POST   | `/api/v2/clusters/{id}/storage-nodes/{id}/{action}` (restart)       | Exists. P0-1 and P0-2: it passes the recorded attachments again and reads the pod's interfaces on every restart                                                                                                                                                        |
| POST   | Node agent `/spdk_process_start`                                    | Exists. P0-1 renders the attachments as the annotation and drops `hostNetwork`. P0-2 returns the pod's `network-status` once the pod is `Running`                                                                                                                      |
| GET    | Node agent `/info`, `/ifc_is_tcp`, `/ifc_is_roce`                   | Exist, unchanged. Not called for a node on a network                                                                                                                                                                                                                   |
| GET    | SPDK proxy link report and ping                                     | New (P0-2). The ping is sent out of a named interface in the SPDK pod's namespace. The health check uses it for an attached data interface                                                                                                                             |
| RPC    | CSI link `netcheck` service, from the operator to a CSI node plugin | New. `InterfaceAddresses` returns a host interface's addresses by name or PCI address, and `Connect` opens a TCP connection to an address and port and reports the outcome. A plugin advertises the capability, and one that does not is treated as unavailable (§8.1) |

P0-1 and P0-2 are the design's external blockers, and P0-2 reaches the health check and the restart flow as well as node-add. Node-add and restart must be idempotent on their attachment parameters, because the reconciler retries them.

---

## 11. Observability

Per-interface traffic already exists: kubelet and cAdvisor report `container_network_*` per interface for a pod with its own network namespace. A host-network pod reports the host's interfaces with no mapping to the pod. Moving the SPDK pod onto an attached network is what makes `interface="data0"` a statement about that pod.

### Kubernetes Events

Events land on the `StorageNode`, which the administrator owns and which outlives the pods.

| Event                                                                                              | Type    | Reason                         |
|----------------------------------------------------------------------------------------------------|---------|--------------------------------|
| The cluster has no `NetworkAttachmentDefinition` kind, so the node is held                         | Warning | `NetworkAttachmentUnsupported` |
| A referenced definition does not exist, so the node is held                                        | Warning | `NetworkAttachmentNotFound`    |
| A network's pool has no address left, so the node is held                                          | Warning | `NetworkAddressPoolExhausted`  |
| No CSI node plugin of the worker can read the interface or test a connection, so the node waits    | Warning | `NetworkHostCheckUnavailable`  |
| A pod could not attach or the worker lacks the interface, so the node is held                      | Warning | `NetworkAttachmentFailed`      |
| A pod's address differs from the declared one, so the node is held or taken out of service         | Warning | `NetworkAddressMismatch`       |
| The interface a definition moves has no usable address, so the node is held                        | Warning | `NetworkInterfaceNoAddress`    |
| A worker cannot reach the SPDK listener at the data address, so volumes cannot be attached from it | Warning | `DataNetworkHostUnreachable`   |
| An attached SPDK pod lost its interface                                                            | Warning | `NetworkAttachmentLost`        |
| Every declared network is attached and verified                                                    | Normal  | `NetworkAttached`              |

### Prometheus Metrics

| Metric                                                  | Labels                       | Description                                                                                                   |
|---------------------------------------------------------|------------------------------|---------------------------------------------------------------------------------------------------------------|
| `simplyblock_storagenode_network_attached_state`        | `cluster`, `node`, `network` | 1 while the network is attached and verified, 0 otherwise                                                     |
| `simplyblock_storagenode_network_host_reachable_state`  | `cluster`, `node`, `network` | 1 while every worker that runs an initiator reaches the node's SPDK listener at its data address, 0 otherwise |
| `simplyblock_storagenode_network_attach_failures_total` | `cluster`, `reason`          | Attach attempts that ended in each reason of the table above                                                  |
| `simplyblock_storagenode_network_preflight_seconds`     | `cluster`                    | Histogram of the time a node spends in the `CheckingNetwork` step                                             |

The attached gauge reading 0 for a node whose pods are running alerts on a lost interface. The host-reachable gauge reading 0 alerts on a volume attach that will fail. The failures counter separates a cluster missing Multus from a mistyped definition name, which look identical from the node's phase.

---

## 12. Testing Strategy

The scenario matrix is in [`test-plan-storage-network.md`](../tests/test-plan-storage-network.md). The sbcli changes of P0-1 and P0-2 are tested in sbcli, and this repository tests the operator's behavior at that boundary, faked.

The risk concentrates where no unit test reaches: the address read and the reachability gate over the CSI link, the return path of a data interface whose initiators are on other subnets and the host-side sibling of a `macvlan` or `ipvlan` parent (§7), an adopted address on a cloud network that routes only the addresses assigned to an interface, and the CNI portability claim (§9). A live run is required on each primary CNI against `host-device`, and against `macvlan` and `ipvlan` with a pool: Flannel on K3s, Cilium, and Calico, plus OpenShift and Talos.

---

## 13. Migration

The legacy fields and the network group are mutually exclusive, and `network` can only be present from the cluster's creation, so no existing cluster changes mode in place. A cluster that states no network is unaffected by every section above: an upgrade leaves its pods on the host network, and its restarts render the same pod as before.

A node already added keeps the networks the control plane recorded at node-add, and a node's `status.network` is empty exactly when it is on the host network. Moving a node between networks is a remove and a re-add, or a node migration (Q3).

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Owner                   |
|-----|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|
| Q1  | How the agent and the control plane's pods are addressed off the host network. The agent is one per worker and serves several SPDK pods, so its address is per worker and cannot come from a node's `spec.config.network`. A control plane inside the cluster can reach it at its pod address, and one outside needs an address routed on the management network. Each control plane replica dials the SPDK pod's management address, so each needs a stated management address | Operator, control plane |
| Q2  | Whether the node-preparation jobs can keep the host network, or any of them reads a network the node's pods now leave                                                                                                                                                                                                                                                                                                                                                           | Control plane (`sbcli`) |
| Q3  | How a node moves between networks without removal, and how a fleet with different NIC names per worker selects among definitions                                                                                                                                                                                                                                                                                                                                                | Operator                |
| Q6  | How the control plane dials SPDK's RPC when no management network is declared and TLS is off. It dials `mgmt_ip` directly today and the per-port DNS name only under TLS                                                                                                                                                                                                                                                                                                        | Control plane (`sbcli`) |
| Q7  | IPv6 and dual-stack data networks                                                                                                                                                                                                                                                                                                                                                                                                                                               | Operator                |
| Q8  | Whether a `StorageNode` recreated after its status was lost can recover the address its SPDK pod still holds. The assigned address is kept in status only, so a recreated object assigns again, and verification then records the pod's address as a mismatch                                                                                                                                                                                                                   | Operator                |
| Q9  | Whether any released version of the API carries `spec.storageNodes.network[].addressPool.exclude` or `status.network[].rejected`. If one does, removing them needs a deprecation path, and if none does they are deleted outright                                                                                                                                                                                                                                               | Operator                |
| Q10 | Whether the gate tests with a plain TCP connection or with an NVMe/TCP discovery against the listener, which also proves the target answers the protocol                                                                                                                                                                                                                                                                                                                        | Operator, CSI driver    |
