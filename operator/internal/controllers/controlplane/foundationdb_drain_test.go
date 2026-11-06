package controlplane

import (
	"testing"

	policyv1 "k8s.io/api/policy/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A drain may take one log and one storage process at a time: the node it is
// draining holds one of each, and a second node's would cost a second copy.
func TestFoundationDBProcessesHaveADisruptionBudgetPerClass(t *testing.T) {
	budgets := map[string]*policyv1.PodDisruptionBudget{}
	for _, o := range foundationDBObjects(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{})) {
		if pdb, ok := o.(*policyv1.PodDisruptionBudget); ok {
			budgets[pdb.Spec.Selector.MatchLabels["foundationdb.org/fdb-process-class"]] = pdb
		}
	}
	for _, class := range []string{"log", "storage"} {
		pdb := budgets[class]
		if pdb == nil {
			t.Fatalf("no disruption budget for the %s processes", class)
		}
		if got := pdb.Spec.MaxUnavailable.IntValue(); got != 1 {
			t.Errorf("%s budget allows %d unavailable, want 1", class, got)
		}
		if pdb.Spec.Selector.MatchLabels["foundationdb.org/fdb-cluster-name"] != ComponentFDBCluster {
			t.Errorf("%s budget selects %v, not the cluster's pods", class, pdb.Spec.Selector.MatchLabels)
		}
	}
	if len(budgets) != 2 {
		t.Errorf("got budgets for %d classes, want 2", len(budgets))
	}
}
