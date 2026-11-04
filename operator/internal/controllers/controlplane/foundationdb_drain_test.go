package controlplane

import (
	"slices"
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A cordon must make the FoundationDB operator replace the node's processes
// straight away, which is what lets kubectl drain finish without the database
// losing a process it still needs.
func TestFoundationDBReplacesProcessesOnACordonedNode(t *testing.T) {
	cluster := foundationDBCluster(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))

	options, found, err := unstructured.NestedSlice(cluster.Object,
		"spec", "automationOptions", "replacements", "taintReplacementOptions")
	if err != nil || !found || len(options) != 1 {
		t.Fatalf("taintReplacementOptions = %v (found=%v, err=%v), want the cordon taint", options, found, err)
	}
	option := options[0].(map[string]any)
	if option["key"] != cordonTaint || option["durationInSeconds"] != int64(0) {
		t.Errorf("the taint option is %v, want %s after 0s", option, cordonTaint)
	}
	if after, _, _ := unstructured.NestedInt64(cluster.Object,
		"spec", "automationOptions", "replacements", "taintReplacementTimeSeconds"); after != 0 {
		t.Errorf("a tainted process waits %ds before it is replaced", after)
	}
	if enabled, _, _ := unstructured.NestedBool(cluster.Object,
		"spec", "automationOptions", "replacements", "enabled"); !enabled {
		t.Error("replacements are off, so the taint option does nothing")
	}
}

// The budget refuses every eviction of a FoundationDB pod, and it is part of
// the step that writes the cluster.
func TestFoundationDBPodsCannotBeEvicted(t *testing.T) {
	cp := aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{})

	var budget *policyv1.PodDisruptionBudget
	for _, obj := range foundationDBObjects(cp) {
		if b, ok := obj.(*policyv1.PodDisruptionBudget); ok {
			budget = b
		}
	}
	if budget == nil {
		t.Fatal("the FoundationDB step writes no PodDisruptionBudget")
	}
	if budget.Namespace != cp.Namespace {
		t.Errorf("the budget is in %q, the database in %q", budget.Namespace, cp.Namespace)
	}
	if budget.Spec.MaxUnavailable == nil || budget.Spec.MaxUnavailable.IntValue() != 0 || budget.Spec.MinAvailable != nil {
		t.Errorf("the budget allows disruptions: %+v", budget.Spec)
	}
	if got := budget.Spec.Selector.MatchLabels["foundationdb.org/fdb-cluster-name"]; got != ComponentFDBCluster {
		t.Errorf("the budget selects cluster %q, want %q", got, ComponentFDBCluster)
	}
}

// The operator waits less than its default 120 s after a recovery before it
// excludes a process, so a node's replacements fit an EKS drain.
func TestFoundationDBOperatorExcludesSoonAfterARecovery(t *testing.T) {
	deployment := fdbOperatorDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	want := "--minimum-recovery-time-for-exclusion=30"
	if !slices.Contains(deployment.Spec.Template.Spec.Containers[0].Args, want) {
		t.Errorf("the FoundationDB operator runs with %v, want %s",
			deployment.Spec.Template.Spec.Containers[0].Args, want)
	}
}

// A cordon reaches the operator only if it watches nodes, which takes the
// label its pods carry the cluster name under.
func TestFoundationDBOperatorReconcilesOnNodeTaints(t *testing.T) {
	deployment := fdbOperatorDeployment(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	want := "--cluster-label-key-for-node-trigger=foundationdb.org/fdb-cluster-name"
	if !slices.Contains(deployment.Spec.Template.Spec.Containers[0].Args, want) {
		t.Errorf("the FoundationDB operator runs with %v, so a cordon never reaches it",
			deployment.Spec.Template.Spec.Containers[0].Args)
	}
}
