// U-02, U-35, U-36, U-37, and the sidecar rows U-85 to U-87.
//
// The registration and the snapshot class both carry spec.driverName, and the
// live CSIDriver of a 26.2.7 release is what the built one is checked against,
// because adoption reconciles toward the state that is running.

package driver

import (
	"testing"

	storagev1 "k8s.io/api/storage/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// altDriverName is a driver name other than the CRD's default. It is what
// separates a registration that carries spec.driverName from one that happens to
// carry the default.
const altDriverName = "csi.example.com"

// U-02: the registration is named by spec.driverName, not by the object's name.
func TestRegistrationIsNamedByDriverName(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.DriverName = altDriverName

	if got := csiDriver(d).Name; got != altDriverName {
		t.Errorf("registration name = %q, want spec.driverName", got)
	}
}

// An object written before admission defaulted the field still registers under
// the driver's real name rather than under the empty string.
func TestRegistrationDefaultsTheDriverName(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.DriverName = ""

	if got := csiDriver(d).Name; got != DefaultDriverName {
		t.Errorf("registration name = %q, want %q", got, DefaultDriverName)
	}
}

// The registration matches what a 26.2.7 chart install leaves running, field for
// field, including the ones the API server would have defaulted. A difference
// here is a diff on the reconcile that adopts.
func TestRegistrationMatchesTheLiveObject(t *testing.T) {
	spec := csiDriver(testDriver("simplyblock")).Spec

	checks := []struct {
		field string
		got   *bool
		want  bool
	}{
		{"attachRequired", spec.AttachRequired, true},
		{"podInfoOnMount", spec.PodInfoOnMount, false},
		{"storageCapacity", spec.StorageCapacity, false},
		{"requiresRepublish", spec.RequiresRepublish, false},
		{"seLinuxMount", spec.SELinuxMount, false},
	}
	for _, c := range checks {
		if c.got == nil {
			t.Errorf("%s is unset, want %v", c.field, c.want)
			continue
		}
		if *c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, *c.got, c.want)
		}
	}

	if spec.FSGroupPolicy == nil || *spec.FSGroupPolicy != storagev1.ReadWriteOnceWithFSTypeFSGroupPolicy {
		t.Errorf("fsGroupPolicy = %v, want ReadWriteOnceWithFSType", spec.FSGroupPolicy)
	}
	if len(spec.VolumeLifecycleModes) != 1 || spec.VolumeLifecycleModes[0] != storagev1.VolumeLifecyclePersistent {
		t.Errorf("volumeLifecycleModes = %v, want [Persistent]", spec.VolumeLifecycleModes)
	}
}

// U-35 and U-36: the snapshot class names the same driver, and a non-default
// driverName leaves the default nowhere.
func TestSnapshotClassNamesTheDriver(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.DriverName = altDriverName

	sc := volumeSnapshotClass(d)
	if got := sc.Object["driver"]; got != altDriverName {
		t.Errorf("snapshot class driver = %v, want spec.driverName", got)
	}
	if got := sc.GetName(); got != "simplyblock-csi-snapshotclass" {
		t.Errorf("snapshot class name = %q, want the derived name", got)
	}
	if got := sc.Object["deletionPolicy"]; got != "Delete" {
		t.Errorf("deletionPolicy = %v, want Delete", got)
	}
	if got := sc.GroupVersionKind().GroupVersion().String(); got != "snapshot.storage.k8s.io/v1" {
		t.Errorf("apiVersion = %q", got)
	}
}

// U-37 in its negative form: the toggle is what decides whether snapshot support
// is part of the deployment, and an unset field means enabled.
func TestSnapshotsEnabledDefaultsToTrue(t *testing.T) {
	tests := []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset", nil, true},
		{"explicitly true", ptr.To(true), true},
		{"explicitly false", ptr.To(false), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := testDriver("simplyblock")
			d.Spec.EnableVolumeSnapshots = tc.set
			if got := snapshotsEnabled(d); got != tc.want {
				t.Errorf("snapshotsEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// U-85: an unset override takes this operator release's version.
func TestSidecarsDefaultToTheOperatorsRelease(t *testing.T) {
	got := sidecars(testDriver("simplyblock"))

	want := resolvedSidecars{
		provisioner:         defaultProvisionerImage,
		attacher:            defaultAttacherImage,
		resizer:             defaultResizerImage,
		snapshotter:         defaultSnapshotterImage,
		healthMonitor:       defaultHealthMonitorImage,
		nodeDriverRegistrar: defaultNodeDriverRegistrarImage,
	}
	if got != want {
		t.Errorf("sidecars = %+v, want %+v", got, want)
	}
}

// U-86: one override reaches its own sidecar and no other, which is the property
// that makes a pin survivable without freezing the rest of the deployment.
func TestOneSidecarOverrideReachesOnlyItsOwn(t *testing.T) {
	const pinned = "quay.io/simplyblock-io/csi-attacher:v4.6.1"

	d := testDriver("simplyblock")
	d.Spec.SidecarImages = simplyblockv1alpha2.SidecarImages{Attacher: pinned}

	got := sidecars(d)
	if got.attacher != pinned {
		t.Errorf("attacher = %q, want the pin %q", got.attacher, pinned)
	}
	if got.provisioner != defaultProvisionerImage ||
		got.resizer != defaultResizerImage ||
		got.snapshotter != defaultSnapshotterImage ||
		got.healthMonitor != defaultHealthMonitorImage ||
		got.nodeDriverRegistrar != defaultNodeDriverRegistrarImage {
		t.Errorf("a pin on one sidecar moved another: %+v", got)
	}
}

// U-87 in part: the snapshot controller's image is the cluster's rather than the
// deployment's, so there is no field for it among the six.
func TestSidecarsAreSixAndExcludeTheSnapshotController(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.SidecarImages = simplyblockv1alpha2.SidecarImages{
		Provisioner:         "quay.io/simplyblock-io/a:v1",
		Attacher:            "quay.io/simplyblock-io/b:v1",
		Resizer:             "quay.io/simplyblock-io/c:v1",
		Snapshotter:         "quay.io/simplyblock-io/d:v1",
		HealthMonitor:       "quay.io/simplyblock-io/e:v1",
		NodeDriverRegistrar: "quay.io/simplyblock-io/f:v1",
	}

	got := sidecars(d)
	distinct := map[string]bool{
		got.provisioner: true, got.attacher: true, got.resizer: true,
		got.snapshotter: true, got.healthMonitor: true, got.nodeDriverRegistrar: true,
	}
	if len(distinct) != 6 {
		t.Errorf("six overrides produced %d distinct images: %+v", len(distinct), got)
	}
}
