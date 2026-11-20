// The listing of a stretched cluster's other site on this site.
//
// On a stretched cluster every site lists every node: this site's operator holds
// a managed StorageNode for each node it runs and a remote StorageNode for each
// node of the other site, so `kubectl get storagenode` shows the whole cluster
// on either side (design-stretched-sync-replication-ramen.md §6.2). A remote node
// is a record, not a workload: this controller creates one for every node the
// control plane reports on the peer site, recreates one that is deleted, and
// removes one whose node has left the cluster. The StorageNode controller keeps
// its status, and nothing on this site ever operates on it.

package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// remoteNodeResync is how often a stretched cluster's listing is compared with
// the control plane's. The node stream's changes reach a registered remote node
// at once; a peer-site node that is new to this site has no record to wake yet,
// so its arrival is picked up on this cadence.
const remoteNodeResync = 30 * time.Second

// RemoteNodeReconciler keeps the remote StorageNodes of a stretched cluster in
// step with the control plane's listing of the peer site.
type RemoteNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      ControlPlane
	Nodes    NodeCache
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch

// SetupWithManager registers the controller. It reconciles the StorageCluster,
// whose stretched block decides whether there is another site to list, and it
// owns the remote records, so deleting one wakes it to recreate the record.
func (r *RemoteNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageCluster{}, builder.OnlyMetadata).
		Owns(&simplyblockv1alpha2.StorageNode{}).
		Named("storagenode-remote").
		Complete(r)
}

func (r *RemoteNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if cluster.Spec.Stretched == nil || cluster.Status.UUID == "" || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	peers, authoritative, err := r.peerNodes(ctx, &cluster)
	if err != nil {
		logf.FromContext(ctx).Error(err, "the control plane's node list could not be read",
			"cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: remoteNodeResync}, nil
	}
	if !authoritative {
		// An empty cold cache and a peer site with no nodes look the same, and
		// acting on the first as the second would delete every remote record.
		return ctrl.Result{RequeueAfter: remoteNodeResync}, nil
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(cluster.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	listed := map[string]bool{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster.Name || !node.IsRemote() || node.Status.UUID == "" {
			continue
		}
		listed[node.Status.UUID] = true
		if _, present := peers[node.Status.UUID]; present {
			continue
		}
		if err := r.Delete(ctx, node); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&cluster, nil, corev1.EventTypeNormal, RemoteNodeRemoved, RemoteNodeRemoved,
			"Node %s of site %s left the cluster; its remote record %s was removed",
			node.Status.UUID, node.Spec.Config.Site, node.Name)
	}

	for id, reading := range peers {
		if listed[id] {
			continue
		}
		if err := r.listRemote(ctx, &cluster, reading); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: remoteNodeResync}, nil
}

// peerNodes are the nodes the control plane reports on the cluster's peer site,
// by id, from the stream's cache once its snapshot has arrived and from the
// control plane otherwise. authoritative is false when neither has answered,
// which is never an empty site.
func (r *RemoteNodeReconciler) peerNodes(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (map[string]NodeReading, bool, error) {
	var readings []NodeReading
	switch {
	case r.Nodes != nil && r.Nodes.Synced(scopeOf(cluster.Status.UUID)):
		for _, dto := range r.Nodes.List(scopeOf(cluster.Status.UUID)) {
			readings = append(readings, readingFromDTO(dto))
		}
	case r.API != nil:
		secret, err := clusterSecretByName(ctx, r.Client, cluster.Namespace, cluster.Name)
		listed, err := r.API.StorageNodes(authenticatedContext(ctx, secret, err), cluster.Status.UUID)
		if err != nil {
			return nil, false, err
		}
		readings = listed
	default:
		return nil, false, nil
	}

	peers := map[string]NodeReading{}
	for _, reading := range readings {
		if reading.Site == cluster.Spec.Stretched.PeerSite && reading.UUID != "" {
			peers[reading.UUID] = reading
		}
	}
	return peers, true, nil
}

// listRemote creates the remote record of one peer-site node, owned by the
// cluster so a cluster delete takes it, and stamps the node's id into its
// status, which is what the StorageNode controller and the device mirror key on.
func (r *RemoteNodeReconciler) listRemote(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, reading NodeReading,
) error {
	vcpu := reading.CPUCount
	if vcpu < 1 {
		vcpu = 1
	}
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      remoteNodeName(cluster.Name, reading.Site, reading.UUID),
			Namespace: cluster.Namespace,
		},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: cluster.Name,
			Config: simplyblockv1alpha2.StorageNodeConfig{
				Site:   reading.Site,
				Sizing: simplyblockv1alpha2.StorageNodeSizing{VCPUCount: ptr.To(vcpu)},
			},
			Stretched: &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, node, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, node); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("listing remote node %s: %w", reading.UUID, err)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
			return err
		}
	}
	if node.Status.UUID != reading.UUID {
		node.Status.UUID = reading.UUID
		node.Status.Phase = simplyblockv1alpha2.StorageNodePhasePending
		if err := r.Status().Update(ctx, node); err != nil {
			return fmt.Errorf("recording remote node %s: %w", reading.UUID, err)
		}
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, RemoteNodeAdded, RemoteNodeAdded,
		"Node %s of site %s is listed here as remote record %s", reading.UUID, reading.Site, node.Name)
	return nil
}

// remoteNodeName names the remote record of a peer-site node. The node DTO does
// not carry the name its managed record has on its own site, so the name is
// derived from what it does carry: deterministic, so a recreate finds the same
// name, and unable to collide with a managed node of this site, whose names come
// from its workers.
func remoteNodeName(cluster, site, nodeID string) string {
	short := nodeID
	if len(short) > 8 {
		short = short[:8]
	}
	suffix := "-" + strings.ToLower(strings.ReplaceAll(site, "_", "-")) + "-" + strings.ToLower(short)
	prefix := cluster
	if limit := 63 - len(suffix); len(prefix) > limit {
		prefix = strings.TrimRight(prefix[:limit], "-.")
	}
	return prefix + suffix
}

// reconcileRemote keeps a remote record's status: the node's id is registered,
// so its status and device streams reach this record, and its status is what the
// control plane reports. It never provisions, never runs a workload, never
// takes a finalizer, and lets a delete through without asking the control plane
// for anything: the node is its own site's, and RemoteNodeReconciler lists it
// again if it is still there.
func (r *StorageNodeReconciler) reconcileRemote(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, cluster *simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	if !node.DeletionTimestamp.IsZero() {
		r.unregister(node)
		if controllerutil.RemoveFinalizer(node, NodeFinalizer) {
			return ctrl.Result{}, r.Update(ctx, node)
		}
		return ctrl.Result{}, nil
	}
	if cluster == nil || node.Status.UUID == "" {
		return ctrl.Result{RequeueAfter: nodeRetry}, nil
	}
	r.register(node, cluster.Status.UUID, node.Status.UUID)

	reading, found, err := r.nodeReading(ctx, cluster.Status.UUID, node.Status.UUID)
	if err != nil || !found {
		// A node the control plane no longer reports is RemoteNodeReconciler's to
		// remove, from a snapshot it can trust.
		return ctrl.Result{RequeueAfter: nodeRetry}, nil
	}
	err = r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		applyReading(status, reading)
		status.Phase = phaseOf(reading)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: nodeRetry}, nil
}
