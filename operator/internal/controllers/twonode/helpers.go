package twonode

import (
	"context"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// storageNodes returns the cluster's StorageNodes.
func (r *ArbitrationReconciler) storageNodes(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) ([]simplyblockv1alpha2.StorageNode, error) {
	var list simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}
	var out []simplyblockv1alpha2.StorageNode
	for _, n := range list.Items {
		if n.Spec.ClusterRef == cluster.Name {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// applyTaints puts the storage-fenced taint with the epoch on the fenced
// Kubernetes nodes and removes it from the others among the cluster's nodes.
func (r *ArbitrationReconciler) applyTaints(
	ctx context.Context, nodes []simplyblockv1alpha2.StorageNode, fenced map[string]bool, epoch int64,
) error {
	for _, sn := range nodes {
		name := sn.Spec.WorkerNode
		if name == "" {
			continue
		}
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		want := fenced[name]
		taints := withoutTaint(node.Spec.Taints, simplyblockv1alpha2.TaintStorageFenced)
		if want {
			taints = append(taints, corev1.Taint{Key: simplyblockv1alpha2.TaintStorageFenced,
				Value: strconv.FormatInt(epoch, 10), Effect: corev1.TaintEffectNoExecute})
		}
		if equality.Semantic.DeepEqual(taints, node.Spec.Taints) {
			continue
		}
		base := node.DeepCopy()
		node.Spec.Taints = taints
		if err := r.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}

// reportRemediation writes what Kubernetes knows about each node's host onto
// its StorageNode, for the arbiter to read as fencing evidence.
func (r *ArbitrationReconciler) reportRemediation(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodes []simplyblockv1alpha2.StorageNode,
) error {
	for i := range nodes {
		sn := &nodes[i]
		if sn.Spec.WorkerNode == "" {
			continue
		}
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: sn.Spec.WorkerNode}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		rem := &simplyblockv1alpha2.NodeRemediationStatus{NodeNotReady: !nodeReady(&node)}
		for _, t := range node.Spec.Taints {
			switch t.Key {
			case simplyblockv1alpha2.TaintOutOfService:
				rem.OutOfService = true
			case simplyblockv1alpha2.TaintStorageFenced:
				rem.StorageFencedEpoch, _ = strconv.ParseInt(t.Value, 10, 64)
			}
		}
		// Positive fencing evidence is out-of-service only: NotReady is also what
		// a partition looks like, and the storage-fenced taint is the arbiter's
		// own verdict. Reported when it changes; a failed report keeps the last
		// accepted value, so the next reconcile retries.
		prev := sn.Status.Remediation
		if prev != nil {
			rem.ReportedFenced = prev.ReportedFenced
		}
		if sn.Status.UUID != "" && (rem.ReportedFenced == nil || *rem.ReportedFenced != rem.OutOfService) {
			if err := r.API.ReportFencing(ctx, cluster, sn.Status.UUID, rem.OutOfService); err == nil {
				v := rem.OutOfService
				rem.ReportedFenced = &v
			}
		}
		if prev != nil && prev.OutOfService == rem.OutOfService && prev.NodeNotReady == rem.NodeNotReady &&
			prev.StorageFencedEpoch == rem.StorageFencedEpoch && boolPtrEq(prev.ReportedFenced, rem.ReportedFenced) {
			continue
		}
		now := metav1.NewTime(r.now())
		rem.ObservedAt = &now
		base := sn.DeepCopy()
		sn.Status.Remediation = rem
		if err := r.Status().Patch(ctx, sn, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}

// setStatus records the arbiter's state on the cluster when it changed.
func (r *ArbitrationReconciler) setStatus(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, st *simplyblockv1alpha2.ArbitrationStatus,
) error {
	prev := cluster.Status.Arbitration
	if st == nil && prev == nil {
		return nil
	}
	if st != nil && prev != nil {
		cmp := prev.DeepCopy()
		cmp.ObservedAt = nil
		if equality.Semantic.DeepEqual(cmp, st) {
			return nil
		}
	}
	if st != nil {
		now := metav1.NewTime(r.now())
		st.ObservedAt = &now
	}
	base := cluster.DeepCopy()
	cluster.Status.Arbitration = st
	return r.Status().Patch(ctx, cluster, client.MergeFrom(base))
}

func withoutTaint(taints []corev1.Taint, key string) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(taints))
	for _, t := range taints {
		if t.Key != key {
			out = append(out, t)
		}
	}
	return out
}

func boolPtrEq(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
