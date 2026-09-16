#!/usr/bin/env bash
# Asserts that a rendered chart contains the objects the operator needs to work.
#
# `helm template` exits zero for a template that renders to nothing, so a guard
# reading a value that no longer exists drops its objects silently. This lists
# what each profile must produce and fails when one of them is missing.
set -uo pipefail

CHART="$(cd "$(dirname "${BASH_SOURCE[0]}")/../charts/simplyblock-operator" && pwd)"
fail=0

# Objects every profile renders, as `Kind/name`.
COMMON=(
  "Deployment/simplyblock-operator"
  "Service/simplyblock-operator-webhook-service"
  "MutatingWebhookConfiguration/simplyblock-operator-mutating-webhook-configuration"
  "ValidatingWebhookConfiguration/simplyblock-operator-validating-webhook-configuration"
  "ServiceAccount/simplyblock-operator"
  "ControlPlane/simplyblock"
)

# objects prints `Kind/name` for every document in a rendered manifest. The name
# is taken from the first `  name:` under a top-level `metadata:`, so a name
# nested in a pod template or a webhook entry is not mistaken for the object's.
objects() {
  awk '
    /^---/                  { kind=""; name=""; depth=0; next }
    /^kind: /               { kind=$2; next }
    /^metadata:/            { depth=1; next }
    depth==1 && /^  name: / { depth=0; if (kind != "") print kind "/" $2; next }
    /^[a-zA-Z]/             { depth=0 }
  '
}

check() {
  local profile="$1"
  shift
  local -a required=("$@")
  local out present missing=0

  out="$(helm template sb "$CHART" --namespace simplyblock \
    --set deployment.profile="$profile" \
    --set controlplane.managed.endpoint=https://cp.example.com 2>/dev/null)"
  if [ -z "$out" ]; then
    echo "  ${profile}: RENDER FAILED"
    fail=1
    return
  fi

  present="$(printf '%s\n' "$out" | objects)"
  for want in "${required[@]}"; do
    if ! printf '%s\n' "$present" | grep -qxF "$want"; then
      echo "  ${profile}: MISSING ${want}"
      missing=1
      fail=1
    fi
  done
  if [ "$missing" -eq 0 ]; then
    echo "  ${profile}: all ${#required[@]} required objects present"
  fi
}

check standalone "${COMMON[@]}"
check managed "${COMMON[@]}"

exit "$fail"
