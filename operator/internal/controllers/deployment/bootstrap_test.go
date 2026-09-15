// What a fresh install does by itself, and the three things that stop it.
//
// The guard is the whole design here, so every case below is one of its
// questions. Probing creates a Job on every worker, so a run that fired on an
// install that already had something would put a Job on every node of a deployed
// fleet each time the operator was upgraded.

package deployment

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

func discoveryFor(t *testing.T, objects ...client.Object) *InitialDiscovery {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	return &InitialDiscovery{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Namespace: theNamespace,
	}
}

// raised reports whether the run exists, which is what every case here asserts.
func raised(t *testing.T, d *InitialDiscovery) bool {
	t.Helper()
	var run simplyblockv1alpha2.OperatorOps
	key := client.ObjectKey{Namespace: theNamespace, Name: InitialDiscoveryName}
	err := d.Get(context.Background(), key, &run)
	return err == nil
}

// An install with nothing in it is the one state this exists for.
func TestAFreshInstallRaisesOneDiscoveryRun(t *testing.T) {
	d := discoveryFor(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !raised(t, d) {
		t.Fatal("a fresh install raised no discovery run")
	}

	var run simplyblockv1alpha2.OperatorOps
	key := client.ObjectKey{Namespace: theNamespace, Name: InitialDiscoveryName}
	if err := d.Get(context.Background(), key, &run); err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if run.Spec.Action != simplyblockv1alpha2.OperatorOpsActionDiscover {
		t.Errorf("action = %q, want Discover", run.Spec.Action)
	}
	// The run states no filter and no selector: what it produces is a draft of
	// everything the fleet has, which is what a reviewer narrows.
	if run.Spec.Discover == nil {
		t.Error("the run carries no discover block")
	} else if len(run.Spec.Discover.NodeSelector) != 0 ||
		run.Spec.Discover.DeviceFilter != nil {
		t.Errorf("the run guessed at a filter: %+v", run.Spec.Discover)
	}
}

// Each of the three questions is enough on its own to decline.
func TestAPreviousResultDeclinesTheRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing client.Object
	}{
		{
			// A terminal run is a previous result, and so is a failed one: the
			// administrator has seen the answer either way.
			name: "an operator operation has already run",
			existing: &simplyblockv1alpha2.OperatorOps{
				ObjectMeta: metav1.ObjectMeta{Name: "earlier", Namespace: theNamespace},
				Spec: simplyblockv1alpha2.OperatorOpsSpec{
					Action: simplyblockv1alpha2.OperatorOpsActionDiscover,
				},
				Status: simplyblockv1alpha2.OperatorOpsStatus{
					Phase: simplyblockv1alpha2.OperatorOpsPhaseFailed,
				},
			},
		},
		{
			name: "a deployment config already exists",
			existing: &simplyblockv1alpha2.ClusterDeploymentConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "written-by-hand", Namespace: theNamespace},
			},
		},
		{
			name:     "a cluster is already deployed",
			existing: aCluster(nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := discoveryFor(t, tc.existing)

			if err := d.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if raised(t, d) {
				t.Errorf("a run was raised even though %s", tc.name)
			}
		})
	}
}

// The check runs on every operator start, and a restart must not raise a second
// run against a fleet the first one already reported on.
func TestARestartRaisesNothingFurther(t *testing.T) {
	d := discoveryFor(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	for pass := 0; pass < 3; pass++ {
		if err := d.Start(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	var runs simplyblockv1alpha2.OperatorOpsList
	if err := d.List(context.Background(), &runs); err != nil {
		t.Fatalf("listing the runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("three starts produced %d runs, want the one", len(runs.Items))
	}
}

// An administrator who does not want the run says so by writing an object with
// that name, which the guard then finds and declines behind.
func TestAnObjectByThatNameIsNotReplaced(t *testing.T) {
	theirs := &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:        InitialDiscoveryName,
			Namespace:   theNamespace,
			Annotations: map[string]string{"theirs": "true"},
		},
		Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action: simplyblockv1alpha2.OperatorOpsActionDiscover,
		},
	}
	d := discoveryFor(t, theirs)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var run simplyblockv1alpha2.OperatorOps
	key := client.ObjectKey{Namespace: theNamespace, Name: InitialDiscoveryName}
	if err := d.Get(context.Background(), key, &run); err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if run.Annotations["theirs"] != "true" {
		t.Error("the administrator's own object was overwritten")
	}
}

// One replica asks, because the three reads are not idempotent the way the create
// is: two replicas racing would both see an empty cluster and both decide to run.
func TestTheCheckIsLeaderElected(t *testing.T) {
	if !(&InitialDiscovery{}).NeedLeaderElection() {
		t.Error("the check runs on every replica, so the reads race")
	}
}

// A cluster with nothing a discovery run would inspect is the fourth question,
// and it is the one with teeth beyond convenience.
//
// A run raised against such a cluster fails, and a failed run is still an object
// carrying a finalizer. Uninstalling the operator deletes its namespace and its
// Deployment together, so the controller that would release that finalizer can be
// gone before it sees the delete, and the namespace stays Terminating. An install
// that raises a run it knows cannot succeed has therefore made its own uninstall
// conditional on timing.
//
// The single-node development cluster is exactly this shape: its one machine is
// the control-plane node, which nothing places storage on unless somebody asks.
func TestAClusterWithNothingToInspectRaisesNoRun(t *testing.T) {
	controlPlane := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "kind-control-plane",
		Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}}
	d := discoveryFor(t, controlPlane)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if raised(t, d) {
		t.Error("a run was raised against a cluster whose only node holds no storage")
	}
}

// The same cluster once somebody has asked for its control-plane node is not that
// case: there is a machine to inspect, so the run is worth raising.
//
// The bootstrap does not set the opt-in, so this asserts the reason rather than
// the outcome: the question the guard asks is whether a machine exists at all,
// and it must not answer it by reading a flag nobody set.
func TestAWorkerIsEnoughToRaiseTheRun(t *testing.T) {
	d := discoveryFor(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !raised(t, d) {
		t.Error("a cluster with a worker in it raised no run")
	}
}

// An unschedulable machine is not one a run would inspect either, so a fleet
// cordoned for maintenance is the empty case rather than the worker case.
func TestACordonedFleetRaisesNoRun(t *testing.T) {
	cordoned := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	d := discoveryFor(t, cordoned)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if raised(t, d) {
		t.Error("a run was raised against a fleet with no schedulable machine")
	}
}
