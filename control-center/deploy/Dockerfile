# syntax=docker/dockerfile:1
#
# simplyblock Control Center — static UI, served by nginx, with an in-pod
# reverse proxy in front of the Kubernetes API, the operator API and Prometheus.
#
#   docker build -t quay.io/simplyblock-io/control-center:26.3.0 -f deploy/Dockerfile .
#
# Build args:
#   KEEP_MOCKS=1   keep the fixture backend in the image (demo build only)
#   REACT_VERSION, BABEL_VERSION            exact versions
#   INTER_VERSION, IBM_PLEX_MONO_VERSION    typeface versions — read the note
#                                           on the vendor stage before pinning
#
# The runtime image needs no network egress: React, Babel, both typefaces and
# the brand mark are all vendored at build time, so an air-gapped cluster
# serves every byte from the pod.

# ---- stage 1: vendor the browser dependencies ------------------------------
FROM docker.io/library/node:22-alpine AS vendor
ARG REACT_VERSION=18.3.1
ARG BABEL_VERSION=7.29.0
# One arg per package. @fontsource/inter and @fontsource/ibm-plex-mono are
# separate packages on separate release trains, so a single shared exact
# version is not safe to assume — npm hard-fails if either has not published
# it. These default to the major range, which resolves for both. To pin
# exactly, check each package's own published versions; for a genuinely
# reproducible air-gapped build, commit a lockfile and use `npm ci`.
ARG INTER_VERSION=5
ARG IBM_PLEX_MONO_VERSION=5
RUN apk add --no-cache curl
WORKDIR /vendor
# Production builds of React — the page pins the development builds for the
# design preview, and prepare.mjs rewrites the tags to point here.
RUN curl -fsSLO "https://unpkg.com/react@${REACT_VERSION}/umd/react.production.min.js" \
 && curl -fsSLO "https://unpkg.com/react-dom@${REACT_VERSION}/umd/react-dom.production.min.js" \
 && curl -fsSL  "https://unpkg.com/@babel/standalone@${BABEL_VERSION}/babel.min.js" -o babel.min.js

# Typefaces. The design is a geometric sans plus a mono, and the mono carries
# every UUID and CRD field name in the console — so these are load-bearing, not
# decorative, and cannot be left to a CDN the pod is not allowed to reach.
# fontsource ships the same upstream files with stable, hashless names.
RUN npm install --no-save --no-audit --no-fund \
      "@fontsource/inter@${INTER_VERSION}" \
      "@fontsource/ibm-plex-mono@${IBM_PLEX_MONO_VERSION}" \
 && mkdir -p fonts \
 && for w in 400 500 600 700; do \
      cp "node_modules/@fontsource/inter/files/inter-latin-${w}-normal.woff2" fonts/; \
    done \
 && for w in 400 500 600; do \
      cp "node_modules/@fontsource/ibm-plex-mono/files/ibm-plex-mono-latin-${w}-normal.woff2" fonts/; \
    done \
 && rm -rf node_modules package.json package-lock.json

# The @font-face stylesheet that replaces the Google Fonts link. Written here
# rather than committed so the weight list stays next to the files it names.
RUN { \
      for w in 400 500 600 700; do \
        printf '@font-face{font-family:"Inter";font-style:normal;font-weight:%s;font-display:swap;src:url(fonts/inter-latin-%s-normal.woff2) format("woff2")}\n' "$w" "$w"; \
      done; \
      for w in 400 500 600; do \
        printf '@font-face{font-family:"IBM Plex Mono";font-style:normal;font-weight:%s;font-display:swap;src:url(fonts/ibm-plex-mono-latin-%s-normal.woff2) format("woff2")}\n' "$w" "$w"; \
      done; \
    } > fonts.css

# The brand mark, for the same reason as the fonts: img-src is 'self' and the
# pod has no route to the public internet.
RUN curl -fsSL "https://simplyblock.io/assets/images/Logo-white.svg" -o logo-white.svg \
 && test -s logo-white.svg

# ---- stage 2: prepare the document -----------------------------------------
FROM docker.io/library/node:22-alpine AS prepare
ARG KEEP_MOCKS=0
WORKDIR /src
COPY . .
# prepare.mjs parses --flag=value (its arg reader treats a space-separated
# value as a separate token), so pass --out with an "=".
RUN node deploy/build/prepare.mjs --keep-mocks="${KEEP_MOCKS}" --out=/out

# ---- stage 3: runtime -------------------------------------------------------
FROM docker.io/library/nginx:1.27-alpine

# nginx must run as a non-root user with a read-only root filesystem, so its
# writable paths are moved under /tmp and declared as volumes in the manifest.
RUN apk add --no-cache tini \
 && rm -f /etc/nginx/conf.d/default.conf \
 && mkdir -p /var/cache/nginx /tmp/nginx /etc/nginx/snippets \
 && chown -R 101:101 /var/cache/nginx /tmp/nginx /usr/share/nginx/html

COPY --from=prepare /out/ /usr/share/nginx/html/
COPY --from=vendor /vendor/ /usr/share/nginx/html/vendor/
COPY deploy/nginx-security-headers.conf /etc/nginx/snippets/security-headers.conf
COPY deploy/nginx.conf.template /etc/nginx/templates/default.conf.template
COPY deploy/nginx-main.conf /etc/nginx/nginx.conf
COPY deploy/docker-entrypoint.sh /usr/local/bin/sb-entrypoint
RUN chmod +x /usr/local/bin/sb-entrypoint

# Defaults. Every one is overridable from the Deployment's env.
# Scope envsubst to our own variables, or it would eat nginx's $uri and $host.
ENV NGINX_ENVSUBST_FILTER=SB_ \
    SB_NAMESPACE=simplyblock \
    SB_AUTH_MODE=serviceaccount \
    SB_K8S_API=https://kubernetes.default.svc \
    SB_K8S_HOST=kubernetes.default.svc \
    SB_K8S_CA_FILE=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt \
    SB_OPERATOR_URL=http://simplyblock-operator:8080 \
    SB_HELM_URL=http://simplyblock-operator:8080 \
    SB_PROMETHEUS_URL=http://simplyblock-prometheus:9090 \
    SB_LOGO_URL=vendor/logo-white.svg \
    SB_MOCK=false \
    SB_LISTEN_PORT=8080 \
    SB_TOKEN_REFRESH_SECONDS=600

USER 101:101
EXPOSE 8080
STOPSIGNAL SIGQUIT
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/sb-entrypoint"]
CMD ["nginx", "-g", "daemon off;"]
