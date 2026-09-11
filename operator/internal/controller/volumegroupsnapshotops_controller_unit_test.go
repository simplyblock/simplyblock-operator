// Unit tests for the VolumeGroupSnapshotOps Restore action (design §7.4,
// test plan U-25 … U-32): claim derivation, group-label stamping, the
// incomplete-generation gate, collision handling, readiness waiting, and the
// terminal contract, against a fake client with no backend involved.
package controller

import (
	"context"
	"strings"
	"testing"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// groupRestoreFixture returns a ready three-member VolumeGroupSnapshot "vgs1"
// in "default": the group snapshot, its content (three volume handles, the
// expected count), three member VolumeSnapshots backref'd to it, and their
// three source PVCs, each requesting the "sc1" storage class as its own.
func groupRestoreFixture() []client.Object {
	quantity := resource.MustParse("2Gi")
	objects := make([]client.Object, 0, 8)
	objects = append(objects,
		&volumegroupsnapshotv1beta1.VolumeGroupSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "vgs1", Namespace: "default"},
			Status: &volumegroupsnapshotv1beta1.VolumeGroupSnapshotStatus{
				ReadyToUse:                          boolPtr(true),
				BoundVolumeGroupSnapshotContentName: strPtr("gsc1"),
			},
		},
		&volumegroupsnapshotv1beta1.VolumeGroupSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "gsc1"},
			Spec: volumegroupsnapshotv1beta1.VolumeGroupSnapshotContentSpec{
				Source: volumegroupsnapshotv1beta1.VolumeGroupSnapshotContentSource{
					VolumeHandles: []string{"h1", "h2", "h3"},
				},
			},
		},
	)
	for _, name := range []string{"pvc-a", "pvc-b", "pvc-c"} {
		objects = append(objects,
			&snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap-" + name, Namespace: "default"},
				Spec: snapshotv1.VolumeSnapshotSpec{
					Source: snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: strPtr(name)},
				},
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse:              boolPtr(true),
					RestoreSize:             &quantity,
					VolumeGroupSnapshotName: strPtr("vgs1"),
				},
			},
			&corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: strPtr("sc1"),
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
					},
				},
			},
		)
	}
	return objects
}

func restoreOps(name string, restore *simplyblockv1alpha1.RestoreOpsSpec) *simplyblockv1alpha1.VolumeGroupSnapshotOps {
	return &simplyblockv1alpha1.VolumeGroupSnapshotOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: simplyblockv1alpha1.VolumeGroupSnapshotOpsSpec{
			VolumeGroupSnapshotRef: "vgs1",
			Action:                 simplyblockv1alpha1.VolumeGroupSnapshotOpsActionRestore,
			Restore:                restore,
		},
	}
}

func newGroupOpsReconciler(t *testing.T, objects ...client.Object) (*VolumeGroupSnapshotOpsReconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme, corev1.AddToScheme,
		volumegroupsnapshotv1beta1.AddToScheme, snapshotv1.AddToScheme)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.VolumeGroupSnapshotOps{}, &corev1.PersistentVolumeClaim{}).
		WithObjects(objects...).
		Build()
	recorder := events.NewFakeRecorder(32)
	return &VolumeGroupSnapshotOpsReconciler{Client: cl, Scheme: scheme, Recorder: recorder}, cl, recorder
}

// reconcileSettle drives the reconciler until the operation stops moving, so a
// test asserts the settled outcome rather than one pass's intermediate step.
func reconcileSettle(t *testing.T, r *VolumeGroupSnapshotOpsReconciler, cl client.Client, name string) *simplyblockv1alpha1.VolumeGroupSnapshotOps {
	t.Helper()
	key := types.NamespacedName{Name: name, Namespace: "default"}
	for range 10 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	ops := &simplyblockv1alpha1.VolumeGroupSnapshotOps{}
	if err := cl.Get(context.Background(), key, ops); err != nil {
		t.Fatalf("get ops: %v", err)
	}
	return ops
}

func restoredClaims(t *testing.T, cl client.Client) map[string]*corev1.PersistentVolumeClaim {
	t.Helper()
	var list corev1.PersistentVolumeClaimList
	if err := cl.List(context.Background(), &list, client.InNamespace("default")); err != nil {
		t.Fatalf("list claims: %v", err)
	}
	claims := map[string]*corev1.PersistentVolumeClaim{}
	for i := range list.Items {
		if list.Items[i].Labels[restoreOpsLabel] != "" {
			claims[list.Items[i].Name] = &list.Items[i]
		}
	}
	return claims
}

// U-25: one claim per member, each dataSource its member snapshot, named
// <prefix>-<source PVC>, class inherited from the source claim.
func TestGroupRestore_CreatesOneClaimPerMember(t *testing.T) {
	objects := append(groupRestoreFixture(), restoreOps("op1", nil))
	r, cl, _ := newGroupOpsReconciler(t, objects...)

	ops := reconcileSettle(t, r, cl, "op1")

	claims := restoredClaims(t, cl)
	if len(claims) != 3 {
		t.Fatalf("restored claims = %d, want 3", len(claims))
	}
	for _, src := range []string{"pvc-a", "pvc-b", "pvc-c"} {
		claim, ok := claims["op1-"+src]
		if !ok {
			t.Fatalf("claim op1-%s not created; have %v", src, claims)
		}
		ds := claim.Spec.DataSource
		if ds == nil || ds.Kind != "VolumeSnapshot" || ds.Name != "snap-"+src {
			t.Errorf("claim op1-%s dataSource = %+v, want VolumeSnapshot snap-%s", src, ds, src)
		}
		if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "sc1" {
			t.Errorf("claim op1-%s did not inherit the source claim's storage class", src)
		}
	}
	if ops.Status.MembersExpected != 3 {
		t.Errorf("membersExpected = %d, want 3", ops.Status.MembersExpected)
	}
	if ops.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", ops.Status.ObservedGeneration)
	}
}

// U-26: an explicit namePrefix replaces the operation-name default.
func TestGroupRestore_HonorsNamePrefix(t *testing.T) {
	objects := append(groupRestoreFixture(),
		restoreOps("op1", &simplyblockv1alpha1.RestoreOpsSpec{NamePrefix: "restored"}))
	r, cl, _ := newGroupOpsReconciler(t, objects...)

	reconcileSettle(t, r, cl, "op1")

	claims := restoredClaims(t, cl)
	if _, ok := claims["restored-pvc-a"]; !ok {
		t.Fatalf("claim restored-pvc-a not created; have %v", claims)
	}
}

// U-27 and U-28: restore.consistencyGroup stamps the membership label on every
// restored claim, and its absence leaves the claims unlabeled.
func TestGroupRestore_ConsistencyGroupLabel(t *testing.T) {
	objects := append(groupRestoreFixture(),
		restoreOps("op1", &simplyblockv1alpha1.RestoreOpsSpec{ConsistencyGroup: "db-restored"}))
	r, cl, _ := newGroupOpsReconciler(t, objects...)
	reconcileSettle(t, r, cl, "op1")
	for name, claim := range restoredClaims(t, cl) {
		if claim.Labels[consistencyGroupLabel] != "db-restored" {
			t.Errorf("claim %s membership label = %q, want db-restored", name, claim.Labels[consistencyGroupLabel])
		}
	}

	objects = append(groupRestoreFixture(), restoreOps("op2", nil))
	r, cl, _ = newGroupOpsReconciler(t, objects...)
	reconcileSettle(t, r, cl, "op2")
	for name, claim := range restoredClaims(t, cl) {
		if _, labeled := claim.Labels[consistencyGroupLabel]; labeled {
			t.Errorf("claim %s carries a membership label without restore.consistencyGroup", name)
		}
	}
}

// withoutMember drops one member snapshot from the fixture, leaving the content
// still expecting three: an incomplete generation.
func withoutMember(objects []client.Object, snapName string) []client.Object {
	kept := objects[:0]
	for _, o := range objects {
		if snap, ok := o.(*snapshotv1.VolumeSnapshot); ok && snap.Name == snapName {
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

// U-29: an incomplete generation fails the operation naming the gap, and no
// claim is created.
func TestGroupRestore_IncompleteGenerationFails(t *testing.T) {
	objects := append(withoutMember(groupRestoreFixture(), "snap-pvc-c"), restoreOps("op1", nil))
	r, cl, _ := newGroupOpsReconciler(t, objects...)

	ops := reconcileSettle(t, r, cl, "op1")

	if ops.Status.Phase != simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "2 of 3") {
		t.Errorf("message %q does not name the incomplete generation", ops.Status.Message)
	}
	if claims := restoredClaims(t, cl); len(claims) != 0 {
		t.Errorf("claims created for a failed incomplete restore: %v", claims)
	}
}

// U-30: enablePartialRestore restores what the generation still has and
// reports the expected count against it.
func TestGroupRestore_PartialRestore(t *testing.T) {
	objects := append(withoutMember(groupRestoreFixture(), "snap-pvc-c"),
		restoreOps("op1", &simplyblockv1alpha1.RestoreOpsSpec{EnablePartialRestore: true}))
	r, cl, _ := newGroupOpsReconciler(t, objects...)

	ops := reconcileSettle(t, r, cl, "op1")

	if claims := restoredClaims(t, cl); len(claims) != 2 {
		t.Fatalf("restored claims = %d, want 2", len(claims))
	}
	if ops.Status.MembersExpected != 3 {
		t.Errorf("membersExpected = %d, want 3", ops.Status.MembersExpected)
	}
}

// U-31: a derived name colliding with a claim this operation does not own
// fails the operation, naming the claim.
func TestGroupRestore_ClaimCollisionFails(t *testing.T) {
	stranger := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "op1-pvc-b", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	objects := append(groupRestoreFixture(), stranger, restoreOps("op1", nil))
	r, cl, _ := newGroupOpsReconciler(t, objects...)

	ops := reconcileSettle(t, r, cl, "op1")

	if ops.Status.Phase != simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "op1-pvc-b") {
		t.Errorf("message %q does not name the colliding claim", ops.Status.Message)
	}
}

// U-32: a target that exists but is not ReadyToUse holds the operation in
// Pending with a RestoreBlocked event and no claims, and the operation
// proceeds once the target turns ready.
func TestGroupRestore_WaitsForTargetReady(t *testing.T) {
	objects := groupRestoreFixture()
	vgs := objects[0].(*volumegroupsnapshotv1beta1.VolumeGroupSnapshot)
	vgs.Status.ReadyToUse = boolPtr(false)
	objects = append(objects, restoreOps("op1", nil))
	r, cl, recorder := newGroupOpsReconciler(t, objects...)

	ops := reconcileSettle(t, r, cl, "op1")
	if ops.Status.Phase != simplyblockv1alpha1.VolumeGroupSnapshotOpsPhasePending {
		t.Fatalf("phase = %q, want Pending while the target is not ready", ops.Status.Phase)
	}
	if claims := restoredClaims(t, cl); len(claims) != 0 {
		t.Fatalf("claims created before the target was ready: %v", claims)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "RestoreBlocked") {
			t.Errorf("event %q, want RestoreBlocked", event)
		}
	default:
		t.Error("no event emitted for the blocked restore")
	}

	fresh := &volumegroupsnapshotv1beta1.VolumeGroupSnapshot{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "vgs1", Namespace: "default"}, fresh); err != nil {
		t.Fatalf("get vgs: %v", err)
	}
	fresh.Status.ReadyToUse = boolPtr(true)
	if err := cl.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update vgs: %v", err)
	}

	reconcileSettle(t, r, cl, "op1")
	if claims := restoredClaims(t, cl); len(claims) != 3 {
		t.Errorf("restored claims = %d after the target turned ready, want 3", len(claims))
	}
}

// The operation succeeds once every restored claim binds, and a terminal
// operation re-reconciles to nothing.
func TestGroupRestore_SucceedsWhenAllClaimsBind(t *testing.T) {
	objects := append(groupRestoreFixture(), restoreOps("op1", nil))
	r, cl, _ := newGroupOpsReconciler(t, objects...)

	ops := reconcileSettle(t, r, cl, "op1")
	if ops.Status.Phase == simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseSucceeded {
		t.Fatal("operation succeeded before any claim bound")
	}

	for name := range restoredClaims(t, cl) {
		claim := &corev1.PersistentVolumeClaim{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, claim); err != nil {
			t.Fatalf("get claim: %v", err)
		}
		claim.Status.Phase = corev1.ClaimBound
		if err := cl.Status().Update(context.Background(), claim); err != nil {
			t.Fatalf("bind claim: %v", err)
		}
	}

	ops = reconcileSettle(t, r, cl, "op1")
	if ops.Status.Phase != simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", ops.Status.Phase)
	}
	if ops.Status.MembersBound != 3 {
		t.Errorf("membersBound = %d, want 3", ops.Status.MembersBound)
	}
	if ops.Status.CompletedAt == nil {
		t.Error("completedAt not set on success")
	}

	// Terminal contract: another pass changes nothing.
	before := ops.Status
	ops = reconcileSettle(t, r, cl, "op1")
	if ops.Status.Phase != before.Phase || ops.Status.MembersBound != before.MembersBound {
		t.Errorf("terminal operation moved: %+v -> %+v", before, ops.Status)
	}
}
