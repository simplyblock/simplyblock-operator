#!/usr/bin/env bash
# Behavioral tests for the observability stack's values: each one is rendered
# with `helm template` and the manifest is checked for its effect.
#
# Helm renders an unread value as nothing, so a key in values.yaml that no
# template reads looks like a working setting. These cases assert that the
# Graylog retention period and the pull policies of Graylog, Grafana, and Thanos
# reach the objects they configure.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$REPO/helm-charts/charts/simplyblock-operator"
YQ="${YQ:-$REPO/.bin/yq}"
command -v "$YQ" >/dev/null 2>&1 || YQ=yq

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

render() {
  helm template sb "$CHART" --namespace simplyblock \
    --api-versions cert-manager.io/v1 \
    --set controlplane.observability.enabled=true "$@"
}

# container prints one field of a Deployment container, or "unset".
container() {
  local manifest="$1" name="$2" field="$3"
  "$YQ" "select(.kind == \"Deployment\") | .spec.template.spec.containers[] | select(.name == \"$name\") | .$field // \"unset\"" <<<"$manifest"
}

graylog_env() {
  local manifest="$1" env="$2"
  "$YQ" "select(.kind == \"Deployment\") | .spec.template.spec.containers[] | select(.name == \"graylog\") | .env[] | select(.name == \"$env\") | .value // \"unset\"" <<<"$manifest"
}

# Each pull policy reaches its containers.
out="$(render \
  --set controlplane.observability.graylog.pullPolicy=Never \
  --set controlplane.observability.grafana.pullPolicy=Always \
  --set controlplane.observability.thanos.pullPolicy=Never)"
[[ "$(container "$out" graylog imagePullPolicy)" == "Never" ]] ||
  fail "graylog imagePullPolicy is $(container "$out" graylog imagePullPolicy), want Never"
[[ "$(container "$out" grafana imagePullPolicy)" == "Always" ]] ||
  fail "grafana imagePullPolicy is $(container "$out" grafana imagePullPolicy), want Always"
for c in thanos-store thanos-query thanos-compactor; do
  [[ "$(container "$out" "$c" imagePullPolicy)" == "Never" ]] ||
    fail "$c imagePullPolicy is $(container "$out" "$c" imagePullPolicy), want Never"
done

# The retention period is split across maxNumberIndex indices: 7d over 3 is 56h each.
out="$(render \
  --set controlplane.observability.graylog.retentionPeriod=7d \
  --set-string controlplane.observability.graylog.maxNumberIndex=3)"
[[ "$(graylog_env "$out" GRAYLOG_ELASTICSEARCH_MAX_TIME_PER_INDEX)" == "56h" ]] ||
  fail "7d over 3 indices gives $(graylog_env "$out" GRAYLOG_ELASTICSEARCH_MAX_TIME_PER_INDEX) per index, want 56h"

# Hours and weeks are accepted: 2w over 7 indices is 48h each.
out="$(render \
  --set controlplane.observability.graylog.retentionPeriod=2w \
  --set-string controlplane.observability.graylog.maxNumberIndex=7)"
[[ "$(graylog_env "$out" GRAYLOG_ELASTICSEARCH_MAX_TIME_PER_INDEX)" == "48h" ]] ||
  fail "2w over 7 indices gives $(graylog_env "$out" GRAYLOG_ELASTICSEARCH_MAX_TIME_PER_INDEX) per index, want 48h"

# A period the chart cannot read is refused rather than rendered.
if render --set controlplane.observability.graylog.retentionPeriod="7 days" >/dev/null 2>&1; then
  fail "retentionPeriod=\"7 days\" rendered, want a render failure"
fi

# A period shorter than one hour per index is refused.
if render \
  --set controlplane.observability.graylog.retentionPeriod=12h \
  --set-string controlplane.observability.graylog.maxNumberIndex=24 >/dev/null 2>&1; then
  fail "12h over 24 indices rendered, want a render failure"
fi

if [[ "$failures" -gt 0 ]]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "observability values: all cases pass"
