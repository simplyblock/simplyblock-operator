# Vendored Ramen manifests

Everything under this directory is vendored **verbatim** from upstream
[RamenDR/ramen](https://github.com/RamenDR/ramen), pinned to a single tag, and
is the source the chart's `templates/ramen-*.yaml` render from. Do not
hand-edit these files — refresh them from upstream instead (below).

**Pinned version:** `v0.1.0-rc1`

```
ramen/
  crds/
    hub/           config/crd/bases  — the 3 hub kinds (config/hub/crd)
    dr-cluster/    config/crd/bases  — the 6 execution kinds (config/dr-cluster/crd)
```

The only modification applied to the vendored CRDs is a
`helm.sh/resource-policy: keep` annotation, injected so a chart uninstall
never removes a CRD (and with it every DR object in the cluster). The operator
Deployments, ConfigMaps (`RamenConfig`) and RBAC are reproduced in
`templates/_ramen_helpers.tpl`, `templates/ramen-hub.yaml` and
`templates/ramen-dr-cluster.yaml` from upstream `config/manager`,
`config/hub` and `config/dr-cluster`, matching the names and labels
kustomize produces (`namePrefix ramen-hub-` / `ramen-dr-cluster-`, namespace
`ramen-system`).

## Refreshing from upstream

When bumping the pinned version, re-vendor the CRDs and re-check the operator
manifests against upstream:

```bash
REF=v0.1.0-rc1   # set to the new tag
tmp=$(mktemp -d)
curl -fsSL "https://codeload.github.com/RamenDR/ramen/tar.gz/refs/tags/$REF" \
  | tar -xz -C "$tmp"
src="$tmp/ramen-${REF#v}/config"

# CRDs, split hub vs dr-cluster exactly as config/hub/crd and
# config/dr-cluster/crd reference them:
for f in drpolicies drplacementcontrols drclusters; do
  cp "$src/crd/bases/ramendr.openshift.io_$f.yaml" ramen/crds/hub/
done
for f in volumereplicationgroups protectedvolumereplicationgrouplists \
         maintenancemodes drclusterconfigs replicationgroupsources \
         replicationgroupdestinations; do
  cp "$src/crd/bases/ramendr.openshift.io_$f.yaml" ramen/crds/dr-cluster/
done

# Re-add the keep annotation to each copied CRD's metadata.annotations, then
# diff config/hub/rbac/role.yaml and config/dr-cluster/rbac/role.yaml against
# the ClusterRole rules in templates/ramen-hub.yaml / ramen-dr-cluster.yaml,
# and config/*/manager/ramen_manager_config.yaml against the defaults baked
# into templates/_ramen_helpers.tpl (sbramen.managerConfig). Bump image.tag
# in values.yaml.
```

Keep the RBAC and `RamenConfig` in the templates in step with upstream: they
are the two places this chart re-types rather than copies, so they are the two
that can drift.
