#!/usr/bin/env bash

set -euo pipefail

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS] [NAMESPACE]

Remove all simplyblock resources from a Kubernetes cluster.

Arguments:
  NAMESPACE                Target namespace (default: simplyblock)

Options:
  -f, --force              Skip interactive confirmation
      --helm-release NAME  Helm release to uninstall (default: simplyblock-operator,
                           or \$HELM_RELEASE)
      --csi-driver NAME    CSI driver name whose PVs are cleaned up
                           (default: csi.simplyblock.io, or \$CSI_DRIVER)
  -h, --help               Show this help message
EOF
    exit 0
}

# Abort with usage if an option that takes a value was given none.
# $1 = option name (for the message), $2 = the value passed (may be empty).
require_value() {
    if [[ -z "$2" ]]; then
        echo "Option $1 requires a value" >&2
        usage
    fi
}

FORCE=false
NAMESPACE=""
HELM_RELEASE_ARG=""
CSI_DRIVER_ARG=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        -f|--force)
            FORCE=true
            shift
            ;;
        -h|--help)
            usage
            ;;
        --helm-release)
            require_value "$1" "${2:-}"
            HELM_RELEASE_ARG="$2"
            shift 2
            ;;
        --helm-release=*)
            HELM_RELEASE_ARG="${1#*=}"
            shift
            ;;
        --csi-driver)
            require_value "$1" "${2:-}"
            CSI_DRIVER_ARG="$2"
            shift 2
            ;;
        --csi-driver=*)
            CSI_DRIVER_ARG="${1#*=}"
            shift
            ;;
        -*)
            echo "Unknown option: $1" >&2
            usage
            ;;
        *)
            if [[ -z "$NAMESPACE" ]]; then
                NAMESPACE="$1"
            else
                echo "Unexpected argument: $1" >&2
                usage
            fi
            shift
            ;;
    esac
done

# Precedence for HELM_RELEASE / CSI_DRIVER: CLI flag > environment variable > default.
NAMESPACE="${NAMESPACE:-simplyblock}"
HELM_RELEASE="${HELM_RELEASE_ARG:-${HELM_RELEASE:-simplyblock-operator}}"
CRD_GROUP="storage.simplyblock.io"
CSI_DRIVER="${CSI_DRIVER_ARG:-${CSI_DRIVER:-csi.simplyblock.io}}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()    { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; }
section() { echo -e "\n${YELLOW}=== $* ===${NC}"; }


if command -v kubectl &>/dev/null; then
    KUBECTL="kubectl"
elif command -v oc &>/dev/null; then
    KUBECTL="oc"
    info "kubectl not found, using oc"
else
    error "Neither kubectl nor oc found. Please install one and try again."
    exit 1
fi

# ---------------------------------------------------------------------------
# Interactive confirmation
# ---------------------------------------------------------------------------
CURRENT_CONTEXT=$($KUBECTL config current-context 2>/dev/null || echo "<unknown>")
CLUSTER_NAME=$($KUBECTL config view -o jsonpath="{.contexts[?(@.name==\"${CURRENT_CONTEXT}\")].context.cluster}" 2>/dev/null || echo "<unknown>")

section "Cleanup target"
echo -e "  Context:      ${RED}${CURRENT_CONTEXT}${NC}"
echo -e "  Cluster:      ${RED}${CLUSTER_NAME}${NC}"
echo -e "  Namespace:    ${RED}${NAMESPACE}${NC}"
echo -e "  Helm release: ${RED}${HELM_RELEASE}${NC}"
echo -e "  CSI driver:   ${RED}${CSI_DRIVER}${NC}"
echo ""

if [[ "$FORCE" != true ]]; then
    if [[ ! -t 0 ]]; then
        error "This script requires interactive input but stdin is not a terminal."
        error "It looks like you are piping this script (e.g. curl | bash)."
        error "Please download and run it directly, or pass --force / -f to skip confirmation:"
        error "  curl -sO https://raw.githubusercontent.com/simplyblock/helm-charts/main/scripts/cleanup-simplyblock.sh"
        error "  bash cleanup-simplyblock.sh [NAMESPACE]"
        error "Or, if you accept the risks:  curl -s ... | bash -s -- -f [NAMESPACE]"
        exit 1
    fi
    warn "This will permanently delete all simplyblock resources in the above context and namespace."
    read -r -p "$(echo -e "${YELLOW}Continue? [y/N]:${NC} ")" answer
    case "$answer" in
        [yY]|[yY][eE][sS]) ;;
        *)
            info "Aborted."
            exit 0
            ;;
    esac
fi

# Prints the pods in the namespace that mount a volume of $CSI_DRIVER, one per
# line as `name owner-kind owner-name`. Kubelet can only unmount such a volume
# through the driver's node plugin, so these pods must be gone before the plugin
# is: once it is uninstalled they stay Terminating, and the namespace with them.
pods_on_csi_volumes() {
    local pods pvcs pvs
    pods=$($KUBECTL get pods -n "$NAMESPACE" -o json 2>/dev/null) || return 0
    pvcs=$($KUBECTL get pvc -n "$NAMESPACE" -o json 2>/dev/null) || pvcs='{"items":[]}'
    pvs=$($KUBECTL get pv -o json 2>/dev/null) || pvs='{"items":[]}'
    PODS="$pods" PVCS="$pvcs" PVS="$pvs" DRIVER="$CSI_DRIVER" python3 -c '
import json, os
driver = os.environ["DRIVER"]
on_driver = {p["metadata"]["name"] for p in json.loads(os.environ["PVS"])["items"]
             if (p["spec"].get("csi") or {}).get("driver") == driver}
claims = {c["metadata"]["name"] for c in json.loads(os.environ["PVCS"])["items"]
          if c["spec"].get("volumeName") in on_driver}
for p in json.loads(os.environ["PODS"])["items"]:
    used = {(v.get("persistentVolumeClaim") or {}).get("claimName") for v in p["spec"].get("volumes", [])}
    if used & claims or any(v.get("csi", {}).get("driver") == driver for v in p["spec"].get("volumes", [])):
        owner = next((o for o in p["metadata"].get("ownerReferences", []) if o.get("controller")), {})
        print(p["metadata"]["name"], owner.get("kind", "-"), owner.get("name", "-"))
'
}

# ---------------------------------------------------------------------------
# 0. Release simplyblock volumes while the CSI node plugin still runs
# ---------------------------------------------------------------------------
section "Releasing simplyblock volumes mounted in namespace '$NAMESPACE'"

# The operator recreates what it manages (the pNFS metadata server's
# StatefulSet among it), so it stops first.
operator_deploys=$($KUBECTL get deployment -n "$NAMESPACE" -l app=simplyblock-operator \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
for deploy in $operator_deploys; do
    info "Scaling the operator Deployment $deploy to 0..."
    $KUBECTL scale deployment "$deploy" -n "$NAMESPACE" --replicas=0 2>/dev/null || \
        warn "Could not scale $deploy to 0"
done
if [[ -n "$operator_deploys" ]]; then
    $KUBECTL wait pod -n "$NAMESPACE" -l app=simplyblock-operator \
        --for=delete --timeout=60s 2>/dev/null || true
fi

csi_pods=$(pods_on_csi_volumes)
if [[ -z "$csi_pods" ]]; then
    info "No pod in '$NAMESPACE' mounts a $CSI_DRIVER volume."
else
    # A pod's controller would replace a deleted pod, so StatefulSets go
    # first, and then any pod left.
    for sts in $(echo "$csi_pods" | awk '$2 == "StatefulSet" {print $3}' | sort -u); do
        info "Deleting StatefulSet $sts, whose pods mount $CSI_DRIVER volumes..."
        $KUBECTL delete statefulset "$sts" -n "$NAMESPACE" --ignore-not-found \
            --wait=false 2>/dev/null || warn "Could not delete StatefulSet $sts"
    done
    for pod in $(echo "$csi_pods" | awk '{print $1}'); do
        $KUBECTL delete pod "$pod" -n "$NAMESPACE" --ignore-not-found \
            --wait=false 2>/dev/null || true
    done
    for pod in $(echo "$csi_pods" | awk '{print $1}'); do
        if ! $KUBECTL wait pod "$pod" -n "$NAMESPACE" --for=delete --timeout=180s 2>/dev/null; then
            warn "Pod $pod still exists after 180s. Its $CSI_DRIVER volume may not unmount."
        fi
    done
fi

# ---------------------------------------------------------------------------
# 1. Helm uninstall
# ---------------------------------------------------------------------------
section "Helm uninstall"

if helm list -n "$NAMESPACE" | grep -q "^${HELM_RELEASE}\b"; then
    info "Uninstalling helm release '$HELM_RELEASE' from namespace '$NAMESPACE'..."
    helm uninstall "$HELM_RELEASE" -n "$NAMESPACE" --wait --timeout 120s
    info "Helm uninstall complete."
else
    warn "Helm release '$HELM_RELEASE' not found in namespace '$NAMESPACE', skipping."
fi

# ---------------------------------------------------------------------------
# 2. Remove CR finalizers and delete CRs
# ---------------------------------------------------------------------------
section "Removing CRs and finalizers"

CRDS=$($KUBECTL get crd -o name 2>/dev/null | grep "$CRD_GROUP" | sed 's|customresourcedefinition.apiextensions.k8s.io/||')

for crd in $CRDS; do
    resources=$($KUBECTL get "$crd" -n "$NAMESPACE" --ignore-not-found -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    if [[ -z "$resources" ]]; then
        continue
    fi
    info "Processing CR: $crd"
    for name in $resources; do
        info "  Removing finalizers from $crd/$name..."
        $KUBECTL patch "$crd" "$name" -n "$NAMESPACE" \
            --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || \
            warn "  Could not patch finalizers on $crd/$name"
        # The delete's own error is reported rather than discarded. A CR that
        # will not go is as often refused by a validating webhook as held by a
        # finalizer, and the two are indistinguishable from the final count
        # alone: the wipe above says "no change" either way.
        if ! delete_error=$($KUBECTL delete "$crd" "$name" -n "$NAMESPACE" \
            --ignore-not-found --timeout=30s 2>&1); then
            warn "  Could not delete $crd/$name: $delete_error"
        fi
    done
done


for crd in $CRDS; do
    names=$($KUBECTL get "$crd" -n "$NAMESPACE" --ignore-not-found \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    for name in $names; do
        warn "  $crd/$name still present, force-wiping finalizers..."
        $KUBECTL patch "$crd" "$name" -n "$NAMESPACE" \
            --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || \
            warn "  Could not patch finalizers on $crd/$name"
        if ! delete_error=$($KUBECTL delete "$crd" "$name" -n "$NAMESPACE" \
            --ignore-not-found --force --grace-period=0 2>&1); then
            warn "  Could not delete $crd/$name: $delete_error"
        fi
    done
done

# Final check
sleep 3
all_clear=true
for crd in $CRDS; do
    remaining=$($KUBECTL get "$crd" -n "$NAMESPACE" --ignore-not-found \
        --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$remaining" -gt 0 ]]; then
        error "  $crd: $remaining resource(s) still present!"
        all_clear=false
    else
        info "  $crd: clean"
    fi
done

if $all_clear; then
    info "All CRs removed successfully."
else
    error "Some CRs could not be removed. Manual intervention may be required."
    exit 1
fi

# ---------------------------------------------------------------------------
# 2b. Unlabel worker nodes
# ---------------------------------------------------------------------------
section "Removing simplyblock labels from worker nodes"

# Find all Kubernetes nodes that carry a simplyblock StorageNodeSet label
# and strip it along with the node-type and storage-node-uuid topology labels.
LABELED_NODES=$($KUBECTL get nodes \
    -l 'io.simplyblock.storagenodeset' \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)

if [[ -z "$LABELED_NODES" ]]; then
    info "No labeled worker nodes found."
else
    for node in $LABELED_NODES; do
        info "Unlabeling node $node..."
        $KUBECTL label node "$node" \
            io.simplyblock.storagenodeset- \
            io.simplyblock.node-type- \
            2>/dev/null || warn "Could not remove base labels from $node"

        UUID_LABELS=$($KUBECTL get node "$node" \
            -o jsonpath='{.metadata.labels}' 2>/dev/null \
            | python3 -c "
import sys, json
d = json.load(sys.stdin)
print(' '.join(k + '-' for k in d if k.startswith('simplyblock.io/storage-node-uuid.')))
" 2>/dev/null || true)

        if [[ -n "$UUID_LABELS" ]]; then
            # shellcheck disable=SC2086
            $KUBECTL label node "$node" $UUID_LABELS 2>/dev/null || true
        fi
    done
    info "Worker node labels removed."
fi

# ---------------------------------------------------------------------------
# 3. Remove remaining workloads
# ---------------------------------------------------------------------------
section "Removing remaining workloads in namespace '$NAMESPACE'"

for kind in pod daemonset deployment statefulset replicaset; do
    count=$($KUBECTL get "$kind" -n "$NAMESPACE" --ignore-not-found --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$count" -gt 0 ]]; then
        info "Deleting ${count} ${kind}(s)..."
        $KUBECTL delete "$kind" --all -n "$NAMESPACE" \
            --ignore-not-found --timeout=60s 2>/dev/null || true
    else
        info "No ${kind}s found."
    fi
done

# A pod whose containers have all exited but which kubelet still has not removed
# is held by a volume it cannot unmount: with the CSI driver uninstalled, no node
# plugin is left to do it, and the namespace waits on the pod for good. That
# happens when the driver went before step 0 could release the pod. Its PVC and
# PV may be gone already, so the pod is found by its state rather than its
# volumes. Only a forced delete removes it, and its node keeps the mount and the
# NVMe-oF connection until it reboots or they are removed by hand.
stranded_pods() {
    $KUBECTL get pods -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if not p["metadata"].get("deletionTimestamp"):
        continue
    states = [c.get("state", {}) for c in p["status"].get("containerStatuses", [])]
    if all("terminated" in s for s in states):
        print(p["metadata"]["name"], p["spec"].get("nodeName", "<unknown>"))
' || true
}
if [[ -n "$(stranded_pods)" ]]; then
    sleep 30  # kubelet may still be finishing a teardown it can do
fi
while read -r pod node; do
    [[ -z "$pod" ]] && continue
    warn "Pod $pod exited but kubelet on $node has not removed it: a volume it cannot unmount."
    warn "  Force-deleting it. Node $node keeps that mount and any NVMe-oF connection."
    $KUBECTL delete pod "$pod" -n "$NAMESPACE" --force --grace-period=0 \
        --ignore-not-found 2>/dev/null || warn "  Could not force-delete $pod"
done <<< "$(stranded_pods)"

# ---------------------------------------------------------------------------
# 4. Remove PVCs and their associated PVs
# ---------------------------------------------------------------------------
section "Removing PVCs and associated PVs"

pvcs=$($KUBECTL get pvc -n "$NAMESPACE" --ignore-not-found -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
pvs_to_delete=()

for pvc in $pvcs; do
    pv=$($KUBECTL get pvc "$pvc" -n "$NAMESPACE" --ignore-not-found \
        -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
    [[ -n "$pv" ]] && pvs_to_delete+=("$pv")
    info "Deleting PVC $pvc (bound to PV: ${pv:-none})..."
    $KUBECTL patch pvc "$pvc" -n "$NAMESPACE" \
        --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    $KUBECTL delete pvc "$pvc" -n "$NAMESPACE" \
        --ignore-not-found --timeout=30s 2>/dev/null || true
done

for pv in "${pvs_to_delete[@]:-}"; do
    [[ -z "$pv" ]] && continue
    info "Deleting PV $pv..."
    $KUBECTL patch pv "$pv" \
        --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    $KUBECTL delete pv "$pv" \
        --ignore-not-found --timeout=30s 2>/dev/null || true
done

# Also catch Released PVs referencing this namespace
info "Cleaning up Released PVs from namespace '$NAMESPACE'..."
released_pvs=$($KUBECTL get pv --ignore-not-found \
    -o jsonpath='{range .items[?(@.status.phase=="Released")]}{.metadata.name} {end}' 2>/dev/null || true)
for pv in $released_pvs; do
    claim_ns=$($KUBECTL get pv "$pv" --ignore-not-found \
        -o jsonpath='{.spec.claimRef.namespace}' 2>/dev/null || true)
    if [[ "$claim_ns" == "$NAMESPACE" ]]; then
        info "  Deleting released PV $pv..."
        $KUBECTL patch pv "$pv" \
            --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
        $KUBECTL delete pv "$pv" --ignore-not-found --timeout=30s 2>/dev/null || true
    fi
done

info "Cleaning up PVs provisioned by CSI driver '$CSI_DRIVER' claimed from namespace '$NAMESPACE'..."
csi_pvs=$($KUBECTL get pv --ignore-not-found \
    -o jsonpath="{range .items[?(@.spec.csi.driver==\"${CSI_DRIVER}\")]}{.metadata.name}{\"\n\"}{end}" 2>/dev/null || true)
for pv in $csi_pvs; do
    [[ -z "$pv" ]] && continue
    claim_ns=$($KUBECTL get pv "$pv" --ignore-not-found \
        -o jsonpath='{.spec.claimRef.namespace}' 2>/dev/null || true)
    [[ "$claim_ns" != "$NAMESPACE" ]] && continue
    claim_name=$($KUBECTL get pv "$pv" --ignore-not-found \
        -o jsonpath='{.spec.claimRef.name}' 2>/dev/null || true)
    if [[ -n "$claim_name" ]]; then
        info "  Deleting PVC $claim_ns/$claim_name (bound to CSI PV $pv)..."
        $KUBECTL patch pvc "$claim_name" -n "$claim_ns" \
            --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
        $KUBECTL delete pvc "$claim_name" -n "$claim_ns" \
            --ignore-not-found --timeout=30s 2>/dev/null || true
    fi
    info "  Deleting CSI PV $pv (claim=$claim_ns/${claim_name:-none})..."
    $KUBECTL patch pv "$pv" \
        --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    $KUBECTL delete pv "$pv" --ignore-not-found --timeout=30s 2>/dev/null || true
done

# With the driver uninstalled, nothing detaches its VolumeAttachments. One whose
# PV is gone attaches nothing and only lists a node as attached.
info "Cleaning up $CSI_DRIVER VolumeAttachments whose PV no longer exists..."
attachments=$($KUBECTL get volumeattachments \
    -o jsonpath="{range .items[?(@.spec.attacher==\"${CSI_DRIVER}\")]}{.metadata.name} {.spec.source.persistentVolumeName}{\"\n\"}{end}" \
    2>/dev/null || true)
while read -r va pv; do
    [[ -z "$va" ]] && continue
    if [[ -n "$pv" ]] && $KUBECTL get pv "$pv" &>/dev/null; then
        continue
    fi
    info "  Deleting VolumeAttachment $va (PV ${pv:-none} is gone)..."
    $KUBECTL patch volumeattachment "$va" \
        --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    $KUBECTL delete volumeattachment "$va" --ignore-not-found --timeout=30s 2>/dev/null || true
done <<< "$attachments"

# ---------------------------------------------------------------------------
# 4b. Remove Deployments in kube-system owned by this Helm release
# ---------------------------------------------------------------------------
section "Removing kube-system Deployments owned by '$HELM_RELEASE'"

deployments=$($KUBECTL get deployment -n kube-system --ignore-not-found \
    -o jsonpath='{range .items[?(@.metadata.annotations.meta\.helm\.sh/release-name=="'"$HELM_RELEASE"'")]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
for deploy in $deployments; do
    info "Deleting Deployment $deploy in kube-system (owned by $HELM_RELEASE)..."
    $KUBECTL delete deployment "$deploy" -n kube-system --ignore-not-found --timeout=60s 2>/dev/null || true
done

# ---------------------------------------------------------------------------
# 5. Remove CRDs
# ---------------------------------------------------------------------------
section "Removing CRDs"

# Before deleting each CRD, check for instances in OTHER namespaces.
# If any exist we cannot safely delete the CRD — inform the user and skip it.
blocked_crds=()

for crd in $CRDS; do
    # Find instances outside the target namespace (cluster-scoped resources have no namespace field)
    orphans=$($KUBECTL get "$crd" --all-namespaces --ignore-not-found \
        --no-headers 2>/dev/null | grep -v "^${NAMESPACE}\s" || true)

    if [[ -n "$orphans" ]]; then
        warn "Cannot delete CRD $crd — instances exist in other namespaces:"
        echo "$orphans" | while IFS= read -r line; do
            ns=$(echo "$line" | awk '{print $1}')
            name=$(echo "$line" | awk '{print $2}')
            warn "  namespace=$ns  name=$name"
            warn "  Delete manually: $KUBECTL delete $crd $name -n $ns"
        done
        blocked_crds+=("$crd")
        continue
    fi

    info "Deleting CRD $crd..."
    $KUBECTL delete crd "$crd" --ignore-not-found --timeout=30s 2>/dev/null || true
done

# Confirm CRDs removed
section "Confirming CRD removal"
remaining_crds=$($KUBECTL get crd -o name 2>/dev/null | grep "$CRD_GROUP" | wc -l | tr -d ' ')
if [[ "$remaining_crds" -eq 0 ]]; then
    info "All CRDs removed successfully."
elif [[ "${#blocked_crds[@]}" -gt 0 ]]; then
    warn "${#blocked_crds[@]} CRD(s) skipped due to instances in other namespaces."
    warn "Delete the listed resources manually, then re-run this script."
else
    warn "$remaining_crds CRD(s) still present — may need a moment to propagate."
fi

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
section "Cleanup complete"
info "Namespace: $NAMESPACE"
info "If the namespace itself should be removed, run: $KUBECTL delete namespace $NAMESPACE"
