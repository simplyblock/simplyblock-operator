# Access control — model and Kubernetes mapping

## The constraint that shapes everything

Kubernetes RBAC scopes by **namespace** or by **`resourceNames`** — and
`resourceNames` only works for `get/update/patch/delete`, never `list/watch/
create`. It has no notion of "this storage cluster" or "this pool": a
StorageNode is its own CR, so "read cluster A but not B" would mean enumerating
every node, device and volume name of A in a rule, and `list` would still leak
B. The requirement — scopes orthogonal to namespaces, bindings limited to a
managed Kubernetes cluster or a storage cluster, per-application DR rights — is
**not expressible in Kubernetes RBAC alone**.

So the model is two layers, and both are real:

```
┌──────────────────────────────────────────────────────────────────────┐
│  1. Kubernetes RBAC — the floor the API server enforces              │
│     ClusterRole simplyblock:<role>  ×  RoleBinding (namespace-wide)  │
│     identities: whoever the API server authenticated (OIDC/x509)     │
├──────────────────────────────────────────────────────────────────────┤
│  2. Operator ACL — the scope, enforced by the admission webhook      │
│     AccessRole    named set of {entity, ops[]}                       │
│     AccessBinding {subject, roleRef, scope}                          │
│     scope ∈ global | k8scluster/<n> | storagecluster/<n> |           │
│             storagepool/<n> | application/<n>                        │
└──────────────────────────────────────────────────────────────────────┘
```

Layer 1 says *whether a user may touch StorageNodes at all*. Layer 2 says
*which ones*. The webhook rejects any write to a `storage.simplyblock.io`
object whose owning cluster/pool/application is outside every scope the
subject is bound to, and the operator's `GET /access/self` returns the
effective rights so a client can render only what applies. Reads that Kubernetes
RBAC allows but the ACL denies are filtered by the operator's list endpoints
and by the console — which is why the console reads through the operator for
scoped kinds rather than listing CRs directly.

**Same identities everywhere.** A subject is a Kubernetes `User` or `Group`
exactly as the API server names them (`oidc:alice@corp.example`,
`system:authenticated`, `oidc:storage-admins`). There is no separate user
database. This is what "embedded in Kubernetes RBAC" means in practice, and it
is why `SB_AUTH_MODE=passthrough` is the mode this feature needs: the console
must act *as the user* for either layer to know who is asking.

## Entities and operations

Rights attach to **main entities**; a right on a main entity covers every
sub-object beneath it with the same operation.

| entity | covers | ops |
|---|---|---|
| `k8scluster` | worker nodes, discovery, deployment documents | C R U D |
| `storagecluster` | hosts, nodes, devices, tasks, logs, migrations, cluster settings incl. S3 target and KMS endpoint | C R U D |
| `storagepool` | volumes, snapshots, clones, storage classes, PVCs, backups, buckets, **KEKs in the KMS** | C R U D + `restoresource` |
| `backupop` | per-volume backup and restore *operations* | `backup` `restore` |
| `replicationpolicy` | pairs, policies, slots, replication ops | C R U D |
| `backuppolicy` | | C R U D |
| `drpolicy` | protection plans, methods, migration paths | C R U D |
| `application` | protected applications, recipes, DRPCs | C R U D + `failover` `failback` `fence` |
| `role` | AccessRole | C R U D |
| `binding` | AccessBinding | C R U D |

Rules the console enforces and the webhook must mirror:

- **Creating a storage cluster is `create` on the `k8scluster`** it is
  deployed to (§9). Approving a deployment document is that check.
- **Policies are cross-cluster**: `replicationpolicy`, `backuppolicy` and
  `drpolicy` rights are evaluated on **every cluster the policy touches**, and
  all must pass (§10). Reading a policy includes its backlog.
- **Applications**: `create` on the **source** cluster; `read/update/delete/
  failover/failback/fence` on the application's cluster *or* on the
  application itself — an `application/<name>` scope grants one app, a
  `storagecluster/<name>` scope grants all apps there (§10).
- **Restore** needs three things (§11): `backupop.restore` on the storage
  cluster, `storagepool.restoresource` on the pool the backup came from, and
  `storagepool.create` on the pool being restored into. Backup needs
  `backupop.backup` on the cluster and `read` on the pool.
- **Bindings**: a global admin binds any subject to any role at any scope and
  authors roles (§5). A cluster admin binds within scopes *contained in* the
  clusters they administer and cannot author roles (§6). The webhook rejects
  a binding whose scope is not contained in one the author holds `binding.
  create` on.

## Scope containment

```
global
 └─ k8scluster/<k>            storage clusters deployed on k
     └─ storagecluster/<c>    pools of c · applications whose source is c
         ├─ storagepool/<p>
         └─ application/<a>
```

A binding at a scope grants its role on everything beneath. Effective rights
on an object = union over bindings whose scope contains the object.

## Pre-defined roles

| role | rights |
|---|---|
| `global-admin` | everything, incl. `role` and `binding` CRUD; only meaningful at `global` |
| `cluster-admin` | CRUD `storagecluster` `storagepool` `backupop.*` all three policies `application.*`; `binding` CRUD **within scope**; no `role` |
| `storage-operator` | CRUD `storagecluster` `storagepool`; R on policies |
| `volume-admin` | CRUD `storagepool`, `backupop.*`; R `storagecluster` |
| `backup-operator` | `backupop.*`, `storagepool` R + `restoresource`, CRUD `backuppolicy` |
| `dr-operator` | R everything; CRUD `replicationpolicy` `drpolicy` `application`; `failover` `failback` `fence` |
| `viewer` | R on every entity |

Pre-defined roles are immutable. Custom roles are authored from the same
rights matrix by anyone with `role.create`, and a role in use cannot be
deleted.

## What the console does with it (§3)

- An object the subject cannot `read` is **not rendered** — not greyed, absent.
  Layers with nothing readable are absent from drill-down cards and the
  section switcher.
- Every action carries the operation it needs. The action menu shows only
  actions the subject holds; "New …" buttons need `create`; a kebab with
  nothing left is not drawn. Same for `restore` (three checks), `failover`,
  `fence`.
- Identity is shown in the top bar with the roles that apply *here*, and the
  fixture backend has a **View as** switcher so every pre-defined role can be
  walked in the preview.

## CRD change requests

Neither kind exists in `storage.simplyblock.io/v1alpha1`. Proposed shape,
served today from `operator /proposed/access-*`:

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: AccessRole
metadata: {name: dr-operator}
spec:
  builtin: true
  rights:
    - entity: application
      ops: [read, update, failover, failback, fence]
---
kind: AccessBinding
metadata: {name: alice-dr-prod}
spec:
  subject: {kind: User, name: "oidc:alice@corp.example"}
  roleRef: dr-operator
  scope:  {kind: storagecluster, name: prod-eu-central-1}
status:
  effective: true           # false when the scope no longer resolves
  boundBy: "oidc:root@corp.example"
```

Plus one read endpoint, `GET /access/self → {user, groups, bindings[],
rights[]}`, and the webhook. The ClusterRoles for layer 1 are generated from
the same pre-defined roles so the two layers cannot drift.
