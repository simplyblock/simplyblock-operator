# Upgrading a 26.3.x Helm install to the v1alpha2 release

The manual procedure that carries a Helm install of release 26.3.x (tested on
`v26.3.2-1`) to the release that serves `storage.simplyblock.io/v1alpha2`, and
what is still missing before `simplyblock-upgrade` can do it unattended.

It is a runbook rather than a design because it records what was run and what
happened. The design is `designs/crd-redesign/design-api-upgrade.md`. Every
command below was run against a K3s cluster; the list of what is not covered is
at the end.

## Why a plain `helm upgrade` fails

Three things stop it, in this order:

1. `validate-controlplane.yaml` refuses the upgrade while the old control plane
   carries no `helm.sh/resource-policy: keep`. It used to refuse after the
   annotation was added as well, because it only tested for the release
   annotation. It now tests for the keep policy.
2. The chart renders `v1alpha2` kinds, and Helm applies a chart's `crds/` only on
   install. The CRDs have to be applied first, and `--force-conflicts` is needed
   because Helm owns `.spec.versions` of the CRDs it installed.
3. The applied CRDs convert through a webhook that nothing serves yet. Helm then
   cannot read the existing `ControlPlane`, and neither can the old operator, so
   the upgrade stops before it reaches the new operator, which is the thing that
   would serve conversion. The conversion webhook has to be running before the
   CRDs are applied.

## Procedure

Run it from a checkout of the target version, with `NS` the installation's
namespace and `OPERATOR_IMAGE` the target operator image. The conversion webhook
runs from the operator image, so the image has to be one that contains
`/usr/local/bin/conversion-webhook`.

### 1. Record what the release was installed with

```bash
helm get values simplyblock-operator -n "$NS" --all > /tmp/values-26.3.yaml
```

The target chart changes two defaults, `tls.enabled` and `tls.mutual_enabled`,
both to true, and the values schema rejects keys the target chart no longer has.
Pass the TLS values the release actually has in step 5, and drop the removed keys
(see "Values that moved").

### 2. Keep what the new chart no longer owns

Without the policy, Helm deletes these on upgrade and takes the FoundationDB
database with them:

```bash
for k in deployment/simplyblock-webappapi deployment/simplyblock-tasks \
  deployment/simplyblock-monitoring deployment/simplyblock-admin-control \
  deployment/simplyblock-fdb-controller-manager deployment/simplyblock-fdb-exporter \
  statefulset/simplyblock-minio foundationdbcluster/simplyblock-fdb-cluster; do
  kubectl -n "$NS" annotate "$k" helm.sh/resource-policy=keep --overwrite
done
```

### 3. Start the conversion webhook

```bash
kubectl kustomize operator/config/conversion-webhook \
  | sed -e "s/simplyblock-operator-system/$NS/g" \
        -e "s#quay.io/simplyblock-io/simplyblock-operator:0.1.0#$OPERATOR_IMAGE#" \
  | kubectl apply -f -
kubectl -n "$NS" rollout status deploy/simplyblock-operator-conversion-webhook
```

On a cluster whose DNS domain is not `cluster.local`, add
`--cluster-dns-domain=<domain>` to the container arguments first.

### 4. Apply the CRDs

```bash
kubectl apply --server-side --force-conflicts -f helm-charts/charts/simplyblock-operator/crds/
```

The CRDs ship a service reference to `simplyblock-operator-system` and no CA
bundle. The conversion webhook corrects the namespace on a 30-second timer. With
the change that accompanies this runbook it restores the CA there too, which has
a unit test and has not yet been run from a built image. Wait for both:

```bash
kubectl get crd controlplanes.storage.simplyblock.io \
  -o jsonpath='{.spec.conversion.webhook.clientConfig.service.namespace} {.spec.conversion.webhook.clientConfig.caBundle}' | cut -c1-40
kubectl -n "$NS" get controlplanes.v1alpha2.storage.simplyblock.io
```

An image built before this change never restores the CA, so the second command
fails with `x509: certificate signed by unknown authority`. Copy it by hand from
the webhook's own secret into each of the seven converted CRDs
(`controlplanes`, `storagebackups`, `storageclusterops`, `storageclusters`,
`storagenodeops`, `storagenodes`, `storagepools`):

```bash
CA=$(kubectl -n "$NS" get secret conversion-webhook-server-cert -o jsonpath='{.data.ca\.crt}')
kubectl patch crd controlplanes.storage.simplyblock.io --type merge \
  -p "{\"spec\":{\"conversion\":{\"webhook\":{\"clientConfig\":{\"caBundle\":\"$CA\"}}}}}"
```

### 5. Upgrade the release

```bash
helm upgrade simplyblock-operator helm-charts/charts/simplyblock-operator -n "$NS" \
  --set deployment.profile=standalone \
  --set tls.enabled=<from step 1> --set tls.mutual_enabled=<from step 1> \
  --set image.operator.repository=... --set image.operator.tag=...
```

### 6. Check the result

- `helm history simplyblock-operator -n "$NS"` shows the new revision `deployed`.
- The `ControlPlane` has the same UID and creation time as before, and its phase
  is `Available`.
- The `FoundationDBCluster` has its old creation time, its pods and PVCs are the
  old ones, and nothing was recreated.
- The chart stopped installing the CSI driver and the operator creates it from
  the `SimplyblockDriver`. The CSI pods are replaced, and the driver reaches
  `Ready` with all nodes. No volume was attached during the test, so what the
  swap does to a mounted volume is untested.
- No pod is crash-looping. The `tasks` pod restarts while the control-plane
  image changes.

What this procedure produced on a three-worker K3s cluster with TLS and mutual
TLS: revision 2 `deployed` after 21 seconds, the `ControlPlane` and the
FoundationDB cluster untouched, a new operator `1/1`, the `SimplyblockDriver`
`Ready 4/4`, and `tasks` `17/17` after one restart. No storage cluster, node, or
volume existed.

## What is not covered

Listed with where each one comes from. Items 1 to 3 are the work that makes this
procedure a command.

1. **The upgrade tool does not do steps 3 and 5.** `simplyblock-upgrade upgrade`
   plans `deploy-conversion-webhook`, `await-conversion-webhook`,
   `smoke-test-conversion`, `upgrade-operator`, `await-operator`, and
   `smoke-test-operator`, all marked as described and not implemented. The reasons
   in `internal/upgrade/steps/upgrade.go` say the conversion webhook and the
   `v1alpha2` types do not exist. They do, so the reasons are stale and the steps
   are unblocked.
2. **The values translation is not written.** Keys the 26.3.x chart had and the
   current chart rejects: `driverName`, `image.csi*`, `controller.*`, `node.*`,
   `storagenode.*` except `storagenode.multiCluster`, `logicalVolume.*`,
   `controlplane.foundationdb.*`, `controlplane.observability.minio.*`,
   `csiHostpathDriver`, `operator.enabled`, and others. Compare the two
   `values.yaml` files.
3. **The release handover covers eight objects.** The chart's guard and the
   tool's annotation step do not cover Services, ConfigMaps, or the two
   Certificates `simplyblock-foundationdb` and `simplyblock-webappapi`. Helm
   deletes what it owned and the new manifest drops. A Certificate that is deleted
   and reissued may rotate the FoundationDB peer certificates; this was not
   tested.
4. **TLS flips silently in conversion.** A v1alpha1 `ControlPlane` has no TLS
   field, and the v1alpha2 CRD defaults `tls` to on, with mutual TLS and
   cert-manager. A converted plaintext install reads as TLS, and one installed
   with `mutual_enabled: false` or the OpenShift provider reads with the wrong
   settings. The test cluster was installed with TLS and mutual TLS, so it
   matched by coincidence. PR #666 makes `spec.source.local.tls` immutable, so a
   wrong value written by conversion cannot be corrected afterward. Conversion has
   to carry the real settings, or the upgrade has to record them before the CRDs
   are applied.
5. **The initial discovery run fires on an upgrade.** The chart renders
   `OperatorOps/initial-discovery` on every `helm upgrade` of a standalone
   install, so an already-deployed cluster gets a discovery run and a draft
   `ClusterDeploymentConfig` it did not ask for. The template needs to render on
   install only.
6. **`storedVersions` is not migrated.** No step rewrites stored objects to
   `v1alpha2` or removes `v1alpha1` from `status.storedVersions`, which is what
   dropping `v1alpha1` requires.
7. **The `migrate` stage was not run.** The cluster had nothing for it to do.
   Backups, node sets, volume handles, and node reparenting are untested here.
8. **Rollback.** `helm rollback` restores the old operator Deployment and its TLS
   environment, but not the CRDs, and the `ControlPlane` TLS field stays
   immutable.
9. **The chart's version label goes down.** The development chart is version
   `26.3.0`, below the released 26.3.1 and 26.3.2, so Helm's history shows a
   downgrade.
10. **No CI covers any of this.** Nothing installs an old release and upgrades it.
    An upgrade job would create a kind cluster with cert-manager, install the
    26.3.x chart with TLS on, run this procedure, and assert: the same
    `ControlPlane` UID, no FoundationDB recreation, `SimplyblockDriver` `Ready`,
    the new operator image, TLS unchanged, and no crash-looping pod. Each older
    release is a matrix entry.
