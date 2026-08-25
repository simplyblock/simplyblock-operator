# Escalation primitives

The grants that are cluster-admin in effect, whatever the role is called. Each
one is a step an attacker who compromised one workload can take to become
another, and the point of listing them is that they are the grants worth an
argument, while the rest are bookkeeping.

The classification follows the standard Kubernetes escalation set. What is
specific to this repository is who holds each one, what contains it, and what the
narrower form would be.

Counts measured 2026-08-25 with `scripts/check-rbac.py`. Re-measure rather than
trusting them.

## The primitives

| Primitive                                              | Severity | Why it is escalation                                                                      |
|--------------------------------------------------------|----------|-------------------------------------------------------------------------------------------|
| `escalate` on roles                                    | critical | Grants the holder any permission, bypassing the check that a grantor cannot exceed itself |
| `bind` on roles                                        | critical | Binds any role, `cluster-admin` included, to any subject                                  |
| `impersonate`                                          | critical | Acts as any user or group, `system:masters` included                                      |
| `pods/exec`, `pods/attach`, `pods/ephemeralcontainers` | critical | Runs code in an existing pod and inherits its service account                             |
| `serviceaccounts/token` create                         | critical | Mints a token for any service account                                                     |
| `verbs: ["*"]` or `resources: ["*"]`                   | critical | Everything, including resources a future Kubernetes release adds                          |
| `create` on pods                                       | high     | Schedules a privileged pod that mounts the host filesystem                                |
| `get`/`list` on secrets                                | high     | Reads every service-account token in scope, and so becomes any of them                    |
| write on roles or bindings                             | high     | Writes RBAC. Capped at the grantor's own permissions without `escalate` or `bind`         |
| write on webhook configurations                        | high     | Disables or redirects admission control, bypassing every policy                           |
| write on nodes, `nodes/proxy`                          | medium   | Relabels or cordons nodes, redirecting where workloads land                               |
| `create` on workloads                                  | medium   | Creates a Deployment or Job that runs with its namespace's service account                |
| write on serviceaccounts                               | medium   | Creates identities, which with a binding is a new subject                                 |

None of `escalate`, `bind`, `impersonate`, `serviceaccounts/token`, or a wildcard
appears anywhere in this repository. That is worth keeping true, and the checker
reports any of them as critical the moment one appears.

## What is held today

| Primitive            | Count | Held by                                                                                 |
|----------------------|-------|-----------------------------------------------------------------------------------------|
| `pods/exec`          | 5     | `simplyblock-storage-node-role`, in the Go builder and duplicated in three chart copies |
| `create` on pods     | 5     | the same role                                                                           |
| secret read          | 16    | the manager's markers (5 controllers), the generated role, and five chart roles         |
| RBAC write           | 6     | the manager, for `reconcileRBAC`                                                        |
| webhook write        | 4     | the manager, for its own webhook configuration                                          |
| node write           | 9     | the manager and the storage node                                                        |
| workload create      | 15    | the manager and the storage node                                                        |
| serviceaccount write | 4     | the manager, for `reconcileRBAC`                                                        |
| default-SA binding   | 2     | `controlplane_clusterrole.yaml`, for `simplyblock-service-reader` and `fluent-bit-read` |

## The finding that matters most

`simplyblock-storage-node-role` grants **cluster-wide `pods` and `pods/exec` with
`list, get, create, delete, watch`**, plus `create`/`delete` on Deployments and
Jobs and write on Nodes. It is built in
`operator/internal/utils/storage_nodeset_ds.go:422` and duplicated verbatim in
`helm-charts/.../storage-node.yaml` and the CSI chart's copy.

The subject is the storage-node DaemonSet, which already runs `privileged: true`
on `hostNetwork: true` with host paths. So an escape from that container does not
merely reach the host, it reaches `pods/exec` on every pod in the cluster, which
is every service account in the cluster. **The privileged container and the
cluster-wide exec compound**, and either alone would be a much smaller problem.

What it appears to need it for is managing the SPDK proxy pods and reading pod
state, because `spdk_process_is_up` lists pods through the API. That is a
namespace-scoped need:

- A **`Role` in the operator's namespace** instead of a `ClusterRole` removes the
  cluster-wide reach without changing the behavior, unless the storage node
  genuinely manages pods in other namespaces.
- `namespaces` needs `get;list;watch` at most, never `create;delete`.
- Splitting the rule separates the read the health check needs from the
  `create;delete;exec` the proxy management needs, so the first can stay broad
  and the second narrow.

Being duplicated in three places, the fix is also a `code-cleanup` deduplication:
the chart copies and the Go builder must not disagree about a security boundary.

## Cluster-wide secret read

Sixteen grants read Secrets, and the manager's is cluster-scoped because a marker
can only generate a ClusterRole. Cluster-wide secret read is, on its own,
equivalent to holding every service account in the cluster.

It is probably necessary in part: the operator reads cluster secrets, TLS
material, and the control-plane credentials, and those live in whatever namespace
a `StorageCluster` was created in. What has never been established is whether it
needs *every* namespace, or only the namespaces it has a CR in. Two narrowings
are available without a design change:

- `resourceNames` on the markers whose secret is known by name. The control-plane
  credential secret and the TLS secrets have generated but derivable names.
- A namespaced `Role` created per watched namespace, which is more machinery and
  is the honest answer if the operator is meant to be namespace-scoped at all.

Until one of those happens, the grant stands and the justification should say
which secrets are read and by which controller, so the next reader can narrow it.

## RBAC write, and why it is contained

The manager holds `create;update;patch;delete` on `clusterroles` and
`clusterrolebindings`, which reads alarmingly and is the least alarming item on
this page. `reconcileRBAC` uses it to give the storage-node DaemonSet an identity,
which an operator that deploys a component has to be able to do.

The containment is Kubernetes' own: **without `escalate` or `bind`, a grantor
cannot create a role holding permissions it does not itself hold.** The manager
has neither verb, so the manager's own role is the ceiling on everything it
creates. That is the actual reason to keep the manager's markers narrow, and the
reason the secret-read row above is more serious than this one.

## Bindings to the `default` ServiceAccount

`controlplane_clusterrole.yaml` binds two ClusterRoles to the release namespace's
`default` ServiceAccount: `simplyblock-service-reader`, over services, pods,
endpoints, and nodes, and `fluent-bit-read`, over pods and namespaces.

Two things are wrong with it independently of the verbs, which are read-only:

- **Every pod in the namespace that does not name a service account gets both**,
  including any workload a user happens to run there.
- **Neither can be revoked from one component.** There is no way to take
  `fluent-bit-read` away from Fluent Bit without taking it from everything else
  that defaulted.

The fix is mechanical: a ServiceAccount per component, named in the pod spec, and
the binding pointed at it. It also makes the audit trail work, because a grant
then names its holder.
