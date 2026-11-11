#!/usr/bin/env bash
# Behavioral tests for TLS on a cluster that serves no cert-manager API yet.
#
# The standalone profile's ControlPlane installs cert-manager when it is
# missing (controlplane.local.prerequisites.installCertManager), so the chart
# renders there and leaves its cert-manager objects to a later render, or to
# the operator. Where nothing installs cert-manager, the chart still refuses.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$REPO/helm-charts/charts/simplyblock-operator"

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

render() {
  helm template sb "$CHART" --namespace simplyblock "$@" 2>&1
}

# Default values, no cert-manager API: renders, and no cert-manager object is
# in the manifest, since none of them could be applied yet.
if out="$(render)"; then
  if grep -q '^apiVersion: cert-manager.io/v1' <<<"$out"; then
    fail "without cert-manager.io/v1 the chart still renders cert-manager objects"
  fi
else
  fail "default values without cert-manager.io/v1 did not render: $(tail -1 <<<"$out")"
fi

# cert-manager served: the chart's certificates are rendered.
if out="$(render --api-versions cert-manager.io/v1)"; then
  grep -q '^kind: Certificate' <<<"$out" ||
    fail "with cert-manager.io/v1 the chart renders no Certificate"
else
  fail "default values with cert-manager.io/v1 did not render: $(tail -1 <<<"$out")"
fi

# Nothing installs cert-manager: the TLS guard refuses the install.
refused() {
  local out
  out="$(render "$@")" && return 1
  grep -q 'cert-manager.io/v1 is not served' <<<"$out"
}
refused --set controlplane.local.prerequisites.installCertManager=false ||
  fail "installCertManager=false without cert-manager.io/v1 was not refused by the TLS guard"
refused --set deployment.profile=managed --set controlplane.managed.endpoint=https://cp.example:443 ||
  fail "the managed profile without cert-manager.io/v1 was not refused by the TLS guard"

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "ok"
