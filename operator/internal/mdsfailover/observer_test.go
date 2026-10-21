// Tests of the Kubernetes observer, against a fake client whose reads fail on
// demand: O-38 to O-40 as the observer sees them, and the pod and node facts
// the decisions rest on.

package mdsfailover

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type sessionSet map[string]bool

func (s sessionSet) HasSession(pod string) bool { return s[pod] }

func observerFixture() []client.Object {
	selector := map[string]string{"app": stsName}
	two := int32(2)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: stsName, Namespace: "ns", UID: "sts-uid",
			Labels: map[string]string{ClusterLabel: simCluster},
			Annotations: map[string]string{
				AnnotationActive: pod0, AnnotationActiveInstance: boot0, AnnotationEpoch: "4",
				AnnotationStateVolume: "state-handle",
			},
		},
		Spec:   appsv1.StatefulSetSpec{Replicas: &two, Selector: &metav1.LabelSelector{MatchLabels: selector}},
		Status: appsv1.StatefulSetStatus{UpdateRevision: rev2},
	}
	pod := func(name, node, uid, cid string, ready bool) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "ns", UID: types.UID(uid),
				Labels: map[string]string{"app": stsName, revisionLabel: rev1},
			},
			Spec: corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{
				PodIP:      "10.0.0.1",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: ContainerName, ContainerID: cid},
				},
			},
		}
	}
	node := func(name, zone string, ready bool) *corev1.Node {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{zoneLabel: zone}},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			},
		}
	}
	return []client.Object{
		sts,
		pod(pod1, "n2", "u1", "c1", true), pod(pod0, "n1", "u0", "c0", false),
		node("n1", "a", false), node("n2", "b", true),
	}
}

func TestObserverReadsTheRecordGuestsAndNodes(t *testing.T) {
	cl := fake.NewClientBuilder().WithObjects(observerFixture()...).Build()
	obs := &KubeObserver{
		Client: cl, Namespace: "ns", StatefulSet: stsName, Clock: newFakeClock(),
		Sessions: sessionSet{pod1: true},
	}
	got, err := obs.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got.Cluster != simCluster || got.StatefulSetUID != "sts-uid" || got.StateVolume != "state-handle" ||
		got.Replicas != 2 || got.UpdateRevision != rev2 {
		t.Errorf("observation = %+v", got)
	}
	if got.Record != (Record{Active: pod0, Instance: boot0, Epoch: 4}) {
		t.Errorf("record = %+v", got.Record)
	}
	if len(got.Guests) != 2 || got.Guests[0].Pod != pod0 || got.Guests[1].Pod != pod1 {
		t.Fatalf("guests = %+v, want mds-0 then mds-1", got.Guests)
	}
	g0, g1 := got.Guests[0], got.Guests[1]
	if g0.Ready || g0.NodeReady || g0.HasSession || g0.Zone != "a" || g0.Instance != boot0 {
		t.Errorf("mds-0 = %+v", g0)
	}
	if !g1.Ready || !g1.NodeReady || !g1.HasSession || g1.Zone != "b" || g1.Ordinal != 1 {
		t.Errorf("mds-1 = %+v", g1)
	}
}

func TestObserverTreatsAMissingNodeAsNotReady(t *testing.T) {
	objs := observerFixture()
	cl := fake.NewClientBuilder().WithObjects(objs...).Build()
	if err := cl.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}); err != nil {
		t.Fatal(err)
	}
	obs := &KubeObserver{Client: cl, Namespace: "ns", StatefulSet: stsName, Clock: newFakeClock()}
	got, err := obs.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got.Guests[0].NodeReady {
		t.Error("a deleted node reads Ready")
	}
}

func failingClient(objs []client.Object, failures *int, kind string) client.Client {
	return fake.NewClientBuilder().WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Node); ok && kind == "node" && *failures > 0 {
				*failures--
				return errors.New("apiserver blip")
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok && kind == "pods" && *failures > 0 {
				*failures--
				return errors.New("apiserver blip")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
}

func TestO38AnObservationRetriesABlipedRead(t *testing.T) {
	failures := 1
	clk := newFakeClock()
	obs := &KubeObserver{
		Client:    failingClient(observerFixture(), &failures, "pods"),
		Namespace: "ns", StatefulSet: stsName, Clock: clk,
	}
	if _, err := obs.Observe(context.Background()); err != nil {
		t.Fatalf("Observe: %v, want the retry to succeed", err)
	}
	if len(clk.sleeps) != 1 {
		t.Errorf("sleeps = %v, want one 2s pause", clk.sleeps)
	}
}

func TestO39AnObservationFailsWhenEveryRetryFails(t *testing.T) {
	failures := 100
	obs := &KubeObserver{
		Client:    failingClient(observerFixture(), &failures, "node"),
		Namespace: "ns", StatefulSet: stsName, Clock: newFakeClock(),
	}
	if _, err := obs.Observe(context.Background()); err == nil {
		t.Fatal("want an error when a node read fails every time")
	}
}

func TestO40TheThirdRetryOfANodeReadStillDecides(t *testing.T) {
	failures := 3
	obs := &KubeObserver{
		Client:    failingClient(observerFixture(), &failures, "node"),
		Namespace: "ns", StatefulSet: stsName, Clock: newFakeClock(),
	}
	if _, err := obs.Observe(context.Background()); err != nil {
		t.Fatalf("Observe: %v, want success on the third retry", err)
	}
	failures = 4
	if _, err := obs.Observe(context.Background()); err == nil {
		t.Fatal("want an error when the third retry fails too")
	}
}

func TestOrdinalAndInstance(t *testing.T) {
	if Ordinal("mds-12") != 12 || Ordinal(stsName) != -1 || Ordinal("mds-x") != -1 {
		t.Error("Ordinal is wrong")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "u"}}
	if Instance(pod) != "u" {
		t.Errorf("a pod with no runner = %q, want its UID", Instance(pod))
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: ContainerName, ContainerID: "c"}}
	if Instance(pod) != "u/c" {
		t.Errorf("a pod with a runner = %q, want uid/containerID", Instance(pod))
	}
}
