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
  list)
    if [[ -f "$S/release" ]]; then
      echo '[{"name":"simplyblock","namespace":"simplyblock","chart":"simplyblock-operator-1.0.0"}]'
    else
      echo '[]'
    fi
    exit 0
    ;;
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

# Regression: the chart's VolumeSnapshotClass CRD survived cleanup on k3s
# (2026-11-09). Step 5 kept it as in use because the operator's own
# VolumeSnapshotClass, deleted only later in step 5a, was still an instance.
snapshot_class_case() {
  local name="own VolumeSnapshotClass does not keep the snapshot CRD"
  local dir="$WORK/snapclass"
  mkdir -p "$dir/bin" "$dir/state"
  touch "$dir/state/vsc"
  cat > "$dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
S="$STATE_DIR"
vsc=volumesnapshotclasses.snapshot.storage.k8s.io
args=" $* "
case "$1" in
  get)
    if [[ "$2" == crd && "${3:-}" == "$vsc" ]]; then
      [[ -f "$S/crd-deleted" ]] && exit 1
      [[ "$args" == *release-namespace* ]] && echo simplyblock
      exit 0
    fi
    if [[ "$*" == "get crd -o name" ]]; then
      echo customresourcedefinition.apiextensions.k8s.io/storageclusters.storage.simplyblock.io
      exit 0
    fi
    [[ "$2" == crd && "${3:-}" != -* ]] && exit 1
    if [[ "$2" == "$vsc" && "$args" == *" --no-headers "* ]]; then
      [[ -f "$S/vsc" ]] && echo "simplyblock-snapshots csi.simplyblock.io Delete 1m"
      exit 0
    fi
    [[ "$args" == *" -o json "* ]] && echo '{"items":[]}'
    exit 0
    ;;
  delete)
    [[ "$2" == volumesnapshotclass && "$args" == *" -l "* ]] && rm -f "$S/vsc"
    [[ "$2" == crd && "$3" == "$vsc" ]] && touch "$S/crd-deleted"
    exit 0
    ;;
esac
exit 0
EOF
  # shellcheck disable=SC2016
  printf '#!/usr/bin/env bash\n[[ "$1" == list ]] && echo "[]"\nexit 0\n' > "$dir/bin/helm"
  chmod +x "$dir/bin/kubectl" "$dir/bin/helm"
  PATH="$dir/bin:$PATH" STATE_DIR="$dir/state" bash "$SCRIPT" -f simplyblock >/dev/null 2>&1
  if [[ -f "$dir/state/crd-deleted" ]]; then
    echo "ok   $name"
  else
    echo "FAIL $name: the volumesnapshotclasses CRD was kept"
    FAILED=1
  fi
}
snapshot_class_case

# Regression: the storage node Jobs (CPU topology, core isolation, and the rest
# sbcli renders) survived step 3, which deleted their pods but not the Jobs.
# A Job still running gets its pod back as soon as that pod is deleted, so the
# Jobs go first, and the pods after them.
job_case() {
  local name="storage node Jobs are deleted before their pods"
  local dir="$WORK/jobs"
  mkdir -p "$dir/bin" "$dir/state"
  touch "$dir/state/job"
  cat > "$dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
S="$STATE_DIR"
args=" $* "
case "$1" in
  get)
    if [[ "$*" == "get crd -o name" ]]; then
      echo customresourcedefinition.apiextensions.k8s.io/storageclusters.storage.simplyblock.io
      exit 0
    fi
    [[ "$2" == crd && "${3:-}" != -* ]] && exit 1
    if [[ "$2" == job && "$args" == *" --no-headers "* ]]; then
      [[ -f "$S/job" ]] && echo "snode-cpu-topology-worker-1 Running 0/1 5m"
      exit 0
    fi
    [[ "$args" == *" -o json "* ]] && echo '{"items":[]}'
    exit 0
    ;;
  delete)
    if [[ "$2" == job ]]; then rm -f "$S/job"; fi
    if [[ "$2" == pod && -f "$S/job" ]]; then touch "$S/pod-recreated"; fi
    exit 0
    ;;
esac
exit 0
EOF
  # shellcheck disable=SC2016
  printf '#!/usr/bin/env bash\n[[ "$1" == list ]] && echo "[]"\nexit 0\n' > "$dir/bin/helm"
  chmod +x "$dir/bin/kubectl" "$dir/bin/helm"
  PATH="$dir/bin:$PATH" STATE_DIR="$dir/state" bash "$SCRIPT" -f simplyblock >/dev/null 2>&1
  if [[ -f "$dir/state/job" ]]; then
    echo "FAIL $name: the Job is still there"
    FAILED=1
  elif [[ -f "$dir/state/pod-recreated" ]]; then
    echo "FAIL $name: the pods were deleted while the Job could still recreate them"
    FAILED=1
  else
    echo "ok   $name"
  fi
}
job_case

# The CPU topology job gives a storage cluster's nodes their kubelet settings on
# OpenShift through a MachineConfigPool, a MachineConfig, and KubeletConfigs named
# after the cluster's UUID, and MCO generates kubelet configs for the pool.
# Cleanup removes this namespace's cluster's, and a previous cluster's that an
# earlier cleanup left, but never those of a cluster that still exists in another
# namespace, and never those of a pool that still has nodes: moving them out of it
# reboots them, and deleting it deletes the rendered config they run.
#
# The stub keeps one file per object, `<mcp|mc|kc>.<name>`, and `node.<id>` for a
# node running pool storage-<id>'s render.
mco_stub() {
  local dir="$1"
  mkdir -p "$dir/bin"
  cat > "$dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
S="$STATE_DIR"
args=" $* "
short() { case "$1" in machineconfigpool*|mcp) echo mcp ;; machineconfig*|mc) echo mc ;; kubeletconfig*) echo kc ;; esac; }
case "$1" in
  get)
    if [[ "$*" == "get crd -o name" ]]; then
      echo customresourcedefinition.apiextensions.k8s.io/storageclusters.storage.simplyblock.io
      exit 0
    fi
    [[ "$2" == crd && "${3:-}" == machineconfigpools.machineconfiguration.openshift.io ]] && exit 0
    [[ "$2" == crd && "${3:-}" == storageclusters.storage.simplyblock.io ]] && exit 0
    [[ "$2" == crd && "${3:-}" != -* ]] && exit 1
    if [[ "$2" == storageclusters.* && "$args" == *uuid* ]]; then
      [[ -n "${OTHERS_FAIL:-}" && "$args" == *" -A "* ]] && exit 1
      if [[ "$args" == *" -A "* ]]; then
        echo "simplyblock abc123de-0000-4000-8000-000000000001"
        echo "other ffffffde-0000-4000-8000-000000000002"
      else
        echo abc123de-0000-4000-8000-000000000001
      fi
      exit 0
    fi
    if [[ "$2" == nodes && "$args" == *" -o json "* ]]; then
      items=""
      for f in "$S"/node.*; do
        [[ -e "$f" ]] || continue
        cfg="rendered-storage-${f##*/node.}-9f"
        items="$items${items:+,}{\"metadata\":{\"name\":\"n-${f##*/node.}\",\"annotations\":{\"machineconfiguration.openshift.io/currentConfig\":\"$cfg\",\"machineconfiguration.openshift.io/desiredConfig\":\"$cfg\"}}}"
      done
      echo "{\"items\":[$items]}"
      exit 0
    fi
    kind=$(short "$2")
    if [[ "$kind" == mcp && "$args" == *" -o json "* ]]; then
      items=""
      for f in "$S"/mcp.*; do
        [[ -e "$f" ]] || continue
        n=${f##*/mcp.}
        items="$items${items:+,}{\"metadata\":{\"name\":\"$n\",\"labels\":{\"custom-kubelet\":\"storage-cpumanager-${n#storage-}\"}}}"
      done
      echo "{\"items\":[$items]}"
      exit 0
    fi
    if [[ "$kind" == mc && "$args" == *" -o name "* ]]; then
      for f in "$S"/mc.*; do [[ -e "$f" ]] && echo "machineconfig.machineconfiguration.openshift.io/${f##*/mc.}"; done
      exit 0
    fi
    if [[ "$kind" == kc && "$args" == *" -o name "* ]]; then
      for f in "$S"/kc.*; do [[ -e "$f" ]] && echo "kubeletconfig.machineconfiguration.openshift.io/${f##*/kc.}"; done
      exit 0
    fi
    if [[ "$kind" == mc && "${3:-}" == simplyblock-master-isolated ]]; then
      [[ -f "$S/mc.simplyblock-master-isolated" ]] || exit 1
      [[ "$args" == *cluster-id* ]] && echo abc123de-0000-4000-8000-000000000001
      exit 0
    fi
    if [[ -n "$kind" && -n "${3:-}" && "${3:-}" != -* ]]; then
      [[ -f "$S/$kind.$3" ]] || exit 1
      if [[ "$args" == *machineCount* ]]; then
        [[ -f "$S/node.${3#storage-}" ]] && echo 1 || echo 0
      fi
      exit 0
    fi
    [[ "$args" == *" -o json "* ]] && echo '{"items":[]}'
    exit 0
    ;;
  patch)
    [[ "$(short "$2")" == mcp ]] && touch "$S/pool-patched"
    exit 0
    ;;
  delete)
    kind=$(short "$2")
    [[ -z "$kind" ]] && exit 0
    shift 2
    for n in "$@"; do
      [[ "$n" == -* ]] && continue
      if [[ "$kind" == mcp && -f "$S/node.${n#storage-}" ]]; then touch "$S/pool-deleted-under-node"; fi
      rm -f "$S/$kind.$n"
    done
    exit 0
    ;;
esac
exit 0
EOF
  # shellcheck disable=SC2016
  printf '#!/usr/bin/env bash\n[[ "$1" == list ]] && echo "[]"\nexit 0\n' > "$dir/bin/helm"
  chmod +x "$dir/bin/kubectl" "$dir/bin/helm"
}

# Objects of a cluster with short ID $1, as stub state file names.
mco_objects() {
  echo "mcp.storage-$1 mc.storage-mc-$1 kc.storage-kubelet-$1 kc.storage-kubelet-$1-master" \
       "mc.97-storage-$1-generated-kubelet mc.98-storage-$1-generated-kubelet mc.99-storage-$1-generated-kubelet"
}

machine_config_case() {
  local name="MachineConfig objects of clusters gone and of pools without nodes go, others stay"
  local dir="$WORK/mco"
  mco_stub "$dir"
  mkdir -p "$dir/state"
  # abc123 is this namespace's, ffffff a live cluster's in another namespace,
  # 103f18 a previous cluster's whose pool still has a node, and 5b5b5b a
  # previous cluster's whose pool has none.
  # shellcheck disable=SC2046
  for o in $(mco_objects abc123) $(mco_objects ffffff) $(mco_objects 103f18) $(mco_objects 5b5b5b) \
           mc.simplyblock-master-isolated node.103f18 mc.99-worker-generated-kubelet \
           mc.97-storage-72ec82-generated-kubelet mc.98-storage-72ec82-generated-kubelet; do
    touch "$dir/state/$o"
  done
  PATH="$dir/bin:$PATH" STATE_DIR="$dir/state" bash "$SCRIPT" -f simplyblock >/dev/null 2>&1
  local left="" gone=""
  # 72ec82's pool went in an earlier cleanup, and MCO kept its generated configs.
  for o in $(mco_objects abc123) $(mco_objects 5b5b5b) mc.simplyblock-master-isolated \
           mc.97-storage-72ec82-generated-kubelet mc.98-storage-72ec82-generated-kubelet; do
    [[ -f "$dir/state/$o" ]] && left="$left $o"
  done
  for o in $(mco_objects ffffff) $(mco_objects 103f18) mc.99-worker-generated-kubelet; do
    [[ -f "$dir/state/$o" ]] || gone="$gone $o"
  done
  if [[ -n "$left" ]]; then
    echo "FAIL $name: left$left"
    FAILED=1
  elif [[ -n "$gone" ]]; then
    echo "FAIL $name: deleted$gone"
    FAILED=1
  elif [[ -f "$dir/state/pool-patched" ]]; then
    echo "FAIL $name: a pool's node selector was changed, which moves its nodes"
    FAILED=1
  else
    echo "ok   $name"
  fi
}
machine_config_case

# This namespace's own pool is kept as well while it has nodes.
own_pool_with_nodes_case() {
  local name="this namespace's pool with nodes is kept"
  local dir="$WORK/mco-own"
  mco_stub "$dir"
  mkdir -p "$dir/state"
  # shellcheck disable=SC2046
  for o in $(mco_objects abc123) node.abc123; do touch "$dir/state/$o"; done
  PATH="$dir/bin:$PATH" STATE_DIR="$dir/state" bash "$SCRIPT" -f simplyblock >/dev/null 2>&1
  local gone=""
  for o in $(mco_objects abc123); do [[ -f "$dir/state/$o" ]] || gone="$gone $o"; done
  if [[ -n "$gone" ]]; then
    echo "FAIL $name: deleted$gone"
    FAILED=1
  elif [[ -f "$dir/state/pool-patched" ]]; then
    echo "FAIL $name: the pool's node selector was changed"
    FAILED=1
  else
    echo "ok   $name"
  fi
}
own_pool_with_nodes_case

# When the other namespaces' storage clusters cannot be listed, a pool whose
# cluster is not in this namespace may be a live cluster's, so only this
# namespace's go.
unknown_others_case() {
  local name="only this namespace's go when other namespaces cannot be listed"
  local dir="$WORK/mco-unknown"
  mco_stub "$dir"
  mkdir -p "$dir/state"
  # shellcheck disable=SC2046
  for o in $(mco_objects abc123) $(mco_objects 103f18); do touch "$dir/state/$o"; done
  PATH="$dir/bin:$PATH" STATE_DIR="$dir/state" OTHERS_FAIL=1 bash "$SCRIPT" -f simplyblock >/dev/null 2>&1
  local left="" gone=""
  for o in $(mco_objects abc123); do [[ -f "$dir/state/$o" ]] && left="$left $o"; done
  for o in $(mco_objects 103f18); do [[ -f "$dir/state/$o" ]] || gone="$gone $o"; done
  if [[ -n "$left" ]]; then
    echo "FAIL $name: left$left"
    FAILED=1
  elif [[ -n "$gone" ]]; then
    echo "FAIL $name: deleted$gone"
    FAILED=1
  else
    echo "ok   $name"
  fi
}
unknown_others_case

exit "$FAILED"
