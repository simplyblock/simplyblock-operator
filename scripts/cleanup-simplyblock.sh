#!/usr/bin/env bash

NAMESPACE="${1:-simplyblock}"
HELM_RELEASE="simplyblock-operator"
CRD_GROUP="storage.simplyblock.io"

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
        $KUBECTL delete "$crd" "$name" -n "$NAMESPACE" \
            --ignore-not-found --timeout=30s 2>/dev/null || true
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
        $KUBECTL delete "$crd" "$name" -n "$NAMESPACE" \
            --ignore-not-found --force --grace-period=0 2>/dev/null || true
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
        --type=merge -p '{"metadata":{"finalizers":[]}}' \
        --ignore-not-found 2>/dev/null || true
    $KUBECTL delete pvc "$pvc" -n "$NAMESPACE" \
        --ignore-not-found --timeout=30s 2>/dev/null || true
done

for pv in "${pvs_to_delete[@]:-}"; do
    [[ -z "$pv" ]] && continue
    info "Deleting PV $pv..."
    $KUBECTL patch pv "$pv" \
        --type=merge -p '{"metadata":{"finalizers":[]}}' \
        --ignore-not-found 2>/dev/null || true
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
            --type=merge -p '{"metadata":{"finalizers":[]}}' \
            --ignore-not-found 2>/dev/null || true
        $KUBECTL delete pv "$pv" --ignore-not-found --timeout=30s 2>/dev/null || true
    fi
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
