# Packaging the Control Center as a pod

The console is a static document transpiled in the browser, served by nginx,
with an in-pod reverse proxy in front of the three APIs it is allowed to use.
There is no build step for the application itself and no server-side code.

```
browser ──► nginx (this pod) ──┬──► kube-apiserver          /k8s/*
                               ├──► simplyblock-operator    /operator/v1/*
                               └──► Prometheus              /prometheus/api/v1/*
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
   `mock-*.jsx` reference survives. No fixture data ships to a cluster.
2. **React, Babel and both typefaces are vendored** into `/vendor`, and the
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

`deploy/helm/` holds the templates and the values to merge into the chart;
`deploy/k8s/` holds the same objects as plain manifests if you are not using
Helm. The templates are **self-contained** — they define their own `sbcc.*`
helpers rather than calling the host chart's, so they render whether they are
dropped into the simplyblock chart or installed on their own. Verify before
applying:

```sh
helm template cc deploy/helm --set controlCenter.enabled=true | kubectl apply --dry-run=client -f -
```

`controlCenter.rbac.create` must stay `true` in `serviceaccount` mode. Without
it the proxied token has no permissions, every request returns 403 and the
console renders empty.

## Runtime configuration

One image serves any cluster: `config.js` is generated at container start from
the environment, into the writable scratch mount. Nothing is baked in.

| Variable | Default | Meaning |
|---|---|---|
| `SB_NAMESPACE` | `simplyblock` | namespace the console manages |
| `SB_AUTH_MODE` | `serviceaccount` | `serviceaccount` or `passthrough` — see below |
| `SB_K8S_API` | `https://kubernetes.default.svc` | API server |
| `SB_K8S_HOST` | `kubernetes.default.svc` | SNI and Host — must be on the API server's certificate |
| `SB_K8S_CA_FILE` | the projected SA CA | CA bundle used to verify it |
| `SB_OPERATOR_URL` | `http://simplyblock-operator:8080` | operator API |
| `SB_PROMETHEUS_URL` | `http://simplyblock-prometheus:9090` | metrics |
| `SB_LOGO_URL` | `vendor/logo-white.svg` | brand mark, vendored into the image |
| `SB_MOCK` | `false` | `true` runs on fixtures (needs a `KEEP_MOCKS=1` image) |
| `SB_TOKEN_REFRESH_SECONDS` | `600` | how often the proxied token is re-read |

Pointing `SB_K8S_API` at anything other than the in-cluster service means
moving `SB_K8S_HOST` and `SB_K8S_CA_FILE` with it: TLS verification stays on,
so the name has to match the certificate and the CA has to be one you mount.

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
- CSP with no external origin — nothing is fetched off-cluster. `unsafe-eval` is
  required because Babel transpiles in the browser; see the caveat below. The
  headers live in an included snippet and are repeated in every location that
  sets its own `Cache-Control`, because nginx discards inherited `add_header`
  directives as soon as a level declares one of its own — `always` does not
  change that, and declaring them once at server level would have dropped them
  from the document itself
- `DENY` framing, `nosniff`, `no-referrer`
- a NetworkPolicy capping egress to the three upstreams plus DNS. The
  API-server rule ships as the RFC1918 ranges, because the right value is
  cluster-specific — **narrow it** to your API server endpoint
  (`kubectl get endpoints kubernetes -n default`) or your service CIDR. A rule
  with ports and no `to:` selector would allow 443 to the whole internet
- the projected ServiceAccount token is re-read on a timer and nginx reloaded,
  because a token read once at startup expires while the pod still runs

## Probes and scaling

`/healthz` is served by nginx directly and deliberately **not** proxied, so a
probe reports on this container rather than on the health of the cluster behind
it. The console keeps no server-side state — its location lives in the browser's
`localStorage` — so replicas need no coordination and a rolling update is safe.

## Caveats

**Babel in the browser.** The document is transpiled on load, which costs about
a second on first paint and forces `unsafe-eval` in the CSP. For a shipped
product this should become a real build step — bundle the `.jsx` at image build
time and drop both the Babel vendor file and `unsafe-eval`. The change is
confined to `prepare.mjs` and the CSP header; no application code moves.

**`index.html` is `no-store`.** The `.jsx` sources cache for an hour, but the
shell must not, or a rolling update would leave a stale document loading new
sources.

**The API surface is still partly inferred.** `CRD-MAIN.md` records which parts
of the console read real `storage.simplyblock.io/v1alpha1` fields and which
still run on `operator /proposed/*` because no CRD models them yet. The proxy
routes both; the second set will move as the CRDs land.
