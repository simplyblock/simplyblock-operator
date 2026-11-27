# Claims the namespace a run works in, for the runners of every level, which
# source this file.
#
# One run at a time per level: a namespace that exists means another run, or
# one that was not cleaned up, and the run is refused. A namespace still being
# deleted is the previous run's cleanup, which deletes without waiting, so a
# run started right after another waits for it instead of failing.

# claim_run_namespace waits up to <seconds> (default 300) for <namespace> to
# finish terminating, and returns 1 when it exists and is not terminating, or
# is still there at the deadline.
#
#   claim_run_namespace <namespace> [seconds]
claim_run_namespace() {
  local ns="$1" deadline=$((SECONDS + ${2:-300})) phase
  while phase="$(oc get namespace "$ns" -o jsonpath='{.status.phase}' 2>/dev/null)"; do
    if [[ "$phase" != Terminating ]]; then
      echo "namespace $ns exists: another run is active or was not cleaned up" >&2
      return 1
    fi
    if [[ $SECONDS -ge $deadline ]]; then
      echo "namespace $ns is still being deleted; the previous run's cleanup did not finish" >&2
      return 1
    fi
    echo "waiting for the previous run's namespace $ns to be deleted"
    sleep 10
  done
  return 0
}
