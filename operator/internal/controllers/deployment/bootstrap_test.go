// What a fresh install does by itself, and the three things that stop it.
//
// The guard is the whole design here, so every case below is one of its
// questions. Probing creates a Job on every worker, so a run that fired on an
// install that already had something would put a Job on every node of a deployed
// fleet each time the operator was upgraded.

package deployment

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// discoveryRefusingCreates builds the check against a client that answers the
// first refusals writes get while an admission webhook is unreachable, and
// admits the write after that. It is how the startup window is reproduced
// without an apiserver: what the API server returns in that window is an
// Internal error naming the webhook it could not call.
func discoveryRefusingCreates(t *testing.T, refusals int, objects ...client.Object) (*InitialDiscovery, *int) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	attempts := 0
	inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	refusing := interceptor.NewClient(inner, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			attempts++
			if attempts <= refusals {
				return apierrors.NewInternalError(fmt.Errorf(
					`failed calling webhook "voperatorops.simplyblock.io": failed to call webhook: ` +
						`Post "https://simplyblock-operator-webhook-service.simplyblock.svc:443/` +
						`validate-storage-simplyblock-io-v1alpha2-operatorops?timeout=10s": ` +
						`dial tcp 10.130.2.137:9443: connect: connection refused`))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	return &InitialDiscovery{Client: refusing, Namespace: theNamespace}, &attempts
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
	// The run narrows nothing: what it produces is a draft of everything the
	// fleet has, which is what a reviewer narrows. The partition waiver it does
	// state is the opposite of a guess at which disks somebody meant — it
	// admits more rather than less, and what it admits is the ordinary state of
	// a machine that has held data before.
	if run.Spec.Discover == nil {
		t.Fatal("the run carries no discover block")
	}
	if len(run.Spec.Discover.NodeSelector) != 0 {
		t.Errorf("the run guessed at which machines: %+v", run.Spec.Discover)
	}
	if filter := run.Spec.Discover.DeviceFilter; filter != nil {
		if len(filter.PcieAllowList) != 0 || len(filter.PcieDenyList) != 0 {
			t.Errorf("the run guessed at which disks: %+v", filter)
		}
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

// The initial run waives a partition table.
//
// A disk carrying one is the normal state of a machine that has held data
// before, and refusing every such disk makes the run that is supposed to show a
// fleet what it has report that it has nothing. The waiver is narrow on its own
// terms: it admits a disk whose only refusal is the table, so a boot disk stays
// out because its partition is mounted and the kernel will not hand it over,
// which are refusals of their own.
func TestTheInitialRunWaivesAPartitionTable(t *testing.T) {
	d := discoveryFor(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var run simplyblockv1alpha2.OperatorOps
	key := client.ObjectKey{Namespace: theNamespace, Name: InitialDiscoveryName}
	if err := d.Get(context.Background(), key, &run); err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	filter := run.Spec.Discover.DeviceFilter
	if filter == nil || filter.EnablePartitionedDevices == nil {
		t.Fatalf("the initial run states no partition waiver: %+v", run.Spec.Discover)
	}
	if !*filter.EnablePartitionedDevices {
		t.Error("the initial run refuses a disk for carrying a partition table")
	}
}

// TestTheRunOutlastsAWebhookThatIsNotServingYet covers the one write in the
// operator whose admission depends on the operator.
//
// Regression: 2026-09-20-initial-discovery-lost-to-its-own-webhook — on a fresh
// install the run was never raised, and the operator log carried
// `the initial discovery run could not be created ... failed calling webhook
// "voperatorops.simplyblock.io": ... connect: connection refused`. The create
// raced the webhook server this same process was still starting, Start is not a
// loop, and the single attempt lost the draft for the lifetime of the
// installation: an administrator found an empty namespace where the fleet's
// disks should have been listed, with nothing to re-raise it.
func TestTheRunOutlastsAWebhookThatIsNotServingYet(t *testing.T) {
	d, attempts := discoveryRefusingCreates(t, 3, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !raised(t, d) {
		t.Fatalf("the run was lost to a webhook that was not serving yet, after %d attempt(s)", *attempts)
	}
}

// A webhook that answers is an answer, and a rejected run is not retried until
// the deadline: the spec is what it is, and waiting cannot change it.
func TestARejectedRunIsNotRetried(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	attempts := 0
	inner := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}).Build()
	denying := interceptor.NewClient(inner, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			attempts++
			return apierrors.NewForbidden(
				schema.GroupResource{Group: "storage.simplyblock.io", Resource: "operatorops"},
				InitialDiscoveryName,
				fmt.Errorf(`admission webhook "voperatorops.simplyblock.io" denied the request`))
		},
	})
	d := &InitialDiscovery{Client: denying, Namespace: theNamespace}

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if attempts != 1 {
		t.Errorf("a denial was retried %d times; a rejected spec is an answer", attempts)
	}
}
