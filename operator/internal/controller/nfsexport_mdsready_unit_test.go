// Tests for the MDSReady condition a Ready NFSExport carries: what the export's
// status says about its metadata server, separately from the phase.

package controller

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func mdsReady(t *testing.T, e *simplyblockv1alpha2.NFSExport) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(e.Status.Conditions, simplyblockv1alpha2.NFSExportConditionMDSReady)
}

func expectMDSReady(t *testing.T, e *simplyblockv1alpha2.NFSExport, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := mdsReady(t, e)
	if c == nil {
		t.Fatalf("no %s condition: the export says nothing about its metadata server",
			simplyblockv1alpha2.NFSExportConditionMDSReady)
	}
	if c.Status != status || c.Reason != reason {
		t.Errorf("%s = %s/%s, want %s/%s (%s)", c.Type, c.Status, c.Reason, status, reason, c.Message)
	}
}

// Regression: 2026-11-12-pnfs-mds-stays-dead-after-state-disk-journal-abort.
// 61 NFSExports reported Ready for 19 h 39 m while their metadata server served
// nothing: the failed health check was an event and nothing else, so no status
// anyone watched said the exports were down.
func TestAFailedHealthCheckTurnsMDSReadyFalse(t *testing.T) {
	asm := &fakeAssembler{checkErr: errors.New("export: not mounted: exit status 32")}
	export := testExport(bound(simplyblockv1alpha2.NFSExportPhaseReady))
	r, cl := newExportReconciler(t, asm, export, mdsPod())

	reconcileExport(t, r)

	got := loadExport(t, cl)
	expectMDSReady(t, got, metav1.ConditionFalse, simplyblockv1alpha2.NFSExportReasonHealthCheckFailed)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady {
		t.Errorf("phase = %q, want it to stay Ready", got.Status.Phase)
	}
}

// Regression: 2026-11-12-pnfs-mds-stays-dead-after-state-disk-journal-abort.
// A metadata server that is down has no session, and an export behind it
// requeued without a word.
func TestAnUnreachableMDSTurnsMDSReadyFalse(t *testing.T) {
	asm := &fakeAssembler{noSessions: true}
	export := testExport(bound(simplyblockv1alpha2.NFSExportPhaseReady))
	r, cl := newExportReconciler(t, asm, export, mdsPod())

	reconcileExport(t, r)

	expectMDSReady(t, loadExport(t, cl), metav1.ConditionFalse, simplyblockv1alpha2.NFSExportReasonMDSUnreachable)
}

func TestAHealthyExportHasMDSReadyTrue(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(bound(simplyblockv1alpha2.NFSExportPhaseReady))
	r, cl := newExportReconciler(t, asm, export, mdsPod())

	reconcileExport(t, r)

	expectMDSReady(t, loadExport(t, cl), metav1.ConditionTrue, simplyblockv1alpha2.NFSExportReasonServing)
}

// The condition follows the metadata server back, so a fault that clears on
// its own leaves nothing for a human to undo.
func TestMDSReadyRecoversWithTheMDS(t *testing.T) {
	asm := &fakeAssembler{checkErr: errors.New("export: not mounted")}
	export := testExport(bound(simplyblockv1alpha2.NFSExportPhaseReady))
	r, cl := newExportReconciler(t, asm, export, mdsPod())

	reconcileExport(t, r)
	asm.checkErr = nil
	reconcileExport(t, r)

	expectMDSReady(t, loadExport(t, cl), metav1.ConditionTrue, simplyblockv1alpha2.NFSExportReasonServing)
}
