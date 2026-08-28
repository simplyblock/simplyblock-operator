---
name: rbac-hardening
description: Grant and audit Kubernetes permissions in this repository under least privilege: the kubebuilder RBAC markers that generate the manager's ClusterRole, the roles the operator builds in Go and applies to what it deploys, the charts' hand-written roles and bindings, and the privileged security contexts of the storage node and CSI plugins. Covers the escalation primitives that are cluster-admin in effect, what privilege this product genuinely requires and why, and the justification a reviewed grant carries. Use when adding or widening a permission, adding an RBAC marker, writing a chart role or binding, setting a security context, or running a privilege or security audit.
---

# RBAC and privilege hardening

Least privilege here is not a posture, it is four questions asked of every grant:
**which verbs, on which resources, in which scope, and for how long.** A grant
that cannot answer all four is wider than the work.

References:

- `references/escalation-primitives.md`: the grants that are cluster-admin in
  effect, who holds each one today, and what contains it.
- `references/workload-hardening.md`: security contexts, host mounts, and
  capabilities: which are required by the storage data path and which are not.
- `scripts/check-rbac.py`: audits all four sources below:

  ```bash
  .claude/skills/rbac-hardening/scripts/check-rbac.py --changed
  .claude/skills/rbac-hardening/scripts/check-rbac.py --severity high
  ```

## Four places this repository grants a permission

Most tooling sees only the first. The second is the one nobody audits, and it is
where the widest grant in the product lives.

| Source                                               | Written as                                             | Reviewed by                                   |
|------------------------------------------------------|--------------------------------------------------------|-----------------------------------------------|
| The manager's own ClusterRole                        | `+kubebuilder:rbac:` markers, 153 of them              | `make -C operator manifests`, then this skill |
| **The roles the operator grants to what it deploys** | Go, in `operator/internal/utils/storage_nodeset_ds.go` | nothing, until now                            |
| The charts' roles, bindings, and service accounts    | hand-written YAML under each chart's `templates/`      | nothing, until now                            |
| The workloads' privilege                             | `securityContext`, `hostPath`, `hostNetwork`           | nothing, until now                            |

**Never hand-edit `operator/config/rbac/role.yaml`.** It is generated from the
markers. An edit survives until the next `make manifests` and no longer, so
narrow the marker instead. See `build-system`.

The second row deserves its own warning. When the operator creates a ClusterRole
for a component it deploys, it is acting as the grantor, and Kubernetes' escalation
prevention caps what it can grant at what it already holds. **The manager's own
role is therefore the ceiling on every role the product creates**, which is why
widening the manager's markers is never a local decision.

## The grant is as narrow as the work

In order of preference, and the first that suffices wins:

1. **A namespaced `Role`, not a `ClusterRole`.** A marker always generates a
   ClusterRole, so a permission that only needs one namespace is expressed as a
   chart-level `Role` or with `resourceNames`. Ask this first: it is the question
   that would fix the largest finding in `references/escalation-primitives.md`.
2. **`resourceNames`,** when the object is known by name. A controller that reads
   one Secret does not need to read every Secret.
3. **Read, not write.** `get;list;watch` unless something is actually written.
4. **The exact verbs.** Not the seven-verb block `controller-gen` scaffolds. A
   reconciler that never deletes does not carry `delete`.
5. **A distinct ServiceAccount per component**, never `default`. A binding to
   `default` grants the permission to every pod in the namespace that did not
   choose an identity, and it cannot be revoked from one component.

## Privilege this product genuinely requires

Some of it is real, and pretending otherwise makes the audit useless. The storage
node and the CSI node plugin run privileged, on the host network, with host
paths, because NVMe-oF attachment is a host-kernel operation: `/dev` for the
block devices, `/sys` for the NVMe tree, the kubelet plugin directory for the
socket, and mount propagation so a mount inside the container reaches the host.

**"Required" is not the same as "unexamined."** For each one the rule is:

- The narrowest form that works. A specific capability set beats `privileged:
  true`, a read-only mount of `/sys` beats a writable one, and one host path beats
  the parent directory.
- A `rbac-justified:` note next to it saying what needs it, so the next reader
  does not have to re-derive it and the checker stops reporting it.
- Containment stated: what an escape from this container reaches. A privileged
  container on the host network that *also* holds cluster-wide `pods/exec` is a
  different risk from one that holds nothing.

The manager pod is the counter-example: it needs no privilege at all, and
anything privileged in the manager's own spec is a finding rather than a
requirement.

## The justification, and what it is not

A grant the checker reports and a reviewer accepts carries a note on the line
above, in the comment syntax of wherever it lives:

```go
// rbac-justified: reconcileRBAC creates the storage node's ServiceAccount,
// ClusterRole, and ClusterRoleBinding, which is how the DaemonSet gets an
// identity. Capped by the manager's own role.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch;delete
```

```yaml
# rbac-justified: NVMe-oF connect writes /dev/nvme-fabrics on the host
privileged: true
```

The note says **what needs the permission**, not that it is needed. "Required by
the storage node" is not a justification. "The storage node reads its own pod to
resolve its NQN" is, because the next reader can check whether that is still
true and delete the grant when it is not.

A justification is not a waiver. It records a decision, and the decision is
reviewable. `check-rbac.py` prints justified findings with their reason so an
audit sees the whole surface, and `--unjustified` narrows to what nobody has
looked at yet.

## Widening a permission

1. **Say why the narrower form does not work.** In the change, not in the chat.
2. **Add the marker or the rule at the narrowest scope** that satisfies it.
3. `make -C operator manifests generate`, then `make helm-sync`. The generated
   role and the chart's copy both change, and a change to only one is drift that
   CI catches. See `build-system`.
4. `scripts/check-rbac.py --changed`. Anything critical or high that the change
   introduced is either narrowed or justified before the work goes back.
5. **If the grant is an escalation primitive**, say so in the change description
   and name the containment. `references/escalation-primitives.md` lists which
   ones those are.

## Auditing

`check-rbac.py` with no arguments reports the whole surface, which is the
backlog rather than one change's findings. For an audit:

```bash
scripts/check-rbac.py --severity high --unjustified   # what nobody has examined
scripts/check-rbac.py --source go                     # the roles the operator grants
scripts/check-rbac.py --source workload               # privilege in the charts
```

The checker reads source, not a cluster, so it runs in CI and finds what is
*intended* rather than what happens to be deployed. A live cluster can hold
grants no chart in this repository creates (an operator installed by hand, an
OLM-generated binding, a customer's own Role), so a cluster audit is a separate
exercise: `kubectl auth can-i --list --as=system:serviceaccount:<ns>:<sa>` per
service account, and `kubectl get clusterrolebindings -o yaml` compared against
what the chart creates.

## Before handing a privilege change back

1. `check-rbac.py --changed` reports nothing critical or high that this change
   introduced, or each is justified with what needs it.
2. The scope is the narrowest of the five in *The grant is as narrow as the work*
   that suffices, and the ones rejected were rejected for a stated reason.
3. No binding names the `default` ServiceAccount.
4. Manifests regenerated and the chart synced, so all four sources agree.
5. A new privileged container, host path, or host namespace carries its
   `rbac-justified:` note and its containment.
6. Nothing was added to `operator/config/rbac/role.yaml` by hand.
