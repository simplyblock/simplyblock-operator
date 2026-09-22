// The fake client, the event recorder, and the scripted control plane every
// test in this package is driven with.
//
// The control plane is a struct of closures rather than a mock library: what
// each test needs is to answer three or four calls a particular way and to
// count how many times one of them was made, and a hand-written double states
// that in the test that cares rather than in a matcher DSL. Every method that
// is not overridden fails the test, which is what stops a reconciler reaching
// the control plane where the test did not expect it to.

package cluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	testNamespace   = "simplyblock"
	testClusterName = "production"
	testClusterUUID = "4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34"
	testOpsName     = "operation-1"

	// testClusterSecret is what a creation response carries. The control
	// plane mints it and never returns it again, which is why an adoption by
	// name has to find it elsewhere.
	testClusterSecret = "cluster-secret"
	otherOpsName      = "somebody-elses-operation"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		scheme.AddToScheme,
		corev1.AddToScheme,
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the test scheme: %v", err)
		}
	}
	return s
}

// newClient returns a fake client with the status subresource enabled for both
// kinds this package writes status on. Without it a Status().Patch is applied
// to the whole object, so a test that means to assert a status write would pass
// on a reconciler that wrote the spec.
func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&simplyblockv1alpha2.StorageCluster{},
			&simplyblockv1alpha2.StorageClusterOps{},
		).
		WithObjects(objects...).
		Build()
}

// objectMeta names an object in the one namespace these tests use. Two
// namespaces is the axis the test plan records as a blank, and reaching it
// needs a second cluster and a second operation rather than a second argument
// here.
func objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNamespace}
}

// newTestCluster is a cluster in steady state: created, active, and holding no
// operation's lock.
func newTestCluster(
	mutate ...func(*simplyblockv1alpha2.StorageCluster),
) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(testClusterName),
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(20)),
			VCPUCount:         ptr.To(int32(8)),
		},
		Status: simplyblockv1alpha2.StorageClusterStatus{
			UUID:   testClusterUUID,
			Status: utils.ClusterStatusActive,
			Phase:  simplyblockv1alpha2.StorageClusterPhaseOnline,
		},
	}
	c.Finalizers = []string{Finalizer}
	for _, m := range mutate {
		m(c)
	}
	return c
}

func newTestOps(
	action simplyblockv1alpha2.StorageClusterOpsAction,
	mutate ...func(*simplyblockv1alpha2.StorageClusterOps),
) *simplyblockv1alpha2.StorageClusterOps {
	ops := &simplyblockv1alpha2.StorageClusterOps{
		ObjectMeta: objectMeta(testOpsName),
		Spec: simplyblockv1alpha2.StorageClusterOpsSpec{
			ClusterRef: testClusterName,
			Action:     action,
		},
	}
	ops.Finalizers = []string{OpsFinalizer}
	for _, m := range mutate {
		m(ops)
	}
	return ops
}

// recorder remembers the events it is told about. events.EventRecorder has one
// method that matters here, and a fake is less machinery than a real
// broadcaster with a fake clientset behind it.
type recorder struct {
	events []recordedEvent
}

type recordedEvent struct {
	Type   string
	Reason string
}

func (r *recorder) Eventf(
	_ runtime.Object, _ runtime.Object, eventType, reason, _, _ string, _ ...any,
) {
	r.events = append(r.events, recordedEvent{Type: eventType, Reason: reason})
}

// count returns how many events carried a reason, which is what a test
// asserting "once, not once per pass" needs.
func (r *recorder) count(reason string) int {
	n := 0
	for _, e := range r.events {
		if e.Reason == reason {
			n++
		}
	}
	return n
}

func (r *recorder) has(reason string) bool { return r.count(reason) > 0 }

var _ events.EventRecorder = (*recorder)(nil)

// fakeControlPlane answers what a test scripts and fails the test on anything
// else. A nil field is a call the test did not expect, and reaching one is the
// failure rather than a zero value quietly standing in for an answer.
type fakeControlPlane struct {
	t *testing.T

	// endpoint is what Endpoint() returns verbatim -- a pure config read, not
	// an action the test scripts, so a zero value is a legitimate "unset"
	// rather than an unexpected call.
	endpoint string

	ready        func() error
	create       func(utils.ClusterAddParams) (webapi.ClusterResponse, error)
	cluster      func(string) (webapi.ClusterResponse, error)
	byName       func(string) (utils.ClusterListEntry, bool, error)
	deleteCall   func(string) error
	activate     func(string) error
	expand       func(string) error
	shutdown     func(string) error
	start        func(string) error
	storageNodes func(string) ([]utils.NodeStatusResponse, error)
	shutdownNode func(string, string) error
	restartNode  func(string, string) error
	tasks        func(string) ([]subscriptions.TaskDTO, error)
	cancelTask   func(string, string) error

	// clusterCtx, when set, is handed the context Cluster() was called with --
	// how a test observes the bearer-token override a caller attached
	// (webapi.BearerTokenFromContext), since the closures above only see the
	// clusterID.
	clusterCtx func(context.Context)

	// The counters the write-ahead tests read. Each is the number of times the
	// control plane was actually asked to do something, which is the only way
	// to tell a step that skipped its call from one that made it twice.
	activateCalls     int
	shutdownCalls     int
	startCalls        int
	createCalls       int
	deleteCalls       int
	shutdownNodeCalls int
	restartNodeCalls  int
	cancelTaskCalls   int
}

func (f *fakeControlPlane) Endpoint() string { return f.endpoint }

func (f *fakeControlPlane) Ready(context.Context) error {
	if f.ready == nil {
		return nil
	}
	return f.ready()
}

func (f *fakeControlPlane) CreateCluster(
	_ context.Context, params utils.ClusterAddParams,
) (webapi.ClusterResponse, error) {
	f.createCalls++
	if f.create == nil {
		f.t.Fatal("the control plane was asked to create a cluster and the test did not script it")
	}
	return f.create(params)
}

func (f *fakeControlPlane) Cluster(
	ctx context.Context, clusterID string,
) (webapi.ClusterResponse, error) {
	if f.clusterCtx != nil {
		f.clusterCtx(ctx)
	}
	if f.cluster == nil {
		f.t.Fatal("the control plane was asked for a cluster and the test did not script it")
	}
	return f.cluster(clusterID)
}

func (f *fakeControlPlane) ClusterByName(
	_ context.Context, name string,
) (utils.ClusterListEntry, bool, error) {
	if f.byName == nil {
		return utils.ClusterListEntry{}, false, nil
	}
	return f.byName(name)
}

func (f *fakeControlPlane) DeleteCluster(_ context.Context, clusterID string) error {
	f.deleteCalls++
	if f.deleteCall == nil {
		return nil
	}
	return f.deleteCall(clusterID)
}

func (f *fakeControlPlane) Activate(_ context.Context, clusterID string) error {
	f.activateCalls++
	if f.activate == nil {
		return nil
	}
	return f.activate(clusterID)
}

func (f *fakeControlPlane) Expand(_ context.Context, clusterID string) error {
	if f.expand == nil {
		return nil
	}
	return f.expand(clusterID)
}

func (f *fakeControlPlane) Shutdown(_ context.Context, clusterID string) error {
	f.shutdownCalls++
	if f.shutdown == nil {
		return nil
	}
	return f.shutdown(clusterID)
}

func (f *fakeControlPlane) Start(_ context.Context, clusterID string) error {
	f.startCalls++
	if f.start == nil {
		return nil
	}
	return f.start(clusterID)
}

func (f *fakeControlPlane) StorageNodes(
	_ context.Context, clusterID string,
) ([]utils.NodeStatusResponse, error) {
	if f.storageNodes == nil {
		return nil, nil
	}
	return f.storageNodes(clusterID)
}

func (f *fakeControlPlane) ShutdownNode(_ context.Context, clusterID, nodeID string) error {
	f.shutdownNodeCalls++
	if f.shutdownNode == nil {
		return nil
	}
	return f.shutdownNode(clusterID, nodeID)
}

func (f *fakeControlPlane) RestartNode(_ context.Context, clusterID, nodeID string) error {
	f.restartNodeCalls++
	if f.restartNode == nil {
		return nil
	}
	return f.restartNode(clusterID, nodeID)
}

func (f *fakeControlPlane) Tasks(
	_ context.Context, clusterID string,
) ([]subscriptions.TaskDTO, error) {
	if f.tasks == nil {
		return nil, nil
	}
	return f.tasks(clusterID)
}

func (f *fakeControlPlane) CancelTask(_ context.Context, clusterID, taskID string) error {
	f.cancelTaskCalls++
	if f.cancelTask == nil {
		return nil
	}
	return f.cancelTask(clusterID, taskID)
}

var _ ControlPlane = (*fakeControlPlane)(nil)

// fakeNodeCache stands in for the storage-node subscription's cache. It counts
// nothing of its own: what the tests assert is that the control plane was not
// asked, which the double next door counts.
type fakeNodeCache struct {
	nodes  map[string][]subscriptions.NodeDTO
	synced bool
}

func (f *fakeNodeCache) List(scope cpinformer.Scope) []subscriptions.NodeDTO {
	return f.nodes[scope[0]]
}

func (f *fakeNodeCache) Synced(cpinformer.Scope) bool { return f.synced }

var _ NodeCache = (*fakeNodeCache)(nil)

// fakeClusterCache stands in for the cluster subscription's cache. The stream
// is root-scoped, so there is one synced flag rather than one per scope.
type fakeClusterCache struct {
	clusters map[string]subscriptions.ClusterDTO
	synced   bool
	triggers chan event.GenericEvent
}

func (f *fakeClusterCache) Lookup(clusterID string) (subscriptions.ClusterDTO, bool) {
	dto, ok := f.clusters[clusterID]
	return dto, ok
}

func (f *fakeClusterCache) SyncedRoot() bool { return f.synced }

func (f *fakeClusterCache) Triggers() <-chan event.GenericEvent {
	if f.triggers == nil {
		f.triggers = make(chan event.GenericEvent)
	}
	return f.triggers
}

var _ ClusterCache = (*fakeClusterCache)(nil)

// fakeTaskCache stands in for the task subscription's cache.
type fakeTaskCache struct {
	tasks    map[string][]subscriptions.TaskDTO
	synced   bool
	triggers chan event.GenericEvent
}

func (f *fakeTaskCache) List(scope cpinformer.Scope) []subscriptions.TaskDTO {
	return f.tasks[scope[0]]
}

func (f *fakeTaskCache) Lookup(taskID string) (subscriptions.TaskDTO, bool) {
	for _, tasks := range f.tasks {
		for _, task := range tasks {
			if task.ID == taskID {
				return task, true
			}
		}
	}
	return subscriptions.TaskDTO{}, false
}

func (f *fakeTaskCache) Synced(cpinformer.Scope) bool { return f.synced }

func (f *fakeTaskCache) Triggers() <-chan event.GenericEvent {
	if f.triggers == nil {
		f.triggers = make(chan event.GenericEvent)
	}
	return f.triggers
}

var _ TaskCache = (*fakeTaskCache)(nil)

// activeCluster is the control plane's answer for a cluster that is up and not
// rebalancing, which is the steady state most tests start from.
func activeCluster() webapi.ClusterResponse {
	return webapi.ClusterResponse{
		UUID:              testClusterUUID,
		NQN:               "nqn.2023-02.io.simplyblock:4f2c8a11",
		Status:            utils.ClusterStatusActive,
		NDCS:              2,
		NPCS:              1,
		MaxFaultTolerance: 1,
	}
}
