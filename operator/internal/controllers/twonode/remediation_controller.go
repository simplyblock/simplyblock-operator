// The StorageFenceRemediation reconciler: NodeHealthCheck's external
// remediation for a node whose host the control plane fenced on a two-node
// cluster (sbcli docs/design/two-node-kubernetes-fencing.md §1).
//
// It skips power fencing. The control plane has revoked the node's NVMe host
// identities on its peer, so the node cannot write to the cluster, and the
// remediation applies node.kubernetes.io/out-of-service directly: pods are
// force-deleted, VolumeAttachments force-detached, and the workloads restart on
// the peer. Every pass checks again before the taint goes on, because the taint
// is only safe while the fence holds.

package twonode

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// RemediationFinalizer holds a remediation until its taint is removed.
	RemediationFinalizer = "storage.simplyblock.io/storage-fence-remediation"
	// AnnoRemediationNodeName is where NodeHealthCheck names the node when the
	// remediation is not named after it.
	AnnoRemediationNodeName = "remediation.medik8s.io/node-name"
	requeueRemediation      = 5 * time.Second
)

// RemediationReconciler applies and removes the out-of-service taint for
// StorageFenceRemediations.
type RemediationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	API    API
	// Now, the clock, is time.Now when nil.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagefenceremediations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagefenceremediations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagefenceremediations/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagefenceremediationtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattachments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile drives one remediation.
func (r *RemediationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rem simplyblockv1alpha2.StorageFenceRemediation
	if err := r.Get(ctx, req.NamespacedName, &rem); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	nodeName := rem.Annotations[AnnoRemediationNodeName]
	if nodeName == "" {
		nodeName = rem.Name
	}

	if !rem.DeletionTimestamp.IsZero() {
		if err := r.removeTaint(ctx, nodeName); err != nil {
			return ctrl.Result{}, err
		}
		if controllerutil.RemoveFinalizer(&rem, RemediationFinalizer) {
			return ctrl.Result{}, r.Update(ctx, &rem)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&rem, RemediationFinalizer) {
		if err := r.Update(ctx, &rem); err != nil {
			return ctrl.Result{}, err
		}
	}

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return r.pending(ctx, &rem, nodeName, simplyblockv1alpha2.StorageFenceRemediationPending,
				"node not found", 0)
		}
		return ctrl.Result{}, err
	}
	if hasTaint(&node, simplyblockv1alpha2.TaintOutOfService) {
		return ctrl.Result{}, r.tainted(ctx, &rem, nodeName, rem.Status.Epoch)
	}

	epoch, why, err := r.fenced(ctx, &node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if why != "" {
		return r.pending(ctx, &rem, nodeName, simplyblockv1alpha2.StorageFenceRemediationPending, why, epoch)
	}
	if !rem.Spec.AllowForeignVolumes {
		foreign, err := r.foreignVolumes(ctx, nodeName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(foreign) > 0 {
			return r.pending(ctx, &rem, nodeName, simplyblockv1alpha2.StorageFenceRemediationBlocked,
				"volumes of other CSI drivers are attached ("+strings.Join(foreign, ", ")+
					"); the out-of-service taint would force-detach them unfenced", epoch)
		}
	}

	base := node.DeepCopy()
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: simplyblockv1alpha2.TaintOutOfService,
		Value: simplyblockv1alpha2.TaintOutOfServiceValue, Effect: corev1.TaintEffectNoExecute})
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[simplyblockv1alpha2.AnnoOutOfServiceBy] = simplyblockv1alpha2.OutOfServiceByStorageFence
	if err := r.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("out-of-service taint applied to a storage-fenced node", "node", nodeName, "epoch", epoch)
	return ctrl.Result{}, r.tainted(ctx, &rem, nodeName, epoch)
}

// fenced confirms the node's host fence with the control plane, not only with
// the condition: the condition is the operator's copy, and the taint must not
// outlive the fence it relies on. why is empty when the fence holds.
func (r *RemediationReconciler) fenced(ctx context.Context, node *corev1.Node) (int64, string, error) {
	cond := slices.IndexFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == simplyblockv1alpha2.ConditionStorageFenced && c.Status == corev1.ConditionTrue
	})
	if cond < 0 {
		return 0, "the node is not storage-fenced", nil
	}
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return 0, "", err
	}
	idx := slices.IndexFunc(nodes.Items, func(sn simplyblockv1alpha2.StorageNode) bool {
		return sn.Spec.WorkerNode == node.Name && sn.Status.UUID != ""
	})
	if idx < 0 {
		return 0, "no storage node runs on the node", nil
	}
	sn := nodes.Items[idx]
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, client.ObjectKey{Namespace: sn.Namespace, Name: sn.Spec.ClusterRef}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, "the storage node's cluster is gone", nil
		}
		return 0, "", err
	}
	if !HostFencingOn(&cluster) {
		return 0, "host fencing is off for cluster " + cluster.Name, nil
	}
	rec, err := r.API.Get(ctx, &cluster)
	if err != nil || rec == nil {
		return 0, "cannot confirm the fence with the control plane", nil
	}
	fence, ok := rec.HostFences[sn.Status.UUID]
	if !ok || !fence.InForce() {
		return 0, "the control plane holds no host fence for the node", nil
	}
	return fence.Epoch, "", nil
}

// foreignVolumes lists the VolumeAttachments on the node that belong to another
// CSI driver.
func (r *RemediationReconciler) foreignVolumes(ctx context.Context, nodeName string) ([]string, error) {
	var vas storagev1.VolumeAttachmentList
	if err := r.List(ctx, &vas); err != nil {
		return nil, err
	}
	var out []string
	for _, va := range vas.Items {
		if va.Spec.NodeName == nodeName && va.Spec.Attacher != kube.DriverName {
			out = append(out, fmt.Sprintf("%s (%s)", va.Name, va.Spec.Attacher))
		}
	}
	slices.Sort(out)
	return out, nil
}

// removeTaint takes off the out-of-service taint this remediation set; one an
// administrator or another remediation set stays.
func (r *RemediationReconciler) removeTaint(ctx context.Context, nodeName string) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return client.IgnoreNotFound(err)
	}
	if node.Annotations[simplyblockv1alpha2.AnnoOutOfServiceBy] != simplyblockv1alpha2.OutOfServiceByStorageFence {
		return nil
	}
	base := node.DeepCopy()
	node.Spec.Taints = withoutTaint(node.Spec.Taints, simplyblockv1alpha2.TaintOutOfService)
	delete(node.Annotations, simplyblockv1alpha2.AnnoOutOfServiceBy)
	if err := r.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("out-of-service taint removed from a node no longer storage-fenced", "node", nodeName)
	return nil
}

func (r *RemediationReconciler) pending(
	ctx context.Context, rem *simplyblockv1alpha2.StorageFenceRemediation, nodeName string,
	phase simplyblockv1alpha2.StorageFenceRemediationPhase, msg string, epoch int64,
) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: requeueRemediation},
		r.writeStatus(ctx, rem, nodeName, phase, msg, epoch, metav1.ConditionTrue, metav1.ConditionFalse)
}

func (r *RemediationReconciler) tainted(
	ctx context.Context, rem *simplyblockv1alpha2.StorageFenceRemediation, nodeName string, epoch int64,
) error {
	return r.writeStatus(ctx, rem, nodeName, simplyblockv1alpha2.StorageFenceRemediationTainted, "", epoch,
		metav1.ConditionFalse, metav1.ConditionTrue)
}

// writeStatus records the phase and the Processing and Succeeded conditions
// NodeHealthCheck reads, when they changed.
func (r *RemediationReconciler) writeStatus(
	ctx context.Context, rem *simplyblockv1alpha2.StorageFenceRemediation, nodeName string,
	phase simplyblockv1alpha2.StorageFenceRemediationPhase, msg string, epoch int64,
	processing, succeeded metav1.ConditionStatus,
) error {
	base := rem.DeepCopy()
	st := &rem.Status
	st.Phase, st.Node, st.Message = phase, nodeName, msg
	if epoch != 0 {
		st.Epoch = epoch
	}
	if phase == simplyblockv1alpha2.StorageFenceRemediationTainted && st.TaintedAt == nil {
		now := metav1.NewTime(r.now())
		st.TaintedAt = &now
	}
	reason := string(phase)
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: simplyblockv1alpha2.RemediationConditionProcessing,
		Status: processing, Reason: reason, Message: msg, ObservedGeneration: rem.Generation})
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: simplyblockv1alpha2.RemediationConditionSucceeded,
		Status: succeeded, Reason: reason, Message: msg, ObservedGeneration: rem.Generation})
	if statusEqual(base.Status, rem.Status) {
		return nil
	}
	return r.Status().Patch(ctx, rem, client.MergeFrom(base))
}

func statusEqual(a, b simplyblockv1alpha2.StorageFenceRemediationStatus) bool {
	if a.Phase != b.Phase || a.Node != b.Node || a.Message != b.Message || a.Epoch != b.Epoch ||
		(a.TaintedAt == nil) != (b.TaintedAt == nil) || len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message {
			return false
		}
	}
	return true
}

func hasTaint(node *corev1.Node, key string) bool {
	return slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool { return t.Key == key })
}

func (r *RemediationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the reconciler on StorageFenceRemediations.
func (r *RemediationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageFenceRemediation{}).
		Named("twonode-storage-fence-remediation").
		Complete(r)
}
