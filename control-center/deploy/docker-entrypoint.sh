#!/bin/sh
# Container entrypoint.
#
#   1. writes config.js from the environment, so one image serves any cluster
#   2. in serviceaccount mode, keeps the proxied bearer token fresh — projected
#      ServiceAccount tokens rotate, so a token read once at startup expires
#      while the pod is still running
#
set -eu

# The root filesystem is read-only, so everything generated at start goes to the
# writable scratch mount; nginx serves config.js from there by alias.
RUNTIME_DIR=/tmp/nginx
CONFIG_JS=${RUNTIME_DIR}/config.js
TOKEN_FILE=/var/run/secrets/kubernetes.io/serviceaccount/token
TOKEN_CONF=${RUNTIME_DIR}/token.conf
mkdir -p "${RUNTIME_DIR}"

: "${SB_NAMESPACE:=simplyblock}"
: "${SB_AUTH_MODE:=serviceaccount}"
: "${SB_MOCK:=false}"
: "${SB_TOKEN_REFRESH_SECONDS:=600}"

# ---- 1. runtime configuration ----------------------------------------------
# In serviceaccount mode the browser gets no credential at all: the proxy in
# this pod attaches one. In passthrough mode the token has to reach the browser,
# which is why it is not the default.
cat > "${CONFIG_JS}" <<EOF
// Generated at container start from the pod environment. Do not edit.
window.SB_CONFIG = {
  k8sBase: "/k8s",
  operatorBase: "/operator/v1",
  helmBase: "/helm/v1",
  promBase: "/prometheus/api/v1",
  agentBase: "/operator/v1/agent",
  namespace: "${SB_NAMESPACE}",
  logoUrl: "${SB_LOGO_URL:-vendor/logo-white.svg}",
  authMode: "${SB_AUTH_MODE}",
  token: "${SB_BROWSER_TOKEN:-}",
  mock: ${SB_MOCK}
};
EOF
echo "config.js written: namespace=${SB_NAMESPACE} authMode=${SB_AUTH_MODE} mock=${SB_MOCK}"

# ---- 2. the proxied token --------------------------------------------------
write_token() {
  if [ -r "${TOKEN_FILE}" ]; then
    printf 'proxy_set_header Authorization "Bearer %s";\n' "$(cat "${TOKEN_FILE}")" > "${TOKEN_CONF}.new"
    mv "${TOKEN_CONF}.new" "${TOKEN_CONF}"
    return 0
  fi
  return 1
}

if [ "${SB_AUTH_MODE}" = "serviceaccount" ]; then
  if ! write_token; then
    echo "FATAL: SB_AUTH_MODE=serviceaccount but ${TOKEN_FILE} is not readable." >&2
    echo "       Mount a ServiceAccount token, or set SB_AUTH_MODE=passthrough." >&2
    exit 1
  fi
  # Refresh well inside the projected token's lifetime and reload nginx so the
  # new value is picked up. A reload is graceful: in-flight requests finish.
  (
    while sleep "${SB_TOKEN_REFRESH_SECONDS}"; do
      if write_token; then
        nginx -s reload 2>/dev/null || true
      else
        echo "WARN: could not re-read ${TOKEN_FILE}" >&2
      fi
    done
  ) &
else
  # passthrough: nginx forwards whatever Authorization header the browser sent
  : > "${TOKEN_CONF}"
fi

exec /docker-entrypoint.sh "$@"
