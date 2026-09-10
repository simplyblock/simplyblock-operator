// The cluster's record that this driver exists, and the snapshot class that
// names it.
//
// The CSIDriver is what a kubelet consults before it asks anything of the node
// plugin, and its name is spec.driverName rather than a derived string, because
// that is the name every PersistentVolume records in spec.csi.driver. Both
// objects are cluster-scoped, so neither carries an owner reference and both are
// the finalizer's to remove.
//
// The snapshot class is built unstructured rather than through the
// external-snapshotter client. The object is three fields, no other package here
// needs the types, and the operator has to work against whichever version of the
// CRD the cluster happens to serve, which is not necessarily the one a pinned
// client module was generated from.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1.

package driver

import (
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// snapshotGroupVersion is the API a cluster has to serve for snapshot support to
// exist, and what §4.1's detection asks the discovery client about.
var snapshotGroupVersion = schema.GroupVersion{Group: "snapshot.storage.k8s.io", Version: "v1"}

var volumeSnapshotClassGVK = snapshotGroupVersion.WithKind("VolumeSnapshotClass")

// csiDriver is the registration. Every field the API server would default is set
// here explicitly, so that the object this builds is byte-identical to the one a
// chart install left running and adoption produces no diff.
func csiDriver(d *simplyblockv1alpha2.SimplyblockDriver) *storagev1.CSIDriver {
	return &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: names(d).csiDriver},
		Spec: storagev1.CSIDriverSpec{
			AttachRequired:       ptr.To(true),
			PodInfoOnMount:       ptr.To(false),
			StorageCapacity:      ptr.To(false),
			RequiresRepublish:    ptr.To(false),
			SELinuxMount:         ptr.To(false),
			FSGroupPolicy:        ptr.To(storagev1.ReadWriteOnceWithFSTypeFSGroupPolicy),
			VolumeLifecycleModes: []storagev1.VolumeLifecycleMode{storagev1.VolumeLifecyclePersistent},
		},
	}
}

func volumeSnapshotClass(d *simplyblockv1alpha2.SimplyblockDriver) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(volumeSnapshotClassGVK)
	obj.SetName(names(d).snapshotClass)
	obj.Object["driver"] = names(d).csiDriver
	obj.Object["deletionPolicy"] = "Delete"
	return obj
}

// TODO(simplyblockdriver): supply the snapshot CRDs and a controller where the
// cluster serves neither, which is design-simplyblockdriver.md §4.1. Today, the
// chart still installs both, into kube-system and annotated
// helm.sh/resource-policy: keep, so an adopted deployment finds the API served
// and records Detected. What is missing here is the detection against the
// discovery client, the apply of the CRDs and the controller where it comes
// back empty, and status.snapshotSupport reading Installed in that case. They
// are cluster-scoped and shared, so they carry no owner reference and outlive
// this object, which is what design §9 Q2 leaves open.
//
// The test plan's U-04, U-05, and U-38 to U-41 are the rows this owes.

// snapshotsEnabled reports whether this deployment includes snapshot support.
// The field defaults to true, so an object written before the default applied
// reads as enabled rather than as disabled by omission.
func snapshotsEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.EnableVolumeSnapshots == nil || *d.Spec.EnableVolumeSnapshots
}
