#!/bin/bash
# Regression test: StorageClusterOps API
#
# Usage:
#   ./test_storageclusterops.sh           # run all tests
#   ./test_storageclusterops.sh 1 3 5     # run specific tests by number
#
# Tests:
#   1  — Short name: kubectl get scops works
#   2  — Unknown action rejected at admission by CRD enum validation
#   3  — node-rolling-restart: initialises (Triggered=true) and eventually Succeeds (E2E)
#   4  — Reference to non-existent cluster → Failed phase
#   5  — Mutual exclusion: second ops requeues while first holds activeOpsRef
#   6  — activeOpsRef cleared after ops completes (Succeeded or Failed)
#   7  — Events emitted on StorageClusterOps CR
#   8  — Activate: cluster transitions to active (E2E, requires inactive cluster)
#   9  — Expand: cluster expands capacity (E2E)
#  10  — Shutdown: cluster shuts down (E2E, destructive)
#  11  — Restart: cluster restarts (E2E, destructive)

set -euo pipefail

NAMESPACE="${NAMESPACE:-simplyblock}"
CLUSTER_REF="${CLUSTER_REF:-simplyblock-cluster}"

TIMEOUT_OPS="${TIMEOUT_OPS:-360}"

PASSED=0
FAILED=0

pass()    { echo "[PASS] $*"; PASSED=$((PASSED + 1)); }
fail()    { echo "[FAIL] $*"; FAILED=$((FAILED + 1)); }
info()    { echo "[INFO] $*"; }
section() { echo ""; echo "══════════════════════════════════════════"; echo " $*"; echo "══════════════════════════════════════════"; }

# ── Helpers ───────────────────────────────────────────────────────────────────

wait_for_ops_phase() {
  local name=$1 want=$2 timeout=${3:-$TIMEOUT_OPS}
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    got=$(kubectl -n "$NAMESPACE" get storageclusterops "$name" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [[ "$got" == "$want" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

wait_for_any_terminal_phase() {
  local name=$1 timeout=${2:-$TIMEOUT_OPS}
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    got=$(kubectl -n "$NAMESPACE" get storageclusterops "$name" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [[ "$got" == "Succeeded" || "$got" == "Failed" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

delete_ops() {
  kubectl -n "$NAMESPACE" delete storageclusterops "$1" --ignore-not-found &>/dev/null || true
}

active_ops_ref() {
  kubectl -n "$NAMESPACE" get storagecluster "$CLUSTER_REF" \
    -o jsonpath='{.status.activeOpsRef}' 2>/dev/null || true
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

if run_test 1; then
  section "Test 1 — Short name: kubectl get scops works"
  if kubectl -n "$NAMESPACE" get scops &>/dev/null; then
    pass "kubectl get scops is recognised (short name registered)"
  else
    fail "kubectl get scops failed — short name 'scops' may not be registered"
  fi
fi

if run_test 2; then
  section "Test 2 — Unknown action rejected at admission (CRD enum validation)"
  # The action field has +kubebuilder:validation:Enum so the API server rejects
  # bogus values before the object is created — the apply itself must fail.
  err=$(kubectl apply -f - 2>&1 <<EOF || true
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-bad-action
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: bogus-action
EOF
)
  if echo "$err" | grep -q "Unsupported value"; then
    pass "Unknown action rejected at admission with 'Unsupported value' — CRD enum validation works"
  else
    fail "Expected admission rejection for bogus action, got: $err"
  fi
  delete_ops "test-bad-action"
fi

if run_test 3; then
  section "Test 3 — node-rolling-restart: initialises and eventually Succeeds (E2E)"
  info "WARNING: This test triggers node-rolling-restart on the live cluster (all nodes)."
  delete_ops "test-node-rolling-restart"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-node-rolling-restart
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: node-rolling-restart
EOF
  # Give the reconciler one tick to initialise (set Triggered, transition to Running)
  sleep 10

  ops_triggered=$(kubectl -n "$NAMESPACE" get storageclusterops test-node-rolling-restart \
    -o jsonpath='{.status.triggered}' 2>/dev/null || true)
  if [[ "$ops_triggered" == "true" ]]; then
    pass "ops.status.triggered=true — node-rolling-restart state machine initialised"
  else
    fail "ops.status.triggered not set (got '$ops_triggered')"
  fi

  ops_phase=$(kubectl -n "$NAMESPACE" get storageclusterops test-node-rolling-restart \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "$ops_phase" == "Running" || "$ops_phase" == "Succeeded" ]]; then
    pass "StorageClusterOps phase=$ops_phase"
  else
    fail "StorageClusterOps phase='$ops_phase', expected Running or Succeeded"
  fi

  # Wait for all nodes to be recycled
  info "Waiting up to ${TIMEOUT_OPS}s for node-rolling-restart to complete..."
  wait_for_any_terminal_phase "test-node-rolling-restart" "$TIMEOUT_OPS" || true
  final_phase=$(kubectl -n "$NAMESPACE" get storageclusterops test-node-rolling-restart \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)
  final_msg=$(kubectl -n "$NAMESPACE" get storageclusterops test-node-rolling-restart \
    -o jsonpath='{.status.message}' 2>/dev/null || true)
  if [[ "$final_phase" == "Succeeded" ]]; then
    pass "node-rolling-restart completed with phase=Succeeded (message: $final_msg)"
  else
    fail "node-rolling-restart did not Succeed (phase=$final_phase, message=$final_msg)"
  fi
  delete_ops "test-node-rolling-restart"
fi

if run_test 4; then
  section "Test 4 — Reference to non-existent cluster → Failed phase"
  delete_ops "test-missing-cluster"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-missing-cluster
  namespace: $NAMESPACE
spec:
  clusterRef: does-not-exist
  action: restart
EOF
  if wait_for_ops_phase "test-missing-cluster" "Failed" 30; then
    msg=$(kubectl -n "$NAMESPACE" get storageclusterops test-missing-cluster \
      -o jsonpath='{.status.message}' 2>/dev/null)
    pass "Non-existent clusterRef correctly transitions to Failed (message: $msg)"
  else
    phase=$(kubectl -n "$NAMESPACE" get storageclusterops test-missing-cluster \
      -o jsonpath='{.status.phase}' 2>/dev/null)
    fail "Non-existent clusterRef did not reach Failed within 30s (phase=$phase)"
  fi
  delete_ops "test-missing-cluster"
fi

if run_test 5; then
  section "Test 5 — Mutual exclusion: second ops requeues while first holds activeOpsRef"
  delete_ops "test-mutex-a"; delete_ops "test-mutex-b"

  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-mutex-a
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: restart
EOF
  sleep 3
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-mutex-b
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: restart
EOF

  # Give the reconciler time to process both
  sleep 10

  active=$(active_ops_ref)
  phase_b=$(kubectl -n "$NAMESPACE" get storageclusterops test-mutex-b \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)

  if [[ -n "$active" ]]; then
    pass "activeOpsRef='$active' — only one ops active at a time"
  else
    fail "activeOpsRef is empty — mutual exclusion may not be working"
  fi

  if [[ "$phase_b" == "Pending" || "$phase_b" == "" ]]; then
    pass "Second ops is still Pending (phase=$phase_b) while first holds the lock"
  else
    info "Second ops phase='$phase_b' (may have run after first completed)"
  fi

  # Wait for both to finish before cleaning up
  wait_for_any_terminal_phase "test-mutex-a" "$TIMEOUT_OPS" || true
  wait_for_any_terminal_phase "test-mutex-b" "$TIMEOUT_OPS" || true
  delete_ops "test-mutex-a"; delete_ops "test-mutex-b"
fi

if run_test 6; then
  section "Test 6 — activeOpsRef cleared after ops completes"
  delete_ops "test-ref-clear"
  # Use the real cluster so the lock is actually acquired and we can verify it is released.
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-ref-clear
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: activate
EOF
  wait_for_ops_phase "test-ref-clear" "Succeeded" 60 || \
  wait_for_ops_phase "test-ref-clear" "Failed" 10 || true

  active=$(active_ops_ref)
  if [[ -z "$active" ]]; then
    pass "activeOpsRef is empty after ops completed (lock released correctly)"
  else
    fail "activeOpsRef='$active' still set after ops completed"
  fi
  delete_ops "test-ref-clear"
fi

if run_test 7; then
  section "Test 7 — Events emitted on StorageClusterOps CR"
  delete_ops "test-events"
  # Use a missing clusterRef so the ops fails fast and emits an event
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-events
  namespace: $NAMESPACE
spec:
  clusterRef: does-not-exist
  action: restart
EOF
  wait_for_ops_phase "test-events" "Failed" 30 || true

  events=$(kubectl -n "$NAMESPACE" get events \
    --field-selector "involvedObject.name=test-events,involvedObject.kind=StorageClusterOps" \
    --no-headers 2>/dev/null | wc -l)
  if [[ $events -gt 0 ]]; then
    pass "StorageClusterOps has $events event(s) — events are being emitted"
    kubectl -n "$NAMESPACE" get events \
      --field-selector "involvedObject.name=test-events,involvedObject.kind=StorageClusterOps" \
      --no-headers 2>/dev/null
  else
    fail "No events found on StorageClusterOps test-events"
  fi
  delete_ops "test-events"
fi

if run_test 8; then
  section "Test 8 — Activate: cluster transitions to active (E2E)"
  info "WARNING: This test requires the cluster to be in an inactive state first."
  info "Cluster: $CLUSTER_REF in namespace $NAMESPACE"
  delete_ops "test-activate"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-activate
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: activate
EOF
  if wait_for_ops_phase "test-activate" "Succeeded" "$TIMEOUT_OPS"; then
    pass "activate ops completed with phase=Succeeded"
    cluster_status=$(kubectl -n "$NAMESPACE" get storagecluster "$CLUSTER_REF" \
      -o jsonpath='{.status.status}' 2>/dev/null)
    info "StorageCluster status after activate: $cluster_status"
  else
    phase=$(kubectl -n "$NAMESPACE" get storageclusterops test-activate \
      -o jsonpath='{.status.phase}' 2>/dev/null)
    msg=$(kubectl -n "$NAMESPACE" get storageclusterops test-activate \
      -o jsonpath='{.status.message}' 2>/dev/null)
    fail "activate did not reach Succeeded within ${TIMEOUT_OPS}s (phase=$phase, message=$msg)"
  fi
  delete_ops "test-activate"
fi

if run_test 9; then
  section "Test 9 — Expand: cluster expands (E2E)"
  info "Cluster: $CLUSTER_REF in namespace $NAMESPACE"
  delete_ops "test-expand"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-expand
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: expand
EOF
  if wait_for_ops_phase "test-expand" "Succeeded" "$TIMEOUT_OPS"; then
    pass "expand ops completed with phase=Succeeded"
  else
    phase=$(kubectl -n "$NAMESPACE" get storageclusterops test-expand \
      -o jsonpath='{.status.phase}' 2>/dev/null)
    msg=$(kubectl -n "$NAMESPACE" get storageclusterops test-expand \
      -o jsonpath='{.status.message}' 2>/dev/null)
    fail "expand did not reach Succeeded within ${TIMEOUT_OPS}s (phase=$phase, message=$msg)"
  fi
  delete_ops "test-expand"
fi

if run_test 10; then
  section "Test 10 — Shutdown: cluster shuts down (E2E, destructive)"
  info "WARNING: This test shuts down the cluster. Ensure you can recover it afterwards."
  info "Cluster: $CLUSTER_REF in namespace $NAMESPACE"
  delete_ops "test-shutdown"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-shutdown
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: shutdown
EOF
  if wait_for_ops_phase "test-shutdown" "Succeeded" "$TIMEOUT_OPS"; then
    pass "shutdown ops completed with phase=Succeeded"
  else
    phase=$(kubectl -n "$NAMESPACE" get storageclusterops test-shutdown \
      -o jsonpath='{.status.phase}' 2>/dev/null)
    msg=$(kubectl -n "$NAMESPACE" get storageclusterops test-shutdown \
      -o jsonpath='{.status.message}' 2>/dev/null)
    fail "shutdown did not reach Succeeded within ${TIMEOUT_OPS}s (phase=$phase, message=$msg)"
  fi
  delete_ops "test-shutdown"
fi

if run_test 11; then
  section "Test 11 — Restart: cluster restarts (E2E, destructive)"
  info "WARNING: This test restarts the cluster."
  info "Cluster: $CLUSTER_REF in namespace $NAMESPACE"
  delete_ops "test-restart"
  kubectl apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: test-restart
  namespace: $NAMESPACE
spec:
  clusterRef: $CLUSTER_REF
  action: restart
EOF
  if wait_for_ops_phase "test-restart" "Succeeded" "$TIMEOUT_OPS"; then
    pass "restart ops completed with phase=Succeeded"
  else
    phase=$(kubectl -n "$NAMESPACE" get storageclusterops test-restart \
      -o jsonpath='{.status.phase}' 2>/dev/null)
    msg=$(kubectl -n "$NAMESPACE" get storageclusterops test-restart \
      -o jsonpath='{.status.message}' 2>/dev/null)
    fail "restart did not reach Succeeded within ${TIMEOUT_OPS}s (phase=$phase, message=$msg)"
  fi
  delete_ops "test-restart"
fi

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "══════════════════════════════════════════"
echo " Results: $PASSED passed, $FAILED failed"
echo "══════════════════════════════════════════"
[[ $FAILED -eq 0 ]]
