#!/usr/bin/env bash
# Behavioral tests for the image values the operator hands to what it deploys:
# each case renders the chart with `helm template` and checks the operator
# Deployment's environment.
#
# The operator derives the rebalancer image from its own (SB_OPERATOR_IMAGE):
# the same registry and tag, under the rebalancer's repository. An install that
# moves the operator to another registry moves the rebalancer with it, unless
# the chart pins one through SIMPLYBLOCK_REBALANCER_IMAGE. These cases assert
# that the chart pins it only when a rebalancer value is set.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$REPO/helm-charts/charts/simplyblock-operator"
YQ="${YQ:-$REPO/.bin/yq}"
command -v "$YQ" >/dev/null 2>&1 || YQ=yq

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

render() {
  helm template sb "$CHART" --namespace simplyblock \
    --api-versions cert-manager.io/v1 "$@"
}

# operator_env prints one environment variable of the operator container, or
# unset when the container has none. The fallback is applied here rather than
# with yq's `//`, which the yq CI pins applies once per document a select
# dropped.
operator_env() {
  local out
  out="$("$YQ" -N "select(.kind == \"Deployment\" and .metadata.name == \"simplyblock-operator\")
    | .spec.template.spec.containers[] | select(.name == \"manager\")
    | .env[] | select(.name == \"$2\") | .value" <<<"$1")"
  if [[ -z "$out" || "$out" == "null" ]]; then
    out="unset"
  fi
  echo "$out"
}

expect() {
  local name="$1" got="$2" want="$3"
  if [[ "$got" != "$want" ]]; then
    fail "$name: got '$got', want '$want'"
  fi
}

ecr=(--set image.operator.repository=public.ecr.aws/simply-block/simplyblock-operator
  --set image.operator.tag=superclusters)

manifest="$(render "${ecr[@]}")"
expect "the operator's own image is the one it is deployed from" \
  "$(operator_env "$manifest" SB_OPERATOR_IMAGE)" \
  "public.ecr.aws/simply-block/simplyblock-operator:superclusters"
expect "no rebalancer value leaves the rebalancer to the operator's registry" \
  "$(operator_env "$manifest" SIMPLYBLOCK_REBALANCER_IMAGE)" "unset"

manifest="$(render "${ecr[@]}" --set image.rebalancer.repository=registry.example.com/rebalancer)"
expect "a rebalancer repository is pinned at the operator's tag" \
  "$(operator_env "$manifest" SIMPLYBLOCK_REBALANCER_IMAGE)" \
  "registry.example.com/rebalancer:superclusters"

manifest="$(render "${ecr[@]}" --set image.rebalancer.tag=v1.2.3)"
expect "a rebalancer tag is pinned in the operator's registry" \
  "$(operator_env "$manifest" SIMPLYBLOCK_REBALANCER_IMAGE)" \
  "public.ecr.aws/simply-block/simplyblock-rebalancer:v1.2.3"

manifest="$(render "${ecr[@]}" --set image.rebalancer.repository=registry.example.com/rebalancer \
  --set image.rebalancer.tag=v1.2.3)"
expect "a rebalancer repository and tag are pinned as given" \
  "$(operator_env "$manifest" SIMPLYBLOCK_REBALANCER_IMAGE)" \
  "registry.example.com/rebalancer:v1.2.3"

if ((failures > 0)); then
  echo "$failures image value case(s) failed"
  exit 1
fi
echo "All image value cases passed."
