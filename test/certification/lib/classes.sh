# Finds the StorageClass and VolumeSnapshotClass a certification run tests, for
# the runners of every level, which source this file.
#
# The classes under test are found by their driver unless named: the operator
# names its StorageClass after the storage cluster, so the name changes with
# every deployment. The suite's own copies (e2e-<namespace>-e2e-sc<suffix>) are
# not candidates, because a failed test can leave one behind. More than one
# candidate is a choice a runner does not make.

DRIVER="csi.simplyblock.io"

# find_class prints the one object of kind whose field names the driver, or says
# why there is not exactly one and returns 1.
find_class() {
  local kind="$1" field="$2" names
  names="$(oc get "$kind" -o jsonpath="{range .items[?(@.$field==\"$DRIVER\")]}{.metadata.name}{\"\\n\"}{end}")" ||
    return 1
  names="$(grep -v '^e2e-' <<<"$names" || true)"
  if [[ -z "$names" ]]; then
    echo "no $kind uses $DRIVER" >&2
    return 1
  fi
  if [[ "$(wc -l <<<"$names")" -ne 1 ]]; then
    echo "several $kind objects use $DRIVER; name the one under test:" >&2
    echo "$names" | sed 's/^/  /' >&2
    return 1
  fi
  echo "$names"
}
