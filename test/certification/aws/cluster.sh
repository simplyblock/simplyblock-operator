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
#   ./cluster.sh uninstall-simplyblock  remove simplyblock and its storage cluster, for a rebuild
#   ./cluster.sh status               the cluster's version, nodes, and operators
#   ./cluster.sh kubeconfig           print the path of the cluster's kubeconfig
#   ./cluster.sh destroy              remove the cluster and everything the installer created
#
# Every step logs to logs/<CLUSTER_NAME>/<step>-<UTC time>.log next to this
# script, and the steps that run openshift-install also keep its debug log there
# (openshift-install-<UTC time>.log). The logs outlive destroy, and the
# kubeadmin password the installer prints is redacted in them.
#
# State lives in clusters/<CLUSTER_NAME>/ next to this script: the installer's
# directory (with auth/kubeconfig and the metadata destroy needs) and a copy of
# the configuration without the pull secret. Losing that directory strands the
# cluster's AWS resources, so it is kept until destroy has run. The tools are
# downloaded once per OpenShift version, into bin/<version>/.
#
# AWS credentials come from the environment or ~/.aws, as for the AWS CLI. The
# Route 53 public zone of BASE_DOMAIN has to exist in the account, and the
# parent domain has to delegate to it: the installer reaches the new cluster's
# API by its public name, so a zone nobody delegates to ends the install at
# "no such host".
#
# Environment:
#   PULL_SECRET_FILE        Red Hat pull secret, JSON (required by create)
#   CLUSTER_NAME            default sb-cert
#   OCP_VERSION             a release (4.20.26, 5.0.1) or a channel (stable-4.22); default stable-4.22
#   BASE_DOMAIN             default api.ocp.cert.simplyblock.ai
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
#   APPROVE_DRAFT           true approves the discovery draft and waits for an active
#                           storage cluster; false stops at the draft for a review (default true)
#   STORAGE_WAIT            seconds to wait for the storage cluster to go active (default 7200)
#   MAX_SUBSYSTEM_COUNT     NVMe-oF subsystems, so volumes, per storage node, set in the draft
#                           (default 128: the 260 volumes of the LUN stress test on three nodes)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"

CLUSTER_NAME="${CLUSTER_NAME:-sb-cert}"
OCP_VERSION="${OCP_VERSION:-stable-4.22}"
BASE_DOMAIN="${BASE_DOMAIN:-api.ocp.cert.simplyblock.ai}"
REGION="${REGION:-us-east-1}"
ZONE="${ZONE:-us-east-1a}"
CONTROL_PLANE_TYPE="${CONTROL_PLANE_TYPE:-m6i.2xlarge}"
CONTROL_PLANE_REPLICAS="${CONTROL_PLANE_REPLICAS:-3}"
WORKER_TYPE="${WORKER_TYPE:-c6id.metal}"
WORKER_REPLICAS="${WORKER_REPLICAS:-3}"
SSH_KEY_FILE="${SSH_KEY_FILE:-$HOME/.ssh/id_ed25519.pub}"
OWNER="${OWNER:-${USER:-unknown}}"
VIRT_CHANNEL="${VIRT_CHANNEL:-stable}"
APPROVE_DRAFT="${APPROVE_DRAFT:-true}"
STORAGE_WAIT="${STORAGE_WAIT:-7200}"
MAX_SUBSYSTEM_COUNT="${MAX_SUBSYSTEM_COUNT:-128}"

STATE_DIR="$SCRIPT_DIR/clusters/$CLUSTER_NAME"
INSTALL_DIR="$STATE_DIR/install"
KUBECONFIG_PATH="$INSTALL_DIR/auth/kubeconfig"
LOG_DIR="$SCRIPT_DIR/logs/$CLUSTER_NAME"
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
  # The first line is "<path of the binary> <release>".
  release="$("$tmp/openshift-install" version | awk 'NR == 1 {print $2}')"
  if [[ -z "$release" ]]; then
    rm -rf "$tmp"
    die "could not read the release of the downloaded openshift-install"
  fi
  mkdir -p "$SCRIPT_DIR/bin/$release"
  mv "$tmp/openshift-install" "$tmp/oc" "$SCRIPT_DIR/bin/$release/"
  rm -rf "$tmp"
  mkdir -p "$STATE_DIR"
  echo "$release" >"$STATE_DIR/ocp-version"
  echo "OpenShift $release"
}

# The installer prints the kubeadmin password at the end of an install; the
# password stays in auth/kubeadmin-password and out of the logs.
redact() {
  perl -pe 'BEGIN { $| = 1 } s/(password:?\s*\\?"?)[^\s"\\]+/$1(redacted)/gi'
}

# Sends the rest of the step's output to a log file as well as the terminal.
start_log() {
  local log
  mkdir -p "$LOG_DIR"
  log="$LOG_DIR/$1-$(date -u +%Y%m%dT%H%M%SZ).log"
  exec > >(redact | tee -a "$log") 2>&1
  echo "log: $log"
}

# Copies openshift-install's own log, which is at debug level and far more
# detailed than its terminal output, next to the step logs.
keep_installer_log() {
  local src="$INSTALL_DIR/.openshift_install.log"
  [[ -r "$src" ]] || return 0
  mkdir -p "$LOG_DIR"
  redact <"$src" >"$LOG_DIR/openshift-install-$(date -u +%Y%m%dT%H%M%SZ).log"
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

# Refuses a base domain that public DNS does not delegate. Route 53 answers for
# a zone only through the name servers the parent's NS records name, and the
# installer finds out an hour later, as "no such host" for the API.
check_delegation() {
  if ! command -v dig >/dev/null; then
    echo "dig is not on PATH; not checking that $BASE_DOMAIN is delegated" >&2
    return 0
  fi
  [[ -n "$(dig +short NS "$BASE_DOMAIN" @1.1.1.1)" ]] ||
    die "public DNS resolves no NS records for $BASE_DOMAIN: the parent has to delegate it," \
      "and its Route 53 zone has to exist on the name servers the delegation names"
}

cmd_create() {
  [[ -e "$INSTALL_DIR/metadata.json" ]] &&
    die "$STATE_DIR already holds cluster $CLUSTER_NAME; destroy it first, or set CLUSTER_NAME"
  check_delegation
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

# Opens TCP between the cluster's nodes. The installer's node security group
# allows only OpenShift's own ports (the kubelet, 9000-9999, the node ports,
# the overlay), and simplyblock needs more between its nodes: the SPDK proxy
# the control plane calls on the worker's address, and the NVMe-oF listeners of
# every storage node, which other storage nodes and the clients connect to. The
# rule references the group itself, so nothing outside the cluster gains
# access, and destroy removes it with the group.
open_storage_traffic() {
  command -v aws >/dev/null || die "the AWS CLI is not on PATH"
  local infra group out
  infra="$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["infraID"])' "$INSTALL_DIR/metadata.json")"
  group="$(aws ec2 describe-security-groups --region "$REGION" \
    --filters "Name=group-name,Values=$infra-node" --query 'SecurityGroups[0].GroupId' --output text)"
  [[ "$group" == sg-* ]] || die "no security group $infra-node in $REGION"
  if out="$(aws ec2 authorize-security-group-ingress --region "$REGION" --group-id "$group" \
    --ip-permissions "IpProtocol=tcp,FromPort=0,ToPort=65535,UserIdGroupPairs=[{GroupId=$group,Description=simplyblock storage traffic between nodes}]" 2>&1)"; then
    echo "opened TCP between the nodes of $group"
  elif [[ "$out" == *InvalidPermission.Duplicate* ]]; then
    echo "TCP between the nodes of $group is open"
  else
    die "$out"
  fi
}

# Red Hat's cert-manager Operator, for the chart's TLS. The chart's default,
# mutual TLS, needs cert-manager; OpenShift's own service CA serves one-way TLS
# only, a configuration the certification runs have not used.
install_cert_manager() {
  if oc_ get crd certificates.cert-manager.io >/dev/null 2>&1; then
    echo "cert-manager is installed"
    return 0
  fi
  oc_ apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: openshift-cert-manager-operator
  namespace: cert-manager-operator
spec:
  targetNamespaces:
  - cert-manager-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: openshift-cert-manager-operator
  namespace: cert-manager-operator
spec:
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  name: openshift-cert-manager-operator
  channel: stable-v1
  installPlanApproval: Automatic
EOF
  echo "waiting for cert-manager"
  wait_for "the cert-manager CRDs" 900 oc_ get crd certificates.cert-manager.io -o name
  wait_for "the cert-manager webhook" 900 oc_ get deployment cert-manager-webhook -n cert-manager -o name
  oc_ rollout status deployment/cert-manager-webhook -n cert-manager --timeout=10m
}

# The OpenShift prerequisites the operator's install notes list (the CSV
# description in operator/config/manifests/bases): the privileged and anyuid
# SCCs for every ServiceAccount in the namespace, and the privileged Pod
# Security level on it. Without them OpenShift refuses the operator's pod,
# which runs as a fixed user.
grant_openshift_prerequisites() {
  oc_ apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: simplyblock
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: simplyblock-scc-privileged
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:openshift:scc:privileged
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:serviceaccounts:simplyblock
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: simplyblock-scc-anyuid
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:openshift:scc:anyuid
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:serviceaccounts:simplyblock
EOF
}

# simplyblock from this checkout's chart. The chart's snapshot controller stays
# off: OpenShift runs its own, and a second one is a finding of the Level 1
# monitor tests.
cmd_install_simplyblock() {
  require_cluster
  open_storage_traffic
  install_cert_manager
  grant_openshift_prerequisites
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
  build_storage_cluster
}

# The storage cluster a standalone install builds: the chart's discovery run
# writes a draft ClusterDeploymentConfig, approving it creates the cluster and
# one storage node per worker, and the cluster is activated once every node is
# online. On OpenShift every worker reboots into the storage MachineConfig on
# the way, one at a time, so this takes most of an hour.
build_storage_cluster() {
  local draft cluster phase step active
  # A rerun of the step, to upgrade the chart, finds the cluster built.
  active="$(oc_ get storagecluster -n simplyblock \
    -o jsonpath='{range .items[?(@.status.status=="active")]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
  if [[ -n "$active" ]]; then
    echo "storage cluster $active is active"
    return 0
  fi
  # A rerun after the approval, before the cluster is active, finds the
  # approved document and goes on waiting for it; there is no draft to wait for.
  draft="$(oc_ get clusterdeploymentconfig -n simplyblock \
    -o jsonpath='{range .items[?(@.spec.approved==true)]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1)"
  if [[ -n "$draft" ]]; then
    draft="clusterdeploymentconfig/$draft"
    echo "$draft is approved already"
  else
    ensure_discovery_run
    wait_for "the discovery draft" 1800 discovery_draft_ready
    draft="$(oc_ get clusterdeploymentconfig -n simplyblock -o name | head -1)"
    echo "draft: $(oc_ get -n simplyblock "$draft" -o jsonpath='{.status.message}')"
    if [[ "$APPROVE_DRAFT" != "true" ]]; then
      echo "APPROVE_DRAFT=$APPROVE_DRAFT: review $draft in namespace simplyblock and set spec.approved"
      return 0
    fi
    # The draft proposes 30 subsystems per storage node, one per volume, and the
    # Level 1 LUN stress test alone puts 260 volumes on the cluster. The count
    # is fixed when the cluster is created, so it is set in the draft.
    oc_ patch -n simplyblock "$draft" --type merge \
      -p "{\"spec\":{\"approved\":true,\"cluster\":{\"maxSubsystemCount\":$MAX_SUBSYSTEM_COUNT}}}" >/dev/null
  fi
  wait_for "the storage cluster of $draft" 600 oc_ get -n simplyblock "$draft" -o jsonpath='{.status.clusterRef}'
  cluster="$(oc_ get -n simplyblock "$draft" -o jsonpath='{.status.clusterRef}')"
  echo "building storage cluster $cluster"

  local deadline=$((SECONDS + STORAGE_WAIT))
  until [[ "$(oc_ get storagecluster -n simplyblock "$cluster" -o jsonpath='{.status.status}' 2>/dev/null)" == active ]]; do
    [[ $SECONDS -lt $deadline ]] || die "storage cluster $cluster is not active after ${STORAGE_WAIT}s"
    phase="$(oc_ get -n simplyblock "$draft" -o jsonpath='{.status.phase}')"
    step="$(oc_ get -n simplyblock "$draft" -o jsonpath='{.status.step.state}')"
    if [[ "$phase" == Failed ]]; then
      # The Activating step gives every node 60 minutes to come online, which
      # three OpenShift workers rebooting one after another can exceed, and a
      # failed document is never retried. What the step would have done next is
      # ask for the activation, so that is done here once every node is online.
      [[ "$step" == Activating ]] ||
        die "$draft failed at $step: $(oc_ get -n simplyblock "$draft" -o jsonpath='{.status.message}')"
      if all_storage_nodes_online "$draft"; then
        request_activation "$cluster"
      fi
    fi
    oc_ get storagenodes -n simplyblock --no-headers \
      -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,STEP:.status.step.state | sed 's/^/  /'
    sleep 60
  done
  echo "storage cluster $cluster is active"
  oc_ get storageclass
}

# The discovery run the chart's bootstrap ConfigMap carries. The operator
# applies it once; after a first install whose operator never ran, a later
# upgrade does not (the report's "Discovery after a failed first install"), so
# a run that has not appeared within five minutes is applied from the
# ConfigMap.
ensure_discovery_run() {
  local deadline=$((SECONDS + 300))
  while [[ -z "$(oc_ get operatorops,clusterdeploymentconfig -n simplyblock -o name 2>/dev/null)" ]]; do
    if [[ $SECONDS -ge $deadline ]]; then
      echo "no discovery run after five minutes; applying the chart's from its bootstrap ConfigMap"
      oc_ get configmap simplyblock-bootstrap-objects -n simplyblock \
        -o jsonpath='{.data.initial-discovery\.yaml}' | oc_ apply -f -
      return 0
    fi
    sleep "$POLL_SECONDS"
  done
}

# Prints the draft's name once it is valid and waits for approval.
discovery_draft_ready() {
  oc_ get clusterdeploymentconfig -n simplyblock \
    -o jsonpath='{range .items[?(@.status.phase=="Draft")]}{.status.message}{"\n"}{end}' |
    grep 'waiting for spec.approved'
}

all_storage_nodes_online() {
  local node
  for node in $(oc_ get -n simplyblock "$1" -o jsonpath='{.status.nodeRefs[*]}'); do
    [[ "$(oc_ get storagenode -n simplyblock "$node" -o jsonpath='{.status.phase}')" == Online ]] || return 1
  done
}

request_activation() {
  oc_ apply -f - <<EOF
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageClusterOps
metadata:
  name: $1-activate
  namespace: simplyblock
spec:
  clusterRef: $1
  action: Activate
EOF
}

# Everything simplyblock put on the cluster, through the chart's own cleanup
# script, which also removes the storage MachineConfigs of a pool with no nodes
# left. The volumes go with it.
cmd_uninstall_simplyblock() {
  require_cluster
  KUBECONFIG="$KUBECONFIG_PATH" PATH="$(tools_dir):$PATH" \
    "$REPO_ROOT/helm-charts/scripts/cleanup-simplyblock.sh" --force simplyblock
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
  keep_installer_log
  trap - EXIT
  rm -rf "$STATE_DIR"
  echo "cluster $CLUSTER_NAME removed"
}

step="${1:-}"
case "$step" in
  create | wait | destroy)
    start_log "$step"
    trap keep_installer_log EXIT
    ;;
  install-virt | install-simplyblock | uninstall-simplyblock) start_log "$step" ;;
esac

case "$step" in
  create) cmd_create ;;
  wait) cmd_wait ;;
  install-virt) cmd_install_virt ;;
  install-simplyblock) cmd_install_simplyblock ;;
  uninstall-simplyblock) cmd_uninstall_simplyblock ;;
  status) cmd_status ;;
  kubeconfig) require_cluster && echo "$KUBECONFIG_PATH" ;;
  destroy) cmd_destroy ;;
  *)
    sed -n '/^#   \.\/cluster\.sh/s/^#   //p' "$0" >&2
    exit 2
    ;;
esac
