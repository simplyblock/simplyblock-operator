// Bucket-delivery node-set diffing (design §5.3): what changes between a
// gateway's current pod placement and what status already records, and how
// that placement is read off real Pod objects. No cluster and no RPC
// involved; desiredDeliveryNodes only needs a client.Client, so the fake one
// the phase tests already use is enough.

package objectstore

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// U-11: a gateway Deployment with 3 pods on 3 distinct nodes, nothing
// delivered yet, needs all three delivered and nothing withdrawn.
func TestDiffDeliveryNodesDeliversEveryNewNode(t *testing.T) {
	toDeliver, toWithdraw := diffDeliveryNodes([]string{"node-a", "node-b", "node-c"}, nil)
	if !reflect.DeepEqual(toDeliver, []string{"node-a", "node-b", "node-c"}) {
		t.Errorf("toDeliver = %v, want all three nodes", toDeliver)
	}
	if len(toWithdraw) != 0 {
		t.Errorf("toWithdraw = %v, want none", toWithdraw)
	}
}

// U-12: a gateway pod rescheduled from node A to node B delivers to B and
// withdraws from A.
func TestDiffDeliveryNodesFollowsAReschedule(t *testing.T) {
	toDeliver, toWithdraw := diffDeliveryNodes([]string{"node-b"}, []string{"node-a"})
	if !reflect.DeepEqual(toDeliver, []string{"node-b"}) {
		t.Errorf("toDeliver = %v, want [node-b]", toDeliver)
	}
	if !reflect.DeepEqual(toWithdraw, []string{"node-a"}) {
		t.Errorf("toWithdraw = %v, want [node-a]", toWithdraw)
	}
}

// U-14: a gateway Deployment scaled to zero pods withdraws from every node
// that was previously delivered, and delivers nowhere.
func TestDiffDeliveryNodesWithdrawsEverythingWhenScaledToZero(t *testing.T) {
	toDeliver, toWithdraw := diffDeliveryNodes(nil, []string{"node-a", "node-b"})
	if len(toDeliver) != 0 {
		t.Errorf("toDeliver = %v, want none", toDeliver)
	}
	if !reflect.DeepEqual(toWithdraw, []string{"node-a", "node-b"}) {
		t.Errorf("toWithdraw = %v, want both nodes", toWithdraw)
	}
}

// U-15: no gateway pods and nothing previously delivered produces no calls.
func TestDiffDeliveryNodesOfTwoEmptySetsIsEmpty(t *testing.T) {
	toDeliver, toWithdraw := diffDeliveryNodes(nil, nil)
	if len(toDeliver) != 0 || len(toWithdraw) != 0 {
		t.Errorf("diff of two empty sets = (%v, %v), want (nil, nil)", toDeliver, toWithdraw)
	}
}

// U-16: the node set unchanged between two reconciles diffs to empty,
// regardless of the order each side was discovered in.
func TestDiffDeliveryNodesOfEqualSetsIsEmptyRegardlessOfOrder(t *testing.T) {
	toDeliver, toWithdraw := diffDeliveryNodes([]string{"node-b", "node-a"}, []string{"node-a", "node-b"})
	if len(toDeliver) != 0 || len(toWithdraw) != 0 {
		t.Errorf("diff of equal sets = (%v, %v), want (nil, nil)", toDeliver, toWithdraw)
	}
}

// gatewayPod builds a gateway pod of objectStoreRef, in testBucketNamespace
// so both the unit tests here (a fresh fake client per test) and the
// integration tests in s3bucket_delivery_controller_test.go (one shared
// apiserver per package, needing a unique objectStoreRef per test) can share
// it.
func gatewayPod(name, objectStoreRef, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testBucketNamespace,
			Labels: map[string]string{gatewayInstanceLabel: objectStoreRef},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "gateway", Image: "gateway:test"}},
		},
	}
}

// U-13: two gateway pods on the same node produce exactly one entry for it,
// which is what makes reconcileMountDelivery's own diff call DeliverBucket
// for that node once rather than twice.
func TestDesiredDeliveryNodesDedupesPodsOnTheSameNode(t *testing.T) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testObjectStoreName, Namespace: testBucketNamespace}}
	cli := newTestClient(t, deployment,
		gatewayPod("gw-1", testObjectStoreName, "node-a"),
		gatewayPod("gw-2", testObjectStoreName, "node-a"),
		gatewayPod("gw-3", testObjectStoreName, "node-b"))
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}

	nodes, err := r.desiredDeliveryNodes(context.Background(), testS3Bucket())
	if err != nil {
		t.Fatalf("desiredDeliveryNodes: %v", err)
	}
	if !reflect.DeepEqual(nodes, []string{"node-a", "node-b"}) {
		t.Errorf("nodes = %v, want [node-a node-b], node-a once despite two pods", nodes)
	}
}

// A pod not yet scheduled (no NodeName) names no node to deliver to.
func TestDesiredDeliveryNodesIgnoresUnscheduledPods(t *testing.T) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testObjectStoreName, Namespace: testBucketNamespace}}
	cli := newTestClient(t, deployment, gatewayPod("gw-1", testObjectStoreName, ""))
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}

	nodes, err := r.desiredDeliveryNodes(context.Background(), testS3Bucket())
	if err != nil {
		t.Fatalf("desiredDeliveryNodes: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("nodes = %v, want none: the pod is not scheduled yet", nodes)
	}
}

// The distinction I-10 and U-14 both turn on: a Deployment that exists with
// no pods (U-15) reports an empty, error-free node set; a Deployment that
// does not exist at all reports NotFound, which reconcileMountDelivery reads
// differently (leave existing deliveries alone rather than withdraw them).
func TestDesiredDeliveryNodesReportsNotFoundWhenTheDeploymentIsGone(t *testing.T) {
	cli := newTestClient(t)
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}

	_, err := r.desiredDeliveryNodes(context.Background(), testS3Bucket())
	if !apierrorsIsNotFound(err) {
		t.Errorf("err = %v, want IsNotFound", err)
	}
}

func TestDesiredDeliveryNodesOfAnExistingEmptyDeploymentIsEmptyNotError(t *testing.T) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testObjectStoreName, Namespace: testBucketNamespace}}
	cli := newTestClient(t, deployment)
	r := &S3BucketReconciler{Client: cli, Scheme: cli.Scheme()}

	nodes, err := r.desiredDeliveryNodes(context.Background(), testS3Bucket())
	if err != nil {
		t.Fatalf("desiredDeliveryNodes: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("nodes = %v, want none", nodes)
	}
}
