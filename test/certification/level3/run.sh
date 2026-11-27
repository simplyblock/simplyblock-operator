#!/usr/bin/env bash
# Runs the Level 3 certification, Red Hat's storage certification harness
# (github.com/openshift-storage-cert/storage-cert-harness), against the cluster
# KUBECONFIG points at, one step per invocation:
#
#   preflight    the harness checks the cluster, the classes, and the plan
#   load-80      run 1: 4K random I/O latency while external load holds the
#                storage at about 80% of its capacity
#   no-load-80   run 2: the VM criteria (boot storm, live migration, density,
#                HA restart, drain, snapshots, clones, lifecycle), continuing
#                from run 1's report
#   package      certification-results-level3.tar.gz from both runs, the archive
#                Red Hat's guide asks to be submitted
#
# The harness ships as an amd64-only image driven by Podman on a Linux host. The
# script runs that image as a Pod in the cluster instead, the way the Level 1
# runner runs openshift-tests, with the same arguments the harness's own partner
# scripts pass (ci/scripts/run-partner-cert.sh), so a workstation of any
# architecture can drive it. The Pod runs as a ServiceAccount bound to
# cluster-admin, which the criteria need (they drain and fence nodes), and is
# kept off the three workers the criteria disrupt.
#
# The harness repository is fetched at a pinned commit for its catalog,
# thresholds, and plans; the script writes backends.yaml from the classes under
# test and fills the no-load plan's node placeholders. Results land in
# $RESULTS_DIR/level3/ in the harness's own layout
# (certification-results/<profile>/{work,report}, logs/), so the package is the
# one `make partner-package-results` builds.
#
# Usage:
#   KUBECONFIG=/abs/path ./run.sh <preflight|load-80|no-load-80|package>
#
# Environment:
#   KUBECONFIG        required; a cluster-admin kubeconfig
#   STORAGE_CLASS     the StorageClass under test (default: the one class whose
#                     provisioner is csi.simplyblock.io)
#   SNAPSHOT_CLASS    its VolumeSnapshotClass (default: the one class whose driver
#                     is csi.simplyblock.io)
#   FAILURE_NODE      the worker TR-VIRT-007 fences (default: the node FAR_CONFIG
#                     names, else the last Ready worker by name)
#   DRAIN_NODE        the worker TR-VIRT-008 drains (default: the next one)
#   CEILING_NODE      the worker TR-VIRT-013 fills with VMs (default: the next one)
#   FAR_CONFIG        a FenceAgentsRemediation manifest for FAILURE_NODE, needed
#                     when TR-VIRT-007 runs (see the harness's
#                     docs/cluster-setup-guide.md, step 4)
#   SNAPSHOT_REPLICAS, SNAPSHOT_COUNT
#                     TR-VIRT-010's batch: VMs and snapshots (default 10 and 10)
#   ONLY, SKIP        comma-separated Test Requirement IDs to run or leave out of
#                     the no-load plan; either marks the run as a rehearsal
#   EXTERNAL_LOAD     required for load-80: what holds the storage at 80% during
#                     the run, recorded in run-info.txt ("none" for a rehearsal)
#   HARNESS_REF       harness commit (default: pinned below)
#   HARNESS_IMAGE     harness image (default: pinned below)
#   RESULTS_DIR       where level3/ goes (default: results/ next to this script)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="simplyblock-cert-level3"
BINDING="simplyblock-cert-level3"
BACKEND="simplyblock"
POLL_SECONDS=30
HARNESS_REPO="https://github.com/openshift-storage-cert/storage-cert-harness.git"
# Pinned together: the commit's catalog, thresholds, and plans are what the
# image at this digest was published with on 2026-11-26.
HARNESS_REF="${HARNESS_REF:-50e2403f300ff7ac4f5c2755fbaeb24e369dc9ec}"
HARNESS_IMAGE="${HARNESS_IMAGE:-quay.io/virtarraycert/storage-cert-harness@sha256:ac747c6625fb91946a60c818b7e2f522aadb12750ac37eea00c43e64c88789c8}"

STEP="${1:-}"
case "$STEP" in
  preflight | load-80 | no-load-80 | package) ;;
  *)
    echo "usage: $0 <preflight|load-80|no-load-80|package>" >&2
    exit 2
    ;;
esac

LEVEL3_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results}/level3"
CERT_RESULTS="$LEVEL3_DIR/certification-results"
LOGS="$LEVEL3_DIR/logs"
mkdir -p "$CERT_RESULTS" "$LOGS"

# Packaging reads only what the two runs left, in the harness's layout.
if [[ "$STEP" == package ]]; then
  for profile in load-80 no-load-80; do
    [[ -s "$CERT_RESULTS/$profile/report/report.json" ]] || {
      echo "no $profile report in $CERT_RESULTS/$profile/report; run that step first" >&2
      exit 2
    }
  done
  tar -czf "$LEVEL3_DIR/certification-results-level3.tar.gz" -C "$LEVEL3_DIR" \
    certification-results/load-80 certification-results/no-load-80 logs
  echo "package: $LEVEL3_DIR/certification-results-level3.tar.gz"
  exit 0
fi

if [[ -z "${KUBECONFIG:-}" || ! -r "$KUBECONFIG" ]]; then
  echo "KUBECONFIG must name a readable kubeconfig" >&2
  exit 2
fi
for tool in oc git yq; do
  command -v "$tool" >/dev/null || { echo "$tool is not on PATH" >&2; exit 2; }
done

# shellcheck source=../lib/classes.sh
source "$SCRIPT_DIR/../lib/classes.sh"
# shellcheck source=../lib/results.sh
source "$SCRIPT_DIR/../lib/results.sh"
if [[ -z "${STORAGE_CLASS:-}" ]]; then
  STORAGE_CLASS="$(find_class storageclass provisioner)" || { echo "set STORAGE_CLASS" >&2; exit 2; }
fi
if [[ -z "${SNAPSHOT_CLASS:-}" ]]; then
  SNAPSHOT_CLASS="$(find_class volumesnapshotclass driver)" || { echo "set SNAPSHOT_CLASS" >&2; exit 2; }
fi
oc get storageclass "$STORAGE_CLASS" >/dev/null
oc get volumesnapshotclass "$SNAPSHOT_CLASS" >/dev/null

if oc get namespace "$NAMESPACE" >/dev/null 2>&1; then
  echo "namespace $NAMESPACE exists: another run is active or was not cleaned up" >&2
  exit 2
fi

# The harness at the pinned commit: catalog, thresholds, and the two plans.
HARNESS_DIR="$LEVEL3_DIR/.harness-$HARNESS_REF"
if [[ ! -d "$HARNESS_DIR" ]]; then
  git clone --quiet "$HARNESS_REPO" "$HARNESS_DIR.tmp"
  git -C "$HARNESS_DIR.tmp" checkout --quiet "$HARNESS_REF"
  mv "$HARNESS_DIR.tmp" "$HARNESS_DIR"
fi

# What the run is told, written into a directory that becomes its ConfigMap.
INPUTS="$(mktemp -d)"
trap 'rm -rf "$INPUTS"' EXIT
cp "$HARNESS_DIR/catalog.json" "$HARNESS_DIR/thresholds.json" "$INPUTS/"
cat >"$INPUTS/backends.yaml" <<EOF
schema_version: "1"
backends:
  - name: $BACKEND
    storage_class: $STORAGE_CLASS
    snapshot_class: $SNAPSHOT_CLASS
EOF

case "$STEP" in
  load-80) PLAN="level3-customer-load-80.yaml" ;;
  *) PLAN="level3-customer-no-load-80.yaml" ;;
esac
cp "$HARNESS_DIR/plans/$PLAN" "$INPUTS/$PLAN"

# The workers the no-load criteria disrupt: three distinct Ready, schedulable
# workers, the FAR target first when a FAR manifest names one.
# A jsonpath filter on spec.unschedulable drops every node without the field,
# which is nearly all of them, so the filtering is awk's.
workers="$(oc get nodes -l node-role.kubernetes.io/worker \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.unschedulable}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' |
  awk -F'\t' '$2 != "true" && $3 == "True" {print $1}' | sort)"
if [[ -n "${FAR_CONFIG:-}" && -z "${FAILURE_NODE:-}" ]]; then
  FAILURE_NODE="$(yq -r '.metadata.name' "$FAR_CONFIG")"
fi
pick_worker() {
  grep -v -x -F -e "${FAILURE_NODE:-}" -e "${DRAIN_NODE:-}" -e "${CEILING_NODE:-}" <<<"$workers" | tail -1 || true
}
FAILURE_NODE="${FAILURE_NODE:-$(pick_worker)}"
DRAIN_NODE="${DRAIN_NODE:-$(pick_worker)}"
CEILING_NODE="${CEILING_NODE:-$(pick_worker)}"
for node in "$FAILURE_NODE" "$DRAIN_NODE" "$CEILING_NODE"; do
  grep -q -x -F "$node" <<<"$workers" || {
    echo "\"$node\" is not a Ready, schedulable worker; three are needed" >&2
    exit 2
  }
done
if [[ "$(printf '%s\n' "$FAILURE_NODE" "$DRAIN_NODE" "$CEILING_NODE" | sort -u | wc -l)" -ne 3 ]]; then
  echo "FAILURE_NODE, DRAIN_NODE, and CEILING_NODE must be three different workers" >&2
  exit 2
fi

REHEARSAL=""
if [[ "$PLAN" == "level3-customer-no-load-80.yaml" ]]; then
  export FAILURE_NODE DRAIN_NODE CEILING_NODE
  export SNAPSHOT_REPLICAS="${SNAPSHOT_REPLICAS:-10}" SNAPSHOT_COUNT="${SNAPSHOT_COUNT:-10}"
  yq -i '
    .overrides["TR-VIRT-007"].node = strenv(FAILURE_NODE) |
    .overrides["TR-VIRT-008"].target_node = strenv(DRAIN_NODE) |
    .overrides["TR-VIRT-013"].node_name = strenv(CEILING_NODE) |
    .overrides["TR-VIRT-010"].replicas = (strenv(SNAPSHOT_REPLICAS) | tonumber) |
    .overrides["TR-VIRT-010"].snapshot_count = (strenv(SNAPSHOT_COUNT) | tonumber)
  ' "$INPUTS/$PLAN"
  if [[ -n "${ONLY:-}" ]]; then
    export ONLY
    yq -i '.select.ids = (strenv(ONLY) | split(","))' "$INPUTS/$PLAN"
    REHEARSAL="only ${ONLY}"
  fi
  if [[ -n "${SKIP:-}" ]]; then
    export SKIP
    yq -i '.select.ids |= map(select(. as $id | (strenv(SKIP) | split(",")) | contains([$id]) | not))' \
      "$INPUTS/$PLAN"
    REHEARSAL="${REHEARSAL:+$REHEARSAL; }without ${SKIP}"
  fi
  # Commented examples in the plan carry placeholders too; only live lines count.
  if grep -v '^[[:space:]]*#' "$INPUTS/$PLAN" | grep -q '<[A-Z_]*>'; then
    echo "the plan still has placeholders after filling it in:" >&2
    grep -v '^[[:space:]]*#' "$INPUTS/$PLAN" | grep '<[A-Z_]*>' >&2
    exit 2
  fi
fi
SELECTED="$(yq -r '.select.ids | join(",")' "$INPUTS/$PLAN")"

# TR-VIRT-007 fences FAILURE_NODE through its BMC; without a manifest that names
# that node it cannot run, and the harness would fail the whole profile.
FAR_NEEDED=""
if [[ ",$SELECTED," == *",TR-VIRT-007,"* ]]; then
  FAR_NEEDED=yes
  [[ -n "${FAR_CONFIG:-}" && -r "$FAR_CONFIG" ]] || {
    echo "TR-VIRT-007 is selected and needs FAR_CONFIG (a FenceAgentsRemediation manifest);" >&2
    echo "leave it out with SKIP=TR-VIRT-007 for a rehearsal without BMC access" >&2
    exit 2
  }
  [[ "$(yq -r '.metadata.name' "$FAR_CONFIG")" == "$FAILURE_NODE" ]] || {
    echo "FAR_CONFIG names $(yq -r '.metadata.name' "$FAR_CONFIG"), not FAILURE_NODE $FAILURE_NODE" >&2
    exit 2
  }
  cp "$FAR_CONFIG" "$INPUTS/far-config.json"
fi

# Run 2 continues from run 1's report.
if [[ "$STEP" == no-load-80 ]]; then
  prior="$CERT_RESULTS/load-80/report/report.json"
  [[ -s "$prior" ]] || { echo "no-load-80 continues from run 1; no report at $prior" >&2; exit 2; }
  cp "$prior" "$INPUTS/prior-report.json"
fi
if [[ "$STEP" == load-80 && -z "${EXTERNAL_LOAD:-}" ]]; then
  echo "load-80 measures latency under external load; say what holds it in EXTERNAL_LOAD" >&2
  echo "(EXTERNAL_LOAD=none for a rehearsal without load)" >&2
  exit 2
fi

# A step's earlier output is kept beside the new one, as the partner scripts do.
case "$STEP" in
  preflight) OUT="$LEVEL3_DIR/preflight" ;;
  *) OUT="$CERT_RESULTS/$STEP" ;;
esac
if [[ -d "$OUT" ]]; then
  mv "$OUT" "$OUT.$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$OUT"

VERSION="$(oc get clusterversion version -o jsonpath='{.status.desired.version}')"
{
  echo "OpenShift:       $VERSION"
  echo "step:            $STEP"
  echo "harness:         $HARNESS_REF"
  echo "harness image:   $HARNESS_IMAGE"
  echo "storage class:   $STORAGE_CLASS"
  echo "snapshot class:  $SNAPSHOT_CLASS"
  echo "plan:            $PLAN ($SELECTED)"
  if [[ "$PLAN" == "level3-customer-no-load-80.yaml" ]]; then
    echo "nodes:           fence $FAILURE_NODE, drain $DRAIN_NODE, ceiling $CEILING_NODE"
    echo "snapshot batch:  $SNAPSHOT_REPLICAS VMs, $SNAPSHOT_COUNT snapshots"
  fi
  if [[ "$STEP" == load-80 ]]; then
    echo "external load:   $EXTERNAL_LOAD"
  fi
  if [[ -n "$REHEARSAL" ]]; then
    echo "rehearsal:       $REHEARSAL (not a submission)"
  fi
} | tee "$OUT/run-info.txt"
cp "$INPUTS/backends.yaml" "$INPUTS/$PLAN" "$OUT/"

cleanup() {
  [[ -n "${LOG_PID:-}" ]] && kill "$LOG_PID" 2>/dev/null || true
  oc delete clusterrolebinding "$BINDING" --ignore-not-found >/dev/null || true
  if [[ -z "${KEEP_NAMESPACE:-}" ]]; then
    oc delete namespace "$NAMESPACE" --ignore-not-found --wait=false >/dev/null || true
  fi
  rm -rf "$INPUTS"
}
trap cleanup EXIT

oc create namespace "$NAMESPACE" >/dev/null
oc -n "$NAMESPACE" create serviceaccount harness >/dev/null
oc create clusterrolebinding "$BINDING" --clusterrole=cluster-admin \
  --serviceaccount="$NAMESPACE:harness" >/dev/null
oc -n "$NAMESPACE" create configmap inputs --from-file="$INPUTS" >/dev/null

# The harness's arguments, as ci/scripts/run-partner-cert.sh and
# run-partner-preflight.sh pass them.
ARGS=(--catalog /inputs/catalog.json --backends /inputs/backends.yaml --backend "$BACKEND")
if [[ "$STEP" == preflight ]]; then
  ARGS=(preflight "${ARGS[@]}" --plan "/inputs/$PLAN" --verbose)
else
  ARGS=(run "${ARGS[@]}" --thresholds /inputs/thresholds.json --plan "/inputs/$PLAN"
    --scenario "$STEP" --workdir /data/work --output /data/results --no-attestations --verbose)
  [[ "$STEP" == no-load-80 ]] && ARGS+=(--continue-from /inputs/prior-report.json)
fi
QUOTED=""
for a in "${ARGS[@]}"; do QUOTED+=" $(printf '%q' "$a")"; done
FAR_MOUNT=""
[[ -n "$FAR_NEEDED" ]] && FAR_MOUNT="    - {name: inputs, mountPath: /far-config.json, subPath: far-config.json}"

oc -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: harness
spec:
  serviceAccountName: harness
  restartPolicy: Never
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - {key: kubernetes.io/arch, operator: In, values: [amd64]}
          - {key: kubernetes.io/hostname, operator: NotIn, values: [$FAILURE_NODE, $DRAIN_NODE, $CEILING_NODE]}
  # With three workers, all three are the ones the harness disrupts, and what
  # is left are the control-plane nodes, which carry a NoSchedule taint. The
  # harness is a client of the API server and runs there as well as anywhere.
  tolerations:
  - {key: node-role.kubernetes.io/master, operator: Exists, effect: NoSchedule}
  - {key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}
  containers:
  - name: harness
    image: $HARNESS_IMAGE
    env:
    - {name: KUBECONFIG, value: /data/kubeconfig}
    command:
    - /bin/sh
    - -c
    - |
      sa=/var/run/secrets/kubernetes.io/serviceaccount
      mkdir -p /data/work /data/results
      cat >/data/kubeconfig <<KC
      apiVersion: v1
      kind: Config
      clusters:
      - name: in-cluster
        cluster: {server: https://kubernetes.default.svc, certificate-authority: \$sa/ca.crt}
      users:
      - name: harness
        user: {tokenFile: \$sa/token}
      contexts:
      - name: in-cluster
        context: {cluster: in-cluster, user: harness}
      current-context: in-cluster
      KC
      /usr/local/bin/container-entrypoint.sh$QUOTED
      echo \$? >/data/exit-code
      sleep 86400
    volumeMounts:
    - {name: data, mountPath: /data}
    - {name: inputs, mountPath: /inputs, readOnly: true}
$FAR_MOUNT
  volumes:
  - {name: data, emptyDir: {}}
  - name: inputs
    configMap: {name: inputs}
EOF

oc -n "$NAMESPACE" wait pod/harness --for=condition=Ready --timeout=15m >/dev/null
oc -n "$NAMESPACE" logs -f pod/harness >"$LOGS/run-$STEP.log" 2>&1 &
LOG_PID=$!
echo "running; log: $LOGS/run-$STEP.log"

until oc -n "$NAMESPACE" exec harness -- test -f /data/exit-code 2>/dev/null; do
  if [[ "$(oc -n "$NAMESPACE" get pod harness -o jsonpath='{.status.phase}')" != Running ]]; then
    echo "the harness pod stopped before the step finished" >&2
    exit 1
  fi
  sleep "$POLL_SECONDS"
done
status="$(oc -n "$NAMESPACE" exec harness -- cat /data/exit-code)"

# Preflight writes nothing but its log; a run's work and report come over as
# the harness lays them out.
if [[ "$STEP" != preflight ]]; then
  mkdir -p "$OUT/work" "$OUT/report"
  if ! fetch_results "$NAMESPACE" harness harness /data/work /data "$OUT/work" ||
    ! fetch_results "$NAMESPACE" harness harness /data/results /data "$OUT/report"; then
    KEEP_NAMESPACE=yes
    echo "the results could not be copied; the harness pod is kept in namespace $NAMESPACE" >&2
    echo "with /data/work and /data/results. Delete the namespace when done." >&2
    exit 1
  fi
fi
cp "$LOGS/run-$STEP.log" "$OUT/"
echo "$STEP: $OUT (harness exit $status)"
exit "$status"
