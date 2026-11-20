#!/usr/bin/env bash
# Collects everything needed to debug a failed cluster discovery, from the
# OperatorOps discover run through ClusterDeploymentConfig to cluster creation,
# into one zip. Lives in scripts/ because it is a support tool, not a build step.
#
# Usage: scripts/collect-discovery-logs.sh <operator-namespace> [output-dir]
set -uo pipefail

NS="${1:?usage: $0 <operator-namespace> [output-dir]}"
OUT_PARENT="${2:-.}"

# A mistyped namespace otherwise yields a bundle of empty files and no logs.
if ! kubectl get namespace "${NS}" >/dev/null 2>&1; then
  echo "namespace ${NS} not found in context $(kubectl config current-context 2>/dev/null)" >&2
  exit 1
fi
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
NAME="simplyblock-discovery-${NS}-${STAMP}"
DIR="${OUT_PARENT}/${NAME}"
mkdir -p "${DIR}"/{crs,nodes,configmaps,logs,pods,jobs}

# Probe objects carry these labels (operator/internal/nodeprobe/configmap.go).
PROBE_SEL="storage.simplyblock.io/component=nodeprobe"

run() { # run <outfile> <cmd...>: never abort the collection on one failure
  local out="$1" rc; shift
  printf '  %-60s ' "${out#"${DIR}"/}"
  { echo "# $*"; "$@"; } >"${out}" 2>&1
  rc=$?
  if [ "${rc}" -eq 0 ]; then echo ok; else echo "# exit ${rc}" >>"${out}"; echo "failed (exit ${rc})"; fi
}

echo "collecting into ${DIR}"

# Cluster context.
run "${DIR}/version.txt" kubectl version
run "${DIR}/api-resources-simplyblock.txt" kubectl api-resources --api-group=storage.simplyblock.io

# Discovery and deployment CRs. Missing kinds just produce an error line.
# Fully qualified, so a same-named CRD from another vendor is never picked up.
for kind in operatorops clusterdeploymentconfigs controlplanes storageclusters storageclusterops \
    storagenodesets storagenodes storagenodeops storagedevices storagepools simplyblockdrivers tasks; do
  res="${kind}.storage.simplyblock.io"
  run "${DIR}/crs/${kind}.txt"  kubectl -n "${NS}" get "${res}" -o wide
  run "${DIR}/crs/${kind}.yaml" kubectl -n "${NS}" get "${res}" -o yaml
  run "${DIR}/crs/${kind}-describe.txt" kubectl -n "${NS}" describe "${res}"
done

# Nodes: labels, taints, and capacity drive what discovery proposes.
run "${DIR}/nodes/nodes.txt"      kubectl get nodes -o wide --show-labels
run "${DIR}/nodes/nodes.yaml"     kubectl get nodes -o yaml
run "${DIR}/nodes/describe.txt"   kubectl describe nodes

# ConfigMaps: the full list, then every probe report in full.
run "${DIR}/configmaps/list.txt" kubectl -n "${NS}" get configmap --show-labels
run "${DIR}/configmaps/probe-reports.yaml" kubectl -n "${NS}" get configmap -l "${PROBE_SEL}" -o yaml
for cm in $(kubectl -n "${NS}" get configmap -l "${PROBE_SEL}" -o name 2>/dev/null); do
  run "${DIR}/configmaps/${cm#configmap/}.yaml" kubectl -n "${NS}" get "${cm}" -o yaml
done

# Probe Jobs and their pods; failed probes are the usual cause.
run "${DIR}/jobs/jobs.txt"  kubectl -n "${NS}" get jobs -o wide
run "${DIR}/jobs/jobs.yaml" kubectl -n "${NS}" get jobs -o yaml
run "${DIR}/jobs/describe.txt" kubectl -n "${NS}" describe jobs

# Namespace-wide state.
run "${DIR}/pods/pods.txt"     kubectl -n "${NS}" get pods -o wide
run "${DIR}/pods/describe.txt" kubectl -n "${NS}" describe pods
run "${DIR}/all.txt"           kubectl -n "${NS}" get all -o wide
run "${DIR}/events.txt"        kubectl -n "${NS}" get events --sort-by=.lastTimestamp
run "${DIR}/events-cluster.txt" kubectl get events -A --field-selector type=Warning --sort-by=.lastTimestamp
run "${DIR}/pvc.txt"           kubectl -n "${NS}" get pvc -o wide
run "${DIR}/storageclass.txt"  kubectl get storageclass
# Only clusters running Multus have this kind.
if kubectl api-resources -o name 2>/dev/null | grep -q '^network-attachment-definitions\.'; then
  run "${DIR}/network-attachment-definitions.yaml" kubectl -n "${NS}" get network-attachment-definitions -o yaml
fi
run "${DIR}/webhooks.txt" kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations
run "${DIR}/helm.txt" helm -n "${NS}" list --deployed --failed --pending --superseded --uninstalled --uninstalling

# Logs: every container of every pod, current and previous. This covers the
# operator, simplyblock-webappapi, each simplyblock-tasks container, and probe pods.
# One "<pod> <container>" pair per line, so nothing depends on word splitting.
kubectl -n "${NS}" get pods -o go-template='{{range .items}}{{$p := .metadata.name}}{{range .spec.initContainers}}{{$p}} {{.name}}{{"\n"}}{{end}}{{range .spec.containers}}{{$p}} {{.name}}{{"\n"}}{{end}}{{end}}' 2>/dev/null \
| while read -r pod c; do
    [ -n "${c}" ] || continue
    run "${DIR}/logs/${pod}__${c}.log" kubectl -n "${NS}" logs "${pod}" -c "${c}" --timestamps
    if kubectl -n "${NS}" logs "${pod}" -c "${c}" --previous --timestamps \
        >"${DIR}/logs/${pod}__${c}.previous.log" 2>/dev/null; then
      printf '  %-60s ok\n' "logs/${pod}__${c}.previous.log"
    else
      rm -f "${DIR}/logs/${pod}__${c}.previous.log"
    fi
  done

# Package.
if command -v zip >/dev/null 2>&1; then
  (cd "${OUT_PARENT}" && zip -qr "${NAME}.zip" "${NAME}") && rm -rf "${DIR}"
  echo "wrote ${OUT_PARENT}/${NAME}.zip"
else
  tar -C "${OUT_PARENT}" -czf "${OUT_PARENT}/${NAME}.tar.gz" "${NAME}" && rm -rf "${DIR}"
  echo "zip not found; wrote ${OUT_PARENT}/${NAME}.tar.gz"
fi
