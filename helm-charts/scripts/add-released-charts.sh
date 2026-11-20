#!/usr/bin/env bash
# Copies newly packaged chart archives into a chart repository and adds them to
# its index.yaml. The release pipeline runs it against a checkout of the charts
# branch, which holds the published Helm repository.
#
# The FTPS upload deletes every server file the uploaded repository does not
# contain, so the charts branch has to carry every released chart. Adding to it,
# instead of rebuilding the repository from the tagged commit, keeps the charts
# of every release, whichever release branch the tag was cut from.
#
# Both directories use the <version>/<chart>-<version>.tgz layout. Only the
# version directories are read, so the chart sources and their vendored
# dependency archives next to them are never indexed. A version that is already
# in the repository is left alone: a release is never replaced in place.
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
