# Recovering a namespace that will not finish deleting

What to do when `kubectl delete namespace simplyblock` sits in `Terminating`,
and why each blocker blocks.

This is here because the recovery has an order, and the order is not guessable
from the symptom: the thing that unsticks the namespace is usually a
cluster-scoped object outside it. Every blocker below was hit on one cluster in
one afternoon.

## The shape of the problem

Deleting the namespace deletes the operator first, or close enough to first that
it makes no difference. Everything that then remains is something that needed the
operator: finalizers it would have released, admission webhooks it would have
answered, cluster-scoped objects its finalizers would have deleted.

`helm uninstall` before deleting the namespace avoids all of it. The rest of this
page is for when that did not happen.

## Read the conditions first

```bash
kubectl get ns simplyblock -o jsonpath='{range .status.conditions[*]}{.type}={.status}: {.message}{"\n"}{end}'
```

The condition that is `True` names the blocker, and there may be more than one --
they are cleared one at a time, so expect to come back here after each step.

## Blocker 1: discovery fails

```
NamespaceDeletionDiscoveryFailure=True: ... metrics.simplyblock.io/v1alpha2:
stale GroupVersion discovery
```

An aggregated APIService whose backing Service died with the namespace. The
namespace controller cannot enumerate what to delete, so it does nothing at all.

This one is not confined to the namespace: aggregated discovery is cluster-wide,
so `kubectl api-resources` is degraded for everyone until it is gone.

```bash
kubectl delete apiservice v1alpha2.metrics.simplyblock.io
```

Nothing in the operator can prevent this. The APIService is cluster-scoped, so it
cannot carry an owner reference to anything in the namespace, and the finalizer
that would delete it needs the operator to still be running.

## Blocker 2: finalizers with nobody to release them

```
NamespaceFinalizersRemaining=True: ... storage.simplyblock.io/storagenode-finalizer
in 6 resource instances
```

The operator is gone, so nothing will release them. It cannot be brought back
either: a Terminating namespace accepts no new objects.

**Delete the webhook configurations first.** They are cluster-scoped, they
outlive the namespace, and the ones that intercept `update` or `delete` carry
`failurePolicy: Fail` -- so with the webhook's Service gone they refuse the very
patch that removes a finalizer:

```
Internal error occurred: failed calling webhook "vstoragenode.simplyblock.io":
... service "simplyblock-operator-webhook-service" not found
```

```bash
kubectl delete validatingwebhookconfiguration simplyblock-operator-validating-webhook-configuration
kubectl delete mutatingwebhookconfiguration simplyblock-operator-mutating-webhook-configuration

for k in controlplanes operatorops simplyblockdrivers storageclusters storagenodes storagepools; do
  kubectl get $k.storage.simplyblock.io -n simplyblock -o name |
    xargs -r -I{} kubectl patch -n simplyblock {} --type=merge -p '{"metadata":{"finalizers":[]}}'
done
```

Patching before deleting the webhooks half-works, which is the confusing part:
the kinds whose validator is `create`-only are patched, and the rest are refused.

What this skips is the operator's own teardown. Storage nodes are not removed
from the control plane -- but on a namespace delete the control plane is going
with them, so there is nothing left to deregister from.

## Blocker 3: pods that cannot be unmounted

```
NamespaceDeletionContentFailure=True: ... unexpected items still remain in
namespace: simplyblock for gvr: /v1, Resource=pods
```

Pods with a deletion timestamp, no finalizers, and no progress. Check what they
mount:

```bash
kubectl get pods -n simplyblock -o custom-columns='NAME:.metadata.name,STATUS:.status.phase,DELETED:.metadata.deletionTimestamp'
kubectl get pods -n simplyblock | grep csi-node
```

A pod holding a simplyblock volume cannot be unmounted once the CSI node plugin
is gone, and the plugin is in the namespace being deleted. The kubelet waits for
a `NodeUnstage` that nothing will answer.

```bash
kubectl delete pod -n simplyblock <pod> --grace-period=0 --force
```

The mount is then left behind on the worker. Where the volume was a simplyblock
one whose cluster is also being deleted there is nothing to corrupt; where it was
not, unmount it on the node before reusing it.

## Afterward: what survived

Cluster-scoped objects the operator's finalizers would have deleted are still
there, because the finalizers never ran:

```bash
for r in crd clusterrole clusterrolebinding apiservice csidriver sc \
         validatingwebhookconfiguration mutatingwebhookconfiguration; do
  echo "$r:"; kubectl get $r 2>/dev/null | grep -i simplyblock | awk '{print "  "$1}'
done
```

They matter for the next install. Helm's release metadata lives in Secrets inside
the namespace, so it died with it, and an object with no release to own it makes
the next `helm install` fail on ownership metadata. Either delete them or accept
adopting them by hand.

**Leave the cert-manager chain alone until it is clear who else uses it.**
`simplyblock-certificate-authority-issuer` has issued certificates to things
outside this release before now -- OpenBao's server certificate among them -- and
deleting the issuer leaves those unable to renew:

```bash
kubectl get certificate -A -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,ISSUER:.spec.issuerRef.name'
```

## Doing it in the right order next time

```bash
helm uninstall simplyblock -n simplyblock
kubectl delete namespace simplyblock
```

The uninstall runs while the operator is alive, so finalizers are released, the
webhooks are answered, and the cluster-scoped objects go with the release.
