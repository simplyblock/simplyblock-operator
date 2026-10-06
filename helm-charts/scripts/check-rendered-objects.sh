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

# Objects every profile renders, as `Kind/name`: the operator and what a CSI
# driver needs from it, whether the chart or an administrator writes the driver.
OPERATOR=(
  "Deployment/simplyblock-operator"
  "Service/simplyblock-operator-webhook-service"
  "MutatingWebhookConfiguration/simplyblock-operator-mutating-webhook-configuration"
  "ValidatingWebhookConfiguration/simplyblock-operator-validating-webhook-configuration"
  "ServiceAccount/simplyblock-operator"
  "Service/simplyblock-csi-link"
  "ConfigMap/simplyblock-bootstrap"
)

# Objects the standalone and managed profiles render on top of OPERATOR.
COMMON=(
  "${OPERATOR[@]}"
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
  # Every membership test below reads the list from a here-string. "pipefail" is
  # set, and grep -q closes the pipe as soon as it matches, so feeding it from a
  # pipe reports the SIGPIPE of the writer and turns a present object into a
  # missing one whenever the list is long enough for the write to be unfinished.
  for want in "${required[@]}"; do
    if ! grep -qxF "$want" <<<"$present"; then
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
  if grep -qxF "StorageClass/local-hostpath" <<<"$present"; then
    echo "  hostpath: StorageClass/local-hostpath is rendered while its provisioner is not installed"
    fail=1
  else
    echo "  hostpath: no StorageClass without its provisioner"
  fi

  present="$(render standalone --set controlplane.csiHostpathDriver.enabled=true)"
  local want
  for want in "StorageClass/local-hostpath" "CSIDriver/hostpath.csi.k8s.io"; do
    if ! grep -qxF "$want" <<<"$present"; then
      echo "  hostpath: MISSING ${want} with the driver enabled"
      fail=1
    fi
  done
}

# checkVendoredCRDs asserts that the CRDs the chart carries for FoundationDB and
# for MongoDB are rendered by default, and that each one's value drops its own
# and leaves the other alone.
#
# They are templates rather than crds/ entries so that a deployment can decline
# them, and crds/ is applied whatever a value says. A guard that read a path
# values.yaml does not define would render nothing at all, and one written
# against the wrong value would go on installing a CRD a cluster asked the chart
# to leave to its own operator.
checkVendoredCRDs() {
  local present want
  local -a fdb=(
    "CustomResourceDefinition/foundationdbclusters.apps.foundationdb.org"
    "CustomResourceDefinition/foundationdbbackups.apps.foundationdb.org"
    "CustomResourceDefinition/foundationdbrestores.apps.foundationdb.org"
  )
  local mongo="CustomResourceDefinition/mongodbcommunity.mongodbcommunity.mongodb.com"
  local missing=0

  present="$(render standalone)"
  for want in "${fdb[@]}" "$mongo"; do
    if ! grep -qxF "$want" <<<"$present"; then
      echo "  crds: MISSING ${want} by default"
      missing=1
      fail=1
    fi
  done

  present="$(render standalone --set controlplane.foundationdb.installCRDs=false)"
  for want in "${fdb[@]}"; do
    if grep -qxF "$want" <<<"$present"; then
      echo "  crds: ${want} is rendered with controlplane.foundationdb.installCRDs=false"
      missing=1
      fail=1
    fi
  done
  if ! grep -qxF "$mongo" <<<"$present"; then
    echo "  crds: controlplane.foundationdb.installCRDs=false also dropped ${mongo}"
    missing=1
    fail=1
  fi

  present="$(render standalone --set controlplane.observability.mongodb.installCRDs=false)"
  if grep -qxF "$mongo" <<<"$present"; then
    echo "  crds: ${mongo} is rendered with controlplane.observability.mongodb.installCRDs=false"
    missing=1
    fail=1
  fi
  for want in "${fdb[@]}"; do
    if ! grep -qxF "$want" <<<"$present"; then
      echo "  crds: controlplane.observability.mongodb.installCRDs=false also dropped ${want}"
      missing=1
      fail=1
    fi
  done

  if [ "$missing" -eq 0 ]; then
    echo "  crds: the 4 vendored CRDs render by default, and each value drops its own"
  fi
}

# checkEmpty asserts that the empty profile renders the operator and nothing it
# would act on.
#
# The profile exists for an administrator who writes the ControlPlane and the
# SimplyblockDriver by hand after the install. A chart-rendered one beside theirs
# is a second CSI deployment, or a control plane they did not ask for, and the
# discovery run is a draft cluster proposed from workers they did not choose.
checkEmpty() {
  local present unwanted bootstrap
  local clean=1

  present="$(render empty)"
  if [ -z "$present" ]; then
    # Absence is what this asserts, and nothing rendered is absent from.
    echo "  empty: RENDER FAILED"
    fail=1
    return
  fi
  for unwanted in "ControlPlane/simplyblock" "SimplyblockDriver/simplyblock"; do
    if grep -qxF "$unwanted" <<<"$present"; then
      echo "  empty: ${unwanted} is rendered"
      clean=0
      fail=1
    fi
  done

  bootstrap="$(helm template sb "$CHART" --namespace simplyblock \
    "${CAPABILITIES[@]}" \
    --set deployment.profile=empty \
    --show-only templates/bootstrap-configmap.yaml 2>/dev/null)"
  if ! grep -qE '^ +enabled: false$' <<<"$bootstrap"; then
    echo "  empty: the bootstrap ConfigMap does not decline the discovery run"
    clean=0
    fail=1
  fi

  if [ "$clean" -eq 1 ]; then
    echo "  empty: no ControlPlane, no SimplyblockDriver, no discovery run"
  fi
}

# checkLogCollector asserts that every workload the chart renders is marked for
# the log collector, with every optional workload switched on.
#
# fluent-bit ships only pods annotated log-collector/enabled=true, so a workload
# left unmarked never reaches Graylog. The observability stack itself is exempt:
# Graylog shipping its own logs into itself is a loop that fails with it.
checkLogCollector() {
  local yq="${YQ:-yq}"
  local unmarked
  local -a exempt=(
    "Deployment/simplyblock-graylog"
    "Deployment/simplyblock-grafana"
    "Deployment/simplyblock-thanos"
    "StatefulSet/simplyblock-opensearch"
    "Deployment/mongodb-kubernetes-operator"
    "DaemonSet/simplyblock-fluent-bit"
  )

  # The render and the query each report their own failure. Read through one
  # pipeline with only pipefail set, a failed step leaves the list empty, and an
  # empty list is exactly what a fully marked chart produces.
  local rendered
  if ! rendered="$(helm template sb "$CHART" --namespace simplyblock \
    "${CAPABILITIES[@]}" \
    --set deployment.profile=standalone \
    --set controlplane.observability.enabled=true \
    --set controlplane.csiHostpathDriver.enabled=true \
    --set snapshotcontroller.create=true)"; then
    echo "  log collector: RENDER FAILED"
    fail=1
    return
  fi
  if ! unmarked="$("$yq" -N 'select(.kind == "Deployment" or .kind == "DaemonSet" or
      .kind == "StatefulSet" or .kind == "Job" or .kind == "CronJob") |
      select(.spec.template.metadata.annotations["log-collector/enabled"] != "true") |
      .kind + "/" + .metadata.name' <<<"$rendered")"; then
    echo "  log collector: QUERY FAILED (${yq})"
    fail=1
    return
  fi
  for want in "${exempt[@]}"; do
    unmarked="$(grep -vxF "$want" <<<"$unmarked")"
  done

  if [ -n "$unmarked" ]; then
    while read -r workload; do
      echo "  log collector: ${workload} is not marked log-collector/enabled=true"
    done <<<"$unmarked"
    fail=1
  else
    echo "  log collector: every workload outside the observability stack is marked"
  fi
}

check standalone "${COMMON[@]}"
check managed "${COMMON[@]}"
check empty "${OPERATOR[@]}"
checkEmpty
checkPair
checkVendoredCRDs
checkLogCollector

exit "$fail"
