// An aborted VolumeMigration is retried like a failed one, but its target is
// not blamed: an abort is a decision (spec.abort, a cancel from elsewhere),
// not a verdict on the node the volume was heading to.

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func drainVM(name string, phase simplyblockv1alpha1.VolumeMigrationPhase, target, sourceNode string) *simplyblockv1alpha1.VolumeMigration {
	vm := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: opsTestNS, UID: types.UID("uid-" + name),
			Labels: map[string]string{"storage.simplyblock.io/drain-node": opsTestNodeUUID},
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{PVName: drainTestPVA, TargetNodeUUID: target},
	}
	vm.Status.Phase = phase
	vm.Status.SourceNodeUUID = sourceNode
	return vm
}

func TestHandleFailedVolumeMigrations_AnAbortIsRetriedWithoutBlamingTheTarget(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	aborted := drainVM("aborted", simplyblockv1alpha1.VolumeMigrationPhaseAborted, drainTestNode2, "node-src")
	failed := drainVM("failed", simplyblockv1alpha1.VolumeMigrationPhaseFailed, drainTestNode3, "node-src")
	r := newOpsReconciler(t, sn, ops, aborted, failed)

	// No StorageNodeSet/cluster CRs: the pause check cannot resolve them and
	// reports "not paused", which is the branch under test.
	_, handled := r.handleFailedVolumeMigrations(context.Background(), ops, nil,
		[]simplyblockv1alpha1.VolumeMigration{*aborted, *failed})
	if !handled {
		t.Fatal("two terminal CRs were not handled")
	}

	updated := reloadOps(t, r)
	got := drainTargetsTriedFor(updated, drainTestPVA)
	if len(got) != 1 || got[0] != drainTestNode3 {
		t.Errorf("exhausted targets = %v, want only node-3 (the failed one); the aborted "+
			"migration's node-2 must stay available", got)
	}

	var list simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(context.Background(), &list, client.InNamespace(opsTestNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("both CRs must be deleted for the drain to re-issue them, %d left", len(list.Items))
	}
}

func TestHandleFailedVolumeMigrations_NothingTerminalIsNotHandled(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	running := drainVM("running", simplyblockv1alpha1.VolumeMigrationPhaseRunning, drainTestNode2, "node-src")
	r := newOpsReconciler(t, sn, ops, running)
	if _, handled := r.handleFailedVolumeMigrations(context.Background(), ops, nil,
		[]simplyblockv1alpha1.VolumeMigration{*running}); handled {
		t.Error("a running migration was treated as terminal")
	}
}
