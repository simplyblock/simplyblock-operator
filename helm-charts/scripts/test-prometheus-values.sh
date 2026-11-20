#!/usr/bin/env bash
# Behavioral tests for prometheus.mode: each case is rendered with
# `helm template` and checked for the Prometheus URL the operator, the Control
# Center, and Thanos Query end up with, and for the combinations the chart refuses.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$REPO/helm-charts/charts/simplyblock-operator"
YQ="${YQ:-$REPO/.bin/yq}"
command -v "$YQ" >/dev/null 2>&1 || YQ=yq

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

render() {
  helm template sb "$CHART" --namespace simplyblock --api-versions cert-manager.io/v1 "$@"
}

# operator_flag prints the operator's --prometheus-url value, or "unset".
operator_flag() {
  local out
  out="$("$YQ" -N 'select(.kind == "Deployment" and .metadata.name == "simplyblock-operator") | .spec.template.spec.containers[0].args[] | select(test("^--prometheus-url="))' <<<"$1" | sed 's/^--prometheus-url=//')"
  echo "${out:-unset}"
}

control_center_url() {
  local out
  out="$("$YQ" -N 'select(.kind == "Deployment") | .spec.template.spec.containers[].env[]? | select(.name == "SB_PROMETHEUS_URL") | .value' <<<"$1")"
  echo "${out:-unset}"
}

# Managed reads the operator's own Prometheus.
out="$(render --set controlCenter.enabled=true --set controlplane.observability.enabled=true)"
[[ "$(operator_flag "$out")" == "http://simplyblock-prometheus:9090" ]] ||
  fail "Managed: --prometheus-url is $(operator_flag "$out")"
[[ "$(control_center_url "$out")" == "http://simplyblock-prometheus:9090" ]] ||
  fail "Managed: Control Center SB_PROMETHEUS_URL is $(control_center_url "$out")"
grep -q -- "--store=simplyblock-prometheus-0.simplyblock-prometheus-headless.simplyblock.svc:10901" <<<"$out" ||
  fail "Managed: Thanos Query has no store for the managed Prometheus's sidecar"

# External reads prometheus-operated unless a URL is given.
out="$(render --set prometheus.mode=External --set controlCenter.enabled=true --set controlplane.observability.enabled=true)"
[[ "$(operator_flag "$out")" == "http://prometheus-operated:9090" ]] ||
  fail "External: --prometheus-url is $(operator_flag "$out")"
[[ "$(control_center_url "$out")" == "http://prometheus-operated:9090" ]] ||
  fail "External: Control Center SB_PROMETHEUS_URL is $(control_center_url "$out")"
grep -q -- "simplyblock-prometheus-headless" <<<"$out" &&
  fail "External: Thanos Query still points at the managed Prometheus's sidecar"
out="$(render --set prometheus.mode=External --set prometheus.external.url=http://prom.monitoring:9090)"
[[ "$(operator_flag "$out")" == "http://prom.monitoring:9090" ]] ||
  fail "External with a URL: --prometheus-url is $(operator_flag "$out")"

# The capacity and performance controllers read the same flag, so it is passed
# with the metrics API off too.
out="$(render --set prometheus.mode=External --set metricsAPI.enabled=false)"
[[ "$(operator_flag "$out")" == "http://prometheus-operated:9090" ]] ||
  fail "External, metrics API off: --prometheus-url is $(operator_flag "$out")"

# An explicit metricsAPI.prometheusURL wins; empty still means no Prometheus.
out="$(render --set prometheus.mode=External --set metricsAPI.prometheusURL=http://x:9090)"
[[ "$(operator_flag "$out")" == "http://x:9090" ]] ||
  fail "metricsAPI.prometheusURL override: --prometheus-url is $(operator_flag "$out")"

# Refused combinations.
render --set prometheus.mode=Off >/dev/null 2>&1 && fail "prometheus.mode=Off rendered"
render --set kube-prometheus-stack.enabled=true >/dev/null 2>&1 &&
  fail "kube-prometheus-stack.enabled without External rendered"

if ((failures > 0)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "prometheus values: ok"
