#!/usr/bin/env bash
# Rewrites the operator chart's simplyblock and operator image tags to a release
# version, for the packaging step of the release pipeline.
#
# values.yaml commits the moving `main` tags, because the chart in the branch
# installs the branch. A published release has to name the images that release
# built instead, and stamping them here rather than in the commit keeps the two
# apart: the branch keeps following main, and no release leaves a pinned tag
# behind for the next development install to pick up.
#
# Only these two tags are rewritten. The rebalancer and the CSI plugin take the
# operator's tag when theirs is empty, and Chart.yaml is left alone because
# `helm package` takes --version and --app-version.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $(basename "$0") <chart-dir> <release-tag>" >&2
  exit 2
fi

CHART="$1"
TAG="$2"
# The operator image is pushed under the release tag verbatim, the simplyblock
# image under the version without its leading "v."
VERSION="${TAG#v}"
VALUES="$CHART/values.yaml"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
YQ="${YQ:-$REPO/.bin/yq}"
command -v "$YQ" >/dev/null 2>&1 || YQ=yq

if [ ! -f "$VALUES" ]; then
  echo "$(basename "$0"): no values.yaml under $CHART" >&2
  exit 1
fi

# An anchored line rewrite rather than an in-place yq edit: yq drops every blank
# line in the file it writes, and values.yaml is documentation a user reads.
awk -v operator_tag="$TAG" -v simplyblock_tag="$VERSION" '
  /^[A-Za-z_][A-Za-z0-9_]*:/ { top = $1; sub(/:.*/, "", top); sub_key = "" }
  /^  [A-Za-z_][A-Za-z0-9_]*:/ { sub_key = $1; sub(/:.*/, "", sub_key) }
  top == "image" && /^    tag:/ {
    if (sub_key == "operator")    { print "    tag: \"" operator_tag "\"";    next }
    if (sub_key == "simplyblock") { print "    tag: \"" simplyblock_tag "\""; next }
  }
  { print }
' "$VALUES" > "$VALUES.stamped"
mv "$VALUES.stamped" "$VALUES"

# The rewrite is anchored to a shape values.yaml is free to change, so it states
# what it produced and fails the release rather than publishing a chart that
# still names the development images.
stamped_operator="$("$YQ" -r '.image.operator.tag' "$VALUES")"
stamped_simplyblock="$("$YQ" -r '.image.simplyblock.tag' "$VALUES")"
if [ "$stamped_operator" != "$TAG" ] || [ "$stamped_simplyblock" != "$VERSION" ]; then
  echo "$(basename "$0"): the image tags did not take: operator is '$stamped_operator' (want '$TAG'), simplyblock is '$stamped_simplyblock' (want '$VERSION')" >&2
  exit 1
fi

echo "Stamped $VALUES: image.operator.tag=$TAG, image.simplyblock.tag=$VERSION"
