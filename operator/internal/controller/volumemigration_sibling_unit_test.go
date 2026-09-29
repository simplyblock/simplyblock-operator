// A VolumeMigration whose subsystem is already being migrated by another one
// waits instead of submitting. Submitting made CreateMigration cancel the live
// migration to make room for its own, and two CRs for one subsystem cancelled
// each other for ever (2026-09-28).

package controller

import (
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func siblingVM(name string, phase simplyblockv1alpha1.VolumeMigrationPhase) *simplyblockv1alpha1.VolumeMigration {
	vm := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testVMNamespace},
		Spec:       simplyblockv1alpha1.VolumeMigrationSpec{PVName: "other-pv", TargetNodeUUID: "target-node"},
	}
	vm.Status.Phase = phase
	vm.Status.SubsystemNQN = testSubsystemNQN
	return vm
}

func TestReconcileStart_WaitsWhileASiblingMigratesTheSubsystem(t *testing.T) {
	posts := 0
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			posts++
		}
		t.Errorf("unexpected request %s %s: nothing must be submitted while a sibling runs", r.Method, r.URL.Path)
	})

	vm := baseVM()
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster(),
		siblingVM("sibling-running", simplyblockv1alpha1.VolumeMigrationPhaseRunning))

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != siblingMigrationRetryDelay {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, siblingMigrationRetryDelay)
	}
	if posts != 0 {
		t.Errorf("CreateMigration was called %d time(s) while a sibling was migrating the subsystem", posts)
	}
	got := getVM(t, cl)
	if got.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseValidating || got.Status.MigrationUUID != "" {
		t.Errorf("the waiting CR must not claim a migration: phase=%q uuid=%q", got.Status.Phase, got.Status.MigrationUUID)
	}
}

func TestReconcileStart_AFinishedSiblingDoesNotBlock(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","target_nqn":"` + testSubsystemNQN +
			`","member_count":1,"connect_strings":[]}`))
	})

	vm := baseVM()
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster(),
		siblingVM("sibling-done", simplyblockv1alpha1.VolumeMigrationPhaseCompleted),
		siblingVM("sibling-failed", simplyblockv1alpha1.VolumeMigrationPhaseFailed))

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want Validating: finished siblings do not hold the subsystem", got.Status.Phase)
	}
}

// A volume already on its target -- moved by a sibling's batch -- completes
// the VolumeMigration instead of failing it and blaming the target.
func TestReconcileStart_AlreadyOnTargetCompletes(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"LVol ` + testVolumeUUID + ` is already on node target-node; cannot migrate to the same node"}`))
	})
	vm := baseVM() // target "target-node"
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseCompleted {
		t.Errorf("phase = %q (err %q), want Completed", got.Status.Phase, got.Status.ErrorMessage)
	}
}

// The same refusal naming a DIFFERENT node is not success.
func TestReconcileStart_AlreadyOnAnotherNodeStillFails(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveVolume(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"LVol ` + testVolumeUUID + ` is already on node other-node; cannot migrate to the same node"}`))
	})
	vm := baseVM()
	pv := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	r, cl := newVMReconciler(t, srv.URL, vm, pv, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}
