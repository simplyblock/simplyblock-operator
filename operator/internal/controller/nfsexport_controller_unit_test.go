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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// workerNodeFor is the Kubernetes node a StorageNode fixture runs on. The two
// names are deliberately unalike: a StorageNode is named by its set and a
// Kubernetes node by its hostname, and nothing makes one derivable from the
// other, so a test that used the same string for both would pass whichever the
// code reached for.
func workerNodeFor(storageNode string) string {
	return "kube-" + storageNode + ".example.internal"
}

const (
	// testExportNS is the claim's namespace and testOperatorNS the deployment's.
	// They are deliberately different, because on a cluster they always are: an
	// NFSExport is created by the CSI controller beside the PVC it backs, and a
	// StorageNode belongs to the StorageNodeSet in the operator's namespace.
	// Fixtures that shared one namespace made every selection test pass against
	// an arrangement that does not occur.
	testExportNS   = "team-a"
	testOperatorNS = "simplyblock"
	testExportName = "nfsexp-7b41c0e2a9"
)

// testMDSHost and testOtherHost are the two storage nodes these tests bind
// exports to. They are constants because the binding is the thing under test:
// a typo in one of eleven literals would assert against a node that does not
// exist, and the assertion would pass.
const (
	testMDSHost   = "sn-worker-1-0"
	testOtherHost = "sn-worker-2-0"
)

// fakeAssembler records what it was asked to do and can be made to fail or to
// report a node unreachable.
type fakeAssembler struct {
	created    []string
	deleted    []string
	createErr  error
	noSessions bool
	nguid      string
}

func (f *fakeAssembler) CreateExport(
	_ context.Context, node string, _ *simplyblockv1alpha2.NFSExport,
) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, node)
	return f.nguid, nil
}

func (f *fakeAssembler) DeleteExport(_ context.Context, node string, _ *simplyblockv1alpha2.NFSExport) error {
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
			VolumeRef:  "nfs:0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90:pool-a:3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
			ExportPath: "/mnt/team-a-shared-3c81",
			FSID:       "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
		},
	}
	if mutate != nil {
		mutate(e)
	}
	return e
}

func testNode(name string, mutate func(*simplyblockv1alpha1.StorageNode)) *simplyblockv1alpha1.StorageNode {
	n := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testOperatorNS},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{WorkerNode: workerNodeFor(name)},
		Status:     simplyblockv1alpha1.StorageNodeStatus{Status: utils.NodeStatusOnline},
	}
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
		[]client.Object{&simplyblockv1alpha2.NFSExport{}, &simplyblockv1alpha1.StorageNode{}},
		objects...,
	)
	return &NFSExportReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  &fakeRecorder{},
		Assembler: asm,
	}, cl
}

// withBaselineKubeNode adds one Kubernetes node when the caller supplied none.
func withBaselineKubeNode(objects []client.Object) []client.Object {
	for _, o := range objects {
		if _, ok := o.(*corev1.Node); ok {
			return objects
		}
	}
	return append(objects, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-baseline"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.168.10.81"},
		}},
	})
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
	if got.Status.StorageNodeRef != testMDSHost {
		t.Errorf("storageNodeRef = %q, want sn-worker-1-0", got.Status.StorageNodeRef)
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
	offline := testNode(testMDSHost, func(n *simplyblockv1alpha1.StorageNode) {
		n.Status.Status = nodeStatusOffline
	})
	busy := testNode(testOtherHost, func(n *simplyblockv1alpha1.StorageNode) {
		n.Status.ActiveOpsRef = "some-ops"
	})
	r, cl := newExportReconciler(t, &fakeAssembler{}, testExport(nil), offline, busy)

	res := reconcileExport(t, r)

	if res.RequeueAfter != nfsExportNoHostRequeue {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, nfsExportNoHostRequeue)
	}
	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.StorageNodeRef != "" {
		t.Errorf("bound %q with nothing eligible", got.Status.StorageNodeRef)
	}
}

// Assembling calls the host and moves to Ready.
func TestAssemblingReachesReady(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.StorageNodeRef = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if want := workerNodeFor(testMDSHost); len(asm.created) != 1 || asm.created[0] != want {
		t.Errorf("CreateExport calls = %v, want one for %s", asm.created, want)
	}
	if got.Status.PhaseDeadline != nil {
		t.Error("Ready still carries a phase deadline")
	}
	for _, want := range []string{
		simplyblockv1alpha2.NFSExportConditionAssembled,
		simplyblockv1alpha2.NFSExportConditionExported,
	} {
		if !hasTrueCondition(got, want) {
			t.Errorf("condition %s is not True", want)
		}
	}
}

// A node that is merely disconnected is a wait, not a failure: it is the normal
// state during a rollout.
func TestAssemblingWaitsForAnUnreachableNode(t *testing.T) {
	asm := &fakeAssembler{noSessions: true}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.StorageNodeRef = testMDSHost
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
		e.Status.StorageNodeRef = testMDSHost
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
		e.Status.StorageNodeRef = testOtherHost
	})
	// A different node is eligible. If the machine restarted at Pending it would
	// select from the whole set and could pick this one instead.
	r, cl := newExportReconciler(t, asm, export,
		testNode(testMDSHost, nil), testNode(testOtherHost, nil))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.StorageNodeRef != testOtherHost {
		t.Errorf("binding moved to %q on restart; it must not", got.Status.StorageNodeRef)
	}
	if want := workerNodeFor(testOtherHost); len(asm.created) != 1 || asm.created[0] != want {
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
		e.Status.StorageNodeRef = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	if want := workerNodeFor(testMDSHost); len(asm.deleted) != 1 || asm.deleted[0] != want {
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
		e.Status.StorageNodeRef = testMDSHost
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
		e.Status.StorageNodeRef = testMDSHost
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

func hasTrueCondition(e *simplyblockv1alpha2.NFSExport, condType string) bool {
	for _, c := range e.Status.Conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// The NGUID is the one identifier neither the CSI controller nor the operator
// can know when the record is written: it is assigned by the target and read
// back from the device, so only a host with the namespace attached has it. The
// host that assembles the export has it, and recording what it observed is what
// lets a client derive its device alias without a second source of truth.
func TestAssemblingRecordsTheObservedNGUID(t *testing.T) {
	const observed = "ef90a8b4c7d21e0356f8a91b2c3d4e5f"

	asm := &fakeAssembler{nguid: observed}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.StorageNodeRef = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	if got := loadExport(t, cl).Status.NGUID; got != observed {
		t.Errorf("status.nguid = %q, want the host's %q", got, observed)
	}
}

// A host that reports no NGUID does not blank one already recorded. The value
// is a property of the namespace rather than of this pass, and overwriting it
// with an empty string would make a client that had a working alias lose it.
func TestAssemblingKeepsAKnownNGUID(t *testing.T) {
	const known = "0123456789abcdef0123456789abcdef"

	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Status.Phase = simplyblockv1alpha2.NFSExportPhaseAssembling
		e.Status.StorageNodeRef = testMDSHost
		e.Status.NGUID = known
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	if got := loadExport(t, cl).Status.NGUID; got != known {
		t.Errorf("status.nguid = %q, want the recorded %q", got, known)
	}
}

// An export never shares a namespace with the hosts that can serve it, and the
// fixtures above hid that by putting both in one.
//
// An NFSExport is namespaced to the claim it backs, because that is where the
// CSI controller creates it and where a user looks for it. A StorageNode is
// namespaced to the deployment that owns it, which is the operator's namespace.
// On a real cluster the two are never the same, so selection that looks only in
// the export's namespace finds nothing and every ReadWriteMany claim in the
// cluster waits forever on a host that was there all along.
func TestMDSSelectionFindsHostsInAnotherNamespace(t *testing.T) {
	const operatorNS = "simplyblock"

	asm := &fakeAssembler{}
	export := testExport(nil)
	node := testNode(testMDSHost, nil)
	if node.Namespace != operatorNS || export.Namespace == node.Namespace {
		t.Fatalf("the fixtures no longer model the arrangement: export in %s, node in %s",
			export.Namespace, node.Namespace)
	}
	r, cl := newExportReconciler(t, asm, export, node)

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.StorageNodeRef != testMDSHost {
		t.Fatalf("bound to %q, want %q: selection did not look outside %s",
			got.Status.StorageNodeRef, testMDSHost, export.Namespace)
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
		e.Status.StorageNodeRef = testMDSHost
	})
	r, cl := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	want := workerNodeFor(testMDSHost)
	if len(asm.created) != 1 || asm.created[0] != want {
		t.Fatalf("CreateExport reached %v, want [%s]", asm.created, want)
	}
	// The record still names the StorageNode, because that is the object a
	// reader goes looking for.
	if got := loadExport(t, cl).Status.StorageNodeRef; got != testMDSHost {
		t.Errorf("storageNodeRef = %q, want the StorageNode %q", got, testMDSHost)
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
		e.Status.StorageNodeRef = testMDSHost
		e.DeletionTimestamp = &now
	})
	r, _ := newExportReconciler(t, asm, export, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	want := workerNodeFor(testMDSHost)
	if len(asm.deleted) != 1 || asm.deleted[0] != want {
		t.Fatalf("DeleteExport reached %v, want [%s]", asm.deleted, want)
	}
}
