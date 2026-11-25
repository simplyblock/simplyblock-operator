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
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"

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
	// defaultCSIAddonsImage is the kubernetes-csi-addons sidecar, pinned at the
	// same v0.15.0 the chart's controller-manager runs (design P0-5). The real
	// upstream image (quay.io/csiaddons/k8s-sidecar) rather than a
	// quay.io/simplyblock-io mirror: that mirror does not exist yet, and
	// mirroring it is a release task, not something to assume has already
	// happened.
	defaultCSIAddonsImage = "quay.io/csiaddons/k8s-sidecar:v0.15.0"
)

// The csi-snapshotter releases for the newer versions of
// groupsnapshot.storage.k8s.io. A release watches VolumeGroupSnapshotClass at
// one version only (8.2, the default above, watches v1beta1), so the sidecar is
// picked by the version the cluster serves (withGroupSnapshotAPI). Mirrored
// under quay.io/simplyblock-io like the rest of the pinned set.
const (
	snapshotterImageV1beta2 = "quay.io/simplyblock-io/csi-snapshotter:v8.4.0"
	snapshotterImageV1      = "quay.io/simplyblock-io/csi-snapshotter:v8.7.0"
)

// csiVolumeGroupSnapshotGate is the sidecar flag that turns group snapshots on
// or off, without its value.
const csiVolumeGroupSnapshotGate = "--feature-gates=CSIVolumeGroupSnapshot="

// withGroupSnapshotAPI sets the csi-snapshotter sidecar for the version of
// groupsnapshot.storage.k8s.io the cluster serves, the empty string for none.
//
// The sidecar is the release that watches that version, and its group snapshot
// gate is on. Where the cluster serves no version the gate is off, so the
// sidecar serves VolumeSnapshots without a watch that fails on every retry; it
// has to be said, because 8.7 turns the gate on by default. An image the
// deployment set is kept: which version it watches is the deployment's to know.
func withGroupSnapshotAPI(
	sts *appsv1.StatefulSet, d *simplyblockv1alpha2.SimplyblockDriver, version string,
) *appsv1.StatefulSet {
	containers := sts.Spec.Template.Spec.Containers
	for i := range containers {
		c := &containers[i]
		if c.Name != "csi-snapshotter" {
			continue
		}
		if d.Spec.SidecarImages.Snapshotter == "" {
			switch version {
			case "v1":
				c.Image = snapshotterImageV1
			case "v1beta2":
				c.Image = snapshotterImageV1beta2
			default:
				c.Image = defaultSnapshotterImage
			}
		}
		gate := csiVolumeGroupSnapshotGate + strconv.FormatBool(version != "")
		args := slices.DeleteFunc(c.Args, func(arg string) bool {
			return strings.HasPrefix(arg, csiVolumeGroupSnapshotGate)
		})
		c.Args = append(args, gate)
	}
	return sts
}

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
		provisioner:         orDefault(string(o.Provisioner), defaultProvisionerImage),
		attacher:            orDefault(string(o.Attacher), defaultAttacherImage),
		resizer:             orDefault(string(o.Resizer), defaultResizerImage),
		snapshotter:         orDefault(string(o.Snapshotter), defaultSnapshotterImage),
		healthMonitor:       orDefault(string(o.HealthMonitor), defaultHealthMonitorImage),
		nodeDriverRegistrar: orDefault(string(o.NodeDriverRegistrar), defaultNodeDriverRegistrarImage),
		csiAddons:           orDefault(string(o.CSIAddons), defaultCSIAddonsImage),
	}
}

func orDefault(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}
