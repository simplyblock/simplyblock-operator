package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// ---- reconcileStart: resolving the volume before submitting ----

// The volume can be gone by the time the migration is reconciled (deleted PVC, a
// stale CR). There is nothing to migrate, so this fails rather than retrying forever.
func TestReconcileStart_VolumeGone_Fails(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/volumes/"+testVolumeUUID+"/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Errorf("unexpected request %s %s: nothing may be submitted for a missing volume",
			r.Method, r.URL.Path)
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
	if !strings.Contains(got.Status.ErrorMessage, "no longer exists") {
		t.Errorf("ErrorMessage = %q, want it to say the volume is gone", got.Status.ErrorMessage)
	}
}

// A transient read failure is not a reason to fail the migration: it must be retried.
func TestReconcileStart_VolumeLookupError_RetriesWithoutFailing(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/volumes/"+testVolumeUUID+"/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, baseVM(), pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err == nil {
		t.Errorf("expected the lookup failure to be returned for a retry")
	}
	if got := getVM(t, cl); got.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = Failed, want the migration left alone for a retry")
	}
}

// A create rejected because a migration already exists cancels the blocking one and
// returns nothing, so the CR must requeue and try again rather than fail.
func TestReconcileStart_ExistingMigrationCancelled_Requeues(t *testing.T) {
	var cancelled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == migrationsPath:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"a migration already exists. Cancel it first."}`))
		case r.Method == http.MethodGet && r.URL.Path == migrationsPath:
			_, _ = w.Write([]byte(`[{"id":"mig-old","status":"running"}]`))
		case r.Method == http.MethodDelete:
			cancelled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, baseVM(), pv, migrationCluster())

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cancelled {
		t.Errorf("expected the blocking migration to be cancelled")
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a retry after the cancellation", res.RequeueAfter)
	}
	got := getVM(t, cl)
	if got.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = Failed, want a retry instead")
	}
	if got.Status.MigrationUUID != "" {
		t.Errorf("MigrationUUID = %q, want empty (no migration was created)", got.Status.MigrationUUID)
	}
}

// Once the cluster accepts the migration, the deferral stamp must be cleared —
// otherwise a later reconcile would measure the window from the first refusal.
func TestReconcileStart_AcceptedAfterDeferral_ClearsDeferredSince(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","member_count":1}`))
	})

	vm := baseVM()
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhasePending
	deferred := metav1.NewTime(time.Now().Add(-time.Minute))
	vm.Status.DeferredSince = &deferred
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Fatalf("phase = %q, want Validating", got.Status.Phase)
	}
	if got.Status.DeferredSince != nil {
		t.Errorf("DeferredSince = %v, want cleared once the migration was accepted", got.Status.DeferredSince)
	}
}

// Every connect parameter has to reach the validation Job: the job passes them to
// `nvme connect`, and a dropped timeout changes the path's failure behaviour.
//
// One is not passed through. ctrl_loss_tmo is replaced with vmigration.CtrlLossTmoSec,
// because a migration target path becomes the volume's data path at cutover and must
// carry the same loss timeout as every other path in the system — the control plane
// answers with an hour, and an abandoned path retries for at least that long. What is
// recorded here is what will be connected, which is the point of overriding it at
// ingestion rather than where the Job is built.
func TestReconcileStart_RecordsEveryConnectionField(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","member_count":2,"connect_strings":[
			{"nqn":"nqn.x","ip":"10.0.0.1","port":4420,"transport":"tcp","nr-io-queues":3,
			 "reconnect-delay":2,"ctrl-loss-tmo":3600,"fast-io-fail-tmo":8,"keep-alive-tmo":4}]}`))
	})

	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, baseVM(), pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if len(got.Status.Connections) != 1 {
		t.Fatalf("Connections = %+v, want one entry", got.Status.Connections)
	}
	want := simplyblockv1alpha1.MigrationConnection{
		NQN: "nqn.x", IP: "10.0.0.1", Port: 4420, Transport: "tcp",
		NrIoQueues: 3, ReconnectDelay: 2, CtrlLossTmo: vmigration.CtrlLossTmoSec,
		FastIOFailTmo: 8, KeepAliveTmo: 4,
	}
	if got.Status.Connections[0] != want {
		t.Errorf("connection = %+v, want %+v", got.Status.Connections[0], want)
	}
	if vmigration.CtrlLossTmoSec == 3600 {
		t.Error("the override is vacuous: CtrlLossTmoSec matches what the control plane answered")
	}
	if got.Status.MemberCount != 2 {
		t.Errorf("MemberCount = %d, want 2", got.Status.MemberCount)
	}
}

// ---- performMigration: the branches around the continue call ----

// A migration that reached a terminal state on its own must not be continued or
// cancelled; it advances to Running so the polling path classifies the outcome.
func TestPerformMigration_AlreadyTerminal_AdvancesForClassification(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"completed","status":"done"}`))
		default:
			t.Errorf("unexpected request %s %s: a terminal migration needs neither continue nor cancel",
				r.Method, r.URL.Path)
		}
	})

	vm := validatingVM(testValidationJobName)
	job := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	r, cl := newVMReconciler(t, srv.URL, vm, job)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running so reconcileRunning classifies it", got.Status.Phase)
	}
}

// The continue call can report an error after having taken effect. If the migration
// has moved past pre_created, it started — cancelling it would kill a healthy move.
func TestPerformMigration_ContinueErroredButAdvanced_TreatedAsContinued(t *testing.T) {
	var cancelCalled bool
	calls := 0
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			calls++
			if calls == 1 {
				// Before the continue attempt: still awaiting its start.
				_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
				return
			}
			// The re-read after the error shows it did start after all.
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"snap_copy","status":"running"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Method == http.MethodDelete:
			cancelCalled = true
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
	if cancelCalled {
		t.Errorf("CancelMigration must not be called for a migration that did start")
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}

// Reading the migration before the continue can fail transiently. That must requeue,
// not cancel: the migration is fine, our view of it is not.
func TestPerformMigration_GetMigrationError_RequeuesWithoutCancelling(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s: a read failure must not act on the migration",
				r.Method, r.URL.Path)
		}
	})

	vm := validatingVM(testValidationJobName)
	job := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	r, cl := newVMReconciler(t, srv.URL, vm, job)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a retry", res.RequeueAfter)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
}

// When the operator gives up and the cancel also fails, the CR must still fail — and
// say that target-side objects may have been left behind.
func TestPerformMigration_ContinueAndCancelBothFail_ReportsBoth(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			http.Error(w, "continue boom", http.StatusInternalServerError)
		case r.Method == http.MethodDelete:
			http.Error(w, "cancel boom", http.StatusInternalServerError)
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
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.ErrorMessage, "ContinueMigration") {
		t.Errorf("ErrorMessage = %q, want the original continue failure", got.Status.ErrorMessage)
	}
}

// The pre-cutover re-check is best effort: if the member listing fails, the migration
// proceeds with the nodes already validated rather than stalling indefinitely.
func TestPerformMigration_LateNodeCheckFails_ContinuesWithValidatedSet(t *testing.T) {
	var continueCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
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

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !continueCalled {
		t.Errorf("expected the migration to continue despite the failed re-check")
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}

// ---- reconcileRunning ----

// Inside the initial delay the control-plane tracker may not have the record yet, so
// nothing is polled and no progress is written.
func TestReconcileRunning_WithinInitialDelay_DoesNotPoll(t *testing.T) {
	srv := newAPIServer(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s inside the initial delay", r.Method, r.URL.Path)
	})

	now := metav1.Now()
	r, cl := newVMReconciler(t, srv.URL, runningVM(&now))

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a poll retry", res.RequeueAfter)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want still Running", got.Status.Phase)
	}
}

// A migration still in flight past the stuck timeout is surfaced as a warning while
// polling continues — it may yet finish, and cancelling it is the operator's call.
func TestReconcileRunning_PastStuckTimeout_WarnsAndKeepsPolling(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"snap_copy","status":"running"}`))
	})

	old := metav1.NewTime(time.Now().Add(-vmigration.MigrationStuckWarningTimeout - time.Minute))
	r, cl := newVMReconciler(t, srv.URL, runningVM(&old))

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want polling to continue", res.RequeueAfter)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want still Running (stuck is a warning, not a verdict)", got.Status.Phase)
	}
}

// Polling failures are transient by nature; they must not decide the migration's fate.
func TestReconcileRunning_PollError_RequeuesWithoutVerdict(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	past := metav1.NewTime(time.Now().Add(-time.Minute))
	r, cl := newVMReconciler(t, srv.URL, runningVM(&past))

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a retry", res.RequeueAfter)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want still Running", got.Status.Phase)
	}
	if got.Status.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want unset while the outcome is unknown", got.Status.CompletedAt)
	}
}

// ---- Validating: resolving the node set ----

// Without the member listing the node set is unknown, and validating a guess would
// risk cutting over with an unvalidated consumer. Retry instead of proceeding.
func TestReconcileValidating_MemberListingFails_RetriesWithoutJobs(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/storage-pools/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request %s %s: nothing may proceed without the member list",
			r.Method, r.URL.Path)
	})

	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	r, cl := newVMReconciler(t, srv.URL, validatingVM(""), pv, pod, migrationCluster())

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a retry", res.RequeueAfter)
	}
	got := getVM(t, cl)
	if len(got.Status.ValidationJobs) != 0 {
		t.Errorf("ValidationJobs = %+v, want none until the members are known", got.Status.ValidationJobs)
	}
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
}

// A subsystem member with no PersistentVolume in this cluster is not consumed through
// this CSI driver, so it contributes no node — and must not block the migration.
func TestReconcileValidating_MemberWithoutPV_ValidatesTheRest(t *testing.T) {
	const orphanVolumeUUID = "orphan-vol"
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r, orphanVolumeUUID) {
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	// Only the migrated volume has a PV; the sibling exists solely in the backend.
	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	r, cl := newVMReconciler(t, srv.URL, validatingVM(""), pv, pod, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if len(got.Status.ValidationJobs) != 1 || got.Status.ValidationJobs[0].Node != testConsumerNode {
		t.Errorf("ValidationJobs = %+v, want one entry for the consumable member",
			got.Status.ValidationJobs)
	}
}

// A sibling's PVC can live in another namespace than the VolumeMigration CR. Its node
// still needs the new paths, so namespace must not scope the search.
func TestReconcileValidating_SiblingInAnotherNamespace_IsIncluded(t *testing.T) {
	const siblingVolumeUUID = "cross-ns-vol"
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r, siblingVolumeUUID) {
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)

	// The sibling's PVC and pod live in a different namespace entirely.
	siblingPV := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + siblingVolumeUUID)
	siblingPV.Name = "pv-cross-ns"
	siblingPV.Spec.ClaimRef = &corev1.ObjectReference{Name: "other-pvc", Namespace: "other-ns"}
	siblingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other-0", Namespace: "other-ns"},
		Spec: corev1.PodSpec{
			NodeName: testSiblingNode,
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "other-pvc"},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, cl := newVMReconciler(t, srv.URL, validatingVM(""), pv, pod, siblingPV, siblingPod,
		migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	nodes := map[string]bool{}
	for _, vj := range getVM(t, cl).Status.ValidationJobs {
		nodes[vj.Node] = true
	}
	if !nodes[testSiblingNode] {
		t.Errorf("validated nodes = %v, want the cross-namespace sibling's node %s included",
			nodes, testSiblingNode)
	}
}

// The image is resolved when the Jobs are built, not only at submit time. If it cannot
// be resolved then, retry — the migration is already created and must not be failed.
func TestReconcileValidating_ImageUnresolvable_Requeues(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	// No StorageCluster: the rebalancer image cannot be resolved.
	r, cl := newVMReconciler(t, srv.URL, validatingVM(""), pv, pod)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a retry", res.RequeueAfter)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
	if len(got.Status.ValidationJobs) != 0 {
		t.Errorf("ValidationJobs = %+v, want none without an image", got.Status.ValidationJobs)
	}
}

// The presence check reads the host's NVMe sysfs, which the container only sees
// through an explicit mount — without it the check would report "no connection"
// everywhere and validation would silently no-op.
func TestBuildValidationJob_MountsHostSysfs(t *testing.T) {
	r, _ := newVMReconciler(t, unreachableAPI)
	vm := validatingVM("")
	job := r.buildValidationJob(vm, testConsumerNode, "rebalancer:test")

	var sysMount *corev1.VolumeMount
	for i, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "host-sys" {
			sysMount = &job.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if sysMount == nil {
		t.Fatalf("the job does not mount the host sysfs; the subsystem check cannot work")
	}
	if !sysMount.ReadOnly {
		t.Errorf("host sysfs mount must be read-only")
	}
	if !hasEnv(job, "VMIG_SYS_ROOT", sysMount.MountPath) {
		t.Errorf("VMIG_SYS_ROOT must point at the mount path %q", sysMount.MountPath)
	}
	var found bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "host-sys" && v.HostPath != nil && v.HostPath.Path == "/sys" {
			found = true
		}
	}
	if !found {
		t.Errorf("host-sys volume must come from the host's /sys")
	}
}
