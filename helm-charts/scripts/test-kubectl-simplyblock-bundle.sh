#!/usr/bin/env bash
# Behavioral tests for kubectl-simplyblock-bundle against a stub kubectl, so no
# cluster is needed. Lives beside the script it covers; run it directly.
#
# The stub logs every call it receives, which is how the cases check that the
# bundle only ever reads and never touches a Secret, and it answers from a
# per-case directory of fixtures:
#   runs.tsv        the OperatorOps: creationTimestamp, name, action, configRef
#   reports/<cm>    the report.json of each probe report ConfigMap
#   probe-pods      the probe pods' names, one per line
#   namespace       the namespace of the kubeconfig's current context
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/kubectl-simplyblock-bundle"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir "$WORK/bin"
cat > "$WORK/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$STUB_LOG"
tab=$'\t'
case "$1 $2" in
  "config view")
    [[ -f "$STUB_DIR/namespace" ]] && cat "$STUB_DIR/namespace"
    exit 0 ;;
  "get operatorops")
    if [[ "$3" == -n ]]; then
      [[ -f "$STUB_DIR/runs.tsv" ]] && cut -f1-3 "$STUB_DIR/runs.tsv"
      exit 0
    fi
    line="$(awk -F'\t' -v run="$3" '$2 == run' "$STUB_DIR/runs.tsv" 2>/dev/null)"
    if [[ -z "$line" ]]; then
      echo "Error from server (NotFound): operatorops.storage.simplyblock.io \"$3\" not found" >&2
      exit 1
    fi
    if [[ " $* " == *" -o jsonpath="* ]]; then
      printf '%s\t%s\n' "$(cut -f1 <<<"$line")" "$(cut -f4 <<<"$line")"
    else
      echo "kind: OperatorOps"; echo "name: $3"
    fi
    exit 0 ;;
  "get configmaps")
    if [[ " $* " == *" -o jsonpath="* ]]; then
      for f in "$STUB_DIR"/reports/*; do [[ -f "$f" ]] && basename "$f"; done
    else
      echo "kind: List"
    fi
    exit 0 ;;
  "get configmap")
    cat "$STUB_DIR/reports/$3"
    exit 0 ;;
  "get pods")
    if [[ " $* " == *" -l control-plane=simplyblock-operator "* ]]; then
      echo "operator-ns${tab}simplyblock-operator-7d9f"
    elif [[ " $* " == *" -o jsonpath="* ]]; then
      [[ -f "$STUB_DIR/probe-pods" ]] && cat "$STUB_DIR/probe-pods"
    else
      echo "pods"
    fi
    exit 0 ;;
esac
if [[ "$1 $2" == "get events" && -n "${STUB_NO_EVENTS:-}" ]]; then
  echo "No resources found in $4 namespace." >&2
  exit 0
fi
if [[ "$1" == logs ]]; then
  if [[ -n "${STUB_FAIL_LOGS:-}" ]]; then
    echo "error: the server is currently unable to handle the request" >&2
    exit 1
  fi
  echo "log of $2"
  exit 0
fi
echo "stub: $*"
EOF
chmod +x "$WORK/bin/kubectl"

FAILED=0
fail() { echo "FAIL $CASE: $*"; FAILED=1; }

# fleet writes the fixtures of a namespace with three runs: two discovery runs,
# the later of them named run-new, and an upgrade run newer than both.
fleet() {
  local dir="$1"
  mkdir -p "$dir/reports"
  printf '%s\t%s\t%s\t%s\n' \
    "2026-10-20T08:00:00Z" run-old Discover "" \
    "2026-10-24T09:00:00Z" run-new Discover draft-1 \
    "2026-10-24T10:00:00Z" upgrade-1 Upgrade "" > "$dir/runs.tsv"
  echo '{"version": 10, "node": "worker-1"}' > "$dir/reports/nodeprobe-run-new-worker-1"
  echo '{"version": 10, "node": "worker-2"}' > "$dir/reports/nodeprobe-run-new-worker-2"
  printf '%s\n' nodeprobe-run-new-worker-1-abcde nodeprobe-run-new-worker-2-fghij > "$dir/probe-pods"
}

# bundle runs the script in a fresh case directory and unpacks what it wrote.
# It sets OUT (the script's output), STATUS, and TREE (the unpacked top
# directory, empty when nothing was written).
bundle() {
  local dir="$WORK/$CASE_ID"
  : > "$dir/calls"
  OUT="$(cd "$dir" && PATH="$WORK/bin:$PATH" STUB_DIR="$dir" STUB_LOG="$dir/calls" \
    "${BUNDLE_BASH:-bash}" "$SCRIPT" "$@" --output "$dir/bundle.tar.gz" 2>&1)"
  STATUS=$?
  TREE=""
  if [[ -f "$dir/bundle.tar.gz" ]]; then
    mkdir -p "$dir/out"
    tar -xzf "$dir/bundle.tar.gz" -C "$dir/out"
    TREE="$(find "$dir/out" -mindepth 1 -maxdepth 1 -type d | head -1)"
  fi
  CALLS="$dir/calls"
}

new_case() {
  CASE="$1"
  CASE_ID="case$((++CASES))"
  mkdir -p "$WORK/$CASE_ID"
  fleet "$WORK/$CASE_ID"
}
CASES=0

expect_file() {
  [[ -n "$TREE" && -s "$TREE/$1" ]] || fail "the bundle has no $1"
}

new_case "a named run is collected whole"
bundle -n simplyblock --run run-new
[[ $STATUS -eq 0 ]] || fail "exit $STATUS: $OUT"
for f in operatorops.yaml operatorops-describe.txt probe-reports.yaml events.txt nodes.yaml nodes-wide.txt \
  cdc.yaml cdc-describe.txt probe-pods.txt probe-pods-describe.txt deployment.yaml version.txt \
  logs/probe/nodeprobe-run-new-worker-1-abcde.log logs/probe/nodeprobe-run-new-worker-2-fghij.log \
  logs/operator/simplyblock-operator-7d9f.log; do
  expect_file "$f"
done
for cm in nodeprobe-run-new-worker-1 nodeprobe-run-new-worker-2; do
  expect_file "reports/$cm.json"
  if [[ -n "$TREE" ]] && ! diff -q "$WORK/$CASE_ID/reports/$cm" "$TREE/reports/$cm.json" >/dev/null; then
    fail "reports/$cm.json is not the ConfigMap's report.json"
  fi
done
[[ -z "$TREE" || ! -s "$TREE/errors.txt" ]] || fail "errors.txt is not empty: $(cat "$TREE/errors.txt")"
grep -q -- "-l storage.simplyblock.io/nodeprobe-run=run-new" "$CALLS" ||
  fail "the probe reports were not selected by the run's label"
grep -q "get clusterdeploymentconfigs draft-1 -n simplyblock" "$CALLS" ||
  fail "the CDC the run wrote (draft-1) was not collected"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"

FAILED_BEFORE=$FAILED; FAILED=0
new_case "without --run, the newest discovery run is taken"
bundle -n simplyblock
[[ $STATUS -eq 0 ]] || fail "exit $STATUS: $OUT"
grep -q "get operatorops run-new -n simplyblock" "$CALLS" || fail "run-new was not the run collected"
grep -q "get operatorops upgrade-1" "$CALLS" && fail "an upgrade run was collected as a discovery run"
grep -q "run-new" <<<"$OUT" || fail "the output does not name the run it collected"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

FAILED_BEFORE=$FAILED; FAILED=0
new_case "the bundle only reads, and never reads a Secret"
bundle -n simplyblock --run run-new
[[ -s "$CALLS" ]] || fail "no kubectl call was made, so there is nothing to judge"
while read -r verb rest; do
  case "$verb" in
    get | describe | logs | version) ;;
    config) [[ "$rest" == view* ]] || fail "kubectl config $rest" ;;
    *) fail "kubectl $verb $rest" ;;
  esac
done < "$CALLS"
grep -qi secret "$CALLS" && fail "a call names a Secret: $(grep -i secret "$CALLS" | head -1)"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

FAILED_BEFORE=$FAILED; FAILED=0
new_case "the operator log starts when the run did"
bundle -n simplyblock --run run-new
grep -q "logs simplyblock-operator-7d9f -n operator-ns .*--since-time=2026-10-24T09:00:00Z" "$CALLS" ||
  fail "the operator log was not read from the run's creation: $(grep '^logs simplyblock-operator' "$CALLS")"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

FAILED_BEFORE=$FAILED; FAILED=0
new_case "a failed read is recorded, and the rest is still collected"
STUB_FAIL_LOGS=1 bundle -n simplyblock --run run-new
[[ $STATUS -eq 0 ]] || fail "exit $STATUS: $OUT"
expect_file operatorops.yaml
expect_file errors.txt
[[ -n "$TREE" ]] && ! grep -q "unable to handle the request" "$TREE/errors.txt" &&
  fail "errors.txt does not carry kubectl's message"
grep -qi "could not be collected" <<<"$OUT" || fail "the output does not say that something is missing"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

FAILED_BEFORE=$FAILED; FAILED=0
new_case "an empty answer says why it is empty"
STUB_NO_EVENTS=1 bundle -n simplyblock --run run-new
expect_file events.txt
[[ -n "$TREE" ]] && ! grep -q "No resources found" "$TREE/events.txt" &&
  fail "events.txt does not say that there were no events"
[[ -z "$TREE" || ! -s "$TREE/errors.txt" ]] || fail "an empty answer was counted as a failure"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

FAILED_BEFORE=$FAILED; FAILED=0
new_case "a run that does not exist writes nothing"
bundle -n simplyblock --run run-missing
[[ $STATUS -ne 0 ]] || fail "exit 0 for a run that does not exist"
[[ -z "$TREE" ]] || fail "a bundle was written"
grep -q "run-missing" <<<"$OUT" || fail "the output does not name the run: $OUT"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

FAILED_BEFORE=$FAILED; FAILED=0
new_case "a namespace without a discovery run writes nothing"
printf '%s\t%s\t%s\t%s\n' "2026-10-24T10:00:00Z" upgrade-1 Upgrade "" > "$WORK/$CASE_ID/runs.tsv"
bundle -n simplyblock
[[ $STATUS -ne 0 ]] || fail "exit 0 without a discovery run"
[[ -z "$TREE" ]] || fail "a bundle was written"
grep -qi "no discovery run" <<<"$OUT" || fail "the output does not say there is no discovery run: $OUT"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

FAILED_BEFORE=$FAILED; FAILED=0
new_case "the namespace defaults to the context's, then to simplyblock"
echo "storage" > "$WORK/$CASE_ID/namespace"
bundle --run run-new
grep -q "get operatorops run-new -n storage" "$CALLS" || fail "the context's namespace was not used"
rm "$WORK/$CASE_ID/namespace"
bundle --run run-new
grep -q "get operatorops run-new -n simplyblock" "$CALLS" || fail "simplyblock was not the fallback"
[[ $FAILED -eq 0 ]] && echo "ok   $CASE"
FAILED=$((FAILED || FAILED_BEFORE))

exit "$FAILED"
