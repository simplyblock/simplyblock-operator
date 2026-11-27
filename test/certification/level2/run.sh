#!/usr/bin/env bash
# Runs the Level 2 certification test, OpenShift Virtualization's KubeVirt
# storage checkup, against the cluster KUBECONFIG points at, and keeps what Red
# Hat asks to be submitted: the checkup's result ConfigMap.
#
# The checkup judges the cluster's storage setup for virtual machines as a whole:
# every StorageProfile, every golden image, the default classes, and then a VM
# that boots from a golden image cloned into the class under test, is
# hot-plugged a disk, migrates live, and is snapshotted and restored. Two of
# those depend on cluster setup rather than on the driver, and the script
# prepares both before the checkup starts:
#
#   - The class under test becomes the default virtualization class
#     (storageclass.kubevirt.io/is-default-virt-class), which is where the golden
#     images are imported and what the checkup tests. A cluster whose default is
#     another class is refused rather than changed: that is somebody's choice.
#   - Every DataImportCron has to be up to date. The checkup fails on one that is
#     not, and skips every VM test when there is no golden image at all, so the
#     script waits for at least one, all up to date, and stops when they are not
#     by GOLDEN_IMAGE_WAIT.
#
# The checkup image is the one the installed OpenShift Virtualization operator
# ships, so the test always matches the version under test.
#
# For the length of a run the script keeps a namespace with the checkup's
# ServiceAccount and Role, and a ClusterRoleBinding to cluster-reader, which the
# checkup needs; all of it is removed on exit, including on an interrupt.
# Results land in $RESULTS_DIR/level2-<UTC time>/: run-info.txt, the result
# ConfigMap, the checkup's log, and the cluster facts Red Hat asks for.
#
# Usage:
#   KUBECONFIG=/abs/path ./run.sh
#
# Environment:
#   KUBECONFIG         required; a cluster-admin kubeconfig
#   STORAGE_CLASS      the StorageClass under test (default: the one class whose
#                      provisioner is csi.simplyblock.io)
#   CHECKUP_IMAGE      override the checkup image (default: the one the
#                      installed OpenShift Virtualization operator lists)
#   CHECKUP_TIMEOUT    the checkup's own deadline (default 30m)
#   VMI_TIMEOUT        the deadline of each VM operation (default 5m)
#   GOLDEN_IMAGE_WAIT  seconds to wait for the golden images (default 2400)
#   RESULTS_DIR        where run directories go (default: results/ next to this script)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="simplyblock-cert-level2"
BINDING="simplyblock-cert-level2-reader"
POLL_SECONDS=20
VIRT_DEFAULT="storageclass.kubevirt.io/is-default-virt-class"

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
oc get storageclass "$STORAGE_CLASS" >/dev/null

CNV_CSV="$(oc get csv -n openshift-cnv -o name | grep kubevirt-hyperconverged-operator || true)"
if [[ -z "$CNV_CSV" ]]; then
  echo "OpenShift Virtualization is not installed (no kubevirt-hyperconverged-operator in openshift-cnv)" >&2
  exit 2
fi
if [[ -z "${CHECKUP_IMAGE:-}" ]]; then
  CHECKUP_IMAGE="$(oc get -n openshift-cnv "$CNV_CSV" -o jsonpath='{.spec.relatedImages[*].image}' |
    tr ' ' '\n' | grep 'kubevirt-storage-checkup' | head -1)"
  [[ -n "$CHECKUP_IMAGE" ]] || { echo "$CNV_CSV lists no storage checkup image; set CHECKUP_IMAGE" >&2; exit 2; }
fi
CHECKUP_TIMEOUT="${CHECKUP_TIMEOUT:-30m}"
VMI_TIMEOUT="${VMI_TIMEOUT:-5m}"
GOLDEN_IMAGE_WAIT="${GOLDEN_IMAGE_WAIT:-2400}"

# One run at a time: the checkup boots VMs and would measure another run's load.
if oc get namespace "$NAMESPACE" >/dev/null 2>&1; then
  echo "namespace $NAMESPACE exists: another run is active or was not cleaned up" >&2
  exit 2
fi

# The default virtualization class decides where golden images go and what the
# checkup tests, so it has to be the class under test.
virt_defaults="$(oc get storageclass \
  -o jsonpath="{range .items[?(@.metadata.annotations.storageclass\.kubevirt\.io/is-default-virt-class==\"true\")]}{.metadata.name}{\"\\n\"}{end}")"
case "$virt_defaults" in
  "")
    oc annotate storageclass "$STORAGE_CLASS" "$VIRT_DEFAULT=true" >/dev/null
    VIRT_DEFAULT_STATE="set on $STORAGE_CLASS by this run"
    ;;
  "$STORAGE_CLASS")
    VIRT_DEFAULT_STATE="$STORAGE_CLASS, already"
    ;;
  *)
    echo "the default virtualization class is $(tr '\n' ' ' <<<"$virt_defaults")rather than $STORAGE_CLASS;" >&2
    echo "golden images and the checkup would test that class. Move the $VIRT_DEFAULT annotation first." >&2
    exit 2
    ;;
esac

# Golden images, in every namespace, up to date before the checkup looks. No
# DataImportCron at all is not "all up to date": without a golden image the
# checkup skips every VM test, so a run that has none, or still has a stale one
# at the deadline, stops rather than producing a result that tested no VM.
stale_golden_images() {
  local crons
  crons="$(oc get dataimportcron -A \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.status.conditions[?(@.type=="UpToDate")].status}{"\n"}{end}')"
  if [[ -z "$crons" ]]; then
    echo "(no DataImportCron yet)"
    return 0
  fi
  awk '$2 != "True" {print $1}' <<<"$crons"
}
deadline=$((SECONDS + GOLDEN_IMAGE_WAIT))
stale="$(stale_golden_images)"
while [[ -n "$stale" && $SECONDS -lt $deadline ]]; do
  echo "waiting for golden images: $(tr '\n' ' ' <<<"$stale")"
  sleep "$POLL_SECONDS"
  stale="$(stale_golden_images)"
done
if [[ -n "$stale" ]]; then
  echo "golden images not ready after ${GOLDEN_IMAGE_WAIT}s: $(tr '\n' ' ' <<<"$stale")" >&2
  echo "the checkup would skip its VM tests; not running it" >&2
  exit 2
fi

VERSION="$(oc get clusterversion version -o jsonpath='{.status.desired.version}')"
CNV_VERSION="$(oc get -n openshift-cnv "$CNV_CSV" -o jsonpath='{.spec.version}')"
RUN_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results}/level2-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RUN_DIR"
{
  echo "OpenShift:         $VERSION"
  echo "Virtualization:    $CNV_VERSION"
  echo "checkup image:     $CHECKUP_IMAGE"
  echo "storage class:     $STORAGE_CLASS"
  echo "virt default:      $VIRT_DEFAULT_STATE"
  echo "golden images:     all up to date"
  echo "checkup timeout:   $CHECKUP_TIMEOUT (VM operations $VMI_TIMEOUT)"
} | tee "$RUN_DIR/run-info.txt"

cleanup() {
  oc delete clusterrolebinding "$BINDING" --ignore-not-found >/dev/null || true
  oc delete namespace "$NAMESPACE" --ignore-not-found --wait=false >/dev/null || true
}
trap cleanup EXIT

oc create namespace "$NAMESPACE" >/dev/null
oc create clusterrolebinding "$BINDING" --clusterrole=cluster-reader \
  --serviceaccount="$NAMESPACE:storage-checkup-sa" >/dev/null

# The checkup's permissions: the union of what OpenShift Virtualization's
# documentation lists and what the checkup's current source needs (it stops,
# patches, snapshots, and restores the VM under test), since which of the two a
# given release's image is cannot be told from outside.
oc -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: storage-checkup-sa
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: storage-checkup-role
rules:
- apiGroups: [""]
  resources: [configmaps]
  verbs: [get, update]
- apiGroups: [kubevirt.io]
  resources: [virtualmachines]
  verbs: [create, delete, get, patch]
- apiGroups: [kubevirt.io]
  resources: [virtualmachineinstances]
  verbs: [get]
- apiGroups: [subresources.kubevirt.io]
  resources: [virtualmachineinstances/addvolume, virtualmachineinstances/removevolume, virtualmachines/stop]
  verbs: [update]
- apiGroups: [kubevirt.io]
  resources: [virtualmachineinstancemigrations]
  verbs: [create]
- apiGroups: [cdi.kubevirt.io]
  resources: [datavolumes]
  verbs: [create, delete]
- apiGroups: [""]
  resources: [persistentvolumeclaims]
  verbs: [delete]
- apiGroups: [snapshot.kubevirt.io]
  resources: [virtualmachinesnapshots, virtualmachinerestores]
  verbs: [create, get, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: storage-checkup-role
subjects:
- kind: ServiceAccount
  name: storage-checkup-sa
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: storage-checkup-role
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: storage-checkup-config
data:
  spec.timeout: $CHECKUP_TIMEOUT
  spec.param.storageClass: $STORAGE_CLASS
  spec.param.vmiTimeout: $VMI_TIMEOUT
---
apiVersion: batch/v1
kind: Job
metadata:
  name: storage-checkup
spec:
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: storage-checkup-sa
      restartPolicy: Never
      containers:
      - name: storage-checkup
        image: $CHECKUP_IMAGE
        env:
        - {name: CONFIGMAP_NAMESPACE, value: $NAMESPACE}
        - {name: CONFIGMAP_NAME, value: storage-checkup-config}
EOF
echo "running the storage checkup in $NAMESPACE"

# The Job ends Complete or Failed; the checkup's own deadline bounds both, and a
# Job still running well past it is reported rather than waited on forever.
job_end() {
  oc -n "$NAMESPACE" get job storage-checkup \
    -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{"\n"}{end}' | grep -E '^(Complete|Failed)$' || true
}
deadline=$((SECONDS + 3600 * 2))
until [[ -n "$(job_end)" ]]; do
  if [[ $SECONDS -gt $deadline ]]; then
    echo "the checkup Job did not end within two hours" >&2
    break
  fi
  sleep "$POLL_SECONDS"
done

oc -n "$NAMESPACE" get configmap storage-checkup-config -o yaml >"$RUN_DIR/storage-checkup-config.yaml"
oc -n "$NAMESPACE" logs job/storage-checkup >"$RUN_DIR/checkup.log" 2>&1 || true
oc get clusterversion -o yaml >"$RUN_DIR/clusterversion.yaml"
oc get -n openshift-cnv "$CNV_CSV" -o yaml >"$RUN_DIR/virtualization-csv.yaml"
oc get storageprofile -o yaml >"$RUN_DIR/storageprofiles.yaml"

succeeded="$(oc -n "$NAMESPACE" get configmap storage-checkup-config -o jsonpath='{.data.status\.succeeded}')"
reason="$(oc -n "$NAMESPACE" get configmap storage-checkup-config -o jsonpath='{.data.status\.failureReason}')"
echo "results: $RUN_DIR (succeeded: ${succeeded:-unknown})"
[[ -n "$reason" ]] && echo "failure reason: $reason"
[[ "$succeeded" == "true" ]]
