#!/usr/bin/env bash
# Renders the operator chart with the Prometheus settings an upgrade from 26.3
# gets, and with the bundled kube-prometheus-stack, and checks the Prometheus
# wiring in each. Lives beside the other chart scripts so CI can run it.
set -euo pipefail

chart="$(cd "$(dirname "$0")/../charts/simplyblock-operator" && pwd)"
failed=0

render() {
  helm template t "$chart" -n sb --api-versions cert-manager.io/v1 \
    --api-versions monitoring.coreos.com/v1/ServiceMonitor \
    --set controlplane.observability.enabled=true --set controlCenter.enabled=true "$@"
}

expect() { # description, pattern, rendered text
  if ! grep -qF -- "$2" <<<"$3"; then echo "FAIL: $1 (missing: $2)"; failed=1; fi
}
reject() {
  if grep -qF -- "$2" <<<"$3"; then echo "FAIL: $1 (found: $2)"; failed=1; fi
}

# Default values: the operator runs simplyblock-prometheus, as after a 26.3 upgrade.
out="$(render)"
expect "Control Center reads the operator's Prometheus" 'value: "http://simplyblock-prometheus:9090"' "$out"
expect "Thanos Query reads the Prometheus sidecar" '--store=simplyblock-prometheus-0.simplyblock-prometheus-headless.sb.svc:10901' "$out"

# Bundled kube-prometheus-stack.
out="$(render --set prometheus.enabled=true)"
expect "Control Center reads the bundled Prometheus" 'value: "http://prometheus-operated:9090"' "$out"
reject "Thanos Query skips the removed sidecar" 'simplyblock-prometheus-headless' "$out"
reject "ScrapeConfig uses no field prometheus-operator lacks" 'credentialsFile' "$out"
expect "ScrapeConfig presents the scraper token" 'name: simplyblock-metrics-scraper-token' "$out"
expect "management API trusts the scraper account" 'system:serviceaccount:sb:simplyblock-metrics-scraper' "$out"

[ "$failed" = 0 ] && echo "prometheus values: ok"
exit "$failed"
