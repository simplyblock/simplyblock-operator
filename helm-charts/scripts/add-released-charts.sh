#!/usr/bin/env bash
# Copies newly packaged chart archives into a committed chart repository and
# adds them to its index.yaml, for the release pipeline's pull request against
# main.
#
# The published Helm repository is rebuilt from the committed charts on every
# release, and the FTPS upload deletes whatever the rebuild does not contain.
# A released chart that is never committed is therefore deleted by the next
# release; committing each one keeps every release in the published repository.
#
# Both directories use the <version>/<chart>-<version>.tgz layout. Only the
# version directories are read, so the chart sources and their vendored
# dependency archives next to them are never indexed. A version that is already
# committed is left alone: a release is never replaced in place.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $(basename "$0") <packaged-dir> <repo-dir> <base-url>" >&2
  exit 2
fi

src="$1"
repo="$2"
base="${3%/}"

added=0
for tgz in "$src"/*/*.tgz; do
  [ -e "$tgz" ] || continue
  version="$(basename "$(dirname "$tgz")")"
  name="$(basename "$tgz")"
  if [ -e "$repo/$version/$name" ]; then
    continue
  fi

  mkdir -p "$repo/$version"
  cp "$tgz" "$repo/$version/$name"

  # Index the one version directory and merge the existing index into it, so
  # the entries already committed keep their digests and creation times.
  scratch="$(mktemp -d)"
  cp "$tgz" "$scratch/"
  helm repo index "$scratch" --url "$base/$version" --merge "$repo/index.yaml"
  mv "$scratch/index.yaml" "$repo/index.yaml"
  rm -rf "$scratch"

  echo "added $version/$name"
  added=$((added + 1))
done

echo "$added chart(s) added to $repo"
