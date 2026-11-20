#!/usr/bin/env bash
# Removes what install.sh put on a dev cluster: the NetworkAttachmentDefinitions,
# Multus, the plugin installer, and the node-side files (the generated CNI
# config, the shim, and the plugin binaries install.sh added). A plugin binary
# that was on a node before install.sh ran is left alone.
#
#     KUBECONFIG=~/.kube/config-61 hack/multus/uninstall.sh --yes
#
# Pods that were given a second interface keep it until they are recreated.

set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"

ASSUME_YES=0
for arg in "$@"; do
	case "$arg" in
	--yes) ASSUME_YES=1 ;;
	*) echo "unknown argument: $arg" >&2; exit 2 ;;
	esac
done

confirm

echo "pods that name a secondary network (they keep it until recreated):"
kubectl get pods -A -o json | python3 -I -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if "k8s.v1.cni.cncf.io/networks" in (p["metadata"].get("annotations") or {}):
        print("  %s/%s" % (p["metadata"]["namespace"], p["metadata"]["name"]))
'

kubectl delete network-attachment-definitions.k8s.cni.cncf.io -A --all --ignore-not-found 2>/dev/null || true
multus_manifest | kubectl delete --ignore-not-found -f -
kubectl -n kube-system wait --for=delete pod -l app=multus --timeout=180s || true

cleanup_manifest | kubectl apply -f -
kubectl -n kube-system rollout status ds/dev-multus-cleanup --timeout=300s
cleanup_manifest | kubectl delete --ignore-not-found -f -
plugins_manifest | kubectl delete --ignore-not-found -f -
echo "uninstalled."
