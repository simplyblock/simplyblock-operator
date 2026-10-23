// What decides whether a fresh install raises its discovery run, now that no
// ConfigMap does.
//
// The switch is the operator's own setting, which the chart sets per profile. A
// ConfigMap left behind by an earlier release is not read, so it can neither
// suppress the run nor change it.

package deployment

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// labeledWorker is a machine a run would inspect, optionally labeled.
func labeledWorker(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// theRun reads the one run in the namespace, whatever it was named.
func theRun(t *testing.T, d *InitialDiscovery) *simplyblockv1alpha2.OperatorOps {
	t.Helper()
	var runs simplyblockv1alpha2.OperatorOpsList
	if err := d.List(context.Background(), &runs); err != nil {
		t.Fatalf("listing the runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("found %d runs, want the one", len(runs.Items))
	}
	return &runs.Items[0]
}

// A managed or empty installation turns the check off, and it raises nothing.
func TestADisabledCheckRaisesNoRun(t *testing.T) {
	d := discoveryFor(t, labeledWorker("worker-1", nil))
	d.Disabled = true

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if raised(t, d) {
		t.Error("a disabled check raised the run")
	}
}

// The ConfigMap earlier releases rendered is no longer an input. One that still
// says `enabled: false` is what an upgrade leaves for a moment, and it must not
// stop the run an installation with nothing in it is owed.
func TestAStaleBootstrapConfigMapDoesNotDecideTheRun(t *testing.T) {
	stale := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-bootstrap", Namespace: theNamespace},
		Data:       map[string]string{"bootstrap.yaml": "enabled: false\nname: other\n"},
	}
	d := discoveryFor(t, labeledWorker("worker-1", nil), stale)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !raised(t, d) {
		t.Error("the run named in the code was not raised: the stale ConfigMap still decides")
	}
}

// With nothing stated the run narrows nothing and waives a partition table, which
// is what an installation that decided nothing always got.
func TestTheRunIsTheDefaultOne(t *testing.T) {
	d := discoveryFor(t, labeledWorker("worker-1", nil))

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := theRun(t, d)
	if run.Name != InitialDiscoveryName || run.Namespace != theNamespace {
		t.Errorf("the run is %s/%s, want %s/%s", run.Namespace, run.Name, theNamespace, InitialDiscoveryName)
	}
	spec := run.Spec.Discover
	if !ptr.BoolFromOrFalse(spec.EnablePartitionedDevices) {
		t.Error("the default run lost its partition waiver")
	}
	if len(spec.NodeSelector) != 0 || len(spec.Tolerations) != 0 || spec.DeviceFilter != nil {
		t.Errorf("the default run narrows something: %+v", spec)
	}
}
