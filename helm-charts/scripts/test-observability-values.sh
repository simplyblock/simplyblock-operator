#!/usr/bin/env bash
# Behavioral tests for the observability stack's values: each one is rendered
# with `helm template` and the manifest is checked for its effect.
#
# Helm renders an unread value as nothing, so a key in values.yaml that no
# template reads looks like a working setting. These cases assert that the
# pull policies of Graylog, Grafana, and Thanos reach the containers they
# configure.
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

if [[ "$failures" -gt 0 ]]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "observability values: all cases pass"
