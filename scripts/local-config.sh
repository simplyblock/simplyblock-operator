#!/usr/bin/env bash
#
# Reads and writes the settings that belong to one machine rather than to this
# repository.
#
# What needs them today is the path to an sbcli checkout, which the openapi
# targets export the control-plane spec from. Where that lives cannot have a
# default in a tracked file, and hand-writing the file it goes in is a step that
# has to be explained to everybody once, so this owns it instead.
#
# Settings are named by a section and a name, so that the next one does not need
# a mechanism of its own:
#
#   repo.sbcli: the path to an sbcli checkout
#   venv.interpreter: the interpreter the openapi environment is built with
#
# They are stored in local.mk, which is untracked and included by the root
# Makefile, as one make variable per setting (repo.sbcli becomes REPO_SBCLI).
# That is an implementation detail of this script: nothing else should read or
# write that file by hand.
#
# Usage:
#   scripts/local-config.sh repo sbcli set ../sbcli   # record a setting
#   scripts/local-config.sh repo sbcli get            # print it, or nothing
#   scripts/local-config.sh repo sbcli unset          # forget it
#   scripts/local-config.sh list [<section>]          # print every setting
#   scripts/local-config.sh detect [--clone]          # find sbcli and record it

set -euo pipefail

readonly SBCLI_REMOTE="https://github.com/simplyblock-io/sbcli.git"
# What makes a directory an sbcli checkout rather than a directory named sbcli:
# the app the export imports, and the requirement set it is imported against.
readonly SBCLI_MARKERS=("simplyblock_web/app.py" "requirements.txt")

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
config="${root}/local.mk"
relative_config="${config#"${root}"/}"

usage() {
  sed -n '3,29p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

# variable is the make variable one setting is stored as. Anything that cannot
# appear in a make variable name becomes an underscore, so a section and a name
# always produce a usable one.
variable() {
  printf '%s_%s\n' "$1" "$2" | tr '[:lower:]' '[:upper:]' | tr -c 'A-Z0-9_\n' '_'
}

read_setting() {
  local var
  var="$(variable "$1" "$2")"
  [ -f "$config" ] || return 0
  sed -n "s/^${var}[[:space:]]*:*=[[:space:]]*//p" "$config" | tail -n 1
}

# write_setting replaces the one assignment it owns and keeps every other line,
# so a setting recorded by hand or by an earlier run survives.
write_setting() {
  local var value tmp
  var="$(variable "$1" "$2")"
  value="$3"
  tmp="$(mktemp)"
  {
    echo "# Settings for this clone alone, written by scripts/local-config.sh."
    [ -n "$value" ] && printf '%s := %s\n' "$var" "$value"
    if [ -f "$config" ]; then
      grep -v \
        -e "^${var}[[:space:]]*:*=" \
        -e '^# Settings for this clone alone, written by scripts/local-config\.sh\.$' \
        "$config" || true
    fi
  } > "$tmp"
  mv "$tmp" "$config"
}

# is_checkout reports whether dir looks like sbcli, naming what is missing when
# the caller pointed at it on purpose.
is_checkout() {
  local dir="$1" complain="${2:-false}" marker
  [ -d "$dir" ] || { "$complain" && echo "no directory at $dir" >&2; return 1; }
  for marker in "${SBCLI_MARKERS[@]}"; do
    if [ ! -e "$dir/$marker" ]; then
      "$complain" && echo "$dir is not an sbcli checkout: no $marker in it" >&2
      return 1
    fi
  done
  return 0
}

# mainRoot is the checkout the other checkouts sit beside, which is not this
# one when make is run from a linked worktree: a worktree lives inside the
# repository, so its own neighbors are the other worktrees. The common git
# directory belongs to the main checkout either way, so its parent is the answer
# in both.
mainRoot() {
  local common
  if common="$(git -C "$root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"; then
    dirname "$common"
  else
    printf '%s\n' "$root"
  fi
}

# candidates are the places a checkout is usually kept, in the order they are
# tried, with the duplicates a main checkout produces folded out. The
# in-repository one comes first because it is the one CI uses, so a machine that
# has both matches what CI would export.
candidates() {
  local main
  main="$(mainRoot)"
  printf '%s\n' \
    "${root}/.sbcli" \
    "${main}/.sbcli" \
    "$(dirname "$main")/sbcli" \
    "$(dirname "$root")/sbcli" \
    "${HOME}/git/sbcli" \
    "${HOME}/src/sbcli" \
    "${HOME}/dev/sbcli" \
    "${HOME}/projects/sbcli" \
    "${HOME}/Projects/sbcli" | awk '!seen[$0]++'
}

# describe says which branch and commit a checkout is on, because that is what
# an export from it means. The committed spec is exported from main, so a
# release branch is worth saying out loud rather than finding in a diff.
describe() {
  local dir="$1" branch commit
  branch="$(git -C "$dir" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown branch")"
  commit="$(git -C "$dir" rev-parse --short HEAD 2>/dev/null || echo "unknown commit")"
  echo "sbcli: $dir ($branch at $commit)"
  if [ "$branch" != "main" ]; then
    echo "note: CI exports the committed spec from main. Run 'make openapi-diff'" \
         "before 'make openapi-sync' to see what exporting from $branch changes."
  fi
}

detect() {
  local clone="${1:-false}" candidate found="" others=()
  while read -r candidate; do
    if is_checkout "$candidate"; then
      if [ -z "$found" ]; then found="$candidate"; else others+=("$candidate"); fi
    fi
  done < <(candidates)

  if [ -z "$found" ] && [ "$clone" = true ]; then
    echo "no sbcli checkout found, cloning into .sbcli"
    git clone --quiet "$SBCLI_REMOTE" "${root}/.sbcli"
    found="${root}/.sbcli"
  fi

  if [ -z "$found" ]; then
    echo "no sbcli checkout found. Looked in:" >&2
    candidates | sed 's/^/  /' >&2
    echo "Record one with 'scripts/local-config.sh repo sbcli set <dir>'," >&2
    echo "or have one cloned with 'scripts/local-config.sh detect --clone'." >&2
    return 1
  fi

  found="$(cd "$found" && pwd)"
  describe "$found"
  if [ ${#others[@]} -gt 0 ]; then
    echo "other checkouts found and not used: ${others[*]}"
  fi
  write_setting repo sbcli "$found"
  echo "recorded in ${relative_config}: repo.sbcli = $found"
}

list() {
  local section="${1:-}" prefix="" line var value
  [ -n "$section" ] && prefix="$(printf '%s' "$section" | tr '[:lower:]' '[:upper:]')_"
  [ -f "$config" ] || { echo "no settings recorded in ${relative_config}"; return 0; }
  while IFS= read -r line; do
    var="${line%%[[:space:]]*:*=*}"
    var="$(printf '%s' "$line" | sed -n 's/^\([A-Z0-9_]*\)[[:space:]]*:*=.*/\1/p')"
    [ -n "$var" ] || continue
    case "$var" in "${prefix}"*) ;; *) continue ;; esac
    value="$(printf '%s' "$line" | sed 's/^[A-Z0-9_]*[[:space:]]*:*=[[:space:]]*//')"
    # Back to the section and name the caller knows it by.
    printf '%s = %s\n' "$(printf '%s' "$var" | tr '[:upper:]_' '[:lower:].' )" "$value"
  done < "$config"
}

[ $# -gt 0 ] || { usage; exit 2; }

case "$1" in
  -h|--help) usage; exit 0 ;;
  list) list "${2:-}" ;;
  detect)
    case "${2:-}" in
      "") detect false ;;
      --clone) detect true ;;
      *) echo "unknown argument: $2" >&2; exit 2 ;;
    esac
    ;;
  *)
    [ $# -ge 3 ] || { usage; exit 2; }
    section="$1" name="$2" verb="$3"
    case "$verb" in
      get) read_setting "$section" "$name" ;;
      unset) write_setting "$section" "$name" ""; echo "unset ${section}.${name}" ;;
      set)
        [ $# -ge 4 ] || { echo "'set' needs a value" >&2; exit 2; }
        value="$4"
        # A repo setting names a directory, and pointing at one that is not
        # there is worth saying now rather than at the next export.
        if [ "$section" = "repo" ] && [ ! -d "$value" ]; then
          echo "warning: no directory at $value" >&2
        fi
        if [ "$section" = "repo" ] && [ "$name" = "sbcli" ] && [ -d "$value" ]; then
          is_checkout "$value" true || exit 1
          value="$(cd "$value" && pwd)"
          describe "$value"
        fi
        write_setting "$section" "$name" "$value"
        echo "recorded in ${relative_config}: ${section}.${name} = $value"
        ;;
      *) echo "unknown verb: $verb" >&2; exit 2 ;;
    esac
    ;;
esac
