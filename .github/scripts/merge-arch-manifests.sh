#!/usr/bin/env bash
# Joins per-architecture images pushed by digest into one tagged multi-arch
# manifest list per repository, for the image builds that run on native runners.
# Usage: TAGS=<newline-separated refs> merge-arch-manifests.sh <digest-dir>
set -euo pipefail

digests=()
for f in "$1"/*; do digests+=("$(cat "$f")"); done

for repo in $(printf '%s\n' "$TAGS" | sed '/^$/d; s/:[^:/]*$//' | sort -u); do
  args=()
  for tag in $TAGS; do if [ "${tag%:*}" = "$repo" ]; then args+=(-t "$tag"); fi; done
  docker buildx imagetools create "${args[@]}" "${digests[@]/#/${repo}@}"
done
