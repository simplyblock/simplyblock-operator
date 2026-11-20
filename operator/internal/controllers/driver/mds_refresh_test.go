// The driver reconciler keeps every metadata server StatefulSet on the pod
// template this release builds, whether or not any export reconciles.

package driver

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func reconcileWithMDS(t *testing.T, sts *appsv1.StatefulSet) *appsv1.StatefulSet {
	t.Helper()
	scheme := reconcilerScheme(t)
	d := withMDS(testDriver("simplyblock"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, sts).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: true}}
	if _, err := r.Reconcile(context.Background(), requestFor(d)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &got); err != nil {
		t.Fatalf("re-read the StatefulSet: %v", err)
	}
	return &got
}

// Regression: 2026-10-10-mds-template-waits-for-an-export (pnfs-1791627120). With
// no export reconciling, an operator upgrade left the metadata server on the
// old image, and the next run's first export restarted it mid-provisioning.
// A driver reconcile updates it at once.
func TestADriverReconcileUpdatesAnOutdatedMDSWithoutAnyExport(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, want := mdsObjects(t, d)
	old := want.DeepCopy()
	old.Annotations[MDSTemplateHashAnnotation] = "0ld0ld0ld0ld0ld0"
	old.Spec.Template.Spec.Containers[0].Image = "public.ecr.aws/simply-block/spdkcsi:pnfs-mds-old"

	got := reconcileWithMDS(t, old)

	if got.Annotations[MDSTemplateHashAnnotation] != want.Annotations[MDSTemplateHashAnnotation] {
		t.Errorf("hash = %q, want this release's %q",
			got.Annotations[MDSTemplateHashAnnotation], want.Annotations[MDSTemplateHashAnnotation])
	}
	if img := got.Spec.Template.Spec.Containers[0].Image; img != want.Spec.Template.Spec.Containers[0].Image {
		t.Errorf("image = %q, want %q", img, want.Spec.Template.Spec.Containers[0].Image)
	}
}

// A StatefulSet already on this release's template is not written, so a
// reconcile never restarts a guest for nothing.
func TestADriverReconcileLeavesACurrentMDSAlone(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, current := mdsObjects(t, d)
	current.Spec.Template.Spec.Containers[0].Args = append(
		current.Spec.Template.Spec.Containers[0].Args, "--debug-ssh") // a hand edit the hash does not see

	got := reconcileWithMDS(t, current)

	args := got.Spec.Template.Spec.Containers[0].Args
	if len(args) == 0 || args[len(args)-1] != "--debug-ssh" {
		t.Errorf("args = %v: the reconcile rewrote a StatefulSet whose hash was current", args)
	}
}
