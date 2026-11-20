#!/usr/bin/env bash
#
# Writes the conversion-webhook stanza into the CRD manifests of every kind that
# declares more than one version. It runs after controller-gen, which regenerates
# each base from the Go types and has no marker for spec.conversion, so the stanza
# has to be put back on every regeneration.
#
# The stanza goes into the base rather than into a kustomize patch because the
# Helm chart copies config/crd/bases verbatim into its own crds/ directory, which
# Helm does not template. A patch applied only by config/default would reach
# `make install` and the installer and miss the chart, which is what actually
# installs these CRDs on a cluster.
#
# The namespace written here is the one the kustomize build and the default chart
# install use. An install into any other namespace is corrected at runtime: the
# operator patches the service reference and the CA bundle together, because it is
# the only party that knows which namespace it is running in
# (internal/webhook/certmanager.go).
#
# Shipping strategy: Webhook with a possibly-wrong namespace is deliberate. The
# alternative, leaving the strategy at None until the operator sets it, makes the
# API server answer a v1alpha2 read of a stored v1alpha1 object by renaming the
# apiVersion and pruning every field the new schema does not know, which is
# silently wrong data rather than a failed read.

set -euo pipefail

CRD_DIR="${1:?usage: apply-conversion-webhook.sh <crd-bases-dir> <yq-binary>}"
YQ="${2:?usage: apply-conversion-webhook.sh <crd-bases-dir> <yq-binary>}"

LIST="$(dirname "$CRD_DIR")/converted-kinds.txt"
SERVICE_NAMESPACE="simplyblock-operator-system"
SERVICE_NAME="simplyblock-operator-conversion-webhook-service"

if [ ! -f "$LIST" ]; then
  echo "missing converted-kinds list: $LIST" >&2
  exit 1
fi

while read -r crd; do
  case "$crd" in
    ''|\#*) continue ;;
  esac

  # The list names CRDs as the API server does, <plural>.<group>, and
  # controller-gen names their files the other way round, <group>_<plural>.yaml.
  plural="${crd%%.*}"
  group="${crd#*.}"
  manifest="$CRD_DIR/${group}_${plural}.yaml"
  if [ ! -f "$manifest" ]; then
    echo "converted kind has no manifest: $manifest" >&2
    exit 1
  fi

  SERVICE_NAMESPACE="$SERVICE_NAMESPACE" SERVICE_NAME="$SERVICE_NAME" \
    "$YQ" e -i '
      .spec.conversion.strategy = "Webhook" |
      .spec.conversion.webhook.conversionReviewVersions = ["v1"] |
      .spec.conversion.webhook.clientConfig.service.namespace = strenv(SERVICE_NAMESPACE) |
      .spec.conversion.webhook.clientConfig.service.name = strenv(SERVICE_NAME) |
      .spec.conversion.webhook.clientConfig.service.path = "/convert"
    ' "$manifest"

  echo "  conversion webhook applied: $crd"
done < "$LIST"
