// fakeCSIBinder stands in for the CSI controller's own PVC provisioning and
// binding (design-bucket-provisioning.md §11, Testing Strategy): envtest runs
// no kubelet, no scheduler, and no real CSI driver, so nothing ever binds a
// PersistentVolumeClaim on its own. This polls for one against a single fake
// StorageClass and binds it by hand, to a PersistentVolume carrying a CSI
// volume handle in the same {clusterID}:{poolID}:{volumeUUID} form
// NFSExport.Spec.VolumeRef validates against, so S3BucketReconciler's own
// "wait for Bound, then find the matching NFSExport" step has something real
// to drive against.
//
// A PVC against any other StorageClass name is left alone: that is what W-03's
// negative scenario (the named StorageClass does not exist) needs, and the
// absence of a second fake binder instance is what produces it, not a flag on
// this one.

package objectstore

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
)

// fakeStorageClassName is the StorageClass a test's S3Bucket points its
// storageClassName at to be picked up by startFakeCSIBinder. It names no real
// StorageClass object: the binder matches on the name alone.
const fakeStorageClassName = "fake-pnfs"

// fakeClusterID and fakePoolID fill the first two segments of every fake CSI
// volume handle this package produces. Fixed rather than derived, because
// nothing in these tests reads them back: only the handle's role as the join
// between a PersistentVolume and its NFSExport matters.
const (
	fakeClusterID = "f0bb9077-78c4-4482-9ccf-a5693ce2df78"
	fakePoolID    = "9d016dd4-34d7-42f0-b549-52a5af2f1399"
)

// fakeVolumeHandle derives a volume handle for the PVC deterministically from
// its own UID, which the apiserver already assigns in UUID form: exactly the
// third segment NFSExport.Spec.VolumeRef's validation pattern requires, with
// no second identifier to invent and keep in step with it.
func fakeVolumeHandle(pvc *corev1.PersistentVolumeClaim) string {
	return fakeClusterID + ":" + fakePoolID + ":" + string(pvc.UID)
}

// startFakeCSIBinder binds every PersistentVolumeClaim against
// fakeStorageClassName as it appears, until ctx is done. It polls rather than
// watches: the reconciler under test is what has to react to a bound PVC, not
// this fake's own responsiveness, and a short poll interval makes that
// difference immaterial to a test's wall-clock time.
func startFakeCSIBinder(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bindPendingClaims(ctx, cli)
			}
		}
	}()
}

func bindPendingClaims(ctx context.Context, cli client.Client) {
	var pvcs corev1.PersistentVolumeClaimList
	if err := cli.List(ctx, &pvcs); err != nil {
		return
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != fakeStorageClassName {
			continue
		}
		if pvc.Status.Phase == corev1.ClaimBound {
			continue
		}
		bindClaim(ctx, cli, pvc)
	}
}

// bindClaim creates the PersistentVolume a real CSI controller and its
// binder would have produced between them, then marks the claim Bound. Errors
// are swallowed: a PV that already exists from the previous tick, or a claim
// mid-delete, both resolve themselves on the next poll.
func bindClaim(ctx context.Context, cli client.Client, pvc *corev1.PersistentVolumeClaim) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-pv-" + string(pvc.UID)},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      pvc.Spec.Resources.Requests,
			AccessModes:                   pvc.Spec.AccessModes,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              fakeStorageClassName,
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "PersistentVolumeClaim",
				Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "fake.csi.simplyblock.io",
					VolumeHandle: fakeVolumeHandle(pvc),
					FSType:       "xfs",
				},
			},
		},
	}
	if err := cli.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
		return
	}
	pv.Status.Phase = corev1.VolumeBound
	_ = cli.Status().Update(ctx, pv)

	pvc.Spec.VolumeName = pv.Name
	if err := cli.Update(ctx, pvc); err != nil {
		return
	}
	pvc.Status.Phase = corev1.ClaimBound
	_ = cli.Status().Update(ctx, pvc)
}

// W-08 acceptance: a test PVC against the fake StorageClass reaches Bound
// without a real CSI driver.
func TestFakeCSIBinderBindsAClaimAgainstTheFakeStorageClass(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-reaches-bound", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To(fakeStorageClassName),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: mustQuantity("1Gi")},
			},
		},
	}
	if err := cli.Create(ctx, pvc); err != nil {
		t.Fatalf("creating the claim: %v", err)
	}

	waitFor(t, "the claim to bind", func() bool {
		var got corev1.PersistentVolumeClaim
		if err := cli.Get(ctx, client.ObjectKeyFromObject(pvc), &got); err != nil {
			return false
		}
		return got.Status.Phase == corev1.ClaimBound && got.Spec.VolumeName != ""
	})

	var bound corev1.PersistentVolumeClaim
	if err := cli.Get(ctx, client.ObjectKeyFromObject(pvc), &bound); err != nil {
		t.Fatalf("reading the bound claim: %v", err)
	}
	var pv corev1.PersistentVolume
	if err := cli.Get(ctx, client.ObjectKey{Name: bound.Spec.VolumeName}, &pv); err != nil {
		t.Fatalf("the claim named a volume that does not exist: %v", err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		t.Fatalf("bound volume carries no CSI volume handle: %+v", pv.Spec)
	}
}

// A claim against any other StorageClass is left alone: W-03's negative
// scenario for a StorageClass that does not exist needs exactly this, and gets
// it from the fake binder's absence of a match rather than a flag.
func TestFakeCSIBinderIgnoresAnotherStorageClass(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-wrong-class", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To("does-not-exist"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: mustQuantity("1Gi")},
			},
		},
	}
	if err := cli.Create(ctx, pvc); err != nil {
		t.Fatalf("creating the claim: %v", err)
	}

	neverWithin(t, 300*time.Millisecond, "the claim to bind", func() bool {
		var got corev1.PersistentVolumeClaim
		if err := cli.Get(ctx, client.ObjectKeyFromObject(pvc), &got); err != nil {
			return false
		}
		return got.Status.Phase == corev1.ClaimBound
	})
}
