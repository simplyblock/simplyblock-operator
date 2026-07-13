#!/bin/bash
# Test: storage node drain-remove — full regression suite
#
# Usage:
#   ./test_drain_remove.sh           # run all tests
#   ./test_drain_remove.sh 1 3 5     # run specific tests by number
#
# Tests:
#   1 — Happy path: full drain-remove (removes a node)
#   2 — Pinned PVC blocks drain (cancel after verifying)
#   3 — Cancel mid-drain, node resumes to online
#   4 — Operator restart mid-drain, sub-phase preserved
#   5 — VolumeMigration failure, node resumes and state = failed
#   6 — fio under drain: I/O not interrupted (removes a node — run last)
#   7 — Empty node: no VolumeMigration CRs created, drain completes directly (removes a node)
#   8 — Rapid action toggle: set/clear/set does not leak state
#   9 — System-volume-only node: sb-fio-baseline-* volumes skipped, no migrations (removes a node)
#  10 — Both pinned + unmanaged visible simultaneously: both blocking events emitted
#  11 — Drain pauses when a second storage node is shut down (cluster degraded), resumes after restart

set -euo pipefail

NAMESPACE="simplyblock"
TEST_NS="drain-remove-test"
SC="simplyblock-simplyblock-simplyblock-cluster-simplyblock-pool"
STORAGENODESET="simplyblock-node"
OPERATOR_DEPLOY="simplyblock-operator"
PVC_COUNT=10
PVC_SIZE="10Gi"

TIMEOUT_PVC_BOUND=120
TIMEOUT_POD_RUNNING=180
TIMEOUT_SUSPENDED=120
TIMEOUT_MIGRATION=600

PASSED=0
FAILED=0

pass()    { echo "[PASS] $*"; PASSED=$((PASSED + 1)); }
fail()    { echo "[FAIL] $*"; FAILED=$((FAILED + 1)); }
info()    { echo "[INFO] $*"; }
section() { echo ""; echo "══════════════════════════════════════════"; echo " $*"; echo "══════════════════════════════════════════"; }

# ── Helpers ──────────────────────────────────────────────────────────────────

clear_action() {
  kubectl patch storagenodeset "$STORAGENODESET" -n "$NAMESPACE" --type=json \
    -p '[{"op":"remove","path":"/spec/action"},{"op":"remove","path":"/spec/nodeUUID"}]' \
    &>/dev/null || true
}

trigger_drain() {
  local node_uuid="$1"
  kubectl patch storagenodeset "$STORAGENODESET" -n "$NAMESPACE" --type=merge -p "{
    \"spec\": {\"action\": \"remove\", \"nodeUUID\": \"$node_uuid\"}
  }"
}

wait_for_subphase() {
  local target_phases="$1"  # pipe-separated e.g. "Suspending|Migrating"
  local timeout="$2"
  local deadline=$((SECONDS + timeout))
  while true; do
    local sub_phase
    sub_phase=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
    info "  subPhase=$sub_phase"
    echo "$sub_phase" | grep -qE "^($target_phases)$" && return 0
    [[ $SECONDS -ge $deadline ]] && return 1
    sleep 5
  done
}

wait_for_action_state() {
  local want_state="$1"
  local timeout="$2"
  local deadline=$((SECONDS + timeout))
  while true; do
    local state sub_phase migrated pending
    state=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath='{.status.actionStatus.state}' 2>/dev/null || true)
    sub_phase=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
    migrated=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath='{.status.actionStatus.volumesMigrated}' 2>/dev/null || echo "0")
    pending=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath='{.status.actionStatus.volumesPending}' 2>/dev/null || echo "?")
    info "  state=$state subPhase=$sub_phase migrated=$migrated pending=$pending"
    [[ "$state" == "$want_state" ]] && return 0
    [[ $SECONDS -ge $deadline ]] && return 1
    sleep 10
  done
}

get_first_node_uuid() {
  kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.nodes[0].uuid}'
}

get_cluster_uuid() {
  kubectl get storagecluster -n "$NAMESPACE" \
    -o jsonpath='{.items[0].status.uuid}'
}

get_pool_uuid() {
  kubectl get pool -n "$NAMESPACE" \
    -o jsonpath='{.items[0].status.uuid}'
}

# Create a backend volume via sbctl in the admin-control pod.
# Args: pool name size [host_node_uuid]
# --host-id pins the primary to a specific storage node, so we know exactly
# which node will have the volume without having to look it up afterwards.
# Returns the volume UUID (last line of sbctl volume add output).
create_backend_volume() {
  local pool="$1" name="$2" size="${3:-1G}" host_id="${4:-}"
  local adminpod
  adminpod=$(kubectl get pods -n "$NAMESPACE" -l app=simplyblock-admin-control \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [[ -z "$adminpod" ]] && { echo ""; return 1; }
  local extra_args=()
  [[ -n "$host_id" ]] && extra_args=("--host-id" "$host_id")
  kubectl exec -n "$NAMESPACE" "$adminpod" -- \
    sbctl volume add "$name" "$size" "$pool" "${extra_args[@]}" 2>/dev/null \
    | tail -1 \
    | grep -oE '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' \
    || echo ""
}

# Delete a backend volume by UUID via sbctl.
delete_backend_volume() {
  local vol_uuid="$1"
  local adminpod
  adminpod=$(kubectl get pods -n "$NAMESPACE" -l app=simplyblock-admin-control \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [[ -z "$adminpod" ]] && return
  kubectl exec -n "$NAMESPACE" "$adminpod" -- \
    sbctl volume delete "$vol_uuid" &>/dev/null || true
}

wait_for_node_online() {
  local node_uuid="$1" timeout="$2"
  local deadline=$((SECONDS + timeout))
  while true; do
    local status
    status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath="{.status.nodes[?(@.uuid==\"$node_uuid\")].status}" 2>/dev/null || true)
    info "  node status=$status"
    [[ "$status" == "online" ]] && return 0
    [[ $SECONDS -ge $deadline ]] && return 1
    sleep 5
  done
}

create_pvcs_and_pods() {
  local count="$1"
  info "Creating $count PVCs and pods in $TEST_NS..."
  kubectl get ns "$TEST_NS" &>/dev/null || kubectl create ns "$TEST_NS"
  for i in $(seq 1 "$count"); do
    kubectl apply -n "$TEST_NS" -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drain-test-pvc-${i}
spec:
  storageClassName: ${SC}
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: Pod
metadata:
  name: drain-test-pod-${i}
spec:
  containers:
  - name: writer
    image: busybox
    command: ["sh", "-c", "while true; do echo ok >> /mnt/data; sleep 5; done"]
    volumeMounts:
    - mountPath: /mnt
      name: data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: drain-test-pvc-${i}
EOF
  done

  local deadline=$((SECONDS + TIMEOUT_PVC_BOUND))
  while true; do
    local bound
    bound=$(kubectl get pvc -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
      | tr ' ' '\n' | grep -c "^Bound$" || true)
    info "  $bound/$count PVCs bound"
    [[ "$bound" -eq "$count" ]] && break
    [[ $SECONDS -ge $deadline ]] && { echo "[FAIL] PVCs did not bind"; return 1; }
    sleep 5
  done

  deadline=$((SECONDS + TIMEOUT_POD_RUNNING))
  while true; do
    local running
    running=$(kubectl get pods -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
      | tr ' ' '\n' | grep -c "^Running$" || true)
    info "  $running/$count pods running"
    [[ "$running" -eq "$count" ]] && break
    [[ $SECONDS -ge $deadline ]] && { echo "[FAIL] Pods did not reach Running"; return 1; }
    sleep 5
  done
}

cleanup_test_ns() {
  info "Cleaning up $TEST_NS..."
  # Delete pods first so volumes are detached before PVCs are removed,
  # preventing DeleteVolume calls while a migration may still be in flight.
  kubectl delete pods --all -n "$TEST_NS" --ignore-not-found \
    --wait=true --timeout=120s &>/dev/null || true
  kubectl delete pvc --all -n "$TEST_NS" --ignore-not-found \
    --wait=true --timeout=120s &>/dev/null || true
  kubectl delete ns "$TEST_NS" --ignore-not-found &>/dev/null || true
  kubectl wait --for=delete ns/"$TEST_NS" --timeout=60s &>/dev/null || true
}

# ══════════════════════════════════════════════════════════════════════════════
# Test 1: Happy path — drain-remove with PV-managed volumes
# ══════════════════════════════════════════════════════════════════════════════
run_test_1() {
section "Test 1: Happy path drain-remove"

cleanup_test_ns
create_pvcs_and_pods "$PVC_COUNT"

NODE_UUID=$(get_first_node_uuid)
[[ -z "$NODE_UUID" ]] && { fail "Test 1: Could not determine node UUID"; } || {
  info "Target node: $NODE_UUID"
  trigger_drain "$NODE_UUID"

  if wait_for_subphase "Suspending|Migrating|Verifying|Removing" "$TIMEOUT_SUSPENDED"; then
    info "Node entered suspend/migrate phase"
    deadline=$((SECONDS + 60))
    vmig_count=0
    while [[ $SECONDS -lt $deadline ]]; do
      vmig_count=$(kubectl get volumemigration -n "$NAMESPACE" \
        -l "storage.simplyblock.io/drain-node=$NODE_UUID" \
        --no-headers 2>/dev/null | wc -l | tr -d ' ')
      [[ "$vmig_count" -gt 0 ]] && break
      sleep 5
    done
    [[ "$vmig_count" -gt 0 ]] && info "VolumeMigration CRs created: $vmig_count" \
      || info "Warning: no VolumeMigration CRs seen (may have already completed)"

    if wait_for_action_state "success" "$TIMEOUT_MIGRATION"; then
      node_final_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
        -o jsonpath="{.status.nodes[?(@.uuid==\"$NODE_UUID\")].status}" 2>/dev/null || true)
      bound=$(kubectl get pvc -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
        | tr ' ' '\n' | grep -c "^Bound$" || true)
      [[ -z "$node_final_status" || "$node_final_status" == "removed" ]] \
        && pass "Test 1: Node removed from cluster" \
        || fail "Test 1: Node still present with status=$node_final_status"
      [[ "$bound" -eq "$PVC_COUNT" ]] && pass "Test 1: All $PVC_COUNT PVCs still bound after drain" \
        || fail "Test 1: Only $bound/$PVC_COUNT PVCs bound after drain"
    else
      fail "Test 1: Drain did not reach success within timeout"
      clear_action
    fi
  else
    fail "Test 1: Node did not reach suspend phase within ${TIMEOUT_SUSPENDED}s"
    clear_action
  fi
}
cleanup_test_ns
} # end run_test_1

# ══════════════════════════════════════════════════════════════════════════════
# Test 2: Pinned PVC — drain blocks, cancel after verifying
# ══════════════════════════════════════════════════════════════════════════════
run_test_2() {
section "Test 2: Pinned PVC blocks drain (cancel after verifying)"

cleanup_test_ns
kubectl get ns "$TEST_NS" &>/dev/null || kubectl create ns "$TEST_NS"

# Create 3 PVCs + pods so volumes spread across all storage nodes via round-robin.
# All three PVCs are pinned so whichever node we pick to drain is guaranteed
# to have at least one pinned volume blocking it.
for i in 1 2 3; do
  kubectl apply -n "$TEST_NS" -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drain-pin-pvc-${i}
spec:
  storageClassName: ${SC}
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: Pod
metadata:
  name: drain-pin-pod-${i}
spec:
  containers:
  - name: writer
    image: busybox
    command: ["sh", "-c", "while true; do echo ok >> /mnt/data; sleep 5; done"]
    volumeMounts:
    - mountPath: /mnt
      name: data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: drain-pin-pvc-${i}
EOF
done

deadline=$((SECONDS + TIMEOUT_PVC_BOUND))
while true; do
  bound=$(kubectl get pvc -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -c "^Bound$" || true)
  info "  $bound/3 PVCs bound"
  [[ "$bound" -eq 3 ]] && break
  [[ $SECONDS -ge $deadline ]] && { fail "Test 2: PVCs did not bind"; break; }
  sleep 5
done

deadline=$((SECONDS + TIMEOUT_POD_RUNNING))
while true; do
  running=$(kubectl get pods -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -c "^Running$" || true)
  info "  $running/3 pods running"
  [[ "$running" -eq 3 ]] && break
  [[ $SECONDS -ge $deadline ]] && { fail "Test 2: Pods did not reach Running"; break; }
  sleep 5
done

if [[ "$(kubectl get pvc -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -c "^Bound$" || true)" -eq 3 ]]; then
  # Pin all three PVCs.
  for i in 1 2 3; do
    kubectl annotate pvc "drain-pin-pvc-${i}" -n "$TEST_NS" \
      "simplyblock.io/pinned-volume=true" --overwrite
  done

  # Pick the first node — with 3 PVCs spread across nodes, it is guaranteed
  # to have at least one pinned volume.
  PIN_NODE_UUID=$(get_first_node_uuid)
  info "Triggering drain on node $PIN_NODE_UUID with pinned PVCs"
  trigger_drain "$PIN_NODE_UUID"

  # Drain should stay in Validating / blocked — NOT advance to Suspending.
  sleep 30
  sub_phase=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
  node_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath="{.status.nodes[?(@.uuid==\"$PIN_NODE_UUID\")].status}" 2>/dev/null || true)
  info "  subPhase=$sub_phase node_status=$node_status"

  [[ "$sub_phase" == "Validating" ]] && pass "Test 2: Drain blocked in Validating (pinned PVC)" \
    || fail "Test 2: Expected Validating, got subPhase=$sub_phase"

  # Check PinnedVolumeBlocking event.
  event=$(kubectl get events -n "$NAMESPACE" --field-selector reason=PinnedVolumeBlocking \
    --sort-by=.lastTimestamp -o jsonpath='{.items[-1].reason}' 2>/dev/null || true)
  [[ "$event" == "PinnedVolumeBlocking" ]] && pass "Test 2: PinnedVolumeBlocking event emitted" \
    || fail "Test 2: PinnedVolumeBlocking event not found"

  # Cancel the drain — node must stay online (don't unblock to save the node).
  info "Cancelling drain after verifying block..."
  clear_action

  deadline=$((SECONDS + 60))
  while [[ $SECONDS -lt $deadline ]]; do
    ns=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath="{.status.nodes[?(@.uuid==\"$PIN_NODE_UUID\")].status}" 2>/dev/null || true)
    [[ "$ns" == "online" ]] && break
    sleep 5
  done
  ns=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath="{.status.nodes[?(@.uuid==\"$PIN_NODE_UUID\")].status}" 2>/dev/null || true)
  [[ "$ns" == "online" ]] && pass "Test 2: Node stayed online after cancel" \
    || fail "Test 2: Node status is '$ns', expected online"
fi
cleanup_test_ns
} # end run_test_2

# ══════════════════════════════════════════════════════════════════════════════
# Test 3: Cancel mid-drain — node resumes and returns to online
# ══════════════════════════════════════════════════════════════════════════════
run_test_3() {
section "Test 3: Cancel mid-drain (node must resume)"

cleanup_test_ns
create_pvcs_and_pods 4

CANCEL_NODE=$(get_first_node_uuid)
[[ -z "$CANCEL_NODE" ]] && { fail "Test 3: Could not determine node UUID"; } || {
  info "Triggering drain on $CANCEL_NODE"
  trigger_drain "$CANCEL_NODE"

  # Wait until we reach Migrating.
  if wait_for_subphase "Migrating" 120; then
    info "Reached Migrating — clearing action to cancel"
    clear_action

    # Verify node returns to online within 60s.
    deadline=$((SECONDS + 60))
    resumed=false
    while [[ $SECONDS -lt $deadline ]]; do
      node_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
        -o jsonpath="{.status.nodes[?(@.uuid==\"$CANCEL_NODE\")].status}" 2>/dev/null || true)
      action_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
        -o jsonpath='{.status.actionStatus}' 2>/dev/null || true)
      info "  node_status=$node_status action_status_cleared=$([ -z "$action_status" ] && echo true || echo false)"
      if [[ "$node_status" == "online" ]]; then
        resumed=true; break
      fi
      sleep 5
    done
    $resumed && pass "Test 3: Node resumed to online after cancellation" \
      || fail "Test 3: Node did not return to online after cancel"

    event=$(kubectl get events -n "$NAMESPACE" --field-selector reason=NodeResumed \
      --sort-by=.lastTimestamp -o jsonpath='{.items[-1].reason}' 2>/dev/null || true)
    [[ "$event" == "NodeResumed" ]] && pass "Test 3: NodeResumed event emitted" \
      || fail "Test 3: NodeResumed event not found"
  else
    fail "Test 3: Did not reach Migrating phase within 120s"
    clear_action
  fi
}
cleanup_test_ns
} # end run_test_3

# ══════════════════════════════════════════════════════════════════════════════
# Test 4: Operator restart mid-drain — sub-phase preserved, then cancel
# ══════════════════════════════════════════════════════════════════════════════
run_test_4() {
section "Test 4: Operator restart mid-drain (cancel after verifying)"

cleanup_test_ns
create_pvcs_and_pods 4

RESTART_NODE=$(get_first_node_uuid)
[[ -z "$RESTART_NODE" ]] && { fail "Test 4: Could not determine node UUID"; } || {
  info "Triggering drain on $RESTART_NODE"
  trigger_drain "$RESTART_NODE"

  if wait_for_subphase "Migrating" 120; then
    info "Reached Migrating — restarting operator"
    kubectl rollout restart deployment/"$OPERATOR_DEPLOY" -n "$NAMESPACE"
    kubectl rollout status deployment/"$OPERATOR_DEPLOY" -n "$NAMESPACE" --timeout=120s

    # After restart, sub-phase should still be Migrating (not reset to Validating).
    sleep 10
    sub_phase=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
    info "  subPhase after restart=$sub_phase"
    [[ "$sub_phase" != "Validating" ]] && pass "Test 4: Drain did not reset to Validating after restart" \
      || fail "Test 4: Drain reset to Validating — state was not preserved"

    info "Cancelling drain after verifying sub-phase preserved"
    clear_action
    deadline=$((SECONDS + 90))
    while [[ $SECONDS -lt $deadline ]]; do
      rs=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
        -o jsonpath="{.status.nodes[?(@.uuid==\"$RESTART_NODE\")].status}" 2>/dev/null || true)
      [[ "$rs" == "online" ]] && break
      sleep 5
    done
    rs=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
      -o jsonpath="{.status.nodes[?(@.uuid==\"$RESTART_NODE\")].status}" 2>/dev/null || true)
    [[ "$rs" == "online" ]] && pass "Test 4: Node resumed to online after cancel" \
      || fail "Test 4: Node status is '$rs', expected online"
  else
    fail "Test 4: Did not reach Migrating phase within 120s"
    clear_action
  fi
}
cleanup_test_ns
} # end run_test_4

# ══════════════════════════════════════════════════════════════════════════════
# Test 5: VolumeMigration failure — node is resumed and state = failed
# ══════════════════════════════════════════════════════════════════════════════
run_test_5() {
section "Test 5: VolumeMigration failure triggers resume"

cleanup_test_ns
create_pvcs_and_pods 4

FAIL_NODE=$(get_first_node_uuid)
[[ -z "$FAIL_NODE" ]] && { fail "Test 5: Could not determine node UUID"; } || {
  info "Triggering drain on $FAIL_NODE"
  trigger_drain "$FAIL_NODE"

  # Wait for VolumeMigration CRs to appear.
  deadline=$((SECONDS + 90))
  vmig_name=""
  while [[ $SECONDS -lt $deadline ]]; do
    vmig_name=$(kubectl get volumemigration -n "$NAMESPACE" \
      -l "storage.simplyblock.io/drain-node=$FAIL_NODE" \
      --no-headers 2>/dev/null | awk '{print $1}' | head -1 || true)
    [[ -n "$vmig_name" ]] && break
    sleep 5
  done

  if [[ -n "$vmig_name" ]]; then
    info "Patching $vmig_name to Failed to simulate migration failure"
    kubectl patch volumemigration "$vmig_name" -n "$NAMESPACE" \
      --type=merge --subresource=status \
      -p '{"status":{"phase":"Failed","errorMessage":"simulated failure for test"}}'

    if wait_for_action_state "failed" 120; then
      pass "Test 5: actionStatus.state = failed after VolumeMigration failure"

      node_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
        -o jsonpath="{.status.nodes[?(@.uuid==\"$FAIL_NODE\")].status}" 2>/dev/null || true)
      info "  node status after failure: $node_status"
      [[ "$node_status" == "online" ]] && pass "Test 5: Node resumed to online after migration failure" \
        || fail "Test 5: Node status is '$node_status', expected online"

      event=$(kubectl get events -n "$NAMESPACE" --field-selector reason=NodeResumed \
        --sort-by=.lastTimestamp -o jsonpath='{.items[-1].reason}' 2>/dev/null || true)
      [[ "$event" == "NodeResumed" ]] && pass "Test 5: NodeResumed event emitted on failure" \
        || fail "Test 5: NodeResumed event not found after migration failure"
    else
      fail "Test 5: actionStatus did not reach failed within 120s"
      clear_action
    fi
  else
    fail "Test 5: No VolumeMigration CRs appeared within 90s"
    clear_action
  fi
}
cleanup_test_ns
} # end run_test_5

# ══════════════════════════════════════════════════════════════════════════════
# Test 6: fio under drain — confirm I/O is not interrupted (removes a node)
# ══════════════════════════════════════════════════════════════════════════════
run_test_6() {
section "Test 6: fio workload survives drain-remove"

cleanup_test_ns
kubectl get ns "$TEST_NS" &>/dev/null || kubectl create ns "$TEST_NS"

FIO_COUNT=6
info "Creating $FIO_COUNT PVCs with fio pods..."
for i in $(seq 1 $FIO_COUNT); do
  kubectl apply -n "$TEST_NS" -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fio-pvc-${i}
spec:
  storageClassName: ${SC}
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: Pod
metadata:
  name: fio-pod-${i}
spec:
  containers:
  - name: fio
    image: nixery.dev/shell/fio
    command:
    - fio
    - --name=test
    - --filename=/mnt/testfile
    - --size=512M
    - --rw=randrw
    - --ioengine=libaio
    - --bs=4k
    - --numjobs=1
    - --runtime=9999999
    - --time_based
    - --fallocate=none
    - --status-interval=5
    - --output-format=normal
    volumeMounts:
    - mountPath: /mnt
      name: data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: fio-pvc-${i}
EOF
done

deadline=$((SECONDS + TIMEOUT_PVC_BOUND))
while true; do
  bound=$(kubectl get pvc -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -c "^Bound$" || true)
  info "  $bound/$FIO_COUNT PVCs bound"
  [[ "$bound" -eq "$FIO_COUNT" ]] && break
  [[ $SECONDS -ge $deadline ]] && { fail "Test 6: PVCs did not bind"; cleanup_test_ns; return; }
  sleep 5
done

deadline=$((SECONDS + TIMEOUT_POD_RUNNING))
while true; do
  running=$(kubectl get pods -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -c "^Running$" || true)
  info "  $running/$FIO_COUNT fio pods running"
  [[ "$running" -eq "$FIO_COUNT" ]] && break
  [[ $SECONDS -ge $deadline ]] && { fail "Test 6: fio pods did not reach Running"; cleanup_test_ns; return; }
  sleep 5
done
pass "Test 6: All $FIO_COUNT fio pods running"

# --fallocate=none means fio starts IO immediately with no file layout phase.
# Wait 30s then verify all pods are still Running before starting the drain.
info "Letting fio warm up for 30s (--fallocate=none skips file layout)..."
sleep 30

running=$(kubectl get pods -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
  | tr ' ' '\n' | grep -c "^Running$" || true)
if [[ "$running" -ne "$FIO_COUNT" ]]; then
  fail "Test 6: Only $running/$FIO_COUNT fio pods still running after warmup"
  cleanup_test_ns
  return
fi
pass "Test 6: All $FIO_COUNT fio pods generating active IO"

FIO_NODE=$(get_first_node_uuid)
[[ -z "$FIO_NODE" ]] && { fail "Test 6: Could not determine node UUID"; cleanup_test_ns; return; }
info "Triggering drain on $FIO_NODE while fio is running"
trigger_drain "$FIO_NODE"

# Poll drain progress — also check fio pods stay Running throughout.
info "Waiting for drain to complete while monitoring fio pods (timeout: ${TIMEOUT_MIGRATION}s)..."
deadline=$((SECONDS + TIMEOUT_MIGRATION))
fio_interrupted=false
while true; do
  state=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.state}' 2>/dev/null || true)
  sp=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
  migrated=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.volumesMigrated}' 2>/dev/null || echo "0")
  pending=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.volumesPending}' 2>/dev/null || echo "?")
  info "  drain: state=$state subPhase=$sp migrated=$migrated pending=$pending"

  # Check all fio pods are still running.
  not_running=$(kubectl get pods -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -vc "^Running$" || true)
  if [[ "$not_running" -gt 0 ]]; then
    fio_interrupted=true
    info "  WARNING: $not_running fio pod(s) not in Running state"
  fi

  [[ "$state" == "success" ]] && break
  [[ "$state" == "failed" ]] && { fail "Test 6: Drain failed: $(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" -o jsonpath='{.status.actionStatus.message}')"; break; }
  [[ $SECONDS -ge $deadline ]] && { fail "Test 6: Drain did not complete within timeout"; break; }
  sleep 10
done

state=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
  -o jsonpath='{.status.actionStatus.state}' 2>/dev/null || true)
[[ "$state" == "success" ]] && pass "Test 6: Drain completed successfully under fio load" \
  || fail "Test 6: Drain did not reach success"

# Wait until the node's backend status is confirmed 'removed' in StorageNodeSet
# status before cleaning up — fio must keep running through the full removal.
info "Waiting for node $FIO_NODE to show 'removed' status in StorageNodeSet (timeout: 120s)..."
deadline=$((SECONDS + 120))
while true; do
  node_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath="{.status.nodes[?(@.uuid==\"$FIO_NODE\")].status}" 2>/dev/null || true)
  info "  node backend status=$node_status"
  [[ "$node_status" == "removed" || -z "$node_status" ]] && break
  [[ $SECONDS -ge $deadline ]] && {
    info "  node status did not reach 'removed' within 120s, proceeding"
    break
  }
  # Verify fio is still running while waiting.
  not_running=$(kubectl get pods -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -vc "^Running$" || true)
  [[ "$not_running" -gt 0 ]] && { fio_interrupted=true; info "  WARNING: $not_running fio pod(s) not Running during removal wait"; }
  sleep 10
done

# Final fio pod check.
running=$(kubectl get pods -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
  | tr ' ' '\n' | grep -c "^Running$" || true)
info "  fio pods still running after drain: $running/$FIO_COUNT"
[[ "$running" -eq "$FIO_COUNT" ]] \
  && pass "Test 6: All $FIO_COUNT fio pods still running after drain" \
  || fail "Test 6: Only $running/$FIO_COUNT fio pods running after drain"

# Check fio logs for fatal I/O errors.
fio_errors=0
for i in $(seq 1 $FIO_COUNT); do
  errors=$(kubectl logs fio-pod-${i} -n "$TEST_NS" 2>/dev/null \
    | grep -ciE "fatal|io error|failed|error:" || true)
  [[ "$errors" -gt 0 ]] && { info "  fio-pod-${i}: $errors error line(s) found"; fio_errors=$((fio_errors + 1)); }
done
[[ "$fio_errors" -eq 0 ]] \
  && pass "Test 6: No fio I/O errors detected in pod logs" \
  || fail "Test 6: fio errors detected in $fio_errors pod(s)"

$fio_interrupted \
  && fail "Test 6: fio pods were interrupted during drain" \
  || pass "Test 6: fio ran uninterrupted throughout drain"

cleanup_test_ns
} # end run_test_6

# ══════════════════════════════════════════════════════════════════════════════
# Test 7: Empty node — no PV-managed volumes, drain completes without migrations
# ══════════════════════════════════════════════════════════════════════════════
run_test_7() {
section "Test 7: Empty node drain (no VolumeMigration CRs expected)"

# Do NOT create any PVCs — we want the target node to have no user volumes.
EMPTY_NODE=$(get_first_node_uuid)
[[ -z "$EMPTY_NODE" ]] && { fail "Test 7: Could not determine node UUID"; return; }
info "Triggering drain on empty node $EMPTY_NODE"
trigger_drain "$EMPTY_NODE"

if wait_for_action_state "success" "$TIMEOUT_MIGRATION"; then
  pass "Test 7: Drain completed on empty node"

  # Verify no VolumeMigration CRs were ever created.
  vmig_count=$(kubectl get volumemigration -n "$NAMESPACE" \
    -l "storage.simplyblock.io/drain-node=$EMPTY_NODE" \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')
  [[ "$vmig_count" -eq 0 ]] \
    && pass "Test 7: No VolumeMigration CRs created (empty node skipped migration)" \
    || fail "Test 7: Unexpected VolumeMigration CRs created: $vmig_count"

  # Verify node removed from status (absent or status=removed).
  node_final_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath="{.status.nodes[?(@.uuid==\"$EMPTY_NODE\")].status}" 2>/dev/null || true)
  [[ -z "$node_final_status" || "$node_final_status" == "removed" ]] \
    && pass "Test 7: Node removed from StorageNodeSet status" \
    || fail "Test 7: Node still present with status=$node_final_status"

  # Confirm volumesMigrated counter is 0.
  migrated=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.volumesMigrated}' 2>/dev/null || echo "0")
  migrated="${migrated:-0}"
  [[ "$migrated" -eq 0 ]] \
    && pass "Test 7: volumesMigrated=0 as expected for empty node" \
    || fail "Test 7: Expected volumesMigrated=0, got $migrated"
else
  fail "Test 7: Drain did not reach success within timeout"
  clear_action
fi
} # end run_test_7

# ══════════════════════════════════════════════════════════════════════════════
# Test 8: Rapid action toggle — set/clear/set does not leak state
# ══════════════════════════════════════════════════════════════════════════════
run_test_8() {
section "Test 8: Action toggle — cancel at Validating, re-trigger, verify clean restart"

cleanup_test_ns
create_pvcs_and_pods 4

TOGGLE_NODE=$(get_first_node_uuid)
[[ -z "$TOGGLE_NODE" ]] && { fail "Test 8: Could not determine node UUID"; cleanup_test_ns; return; }

# Step 1: trigger drain → allow one reconcile to initialise → cancel immediately.
# Cancel early (Validating) so the node is never suspended — no resume needed.
info "Step 1: trigger drain, cancel at Validating"
trigger_drain "$TOGGLE_NODE"
sleep 15
clear_action

# Wait for ActionStatus to be cleared by drainHandleCancellation.
deadline=$((SECONDS + 60))
while [[ $SECONDS -lt $deadline ]]; do
  state=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.state}' 2>/dev/null || true)
  sp=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
  info "  waiting for clear: state=$state subPhase=$sp"
  [[ -z "$state" || -z "$sp" ]] && break
  sleep 5
done

state=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
  -o jsonpath='{.status.actionStatus.state}' 2>/dev/null || true)
[[ -z "$state" ]] \
  && pass "Test 8: ActionStatus cleared after first cancel" \
  || fail "Test 8: ActionStatus not cleared (state=$state)"

# Verify node stayed online — it was never suspended.
node_status=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
  -o jsonpath="{.status.nodes[?(@.uuid==\"$TOGGLE_NODE\")].status}" 2>/dev/null || true)
[[ "$node_status" == "online" ]] \
  && pass "Test 8: Node stayed online after cancel at Validating" \
  || fail "Test 8: Node status is '$node_status', expected online"

# Step 2: re-trigger drain — state machine must restart cleanly from Validating.
info "Step 2: re-trigger drain — verify clean restart"
trigger_drain "$TOGGLE_NODE"

if wait_for_subphase "Suspending|Migrating|Verifying|Removing" 120; then
  sp=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
  pass "Test 8: Drain restarted cleanly from scratch (subPhase=$sp)"
else
  fail "Test 8: Drain did not advance past Validating after re-trigger"
  clear_action
  cleanup_test_ns
  return
fi

# Cancel the second drain immediately — don't wait for it to progress further.
info "Cancelling second drain immediately"
clear_action

# Give the cancellation handler time to resume and clear state.
if wait_for_node_online "$TOGGLE_NODE" 120; then
  pass "Test 8: Node returned to online after second cancel"
else
  fail "Test 8: Node did not return to online after second cancel"
fi

sp=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
  -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
[[ -z "$sp" ]] \
  && pass "Test 8: No leaked SubPhase after toggle" \
  || fail "Test 8: Leaked SubPhase=$sp after toggle"

cleanup_test_ns
} # end run_test_8

# ══════════════════════════════════════════════════════════════════════════════
# Test 9: System-volume-only node — drain completes without VolumeMigration CRs
# ══════════════════════════════════════════════════════════════════════════════
run_test_9() {
section "Test 9: System-volume-only node skips migration"

POOL_NAME=$(kubectl get pool -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}')
[[ -z "$POOL_NAME" ]] && {
  fail "Test 9: Could not resolve pool name"
  return
}

# Pin the system volume to the first node using --host-id so we know exactly
# which node to drain without a separate lookup.
SYS_NODE=$(get_first_node_uuid)
[[ -z "$SYS_NODE" ]] && { fail "Test 9: Could not determine node UUID"; return; }

SYS_VOL_NAME="sb-fio-baseline-drain-test-$$"
info "Creating system volume $SYS_VOL_NAME on node $SYS_NODE"
SYS_VOL_UUID=$(create_backend_volume "$POOL_NAME" "$SYS_VOL_NAME" "2G" "$SYS_NODE")
if [[ -z "$SYS_VOL_UUID" ]]; then
  fail "Test 9: Failed to create system volume via sbctl"
  return
fi
info "  system volume UUID: $SYS_VOL_UUID"

# Trigger drain on that node — should complete without VolumeMigration CRs.
trigger_drain "$SYS_NODE"

if wait_for_action_state "success" "$TIMEOUT_MIGRATION"; then
  pass "Test 9: Drain completed on system-volume-only node"

  vmig_count=$(kubectl get volumemigration -n "$NAMESPACE" \
    -l "storage.simplyblock.io/drain-node=$SYS_NODE" \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')
  [[ "$vmig_count" -eq 0 ]] \
    && pass "Test 9: No VolumeMigration CRs created (system volumes skipped)" \
    || fail "Test 9: Unexpected VolumeMigration CRs created: $vmig_count"

  migrated=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.volumesMigrated}' 2>/dev/null || echo "0")
  migrated="${migrated:-0}"
  [[ "$migrated" -eq 0 ]] \
    && pass "Test 9: volumesMigrated=0 — system volumes correctly excluded" \
    || fail "Test 9: Expected volumesMigrated=0, got $migrated"
else
  fail "Test 9: Drain did not reach success within timeout"
  clear_action
  delete_backend_volume "$SYS_VOL_UUID"
fi
# System volume deleted when node was removed; nothing to clean up.
} # end run_test_9

# ══════════════════════════════════════════════════════════════════════════════
# Test 10: Both pinned and unmanaged volumes visible simultaneously
# ══════════════════════════════════════════════════════════════════════════════
run_test_10() {
section "Test 10: Both pinned + unmanaged volumes block drain simultaneously"

POOL_NAME=$(kubectl get pool -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}')
[[ -z "$POOL_NAME" ]] && {
  fail "Test 10: Could not resolve pool name"
  return
}

cleanup_test_ns
kubectl get ns "$TEST_NS" &>/dev/null || kubectl create ns "$TEST_NS"

# Create 3 PVCs (distributed across nodes) and pin all of them.
for i in 1 2 3; do
  kubectl apply -n "$TEST_NS" -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drain-both-pvc-${i}
spec:
  storageClassName: ${SC}
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: Pod
metadata:
  name: drain-both-pod-${i}
spec:
  containers:
  - name: writer
    image: busybox
    command: ["sh", "-c", "while true; do echo ok >> /mnt/data; sleep 5; done"]
    volumeMounts:
    - mountPath: /mnt
      name: data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: drain-both-pvc-${i}
EOF
done

deadline=$((SECONDS + TIMEOUT_PVC_BOUND))
while true; do
  bound=$(kubectl get pvc -n "$TEST_NS" -o jsonpath='{.items[*].status.phase}' \
    | tr ' ' '\n' | grep -c "^Bound$" || true)
  [[ "$bound" -eq 3 ]] && break
  [[ $SECONDS -ge $deadline ]] && { fail "Test 10: PVCs did not bind"; cleanup_test_ns; return; }
  sleep 5
done

for i in 1 2 3; do
  kubectl annotate pvc "drain-both-pvc-${i}" -n "$TEST_NS" \
    "simplyblock.io/pinned-volume=true" --overwrite
done

# Pick the target node first, then pin the unmanaged volume to it via --host-id.
# This guarantees both a pinned PVC and an unmanaged volume land on the same node.
TARGET_NODE=$(get_first_node_uuid)
[[ -z "$TARGET_NODE" ]] && { fail "Test 10: Could not determine node UUID"; cleanup_test_ns; return; }

UNMANAGED_NAME="drain-unmanaged-test-$$"
info "Creating unmanaged volume $UNMANAGED_NAME on node $TARGET_NODE"
UNMANAGED_UUID=$(create_backend_volume "$POOL_NAME" "$UNMANAGED_NAME" "1G" "$TARGET_NODE")
if [[ -z "$UNMANAGED_UUID" ]]; then
  fail "Test 10: Failed to create unmanaged volume"
  cleanup_test_ns
  return
fi
info "  unmanaged volume UUID: $UNMANAGED_UUID"

info "Triggering drain on $TARGET_NODE"
trigger_drain "$TARGET_NODE"

# Wait 60s for both events to surface.
sleep 60
sp=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
  -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
[[ "$sp" == "Validating" ]] \
  && pass "Test 10: Drain blocked in Validating (both blockers present)" \
  || fail "Test 10: Expected Validating subPhase, got '$sp'"

pinned_event=$(kubectl get events -n "$NAMESPACE" \
  --field-selector reason=PinnedVolumeBlocking \
  --sort-by=.lastTimestamp -o jsonpath='{.items[-1].reason}' 2>/dev/null || true)
[[ "$pinned_event" == "PinnedVolumeBlocking" ]] \
  && pass "Test 10: PinnedVolumeBlocking event emitted" \
  || fail "Test 10: PinnedVolumeBlocking event not found"

unmanaged_event=$(kubectl get events -n "$NAMESPACE" \
  --field-selector reason=UnmanagedVolumeBlocking \
  --sort-by=.lastTimestamp -o jsonpath='{.items[-1].reason}' 2>/dev/null || true)
[[ "$unmanaged_event" == "UnmanagedVolumeBlocking" ]] \
  && pass "Test 10: UnmanagedVolumeBlocking event emitted" \
  || fail "Test 10: UnmanagedVolumeBlocking event not found (unmanaged volume may be on a different node)"

# Cancel drain and clean up.
info "Cancelling drain and cleaning up"
clear_action
sleep 30
delete_backend_volume "$UNMANAGED_UUID"
cleanup_test_ns
} # end run_test_10

# ══════════════════════════════════════════════════════════════════════════════
# Test 11: Drain pauses when cluster becomes rebalancing mid-drain, resumes when active
# ══════════════════════════════════════════════════════════════════════════════
run_test_11() {
section "Test 11: Drain pauses when second node shuts down (cluster degraded), resumes after restart"

# Get the admin-control pod to run sbctl commands.
ADMINPOD=$(kubectl get pods -n "$NAMESPACE" -l app=simplyblock-admin-control \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -z "$ADMINPOD" ]] && { fail "Test 11: Could not find admin-control pod"; return; }

# More PVCs so migration runs long enough to observe the pause mid-flight.
cleanup_test_ns
create_pvcs_and_pods 24

# Pick drain node (index 0) and the node to shut down (index 1).
DRAIN_NODE=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
  -o jsonpath='{.status.nodes[0].uuid}')
SECOND_NODE=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
  -o jsonpath='{.status.nodes[1].uuid}')
[[ -z "$DRAIN_NODE" || -z "$SECOND_NODE" ]] && {
  fail "Test 11: Could not determine drain/second node UUIDs"
  cleanup_test_ns; return
}
info "Drain node: $DRAIN_NODE  Second node (to shut down): $SECOND_NODE"

info "Triggering drain on $DRAIN_NODE"
trigger_drain "$DRAIN_NODE"

# Wait until Migrating so volumes are actively being moved.
if ! wait_for_subphase "Migrating" 180; then
  fail "Test 11: Drain did not reach Migrating within 180s"
  clear_action
  cleanup_test_ns
  return
fi
pass "Test 11: Drain reached Migrating phase"

# Shut down the second storage node — this degrades the cluster, which should
# cause the drain to pause rather than continue or fail.
info "Shutting down second node $SECOND_NODE to degrade the cluster..."
kubectl exec -n "$NAMESPACE" "$ADMINPOD" -- \
  sbctl sn shutdown "$SECOND_NODE" 2>/dev/null || true

# Wait up to 120s for the cluster to become degraded and the drain to pause.
info "Waiting for drain to pause (cluster degraded)..."
deadline=$((SECONDS + 120))
paused=false
while [[ $SECONDS -lt $deadline ]]; do
  sp=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
  msg=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.message}' 2>/dev/null || true)
  cluster_status=$(kubectl get storagecluster -n "$NAMESPACE" \
    -o jsonpath='{.items[0].status.status}' 2>/dev/null || true)
  info "  subPhase=$sp cluster_status=$cluster_status message=$msg"
  if echo "$msg" | grep -qi "paused"; then
    paused=true; break
  fi
  sleep 10
done

$paused \
  && pass "Test 11: Drain paused while cluster is degraded" \
  || fail "Test 11: Drain did not pause within 120s after second node shutdown"

event=$(kubectl get events -n "$NAMESPACE" --field-selector reason=DrainPaused \
  --sort-by=.lastTimestamp -o jsonpath='{.items[-1].reason}' 2>/dev/null || true)
[[ "$event" == "DrainPaused" ]] \
  && pass "Test 11: DrainPaused event emitted" \
  || fail "Test 11: DrainPaused event not found"

# Restart the second node to bring the cluster back to active.
info "Restarting second node $SECOND_NODE to recover the cluster..."
kubectl exec -n "$NAMESPACE" "$ADMINPOD" -- \
  sbctl sn restart "$SECOND_NODE" 2>/dev/null || true

# Wait for cluster to become active again.
info "Waiting for cluster to become active..."
deadline=$((SECONDS + 300))
while [[ $SECONDS -lt $deadline ]]; do
  cluster_status=$(kubectl get storagecluster -n "$NAMESPACE" \
    -o jsonpath='{.items[0].status.status}' 2>/dev/null || true)
  info "  cluster_status=$cluster_status"
  [[ "$cluster_status" == "active" ]] && break
  sleep 15
done
cluster_status=$(kubectl get storagecluster -n "$NAMESPACE" \
  -o jsonpath='{.items[0].status.status}' 2>/dev/null || true)
[[ "$cluster_status" == "active" ]] \
  && pass "Test 11: Cluster returned to active after node restart" \
  || fail "Test 11: Cluster did not become active within 300s"

# Wait for drain to advance past Migrating (resume).
info "Waiting for drain to resume..."
deadline=$((SECONDS + 600))
resumed=false
while [[ $SECONDS -lt $deadline ]]; do
  sp=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.subPhase}' 2>/dev/null || true)
  state=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.state}' 2>/dev/null || true)
  migrated=$(kubectl get storagenodeset "$STORAGENODESET" -n "$NAMESPACE" \
    -o jsonpath='{.status.actionStatus.volumesMigrated}' 2>/dev/null || echo "0")
  info "  subPhase=$sp state=$state migrated=$migrated"
  if [[ "$sp" != "Migrating" || "$state" == "success" || "$state" == "failed" ]]; then
    resumed=true; break
  fi
  sleep 10
done

$resumed \
  && pass "Test 11: Drain resumed after cluster became active" \
  || fail "Test 11: Drain did not resume within 300s after cluster activated"

# Cancel the drain — non-destructive, node stays online.
info "Cancelling drain"
clear_action
if wait_for_node_online "$DRAIN_NODE" 90; then
  pass "Test 11: Drain node resumed online after cancel"
else
  fail "Test 11: Drain node did not return to online after cancel"
fi

cleanup_test_ns
} # end run_test_11

# ── Dispatcher ────────────────────────────────────────────────────────────────
# Default order: non-destructive tests first, node-removing tests last
if [[ $# -gt 0 ]]; then
  TESTS=("$@")
else
  TESTS=(2 3 4 5 8 11 10 9 7 1 6)
fi

for t in "${TESTS[@]}"; do
  case "$t" in
    1) run_test_1 ;;
    2) run_test_2 ;;
    3) run_test_3 ;;
    4) run_test_4 ;;
    5) run_test_5 ;;
    6) run_test_6 ;;
    7) run_test_7 ;;
    8) run_test_8 ;;
    9) run_test_9 ;;
    10) run_test_10 ;;
    11) run_test_11 ;;
    *) echo "[WARN] Unknown test number: $t (valid: 1-11)"; FAILED=$((FAILED + 1)) ;;
  esac
done

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════"
echo " Results: $PASSED passed, $FAILED failed"
echo "════════════════════════════════════════════"
[[ "$FAILED" -eq 0 ]] && exit 0 || exit 1
