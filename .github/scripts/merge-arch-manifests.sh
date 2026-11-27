#!/usr/bin/env bash
# Joins per-architecture images pushed by digest into one tagged multi-arch
# manifest list per repository, for the image builds that run on native runners.
# Usage: TAGS=<newline-separated refs> merge-arch-manifests.sh <digest-dir>
# Writes the manifest list digest to $GITHUB_OUTPUT as "digest".
set -euo pipefail

sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }

digests=()
for f in "$1"/*; do digests+=("$(cat "$f")"); done

repos=$(printf '%s\n' "$TAGS" | sed '/^$/d; s/:[^:/]*$//' | sort -u)

# The digest comes from the list's own bytes, which are the same in every
# repository. Reading it back by tag can return the previous list on a registry
# with read-after-write lag, and preflight then checks the wrong image.
first=$(printf '%s\n' "$repos" | head -n1)
manifest="$(docker buildx imagetools create --dry-run "${digests[@]/#/${first}@}")"
list_digest="sha256:$(printf '%s' "$manifest" | sha256)"

for repo in $repos; do
  args=()
  for tag in $TAGS; do if [ "${tag%:*}" = "$repo" ]; then args+=(-t "$tag"); fi; done
  docker buildx imagetools create "${args[@]}" "${digests[@]/#/${repo}@}"
  # A read by digest is not subject to tag lag, so it proves this list was pushed.
  docker buildx imagetools inspect "${repo}@${list_digest}" > /dev/null
done
echo "digest=${list_digest}" >> "${GITHUB_OUTPUT:-/dev/null}"
