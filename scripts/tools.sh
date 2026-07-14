#!/usr/bin/env bash
#
# Reusable dev-tool downloader for the simplyblock monorepo.
#
# Installs pinned helper tools (golangci-lint, kustomize, yq, controller-gen,
# setup-envtest, ...) into <repo-root>/.bin, shared by all components
# (csi-driver, operator, atlas-lib).
#
# Two backends, one interface:
#   * "binary" tools that ship prebuilt release archives are downloaded and
#     verified against a pinned sha256 checksum from scripts/tools.lock. This is
#     the path that makes builds reproducible.
#   * "go" tools that only ship as Go modules (controller-gen, setup-envtest)
#     are installed via `go install pkg@version`, then verified against a
#     pinned Go module checksum -- the `h1:` sum read back from the built
#     binary with `go version -m`. That reproducibly detects a re-pointed or
#     modified version tag without depending on the toolchain-specific binary
#     bytes (which legitimately vary by Go version / OS / arch).
#
# Binaries are stored version-suffixed (e.g. .bin/golangci-lint-v2.11.4) with a
# stable unversioned symlink (.bin/golangci-lint) pointing at the active one, so
# switching pinned versions is atomic and cheap.
#
# Tool metadata (backend, download URL, pinned version, ...) is data-driven from
# scripts/tools.manifest -- adding a tool needs no change to this script.
#
# Usage:
#   scripts/tools.sh install <tool> [version]   # ensure <tool> in .bin (version
#                                               # defaults to the manifest pin)
#   scripts/tools.sh lock    <tool> [version]   # record the pinned hash(es)
#   scripts/tools.sh version <tool>             # print the manifest-pinned version
#
# Environment:
#   TOOLS_BIN_DIR       override the install dir (default <repo-root>/.bin)
#   TOOLS_LOCK_FILE     override the lock file   (default scripts/tools.lock)
#   TOOLS_MANIFEST_FILE override the manifest    (default scripts/tools.manifest)
#   TOOLS_STRICT=1      fail (don't warn) when a tool has no pinned hash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="${TOOLS_BIN_DIR:-$REPO_ROOT/.bin}"
LOCK_FILE="${TOOLS_LOCK_FILE:-$SCRIPT_DIR/tools.lock}"
MANIFEST_FILE="${TOOLS_MANIFEST_FILE:-$SCRIPT_DIR/tools.manifest}"
TOOLS_STRICT="${TOOLS_STRICT:-0}"

# Platforms for which `lock` records checksums.
SUPPORTED_PLATFORMS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

log() { printf '>> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

detect_os() {
  case "$(uname -s)" in
    Linux) echo linux ;;
    Darwin) echo darwin ;;
    *) die "unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

download() {
  # $1 url, $2 dest
  curl --fail --silent --show-error --location --retry 3 -o "$2" "$1"
}

# manifest_get <tool> <field> -> prints the value (empty if absent)
manifest_get() {
  [ -f "$MANIFEST_FILE" ] || die "manifest not found: $MANIFEST_FILE"
  awk -v key="$1.$2" '
    { k=$0; sub(/[ \t]*=.*/, "", k); sub(/^[ \t]+/, "", k); sub(/[ \t]+$/, "", k) }
    k==key { v=substr($0, index($0,"=")+1); sub(/^[ \t]+/, "", v); sub(/[ \t]+$/, "", v); print v; exit }
  ' "$MANIFEST_FILE"
}

# subst <template> <version> <version_nov> <os> <arch>
subst() {
  local s="$1"
  s="${s//\{version_nov\}/$3}"
  s="${s//\{version\}/$2}"
  s="${s//\{os\}/$4}"
  s="${s//\{arch\}/$5}"
  printf '%s' "$s"
}

# resolve_version <tool> <version-or-empty> -> the version to use
resolve_version() {
  local name="$1" version="$2"
  if [ -z "$version" ]; then
    version="$(manifest_get "$name" version)"
    [ -n "$version" ] || die "no version given for $name and none pinned in $(basename "$MANIFEST_FILE")"
  fi
  printf '%s' "$version"
}

# tool_meta <name> <version> <os> <arch>
# Reads scripts/tools.manifest and populates: BACKEND (binary|go), KIND
# (tar.gz|raw), URL, INNER, GOPKG.
tool_meta() {
  local name="$1" version="$2" os="$3" arch="$4"
  local nov="${version#v}" # version without leading "v"
  BACKEND="$(manifest_get "$name" backend)"
  [ -n "$BACKEND" ] || die "unknown tool: $name (not defined in $(basename "$MANIFEST_FILE"))"
  KIND="" URL="" INNER="" GOPKG=""
  case "$BACKEND" in
    binary)
      KIND="$(manifest_get "$name" kind)"
      URL="$(subst "$(manifest_get "$name" url)" "$version" "$nov" "$os" "$arch")"
      INNER="$(subst "$(manifest_get "$name" inner)" "$version" "$nov" "$os" "$arch")"
      [ -n "$URL" ] || die "$name: missing 'url' in $(basename "$MANIFEST_FILE")"
      ;;
    go)
      GOPKG="$(manifest_get "$name" package)"
      [ -n "$GOPKG" ] || die "$name: missing 'package' in $(basename "$MANIFEST_FILE")"
      ;;
    *)
      die "$name: invalid backend '$BACKEND' in $(basename "$MANIFEST_FILE")"
      ;;
  esac
}

# lock_lookup <name> <version> <os> <arch> -> prints sha256 (empty if absent)
lock_lookup() {
  [ -f "$LOCK_FILE" ] || return 0
  awk -v n="$1" -v v="$2" -v o="$3" -v a="$4" \
    '$1==n && $2==v && $3==o && $4==a {print $5; exit}' "$LOCK_FILE"
}

# lock_set <name> <version> <os> <arch> <hash>
# Replaces the row for this key (if any), then rewrites the file as the comment
# header (original order) followed by all data rows sorted. Only data rows are
# sorted; the header block is left intact.
lock_set() {
  local header="$WORK_DIR/lock.header" data="$WORK_DIR/lock.data"
  : >"$header"
  : >"$data"
  if [ -f "$LOCK_FILE" ]; then
    grep -E '^[[:space:]]*(#|$)' "$LOCK_FILE" >"$header" || true
    grep -vE '^[[:space:]]*(#|$)' "$LOCK_FILE" | grep -vE "^$1 $2 $3 $4 " >"$data" || true
  fi
  printf '%s %s %s %s %s\n' "$1" "$2" "$3" "$4" "$5" >>"$data"
  LC_ALL=C sort -o "$data" "$data"
  cat "$header" "$data" >"$LOCK_FILE"
}

# go_mod_hash <binary> -> prints the main module's go.sum h1: checksum, read
# back from the compiled binary's embedded build info. This is the same content
# hash Go's checksum database protects, so it detects a re-pointed/tampered tag
# (supply-chain injection) independent of the toolchain-specific binary bytes.
go_mod_hash() {
  go version -m "$1" 2>/dev/null | awk '$1=="mod"{print $4; exit}'
}

# check_hash <name> <version> <os> <arch> <computed>
# Compares a freshly computed hash against the pinned value in the lock file.
# A mismatch is always fatal (tamper signal); a missing pin warns unless
# TOOLS_STRICT=1.
check_hash() {
  local name="$1" version="$2" os="$3" arch="$4" got="$5"
  local want
  want="$(lock_lookup "$name" "$version" "$os" "$arch")"
  if [ -z "$want" ]; then
    if [ "$TOOLS_STRICT" = "1" ]; then
      die "no pinned hash for $name $version $os/$arch (computed $got). Run: scripts/tools.sh lock $name $version"
    fi
    log "WARNING: no pinned hash for $name $version $os/$arch"
    log "         computed: $got"
    log "         record it with: scripts/tools.sh lock $name $version"
    return 0
  fi
  [ "$want" = "$got" ] || die "hash mismatch for $name $version $os/$arch: want $want, got $got"
  log "hash verified: $name $version $os/$arch"
}

ensure_symlink() {
  # $1 versioned file, $2 stable link. Relative target keeps .bin relocatable.
  ln -sf "$(basename "$1")" "$2"
}

install_binary() {
  local name="$1" version="$2"
  local os arch
  os="$(detect_os)"
  arch="$(detect_arch)"
  tool_meta "$name" "$version" "$os" "$arch"

  local versioned="$BIN_DIR/${name}-${version}"
  local link="$BIN_DIR/${name}"
  if [ -x "$versioned" ]; then
    ensure_symlink "$versioned" "$link"
    return 0
  fi

  mkdir -p "$BIN_DIR"
  local artifact="$WORK_DIR/${name}-artifact"
  log "downloading $name $version ($os/$arch)"
  download "$URL" "$artifact"
  check_hash "$name" "$version" "$os" "$arch" "sha256:$(sha256_of "$artifact")"

  case "$KIND" in
    tar.gz)
      local extract="$WORK_DIR/${name}-extract"
      mkdir -p "$extract"
      tar -xzf "$artifact" -C "$extract"
      install -m 0755 "$extract/$INNER" "$versioned"
      ;;
    raw)
      install -m 0755 "$artifact" "$versioned"
      ;;
    *)
      die "unhandled archive kind: $KIND"
      ;;
  esac
  ensure_symlink "$versioned" "$link"
  log "installed $name -> $(basename "$versioned")"
}

install_go() {
  local name="$1" version="$2"
  tool_meta "$name" "$version" "" ""
  [ -n "$version" ] || die "install $name: a version is required"

  local versioned="$BIN_DIR/${name}-${version}"
  local link="$BIN_DIR/${name}"
  if [ -x "$versioned" ]; then
    ensure_symlink "$versioned" "$link"
    return 0
  fi

  mkdir -p "$BIN_DIR"
  local gobin="$WORK_DIR/gobin"
  mkdir -p "$gobin"
  log "go install ${GOPKG}@${version}"
  GOBIN="$gobin" go install "${GOPKG}@${version}"
  local built="$gobin/${name}"
  # Verify the module checksum before trusting the binary (see go_mod_hash).
  check_hash "$name" "$version" module all "$(go_mod_hash "$built")"
  install -m 0755 "$built" "$versioned"
  ensure_symlink "$versioned" "$link"
  log "installed $name -> $(basename "$versioned")"
}

cmd_install() {
  [ $# -ge 1 ] && [ $# -le 2 ] || die "usage: scripts/tools.sh install <tool> [version]"
  local name="$1" version
  version="$(resolve_version "$name" "${2:-}")"
  tool_meta "$name" "$version" linux amd64 # resolve BACKEND
  case "$BACKEND" in
    binary) install_binary "$name" "$version" ;;
    go) install_go "$name" "$version" ;;
    *) die "no backend for $name" ;;
  esac
}

cmd_version() {
  [ $# -eq 1 ] || die "usage: scripts/tools.sh version <tool>"
  resolve_version "$1" ""
  echo
}

cmd_lock() {
  [ $# -ge 1 ] && [ $# -le 2 ] || die "usage: scripts/tools.sh lock <tool> [version]"
  local name="$1" version
  version="$(resolve_version "$name" "${2:-}")"
  tool_meta "$name" "$version" linux amd64
  mkdir -p "$(dirname "$LOCK_FILE")"

  if [ "$BACKEND" = "go" ]; then
    local gobin="$WORK_DIR/lock-gobin"
    mkdir -p "$gobin"
    log "go install ${GOPKG}@${version} (for module hash)"
    GOBIN="$gobin" go install "${GOPKG}@${version}"
    local h
    h="$(go_mod_hash "$gobin/$name")"
    [ -n "$h" ] || die "could not read module hash for $name $version"
    lock_set "$name" "$version" module all "$h"
    log "  $h"
    return 0
  fi

  local plat os arch f sum
  for plat in "${SUPPORTED_PLATFORMS[@]}"; do
    os="${plat%/*}"
    arch="${plat#*/}"
    tool_meta "$name" "$version" "$os" "$arch"
    f="$WORK_DIR/lock-$name-$os-$arch"
    log "fetching $name $version $os/$arch"
    if ! download "$URL" "$f"; then
      log "WARNING: could not download $URL (skipping $os/$arch)"
      continue
    fi
    sum="sha256:$(sha256_of "$f")"
    lock_set "$name" "$version" "$os" "$arch" "$sum"
    log "  $sum"
  done
}

main() {
  local cmd="${1:-}"
  shift || true
  case "$cmd" in
    install) cmd_install "$@" ;;
    lock) cmd_lock "$@" ;;
    version) cmd_version "$@" ;;
    *) die "usage: scripts/tools.sh {install|lock|version} <tool> [version]" ;;
  esac
}

main "$@"