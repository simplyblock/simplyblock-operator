#!/usr/bin/env bash
#
# Runs the house style quality gates over the design documents, the test plans,
# and the prose in source comments.
#
# Every gate runs, even when an earlier one failed, so a single run reports all
# problems at once. The script exits non-zero if any gate failed.
#
# Usage:
#   quality-gate.sh                            # all gates, over operator/docs
#   quality-gate.sh spelling                   # the named gates only
#   quality-gate.sh --changed                  # only the files changed vs. HEAD
#   quality-gate.sh --paths a.md b.go          # the given files or directories
#   quality-gate.sh voice --changed            # both selections combined
#
# Every error of every gate is repeated at the end of a run, all of them, so that
# the list to work through is in one place and no warning has to be read to find
# it.
#
# To add a gate, append its name to ALL_GATES and implement the matching
# gate_<name> function together with its gate_<name>_description.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The scripts live under .claude/skills/house-style/scripts/, so the repository
# root is looked up rather than derived from the script location.
if [ -z "${HOUSE_STYLE_ROOT:-}" ]; then
  # "A || B && C" groups as "(A || B) && C", so the fallback needs its own subshell.
  REPO_ROOT="$(git -C "${SCRIPT_DIR}" rev-parse --show-toplevel 2>/dev/null || (cd "${SCRIPT_DIR}/../../../.." && pwd))"
else
  REPO_ROOT="${HOUSE_STYLE_ROOT}"
fi
export HOUSE_STYLE_ROOT="${REPO_ROOT}"

PYTHON="${PYTHON:-python3}"

if [ -t 1 ]; then
  BOLD="\033[1m"
  RED="\033[31m"
  GREEN="\033[32m"
  RESET="\033[0m"
else
  BOLD=""
  RED=""
  GREEN=""
  RESET=""
fi

# The available gates, in execution order.
ALL_GATES=(spelling terminology american identifiers prose voice punctuation tables)

gate_spelling_description="Brand name spelling and casing"
gate_spelling() {
  "${PYTHON}" "${SCRIPT_DIR}/check-simplyblock-spelling.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

gate_terminology_description="Spelling of product names, projects and acronyms"
gate_terminology() {
  "${PYTHON}" "${SCRIPT_DIR}/check-terminology.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

gate_american_description="American English spelling"
gate_american() {
  "${PYTHON}" "${SCRIPT_DIR}/check-american-english.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

gate_identifiers_description="American English in declared names (Go, Python, YAML)"
gate_identifiers() {
  "${PYTHON}" "${SCRIPT_DIR}/check-identifiers.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

gate_prose_description="Misspellings, repeated words, and the comma of an abbreviation"
gate_prose() {
  "${PYTHON}" "${SCRIPT_DIR}/check-prose.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

gate_voice_description="Impersonal voice, without addressing the reader or the author"
gate_voice() {
  "${PYTHON}" "${SCRIPT_DIR}/check-voice.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

gate_punctuation_description="Oxford comma, semicolon, em dash, list item punctuation, and the placement of a mark"
gate_punctuation() {
  "${PYTHON}" "${SCRIPT_DIR}/check-punctuation.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

gate_tables_description="Markdown table alignment and the separator row under a header"
gate_tables() {
  "${PYTHON}" "${SCRIPT_DIR}/check-tables.py" "${TARGETS[@]+"${TARGETS[@]}"}"
}

ensure_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Command $1 not found. Please install it." >&2
    exit 1
  fi
}

ensure_command "${PYTHON}"

gates=()
TARGETS=()
changed_only=false

while [ $# -gt 0 ]; do
  case "$1" in
    --changed)
      changed_only=true
      shift
      ;;
    --paths)
      shift
      while [ $# -gt 0 ] && [[ "$1" != --* ]]; do
        TARGETS+=("$1")
        shift
      done
      ;;
    -h|--help)
      sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      gates+=("$1")
      shift
      ;;
  esac
done

if [ ${#gates[@]} -eq 0 ]; then
  gates=("${ALL_GATES[@]}")
fi

# The files touched in the working tree and against the merge base, so that a
# gate run reviews the work in progress rather than the whole repository.
if [ "${changed_only}" = true ]; then
  # "mapfile" is bash 4, and macOS still ships bash 3.2 as /bin/bash.
  changed=()
  while IFS= read -r file; do
    [ -n "${file}" ] && changed+=("${file}")
  done < <(
    {
      git -C "${REPO_ROOT}" diff --name-only --diff-filter=ACMR HEAD
      git -C "${REPO_ROOT}" ls-files --others --exclude-standard
    } | grep -Ei '\.(md|go|py|ya?ml)$' | sort -u
  )
  if [ ${#changed[@]} -eq 0 ]; then
    echo "No changed Markdown, Go, Python, or YAML files to check."
    exit 0
  fi
  for file in "${changed[@]}"; do
    [ -f "${REPO_ROOT}/${file}" ] && TARGETS+=("${REPO_ROOT}/${file}")
  done
fi

for gate in "${gates[@]}"; do
  if ! declare -F "gate_${gate}" >/dev/null; then
    echo "Unknown quality gate: ${gate}" >&2
    echo "Available gates: ${ALL_GATES[*]}" >&2
    exit 2
  fi
done

cd "${REPO_ROOT}"

# Every gate writes into its own log, so that its errors can be listed again once
# all gates have run. Without that, the first failure of a long run has scrolled
# away by the time the last gate is done.
LOG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/quality-gate.XXXXXX")"
cleanup() {
  rm -rf "${LOG_DIR}"
}
trap cleanup EXIT

failed_count=0
failed_gates=()
error_count=0

for gate in "${gates[@]}"; do
  description="gate_${gate}_description"
  echo ""
  echo -e "${BOLD}▶ ${gate}: ${!description}${RESET}"

  # "pipefail" is set, so the status of the gate survives the pipe into tee.
  if "gate_${gate}" 2>&1 | tee "${LOG_DIR}/${gate}.log"; then
    echo -e "${GREEN}✔ ${gate} passed${RESET}"
  else
    gate_errors="$(grep -c "^ERROR" "${LOG_DIR}/${gate}.log" || true)"
    if [ "${gate_errors}" -gt 0 ]; then
      echo -e "${RED}✘ ${gate} failed with ${gate_errors} error(s)${RESET}"
    else
      echo -e "${RED}✘ ${gate} failed without reporting a finding${RESET}"
    fi
    failed_count=$((failed_count + 1))
    failed_gates+=("${gate}")
    error_count=$((error_count + gate_errors))
  fi
done

echo ""
if [ "${failed_count}" -eq 0 ]; then
  echo -e "${GREEN}All ${#gates[@]} quality gate(s) passed.${RESET}"
  exit 0
fi

# The collected errors of the whole run, so that the list to work through is in
# one place instead of spread over the output of every gate.
if [ "${error_count}" -gt 0 ]; then
  echo -e "${BOLD}${RED}━━ ${error_count} error(s) in ${failed_count} of ${#gates[@]} quality gate(s) ━━${RESET}"
else
  echo -e "${BOLD}${RED}━━ ${failed_count} of ${#gates[@]} quality gate(s) failed ━━${RESET}"
fi
for gate in "${failed_gates[@]}"; do
  echo ""
  echo -e "${BOLD}${gate}:${RESET}"
  if grep -q "^ERROR" "${LOG_DIR}/${gate}.log"; then
    # Every error, never a warning: this list is the work to do. The excerpt of a
    # finding stays in the output of the gate above.
    grep "^ERROR" "${LOG_DIR}/${gate}.log"
  else
    # A gate that fails without reporting a finding did not run to its end.
    echo "  The gate itself failed, it reported no finding. Its last output was:"
    tail -n 20 "${LOG_DIR}/${gate}.log" | sed 's/^/  /'
  fi
done

echo ""
echo -e "${RED}${failed_count} of ${#gates[@]} quality gate(s) failed: ${failed_gates[*]}${RESET}"
exit 1
