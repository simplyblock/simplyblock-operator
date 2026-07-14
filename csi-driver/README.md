# Simplyblock CSI Driver

**High-performance NVMe/TCP (NVMe-over-Fabrics) CSI driver for Kubernetes**

![](../assets/simplyblock-logo.svg)

> Part of the [simplyblock-operator](../README.md) monorepo. For the repository overview, license,
> and contribution guidelines, see the [root README](../README.md).

---

## 🚀 Overview

The **simplyblock CSI driver** is the official simplyblock storage plugin for Kubernetes. It leverages
SPDK and NVMe-over-TCP to deliver **high-performance, ultra-low-latency block storage** directly inside
Kubernetes, without specialized hardware or vendor lock-in. It supports dynamic provisioning, snapshots,
and seamless integration via the Container Storage Interface (CSI).

Where the [operator](../operator/README.md) manages the lifecycle of the storage platform, the CSI
driver provisions and attaches volumes to workloads, enabling **software-defined storage (SDS)** with
features like:

- ⚡ **Ultra-low latency**: Unlock performance with NVMe-over-TCP
- 🛡️ **Enterprise data services**: Snapshots, clones, erasure coding, multi-tenancy
- 🔒 **Secure & robust**: Cluster authentication and Quality of Service (QoS)
- ☁️ **Cloud & on-prem flexibility**: Deploy into any Kubernetes-based distribution

👉 For full documentation, see the [Simplyblock Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/).

---

## ✨ Features

| Feature                           | Benefit                                                                 |
|----------------------------------|-------------------------------------------------------------------------|
| **Dynamic Volume Provisioning**   | Dynamically provision and lifecycle-manage persistent volumes in Kubernetes |
| **NVMe/TCP Support**              | High throughput, low latency storage over standard Ethernet              |
| **Snapshots & Clones**           | Efficient data protection and instant provisioning                      |
| **Erasure Coding**                | Fault-tolerant, space-efficient redundancy                             |
| **Multi-tenancy & QoS**          | Isolated tenants with guaranteed IOPS, bandwidth, and latency           |
| **Resilient Networking**         | Supports redundant or isolated storage/control networks                  |
| **Auto-Reconnect**               | Automatic reconnection of NVMe devices after network or host failures   |

---

## 📦 Getting Started

This section covers installing the CSI driver on its own. To install a full simplyblock storage cluster
into Kubernetes, refer to the [Simplyblock Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/).

### 1. Prerequisites

Ensure the kernel NVMe/TCP driver is loaded on every node:

```bash
modprobe nvme-tcp
lsmod | grep 'nvme_'
```

You should see `nvme_tcp`, `nvme_fabrics`, and related modules. To make it persistent, add the module
to `/etc/modules-load.d/nvme-tcp.conf` or `/etc/modules`, depending on your distro.

### 2. Install via Helm (Recommended)

```bash
helm repo add simplyblock-csi https://install.simplyblock.io/helm
helm repo update

export CLUSTER_UUID="<CLUSTER_ID>"
export CLUSTER_SECRET="<CLUSTER_SECRET>"
export CNTR_ADDR="<CONTROL_PLANE_ADDR>"
export POOL_NAME="<STORAGE_POOL_NAME>"

helm install -n simplyblock-csi --create-namespace simplyblock-csi simplyblock-csi/spdk-csi \
  --set csiConfig.simplybk.uuid=${CLUSTER_UUID} \
  --set csiConfig.simplybk.ip=${CNTR_ADDR} \
  --set csiSecret.simplybk.secret=${CLUSTER_SECRET} \
  --set logicalVolume.pool_name=${POOL_NAME}
```

Verify the deployment:

```bash
kubectl -n simplyblock-csi get pods -l release=simplyblock-csi
```

You should see controller and node pods in the `Running` state.

### Deployment topologies

Simplyblock supports different Kubernetes deployment topologies:

* **Disaggregated Setup** — storage nodes on dedicated worker nodes or separate clusters; the CSI driver
  connects client clusters to storage.
  📖 [Docs](https://docs.simplyblock.io/latest/deployments/kubernetes/k8s-disaggregated/)
* **Hyper-Converged Setup** — the CSI driver runs alongside storage and workloads in the same cluster,
  enabling local storage affinity and a simplified topology.
  📖 [Docs](https://docs.simplyblock.io/latest/deployments/kubernetes/k8s-hyperconverged/)

---

## 🖥️ Usage

Once deployed, manage persistent volumes using standard Kubernetes objects: **StorageClasses**,
**PersistentVolumeClaims**, **Volume Snapshots**, clones, resizing, and QoS policies.

See full examples in the [Usage Guide](https://docs.simplyblock.io/latest/usage/simplyblock-csi/).

---

## 🛠️ Troubleshooting

```bash
kubectl -n simplyblock-csi get pods            # verify CSI pods are running
kubectl -n simplyblock-csi logs <pod-name>     # inspect a node or controller pod
```

For advanced troubleshooting, monitoring, and maintenance guidance, see the
[Operations Docs](https://docs.simplyblock.io/latest/usage/simplyblock-csi/).

---

## 📚 Documentation

* [Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/)
* [Install CSI Driver Only](https://docs.simplyblock.io/latest/deployments/kubernetes/install-csi/)
* [Disaggregated Setup](https://docs.simplyblock.io/latest/deployments/kubernetes/k8s-disaggregated/)
* [Hyper-Converged Setup](https://docs.simplyblock.io/latest/deployments/kubernetes/k8s-hyperconverged/)

For license, contribution guidelines, and support, see the [root README](../README.md).