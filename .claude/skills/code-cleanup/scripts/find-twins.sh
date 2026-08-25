#!/usr/bin/env bash
#
# Finds the same code written twice: functions whose bodies are identical across
# the operator and the CSI driver, functions duplicated inside one module, and
# places where a consumer hand-rolls a primitive atlas-lib already owns.
#
# It lives with the code-cleanup scripts because both skills that need it read
# the same records: code-cleanup's deduplication pass works on the within-module
# list, and extract-to-atlas-lib works on the cross-module list, which is the
# strongest evidence a concern belongs in the shared library.
#
# Usage:
#   find-twins.sh                       # everything below
#   find-twins.sh --cross               # operator <-> csi-driver only
#   find-twins.sh --within              # copies inside one module only
#   find-twins.sh --handrolled          # atlas-lib primitives written by hand
#   find-twins.sh --min-lines 8         # ignore shorter bodies (default 5)
#   find-twins.sh --with-tests          # count _test.go files too
#
# A cross-module twin is reported by its normalized body, so two copies that
# drifted in formatting or comments still pair up, while two that drifted in
# behavior no longer do — and that difference is itself worth knowing before a
# move, because one of the two copies has the bug fix.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AWK_LIB="${SCRIPT_DIR}/lib/gofuncs.awk"

if [ -z "${CLEANUP_ROOT:-}" ]; then
  # "A || B && C" groups as "(A || B) && C", so the fallback needs its own subshell.
  REPO_ROOT="$(git -C "${SCRIPT_DIR}" rev-parse --show-toplevel 2>/dev/null || (cd "${SCRIPT_DIR}/../../../.." && pwd))"
else
  REPO_ROOT="${CLEANUP_ROOT}"
fi

MIN_LINES="${MIN_LINES:-5}"
WITH_TESTS=0
DO_CROSS=0
DO_WITHIN=0
DO_HANDROLLED=0

while [ $# -gt 0 ]; do
  case "$1" in
    --cross) DO_CROSS=1; shift ;;
    --within) DO_WITHIN=1; shift ;;
    --handrolled) DO_HANDROLLED=1; shift ;;
    --min-lines) MIN_LINES="$2"; shift 2 ;;
    --with-tests) WITH_TESTS=1; shift ;;
    -h|--help) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "find-twins.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ $((DO_CROSS + DO_WITHIN + DO_HANDROLLED)) -eq 0 ]; then
  DO_CROSS=1
  DO_WITHIN=1
  DO_HANDROLLED=1
fi

cd "${REPO_ROOT}" || exit 1

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

BOLD=""; DIM=""; RESET=""
if [ -t 1 ]; then
  BOLD="\033[1m"; DIM="\033[2m"; RESET="\033[0m"
fi

# One record set per module, tagged with the module name, so a body can be told
# apart by where it lives.
: > "${TMP}/funcs"
for module in atlas-lib operator csi-driver; do
  find "${module}" -name '*.go' -type f 2>/dev/null \
    | grep -v -e '\.gen\.go$' -e 'zz_generated' -e '/vendor/' -e '/third_party/' \
    | { if [ "${WITH_TESTS}" -eq 1 ]; then cat; else grep -v '_test\.go$'; fi; } \
    > "${TMP}/files.${module}" || true
  [ -s "${TMP}/files.${module}" ] || continue
  xargs awk -f "${AWK_LIB}" < "${TMP}/files.${module}" 2>/dev/null \
    | awk -F'\t' -v m="${module}" -v min="${MIN_LINES}" \
        '$1 >= min { printf "%s\t%s\n", m, $0 }' >> "${TMP}/funcs"
done

# Fields after the module tag: length, maxIndent, branches, location, signature,
# normalized body.
BODY=7

if [ "${DO_CROSS}" -eq 1 ]; then
  printf "${BOLD}# the same body in two modules — extract-to-atlas-lib candidates${RESET}\n"
  # Each group is emitted as one line with \001 where the newlines belong, so
  # that sort orders whole groups instead of scattering their member lines.
  awk -F'\t' -v body="${BODY}" '
    {
      modules[$body] = modules[$body] " " $1
      sites[$body] = sites[$body] sprintf("\001    %-11s %-50s %s", $1, $5, substr($6, 1, 68))
    }
    END {
      for (b in modules) {
        n = split(modules[b], seen, " ")
        distinct = ""
        for (i = 1; i <= n; i++) {
          if (seen[i] != "" && index(distinct, seen[i]) == 0) {
            distinct = distinct " " seen[i]
          }
        }
        if (split(distinct, d, " ") > 1) {
          printf "%s%s\n", substr(distinct, 2), sites[b]
        }
      }
    }
  ' "${TMP}/funcs" | sort | tr '\001' '\n'
  echo
fi

if [ "${DO_WITHIN}" -eq 1 ]; then
  printf "${BOLD}# the same body twice in one module — local deduplication candidates${RESET}\n"
  awk -F'\t' -v body="${BODY}" '
    {
      key = $1 "\t" $body
      count[key]++
      sites[key] = sites[key] sprintf("\001    %-50s %s", $5, substr($6, 1, 68))
    }
    END {
      for (k in count) {
        if (count[k] > 1) {
          split(k, parts, "\t")
          printf "%s: %d copies%s\n", parts[1], count[k], sites[k]
        }
      }
    }
  ' "${TMP}/funcs" | sort | tr '\001' '\n'
  echo
fi

if [ "${DO_HANDROLLED}" -eq 1 ]; then
  printf "${BOLD}# written by hand where atlas-lib owns the primitive${RESET}\n"
  printf "${DIM}Read the package before replacing a hit: go doc github.com/simplyblock/atlas/<pkg>${RESET}\n\n"
  # `atlas package ~ what it replaces ~ regex`; the separator is a tilde because
  # every regex here is free to use alternation.
  PATTERNS='nqn~an NQN spelled out as a format string~nqn\.(2014-08|2023-02)\.io\.simplyblock
lvol~a volume handle split by hand~strings\.Split\([^)]*, ?":"\)
errs/class~an error classified by string or status code~strings\.Contains\(err\.Error\(\)|StatusCode == [45][0-9][0-9]
statemachine~a phase advanced by a hand-rolled switch~switch [^{]*(Phase|SubPhase)
nvme~sysfs read directly~"/sys/(class|devices)
nvmeof~nvme-cli shelled out to~"nvme", ?"(connect|disconnect|list|list-subsys|discover)"
controlplane~a second control-plane client~webapi\.|pkg/util/nvmf'
  printf '%s\n' "${PATTERNS}" | while IFS='~' read -r pkg what regex; do
    [ -z "${pkg}" ] && continue
    hits="$(grep -rnE "${regex}" operator csi-driver --include='*.go' 2>/dev/null \
      | grep -v -e '\.gen\.go:' -e 'zz_generated' || true)"
    [ "${WITH_TESTS}" -eq 1 ] || hits="$(printf '%s\n' "${hits}" | grep -v '_test\.go:' || true)"
    count="$(printf '%s\n' "${hits}" | grep -c . || true)"
    printf "%-14s %-46s %4s\n" "${pkg}" "${what}" "${count}"
    printf '%s\n' "${hits}" | grep . | head -6 | awk '{ printf "    %s\n", substr($0, 1, 130) }'
  done
fi
