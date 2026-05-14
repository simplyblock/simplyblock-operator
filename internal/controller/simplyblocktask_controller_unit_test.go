package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

func TestTaskReconcileAddsFinalizer(t *testing.T) {
	task := &simplyblockv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.TaskSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newTaskStateTestReconciler(t, task)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerTask) {
		t.Fatalf("expected task finalizer to be added")
	}
}

func TestTaskReconcileDeletionRemovesFinalizer(t *testing.T) {
	task := &simplyblockv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-b",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerTask},
		},
		Spec: simplyblockv1alpha1.TaskSpec{
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

	current := &simplyblockv1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("failed to get task: %v", err)
	}
	if contains(current.Finalizers, utils.FinalizerTask) {
		t.Fatalf("expected task finalizer to be removed")
	}
}

func TestTaskReconcilePreventsStatusRegressionWhenClusterMissing(t *testing.T) {
	task := &simplyblockv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-c",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerTask},
		},
		Spec: simplyblockv1alpha1.TaskSpec{
			ClusterName: "cluster-missing",
		},
		Status: simplyblockv1alpha1.TaskStatus{
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

	current := &simplyblockv1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if len(current.Status.Tasks) != 1 || current.Status.Tasks[0].UUID != "task-1" {
		t.Fatalf("status regressed unexpectedly: %#v", current.Status.Tasks)
	}
}

func TestTaskReconcilePreventsStatusRegressionWhenSecretMissing(t *testing.T) {
	const clusterUUID = "cluster-uuid-missing-secret"

	task := &simplyblockv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-d",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerTask},
		},
		Spec: simplyblockv1alpha1.TaskSpec{
			ClusterName: "cluster-a",
		},
		Status: simplyblockv1alpha1.TaskStatus{
			Tasks: []simplyblockv1alpha1.TaskEntry{
				{UUID: "task-2", TaskType: "migrate", TaskStatus: "running"},
			},
		},
	}

	r := newTaskStateTestReconciler(t,
		task,
		testCluster("default", "cluster-a", clusterUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when cluster secret is missing")
	}

	current := &simplyblockv1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if len(current.Status.Tasks) != 1 || current.Status.Tasks[0].UUID != "task-2" {
		t.Fatalf("status regressed unexpectedly: %#v", current.Status.Tasks)
	}
}

func TestTaskReconcileWorksInNonDefaultNamespace(t *testing.T) {
	const ns = "tenant-a"
	const clusterName = "cluster-z"
	const clusterUUID = "cluster-uuid-tenant-a"
	const clusterSecret = "secret-tenant-a"

	// Task endpoints are not present in current OpenAPI spec, so keep allowUnknown=true.
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/tasks",
		webapimock.RouteResponse{
			Status:  http.StatusOK,
			Body:    `[]`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	task := &simplyblockv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-nondefault",
			Namespace:  ns,
			Finalizers: []string{utils.FinalizerTask},
		},
		Spec: simplyblockv1alpha1.TaskSpec{
			ClusterName: clusterName,
		},
	}

	r := newTaskStateTestReconciler(t,
		task,
		testCluster(ns, clusterName, clusterUUID),
		testClusterSecret(ns, clusterName, clusterUUID, clusterSecret),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	// API backend is not mocked here, so reconcile should reach API call and requeue.
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue after task API polling failure in non-default namespace")
	}

	current := &simplyblockv1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if current.Namespace != ns {
		t.Fatalf("namespace changed unexpectedly: got %q want %q", current.Namespace, ns)
	}
	if current.Spec.ClusterName != clusterName {
		t.Fatalf("clusterName changed unexpectedly: got %q want %q", current.Spec.ClusterName, clusterName)
	}
	reqs := mock.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/api/v2/clusters/"+clusterUUID+"/tasks" {
		t.Fatalf("expected task API call with cluster UUID %q, got %#v", clusterUUID, reqs)
	}
}

func TestTaskReconcileFiltersCompletedTasksViaMock(t *testing.T) {
	const clusterUUID = "cluster-uuid-filter"

	// Task endpoints are not present in the current OpenAPI spec; allow unknown for this controller path.
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/tasks",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body: `[
				{"id":"t1","function_name":"rebalance","status":"running","function_result":"in progress","canceled":false,"retry":0},
				{"id":"t2","function_name":"cleanup","status":"done","function_result":"success","canceled":false,"retry":1},
				{"id":"t3","function_name":"remove","status":"running","function_result":"done","canceled":false,"retry":2}
			]`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	task := &simplyblockv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-mock",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerTask},
		},
		Spec: simplyblockv1alpha1.TaskSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newTaskStateTestReconciler(t,
		task,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected periodic requeue for task polling")
	}

	current := &simplyblockv1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if len(current.Status.Tasks) != 1 || current.Status.Tasks[0].UUID != "t1" {
		t.Fatalf("expected only active task to remain in status, got %#v", current.Status.Tasks)
	}
	reqs := mock.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/api/v2/clusters/"+clusterUUID+"/tasks" {
		t.Fatalf("expected task API call with cluster UUID %q, got %#v", clusterUUID, reqs)
	}
}

func TestTaskReconcileNon2xxTaskAPIRequeuesAndPreservesStatus(t *testing.T) {
	const clusterUUID = "cluster-uuid-task-non2xx"

	// Task endpoints are not present in current OpenAPI spec; allow unknown for this controller path.
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/tasks",
		webapimock.RouteResponse{
			Status:  http.StatusBadGateway,
			Body:    `{"error":"upstream unavailable"}`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	task := &simplyblockv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "task-mock-non2xx",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerTask},
		},
		Spec: simplyblockv1alpha1.TaskSpec{
			ClusterName: "cluster-a",
		},
		Status: simplyblockv1alpha1.TaskStatus{
			Tasks: []simplyblockv1alpha1.TaskEntry{
				{UUID: "existing-task", TaskType: "rebalance", TaskStatus: "running"},
			},
		},
	}

	r := newTaskStateTestReconciler(t,
		task,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after non-2xx tasks API response, got %+v", res)
	}

	current := &simplyblockv1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if len(current.Status.Tasks) != 1 || current.Status.Tasks[0].UUID != "existing-task" {
		t.Fatalf("status regressed unexpectedly after non-2xx API response: %#v", current.Status.Tasks)
	}
}

func newTaskStateTestReconciler(t *testing.T, objects ...client.Object) *TaskReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.Task{},
		&simplyblockv1alpha1.StorageCluster{},
		&simplyblockv1alpha1.Pool{},
	}, objects...)

	return &TaskReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
