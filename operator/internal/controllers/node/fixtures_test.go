// The world the operation suites in this package drive, and the control plane
// they drive it against.
//
// Every step of every action is a call against the control plane followed by a
// predicate over what it answers next, so a suite that exercises a step needs
// both halves scripted: what the backend reports now, and what it was asked to
// do. One fake rather than one per suite, because the seven actions ask for
// different things and assert the same way — a restart issued once, a suspend
// skipped because the node was already suspended, a system volume deleted.
//
// It lives in a file of its own for the reason internal/controllers/testsupport
// does: a fixture beside one suite becomes a second copy beside the next.

package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// The one deployment every suite built on these fixtures runs in: a cluster
	// of one object, a node of one object, and the worker it runs on.
	opsNamespace = "simplyblock"
	opsCluster   = "a-cluster"
	opsWorker    = "worker-1"
	opsTarget    = "worker-2"

	// opsNodeID is the backend node behind the object, and opsPeerID a second
	// node of the same cluster — the one a drain moves volumes to.
	opsNodeID = "node-1111"
	opsPeerID = "node-2222"

	// opsClusterID is what the control plane knows the cluster by.
	opsClusterID = "cluster-1111"

	// opsNodeName is the node object every operation here targets, and opsPool
	// the one pool its volumes live in.
	opsNodeName = "a-node"
	opsPool     = "pool-1"
)

// scriptedControlPlane answers what a test scripted and records what it was
// asked.
//
// The embedded interface is nil on purpose: a call no suite scripts is a test
// reaching past what it is about, and a panic says so where a zero value would
// quietly pass.
type scriptedControlPlane struct {
	ControlPlane

	// nodes is what the control plane currently reports, by backend id. A test
	// that wants a step's second pass to see a different state rewrites an entry
	// between passes, which is what the backend doing the thing looks like from
	// here.
	nodes map[string]NodeReading

	// pools and volumes are what a drain's classification walks. The volumes are
	// keyed by pool, because that is the shape the control plane serves them in
	// and the shape the walk reads them in.
	pools   []webapi.StoragePoolInfo
	volumes map[string][]webapi.VolumeInfo

	// refuse fails one call by method name, which is how a suite reaches the
	// paths that only a refusing control plane produces.
	refuse map[string]error

	// calls is every call in order, each spelled as its method, a colon, and its
	// argument, which is what an assertion about a call being skipped or issued
	// once reads.
	calls []string

	// bearerTokens parallels calls: bearerTokens[i] is what
	// webapi.BearerTokenFromContext reported for calls[i]'s own context, so a
	// test can check that a cluster-scoped call authenticated as the cluster
	// it was actually asked about rather than as this operator's own
	// Kubernetes identity.
	bearerTokens []bearerObservation

	// restarts carries the parameters of each restart, because three actions
	// issue one and they differ precisely in what they fill in.
	restarts []RestartParams
}

// bearerObservation is one entry of scriptedControlPlane.bearerTokens.
type bearerObservation struct {
	token string
	ok    bool
}

// aControlPlane reports one online node and nothing else.
func aControlPlane() *scriptedControlPlane {
	return &scriptedControlPlane{
		nodes: map[string]NodeReading{
			opsNodeID: {UUID: opsNodeID, Status: nodeStatusOnline, ManagementIP: "10.0.0.1"},
		},
		volumes: map[string][]webapi.VolumeInfo{},
		refuse:  map[string]error{},
	}
}

// reporting replaces what the control plane says about the node under
// operation, which is how a test moves the backend between two passes of a step.
func (c *scriptedControlPlane) reporting(status string) *scriptedControlPlane {
	reading := c.nodes[opsNodeID]
	reading.UUID, reading.Status = opsNodeID, status
	c.nodes[opsNodeID] = reading
	return c
}

// withPeer adds a second node to the cluster, which is what a drain needs one of
// to have anywhere to move to.
func (c *scriptedControlPlane) withPeer(nodeID, status string) *scriptedControlPlane {
	c.nodes[nodeID] = NodeReading{UUID: nodeID, Status: status}
	return c
}

// holding puts volumes in the cluster's pool.
func (c *scriptedControlPlane) holding(volumes ...webapi.VolumeInfo) *scriptedControlPlane {
	if len(c.pools) == 0 {
		c.pools = append(c.pools, webapi.StoragePoolInfo{UUID: opsPool, Name: opsPool})
	}
	c.volumes[opsPool] = append(c.volumes[opsPool], volumes...)
	return c
}

// refusing makes one method answer with an error.
func (c *scriptedControlPlane) refusing(method string, err error) *scriptedControlPlane {
	c.refuse[method] = err
	return c
}

// asked is how many times one method was called, which is what "the call was
// skipped" and "the call was issued once" are both assertions about.
func (c *scriptedControlPlane) asked(method string) int {
	count := 0
	for _, call := range c.calls {
		if call == method || len(call) > len(method) && call[:len(method)+1] == method+":" {
			count++
		}
	}
	return count
}

func (c *scriptedControlPlane) record(ctx context.Context, method, argument string) error {
	token, ok := webapi.BearerTokenFromContext(ctx)
	c.calls = append(c.calls, method+":"+argument)
	c.bearerTokens = append(c.bearerTokens, bearerObservation{token, ok})
	return c.refuse[method]
}

func (c *scriptedControlPlane) StorageNode(
	ctx context.Context, _, nodeID string,
) (NodeReading, bool, error) {
	if err := c.record(ctx, "StorageNode", nodeID); err != nil {
		return NodeReading{}, false, err
	}
	reading, found := c.nodes[nodeID]
	return reading, found, nil
}

func (c *scriptedControlPlane) StorageNodes(
	ctx context.Context, clusterID string,
) ([]NodeReading, error) {
	if err := c.record(ctx, "StorageNodes", clusterID); err != nil {
		return nil, err
	}
	readings := make([]NodeReading, 0, len(c.nodes))
	for _, reading := range c.nodes {
		readings = append(readings, reading)
	}
	return readings, nil
}

func (c *scriptedControlPlane) AddNode(
	ctx context.Context, clusterID string, _ utils.StorageNodeSetAddParams,
) error {
	return c.record(ctx, "AddNode", clusterID)
}

func (c *scriptedControlPlane) Suspend(ctx context.Context, _, nodeID string) error {
	return c.record(ctx, "Suspend", nodeID)
}

func (c *scriptedControlPlane) Resume(ctx context.Context, _, nodeID string) error {
	return c.record(ctx, "Resume", nodeID)
}

func (c *scriptedControlPlane) ShutdownNode(ctx context.Context, _, nodeID string) error {
	return c.record(ctx, "ShutdownNode", nodeID)
}

func (c *scriptedControlPlane) RestartNode(
	ctx context.Context, _, nodeID string, params RestartParams,
) error {
	c.restarts = append(c.restarts, params)
	return c.record(ctx, "RestartNode", nodeID)
}

func (c *scriptedControlPlane) Promote(ctx context.Context, _, nodeID string) error {
	return c.record(ctx, "Promote", nodeID)
}

func (c *scriptedControlPlane) RemoveNode(ctx context.Context, _, nodeID string) error {
	return c.record(ctx, "RemoveNode", nodeID)
}

func (c *scriptedControlPlane) StoragePools(
	ctx context.Context, clusterID string,
) ([]webapi.StoragePoolInfo, error) {
	if err := c.record(ctx, "StoragePools", clusterID); err != nil {
		return nil, err
	}
	return c.pools, nil
}

func (c *scriptedControlPlane) PoolVolumes(
	ctx context.Context, _, poolID string,
) ([]webapi.VolumeInfo, error) {
	if err := c.record(ctx, "PoolVolumes", poolID); err != nil {
		return nil, err
	}
	return c.volumes[poolID], nil
}

func (c *scriptedControlPlane) DeleteVolume(ctx context.Context, _, _, volumeID string) error {
	return c.record(ctx, "DeleteVolume", volumeID)
}

// anOpsNode is the node every operation in these suites targets: provisioned,
// on opsWorker, holding no lock.
func anOpsNode() *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: opsNodeName, Namespace: opsNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: opsCluster,
			WorkerNode: opsWorker,
		},
	}
	node.Status.UUID = opsNodeID
	return node
}

// anOpsCluster is the node's cluster, already created in the control plane.
func anOpsCluster() *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: opsCluster, Namespace: opsNamespace},
	}
	cluster.Status.UUID = opsClusterID
	return cluster
}

// anOperation is one operation of the given action against anOpsNode.
func anOperation(
	name string, action simplyblockv1alpha2.StorageNodeOpsAction,
) *simplyblockv1alpha2.StorageNodeOps {
	return &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: opsNamespace},
		Spec: simplyblockv1alpha2.StorageNodeOpsSpec{
			NodeRef: opsNodeName,
			Action:  action,
		},
	}
}

// aWorker is a Kubernetes node, Ready unless a test says otherwise.
func aWorker(name string, ready bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

// anOpsWorld builds the reconciler over a fake client holding the node, its
// cluster, and whatever else the suite put in.
func anOpsWorld(
	t *testing.T, api ControlPlane, objects ...client.Object,
) (*StorageNodeOpsReconciler, client.Client) {
	t.Helper()
	return anOpsWorldWith(t, api, interceptor.Funcs{}, objects...)
}

// anOpsWorldWith scripts the client's answers too, which is how a suite reaches
// a read that fails — the census is written to treat one of those as a fact
// about the census rather than about the volume.
func anOpsWorldWith(
	t *testing.T, api ControlPlane, funcs interceptor.Funcs, objects ...client.Object,
) (*StorageNodeOpsReconciler, client.Client) {
	t.Helper()
	scheme := testsupport.NewScheme(t,
		corev1.AddToScheme, policyv1.AddToScheme, discoveryv1.AddToScheme)

	world := append([]client.Object{anOpsNode(), anOpsCluster()}, objects...)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(world...).
		WithStatusSubresource(
			&simplyblockv1alpha2.StorageNodeOps{},
			&simplyblockv1alpha2.StorageNode{},
			&simplyblockv1alpha2.StorageCluster{},
		).
		WithInterceptorFuncs(funcs).
		Build()

	reconciler := &StorageNodeOpsReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      api,
		Workload: &Workload{Client: apiClient},
	}
	return reconciler, apiClient
}

// aPersistentVolume is one simplyblock volume as Kubernetes accounts for it,
// claimed by a PersistentVolumeClaim named after it.
func aPersistentVolume(name, volumeUUID string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       utils.CSIProvisioner,
					VolumeHandle: fmt.Sprintf("%s:%s:%s", opsClusterID, opsPool, volumeUUID),
				},
			},
			ClaimRef: &corev1.ObjectReference{
				Namespace: opsNamespace,
				Name:      name + "-claim",
			},
		},
	}
}

// aClaim is the claim behind such a volume, pinned or not.
func aClaim(name string, pinned bool) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-claim", Namespace: opsNamespace},
	}
	if pinned {
		claim.Annotations = map[string]string{atlaskube.AnnoSelectedStorageNode: opsNodeID}
	}
	return claim
}

// refusingClaims fails every claim read, which is the transient failure the
// census is written to treat as a fact about the census rather than as a fact
// about the volume.
func refusingClaims() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey,
			object client.Object, options ...client.GetOption,
		) error {
			if _, claim := object.(*corev1.PersistentVolumeClaim); claim {
				return errors.New("the API server is briefly away")
			}
			return c.Get(ctx, key, object, options...)
		},
	}
}
