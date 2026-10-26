#!/usr/bin/env bash
# Behavioral tests for stamp-release-version.sh: each case stamps a copy of the
# operator chart with a release tag and checks the image tags it wrote.
#
# The operator image is published under the release tag verbatim. The
# simplyblock image is published without the leading "v" and with a hotfix
# suffix as a fourth dotted component, so v26.3.2-1 ships simplyblock:26.3.2.1.
#
# Regression: 2026-10-21-stamp-simplyblock-dotted-hotfix-tag (release/26.3.2 #762)
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$REPO/helm-charts/charts/simplyblock-operator"
STAMP="$REPO/helm-charts/scripts/stamp-release-version.sh"
YQ="${YQ:-$REPO/.bin/yq}"
command -v "$YQ" >/dev/null 2>&1 || YQ=yq

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

# check stamps a fresh copy of the chart with a tag and compares both image tags.
check() {
  local tag="$1" want_operator="$2" want_simplyblock="$3"
  local copy="$WORK/$tag"
  cp -R "$CHART" "$copy"
  if ! "$STAMP" "$copy" "$tag" >/dev/null; then
    fail "$tag: the stamp exited non-zero"
    return
  fi
  local operator simplyblock
  operator="$("$YQ" -r '.image.operator.tag' "$copy/values.yaml")"
  simplyblock="$("$YQ" -r '.image.simplyblock.tag' "$copy/values.yaml")"
  [[ "$operator" == "$want_operator" ]] ||
    fail "$tag: image.operator.tag is '$operator', want '$want_operator'"
  [[ "$simplyblock" == "$want_simplyblock" ]] ||
    fail "$tag: image.simplyblock.tag is '$simplyblock', want '$want_simplyblock'"
}

check v26.4.0 v26.4.0 26.4.0
check v26.4.0-1 v26.4.0-1 26.4.0.1
check v26.4.0-12 v26.4.0-12 26.4.0.12
# Only a numeric hotfix suffix is a fourth component; any other suffix is kept.
check v0.0.0-lint v0.0.0-lint 0.0.0-lint

if [[ "$failures" -gt 0 ]]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "stamp-release-version: all cases passed"
