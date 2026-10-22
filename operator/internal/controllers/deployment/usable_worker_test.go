// Tests for UsableWorker against tainted machines: a discovery run's
// tolerations are what let it probe a dedicated, tainted storage plane, so a
// taint the run tolerates must not exclude the machine. Lives beside the
// predicate it pins.
package deployment

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func taintedWorker(taints ...corev1.Taint) corev1.Node {
	return corev1.Node{Spec: corev1.NodeSpec{Taints: taints}}
}

var dedicatedStorage = corev1.Taint{
	Key: "storage.simplyblock.io/dedicated", Value: "true", Effect: corev1.TaintEffectNoSchedule,
}

func TestATaintTheRunToleratesDoesNotExcludeTheWorker(t *testing.T) {
	node := taintedWorker(dedicatedStorage)
	tolerations := []corev1.Toleration{{
		Key: "storage.simplyblock.io/dedicated", Operator: corev1.TolerationOpEqual,
		Value: "true", Effect: corev1.TaintEffectNoSchedule,
	}}
	if !UsableWorker(node, false, tolerations) {
		t.Error("a dedicated storage worker the run tolerates was excluded; " +
			"discovery against a tainted storage plane finds nothing")
	}
}

func TestAnExistsTolerationCoversEveryValueOfItsKey(t *testing.T) {
	node := taintedWorker(dedicatedStorage)
	tolerations := []corev1.Toleration{{Key: "storage.simplyblock.io/dedicated", Operator: corev1.TolerationOpExists}}
	if !UsableWorker(node, false, tolerations) {
		t.Error("an Exists toleration of the key did not cover the taint")
	}
}

func TestATaintTheRunDoesNotTolerateStillExcludesTheWorker(t *testing.T) {
	other := corev1.Taint{Key: "simplyblock.io/role", Value: "client", Effect: corev1.TaintEffectNoSchedule}
	node := taintedWorker(dedicatedStorage, other)
	tolerations := []corev1.Toleration{{
		Key: "storage.simplyblock.io/dedicated", Operator: corev1.TolerationOpEqual,
		Value: "true", Effect: corev1.TaintEffectNoSchedule,
	}}
	if UsableWorker(node, false, tolerations) {
		t.Error("a worker with a taint the run does not tolerate was used")
	}
	if reason := unschedulableReason(node, tolerations); !strings.Contains(reason, "simplyblock.io/role=client") {
		t.Errorf("reason %q does not name the untolerated taint", reason)
	}
	if UsableWorker(taintedWorker(dedicatedStorage), false, nil) {
		t.Error("a run with no tolerations used a tainted worker")
	}
}

func TestACordonedWorkerIsExcludedWhateverItTolerates(t *testing.T) {
	node := taintedWorker()
	node.Spec.Unschedulable = true
	if UsableWorker(node, false, []corev1.Toleration{{Operator: corev1.TolerationOpExists}}) {
		t.Error("a cordoned worker was used")
	}
}
