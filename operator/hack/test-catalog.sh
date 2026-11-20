#!/usr/bin/env bash
#
# Puts one build of the operator in front of OLM on an OpenShift cluster, from
# the operator image and nothing else.
#
# Installing an operator through OLM means three images, and only the first is
# interesting: the operator, a bundle holding the ClusterServiceVersion that
# names it, and a catalog indexing that bundle. The second and third exist only
# to carry the first to the cluster, so this script derives them -- it generates
# the bundle around the operator image given to it, builds and pushes both, and
# points a CatalogSource at the result.
#
# It lives in operator/hack because it is a development path. The catalog it
# builds indexes one bundle, which is what a test cluster needs and what a
# published catalog is not, and nothing in a release runs it.
#
# The catalog is deployed by digest rather than by the tag it was pushed under.
# OLM re-pulls a CatalogSource image only when the reference changes, so a second
# run pushing to the same tag would leave the old catalog pod serving the old
# bundle. Resolving the digest after the push makes every run a real upgrade.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPERATOR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$OPERATOR_DIR/.." && pwd)"
TOOLS_SH="$REPO_ROOT/scripts/tools.sh"
BIN_DIR="${TOOLS_BIN_DIR:-$REPO_ROOT/.bin}"
OPERATOR_MAKEFILE="$OPERATOR_DIR/Makefile"

# Where the two derived images go. They are build artifacts of a test run rather
# than anything anybody deploys from, so they share one repository apiece and are
# told apart by the catalog version.
BUNDLE_REPOSITORY="${BUNDLE_REPOSITORY:-docker.io/simplyblock/olm-operator-test-bundle}"
CATALOG_REPOSITORY="${CATALOG_REPOSITORY:-docker.io/simplyblock/olm-operator-test-catalog}"

# The catalog pod runs on the cluster, which is amd64 on every OpenShift cluster
# this is tested against, while the machine building it is frequently an ARM64
# Mac. Building for the host by default would produce a catalog whose pod crashes
# with an exec format error, so the target platform is stated rather than
# inherited. The catalog build runs opm inside the image to warm its cache, so
# building for a platform the host is not needs working emulation: Docker
# Desktop ships it, and Podman needs qemu-user-static installed.
PLATFORM="${PLATFORM:-linux/amd64}"

CATALOG_SOURCE_NAME="${CATALOG_SOURCE_NAME:-simplyblock-test}"
# openshift-marketplace is OpenShift's global catalog namespace: a CatalogSource
# there is visible to a Subscription in any namespace and appears in the
# OperatorHub UI. A CatalogSource anywhere else resolves only for Subscriptions
# in that same namespace.
CATALOG_NAMESPACE="${CATALOG_NAMESPACE:-openshift-marketplace}"
CHANNEL="${CHANNEL:-test}"
DISPLAY_NAME="${DISPLAY_NAME:-simplyblock (test)}"
# The upstream opm base serves a file-based catalog on any cluster. A shipped
# catalog uses registry.redhat.io/openshift4/ose-operator-registry-rhel9, which
# needs a pull secret this script does not ask for.
BASE_IMAGE="${BASE_IMAGE:-quay.io/operator-framework/opm:latest}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
OC="${OC:-oc}"
READY_TIMEOUT="${READY_TIMEOUT:-300}"

OPERATOR_IMAGE=""
CATALOG_VERSION=""
BUNDLE_IMAGE=""
CATALOG_IMAGE=""
PULL_SECRETS=""
DO_DEPLOY=1
NEEDS_POLICY_SHIM=0

log() { printf '>> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'USAGE'
usage: test-catalog.sh [options] <operator-image>

    <operator-image>
        The operator build to test, repo:tag or repo@digest. It is written into
        the ClusterServiceVersion of a bundle generated for this run, so it is
        the only image that has to be built and pushed beforehand.

Options:

    --catalog-version VERSION
        The version of the generated bundle, which becomes the CSV version and
        the tag on both derived images. Semver, because OLM orders a channel by
        it. Defaults to the operator Makefile's VERSION.

    --channel NAME
        The channel the bundle is the head of. Default: test.

    --bundle-image IMAGE
    --catalog-image IMAGE
        Override where the generated bundle and catalog are pushed. Both default
        to a tag named after the catalog version on the test repositories in
        BUNDLE_REPOSITORY and CATALOG_REPOSITORY.

    --name NAME
        CatalogSource name. Default: simplyblock-test.

    --namespace NAMESPACE
        CatalogSource namespace. Default: openshift-marketplace.

    --pull-secret NAME
        A secret in the CatalogSource namespace granting access to a private
        catalog registry. Repeatable.

    --base-image IMAGE
        The base image the catalog is built on.

    --platform PLATFORM
        The build platform. Default: linux/amd64.

    --container-tool BINARY
        Default: docker.

    --timeout SECONDS
        How long to wait for the catalog to report READY. Default: 300.

    --skip-deploy
        Build and push both images, touch no cluster.

    -h, --help
        This text.

Options also accept --option=value. Every default above is an environment
variable as well: BUNDLE_REPOSITORY, CATALOG_REPOSITORY, CATALOG_SOURCE_NAME,
CATALOG_NAMESPACE, CHANNEL, BASE_IMAGE, PLATFORM, CONTAINER_TOOL, OC,
READY_TIMEOUT, and CLUSTER_IMAGE, SPDK_IMAGE and REBALANCER_IMAGE for the three
images the CSV names besides the operator.
USAGE
}

parse_args() {
  local option value

  while [ $# -gt 0 ]; do
    # --option=value and --option value are the same thing, split here so every
    # case below reads one form.
    case "$1" in
      --*=*)
        option="${1%%=*}"
        value="${1#*=}"
        shift
        set -- "$option" "$value" "$@"
        ;;
    esac

    case "$1" in
      --catalog-version) CATALOG_VERSION="${2:?--catalog-version needs a version}"; shift 2 ;;
      --channel) CHANNEL="${2:?--channel needs a name}"; shift 2 ;;
      --bundle-image) BUNDLE_IMAGE="${2:?--bundle-image needs an image}"; shift 2 ;;
      --catalog-image) CATALOG_IMAGE="${2:?--catalog-image needs an image}"; shift 2 ;;
      --name) CATALOG_SOURCE_NAME="${2:?--name needs a name}"; shift 2 ;;
      --namespace) CATALOG_NAMESPACE="${2:?--namespace needs a namespace}"; shift 2 ;;
      --pull-secret) PULL_SECRETS="$PULL_SECRETS${2:?--pull-secret needs a name}"$'\n'; shift 2 ;;
      --base-image) BASE_IMAGE="${2:?--base-image needs an image}"; shift 2 ;;
      --platform) PLATFORM="${2:?--platform needs a platform}"; shift 2 ;;
      --container-tool) CONTAINER_TOOL="${2:?--container-tool needs a binary}"; shift 2 ;;
      --timeout) READY_TIMEOUT="${2:?--timeout needs seconds}"; shift 2 ;;
      --skip-deploy) DO_DEPLOY=0; shift ;;
      -h | --help) usage; exit 0 ;;
      -*) usage; die "unknown option: $1" ;;
      *)
        [ -z "$OPERATOR_IMAGE" ] || { usage; die "only one operator image is accepted"; }
        OPERATOR_IMAGE="$1"
        shift
        ;;
    esac
  done

  [ -n "$OPERATOR_IMAGE" ] || { usage; die "no operator image given"; }
  case "$OPERATOR_IMAGE" in
    */*) ;;
    *) die "the operator image needs a repository: $OPERATOR_IMAGE" ;;
  esac
  [ -n "$(image_tag "$OPERATOR_IMAGE")" ] || [ "${OPERATOR_IMAGE#*@}" != "$OPERATOR_IMAGE" ] \
    || die "the operator image needs a tag or a digest: $OPERATOR_IMAGE"
}

# The default of a `VAR ?= value` line in the operator Makefile, so that the
# images and versions this script does not take from the caller are the ones the
# build actually uses.
makefile_default() {
  sed -n "s/^$1[[:space:]]*?=[[:space:]]*//p" "$OPERATOR_MAKEFILE" | head -1
}

# The repository half of an image reference, with any tag or digest removed.
image_repository() {
  local ref="${1%%@*}"
  printf '%s\n' "${ref%:*}"
}

# The tag of an image reference, empty when it carries none. A registry port
# looks like a tag until the rest of the path is accounted for, which is what the
# "/" case rules out.
image_tag() {
  local ref="${1%%@*}" tag="${1##*:}"
  case "$ref" in
    *:*) ;;
    *) return 0 ;;
  esac
  case "$tag" in
    */*) return 0 ;;
    *) printf '%s\n' "$tag" ;;
  esac
}

resolve_images() {
  if [ -z "$CATALOG_VERSION" ]; then
    CATALOG_VERSION="$(makefile_default VERSION)"
    [ -n "$CATALOG_VERSION" ] || die "could not read VERSION from $OPERATOR_MAKEFILE: pass --catalog-version"
  fi
  # OLM picks the head of a channel by semver, and a CSV whose version is not one
  # is rejected at bundle generation with a message about the name rather than
  # the version.
  case "$CATALOG_VERSION" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) die "--catalog-version must be semver, such as 0.2.0: $CATALOG_VERSION" ;;
  esac

  [ -n "$BUNDLE_IMAGE" ] || BUNDLE_IMAGE="$BUNDLE_REPOSITORY:$CATALOG_VERSION"
  [ -n "$CATALOG_IMAGE" ] || CATALOG_IMAGE="$CATALOG_REPOSITORY:$CATALOG_VERSION"

  # The three images the CSV names besides the operator. They are not what a run
  # is testing, so they default to what the Makefile ships.
  CLUSTER_IMAGE="${CLUSTER_IMAGE:-$(makefile_default CLUSTER_IMAGE_BASE):$(makefile_default CLUSTER_IMAGE_TAG)}"
  SPDK_IMAGE="${SPDK_IMAGE:-$(makefile_default SPDK_IMAGE_BASE):$(makefile_default SPDK_IMAGE_TAG)}"
  local operator_tag
  operator_tag="$(image_tag "$OPERATOR_IMAGE")"
  [ -n "$operator_tag" ] || operator_tag="$(makefile_default IMG_TAG)"
  REBALANCER_IMAGE="${REBALANCER_IMAGE:-$(makefile_default REBALANCER_IMG_BASE):$operator_tag}"
  OPENSHIFT_VERSION="${OPENSHIFT_VERSION:-$(makefile_default OPENSHIFT_VERSION)}"
}

# Refuses a derived image that would overwrite one of the product's own.
#
# The bundle and the catalog are built and pushed by this script, and a
# repository holding a product image is not a place to put either: the push would
# replace the operator, a storage node image, or the released bundle with a build
# artifact, in a repository somebody deploys from. Anyone with rights to push to
# those repositories would not get an error, so the check has to be here.
reject_product_repository() {
  local name repo var product

  for name in BUNDLE_IMAGE CATALOG_IMAGE; do
    eval "repo=\"\$(image_repository \"\$$name\")\""
    for var in IMG_BASE ECR_IMG_BASE REBALANCER_IMG_BASE ECR_REBALANCER_IMG_BASE \
      CLUSTER_IMAGE_BASE SPDK_IMAGE_BASE BUNDLE_IMG; do
      product="$(image_repository "$(makefile_default "$var")")"
      [ -n "$product" ] || continue
      [ "$repo" != "$product" ] || die "$repo is the $var repository, which holds a product image.
This script pushes the generated bundle and catalog there, which would replace it.
Point --${name%_IMAGE}-image, or \$${name%_IMAGE}_REPOSITORY, at a repository of
their own."
    done
    [ "$repo" != "$(image_repository "$OPERATOR_IMAGE")" ] \
      || die "$repo holds the operator image being tested, which this script would overwrite.
Point --${name%_IMAGE}-image, or \$${name%_IMAGE}_REPOSITORY, at a repository of
their own."
  done
}

require_tools() {
  "$TOOLS_SH" install opm >&2
  "$TOOLS_SH" install yq >&2
  "$TOOLS_SH" install kustomize >&2
  OPM="$BIN_DIR/opm"
  YQ="$BIN_DIR/yq"
  KUSTOMIZE="$BIN_DIR/kustomize"

  command -v "$CONTAINER_TOOL" >/dev/null 2>&1 \
    || die "$CONTAINER_TOOL is not on PATH (set CONTAINER_TOOL=podman to use Podman)"
  command -v operator-sdk >/dev/null 2>&1 \
    || die "operator-sdk is not on PATH, and generating the bundle needs it"

  # opm pulls the bundle image through containers/image, which refuses to pull at
  # all when it can find no signature policy. Every Linux distribution shipping
  # Podman installs one; a Mac carrying only Docker Desktop has none.
  if [ ! -f "${HOME}/.config/containers/policy.json" ] \
    && [ ! -f /etc/containers/policy.json ] \
    && [ ! -f /usr/share/containers/policy.json ]; then
    NEEDS_POLICY_SHIM=1
  fi
}

require_cluster() {
  command -v "$OC" >/dev/null 2>&1 || die "$OC is not on PATH"
  "$OC" whoami >/dev/null 2>&1 \
    || die "not logged in to a cluster: run 'oc login' against the test cluster first"
  "$OC" get crd catalogsources.operators.coreos.com >/dev/null 2>&1 \
    || die "this cluster has no OLM (no CatalogSource CRD), so it cannot serve a catalog"
  "$OC" auth can-i create catalogsource -n "$CATALOG_NAMESPACE" >/dev/null 2>&1 \
    || die "$("$OC" whoami) may not create a CatalogSource in $CATALOG_NAMESPACE"
  log "cluster: $("$OC" whoami --show-server) as $("$OC" whoami)"
}

# Generates the bundle manifests around the operator image under test.
#
# This is `make bundle` without its digest resolution. That target pins every
# image by a digest it looks up on quay, which is right for a release and wrong
# here: the image being tested is wherever it was pushed, frequently not quay,
# and a digest lookup against the wrong registry would silently pin a reference
# that resolves to nothing. A test bundle names the image it was given.
#
# operator-sdk regenerates bundle.Dockerfile in the operator directory, which is
# tracked. The generated one is what the bundle is built from -- its channel
# labels have to agree with the metadata generated beside them -- so it is moved
# into the work directory and the tracked file put back as it was. Its COPY
# paths stay relative to the operator directory, which is the build context.
generate_bundle() {
  local dockerfile="$OPERATOR_DIR/bundle.Dockerfile"

  log "generating the bundle for $OPERATOR_IMAGE at version $CATALOG_VERSION"
  cp "$dockerfile" "$WORK/bundle.Dockerfile.orig"

  (
    cd "$OPERATOR_DIR"
    "$KUSTOMIZE" build config/manifests \
      | operator-sdk generate bundle -q --overwrite \
        --version "$CATALOG_VERSION" --channels "$CHANNEL" --default-channel "$CHANNEL"

    OPERATOR_IMG="$OPERATOR_IMAGE" \
      CLUSTER_IMG="$CLUSTER_IMAGE" \
      SPDK_IMG="$SPDK_IMAGE" \
      REBALANCER_IMG="$REBALANCER_IMAGE" \
      "$YQ" e '.metadata.annotations.containerImage = strenv(OPERATOR_IMG)
        | .spec.relatedImages = [{"name": "simplyblock-operator", "image": strenv(OPERATOR_IMG)},
          {"name": "simplyblock", "image": strenv(CLUSTER_IMG)},
          {"name": "ultra-spdk", "image": strenv(SPDK_IMG)},
          {"name": "simplyblock-rebalancer", "image": strenv(REBALANCER_IMG)}]
        | .spec.install.spec.deployments[0].spec.template.spec.containers[0].image = strenv(OPERATOR_IMG)' \
        -i bundle/manifests/simplyblock-operator.clusterserviceversion.yaml

    OPENSHIFT_VERSION="$OPENSHIFT_VERSION" \
      "$YQ" e '.annotations."com.redhat.openshift.versions" = strenv(OPENSHIFT_VERSION)' \
        -i bundle/metadata/annotations.yaml
  )

  cp "$dockerfile" "$WORK/bundle.Dockerfile"
  cp "$WORK/bundle.Dockerfile.orig" "$dockerfile"
}

build_push() {
  local image="$1" dockerfile="$2" context="$3"

  log "building $image for $PLATFORM"
  "$CONTAINER_TOOL" build --platform "$PLATFORM" -f "$dockerfile" -t "$image" "$context"
  log "pushing $image"
  "$CONTAINER_TOOL" push "$image"
}

# Runs opm against a policy this script owns when the machine has none.
#
# containers/image takes its policy from $HOME/.config/containers/policy.json or
# from /etc, and opm exposes no flag for either, so the only way to supply one is
# a home directory. Writing into the real one would change how every other
# container tool on the machine verifies signatures, to make one development
# script work, so the shim home lives in the work directory and is thrown away
# with it. A machine that does have a policy keeps using it: somebody who
# configured signature verification meant it.
#
# The registry credentials live in the real home, which the shim hides, so they
# are passed separately through REGISTRY_AUTH_FILE -- without which a private
# bundle image would start failing to authenticate the moment the shim engaged.
opm_render() {
  local home="$WORK/policy-home" candidate

  if [ "$NEEDS_POLICY_SHIM" = 0 ]; then
    "$OPM" render "$@"
    return
  fi

  mkdir -p "$home/.config/containers"
  printf '%s\n' '{"default":[{"type":"insecureAcceptAnything"}]}' \
    > "$home/.config/containers/policy.json"

  if [ -z "${REGISTRY_AUTH_FILE:-}" ]; then
    for candidate in \
      "$HOME/.config/containers/auth.json" \
      "${XDG_RUNTIME_DIR:-}/containers/auth.json" \
      "$HOME/.docker/config.json"; do
      if [ -f "$candidate" ]; then
        export REGISTRY_AUTH_FILE="$candidate"
        break
      fi
    done
  fi

  log "no containers signature policy on this machine, using a temporary permissive one"
  HOME="$home" "$OPM" render "$@"
}

# Renders the file-based catalog into $WORK/catalog. The package and CSV names
# are read back out of the render rather than assumed: a channel entry that names
# a CSV the bundle does not contain produces a catalog that validates and
# resolves to nothing.
render_catalog() {
  local dir

  log "rendering $BUNDLE_IMAGE"
  opm_render "$BUNDLE_IMAGE" --output=yaml > "$WORK/bundle.yaml"

  PACKAGE="$("$YQ" 'select(.schema == "olm.bundle") | .package' "$WORK/bundle.yaml" | head -1)"
  CSV_NAME="$("$YQ" 'select(.schema == "olm.bundle") | .name' "$WORK/bundle.yaml" | head -1)"
  [ -n "$PACKAGE" ] && [ "$PACKAGE" != "null" ] || die "the render produced no olm.bundle from $BUNDLE_IMAGE"
  [ -n "$CSV_NAME" ] && [ "$CSV_NAME" != "null" ] || die "the rendered bundle has no name"

  # A file-based catalog holds one directory per package, named for it.
  dir="$WORK/catalog/$PACKAGE"
  mkdir -p "$dir"

  cat > "$dir/catalog.yaml" <<EOF
---
schema: olm.package
name: $PACKAGE
defaultChannel: $CHANNEL
---
schema: olm.channel
package: $PACKAGE
name: $CHANNEL
entries:
  - name: $CSV_NAME
EOF
  cat "$WORK/bundle.yaml" >> "$dir/catalog.yaml"

  log "validating the catalog"
  "$OPM" validate "$WORK/catalog"
  log "catalog: package $PACKAGE, channel $CHANNEL, head $CSV_NAME"

  # opm writes <dir>.Dockerfile beside the directory and refuses to overwrite one,
  # which is why the whole build happens in a work directory rather than in the
  # tree.
  "$OPM" generate dockerfile "$WORK/catalog" --base-image "$BASE_IMAGE"
}

# The digest of what is now in the registry under that tag. Read from the local
# image's repo digests, which both Docker and Podman record at push time.
resolve_digest() {
  local repo="$(image_repository "$CATALOG_IMAGE")" digests
  digests="$("$CONTAINER_TOOL" inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$CATALOG_IMAGE" 2>/dev/null || true)"
  CATALOG_REF="$(printf '%s\n' "$digests" | grep -m1 "^${repo}@sha256:" || true)"
  if [ -z "$CATALOG_REF" ]; then
    log "warning: no digest for $CATALOG_IMAGE, deploying the tag instead."
    log "         OLM will not re-pull a tag it already has, so a reused tag may keep serving the old catalog."
    CATALOG_REF="$CATALOG_IMAGE"
  fi
}

apply_catalog_source() {
  local secrets="" secret
  if [ -n "$PULL_SECRETS" ]; then
    secrets=$'\n  secrets:'
    while IFS= read -r secret; do
      [ -n "$secret" ] || continue
      secrets="$secrets"$'\n  - '"$secret"
    done <<< "$PULL_SECRETS"
  fi

  log "applying CatalogSource $CATALOG_SOURCE_NAME in $CATALOG_NAMESPACE -> $CATALOG_REF"
  "$OC" apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: $CATALOG_SOURCE_NAME
  namespace: $CATALOG_NAMESPACE
spec:
  sourceType: grpc
  image: $CATALOG_REF
  displayName: $DISPLAY_NAME
  publisher: simplyblock
  updateStrategy:
    registryPoll:
      interval: 10m$secrets
EOF
}

# A CatalogSource whose image changed keeps reporting READY from the pod still
# serving the old one, so readiness alone proves nothing. What proves the new
# catalog is serving is the pod running the image that was just pushed.
wait_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT)) state pod

  log "waiting for the catalog pod to run $CATALOG_REF"
  while [ "$SECONDS" -lt "$deadline" ]; do
    pod="$("$OC" -n "$CATALOG_NAMESPACE" get pods \
      -l "olm.catalogSource=$CATALOG_SOURCE_NAME" \
      -o jsonpath="{range .items[?(@.status.phase=='Running')]}{.spec.containers[0].image}{'\n'}{end}" 2>/dev/null || true)"
    state="$("$OC" -n "$CATALOG_NAMESPACE" get catalogsource "$CATALOG_SOURCE_NAME" \
      -o jsonpath='{.status.connectionState.lastObservedState}' 2>/dev/null || true)"
    if [ "$state" = "READY" ] && printf '%s\n' "$pod" | grep -qxF "$CATALOG_REF"; then
      log "catalog is READY"
      return 0
    fi
    sleep 5
  done

  printf 'error: the catalog did not become ready within %ss (last state: %s)\n' \
    "$READY_TIMEOUT" "${state:-unknown}" >&2
  "$OC" -n "$CATALOG_NAMESPACE" get pods -l "olm.catalogSource=$CATALOG_SOURCE_NAME" >&2 || true
  return 1
}

report() {
  cat >&2 <<EOF

$CSV_NAME is serving, installing $OPERATOR_IMAGE. What the catalog offers:

  oc get packagemanifest -n $CATALOG_NAMESPACE $PACKAGE

Subscribe to it, in a namespace holding an OperatorGroup with an empty spec --
the CSV supports AllNamespaces only:

  apiVersion: operators.coreos.com/v1alpha1
  kind: Subscription
  metadata:
    name: $PACKAGE
    namespace: simplyblock
  spec:
    channel: $CHANNEL
    name: $PACKAGE
    source: $CATALOG_SOURCE_NAME
    sourceNamespace: $CATALOG_NAMESPACE
    installPlanApproval: Manual

An existing Subscription on this channel picks the new bundle up when the CSV
version moved, and does nothing when it did not, because OLM has already
installed that CSV. Reinstalling one version means deleting the Subscription and
its CSV first, or running again with a higher --catalog-version.
EOF
}

main() {
  parse_args "$@"
  resolve_images
  reject_product_repository

  log "operator under test: $OPERATOR_IMAGE"
  log "bundle:              $BUNDLE_IMAGE"
  log "catalog:             $CATALOG_IMAGE"

  require_tools
  if [ "$DO_DEPLOY" = 1 ]; then
    require_cluster
  fi

  WORK="$(mktemp -d "${TMPDIR:-/tmp}/simplyblock-catalog.XXXXXXXXXX")"
  trap 'rm -rf "$WORK"' EXIT

  generate_bundle
  build_push "$BUNDLE_IMAGE" "$WORK/bundle.Dockerfile" "$OPERATOR_DIR"
  render_catalog
  build_push "$CATALOG_IMAGE" "$WORK/catalog.Dockerfile" "$WORK"

  if [ "$DO_DEPLOY" = 1 ]; then
    resolve_digest
    apply_catalog_source
    wait_ready
    report
  fi
}

main "$@"
