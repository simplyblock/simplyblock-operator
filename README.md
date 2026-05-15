# Simplyblock Helm Charts

This repository contains the official Helm charts for Simplyblock.

## Charts

| Chart | Description |
|-------|-------------|
| [simplyblock-operator](charts/simplyblock-operator) | Deploys the Simplyblock Operator and all required components on Kubernetes |

## Usage

### Install the Simplyblock Operator

Clone the repository and install directly from the local path:

```bash
git clone https://github.com/simplyblock/helm-charts.git
cd helm-charts

helm install simplyblock-operator charts/simplyblock-operator \
  --namespace simplyblock \
  --create-namespace
```

After the Helm installation completes, wait for the Simplyblock control plane to be ready before creating custom resources such as `StorageCluster`, `Pool`, or `StorageNode`:

```bash
kubectl -n simplyblock wait controlplane simplyblock \
  --for=jsonpath='{.status.phase}'=Ready \
  --timeout=180s
```

### Upgrade

```bash
helm upgrade simplyblock-operator charts/simplyblock-operator \
  --namespace simplyblock
```

> **Note:** Helm does not update CRDs during `helm upgrade`. After upgrading the chart, apply any updated CRDs manually:
>
> ```bash
> kubectl apply --server-side -f charts/simplyblock-operator/crds/
> ```

### Uninstall

```bash
helm uninstall simplyblock-operator --namespace simplyblock
```

## Syncing CRDs and Roles from simplyblock-manager

Run the sync script whenever CRDs or RBAC roles change in the [simplyblock-manager](https://github.com/simplyblock/simplyblock-manager) repo:

```bash
# Sync only (update files in this repo)
scripts/sync-from-manager.sh

# Sync and apply CRDs to the cluster in one step
APPLY=true scripts/sync-from-manager.sh
```

By default the script looks for `simplyblock-manager` as a sibling directory. Pass a custom path as the first argument if needed:

```bash
scripts/sync-from-manager.sh /path/to/simplyblock-manager
```

## Development

To update chart dependencies locally:

```bash
helm dependency update charts/simplyblock-operator
helm upgrade --install simplyblock-operator ./charts/simplyblock-operator/ --namespace simplyblock \
    --create-namespace \
    --set image.operator.repository=docker.io/simplyblock/simplyblock-operator \
    --set image.operator.tag="main" \
    --set image.simplyblock.repository=docker.io/simplyblock/simplyblock \
    --set image.simplyblock.tag="main" \
    --set image.csi.repository=docker.io/simplyblock/spdkcsi \
    --set image.csi.tag=latest
```

### Vendored CRDs

The chart's `crds/` directory contains CRDs that Helm applies automatically before any chart resources. Most CRDs are managed directly, but the MongoDB Community CRD is extracted from the vendored sub-chart tarball and committed here so it is installed on clean clusters without any extra steps.

To regenerate it after updating the `mongodb-kubernetes` dependency:

```bash
tar -xOf charts/simplyblock-operator/charts/mongodb-kubernetes-*.tgz \
  mongodb-kubernetes/crds/mongodbcommunity.mongodb.com_mongodbcommunity.yaml \
  > charts/simplyblock-operator/crds/mongodbcommunity.mongodb.com_mongodbcommunity.yaml
```

To lint the chart:

```bash
helm lint charts/simplyblock-operator
```

## Links

- [Simplyblock Documentation](https://docs.simplyblock.io)
- [GitHub](https://github.com/simplyblock/helm-charts)
