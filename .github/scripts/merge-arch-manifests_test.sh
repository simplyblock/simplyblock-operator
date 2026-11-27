#!/usr/bin/env bash
# Regression test for merge-arch-manifests.sh, run against a stub Docker CLI so
# it needs no registry. Lives beside the script because no shell-test harness
# exists in this repository. Run: .github/scripts/merge-arch-manifests_test.sh
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/merge-arch-manifests.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }

# The stub's tag read returns the list from before the push, as a registry with
# read-after-write lag does. A digest read finds only what was pushed.
# STUB_PUSH_DIFFERS makes a push store other bytes than the dry run printed.
mkdir -p "$work/bin"
cat > "$work/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
manifest() { printf '{\n  "manifests": [\n'; printf '    "%s"\n' "${@##*@}"; printf '  ]\n}'; }
shift 2 # buildx imagetools
cmd="$1"; shift
case "$cmd" in
  create)
    dry=0; srcs=()
    while [ $# -gt 0 ]; do
      case "$1" in --dry-run) dry=1 ;; -t) shift ;; *) srcs+=("$1") ;; esac
      shift
    done
    if [ "$dry" = 1 ]; then manifest "${srcs[@]}"; echo; exit 0; fi
    [ -z "${STUB_PUSH_DIFFERS:-}" ] || srcs+=("x@sha256:extra")
    echo "sha256:$(manifest "${srcs[@]}" | sha256)" >> "$STUB_STATE/pushed"
    ;;
  inspect)
    ref="$1"
    if [[ "$ref" == *@sha256:* ]]; then
      grep -qx "${ref##*@}" "$STUB_STATE/pushed" || { echo "not found: $ref" >&2; exit 1; }
    else
      echo '{"digest":"sha256:stale"}'
    fi
    ;;
esac
STUB
chmod +x "$work/bin/docker"

mkdir -p "$work/digests"
echo "sha256:aaaa" > "$work/digests/amd64"
echo "sha256:bbbb" > "$work/digests/arm64"
tags=$'docker.io/acme/img:br\nregistry.example/acme/img:br'
expected="sha256:$(printf '{\n  "manifests": [\n    "sha256:aaaa"\n    "sha256:bbbb"\n  ]\n}' | sha256)"

# run <case> [env...]: runs the script against a fresh stub state, prints its exit code.
run() {
  local dir="$work/$1"; shift
  mkdir -p "$dir"; : > "$dir/pushed"
  env "$@" STUB_STATE="$dir" GITHUB_OUTPUT="$dir/out" PATH="$work/bin:$PATH" TAGS="$tags" \
    "$script" "$work/digests" > "$dir/log" 2>&1 && echo 0 || echo $?
}

failed=0

# The digest reported is the list just pushed, not what a lagging tag read returns.
rc="$(run fresh)"
got="$(sed -n 's/^digest=//p' "$work/fresh/out")"
if [ "$rc" = 0 ] && [ "$got" = "$expected" ]; then
  echo "PASS: reports the pushed list's digest"
else
  echo "FAIL: exit $rc, digest=${got:-<none>}, want exit 0 and $expected"
  sed 's/^/  | /' "$work/fresh/log"
  failed=1
fi

# A push that stored other bytes than the computed digest fails the step rather
# than handing preflight a digest nobody pushed.
rc="$(run mismatch STUB_PUSH_DIFFERS=1)"
if [ "$rc" != 0 ]; then
  echo "PASS: fails when the pushed list is not the computed one"
else
  echo "FAIL: exit 0 although the pushed list differs from the computed digest"
  failed=1
fi

exit "$failed"
