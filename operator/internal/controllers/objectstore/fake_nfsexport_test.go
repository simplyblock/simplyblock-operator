// fakeNFSExportCreator stands in for the CSI controller's own NFSExport
// creation and for NFSExportReconciler driving it to Ready
// (design-pnfs-rwx.md §9.3): envtest runs neither, so nothing ever produces an
// export for a bound pNFS volume on its own. This polls for a bound
// PersistentVolume carrying a CSI volume handle and creates a matching
// NFSExport already Ready, with spec.volumeRef set to that same handle, so
// S3BucketReconciler's "find the export bound to this bucket's volume" step
// (design-bucket-provisioning.md §6) has something real to find.
//
// A test that wants W-03's negative scenario for this step (no NFSExport ever
// appears) simply does not start this fake: there is no flag to tell it to
// skip one bucket, because the reconciler cannot tell "the CSI controller
// hasn't created one yet" apart from "it never will" either, and a fake that
// could would be testing something the real system cannot do.

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
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// startFakeNFSExportCreator creates an NFSExport for every bound
// PersistentVolume carrying a CSI volume handle, until ctx is done. One
// export per PV, named deterministically from the PV so a second tick over
// the same PV is a no-op rather than a second create.
func startFakeNFSExportCreator(ctx context.Context, cli client.Client) {
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				createExportsForBoundVolumes(ctx, cli)
			}
		}
	}()
}

func createExportsForBoundVolumes(ctx context.Context, cli client.Client) {
	var pvs corev1.PersistentVolumeList
	if err := cli.List(ctx, &pvs); err != nil {
		return
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Status.Phase != corev1.VolumeBound || pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
			continue
		}
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace == "" {
			continue
		}
		createReadyExport(ctx, cli, pv)
	}
}

// createReadyExport creates the NFSExport a real CSI controller would have
// created for pv's claim, then drives it straight to Ready: this fake plays
// both the CSI controller's and NFSExportReconciler's parts, since the
// reconciler under test only reads an export's spec.volumeRef and
// status.phase, never how it got there.
func createReadyExport(ctx context.Context, cli client.Client, pv *corev1.PersistentVolume) {
	export := &simplyblockv1alpha2.NFSExport{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-nfsexp-" + pv.Name, Namespace: pv.Spec.ClaimRef.Namespace},
		Spec: simplyblockv1alpha2.NFSExportSpec{
			VolumeRef:  pv.Spec.CSI.VolumeHandle,
			ExportPath: "/var/lib/simplyblock/exports/" + pv.Name,
		},
	}
	if err := cli.Create(ctx, export); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return
		}
		if err := cli.Get(ctx, client.ObjectKeyFromObject(export), export); err != nil {
			return
		}
	}
	if export.Status.Phase == simplyblockv1alpha2.NFSExportPhaseReady {
		return
	}
	export.Status.Phase = simplyblockv1alpha2.NFSExportPhaseReady
	export.Status.AllowedClients = []string{"192.168.10.0/24"}
	_ = cli.Status().Update(ctx, export)
}

// W-08 acceptance: a matching NFSExport appears in Ready with a spec.volumeRef
// that resolves against the bound PersistentVolume.
func TestFakeNFSExportCreatorProducesAReadyMatchingExport(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)
	startFakeNFSExportCreator(ctx, cli)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-wants-an-export", Namespace: "default"},
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

	var boundHandle string
	waitFor(t, "the claim to bind", func() bool {
		var got corev1.PersistentVolumeClaim
		if err := cli.Get(ctx, client.ObjectKeyFromObject(pvc), &got); err != nil || got.Spec.VolumeName == "" {
			return false
		}
		var pv corev1.PersistentVolume
		if err := cli.Get(ctx, client.ObjectKey{Name: got.Spec.VolumeName}, &pv); err != nil || pv.Spec.CSI == nil {
			return false
		}
		boundHandle = pv.Spec.CSI.VolumeHandle
		return boundHandle != ""
	})

	var export simplyblockv1alpha2.NFSExport
	waitFor(t, "a matching NFSExport to reach Ready", func() bool {
		var list simplyblockv1alpha2.NFSExportList
		if err := cli.List(ctx, &list); err != nil {
			return false
		}
		for _, e := range list.Items {
			if e.Spec.VolumeRef == boundHandle && e.Status.Phase == simplyblockv1alpha2.NFSExportPhaseReady {
				export = e
				return true
			}
		}
		return false
	})

	if export.Spec.VolumeRef != boundHandle {
		t.Errorf("export.Spec.VolumeRef = %q, want it to resolve against the bound volume's handle %q",
			export.Spec.VolumeRef, boundHandle)
	}
}
