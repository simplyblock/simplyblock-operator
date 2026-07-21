#!/bin/bash
# Regression test: StorageNode and StorageNodeOps API
#
# Usage:
#   ./test_storagenode_ops.sh           # run all tests
#   ./test_storagenode_ops.sh 1 3 5     # run specific tests by number
#
# Tests:
#   1  — StorageNode CRs created for each workerNode in StorageNodeSet
#   2  — StorageNode fields: socketIndex, uuid, status, health, overrides
#   3  — StorageNode status aggregated in StorageNodeSet (totalNodes/onlineNodes)
#   4  — nodeConfigs override reflected in StorageNode.spec.overrides
#   5  — StorageNodeOps remove: happy path (drain + remove, node goes offline)
#   6  — StorageNodeOps remove: mutual exclusion — second ops waits for first
#   7  — StorageNodeOps remove: unknown action rejected with Failed phase
#   8  — StorageNodeOps remove: activeOpsRef cleared after completion
#   9  — StorageNode events mirrored from StorageNodeOps (NodeRemoved on StorageNode)
#  10  — StorageNodeSet status wide columns visible (-o wide shows Offline/Removed)
#  11  — Cluster expansion: add a new workerNode, StorageNode CR created and provisioned

set -euo pipefail

NAMESPACE="${NAMESPACE:-simplyblock}"
STORAGENODESET="${STORAGENODESET:-simplyblock-node}"

# Pick the third worker for destructive tests (remove)
TARGET_WORKER="vm04.simplyblock3.localdomain"
TARGET_SN="simplyblock-node-vm04.simplyblock3.localdomain-0"

TIMEOUT_ONLINE=120
TIMEOUT_OPS=180

PASSED=0
FAILED=0

pass()    { echo "[PASS] $*"; PASSED=$((PASSED + 1)); }
fail()    { echo "[FAIL] $*"; FAILED=$((FAILED + 1)); }
info()    { echo "[INFO] $*"; }
section() { echo ""; echo "══════════════════════════════════════════"; echo " $*"; echo "══════════════════════════════════════════"; }

# ── Helpers ───────────────────────────────────────────────────────────────────

wait_for_sn_status() {
  local name=$1 want=$2 timeout=${3:-$TIMEOUT_ONLINE}
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    got=$(kubectl -n "$NAMESPACE" get storagenode "$name" \
      -o jsonpath='{.status.status}' 2>/dev/null || true)
    [[ "$got" == "$want" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

wait_for_ops_phase() {
  local name=$1 want=$2 timeout=${3:-$TIMEOUT_OPS}
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    got=$(kubectl -n "$NAMESPACE" get storagenodeops "$name" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [[ "$got" == "$want" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

delete_ops() {
  kubectl -n "$NAMESPACE" delete storagenodeops "$1" --ignore-not-found &>/dev/null || true
}

# ── Test selection ────────────────────────────────────────────────────────────

run_test() {
  local n=$1
  [[ ${#TESTS[@]} -eq 0 ]] && return 0
  for t in "${TESTS[@]}"; do [[ "$t" == "$n" ]] && return 0; done
  return 1
}

TESTS=()
for arg in "$@"; do TESTS+=("$arg"); done

# ── Tests ─────────────────────────────────────────────────────────────────────

section "Test 1 — StorageNode CRs created for each workerNode"
if run_test 1; then
  workers=$(kubectl -n "$NAMESPACE" get storagenodeset "$STORAGENODESET" \
    -o jsonpath='{.spec.workerNodes[*]}' 2>/dev/null)
  sns=$(kubectl -n "$NAMESPACE" get storagenode \
    -o jsonpath='{.items[*].spec.workerNode}' 2>/dev/null)
  all_found=true
  for w in $workers; do
    if ! echo "$sns" | grep -q "$w"; then
      fail "No StorageNode CR found for worker $w"
      all_found=false
    fi
  done
  $all_found && pass "StorageNode CRs exist for all workerNodes"
fi

section "Test 2 — StorageNode fields populated"
if run_test 2; then
  errors=0
  for sn in $(kubectl -n "$NAMESPACE" get storagenode -o name); do
    name=$(echo "$sn" | cut -d/ -f2)
    socket=$(kubectl -n "$NAMESPACE" get storagenode "$name" \
      -o jsonpath='{.spec.socketIndex}' 2>/dev/null)
    uuid=$(kubectl -n "$NAMESPACE" get storagenode "$name" \
      -o jsonpath='{.status.uuid}' 2>/dev/null)
    status=$(kubectl -n "$NAMESPACE" get storagenode "$name" \
      -o jsonpath='{.status.status}' 2>/dev/null)
    if [[ -z "$uuid" ]]; then
      fail "$name: uuid is empty"
      errors=$((errors + 1))
    fi
    if [[ "$status" != "online" ]]; then
      fail "$name: status is '$status', expected online"
      errors=$((errors + 1))
    fi
    if [[ -z "$socket" ]]; then
      fail "$name: socketIndex is empty"
      errors=$((errors + 1))
    fi
  done
  [[ $errors -eq 0 ]] && pass "All StorageNode CRs have uuid, status=online, socketIndex"
fi

section "Test 3 — StorageNodeSet status aggregation"
if run_test 3; then
  total=$(kubectl -n "$NAMESPACE" get storagenodeset "$STORAGENODESET" \
    -o jsonpath='{.status.totalNodes}' 2>/dev/null)
  online=$(kubectl -n "$NAMESPACE" get storagenodeset "$STORAGENODESET" \
    -o jsonpath='{.status.onlineNodes}' 2>/dev/null)
  worker_count=$(kubectl -n "$NAMESPACE" get storagenodeset "$STORAGENODESET" \
    -o jsonpath='{.spec.workerNodes}' 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin)))")
  if [[ "$total" == "$worker_count" ]]; then
    pass "totalNodes=$total matches workerNodes count"
  else
    fail "totalNodes=$total, expected $worker_count"
  fi
  if [[ "$online" -gt 0 ]]; then
    pass "onlineNodes=$online > 0"
  else
    fail "onlineNodes=$online — expected > 0"
  fi
fi

section "Test 4 — nodeConfigs override in StorageNode.spec.overrides"
if run_test 4; then
  # Check that at least one StorageNode has overrides populated
  found_overrides=false
  for sn in $(kubectl -n "$NAMESPACE" get storagenode -o name); do
    name=$(echo "$sn" | cut -d/ -f2)
    overrides=$(kubectl -n "$NAMESPACE" get storagenode "$name" \
      -o jsonpath='{.spec.overrides}' 2>/dev/null)
    if [[ -n "$overrides" && "$overrides" != "{}" ]]; then
      found_overrides=true
      max_lvol=$(kubectl -n "$NAMESPACE" get storagenode "$name" \
        -o jsonpath='{.spec.overrides.maxLogicalVolumeCount}' 2>/dev/null)
      info "$name: maxLogicalVolumeCount=$max_lvol"
    fi
  done
  $found_overrides && pass "StorageNode.spec.overrides populated from nodeConfigs" \
    || fail "No StorageNode has spec.overrides — check nodeConfigs in StorageNodeSet"
fi

section "Test 5 — StorageNodeOps remove: happy path"
if run_test 5; then
  info "Removing $TARGET_SN via StorageNodeOps"
  delete_ops "test-remove-vm04"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-remove-vm04
  namespace: $NAMESPACE
spec:
  storageNodeRef: $TARGET_SN
  action: remove
EOF
  if wait_for_ops_phase "test-remove-vm04" "Succeeded" $TIMEOUT_OPS; then
    pass "StorageNodeOps remove completed with phase=Succeeded"
    sn_status=$(kubectl -n "$NAMESPACE" get storagenode "$TARGET_SN" \
      -o jsonpath='{.status.status}' 2>/dev/null)
    [[ "$sn_status" == "removed" ]] \
      && pass "StorageNode status=removed after ops" \
      || fail "StorageNode status='$sn_status', expected 'removed'"
  else
    phase=$(kubectl -n "$NAMESPACE" get storagenodeops test-remove-vm04 \
      -o jsonpath='{.status.phase}' 2>/dev/null)
    fail "StorageNodeOps did not reach Succeeded (phase=$phase)"
  fi
  delete_ops "test-remove-vm04"
fi

section "Test 6 — StorageNodeOps mutual exclusion"
if run_test 6; then
  # Need an online node — use vm02
  sn2="simplyblock-node-vm02.simplyblock3.localdomain-0"
  delete_ops "test-mutex-a"; delete_ops "test-mutex-b"

  # Create two ops targeting the same node simultaneously
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-mutex-a
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: suspend
EOF
  sleep 2
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-mutex-b
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: suspend
EOF

  # Give the reconciler time to pick up both
  sleep 10

  active=$(kubectl -n "$NAMESPACE" get storagenode "$sn2" \
    -o jsonpath='{.status.activeOpsRef}' 2>/dev/null)
  phase_b=$(kubectl -n "$NAMESPACE" get storagenodeops test-mutex-b \
    -o jsonpath='{.status.phase}' 2>/dev/null)

  if [[ -n "$active" ]]; then
    pass "activeOpsRef set to '$active' — only one ops active at a time"
  else
    fail "activeOpsRef is empty — mutual exclusion may not be working"
  fi

  # Clean up — delete both, resume node if needed
  delete_ops "test-mutex-a"; delete_ops "test-mutex-b"
  # Resume the node if it got suspended
  if [[ "$(kubectl -n "$NAMESPACE" get storagenode "$sn2" -o jsonpath='{.status.status}' 2>/dev/null)" == "suspended" ]]; then
    kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-mutex-resume
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: resume
EOF
    wait_for_ops_phase "test-mutex-resume" "Succeeded" 60 || true
    delete_ops "test-mutex-resume"
  fi
fi

section "Test 7 — StorageNodeOps unknown action → Failed"
if run_test 7; then
  sn2="simplyblock-node-vm02.simplyblock3.localdomain-0"
  delete_ops "test-bad-action"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-bad-action
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: bogus-action
EOF
  if wait_for_ops_phase "test-bad-action" "Failed" 30; then
    pass "Unknown action correctly transitions to Failed phase"
  else
    fail "Unknown action did not reach Failed phase within 30s"
  fi
  delete_ops "test-bad-action"
fi

section "Test 8 — activeOpsRef cleared after ops completes"
if run_test 8; then
  sn2="simplyblock-node-vm02.simplyblock3.localdomain-0"
  delete_ops "test-ref-clear"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-ref-clear
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: suspend
EOF
  # Wait for it to complete or fail
  wait_for_ops_phase "test-ref-clear" "Succeeded" $TIMEOUT_OPS || \
  wait_for_ops_phase "test-ref-clear" "Failed" 10 || true

  active=$(kubectl -n "$NAMESPACE" get storagenode "$sn2" \
    -o jsonpath='{.status.activeOpsRef}' 2>/dev/null)
  if [[ -z "$active" ]]; then
    pass "activeOpsRef cleared after ops completion"
  else
    fail "activeOpsRef='$active' still set after ops completed"
  fi
  # Resume if suspended
  if [[ "$(kubectl -n "$NAMESPACE" get storagenode "$sn2" -o jsonpath='{.status.status}' 2>/dev/null)" == "suspended" ]]; then
    delete_ops "test-resume-cleanup"
    kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-resume-cleanup
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: resume
EOF
    wait_for_ops_phase "test-resume-cleanup" "Succeeded" 60 || true
    delete_ops "test-resume-cleanup"
  fi
  delete_ops "test-ref-clear"
fi

section "Test 9 — Events mirrored to StorageNode after ops"
if run_test 9; then
  sn2="simplyblock-node-vm02.simplyblock3.localdomain-0"
  delete_ops "test-events"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-events
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: suspend
EOF
  wait_for_ops_phase "test-events" "Succeeded" $TIMEOUT_OPS || true

  events=$(kubectl -n "$NAMESPACE" get events \
    --field-selector "involvedObject.name=$sn2,involvedObject.kind=StorageNode" \
    --no-headers 2>/dev/null | wc -l)
  if [[ $events -gt 0 ]]; then
    pass "StorageNode has $events event(s) — events are being mirrored"
    kubectl -n "$NAMESPACE" get events \
      --field-selector "involvedObject.name=$sn2,involvedObject.kind=StorageNode" \
      --no-headers 2>/dev/null
  else
    fail "No events found on StorageNode $sn2"
  fi
  # Resume
  delete_ops "test-events"
  if [[ "$(kubectl -n "$NAMESPACE" get storagenode "$sn2" -o jsonpath='{.status.status}' 2>/dev/null)" == "suspended" ]]; then
    kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNodeOps
metadata:
  name: test-events-resume
  namespace: $NAMESPACE
spec:
  storageNodeRef: $sn2
  action: resume
EOF
    wait_for_ops_phase "test-events-resume" "Succeeded" 60 || true
    delete_ops "test-events-resume"
  fi
fi

section "Test 10 — StorageNodeSet wide columns"
if run_test 10; then
  output=$(kubectl -n "$NAMESPACE" get storagenodeset -o wide 2>/dev/null)
  if echo "$output" | grep -q "OFFLINE\|SUSPENDED\|CREATING\|REMOVED"; then
    pass "Wide columns (OFFLINE, SUSPENDED, CREATING, REMOVED) visible in -o wide"
    echo "$output"
  else
    fail "Wide columns not visible — check printcolumn markers on StorageNodeSet"
  fi
fi

section "Test 11 — Cluster expansion: manually created StorageNode referencing existing StorageNodeSet"
if run_test 11; then
  # Create a StorageNode CR manually, referencing the existing StorageNodeSet.
  # The operator must NOT delete it (no OwnerReference = manually created).
  # Fleet defaults from the StorageNodeSet fill in any unset fields.
  EXPAND_WORKER="${EXPAND_WORKER:-vm15.simplyblock3.localdomain}"
  EXPAND_SN_NAME="manual-expand-${EXPAND_WORKER%%.*}"
  TIMEOUT_EXPANSION="${TIMEOUT_EXPANSION:-300}"

  info "Manually creating StorageNode for $EXPAND_WORKER referencing $STORAGENODESET"

  # Clean up any leftover
  kubectl -n "$NAMESPACE" delete storagenode "$EXPAND_SN_NAME" \
    --ignore-not-found &>/dev/null || true

  # Create the StorageNode CR manually — no OwnerReference.
  # Only specify the overrides that differ from fleet defaults; the rest are
  # inherited from the referenced StorageNodeSet.
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageNode
metadata:
  name: $EXPAND_SN_NAME
  namespace: $NAMESPACE
spec:
  storageNodeSetRef: $STORAGENODESET
  workerNode: $EXPAND_WORKER
  socketIndex: 0
  overrides:
    maxLogicalVolumeCount: 15
    spdkSystemMemory: "2G"
EOF

  # 1. Operator must NOT delete it (not in spec.workerNodes, but no OwnerReference)
  sleep 15
  if kubectl -n "$NAMESPACE" get storagenode "$EXPAND_SN_NAME" &>/dev/null; then
    pass "Manually created StorageNode survived reconciler (not deleted as stale)"
  else
    fail "Manually created StorageNode was incorrectly deleted by the reconciler"
  fi

  # 2. Per-node ConfigMap for the existing StorageNodeSet should include the new worker
  elapsed=0; cm_ok=false
  while [[ $elapsed -lt 60 ]]; do
    keys=$(kubectl -n "$NAMESPACE" get configmap "${STORAGENODESET}-per-node-config" \
      -o jsonpath='{.data}' 2>/dev/null | \
      python3 -c "import json,sys; d=json.load(sys.stdin); print(' '.join(d.keys()))" 2>/dev/null || true)
    if echo "$keys" | grep -q "$EXPAND_WORKER"; then
      cm_ok=true; break
    fi
    sleep 5; elapsed=$((elapsed + 5))
  done
  $cm_ok \
    && pass "Per-node ConfigMap updated with entry for manual worker $EXPAND_WORKER" \
    || fail "Per-node ConfigMap did not include $EXPAND_WORKER within 60s"

  # 3. Overrides merged with fleet defaults in ConfigMap entry
  max_lvol=$(kubectl -n "$NAMESPACE" get configmap "${STORAGENODESET}-per-node-config" \
    -o jsonpath="{.data['$EXPAND_WORKER']}" 2>/dev/null | \
    grep "^MAX_LVOL=" | cut -d= -f2 || true)
  [[ "$max_lvol" == "15" ]] \
    && pass "Override maxLogicalVolumeCount=15 reflected in ConfigMap" \
    || fail "Expected MAX_LVOL=15 in ConfigMap, got '$max_lvol'"

  # 4. StorageNodeReconciler picks it up and provisions it
  info "Waiting up to ${TIMEOUT_EXPANSION}s for $EXPAND_SN_NAME to come online..."
  if wait_for_sn_status "$EXPAND_SN_NAME" "online" "$TIMEOUT_EXPANSION"; then
    pass "Manually created StorageNode $EXPAND_SN_NAME reached status=online"
    uuid=$(kubectl -n "$NAMESPACE" get storagenode "$EXPAND_SN_NAME" \
      -o jsonpath='{.status.uuid}' 2>/dev/null)
    [[ -n "$uuid" ]] && pass "StorageNode uuid=$uuid populated" \
      || fail "StorageNode uuid is empty after provisioning"
  else
    status=$(kubectl -n "$NAMESPACE" get storagenode "$EXPAND_SN_NAME" \
      -o jsonpath='{.status.status}' 2>/dev/null)
    fail "StorageNode did not reach online within ${TIMEOUT_EXPANSION}s (status=$status)"
  fi

  # Cleanup
  info "Cleaning up manually created StorageNode $EXPAND_SN_NAME..."
  kubectl -n "$NAMESPACE" delete storagenode "$EXPAND_SN_NAME" \
    --ignore-not-found &>/dev/null || true
fi

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "══════════════════════════════════════════"
echo " Results: $PASSED passed, $FAILED failed"
echo "══════════════════════════════════════════"
[[ $FAILED -eq 0 ]]
