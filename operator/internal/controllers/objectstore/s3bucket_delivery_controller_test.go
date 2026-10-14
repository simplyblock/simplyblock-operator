// Bucket-delivery reconciliation (design §5.3) against a real apiserver,
// once a bucket has already reached Ready: a gateway pod moving to a new
// node, status drifting from what the gateway's pods actually show, and the
// gateway Deployment disappearing entirely. I-08 (two concurrent reconciles
// converge, no duplicate calls) is not tested here: design §7.3 names it as
// the standard controller-runtime guarantee that a single object's key is
// never processed by two workers at once, not discipline this reconciler's
// own code provides, so a test of it would only reprove controller-runtime
// itself.

package objectstore

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
)

// gatewayDeployment is a minimal Deployment a real apiserver accepts (a
// selector matching its own pod template, one container): this package's
// reconciler never reads any of it beyond the Deployment existing at all,
// per design-objectstore.md.
func gatewayDeployment(name string) *appsv1.Deployment {
	labels := map[string]string{gatewayInstanceLabel: name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "gateway", Image: "gateway:test"}},
				},
			},
		},
	}
}

// readyBucketWithGatewayPod drives a fresh bucket to Ready with one gateway
// pod already placed on firstNode, and returns the reconciler (carrying the
// fake registry) and the bucket's key, ready for a test to change the
// gateway's pod placement or status and reconcile again.
func readyBucketWithGatewayPod(
	t *testing.T, storeName, bucketName, firstNode string, registryNodes ...string,
) (*S3BucketReconciler, client.ObjectKey, client.Client, *fakePeerRegistry) {
	t.Helper()
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeCSIBinder(ctx, t, cli)
	startFakeNFSExportCreator(ctx, cli)

	store := testObjectStoreNamed(storeName)
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	if err := cli.Create(ctx, gatewayDeployment(storeName)); err != nil {
		t.Fatalf("creating the gateway Deployment: %v", err)
	}
	if err := cli.Create(ctx, gatewayPod(bucketName+"-gw", storeName, firstNode)); err != nil {
		t.Fatalf("creating the gateway pod: %v", err)
	}

	bucket := &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: "default"},
		Spec: objectstorev1alpha1.S3BucketSpec{
			ObjectStoreRef: storeName, BucketName: bucketName,
			CapacityBytes: 50 * 1 << 20, StorageClassName: fakeStorageClassName,
		},
	}
	if err := cli.Create(ctx, bucket); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	registry := newFakePeerRegistry(ctx, t, registryNodes...)
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme(), Registry: registry}
	key := client.ObjectKeyFromObject(bucket)
	reconcileBucketUntil(t, r, key, func(b *objectstorev1alpha1.S3Bucket) bool {
		return b.Status.Phase == phaseReady
	})
	return r, key, cli, registry
}

// I-07: a gateway pod rescheduled to a new node mid-Ready gets DeliverBucket
// called there, and WithdrawBucket called for the stale node, within the one
// reconcile that follows.
func TestMountDeliveryFollowsAGatewayPodRescheduleWhileReady(t *testing.T) {
	r, key, cli, registry := readyBucketWithGatewayPod(t, "i07-store", "i07-bucket", "node-a", "node-a", "node-b")

	if got := registry.Delivered("node-a"); len(got) != 1 {
		t.Fatalf("node-a delivered = %v before the reschedule, want exactly one", got)
	}

	ctx := context.Background()
	var oldPod corev1.Pod
	if err := cli.Get(ctx, client.ObjectKey{Namespace: "default", Name: "i07-bucket-gw"}, &oldPod); err != nil {
		t.Fatalf("reading the gateway pod: %v", err)
	}
	if err := cli.Delete(ctx, &oldPod); err != nil {
		t.Fatalf("deleting the gateway pod: %v", err)
	}
	if err := cli.Create(ctx, gatewayPod("i07-bucket-gw2", "i07-store", "node-b")); err != nil {
		t.Fatalf("creating the rescheduled gateway pod: %v", err)
	}

	if _, err := r.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := registry.Delivered("node-b"); len(got) != 1 {
		t.Errorf("node-b delivered = %v, want exactly one DeliverBucket call within the one reconcile", got)
	}
	if got := registry.Withdrawn("node-a"); len(got) != 1 {
		t.Errorf("node-a withdrawn = %v, want exactly one WithdrawBucket call within the one reconcile", got)
	}
	var bucket objectstorev1alpha1.S3Bucket
	if err := cli.Get(ctx, key, &bucket); err != nil {
		t.Fatalf("reading the bucket back: %v", err)
	}
	if len(bucket.Status.MountDeliveredNodes) != 1 || bucket.Status.MountDeliveredNodes[0] != "node-b" {
		t.Errorf("status.mountDeliveredNodes = %v, want [node-b]", bucket.Status.MountDeliveredNodes)
	}
}

// I-09: status naming a node the gateway's pods no longer place anything on
// (drifted out-of-band, simulated by writing it directly rather than
// through a real delivery) is corrected on the next pass: withdrawn, even
// though that node was never actually linked to acknowledge it.
func TestMountDeliveryCorrectsADriftedNodeInStatus(t *testing.T) {
	r, key, cli, registry := readyBucketWithGatewayPod(t, "i09-store", "i09-bucket", "node-a", "node-a")

	ctx := context.Background()
	var bucket objectstorev1alpha1.S3Bucket
	if err := cli.Get(ctx, key, &bucket); err != nil {
		t.Fatalf("reading the bucket: %v", err)
	}
	bucket.Status.MountDeliveredNodes = append(bucket.Status.MountDeliveredNodes, "node-drifted")
	if err := cli.Status().Update(ctx, &bucket); err != nil {
		t.Fatalf("injecting the drifted node: %v", err)
	}

	if _, err := r.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := registry.Withdrawn("node-drifted"); len(got) != 0 {
		t.Errorf("node-drifted withdrawn = %v, want none: it was never linked, so there is nothing to call", got)
	}
	var got objectstorev1alpha1.S3Bucket
	if err := cli.Get(ctx, key, &got); err != nil {
		t.Fatalf("reading the bucket back: %v", err)
	}
	if len(got.Status.MountDeliveredNodes) != 1 || got.Status.MountDeliveredNodes[0] != "node-a" {
		t.Errorf("status.mountDeliveredNodes = %v, want [node-a]: the drift corrected itself", got.Status.MountDeliveredNodes)
	}
}

// I-10: the gateway Deployment deleted entirely while a bucket is Ready
// leaves its existing deliveries in place, per design §6's failure table,
// rather than withdrawing them for lack of a node set to target.
func TestMountDeliveryLeavesDeliveriesWhenTheGatewayDeploymentIsGone(t *testing.T) {
	r, key, cli, registry := readyBucketWithGatewayPod(t, "i10-store", "i10-bucket", "node-a", "node-a")

	ctx := context.Background()
	var deployment appsv1.Deployment
	if err := cli.Get(ctx, client.ObjectKey{Namespace: "default", Name: "i10-store"}, &deployment); err != nil {
		t.Fatalf("reading the gateway Deployment: %v", err)
	}
	if err := cli.Delete(ctx, &deployment); err != nil {
		t.Fatalf("deleting the gateway Deployment: %v", err)
	}

	if _, err := r.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := registry.Withdrawn("node-a"); len(got) != 0 {
		t.Errorf("node-a withdrawn = %v, want none: existing deliveries stay in place", got)
	}
	var got objectstorev1alpha1.S3Bucket
	if err := cli.Get(ctx, key, &got); err != nil {
		t.Fatalf("reading the bucket back: %v", err)
	}
	if len(got.Status.MountDeliveredNodes) != 1 || got.Status.MountDeliveredNodes[0] != "node-a" {
		t.Errorf("status.mountDeliveredNodes = %v, want [node-a] unchanged", got.Status.MountDeliveredNodes)
	}
}
