// Unit tests for NFSExportReconciler: one per phase transition, plus the two
// cases a phase machine exists for at all -- a position restored after a
// restart, and a transition the graph does not allow.
//
// The assembler is faked rather than mocked at the transport, because what these
// tests pin is the controller's decisions: which host it binds, when it waits,
// when it gives up, and what it leaves behind on delete. Whether csi-link
// carries the call is a different test at a different level.

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	exportpkg "github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// testExportNS is the claim's namespace. An NFSExport is created by the CSI
	// controller beside the PVC it backs, while the nodes it binds to are
	// cluster-scoped, so nothing here shares a namespace.
	testExportNS   = "team-a"
	testExportName = "nfsexp-7b41c0e2a9"
)

// testMDSHost and testOtherHost are the two Kubernetes nodes these tests bind
// exports to. They are constants because the binding is the thing under test:
// a typo in one of eleven literals would assert against a node that does not
// exist, and the assertion would pass.
const (
	testMDSHost   = "kube-worker-1.example.internal"
	testOtherHost = "kube-worker-2.example.internal"
)

// fakeAssembler records what it was asked to do and can be made to fail or to
// report a node unreachable.
type fakeAssembler struct {
	created    []string
	deleted    []string
	createErr  error
	deleteErr  error
	noSessions bool
}

func (f *fakeAssembler) CreateExport(
	_ context.Context, node string, _ *simplyblockv1alpha2.NFSExport,
) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, node)
	return nil
}

func (f *fakeAssembler) DeleteExport(_ context.Context, node string, _ *simplyblockv1alpha2.NFSExport) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, node)
	return nil
}

func (f *fakeAssembler) HasSession(string) bool { return !f.noSessions }

func testExport(mutate func(*simplyblockv1alpha2.NFSExport)) *simplyblockv1alpha2.NFSExport {
	e := &simplyblockv1alpha2.NFSExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testExportName,
			Namespace:  testExportNS,
			Finalizers: []string{simplyblockv1alpha2.NFSExportFinalizer},
		},
		Spec: simplyblockv1alpha2.NFSExportSpec{
			VolumeRef:  "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90:pool-a:3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
			ExportPath: "/var/lib/simplyblock/exports/team-a-shared-3c81",
		},
	}
	if mutate != nil {
		mutate(e)
	}
	return e
}

func testNode(name string, mutate func(*corev1.Node)) *corev1.Node {
	n := kubeNode(name, "192.168.10.83")
	if mutate != nil {
		mutate(n)
	}
	return n
}

func newExportReconciler(
	t *testing.T,
	asm ExportAssembler,
	objects ...client.Object,
) (*NFSExportReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, corev1.AddToScheme)
	// A cluster has nodes, and the default NodeScoped client policy resolves
	// against them. A fixture with none makes every binding test assert the
	// no-clients path instead of the one it is named for, so one is supplied
	// unless the test brought its own to assert on.
	objects = withBaselineKubeNode(objects)
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.NFSExport{}},
		objects...,
	)
	return &NFSExportReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  &fakeRecorder{},
		Assembler: asm,
	}, cl
}

// withBaselineKubeNode keeps the cluster non-empty when a test supplied no
// nodes of its own.
//
// The client set resolves against the cluster's nodes, so a fixture with none
// makes every binding test assert the cannot-bind path instead of the one it is
// named for.
func withBaselineKubeNode(objects []client.Object) []client.Object {
	for _, o := range objects {
		if _, ok := o.(*corev1.Node); ok {
			return objects
		}
	}
	return append(objects, kubeNode("kube-baseline", testNodeIP))
}

func reconcileExport(t *testing.T, r *NFSExportReconciler) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: testExportName, Namespace: testExportNS},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func loadExport(t *testing.T, cl client.Client) *simplyblockv1alpha2.NFSExport {
	t.Helper()
	var e simplyblockv1alpha2.NFSExport
	if err := cl.Get(context.Background(),
		client.ObjectKey{Name: testExportName, Namespace: testExportNS}, &e); err != nil {
		t.Fatalf("reading back the export: %v", err)
	}
	return &e
}

// A new export with an eligible host binds to it and moves to Assembling. The
// binding must be written before the host is asked for anything, so that a
// reconcile dying mid-assembly finds it rather than picking a second host.
func TestPendingBindsAnEligibleHost(t *testing.T) {
	asm := &fakeAssembler{}
	r, cl := newExportReconciler(t, asm, testExport(nil), testNode(testMDSHost, nil))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Errorf("phase = %q, want Assembling", got.Status.Phase)
	}
	if got.Status.MDSNodeName != testMDSHost {
		t.Errorf("mdsNodeName = %q, want the bound node", got.Status.MDSNodeName)
	}
	if len(asm.created) != 0 {
		t.Errorf("assembler was called before the binding was persisted: %v", asm.created)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}

// With no eligible host the export waits rather than failing, and says so. A
// refusal that emits nothing is indistinguishable from a reconcile that never
// ran.
func TestPendingWaitsWhenNoHostIsEligible(t *testing.T) {
	notReady := testNode(testMDSHost, func(n *corev1.Node) {
		n.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
		}
	})
	cordoned := testNode(testOtherHost, func(n *corev1.Node) {
		n.Spec.Unschedulable = true
	})
	r, cl := newExportReconciler(t, &fakeAssembler{}, testExport(nil), notReady, cordoned)

	res := reconcileExport(t, r)

	if res.RequeueAfter != nfsExportNoHostRequeue {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, nfsExportNoHostRequeue)
	}
	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.MDSNodeName != "" {
		t.Errorf("bound %q with nothing eligible", got.Status.MDSNodeName)
	}
}

// Assembling calls the host and moves to Ready.
func TestAssemblingReachesReady(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.MDSNodeName = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if want := testMDSHost; len(asm.created) != 1 || asm.created[0] != want {
		t.Errorf("CreateExport calls = %v, want one for %s", asm.created, want)
	}
	if got.Status.PhaseDeadline != nil {
		t.Error("Ready still carries a phase deadline")
	}
}

// A node that is merely disconnected is a wait, not a failure: it is the normal
// state during a rollout.
func TestAssemblingWaitsForAnUnreachableNode(t *testing.T) {
	asm := &fakeAssembler{noSessions: true}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.MDSNodeName = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	res := reconcileExport(t, r)

	if res.RequeueAfter != nfsExportNoSessionRequeue {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, nfsExportNoSessionRequeue)
	}
	if got := loadExport(t, cl); got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Errorf("phase = %q, want it to stay Assembling", got.Status.Phase)
	}
	if len(asm.created) != 0 {
		t.Errorf("called an unreachable node: %v", asm.created)
	}
}

// An assembly that outlives its deadline is given up on rather than retried
// forever, and the export parks where a human can see it.
func TestAssemblingGivesUpAtTheDeadline(t *testing.T) {
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.MDSNodeName = testMDSHost
		e.Status.PhaseDeadline = &past
	})
	r, cl := newExportReconciler(t, &fakeAssembler{}, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	if got.Status.Message == "" {
		t.Error("Degraded without a message saying why")
	}
}

// The phase machine is restored from status, not begun afresh. Without this a
// restarted operator would re-enter Pending and bind a second host to an export
// that already has one, which is the one mistake that destroys data.
func TestPhaseSurvivesARestart(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.MDSNodeName = testOtherHost
	})
	// A different node is eligible. If the machine restarted at Pending it would
	// select from the whole set and could pick this one instead.
	r, cl := newExportReconciler(t, asm, export,
		testNode(testMDSHost, nil), testNode(testOtherHost, nil))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.MDSNodeName != testOtherHost {
		t.Errorf("binding moved to %q on restart; it must not", got.Status.MDSNodeName)
	}
	if want := testOtherHost; len(asm.created) != 1 || asm.created[0] != want {
		t.Errorf("assembled on %v, want the already-bound %s", asm.created, want)
	}
}

// Degraded is terminal for this controller: it is reached by declining to act,
// and re-deciding it on a timer would bury the event that explains it.
func TestDegradedIsTerminal(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseDegraded
		e.Status.Message = "assembly timed out"
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	res := reconcileExport(t, r)

	if res.RequeueAfter != 0 {
		t.Errorf("Degraded requeued: %+v", res)
	}
	if len(asm.created) != 0 {
		t.Errorf("Degraded still drove the host: %v", asm.created)
	}
	if got := loadExport(t, cl); got.Status.Message != "assembly timed out" {
		t.Errorf("message was rewritten to %q", got.Status.Message)
	}
}

// Deleting tears the export down on its host and then drops the finalizer. The
// order matters: the host is the only place the mount and the export entry
// exist.
func TestDeleteTearsDownThenReleases(t *testing.T) {
	now := metav1.Now()
	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.DeletionTimestamp = &now
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseReady
		e.Status.MDSNodeName = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	if want := testMDSHost; len(asm.deleted) != 1 || asm.deleted[0] != want {
		t.Errorf("DeleteExport calls = %v, want one for %s", asm.deleted, want)
	}
	var e simplyblockv1alpha2.NFSExport
	err := cl.Get(context.Background(),
		client.ObjectKey{Name: testExportName, Namespace: testExportNS}, &e)
	if err == nil && len(e.Finalizers) != 0 {
		t.Errorf("finalizer still held: %v", e.Finalizers)
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("reading back: %v", err)
	}
}

// A teardown that cannot reach the host waits instead of dropping the
// finalizer, because releasing it would orphan the mount and the export entry
// with nothing left to name them.
func TestDeleteWaitsForAnUnreachableNode(t *testing.T) {
	now := metav1.Now()
	asm := &fakeAssembler{noSessions: true}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.DeletionTimestamp = &now
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseReady
		e.Status.MDSNodeName = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	res := reconcileExport(t, r)

	if res.RequeueAfter != nfsExportNoSessionRequeue {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, nfsExportNoSessionRequeue)
	}
	if len(asm.deleted) != 0 {
		t.Errorf("tore down through an unreachable node: %v", asm.deleted)
	}
	got := loadExport(t, cl)
	if len(got.Finalizers) == 0 {
		t.Error("finalizer released while the host was unreachable")
	}
}

// An assembly error is returned so controller-runtime backs off, rather than
// being swallowed into a phase that looks settled.
func TestAssemblyErrorIsRetried(t *testing.T) {
	asm := &fakeAssembler{createErr: errors.New("mount: device busy")}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.MDSNodeName = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: testExportName, Namespace: testExportNS},
	})
	if err == nil {
		t.Fatal("a failed assembly returned no error, so it will not be retried")
	}
	if got := loadExport(t, cl); got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Errorf("phase = %q, want it to stay Assembling for the retry", got.Status.Phase)
	}
}

// The graph refuses a jump it does not declare. Pending cannot reach Ready
// without assembling, and the machine says so rather than writing the status.
func TestGraphRefusesAnUndeclaredJump(t *testing.T) {
	sm, err := newExportMachineAt(simplyblockv1alpha2.NFSExportPhasePending)
	if err != nil {
		t.Fatalf("building the machine: %v", err)
	}
	defer sm.Close()

	if sm.CanTransitionTo(simplyblockv1alpha2.NFSExportPhaseReady) {
		t.Error("Pending -> Ready is allowed; assembly can be skipped")
	}
	if !sm.CanTransitionTo(simplyblockv1alpha2.NFSExportPhaseAssembling) {
		t.Error("Pending -> Assembling is not allowed")
	}
}

// newExportMachineAt restores the declared graph at one phase, so a test can ask
// what the graph permits without driving a whole reconcile.
func newExportMachineAt(at simplyblockv1alpha2.NFSExportPhase) (*statemachine.Machine[phase], error) {
	return statemachine.NewFromSnapshot(context.Background(), exportPhases(),
		statemachine.Snapshot[phase]{State: at})
}

// An export binds a node whatever namespace the export itself is in.
//
// An NFSExport is namespaced to the claim it backs, because that is where the
// CSI controller creates it and where a user looks for it. The nodes it binds
// to are cluster-scoped, so there is no namespace to get wrong -- which is the
// point: an earlier arrangement selected StorageNodes, which live in the
// operator's namespace, and a list scoped to the export found nothing on every
// real cluster.
func TestSelectionIsClusterScoped(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(nil)
	node := testNode(testMDSHost, nil)
	if export.Namespace == "" || node.Namespace != "" {
		t.Fatalf("the fixtures no longer model the arrangement: export in %q, node in %q",
			export.Namespace, node.Namespace)
	}
	r, cl := newExportReconciler(t, asm, export, node)

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.MDSNodeName != testMDSHost {
		t.Fatalf("bound to %q, want %q", got.Status.MDSNodeName, testMDSHost)
	}
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Errorf("phase = %q, want Assembling", got.Status.Phase)
	}
}

// The link addresses a Kubernetes node, and status.storageNodeRef names a
// StorageNode. They are different names for different objects, and the mapping
// between them is spec.workerNode.
//
// Getting this wrong is invisible until a cluster runs it: HasSession returns
// false for a name no peer registered under, which the reconciler reads as a
// host that is merely disconnected, so the export waits in Assembling until the
// deadline and reports a timeout rather than a mismatch.
func TestAssemblyReachesTheKubernetesNodeNotTheStorageNodeName(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.MDSNodeName = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	want := testMDSHost
	if len(asm.created) != 1 || asm.created[0] != want {
		t.Fatalf("CreateExport reached %v, want [%s]", asm.created, want)
	}
	// The record still names the StorageNode, because that is the object a
	// reader goes looking for.
	if got := loadExport(t, cl).Status.MDSNodeName; got != testMDSHost {
		t.Errorf("mdsNodeName = %q, want the StorageNode %q", got, testMDSHost)
	}
}

// Tearing down has to reach the same host, or the export is dropped from the
// record while its filesystem stays mounted and published on a host nothing
// points at any more.
func TestTeardownReachesTheKubernetesNode(t *testing.T) {
	asm := &fakeAssembler{}
	now := metav1.Now()
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseReady
		e.Status.MDSNodeName = testMDSHost
		e.DeletionTimestamp = &now
	})
	r, _ := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	want := testMDSHost
	if len(asm.deleted) != 1 || asm.deleted[0] != want {
		t.Fatalf("DeleteExport reached %v, want [%s]", asm.deleted, want)
	}
}

// An export that never got far enough to describe itself must still delete.
//
// A record that was refused, or that failed before its client set or its
// backing volume was recorded, cannot produce a spec for the host. There is
// also nothing on the host to tear down, precisely because it never got that
// far. Treating the refusal as a reason to retry leaves the object with a
// finalizer nothing will ever remove, and the only remedy is editing finalizers
// by hand -- on an object whose whole purpose was to avoid that.
func TestDeletingAnExportThatNeverAssembledConverges(t *testing.T) {
	asm := &fakeAssembler{deleteErr: exportpkg.ErrInvalidSpec}
	now := metav1.Now()
	e := testExport(func(x *simplyblockv1alpha2.NFSExport) {
		x.Status.Phase = simplyblockv1alpha2.NFSExportPhaseDegraded
		x.Status.MDSNodeName = testMDSHost
		// No client set was ever resolved, so there is nothing to build a
		// spec from and nothing on the host to tear down.
		x.Status.AllowedClients = nil
		x.DeletionTimestamp = &now
	})
	r, cl := newExportReconciler(t, asm, e, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	var got simplyblockv1alpha2.NFSExport
	err := cl.Get(context.Background(),
		client.ObjectKey{Name: testExportName, Namespace: testExportNS}, &got)
	if err == nil && controllerutil.ContainsFinalizer(&got, simplyblockv1alpha2.NFSExportFinalizer) {
		t.Error("the finalizer survived, so the record can never be deleted")
	}
}

// A teardown that reached the host and failed there is a different thing, and
// is retried: the mount and the exports entry may well exist, and dropping the
// finalizer would orphan them with nothing left naming them.
func TestDeletingRetriesWhenTheHostRefuses(t *testing.T) {
	asm := &fakeAssembler{deleteErr: errors.New("exportfs exited 1")}
	now := metav1.Now()
	e := testExport(func(x *simplyblockv1alpha2.NFSExport) {
		x.Status.Phase = simplyblockv1alpha2.NFSExportPhaseReady
		x.Status.MDSNodeName = testMDSHost
		x.Status.AllowedClients = []string{"192.168.10.0/24"}
		x.DeletionTimestamp = &now
	})
	r, cl := newExportReconciler(t, asm, e, testNode(testMDSHost, nil))

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: testExportName, Namespace: testExportNS},
	})
	if err == nil {
		t.Fatal("a failed teardown reported success")
	}

	var got simplyblockv1alpha2.NFSExport
	if err := cl.Get(context.Background(),
		client.ObjectKey{Name: testExportName, Namespace: testExportNS}, &got); err != nil {
		t.Fatalf("the record was deleted despite a failed teardown: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, simplyblockv1alpha2.NFSExportFinalizer) {
		t.Error("the finalizer was dropped while the host may still hold the export")
	}
}

// The kind declares exactly the phases this controller implements.
//
// A phase in the CRD's enum that the controller cannot drive is a promise the
// code does not keep: a user sees it in `kubectl explain`, a reviewer counts it
// as built, and an object that somehow reaches it parks with a message saying
// the feature does not exist. Failover is real work with its own section in the
// design, and until it is written the kind should not advertise it.
//
// This is spelled as a comparison against the state graph rather than a list of
// strings, so the two cannot drift: adding a phase to one without the other
// fails here.
func TestTheKindDeclaresOnlyThePhasesTheControllerDrives(t *testing.T) {
	implemented := map[simplyblockv1alpha2.NFSExportPhase]bool{}
	for p := range exportPhases().States {
		implemented[p] = true
	}

	declared := []simplyblockv1alpha2.NFSExportPhase{
		simplyblockv1alpha2.NFSExportPhasePending,
		simplyblockv1alpha2.NFSExportPhaseAssembling,
		simplyblockv1alpha2.NFSExportPhaseReady,
		simplyblockv1alpha2.NFSExportPhaseDegraded,
	}
	if len(implemented) != len(declared) {
		t.Errorf("the graph has %d phases and the kind declares %d", len(implemented), len(declared))
	}
	for _, p := range declared {
		if !implemented[p] {
			t.Errorf("the kind declares %q, which the controller cannot drive", p)
		}
	}
}
