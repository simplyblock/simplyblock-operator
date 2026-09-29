// What an installation's stated configuration does to the run a fresh install
// raises by itself.
//
// The cases here are the half of the guard that reads the configuration. The
// other half, in bootstrap_test.go, asks whether anything already exists and is
// unaffected by any of this: an installation that states a selector and a name
// still declines behind a cluster that is already deployed.

package deployment

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/bootstrap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statedConfig is the ConfigMap the chart renders, carrying whatever document a
// case is about.
func statedConfig(document string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrap.ConfigMapName,
			Namespace: theNamespace,
		},
		Data: map[string]string{bootstrap.ConfigKey: document},
	}
}

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

// The managed profile renders one key, and this is what it is for: the cluster
// that manages this one raises the run, in the namespace it chooses, so the
// operator here raises none of its own.
func TestAManagedInstallationRaisesNoRunOfItsOwn(t *testing.T) {
	d := discoveryFor(t, labeledWorker("worker-1", nil), statedConfig("enabled: false\n"))

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var runs simplyblockv1alpha2.OperatorOpsList
	if err := d.List(context.Background(), &runs); err != nil {
		t.Fatalf("listing the runs: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Errorf("a managed installation raised %d run(s) of its own", len(runs.Items))
	}
}

// What the installation stated is what the run carries.
func TestTheRunIsWhatTheInstallationStated(t *testing.T) {
	d := discoveryFor(t,
		labeledWorker("worker-1", map[string]string{"storage": "yes"}),
		statedConfig(`
name: fleet-discovery
nodeSelector:
  storage: "yes"
tolerations:
  - key: storage
    operator: Equal
    value: dedicated
    effect: NoSchedule
deviceFilter:
  enableLogicalBlockDevices: true
  enablePartitionedDevices: false
  blockDenyList:
    - /dev/sda
draft:
  name: fleet-draft
`))

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := theRun(t, d)
	if run.Name != "fleet-discovery" {
		t.Errorf("the run is named %q, want the stated name", run.Name)
	}
	if run.Spec.Discover == nil {
		t.Fatal("the run carries no discover block")
	}
	if run.Spec.Discover.ConfigName != "fleet-draft" {
		t.Errorf("configName = %q, want the stated draft name", run.Spec.Discover.ConfigName)
	}
	if run.Spec.Discover.NodeSelector["storage"] != "yes" {
		t.Errorf("nodeSelector = %v, want the stated selector", run.Spec.Discover.NodeSelector)
	}
	if len(run.Spec.Discover.Tolerations) != 1 {
		t.Errorf("tolerations = %+v, want the stated one", run.Spec.Discover.Tolerations)
	}
	filter := run.Spec.Discover.DeviceFilter
	if filter == nil {
		t.Fatal("the stated device filter did not reach the run")
	}
	if !ptr.BoolFromOrFalse(filter.EnableLogicalBlockDevices) {
		t.Error("enableLogicalBlockDevices was dropped")
	}
	if ptr.BoolFromOrFalse(filter.EnablePartitionedDevices) {
		t.Error("the stated partition refusal was overridden by the default waiver")
	}
	if len(filter.BlockDenyList) != 1 {
		t.Errorf("blockDenyList = %v, want the boot disk", filter.BlockDenyList)
	}
}

// A stated namespace places the run, which places its probe Jobs, its reports,
// and the draft it writes.
func TestAStatedNamespacePlacesTheRun(t *testing.T) {
	d := discoveryFor(t, labeledWorker("worker-1", nil), statedConfig("namespace: storage\n"))

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run := theRun(t, d); run.Namespace != "storage" {
		t.Errorf("the run is in %q, want the stated namespace", run.Namespace)
	}
}

// The seed applies to this run and no other, so the run has to be recognizable as
// the one the installation configured.
//
// It is a label rather than a comparison against the configured name, because the
// reconciler that reads it would otherwise re-derive the bootstrap's decision from
// a ConfigMap that may have been edited in between, and disagree with it.
func TestTheInitialRunIsLabeledAsSuch(t *testing.T) {
	d := discoveryFor(t, labeledWorker("worker-1", nil), statedConfig("name: fleet-discovery\n"))

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run := theRun(t, d); run.Labels[InitialDiscoveryLabel] != "true" {
		t.Errorf("labels = %v, want the run marked as the initial one", run.Labels)
	}
}

// The guard asks whether there is a machine the run would inspect, so it has to
// ask about the machines the run it would raise is narrowed to.
//
// Without this it raises a run against a fleet the selector excludes entirely.
// That run fails, and a failed run is an object holding a finalizer: uninstalling
// the operator deletes its namespace and its Deployment together, so the
// controller that would release it can be gone before it sees the delete and the
// namespace stays Terminating.
func TestTheGuardAsksAboutTheWorkersTheRunWouldInspect(t *testing.T) {
	d := discoveryFor(t,
		labeledWorker("worker-1", map[string]string{"storage": "no"}),
		labeledWorker("worker-2", nil),
		statedConfig("nodeSelector:\n  storage: \"yes\"\n"),
	)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var runs simplyblockv1alpha2.OperatorOpsList
	if err := d.List(context.Background(), &runs); err != nil {
		t.Fatalf("listing the runs: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Error("a run was raised against a fleet its own selector excludes entirely")
	}
}

// A selector that matches is the ordinary case, and it must still raise the run.
func TestAMatchingSelectorStillRaisesTheRun(t *testing.T) {
	d := discoveryFor(t,
		labeledWorker("worker-1", map[string]string{"storage": "yes"}),
		statedConfig("nodeSelector:\n  storage: \"yes\"\n"),
	)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run := theRun(t, d); run.Spec.Discover.NodeSelector["storage"] != "yes" {
		t.Errorf("the run lost its selector: %+v", run.Spec.Discover)
	}
}

// The single-node development cluster: its one machine is the control-plane node,
// which the guard declines unless the installation asked for it. Asking is what
// this states, and the guard has to read it or the deployment it was configured
// for never starts.
func TestTheGuardHonorsTheControlPlaneOptIn(t *testing.T) {
	controlPlane := labeledWorker("kind-control-plane",
		map[string]string{"node-role.kubernetes.io/control-plane": ""})
	d := discoveryFor(t, controlPlane, statedConfig("enableControlPlaneNodes: true\n"))

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := theRun(t, d)
	if !ptr.BoolFromOrFalse(run.Spec.Discover.EnableControlPlaneNodes) {
		t.Error("the opt-in did not reach the run, so it will decline the machine it was raised for")
	}
}

// Content that cannot be read is the state an installation predating the chart is
// in, and it keeps that installation's behavior rather than stopping the run.
func TestAnUnreadableConfigMapStillRaisesTheOperatorsOwnRun(t *testing.T) {
	d := discoveryFor(t,
		labeledWorker("worker-1", nil),
		statedConfig("name: fleet\n  nodeSelector: [unbalanced\n"),
	)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := theRun(t, d)
	if run.Name != InitialDiscoveryName {
		t.Errorf("the run is named %q, want the operator's own constant", run.Name)
	}
	if !ptr.BoolFromOrFalse(run.Spec.Discover.DeviceFilter.EnablePartitionedDevices) {
		t.Error("unreadable content cost the fleet its partition waiver")
	}
}

// A read that failed is not evidence of anything, and the run is declined rather
// than raised against a cluster nobody could read.
func TestTheRunStillDeclinesBehindWhatAlreadyExists(t *testing.T) {
	d := discoveryFor(t,
		labeledWorker("worker-1", nil),
		statedConfig("name: fleet-discovery\n"),
		&simplyblockv1alpha2.ClusterDeploymentConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "written-by-hand", Namespace: theNamespace},
		},
	)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var runs simplyblockv1alpha2.OperatorOpsList
	if err := d.List(context.Background(), &runs); err != nil {
		t.Fatalf("listing the runs: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Error("a stated configuration overrode the guard")
	}
}
