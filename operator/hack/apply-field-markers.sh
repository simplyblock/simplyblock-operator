#!/usr/bin/env bash
#
# Writes the +simplyblock:operatorManaged, +simplyblock:advanced, and
# +simplyblock:invisible field markers onto the CRD manifests as annotations. It runs after controller-gen,
# which regenerates each base from the Go types and drops anything it did not
# write, so the annotations have to be put back on every regeneration.
#
# The paths come from hack/fieldmarkers, which resolves each marker through the
# Go types. This script only edits the manifests, through yq, so controller-gen's
# formatting is left as it is. The annotations go into the base for the same
# reason as the conversion stanza (`apply-conversion-webhook.sh`): the Helm chart
# copies config/crd/bases verbatim.

set -euo pipefail

CRD_DIR="${1:?usage: apply-field-markers.sh <crd-bases-dir> <yq-binary>}"
YQ="${2:?usage: apply-field-markers.sh <crd-bases-dir> <yq-binary>}"

lines="$(go run ./hack/fieldmarkers "$CRD_DIR")"

while IFS=$'\t' read -r manifest key value
do
  test -n "$manifest" || continue
  KEY="$key" VALUE="$value" "$YQ" e -i '.metadata.annotations[strenv(KEY)] = strenv(VALUE)' "$manifest"
  echo "  field markers applied: $(basename "$manifest") $key"
done <<< "$lines"
