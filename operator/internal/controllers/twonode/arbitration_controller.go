// Package twonode applies the two-node arbiter's verdicts on the edge: the
// storage-fenced taint on the Kubernetes node of a storage node the arbiter
// fenced, its removal once the cluster is steady again, and the remediation
// evidence the arbiter reads back
// (sbcli docs/design/two-node-arbitration.md §8, §9).
//
// The operator only follows the control plane: it never decides a verdict.
// With host fencing (sbcli docs/design/two-node-kubernetes-fencing.md) it sets
// the StorageFenced condition, and the StorageFenceRemediation a NodeHealthCheck
// creates on it is the one place node.kubernetes.io/out-of-service is set; the
// arbitration reconciler itself never sets it. When the control plane cannot be
// read, nothing changes.
package twonode

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Record is what the operator needs from the arbiter's ClusterArbitration
// record (§7.2): state, epoch and the control-plane ids of the storage nodes it
// holds fenced. The arbiter has no preferred node; a record that still carries
// one is read without it.
type Record struct {
	State       string
	Epoch       int64
	FencedNodes []string
	// Enabled and HostFencing are the cluster's settings as the control plane
	// holds them.
	Enabled     bool
	HostFencing bool
	// HostFences are the host fences by the fenced node's control-plane id
	// (sbcli docs/design/two-node-kubernetes-fencing.md §3.2).
	HostFences map[string]HostFence
	// ClientHosts are the host NQNs the control plane holds per node id.
	ClientHosts map[string][]string
}

// HostFence is one host fence: the fenced node's host NQNs are revoked on the
// winner (On, a control-plane node id).
type HostFence struct {
	State string
	On    string
	Epoch int64
	// Since is when the fence was armed.
	Since time.Time
}

// Host fence states.
const (
	HostFenceRevoked    = "revoked"
	HostFenceReleasable = "releasable"
	HostFenceRestoring  = "restoring"
)

// InForce reports whether the node's host is cut off: revoked, and kept so
// until the release has run.
func (f HostFence) InForce() bool {
	return f.State == HostFenceRevoked || f.State == HostFenceReleasable || f.State == HostFenceRestoring
}

// API is the operator's view of the control plane's arbitration endpoints
// (§7.4). Get returns nil, nil when the control plane holds no record yet.
type API interface {
	Get(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster) (*Record, error)
	// ReportFencing sends positive fencing evidence for a node, or clears it
	// (PUT .../arbitration/remediation).
	ReportFencing(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodeUUID string, fenced bool) error
	// SetTwoNodeSettings writes two_node_arbitration and two_node_host_fencing.
	SetTwoNodeSettings(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, arbitration, hostFencing bool) error
	// SetClientHosts reports the host NQNs of a storage node's Kubernetes node.
	SetClientHosts(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodeUUID string, hostNQNs []string) error
	// ReleaseHostFence gives a fenced node's host access back.
	ReleaseHostFence(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodeUUID string) error
}

const (
	stateSteady = "steady"
	// requeue while the arbiter is acting, and slower when it is not.
	requeueActive = 2 * time.Second
	requeueSteady = 15 * time.Second
	requeueError  = 5 * time.Second
)

// ArbitrationReconciler follows the arbiter for StorageClusters in clusterMode
// Dual with spec.enableArbitration on.
type ArbitrationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	API    API
	// Now, the clock, is time.Now when nil.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile reads the arbiter's record and makes the taints and statuses match.
func (r *ArbitrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	nodes, err := r.storageNodes(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster.Spec.ClusterMode != simplyblockv1alpha2.ClusterModeDual || !ptr.BoolFromOrFalse(cluster.Spec.EnableArbitration) {
		// Off: withdraw what this controller put on the nodes.
		if err := r.applyTaints(ctx, nodes, nil, 0); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.clearStorageFenced(ctx, nodes); err != nil {
			return ctrl.Result{}, err
		}
		if cluster.Spec.ClusterMode == simplyblockv1alpha2.ClusterModeDual && cluster.Status.UUID != "" {
			r.pushSettings(ctx, &cluster, nil)
		}
		return ctrl.Result{}, r.setStatus(ctx, &cluster, nil)
	}
	if cluster.Status.UUID == "" {
		return ctrl.Result{RequeueAfter: requeueSteady}, nil
	}
	if len(nodes) != 2 {
		return ctrl.Result{RequeueAfter: requeueSteady}, r.setStatus(ctx, &cluster, &simplyblockv1alpha2.ArbitrationStatus{
			Message: "two-node arbitration needs exactly two storage nodes"})
	}

	rec, err := r.API.Get(ctx, &cluster)
	if err != nil || rec == nil {
		// Unreadable or not initialized: no verdict to follow, so nothing changes.
		msg := "the control plane holds no arbitration record yet"
		if err != nil {
			msg = "cannot read the arbitration record: " + err.Error()
			log.Info("arbitration record unreadable; leaving taints as they are", "error", err.Error())
		}
		prev := cluster.Status.Arbitration.DeepCopy()
		if prev == nil {
			prev = &simplyblockv1alpha2.ArbitrationStatus{}
		}
		prev.Message = msg
		return ctrl.Result{RequeueAfter: requeueError}, r.setStatus(ctx, &cluster, prev)
	}

	byUUID := map[string]string{}
	for _, n := range nodes {
		if n.Status.UUID != "" {
			byUUID[n.Status.UUID] = n.Spec.WorkerNode
		}
	}

	// taint_requests is the arbiter's list: kept through healing, cleared once
	// the cluster is steady again.
	fenced := map[string]bool{}
	for _, id := range rec.FencedNodes {
		if w, ok := byUUID[id]; ok && w != "" {
			fenced[w] = true
		}
	}
	if err := r.applyTaints(ctx, nodes, fenced, rec.Epoch); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reportRemediation(ctx, &cluster, nodes); err != nil {
		return ctrl.Result{}, err
	}
	r.pushSettings(ctx, &cluster, rec)
	hostFences, active, err := r.followHostFences(ctx, &cluster, nodes, rec)
	if err != nil {
		return ctrl.Result{}, err
	}

	st := &simplyblockv1alpha2.ArbitrationStatus{State: rec.State, Epoch: rec.Epoch,
		FencedNodes: sortedKeys(fenced), HostFences: hostFences}
	if err := r.setStatus(ctx, &cluster, st); err != nil {
		return ctrl.Result{}, err
	}
	if rec.State == stateSteady && !active {
		return ctrl.Result{RequeueAfter: requeueSteady}, nil
	}
	return ctrl.Result{RequeueAfter: requeueActive}, nil
}

// SetupWithManager registers the reconciler on StorageClusters.
func (r *ArbitrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageCluster{}).
		Named("twonode-arbitration").
		Complete(r)
}

func (r *ArbitrationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
