#!/usr/bin/env bash
# Behavioral tests for cleanup-simplyblock.sh against a stub kubectl and helm,
# so no cluster is needed. Lives beside the script it covers; run it directly.
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cleanup-simplyblock.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir "$WORK/bin"
# The stub answers a `get <kind> -o json` from $STUB_DIR/<kind>.json when that
# file exists and with an empty list otherwise. One simplyblock CRD is
# installed, so the CRD steps have something to iterate. Every other call
# succeeds with no output, which reads as "nothing there" to the script.
cat > "$WORK/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == get && " $* " == *" -o json "* ]]; then
  if [[ -f "$STUB_DIR/$2.json" ]]; then cat "$STUB_DIR/$2.json"; else echo '{"items":[]}'; fi
  exit 0
fi
if [[ "$*" == "get crd -o name" ]]; then
  echo customresourcedefinition.apiextensions.k8s.io/storageclusters.storage.simplyblock.io
  exit 0
fi
[[ "$1 $2" == "get crd" && "${3:-}" != -* ]] && exit 1
exit 0
EOF
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/helm"
chmod +x "$WORK/bin/kubectl" "$WORK/bin/helm"

FAILED=0

# A list larger than macOS's 1 MiB limit on a process's arguments and
# environment together, which is what a cluster with many PVs or pods returns.
big_list() {
  python3 -c '
import json, sys
pad = "x" * 4096
print(json.dumps({"items": [{"metadata": {"name": f"obj-{i}", "annotations": {"pad": pad}},
                             "spec": {}} for i in range(int(sys.argv[1]))]}))
' "$1"
}

run_case() {
  local name="$1" expect="$2"
  local stub="$WORK/$name"
  mkdir -p "$stub"
  shift 2
  for kind in "$@"; do big_list 600 > "$stub/$kind.json"; done
  local out
  out=$(PATH="$WORK/bin:$PATH" STUB_DIR="$stub" bash "$SCRIPT" -f simplyblock 2>&1)
  if grep -q "Argument list too long" <<< "$out" || ! grep -q "$expect" <<< "$out"; then
    echo "FAIL $name: expected '$expect' and no 'Argument list too long'"
    echo "$out" | grep -E "ERROR|WARN|too long" | sed 's/^/    /'
    FAILED=1
  else
    echo "ok   $name"
  fi
}

run_case "small lists" "Cleanup complete"
run_case "many PVs" "No pod in 'simplyblock' mounts a csi.simplyblock.io volume" pv
run_case "many pods" "Cleanup complete" pods
run_case "many StorageClasses" "Cleanup complete" storageclass

# Regression: 2026-10-23-cleanup-operatorops-finalizer — the chart renders the
# initial discovery run as an OperatorOps carrying the operator's finalizer, and
# Helm deletes it. Step 0 stopped the operator before the uninstall, so nothing
# could clear the finalizer, `helm uninstall --wait` timed out after 120s, and
# the script exited with the storage cluster, its SPDK pods, the control plane,
# and every CR still in place.
#
# This stub keeps state: the operator runs until it is scaled down, and a
# deleted OperatorOps goes only while the operator runs or once its finalizer is
# stripped. The helm stub fails its wait the way Helm does while one is still there.
stateful_case() {
  local name="chart OperatorOps outlives a stopped operator"
  local dir="$WORK/stateful"
  mkdir -p "$dir/bin" "$dir/state"
  touch "$dir/state/operator" "$dir/state/ops" "$dir/state/release"
  cat > "$dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
S="$STATE_DIR"
ops=operatorops.storage.simplyblock.io
args=" $* "
case "$1" in
  get)
    if [[ "$*" == "get crd -o name" ]]; then
      echo customresourcedefinition.apiextensions.k8s.io/$ops
      echo customresourcedefinition.apiextensions.k8s.io/storageclusters.storage.simplyblock.io
      exit 0
    fi
    [[ "$2" == crd && "${3:-}" == "$ops" ]] && exit 0
    [[ "$2" == crd && "${3:-}" != -* ]] && exit 1
    if [[ "$2" == deployment && "$args" == *" app=simplyblock-operator "* ]]; then
      [[ -f "$S/operator" ]] && echo simplyblock-operator
      exit 0
    fi
    if [[ "$2" == "$ops" ]]; then
      if [[ "$args" == *" -o json "* ]]; then echo '{"items":[]}'; exit 0; fi
      if [[ -f "$S/ops" ]]; then
        [[ "$args" == *" --no-headers "* ]] && echo "initial-discovery x" || echo initial-discovery
      fi
      exit 0
    fi
    [[ "$args" == *" -o json "* ]] && echo '{"items":[]}'
    exit 0
    ;;
  scale)
    [[ "$2" == deployment ]] && rm -f "$S/operator"
    exit 0
    ;;
  delete)
    if [[ "$2" == "$ops" && -f "$S/ops" ]]; then
      if [[ -f "$S/operator" ]]; then rm -f "$S/ops"; else touch "$S/ops-deleting"; fi
    fi
    exit 0
    ;;
  patch)
    if [[ "$2" == "$ops" && "$args" == *'"finalizers":[]'* && -f "$S/ops-deleting" ]]; then
      rm -f "$S/ops" "$S/ops-deleting"
    fi
    exit 0
    ;;
  wait)
    if [[ "$2" == "$ops" || "$2" == "$ops/"* ]]; then [[ ! -f "$S/ops" ]]; exit; fi
    exit 0
    ;;
esac
exit 0
EOF
  cat > "$dir/bin/helm" <<'EOF'
#!/usr/bin/env bash
S="$STATE_DIR"
case "$1" in
  list) [[ -f "$S/release" ]] && printf 'simplyblock-operator\tsimplyblock\t1\n'; exit 0 ;;
  uninstall)
    rm -f "$S/release"
    if [[ -f "$S/ops" ]]; then
      if [[ -f "$S/operator" ]]; then
        rm -f "$S/ops"
      else
        touch "$S/ops-deleting"
        echo "Error: uninstallation completed with 1 error(s): timed out waiting for the condition" >&2
        exit 1
      fi
    fi
    exit 0
    ;;
esac
exit 0
EOF
  chmod +x "$dir/bin/kubectl" "$dir/bin/helm"
  local out
  out=$(PATH="$dir/bin:$PATH" STATE_DIR="$dir/state" bash "$SCRIPT" -f simplyblock 2>&1)
  if grep -q "timed out waiting" <<< "$out" || ! grep -q "Cleanup complete" <<< "$out"; then
    echo "FAIL $name: expected 'Cleanup complete' and no helm wait timeout"
    echo "$out" | grep -E "ERROR|WARN|Error|timed out" | sed 's/^/    /'
    FAILED=1
  elif [[ -f "$dir/state/ops" ]]; then
    echo "FAIL $name: the OperatorOps is still there"
    FAILED=1
  else
    echo "ok   $name"
  fi
}
stateful_case

exit "$FAILED"
