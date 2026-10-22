// The Kubernetes side of the control plane's host fencing on a two-node cluster
// (sbcli docs/design/two-node-kubernetes-fencing.md §3.4): the settings pushed
// to the control plane, the host NQNs each storage node's Kubernetes node
// connects with, the StorageFenced condition a NodeHealthCheck remediates on,
// and the release once a fenced node is clean again.
//
// It lives beside the arbitration reconciler because it follows the same record
// on the same pass: the operator never decides a fence, it mirrors one.

package twonode

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// HostFencingOn reports whether a cluster asks for host fencing: a Dual
// cluster with arbitration and spec.arbitration.hostFencing on.
func HostFencingOn(cluster *simplyblockv1alpha2.StorageCluster) bool {
	return cluster.HostFencingEnabled()
}

// pushSettings writes two_node_arbitration and two_node_host_fencing to the
// control plane when they differ from the spec. rec is the record already read
// on this pass, or nil to read it. A failure is logged and retried on the next
// pass: the settings only change what the next loss does.
func (r *ArbitrationReconciler) pushSettings(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, rec *Record,
) {
	log := logf.FromContext(ctx)
	if rec == nil {
		got, err := r.API.Get(ctx, cluster)
		if err != nil || got == nil {
			return
		}
		rec = got
	}
	wantArb := cluster.Spec.ClusterMode == simplyblockv1alpha2.ClusterModeDual &&
		ptr.BoolFromOrFalse(cluster.Spec.EnableArbitration)
	wantHF := HostFencingOn(cluster)
	if rec.Enabled == wantArb && rec.HostFencing == wantHF {
		return
	}
	if err := r.API.SetTwoNodeSettings(ctx, cluster, wantArb, wantHF); err != nil {
		log.Info("cannot write the two-node settings; retrying", "error", err.Error())
	}
}

// followHostFences mirrors the control plane's host fences onto the cluster's
// Kubernetes nodes. It returns the fences for the cluster status and whether one
// is in progress (the reconcile then requeues at the active rate).
func (r *ArbitrationReconciler) followHostFences(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
	nodes []simplyblockv1alpha2.StorageNode, rec *Record,
) ([]simplyblockv1alpha2.HostFenceStatus, bool, error) {
	log := logf.FromContext(ctx)
	on := HostFencingOn(cluster)
	workerOf := map[string]string{}
	for _, sn := range nodes {
		workerOf[sn.Status.UUID] = sn.Spec.WorkerNode
	}
	var out []simplyblockv1alpha2.HostFenceStatus
	active := false
	for i := range nodes {
		sn := &nodes[i]
		if sn.Spec.WorkerNode == "" || sn.Status.UUID == "" {
			continue
		}
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: sn.Spec.WorkerNode}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, false, err
		}
		if on {
			if err := r.reportClientHosts(ctx, cluster, sn, &node, rec); err != nil {
				return nil, false, err
			}
		}
		fence, ok := rec.HostFences[sn.Status.UUID]
		cutOff := on && ok && fence.Effective()
		msg := ""
		if cutOff {
			msg = fmt.Sprintf("epoch %d: the node's NVMe hosts are revoked on %s", fence.Epoch, workerOf[fence.On])
		}
		if err := r.setStorageFenced(ctx, &node, cutOff, msg); err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		active = true
		out = append(out, simplyblockv1alpha2.HostFenceStatus{Node: sn.Spec.WorkerNode, State: fence.State,
			Winner: workerOf[fence.On], Epoch: fence.Epoch})
		if !on || fence.State != HostFenceReleasable {
			continue
		}
		clean, why, err := r.nodeClean(ctx, &node, fence.Since)
		if err != nil {
			return nil, false, err
		}
		if !clean {
			log.Info("host fence releasable; waiting for the node to be clean", "node", node.Name, "reason", why)
			continue
		}
		if err := r.API.ReleaseHostFence(ctx, cluster, sn.Status.UUID); err != nil {
			log.Info("host fence release refused; retrying", "node", node.Name, "error", err.Error())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, active, nil
}

// HostNQNs are the NVMe host NQNs a Kubernetes node connects with: the
// simplyblock form derived from its UID (access-controlled volumes), and the
// kernel default the CSI node plugin publishes (plain volumes).
func HostNQNs(node *corev1.Node) []string {
	set := map[string]bool{}
	if node.UID != "" {
		set[nqn.Host(string(node.UID))] = true
	}
	if v := strings.TrimSpace(node.Annotations[kube.AnnoNVMeHostNQN]); strings.HasPrefix(v, "nqn.") {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// reportClientHosts sends a storage node's host NQNs when the control plane
// holds others, and records what it accepted on the StorageNode.
func (r *ArbitrationReconciler) reportClientHosts(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
	sn *simplyblockv1alpha2.StorageNode, node *corev1.Node, rec *Record,
) error {
	want := HostNQNs(node)
	have := slices.Clone(rec.ClientHosts[sn.Status.UUID])
	sort.Strings(have)
	if !slices.Equal(have, want) {
		if err := r.API.SetClientHosts(ctx, cluster, sn.Status.UUID, want); err != nil {
			logf.FromContext(ctx).Info("cannot report the node's host NQNs; retrying",
				"node", node.Name, "error", err.Error())
			return nil
		}
	}
	if sn.Status.Remediation != nil && slices.Equal(sn.Status.Remediation.ReportedHostNQNs, want) {
		return nil
	}
	base := sn.DeepCopy()
	if sn.Status.Remediation == nil {
		sn.Status.Remediation = &simplyblockv1alpha2.NodeRemediationStatus{}
	}
	sn.Status.Remediation.ReportedHostNQNs = want
	return r.Status().Patch(ctx, sn, client.MergeFrom(base))
}

// setStorageFenced sets the node's StorageFenced condition. A node that never
// had one gets none while it is not fenced. A strategic merge patch keys the
// conditions by type, so the kubelet's own are left alone.
func (r *ArbitrationReconciler) setStorageFenced(ctx context.Context, node *corev1.Node, fenced bool, msg string) error {
	idx := slices.IndexFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == simplyblockv1alpha2.ConditionStorageFenced
	})
	want := corev1.ConditionFalse
	reason := simplyblockv1alpha2.StorageFencedReasonReleased
	if fenced {
		want, reason = corev1.ConditionTrue, simplyblockv1alpha2.StorageFencedReasonWitness
	}
	if idx < 0 && !fenced {
		return nil
	}
	if idx >= 0 {
		cur := node.Status.Conditions[idx]
		if cur.Status == want && cur.Reason == reason && cur.Message == msg {
			return nil
		}
	}
	now := metav1.NewTime(r.now())
	cond := corev1.NodeCondition{Type: simplyblockv1alpha2.ConditionStorageFenced, Status: want,
		Reason: reason, Message: msg, LastHeartbeatTime: now, LastTransitionTime: now}
	base := node.DeepCopy()
	if idx >= 0 {
		if node.Status.Conditions[idx].Status == want {
			cond.LastTransitionTime = node.Status.Conditions[idx].LastTransitionTime
		}
		node.Status.Conditions[idx] = cond
	} else {
		node.Status.Conditions = append(node.Status.Conditions, cond)
	}
	return r.Status().Patch(ctx, node, client.StrategicMergeFrom(base))
}

// clearStorageFenced withdraws the condition from the cluster's nodes once
// arbitration is off.
func (r *ArbitrationReconciler) clearStorageFenced(ctx context.Context, nodes []simplyblockv1alpha2.StorageNode) error {
	for _, sn := range nodes {
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
		if err := r.setStorageFenced(ctx, &node, false, ""); err != nil {
			return err
		}
	}
	return nil
}

// nodeClean reports whether a fenced node may get its host access back: it is
// Ready, and no pod that was there when the fence was armed still is, except the
// ones that tolerate the out-of-service taint (the storage pods, which the heal
// needs). A pod from before the fence may be the first instance of a workload
// that runs on the peer now; giving the host its access back while it lives
// would let both write.
func (r *ArbitrationReconciler) nodeClean(ctx context.Context, node *corev1.Node, since time.Time) (bool, string, error) {
	if !nodeReady(node) {
		return false, "the node is not Ready", nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return false, "", err
	}
	stale := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != node.Name || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if !p.CreationTimestamp.Time.Before(since) || ToleratesOutOfService(p.Spec.Tolerations) {
			continue
		}
		stale++
	}
	if stale > 0 {
		return false, fmt.Sprintf("%d pods from before the fence still on the node", stale), nil
	}
	return true, "", nil
}

// ToleratesOutOfService reports whether tolerations let a pod stay on a node
// with the out-of-service taint.
func ToleratesOutOfService(tolerations []corev1.Toleration) bool {
	for _, t := range tolerations {
		if t.Effect != "" && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if t.Key == simplyblockv1alpha2.TaintOutOfService || (t.Key == "" && t.Operator == corev1.TolerationOpExists) {
			return true
		}
	}
	return false
}
