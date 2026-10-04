#!/usr/bin/env bash
# Behavioral tests for setup-kms.sh against a stub kubectl and helm, so no
# cluster is needed. Lives beside the script it covers; run it directly.
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/setup-kms.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir "$WORK/bin"
# The stub answers the calls the script makes. STUB_INITIALIZED picks what
# `bao status` reports, and every `bao operator init` is appended to INIT_LOG.
cat > "$WORK/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *"bao status"*)
    if [[ "$STUB_INITIALIZED" == "true" ]]; then
      echo '{"initialized": true, "sealed": true}'
    else
      echo '{"initialized": false, "sealed": true}'
    fi
    exit 2 ;;
  *"bao operator init"*)
    echo init >> "$INIT_LOG"
    if [[ "$STUB_INITIALIZED" == "true" ]]; then
      echo "* Vault is already initialized"
      exit 2
    fi
    for i in 1 2 3 4 5; do echo "Unseal Key $i: key$i"; done
    echo "Initial Root Token: root-token"
    exit 0 ;;
esac
exit 0
EOF
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/helm"
chmod +x "$WORK/bin/kubectl" "$WORK/bin/helm"

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

# run_script <initialized> [env assignment...]: sets OUT, RC, and INITS, the
# number of times init was attempted.
run_script() {
  local initialized="$1"
  shift
  : > "$WORK/init.log"
  OUT="$(env PATH="$WORK/bin:$PATH" STUB_INITIALIZED="$initialized" \
    INIT_LOG="$WORK/init.log" "$@" bash "$SCRIPT" 2>&1)"
  RC=$?
  INITS="$(wc -l < "$WORK/init.log" | tr -d ' ')"
}

# Regression: 2026-10-01-setup-kms-init-on-initialized — a rerun against an
# OpenBao whose data volume survived called `bao operator init` unconditionally,
# got "Vault is already initialized", and dumped the previous run's pod log
# instead of saying that BAO_TOKEN was needed.
run_script true
[[ "$INITS" == 0 ]] || fail "initialized server: init attempted $INITS times, want 0"
[[ $RC -ne 0 ]] || fail "initialized server without BAO_TOKEN: exit 0, want a failure"
grep -q "BAO_TOKEN" <<<"$OUT" || fail "initialized server: output does not ask for BAO_TOKEN"
grep -q "already initialized" <<<"$OUT" || fail "initialized server: output does not say so"

# An uninitialized server is still initialized and configured.
run_script false
[[ "$INITS" == 1 ]] || fail "fresh server: init attempted $INITS times, want 1"
[[ $RC -eq 0 ]] || fail "fresh server: exit $RC, want 0"

# A supplied token skips init and unseal, whatever the server's state.
run_script true BAO_TOKEN=supplied
[[ "$INITS" == 0 ]] || fail "BAO_TOKEN supplied: init attempted $INITS times, want 0"
[[ $RC -eq 0 ]] || fail "BAO_TOKEN supplied: exit $RC, want 0"

if [[ $failures -ne 0 ]]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "ok"
