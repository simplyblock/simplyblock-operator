package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	testVMNamespace   = "sb"
	testVMName        = "mig-test"
	testPVName        = "pv-1"
	testClusterUUID   = "cluster-uuid"
	testPoolUUID      = "pool-uuid"
	testVolumeUUID    = "vol-uuid"
	testMigrationUUID = "migration-1"
)

// unreachableAPI is a base URL that always fails to connect; use it for tests
// that must never reach the storage API.
const unreachableAPI = "http://127.0.0.1:1"

// newVMReconciler builds a VolumeMigrationReconciler backed by a fake k8s client
// (with VolumeMigration status subresource enabled) and a webapi client pointed
// at apiURL. Pass unreachableAPI when the API must not be called.
func newVMReconciler(t *testing.T, apiURL string, objs ...client.Object) (*VolumeMigrationReconciler, client.Client) {
	t.Helper()

	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
		batchv1.AddToScheme,
	)
	cl := newTestClient(t, scheme, []client.Object{&simplyblockv1alpha1.VolumeMigration{}}, objs...)

	r := &VolumeMigrationReconciler{
		Client:     cl,
		Scheme:     scheme,
		Recorder:   record.NewFakeRecorder(64),
		apiClient:  webapi.NewClient(apiURL),
		coreClient: k8sfake.NewSimpleClientset().CoreV1(),
	}
	return r, cl
}

// newAPIServer starts an httptest server that is closed at test end.
func newAPIServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func vmRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testVMNamespace, Name: testVMName}}
}

func getVM(t *testing.T, cl client.Client) *simplyblockv1alpha1.VolumeMigration {
	t.Helper()
	vm := &simplyblockv1alpha1.VolumeMigration{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testVMNamespace, Name: testVMName}, vm); err != nil {
		t.Fatalf("get VolumeMigration: %v", err)
	}
	return vm
}

func baseVM() *simplyblockv1alpha1.VolumeMigration {
	return &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testVMNamespace},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         testPVName,
			TargetNodeUUID: "target-node",
		},
	}
}

// csiPV returns a CSI-provisioned PV (named testPVName, matching baseVM's PVName)
// with the given volume handle.
func csiPV(handle string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: testPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: handle},
			},
		},
	}
}

// clusterWithSettings returns a StorageCluster matching testClusterUUID with the given
// volume-migration settings (pass nil for "not configured").
func clusterWithSettings(s *simplyblockv1alpha1.VolumeMigrationSettings) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: testVMNamespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{VolumeMigrationSettings: s},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: testClusterUUID},
	}
}

// migrationCluster returns a StorageCluster with volume migration enabled and a
// rebalancer image set — the precondition reconcileStart's enablement check
// (resolveRebalancerImage) requires before starting a migration.
func migrationCluster() *simplyblockv1alpha1.StorageCluster {
	enabled := true
	image := "rebalancer:test"
	return clusterWithSettings(&simplyblockv1alpha1.VolumeMigrationSettings{
		Enabled:         &enabled,
		RebalancerImage: &image,
	})
}

// ---- reconcileStart (Pending -> Validating / Failed) ----

func TestReconcileStart_TransitionsToValidating(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/migrations") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","connect_strings":[{"nqn":"nqn.x","ip":"10.0.0.1","port":4420,"transport":"tcp"}]}`))
	})

	vm := baseVM()
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster())

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if (res == ctrl.Result{}) {
		t.Errorf("expected a requeue, got empty result")
	}

	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want Validating", got.Status.Phase)
	}
	if got.Status.MigrationUUID != testMigrationUUID {
		t.Errorf("MigrationUUID = %q, want %q", got.Status.MigrationUUID, testMigrationUUID)
	}
	if got.Status.ClusterUUID != testClusterUUID || got.Status.PoolUUID != testPoolUUID || got.Status.VolumeUUID != testVolumeUUID {
		t.Errorf("UUIDs not resolved from CSI handle: %+v", got.Status)
	}
	if got.Status.StartedAt == nil {
		t.Errorf("StartedAt should be set after start")
	}
	if len(got.Status.Connections) != 1 || got.Status.Connections[0].NQN != "nqn.x" {
		t.Errorf("Connections = %+v, want one entry with NQN nqn.x", got.Status.Connections)
	}
}

func TestReconcileStart_PVNotFound_Fails(t *testing.T) {
	r, cl := newVMReconciler(t, unreachableAPI, baseVM())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.ErrorMessage, "not found") {
		t.Errorf("ErrorMessage = %q, want mention of not found", got.Status.ErrorMessage)
	}
}

func TestReconcileStart_BadCSIHandle_Fails(t *testing.T) {
	vm := baseVM()
	pv := csiPV("not-a-valid-handle")
	r, cl := newVMReconciler(t, unreachableAPI, vm, pv)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcileStart_EmptyMigrationUUID_Fails(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":""}`))
	})
	vm := baseVM()
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.ErrorMessage, "empty migration ID") {
		t.Errorf("ErrorMessage = %q, want empty migration ID", got.Status.ErrorMessage)
	}
}

// TestReconcileStart_Disabled_NeverMigrates guards the safety invariant: an explicit
// Enabled=false must block migration — CreateMigration is never called (the fake API
// fails the test if its migrations endpoint is hit) and the CR ends Failed with no
// MigrationUUID.
func TestReconcileStart_Disabled_NeverMigrates(t *testing.T) {
	disabled := false
	image := "rebalancer:test"

	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/migrations") {
			t.Errorf("CreateMigration must not be called when migration is disabled")
		}
		w.WriteHeader(http.StatusNotFound)
	})

	vm := baseVM()
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	cluster := clusterWithSettings(&simplyblockv1alpha1.VolumeMigrationSettings{Enabled: &disabled, RebalancerImage: &image})
	r, cl := newVMReconciler(t, srv.URL, vm, pv, cluster)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.MigrationUUID != "" {
		t.Errorf("MigrationUUID = %q, want empty (no migration started)", got.Status.MigrationUUID)
	}
	if got.Status.StartedAt != nil {
		t.Errorf("StartedAt should be nil when no migration is started")
	}
	if !strings.Contains(got.Status.ErrorMessage, "disabled") {
		t.Errorf("ErrorMessage = %q, want contains %q", got.Status.ErrorMessage, "disabled")
	}
}

// TestReconcileStart_DefaultsToEnabled verifies volume migration is enabled by default:
// an omitted VolumeMigrationSettings block, or one that enables migration without pinning
// an image, still proceeds (using the default rebalancer image) and reaches Validating.
func TestReconcileStart_DefaultsToEnabled(t *testing.T) {
	enabled := true
	cases := []struct {
		name     string
		settings *simplyblockv1alpha1.VolumeMigrationSettings
	}{
		{name: "settings block omitted", settings: nil},
		{name: "enabled without pinned image", settings: &simplyblockv1alpha1.VolumeMigrationSettings{Enabled: &enabled}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `"}`))
			})

			vm := baseVM()
			pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
			r, cl := newVMReconciler(t, srv.URL, vm, pv, clusterWithSettings(tc.settings))

			if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got := getVM(t, cl)
			if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
				t.Errorf("phase = %q, want Validating (migration enabled by default)", got.Status.Phase)
			}
			if got.Status.MigrationUUID != testMigrationUUID {
				t.Errorf("MigrationUUID = %q, want %q", got.Status.MigrationUUID, testMigrationUUID)
			}
		})
	}
}

// ---- reconcileValidating / pollValidationJob ----

func validatingVM(jobName string) *simplyblockv1alpha1.VolumeMigration {
	vm := baseVM()
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseValidating
	vm.Status.MigrationUUID = testMigrationUUID
	vm.Status.ClusterUUID = testClusterUUID
	vm.Status.PoolUUID = testPoolUUID
	vm.Status.VolumeUUID = testVolumeUUID
	vm.Status.ValidationJobName = jobName
	now := metav1.Now()
	vm.Status.StartedAt = &now
	return vm
}

func validationJob(name string, conditions ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testVMNamespace},
		Status:     batchv1.JobStatus{Conditions: conditions},
	}
}

// This is the regression test for the wedge fix: a missing validation Job must
// clear ValidationJobName and requeue (so the Job is rebuilt) rather than leave
// the migration stuck in Validating forever.
func TestPollValidationJob_NotFound_ClearsNameAndRequeues(t *testing.T) {
	vm := validatingVM("vmig-validate-gone")
	// No Job object exists in the fake client.
	r, cl := newVMReconciler(t, unreachableAPI, vm)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if (res == ctrl.Result{}) {
		t.Errorf("expected a requeue to rebuild the Job, got empty result")
	}
	got := getVM(t, cl)
	if got.Status.ValidationJobName != "" {
		t.Errorf("ValidationJobName = %q, want cleared", got.Status.ValidationJobName)
	}
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
}

func TestPollValidationJob_InProgress_NoTransition(t *testing.T) {
	vm := validatingVM("vmig-validate-1")
	job := validationJob("vmig-validate-1") // no terminal conditions
	r, cl := newVMReconciler(t, unreachableAPI, vm, job)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if (res != ctrl.Result{}) {
		t.Errorf("expected no requeue while job in progress (watch-driven), got %+v", res)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want Validating", got.Status.Phase)
	}
	if got.Status.ValidationJobName != "vmig-validate-1" {
		t.Errorf("ValidationJobName = %q, want unchanged", got.Status.ValidationJobName)
	}
}

func TestPollValidationJob_Succeeded_ContinuesToRunning(t *testing.T) {
	var continueCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/continue") {
			continueCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := validatingVM("vmig-validate-1")
	job := validationJob("vmig-validate-1", batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	r, cl := newVMReconciler(t, srv.URL, vm, job)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !continueCalled {
		t.Errorf("expected ContinueMigration to be called")
	}
	if res.RequeueAfter != vmigration.MigrationInitialDelay {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, vmigration.MigrationInitialDelay)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if got.Status.ValidationJobName != "" {
		t.Errorf("ValidationJobName = %q, want cleared on transition to Running", got.Status.ValidationJobName)
	}
	if got.Status.Connections != nil {
		t.Errorf("Connections = %+v, want nil after transition to Running", got.Status.Connections)
	}
}

func TestPollValidationJob_Failed_CancelsAndFails(t *testing.T) {
	var cancelCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			cancelCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := validatingVM("vmig-validate-1")
	job := validationJob("vmig-validate-1", batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue})
	r, cl := newVMReconciler(t, srv.URL, vm, job)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cancelCalled {
		t.Errorf("expected CancelMigration to be called on validation failure")
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcileValidating_EmptyMigrationUUID_Fails(t *testing.T) {
	vm := validatingVM("")
	vm.Status.MigrationUUID = ""
	r, cl := newVMReconciler(t, unreachableAPI, vm)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ---- reconcileRunning (Running -> Completed / Failed / progress) ----

func runningVM(startedAt *metav1.Time) *simplyblockv1alpha1.VolumeMigration {
	vm := baseVM()
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseRunning
	vm.Status.MigrationUUID = testMigrationUUID
	vm.Status.ClusterUUID = testClusterUUID
	vm.Status.PoolUUID = testPoolUUID
	vm.Status.VolumeUUID = testVolumeUUID
	vm.Status.StartedAt = startedAt
	return vm
}

func TestReconcileRunning_Completed(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","status":"done","snaps_total":5,"snaps_migrated":5}`))
	})
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	r, cl := newVMReconciler(t, srv.URL, runningVM(&past))

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseCompleted {
		t.Errorf("phase = %q, want Completed", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Errorf("CompletedAt should be set")
	}
}

// Regression test: a migration that recovered after a retried step may report
// status=done while error_message still carries the transient error. That must
// be treated as success, not failure.
func TestReconcileRunning_DoneWithLingeringError_Completes(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","status":"done","error_message":"transient nvme reconnect, retried"}`))
	})
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	r, cl := newVMReconciler(t, srv.URL, runningVM(&past))

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseCompleted {
		t.Errorf("phase = %q, want Completed despite lingering error_message", got.Status.Phase)
	}
}

func TestReconcileRunning_Failed(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","status":"failed","error_message":"boom"}`))
	})
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	r, cl := newVMReconciler(t, srv.URL, runningVM(&past))

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.ErrorMessage != "boom" {
		t.Errorf("ErrorMessage = %q, want boom", got.Status.ErrorMessage)
	}
}

// A backend-cancelled migration is terminal and not a success.
func TestReconcileRunning_Cancelled_Fails(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","status":"cancelled"}`))
	})
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	r, cl := newVMReconciler(t, srv.URL, runningVM(&past))

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcileRunning_InProgress_UpdatesProgress(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","status":"running","snaps_total":10,"snaps_migrated":3}`))
	})
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	r, cl := newVMReconciler(t, srv.URL, runningVM(&past))

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 10*time.Second {
		t.Errorf("RequeueAfter = %v, want 10s", res.RequeueAfter)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want still Running", got.Status.Phase)
	}
	if got.Status.SnapsTotal != 10 || got.Status.SnapsMigrated != 3 {
		t.Errorf("progress = %d/%d, want 3/10", got.Status.SnapsMigrated, got.Status.SnapsTotal)
	}
}

// Regression test for the nil-StartedAt fix: a Running migration with no
// StartedAt must not panic; the field is backfilled instead.
func TestReconcileRunning_NilStartedAt_BackfillsWithoutPanic(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","completed_at":0}`))
	})
	r, cl := newVMReconciler(t, srv.URL, runningVM(nil))

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.StartedAt == nil {
		t.Errorf("StartedAt should have been backfilled")
	}
}

// ---- Abort semantics ----

func TestReconcileAbort_FromValidating(t *testing.T) {
	assertAbort(t, simplyblockv1alpha1.VolumeMigrationPhaseValidating)
}

func TestReconcileAbort_FromRunning(t *testing.T) {
	assertAbort(t, simplyblockv1alpha1.VolumeMigrationPhaseRunning)
}

func assertAbort(t *testing.T, from simplyblockv1alpha1.VolumeMigrationPhase) {
	t.Helper()
	var cancelCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			cancelCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := baseVM()
	vm.Spec.Abort = true
	vm.Status.Phase = from
	vm.Status.MigrationUUID = testMigrationUUID
	vm.Status.ClusterUUID = testClusterUUID
	vm.Status.PoolUUID = testPoolUUID
	vm.Status.VolumeUUID = testVolumeUUID
	r, cl := newVMReconciler(t, srv.URL, vm)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cancelCalled {
		t.Errorf("expected CancelMigration to be called on abort")
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseAborted {
		t.Errorf("phase = %q, want Aborted", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Errorf("CompletedAt should be set on abort")
	}
}

// ---- terminal phases are no-ops ----

func TestReconcile_TerminalPhase_NoOp(t *testing.T) {
	for _, phase := range []simplyblockv1alpha1.VolumeMigrationPhase{
		simplyblockv1alpha1.VolumeMigrationPhaseCompleted,
		simplyblockv1alpha1.VolumeMigrationPhaseFailed,
		simplyblockv1alpha1.VolumeMigrationPhaseAborted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			vm := baseVM()
			vm.Status.Phase = phase
			// unreachableAPI guarantees the API is never touched for terminal objects.
			r, cl := newVMReconciler(t, unreachableAPI, vm)

			res, err := r.Reconcile(context.Background(), vmRequest())
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if (res != ctrl.Result{}) {
				t.Errorf("expected no requeue for terminal phase, got %+v", res)
			}
			if got := getVM(t, cl); got.Status.Phase != phase {
				t.Errorf("phase = %q, want unchanged %q", got.Status.Phase, phase)
			}
		})
	}
}
