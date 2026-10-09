// Package twonode applies the two-node arbiter's verdicts on the edge: the
// storage-fenced taint on the Kubernetes node of a storage node the arbiter
// fenced, its removal once the cluster is steady again, the preferred node, and
// the remediation evidence the arbiter reads back
// (sbcli docs/design/two-node-arbitration.md §8, §9).
//
// The operator only follows the control plane: it never decides a verdict, and
// it never sets node.kubernetes.io/out-of-service (that is OpenShift's
// remediation or an administrator's call). When the control plane cannot be
// read, nothing changes.
package twonode

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Record is what the operator needs from the arbiter's ClusterArbitration
// record (§7.2): state, epoch, preferred node and the control-plane ids of the
// storage nodes it holds fenced.
type Record struct {
	State         string
	Epoch         int64
	PreferredNode string
	FencedNodes   []string
}

// API is the operator's view of the control plane's arbitration endpoints
// (§7.4). Get returns nil, nil when the control plane holds no record yet.
type API interface {
	Get(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster) (*Record, error)
	SetPreferred(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodeUUID string) error
}

const (
	stateSteady = "steady"
	// requeue while the arbiter is acting, and slower when it is not.
	requeueActive = 2 * time.Second
	requeueSteady = 15 * time.Second
	requeueError  = 5 * time.Second
)

// ArbitrationReconciler follows the arbiter for StorageClusters with
// spec.twoNode.arbitration on.
type ArbitrationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	API    API
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

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
	if cluster.Spec.TwoNode == nil || !cluster.Spec.TwoNode.Arbitration {
		// Off: withdraw what this controller put on the nodes.
		if err := r.applyTaints(ctx, nodes, nil, 0); err != nil {
			return ctrl.Result{}, err
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
		// Unreadable or not initialised: no verdict to follow, so nothing changes.
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
	if want := cluster.Spec.TwoNode.PreferredNode; want != "" {
		for _, n := range nodes {
			if n.Spec.WorkerNode == want && n.Status.UUID != "" && n.Status.UUID != rec.PreferredNode {
				if err := r.API.SetPreferred(ctx, &cluster, n.Status.UUID); err != nil {
					log.Info("could not set the preferred node", "error", err.Error())
				}
			}
		}
	}

	fenced := map[string]bool{}
	if rec.State != stateSteady {
		for _, id := range rec.FencedNodes {
			if w, ok := byUUID[id]; ok && w != "" {
				fenced[w] = true
			}
		}
	}
	if err := r.applyTaints(ctx, nodes, fenced, rec.Epoch); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reportRemediation(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}

	st := &simplyblockv1alpha2.ArbitrationStatus{State: rec.State, Epoch: rec.Epoch,
		PreferredNode: byUUID[rec.PreferredNode], FencedNodes: sortedKeys(fenced)}
	if err := r.setStatus(ctx, &cluster, st); err != nil {
		return ctrl.Result{}, err
	}
	if rec.State == stateSteady {
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
