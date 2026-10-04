// Tests for the in-flight checks. The design calls these the ones that matter
// most, and the failure that matters is the false negative: a migration that
// starts while an operation is running rewrites the object the operation is
// keeping its state in.

package check

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

func nodeOpsIn(name string, phase simplyblockv1alpha1.StorageNodeOpsPhase) *simplyblockv1alpha1.StorageNodeOps {
	ops := &simplyblockv1alpha1.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
	}
	ops.Status.Phase = phase
	return ops
}

func migrationAtPhase(name string, phase simplyblockv1alpha1.VolumeMigrationPhase) *simplyblockv1alpha1.VolumeMigration {
	migration := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
	}
	migration.Status.Phase = phase
	return migration
}

func TestInFlight_ARunningNodeOperationRefuses(t *testing.T) {
	scope := scopeOver(t, nodeOpsIn("restart", simplyblockv1alpha1.StorageNodeOpsPhaseRunning))

	findings := runCheck(t, InFlight(), IDOperationsInFlight, scope)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1:\n%v", len(findings), findings)
	}
	if !strings.Contains(findings[0].String(), "phase Running") {
		t.Errorf("the finding does not say what phase it is in:\n%s", findings[0])
	}
}

func TestInFlight_APendingOperationRefusesToo(t *testing.T) {
	// Pending is an operation the operator is about to start, which is the
	// same problem arriving a moment later.
	scope := scopeOver(t, nodeOpsIn("restart", simplyblockv1alpha1.StorageNodeOpsPhasePending))

	if findings := runCheck(t, InFlight(), IDOperationsInFlight, scope); len(findings) != 1 {
		t.Fatalf("a pending operation did not refuse:\n%v", findings)
	}
}

func TestInFlight_AFinishedOperationIsFine(t *testing.T) {
	scope := scopeOver(t,
		nodeOpsIn("done", simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded),
		nodeOpsIn("broke", simplyblockv1alpha1.StorageNodeOpsPhaseFailed),
		migrationAtPhase("moved", simplyblockv1alpha1.VolumeMigrationPhaseCompleted),
		migrationAtPhase("stopped", simplyblockv1alpha1.VolumeMigrationPhaseAborted),
	)

	if findings := runCheck(t, InFlight(), IDOperationsInFlight, scope); len(findings) != 0 {
		t.Fatalf("finished operations refused the upgrade:\n%v", findings)
	}
}

func TestInFlight_AnObjectWithNoPhaseYetRefuses(t *testing.T) {
	// The operator has not reached it, so it is about to start rather than
	// finished, and treating an empty phase as terminal is how a migration
	// starts underneath one.
	scope := scopeOver(t, nodeOpsIn("fresh", ""))

	if findings := runCheck(t, InFlight(), IDOperationsInFlight, scope); len(findings) != 1 {
		t.Fatalf("an operation the operator has not started did not refuse:\n%v", findings)
	}
}

func TestInFlight_ADrainInProgressRefuses(t *testing.T) {
	set := nodeSet("set-a", "cluster-a")
	set.Status.DrainCoordination = []simplyblockv1alpha1.NodeDrainState{
		{Hostname: "worker-1", Phase: "draining"},
		{Hostname: "worker-2", Phase: "complete"},
	}
	scope := scopeOver(t, set)

	findings := runCheck(t, InFlight(), IDOperationsInFlight, scope)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want the one worker still draining:\n%v", len(findings), findings)
	}
	if !strings.Contains(findings[0].String(), "worker-1") {
		t.Errorf("the finding does not name the worker:\n%s", findings[0])
	}
}

func TestInFlight_TheDrainPhasesAreMatchedInTheirOwnSpelling(t *testing.T) {
	// The drain workflow spells its phases in lower case with underscores,
	// where the Ops kinds use title case. A terminal check that only knew one
	// spelling would refuse on every finished drain.
	set := nodeSet("set-a", "cluster-a")
	set.Status.DrainCoordination = []simplyblockv1alpha1.NodeDrainState{
		{Hostname: "worker-1", Phase: "complete"},
		{Hostname: "worker-2", Phase: "failed"},
	}

	if findings := runCheck(t, InFlight(), IDOperationsInFlight, scopeOver(t, set)); len(findings) != 0 {
		t.Fatalf("a finished drain refused the upgrade:\n%v", findings)
	}
}

func TestOnline_AnOfflineNodeRefuses(t *testing.T) {
	node := storageNode("node-1", "set-a", ownedBySet("set-a", true))
	node.Status.Status = "offline"

	findings := runCheck(t, InFlight(), IDNodesOnline, scopeOver(t, node))
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1:\n%v", len(findings), findings)
	}
	if !findings[0].Error() {
		t.Errorf("severity = %q, want an error by default", findings[0].Severity)
	}
}

func TestOnline_AcknowledgingDowngradesItToAWarning(t *testing.T) {
	// An operator who knows a node is down and intends to upgrade anyway is
	// making a legitimate call, and the finding stays in the report so the
	// call is visible afterward.
	node := storageNode("node-1", "set-a", ownedBySet("set-a", true))
	node.Status.Status = "offline"

	scope := scopeOver(t, node)
	scope.Options.AcknowledgeOffline = true

	findings := runCheck(t, InFlight(), IDNodesOnline, scope)
	if len(findings) != 1 {
		t.Fatalf("acknowledging removed the finding rather than downgrading it:\n%v", findings)
	}
	if findings[0].Error() {
		t.Errorf("severity = %q, want a warning once acknowledged", findings[0].Severity)
	}
	if findings.Blocked() {
		t.Error("an acknowledged offline node still blocked the upgrade")
	}
}

func TestOnline_AnOnlineNodeIsFine(t *testing.T) {
	scope := scopeOver(t, storageNode("node-1", "set-a", ownedBySet("set-a", true)))

	if findings := runCheck(t, InFlight(), IDNodesOnline, scope); len(findings) != 0 {
		t.Fatalf("an online node was reported:\n%v", findings)
	}
}

func TestOnline_AStatusTheOperatorHasNotWrittenIsReported(t *testing.T) {
	node := storageNode("node-1", "set-a", ownedBySet("set-a", true))
	node.Status.Status = ""

	findings := runCheck(t, InFlight(), IDNodesOnline, scopeOver(t, node))
	if len(findings) != 1 || !strings.Contains(findings[0].String(), "unreported") {
		t.Fatalf("a node with no status was not reported as such:\n%v", findings)
	}
}

func TestInFlight_AnEmptyClusterProducesNothing(t *testing.T) {
	scope := scopeOver(t)

	for _, id := range []upgrade.ID{IDOperationsInFlight, IDNodesOnline} {
		if findings := runCheck(t, InFlight(), id, scope); len(findings) != 0 {
			t.Errorf("%s produced %d findings on an empty cluster:\n%v", id, len(findings), findings)
		}
	}
}
