#!/usr/bin/env bash
#
# Counts the mechanism in a scope of Go code: function lengths, indentation,
# branches, duplicated bodies, oversized files, suppressions, and the patterns
# that a shared atlas-lib primitive already covers.
#
# It exists so that the mechanism gate of the code-cleanup skill can be a
# measurement rather than an impression. A cleanup records a baseline before it
# starts and recounts the same scope with the same method afterwards; anything
# else cannot tell a removal from a relocation.
#
# Usage:
#   measure.sh                                  # the whole repository
#   measure.sh --paths operator/internal/controller
#   measure.sh --changed                        # only files changed vs. HEAD
#   measure.sh --paths atlas-lib --with-tests   # include _test.go files
#   measure.sh --paths operator --baseline /tmp/before.tsv
#   measure.sh --paths operator --compare /tmp/before.tsv
#   measure.sh --paths operator --detail        # the per-function lists too
#
# The metric block is tab-separated `name<TAB>value` lines and is the only part
# --compare reads, so detail sections can be added freely.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AWK_LIB="${SCRIPT_DIR}/lib/gofuncs.awk"

if [ -z "${CLEANUP_ROOT:-}" ]; then
  # "A || B && C" groups as "(A || B) && C", so the fallback needs its own subshell.
  REPO_ROOT="$(git -C "${SCRIPT_DIR}" rev-parse --show-toplevel 2>/dev/null || (cd "${SCRIPT_DIR}/../../../.." && pwd))"
else
  REPO_ROOT="${CLEANUP_ROOT}"
fi

# Lengths at which a function is worth a look and at which it needs a reason.
LONG_FUNC="${LONG_FUNC:-60}"
VERY_LONG_FUNC="${VERY_LONG_FUNC:-100}"
# Leading tabs: 5 means four nested blocks inside a function body.
DEEP_INDENT="${DEEP_INDENT:-5}"
# A file long enough that its subject is worth re-reading.
BIG_FILE="${BIG_FILE:-600}"
# Below this a shared body is a getter or a one-line delegation, not a copy.
MIN_DUP_LINES="${MIN_DUP_LINES:-5}"

PATHS=()
WITH_TESTS=0
CHANGED=0
DETAIL=0
BASELINE=""
COMPARE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --paths)
      shift
      while [ $# -gt 0 ] && [ "${1#--}" = "$1" ]; do
        PATHS+=("$1")
        shift
      done
      ;;
    --changed) CHANGED=1; shift ;;
    --with-tests) WITH_TESTS=1; shift ;;
    --detail) DETAIL=1; shift ;;
    --baseline) BASELINE="$2"; shift 2 ;;
    --compare) COMPARE="$2"; shift 2 ;;
    -h|--help) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "measure.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done

cd "${REPO_ROOT}" || exit 1
[ ${#PATHS[@]} -eq 0 ] && PATHS=(atlas-lib operator csi-driver)

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
FILES="${TMP}/files"
FUNCS="${TMP}/funcs"
METRICS="${TMP}/metrics"

# The file list. Generated output is never a cleanup target, so it is excluded
# here rather than in every metric below.
if [ "${CHANGED}" -eq 1 ]; then
  {
    git diff --name-only HEAD -- "${PATHS[@]}"
    git diff --name-only --cached -- "${PATHS[@]}"
    git ls-files --others --exclude-standard -- "${PATHS[@]}"
  } | sort -u | grep '\.go$' > "${FILES}.raw" || true
else
  find "${PATHS[@]}" -name '*.go' -type f > "${FILES}.raw" 2>/dev/null || true
fi

grep -v -e '\.gen\.go$' -e 'zz_generated' -e '/vendor/' -e '/third_party/' "${FILES}.raw" \
  > "${FILES}.nogen" || true
if [ "${WITH_TESTS}" -eq 1 ]; then
  mv "${FILES}.nogen" "${FILES}"
else
  grep -v '_test\.go$' "${FILES}.nogen" > "${FILES}" || true
fi

# Files may have been deleted in a --changed run; keep only what still exists.
: > "${FILES}.live"
while IFS= read -r f; do
  [ -f "${f}" ] && printf '%s\n' "${f}" >> "${FILES}.live"
done < "${FILES}"
mv "${FILES}.live" "${FILES}"

if [ ! -s "${FILES}" ]; then
  # An empty --changed scope is the normal answer to "did this commit touch Go
  # code", so it is reported and not treated as a failure.
  echo "measure.sh: no Go files in scope"
  [ "${CHANGED}" -eq 1 ] && exit 0
  exit 1
fi

xargs awk -f "${AWK_LIB}" < "${FILES}" > "${FUNCS}" 2>/dev/null

metric() { printf '%s\t%s\n' "$1" "$2" >> "${METRICS}"; }
: > "${METRICS}"

metric files "$(wc -l < "${FILES}" | tr -d ' ')"
metric lines "$(xargs wc -l < "${FILES}" | awk '$2 != "total" { n += $1 } END { print n + 0 }')"
metric funcs "$(wc -l < "${FUNCS}" | tr -d ' ')"

awk -F'\t' -v long="${LONG_FUNC}" -v vlong="${VERY_LONG_FUNC}" -v deep="${DEEP_INDENT}" '
  { total += $1; if ($1 > max) max = $1 }
  $1 >= long  { l++ }
  $1 >= vlong { v++ }
  $2 >= deep  { d++ }
  $3 >= 15    { b++ }
  END {
    printf "func_lines\t%d\nmax_func_len\t%d\nfuncs_long\t%d\nfuncs_very_long\t%d\nfuncs_deep\t%d\nfuncs_branchy\t%d\n",
      total, max, l + 0, v + 0, d + 0, b + 0
  }
' "${FUNCS}" >> "${METRICS}"

# Duplicated bodies. Two functions whose normalized bodies are identical are a
# copy whatever their signatures say, which is what makes this cheap and exact.
awk -F'\t' -v min="${MIN_DUP_LINES}" '$1 >= min { print $6 }' "${FUNCS}" \
  | sort | uniq -c | awk '$1 > 1' > "${TMP}/dups"
metric dup_bodies "$(wc -l < "${TMP}/dups" | tr -d ' ')"
metric dup_funcs "$(awk '{ n += $1 } END { print n + 0 }' "${TMP}/dups")"

xargs wc -l < "${FILES}" | awk -v big="${BIG_FILE}" '
  $2 != "total" && $1 >= big { n++ } END { printf "files_big\t%d\n", n + 0 }' >> "${METRICS}"

count() { xargs grep -cE "$2" < "${FILES}" 2>/dev/null | awk -F: -v n="$1" '{ s += $NF } END { printf "%s\t%d\n", n, s + 0 }'; }
{
  count nolint 'nolint'
  count todo '(TODO|FIXME|XXX|HACK)'
  # Comment lines that are commented-out code rather than prose.
  count commented_code '^[ \t]*//[ \t]*(if |for |return|func |} |err (:|=)|fmt\.|log\.|//)'
  count atlas_imports '"github.com/simplyblock/atlas(/[a-z/]+)?"'
} >> "${METRICS}"

# Hand-rolled equivalents of a primitive atlas-lib already owns. Each line is
# `metric ~ package that supplants it ~ regex`; the separator is a tilde because
# every regex here is free to use alternation.
HANDROLLED='handrolled_nqn~nqn~nqn\.(2014-08|2023-02)\.io\.simplyblock
handrolled_handle~lvol~strings\.Split\([^)]*, ?":"\)
handrolled_errclass~errs/class~strings\.Contains\(err\.Error\(\)|StatusCode == [45][0-9][0-9]
handrolled_phase_switch~statemachine~switch [^{]*(Phase|SubPhase)
handrolled_sysfs~nvme~"/sys/(class|devices)
handrolled_nvmecli~nvmeof~"nvme", ?"(connect|disconnect|list|list-subsys|discover)"
handrolled_cpclient~controlplane~webapi\.'
printf '%s\n' "${HANDROLLED}" | while IFS='~' read -r name _pkg regex; do
  [ -z "${name}" ] && continue
  count "${name}" "${regex}"
done >> "${METRICS}"
awk -F'\t' '$1 ~ /^handrolled_/ { n += $2 } END { printf "handrolled_total\t%d\n", n + 0 }' \
  "${METRICS}" > "${TMP}/handrolled_total"
cat "${TMP}/handrolled_total" >> "${METRICS}"

if [ -n "${BASELINE}" ]; then
  cp "${METRICS}" "${BASELINE}"
fi

BOLD=""; RED=""; GREEN=""; DIM=""; RESET=""
if [ -t 1 ]; then
  BOLD="\033[1m"; RED="\033[31m"; GREEN="\033[32m"; DIM="\033[2m"; RESET="\033[0m"
fi

printf "${BOLD}scope${RESET}  %s  ${DIM}(%s files, tests %s)${RESET}\n" \
  "${PATHS[*]}" "$(wc -l < "${FILES}" | tr -d ' ')" \
  "$([ "${WITH_TESTS}" -eq 1 ] && echo included || echo excluded)"
echo

if [ -n "${COMPARE}" ]; then
  if [ ! -f "${COMPARE}" ]; then
    echo "measure.sh: no such baseline: ${COMPARE}" >&2
    exit 2
  fi
  printf "${BOLD}%-22s %10s %10s %10s${RESET}\n" metric before after delta
  awk -F'\t' -v red="${RED}" -v green="${GREEN}" -v reset="${RESET}" '
    NR == FNR { before[$1] = $2; next }
    {
      b = ($1 in before) ? before[$1] : 0
      d = $2 - b
      color = (d > 0) ? red : (d < 0 ? green : "")
      printf "%-22s %10s %10s %s%+10d%s\n", $1, b, $2, color, d, (color == "" ? "" : reset)
    }
  ' "${COMPARE}" "${METRICS}"
  echo
  printf "${DIM}A cleanup that removed nothing shows every delta at zero: that is a\n"
  printf "relocation, not a reduction. Growth in one metric is only paid for by a\n"
  printf "larger fall in another, named in the report.${RESET}\n"
else
  awk -F'\t' '{ printf "%-22s %8s\n", $1, $2 }' "${METRICS}"
fi

if [ "${DETAIL}" -eq 1 ]; then
  echo
  printf "${BOLD}# functions of %s lines or more${RESET}\n" "${LONG_FUNC}"
  awk -F'\t' -v long="${LONG_FUNC}" '$1 >= long { printf "%5d  %-52s %s\n", $1, $4, substr($5, 1, 76) }' \
    "${FUNCS}" | sort -rn | head -40

  echo
  printf "${BOLD}# functions indenting to %s tabs or more${RESET}\n" "${DEEP_INDENT}"
  awk -F'\t' -v deep="${DEEP_INDENT}" '$2 >= deep { printf "%5d tabs %4d branches  %-46s %s\n", $2, $3, $4, substr($5, 1, 60) }' \
    "${FUNCS}" | sort -rn | head -25

  echo
  printf "${BOLD}# duplicated bodies (%s lines or more)${RESET}\n" "${MIN_DUP_LINES}"
  awk -F'\t' -v min="${MIN_DUP_LINES}" '
    NR == FNR { sub(/^ *[0-9]+ /, ""); dup[$0] = 1; next }
    $1 >= min && ($6 in dup) { printf "%5d  %-52s %s\n", $1, $4, substr($5, 1, 70) }
  ' "${TMP}/dups" "${FUNCS}" | sort -k2

  echo
  printf "${BOLD}# files of %s lines or more${RESET}\n" "${BIG_FILE}"
  xargs wc -l < "${FILES}" | awk -v big="${BIG_FILE}" '$2 != "total" && $1 >= big' | sort -rn
fi
