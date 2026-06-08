# Simplyblock Operator
// TODO(user): Add simple overview of use/purpose

## Description
// TODO(user): An in-depth paragraph about your project and overview of use

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/simplyblock-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/simplyblock-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Access control (RBAC)

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

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/simplyblock-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/simplyblock-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

