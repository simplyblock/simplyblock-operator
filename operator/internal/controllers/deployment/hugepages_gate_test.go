// Whether the Preprovisioned huge-page gate blocks CreatingCluster and
// CreatingNodes, and how it reports its outcome.
//
// design-preprovisioned-hugepages.md S:5, S:6.1; scenarios I-01..I-04 of
// test-plan-preprovisioned-hugepages.md, at the method level rather than
// through envtest (this suite otherwise drives the reconciler's own step
// methods directly, as TestTheClusterIsCreatedWithTheClassReadOffTheGroups and
// its neighbors already do).

package deployment

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/hugepages"
)

// stubEstimator is the "stub controlplane.Client" the test plan calls for: a
// fixed EstimateHugePages result or a fixed error, no server involved.
type stubEstimator struct {
	mib int64
	err error

	// calls counts invocations, so a test can assert the control plane was
	// never asked (I-03: hugePagesMode unset).
	calls *int
}

func (s stubEstimator) EstimateHugePages(context.Context, int32, int32) (int64, error) {
	if s.calls != nil {
		*s.calls++
	}
	return s.mib, s.err
}

// hugePagesWorker is a Node with allocatableMiB of hugepages-2Mi, both
// capacity and allocatable.
func hugePagesWorker(name string, allocatableMiB int64) *corev1.Node {
	qty := resource.NewQuantity(allocatableMiB*1024*1024, resource.BinarySI)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"hugepages-2Mi": *qty},
			Allocatable: corev1.ResourceList{"hugepages-2Mi": *qty},
		},
	}
}

// withControlPlane builds a reconciler exactly like reconcilerFor, with a
// control plane stub attached: hugePagesReady is the one method here that
// calls out to it.
func withControlPlane(
	t *testing.T, estimator hugepages.EstimateHugePagesClient, objects ...client.Object,
) *ClusterDeploymentConfigReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha2.ClusterDeploymentConfig{},
			&simplyblockv1alpha2.StorageCluster{}).
		WithObjects(objects...)
	return &ClusterDeploymentConfigReconciler{
		Client:       builder.Build(),
		Scheme:       scheme,
		Recorder:     events.NewFakeRecorder(64),
		Namespace:    theNamespace,
		ControlPlane: estimator,
	}
}

// preprovisioned is aDocument with HugePagesMode set and a single worker, so
// a case states only the worker's capacity.
func preprovisioned(worker string) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.HugePagesMode = simplyblockv1alpha2.HugePagesModePreprovisioned
		c.Spec.Cluster.MinHugePagesSize = "" // isolate the formula's own figure
		c.Spec.NodeSets[0].Groups[0].Workers = []string{worker}
	})
}

func conditionOf(config *simplyblockv1alpha2.ClusterDeploymentConfig) *metav1.Condition {
	return apimeta.FindStatusCondition(config.Status.Conditions, hugepages.ConditionHugePagesReady)
}

// U-01/I-01 equivalent: an undersized worker holds the gate and records why.
func TestHugePagesGateHoldsOnAnUndersizedWorker(t *testing.T) {
	config := preprovisioned("worker-1")
	r := withControlPlane(t, stubEstimator{mib: 100}, config, hugePagesWorker("worker-1", 100))

	ready, err := r.hugePagesReady(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("an undersized worker was reported ready")
	}

	cond := conditionOf(config)
	if cond == nil {
		t.Fatal("no HugePagesReady condition was written")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != hugepages.ReasonInsufficientHugePages {
		t.Errorf("condition = %+v, want False/%s", cond, hugepages.ReasonInsufficientHugePages)
	}
}

// I-02 equivalent: the same document, corrected, flips the condition to True
// and reports ready, on the next call -- no new document, no new approval.
func TestHugePagesGateFlipsToReadyOnceTheWorkerIsCorrected(t *testing.T) {
	config := preprovisioned("worker-1")
	r := withControlPlane(t, stubEstimator{mib: 100}, config, hugePagesWorker("worker-1", 100))

	if ready, err := r.hugePagesReady(context.Background(), config); err != nil || ready {
		t.Fatalf("ready = %v, err = %v; want not ready first", ready, err)
	}

	// The worker is corrected in place -- the same object, enough capacity now.
	var worker corev1.Node
	if err := r.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &worker); err != nil {
		t.Fatal(err)
	}
	corrected := hugePagesWorker("worker-1", 100000)
	corrected.ResourceVersion = worker.ResourceVersion
	if err := r.Status().Update(context.Background(), corrected); err != nil {
		t.Fatal(err)
	}

	ready, err := r.hugePagesReady(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("a corrected worker was still reported not ready")
	}
	cond := conditionOf(config)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != hugepages.ReasonReady {
		t.Errorf("condition = %+v, want True/%s", cond, hugepages.ReasonReady)
	}
}

// I-03 equivalent: hugePagesMode unset never calls the control plane and
// writes no condition.
func TestHugePagesGateSkipsEntirelyWhenModeIsUnset(t *testing.T) {
	calls := 0
	config := aDocument(nil) // no HugePagesMode
	r := withControlPlane(t, stubEstimator{mib: 100, calls: &calls},
		config, hugePagesWorker("worker-1", 1), hugePagesWorker("worker-2", 1))

	ready, err := r.hugePagesReady(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Error("a document with no HugePagesMode was held by the gate")
	}
	if calls != 0 {
		t.Errorf("the control plane was called %d time(s), want 0", calls)
	}
	if conditionOf(config) != nil {
		t.Error("a condition was written for a document that asked for no check")
	}
}

// I-04 equivalent: a control-plane error is returned as-is, and the
// condition, having never been set, is left exactly as it was found.
func TestHugePagesGateLeavesTheConditionOnAControlPlaneError(t *testing.T) {
	config := preprovisioned("worker-1")
	r := withControlPlane(t,
		stubEstimator{err: context.DeadlineExceeded},
		config, hugePagesWorker("worker-1", 100000))

	_, err := r.hugePagesReady(context.Background(), config)
	if err == nil {
		t.Fatal("want an error from an unreachable control plane")
	}
	if conditionOf(config) != nil {
		t.Error("a condition was written despite the control-plane call erroring")
	}
}

// A growth document reads HugePagesMode off the StorageCluster it names,
// since it states no template of its own.
func TestHugePagesGateReadsModeOffTheExistingClusterForAGrowthDocument(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
		c.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-1"}
	})
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.HugePagesMode = simplyblockv1alpha2.HugePagesModePreprovisioned
		c.Spec.MinHugePagesSize = ""
	})
	r := withControlPlane(t, stubEstimator{mib: 100}, config, cluster, hugePagesWorker("worker-1", 100))

	ready, err := r.hugePagesReady(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("an undersized worker on a growth document was reported ready")
	}
	if cond := conditionOf(config); cond == nil || cond.Reason != hugepages.ReasonInsufficientHugePages {
		t.Errorf("condition = %+v, want %s", cond, hugepages.ReasonInsufficientHugePages)
	}
}

// performStep itself refuses to run CreatingCluster while the gate holds,
// which is what keeps the document's phase short of CreatingNodes (I-01).
func TestPerformStepDoesNotCreateTheClusterWhileHugePagesGateHolds(t *testing.T) {
	config := preprovisioned("worker-1")
	r := withControlPlane(t, stubEstimator{mib: 100}, config, hugePagesWorker("worker-1", 100))

	done, err := r.performStep(context.Background(), config, stepCreatingCluster)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("stepCreatingCluster finished while the huge-page gate was holding it")
	}
	var cluster simplyblockv1alpha2.StorageCluster
	notFound := r.Get(context.Background(), client.ObjectKey{Namespace: theNamespace, Name: theCluster}, &cluster)
	if notFound == nil {
		t.Fatal("a StorageCluster was created while the huge-page gate was holding")
	}
}
