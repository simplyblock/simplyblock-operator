#!/usr/bin/env bash
# Creates, prepares, and removes an OpenShift cluster on AWS for the
# certification runs of ../level1, ../level2, and ../level3: an
# installer-provisioned cluster on metal workers, OpenShift Virtualization on
# it, and simplyblock installed from this checkout's Helm chart.
#
# Every step is a separate command, so a CI job can run them in order and a
# person can resume after a failure:
#
#   ./cluster.sh create               download the tools, render the config, install OpenShift
#   ./cluster.sh wait                 wait for an install that timed out to finish
#   ./cluster.sh install-virt         OpenShift Virtualization from the Red Hat catalog
#   ./cluster.sh install-simplyblock  simplyblock from helm-charts/charts/simplyblock-operator
#   ./cluster.sh status               the cluster's version, nodes, and operators
#   ./cluster.sh kubeconfig           print the path of the cluster's kubeconfig
#   ./cluster.sh destroy              remove the cluster and everything the installer created
#
# State lives in clusters/<CLUSTER_NAME>/ next to this script: the installer's
# directory (with auth/kubeconfig and the metadata destroy needs) and a copy of
# the configuration without the pull secret. Losing that directory strands the
# cluster's AWS resources, so it is kept until destroy has run. The tools are
# downloaded once per OpenShift version, into bin/<version>/.
#
# AWS credentials come from the environment or ~/.aws, as for the AWS CLI. The
# Route 53 public zone of BASE_DOMAIN has to exist in the account.
#
# Environment:
#   PULL_SECRET_FILE        Red Hat pull secret, JSON (required by create)
#   CLUSTER_NAME            default sb-cert
#   OCP_VERSION             a release (4.20.26, 5.0.1) or a channel (stable-4.20); default stable-4.20
#   BASE_DOMAIN             default api.simplyblock.io
#   REGION, ZONE            default us-east-1, us-east-1a
#   CONTROL_PLANE_TYPE      default m6i.2xlarge (Level 3 asks for 8 vCPU and 32 GB)
#   CONTROL_PLANE_REPLICAS  default 3
#   WORKER_TYPE             default c6id.metal (64 cores, 256 GB, 4 NVMe drives); m6id.metal has 512 GB
#   WORKER_REPLICAS         default 3
#   SSH_KEY_FILE            public key for the nodes (default ~/.ssh/id_ed25519.pub)
#   OWNER                   the owner tag on every AWS resource (default $USER)
#   EXPIRES                 the expiry tag, a date (default three days from now); nothing acts on it
#   VIRT_CHANNEL            OpenShift Virtualization channel (default stable)
#   OPERATOR_IMAGE          simplyblock operator image (default: the branch build of this checkout)
#   SIMPLYBLOCK_IMAGE       simplyblock control plane image (default public.ecr.aws/simply-block/simplyblock:main)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"

CLUSTER_NAME="${CLUSTER_NAME:-sb-cert}"
OCP_VERSION="${OCP_VERSION:-stable-4.20}"
BASE_DOMAIN="${BASE_DOMAIN:-api.simplyblock.io}"
REGION="${REGION:-us-east-1}"
ZONE="${ZONE:-us-east-1a}"
CONTROL_PLANE_TYPE="${CONTROL_PLANE_TYPE:-m6i.2xlarge}"
CONTROL_PLANE_REPLICAS="${CONTROL_PLANE_REPLICAS:-3}"
WORKER_TYPE="${WORKER_TYPE:-c6id.metal}"
WORKER_REPLICAS="${WORKER_REPLICAS:-3}"
SSH_KEY_FILE="${SSH_KEY_FILE:-$HOME/.ssh/id_ed25519.pub}"
OWNER="${OWNER:-${USER:-unknown}}"
VIRT_CHANNEL="${VIRT_CHANNEL:-stable}"

STATE_DIR="$SCRIPT_DIR/clusters/$CLUSTER_NAME"
INSTALL_DIR="$STATE_DIR/install"
KUBECONFIG_PATH="$INSTALL_DIR/auth/kubeconfig"
POLL_SECONDS=20

die() {
  echo "$*" >&2
  exit 1
}

# The tools of the cluster's OpenShift version. A channel resolves to a release
# when it is downloaded, and the release is recorded so later steps use the
# same binaries.
tools_dir() {
  local version="$OCP_VERSION"
  [[ -r "$STATE_DIR/ocp-version" ]] && version="$(cat "$STATE_DIR/ocp-version")"
  echo "$SCRIPT_DIR/bin/$version"
}

download_tools() {
  local suffix base file_version tool archive tmp release
  case "$(uname -s)-$(uname -m)" in
    Darwin-arm64) suffix=mac-arm64 ;;
    Darwin-x86_64) suffix=mac ;;
    Linux-x86_64) suffix=linux ;;
    Linux-aarch64) suffix=linux-arm64 ;;
    *) die "unsupported platform $(uname -s)-$(uname -m)" ;;
  esac
  base="https://mirror.openshift.com/pub/openshift-v4/clients/ocp/$OCP_VERSION"
  # A channel directory names its archives without a version, a release
  # directory with one.
  file_version="-$OCP_VERSION"
  [[ "$OCP_VERSION" == stable-* || "$OCP_VERSION" == fast-* || "$OCP_VERSION" == candidate-* ]] && file_version=""

  tmp="$(mktemp -d)"
  for tool in openshift-install openshift-client; do
    archive="$tool-$suffix$file_version.tar.gz"
    echo "downloading $base/$archive"
    curl -fsSL "$base/$archive" | tar -xz -C "$tmp"
  done
  release="$("$tmp/openshift-install" version | awk '/^openshift-install/ {print $2; exit}')"
  [[ -n "$release" ]] || die "could not read the release of the downloaded openshift-install"
  mkdir -p "$SCRIPT_DIR/bin/$release"
  mv "$tmp/openshift-install" "$tmp/oc" "$SCRIPT_DIR/bin/$release/"
  rm -rf "$tmp"
  mkdir -p "$STATE_DIR"
  echo "$release" >"$STATE_DIR/ocp-version"
  echo "OpenShift $release"
}

installer() { "$(tools_dir)/openshift-install" "$@"; }
oc_() { KUBECONFIG="$KUBECONFIG_PATH" "$(tools_dir)/oc" "$@"; }

require_cluster() {
  [[ -r "$KUBECONFIG_PATH" ]] || die "no cluster $CLUSTER_NAME: $KUBECONFIG_PATH does not exist (run create first)"
}

# Writes install-config.yaml with the pull secret, which openshift-install
# consumes (deletes) when it creates the manifests, and a copy without it for
# the record. Neither secret is passed on a command line.
render_config() {
  : "${PULL_SECRET_FILE:?set PULL_SECRET_FILE to the Red Hat pull secret (JSON)}"
  [[ -r "$SSH_KEY_FILE" ]] || die "SSH_KEY_FILE $SSH_KEY_FILE is not readable"
  local expires="${EXPIRES:-}"
  if [[ -z "$expires" ]]; then
    expires="$(date -u -v+3d +%Y-%m-%d 2>/dev/null || date -u -d '+3 days' +%Y-%m-%d)"
  fi
  mkdir -p "$INSTALL_DIR"
  (
    umask 077
    TEMPLATE="$SCRIPT_DIR/install-config.template.yaml" OUT_DIR="$INSTALL_DIR" RECORD="$STATE_DIR/install-config.public.yaml" \
      PULL_SECRET_FILE="$PULL_SECRET_FILE" SSH_KEY_FILE="$SSH_KEY_FILE" \
      CLUSTER_NAME="$CLUSTER_NAME" BASE_DOMAIN="$BASE_DOMAIN" REGION="$REGION" ZONE="$ZONE" \
      CONTROL_PLANE_TYPE="$CONTROL_PLANE_TYPE" CONTROL_PLANE_REPLICAS="$CONTROL_PLANE_REPLICAS" \
      WORKER_TYPE="$WORKER_TYPE" WORKER_REPLICAS="$WORKER_REPLICAS" OWNER="$OWNER" EXPIRES="$expires" \
      python3 -I - <<'EOF'
import json, os, sys
env = os.environ
try:
    secret = json.load(open(env["PULL_SECRET_FILE"]))
except (OSError, ValueError) as e:
    sys.exit(f"the pull secret is not readable JSON: {e.__class__.__name__}")
auths = secret.get("auths", {})
missing = [r for r in ("registry.redhat.io", "quay.io") if r not in auths]
if missing:
    sys.exit("the pull secret has no credentials for " + ", ".join(missing))
t = open(env["TEMPLATE"]).read()
for key in ("CLUSTER_NAME", "BASE_DOMAIN", "REGION", "ZONE", "CONTROL_PLANE_TYPE", "CONTROL_PLANE_REPLICAS",
            "WORKER_TYPE", "WORKER_REPLICAS", "OWNER", "EXPIRES"):
    t = t.replace(f"__{key}__", env[key])
t = t.replace("__SSH_KEY__", open(env["SSH_KEY_FILE"]).read().strip())
open(env["RECORD"], "w").write(t.replace("__PULL_SECRET__", "(not recorded)"))
open(os.path.join(env["OUT_DIR"], "install-config.yaml"), "w").write(
    t.replace("__PULL_SECRET__", json.dumps(secret, separators=(",", ":"))))
EOF
  )
}

cmd_create() {
  [[ -e "$INSTALL_DIR/metadata.json" ]] &&
    die "$STATE_DIR already holds cluster $CLUSTER_NAME; destroy it first, or set CLUSTER_NAME"
  [[ -x "$(tools_dir)/openshift-install" ]] || download_tools
  render_config
  echo "creating $CLUSTER_NAME.$BASE_DOMAIN: OpenShift $(cat "$STATE_DIR/ocp-version")," \
    "$CONTROL_PLANE_REPLICAS x $CONTROL_PLANE_TYPE control plane, $WORKER_REPLICAS x $WORKER_TYPE workers, $ZONE"
  installer create cluster --dir "$INSTALL_DIR" --log-level=info
  echo "kubeconfig: $KUBECONFIG_PATH"
}

cmd_wait() {
  require_cluster
  installer wait-for install-complete --dir "$INSTALL_DIR" --log-level=info
}

# Waits until a command prints something, or gives up after a deadline.
wait_for() {
  local what="$1" seconds="$2"
  shift 2
  local deadline=$((SECONDS + seconds))
  until [[ -n "$("$@" 2>/dev/null || true)" ]]; do
    [[ $SECONDS -lt $deadline ]] || die "gave up waiting for $what after ${seconds}s"
    sleep "$POLL_SECONDS"
  done
}

# The OpenShift Virtualization operator's CSV, once it has succeeded.
hco_csv() {
  oc_ get csv -n openshift-cnv \
    -o jsonpath='{range .items[?(@.status.phase=="Succeeded")]}{.metadata.name}{"\n"}{end}' |
    grep kubevirt-hyperconverged
}

cmd_install_virt() {
  require_cluster
  oc_ apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-cnv
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kubevirt-hyperconverged-group
  namespace: openshift-cnv
spec:
  targetNamespaces:
  - openshift-cnv
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: hco-operatorhub
  namespace: openshift-cnv
spec:
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  name: kubevirt-hyperconverged
  channel: $VIRT_CHANNEL
  installPlanApproval: Automatic
EOF
  echo "waiting for the OpenShift Virtualization operator"
  wait_for "the kubevirt-hyperconverged CSV" 1200 hco_csv
  oc_ apply -f - <<EOF
apiVersion: hco.kubevirt.io/v1beta1
kind: HyperConverged
metadata:
  name: kubevirt-hyperconverged
  namespace: openshift-cnv
spec: {}
EOF
  oc_ wait hyperconverged/kubevirt-hyperconverged -n openshift-cnv --for=condition=Available --timeout=30m
  echo "OpenShift Virtualization $(oc_ get -n openshift-cnv "csv/$(hco_csv | head -1)" -o jsonpath='{.spec.version}') is available"
}

# simplyblock from this checkout's chart. The chart's snapshot controller stays
# off: OpenShift runs its own, and a second one is a finding of the Level 1
# monitor tests.
cmd_install_simplyblock() {
  require_cluster
  command -v helm >/dev/null || die "helm is not on PATH"
  local branch operator_image simplyblock_image
  branch="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD | tr '/' '-')"
  operator_image="${OPERATOR_IMAGE:-registry.simplyblock.ai/simplyblock/simplyblock-operator:$branch}"
  simplyblock_image="${SIMPLYBLOCK_IMAGE:-public.ecr.aws/simply-block/simplyblock:main}"
  echo "simplyblock: operator $operator_image, control plane $simplyblock_image"
  KUBECONFIG="$KUBECONFIG_PATH" helm upgrade --install simplyblock-operator \
    "$REPO_ROOT/helm-charts/charts/simplyblock-operator" \
    --namespace simplyblock --create-namespace \
    --set deployment.profile=standalone \
    --set image.operator.repository="${operator_image%:*}" \
    --set image.operator.tag="${operator_image##*:}" \
    --set image.simplyblock.repository="${simplyblock_image%:*}" \
    --set image.simplyblock.tag="${simplyblock_image##*:}" \
    --set snapshotcontroller.create=false \
    --set controlplane.observability.enabled=false
}

cmd_status() {
  require_cluster
  oc_ get clusterversion
  oc_ get nodes -o wide
  oc_ get clusteroperators | awk 'NR == 1 || $3 != "True" || $4 != "False" || $5 != "False"'
}

cmd_destroy() {
  [[ -e "$INSTALL_DIR/metadata.json" ]] || die "no cluster $CLUSTER_NAME in $STATE_DIR"
  installer destroy cluster --dir "$INSTALL_DIR" --log-level=info
  rm -rf "$STATE_DIR"
  echo "cluster $CLUSTER_NAME removed"
}

case "${1:-}" in
  create) cmd_create ;;
  wait) cmd_wait ;;
  install-virt) cmd_install_virt ;;
  install-simplyblock) cmd_install_simplyblock ;;
  status) cmd_status ;;
  kubeconfig) require_cluster && echo "$KUBECONFIG_PATH" ;;
  destroy) cmd_destroy ;;
  *)
    sed -n '/^#   \.\/cluster\.sh/s/^#   //p' "$0" >&2
    exit 2
    ;;
esac
