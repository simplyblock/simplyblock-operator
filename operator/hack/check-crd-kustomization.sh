#!/usr/bin/env bash
#
# Checks that every CRD controller-gen wrote is listed in config/crd/kustomization.yaml.
#
# controller-gen writes one base per kind into config/crd/bases, and the
# kustomization names them one by one. Nothing reconciles the two: adding a kind
# regenerates its base, and if the resources list is not extended by hand the
# kustomization silently omits it.
#
# The omission is invisible on the path most installs take. The Helm chart copies
# config/crd/bases into its own crds/ directory verbatim, so a chart install gets
# the CRD regardless. Everything built through Kustomize does not: `make install`,
# the consolidated installer, and the OLM bundle all render config/default, which
# renders config/crd, which has only what the list names. The operator then starts
# against a cluster missing that kind, fails to sync its informer, and exits --
# the first evidence being a manager crash loop rather than a missing file.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CRD_DIR="$(cd "$SCRIPT_DIR/../config/crd" && pwd)"
KUSTOMIZATION="$CRD_DIR/kustomization.yaml"

written="$(find "$CRD_DIR/bases" -name '*.yaml' -exec basename {} \; | sort)"
listed="$(grep -oE 'bases/[A-Za-z0-9._-]+\.yaml' "$KUSTOMIZATION" | sed 's|bases/||' | sort)"

missing="$(comm -23 <(printf '%s\n' "$written") <(printf '%s\n' "$listed"))"
stale="$(comm -13 <(printf '%s\n' "$written") <(printf '%s\n' "$listed"))"

status=0

if [ -n "$missing" ]; then
  echo "ERROR: generated CRDs that config/crd/kustomization.yaml does not list."
  echo "They reach a chart install and no other, so add each under the resources key:"
  printf '  - bases/%s\n' $missing
  status=1
fi

if [ -n "$stale" ]; then
  echo "ERROR: config/crd/kustomization.yaml lists CRDs that no longer exist:"
  printf '  - bases/%s\n' $stale
  status=1
fi

if [ "$status" = 0 ]; then
  echo "config/crd: all $(printf '%s\n' "$written" | wc -l | tr -d ' ') generated CRDs are listed"
fi

exit "$status"
