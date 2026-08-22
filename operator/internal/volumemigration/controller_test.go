package volumemigration

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/ctrltest"
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
	// testValidationJobName is the NVMe path-validation Job name shared by
	// validatingVM (which records it in status) and validationJob.
	testValidationJobName = "vmig-validate-1"
	// testValidationNode is the node the pre-existing validation Job is pinned to;
	// testConsumerNode and testSiblingNode are the two nodes consuming the volumes of
	// one shared subsystem in the fan-out tests.
	testValidationNode = "validation-worker"
	testConsumerNode   = "consumer-worker"
	testSiblingNode    = "sibling-worker"
)

// newVMReconciler builds a Reconciler backed by a fake k8s client
// (with VolumeMigration status subresource enabled) and a webapi client pointed
// at apiURL. Pass ctrltest.UnreachableAPI when the API must not be called.
func newVMReconciler(t *testing.T, apiURL string, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()

	scheme := ctrltest.NewScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
		batchv1.AddToScheme,
	)
	cl := ctrltest.NewClient(t, scheme, []client.Object{&simplyblockv1alpha1.VolumeMigration{}}, objs...)

	r := &Reconciler{
		Client:     cl,
		Scheme:     scheme,
		Recorder:   events.NewFakeRecorder(64),
		apiClient:  webapi.NewClient(apiURL),
		coreClient: k8sfake.NewSimpleClientset().CoreV1(),
		// The fake client serves both cached and uncached reads in tests.
		apiReader: cl,
	}
	return r, cl
}

// newPass builds the per-reconcile state the phase steps read and write, for a test
// that calls one step directly instead of going through Reconcile.
func newPass(vm *simplyblockv1alpha1.VolumeMigration) *migrationPass {
	return &migrationPass{vm: vm, before: vm.DeepCopy()}
}

// runStep drives one step of the lifecycle exactly as Reconcile does around it: the
// machine is restored to the phase status names, the step runs, and whatever it
// changed is persisted. It exists for the tests that exercise one step in isolation
// and still want to assert on the object that comes back out of the API.
//
// It reports the step's own error and swallows its requeue, since a test calling one
// step does not go on to act on it; assert on requeues through Reconcile.
func runStep(
	t *testing.T,
	r *Reconciler,
	vm *simplyblockv1alpha1.VolumeMigration,
	step func(context.Context, *migrationPass, *statemachine.Machine[phase]) (ctrl.Result, error),
) error {
	t.Helper()

	p := newPass(vm)
	sm := r.machineFor(t.Context(), p)
	defer sm.Close()
	if err := r.restorePhase(t.Context(), sm, vm); err != nil {
		t.Fatalf("restore phase %q: %v", vm.Status.Phase, err)
	}

	_, stepErr := step(t.Context(), p, sm)
	if err := r.persist(t.Context(), p, sm); err != nil {
		t.Fatalf("persist: %v", err)
	}
	return stepErr
}

// expiredPhase is a phase deadline in the past: the phase ran out of time, possibly
// while the operator was down. It is how a test reaches a timeout path without
// spending the phase's bound waiting for it.
func expiredPhase() *metav1.Time {
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	return &past
}

// testSubsystemNQN is the NQN the fake API reports for testVolumeUUID. Migrations
// are addressed by it, so it appears in every migration URL the controller calls.
const testSubsystemNQN = "nqn.2014-08.io.simplyblock:cluster-uuid:lvol:vol-uuid"

// migrationsPath is the collection endpoint the controller must use for the
// volume under test.
const migrationsPath = "/api/v2/clusters/" + testClusterUUID + "/subsystems/" + testSubsystemNQN + "/migrations/"

// migrationPath and continuePath are the endpoints of the single migration under
// test. The trailing slashes are the control plane's own — a path without them
// only resolves via a redirect.
const migrationPath = migrationsPath + testMigrationUUID + "/"
const continuePath = migrationPath + "continue"

// serveVolume answers the GetVolume call submitMigration makes to resolve the
// volume's subsystem, and reports whether it handled the request. Fake API
// handlers used by submitMigration tests must call it first.
func serveVolume(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/volumes/"+testVolumeUUID+"/") {
		return false
	}
	_, _ = w.Write([]byte(`{"id":"` + testVolumeUUID + `","nqn":"` + testSubsystemNQN + `"}`))
	return true
}

// serveSubsystemMembers answers the pool and volume listings the controller walks to
// find the volumes sharing the migrated subsystem, and reports whether it handled the
// request. Members are the volume under test plus siblingVolumeUUIDs (same NQN); a
// decoy volume in another subsystem guards against the filter being dropped.
//
// Fake API handlers for the Validating phase must call it first: resolving which nodes
// to validate starts from this listing.
func serveSubsystemMembers(w http.ResponseWriter, r *http.Request, siblingVolumeUUIDs ...string) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
		_, _ = w.Write([]byte(`[{"id":"` + testPoolUUID + `"}]`))
	case strings.HasSuffix(r.URL.Path, "/volumes/"):
		vols := []string{`{"id":"` + testVolumeUUID + `","nqn":"` + testSubsystemNQN + `"}`}
		for _, uuid := range siblingVolumeUUIDs {
			vols = append(vols, `{"id":"`+uuid+`","nqn":"`+testSubsystemNQN+`"}`)
		}
		vols = append(vols, `{"id":"other-vol","nqn":"nqn.other:lvol:other-vol"}`)
		_, _ = w.Write([]byte("[" + strings.Join(vols, ",") + "]"))
	default:
		return false
	}
	return true
}

// newAPIServer starts an httptest server that is closed at test end.
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

// boundCSIPV returns a CSI PV bound to the named PVC in the test namespace.
func boundCSIPV(handle, pvcName string) *corev1.PersistentVolume {
	pv := csiPV(handle)
	pv.Spec.ClaimRef = &corev1.ObjectReference{Name: pvcName, Namespace: testVMNamespace}
	return pv
}

// consumerPod returns a pod in the test namespace, in the given phase, that
// references pvcName.
func consumerPod(name, nodeName, pvcName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testVMNamespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
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
// rebalancer image set — the precondition submitMigration's enablement check
// (resolveRebalancerImage) requires before starting a migration.
func migrationCluster() *simplyblockv1alpha1.StorageCluster {
	enabled := true
	image := "rebalancer:test"
	return clusterWithSettings(&simplyblockv1alpha1.VolumeMigrationSettings{
		Enabled:         &enabled,
		RebalancerImage: &image,
	})
}

// ---- submitMigration (Pending -> Validating / Failed) ----

func TestReconcileStart_TransitionsToValidating(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != migrationsPath {
			t.Errorf("unexpected request %s %s, want POST %s", r.Method, r.URL.Path, migrationsPath)
		}
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","target_nqn":"` + testSubsystemNQN +
			`","member_count":3,"connect_strings":[{"nqn":"nqn.x","ip":"10.0.0.1","port":4420,"transport":"tcp"}]}`))
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
	if got.Status.SubsystemNQN != testSubsystemNQN {
		t.Errorf("SubsystemNQN = %q, want %q", got.Status.SubsystemNQN, testSubsystemNQN)
	}
	if got.Status.MemberCount != 3 {
		t.Errorf("MemberCount = %d, want 3", got.Status.MemberCount)
	}
	if got.Status.StartedAt == nil {
		t.Errorf("StartedAt should be set after start")
	}
	if len(got.Status.Connections) != 1 || got.Status.Connections[0].NQN != "nqn.x" {
		t.Errorf("Connections = %+v, want one entry with NQN nqn.x", got.Status.Connections)
	}
}

func TestReconcileStart_PVNotFound_Fails(t *testing.T) {
	r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, baseVM())

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
	r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, vm, pv)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcileStart_EmptyMigrationUUID_Fails(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
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
	if !strings.Contains(got.Status.ErrorMessage, "empty migration UUID") {
		t.Errorf("ErrorMessage = %q, want empty migration UUID", got.Status.ErrorMessage)
	}
}

// TestReconcileStart_Disabled_NeverMigrates guards the safety invariant: an explicit
// Enabled=false must block migration — CreateMigration is never called (the fake API
// fails the test if its migrations endpoint is hit) and the CR ends Failed with no
// MigrationUUID.
func TestReconcileStart_Disabled_NeverMigrates(t *testing.T) {
	disabled := false
	image := "rebalancer:test"

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/migrations/") {
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
			srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				if serveVolume(w, r) {
					return
				}
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

// A cluster busy rebalancing refuses new migrations until it settles — and the
// operator itself causes that, since every completed migration triggers a data
// realignment. The migration must therefore be retried, not failed: it stays pending
// with no migration UUID, and the request is re-submitted later.
func TestReconcileStart_ClusterRebalancing_RetriesInsteadOfFailing(t *testing.T) {
	var creates int
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		creates++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Cluster ` + testClusterUUID +
			` is rebalancing; wait for it to finish before migrating"}`))
	})

	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, baseVM(), pv, migrationCluster())

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a retry delay", res.RequeueAfter)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhasePending {
		t.Errorf("phase = %q, want Pending while deferred", got.Status.Phase)
	}
	if got.Status.DeferredSince == nil {
		t.Errorf("DeferredSince not stamped; the deferral window could not be bounded")
	}
	if got.Status.MigrationUUID != "" {
		t.Errorf("MigrationUUID = %q, want empty (nothing was created)", got.Status.MigrationUUID)
	}

	// The retry re-submits rather than giving up.
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if creates != 2 {
		t.Errorf("CreateMigration called %d time(s), want 2 (retried)", creates)
	}
}

// The retrying is bounded: a cluster that never starts accepting migrations must not
// leave the CR in a non-terminal phase forever, since everything waiting on it waits
// for a terminal phase.
func TestReconcileStart_ClusterRebalancing_FailsAfterDeferralWindow(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Cluster ` + testClusterUUID +
			` is rebalancing; wait for it to finish before migrating"}`))
	})

	vm := baseVM()
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhasePending
	deferred := metav1.NewTime(time.Now().Add(-maxMigrationDeferral - time.Minute))
	vm.Status.DeferredSince = &deferred
	// The window is the Pending phase's own bound, armed on the first refusal.
	vm.Status.PhaseDeadline = expiredPhase()
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed once the deferral window elapsed", got.Status.Phase)
	}
	// The reason must name both the window and the control plane's own detail.
	for _, want := range []string{maxMigrationDeferral.String(), "is rebalancing"} {
		if !strings.Contains(got.Status.ErrorMessage, want) {
			t.Errorf("ErrorMessage = %q, want it to mention %q", got.Status.ErrorMessage, want)
		}
	}
}

// ---- subsystem-scoped migration addressing ----

// A volume with no resolvable subsystem cannot be migrated: without an NQN there
// is no migration endpoint to address, so the migration must fail instead of
// being submitted somewhere else.
func TestReconcileStart_VolumeWithoutNQN_Fails(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("CreateMigration must not be called without a subsystem NQN")
		}
		_, _ = w.Write([]byte(`{"id":"` + testVolumeUUID + `"}`))
	})

	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, baseVM(), pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.ErrorMessage, "no subsystem NQN") {
		t.Errorf("ErrorMessage = %q, want mention of the missing subsystem NQN", got.Status.ErrorMessage)
	}
}

// Every call after create — read, continue, cancel — must address the migration
// under the subsystem recorded in status, since that is the only collection the
// migration ID resolves in.
//
// Run for both shapes the control plane returns: a migration of a shared subsystem
// reports status "running" from the moment it is created, where a single-namespace one
// starts at "new". Neither is terminal, so both must still be continued.
func TestPerformMigration_AddressesMigrationBySubsystem(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase string
		state string
	}{
		{"single-namespace subsystem", "pre_created", "new"},
		{"shared subsystem", "pre_created", "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				if serveSubsystemMembers(w, r) {
					return // the pre-cutover re-check of the consuming nodes
				}
				paths = append(paths, r.Method+" "+r.URL.Path)
				switch {
				case r.Method == http.MethodGet && r.URL.Path == migrationPath:
					_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID +
						`","phase":"` + tc.phase + `","status":"` + tc.state + `"}`))
				case r.Method == http.MethodPost && r.URL.Path == continuePath:
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})

			vm := validatingVM(testValidationJobName)
			job := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
			r, cl := newVMReconciler(t, srv.URL, vm, job)

			if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			want := []string{
				http.MethodGet + " " + migrationPath,
				http.MethodPost + " " + continuePath,
			}
			if !slices.Equal(paths, want) {
				t.Errorf("requests = %v, want %v", paths, want)
			}
			if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
				t.Errorf("phase = %q, want Running", got.Status.Phase)
			}
		})
	}
}

// ---- validateMigration / pollValidationJobs ----

func validatingVM(jobName string) *simplyblockv1alpha1.VolumeMigration {
	vm := baseVM()
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseValidating
	vm.Status.MigrationUUID = testMigrationUUID
	vm.Status.ClusterUUID = testClusterUUID
	vm.Status.PoolUUID = testPoolUUID
	vm.Status.VolumeUUID = testVolumeUUID
	vm.Status.SubsystemNQN = testSubsystemNQN
	if jobName != "" {
		vm.Status.ValidationJobs = []simplyblockv1alpha1.ValidationJob{
			{Node: testValidationNode, JobName: jobName},
		}
	}
	now := metav1.Now()
	vm.Status.StartedAt = &now
	return vm
}

// validationJob returns the validation Job named testValidationJobName — the
// name validatingVM records in status — with the given conditions.
func validationJob(conditions ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: testValidationJobName, Namespace: testVMNamespace},
		Status:     batchv1.JobStatus{Conditions: conditions},
	}
}

// This is the regression test for the wedge fix: a missing validation Job must
// clear ValidationJobName and requeue (so the Job is rebuilt) rather than leave
// the migration stuck in Validating forever.
func TestPollValidationJob_NotFound_ClearsNameAndRequeues(t *testing.T) {
	vm := validatingVM("vmig-validate-gone")
	// No Job object exists in the fake client.
	r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, vm)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if (res == ctrl.Result{}) {
		t.Errorf("expected a requeue to rebuild the Job, got empty result")
	}
	got := getVM(t, cl)
	if len(got.Status.ValidationJobs) != 0 {
		t.Errorf("ValidationJobs = %+v, want cleared", got.Status.ValidationJobs)
	}
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
}

func TestPollValidationJob_InProgress_NoTransition(t *testing.T) {
	vm := validatingVM(testValidationJobName)
	job := validationJob() // no terminal conditions
	r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, vm, job)

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
	if len(got.Status.ValidationJobs) != 1 || got.Status.ValidationJobs[0].JobName != testValidationJobName {
		t.Errorf("ValidationJobs = %+v, want unchanged", got.Status.ValidationJobs)
	}
}

func TestPollValidationJob_Succeeded_ContinuesToRunning(t *testing.T) {
	var continueCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			// cutover reads the phase before continuing.
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			continueCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	vm := validatingVM(testValidationJobName)
	job := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	r, cl := newVMReconciler(t, srv.URL, vm, job)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !continueCalled {
		t.Errorf("expected ContinueMigration to be called")
	}
	if res.RequeueAfter != MigrationInitialDelay {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, MigrationInitialDelay)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if len(got.Status.ValidationJobs) != 0 {
		t.Errorf("ValidationJobs = %+v, want cleared on transition to Running", got.Status.ValidationJobs)
	}
	if got.Status.Connections != nil {
		t.Errorf("Connections = %+v, want nil after transition to Running", got.Status.Connections)
	}
}

func TestPollValidationJob_Failed_CancelsAndFails(t *testing.T) {
	var cancelCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/") {
			cancelCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := validatingVM(testValidationJobName)
	job := validationJob(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue})
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

// When no running pod consumes the volume there are no NVMe I/O paths to
// validate, so validation is skipped and the migration continues directly to
// Running instead of requeuing forever. Here the PV is unbound (no claimRef).
func TestReconcileValidating_NoRunningConsumer_SkipsValidationAndContinues(t *testing.T) {
	var continueCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			// cutover reads the phase before continuing.
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			continueCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	vm := validatingVM("") // no validation job yet
	pv := csiPV("cluster-uuid:pool-uuid:vol-uuid")
	r, cl := newVMReconciler(t, srv.URL, vm, pv)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !continueCalled {
		t.Errorf("expected ContinueMigration to be called when validation is skipped")
	}
	if res.RequeueAfter != MigrationInitialDelay {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, MigrationInitialDelay)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if len(got.Status.ValidationJobs) != 0 {
		t.Errorf("ValidationJobs = %+v, want empty (no job created)", got.Status.ValidationJobs)
	}
}

// A bound PVC that no pod references (workload scaled to zero) is genuinely idle:
// validation is skipped and the migration continues, just like the unbound case.
func TestReconcileValidating_BoundButNoConsumerPod_SkipsValidation(t *testing.T) {
	var continueCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			continueCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	vm := validatingVM("")
	pv := boundCSIPV("cluster-uuid:pool-uuid:vol-uuid", "app-pvc")
	// No pod references app-pvc.
	r, cl := newVMReconciler(t, srv.URL, vm, pv)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !continueCalled {
		t.Errorf("expected ContinueMigration to be called (no consumer pod → skip validation)")
	}
	if res.RequeueAfter != MigrationInitialDelay {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, MigrationInitialDelay)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}

// A consumer pod that exists but is not Running yet must NOT skip validation:
// the migration waits (requeues) so it can validate on the consumer's node once
// the pod is Running. The storage API must not be touched while waiting.
func TestReconcileValidating_ConsumerNotReady_WaitsWithoutContinuing(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		t.Errorf("unexpected request %s %s while waiting for a consumer", r.Method, r.URL.Path)
	})

	vm := validatingVM("")
	pv := boundCSIPV("cluster-uuid:pool-uuid:vol-uuid", "app-pvc")
	pod := consumerPod("app-0", "", "app-pvc", corev1.PodPending)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, pod)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != consumerWaitRetryDelay {
		t.Errorf("RequeueAfter = %v, want %v (waiting for consumer)", res.RequeueAfter, consumerWaitRetryDelay)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating while waiting", got.Status.Phase)
	}
	if len(got.Status.ValidationJobs) != 0 {
		t.Errorf("ValidationJobs = %+v, want empty (no job while consumer not ready)", got.Status.ValidationJobs)
	}
}

// A Running consumer pod drives the normal validation path: a validation Job is
// created on the consumer's node and the storage API is not continued yet.
func TestReconcileValidating_RunningConsumer_CreatesValidationJob(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		t.Errorf("unexpected request %s %s while creating validation jobs", r.Method, r.URL.Path)
	})

	vm := validatingVM("")
	pv := boundCSIPV("cluster-uuid:pool-uuid:vol-uuid", "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	// migrationCluster provides the rebalancer image resolveRebalancerImage needs.
	r, cl := newVMReconciler(t, srv.URL, vm, pv, pod, migrationCluster())

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s after job creation", res.RequeueAfter)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
	if len(got.Status.ValidationJobs) != 1 || got.Status.ValidationJobs[0].Node != testConsumerNode {
		t.Fatalf("ValidationJobs = %+v, want one entry for %s", got.Status.ValidationJobs, testConsumerNode)
	}
	job := &batchv1.Job{}
	name := got.Status.ValidationJobs[0].JobName
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testVMNamespace, Name: name}, job); err != nil {
		t.Fatalf("expected validation job %q to exist: %v", name, err)
	}
	if node := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; node != testConsumerNode {
		t.Errorf("job node selector = %q, want %s (the consumer's node)", node, testConsumerNode)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64(validationJobDeadline.Seconds()) {
		t.Errorf("ActiveDeadlineSeconds = %v, want %v — without it a job that never finishes wedges the migration",
			job.Spec.ActiveDeadlineSeconds, int64(validationJobDeadline.Seconds()))
	}
	if !hasEnv(job, "VMIG_SUBSYSTEM_NQN", testSubsystemNQN) {
		t.Errorf("job does not carry the subsystem NQN; it cannot check for a host connection")
	}
}

// hasEnv reports whether the job's container sets name to value.
func hasEnv(job *batchv1.Job, name, value string) bool {
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == name && e.Value == value {
				return true
			}
		}
	}
	return false
}

// The regression this fan-out exists for: a namespaced volume shares its NVMe
// subsystem with siblings whose consumers run on other nodes, and cutover switches the
// whole subsystem at once. Validating only the named volume's node leaves those hosts
// pointing at the source, and they lose their volume the moment the migration cuts
// over. Every consuming node must get a Job, and the migration must not continue until
// all of them pass.
func TestReconcileValidating_SiblingsOnOtherNodes_ValidatesEveryConsumerNode(t *testing.T) {
	const siblingVolumeUUID = "sibling-vol"
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r, siblingVolumeUUID) {
			return
		}
		t.Errorf("unexpected request %s %s: the migration must not proceed while validating",
			r.Method, r.URL.Path)
	})

	// The migrated volume is consumed on worker-1; its subsystem sibling on worker-2.
	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	siblingPV := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+siblingVolumeUUID, "sibling-pvc")
	siblingPV.Name = "pv-sibling"
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	siblingPod := consumerPod("sibling-0", testSiblingNode, "sibling-pvc", corev1.PodRunning)

	r, cl := newVMReconciler(t, srv.URL, validatingVM(""), pv, siblingPV, pod, siblingPod,
		migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Fatalf("phase = %q, want still Validating until every node has passed", got.Status.Phase)
	}
	nodes := make([]string, 0, len(got.Status.ValidationJobs))
	for _, vj := range got.Status.ValidationJobs {
		nodes = append(nodes, vj.Node)
	}
	slices.Sort(nodes)
	if !slices.Equal(nodes, []string{testConsumerNode, testSiblingNode}) {
		t.Fatalf("validated nodes = %v, want [worker-1 worker-2] — the sibling's node must be included", nodes)
	}
	for _, vj := range got.Status.ValidationJobs {
		job := &batchv1.Job{}
		if err := cl.Get(context.Background(),
			types.NamespacedName{Namespace: testVMNamespace, Name: vj.JobName}, job); err != nil {
			t.Errorf("expected a validation job for node %s: %v", vj.Node, err)
			continue
		}
		if node := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; node != vj.Node {
			t.Errorf("job %s pinned to %q, want %q", vj.JobName, node, vj.Node)
		}
	}
}

// A shared subsystem whose volumes are all consumed on one node needs that node
// validated exactly once. Two Jobs for one node would collide on their name and be
// double-counted by the gate, so the node set must be deduplicated rather than
// following the member list one-to-one.
func TestReconcileValidating_SiblingsOnSameNode_ValidatesThatNodeOnce(t *testing.T) {
	const sibling1, sibling2 = "sibling-vol-1", "sibling-vol-2"
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r, sibling1, sibling2) {
			return
		}
		t.Errorf("unexpected request %s %s while validating", r.Method, r.URL.Path)
	})

	// Three volumes of one subsystem, all mounted by pods on the same node.
	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	siblingUUIDs := []string{sibling1, sibling2}
	objs := make([]client.Object, 0, 4+2*len(siblingUUIDs))
	objs = append(objs, validatingVM(""), pv, pod, migrationCluster())
	for i, uuid := range siblingUUIDs {
		sPV := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+uuid, fmt.Sprintf("sib-pvc-%d", i))
		sPV.Name = fmt.Sprintf("pv-sibling-%d", i)
		objs = append(objs, sPV,
			consumerPod(fmt.Sprintf("sib-%d", i), testConsumerNode,
				fmt.Sprintf("sib-pvc-%d", i), corev1.PodRunning))
	}

	r, cl := newVMReconciler(t, srv.URL, objs...)
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getVM(t, cl)
	if len(got.Status.ValidationJobs) != 1 || got.Status.ValidationJobs[0].Node != testConsumerNode {
		t.Fatalf("ValidationJobs = %+v, want exactly one entry for %s",
			got.Status.ValidationJobs, testConsumerNode)
	}
	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs, client.InNamespace(testVMNamespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		names := make([]string, 0, len(jobs.Items))
		for i := range jobs.Items {
			names = append(names, jobs.Items[i].Name)
		}
		t.Errorf("created %d jobs (%v), want 1 for the single consuming node", len(jobs.Items), names)
	}
}

// The control plane lets a volume join the subsystem until the migration is activated,
// so a sibling can appear after the node set was resolved — on a node nobody validated.
// Cutting over then strands it. The re-check just before ContinueMigration must notice
// the new node, validate it, and hold the cutover until it passes.
func TestPerformMigration_SiblingAppearsDuringValidation_ValidatesItBeforeCutover(t *testing.T) {
	const lateVolumeUUID = "late-sibling-vol"
	var continueCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The listing now reports a sibling that did not exist when validation started.
		if serveSubsystemMembers(w, r, lateVolumeUUID) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			continueCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	// The named volume's node has already been validated; the late sibling is consumed
	// on another node, which has not.
	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = []simplyblockv1alpha1.ValidationJob{
		{Node: testConsumerNode, JobName: testValidationJobName},
	}
	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	latePV := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+lateVolumeUUID, "late-pvc")
	latePV.Name = "pv-late-sibling"
	latePod := consumerPod("late-0", testSiblingNode, "late-pvc", corev1.PodRunning)
	// The already-validated node's Job has passed, so without the re-check this
	// reconcile would go straight to ContinueMigration.
	done := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})

	r, cl := newVMReconciler(t, srv.URL, vm, pv, pod, latePV, latePod, done, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if continueCalled {
		t.Errorf("ContinueMigration was called while the late sibling's node was unvalidated")
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating until the new node passes", got.Status.Phase)
	}
	nodes := make([]string, 0, len(got.Status.ValidationJobs))
	for _, vj := range got.Status.ValidationJobs {
		nodes = append(nodes, vj.Node)
	}
	slices.Sort(nodes)
	if !slices.Equal(nodes, []string{testConsumerNode, testSiblingNode}) {
		t.Fatalf("validated nodes = %v, want both %s and the late %s",
			nodes, testConsumerNode, testSiblingNode)
	}
	for _, vj := range got.Status.ValidationJobs {
		if vj.Node != testSiblingNode {
			continue
		}
		job := &batchv1.Job{}
		if err := cl.Get(context.Background(),
			types.NamespacedName{Namespace: testVMNamespace, Name: vj.JobName}, job); err != nil {
			t.Fatalf("expected a validation job for the late sibling's node: %v", err)
		}
		if node := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; node != testSiblingNode {
			t.Errorf("late job pinned to %q, want %s", node, testSiblingNode)
		}
	}
}

// A node that drops out of the set during validation must not hold up the cutover: its
// Job either already passed or never mattered, and a connected path it no longer needs
// does no harm.
func TestPerformMigration_ConsumerDisappearedDuringValidation_ContinuesAnyway(t *testing.T) {
	var continueCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			continueCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	// Validated two nodes; by now only the named volume's PV exists, with no consumer.
	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = []simplyblockv1alpha1.ValidationJob{
		{Node: testConsumerNode, JobName: testValidationJobName},
		{Node: testSiblingNode, JobName: "vmig-validate-gone-node"},
	}
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	done := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	doneToo := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "vmig-validate-gone-node", Namespace: testVMNamespace},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}},
	}

	r, cl := newVMReconciler(t, srv.URL, vm, pv, done, doneToo, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !continueCalled {
		t.Errorf("expected ContinueMigration; a node leaving the set must not block cutover")
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}

// One node passing is not enough: the migration continues only after every node's Job
// has, because cutover moves the whole subsystem.
func TestPollValidationJobs_OneNodeStillRunning_DoesNotContinue(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: the migration must not continue with a node pending",
			r.Method, r.URL.Path)
	})

	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = append(vm.Status.ValidationJobs,
		simplyblockv1alpha1.ValidationJob{Node: testSiblingNode, JobName: "vmig-validate-2"})
	done := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	pending := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "vmig-validate-2", Namespace: testVMNamespace},
	}

	r, cl := newVMReconciler(t, srv.URL, vm, done, pending)
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating while a node's job is pending", got.Status.Phase)
	}
}

// A single node failing validation cancels the migration: continuing would strand the
// consumers on that node.
func TestPollValidationJobs_OneNodeFailed_CancelsMigration(t *testing.T) {
	var cancelCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/") {
			cancelCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s: a failed node must cancel, not continue",
			r.Method, r.URL.Path)
	})

	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = append(vm.Status.ValidationJobs,
		simplyblockv1alpha1.ValidationJob{Node: testSiblingNode, JobName: "vmig-validate-2"})
	ok := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "vmig-validate-2", Namespace: testVMNamespace},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
		}},
	}

	r, cl := newVMReconciler(t, srv.URL, vm, ok, failed)
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cancelCalled {
		t.Errorf("expected the migration to be cancelled when a node failed validation")
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.ErrorMessage, testSiblingNode) {
		t.Errorf("ErrorMessage = %q, want it to name the node that failed", got.Status.ErrorMessage)
	}
}

// Waiting for consumers is bounded: the control plane will not hold a created-but-
// unstarted migration open forever, so a consumer that never becomes Running must fail
// the migration rather than park it in Validating.
func TestReconcileValidating_ConsumerNeverReady_FailsAfterMaxWait(t *testing.T) {
	var cancelCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/") {
			cancelCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := validatingVM("")
	// Too little of the Validating bound left to still run the validation Jobs, which
	// is what "waited maxConsumerWait for the consumers" reduces to.
	almostUp := metav1.NewTime(time.Now().Add(validationJobDeadline - time.Second))
	vm.Status.PhaseDeadline = &almostUp
	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", "", "app-pvc", corev1.PodPending)

	r, cl := newVMReconciler(t, srv.URL, vm, pv, pod)
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cancelCalled {
		t.Errorf("expected the migration to be cancelled when consumers never became Running")
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.ErrorMessage, maxConsumerWait.String()) {
		t.Errorf("ErrorMessage = %q, want it to mention the wait limit", got.Status.ErrorMessage)
	}
}

// Regression test for the non-idempotent continue hazard: if a prior reconcile
// already called ContinueMigration (promoting the backend migration past
// pre_created) but crashed before persisting phase=Running, the replay must NOT
// re-issue ContinueMigration (the backend would reject it) and must NOT cancel
// the healthy, running migration. It should observe the advanced phase, skip the
// continue call, and reach Running.
func TestPerformMigration_AlreadyContinued_SkipsContinueAndCancel(t *testing.T) {
	var continueCalled, cancelCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			// Backend already advanced: continue happened in a prior reconcile.
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"snap_copy","status":"running"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			continueCalled = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/"):
			cancelCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	// Skip path (unbound PV → no running consumer) drives cutover.
	vm := validatingVM("")
	pv := csiPV("cluster-uuid:pool-uuid:vol-uuid")
	r, cl := newVMReconciler(t, srv.URL, vm, pv)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if continueCalled {
		t.Errorf("ContinueMigration must not be re-issued when the migration already advanced")
	}
	if cancelCalled {
		t.Errorf("CancelMigration must not be called for a healthy, already-advanced migration")
	}
	if res.RequeueAfter != MigrationInitialDelay {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, MigrationInitialDelay)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}

// When ContinueMigration itself reports an error but the migration is genuinely
// still stuck in pre_created (a real start failure), the migration is cancelled
// and marked Failed.
func TestPerformMigration_ContinueFails_StillPreCreated_CancelsAndFails(t *testing.T) {
	var cancelCalled bool
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			// Never advances past pre_created, even after the continue attempt.
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/"):
			cancelCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	vm := validatingVM("")
	pv := csiPV("cluster-uuid:pool-uuid:vol-uuid")
	r, cl := newVMReconciler(t, srv.URL, vm, pv)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cancelCalled {
		t.Errorf("expected CancelMigration on a genuine continue failure")
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcileValidating_EmptyMigrationUUID_Fails(t *testing.T) {
	vm := validatingVM("")
	vm.Status.MigrationUUID = ""
	r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, vm)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ---- pollMigration (Running -> Completed / Failed / progress) ----

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
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID +
			`","status":"running","source_node_id":"source-node","member_count":3}`))
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
	if got.Status.SourceNodeUUID != "source-node" || got.Status.MemberCount != 3 {
		t.Errorf("progress = source %q, %d member(s); want source-node, 3",
			got.Status.SourceNodeUUID, got.Status.MemberCount)
	}
}

// Regression test for the nil-StartedAt fix: a Running migration with no
// StartedAt must not panic; the field is backfilled instead.
func TestReconcileRunning_NilStartedAt_BackfillsWithoutPanic(t *testing.T) {
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/") {
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
			// ctrltest.UnreachableAPI guarantees the API is never touched for terminal objects.
			r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, vm)

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
