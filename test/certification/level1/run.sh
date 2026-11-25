#!/usr/bin/env bash
# Runs the Level 1 certification suite (openshift-tests run openshift/csi) inside
# the cluster KUBECONFIG points at, as a Pod running the `tests` image of that
# cluster's own release payload, so the test binary always matches the
# OpenShift version under test.
#
# It runs in the cluster rather than in a local container because
# openshift-tests only runs on the cluster's architecture: it extracts further
# test binaries from the release payload and refuses ones of another
# architecture, and an amd64 binary emulated on an ARM64 workstation crashes.
# A Pod works the same from a laptop and from a CI runner, and needs only oc.
#
# Two profiles, one per manifest (see manifest.yaml for why there are two):
#   main              every test pattern, manifest.yaml
#   block-multinode   ReadWriteMany and ReadOnlyMany, block patterns only,
#                     manifest-block-multinode.yaml
#
# For the length of a run the script keeps a namespace with a ServiceAccount
# bound to cluster-admin, which openshift-tests needs; both are removed on exit,
# including on an interrupt. Results land in
# $RESULTS_DIR/level1-<profile>-<UTC time>/: the rendered manifests,
# run-info.txt, the full log, the JUnit files under junit/, the monitor output,
# and the suite's exit code.
#
# Usage:
#   KUBECONFIG=/abs/path ./run.sh [main|block-multinode] [-- <openshift-tests args>]
#   for example, ./run.sh main -- --dry-run
#
# Environment:
#   KUBECONFIG       required; a cluster-admin kubeconfig
#   STORAGE_CLASS    the StorageClass under test (default simplyblock-csi-sc)
#   SNAPSHOT_CLASS   its VolumeSnapshotClass (default simplyblock-csi-snapshotclass)
#   TESTS_IMAGE      override the tests image (default: oc adm release info --image-for=tests)
#   RESULTS_DIR      where run directories go (default: results/ next to this script)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="simplyblock-cert-level1"
BINDING="simplyblock-cert-level1"
POLL_SECONDS=30

PROFILE="${1:-main}"
shift || true
if [[ "${1:-}" == "--" ]]; then
  shift
fi
EXTRA_ARGS=("$@")

case "$PROFILE" in
  main)
    MANIFEST="$SCRIPT_DIR/manifest.yaml"
    RUN_FILTER=""
    ;;
  block-multinode)
    MANIFEST="$SCRIPT_DIR/manifest-block-multinode.yaml"
    RUN_FILTER='--run=\(block volmode\)'
    ;;
  *)
    echo "unknown profile '$PROFILE'; use main or block-multinode" >&2
    exit 2
    ;;
esac

if [[ -z "${KUBECONFIG:-}" || ! -r "$KUBECONFIG" ]]; then
  echo "KUBECONFIG must name a readable kubeconfig" >&2
  exit 2
fi
command -v oc >/dev/null || { echo "oc is not on PATH" >&2; exit 2; }

STORAGE_CLASS="${STORAGE_CLASS:-simplyblock-csi-sc}"
SNAPSHOT_CLASS="${SNAPSHOT_CLASS:-simplyblock-csi-snapshotclass}"

# What is under test has to exist before an hour of tests finds out it does not.
oc get storageclass "$STORAGE_CLASS" >/dev/null
oc get volumesnapshotclass "$SNAPSHOT_CLASS" >/dev/null
# One run at a time: the LUN stress test expects nothing else beside it.
if oc get namespace "$NAMESPACE" >/dev/null 2>&1; then
  echo "namespace $NAMESPACE exists: another run is active or was not cleaned up" >&2
  exit 2
fi

TESTS_IMAGE="${TESTS_IMAGE:-$(oc adm release info --image-for=tests)}"
VERSION="$(oc get clusterversion version -o jsonpath='{.status.desired.version}')"

RUN_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results}/level1-$PROFILE-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RUN_DIR"
sed -e "s|\${STORAGE_CLASS}|$STORAGE_CLASS|g" -e "s|\${SNAPSHOT_CLASS}|$SNAPSHOT_CLASS|g" \
  "$MANIFEST" >"$RUN_DIR/manifest.yaml"
cp "$SCRIPT_DIR/ocp-manifest.yaml" "$RUN_DIR/ocp-manifest.yaml"

{
  echo "OpenShift:      $VERSION"
  echo "tests image:    $TESTS_IMAGE"
  echo "profile:        $PROFILE"
  echo "storage class:  $STORAGE_CLASS"
  echo "snapshot class: $SNAPSHOT_CLASS"
  echo "extra args:     ${EXTRA_ARGS[*]-}"
} | tee "$RUN_DIR/run-info.txt"

LOG_PID=""
cleanup() {
  [[ -n "$LOG_PID" ]] && kill "$LOG_PID" 2>/dev/null || true
  oc delete clusterrolebinding "$BINDING" --ignore-not-found >/dev/null || true
  oc delete namespace "$NAMESPACE" --ignore-not-found --wait=false >/dev/null || true
}
trap cleanup EXIT

oc create namespace "$NAMESPACE" >/dev/null
oc -n "$NAMESPACE" create serviceaccount openshift-tests >/dev/null
oc create clusterrolebinding "$BINDING" --clusterrole=cluster-admin \
  --serviceaccount="$NAMESPACE:openshift-tests" >/dev/null
oc -n "$NAMESPACE" create configmap manifests \
  --from-file=manifest.yaml="$RUN_DIR/manifest.yaml" \
  --from-file=ocp-manifest.yaml="$RUN_DIR/ocp-manifest.yaml" >/dev/null

# The suite's arguments, quoted for the container's shell.
ARGS="run openshift/csi --junit-dir /results/junit"
[[ -n "$RUN_FILTER" ]] && ARGS+=" $(printf '%q' "$RUN_FILTER")"
for a in ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}; do
  ARGS+=" $(printf '%q' "$a")"
done

# The Pod builds a kubeconfig from its ServiceAccount token, runs the suite in
# /results (openshift-tests writes its monitor output into its working
# directory), records the exit code, and stays up until the results are
# collected.
oc -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: openshift-tests
spec:
  serviceAccountName: openshift-tests
  restartPolicy: Never
  containers:
  - name: tests
    image: $TESTS_IMAGE
    env:
    - {name: HOME, value: /results}
    - {name: TMPDIR, value: /scratch}
    - {name: KUBECONFIG, value: /scratch/kubeconfig}
    - {name: TEST_CSI_DRIVER_FILES, value: /manifests/manifest.yaml}
    - {name: TEST_OCP_CSI_DRIVER_FILES, value: /manifests/ocp-manifest.yaml}
    command:
    - /bin/bash
    - -c
    - |
      sa=/var/run/secrets/kubernetes.io/serviceaccount
      cat >/scratch/kubeconfig <<KC
      apiVersion: v1
      kind: Config
      clusters:
      - name: in-cluster
        cluster: {server: https://kubernetes.default.svc, certificate-authority: \$sa/ca.crt}
      users:
      - name: openshift-tests
        user: {tokenFile: \$sa/token}
      contexts:
      - name: in-cluster
        context: {cluster: in-cluster, user: openshift-tests}
      current-context: in-cluster
      KC
      cd /results
      openshift-tests $ARGS
      echo \$? >/results/exit-code
      sleep 86400
    volumeMounts:
    - {name: results, mountPath: /results}
    - {name: scratch, mountPath: /scratch}
    - {name: manifests, mountPath: /manifests}
  volumes:
  - {name: results, emptyDir: {}}
  - {name: scratch, emptyDir: {}}
  - name: manifests
    configMap: {name: manifests}
EOF

oc -n "$NAMESPACE" wait pod/openshift-tests --for=condition=Ready --timeout=15m >/dev/null
oc -n "$NAMESPACE" logs -f pod/openshift-tests >"$RUN_DIR/openshift-tests.log" 2>&1 &
LOG_PID=$!
echo "running; log: $RUN_DIR/openshift-tests.log"

until oc -n "$NAMESPACE" exec openshift-tests -- test -f /results/exit-code 2>/dev/null; do
  if [[ "$(oc -n "$NAMESPACE" get pod openshift-tests -o jsonpath='{.status.phase}')" != Running ]]; then
    echo "the test pod stopped before the suite finished" >&2
    exit 1
  fi
  sleep "$POLL_SECONDS"
done

oc -n "$NAMESPACE" exec openshift-tests -- tar cf - -C /results . | tar xf - -C "$RUN_DIR"
status="$(cat "$RUN_DIR/exit-code")"
echo "results: $RUN_DIR (openshift-tests exit $status)"
exit "$status"
