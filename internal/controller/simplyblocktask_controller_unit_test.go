package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
)

func TestTaskReconcileAddsFinalizer(t *testing.T) {
	task := &simplyblockv1alpha1.SimplyBlockTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockTaskSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newTaskStateTestReconciler(t, task)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockTask{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if !contains(current.Finalizers, "simplyblock.task.finalizer") {
		t.Fatalf("expected task finalizer to be added")
	}
}

func TestTaskReconcileDeletionRemovesFinalizer(t *testing.T) {
	task := &simplyblockv1alpha1.SimplyBlockTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-b",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.task.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockTaskSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newTaskStateTestReconciler(t, task)
	if err := r.Delete(context.Background(), task); err != nil {
		t.Fatalf("failed to trigger deletion: %v", err)
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockTask{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("failed to get task: %v", err)
	}
	if contains(current.Finalizers, "simplyblock.task.finalizer") {
		t.Fatalf("expected task finalizer to be removed")
	}
}

func TestTaskReconcilePreventsStatusRegressionWhenClusterMissing(t *testing.T) {
	task := &simplyblockv1alpha1.SimplyBlockTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-c",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.task.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockTaskSpec{
			ClusterName: "cluster-missing",
		},
		Status: simplyblockv1alpha1.SimplyBlockTaskStatus{
			Tasks: []simplyblockv1alpha1.TaskEntry{
				{UUID: "task-1", TaskType: "rebuild", TaskStatus: "running"},
			},
		},
	}

	r := newTaskStateTestReconciler(t, task)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when cluster UUID is unresolved")
	}

	current := &simplyblockv1alpha1.SimplyBlockTask{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if len(current.Status.Tasks) != 1 || current.Status.Tasks[0].UUID != "task-1" {
		t.Fatalf("status regressed unexpectedly: %#v", current.Status.Tasks)
	}
}

func TestTaskReconcilePreventsStatusRegressionWhenSecretMissing(t *testing.T) {
	task := &simplyblockv1alpha1.SimplyBlockTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-d",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.task.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockTaskSpec{
			ClusterName: "cluster-a",
		},
		Status: simplyblockv1alpha1.SimplyBlockTaskStatus{
			Tasks: []simplyblockv1alpha1.TaskEntry{
				{UUID: "task-2", TaskType: "migrate", TaskStatus: "running"},
			},
		},
	}

	r := newTaskStateTestReconciler(t,
		task,
		testCluster("default", "cluster-a", "cluster-uuid"),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when cluster secret is missing")
	}

	current := &simplyblockv1alpha1.SimplyBlockTask{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if len(current.Status.Tasks) != 1 || current.Status.Tasks[0].UUID != "task-2" {
		t.Fatalf("status regressed unexpectedly: %#v", current.Status.Tasks)
	}
}

func testCluster(namespace, clusterName, uuid string) *simplyblockv1alpha1.SimplyBlockStorageCluster {
	return &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-" + clusterName,
			Namespace: namespace,
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
			ClusterName: clusterName,
		},
		Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
			UUID: uuid,
		},
	}
}

func testClusterSecret(namespace, clusterName, uuid, secret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(uuid),
			"secret": []byte(secret),
		},
	}
}

func testPool(namespace, poolName, clusterName, uuid string) *simplyblockv1alpha1.SimplyBlockPool {
	return &simplyblockv1alpha1.SimplyBlockPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: namespace,
		},
		Spec: simplyblockv1alpha1.SimplyBlockPoolSpec{
			Name:        poolName,
			ClusterName: clusterName,
		},
		Status: simplyblockv1alpha1.SimplyBlockPoolStatus{
			UUID: uuid,
		},
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func newTaskStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockTaskReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add simplyblock scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.SimplyBlockTask{},
			&simplyblockv1alpha1.SimplyBlockStorageCluster{},
			&simplyblockv1alpha1.SimplyBlockPool{},
		).
		WithObjects(objects...).
		Build()

	return &SimplyBlockTaskReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
