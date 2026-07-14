# Simplyblock for Kubernetes

**Everything you need to run simplyblock — high-performance NVMe/TCP block storage — natively on Kubernetes**

[![Documentation](https://img.shields.io/badge/Docs-simplyblock-blue)](https://docs.simplyblock.io/latest/deployments/kubernetes/) [![License](https://img.shields.io/badge/License-Apache%202.0-green.svg)](LICENSE) [![Issues](https://img.shields.io/github/issues/simplyblock/simplyblock-operator)](https://github.com/simplyblock/simplyblock-operator/issues)

![](assets/simplyblock-logo.svg)

### 📖 Read the full documentation at **[docs.simplyblock.io](https://docs.simplyblock.io)**

---

## 🚀 Overview

This repository is the home of simplyblock's Kubernetes integration. It bundles the operator, the
CSI driver, the shared node-level storage library, and the Helm charts that deploy them — so the
whole stack is developed, versioned, built, and released together from a single source tree.

**Simplyblock** delivers enterprise-grade, software-defined block storage to Kubernetes over
**NVMe-over-TCP**: ultra-low latency, snapshots, clones, erasure coding, multi-tenancy, and QoS —
without specialized hardware or vendor lock-in.

👉 For the supported, end-to-end installation flow, see the
[Simplyblock Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/).

---

## 🧱 Components

| Component | Path | Description |
|-----------|------|-------------|
| **Operator** | [`operator/`](operator/README.md) | Kubernetes operator for declarative lifecycle management of simplyblock storage clusters, nodes, pools, backups, and replication via Custom Resources. |
| **CSI Driver** | [`csi-driver/`](csi-driver/README.md) | Container Storage Interface driver that provisions and attaches NVMe/TCP volumes to workloads (dynamic provisioning, snapshots, clones, QoS). |
| **Atlas Library** | [`atlas-lib/`](atlas-lib/README.md) | Shared Go library holding the node-level storage primitives (NVMe discovery, NVMe-oF fabric management, lvol↔device mapping) that both the operator and CSI driver depend on. |
| **Helm Charts** | [`helm-charts/`](helm-charts/README.md) | Official Helm charts that deploy the operator, CSI driver, and supporting components onto Kubernetes. |

Each component has its own README with focused documentation.

---

## 📦 Installation

Simplyblock is installed as a whole via the **official Helm charts**, following the documentation.
The guide wires up the control plane, the operator, cert-manager, and the CSI driver together:

👉 **[Simplyblock Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/)**

For component-specific installation and development notes, see each component's README linked above.

---

## 🛠️ Building

The root [`Makefile`](Makefile) orchestrates builds and tests across every component, delegating to
each component's own Makefile:

```sh
make build   # Build every component (atlas, csi, operator) and sync CRDs into the Helm chart
make test    # Test every component
make lint    # Lint every component
make help    # List all available targets
```

Individual components can be built directly, e.g. `make operator-build` or `make csi-test`. See
`make help` for the full list.

---

## 📄 License

This project is licensed under the **Apache 2.0 License** — see the [LICENSE](LICENSE) file for details.

---

## 🤝 Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on reporting
issues, submitting pull requests, and coding conventions.

---

## 📚 Documentation

* [Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/)
* [Operator Architecture Overview](operator/ARCHITECTURE.md)
* [Simplyblock Documentation](https://docs.simplyblock.io)

---

## 📬 Support

* 📖 [Documentation](https://docs.simplyblock.io/latest/deployments/kubernetes/)
* 🐞 [GitHub Issues](https://github.com/simplyblock/simplyblock-operator/issues)
* 🌐 [Simplyblock Website](https://www.simplyblock.io)

Maintained by the **simplyblock team**.

---

**Manage NVMe-grade simplyblock storage the Kubernetes-native way.**