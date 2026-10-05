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
MODULES_CONF=${RUNTIME_DIR}/modules.conf
CP_AUTH_CONF=${RUNTIME_DIR}/cp-auth.conf
NJS_MODULE=/usr/lib/nginx/modules/ngx_http_js_module.so
mkdir -p "${RUNTIME_DIR}"
: > "${MODULES_CONF}"

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

# The control plane API (simplyblock management API v2): the source of the
# storage screens when the clusters are not CRDs in this Kubernetes cluster —
# a hub whose control plane manages storage clusters on other sites. Enabled
# only together with the njs filter that scrubs credentials out of its
# responses: some records carry a cluster's API secret, and nothing this proxy
# forwards may reach the browser with one in it.
SB_CONTROLPLANE_URL=${SB_CONTROLPLANE_URL:-}
SB_CONTROLPLANE_URL=${SB_CONTROLPLANE_URL%/}
CP_ON=false
if [ -n "${SB_CONTROLPLANE_URL}" ]; then
  if [ -r "${NJS_MODULE}" ]; then
    echo "load_module ${NJS_MODULE};" > "${MODULES_CONF}"
    CP_ON=true
  else
    echo "WARN: SB_CONTROLPLANE_URL is set but the njs module (${NJS_MODULE}) is missing:" >&2
    echo "      the control plane upstream stays disabled, its responses cannot be scrubbed." >&2
  fi
fi
up() { [ -n "$1" ] && echo true || echo false; }

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
  cpBase: "/controlplane/api/v2",
  // which optional upstreams this pod proxies: a screen whose source is off
  // says so instead of failing
  upstreams: {operator: $(up "${SB_OPERATOR_URL:-}"), helm: $(up "${SB_HELM_URL:-}"),
    prometheus: $(up "${SB_PROMETHEUS_URL:-}"), controlPlane: ${CP_ON}},
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
    return 503 '{"kind":"Status","apiVersion":"v1","status":"Failure","code":503,"reason":"NotInDeployment","message":"$2 is not part of this deployment"}';
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
if [ "${CP_ON}" = "true" ]; then
  # nginx resolves a proxy_pass variable at request time with this resolver,
  # so the console starts before the control plane's Service exists (the
  # operator creates it after the chart is installed). That needs a fully
  # qualified name in SB_CONTROLPLANE_URL: the resolver does not apply the
  # pod's search domains.
  RESOLVER=$(awk '/^nameserver/ {print $2; exit}' /etc/resolv.conf 2>/dev/null)
  case "${RESOLVER}" in *:*) RESOLVER="[${RESOLVER}]" ;; esac
  [ -n "${RESOLVER}" ] || RESOLVER=127.0.0.11
  # A control plane serving TLS (the chart's default): verify it against the
  # mounted CA and, where it requires client certificates, present the
  # console's own.
  CP_TLS=""
  case "${SB_CONTROLPLANE_URL}" in
    https://*)
      CP_HOST=${SB_CONTROLPLANE_URL#https://}; CP_HOST=${CP_HOST%%/*}; CP_HOST=${CP_HOST%%:*}
      CP_TLS="proxy_ssl_server_name on;
    proxy_ssl_name ${CP_HOST};"
      if [ -n "${SB_CONTROLPLANE_CA_FILE:-}" ]; then
        CP_TLS="${CP_TLS}
    proxy_ssl_verify on;
    proxy_ssl_trusted_certificate ${SB_CONTROLPLANE_CA_FILE};"
      fi
      if [ -n "${SB_CONTROLPLANE_CLIENT_CERT:-}" ] && [ -n "${SB_CONTROLPLANE_CLIENT_KEY:-}" ]; then
        CP_TLS="${CP_TLS}
    proxy_ssl_certificate ${SB_CONTROLPLANE_CLIENT_CERT};
    proxy_ssl_certificate_key ${SB_CONTROLPLANE_CLIENT_KEY};"
      fi
      ;;
  esac
  cat >> "${UPSTREAMS_CONF}" <<EOF
  # control plane API, read-only. Every response is parsed and scrubbed of
  # credentials by the njs filter before it leaves this pod (redact.js); a
  # body that is not JSON is replaced by a Status, never passed through.
  js_import sbredact from /etc/nginx/njs/redact.js;
  location /controlplane/ {
    limit_except GET { deny all; }
    include ${CP_AUTH_CONF};
    resolver ${RESOLVER} valid=30s;
    resolver_timeout 5s;
    set \$sb_controlplane "${SB_CONTROLPLANE_URL}";
    rewrite ^/controlplane/(.*)\$ /\$1 break;
    proxy_pass \$sb_controlplane;
    proxy_http_version 1.1;
    proxy_set_header Host            \$proxy_host;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_set_header Accept-Encoding "";
    proxy_read_timeout 60s;
    ${CP_TLS}
    js_header_filter sbredact.headers;
    js_body_filter sbredact.body;
  }
EOF
elif [ -n "${SB_CONTROLPLANE_URL}" ]; then
  unavailable /controlplane/ "the control plane API (its response filter is missing from this image)"
else
  unavailable /controlplane/ "the control plane API"
fi
echo "upstreams: operator=${SB_OPERATOR_URL:-none} helm=${SB_HELM_URL:-none} prometheus=${SB_PROMETHEUS_URL:-none} controlplane=$([ "${CP_ON}" = true ] && echo "${SB_CONTROLPLANE_URL}" || echo none)"

# ---- 3. the proxied token --------------------------------------------------
write_token() {
  if [ -r "${TOKEN_FILE}" ]; then
    printf 'proxy_set_header Authorization "Bearer %s";\n' "$(cat "${TOKEN_FILE}")" > "${TOKEN_CONF}.new"
    mv "${TOKEN_CONF}.new" "${TOKEN_CONF}"
    return 0
  fi
  return 1
}

# The control plane accepts the console's ServiceAccount token when that
# account is one of its admin service accounts (the chart has the operator
# add it), or a static admin token mounted at SB_CONTROLPLANE_TOKEN_FILE. The
# token file wins when it is set.
write_cp_auth() {
  if [ -n "${SB_CONTROLPLANE_TOKEN_FILE:-}" ] && [ -r "${SB_CONTROLPLANE_TOKEN_FILE}" ]; then
    printf 'proxy_set_header Authorization "Bearer %s";\n' "$(tr -d '\r\n' < "${SB_CONTROLPLANE_TOKEN_FILE}")" > "${CP_AUTH_CONF}.new"
    mv "${CP_AUTH_CONF}.new" "${CP_AUTH_CONF}"
  elif [ -f "${TOKEN_CONF}" ]; then
    cp "${TOKEN_CONF}" "${CP_AUTH_CONF}.new" && mv "${CP_AUTH_CONF}.new" "${CP_AUTH_CONF}"
  else
    : > "${CP_AUTH_CONF}"
  fi
}

if [ "${SB_AUTH_MODE}" = "serviceaccount" ]; then
  if ! write_token; then
    echo "FATAL: SB_AUTH_MODE=serviceaccount but ${TOKEN_FILE} is not readable." >&2
    echo "       Mount a ServiceAccount token, or set SB_AUTH_MODE=passthrough." >&2
    exit 1
  fi
else
  # passthrough: nginx forwards whatever Authorization header the browser sent
  : > "${TOKEN_CONF}"
fi
write_cp_auth
if [ "${SB_AUTH_MODE}" = "serviceaccount" ] || [ -n "${SB_CONTROLPLANE_TOKEN_FILE:-}" ]; then
  # Refresh well inside the projected token's lifetime and reload nginx so the
  # new value is picked up. A reload is graceful: in-flight requests finish.
  (
    while sleep "${SB_TOKEN_REFRESH_SECONDS}"; do
      if [ "${SB_AUTH_MODE}" = "serviceaccount" ] && ! write_token; then
        echo "WARN: could not re-read ${TOKEN_FILE}" >&2
      fi
      write_cp_auth
      nginx -s reload 2>/dev/null || true
    done
  ) &
fi

exec /docker-entrypoint.sh "$@"
