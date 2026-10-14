// The S3Bucket phase-transition table (design-bucket-provisioning.md §6),
// proved against a fake client: no cluster, no csi-link session, one
// reconcile pass per assertion. What a bound volume or a Ready NFSExport
// looks like is fixture data here, not anything a real CSI driver or
// NFSExportReconciler produced: that is fake_csi_binder_test.go's and
// fake_nfsexport_test.go's job, exercised by the integration class instead.

package objectstore

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testBucketNamespace = "default"
	testBucketName      = "team-a-reports"
	testObjectStoreName = "team-a-store"
)

func testObjectStore() *objectstorev1alpha1.ObjectStore {
	return &objectstorev1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: testObjectStoreName, Namespace: testBucketNamespace},
	}
}

func testS3Bucket(mutate ...func(*objectstorev1alpha1.S3Bucket)) *objectstorev1alpha1.S3Bucket {
	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name: testBucketName, Namespace: testBucketNamespace,
			Finalizers: []string{objectstorev1alpha1.S3BucketFinalizer},
		},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef:   testObjectStoreName,
			BucketName:       testBucketName,
			CapacityBytes:    100 * 1 << 20,
			StorageClassName: fakeStorageClassName,
		},
	}
	for _, m := range mutate {
		m(bucket)
	}
	return bucket
}

func boundPVC(bucket *objectstorev1alpha1.S3Bucket, volumeHandle string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + bucket.Name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "fake.csi.simplyblock.io", VolumeHandle: volumeHandle},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcNameFor(bucket.Spec.BucketName), Namespace: bucket.Namespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	return pvc, pv
}

func testNFSExport(volumeHandle string, phase simplyblockv1alpha2.NFSExportPhase) *simplyblockv1alpha2.NFSExport {
	return &simplyblockv1alpha2.NFSExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nfsexp-" + testBucketName},
		Spec: simplyblockv1alpha2.NFSExportSpec{
			VolumeRef:  volumeHandle,
			ExportPath: "/var/lib/simplyblock/exports/" + testBucketName,
		},
		Status: simplyblockv1alpha2.NFSExportStatus{Phase: phase},
	}
}

func reconcile(t *testing.T, cli client.Client, bucket *objectstorev1alpha1.S3Bucket) objectstorev1alpha1.S3Bucket {
	t.Helper()
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}
	if _, err := r.Reconcile(context.Background(), ctrlRequest(bucket)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got objectstorev1alpha1.S3Bucket
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(bucket), &got); err != nil {
		t.Fatalf("reading back the bucket: %v", err)
	}
	return got
}

// U-01: a brand new bucket, no PVC yet, reconciles to Pending (and creates
// the PVC, which is Pending's own first act, per §6's label on the edge out).
func TestNewBucketStartsPending(t *testing.T) {
	bucket := testS3Bucket()
	cli := newTestClient(t, testObjectStore(), bucket)

	got := reconcile(t, cli, bucket)

	if got.Status.Phase != phasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := cli.Get(context.Background(),
		client.ObjectKey{Namespace: bucket.Namespace, Name: pvcNameFor(bucket.Spec.BucketName)}, &pvc); err != nil {
		t.Fatalf("no PVC was created: %v", err)
	}
}

// U-02: the PVC already exists but has not bound yet, so the bucket stays
// Pending rather than advancing on the create alone.
func TestClaimNotYetBoundStaysPending(t *testing.T) {
	bucket := testS3Bucket()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcNameFor(bucket.Spec.BucketName), Namespace: bucket.Namespace},
	}
	cli := newTestClient(t, testObjectStore(), bucket, pvc)

	got := reconcile(t, cli, bucket)

	if got.Status.Phase != phasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
}

// U-03: the PVC is Bound and no NFSExport matches it yet, so the bucket
// enters Provisioning to wait on one.
func TestBoundClaimEntersProvisioning(t *testing.T) {
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) { b.Status.Phase = phasePending })
	pvc, pv := boundPVC(bucket, "f0bb9077-78c4-4482-9ccf-a5693ce2df78:9d016dd4-34d7-42f0-b549-52a5af2f1399:cb2f293c-6d6f-4687-ad13-eb81fbec7314")
	cli := newTestClient(t, testObjectStore(), bucket, pvc, pv)

	got := reconcile(t, cli, bucket)

	if got.Status.Phase != phaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
	if got.Status.PhaseDeadline == nil {
		t.Error("Provisioning carries no phase deadline")
	}
}

// U-04: the NFSExport exists but has not reached Ready yet, so Provisioning
// waits rather than advancing.
func TestExportNotYetReadyStaysProvisioning(t *testing.T) {
	handle := "f0bb9077-78c4-4482-9ccf-a5693ce2df78:9d016dd4-34d7-42f0-b549-52a5af2f1399:cb2f293c-6d6f-4687-ad13-eb81fbec7314"
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) { b.Status.Phase = phaseProvisioning })
	pvc, pv := boundPVC(bucket, handle)
	export := testNFSExport(handle, simplyblockv1alpha2.NFSExportPhaseAssembling)
	cli := newTestClient(t, testObjectStore(), bucket, pvc, pv, export)

	got := reconcile(t, cli, bucket)

	if got.Status.Phase != phaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
	if got.Status.NFSExportName != export.Name {
		t.Errorf("NFSExportName = %q, want %q recorded even before Ready", got.Status.NFSExportName, export.Name)
	}
}

// U-05: the NFSExport is Ready, and with nothing yet to wait on for mount
// delivery (W-12), the bucket reaches Ready.
func TestReadyExportCompletesProvisioning(t *testing.T) {
	handle := "f0bb9077-78c4-4482-9ccf-a5693ce2df78:9d016dd4-34d7-42f0-b549-52a5af2f1399:cb2f293c-6d6f-4687-ad13-eb81fbec7314"
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) { b.Status.Phase = phaseProvisioning })
	pvc, pv := boundPVC(bucket, handle)
	export := testNFSExport(handle, simplyblockv1alpha2.NFSExportPhaseReady)
	cli := newTestClient(t, testObjectStore(), bucket, pvc, pv, export)

	got := reconcile(t, cli, bucket)

	if got.Status.Phase != phaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.PhaseDeadline != nil {
		t.Error("Ready still carries a phase deadline")
	}
}

// U-06: the NFSExport itself degrades while Provisioning, which fails the
// bucket at once rather than waiting out the deadline for something that is
// not going to recover on its own.
func TestDegradedExportDegradesTheBucket(t *testing.T) {
	handle := "f0bb9077-78c4-4482-9ccf-a5693ce2df78:9d016dd4-34d7-42f0-b549-52a5af2f1399:cb2f293c-6d6f-4687-ad13-eb81fbec7314"
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) { b.Status.Phase = phaseProvisioning })
	pvc, pv := boundPVC(bucket, handle)
	export := testNFSExport(handle, simplyblockv1alpha2.NFSExportPhaseDegraded)
	cli := newTestClient(t, testObjectStore(), bucket, pvc, pv, export)

	got := reconcile(t, cli, bucket)

	if got.Status.Phase != phaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	exportReady := findCondition(got.Status.Conditions, conditionExportReady)
	if exportReady == nil || exportReady.Status != metav1.ConditionFalse {
		t.Errorf("ExportReady condition = %+v, want False", exportReady)
	}
}

// U-07: Provisioning's own deadline has already passed with the NFSExport
// Ready, so the only thing it could still be waiting on is mount delivery
// (W-12's own check, stubbed true today). The deadline path still degrades
// rather than reaching Ready on a timed-out wait.
func TestProvisioningDeadlinePassedDegrades(t *testing.T) {
	handle := "f0bb9077-78c4-4482-9ccf-a5693ce2df78:9d016dd4-34d7-42f0-b549-52a5af2f1399:cb2f293c-6d6f-4687-ad13-eb81fbec7314"
	past := metav1.NewTime(metav1.Now().Add(-time.Minute))
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) {
		b.Status.Phase = phaseProvisioning
		b.Status.PhaseDeadline = &past
	})
	pvc, pv := boundPVC(bucket, handle)
	export := testNFSExport(handle, simplyblockv1alpha2.NFSExportPhaseAssembling)
	cli := newTestClient(t, testObjectStore(), bucket, pvc, pv, export)

	got := reconcile(t, cli, bucket)

	if got.Status.Phase != phaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
}

// U-08: a Ready bucket with nothing changed re-reconciles to no writes: the
// resourceVersion does not move.
func TestReadyBucketReconcilesToNoWrites(t *testing.T) {
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) { b.Status.Phase = phaseReady })
	cli := newTestClient(t, testObjectStore(), bucket)
	before := bucket.DeepCopy()
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(bucket), before); err != nil {
		t.Fatalf("reading the fixture back: %v", err)
	}

	got := reconcile(t, cli, bucket)

	if got.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion moved from %s to %s on a no-op reconcile", before.ResourceVersion, got.ResourceVersion)
	}
}

// U-09: deletion requested while Pending, with no PVC ever created, removes
// the finalizer immediately and attempts no PVC delete.
func TestDeletePendingBucketRemovesFinalizerWithNoPVCAttempt(t *testing.T) {
	now := metav1.Now()
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) {
		b.Status.Phase = phasePending
		b.DeletionTimestamp = &now
	})
	cli := newTestClient(t, testObjectStore(), bucket)

	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}
	if _, err := r.Reconcile(context.Background(), ctrlRequest(bucket)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got objectstorev1alpha1.S3Bucket
	err := cli.Get(context.Background(), client.ObjectKeyFromObject(bucket), &got)
	if err == nil && len(got.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want none left", got.Finalizers)
	}
}

// U-10: deletion requested while Ready withdraws from every delivered node,
// deletes the PVC, and only then drops the finalizer.
func TestDeleteReadyBucketWithdrawsThenDeletesThenReleasesFinalizer(t *testing.T) {
	now := metav1.Now()
	pvcName := pvcNameFor(testBucketName)
	bucket := testS3Bucket(func(b *objectstorev1alpha1.S3Bucket) {
		b.Status.Phase = phaseReady
		b.Status.PersistentVolumeClaimName = pvcName
		b.Status.MountDeliveredNodes = []string{"node-a"}
		b.DeletionTimestamp = &now
	})
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: bucket.Namespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-" + bucket.Name},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + bucket.Name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "f0bb9077-78c4-4482-9ccf-a5693ce2df78:9d016dd4-34d7-42f0-b549-52a5af2f1399:cb2f293c-6d6f-4687-ad13-eb81fbec7314"},
		}},
	}
	cli := newTestClient(t, testObjectStore(), bucket, pvc, pv)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registry := newFakePeerRegistry(ctx, t, "node-a")

	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme(), Registry: registry}
	if _, err := r.Reconcile(ctx, ctrlRequest(bucket)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := registry.Withdrawn("node-a"); len(got) != 1 || got[0].Name != testBucketName {
		t.Errorf("node-a withdrawn = %+v, want exactly one withdrawal of %q", got, testBucketName)
	}

	var gotPVC corev1.PersistentVolumeClaim
	if err := cli.Get(ctx, client.ObjectKeyFromObject(pvc), &gotPVC); !apierrorsIsNotFound(err) {
		t.Errorf("PVC still exists after delete: err=%v", err)
	}

	var got objectstorev1alpha1.S3Bucket
	err := cli.Get(ctx, client.ObjectKeyFromObject(bucket), &got)
	if err == nil && len(got.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want none left", got.Finalizers)
	}
}

func findCondition(conditions []metav1.Condition, t string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == t {
			return &conditions[i]
		}
	}
	return nil
}
