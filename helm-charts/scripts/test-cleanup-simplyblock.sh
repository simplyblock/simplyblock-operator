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

exit "$FAILED"
