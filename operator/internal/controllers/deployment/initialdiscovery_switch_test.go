// What decides whether a fresh install raises its discovery run, and with what,
// now that no ConfigMap does.
//
// The switch and the spec are the operator's own settings, which the chart sets
// per profile. A ConfigMap left behind by an earlier release is not read, so it
// can neither suppress the run nor change it.

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

// What an installation states about the run is what the run carries, with the
// partition waiver it would have had anyway.
func TestTheRunCarriesWhatTheInstallationStated(t *testing.T) {
	spec, err := ParseDiscoverSpec(`{"nodeSelector":{"tier":"storage"},"enableControlPlaneNodes":true,` +
		`"deviceFilter":{"pcieDenyList":["0000:00:1f.2"]}}`)
	if err != nil {
		t.Fatalf("ParseDiscoverSpec: %v", err)
	}
	d := discoveryFor(t, labeledWorker("worker-1", map[string]string{"tier": "storage"}))
	d.Spec = spec

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := theRun(t, d).Spec.Discover
	if got.NodeSelector["tier"] != "storage" || !ptr.BoolFromOrFalse(got.EnableControlPlaneNodes) ||
		got.DeviceFilter == nil || got.DeviceFilter.PcieDenyList[0] != "0000:00:1f.2" {
		t.Errorf("the run lost what the installation stated: %+v", got)
	}
	if !ptr.BoolFromOrFalse(got.EnablePartitionedDevices) {
		t.Error("stating other settings dropped the partition waiver")
	}
}

// A selector that matches no worker is a run that would fail, and a failed run
// holds a finalizer, so it is not raised.
func TestASelectorMatchingNothingRaisesNoRun(t *testing.T) {
	d := discoveryFor(t, labeledWorker("worker-1", map[string]string{"tier": "compute"}))
	d.Spec = &simplyblockv1alpha2.DiscoverSpec{NodeSelector: map[string]string{"tier": "storage"}}

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if raised(t, d) {
		t.Error("a run was raised against a selector that matches no worker")
	}
}

// The only machine of a single-node cluster is its control-plane node, which the
// run considers only when the installation said so.
func TestAControlPlaneNodeCountsOnlyWhenOptedIn(t *testing.T) {
	controlPlane := map[string]string{"node-role.kubernetes.io/control-plane": ""}

	d := discoveryFor(t, labeledWorker("cp-1", controlPlane))
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if raised(t, d) {
		t.Error("a run was raised for a cluster whose only machine is a control-plane node")
	}

	d = discoveryFor(t, labeledWorker("cp-1", controlPlane))
	d.Spec = &simplyblockv1alpha2.DiscoverSpec{EnableControlPlaneNodes: ptr.To(true)}
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !raised(t, d) {
		t.Error("the opt-in did not make the control-plane node count")
	}
}

// The document is the chart's rendering of discovery.*, so it is read strictly: a
// field this operator does not know is a typo, and a run silently missing the
// setting it was meant to carry is worse than no run.
func TestParseDiscoverSpec(t *testing.T) {
	spec, err := ParseDiscoverSpec("")
	if err != nil || !ptr.BoolFromOrFalse(spec.EnablePartitionedDevices) {
		t.Errorf("an empty document is not the default run: %+v, %v", spec, err)
	}

	spec, err = ParseDiscoverSpec(`{"enablePartitionedDevices":false}`)
	if err != nil || ptr.BoolFromOrFalse(spec.EnablePartitionedDevices) {
		t.Errorf("an explicit false was overridden: %+v, %v", spec, err)
	}

	if _, err = ParseDiscoverSpec(`{"nodeSelectr":{"a":"b"}}`); err == nil {
		t.Error("an unknown field was accepted")
	}
	if _, err = ParseDiscoverSpec(`{`); err == nil {
		t.Error("a malformed document was accepted")
	}
}
