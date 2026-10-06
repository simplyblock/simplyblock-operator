# sb-mock and the multi-cluster / hub-agent paradigm

The console is being designed against a hub/agent, multi-tenant, drift-managed
model that **does not exist on `simplyblock-operator@main`**. It lives only in
the design docs — `operator/docs/designs/crd-redesign/` and, for RBAC,
`uploads/simplyblock-multicluster-rbac-design.md` (mirrored in
`control-center/RBAC-DESIGN.md`). sb-mock is shaped to that target model so the
UI can be exercised against it before the operator implements it.

This file records, principle by principle, **what the mock reproduces
faithfully** and **where it necessarily deviates** — because a mock impersonates
an API server, it can model the *shape and outcomes* of the paradigm but not the
parts that are real cryptographic identity, admission, or field-ownership.

The section numbers below reference the RBAC design doc.

---

## What the mock reproduces faithfully

### Namespace layout & tenants (§2.1, §2.2)
The world is **tenant-centric**: a tenant is a namespace, and it holds one or
more whole clusters plus that tenant's DR-policies and DR-applications. Global
admins create and manage the tenant namespaces; a tenant-admin provisions
objects into them.

| Namespace | Holds | scope-kind label |
|---|---|---|
| `simplyblock-system` (the `--namespace` value) | control plane, Helm release Secret, operator/agent pods | `system` |
| `sb-mc-<managed>` | ManagedCluster transport: the **projected** StorageCluster, the deployment config | `managed-cluster` |
| `sb-<tenant>` | the tenant / RBAC target — the StorageCluster + nodes/devices/pools/backups/ops, the DR-policy (replication) chain, and the DR-applications | `tenant` |
| `<application>` | k8s app workloads the recipe editor discovers | `application` |

Extra tenants (e.g. `sb-tenant-b` in `large-scale`/`chaos`) are generated
**empty** with only a tenant-admin grant — a namespace a global admin has
created and handed to a team, ready to provision into. Hierarchy is in labels
(`simplyblock.io/tenant` / `-managed-cluster` / `-storage-cluster` /
`-scope-kind`), never in names — the scope-projection API reads exactly these.

### Hub → agent projection & drift (§1.3, and design-crd-model §7.9)
- The StorageCluster exists twice: the user-facing object in the tenant
  namespace `sb-<tenant>` and a **projection** in `sb-mc-*` (same kind, not a
  wrapper) carrying `simplyblock.io/managed-by: hub` and a hub-owned spec
  subset (`nodePoolAllocationRef`).
- The projection's **status is agent-authored**: `lastSyncTime`,
  `agentConnected`, and `Applied`/`AgentConnected` conditions. The simulator
  plays the agent — it advances `lastSyncTime` each tick and heals drift.
- **`observedGeneration` is carried on every simplyblock kind** and the
  simulator maintains the applied/pending distinction: when the hub spec
  generation is ahead of the agent's `observedGeneration`, the object reads
  `Applied=Pending`; a tick later the agent catches up and it reads
  `Applied=True`. This is the exact primitive the design (§7.9) says the UI
  needs to tell "applied" from "pending".
- The ManagedCluster carries the **hub-only conditions** the managed cluster
  cannot know about itself: `AgentConnected` and `InSync`/`DriftDetected`.

### Actor provenance (§3.2)
Every hub-authored simplyblock object is stamped with the actor annotations a
mutating webhook would write: `simplyblock.io/actor-username`, `-uid`,
`-groups`, `-timestamp`.

### Role model & grants (§2.3, §4.2)
The role model is **product-facing**, not the uploaded design's eight
infra-roles (see the deviation note below). It is:

| Role | Bound at | What it grants |
|---|---|---|
| `sb:infra-admin` | cluster (global) | create/manage tenant namespaces, do the bindings (namespaces, accessgrants, clusterroles, rolebindings, nodepoolallocations, managedclusters, storageclusterclasses) |
| `sb:tenant-admin` | tenant namespace | **full CRUD** on every object in the tenant — the provisioning role that creates clusters, DR-policies and DR-applications |
| `sb:cluster-reader` | tenant namespace | **read-only** on the cluster main object + its sub-objects (nodes, devices, pools, backups, ops) |
| `sb:dr-policy-reader` | tenant namespace | **read-only** on the DR-policy main object + its sub-objects (replication pairs/policies/slots/ops) |
| `sb:dr-application-reader` | tenant namespace | **read-only** on the DR-application main object + its sub-objects (protected applications, application failovers) |

One full-admin (CRUD) role plus one read-only role per main object — for now
the three main objects are **cluster**, **DR-policy** and **DR-application**,
each reader covering that object and its sub-objects. Adding a fourth main
object is one entry in `sbRoles()` (tenancy.go).

- Each role is generated as an aggregated `sb:*` ClusterRole with a
  rules-carrying child labelled `simplyblock.io/aggregate-to-<role>`.
- Grants are real RoleBindings (and a ClusterRoleBinding for `sb:infra-admin`)
  labelled `simplyblock.io/managed-by: control-center` and `scope-kind: tenant`,
  plus an `AccessGrant` CR — so a grant "made in the UI" and one made with
  kubectl are the same object. Every tenant gets a tenant-admin binding; the
  primary tenant also gets the three reader bindings.
- The privileged-op envelope object `NodePoolAllocation`, plus
  `StorageClusterClass` and `ManagedCluster`, are generated (cluster-scoped).

### Authorization queries (§5)
The mock answers the console's read/write-path queries:
- `SelfSubjectRulesReview` (per namespace) → the viewer's resource rules.
- `SubjectAccessReview` / `SelfSubjectAccessReview` → allow/deny for a
  (namespace, verb, resource).
- The **scope-projection** (`GET /operator/v1/access/scopes`, and
  `/access/self`) → the scope tree the viewer may see, enumerated from the
  namespace hierarchy labels.

A `--viewer` flag selects the identity being modelled: `global` (default, a
global admin — matches the console's serviceaccount auth mode), `none`, or
`<sb:role>@<namespace>[,...]` to exercise the console's disabled/hidden/403
states.

---

## Where the mock necessarily deviates (and why)

These are the parts of the paradigm that cannot be reproduced by an API-server
mock. Each is a deliberate, documented stand-in.

1. **Real identity / impersonation — stand-in, not enforced.** The mock has no
   OIDC, no impersonation, no TokenReview. Authorization is answered from the
   `--viewer` config, and — as the design itself states for SSRR (§5.3) — the
   reviews are **display APIs**: the mock does **not** enforce them on the CRUD
   path. Every create/update/delete still succeeds regardless of viewer. So a
   scoped `--viewer` changes what the *access screen* renders, not what a write
   actually does. Enforcing for real needs the hub API server.

2. **Server-side apply field managers (design principle #4) — outcome modelled,
   mechanism absent.** The store uses merge/replace, not SSA field ownership.
   The hub-owned-vs-locally-resolved split is modelled as *shape* — a hub spec
   projection plus an agent-authored status on the transport copy — but distinct
   field managers, conflict detection, and `managedFields` are not tracked. Two
   writers can still clobber each other in the mock; only a real apiserver gives
   field-level ownership.

3. **`managed-by: hub` admission gate — labelled, not enforced.** The design
   has managed-cluster admission reject spec edits from any identity except the
   agent SA and surface local attempts as a status condition. The mock stamps
   the `managed-by: hub` label and models the *result* (the projection's status
   is agent-authored), but with no requester identity on the CRUD path it cannot
   reject a spec edit by identity. A write to a `managed-by: hub` object is
   accepted.

4. **The privileged-op envelope CEL (§2.4) — object present, not evaluated.**
   `NodePoolAllocation` is generated and referenced from the projected spec, but
   the `ValidatingAdmissionPolicy`/webhook that checks a StorageCluster's node
   selector and device paths against the allocation is not run. A
   `dryRun=All` create does not return an envelope violation.

5. **Agent registration (§1.3) — result shown, handshake skipped.** The mock
   shows the steady state (ManagedCluster registered, `sb-mc-*` namespace,
   mirrored status) but does not simulate the bootstrap-token → CSR →
   auto-approve → client-cert pull-model handshake. `agentConnected` is a
   generated fact, not the product of a real connection.

6. **Delegated tokens / RFC 8693 token exchange (§3) — out of scope.** The
   actor→operator→REST-API delegation and scope intersection live entirely in
   the operator and the simplyblock REST API, below the Kubernetes surface the
   console (and this mock) talk to. The mock stamps the actor annotations that
   *feed* that chain, but implements none of the token exchange.

7. **Role model simplified from the uploaded design (deliberate, per the
   product owner).** The uploaded RBAC design (and `control-center/RBAC-DESIGN.md`)
   specifies **eight** infra-oriented roles (`sb:cluster-admin`, `sb:pool-admin`,
   `sb:dr-admin`, `sb:app-admin` and their readers, scoped by `sb-sc-*` /
   `sb-sp-*` / `sb-dr-system`). This mock instead implements the refined
   **product-facing** model the owner asked for: one **full-admin (CRUD)** role
   per tenant plus **one read-only role per main object** (cluster, DR-policy,
   DR-application), all bound in the single tenant namespace, with a global
   `sb:infra-admin` managing the tenants. When the design and the model
   conflict, the product owner's model wins here. **The console side
   (`control-center/*.jsx` and `RBAC-DESIGN.md`) still describes the old
   eight-role model** — those are re-exported from Claude Design, so aligning
   them is a design-export change, not a mock change. The mock now serves the
   role set the UI should converge on.

8. **DR kinds mapping.** DR-policy's sub-objects are the simplyblock replication
   kinds (`replicationpairs/policies/slots/ops`); DR-application is
   `protectedapplications` + `applicationfailovers`. The mock also still
   generates the **Ramen** kinds (`ramendr.openshift.io`) the console consumes
   for the actual DR mechanism. There is no separate `sb-dr-system` namespace
   any more — DR objects live in the tenant, per the tenant-centric model.

9. **Pool tenant namespaces (`sb-sp-*`) not used.** Pools live in the tenant
   namespace (the flat default); the mock does not split a pool into its own
   `sb-sp-*` namespace.

10. **Namespace name.** The design names the control-plane namespace
   `simplyblock-system`; the mock uses the `--namespace` value (the chart
   installs into `simplyblock`). Pass `--namespace=simplyblock-system` to match
   the doc exactly.

11. **Everything is still a mock (unchanged from the base README).** Writes
    persist in memory and fire watch events but perform no real action; the
    simulator advances phases; no data path exists.

---

## Relationship to `main`

To be explicit, because it is easy to lose: **almost none of this is on
`main`.** The operator today is single-cluster, single-namespace, with
`observedGeneration` on only 3 of ~20 kinds and no hub/agent, projection,
AccessGrant, `sb:*` roles, or actor stamping. This mock is deliberately ahead of
the operator so the UI can be built and tested against the intended model; when
the operator implements the CRD redesign, the mock's shapes are the contract to
converge on.
