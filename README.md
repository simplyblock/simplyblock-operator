# Simplyblock Helm Charts

This repository contains the official Helm charts for Simplyblock.

## Charts

| Chart | Description |
|-------|-------------|
| [simplyblock-operator](charts/simplyblock-operator) | Deploys the Simplyblock Operator and all required components on Kubernetes |

## Usage

### Add the Helm repository

```bash
helm repo add simplyblock https://simplyblock.github.io/helm-charts
helm repo update
```

### Install the Simplyblock Operator

```bash
helm install simplyblock-operator simplyblock/simplyblock-operator \
  --namespace simplyblock \
  --create-namespace
```

### Upgrade

```bash
helm upgrade simplyblock-operator simplyblock/simplyblock-operator \
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
```

To lint the chart:

```bash
helm lint charts/simplyblock-operator
```

## Links

- [Simplyblock Documentation](https://docs.simplyblock.io)
- [GitHub](https://github.com/simplyblock/helm-charts)
