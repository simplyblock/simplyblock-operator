#!/usr/bin/env bash
# Behavioral tests for the observability stack's values: each one is rendered
# with `helm template` and the manifest is checked for its effect.
#
# Helm renders an unread value as nothing, so a key in values.yaml that no
# template reads looks like a working setting. These cases assert that the
# Graylog retention period and the pull policies of Graylog, Grafana, and Thanos
# reach the objects they configure, and so do Graylog's sizing and journal.
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

# query runs a yq expression over a rendered manifest and prints its result, or
# "unset" when nothing matched. The fallback is applied here rather than with
# yq's `//`: the yq CI pins (v4.44.3) applies `//` to every document a select
# dropped, and prints "unset" once for each of them.
query() {
  local manifest="$1" expr="$2" out
  out="$("$YQ" -N "$expr" <<<"$manifest")"
  if [[ -z "$out" || "$out" == "null" ]]; then
    out="unset"
  fi
  echo "$out"
}

# container prints one field of a Deployment container, or "unset".
container() {
  query "$1" "select(.kind == \"Deployment\") | .spec.template.spec.containers[] | select(.name == \"$2\") | .$3"
}

graylog_env() {
  query "$1" "select(.kind == \"Deployment\") | .spec.template.spec.containers[] | select(.name == \"graylog\") | .env[] | select(.name == \"$2\") | .value"
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

# Graylog is sized for the storage nodes' SPDK logs by default: 4 CPUs, a 3g heap
# within a 6Gi limit, more processing threads, and its journal on a volume.
out="$(render --set controlplane.storageclass.name=gp3)"
[[ "$(container "$out" graylog resources.limits.cpu)" == "4" ]] ||
  fail "graylog cpu limit is $(container "$out" graylog resources.limits.cpu), want 4"
[[ "$(container "$out" graylog resources.limits.memory)" == "6Gi" ]] ||
  fail "graylog memory limit is $(container "$out" graylog resources.limits.memory), want 6Gi"
[[ "$(graylog_env "$out" GRAYLOG_SERVER_JAVA_OPTS)" == "-Xms3g -Xmx3g -XX:+UseG1GC" ]] ||
  fail "graylog java opts are $(graylog_env "$out" GRAYLOG_SERVER_JAVA_OPTS), want a 3g heap"
[[ "$(graylog_env "$out" GRAYLOG_PROCESSBUFFER_PROCESSORS)" == "4" ]] ||
  fail "graylog processbuffer processors are $(graylog_env "$out" GRAYLOG_PROCESSBUFFER_PROCESSORS), want 4"
[[ "$(graylog_env "$out" GRAYLOG_MESSAGE_JOURNAL_MAX_SIZE)" == "15gb" ]] ||
  fail "graylog journal max size is $(graylog_env "$out" GRAYLOG_MESSAGE_JOURNAL_MAX_SIZE), want 15gb"
journal_claim="$(query "$out" 'select(.kind == "Deployment" and .metadata.name == "simplyblock-graylog") | .spec.template.spec.volumes[] | select(.name == "simplyblock-graylog-journal") | .persistentVolumeClaim.claimName')"
[[ "$journal_claim" == "simplyblock-graylog-journal" ]] ||
  fail "graylog journal volume claim is $journal_claim, want simplyblock-graylog-journal"
[[ "$(query "$out" 'select(.kind == "PersistentVolumeClaim" and .metadata.name == "simplyblock-graylog-journal") | .spec.storageClassName')" == "gp3" ]] ||
  fail "graylog journal claim does not use controlplane.storageclass.name"
[[ "$(query "$out" 'select(.kind == "Deployment" and .metadata.name == "simplyblock-graylog") | .spec.strategy.type')" == "Recreate" ]] ||
  fail "graylog with a journal volume does not use the Recreate strategy"

# Each sizing value reaches the container.
out="$(render   --set controlplane.observability.graylog.resources.limits.cpu=8   --set controlplane.observability.graylog.heapSize=6g   --set controlplane.observability.graylog.outputBatchSize=1000)"
[[ "$(container "$out" graylog resources.limits.cpu)" == "8" ]] ||
  fail "graylog cpu limit is $(container "$out" graylog resources.limits.cpu), want 8"
[[ "$(graylog_env "$out" GRAYLOG_SERVER_JAVA_OPTS)" == "-Xms6g -Xmx6g -XX:+UseG1GC" ]] ||
  fail "graylog java opts are $(graylog_env "$out" GRAYLOG_SERVER_JAVA_OPTS), want a 6g heap"
[[ "$(graylog_env "$out" GRAYLOG_OUTPUT_BATCH_SIZE)" == "1000" ]] ||
  fail "graylog output batch size is $(graylog_env "$out" GRAYLOG_OUTPUT_BATCH_SIZE), want 1000"

# Without journal persistence: an emptyDir, no claim, the default rollout.
out="$(render --set controlplane.observability.graylog.journal.persistence=false)"
[[ "$(query "$out" 'select(.kind == "PersistentVolumeClaim" and .metadata.name == "simplyblock-graylog-journal") | .metadata.name')" == "unset" ]] ||
  fail "graylog journal claim rendered with persistence off"
[[ "$(query "$out" 'select(.kind == "Deployment" and .metadata.name == "simplyblock-graylog") | .spec.template.spec.volumes[] | select(.name == "simplyblock-graylog-journal") | has("emptyDir")')" == "true" ]] ||
  fail "graylog journal is not an emptyDir with persistence off"
[[ "$(query "$out" 'select(.kind == "Deployment" and .metadata.name == "simplyblock-graylog") | .spec.strategy.type')" == "unset" ]] ||
  fail "graylog without a journal volume sets a rollout strategy"

if [[ "$failures" -gt 0 ]]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "observability values: all cases pass"
