// Tests for the preflight gate on approval: a draft written by a discovery run
// with a Fail finding is not approved until the finding is fixed or waived.

package webhook

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const testRun = "discover-1"

// writtenBy marks a draft as written by the named discovery run.
func writtenBy(config *simplyblockv1alpha2.ClusterDeploymentConfig, run string) *simplyblockv1alpha2.ClusterDeploymentConfig {
	config.Labels = map[string]string{nodeprobe.LabelRun: run}
	return config
}

// runFinding is a completed discovery run whose status carries one finding.
func runFinding(severity simplyblockv1alpha2.PreflightSeverity, ignore ...string) *simplyblockv1alpha2.OperatorOps {
	return &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{Name: testRun, Namespace: testDeploymentNamespace},
		Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action:   simplyblockv1alpha2.OperatorOpsActionDiscover,
			Discover: &simplyblockv1alpha2.DiscoverSpec{IgnoreFindings: ignore},
		},
		Status: simplyblockv1alpha2.OperatorOpsStatus{
			Phase: simplyblockv1alpha2.OperatorOpsPhaseSucceeded,
			Findings: []simplyblockv1alpha2.PreflightFinding{{
				Node: "worker-1", Severity: severity, Reason: "StorageMTUMismatch",
				Message: "worker-1 carries MTU 1500 on ens1f0", Remedy: "set the MTU to 9000",
			}},
		},
	}
}

func preflightExisting(run *simplyblockv1alpha2.OperatorOps) []client.Object {
	return []client.Object{testWorker("worker-1"), run}
}

func TestApprovingADraftWithAFailFindingIsRefused(t *testing.T) {
	config := writtenBy(testConfig(), testRun)
	mustDeny(t, approve(t, preflightExisting(runFinding(simplyblockv1alpha2.PreflightSeverityFail)), config),
		"StorageMTUMismatch", "worker-1", "ignoreFindings")
}

func TestApprovingADraftWithAWaivedFailIsAdmitted(t *testing.T) {
	config := writtenBy(testConfig(), testRun)
	mustAllow(t, approve(t, preflightExisting(
		runFinding(simplyblockv1alpha2.PreflightSeverityFail, "StorageMTUMismatch")), config))
}

func TestApprovingADraftWithOnlyWarningsIsAdmitted(t *testing.T) {
	config := writtenBy(testConfig(), testRun)
	mustAllow(t, approve(t, preflightExisting(runFinding(simplyblockv1alpha2.PreflightSeverityWarn)), config))
}

func TestApprovingADraftWhoseRunIsGoneIsAdmitted(t *testing.T) {
	// A run deleted after review leaves nothing to gate on, and a hand-written
	// document never had one.
	config := writtenBy(testConfig(), testRun)
	mustAllow(t, approve(t, []client.Object{testWorker("worker-1")}, config))
}
