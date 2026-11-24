#!/usr/bin/env bash
# Joins per-architecture images pushed by digest into one tagged multi-arch
# manifest list per repository, for the image builds that run on native runners.
# Usage: TAGS=<newline-separated refs> merge-arch-manifests.sh <digest-dir>
# Writes the manifest list digest to $GITHUB_OUTPUT as "digest".
set -euo pipefail

digests=()
for f in "$1"/*; do digests+=("$(cat "$f")"); done

list_digest=""
for repo in $(printf '%s\n' "$TAGS" | sed '/^$/d; s/:[^:/]*$//' | sort -u); do
  args=()
  for tag in $TAGS; do if [ "${tag%:*}" = "$repo" ]; then args+=(-t "$tag"); fi; done
  docker buildx imagetools create "${args[@]}" "${digests[@]/#/${repo}@}"
  [ -n "$list_digest" ] || list_digest="$(docker buildx imagetools inspect "${args[1]}" --format '{{json .Manifest}}' | jq -r .digest)"
done
echo "digest=${list_digest}" >> "${GITHUB_OUTPUT:-/dev/null}"
