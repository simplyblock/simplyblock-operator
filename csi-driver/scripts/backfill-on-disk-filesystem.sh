#!/bin/bash
#
# Back-fills the storage.simplyblock.io/on-disk-filesystem annotation onto
# simplyblock claims that already carry a filesystem, so the node plugin knows
# what a volume holds without having to read the device.
#
# It exists for the window an upgrade opens. The plugin records that annotation
# itself, but only from a volume's first successful stage onward, so a claim
# provisioned by an older driver has nothing recorded until then, and a stage
# that cannot read its device has nothing to fall back on. Running this once
# after the upgrade annotates the whole fleet in a single pass instead of one
# volume at a time.
#
# A claim is annotated only when a running pod is using it, which is the
# evidence that matters: a volume mounted into a running pod has been formatted,
# whereas a claim that is merely bound may never have been staged at all. The
# filesystem recorded is the one the claim's StorageClass asks for
# (csi.storage.k8s.io/fstype), which is what the driver formatted it with.
#
# Dry run by default; pass --apply to write.
#
# Usage:
#   backfill-on-disk-filesystem.sh [--apply] [-n NAMESPACE | -A] [CLAIM]

set -euo pipefail

ANNOTATION="storage.simplyblock.io/on-disk-filesystem"
PROVISIONER="csi.simplyblock.io"

# The filesystems the node plugin formats and mounts. A StorageClass asking for
# anything else is left alone: the plugin ignores an annotation outside this set,
# so writing one would only mislead whoever reads it next.
SUPPORTED_FS="ext4 xfs"

# The plugin's own default when a StorageClass names no filesystem, from
# fsTypeOrDefault in csi-driver/pkg/spdk/nodeserver.go. Keep the two in step.
DEFAULT_FS="ext4"

usage() {
  sed -n '3,23p' "$0" | sed 's/^#\{1,\} \{0,1\}//'
}

apply=false
ns_args=()
claim_filter=""

while [ $# -gt 0 ]; do
  case "$1" in
    --apply)               apply=true; shift ;;
    -A|--all-namespaces)   ns_args=(--all-namespaces); shift ;;
    -n|--namespace)        ns_args=(--namespace "$2"); shift 2 ;;
    -h|--help)             usage; exit 0 ;;
    -*)                    echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    *)                     claim_filter="$1"; shift ;;
  esac
done

command -v kubectl >/dev/null || { echo "kubectl is not on PATH" >&2; exit 1; }

# Every claim a running pod mounts, as namespace/name. One pass over the pods
# rather than a describe per claim: `describe` prints only the first consumer on
# its "Used By:" line, and a cluster with a few thousand claims would otherwise
# spend minutes in round trips. A pending pod does not count — it has not
# mounted its volume yet, so it is no evidence the volume was ever formatted.
echo "reading pods..." >&2
# shellcheck disable=SC2016  # $ns is a go-template variable, not a shell one.
claims_in_use=$(
  kubectl get pods "${ns_args[@]}" --field-selector=status.phase=Running -o go-template='
{{- range .items -}}
  {{- $ns := .metadata.namespace -}}
  {{- range .spec.volumes -}}
    {{- if .persistentVolumeClaim -}}
      {{- $ns }}/{{ .persistentVolumeClaim.claimName }}{{ "\n" -}}
    {{- end -}}
  {{- end -}}
{{- end -}}' 2>/dev/null | sort -u
)

# StorageClass -> filesystem, read once for the whole run.
class_filesystems=$(
  kubectl get storageclass -o go-template='
{{- range .items -}}
  {{- if eq .provisioner "'"$PROVISIONER"'" -}}
    {{- .metadata.name }}{{ "\t" }}
    {{- with .parameters }}{{ with index . "csi.storage.k8s.io/fstype" }}{{ . }}{{ end }}{{ end }}{{ "\n" -}}
  {{- end -}}
{{- end -}}' 2>/dev/null
)

printf '%-22s %-38s %-8s %s\n' NAMESPACE CLAIM FS ACTION

annotated=0
skipped=0

while IFS=$'\t' read -r ns name class phase mode existing; do
  [ -n "${name:-}" ] || continue
  [ -z "$claim_filter" ] || [ "$name" = "$claim_filter" ] || continue

  fstype=""
  reason=""

  # A raw block volume carries no filesystem at all, and recording one would be
  # a claim the plugin might later act on.
  if [ "${mode:-}" = "Block" ]; then
    reason="skip: raw block volume"
  elif [ "${phase:-}" != "Bound" ]; then
    reason="skip: claim is ${phase:-unknown}"
  elif [ -n "${existing:-}" ]; then
    reason="skip: already records $existing"
  elif ! printf '%s\n' "$class_filesystems" | grep -q "^${class}"$'\t'; then
    reason="skip: not a simplyblock class (${class:-none})"
  elif ! printf '%s\n' "$claims_in_use" | grep -qxF "$ns/$name"; then
    reason="skip: no running pod mounts it"
  else
    fstype=$(printf '%s\n' "$class_filesystems" | grep "^${class}"$'\t' | cut -f2)
    [ -n "$fstype" ] || fstype="$DEFAULT_FS"
    case " $SUPPORTED_FS " in
      *" $fstype "*) ;;
      *) reason="skip: unsupported filesystem $fstype" ;;
    esac
  fi

  if [ -n "$reason" ]; then
    printf '%-22s %-38s %-8s %s\n' "$ns" "$name" "${fstype:--}" "$reason"
    skipped=$((skipped + 1))
    continue
  fi

  if [ "$apply" = true ]; then
    if kubectl -n "$ns" annotate pvc "$name" "$ANNOTATION=$fstype" >/dev/null 2>&1; then
      printf '%-22s %-38s %-8s %s\n' "$ns" "$name" "$fstype" "annotated"
      annotated=$((annotated + 1))
    else
      printf '%-22s %-38s %-8s %s\n' "$ns" "$name" "$fstype" "FAILED"
    fi
  else
    printf '%-22s %-38s %-8s %s\n' "$ns" "$name" "$fstype" "would annotate"
    annotated=$((annotated + 1))
  fi
done < <(
  kubectl get pvc "${ns_args[@]}" -o go-template='
{{- range .items -}}
  {{- .metadata.namespace }}{{ "\t" }}{{ .metadata.name }}{{ "\t" }}
  {{- with .spec.storageClassName }}{{ . }}{{ end }}{{ "\t" }}
  {{- .status.phase }}{{ "\t" }}
  {{- with .spec.volumeMode }}{{ . }}{{ end }}{{ "\t" }}
  {{- with .metadata.annotations }}{{ with index . "storage.simplyblock.io/on-disk-filesystem" }}{{ . }}{{ end }}{{ end }}{{ "\n" -}}
{{- end -}}' 2>/dev/null
)

echo
if [ "$apply" = true ]; then
  echo "annotated $annotated claim(s), skipped $skipped"
else
  echo "would annotate $annotated claim(s), skipped $skipped — rerun with --apply to write"
fi
