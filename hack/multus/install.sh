#!/usr/bin/env bash
# Installs Multus and the CNI reference plugins on a dev cluster, so a
# NetworkAttachmentDefinition can hand a pod a second interface (design:
# moving the storage node off the host network). Undone by uninstall.sh.
#
#     KUBECONFIG=~/.kube/config-61 hack/multus/install.sh --yes
#     hack/multus/install.sh --print     # the manifests, applying nothing
#
# This changes every node: it copies plugin binaries into CNI_BIN_DIR and
# starts a Multus that writes a CNI config ahead of the existing one. It is
# for dev clusters. Run it where nothing else is being created.

set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"

ASSUME_YES=0
PRINT=0
for arg in "$@"; do
	case "$arg" in
	--yes) ASSUME_YES=1 ;;
	--print) PRINT=1 ;;
	*) echo "unknown argument: $arg" >&2; exit 2 ;;
	esac
done

if [ "$PRINT" = 1 ]; then
	plugins_manifest
	echo ---
	multus_manifest
	exit 0
fi

confirm
plugins_manifest | kubectl apply -f -
kubectl -n kube-system rollout status ds/dev-cni-plugins --timeout=300s
multus_manifest | kubectl apply -f -
kubectl -n kube-system rollout status ds/kube-multus-ds --timeout=300s
kubectl get crd network-attachment-definitions.k8s.cni.cncf.io
echo "installed. create a NetworkAttachmentDefinition, then annotate a pod with k8s.v1.cni.cncf.io/networks"
