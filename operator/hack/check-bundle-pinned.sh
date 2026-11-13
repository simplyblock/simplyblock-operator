#!/usr/bin/env bash
# Checks a generated OLM bundle before it is published: every image the CSV
# names is pinned by digest, and the simplyblock and ultra images are the ones
# the release tag ships.
#
# Red Hat's certification pipeline fails a bundle that names any image by tag
# alone, and the CSV carries images in places `make bundle` does not rewrite on
# its own (the ControlPlane sample in alm-examples and in the
# initialization-resource annotation). Catching both here fails the release
# instead of the certified-operators PR.
#
# Usage: check-bundle-pinned.sh <bundle-dir> <release-tag>
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $(basename "$0") <bundle-dir> <release-tag>" >&2
  exit 2
fi
CSV="$1/manifests/simplyblock-operator.clusterserviceversion.yaml"
TAG="$2"
# The simplyblock and ultra images drop the "v" and write a hotfix dash as a
# dot, the same mapping helm-charts/scripts/stamp-release-version.sh applies.
VERSION="$(printf '%s' "${TAG#v}" | sed -E 's/-([0-9]+)$/.\1/')"

if [ ! -f "$CSV" ]; then
  echo "$(basename "$0"): no CSV at $CSV" >&2
  exit 1
fi

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

# Every registry reference in the file, including those inside the JSON
# strings of the annotations.
refs="$(grep -oE '(quay\.io|docker\.io|public\.ecr\.aws|registry\.[a-z.]+)/[A-Za-z0-9_./-]+(:[A-Za-z0-9_.-]+)?(@sha256:[0-9a-f]{64})?' "$CSV" | sort -u)"
# Plus every image field, whatever its registry: a sample can name a Docker Hub
# image with no host at all (simplyblock/gateway:s3).
refs="$(printf '%s\n%s\n' "$refs" "$(grep -oE '"?image"?: *"?[A-Za-z0-9][A-Za-z0-9_./:@-]*' "$CSV" |
  sed -E 's/^"?image"?: *"?//' | grep '/' || true)" | sort -u)"

while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  case "$ref" in
    *@sha256:*) ;;
    *) fail "not pinned by digest: $ref" ;;
  esac
  case "$ref" in
    */simplyblock-io/simplyblock:* | */simplyblock-io/ultra:*)
      tag="${ref#*:}"
      tag="${tag%%@*}"
      [ "$tag" = "$VERSION" ] || fail "$ref: tag '$tag' is not the release's '$VERSION'"
      ;;
  esac
done <<<"$refs"

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s) in $CSV"
  exit 1
fi
echo "check-bundle-pinned: every image in $CSV is pinned, simplyblock and ultra at $VERSION"
