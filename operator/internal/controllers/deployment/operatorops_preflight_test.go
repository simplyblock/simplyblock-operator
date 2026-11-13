// Tests that a discovery run records its preflight findings in its status, so
// that they outlive the hour an event lives for, and that a waiver is recorded
// there as well.

package deployment

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// reportWithMTU is a probe report whose one interface, eth0, carries the node's
// address at the MTU given, so that it is the storage network of the draft.
func reportWithMTU(t *testing.T, node string, mtu int) *corev1.ConfigMap {
	t.Helper()
	cm := reportConfigMap(t, node, "0000:5e:00.0", "0000:5f:00.0")
	report, err := nodeprobe.ReportFromConfigMap(cm)
	if err != nil {
		t.Fatalf("read the report back: %v", err)
	}
	report.Interfaces = []nodeprobe.Interface{{
		Name: "eth0", MTU: mtu, State: "up", SpeedMbps: 25000,
		Driver: "ice", PCIAddress: "0000:3b:00.0", NUMANode: 0,
		Addresses: []string{"10.0.0.10/24"},
	}}
	cm, err = nodeprobe.ConfigMap(opsNamespace, opsName, nil, report)
	if err != nil {
		t.Fatalf("render the report ConfigMap: %v", err)
	}
	return cm
}

// runWithMTUs drives a discovery run to its end over workers whose storage
// interface carries the MTUs given, in worker order.
func runWithMTUs(t *testing.T, spec *simplyblockv1alpha2.DiscoverSpec, mtus ...int) *simplyblockv1alpha2.OperatorOps {
	t.Helper()
	names := []string{"worker-1", "worker-2", "worker-3"}[:len(mtus)]
	objects := []client.Object{discoverRun(spec)}
	for _, name := range names {
		objects = append(objects, worker(name))
	}
	r := newRunner(t, objects...)
	r.step() // start
	r.step() // inspect
	r.step() // probing: creates the Jobs
	for i, name := range names {
		if err := r.client.Create(context.Background(), reportWithMTU(t, name, mtus[i])); err != nil {
			t.Fatalf("write a report: %v", err)
		}
	}
	r.step() // probing: sees the reports
	_, ops := r.step()
	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseSucceeded {
		t.Fatalf("the run is %q: %s", ops.Status.Phase, ops.Status.Message)
	}
	return ops
}

func TestARunRecordsItsFindingsInItsStatus(t *testing.T) {
	ops := runWithMTUs(t, nil, 9000, 9000, 1500)

	var fails []simplyblockv1alpha2.PreflightFinding
	for _, found := range ops.Status.Findings {
		if found.Severity == simplyblockv1alpha2.PreflightSeverityFail {
			fails = append(fails, found)
		}
	}
	if len(fails) != 1 || fails[0].Node != "worker-3" || fails[0].Reason != discoverypkg.StorageMTUMismatch {
		t.Fatalf("the status findings are %+v, want one Fail StorageMTUMismatch on worker-3", ops.Status.Findings)
	}
	if ops.Status.Findings[0].Severity != simplyblockv1alpha2.PreflightSeverityFail {
		t.Errorf("the first finding is %q, want Fail findings ordered first", ops.Status.Findings[0].Severity)
	}
	if ops.Status.FailCount != 1 {
		t.Errorf("failCount is %d, want 1", ops.Status.FailCount)
	}
	condition := meta.FindStatusCondition(ops.Status.Conditions, simplyblockv1alpha2.OperatorOpsConditionPreflight)
	if condition == nil || condition.Status != "False" {
		t.Errorf("the Preflight condition is %+v, want False", condition)
	}
}

func TestAWaivedFindingStaysInTheStatusAndStopsCounting(t *testing.T) {
	ops := runWithMTUs(t, &simplyblockv1alpha2.DiscoverSpec{
		IgnoreFindings: []string{discoverypkg.StorageMTUMismatch},
	}, 9000, 9000, 1500)

	if len(ops.Status.Findings) == 0 || !ops.Status.Findings[0].Waived {
		t.Fatalf("the status findings are %+v, want the MTU finding kept and marked waived", ops.Status.Findings)
	}
	if ops.Status.FailCount != 0 {
		t.Errorf("failCount is %d, want 0 once the only Fail is waived", ops.Status.FailCount)
	}
	condition := meta.FindStatusCondition(ops.Status.Conditions, simplyblockv1alpha2.OperatorOpsConditionPreflight)
	if condition == nil || condition.Status != "True" {
		t.Errorf("the Preflight condition is %+v, want True", condition)
	}
}

func TestACleanFleetPassesPreflight(t *testing.T) {
	ops := runWithMTUs(t, nil, 9000, 9000)

	if ops.Status.FailCount != 0 {
		t.Errorf("failCount is %d for a fleet that agrees", ops.Status.FailCount)
	}
	condition := meta.FindStatusCondition(ops.Status.Conditions, simplyblockv1alpha2.OperatorOpsConditionPreflight)
	if condition == nil || condition.Status != "True" {
		t.Errorf("the Preflight condition is %+v, want True", condition)
	}
}

func TestAWaiverAddedAfterTheRunIsRecorded(t *testing.T) {
	ops := runWithMTUs(t, nil, 9000, 9000, 1500)
	ops.Spec.Discover = &simplyblockv1alpha2.DiscoverSpec{
		IgnoreFindings: []string{discoverypkg.StorageMTUMismatch},
	}

	changed := refreshWaivers(ops)
	if !changed || ops.Status.FailCount != 0 || !ops.Status.Findings[0].Waived {
		t.Errorf("after the waiver the status is %+v, want the finding waived and nothing failing", ops.Status)
	}
	if refreshWaivers(ops) {
		t.Error("a second refresh with nothing new reports a change")
	}
}
