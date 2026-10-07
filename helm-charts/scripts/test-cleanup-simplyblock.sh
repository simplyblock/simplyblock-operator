#!/usr/bin/env bash
# Behavioral tests for cleanup-simplyblock.sh against a stub kubectl and helm, so
# no cluster is needed. Lives beside the script it covers; run it directly.
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cleanup-simplyblock.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir "$WORK/bin"
# The stub records every call in CALLS and answers the few the script's
# StorageClass step makes. A list selected by label returns STUB_MANAGED_CLASSES,
# and the list of every class of the driver returns STUB_DRIVER_CLASSES, one
# "<name> <managed-by label>" per line. Everything else answers with nothing.
cat > "$WORK/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$CALLS"
case "$*" in
  "config current-context") echo stub-context ;;
  "config view"*) echo stub-cluster ;;
  "get crd -o name")
    # STUB_CRD_LISTINGS listings report one CRD, and later ones report none, as
    # the cluster does once the script has deleted it.
    n="$(cat "$STATE" 2>/dev/null || echo 0)"
    echo $((n + 1)) > "$STATE"
    if (( n < ${STUB_CRD_LISTINGS:-0} )); then
      echo customresourcedefinition.apiextensions.k8s.io/storagenodes.storage.simplyblock.io
    fi ;;
  "get storageclass"*" -l "*|"get storageclass"*"--selector"*) printf '%b' "${STUB_MANAGED_CLASSES:-}" ;;
  "get storageclass"*) printf '%b' "${STUB_DRIVER_CLASSES:-}" ;;
esac
exit 0
EOF
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/helm"
chmod +x "$WORK/bin/kubectl" "$WORK/bin/helm"

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

# run_script <managed classes> <driver classes> [script args...]: sets OUT, RC,
# and leaves every kubectl call in CALLS.
run_script() {
  local managed="$1" driver="$2"
  shift 2
  : > "$WORK/calls.log"
  rm -f "$WORK/state"
  OUT="$(env PATH="$WORK/bin:$PATH" CALLS="$WORK/calls.log" STATE="$WORK/state" \
    STUB_MANAGED_CLASSES="$managed" STUB_DRIVER_CLASSES="$driver" \
    bash "$SCRIPT" --force "$@" 2>&1)"
  RC=$?
}

deleted_classes() { grep -E '^delete storageclass ' "$WORK/calls.log" | awk '{print $3}' | sort | tr '\n' ' '; }

# Regression: 2026-10-07-cleanup-leaves-storageclasses. The script wipes the
# finalizers of every custom resource before it deletes them, which skips the
# storage pool's own deletion path, the only thing that removes the StorageClass
# the operator wrote for the default pool. The class stayed behind carrying the
# old cluster's cluster_id, which a StorageClass cannot change, so a cluster
# recreated under the same name provisioned nothing: "cluster not found in
# secret configuration".
run_script 'simplyblock-simplyblock-a\nsimplyblock-simplyblock-b\n' ''
[[ $RC -eq 0 ]] || fail "exit $RC, want 0"
got="$(deleted_classes)"
[[ "$got" == "simplyblock-simplyblock-a simplyblock-simplyblock-b " ]] ||
  fail "deleted StorageClasses = [$got], want both operator-written classes"

# Only the classes this namespace's operator wrote are selected: the managed-by
# marker and the namespace label are both in the selector, so another namespace's
# class, and a class somebody authored, are not matched.
selector="$(grep -E '^get storageclass.* -l ' "$WORK/calls.log" | head -1)"
[[ "$selector" == *"storage.simplyblock.io/managed-by=storagecluster"* ]] ||
  fail "selector [$selector] does not require the managed-by marker"
[[ "$selector" == *"storage.simplyblock.io/namespace=simplyblock"* ]] ||
  fail "selector [$selector] does not name the namespace"

# The namespace argument is honored.
run_script 'simplyblock-other-a\n' '' other
selector="$(grep -E '^get storageclass.* -l ' "$WORK/calls.log" | head -1)"
[[ "$selector" == *"storage.simplyblock.io/namespace=other"* ]] ||
  fail "selector [$selector] does not name the namespace other"

# A class the operator did not write stays, and the output names it, since it may
# point at a cluster that no longer exists.
run_script '' 'my-authored-class \nsimplyblock-simplyblock-a storagecluster\n'
[[ -z "$(deleted_classes)" ]] || fail "deleted [$(deleted_classes)], want nothing deleted"
[[ "$OUT" == *"my-authored-class"* ]] || fail "output does not mention the authored class my-authored-class"
[[ "$OUT" != *"simplyblock-simplyblock-a"* ]] ||
  fail "output mentions the operator-written class simplyblock-simplyblock-a, which another namespace owns"

# Nothing to remove is not an error.
run_script '' ''
[[ $RC -eq 0 ]] || fail "no classes: exit $RC, want 0"
[[ -z "$(deleted_classes)" ]] || fail "no classes: deleted [$(deleted_classes)]"

# Regression: 2026-10-07-cleanup-aborts-without-crds. grep exits 1 when nothing
# matches, and with pipefail that ended the script. On a cluster with no
# simplyblock CRDs it stopped after the first step, so a rerun after the CRDs were
# gone never reached anything below it. Once the script had deleted the last CRD
# it stopped at the confirmation, with exit 1, before saying it was done.
run_script 'simplyblock-simplyblock-a\n' ''
[[ $RC -eq 0 ]] || fail "no CRDs on the cluster: exit $RC, want 0"
[[ "$OUT" == *"Cleanup complete"* ]] || fail "no CRDs on the cluster: the script did not finish"

STUB_CRD_LISTINGS=1 run_script '' ''
[[ $RC -eq 0 ]] || fail "CRDs removed by the script: exit $RC, want 0"
[[ "$OUT" == *"All CRDs removed successfully"* ]] || fail "CRDs removed by the script: no confirmation"
[[ "$OUT" == *"Cleanup complete"* ]] || fail "CRDs removed by the script: the script did not finish"

if [[ $failures -gt 0 ]]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "ok"
