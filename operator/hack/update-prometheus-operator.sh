#!/usr/bin/env bash
# Regenerates the embedded prometheus-operator prerequisite from the upstream
# release bundle.yaml of one version, and pins its images in related-images.yaml.
# The manifest is stored gzipped: it is 77K lines of upstream CRDs.
# Lives beside the other operator maintenance scripts; run it through
# `make prometheus-operator-manifest PROMETHEUS_OPERATOR_VERSION=vX.Y.Z`.
#
# The bundle is kept as upstream ships it except for line-level edits: the
# namespace moves from default to prometheus-operator, the --kubelet-* flags are
# dropped so it maintains no kubelet Service, and both images are pinned by the
# digest quay.io serves for the tag.
#
# Usage: update-prometheus-operator.sh <version>
set -euo pipefail

version="${1:?usage: $0 <version, e.g. v0.94.1>}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
manifest="$here/../internal/prerequisites/manifests/prometheus-operator.yaml.gz"
related="$here/../internal/prerequisites/related-images.yaml"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL -o "$work/bundle.yaml" \
  "https://github.com/prometheus-operator/prometheus-operator/releases/download/$version/bundle.yaml"

digest() { # repository under quay.io
  curl -fsSL "https://quay.io/api/v1/repository/$1/tag/?specificTag=$version&onlyActiveTags=true" |
    python3 -c 'import json,sys; t=json.load(sys.stdin)["tags"]; print(t[0]["manifest_digest"] if t else "")'
}
operator_ref="quay.io/prometheus-operator/prometheus-operator:$version"
reloader_ref="quay.io/prometheus-operator/prometheus-config-reloader:$version"
operator_pinned="$operator_ref@$(digest prometheus-operator/prometheus-operator)"
reloader_pinned="$reloader_ref@$(digest prometheus-operator/prometheus-config-reloader)"
for ref in "$operator_pinned" "$reloader_pinned"; do
  [[ "$ref" == *@sha256:* ]] || {
    echo "quay.io has no image ${ref%@} although the $version bundle names it." >&2
    echo "Some releases never published images (v0.95.0 did not); use a later patch release." >&2
    exit 1
  }
done

python3 - "$work/bundle.yaml" "$manifest" "$version" "$operator_ref" "$operator_pinned" \
  "$reloader_ref" "$reloader_pinned" <<'EOF'
import gzip
import sys
src, dst, version, op, op_pinned, rl, rl_pinned = sys.argv[1:]
lines = open(src).read().splitlines()
out, counts = [], {"namespace": 0, "kubelet": 0, "operator": 0, "reloader": 0}
for line in lines:
    if line.strip().startswith("- --kubelet"):
        counts["kubelet"] += 1
        continue
    if line.rstrip() == "  namespace: default":
        line = "  namespace: prometheus-operator"
        counts["namespace"] += 1
    if rl in line:
        line = line.replace(rl, rl_pinned)
        counts["reloader"] += 1
    if line.rstrip().endswith("image: " + op):
        line = line.replace(op, op_pinned)
        counts["operator"] += 1
    out.append(line)
# Each edit has to land, or the bundle changed shape and the edits need a look.
missing = [k for k, n in counts.items() if n == 0]
if missing:
    sys.exit(f"the {version} bundle has nothing to edit for: {', '.join(missing)}")
header = f"""# prometheus-operator {version}, the upstream bundle.yaml with these changes: it
# runs in its own namespace, both images are pinned by digest, and the
# --kubelet-* flags are dropped, so it maintains no kubelet Service. The
# operator applies it where monitoring.coreos.com/v1 is not served.
# Regenerate with `make -C operator prometheus-operator-manifest`.
---
apiVersion: v1
kind: Namespace
metadata:
  name: prometheus-operator
"""
# mtime=0 and no file name, so one version always produces the same bytes.
with open(dst, "wb") as f, gzip.GzipFile(fileobj=f, mode="wb", compresslevel=9, mtime=0, filename="") as gz:
    gz.write((header + "\n".join(out) + "\n").encode())
EOF

python3 - "$related" "$operator_pinned" "$reloader_pinned" <<'EOF'
import sys
path, *refs = sys.argv[1:]
text = open(path).read()
for ref in refs:
    name = ref.split("/")[-1].split(":")[0]
    entry = f"- name: {name}\n  image: {ref}\n"
    lines = text.splitlines(keepends=True)
    for i, line in enumerate(lines):
        if line == f"- name: {name}\n":
            lines[i:i + 2] = [entry]
            break
    else:
        lines.append(entry)
    text = "".join(lines)
open(path, "w").write(text)
EOF
echo "prometheus-operator $version: $manifest"
