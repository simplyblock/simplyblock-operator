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

### Upgrade

```bash
helm upgrade simplyblock-operator charts/simplyblock-operator \
  --namespace simplyblock
```

### Uninstall

```bash
helm uninstall simplyblock-operator --namespace simplyblock
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

To lint the chart:

```bash
helm lint charts/simplyblock-operator
```

## Links

- [Simplyblock Documentation](https://docs.simplyblock.io)
- [GitHub](https://github.com/simplyblock/helm-charts)
