#!/usr/bin/env bash
# Reports every `.Values.x.y` a template reads that values.yaml does not define.
#
# Helm renders a missing value as the empty string, so a reference that no longer
# resolves is silent: a `{{- if }}` on one drops its whole block, and a value
# written into an env var reaches the container empty. Both have shipped.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
YQ="${YQ:-$REPO/.bin/yq}"
command -v "$YQ" >/dev/null 2>&1 || YQ=yq

# The charts whose templates are hand-written, each as `<chart dir>`.
CHARTS=(
  "$REPO/helm-charts/charts/simplyblock-operator"
  "$REPO/csi-driver/charts/spdk-csi/latest/spdk-csi"
)

fail=0

for chart in "${CHARTS[@]}"; do
  name="${chart#"$REPO"/}"
  if [ ! -f "$chart/values.yaml" ]; then
    echo "  ${name}: no values.yaml"
    fail=1
    continue
  fi

  # Every path values.yaml defines, including the intermediate maps, so that a
  # reference to a block rather than to a leaf resolves too.
  defined="$("$YQ" -r '[.. | path | join(".")] | .[]' "$chart/values.yaml" 2>/dev/null | grep -v '^$' | sort -u)"

  # Every path the templates read. A trailing dot belongs to the template syntax
  # around the reference rather than to the path.
  referenced="$(grep -rhoE '\.Values\.[A-Za-z_][A-Za-z0-9_.]*' "$chart/templates" 2>/dev/null |
    sed -e 's/^\.Values\.//' -e 's/\.$//' | sort -u)"

  # grep reads the list from a here-string rather than from a pipe. "pipefail"
  # is set, and grep -q closes the pipe the moment it matches, so the printf
  # feeding it dies of SIGPIPE and the pipeline reports 141: a defined path read
  # as an undefined one, on whichever run the write had not finished.
  missing=0
  while read -r path; do
    [ -z "$path" ] && continue
    if ! grep -qxF "$path" <<<"$defined"; then
      echo "  ${name}: .Values.${path} is read by a template and not defined"
      grep -rn "Values\.${path}" "$chart/templates" | sed 's|^|      |'
      missing=1
      fail=1
    fi
  done <<<"$referenced"

  if [ "$missing" -eq 0 ]; then
    echo "  ${name}: every referenced value is defined"
  fi
done

exit "$fail"
