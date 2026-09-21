#!/usr/bin/env bash
# Asserts that a rendered chart contains the objects the operator needs to work.
#
# `helm template` exits zero for a template that renders to nothing, so a guard
# reading a value that no longer exists drops its objects silently. This lists
# what each profile must produce and fails when one of them is missing.
set -uo pipefail

CHART="$(cd "$(dirname "${BASH_SOURCE[0]}")/../charts/simplyblock-operator" && pwd)"
fail=0

# The capabilities a cluster-less render has to be told about.
#
# TLS is on by default and the chart refuses a cluster that does not serve
# cert-manager's API, which is the right refusal at install time and an
# impossible one here: `helm template` asks no cluster anything, so the guard
# fires on every render. Stating the capability is what a renderer with a
# cluster behind it does, and without it every profile below reads as a render
# failure rather than as the objects it is meant to check.
CAPABILITIES=(--api-versions cert-manager.io/v1)

# Objects every profile renders, as `Kind/name`.
COMMON=(
  "Deployment/simplyblock-operator"
  "Service/simplyblock-operator-webhook-service"
  "MutatingWebhookConfiguration/simplyblock-operator-mutating-webhook-configuration"
  "ValidatingWebhookConfiguration/simplyblock-operator-validating-webhook-configuration"
  "ServiceAccount/simplyblock-operator"
  "ControlPlane/simplyblock"
  # Both profiles run workloads that mount simplyblock volumes, so both need a
  # CSI driver, and nothing but this object produces one: the chart stopped
  # rendering the plugins when the operator took them over.
  "SimplyblockDriver/simplyblock"
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
    "${CAPABILITIES[@]}" \
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

# render prints the objects a profile produces, with the extra settings given.
render() {
  local profile="$1"
  shift
  helm template sb "$CHART" --namespace simplyblock \
    "${CAPABILITIES[@]}" \
    --set deployment.profile="$profile" \
    --set controlplane.managed.endpoint=https://cp.example.com \
    "$@" 2>/dev/null | objects
}

# checkPair asserts that a StorageClass and the provisioner it names are
# rendered together.
#
# A class naming a provisioner nothing installed is worse than no class: it is
# advertised by `kubectl get storageclass`, a PVC can be pointed at it, and that
# PVC then waits for a provisioner that is never coming, with nothing in the
# cluster saying why.
checkPair() {
  local present

  present="$(render standalone)"
  if printf '%s\n' "$present" | grep -qxF "StorageClass/local-hostpath"; then
    echo "  hostpath: StorageClass/local-hostpath is rendered while its provisioner is not installed"
    fail=1
  else
    echo "  hostpath: no StorageClass without its provisioner"
  fi

  present="$(render standalone --set controlplane.csiHostpathDriver.enabled=true)"
  local want
  for want in "StorageClass/local-hostpath" "CSIDriver/hostpath.csi.k8s.io"; do
    if ! printf '%s\n' "$present" | grep -qxF "$want"; then
      echo "  hostpath: MISSING ${want} with the driver enabled"
      fail=1
    fi
  done
}

check standalone "${COMMON[@]}"
check managed "${COMMON[@]}"
checkPair

exit "$fail"
