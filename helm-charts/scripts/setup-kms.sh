#!/usr/bin/env bash
# Installs, initializes, unseals, and configures OpenBao for Simplyblock KMS.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

NAMESPACE="${NAMESPACE:-vault}"
STORAGE_CLASS="${STORAGE_CLASS:-local-path}"
UNSEAL_KEYS_FILE="${UNSEAL_KEYS_FILE:-}"
ROOT_TOKEN="${BAO_TOKEN:-}"

POD="openbao-0"
BAO_ADDR="https://openbao.vault:8200/"

info() { echo "[INFO]  $*"; }
warn() { echo "[WARN]  $*" >&2; }
die()  { echo "[ERROR] $*" >&2; exit 1; }

bao() {
  kubectl -n "$NAMESPACE" exec -i "$POD" -- \
    env BAO_ADDR="$BAO_ADDR" BAO_TOKEN="$ROOT_TOKEN" bao "$@"
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<EOF
Usage: $0

Env vars:
  NAMESPACE         Kubernetes namespace        (default: vault)
  STORAGE_CLASS     StorageClass for data PVC   (default: local-path)
  UNSEAL_KEYS_FILE  File to save init output    (default: stdout only)
  BAO_TOKEN         Skip init/unseal, use token (default: run init)
EOF
  exit 0
fi

command -v kubectl &>/dev/null || die "kubectl not found"
command -v helm    &>/dev/null || die "helm not found"

info "Checking for ClusterIssuer simplyblock-certificate-authority-issuer..."
kubectl get clusterissuer simplyblock-certificate-authority-issuer &>/dev/null \
  || die "ClusterIssuer 'simplyblock-certificate-authority-issuer' not found — ensure cert-manager is installed, the issuer exists, and the ControlPlane is installed with TLS enabled"

# ── Install ────────────────────────────────────────────────────────────────────
info "Installing OpenBao..."
helm repo add openbao https://openbao.github.io/openbao-helm
helm repo update
helm upgrade --install openbao openbao/openbao \
  -n "$NAMESPACE" --create-namespace \
  -f "$SCRIPT_DIR/openbao-values.yaml" \
  --set server.dataStorage.storageClass="$STORAGE_CLASS"

info "Waiting for $POD to be running..."
kubectl -n "$NAMESPACE" wait pod/"$POD" \
  --for=jsonpath='{.status.phase}'=Running --timeout=120s

# ── Init + unseal ──────────────────────────────────────────────────────────────
if [[ -z "$ROOT_TOKEN" ]]; then
  info "Initializing OpenBao..."
  INIT_OUTPUT="$(kubectl -n "$NAMESPACE" exec "$POD" -- \
    env BAO_ADDR="$BAO_ADDR" bao operator init 2>&1)"

  echo ""
  echo "=========================================="
  echo "  INIT OUTPUT — SAVE THIS NOW"
  echo "=========================================="
  echo "$INIT_OUTPUT"
  echo "=========================================="
  echo ""

  if [[ -n "$UNSEAL_KEYS_FILE" ]]; then
    echo "$INIT_OUTPUT" > "$UNSEAL_KEYS_FILE"
    chmod 600 "$UNSEAL_KEYS_FILE"
    info "Saved to: $UNSEAL_KEYS_FILE"
  fi

  mapfile -t UNSEAL_KEYS < <(echo "$INIT_OUTPUT" | grep -E '^Unseal Key [0-9]+:' | awk '{print $NF}')
  ROOT_TOKEN="$(echo "$INIT_OUTPUT" | grep '^Initial Root Token:' | awk '{print $NF}')"

  [[ ${#UNSEAL_KEYS[@]} -ge 3 ]] || die "Could not parse unseal keys"
  [[ -n "$ROOT_TOKEN" ]]         || die "Could not parse root token"

  info "Unsealing (3 of 5 keys)..."
  for i in 0 1 2; do
    kubectl -n "$NAMESPACE" exec "$POD" -- \
      env BAO_ADDR="$BAO_ADDR" bao operator unseal "${UNSEAL_KEYS[$i]}"
  done

  info "Waiting for $POD to be ready..."
  kubectl -n "$NAMESPACE" wait pod/"$POD" --for=condition=Ready --timeout=60s
else
  info "Skipping init/unseal — using provided BAO_TOKEN"
fi

# ── Configure ──────────────────────────────────────────────────────────────────
info "Writing webappapi-policy..."
bao policy write simplyblock-webappapi-policy - <<'EOF'
path "simplyblock/transit/keys/*" {
  capabilities = ["create", "update", "read", "delete"]
}
path "simplyblock/transit/datakey/plaintext/*" {
  capabilities = ["create", "update"]
}
path "simplyblock/transit/datakey/wrapped/*" {
  capabilities = ["create", "update"]
}
path "simplyblock/transit/encrypt/*" {
  capabilities = ["create", "update"]
}
path "simplyblock/transit/decrypt/*" {
  capabilities = ["create", "update"]
}
path "simplyblock/kv/*" {
  capabilities = ["create", "read", "update", "delete"]
}
EOF

info "Enabling cert auth..."
bao auth enable cert || warn "cert auth already enabled, continuing..."
bao write auth/cert/certs/simplyblock-webappapi \
  certificate=@/openbao/tls/ca.crt \
  allowed_dns_sans="simplyblock-webappapi" \
  token_policies=simplyblock-webappapi-policy \
  token_ttl=10m \
  token_max_ttl=30m

info "Enabling secrets engines..."
bao secrets enable -path=simplyblock/transit transit        || warn "transit already enabled, continuing..."
bao secrets enable -path=simplyblock/kv kv  || warn "kv already enabled, continuing..."

# ── Done ───────────────────────────────────────────────────────────────────────
info "Done."
if [[ -z "${BAO_TOKEN:-}" ]]; then
  info "Root token: $ROOT_TOKEN  (store it securely)"
fi
