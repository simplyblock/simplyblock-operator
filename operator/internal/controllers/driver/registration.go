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
	"context"
	"fmt"

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
// cluster serves neither, which is the second half of design-simplyblockdriver.md
// §4.1 and is why SnapshotSupportOriginInstalled is not yet reachable.
//
// The chart installs both today, and conditionally: its CRD templates are
// guarded on .Capabilities.APIVersions.Has, so a cluster already serving the
// kinds gets nothing, which is the same rule §4.1 states for the operator. What
// a chart cannot cover is an installation that is not a chart — an OLM bundle,
// or a release with snapshotcontroller.create false — and that is the case this
// owes. It needs the upstream manifests carried in this binary and an image for
// the controller, which no field on the spec names, so it is a change of its own
// rather than a line here. §9 Q2 is what removes them afterward, and it is open.
//
// The test plan's U-38 to U-41 are the rows this owes. U-04 and U-05 are the
// detection below.

// SnapshotAPI answers whether the cluster serves the snapshot kinds, which is
// §4.1's detection: a snapshot-controller exists to reconcile those kinds and
// its Deployment is named differently by every distribution, so the API being
// served is the question rather than any object being present.
//
// It is an interface because the answer comes from discovery rather than from
// the object graph, and a reconciler that reached for a discovery client
// directly could not be tested against a cluster that has no snapshot API —
// which is the case this exists to handle.
type SnapshotAPI interface {
	// SnapshotAPIServed reports whether snapshot.storage.k8s.io/v1 is served.
	SnapshotAPIServed(ctx context.Context) (bool, error)
}

// snapshotOrigin is what status.snapshotSupport should read, and the empty
// string for a deployment that asked for no snapshots.
//
// Only Detected is reachable today. Installed is what the apply above would
// record, and recording it before that apply exists would say this deployment
// brought snapshot support to a cluster where nothing did.
func (r *SimplyblockDriverReconciler) snapshotOrigin(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) (simplyblockv1alpha2.SnapshotSupportOrigin, error) {
	if !snapshotsEnabled(d) {
		return "", nil
	}
	served, err := r.snapshotAPIServed(ctx)
	if err != nil {
		return "", err
	}
	if !served {
		return "", nil
	}
	return simplyblockv1alpha2.SnapshotSupportOriginDetected, nil
}

// snapshotAPIServed asks the cluster, and refuses to guess.
//
// A reconcile with no way to ask is a reconcile that must fail rather than
// assume. Assuming served applies a class the API server may have no kind for,
// and assuming absent drops a class an adopted cluster already has, which
// withdraws snapshot support from a working deployment on a transient error.
func (r *SimplyblockDriverReconciler) snapshotAPIServed(ctx context.Context) (bool, error) {
	if r.Snapshots == nil {
		return false, fmt.Errorf(
			"no snapshot-API detector is configured, so whether this cluster serves " +
				"snapshot.storage.k8s.io/v1 cannot be established")
	}
	served, err := r.Snapshots.SnapshotAPIServed(ctx)
	if err != nil {
		return false, fmt.Errorf("ask whether the cluster serves the snapshot API: %w", err)
	}
	return served, nil
}

// snapshotsEnabled reports whether this deployment includes snapshot support.
// The field defaults to true, so an object written before the default applied
// reads as enabled rather than as disabled by omission.
func snapshotsEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.EnableVolumeSnapshots == nil || *d.Spec.EnableVolumeSnapshots
}
