package controlplane

import (
	"slices"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The operator waits less than its default 120 s after a recovery before it
// excludes a process, so the processes a drain replaces fit an EKS drain.
func TestFoundationDBOperatorExcludesSoonAfterARecovery(t *testing.T) {
	deployment := fdbOperatorDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	want := "--minimum-recovery-time-for-exclusion=30"
	if !slices.Contains(deployment.Spec.Template.Spec.Containers[0].Args, want) {
		t.Errorf("the FoundationDB operator runs with %v, want %s",
			deployment.Spec.Template.Spec.Containers[0].Args, want)
	}
}
