#!/usr/bin/env bash
# Exercises setup-kms.sh against command doubles so its OpenBao engine contract
# is checked without requiring a Kubernetes cluster.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
kms_script="${script_dir}/../setup-kms.sh"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT

export COMMAND_LOG="${test_dir}/commands.log"
mkdir -p "${test_dir}/bin"

cat >"${test_dir}/bin/helm" <<'EOF'
#!/usr/bin/env bash
printf 'helm %s\n' "$*" >>"$COMMAND_LOG"
EOF

cat >"${test_dir}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"$COMMAND_LOG"
if [[ "$*" == *"operator init"* ]]; then
  cat <<'INIT'
Unseal Key 1: key-one
Unseal Key 2: key-two
Unseal Key 3: key-three
Unseal Key 4: key-four
Unseal Key 5: key-five
Initial Root Token: root-token
INIT
fi
if [[ " $* " == *" exec "* && ! -t 0 ]]; then
  cat >/dev/null
fi
EOF

chmod +x "${test_dir}/bin/helm" "${test_dir}/bin/kubectl"

PATH="${test_dir}/bin:${PATH}" bash "$kms_script" >/dev/null

# Regression: 2026-09-23-openbao-kv-v1 — the installer mounted KV v1 while
# sbcli's HCPClient uses KV v2 endpoints, so encrypted-volume key writes failed.
if ! grep -Fq \
  'secrets enable -path=simplyblock/kv -version=2 kv' "$COMMAND_LOG"; then
  echo "setup-kms.sh did not enable simplyblock/kv as KV v2" >&2
  grep -F 'secrets enable -path=simplyblock/kv' "$COMMAND_LOG" >&2 || true
  exit 1
fi
