# Access control — what the console implements

Source of truth: `uploads/simplyblock-multicluster-rbac-design.md`. This note
records only the console's part of it (§4 grants, §5 enforcement, §6 invariants)
and where the fixture stands in for a component that is not built yet.

## One policy decision point

The hub API server decides. The console asks — it never keeps an authorization
store of its own, and it never filters as a boundary. A grant made here and one
made with `kubectl` are the same RoleBinding.

```
grant (subject, role, scope)
  └─ RoleBinding in the scope's namespace, roleRef ClusterRole sb:<role>
  └─ AccessGrant when it needs expiry, a reason, or DR fan-out
       controller reconciles it into RoleBindings; webhook re-checks the
       requesting admin (bind on the role, or every rule of it) — §4.3
```

## Roles — eight aggregated ClusterRoles, no editor

`sb:infra-admin` (cluster scope) · `sb:cluster-admin` / `sb:cluster-reader`
(`sb-sc-*`) · `sb:pool-admin` / `sb:pool-reader` (`sb-sc-*` or `sb-sp-*`) ·
`sb:dr-admin` / `sb:dr-reader` (`sb-dr-system`) · `sb:app-admin` (application
namespace). Rules aggregate from ClusterRoles labelled
`simplyblock.io/aggregate-to-<role>: "true"`. The Roles tab is a read-only
reference. Actions are resources: failover is `create applicationfailovers`.

## Scopes → namespaces

| scope | namespace | note |
|---|---|---|
| cluster scope | — | `sb:infra-admin` only |
| managed cluster | `sb-mc-<name>` | |
| storage cluster | `sb-sc-<name>` | pools live here by default |
| pool | `sb-sp-<pool>-<hash>` | only when the pool is a tenant boundary; a grant on a shared pool covers every pool in `sb-sc-*` and the form says so |
| DR | `sb-dr-system` | |
| application | `<ns>@<cluster>` | with a DR policy: both members or neither (§4.4) |

The server returns id → namespace for every visible scope; the client never
derives names.

## What the console does (§5)

- **Read path** — scope discovery (`/access/scopes`, a SAR per candidate)
  decides which clusters, pools and applications appear. Inside a visible scope
  the list is shown as the API server returns it. A scope the caller may not
  read renders a **403 state**, never an empty list.
- **Write path** — one `SelfSubjectRulesReview` per namespace (aggregated in
  `/access/self`), evaluated locally, cached until identity changes.
  `incomplete: true` renders optimistically.
- **Disabled, not hidden** — every denied action, create button and role option
  is disabled with the missing `verb on resource in namespace` as tooltip.
  Only whole scopes are hidden.
- **Access screen** — scope tree · grants with a **source** column
  (`control-center` / `external`) · add grant with roles the caller cannot bind
  disabled and the reason shown, group-first with users flagged, expiry and
  reason · effective access per subject, labelled "grants issued through
  simplyblock" · SubjectAccessReview spot check · §6 invariants run against the
  authorizer on every load.

## Fixture stand-ins

`mock-rbac.jsx` plays the API server: SSRR, SAR, the AccessGrant webhook
(§4.3 bind check, §4.4 symmetry, actor stamping), and the admission envelope
(`SB_ENVELOPE`). "View as" stands in for impersonation. Invariants 5 and 8 are
backend-only and are listed as such rather than marked as passing.

## CRD change requests

`AccessGrant` (§4.3) and `NodePoolAllocation` (§2.4) do not exist in
`storage.simplyblock.io/v1alpha1`; both are served from `/proposed/`. The
`applicationfailovers` resource that makes failover grantable on its own is
likewise proposed.
