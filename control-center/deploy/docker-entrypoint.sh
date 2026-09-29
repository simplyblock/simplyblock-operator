#!/bin/sh
# Container entrypoint.
#
#   1. writes config.js from the environment, so one image serves any cluster
#   2. renders the reverse-proxy locations for the optional upstreams (operator
#      API, Helm, Prometheus) — a DR-only hub has none of them, and nginx must
#      not be handed an empty proxy_pass
#   3. in serviceaccount mode, keeps the proxied bearer token fresh — projected
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
UPSTREAMS_CONF=${RUNTIME_DIR}/upstreams.conf
mkdir -p "${RUNTIME_DIR}"

: "${SB_NAMESPACE:=simplyblock}"
: "${SB_DR_NAMESPACE:=ramen-ops}"
: "${SB_MODE:=full}"
: "${SB_AUTH_MODE:=serviceaccount}"
: "${SB_MOCK:=false}"
: "${SB_TOKEN_REFRESH_SECONDS:=600}"

case "${SB_MODE}" in
  full|dr) ;;
  *) echo "FATAL: SB_MODE must be 'full' or 'dr', got '${SB_MODE}'" >&2; exit 1 ;;
esac
# The DR-only console reads nothing but the Kubernetes API: drop the other
# upstreams unless they were set on purpose.
if [ "${SB_MODE}" = "dr" ] && [ "${SB_MOCK}" != "true" ]; then
  [ "${SB_OPERATOR_URL:-}" = "http://simplyblock-operator:8080" ] && SB_OPERATOR_URL=""
  [ "${SB_HELM_URL:-}" = "http://simplyblock-operator:8080" ] && SB_HELM_URL=""
  [ "${SB_PROMETHEUS_URL:-}" = "http://simplyblock-prometheus:9090" ] && SB_PROMETHEUS_URL=""
fi

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
  drNamespace: "${SB_DR_NAMESPACE}",
  mode: "${SB_MODE}",
  logoUrl: "${SB_LOGO_URL:-vendor/logo-white.svg}",
  authMode: "${SB_AUTH_MODE}",
  token: "${SB_BROWSER_TOKEN:-}",
  mock: ${SB_MOCK}
};
EOF
echo "config.js written: mode=${SB_MODE} namespace=${SB_NAMESPACE} drNamespace=${SB_DR_NAMESPACE} authMode=${SB_AUTH_MODE} mock=${SB_MOCK}"

# ---- 2. optional upstreams ------------------------------------------------
# One location block per configured upstream; an unset one answers 503 with a
# Kubernetes-style Status so the console shows "not available" instead of
# parsing index.html as JSON. nginx variables are written literally here
# (quoted heredocs) — only our own SB_* values are expanded.
: > "${UPSTREAMS_CONF}"
unavailable() {
  cat >> "${UPSTREAMS_CONF}" <<EOF
  location $1 {
    default_type application/json;
    return 503 '{"kind":"Status","apiVersion":"v1","status":"Failure","code":503,"reason":"ServiceUnavailable","message":"$2 is not part of this deployment"}';
  }
EOF
}
if [ -n "${SB_OPERATOR_URL:-}" ]; then
  cat >> "${UPSTREAMS_CONF}" <<EOF
  # operator API: the kinds v1alpha1 does not model yet (/proposed/*), the
  # observability reads that are not in any API
  location /operator/v1/ {
    proxy_pass ${SB_OPERATOR_URL}/v1/;
    proxy_http_version 1.1;
    proxy_set_header Host            \$host;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_buffering off;
    proxy_read_timeout 3600s;
  }
EOF
else
  unavailable /operator/v1/ "the operator API"
fi
if [ -n "${SB_HELM_URL:-}" ]; then
  cat >> "${UPSTREAMS_CONF}" <<EOF
  location /helm/v1/ {
    proxy_pass ${SB_HELM_URL}/v1/;
    proxy_http_version 1.1;
    proxy_set_header Host            \$host;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
  }
EOF
else
  unavailable /helm/v1/ "the Helm release view"
fi
if [ -n "${SB_PROMETHEUS_URL:-}" ]; then
  cat >> "${UPSTREAMS_CONF}" <<EOF
  # Prometheus, read-only: the console queries, it never writes or administers
  location /prometheus/api/v1/ {
    proxy_pass ${SB_PROMETHEUS_URL}/api/v1/;
    proxy_http_version 1.1;
    proxy_set_header Host            \$host;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    limit_except GET POST { deny all; }
  }
EOF
else
  unavailable /prometheus/api/v1/ "Prometheus"
fi
echo "upstreams: operator=${SB_OPERATOR_URL:-none} helm=${SB_HELM_URL:-none} prometheus=${SB_PROMETHEUS_URL:-none}"

# ---- 3. the proxied token --------------------------------------------------
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
