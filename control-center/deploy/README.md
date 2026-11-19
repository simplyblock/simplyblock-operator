# Packaging the Control Center as a pod

The console is a static document transpiled in the browser, served by nginx,
with an in-pod reverse proxy in front of the APIs it is allowed to use.
There is no build step for the application itself and no server-side code.

```
browser ──► nginx (this pod) ──┬──► kube-apiserver             /k8s/*
                               ├──► control plane v2 API       /cp/local/*
                               ├──► simplyblock-operator       /operator/v1/*
                               ├──► Prometheus                 /prometheus/api/v1/*
                               └──► per context <name>
                                     ├──► its kube-apiserver   /k8s/<name>/*
                                     └──► its control plane    /cp/<name>/*
```

Everything else is refused by the server block. The browser only ever makes
same-origin requests, which is what keeps a cluster credential out of it.

## Build

```sh
docker build -t quay.io/simplyblock-io/control-center:26.3.0 -f deploy/Dockerfile .
```

Four things happen at build time (`deploy/build/prepare.mjs`):

1. **The fixture backend is stripped.** Everything between `MOCK:START` and
   `MOCK:END` in `index.html` is removed and the build fails if any
   `mock-*.jsx` or `dist/mock.js` reference survives. No fixture data ships to a cluster.
2. **React and both typefaces are vendored** into `/vendor`, and the
   development React builds the design preview pins are swapped for the
   production ones. Inter and IBM Plex Mono come from fontsource and are served
   from the pod, replacing the Google Fonts link — the mono carries every UUID
   and CRD field name in the console, so losing it to a blocked CDN would not be
   cosmetic. The brand mark is vendored too and read from `SB_LOGO_URL`, because
   `img-src` is `'self'`. Together with step 3 this is what lets the pod run
   with no network egress at all.
3. **No external subresource may survive.** The build scans every shipped file,
   not just `index.html` — the logo lives in `app.jsx`, which is how it went
   unnoticed once. The check covers `src=` and `<link href=`; a plain `<a href>`
   is a navigation target, needs no egress, and does not fail the build.
4. Only the files `index.html` actually references are copied in.

`--build-arg KEEP_MOCKS=1` keeps the fixture backend, for a demo image that
runs with no cluster behind it.

## Install

With the chart, alongside the operator:

```sh
helm upgrade --install -n simplyblock simplyblock simplyblock/spdk-csi \
  --set operator.enabled=true \
  --set controlCenter.enabled=true
kubectl -n simplyblock port-forward svc/simplyblock-control-center 8080:80
```

The chart templates live in `helm-charts/charts/simplyblock-operator`
(`control-center*.yaml` and `_control_center_*.tpl`), with every value under
`controlCenter:`; `deploy/k8s/` holds the same objects as plain manifests if
you are not using Helm. The templates are **self-contained** — they define
their own `sbcc.*` helpers rather than calling the host chart's, so they
render whether they are dropped into the simplyblock chart or installed on
their own. Verify before applying:

```sh
helm template cc helm-charts/charts/simplyblock-operator --set controlCenter.enabled=true | kubectl apply --dry-run=client -f -
```

`controlCenter.rbac.create` must stay `true` in `serviceaccount` mode. Without
it the proxied token has no permissions, every request returns 403 and the
console renders empty.

## Runtime configuration

One image serves any cluster: `config.js` is generated at container start from
the environment, into the writable scratch mount. Nothing is baked in.

| Variable                   | Default                              | Meaning                                                          |
|----------------------------|--------------------------------------|------------------------------------------------------------------|
| `SB_NAMESPACE`             | `simplyblock`                        | namespace the console manages                                    |
| `SB_AUTH_MODE`             | `serviceaccount`                     | `serviceaccount` or `passthrough` — see below                    |
| `SB_K8S_API`               | `https://kubernetes.default.svc`     | API server                                                       |
| `SB_K8S_HOST`              | `kubernetes.default.svc`             | SNI and Host — must be on the API server's certificate           |
| `SB_K8S_CA_FILE`           | the projected SA CA                  | CA bundle used to verify it                                      |
| `SB_OPERATOR_URL`          | `http://simplyblock-operator:8080`   | operator API                                                     |
| `SB_PROMETHEUS_URL`        | `http://simplyblock-prometheus:9090` | metrics                                                          |
| `SB_LOGO_URL`              | `vendor/logo-white.svg`              | brand mark, vendored into the image                              |
| `SB_MOCK`                  | `false`                              | `true` runs on fixtures (needs a `KEEP_MOCKS=1` image)           |
| `SB_TOKEN_REFRESH_SECONDS` | `600`                                | how often every proxied token is re-read                         |
| `SB_CP_URL`                | `http://simplyblock-webappapi:5000`  | the local control plane v2 API, proxied as `/cp/local/`          |
| `SB_CP_TOKEN_FILE`         | the projected SA token               | bearer token for `/cp/local/`                                    |
| `SB_CP_CA_FILE`            | the image's system bundle            | CA that verifies an HTTPS `SB_CP_URL`                            |
| `SB_CP_HOST`               | the URL's host                       | SNI and Host for an HTTPS `SB_CP_URL`                            |
| `SB_CONTEXTS_FILE`         | `/etc/sb/contexts/contexts.json`     | the contexts file, read when present                             |
| `SB_LOCAL_CONTEXT_NAME`    | `local`                              | the local cluster's name among the contexts                      |
| `SB_LOCAL_LABELS`          | empty                                | `site=…,stage=…,tenant=…` labels of the local cluster            |
| `SB_LOCAL_NAMESPACES`      | `SB_NAMESPACE`                       | comma list of namespaces the console manages locally             |
| `SB_FEATURES`              | empty                                | comma list of feature flags, handed to the console as `features` |

Pointing `SB_K8S_API` at anything other than the in-cluster service means
moving `SB_K8S_HOST` and `SB_K8S_CA_FILE` with it: TLS verification stays on,
so the name has to match the certificate and the CA has to be one you mount.

## Contexts: several clusters in one console

A context is another Kubernetes cluster the console reaches through this pod.
The browser calls same-origin `/k8s/<name>/` and `/cp/<name>/`; nginx proxies
the first to that cluster's API server and the second to its control plane v2
API, each with a credential mounted into this pod. The browser never holds a
URL, a CA, or a token for any cluster: `config.js` lists only the names,
labels, namespaces, and base paths.

The local cluster is the context named by `SB_LOCAL_CONTEXT_NAME` (`local`),
served as before by `/k8s/`, and by `/cp/local/` for the control plane the pod
shares a cluster with. The control plane v2 API accepts a ServiceAccount token
of the cluster it runs in, so `/cp/local/` uses the pod's projected token (or
`SB_CP_TOKEN_FILE`), and a remote context's credential is a ServiceAccount
token from that remote cluster.

### The contexts file

`SB_CONTEXTS_FILE` (`/etc/sb/contexts/contexts.json`) is a JSON array, read
once at container start. It is optional: without it the console shows the
local cluster only.

```json
[
  {
    "name": "site-b",
    "kubernetesApi": "https://10.1.2.3:6443",
    "kubernetesApiHost": "kubernetes",
    "namespaces": ["simplyblock"],
    "labels": {"site": "ams2", "stage": "prd"},
    "controlPlane": {"url": "https://cp.site-b.example:5000", "host": "cp.site-b.example"}
  }
]
```

| Field               | Meaning                                                                                                       |
|---------------------|---------------------------------------------------------------------------------------------------------------|
| `name`              | the path segment and the name the console shows: lowercase letters, digits, and dashes, at most 32 characters |
| `kubernetesApi`     | the API server, HTTPS; the entrypoint refuses to start without it                                             |
| `kubernetesApiHost` | SNI and Host for the API server, a name on its certificate; defaults to the URL's host                        |
| `namespaces`        | the namespaces the console manages there                                                                      |
| `labels`            | `site`, `stage`, and `tenant`: how the fleet views group the cluster                                          |
| `controlPlane.url`  | the control plane v2 API, reached directly; when omitted, `/cp/<name>/` answers a 503 `Status`                |
| `controlPlane.host` | SNI and Host for the control plane; defaults to the URL's host                                                |

The names `api`, `apis`, `version`, `openapi`, `healthz`, `livez`, `readyz`,
`metrics`, and `local` are refused, and so is a duplicate: a context named
`apis` would shadow `/k8s/apis/`, and nginx would accept that silently.
Validation is fatal on purpose, because a console that proxies the wrong
cluster is worse than one that does not start.

A control plane is never reached through the API server's `services/proxy`:
the kube-apiserver strips the `Authorization` header from a proxied request,
and the v2 API needs the bearer token. Each context names its control plane
URL directly, which also means the hub needs a route to it (the NetworkPolicy
opens what `egress[]` declares).

### The per-context Secret

Each context's credentials live in a directory named after it, next to the
contexts file: `/etc/sb/contexts/<name>/`. In Kubernetes that is one Secret
per context, mounted read-only.

| Key         | Required  | Used for                                                                                                                              |
|-------------|-----------|---------------------------------------------------------------------------------------------------------------------------------------|
| `token`     | yes       | the bearer token for `/k8s/<name>/`: a ServiceAccount token of that cluster                                                           |
| `ca.crt`    | for HTTPS | verifies the API server's certificate                                                                                                 |
| `cp-token`  | no        | the bearer token for `/cp/<name>/`; falls back to `token`, which the control plane v2 API accepts                                     |
| `cp-ca.crt` | no        | verifies the control plane's certificate; falls back to `ca.crt`, then to the image's system bundle for a publicly signed certificate |

Every token, local and per context, is re-read every
`SB_TOKEN_REFRESH_SECONDS` and nginx is reloaded once, so a rotated Secret
takes effect without a restart. The contexts file itself does not: adding a
context rolls the pods (the chart annotates the pod template with its
checksum).

### Producing the credential on the remote cluster

`deploy/k8s/remote-access.yaml` (with the chart,
`controlCenter.remoteAccess.enabled=true`, which needs no console there)
creates on the remote cluster a ServiceAccount holding the console's
ClusterRole and a long-lived token Secret. A hub's console acts on a remote
cluster with exactly the authority it has at home, which is why the chart
renders both roles from one shared rule list and the plain manifest binds the
ServiceAccount to the ClusterRole from `rbac.yaml`.

```sh
# on the remote cluster
kubectl -n simplyblock get secret simplyblock-control-center-remote-token -o jsonpath='{.data.token}'  | base64 -d > token
kubectl -n simplyblock get secret simplyblock-control-center-remote-token -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt
# on the hub
kubectl -n simplyblock create secret generic cc-ctx-site-b --from-file=token --from-file=ca.crt
```

Rotate by deleting the Secret on the remote cluster (the token controller
re-issues it) and copying the new token over; the console picks it up on the
next refresh.

### With Helm

`controlCenter.contexts[]` carries the same fields plus `secretName` (the
Secret above) and `egress[]` (`{cidr, port}` pairs the NetworkPolicy opens).
The chart renders the ConfigMap `<fullname>-contexts`, mounts it and every
Secret, and sets the environment. `controlCenter.localContext` names and
labels the local cluster, `controlCenter.controlPlaneUrl` overrides the local
control plane (empty means the chart's own service, HTTPS verified against the
chart's CA bundle when `tls.enabled`), and `controlCenter.features` lists
feature flags. With `controlCenter.mock.enabled` every context's URLs point at
the mock's `/k8s/<name>` and `/cp/<name>` routes, so a demo shows a fleet from
one pod.

### With plain manifests

`deploy/k8s/contexts-example.yaml` holds the ConfigMap and a Secret skeleton,
and `deployment.yaml` has the matching volume and mount commented out.

## Streaming through an Ingress

Watches on `/k8s/*` and SSE on `/cp/*` are long-lived responses. The pod's
nginx is configured for them (no buffering, hour-long timeouts), and the
Ingress in front has to be too, or events arrive in bursts and streams drop
every minute. For ingress-nginx:

```yaml
nginx.ingress.kubernetes.io/proxy-buffering: "off"
nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
```

Serve the console over HTTP/2, which any TLS-terminating Ingress does by
default: browsers cap HTTP/1.1 connections per origin at about six, and a
dashboard with several clusters keeps more streams than that open at once.
Over HTTP/1.1 the extra requests queue behind the open streams and the console
appears stuck.

## Who can do what

**Per-user authority is now a feature of the console, not just a deployment
option.** RBAC-DESIGN.md defines roles, scoped bindings and what the console
hides for a given user. That model needs the console to act *as the user* —
so with it, `SB_AUTH_MODE=passthrough` behind an OIDC proxy is the intended
mode, and `serviceaccount` becomes the single-tenant shortcut it always was:
everyone who reaches the pod is whoever the ServiceAccount is. The
"View as" switcher in the identity menu exists only on the fixture backend.

**This is the decision to get right.** In `serviceaccount` mode the pod attaches
its own ServiceAccount token to every proxied request. The browser holds no
credential — good — but the console's authority is then the ClusterRole in
`deploy/k8s/rbac.yaml`, shared by **everyone who can reach the Service**. There
is no per-user identity and nothing in the audit log attributing an action to a
person.

So authentication in front of the Service is required. In order of preference:

1. **`SB_AUTH_MODE=passthrough` behind an OIDC/OAuth2 proxy** that injects the
   user's own bearer token. Kubernetes then enforces that user's RBAC, the
   audit log names them, and the pod needs no permissions at all — set
   `controlCenter.rbac.create=false`.
2. **`serviceaccount` behind an authenticating proxy.** One shared role, but at
   least the door is locked.
3. **No Ingress: `kubectl port-forward`.** The console inherits whoever holds
   the kubeconfig. This is the default in `values.yaml` and the safest place to
   start.

Basic auth is shipped as an annotation placeholder in the Ingress so the
manifest is not silently open. Replace it; do not just delete it.

### What the role grants

Read across the simplyblock CRDs, core objects, and the Ramen kinds as
*instances only* — redefining `ramendr.openshift.io` CRDs breaks a supported
RHACM install. Writes are deliberately narrow:

- **create** on the `*Ops` kinds. An action is not a verb against an entity: a
  shutdown, restart, migration or failover is an Ops object the operator
  reconciles. Nothing here grants delete on a StorageCluster, node, device,
  pool or volume.
- **create/delete** on `ReplicationPair` and `ReplicationPolicy` — these are
  configuration, and the operator refuses the delete while anything still
  references them.
- **patch** on PVCs, because a PVC annotation is the entire membership model for
  both replication and backup policies.
- **get/list** on Secrets, for the Helm release view only (Helm stores releases
  as Secrets). No other Secret is read: a backup `credentialsSecretRef` is only
  ever shown by name.

## Security posture

- non-root (uid 101), `readOnlyRootFilesystem`, all capabilities dropped,
  `RuntimeDefault` seccomp
- two writable mounts, both `emptyDir` in memory: `/tmp/nginx` for the generated
  config, the proxied token and nginx's temp files, and `/etc/nginx/conf.d`
  because the image renders its server block from a template at startup
- CSP with no external origin and no `unsafe-eval` — nothing is fetched
  off-cluster, and the JSX is precompiled in the image (`deploy/build/bundle.mjs`). The
  headers live in an included snippet and are repeated in every location that
  sets its own `Cache-Control`, because nginx discards inherited `add_header`
  directives as soon as a level declares one of its own — `always` does not
  change that, and declaring them once at server level would have dropped them
  from the document itself
- `DENY` framing, `nosniff`, `no-referrer`
- a NetworkPolicy capping egress to the in-namespace upstreams, the API
  server, each context's declared `egress[]`, and DNS. The API-server rule
  ships as the RFC1918 ranges, because the right value is cluster-specific —
  **narrow it** to your API server endpoint
  (`kubectl get endpoints kubernetes -n default`) or your service CIDR. A rule
  with ports and no `to:` selector would allow 443 to the whole internet
- every proxied token, the projected ServiceAccount token and each context's,
  is re-read on a timer and nginx reloaded, because a token read once at
  startup expires while the pod still runs
- context names are validated at startup against a pattern and a reserved
  list, so no context can shadow `/k8s/apis/` or `/cp/local/`; API URLs, CA
  paths, and tokens stay in the pod and never reach `config.js`

## Probes and scaling

`/healthz` is served by nginx directly and deliberately **not** proxied, so a
probe reports on this container rather than on the health of the cluster behind
it. The console keeps no server-side state — its location lives in the browser's
`localStorage` — so replicas need no coordination and a rolling update is safe.

## Caveats

**`dist/` is a build artifact.** The `.jsx` files are the source; `dist/app.js`
and `dist/mock.js` are their precompiled form. The image rebuilds them
(`deploy/build/bundle.mjs`) so a stale checked-in bundle can never ship; for the
design preview, rebuild after editing any `.jsx` (`build.md`).

**`index.html` is `no-store`.** `dist/*.js` caches for an hour, but the
shell must not, or a rolling update would leave a stale document loading new
sources.

**The API surface is still partly inferred.** `CRD-MAIN.md` records which parts
of the console read real `storage.simplyblock.io/v1alpha1` fields and which
still run on `operator /proposed/*` because no CRD models them yet. The proxy
routes both; the second set will move as the CRDs land.
