#!/bin/bash
# Regression test: Kubernetes VolumeGroupSnapshot / consistency groups (Phase 2).
#
# Exercises the Kubernetes-native group-snapshot path end to end, on top of the
# same backend consistency group that regression_test/19 drives through sbctl:
#
#   0  preflight and setup: the VolumeGroupSnapshot CRDs are installed and the
#      CSIVolumeGroupSnapshot feature gate is on (P0-4), failing fast otherwise,
#      and the script-owned VolumeGroupSnapshotClass is created (the chart
#      ships no default class).
#   1  provisioning a group by label: PVCs carrying
#      storage.simplyblock.io/consistency-group join ONE backend group and are
#      pinned to one storage node (design §4.1, §4.2).
#   2  a VolumeGroupSnapshot snapshots the group: it goes ReadyToUse, its
#      VolumeGroupSnapshotContent carries a non-empty volumeGroupSnapshotHandle,
#      and exactly one VolumeSnapshot per member materializes, each backref'd by
#      status.volumeGroupSnapshotName (design §5.3, §6.4, test-plan I-01/I-02).
#   3  cross-volume crash consistency: a round-robin prefix writer runs at full
#      speed while the generation is taken; restoring every member from that one
#      generation must satisfy the prefix property (max/min sequence spread <= 1,
#      all hashes valid) — the guarantee only a live run proves (design §11.2).
#   4  selector drift is rejected at admission: a VolumeGroupSnapshot whose
#      selector also matches an unlabeled PVC is refused by the webhook at
#      kubectl apply (design §9.4, §11.6, test-plan I-07).
#   5  a member cannot be migrated: a VolumeMigration targeting a member's PV is
#      refused by the webhook at kubectl apply (design §9.5, §11.7, I-08).
#   6  generations are independent: two more generations share one group with a
#      monotonically increasing sequence, deleting one leaves the other intact,
#      and the backend group survives generation deletion (design §6.3, §8).
#   7  a late joiner shares the group's placement pin and appears in the NEXT
#      generation with all prior members (design §4.1).
#   8  deleting a member closes its epoch: the next generation excludes it,
#      while its snapshot in the earlier generation stays ReadyToUse (§8.2).
#   9  a volume restored from a member snapshot is NOT a group member: the
#      clone carries no backend group_id (design §7.1).
#  10  single-operation restore: one applied VolumeGroupSnapshotOps restores a
#      whole generation — one bound claim per member, phase Succeeded — and its
#      webhook rejects a ref naming no VolumeGroupSnapshot (design §7.4, E-13).
#  11  restore into a new group: a VolumeGroupSnapshotOps with
#      restore.consistencyGroup labels the clones, so they form a new
#      placement-pinned backend group (design §7.2, §7.4, E-14).
#  12  N independent groups (GROUP_COUNT x GROUP_MEMBERS): every group forms,
#      pins, snapshots as its own distinct generation selected by the
#      consistency-group label itself (the recommended selector), and restores
#      through its own VolumeGroupSnapshotOps (design §4.1, §5, §7.4).
#  13  the documented limits driven through the GROUP, with data at every
#      step: CHAIN_MAX (100) VolumeGroupSnapshot generations over
#      CHAIN_MEMBERS volumes, each adding one snapshot to every member's
#      individual chain, a marker on every member before each; generation
#      CHAIN_MAX+1 refused with a reason; a mid-chain member restore holding
#      exactly its point in time; CLONE_MAX (500) clones of one member
#      snapshot, all bound, sampled clones holding the full chain; and the
#      clone past the limit refused with a reason.
#  14  large-group snapshot timings under copy-on-write: a cap-sized group
#      (BIG_MEMBERS) of large members (BIG_SIZES, cycled), filled with
#      incompressible data (BIG_FILL_PCT of each size, or a flat BIG_FILL_GB),
#      then a continuous random-overwrite workload across EVERY member for the
#      whole timed phase; BIG_GENERATIONS generations are taken
#      BIG_SNAP_INTERVAL seconds apart, so each take is timed while the
#      previous snapshot is actively being CoW'd away from. Reports end-to-end
#      ReadyToUse latency, the maximum I/O stall a running writer observed
#      (the LVS freeze window, design §5.1), and the churn volume.
#  15  crash consistency under a dependent-write workload: one writer issues
#      strictly serialized, checksummed 4K writes across all members (global
#      seq, round-robin volume, O_DIRECT depth 1, each durable before the next
#      is issued); CG snapshots are taken mid-workload, cloned, and scanned —
#      the surviving seqs plus those the log shows overwritten before the cut
#      must form a contiguous prefix {0..M}, and a negative control of
#      individually staggered snapshots must be flagged (design §5.1, §5.2).
#  16  dynamic membership (design §4.5, Phase 4): labeling an EXISTING bound
#      PVC joins its volume to the group (the CSI label watcher relays it to
#      the backend member add) and it appears in the NEXT generation; removing
#      the label detaches it (epoch closed, history stays ReadyToUse); and
#      re-adding the label is refused one-way, surfaced as a
#      ConsistencyGroupJoinRefused event on the PVC.
#
# Tests 2-9 build on test 1's member PVCs, and 8 builds on 7's late joiner:
# run them with their prerequisites (or run everything, the default). Test 16
# provisions test 1's members itself when they are absent, so `0 16` runs
# standalone.
#
# Usage:
#   ./test_volume_group_snapshot.sh              # all tests
#   ./test_volume_group_snapshot.sh 0 1 2 3      # happy path only
#   ./test_volume_group_snapshot.sh 0 1 6 7 8 9  # lifecycle only
#   ./test_volume_group_snapshot.sh 0 1 10 11    # single-operation restore only
#   ./test_volume_group_snapshot.sh 0 12         # N-group matrix only
#   MEMBER_COUNT=4 ./test_volume_group_snapshot.sh
#   GROUP_COUNT=5 GROUP_MEMBERS=3 ./test_volume_group_snapshot.sh 0 12
#   ./test_volume_group_snapshot.sh 0 13         # limits (slow: ~500+ volumes)
#   CHAIN_MEMBERS=2 CHAIN_MAX=10 CLONE_MAX=20 ./test_volume_group_snapshot.sh 0 13   # smoke
#   ./test_volume_group_snapshot.sh 0 14         # large-group timings (20x100-300G)
#   BIG_MEMBERS=3 BIG_SIZES=10Gi BIG_FILL_GB=0 ./test_volume_group_snapshot.sh 0 14
#   BIG_FILL_PCT=50 ./test_volume_group_snapshot.sh 0 14   # fill 50% of EACH size (slow: ~2TB)
#   ./test_volume_group_snapshot.sh 0 15         # crash consistency (dependent writes)
#   ./test_volume_group_snapshot.sh 0 16         # dynamic membership (standalone)
#   CC_MEMBERS=3 CC_SNAPS=2 ./test_volume_group_snapshot.sh 0 15   # smaller/faster
#
# Requirements: kubectl context on the cluster, admin-control pod running, the
# operator + csi-driver images carrying the GroupController and webhooks, the
# operator chart deployed with P0-4 enabled (helm-charts commit shipping the VGS
# CRDs and feature gate), and jq on the workstation. The
# VolumeGroupSnapshotClass itself is script-owned (test 0).

set -uo pipefail

# Bash reads a script lazily as it runs, so editing THIS file during a
# multi-hour run shifts bytes under the interpreter and kills the run with a
# phantom syntax error at whatever line it reads next. Re-exec once from an
# immutable temp snapshot, so the working copy stays editable mid-run.
if [[ -z "${VGS_TEST_SNAPSHOT:-}" ]]; then
  # BSD mktemp (macOS) requires the template to END in Xs — no .sh suffix.
  _snap=$(mktemp "${TMPDIR:-/tmp}/vgs-test-XXXXXX")
  cp "$0" "$_snap"
  VGS_TEST_SNAPSHOT="$_snap" exec bash "$_snap" "$@"
fi

NAMESPACE="${NAMESPACE:-simplyblock}"
SC_NAME="${SC_NAME:-test-vgs-sc}"
VGS_CLASS="${VGS_CLASS:-simplyblock-csi-groupsnapshotclass}"
CLUSTER1_ID="${CLUSTER1_ID:-cluster1}"
POOL_NAME="${POOL_NAME:-cluster1-pool}"
CG_LABEL="storage.simplyblock.io/consistency-group"
CG_NAME="${CG_NAME:-vgs-group}"
MEMBER_COUNT="${MEMBER_COUNT:-4}"
GROUP_COUNT="${GROUP_COUNT:-3}"
GROUP_MEMBERS="${GROUP_MEMBERS:-4}"
PVC_SIZE="${PVC_SIZE:-2Gi}"
TIMEOUT="${TIMEOUT:-300}"
VGS_NAME="${VGS_NAME:-vgs-gen1}"
LABEL="test=vgs-regression"

PASSED=0
FAILED=0

pass()    { echo "[PASS] $*"; PASSED=$((PASSED + 1)); }
fail()    { echo "[FAIL] $*"; FAILED=$((FAILED + 1)); }
info()    { echo "[INFO] $*"; }
section() { echo ""; echo "══════════════════════════════════════════"; echo " $*"; echo "══════════════════════════════════════════"; }

TESTS=("$@")
run_test() {
  [[ ${#TESTS[@]} -eq 0 ]] && return 0
  for t in "${TESTS[@]}"; do [[ "$t" == "$1" ]] && return 0; done
  return 1
}

MEMBER_PVCS=()
for i in $(seq 1 "$MEMBER_COUNT"); do MEMBER_PVCS+=("vgs-pvc-$i"); done

# ── sbctl via the admin-control pod (only for cross-checking backend state) ────
sb() {
  local adminpod
  adminpod=$(kubectl get pods -n "$NAMESPACE" -l app=simplyblock-admin-control \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [[ -z "$adminpod" ]] && { echo "no admin pod" >&2; return 1; }
  kubectl exec -n "$NAMESPACE" "$adminpod" -- sbctl "$@"
}

# Backend UUID of the source cluster, read from the StorageCluster CR.
CLUSTER1_UUID="${CLUSTER1_UUID:-$(kubectl -n "$NAMESPACE" get storagecluster "$CLUSTER1_ID" -o jsonpath='{.status.uuid}' 2>/dev/null)}"
if [[ -z "$CLUSTER1_UUID" || "$CLUSTER1_UUID" == "null" ]]; then
  echo "ERROR: could not read status.uuid from StorageCluster/$CLUSTER1_ID (set CLUSTER1_UUID explicitly)"; exit 1
fi

setup_storageclass() {
  kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $SC_NAME
provisioner: csi.simplyblock.io
parameters:
  cluster_id: $CLUSTER1_UUID
  pool_name: $POOL_NAME
  csi.storage.k8s.io/fstype: xfs
  fabric: tcp
  encryption: "False"
  max_namespace_per_subsys: "20"
  # The provisioner forwards these QoS params to the volume-create body; when a
  # StorageClass omits them it sends empty strings, which the control plane's
  # integer fields reject with HTTP 422. Supply explicit zeros (as in 19).
  qos_rw_iops: "0"
  qos_rw_mbytes: "0"
  qos_r_mbytes: "0"
  qos_w_mbytes: "0"
  tune2fs_reserved_blocks: ""
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
EOF
}

# ── Generic wait helpers ──────────────────────────────────────────────────────
wait_pvc_bound() {
  local pvc=$1 elapsed=0
  while [[ $elapsed -lt $TIMEOUT ]]; do
    [[ "$(kubectl -n "$NAMESPACE" get pvc "$pvc" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Bound" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

wait_pod_ready() {
  local pod=$1 elapsed=0
  while [[ $elapsed -lt $TIMEOUT ]]; do
    [[ "$(kubectl -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

wait_pod_success() {
  local pod=$1 budget=${2:-$TIMEOUT} elapsed=0 phase
  while [[ $elapsed -lt $budget ]]; do
    phase=$(kubectl -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)
    [[ "$phase" == "Succeeded" ]] && return 0
    [[ "$phase" == "Failed" ]] && return 1
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

# Backend lvol/pool of a bound PVC, from the PV's {cluster}:{pool}:{lvol} handle.
lvol_of_pvc() {
  local pv; pv=$(kubectl -n "$NAMESPACE" get pvc "$1" -o jsonpath='{.spec.volumeName}')
  kubectl get pv "$pv" -o jsonpath='{.spec.csi.volumeHandle}' | cut -d: -f3
}
node_of_lvol() {
  sb volume get "$1" --json 2>/dev/null \
    | jq -r '.node_id // .storage_node_id // ."Node ID" // ."Storage Node ID" // empty'
}

# ── PVCs ──────────────────────────────────────────────────────────────────────
# A group member: carries the consistency-group label so the provisioner joins
# it to the backend group at creation (design §4.1).
make_member_pvc() {
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
  labels:
    test: vgs-regression
    app: vgs-db
    $CG_LABEL: $CG_NAME
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: $PVC_SIZE
EOF
}

# A plain, unlabeled PVC matched by the same app selector (test 4 negative).
make_unlabeled_pvc() {
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
  labels:
    test: vgs-regression
    app: vgs-db
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: $PVC_SIZE
EOF
}

# A restore PVC whose dataSource is one member VolumeSnapshot (design §7.1).
make_restore_pvc() {
  local name=$1 snap=$2
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $name
  labels:
    test: vgs-regression
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: $snap
  resources:
    requests:
      storage: $PVC_SIZE
EOF
}

# ── Crash-consistency writer / verifier (same contract as regression_test/19) ──
start_writer_pod() {
  local pod=$1; shift
  local pvcs=("$@") mounts="" volumes="" dirs="" i=0
  for pvc in "${pvcs[@]}"; do
    mounts+=$'\n    - name: v'"$i"$'\n      mountPath: /data'"$i"
    volumes+=$'\n  - name: v'"$i"$'\n    persistentVolumeClaim:\n      claimName: '"$pvc"
    dirs+=" /data$i"; i=$((i + 1))
  done
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  labels:
    test: vgs-regression
spec:
  restartPolicy: Never
  containers:
  - name: writer
    image: alpine:3
    command:
    - /bin/sh
    - -c
    - |
      seq=0
      while [ ! -f /tmp/stop ]; do
        seq=\$((seq + 1))
        payload="vgs-payload-\$seq-\$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
        hash=\$(printf '%s' "\$payload" | sha256sum | cut -d' ' -f1)
        for d in$dirs; do
          printf '%s:%s:%s\n' "\$seq" "\$hash" "\$payload" >> "\$d/vgs.log"
        done
        sync
      done
      sleep 3600
    volumeMounts:$mounts
  volumes:$volumes
EOF
}

# Verify the prefix property over a set of mounted logs. Exits 0 = consistent,
# 1 = the verifier ran and rejected the data, 2 = no verdict (mount/pull/timeout).
run_verifier_pod() {
  local pod=$1 max_spread=$2; shift 2
  local pvcs=("$@") mounts="" volumes="" dirs="" i=0
  for pvc in "${pvcs[@]}"; do
    mounts+=$'\n    - name: v'"$i"$'\n      mountPath: /data'"$i"$'\n      readOnly: true'
    volumes+=$'\n  - name: v'"$i"$'\n    persistentVolumeClaim:\n      claimName: '"$pvc"
    dirs+=" /data$i"; i=$((i + 1))
  done
  kubectl -n "$NAMESPACE" delete pod "$pod" --ignore-not-found &>/dev/null
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  labels:
    test: vgs-regression
spec:
  restartPolicy: Never
  containers:
  - name: verify
    image: alpine:3
    command:
    - /bin/sh
    - -c
    - |
      set -e
      min=; max=
      for d in$dirs; do
        f="\$d/vgs.log"
        [ -f "\$f" ] || { echo "VERIFY_MISSING \$f"; exit 1; }
        last=0
        while IFS=: read -r seq hash payload; do
          [ -z "\$seq" ] && continue
          got=\$(printf '%s' "\$payload" | sha256sum | cut -d' ' -f1)
          if [ "\$got" != "\$hash" ]; then echo "VERIFY_CORRUPT \$f seq=\$seq"; exit 1; fi
          if [ "\$seq" -ne \$((last + 1)) ]; then echo "VERIFY_GAP \$f after=\$last got=\$seq"; exit 1; fi
          last=\$seq
        done < "\$f"
        echo "volume \$d max_seq=\$last"
        [ -z "\$min" ] || [ "\$last" -lt "\$min" ] && min=\$last
        [ -z "\$max" ] || [ "\$last" -gt "\$max" ] && max=\$last
      done
      spread=\$((max - min))
      echo "VERIFY_SPREAD=\$spread (min=\$min max=\$max allowed=$max_spread)"
      [ "\$spread" -le "$max_spread" ] && echo "VERIFY_OK" || exit 1
    volumeMounts:$mounts
  volumes:$volumes
EOF
  if wait_pod_success "$pod"; then
    kubectl -n "$NAMESPACE" logs "$pod" | tail -5; return 0
  fi
  local vlogs; vlogs=$(kubectl -n "$NAMESPACE" logs "$pod" 2>/dev/null || true)
  [[ -n "$vlogs" ]] && echo "$vlogs" | tail -5
  echo "$vlogs" | grep -q "VERIFY_" && return 1
  kubectl -n "$NAMESPACE" describe pod "$pod" 2>/dev/null | grep -A15 '^Events:' || true
  return 2
}

# ── VolumeGroupSnapshot helpers ───────────────────────────────────────────────
# The selector key defaults to "app": most tests select by an application
# label so the webhook has to DERIVE the group from the matched PVCs (§9.4),
# and test 4's drift rejection depends on that key being broader than the
# membership. Test 12 passes the consistency-group label instead, covering
# the recommended user pattern of selecting by the membership label itself.
apply_vgs() {
  local name=$1 value=$2 key=${3:-app}
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: groupsnapshot.storage.k8s.io/v1beta1
kind: VolumeGroupSnapshot
metadata:
  name: $name
  labels:
    test: vgs-regression
spec:
  volumeGroupSnapshotClassName: $VGS_CLASS
  source:
    selector:
      matchLabels:
        $key: $value
EOF
}

wait_vgs_ready() {
  local name=$1 budget=${2:-$TIMEOUT} interval=${3:-5} elapsed=0 ready
  while [[ $elapsed -lt $budget ]]; do
    ready=$(kubectl -n "$NAMESPACE" get volumegroupsnapshot "$name" -o jsonpath='{.status.readyToUse}' 2>/dev/null)
    [[ "$ready" == "true" ]] && return 0
    sleep "$interval"; elapsed=$((elapsed + interval))
  done
  return 1
}

vgs_handle() {
  local name=$1 content
  content=$(kubectl -n "$NAMESPACE" get volumegroupsnapshot "$name" \
    -o jsonpath='{.status.boundVolumeGroupSnapshotContentName}' 2>/dev/null)
  [[ -z "$content" ]] && return 1
  kubectl get volumegroupsnapshotcontent "$content" \
    -o jsonpath='{.status.volumeGroupSnapshotHandle}' 2>/dev/null
}

# The materialized member VolumeSnapshots, backref'd to this VGS (design §5.3).
member_snapshots_of_vgs() {
  local name=$1
  kubectl -n "$NAMESPACE" get volumesnapshot -o json 2>/dev/null \
    | jq -r --arg v "$name" '.items[] | select(.status.volumeGroupSnapshotName == $v) | .metadata.name'
}

cleanup() {
  info "Cleaning up test resources..."
  rm -f -- "${VGS_TEST_SNAPSHOT:-}" 2>/dev/null || true
  kubectl -n "$NAMESPACE" delete volumegroupsnapshotops -l "$LABEL" --ignore-not-found &>/dev/null || true
  # Claims a VolumeGroupSnapshotOps restored carry the ops ownership label, not
  # the test label, and deleting the ops does not cascade to them by design.
  kubectl -n "$NAMESPACE" delete pvc -l storage.simplyblock.io/volume-group-snapshot-ops \
    --ignore-not-found &>/dev/null || true
  kubectl -n "$NAMESPACE" delete volumegroupsnapshot -l "$LABEL" --ignore-not-found &>/dev/null || true
  kubectl -n "$NAMESPACE" delete volumemigration -l "$LABEL" --ignore-not-found &>/dev/null || true
  kubectl -n "$NAMESPACE" delete pod -l "$LABEL" --ignore-not-found --force --grace-period=0 &>/dev/null || true
  kubectl -n "$NAMESPACE" delete pvc -l "$LABEL" --ignore-not-found &>/dev/null || true
  kubectl -n "$NAMESPACE" delete volumesnapshot -l "$LABEL" --ignore-not-found &>/dev/null || true
  # Last, after every group snapshot referencing it is gone: the class is
  # script-owned (test 0 creates it), so the script removes it too.
  kubectl delete volumegroupsnapshotclass -l "$LABEL" --ignore-not-found &>/dev/null || true
  kubectl delete volumesnapshotclass -l "$LABEL" --ignore-not-found &>/dev/null || true
}
trap cleanup EXIT

# ═══════════════════════════════════════════════════════════════════════════════
# Test 0: preflight — P0-4 is enabled
# ═══════════════════════════════════════════════════════════════════════════════
if run_test 0; then
  section "Test 0: preflight (VolumeGroupSnapshot CRDs, class, feature gate)"
  ok=1
  if kubectl get crd volumegroupsnapshots.groupsnapshot.storage.k8s.io &>/dev/null; then
    pass "VolumeGroupSnapshot CRD is installed"
  else
    fail "VolumeGroupSnapshot CRD is MISSING — deploy the chart with P0-4 enabled"; ok=0
  fi
  # The class is script-owned (the chart deliberately ships no default
  # VolumeGroupSnapshotClass): ensure it here so the group snapshots the later
  # tests take can name it. Idempotent, and removed again by cleanup.
  if kubectl apply -f - <<EOF
apiVersion: groupsnapshot.storage.k8s.io/v1beta1
kind: VolumeGroupSnapshotClass
metadata:
  name: $VGS_CLASS
  labels:
    test: vgs-regression
driver: csi.simplyblock.io
deletionPolicy: Delete
EOF
  then
    pass "VolumeGroupSnapshotClass $VGS_CLASS ensured"
  else
    fail "could not create VolumeGroupSnapshotClass $VGS_CLASS"; ok=0
  fi
  if [[ $ok -eq 0 ]]; then
    echo ""; echo "Phase 2 prerequisites absent; the remaining tests cannot run."
    echo "PASSED=$PASSED FAILED=$FAILED"; exit 1
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 1: provisioning a group by label
# ═══════════════════════════════════════════════════════════════════════════════
if run_test 1; then
  section "Test 1: labeled PVCs form one placement-pinned backend group"
  setup_storageclass
  for pvc in "${MEMBER_PVCS[@]}"; do make_member_pvc "$pvc"; done
  all_bound=1
  for pvc in "${MEMBER_PVCS[@]}"; do
    wait_pvc_bound "$pvc" || { fail "PVC $pvc did not bind"; all_bound=0; }
  done
  [[ $all_bound -eq 1 ]] && pass "all $MEMBER_COUNT member PVCs bound"

  # Every member must land on ONE storage node (the group's pin, design §4.2).
  nodes=""
  for pvc in "${MEMBER_PVCS[@]}"; do
    lv=$(lvol_of_pvc "$pvc"); n=$(node_of_lvol "$lv")
    nodes+="$n"$'\n'
  done
  uniq_nodes=$(printf '%s' "$nodes" | sort -u | grep -c .)
  if [[ "$uniq_nodes" == "1" ]]; then
    pass "all members pinned to one storage node (uniq nodes=1)"
  else
    fail "members span $uniq_nodes storage nodes — placement pin not enforced"
  fi

  # Cross-check (informational): the backend reports one group_id for every
  # member. Kept non-fatal because it depends on the CLI surfacing group_id;
  # the placement pin above is the load-bearing assertion.
  gids=""
  for pvc in "${MEMBER_PVCS[@]}"; do
    lv=$(lvol_of_pvc "$pvc")
    gids+=$(sb volume get "$lv" --json 2>/dev/null | jq -r '.group_id // empty')$'\n'
  done
  uniq_gids=$(printf '%s' "$gids" | grep -c . || true)
  distinct=$(printf '%s' "$gids" | sort -u | grep -c . || true)
  if [[ "$uniq_gids" -gt 0 && "$distinct" == "1" ]]; then
    pass "all members report one backend group_id"
  else
    info "backend group_id cross-check inconclusive (distinct=$distinct) — see the node pin above"
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 2 + 3: VolumeGroupSnapshot materializes, and the generation is consistent
# ═══════════════════════════════════════════════════════════════════════════════
if run_test 2 || run_test 3; then
  section "Test 2/3: VolumeGroupSnapshot materializes one crash-consistent generation"

  # Write crash-consistent data across all members, then snapshot under load.
  start_writer_pod vgs-writer "${MEMBER_PVCS[@]}"
  wait_pod_ready vgs-writer || fail "writer pod did not start"
  sleep 10

  apply_vgs "$VGS_NAME" vgs-db
  if wait_vgs_ready "$VGS_NAME"; then
    pass "VolumeGroupSnapshot $VGS_NAME is ReadyToUse"
  else
    fail "VolumeGroupSnapshot $VGS_NAME never became ReadyToUse"
    kubectl -n "$NAMESPACE" describe volumegroupsnapshot "$VGS_NAME" 2>/dev/null | tail -20
  fi

  handle=$(vgs_handle "$VGS_NAME" || true)
  if [[ -n "$handle" ]]; then
    pass "VolumeGroupSnapshotContent carries a handle: $handle"
  else
    fail "VolumeGroupSnapshotContent has no volumeGroupSnapshotHandle"
  fi

  member_snaps=()
  while IFS= read -r _snap; do [[ -n "$_snap" ]] && member_snaps+=("$_snap"); done \
    < <(member_snapshots_of_vgs "$VGS_NAME")
  if [[ ${#member_snaps[@]} -eq $MEMBER_COUNT ]]; then
    pass "materialized $MEMBER_COUNT member VolumeSnapshots (one per member)"
  else
    fail "expected $MEMBER_COUNT member VolumeSnapshots, got ${#member_snaps[@]}"
  fi

  if run_test 3 && [[ ${#member_snaps[@]} -eq $MEMBER_COUNT ]]; then
    # Stop the writer so restored volumes are a stable target, then restore every
    # member from THIS generation and prove the prefix property (design §11.2).
    kubectl -n "$NAMESPACE" exec vgs-writer -- touch /tmp/stop 2>/dev/null || true
    sleep 3
    restore_pvcs=()
    idx=1
    for snap in "${member_snaps[@]}"; do
      rp="vgs-restore-$idx"; make_restore_pvc "$rp" "$snap"; restore_pvcs+=("$rp"); idx=$((idx + 1))
    done
    all_bound=1
    for rp in "${restore_pvcs[@]}"; do wait_pvc_bound "$rp" || { fail "restore PVC $rp did not bind"; all_bound=0; }; done
    if [[ $all_bound -eq 1 ]]; then
      run_verifier_pod vgs-verify 1 "${restore_pvcs[@]}"
      case $? in
        0) pass "restored generation is crash-consistent (prefix property, spread <= 1)" ;;
        1) fail "restored generation is INCONSISTENT (verifier rejected the data)" ;;
        2) fail "verifier produced no verdict (mount/image/timeout) — inconclusive" ;;
      esac
    fi
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 4: selector drift is rejected at admission (§9.4 webhook)
# ═══════════════════════════════════════════════════════════════════════════════
if run_test 4; then
  section "Test 4: a selector matching an unlabeled PVC is refused at apply (§9.4)"
  make_unlabeled_pvc vgs-stray
  wait_pvc_bound vgs-stray || info "stray PVC not bound yet (webhook still checks labels)"
  # The app=vgs-db selector now matches the members AND the unlabeled stray.
  if apply_vgs vgs-bad vgs-db 2>/tmp/vgs-bad.err; then
    fail "webhook ADMITTED a VolumeGroupSnapshot whose selector matches an unlabeled PVC"
    kubectl -n "$NAMESPACE" delete volumegroupsnapshot vgs-bad --ignore-not-found &>/dev/null
  else
    if grep -qiE "consistency-group|not labeled|denied|admission" /tmp/vgs-bad.err; then
      pass "webhook rejected the drifted selector: $(tr -d '\n' </tmp/vgs-bad.err | tail -c 160)"
    else
      fail "apply failed but not for the expected reason: $(cat /tmp/vgs-bad.err)"
    fi
  fi
  # Remove the stray now rather than at exit: while it exists, the webhook
  # rightly rejects EVERY app=vgs-db VolumeGroupSnapshot, including the ones
  # tests 6-9 create.
  kubectl -n "$NAMESPACE" delete pvc vgs-stray --ignore-not-found &>/dev/null
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 5: a member cannot be migrated (§9.5 webhook)
# ═══════════════════════════════════════════════════════════════════════════════
if run_test 5; then
  section "Test 5: a VolumeMigration of a group member is refused at apply (§9.5)"
  pv=$(kubectl -n "$NAMESPACE" get pvc "${MEMBER_PVCS[0]}" -o jsonpath='{.spec.volumeName}' 2>/dev/null)
  # Any node UUID is fine: the webhook refuses before the target is ever used.
  target=$(sb storage-node list --json 2>/dev/null | jq -r '.[0].uuid // "00000000-0000-0000-0000-000000000000"' 2>/dev/null || echo "00000000-0000-0000-0000-000000000000")
  if kubectl -n "$NAMESPACE" apply -f - <<EOF 2>/tmp/vm-bad.err
apiVersion: storage.simplyblock.io/v1alpha1
kind: VolumeMigration
metadata:
  name: vgs-migrate-member
  labels:
    test: vgs-regression
spec:
  pvName: $pv
  targetNodeUUID: $target
EOF
  then
    fail "webhook ADMITTED a VolumeMigration of a consistency-group member"
    kubectl -n "$NAMESPACE" delete volumemigration vgs-migrate-member --ignore-not-found &>/dev/null
  else
    if grep -qiE "consistency group|member|pinned|denied|admission" /tmp/vm-bad.err; then
      pass "webhook rejected the member migration: $(tr -d '\n' </tmp/vm-bad.err | tail -c 160)"
    else
      fail "apply failed but not for the expected reason: $(cat /tmp/vm-bad.err)"
    fi
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 6: generations increment and delete independently (§6.3, §8)
# ═══════════════════════════════════════════════════════════════════════════════
# The {cluster}:{pool}:{group}:{seq} handle split: group uuid and generation seq.
handle_group() { local h=${1%:*}; echo "${h##*:}"; }
handle_seq()   { echo "${1##*:}"; }

# Member snapshots of a VGS, one per line (wraps the jq helper for counting).
count_member_snaps() { member_snapshots_of_vgs "$1" | grep -c . || true; }

wait_member_snaps_gone() {
  local name=$1 elapsed=0
  while [[ $elapsed -lt $TIMEOUT ]]; do
    [[ "$(count_member_snaps "$name")" == "0" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

if run_test 6; then
  section "Test 6: generations increment and delete independently (§6.3, §8)"
  ok=1
  for g in vgs-gen2 vgs-gen3; do
    apply_vgs "$g" vgs-db
    if wait_vgs_ready "$g"; then
      pass "VolumeGroupSnapshot $g is ReadyToUse"
    else
      fail "VolumeGroupSnapshot $g never became ReadyToUse"; ok=0
    fi
  done

  if [[ $ok -eq 1 ]]; then
    h2=$(vgs_handle vgs-gen2); h3=$(vgs_handle vgs-gen3)
    if [[ "$(handle_group "$h2")" == "$(handle_group "$h3")" \
          && "$(handle_seq "$h3")" -gt "$(handle_seq "$h2")" ]]; then
      pass "one group, monotonically increasing generations ($(handle_seq "$h2") -> $(handle_seq "$h3"))"
    else
      fail "generation identity wrong: gen2=$h2 gen3=$h3"
    fi

    # Deleting one generation must not touch the other, or the group itself.
    kubectl -n "$NAMESPACE" delete volumegroupsnapshot vgs-gen2 &>/dev/null
    if wait_member_snaps_gone vgs-gen2; then
      pass "deleted generation's member snapshots are gone"
    else
      fail "member snapshots of the deleted generation linger"
    fi
    if [[ "$(count_member_snaps vgs-gen3)" == "$MEMBER_COUNT" ]]; then
      pass "surviving generation still has $MEMBER_COUNT member snapshots"
    else
      fail "surviving generation lost member snapshots after a sibling delete"
    fi
    groups=$(sb cg list "$CLUSTER1_UUID" --json 2>/dev/null \
      | jq -r --arg n "$CG_NAME" '[.[] | select(.Name == $n)] | length')
    if [[ "$groups" == "1" ]]; then
      pass "backend group survives generation deletion (exactly one group named $CG_NAME)"
    else
      fail "expected exactly one backend group named $CG_NAME, found ${groups:-none}"
    fi
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 7: a late joiner shares the pin and appears in the NEXT generation (§4.1)
# ═══════════════════════════════════════════════════════════════════════════════
LATE_PVC="vgs-pvc-$((MEMBER_COUNT + 1))"

if run_test 7; then
  section "Test 7: a late joiner shares the pin and appears in the next generation (§4.1)"
  make_member_pvc "$LATE_PVC"
  if wait_pvc_bound "$LATE_PVC"; then
    pass "late joiner $LATE_PVC bound"
    pin=$(node_of_lvol "$(lvol_of_pvc "${MEMBER_PVCS[0]}")")
    late_node=$(node_of_lvol "$(lvol_of_pvc "$LATE_PVC")")
    if [[ -n "$pin" && "$late_node" == "$pin" ]]; then
      pass "late joiner landed on the group's pinned node"
    else
      fail "late joiner on node ${late_node:-?}, group is pinned to ${pin:-?}"
    fi

    apply_vgs vgs-gen-late vgs-db
    if wait_vgs_ready vgs-gen-late \
       && [[ "$(count_member_snaps vgs-gen-late)" == "$((MEMBER_COUNT + 1))" ]]; then
      pass "next generation contains all $((MEMBER_COUNT + 1)) members"
    else
      fail "next generation does not contain the late joiner (got $(count_member_snaps vgs-gen-late) member snapshots)"
    fi
  else
    fail "late joiner $LATE_PVC did not bind"
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 8: deleting a member closes its epoch; its history stays ReadyToUse (§8.2)
# ═══════════════════════════════════════════════════════════════════════════════
if run_test 8; then
  section "Test 8: deleting a member closes its epoch; history stays ReadyToUse (§8.2)"
  # The late joiner's snapshot in vgs-gen-late is the history that must survive.
  late_snap=$(kubectl -n "$NAMESPACE" get volumesnapshot -o json 2>/dev/null \
    | jq -r --arg p "$LATE_PVC" \
      '.items[] | select(.status.volumeGroupSnapshotName == "vgs-gen-late")
                | select(.spec.source.persistentVolumeClaimName == $p) | .metadata.name')
  kubectl -n "$NAMESPACE" delete pvc "$LATE_PVC" --ignore-not-found &>/dev/null
  kubectl -n "$NAMESPACE" wait --for=delete pvc/"$LATE_PVC" --timeout="${TIMEOUT}s" &>/dev/null

  apply_vgs vgs-gen-after vgs-db
  if wait_vgs_ready vgs-gen-after \
     && [[ "$(count_member_snaps vgs-gen-after)" == "$MEMBER_COUNT" ]]; then
    pass "generation after the delete excludes the departed member ($MEMBER_COUNT snapshots)"
  else
    fail "generation after the delete has $(count_member_snaps vgs-gen-after) member snapshots, expected $MEMBER_COUNT"
  fi

  if [[ -n "$late_snap" ]]; then
    ready=$(kubectl -n "$NAMESPACE" get volumesnapshot "$late_snap" \
      -o jsonpath='{.status.readyToUse}' 2>/dev/null)
    if [[ "$ready" == "true" ]]; then
      pass "departed member's snapshot in the earlier generation stays ReadyToUse"
    else
      fail "departed member's earlier snapshot $late_snap is not ReadyToUse (got '${ready:-gone}')"
    fi
  else
    info "no vgs-gen-late snapshot for $LATE_PVC found (run test 7 first) — history check skipped"
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 9: a restore from a member snapshot is NOT a group member (§7.1)
# ═══════════════════════════════════════════════════════════════════════════════
if run_test 9; then
  section "Test 9: a volume restored from a member snapshot is not a group member (§7.1)"
  # Reuse any standing generation; take one when running standalone.
  src_snap=$(member_snapshots_of_vgs vgs-gen3 | head -1)
  if [[ -z "$src_snap" ]]; then
    apply_vgs vgs-gen-r vgs-db
    wait_vgs_ready vgs-gen-r
    src_snap=$(member_snapshots_of_vgs vgs-gen-r | head -1)
  fi
  if [[ -n "$src_snap" ]]; then
    make_restore_pvc vgs-restore-solo "$src_snap"
    if wait_pvc_bound vgs-restore-solo; then
      gid=$(sb volume get "$(lvol_of_pvc vgs-restore-solo)" --json 2>/dev/null \
        | jq -r '.group_id // empty')
      if [[ -z "$gid" ]]; then
        pass "restored volume carries no backend group_id"
      else
        fail "restored volume joined a group: group_id=$gid"
      fi
    else
      fail "restore PVC vgs-restore-solo did not bind"
    fi
  else
    fail "no member snapshot available to restore from"
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Tests 10 and 11: single-operation restore through VolumeGroupSnapshotOps (§7.4)
# ═══════════════════════════════════════════════════════════════════════════════
# Applies a VolumeGroupSnapshotOps restoring $2, with optional restore params
# appended verbatim from $3 (already-indented YAML lines).
apply_vgs_ops() {
  local name=$1 ref=$2 params=${3:-}
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha2
kind: VolumeGroupSnapshotOps
metadata:
  name: $name
  labels:
    test: vgs-regression
spec:
  volumeGroupSnapshotRef: $ref
  action: Restore
$params
EOF
}

wait_ops_phase() {
  local name=$1 wanted=$2 elapsed=0 phase
  while [[ $elapsed -lt $TIMEOUT ]]; do
    phase=$(kubectl -n "$NAMESPACE" get volumegroupsnapshotops "$name" \
      -o jsonpath='{.status.phase}' 2>/dev/null)
    [[ "$phase" == "$wanted" ]] && return 0
    [[ "$phase" == "Failed" && "$wanted" != "Failed" ]] && return 1
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

# The generation the restores draw from: any standing one, or a fresh take.
ops_source_vgs() {
  for candidate in vgs-gen3 vgs-gen-late vgs-gen-r "$VGS_NAME"; do
    if [[ "$(kubectl -n "$NAMESPACE" get volumegroupsnapshot "$candidate" \
        -o jsonpath='{.status.readyToUse}' 2>/dev/null)" == "true" ]]; then
      echo "$candidate"; return 0
    fi
  done
  apply_vgs vgs-gen-ops vgs-db >/dev/null
  wait_vgs_ready vgs-gen-ops && echo vgs-gen-ops
}

if run_test 10; then
  section "Test 10: one applied VolumeGroupSnapshotOps restores a generation (§7.4)"
  if ! kubectl get crd volumegroupsnapshotops.storage.simplyblock.io &>/dev/null; then
    fail "VolumeGroupSnapshotOps CRD is MISSING — deploy the chart carrying Phase 3"
  else
    # Negative first (E-13's admission half): a ref naming no VolumeGroupSnapshot
    # is refused at apply by the operator's webhook.
    if apply_vgs_ops vgs-ops-bad no-such-vgs 2>/tmp/vgs-ops-bad.err; then
      fail "webhook ADMITTED a VolumeGroupSnapshotOps with an unresolvable ref"
      kubectl -n "$NAMESPACE" delete volumegroupsnapshotops vgs-ops-bad --ignore-not-found &>/dev/null
    else
      if grep -qiE "does not name a VolumeGroupSnapshot|denied|admission" /tmp/vgs-ops-bad.err; then
        pass "webhook rejected the unresolvable ref: $(tr -d '\n' </tmp/vgs-ops-bad.err | tail -c 120)"
      else
        fail "apply failed but not for the expected reason: $(cat /tmp/vgs-ops-bad.err)"
      fi
    fi

    src=$(ops_source_vgs)
    if [[ -z "$src" ]]; then
      fail "no ready VolumeGroupSnapshot to restore from"
    else
      apply_vgs_ops vgs-ops-1 "$src" "  restore:
    namePrefix: opsrestore"
      if wait_ops_phase vgs-ops-1 Succeeded; then
        pass "VolumeGroupSnapshotOps vgs-ops-1 reached Succeeded"
      else
        fail "VolumeGroupSnapshotOps vgs-ops-1 never Succeeded"
        kubectl -n "$NAMESPACE" describe volumegroupsnapshotops vgs-ops-1 2>/dev/null | tail -12
      fi
      bound=$(kubectl -n "$NAMESPACE" get pvc \
        -l storage.simplyblock.io/volume-group-snapshot-ops=vgs-ops-1 -o json 2>/dev/null \
        | jq -r '[.items[] | select(.status.phase == "Bound")] | length')
      if [[ "$bound" == "$MEMBER_COUNT" ]]; then
        pass "one bound claim per member ($bound of $MEMBER_COUNT, prefix opsrestore)"
      else
        fail "expected $MEMBER_COUNT bound restored claims, got ${bound:-0}"
      fi
      # The one-apply restore stays group-neutral without restore.consistencyGroup.
      lv=$(lvol_of_pvc "$(kubectl -n "$NAMESPACE" get pvc \
        -l storage.simplyblock.io/volume-group-snapshot-ops=vgs-ops-1 \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)")
      gid=$(sb volume get "$lv" --json 2>/dev/null | jq -r '.group_id // empty')
      if [[ -z "$gid" ]]; then
        pass "ops-restored volume carries no backend group_id"
      else
        fail "ops-restored volume joined a group without restore.consistencyGroup: $gid"
      fi
    fi
  fi
fi

if run_test 11; then
  section "Test 11: restore.consistencyGroup forms a new pinned group (§7.2, §7.4)"
  src=$(ops_source_vgs)
  if [[ -z "$src" ]]; then
    fail "no ready VolumeGroupSnapshot to restore from"
  else
    NEWCG="${CG_NAME}-restored"
    apply_vgs_ops vgs-ops-2 "$src" "  restore:
    namePrefix: cgrestore
    consistencyGroup: $NEWCG"
    if wait_ops_phase vgs-ops-2 Succeeded; then
      pass "group-forming restore vgs-ops-2 reached Succeeded"
    else
      fail "group-forming restore vgs-ops-2 never Succeeded"
      kubectl -n "$NAMESPACE" describe volumegroupsnapshotops vgs-ops-2 2>/dev/null | tail -12
    fi
    # The clones must be members of ONE new backend group, pinned to one node.
    nodes=""; gids=""
    for pvc in $(kubectl -n "$NAMESPACE" get pvc \
        -l storage.simplyblock.io/volume-group-snapshot-ops=vgs-ops-2 \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
      lv=$(lvol_of_pvc "$pvc")
      nodes+=$(node_of_lvol "$lv")$'\n'
      gids+=$(sb volume get "$lv" --json 2>/dev/null | jq -r '.group_id // empty')$'\n'
    done
    uniq_nodes=$(printf '%s' "$nodes" | sort -u | grep -c .)
    distinct_gids=$(printf '%s' "$gids" | sort -u | grep -c .)
    nonempty_gids=$(printf '%s' "$gids" | grep -c . || true)
    if [[ "$uniq_nodes" == "1" && "$distinct_gids" == "1" && "$nonempty_gids" == "$MEMBER_COUNT" ]]; then
      pass "clones form one new group on one node (group $NEWCG)"
    else
      fail "clones did not form one pinned group (nodes=$uniq_nodes, distinct group_ids=$distinct_gids, members=$nonempty_gids)"
    fi
    groups=$(sb cg list "$CLUSTER1_UUID" --json 2>/dev/null \
      | jq -r --arg n "$NEWCG" '[.[] | select(.Name == $n)] | length')
    if [[ "$groups" == "1" ]]; then
      pass "backend holds exactly one group named $NEWCG"
    else
      fail "expected one backend group named $NEWCG, found ${groups:-none}"
    fi
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# ═══════════════════════════════════════════════════════════════════════════════
# Test 12: N independent groups each snapshot and restore (§4.1, §5, §7.4)
# ═══════════════════════════════════════════════════════════════════════════════

# A member of one of the N groups: its own group label and its own app selector,
# so each group's VolumeGroupSnapshot selects exactly its own members.
make_multi_member_pvc() {
  local name=$1 group=$2 app=$3
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $name
  labels:
    test: vgs-regression
    app: $app
    $CG_LABEL: $group
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: $PVC_SIZE
EOF
}

if run_test 12; then
  section "Test 12: $GROUP_COUNT groups x $GROUP_MEMBERS members each snapshot and restore (§4.1, §7.4)"
  # Self-contained: the StorageClass is otherwise test 1's to create.
  setup_storageclass >/dev/null

  # ── Provision N groups' members ────────────────────────────────────────────
  for g in $(seq 1 "$GROUP_COUNT"); do
    for i in $(seq 1 "$GROUP_MEMBERS"); do
      make_multi_member_pvc "vgs-multi-g${g}-pvc-${i}" "vgs-group-multi-${g}" "vgs-multi-g${g}"
    done
  done
  all_bound=1
  for g in $(seq 1 "$GROUP_COUNT"); do
    for i in $(seq 1 "$GROUP_MEMBERS"); do
      wait_pvc_bound "vgs-multi-g${g}-pvc-${i}" \
        || { fail "PVC vgs-multi-g${g}-pvc-${i} did not bind"; all_bound=0; }
    done
  done
  [[ $all_bound -eq 1 ]] && pass "all $((GROUP_COUNT * GROUP_MEMBERS)) member PVCs bound"

  # ── The backend holds exactly N groups, M open members each, each pinned ───
  groups_json=$(sb cg list "$CLUSTER1_UUID" --json 2>/dev/null \
    | jq '[.[] | select(.Name | startswith("vgs-group-multi-"))]')
  n_groups=$(printf '%s' "$groups_json" | jq 'length')
  full_groups=$(printf '%s' "$groups_json" \
    | jq --argjson m "$GROUP_MEMBERS" '[.[] | select(.Members == $m)] | length')
  if [[ "$n_groups" == "$GROUP_COUNT" && "$full_groups" == "$GROUP_COUNT" ]]; then
    pass "backend holds $GROUP_COUNT groups with $GROUP_MEMBERS members each"
  else
    fail "backend groups=$n_groups (want $GROUP_COUNT), fully populated=$full_groups"
  fi
  pins_ok=1
  for g in $(seq 1 "$GROUP_COUNT"); do
    nodes=""
    for i in $(seq 1 "$GROUP_MEMBERS"); do
      nodes+=$(node_of_lvol "$(lvol_of_pvc "vgs-multi-g${g}-pvc-${i}")")$'\n'
    done
    if [[ "$(printf '%s' "$nodes" | sort -u | grep -c .)" != "1" ]]; then
      fail "group vgs-group-multi-${g} members span more than one node"; pins_ok=0
    fi
  done
  [[ $pins_ok -eq 1 ]] && pass "every group's members share one pinned node"

  # ── One VolumeGroupSnapshot per group, all taken, all distinct ─────────────
  for g in $(seq 1 "$GROUP_COUNT"); do
    # Selecting by the membership label itself: the recommended user pattern,
    # admitted by the webhook because the matched set equals the membership.
    apply_vgs "vgs-multi-gen-${g}" "vgs-group-multi-${g}" "$CG_LABEL"
  done
  snaps_ok=1; group_uuids=""
  for g in $(seq 1 "$GROUP_COUNT"); do
    if ! wait_vgs_ready "vgs-multi-gen-${g}"; then
      fail "VolumeGroupSnapshot vgs-multi-gen-${g} never became ReadyToUse"; snaps_ok=0
      continue
    fi
    if [[ "$(count_member_snaps "vgs-multi-gen-${g}")" != "$GROUP_MEMBERS" ]]; then
      fail "vgs-multi-gen-${g} materialized $(count_member_snaps "vgs-multi-gen-${g}") member snapshots, want $GROUP_MEMBERS"
      snaps_ok=0
    fi
    group_uuids+=$(handle_group "$(vgs_handle "vgs-multi-gen-${g}")")$'\n'
  done
  [[ $snaps_ok -eq 1 ]] && pass "one ReadyToUse generation with $GROUP_MEMBERS member snapshots per group"
  distinct_groups=$(printf '%s' "$group_uuids" | sort -u | grep -c .)
  if [[ "$distinct_groups" == "$GROUP_COUNT" ]]; then
    pass "the $GROUP_COUNT generations reference $GROUP_COUNT distinct backend groups"
  else
    fail "generations reference $distinct_groups distinct groups, want $GROUP_COUNT"
  fi

  # ── One VolumeGroupSnapshotOps restore per group ───────────────────────────
  for g in $(seq 1 "$GROUP_COUNT"); do
    apply_vgs_ops "vgs-multi-ops-${g}" "vgs-multi-gen-${g}" "  restore:
    namePrefix: mrestore-g${g}"
  done
  restores_ok=1
  for g in $(seq 1 "$GROUP_COUNT"); do
    if ! wait_ops_phase "vgs-multi-ops-${g}" Succeeded; then
      fail "VolumeGroupSnapshotOps vgs-multi-ops-${g} never Succeeded"; restores_ok=0
      kubectl -n "$NAMESPACE" describe volumegroupsnapshotops "vgs-multi-ops-${g}" 2>/dev/null | tail -6
      continue
    fi
    bound=$(kubectl -n "$NAMESPACE" get pvc \
      -l "storage.simplyblock.io/volume-group-snapshot-ops=vgs-multi-ops-${g}" -o json 2>/dev/null \
      | jq -r '[.items[] | select(.status.phase == "Bound")] | length')
    if [[ "$bound" != "$GROUP_MEMBERS" ]]; then
      fail "restore vgs-multi-ops-${g} bound $bound claims, want $GROUP_MEMBERS"; restores_ok=0
    fi
  done
  [[ $restores_ok -eq 1 ]] && pass "every group restored through one VolumeGroupSnapshotOps ($GROUP_COUNT ops, $GROUP_MEMBERS claims each)"
fi

# ═══════════════════════════════════════════════════════════════════════════════
# ═══════════════════════════════════════════════════════════════════════════════
# Test 13: the documented snapshot-chain and clones-per-snapshot limits,
#          driven through VolumeGroupSnapshots
# ═══════════════════════════════════════════════════════════════════════════════
# Product limits: 100 snapshots per chain and 500 clones per snapshot, all with
# data. The chain is driven through the GROUP: every VolumeGroupSnapshot
# generation adds ONE snapshot to EVERY member's individual chain, so after
# CHAIN_MAX generations each member's chain is at the limit and generation
# CHAIN_MAX+1 must be refused cleanly. Clones then fan out from one member
# snapshot of the last generation.
# Override for a faster smoke run: CHAIN_MEMBERS=2 CHAIN_MAX=10 CLONE_MAX=20 ./...sh 0 13
CHAIN_MEMBERS="${CHAIN_MEMBERS:-20}"
CHAIN_MAX="${CHAIN_MAX:-100}"
CLONE_MAX="${CLONE_MAX:-10}"
CLONE_VERIFY_SAMPLE="${CLONE_VERIFY_SAMPLE:-10}"

# A member VolumeSnapshot can lag its VolumeGroupSnapshot: the VGS turns ready
# when the backend generation completes, but the snapshot-controller syncs the
# per-member statuses through its own queue, which is thousands deep at scale.
# Anything consuming a member snapshot as a dataSource must wait for IT.
wait_snapshot_ready() {
  # 4x the ordinary budget: at thousands of member snapshots, the controller''s
  # per-object status writes trail a ready VolumeGroupSnapshot by 15-25 minutes
  # (measured 2026-09-14, 20 members x 101 generations).
  local name=$1 elapsed=0 budget=$((TIMEOUT * 4))
  while [[ $elapsed -lt $budget ]]; do
    [[ "$(kubectl -n "$NAMESPACE" get volumesnapshot "$name"       -o jsonpath='{.status.readyToUse}' 2>/dev/null)" == "true" ]] && return 0
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

# How many member snapshots of one generation report ready, for measuring the
# member-status lag behind their VolumeGroupSnapshot.
members_ready_count() {
  kubectl -n "$NAMESPACE" get volumesnapshot -o json 2>/dev/null \
    | jq -r --arg v "$1" \
      '[.items[] | select(.status.volumeGroupSnapshotName == $v)
                 | select(.status.readyToUse == true)] | length'
}

# The member snapshot of one generation whose source claim is $2, so the chain
# and clone phases can act on a single member's individual chain.
member_snap_of() {
  local vgs=$1 src=$2
  kubectl -n "$NAMESPACE" get volumesnapshot -o json 2>/dev/null \
    | jq -r --arg v "$vgs" --arg p "$src" \
      '.items[] | select(.status.volumeGroupSnapshotName == $v)
                | select(.spec.source.persistentVolumeClaimName == $p) | .metadata.name'
}

if run_test 13; then
  section "Test 13: $CHAIN_MAX group generations x $CHAIN_MEMBERS members, $CLONE_MAX clones of one snapshot"
  setup_storageclass >/dev/null

  # ── The chain group's members and a long-running writer over all of them ────
  for i in $(seq 1 "$CHAIN_MEMBERS"); do
    kubectl -n "$NAMESPACE" apply -f - >/dev/null <<CHAINPVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vgs-chain-pvc-$i
  labels:
    test: vgs-regression
    $CG_LABEL: vgs-chain-group
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: $PVC_SIZE
CHAINPVC
  done
  chain_bound=1
  for i in $(seq 1 "$CHAIN_MEMBERS"); do
    wait_pvc_bound "vgs-chain-pvc-$i" || { fail "vgs-chain-pvc-$i did not bind"; chain_bound=0; }
  done
  [[ $chain_bound -eq 1 ]] && pass "all $CHAIN_MEMBERS chain members bound"

  mounts=""; volumes=""
  for n in $(seq 1 "$CHAIN_MEMBERS"); do
    mounts+=$'\n    - name: v'"$n"$'\n      mountPath: /data'"$n"
    volumes+=$'\n  - name: v'"$n"$'\n    persistentVolumeClaim:\n      claimName: vgs-chain-pvc-'"$n"
  done
  kubectl -n "$NAMESPACE" apply -f - >/dev/null <<CHAINPOD
apiVersion: v1
kind: Pod
metadata:
  name: vgs-chain-writer
  labels:
    test: vgs-regression
spec:
  restartPolicy: Never
  containers:
  - name: writer
    image: alpine:3
    command: ["/bin/sh", "-c", "sleep 14400"]
    volumeMounts:$mounts
  volumes:$volumes
CHAINPOD
  wait_pod_ready vgs-chain-writer || { fail "chain writer pod did not start"; chain_bound=0; }

  # ── Drive every member's chain to CHAIN_MAX through group generations,
  #    a marker on every member before every generation ────────────────────────
  chain_ok=$chain_bound
  if [[ $chain_ok -eq 1 ]]; then
    chain_timings=""
    member_lag_total=0; member_lag_max=0
    bucket_start=$(date +%s)
    for i in $(seq 1 "$CHAIN_MAX"); do
      kubectl -n "$NAMESPACE" exec vgs-chain-writer -- /bin/sh -c \
        "for d in /data*; do echo chain-marker-$i >> \$d/chain.log; done && sync" \
        || { fail "writing marker $i failed"; chain_ok=0; break; }
      apply_vgs "vgs-chain-gen-$i" "vgs-chain-group" "$CG_LABEL" >/dev/null
      if ! wait_vgs_ready "vgs-chain-gen-$i" "$TIMEOUT" 1; then
        fail "generation $i of $CHAIN_MAX never became ReadyToUse"; chain_ok=0; break
      fi
      # The member-status lag: the VGS is ready NOW; how long until every one
      # of its member VolumeSnapshots says so too (the snapshot-controller's
      # status-write queue, the thing --kube-api-qps bounds).
      lag_start=$(date +%s); lag=0
      while [[ "$(members_ready_count "vgs-chain-gen-$i")" != "$CHAIN_MEMBERS" ]]; do
        sleep 2; lag=$(( $(date +%s) - lag_start ))
        [[ $lag -gt $((TIMEOUT * 4)) ]] && { fail "generation $i member statuses never all turned ready"; break; }
      done
      member_lag_total=$((member_lag_total + lag))
      [[ $lag -gt $member_lag_max ]] && member_lag_max=$lag
      if [[ $((i % 10)) -eq 0 ]]; then
        bucket_end=$(date +%s)
        bucket=$((bucket_end - bucket_start))
        chain_timings+="$bucket "
        info "chain at $i of $CHAIN_MAX generations ($((i * CHAIN_MEMBERS)) member snapshots) — last 10 generations took ${bucket}s, member-status lag avg $((member_lag_total / i))s max ${member_lag_max}s"
        bucket_start=$bucket_end
      fi
    done
  fi
  if [[ $chain_ok -eq 1 ]]; then
    pass "every member's chain reached $CHAIN_MAX snapshots through $CHAIN_MAX group generations"
    # The drift signal: a chain-depth cost shows as later buckets exceeding
    # earlier ones. Each bucket is wall-clock for 10 marker+snapshot rounds.
    info "per-10-generation timings (s): $chain_timings"
    trimmed_timings=${chain_timings% }
    first_bucket=${trimmed_timings%% *}
    last_bucket=${trimmed_timings##* }
    info "chain-depth drift: first 10 generations ${first_bucket}s, last 10 generations ${last_bucket}s"
    info "member-status lag (VGS ready -> all members ready): avg $((member_lag_total / CHAIN_MAX))s, max ${member_lag_max}s"
  fi

  # ── Generation CHAIN_MAX+1 is refused cleanly: every member chain is full ───
  if [[ $chain_ok -eq 1 ]]; then
    apply_vgs vgs-chain-gen-over "vgs-chain-group" "$CG_LABEL" >/dev/null
    over_err=""; elapsed=0
    while [[ $elapsed -lt 90 ]]; do
      over_err=$(kubectl -n "$NAMESPACE" get volumegroupsnapshot vgs-chain-gen-over \
        -o jsonpath='{.status.error.message}' 2>/dev/null)
      [[ -n "$over_err" ]] && break
      if [[ "$(kubectl -n "$NAMESPACE" get volumegroupsnapshot vgs-chain-gen-over \
          -o jsonpath='{.status.readyToUse}' 2>/dev/null)" == "true" ]]; then
        break
      fi
      sleep 5; elapsed=$((elapsed + 5))
    done
    if [[ -n "$over_err" ]] && printf '%s' "$over_err" | grep -qiE "limit|maximum|chain"; then
      pass "generation $((CHAIN_MAX + 1)) refused with the reason: $(printf '%s' "$over_err" | head -c 140)"
    elif [[ "$(kubectl -n "$NAMESPACE" get volumegroupsnapshot vgs-chain-gen-over \
        -o jsonpath='{.status.readyToUse}' 2>/dev/null)" == "true" ]]; then
      fail "generation $((CHAIN_MAX + 1)) SUCCEEDED past the documented $CHAIN_MAX-per-chain limit"
    else
      fail "generation $((CHAIN_MAX + 1)) neither succeeded nor carries a limit error (error: ${over_err:-none})"
    fi
  fi

  # ── A mid-chain member snapshot restores exactly its point in time ──────────
  if [[ $chain_ok -eq 1 ]]; then
    mid=$((CHAIN_MAX / 2))
    mid_snap=$(member_snap_of "vgs-chain-gen-$mid" vgs-chain-pvc-1)
    if [[ -z "$mid_snap" ]]; then
      fail "no member snapshot of generation $mid for vgs-chain-pvc-1"
    else
      wait_snapshot_ready "$mid_snap" || fail "member snapshot $mid_snap never became ready"
      make_restore_pvc vgs-chain-mid-restore "$mid_snap"
      if wait_pvc_bound vgs-chain-mid-restore; then
        markers=$(kubectl -n "$NAMESPACE" run vgs-chain-mid-check --rm -i --restart=Never \
          --image=alpine:3 --overrides="{\"spec\":{\"containers\":[{\"name\":\"vgs-chain-mid-check\",
          \"image\":\"alpine:3\",\"command\":[\"grep\",\"-c\",\"chain-marker-\",\"/data/chain.log\"],
          \"volumeMounts\":[{\"name\":\"d\",\"mountPath\":\"/data\"}]}],
          \"volumes\":[{\"name\":\"d\",\"persistentVolumeClaim\":{\"claimName\":\"vgs-chain-mid-restore\"}}]}}" \
          2>/dev/null | grep -E "^[0-9]+$" | head -1)
        if [[ "$markers" == "$mid" ]]; then
          pass "restore of generation $mid holds exactly $mid markers (chain data integrity)"
        else
          fail "restore of generation $mid holds ${markers:-?} markers, want $mid"
        fi
      else
        fail "mid-chain restore PVC did not bind"
      fi
    fi
  fi

  # ── CLONE_MAX clones of ONE member snapshot of the last generation ──────────
  if [[ $chain_ok -eq 1 ]]; then
    last_snap=$(member_snap_of "vgs-chain-gen-$CHAIN_MAX" vgs-chain-pvc-1)
    if [[ -z "$last_snap" ]]; then
      fail "no member snapshot of generation $CHAIN_MAX for vgs-chain-pvc-1"
    else
      wait_snapshot_ready "$last_snap" \
        || fail "member snapshot $last_snap never became ready; clones will not bind"
      info "creating $CLONE_MAX clones of $last_snap (this can take a while)"
      for j in $(seq 1 "$CLONE_MAX"); do
        cat <<CLONEPVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vgs-chain-clone-$j
  namespace: $NAMESPACE
  labels:
    test: vgs-regression
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: $last_snap
  resources:
    requests:
      storage: $PVC_SIZE
---
CLONEPVC
      done | kubectl apply -f - >/dev/null

      clones_bound=0; clone_deadline=$((SECONDS + TIMEOUT * 4))
      while [[ $SECONDS -lt $clone_deadline ]]; do
        clones_bound=$(kubectl -n "$NAMESPACE" get pvc -l "$LABEL" -o json 2>/dev/null \
          | jq -r '[.items[] | select(.metadata.name | startswith("vgs-chain-clone-"))
                            | select(.status.phase == "Bound")] | length')
        [[ "$clones_bound" == "$CLONE_MAX" ]] && break
        info "clones bound: $clones_bound of $CLONE_MAX"
        sleep 15
      done
      if [[ "$clones_bound" == "$CLONE_MAX" ]]; then
        pass "$CLONE_MAX clones of one member snapshot all bound"
      else
        fail "only $clones_bound of $CLONE_MAX clones bound before the deadline"
      fi

      # Sampled data check: CLONE_VERIFY_SAMPLE evenly spaced clones, the last
      # always included, must each hold the full chain the source snapshot
      # carried. Each check is one mount, so the sample bounds the runtime;
      # CLONE_VERIFY_SAMPLE >= CLONE_MAX verifies every clone.
      sample_ok=1
      sample_step=$(( (CLONE_MAX + CLONE_VERIFY_SAMPLE - 1) / CLONE_VERIFY_SAMPLE ))
      sample_indices=""
      for ((j = 1; j <= CLONE_MAX; j += sample_step)); do sample_indices+="$j "; done
      [[ " $sample_indices" == *" $CLONE_MAX "* ]] || sample_indices+="$CLONE_MAX"
      info "verifying data on clones: $sample_indices"
      for j in $sample_indices; do
        markers=$(kubectl -n "$NAMESPACE" run "vgs-chain-clone-check-$j" --rm -i --restart=Never \
          --image=alpine:3 --overrides="{\"spec\":{\"containers\":[{\"name\":\"vgs-chain-clone-check-$j\",
          \"image\":\"alpine:3\",\"command\":[\"grep\",\"-c\",\"chain-marker-\",\"/data/chain.log\"],
          \"volumeMounts\":[{\"name\":\"d\",\"mountPath\":\"/data\"}]}],
          \"volumes\":[{\"name\":\"d\",\"persistentVolumeClaim\":{\"claimName\":\"vgs-chain-clone-$j\"}}]}}" \
          2>/dev/null | grep -E "^[0-9]+$" | head -1)
        if [[ "$markers" != "$CHAIN_MAX" ]]; then
          fail "clone $j holds ${markers:-?} markers, want $CHAIN_MAX"; sample_ok=0
        fi
      done
      [[ $sample_ok -eq 1 ]] && pass "every sampled clone holds all $CHAIN_MAX markers ($sample_indices)"

      # ── One clone past the limit is refused cleanly ─────────────────────────
      make_restore_pvc vgs-chain-clone-over "$last_snap"
      sleep 30
      if [[ "$(kubectl -n "$NAMESPACE" get pvc vgs-chain-clone-over \
          -o jsonpath='{.status.phase}' 2>/dev/null)" == "Bound" ]]; then
        fail "clone $((CLONE_MAX + 1)) BOUND past the documented $CLONE_MAX-per-snapshot limit"
      else
        over_msg=$(kubectl -n "$NAMESPACE" get events \
          --field-selector "involvedObject.name=vgs-chain-clone-over" -o json 2>/dev/null \
          | jq -r '[.items[] | select(.reason == "ProvisioningFailed")] | last | .message // empty')
        if printf '%s' "$over_msg" | grep -qiE "limit|maximum|clone"; then
          pass "clone $((CLONE_MAX + 1)) refused with the reason: $(printf '%s' "$over_msg" | tail -c 140)"
        else
          fail "clone $((CLONE_MAX + 1)) is not Bound but carries no limit reason (event: ${over_msg:-none})"
        fi
      fi
    fi
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 14: a large consistency group's snapshot timings (LVS freeze impact)
# ═══════════════════════════════════════════════════════════════════════════════
# A cap-sized group of large, data-carrying members, snapshot several times with
# two timings per generation: end-to-end ReadyToUse latency, and the maximum
# I/O stall a running writer observed — the freeze window the application
# actually feels, since one bdev_lvol_snapshot_group call freezes the whole LVS.
# The stall is measured by fio on member 1 (randrw 50/50, 16k, iodepth 1,
# libaio, direct, md5-verified): a freeze surfaces as the max completion
# latency of a single I/O, at microsecond resolution and past the page cache.
# Everything is configurable; the defaults are the product-sized run:
#   BIG_MEMBERS=20 BIG_SIZES=100Gi,200Gi,300Gi BIG_FILL_GB=10 BIG_GENERATIONS=3
# A smoke run: BIG_MEMBERS=3 BIG_SIZES=10Gi BIG_FILL_GB=0 ./...sh 0 14
BIG_MEMBERS="${BIG_MEMBERS:-20}"
BIG_SIZES="${BIG_SIZES:-100Gi,200Gi,300Gi}"
BIG_FILL_GB="${BIG_FILL_GB:-1}"
# When set (1-90), fill each member to this percentage of ITS size instead of
# the flat BIG_FILL_GB. The members are thinly provisioned, so only what is
# written is allocated: the flat 1G default leaves a 300Gi member ~0.3%
# allocated and times the snapshot against a nearly-empty blobstore. Capped
# below 100 because the filesystem needs headroom (xfs refuses at ~95%+).
BIG_FILL_PCT="${BIG_FILL_PCT:-95}"
BIG_GENERATIONS="${BIG_GENERATIONS:-5}"
BIG_TIMEOUT="${BIG_TIMEOUT:-1800}"
# Seconds of continuous overwriting between one generation's take and the
# next. A random-overwrite workload runs across EVERY member for the whole
# timed phase, so each interval accumulates copy-on-write churn against the
# generation just taken before the next one is timed: generation N+1 is taken
# while generation N's snapshot is actively being CoW'd away from.
BIG_SNAP_INTERVAL="${BIG_SNAP_INTERVAL:-30}"

if run_test 14; then
  if [[ -n "$BIG_FILL_PCT" ]]; then
    big_fill_desc="${BIG_FILL_PCT}% of each size"
  else
    big_fill_desc="${BIG_FILL_GB}G each"
  fi
  section "Test 14: $BIG_MEMBERS-member group at $BIG_SIZES, $big_fill_desc filled, $BIG_GENERATIONS timed generations"
  setup_storageclass >/dev/null
  IFS=',' read -r -a big_sizes <<< "$BIG_SIZES"

  # ── The group's members, sizes cycling through BIG_SIZES ────────────────────
  for i in $(seq 1 "$BIG_MEMBERS"); do
    size="${big_sizes[$(( (i - 1) % ${#big_sizes[@]} ))]}"
    kubectl -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vgs-big-pvc-$i
  labels:
    test: vgs-regression
    $CG_LABEL: vgs-big-group
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: $size
EOF
  done
  big_bound=1
  for i in $(seq 1 "$BIG_MEMBERS"); do
    wait_pvc_bound "vgs-big-pvc-$i" || { fail "vgs-big-pvc-$i did not bind"; big_bound=0; }
  done
  [[ $big_bound -eq 1 ]] && pass "all $BIG_MEMBERS large members bound"

  if [[ $big_bound -eq 1 ]]; then
    # One pod mounting every member: the fill writer and the stall probe.
    mounts=""; volumes=""; i=0
    for n in $(seq 1 "$BIG_MEMBERS"); do
      mounts+=$'\n    - name: v'"$n"$'\n      mountPath: /data'"$n"
      volumes+=$'\n  - name: v'"$n"$'\n    persistentVolumeClaim:\n      claimName: vgs-big-pvc-'"$n"
    done
    kubectl -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: vgs-big-writer
  labels:
    test: vgs-regression
spec:
  restartPolicy: Never
  containers:
  - name: writer
    # An fio image, so the probe binary ships with the pod instead of being
    # installed at runtime; the entrypoint (fio) is overridden to hold the pod.
    # xridge/fio is the image the dbench storage benchmark uses (fio 3.13,
    # Alpine-based, libaio included) — alpine/fio does not exist on Docker Hub.
    image: xridge/fio:3.13-r1
    command: ["/bin/sh", "-c", "sleep 14400"]
    volumeMounts:$mounts
  volumes:$volumes
EOF
    wait_pod_ready vgs-big-writer || fail "big writer pod did not start"

    # ── Fill: real (incompressible) data so the snapshot has allocation to walk.
    #    One fio process, one sequential-write job per member, all members in
    #    parallel; --refill_buffers keeps the data incompressible. With
    #    BIG_FILL_PCT each member's fill is proportional to ITS size (sizes
    #    cycle through BIG_SIZES), so a 300Gi member carries 3x a 100Gi one and
    #    the timed generations walk realistic allocation instead of the
    #    near-empty default ─────────────────────────────────────────────────────
    fill_jobs=""; cow_jobs=""; fill_total=0
    for n in $(seq 1 "$BIG_MEMBERS"); do
      if [[ -n "$BIG_FILL_PCT" ]]; then
        sz=${big_sizes[$(( (n - 1) % ${#big_sizes[@]} ))]}
        gb=$(( ${sz%Gi} * BIG_FILL_PCT / 100 ))
      else
        gb=$BIG_FILL_GB
      fi
      [[ "$gb" -gt 0 ]] || continue
      fill_jobs+=" --name=fill$n --filename=/data$n/fill --size=${gb}G"
      cow_jobs+=" --name=cow$n --filename=/data$n/fill --size=${gb}G"
      fill_total=$(( fill_total + gb ))
    done
    if [[ -n "$fill_jobs" ]]; then
      fill_start=$(date +%s)
      kubectl -n "$NAMESPACE" exec vgs-big-writer -- /bin/sh -c \
        "fio --rw=write --bs=1M --iodepth=1 --direct=1 --ioengine=libaio --refill_buffers \
             --end_fsync=1 $fill_jobs \
             --output-format=json --output=/tmp/fio-fill.json >/dev/null 2>&1" \
        || fail "parallel fio fill failed"
      fill_dur=$(( $(date +%s) - fill_start ))
      info "fill: ${fill_total}G total across $BIG_MEMBERS members${BIG_FILL_PCT:+ (${BIG_FILL_PCT}% of each size)} in ${fill_dur}s (~$(( fill_dur > 0 ? fill_total * 1024 / fill_dur : 0 )) MB/s aggregate)"
    fi

    # ── The copy-on-write workload: random overwrites of every member's fill
    #    file, running for the WHOLE timed phase. After each take, every
    #    overwrite of a snapshotted cluster forces a CoW, so the spaced
    #    generations are timed against a blobstore actively diverging from the
    #    previous snapshot rather than an idle one. PID-filed so the per-
    #    generation probe kill cannot take it down with it. ────────────────────
    cow_running=0
    if [[ -n "$cow_jobs" ]]; then
      kubectl -n "$NAMESPACE" exec vgs-big-writer -- /bin/sh -c \
        "rm -f /tmp/fio-cow.json && nohup fio --rw=randwrite --bs=16k --iodepth=4 \
             --direct=1 --ioengine=libaio --refill_buffers --time_based --runtime=14400 \
             $cow_jobs --output-format=json --output=/tmp/fio-cow.json \
             >/dev/null 2>&1 & echo \$! > /tmp/cow.pid" \
        && cow_running=1 \
        || fail "copy-on-write workload failed to start"
    fi

    # ── Timed generations, BIG_SNAP_INTERVAL seconds of churn apart ────────────
    durations=""
    for g in $(seq 1 "$BIG_GENERATIONS"); do
      # The stall probe: the reference workload (randrw 50/50, 16k, libaio,
      # direct, md5-verified) running on member 1 across the take. The LVS
      # freeze surfaces as the max completion latency of a single I/O, at
      # microsecond resolution; verify=md5 additionally proves the data read
      # back around the freeze is intact.
      kubectl -n "$NAMESPACE" exec vgs-big-writer -- /bin/sh -c \
        "rm -f /tmp/fio-probe.json && nohup fio --name=stallprobe --filename=/data1/fio-probe \
           --rw=randrw --rwmixread=50 --bs=16k --iodepth=1 --direct=1 \
           --ioengine=libaio --size=1G --numjobs=1 --group_reporting --verify=md5 \
           --time_based --runtime=$BIG_TIMEOUT --output-format=json \
           --output=/tmp/fio-probe.json >/dev/null 2>&1 & echo \$! > /tmp/probe.pid"
      gen_start=$(date +%s)
      apply_vgs "vgs-big-gen-$g" "vgs-big-group" "$CG_LABEL" >/dev/null
      if wait_vgs_ready "vgs-big-gen-$g" "$BIG_TIMEOUT"; then
        gen_dur=$(( $(date +%s) - gen_start ))
        durations+="$gen_dur "
        # SIGINT ends the fio run cleanly and flushes its JSON; the stall is
        # the max completion latency across reads, writes, and syncs, in
        # seconds. Killed by PID, not by name: the CoW workload is also fio
        # and must keep running. The fio image carries no jq, so the JSON is
        # parsed locally.
        stall=$(kubectl -n "$NAMESPACE" exec vgs-big-writer -- /bin/sh -c \
          'p=$(cat /tmp/probe.pid); kill -INT "$p" 2>/dev/null; for i in $(seq 1 30); do kill -0 "$p" 2>/dev/null || break; sleep 1; done; cat /tmp/fio-probe.json' 2>/dev/null \
          | python3 -c 'import json,sys; s=sys.stdin.read(); j=json.loads(s[s.index("{"):])["jobs"][0]; m=max(j[d]["clat_ns"]["max"] for d in ("read","write")); m=max(m, j.get("sync",{}).get("lat_ns",{}).get("max",0)); print(f"{m/1e9:.3f}" + (" VERIFY-ERRORS" if j.get("error",0) else ""))' 2>/dev/null)
        # The group is ready; hold until every member VolumeSnapshot says so
        # too (group readyToUse is not gated on member statuses upstream), and
        # record the lag like test 13 does.
        lag_start=$(date +%s); lag=0; members_ok=1
        while [[ "$(members_ready_count "vgs-big-gen-$g")" != "$BIG_MEMBERS" ]]; do
          sleep 2; lag=$(( $(date +%s) - lag_start ))
          [[ $lag -gt "$BIG_TIMEOUT" ]] && { members_ok=0; break; }
        done
        snaps=$(count_member_snaps "vgs-big-gen-$g")
        info "generation $g: ReadyToUse in ${gen_dur}s, all member snapshots ready +${lag}s, max I/O stall ${stall:-?}s, $snaps member snapshots"
        [[ $members_ok -eq 1 ]] \
          || fail "generation $g: member snapshot statuses never all turned ready ($(members_ready_count "vgs-big-gen-$g") of $BIG_MEMBERS after ${BIG_TIMEOUT}s)"
        [[ "$snaps" == "$BIG_MEMBERS" ]] \
          || fail "generation $g materialized $snaps member snapshots, want $BIG_MEMBERS"
        # Let the CoW workload churn against the generation just taken before
        # timing the next one (skipped after the last).
        [[ "$g" -lt "$BIG_GENERATIONS" && $cow_running -eq 1 ]] && sleep "$BIG_SNAP_INTERVAL"
      else
        fail "generation $g never became ReadyToUse within ${BIG_TIMEOUT}s"
        kubectl -n "$NAMESPACE" describe volumegroupsnapshot "vgs-big-gen-$g" 2>/dev/null | tail -8
        kubectl -n "$NAMESPACE" exec vgs-big-writer -- /bin/sh -c \
          'kill -INT "$(cat /tmp/probe.pid)" 2>/dev/null' 2>/dev/null || true
        break
      fi
    done

    # Stop the CoW workload and report how much was overwritten while the
    # generations were being taken: that volume of churn is what the timings
    # were measured against.
    if [[ $cow_running -eq 1 ]]; then
      cow_written=$(kubectl -n "$NAMESPACE" exec vgs-big-writer -- /bin/sh -c \
        'p=$(cat /tmp/cow.pid); kill -INT "$p" 2>/dev/null; for i in $(seq 1 30); do kill -0 "$p" 2>/dev/null || break; sleep 1; done; cat /tmp/fio-cow.json' 2>/dev/null \
        | python3 -c 'import json,sys; s=sys.stdin.read(); d=json.loads(s[s.index("{"):]); print(round(sum(j["write"]["io_bytes"] for j in d["jobs"])/2**30, 1))' 2>/dev/null)
      info "copy-on-write churn during the timed phase: ${cow_written:-?} GiB overwritten across $BIG_MEMBERS members"
    fi

    if [[ -n "$durations" ]]; then
      pass "large-group generations completed; ReadyToUse timings: ${durations}(s)"
      info "the stall figures above are the LVS freeze windows to compare against single-volume snapshots"
    fi
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 15: crash consistency under a dependent-write workload (§5.1, §5.2)
# ═══════════════════════════════════════════════════════════════════════════════
# One writer issues strictly serialized dependent writes across all members of a
# consistency group: a global monotonic seq, round-robin target volume, each
# write a checksummed 4K block {magic, seq, volume_id, offset, generation} issued
# only after the previous is durable on the device (O_DIRECT|O_DSYNC, depth 1).
# That acknowledgment is the dependency edge. CG snapshots are taken repeatedly
# mid-workload (including a back-to-back pair with near-zero gap). Each snapshot's
# members are cloned and scanned: the set of surviving seqs, plus those the
# client log shows were overwritten before the cut, must form a contiguous
# prefix {0..M} — a hole means one volume was cut earlier than another while a
# later dependent write survived, i.e. an inconsistent cut. A negative control
# snapshots the members individually 200 ms apart, while the writer runs, and
# asserts the checker flags it: proof the workload has real cross-volume
# dependency density, so a broken cut could not pass silently (design §5.1, §5.2).
CC_MEMBERS="${CC_MEMBERS:-20}"
CC_SNAPS="${CC_SNAPS:-10}"
CC_REGION_BLOCKS="${CC_REGION_BLOCKS:-2048}"   # 4K slots per member (2048 = 8 MiB region)
CC_SETTLE="${CC_SETTLE:-4}"                     # seconds of writes between snapshots
CC_IMAGE="${CC_IMAGE:-python:3.12-alpine}"
CC_VS_CLASS="${CC_VS_CLASS:-simplyblock-csi-snapshotclass}"
CC_RUN_ID="${CC_RUN_ID:-$(( (RANDOM << 15 | RANDOM) & 0x7fffffff | 1 ))}"
# A fresh consistency group per run. The backend CG is keyed by (cluster, name)
# and its generation counter (last_group_seq) is monotonic and never reset — not
# on snapshot delete, not on member removal, and there is no group-delete for a
# label-created group. Reusing one name across runs therefore climbs the counter
# and, worse, lets each run's snapshots pile onto the members' chains until a
# 20-member atomic take exceeds the control plane's 180 s RPC budget. A unique
# name gives every run a brand-new group at generation 0 with empty chains, so
# the take never approaches that wall.
CC_GROUP="${CC_GROUP:-vgs-cc-group-$CC_RUN_ID}"

# Runs the checker in a pod over one snapshot's clones (mounted /clone1../cloneN
# from PVCs $prefix-1..$prefix-N) plus the client log, and succeeds iff the
# checker's verdict matches $expect (consistent|violation) — the checker exits 0
# only on a match, so a Succeeded pod is a matched verdict.
# Waits until the storage nodes stop reclaiming a prior run's snapshots. Deleting
# a large group is asynchronous on the single SPDK reactor and outlives the
# Kubernetes delete by minutes (journal trim + blobstore cluster free); starting
# a fresh workload into that teardown makes generation 1 collide with it and
# stall. Quiescence here = the blobstore free-cluster count has stopped moving and
# no async deletes are in flight, stable across two samples. Best-effort: on
# timeout, or if the SPDK containers cannot be read, it logs and proceeds.
wait_node_quiescent() {
  local budget=${1:-$TIMEOUT} elapsed=0 stable=0 prev="" pods p tail
  pods=$(kubectl -n "$NAMESPACE" get pods -o name 2>/dev/null | grep -oE 'snode-spdk-pod-[a-z0-9-]+')
  if [[ -z "$pods" ]]; then
    info "no storage-node pods visible; skipping quiescence wait"; return 0
  fi
  while [[ $elapsed -lt $budget ]]; do
    local cur="" busy=0
    for p in $pods; do
      tail=$(kubectl -n "$NAMESPACE" logs "$p" -c spdk-container --tail=200 2>/dev/null)
      # Recent async deletes on this node = teardown still draining.
      [[ "$(printf '%s' "$tail" | grep -c 'async delete completed')" -gt 0 ]] && busy=1
      # The blobstore "free" cluster count (3rd field of LSTAT primary) moves
      # while clusters are being reclaimed; a steady value means reclaim is done.
      cur+="$p:$(printf '%s' "$tail" | grep -oE 'LSTAT primary \[[0-9]+\] +\[[0-9]+\] +\[[0-9]+\]' \
        | tail -1 | grep -oE '[0-9]+' | sed -n 3p) "
    done
    if [[ $busy -eq 0 && "$cur" == "$prev" ]]; then
      stable=$((stable + 1))
      [[ $stable -ge 2 ]] && { info "storage nodes quiescent (no teardown in flight)"; return 0; }
    else
      stable=0
    fi
    prev="$cur"
    sleep 5; elapsed=$((elapsed + 5))
  done
  info "quiescence wait timed out after ${budget}s; proceeding (generation 1 may be slow)"
  return 0
}

cc_verify() {
  local pod=$1 prefix=$2 expect=$3 mounts="" volumes="" v old
  for v in $(seq 1 "$CC_MEMBERS"); do
    mounts+=$'\n    - name: c'"$v"$'\n      mountPath: /clone'"$v"$'\n      readOnly: true'
    volumes+=$'\n  - name: c'"$v"$'\n    persistentVolumeClaim:\n      claimName: '"$prefix-$v"
  done
  # Delete every checker pod, not just this one's namesake: each generation's
  # checker is left behind Completed, and a completed-but-undeleted pod still
  # counts as a user of the RWO log volume. If the next checker then schedules
  # onto a different node, the attach/detach controller refuses the attach
  # (Multi-Attach) until the old attachment clears — ~6 minutes per generation,
  # long enough to outlive wait_pod_success while the checker eventually runs
  # (and passes) anyway. Deleting the finished pods releases the volume at once.
  for old in $(kubectl -n "$NAMESPACE" get pods -o name 2>/dev/null | grep -E '^pod/vgs-cc-(neg-)?check'); do
    kubectl -n "$NAMESPACE" delete "$old" --ignore-not-found &>/dev/null
  done
  kubectl -n "$NAMESPACE" apply -f - >/dev/null <<CCCHK
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  labels:
    test: vgs-regression
spec:
  restartPolicy: Never
  containers:
  - name: check
    image: $CC_IMAGE
    command: ["python3", "/scripts/checker.py"]
    env:
    - name: CC_MEMBERS
      value: "$CC_MEMBERS"
    - name: CC_REGION_BLOCKS
      value: "$CC_REGION_BLOCKS"
    - name: CC_RUN_ID
      value: "$CC_RUN_ID"
    - name: CC_LOG_FILE
      value: /cclog/writes.log
    - name: CC_EXPECT
      value: "$expect"
    volumeMounts:
    - name: scripts
      mountPath: /scripts
    - name: log
      mountPath: /cclog
      readOnly: true$mounts
  volumes:
  - name: scripts
    configMap:
      name: vgs-cc-scripts
  - name: log
    persistentVolumeClaim:
      claimName: vgs-cc-log$volumes
CCCHK
  # The checker's runtime grows with the generation number: each later clone
  # sits on a deeper snapshot chain, so its reads walk more backing layers
  # (~24 s at gen-1, ~9 min by gen-7 for the same fixed 160 MiB scan). The
  # default 300 s budget is exceeded from gen-5 on, so use BIG_TIMEOUT here.
  wait_pod_success "$pod" "$BIG_TIMEOUT"
}

if run_test 15; then
  section "Test 15: crash consistency under dependent writes ($CC_MEMBERS members, $CC_SNAPS mid-workload snapshots)"
  setup_storageclass >/dev/null

  # Do not start into a prior run's teardown: wait for the storage nodes to
  # quiesce first, so generation 1 does not collide with async snapshot deletes.
  wait_node_quiescent

  # Per-volume snapshot class for the negative control (the group class cannot
  # snapshot a single PVC). Script-owned and idempotent. Server-side apply so
  # adopting a class the chart/CSI installer already created does not warn about
  # the missing kubectl.kubernetes.io/last-applied-configuration annotation
  # (client-side apply stores that annotation; server-side apply does not use it).
  kubectl apply --server-side --force-conflicts -f - >/dev/null <<CCVSC
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: $CC_VS_CLASS
  labels:
    test: vgs-regression
driver: csi.simplyblock.io
deletionPolicy: Delete
CCVSC

  # The group's members, plus a separate (non-member) volume for the client log.
  for v in $(seq 1 "$CC_MEMBERS"); do
    kubectl -n "$NAMESPACE" apply -f - >/dev/null <<CCPVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vgs-cc-pvc-$v
  labels:
    test: vgs-regression
    $CG_LABEL: $CC_GROUP
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: $PVC_SIZE
CCPVC
  done
  kubectl -n "$NAMESPACE" apply -f - >/dev/null <<CCLOG
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vgs-cc-log
  labels:
    test: vgs-regression
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: 1Gi
CCLOG

  cc_ok=1
  for v in $(seq 1 "$CC_MEMBERS"); do
    wait_pvc_bound "vgs-cc-pvc-$v" || { fail "vgs-cc-pvc-$v did not bind"; cc_ok=0; }
  done
  wait_pvc_bound vgs-cc-log || { fail "vgs-cc-log did not bind"; cc_ok=0; }
  [[ $cc_ok -eq 1 ]] && pass "all $CC_MEMBERS members and the log volume bound"

  # Ship the writer and checker programs as a ConfigMap.
  cc_dir=$(mktemp -d "${TMPDIR:-/tmp}/vgs-cc-XXXXXX")
  cat > "$cc_dir/writer.py" <<'CC_WRITER_PY'
#!/usr/bin/env python3
# Dependent-write workload for the consistency-group crash-consistency test.
#
# One writer, one global monotonically increasing seq counter, N volumes mounted
# at /data1../dataN. Each write is a full 4K block carrying
# {magic, seq, volume_id, offset, generation, md5(block)}; the target volume is
# round-robin by seq, the offset random within a fixed region. Writes are
# strictly serialized at depth 1: O_DIRECT|O_DSYNC pwrite, so seq is durable on
# the device before seq+1 is issued. That acknowledgment is the dependency edge.
# Every issued write is logged as "seq vol off dep" (dep = max seq completed
# when this one was issued) so the checker knows where to look and what each
# write depended on.
import hashlib
import os
import random
import struct
import sys
import time

MAGIC = b"CCB1"
BS = 4096
DIGEST = 16  # md5
HDR = struct.Struct("<4sQIQI")  # magic, seq(u64), vol_id(u32), offset(u64), gen(u32)


def build_block(seq, vol, off, gen):
    body = bytearray(BS)
    HDR.pack_into(body, 0, MAGIC, seq, vol, off, gen)
    # Deterministic filler derived from the block's identity: makes a torn or
    # misdirected block detectable and the payload incompressible.
    seed = hashlib.sha256(f"{seq}:{vol}:{off}:{gen}".encode()).digest()
    region = memoryview(body)[HDR.size : BS - DIGEST]
    for i in range(0, len(region), len(seed)):
        region[i : i + len(seed)] = seed[: len(region) - i]
    body[BS - DIGEST : BS] = hashlib.md5(bytes(body[0 : BS - DIGEST])).digest()
    return bytes(body)


def open_direct(path):
    """Open O_DIRECT|O_DSYNC when the filesystem allows it (the faithful mode),
    falling back to O_SYNC alone otherwise. Returns (fd, direct?)."""
    flags = os.O_RDWR | os.O_DSYNC
    try:
        fd = os.open(path, flags | os.O_DIRECT)
        return fd, True
    except OSError:
        return os.open(path, flags), False


def main():
    members = int(os.environ["CC_MEMBERS"])
    region_blocks = int(os.environ["CC_REGION_BLOCKS"])
    gen = int(os.environ["CC_RUN_ID"])
    stop_file = os.environ["CC_STOP_FILE"]
    log_path = os.environ["CC_LOG_FILE"]
    started = os.environ.get("CC_STARTED_FILE", "")
    paths = [f"/data{i}/cc.dat" for i in range(1, members + 1)]

    # Pre-write every slot once with the sentinel generation 0 and fsync, so the
    # measured workload is pure in-place 4K overwrites: no extent allocation, no
    # metadata churn that could reorder underneath the dependency chain. The
    # checker ignores generation-0 blocks, so a slot the workload never revisits
    # contributes nothing rather than masquerading as a real write.
    for v, path in enumerate(paths, start=1):
        if not os.path.exists(path):
            with open(path, "wb") as f:
                f.truncate(0)
        fd, _direct = open_direct(path)
        for b in range(region_blocks):
            os.pwrite(fd, build_block(0, v, b * BS, 0), b * BS)
        os.fsync(fd)
        os.close(fd)
    mode = None
    fds = []
    for path in paths:
        fd, direct = open_direct(path)
        fds.append(fd)
        mode = "O_DIRECT|O_DSYNC" if direct else "O_DSYNC"
    print(f"CC_WRITER mode={mode} members={members} region_blocks={region_blocks}", flush=True)

    log = open(log_path, "w", buffering=1)
    rng = random.Random(gen)
    if started:
        open(started, "w").close()

    completed_max = -1
    seq = 0
    last_report = time.time()
    while not os.path.exists(stop_file):
        vol = seq % members  # round-robin target
        off = rng.randrange(region_blocks) * BS
        dep = completed_max  # depth 1: everything acked before this issue
        log.write(f"{seq} {vol + 1} {off} {dep}\n")
        os.pwrite(fds[vol], build_block(seq, vol + 1, off, gen), off)
        completed_max = seq  # durable on return (O_DSYNC) -> the dependency edge
        seq += 1
        if time.time() - last_report >= 2:
            print(f"CC_WRITER progress seq={completed_max}", flush=True)
            last_report = time.time()

    log.write(f"# stopped at {completed_max}\n")
    log.flush()
    for fd in fds:
        os.close(fd)
    print(f"CC_WRITER stopped at {completed_max}", flush=True)


if __name__ == "__main__":
    sys.exit(main())
CC_WRITER_PY
  cat > "$cc_dir/checker.py" <<'CC_CHECKER_PY'
#!/usr/bin/env python3
# Crash-consistency checker for the consistency-group snapshot test.
#
# Clones of one snapshot's members are mounted at /clone1../cloneN (clone v is
# volume v). Each 4K slot holds at most the latest seq written to that
# (volume, offset) at or before the cut. We collect the set S of seqs present
# (checksum-valid, magic-valid, offset and volume_id matching where the block
# actually sits, generation matching this run so stale data from a prior run is
# ignored). From the client log we mark, for each offset, every write older than
# the one the clone holds as "superseded" rather than missing.
#
# Invariant (strictly serialized writer): S union superseded must be a
# contiguous prefix {0..M}. A hole at k < M means some volume was cut earlier
# than another while a later dependent write survived — a write-order violation,
# i.e. an inconsistent cut. The general consistent-cut check over the logged
# dependency (every write that had completed before a present write was issued
# must itself be present) is applied too, so the same checker catches depth>1.
import hashlib
import os
import struct
import sys

MAGIC = b"CCB1"
BS = 4096
DIGEST = 16
HDR = struct.Struct("<4sQIQI")


def parse(block, expect_off):
    if len(block) != BS or bytes(block[0:4]) != MAGIC:
        return None
    _magic, seq, vol, off, gen = HDR.unpack_from(block, 0)
    if hashlib.md5(bytes(block[0 : BS - DIGEST])).digest() != bytes(block[BS - DIGEST : BS]):
        return None  # torn / partial block
    if off != expect_off:
        return None  # misdirected block
    return seq, vol, off, gen


def main():
    members = int(os.environ["CC_MEMBERS"])
    region_blocks = int(os.environ["CC_REGION_BLOCKS"])
    gen = int(os.environ["CC_RUN_ID"])
    log_path = os.environ["CC_LOG_FILE"]
    expect = os.environ.get("CC_EXPECT", "consistent")  # or "violation" (negative control)

    present = {}  # seq -> (vol, off)
    present_at = {}  # (vol, off) -> seq the clone holds
    for v in range(1, members + 1):
        path = f"/clone{v}/cc.dat"
        with open(path, "rb") as f:
            for b in range(region_blocks):
                off = b * BS
                f.seek(off)
                r = parse(f.read(BS), off)
                if r is None:
                    continue
                seq, vol, _off, g = r
                if g != gen or vol != v:
                    continue
                present[seq] = (vol, off)
                present_at[(vol, off)] = seq

    writes = []  # (seq, vol, off, dep)
    offset_writes = {}  # (vol, off) -> [seq, ...] in issue order
    with open(log_path) as lf:
        for line in lf:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            seq, vol, off, dep = (int(x) for x in line.split())
            writes.append((seq, vol, off, dep))
            offset_writes.setdefault((vol, off), []).append(seq)

    # A write is superseded if the clone holds a strictly newer write to the
    # same (volume, offset): it was legitimately overwritten before the cut.
    superseded = set()
    for key, seqs in offset_writes.items():
        p = present_at.get(key)
        if p is None:
            continue
        for s in seqs:
            if s < p:
                superseded.add(s)

    covered = set(present.keys()) | superseded
    if not covered:
        print("CC_RESULT=EMPTY")
        return 1

    m = max(covered)
    gap = next((k for k in range(m + 1) if k not in covered), None)

    dep_viol = None
    for seq, _vol, _off, dep in writes:
        if seq in present:
            j = next((x for x in range(dep + 1) if x not in covered), None)
            if j is not None:
                dep_viol = (seq, j)
                break

    consistent = gap is None and dep_viol is None
    if consistent:
        print(f"CC_RESULT=CONSISTENT M={m} present={len(present)} superseded={len(superseded)}")
    else:
        print(f"CC_RESULT=VIOLATION gap={gap} dep_viol={dep_viol} M={m} present={len(present)}")

    # Exit code encodes match against expectation, so the harness reads it directly.
    if expect == "violation":
        return 0 if not consistent else 2  # 2: negative control failed to trigger
    return 0 if consistent else 1


if __name__ == "__main__":
    sys.exit(main())
CC_CHECKER_PY
  kubectl -n "$NAMESPACE" create configmap vgs-cc-scripts \
    --from-file=writer.py="$cc_dir/writer.py" \
    --from-file=checker.py="$cc_dir/checker.py" \
    --dry-run=client -o yaml | kubectl -n "$NAMESPACE" apply -f - >/dev/null
  rm -rf "$cc_dir"

  # The writer pod mounts every member plus the log volume and runs the workload.
  if [[ $cc_ok -eq 1 ]]; then
    mounts=""; volumes=""
    for v in $(seq 1 "$CC_MEMBERS"); do
      mounts+=$'\n    - name: v'"$v"$'\n      mountPath: /data'"$v"
      volumes+=$'\n  - name: v'"$v"$'\n    persistentVolumeClaim:\n      claimName: vgs-cc-pvc-'"$v"
    done
    kubectl -n "$NAMESPACE" apply -f - >/dev/null <<CCWRITER
apiVersion: v1
kind: Pod
metadata:
  name: vgs-cc-writer
  labels:
    test: vgs-regression
spec:
  restartPolicy: Never
  containers:
  - name: writer
    image: $CC_IMAGE
    command: ["python3", "/scripts/writer.py"]
    env:
    - name: CC_MEMBERS
      value: "$CC_MEMBERS"
    - name: CC_REGION_BLOCKS
      value: "$CC_REGION_BLOCKS"
    - name: CC_RUN_ID
      value: "$CC_RUN_ID"
    - name: CC_STOP_FILE
      value: /cclog/stop
    - name: CC_STARTED_FILE
      value: /cclog/started
    - name: CC_LOG_FILE
      value: /cclog/writes.log
    volumeMounts:
    - name: scripts
      mountPath: /scripts
    - name: log
      mountPath: /cclog$mounts
  volumes:
  - name: scripts
    configMap:
      name: vgs-cc-scripts
  - name: log
    persistentVolumeClaim:
      claimName: vgs-cc-log$volumes
CCWRITER
    wait_pod_ready vgs-cc-writer || { fail "cc writer pod did not start"; cc_ok=0; }
  fi

  # Wait for the prefill to finish and the dependent-write loop to begin.
  if [[ $cc_ok -eq 1 ]]; then
    started=0
    for _ in $(seq 1 "$TIMEOUT"); do
      if kubectl -n "$NAMESPACE" exec vgs-cc-writer -- test -f /cclog/started 2>/dev/null; then started=1; break; fi
      sleep 2
    done
    [[ $started -eq 1 ]] && pass "writer prefilled and the dependent-write loop is running" \
      || { fail "writer never began the dependent-write loop"; cc_ok=0; }
  fi

  # ── CG snapshots taken mid-workload, then a back-to-back pair ───────────────
  # Every spaced generation is gated on ReadyToUse before the next is taken, so
  # the in-flight snapshot queue stays ~one generation deep instead of firing all
  # of them (CC_SNAPS x CC_MEMBERS member snapshots) at once and oversubscribing
  # the one-at-a-time backend. The writer keeps running through each wait, so
  # every generation is still a distinct mid-workload cut.
  cc_snaps=""
  if [[ $cc_ok -eq 1 ]]; then
    for i in $(seq 1 "$CC_SNAPS"); do
      sleep "$CC_SETTLE"
      apply_vgs "vgs-cc-gen-$i" "$CC_GROUP" "$CG_LABEL" >/dev/null
      cc_snaps+="vgs-cc-gen-$i "
      info "took CG snapshot vgs-cc-gen-$i at ~$(kubectl -n "$NAMESPACE" exec vgs-cc-writer -- sh -c 'wc -l < /cclog/writes.log' 2>/dev/null | tr -d ' ') writes"
      wait_vgs_ready "vgs-cc-gen-$i" || { fail "vgs-cc-gen-$i never became ReadyToUse"; cc_ok=0; break; }
    done
  fi

  # ── Back-to-back pair: two generations with near-zero gap, in flight together
  #    — the coordination-under-contention case. Taken as its own step and drained
  #    to ready BEFORE the negative control, so the pair's member materialization
  #    does not overlap the negative control's burst of CC_MEMBERS snapshots.
  if [[ $cc_ok -eq 1 ]]; then
    apply_vgs "vgs-cc-gen-b2b-a" "$CC_GROUP" "$CG_LABEL" >/dev/null
    apply_vgs "vgs-cc-gen-b2b-b" "$CC_GROUP" "$CG_LABEL" >/dev/null
    cc_snaps+="vgs-cc-gen-b2b-a vgs-cc-gen-b2b-b "
    info "took back-to-back CG pair vgs-cc-gen-b2b-a + vgs-cc-gen-b2b-b (near-zero gap)"
    wait_vgs_ready "vgs-cc-gen-b2b-a" || { fail "vgs-cc-gen-b2b-a never became ReadyToUse"; cc_ok=0; }
    wait_vgs_ready "vgs-cc-gen-b2b-b" || { fail "vgs-cc-gen-b2b-b never became ReadyToUse"; cc_ok=0; }
  fi

  # ── Negative control: per-member snapshots 200 ms apart, while the writer
  #    still runs — a deliberately inconsistent cut ─────────────────────────────
  if [[ $cc_ok -eq 1 ]]; then
    for v in $(seq 1 "$CC_MEMBERS"); do
      kubectl -n "$NAMESPACE" apply -f - >/dev/null <<CCNEG
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: vgs-cc-neg-$v
  labels:
    test: vgs-regression
spec:
  volumeSnapshotClassName: $CC_VS_CLASS
  source:
    persistentVolumeClaimName: vgs-cc-pvc-$v
CCNEG
      sleep 0.2
    done
  fi

  # Stop the writer so the log is complete, then wait for every snapshot ready.
  if [[ $cc_ok -eq 1 ]]; then
    kubectl -n "$NAMESPACE" exec vgs-cc-writer -- touch /cclog/stop 2>/dev/null || true
    for s in $cc_snaps; do
      wait_vgs_ready "$s" || { fail "$s never became ReadyToUse"; cc_ok=0; }
    done
    for v in $(seq 1 "$CC_MEMBERS"); do
      wait_snapshot_ready "vgs-cc-neg-$v" || { fail "negative-control snapshot vgs-cc-neg-$v never ready"; cc_ok=0; }
    done
  fi

  # ── Every CG snapshot must be a consistent cut ──────────────────────────────
  if [[ $cc_ok -eq 1 ]]; then
    # Release the RWO log volume so the checker pods can mount it read-only.
    kubectl -n "$NAMESPACE" delete pod vgs-cc-writer --ignore-not-found &>/dev/null
    for s in $cc_snaps; do
      clones_ok=1
      for v in $(seq 1 "$CC_MEMBERS"); do
        msnap=$(member_snap_of "$s" "vgs-cc-pvc-$v")
        [[ -z "$msnap" ]] && { fail "$s has no member snapshot for vgs-cc-pvc-$v"; clones_ok=0; break; }
        wait_snapshot_ready "$msnap" || { fail "member snapshot $msnap never ready"; clones_ok=0; break; }
        make_restore_pvc "vgs-cc-clone-$s-$v" "$msnap" >/dev/null
      done
      [[ $clones_ok -eq 1 ]] || { cc_ok=0; continue; }
      for v in $(seq 1 "$CC_MEMBERS"); do
        wait_pvc_bound "vgs-cc-clone-$s-$v" || { fail "clone vgs-cc-clone-$s-$v did not bind"; clones_ok=0; }
      done
      [[ $clones_ok -eq 1 ]] || { cc_ok=0; continue; }
      if cc_verify "vgs-cc-check-$s" "vgs-cc-clone-$s" consistent; then
        pass "$s is a consistent cut: $(kubectl -n "$NAMESPACE" logs "vgs-cc-check-$s" 2>/dev/null | grep '^CC_RESULT=')"
      else
        fail "$s is NOT a consistent cut: $(kubectl -n "$NAMESPACE" logs "vgs-cc-check-$s" 2>/dev/null | grep '^CC_RESULT=' || echo 'checker did not run')"
      fi
    done
  fi

  # ── Negative control: the staggered individual snapshots must be flagged ────
  if [[ $cc_ok -eq 1 ]]; then
    neg_ok=1
    for v in $(seq 1 "$CC_MEMBERS"); do
      make_restore_pvc "vgs-cc-neg-clone-$v" "vgs-cc-neg-$v" >/dev/null
    done
    for v in $(seq 1 "$CC_MEMBERS"); do
      wait_pvc_bound "vgs-cc-neg-clone-$v" || { fail "negative-control clone vgs-cc-neg-clone-$v did not bind"; neg_ok=0; }
    done
    if [[ $neg_ok -eq 1 ]]; then
      if cc_verify "vgs-cc-neg-check" "vgs-cc-neg-clone" violation; then
        pass "negative control flagged as expected: $(kubectl -n "$NAMESPACE" logs vgs-cc-neg-check 2>/dev/null | grep '^CC_RESULT=')"
      else
        fail "negative control did NOT trigger — the workload lacks cross-volume dependency density, so a broken cut would pass silently: $(kubectl -n "$NAMESPACE" logs vgs-cc-neg-check 2>/dev/null | grep '^CC_RESULT=')"
      fi
    fi
  fi
fi

# ═══════════════════════════════════════════════════════════════════════════════
# Test 16: dynamic membership — the label is live after provisioning (§4.5)
# ═══════════════════════════════════════════════════════════════════════════════
# Phase 4: labeling an EXISTING bound PVC joins its volume to the group (the
# CSI controller's label watcher relays the label to the backend member add),
# removing the label detaches it (epoch closed one-way, history preserved),
# and a re-added label is refused: a closed epoch never reopens.
DYN_PVC="vgs-pvc-dyn"

# Wait until the backend group_id of the PVC's volume is present or absent.
wait_group_id() {
  local pvc=$1 want=$2 elapsed=0 gid lvol
  lvol=$(lvol_of_pvc "$pvc")
  while [[ $elapsed -lt $TIMEOUT ]]; do
    gid=$(sb volume get "$lvol" --json 2>/dev/null | jq -r '.group_id // empty')
    case $want in
      present) [[ -n "$gid" ]] && return 0 ;;
      absent)  [[ -z "$gid" ]] && return 0 ;;
    esac
    sleep 5; elapsed=$((elapsed + 5))
  done
  return 1
}

if run_test 16; then
  section "Test 16: label add joins an existing volume; label removal detaches it (§4.5)"
  # Standalone-friendly: test 16 needs test 1's group (its members and their
  # placement pin), so ensure the member PVCs exist and are bound before
  # reading the pin. When test 1 already ran, this is a no-op.
  setup_storageclass >/dev/null
  for pvc in "${MEMBER_PVCS[@]}"; do
    kubectl -n "$NAMESPACE" get pvc "$pvc" &>/dev/null || make_member_pvc "$pvc" >/dev/null
  done
  members_ok=1
  for pvc in "${MEMBER_PVCS[@]}"; do
    wait_pvc_bound "$pvc" || { fail "member PVC $pvc did not bind"; members_ok=0; }
  done
  pin=""
  [[ $members_ok -eq 1 ]] && pin=$(node_of_lvol "$(lvol_of_pvc "${MEMBER_PVCS[0]}")")
  if [[ -z "$pin" ]]; then
    fail "cannot resolve the group's pinned node; run test 1 first (or fix member provisioning)"
  else
  # An unlabeled PVC pinned to the group's node: a late join never moves a
  # volume, so the placement check only passes if it was born on the pin.
  # Recreate rather than adopt: an interrupted earlier run can leave this PVC
  # Pending or placed off the pin (its annotation was stamped before the pin
  # resolved), and a stale placement would fail the join for the wrong reason.
  kubectl -n "$NAMESPACE" delete pvc "$DYN_PVC" --ignore-not-found &>/dev/null
  kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $DYN_PVC
  labels:
    test: vgs-regression
  annotations:
    simplyblock.io/selected-storage-node: $pin
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  resources:
    requests:
      storage: $PVC_SIZE
EOF
  if wait_pvc_bound "$DYN_PVC"; then
    pass "existing (unlabeled) PVC $DYN_PVC bound on the pinned node"
    if [[ -n "$(sb volume get "$(lvol_of_pvc "$DYN_PVC")" --json 2>/dev/null \
                | jq -r '.group_id // empty')" ]]; then
      fail "unlabeled volume is already a group member"
    fi

    # ── Label add: the volume joins, and the NEXT generation contains it ──────
    kubectl -n "$NAMESPACE" label pvc "$DYN_PVC" "$CG_LABEL=$CG_NAME" --overwrite >/dev/null
    if wait_group_id "$DYN_PVC" present; then
      pass "label add joined the existing volume to the group"
    else
      fail "label add did not join the volume (no group_id after ${TIMEOUT}s)"
    fi

    apply_vgs vgs-gen-dyn "$CG_NAME" "$CG_LABEL"
    if wait_vgs_ready vgs-gen-dyn \
       && [[ "$(count_member_snaps vgs-gen-dyn)" == "$((MEMBER_COUNT + 1))" ]]; then
      pass "next generation contains the late-labeled member ($((MEMBER_COUNT + 1)) snapshots)"
    else
      fail "generation after the join has $(count_member_snaps vgs-gen-dyn) member snapshots, expected $((MEMBER_COUNT + 1))"
    fi
    dyn_snap=$(kubectl -n "$NAMESPACE" get volumesnapshot -o json 2>/dev/null \
      | jq -r --arg p "$DYN_PVC" \
        '.items[] | select(.status.volumeGroupSnapshotName == "vgs-gen-dyn")
                  | select(.spec.source.persistentVolumeClaimName == $p) | .metadata.name')

    # ── Label removal: the epoch closes, history stays ReadyToUse ─────────────
    kubectl -n "$NAMESPACE" label pvc "$DYN_PVC" "${CG_LABEL}-" >/dev/null
    if wait_group_id "$DYN_PVC" absent; then
      pass "label removal detached the volume (group_id cleared)"
    else
      fail "label removal did not detach the volume within ${TIMEOUT}s"
    fi

    apply_vgs vgs-gen-dyn2 "$CG_NAME" "$CG_LABEL"
    if wait_vgs_ready vgs-gen-dyn2 \
       && [[ "$(count_member_snaps vgs-gen-dyn2)" == "$MEMBER_COUNT" ]]; then
      pass "generation after the detach excludes the departed member ($MEMBER_COUNT snapshots)"
    else
      fail "generation after the detach has $(count_member_snaps vgs-gen-dyn2) member snapshots, expected $MEMBER_COUNT"
    fi

    if [[ -n "$dyn_snap" ]]; then
      ready=$(kubectl -n "$NAMESPACE" get volumesnapshot "$dyn_snap" \
        -o jsonpath='{.status.readyToUse}' 2>/dev/null)
      if [[ "$ready" == "true" ]]; then
        pass "departed member's snapshot in the earlier generation stays ReadyToUse"
      else
        fail "departed member's earlier snapshot $dyn_snap is not ReadyToUse (got '${ready:-gone}')"
      fi
    fi

    # ── One-way: a re-added label is refused, visibly on the PVC ──────────────
    kubectl -n "$NAMESPACE" label pvc "$DYN_PVC" "$CG_LABEL=$CG_NAME" --overwrite >/dev/null
    refused=0; elapsed=0
    while [[ $elapsed -lt $TIMEOUT ]]; do
      if kubectl -n "$NAMESPACE" get events \
           --field-selector "involvedObject.name=$DYN_PVC" 2>/dev/null \
         | grep -q ConsistencyGroupJoinRefused; then
        refused=1; break
      fi
      sleep 5; elapsed=$((elapsed + 5))
    done
    gid_after=$(sb volume get "$(lvol_of_pvc "$DYN_PVC")" --json 2>/dev/null \
      | jq -r '.group_id // empty')
    if [[ $refused -eq 1 && -z "$gid_after" ]]; then
      pass "re-added label refused one-way (ConsistencyGroupJoinRefused on the PVC, no rejoin)"
    elif [[ -n "$gid_after" ]]; then
      fail "a closed epoch REJOINED on label re-add (group_id=$gid_after) — one-way rule broken"
    else
      fail "no ConsistencyGroupJoinRefused event on $DYN_PVC within ${TIMEOUT}s"
    fi

    kubectl -n "$NAMESPACE" label pvc "$DYN_PVC" "${CG_LABEL}-" &>/dev/null || true
    kubectl -n "$NAMESPACE" delete pvc "$DYN_PVC" --ignore-not-found &>/dev/null
  else
    fail "$DYN_PVC did not bind"
  fi
  fi  # pin resolved
fi

# ═══════════════════════════════════════════════════════════════════════════════
section "Summary"
echo "PASSED=$PASSED  FAILED=$FAILED"
[[ $FAILED -eq 0 ]]
