#!/bin/sh
# Container entrypoint.
#
#   1. writes config.js from the environment, so one image serves any cluster
#   2. renders the reverse-proxy locations nginx cannot be handed statically:
#      the optional upstreams (operator API, Helm, Prometheus), the local
#      control plane API (/cp/local/), and one Kubernetes API and one control
#      plane location per configured context (/k8s/<name>/, /cp/<name>/)
#   3. keeps every proxied bearer token fresh: projected ServiceAccount tokens
#      rotate, and a context's Secret may be rotated by its administrator, so
#      a token read once at startup would expire while the pod still runs
#
# Contexts are the console's way of reaching several Kubernetes clusters: the
# browser calls same-origin /k8s/<name>/ and /cp/<name>/, and this pod proxies
# each to that cluster's API server and control plane with a credential that is
# mounted here. API URLs, CA paths, and tokens never reach the browser; config.js
# carries only the names, labels, namespaces, and base paths.
#
set -eu

# The root filesystem is read-only, so everything generated at start goes to the
# writable scratch mount; nginx serves config.js from there by alias. The two
# SB_RUNTIME_DIR and SB_SA_TOKEN_FILE overrides exist so the rendering can be
# exercised outside a pod; a Deployment never sets them.
RUNTIME_DIR=${SB_RUNTIME_DIR:-/tmp/nginx}
CONFIG_JS=${RUNTIME_DIR}/config.js
TOKEN_FILE=${SB_SA_TOKEN_FILE:-/var/run/secrets/kubernetes.io/serviceaccount/token}
TOKEN_CONF=${RUNTIME_DIR}/token.conf
UPSTREAMS_CONF=${RUNTIME_DIR}/upstreams.conf
CONTEXTS_CONF=${RUNTIME_DIR}/contexts.conf
SECURITY_HEADERS=/etc/nginx/snippets/security-headers.conf
# The Alpine base image's CA bundle, for a control plane behind a public
# certificate when no CA file was mounted for it.
SYSTEM_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
mkdir -p "${RUNTIME_DIR}"

: "${SB_NAMESPACE:=simplyblock}"
: "${SB_DR_NAMESPACE:=ramen-ops}"
: "${SB_MODE:=full}"
: "${SB_AUTH_MODE:=serviceaccount}"
: "${SB_MOCK:=false}"
: "${SB_TOKEN_REFRESH_SECONDS:=600}"
: "${SB_CP_URL:=http://simplyblock-webappapi:5000}"
: "${SB_CP_TOKEN_FILE:=}"
: "${SB_CP_CA_FILE:=}"
: "${SB_CP_HOST:=}"
: "${SB_CONTEXTS_FILE:=/etc/sb/contexts/contexts.json}"
: "${SB_LOCAL_CONTEXT_NAME:=local}"
: "${SB_LOCAL_LABELS:=}"
: "${SB_LOCAL_NAMESPACES:=}"
: "${SB_FEATURES:=}"
# A context's credential files live next to the contexts file, one directory
# per context: <dir>/<name>/token, ca.crt, and optionally cp-token, cp-ca.crt.
CONTEXTS_DIR=$(dirname "${SB_CONTEXTS_FILE}")

# Path segments under /k8s/ and /cp/ that a context name must never take: a
# context named `apis` would shadow /k8s/apis/, and `local` is the local
# control plane. Checked at startup because nginx would accept the shadowing
# location silently.
RESERVED_NAMES="api apis version openapi healthz livez readyz metrics local"
NAME_PATTERN='^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$'

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

fatal() { echo "FATAL: $*" >&2; exit 1; }
warn()  { echo "WARN: $*" >&2; }

# ---- helpers for the rendered locations ------------------------------------
# nginx variables are written literally here (escaped in the heredocs) and only
# the script's own values are expanded. Each helper appends to the file named first.

# An unset upstream answers 503 with a Kubernetes-style Status so the console
# shows "not available" instead of parsing index.html as JSON.
unavailable() {
  cat >> "$1" <<EOF
  location $2 {
    default_type application/json;
    return 503 '{"kind":"Status","apiVersion":"v1","status":"Failure","code":503,"reason":"ServiceUnavailable","message":"$3 is not part of this deployment"}';
  }
EOF
}

# The host part of a URL: scheme, userinfo, port, and path stripped. Used for
# SNI and the Host header when a context does not name the host explicitly.
url_host() {
  printf '%s' "$1" | sed -E 's#^[a-z]+://##; s#^[^@/]*@##; s#[/?].*$##; s#^\[([^]]*)\].*$#\1#; s#:[0-9]+$##'
}

# The TLS verification block for an HTTPS upstream: CA file, SNI, and server
# name verification. Rendered only for HTTPS, because nginx loads the CA file
# at configuration time and an HTTP context (the mock) mounts none.
#   $1 file  $2 CA file  $3 server name
ssl_block() {
  cat >> "$1" <<EOF
    proxy_ssl_verify              on;
    proxy_ssl_trusted_certificate $2;
    proxy_ssl_name                $3;
    proxy_ssl_server_name         on;
EOF
}

# A control plane v2 API location. The API streams (SSE on ?watch=true, kept
# open for up to an hour by the server), so no buffering, no caching, no
# compression, and timeouts past the server's own cap. Read-only: the console
# issues no write through it, and limit_except keeps it that way.
#   $1 file  $2 path  $3 token include  $4 upstream URL  $5 host  $6 CA file
cp_location() {
  cat >> "$1" <<EOF
  location $2 {
    include ${SECURITY_HEADERS};
    include $3;
    proxy_pass $4/;
EOF
  case "$4" in
    https://*) ssl_block "$1" "$6" "$5" ;;
  esac
  cat >> "$1" <<EOF
    proxy_http_version 1.1;
    proxy_set_header Host            $5;
    proxy_set_header Connection      "";
    proxy_set_header Accept-Encoding "";
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_buffering off;
    proxy_cache off;
    gzip off;
    proxy_read_timeout 3700s;
    proxy_send_timeout 3700s;
    limit_except GET { deny all; }
  }
EOF
}

# A Kubernetes API location for a context: the same body as /k8s/ in the
# server template, with the context's own CA, SNI, Host, and token include.
#   $1 file  $2 path  $3 token include  $4 upstream URL  $5 host  $6 CA file
k8s_location() {
  cat >> "$1" <<EOF
  location $2 {
    include ${SECURITY_HEADERS};
    include $3;
    proxy_pass $4/;
EOF
  case "$4" in
    https://*) ssl_block "$1" "$6" "$5" ;;
  esac
  cat >> "$1" <<EOF
    proxy_http_version 1.1;
    proxy_set_header Host            $5;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_set_header Accept-Encoding "";
    # watch requests stream, so no buffering and no read timeout
    proxy_buffering off;
    proxy_read_timeout 3600s;
    # The verbs the console has no use for at all. Which resources may be
    # written is RBAC on the remote cluster, not this line.
    limit_except GET POST PATCH PUT DELETE { deny all; }
  }
EOF
}

# The first readable file among the arguments, else the last resort given as
# the final argument: the CA fallback chain and the cp-token to token fallback.
first_readable() {
  for f in "$@"; do
    if [ -n "$f" ] && [ -r "$f" ]; then printf '%s' "$f"; return 0; fi
  done
  printf '%s' "$1"
}

# The CA file for an HTTPS control plane: the first readable candidate, else
# the system bundle for a publicly signed certificate.
cp_ca_file() {
  first_readable "$@" "${SYSTEM_CA_BUNDLE}"
}

# ---- contexts: read and validate -------------------------------------------
# An array of {name, kubernetesApi, kubernetesApiHost, namespaces, labels,
# controlPlane: {url, host}}. Validation is fatal: a bad name shadows a path,
# and a context without an API server has nothing to proxy.
CONTEXTS_JSON='[]'
if [ -r "${SB_CONTEXTS_FILE}" ]; then
  CONTEXTS_JSON=$(jq -c 'if type == "array" then . elif . == null then [] else error("not an array") end' "${SB_CONTEXTS_FILE}") \
    || fatal "${SB_CONTEXTS_FILE} is not a JSON array of contexts"
fi

check_name() {
  printf '%s' "$1" | grep -Eq "${NAME_PATTERN}" \
    || fatal "context name '$1' must match ${NAME_PATTERN}"
  for r in ${RESERVED_NAMES}; do
    [ "$1" = "$r" ] && fatal "context name '$1' is reserved: it would shadow /k8s/$1/ or /cp/$1/"
  done
  return 0
}

# The local context is named by the deployment, not by the file; "local" is the
# default and the only reserved name it may take.
printf '%s' "${SB_LOCAL_CONTEXT_NAME}" | grep -Eq "${NAME_PATTERN}" \
  || fatal "SB_LOCAL_CONTEXT_NAME '${SB_LOCAL_CONTEXT_NAME}' must match ${NAME_PATTERN}"

CONTEXT_COUNT=$(printf '%s' "${CONTEXTS_JSON}" | jq 'length')
SEEN_NAMES=" ${SB_LOCAL_CONTEXT_NAME} "
i=0
while [ "$i" -lt "${CONTEXT_COUNT}" ]; do
  name=$(printf '%s' "${CONTEXTS_JSON}" | jq -r --argjson i "$i" '.[$i].name // ""')
  [ -n "${name}" ] || fatal "context #$i has no name"
  check_name "${name}"
  case "${SEEN_NAMES}" in
    *" ${name} "*) fatal "context name '${name}' is used twice" ;;
  esac
  SEEN_NAMES="${SEEN_NAMES}${name} "
  api=$(printf '%s' "${CONTEXTS_JSON}" | jq -r --argjson i "$i" '.[$i].kubernetesApi // ""')
  case "${api}" in
    https://*|http://*) ;;
    "") fatal "context '${name}' has no kubernetesApi" ;;
    *)  fatal "context '${name}': kubernetesApi must be an HTTP or HTTPS URL, got '${api}'" ;;
  esac
  cp=$(printf '%s' "${CONTEXTS_JSON}" | jq -r --argjson i "$i" '.[$i].controlPlane.url // ""')
  case "${cp}" in
    ""|https://*|http://*) ;;
    *) fatal "context '${name}': controlPlane.url must be an HTTP or HTTPS URL, got '${cp}'" ;;
  esac
  i=$((i + 1))
done

# ---- 1. runtime configuration ----------------------------------------------
# In serviceaccount mode the browser gets no credential at all: the proxy in
# this pod attaches one. In passthrough mode the token has to reach the browser,
# which is why it is not the default. Contexts always use the pod's mounted
# credentials: the browser has no token for a remote cluster in either mode.
#
# cpBase is empty in DR mode, where there is no control plane to reach, and a
# context's cpBase is empty when it declares no control plane. The console
# treats an empty base as "not available".
LOCAL_CP_BASE=/cp/local
[ "${SB_MODE}" = "dr" ] && LOCAL_CP_BASE=""
LOCAL_NAMESPACES=${SB_LOCAL_NAMESPACES:-${SB_NAMESPACE}}
CONTEXTS_CONFIG=$(printf '%s' "${CONTEXTS_JSON}" | jq -c \
  --arg lname "${SB_LOCAL_CONTEXT_NAME}" \
  --arg lns "${LOCAL_NAMESPACES}" \
  --arg llabels "${SB_LOCAL_LABELS}" \
  --arg lcp "${LOCAL_CP_BASE}" \
  --arg mode "${SB_MODE}" '
  def csv: split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0));
  def kv: split("=") | {key: .[0], value: (.[1:] | join("="))};
  [{name: $lname,
    labels: ($llabels | csv | map(kv) | from_entries),
    namespaces: ($lns | csv),
    k8sBase: "/k8s",
    cpBase: $lcp,
    local: true}]
  + map({name: .name,
         labels: (.labels // {}),
         namespaces: (.namespaces // []),
         k8sBase: ("/k8s/" + .name),
         cpBase: (if $mode == "dr" or ((.controlPlane.url // "") == "") then "" else "/cp/" + .name end)})')
FEATURES_CONFIG=$(jq -cn --arg f "${SB_FEATURES}" \
  '$f | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0)) | map({key: ., value: true}) | from_entries')

cat > "${CONFIG_JS}" <<EOF
// Generated at container start from the pod environment. Do not edit.
window.SB_CONFIG = {
  k8sBase: "/k8s",
  cpBase: "${LOCAL_CP_BASE}",
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
  mock: ${SB_MOCK},
  contexts: ${CONTEXTS_CONFIG},
  features: ${FEATURES_CONFIG}
};
EOF
echo "config.js written: mode=${SB_MODE} namespace=${SB_NAMESPACE} drNamespace=${SB_DR_NAMESPACE} authMode=${SB_AUTH_MODE} mock=${SB_MOCK} contexts=$((CONTEXT_COUNT + 1)) features=${SB_FEATURES:-none}"

# ---- 2. optional upstreams --------------------------------------------------
# One location block per configured upstream; an unset one answers 503.
: > "${UPSTREAMS_CONF}"
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
  unavailable "${UPSTREAMS_CONF}" /operator/v1/ "the operator API"
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
  unavailable "${UPSTREAMS_CONF}" /helm/v1/ "the Helm release view"
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
  unavailable "${UPSTREAMS_CONF}" /prometheus/api/v1/ "Prometheus"
fi

# The local control plane v2 API. In DR mode there is no control plane on this
# cluster, so the whole /cp/ prefix answers 503 and nothing is proxied.
if [ "${SB_MODE}" = "dr" ]; then
  unavailable "${UPSTREAMS_CONF}" /cp/ "the control plane API"
  LOCAL_CP_SUMMARY=none
else
  LOCAL_CP_HOST=${SB_CP_HOST:-$(url_host "${SB_CP_URL}")}
  LOCAL_CP_CA=$(cp_ca_file "${SB_CP_CA_FILE}")
  cat >> "${UPSTREAMS_CONF}" <<EOF
  # the control plane v2 API of this cluster: tasks, the event log, capacity,
  # iostats, device health, and their SSE streams
EOF
  cp_location "${UPSTREAMS_CONF}" /cp/local/ "${RUNTIME_DIR}/cp-token-local.conf" "${SB_CP_URL}" "${LOCAL_CP_HOST}" "${LOCAL_CP_CA}"
  LOCAL_CP_SUMMARY=${SB_CP_URL}
fi
echo "upstreams: operator=${SB_OPERATOR_URL:-none} helm=${SB_HELM_URL:-none} prometheus=${SB_PROMETHEUS_URL:-none} controlplane=${LOCAL_CP_SUMMARY}"

# ---- 3. contexts --------------------------------------------------------------
# One /k8s/<name>/ and one /cp/<name>/ location per context. The locations are
# rendered once; only the token includes they reference are rewritten later.
: > "${CONTEXTS_CONF}"
i=0
while [ "$i" -lt "${CONTEXT_COUNT}" ]; do
  ctx=$(printf '%s' "${CONTEXTS_JSON}" | jq -c --argjson i "$i" '.[$i]')
  name=$(printf '%s' "${ctx}" | jq -r '.name')
  api=$(printf '%s' "${ctx}" | jq -r '.kubernetesApi')
  host=$(printf '%s' "${ctx}" | jq -r '.kubernetesApiHost // ""')
  [ -n "${host}" ] || host=$(url_host "${api}")
  dir=${CONTEXTS_DIR}/${name}
  case "${api}" in
    https://*)
      [ -r "${dir}/ca.crt" ] || fatal "context '${name}': ${dir}/ca.crt is required to verify ${api}"
      ;;
    *) warn "context '${name}': kubernetesApi ${api} is not HTTPS; fine for the mock, never for a cluster" ;;
  esac
  [ -r "${dir}/token" ] || warn "context '${name}': ${dir}/token is missing; requests to /k8s/${name}/ will carry no credential"
  printf '  # context %s\n' "${name}" >> "${CONTEXTS_CONF}"
  k8s_location "${CONTEXTS_CONF}" "/k8s/${name}/" "${RUNTIME_DIR}/token-${name}.conf" "${api}" "${host}" "${dir}/ca.crt"

  cp=$(printf '%s' "${ctx}" | jq -r '.controlPlane.url // ""')
  if [ "${SB_MODE}" = "dr" ]; then
    # /cp/ as a whole already answers 503 in DR mode
    :
  elif [ -n "${cp}" ]; then
    cphost=$(printf '%s' "${ctx}" | jq -r '.controlPlane.host // ""')
    [ -n "${cphost}" ] || cphost=$(url_host "${cp}")
    cp_location "${CONTEXTS_CONF}" "/cp/${name}/" "${RUNTIME_DIR}/cp-token-${name}.conf" "${cp}" "${cphost}" "$(cp_ca_file "${dir}/cp-ca.crt" "${dir}/ca.crt")"
  else
    unavailable "${CONTEXTS_CONF}" "/cp/${name}/" "the control plane of context ${name}"
  fi
  echo "context ${name}: k8s=${api} controlplane=${cp:-none}"
  i=$((i + 1))
done

# ---- 4. the proxied tokens ---------------------------------------------------
# Each include sets the Authorization header nginx sends upstream, replacing
# whatever the browser sent. Written atomically so a reload never reads half a
# file. A missing source leaves the include empty, so the request goes out
# without a credential rather than nginx refusing to start.
write_bearer() {
  if [ -r "$1" ]; then
    printf 'proxy_set_header Authorization "Bearer %s";\n' "$(tr -d '\r\n' < "$1")" > "$2.new"
  else
    : > "$2.new"
  fi
  mv "$2.new" "$2"
  [ -r "$1" ]
}

write_all_tokens() {
  ok=0
  # the local Kubernetes API: the projected token in serviceaccount mode, the
  # browser's own header in passthrough mode
  if [ "${SB_AUTH_MODE}" = "serviceaccount" ]; then
    write_bearer "${TOKEN_FILE}" "${TOKEN_CONF}" || ok=1
  else
    : > "${TOKEN_CONF}"
  fi
  # the local control plane accepts a ServiceAccount token of this cluster, so
  # the projected token serves unless a dedicated one is mounted
  if [ "${SB_MODE}" != "dr" ]; then
    write_bearer "$(first_readable "${SB_CP_TOKEN_FILE}" "${TOKEN_FILE}")" "${RUNTIME_DIR}/cp-token-local.conf" || ok=1
  fi
  i=0
  while [ "$i" -lt "${CONTEXT_COUNT}" ]; do
    name=$(printf '%s' "${CONTEXTS_JSON}" | jq -r --argjson i "$i" '.[$i].name')
    dir=${CONTEXTS_DIR}/${name}
    write_bearer "${dir}/token" "${RUNTIME_DIR}/token-${name}.conf" || ok=1
    if [ "${SB_MODE}" != "dr" ]; then
      write_bearer "$(first_readable "${dir}/cp-token" "${dir}/token")" "${RUNTIME_DIR}/cp-token-${name}.conf" || ok=1
    fi
    i=$((i + 1))
  done
  return $ok
}

if [ "${SB_AUTH_MODE}" = "serviceaccount" ] && [ ! -r "${TOKEN_FILE}" ]; then
  echo "FATAL: SB_AUTH_MODE=serviceaccount but ${TOKEN_FILE} is not readable." >&2
  echo "       Mount a ServiceAccount token, or set SB_AUTH_MODE=passthrough." >&2
  exit 1
fi
write_all_tokens || warn "one or more token files are missing; see the messages above"

# Refresh well inside the projected token's lifetime and reload nginx once so
# every new value is picked up. A reload is graceful: in-flight requests finish.
(
  while sleep "${SB_TOKEN_REFRESH_SECONDS}"; do
    write_all_tokens || warn "could not re-read one or more token files"
    nginx -s reload 2>/dev/null || true
  done
) &

exec /docker-entrypoint.sh "$@"
