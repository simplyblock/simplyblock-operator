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
# including on an interrupt. The one exception is a run whose results could not
# be copied: the binding goes, and the namespace stays with the test pod and its
# results until someone deletes it. Results land in
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
#   STORAGE_CLASS    the StorageClass under test (default: the one class whose
#                    provisioner is csi.simplyblock.io)
#   SNAPSHOT_CLASS   its VolumeSnapshotClass (default: the one class whose driver
#                    is csi.simplyblock.io)
#   TESTS_IMAGE      override the tests image (default: oc adm release info --image-for=tests)
#   RESULTS_DIR      where run directories go (default: results/ next to this script)
#   LUN_STRESS_PODS  lower the LUN stress test's pod count (ocp-manifest.yaml) for
#                    a rehearsal on a cluster that cannot hold the full count; a
#                    run with it set is recorded as such and is not a submission
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
    LEAVE_OUT=""
    ;;
  block-multinode)
    MANIFEST="$SCRIPT_DIR/manifest-block-multinode.yaml"
    RUN_FILTER='--run=\(block volmode\)'
    # A block pattern whose test also claims a filesystem volume, which this
    # profile asks for ReadWriteMany like every claim. simplyblock serves that
    # only from a pNFS class, so the claim is refused by design; the main
    # profile runs the same test with ReadWriteOnce.
    LEAVE_OUT='should access to two volumes with different volume mode'
    LEAVE_OUT_WHY='claims a ReadWriteMany filesystem volume, served only by a pNFS class; runs in the main profile'
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

# shellcheck source=../lib/classes.sh
source "$SCRIPT_DIR/../lib/classes.sh"
if [[ -z "${STORAGE_CLASS:-}" ]]; then
  STORAGE_CLASS="$(find_class storageclass provisioner)" || {
    echo "set STORAGE_CLASS" >&2
    exit 2
  }
fi
if [[ -z "${SNAPSHOT_CLASS:-}" ]]; then
  SNAPSHOT_CLASS="$(find_class volumesnapshotclass driver)" || {
    echo "set SNAPSHOT_CLASS" >&2
    exit 2
  }
fi

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
if [[ -n "${LUN_STRESS_PODS:-}" ]]; then
  [[ "$LUN_STRESS_PODS" =~ ^[1-9][0-9]*$ ]] || { echo "LUN_STRESS_PODS must be a positive number" >&2; exit 2; }
  sed -e "s|^\(  PodsTotal:\).*|\1 $LUN_STRESS_PODS|" "$SCRIPT_DIR/ocp-manifest.yaml" >"$RUN_DIR/ocp-manifest.yaml"
else
  cp "$SCRIPT_DIR/ocp-manifest.yaml" "$RUN_DIR/ocp-manifest.yaml"
fi

{
  echo "OpenShift:      $VERSION"
  echo "tests image:    $TESTS_IMAGE"
  echo "profile:        $PROFILE"
  echo "storage class:  $STORAGE_CLASS"
  echo "snapshot class: $SNAPSHOT_CLASS"
  echo "extra args:     ${EXTRA_ARGS[*]-}"
  if [[ -n "$LEAVE_OUT" ]]; then
    echo "left out:       tests matching \"$LEAVE_OUT\" ($LEAVE_OUT_WHY)"
  fi
  echo "LUN stress:     $(sed -n 's/^  PodsTotal: //p' "$RUN_DIR/ocp-manifest.yaml") pods${LUN_STRESS_PODS:+ (lowered: rehearsal, not a submission)}"
} | tee "$RUN_DIR/run-info.txt"

LOG_PID=""
KEEP_NAMESPACE=""
cleanup() {
  [[ -n "$LOG_PID" ]] && kill "$LOG_PID" 2>/dev/null || true
  oc delete clusterrolebinding "$BINDING" --ignore-not-found >/dev/null || true
  if [[ -z "$KEEP_NAMESPACE" ]]; then
    oc delete namespace "$NAMESPACE" --ignore-not-found --wait=false >/dev/null || true
  fi
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
FILTER=""
[[ -n "$RUN_FILTER" ]] && FILTER=" $(printf '%q' "$RUN_FILTER")"
EXTRA=""
for a in ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}; do
  EXTRA+=" $(printf '%q' "$a")"
done

# A profile that leaves tests out runs from a list instead of the suite: the
# suite's own dry run under the same filter, less the tests left out, which
# matches a name in every pattern it appears in. The list lands in the results,
# so a run shows exactly what it covered.
if [[ -n "$LEAVE_OUT" ]]; then
  SUITE="openshift-tests run openshift/csi --dry-run$FILTER >/scratch/suite-tests.txt"
  # The dry run prints its log on the same stream; a test name is a quoted line.
  SUITE+=" && grep '^\"' /scratch/suite-tests.txt | grep -v -F $(printf '%q' "$LEAVE_OUT") >/results/tests.txt"
  SUITE+=" && openshift-tests run --junit-dir /results/junit -f /results/tests.txt$EXTRA"
else
  SUITE="openshift-tests run openshift/csi --junit-dir /results/junit$FILTER$EXTRA"
fi

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
    - {name: HOME, value: /scratch}
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
      $SUITE
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

status="$(oc -n "$NAMESPACE" exec openshift-tests -- cat /results/exit-code)"

# The results come over as one archive with its checksum, because a stream
# through oc exec can end early without an error from either side: a tar piped
# straight through lost the end of a 250 MB result set that way, exit code
# included. A copy that does not match is fetched again.
sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}
oc -n "$NAMESPACE" exec openshift-tests -- \
  sh -c 'tar czf /scratch/results.tar.gz -C /results . && sha256sum /scratch/results.tar.gz' \
  >"$RUN_DIR/results.sha256"
want="$(cut -d' ' -f1 "$RUN_DIR/results.sha256")"
copied=""
for attempt in 1 2 3; do
  oc -n "$NAMESPACE" exec openshift-tests -- cat /scratch/results.tar.gz >"$RUN_DIR/results.tar.gz" || true
  if [[ "$(sha256 "$RUN_DIR/results.tar.gz")" == "$want" ]]; then
    copied=yes
    break
  fi
  echo "results copy $attempt did not match its checksum" >&2
done
if [[ -z "$copied" ]]; then
  KEEP_NAMESPACE=yes
  echo "the results could not be copied; the test pod is kept in namespace $NAMESPACE," >&2
  echo "with the archive at /scratch/results.tar.gz. Delete the namespace when done." >&2
  echo "openshift-tests exit $status" >&2
  exit 1
fi
tar xzf "$RUN_DIR/results.tar.gz" -C "$RUN_DIR"
rm "$RUN_DIR/results.tar.gz" "$RUN_DIR/results.sha256"
echo "results: $RUN_DIR (openshift-tests exit $status)"
exit "$status"
