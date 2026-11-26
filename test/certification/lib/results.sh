# Copies a run's results out of the pod that produced them, for the runners of
# every level, which source this file.
#
# The results come over as one archive with its checksum, because a stream
# through oc exec can end early without an error from either side: a tar piped
# straight through lost the end of a 250 MB result set that way, exit code
# included. A copy that does not match is fetched again.

# sha256_of prints a file's SHA-256, on Linux and on macOS.
sha256_of() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# fetch_results packs <source dir> in the pod's container into an archive under
# <scratch dir>, copies it into <local dir>, checks it against the pod's own
# checksum (three attempts), and unpacks it there. It returns 1 when no copy
# matched, leaving the archive in the pod for a copy by hand.
#
#   fetch_results <namespace> <pod> <container> <source dir> <scratch dir> <local dir>
fetch_results() {
  local ns="$1" pod="$2" container="$3" src="$4" scratch="$5" dest="$6" want attempt
  want="$(oc -n "$ns" exec "$pod" -c "$container" -- \
    sh -c "tar czf $scratch/results.tar.gz -C $src . && sha256sum $scratch/results.tar.gz" | cut -d' ' -f1)" ||
    return 1
  for attempt in 1 2 3; do
    oc -n "$ns" exec "$pod" -c "$container" -- cat "$scratch/results.tar.gz" >"$dest/.results.tar.gz" || true
    if [[ "$(sha256_of "$dest/.results.tar.gz")" == "$want" ]]; then
      tar xzf "$dest/.results.tar.gz" -C "$dest"
      rm -f "$dest/.results.tar.gz"
      return 0
    fi
    echo "results copy $attempt did not match its checksum" >&2
  done
  rm -f "$dest/.results.tar.gz"
  return 1
}
