# Simplyblock Operator for Kubernetes

**Kubernetes Operator for declarative management of simplyblock storage clusters**

[![Documentation](https://img.shields.io/badge/Docs-simplyblock-blue)](https://docs.simplyblock.io/latest/deployments/kubernetes/) [![Issues](https://img.shields.io/github/issues/simplyblock/simplyblock-operator)](https://github.com/simplyblock/simplyblock-operator/issues)

![](assets/simplyblock-logo.svg)

### 📖 Read the full documentation at **[docs.simplyblock.io](https://docs.simplyblock.io)**

---

## 🚀 Overview

`simplyblock-operator` is the official **Kubernetes operator for simplyblock storage**. It turns the
simplyblock control plane into a set of native Kubernetes Custom Resources, so you can provision and
operate **high-performance NVMe/TCP block storage** the same way you manage any other Kubernetes object.

The operator watches simplyblock CRs in its namespace and reconciles the desired state into actual
storage-system state by calling the simplyblock Web API. Observed results are written back into each
CR's status, giving you a declarative, GitOps-friendly, status-driven view of your storage estate.

Where the [simplyblock CSI driver](https://github.com/simplyblock/simplyblock-csi) provisions and
attaches volumes to workloads, the operator manages the **lifecycle of the storage platform itself** —
clusters, nodes, pools, backups, restores, and replication.

👉 For full documentation, see the [Simplyblock Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/).

---

## ✨ Features

| Feature                        | Benefit                                                                        |
|--------------------------------|--------------------------------------------------------------------------------|
| **Declarative Storage Clusters** | Create, activate, expand, and lifecycle-manage clusters via Kubernetes CRs     |
| **Storage Node Management**    | Reconciles the storage-node DaemonSet, node labels, and per-namespace RBAC      |
| **Pool Provisioning**          | Manages storage pools, QoS, dhchap security, and host allow-lists              |
| **Backup, Restore & Import**   | First-class backups with cross-cluster import and policy-driven retention       |
| **Snapshot Replication**       | Replicates snapshots between clusters/pools, including failback support         |
| **Drain-Aware Node Lifecycle** | Coordinates storage-node shutdown/restart with Kubernetes drains via PDBs        |
| **Standard Kubernetes RBAC**   | Authorization delegated entirely to native K8s RBAC (no custom identity model)   |
| **mTLS to the Control Plane**  | Optional cert-manager-issued mTLS between the operator and the Web API           |

---

## 🧩 Custom Resources

The operator manages the following `storage.simplyblock.io/v1alpha1` resources:

| Kind                  | Purpose                                                                 |
|-----------------------|-------------------------------------------------------------------------|
| `ControlPlane`        | Singleton gating the system on control-plane readiness                  |
| `StorageCluster`      | Create/activate/expand clusters and provision per-pool StorageClasses   |
| `StorageNodeSet`      | Manage storage nodes and the storage-node DaemonSet                     |
| `Pool`                | Create storage pools with QoS, dhchap security, and host affinity       |
| `Task`                | Observe long-running cluster tasks                                      |
| `StorageBackup`       | Snapshot a PVC's backing volume and create a cluster backup             |
| `BackupRestore`       | Restore a backup into a cluster/pool/node                               |
| `BackupImport`        | Import a backup from a source cluster's backend into a target cluster   |
| `BackupPolicy`        | Define retention (`maxVersions`, `maxAge`) attached to PVCs             |
| `SnapshotReplication` | Replicate snapshots between clusters/pools                              |
| `VolumeMigration`     | Migrate volumes between clusters                                        |

See [ARCHITECTURE.md](ARCHITECTURE.md) for a detailed component and reconciliation overview.

---

## 📦 Getting Started

### 1. Prerequisites

- Go v1.24.6+ (for building from source)
- Docker v17.03+
- kubectl v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster
- A reachable simplyblock control plane (Web API)

### 2. Install via bundle (Recommended)

Apply the pre-built installer bundle, which contains all CRDs, RBAC, and the manager deployment:

```sh
kubectl apply -f https://raw.githubusercontent.com/simplyblock/simplyblock-operator/main/dist/install.yaml
```

### 3. Verify the deployment

```sh
kubectl -n simplyblock-operator get pods
```

You should see the manager pod in the `Running` state.

### 4. Create your first resources

Apply the bundled samples to try it out:

```sh
kubectl apply -k config/samples/
```

To remove them again:

```sh
kubectl delete -k config/samples/
```

---

## 🛠️ Build & Deploy from Source

```sh
# Build and push the operator image
make docker-build docker-push IMG=<some-registry>/simplyblock-operator:tag

# Install CRDs and deploy the manager with your image
make install
make deploy IMG=<some-registry>/simplyblock-operator:tag

# Regenerate the install bundle (dist/install.yaml) or a Helm chart
make build-installer IMG=<some-registry>/simplyblock-operator:tag
kubebuilder edit --plugins=helm/v2-alpha        # optional: generate dist/chart

# Tear everything down
make undeploy      # remove the controller
make uninstall     # remove the CRDs
```

> **NOTE:** The pushed image must be reachable from the cluster — ensure the registry is accessible and
> that you have pull permissions. If you hit RBAC errors during `make deploy`, you may need cluster-admin
> privileges. Run `make help` for the full list of targets.

---

## 🔐 Access Control (RBAC)

The operator delegates user authorisation entirely to standard Kubernetes RBAC.
It does not ship per-CR `admin`/`editor`/`viewer` ClusterRoles or any
identity-bearing fields on its CRs; cluster admins write `Role`s,
`RoleBinding`s and `ClusterRoleBinding`s using the normal K8s primitives.

### Tenancy model: namespace-per-cluster

Each `StorageCluster` is namespace-scoped, and **a `Pool` must live in the same
namespace as the `StorageCluster` it references via `spec.clusterName`**. The
Pool controller enforces this: if a Pool references a `StorageCluster` that
does not exist in the Pool's namespace, the controller refuses to call the
backend, sets `status.status = "InvalidClusterReference"`, and emits a
`InvalidClusterReference` Event on the Pool.

This converts "admin of cluster `foo`" into "admin of the namespace where
StorageCluster `foo` lives" — a problem standard K8s RBAC already solves
cleanly. The recommended layout is one namespace per logical storage cluster
(e.g. `cluster-prod`, `cluster-staging`).

### Aggregation into the built-in `view`/`edit`/`admin` roles

The operator installs two `ClusterRole`s labelled to aggregate into the
standard Kubernetes ClusterRoles:

| Operator ClusterRole              | Aggregates into     | Grants on simplyblock CRs            |
|-----------------------------------|---------------------|--------------------------------------|
| `simplyblock-aggregate-to-view`   | `view`              | `get`, `list`, `watch`               |
| `simplyblock-aggregate-to-edit`   | `edit`, `admin`     | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` |

Effect: anyone already bound to the built-in `view`, `edit`, or `admin`
ClusterRole in a namespace automatically gets the corresponding access to the
`StorageCluster`s and `Pool`s in that namespace. No further configuration is
needed for the common case.

For example, to make `alice` an admin of cluster `prod` (assuming
`StorageCluster/prod` lives in namespace `cluster-prod`):

```sh
kubectl create rolebinding alice-admin \
    --clusterrole=admin \
    --user=alice \
    --namespace=cluster-prod
```

### Per-resource scoping with `resourceNames`

For finer-grained delegation — e.g. admin only of `StorageCluster/prod`, not
any other `StorageCluster` in the same namespace — write a `Role` with
`resourceNames`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: prod-cluster-admin
  namespace: cluster-prod
rules:
- apiGroups: ["storage.simplyblock.io"]
  resources: ["storageclusters"]
  resourceNames: ["prod"]
  verbs: ["get", "update", "patch", "delete"]
- apiGroups: ["storage.simplyblock.io"]
  resources: ["storageclusters/status"]
  resourceNames: ["prod"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: alice-prod-cluster-admin
  namespace: cluster-prod
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: prod-cluster-admin
subjects:
- kind: User
  name: alice
```

> **K8s RBAC limitation**: `resourceNames` only filters verbs that target a
> named object (`get`, `update`, `patch`, `delete`). It is silently ignored
> for `list`, `watch`, and `create`. A user with only the Role above can
> `kubectl get storagecluster prod` (a named GET) but not
> `kubectl get storagecluster` (a LIST) — they will need a separate, broader
> binding (e.g. the `view` ClusterRole) if you want them to enumerate. This is
> a property of K8s RBAC, not the operator.

### Delegating who can create clusters and grant admin

There is no shipped "platform admin" ClusterRole — choose your own gate. Two
common patterns:

* **Gate by namespace ownership.** Whoever has the built-in `admin` ClusterRole
  in a namespace can create and fully manage `StorageCluster`s there (the
  aggregation role makes that work). To stop arbitrary users from creating
  namespaces, restrict `create namespaces` at the cluster scope.
* **Gate by SA.** Reserve `create storageclusters` for a small set of service
  accounts (e.g. your platform automation) and have them stand up tenant
  namespaces on demand.

To let a "cluster owner" delegate admin to teammates *without* giving them
`escalate` on RBAC, grant them the `bind` verb on the specific Role they're
allowed to hand out:

```yaml
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles"]
  resourceNames: ["prod-cluster-admin"]
  verbs: ["bind"]
```

See the upstream docs on [privilege escalation
prevention](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#privilege-escalation-prevention-and-bootstrapping)
for the full mechanism.

### A note on webapi authentication

The operator's pod is the sole caller of the simplyblock webapi; user
identities are **not** propagated to the backend. K8s RBAC governs what users
can do to the CRs, the operator then talks to webapi using its own service
account token.

---

## 📄 License

This project is licensed under the **Apache 2.0 License** — see the [LICENSE](LICENSE) file for details.

---

## 📚 Documentation

* [Architecture Overview](ARCHITECTURE.md)
* [Kubernetes Deployment Guide](https://docs.simplyblock.io/latest/deployments/kubernetes/)
* [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

---

## 🤝 Contributing

We welcome contributions!

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Commit your changes with a clear message
4. Push to your fork and open a Pull Request

Run `make help` for the full list of available `make` targets.

---

## 📬 Support

* 📖 [Documentation](https://docs.simplyblock.io/latest/deployments/kubernetes/)
* 🐞 [GitHub Issues](https://github.com/simplyblock/simplyblock-operator/issues)
* 🌐 [Simplyblock Website](https://www.simplyblock.io)

Maintained by the **simplyblock team**.

---

**Manage NVMe-grade simplyblock storage the Kubernetes-native way.**