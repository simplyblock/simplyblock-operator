// S3BucketReconciler's phase machine, driven against a real apiserver with
// W-08's fakes standing in for the CSI controller, NFSExportReconciler, and
// a real csi-link session (design-bucket-provisioning.md §11). Each test
// calls Reconcile directly, in a loop, rather than running a full manager:
// what is proved is the reconcile loop's own behavior against real CRD
// validation and a real PVC/PV/NFSExport lifecycle, not controller-runtime's
// event plumbing, which none of these tests are about.

package objectstore

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
)

// expectEvent drains recorder's channel looking for one event containing
// reason, failing if none of the events recorded so far name it. Every
// Degraded transition and every blocked wait owes an event
// (reconciler-patterns §8), so a scenario naming one checks for it rather
// than only the resulting phase. Earlier, unrelated events (a prior phase's
// own wait, fired on an earlier pass) are expected and skipped rather than
// failing the check.
func expectEvent(t *testing.T, recorder *events.FakeRecorder, reason string) {
	t.Helper()
	for {
		select {
		case e := <-recorder.Events:
			if strings.Contains(e, reason) {
				return
			}
		default:
			t.Errorf("no recorded event named reason %q", reason)
			return
		}
	}
}

// testObjectStoreNamed is a valid ObjectStore fixture, filling the required
// fields this package's own reconciler never reads (only existence matters
// to it): the real values are design-objectstore.md's own concern.
func testObjectStoreNamed(name string) *objectstorev1alpha1.ObjectStore {
	return &objectstorev1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       objectstorev1alpha1.ObjectStoreSpec{Image: "quay.io/simplyblock-io/objectstore:latest", Port: 8000},
	}
}

// reconcileBucketUntil calls Reconcile in a loop against a real apiserver,
// where W-08's fakes make progress between calls in their own background
// goroutines, until cond holds or the budget runs out.
func reconcileBucketUntil(
	t *testing.T, r *S3BucketReconciler, key client.ObjectKey, cond func(*objectstorev1alpha1.S3Bucket) bool,
) *objectstorev1alpha1.S3Bucket {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Reconcile(context.Background(), ctrlRequestKey(key)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		var got objectstorev1alpha1.S3Bucket
		if err := r.Get(context.Background(), key, &got); err != nil {
			t.Fatalf("reading the bucket back: %v", err)
		}
		if cond(&got) {
			return &got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the bucket to reach the expected state")
	return nil
}

// I-01: a created S3Bucket gets a PVC carrying its spec's storageClassName
// and capacityBytes.
func TestS3BucketCreatesThePVC(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := testObjectStoreNamed("i01-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "i01-bucket", Namespace: "default"},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef: store.Name, BucketName: "i01-bucket",
			CapacityBytes: 50 * 1 << 20, StorageClassName: fakeStorageClassName,
		},
	}
	if err := cli.Create(ctx, bucket); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}
	key := client.ObjectKeyFromObject(bucket)
	// The first pass only adds the finalizer and comes back with a fresh
	// copy (reconciler-patterns: a finalizer write bumps resourceVersion, so
	// nothing after it trusts the copy already in hand). The PVC create is
	// the second pass's own work.
	for range 2 {
		if _, err := r.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	var pvc corev1.PersistentVolumeClaim
	if err := cli.Get(ctx, client.ObjectKey{Namespace: "default", Name: pvcNameFor(bucket.Spec.BucketName)}, &pvc); err != nil {
		t.Fatalf("no PVC was created: %v", err)
	}
	if got := pvc.Spec.StorageClassName; got == nil || *got != fakeStorageClassName {
		t.Errorf("PVC storageClassName = %v, want %q", got, fakeStorageClassName)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Value() != bucket.Spec.CapacityBytes {
		t.Errorf("PVC capacity = %s, want %d bytes", got.String(), bucket.Spec.CapacityBytes)
	}
}

// I-02: once the PVC binds (W-08's fake CSI binder) and a matching
// NFSExport is fabricated Ready (W-08's fake NFSExport creator), the bucket
// reaches Ready.
func TestS3BucketReachesReadyOnceExportIsReady(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)
	startFakeNFSExportCreator(ctx, cli)

	store := testObjectStoreNamed("i02-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "i02-bucket", Namespace: "default"},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef: store.Name, BucketName: "i02-bucket",
			CapacityBytes: 50 * 1 << 20, StorageClassName: fakeStorageClassName,
		},
	}
	if err := cli.Create(ctx, bucket); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	recorder := events.NewFakeRecorder(16)
	// No gateway Deployment is created (that is design-objectstore.md's own
	// scope), so mount delivery converges immediately with zero nodes to
	// deliver to (design §6: no Deployment to target).
	r := &S3BucketReconciler{
		Client: cli, Scheme: cli.Scheme(), Recorder: recorder,
		Registry: newFakePeerRegistry(ctx, t),
	}
	key := client.ObjectKeyFromObject(bucket)
	got := reconcileBucketUntil(t, r, key, func(b *objectstorev1alpha1.S3Bucket) bool {
		return b.Status.Phase == phaseReady
	})
	if got.Status.NFSExportName == "" {
		t.Error("Ready bucket carries no NFSExportName")
	}
	expectEvent(t, recorder, "BucketReady")
}

// I-03: a storageClassName that does not exist (no fake binder started for
// it) leaves the PVC unbound, and the bucket stays Pending rather than
// degrading: design §9's own retry-forever case.
func TestS3BucketStaysPendingOnUnknownStorageClass(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Deliberately no startFakeCSIBinder: nothing ever binds this PVC.

	store := testObjectStoreNamed("i03-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "i03-bucket", Namespace: "default"},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef: store.Name, BucketName: "i03-bucket",
			CapacityBytes: 50 * 1 << 20, StorageClassName: "does-not-exist",
		},
	}
	if err := cli.Create(ctx, bucket); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	recorder := events.NewFakeRecorder(16)
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme(), Recorder: recorder}
	key := client.ObjectKeyFromObject(bucket)
	for range 2 {
		if _, err := r.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	var got objectstorev1alpha1.S3Bucket
	if err := cli.Get(ctx, key, &got); err != nil {
		t.Fatalf("reading the bucket back: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	expectEvent(t, recorder, "PersistentVolumeClaimNotBound")
}

// I-04: the PVC binds but no NFSExport ever appears (no fake NFSExport
// creator started), so once Provisioning's own deadline is forced to the
// past, the bucket degrades naming NoMatchingExport.
func TestS3BucketDegradesWhenNoExportAppears(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)
	// Deliberately no startFakeNFSExportCreator: nothing ever creates an export.

	store := testObjectStoreNamed("i04-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "i04-bucket", Namespace: "default"},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef: store.Name, BucketName: "i04-bucket",
			CapacityBytes: 50 * 1 << 20, StorageClassName: fakeStorageClassName,
		},
	}
	if err := cli.Create(ctx, bucket); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	recorder := events.NewFakeRecorder(16)
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme(), Recorder: recorder}
	key := client.ObjectKeyFromObject(bucket)
	reconcileBucketUntil(t, r, key, func(b *objectstorev1alpha1.S3Bucket) bool {
		return b.Status.Phase == phaseProvisioning
	})

	// Force the deadline into the past rather than waiting out
	// s3BucketProvisioningDeadline for real.
	forcePastDeadline(t, cli, key)

	got := reconcileBucketUntil(t, r, key, func(b *objectstorev1alpha1.S3Bucket) bool {
		return b.Status.Phase == phaseDegraded
	})
	if got.Status.Message == "" {
		t.Error("Degraded bucket carries no message naming why")
	}
	expectEvent(t, recorder, "NoMatchingExport")
}

// I-05: spec.objectStoreRef names no ObjectStore at all, so the bucket
// degrades on its very first Provisioning pass, before any deadline.
func TestS3BucketDegradesWhenObjectStoreMissing(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)
	// Deliberately no ObjectStore created.

	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "i05-bucket", Namespace: "default"},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef: "does-not-exist", BucketName: "i05-bucket",
			CapacityBytes: 50 * 1 << 20, StorageClassName: fakeStorageClassName,
		},
	}
	if err := cli.Create(ctx, bucket); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	recorder := events.NewFakeRecorder(16)
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme(), Recorder: recorder}
	key := client.ObjectKeyFromObject(bucket)
	got := reconcileBucketUntil(t, r, key, func(b *objectstorev1alpha1.S3Bucket) bool {
		return b.Status.Phase == phaseDegraded
	})
	cond := findCondition(got.Status.Conditions, conditionObjectStoreOK)
	if cond == nil || cond.Reason != "ObjectStoreNotFound" {
		t.Errorf("ObjectStoreReady condition = %+v, want reason ObjectStoreNotFound", cond)
	}
	expectEvent(t, recorder, "ObjectStoreNotFound")
}

// I-06: a reconciler built fresh mid-Provisioning (standing in for an
// operator restart, since nothing here is held in memory between calls
// anyway) resumes from status rather than re-creating the PVC.
func TestS3BucketResumesMidProvisioningWithoutRecreatingThePVC(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)

	store := testObjectStoreNamed("i06-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "i06-bucket", Namespace: "default"},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef: store.Name, BucketName: "i06-bucket",
			CapacityBytes: 50 * 1 << 20, StorageClassName: fakeStorageClassName,
		},
	}
	if err := cli.Create(ctx, bucket); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	key := client.ObjectKeyFromObject(bucket)
	first := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}
	reconcileBucketUntil(t, first, key, func(b *objectstorev1alpha1.S3Bucket) bool {
		return b.Status.Phase == phaseProvisioning
	})

	var pvcBefore corev1.PersistentVolumeClaim
	if err := cli.Get(ctx, client.ObjectKey{Namespace: "default", Name: pvcNameFor(bucket.Spec.BucketName)}, &pvcBefore); err != nil {
		t.Fatalf("reading the PVC before the restart: %v", err)
	}

	// A fresh reconciler instance: nothing carries over except what is in the
	// resource, the same as a restarted process.
	second := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}
	if _, err := second.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}

	var pvcAfter corev1.PersistentVolumeClaim
	if err := cli.Get(ctx, client.ObjectKey{Namespace: "default", Name: pvcNameFor(bucket.Spec.BucketName)}, &pvcAfter); err != nil {
		t.Fatalf("reading the PVC after the restart: %v", err)
	}
	if pvcAfter.UID != pvcBefore.UID {
		t.Errorf("PVC UID changed from %s to %s: the restart recreated it", pvcBefore.UID, pvcAfter.UID)
	}
	var got objectstorev1alpha1.S3Bucket
	if err := cli.Get(ctx, key, &got); err != nil {
		t.Fatalf("reading the bucket back: %v", err)
	}
	if got.Status.Phase != phaseProvisioning {
		t.Errorf("phase = %q, want Provisioning resumed", got.Status.Phase)
	}
}

// forcePastDeadline patches status.phaseDeadline into the past, standing in
// for waiting out s3BucketProvisioningDeadline for real.
func forcePastDeadline(t *testing.T, cli client.Client, key client.ObjectKey) {
	t.Helper()
	var bucket objectstorev1alpha1.S3Bucket
	if err := cli.Get(context.Background(), key, &bucket); err != nil {
		t.Fatalf("reading the bucket: %v", err)
	}
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	bucket.Status.PhaseDeadline = &past
	if err := cli.Status().Update(context.Background(), &bucket); err != nil {
		t.Fatalf("forcing the deadline into the past: %v", err)
	}
}
