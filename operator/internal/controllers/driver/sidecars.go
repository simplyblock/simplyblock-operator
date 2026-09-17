// The seven CSI sidecar images, and which of them a deployment gets.
//
// The versions below are this operator release's, meaning the combination it was
// tested against, and spec.sidecarImages overrides one at a time. The field
// exists so that a pin a Helm release made survives the adoption of that
// release's deployment; a sidecar the release left at its chart default takes the
// version here instead, so a deployment that never made a choice is not frozen at
// a tag nobody maintains.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §3.1 and §4.3.

package driver

import (
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The pinned set. Moving one of these is a driver rollout for every deployment
// that has not overridden it, so it is a deliberate edit rather than a bump.
const (
	defaultProvisionerImage         = "quay.io/simplyblock-io/csi-provisioner:v5.1.0"
	defaultAttacherImage            = "quay.io/simplyblock-io/csi-attacher:v4.7.0"
	defaultResizerImage             = "quay.io/simplyblock-io/csi-resizer:v1.12.0"
	defaultSnapshotterImage         = "quay.io/simplyblock-io/csi-snapshotter:v8.2.0"
	defaultHealthMonitorImage       = "quay.io/simplyblock-io/csi-external-health-monitor-controller:v0.14.0"
	defaultNodeDriverRegistrarImage = "quay.io/simplyblock-io/csi-node-driver-registrar:v2.12.0"
	// defaultCSIAddonsImage is the kubernetes-csi-addons sidecar (upstream
	// quay.io/csiaddons/k8s-sidecar), pinned at the same v0.15.0 the chart's
	// controller-manager runs (design P0-5). Named for the eventual
	// quay.io/simplyblock-io mirror this field's validation pattern requires,
	// which does not exist yet; mirroring it is a release task.
	defaultCSIAddonsImage = "quay.io/simplyblock-io/csi-addons-sidecar:v0.15.0"
)

// resolvedSidecars is the image each sidecar runs, after the overrides.
type resolvedSidecars struct {
	provisioner         string
	attacher            string
	resizer             string
	snapshotter         string
	healthMonitor       string
	nodeDriverRegistrar string
	csiAddons           string
}

func sidecars(d *simplyblockv1alpha2.SimplyblockDriver) resolvedSidecars {
	o := d.Spec.SidecarImages
	return resolvedSidecars{
		provisioner:         orDefault(o.Provisioner, defaultProvisionerImage),
		attacher:            orDefault(o.Attacher, defaultAttacherImage),
		resizer:             orDefault(o.Resizer, defaultResizerImage),
		snapshotter:         orDefault(o.Snapshotter, defaultSnapshotterImage),
		healthMonitor:       orDefault(o.HealthMonitor, defaultHealthMonitorImage),
		nodeDriverRegistrar: orDefault(o.NodeDriverRegistrar, defaultNodeDriverRegistrarImage),
		csiAddons:           orDefault(o.CSIAddons, defaultCSIAddonsImage),
	}
}

func orDefault(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}
