// The Kubernetes-side failure a storage node's own status cannot show.
//
// Every other reason a node is held is one the operator or the control plane
// knows: the cluster has no UUID, the worker's API does not answer, no slot is
// free. A pod the scheduler has refused is none of those. The backend never
// hears of the node, the worker looks healthy, and the operator sits in
// CheckingHost until its deadline with nothing to say beyond that the host is
// unreachable. That is true, and it names the symptom rather than the cause.
//
// The scheduler has already written the cause down, in the message of the pod's
// PodScheduled condition, and it is specific: which nodes it considered and what
// each of them was short of. This file copies that sentence onto the StorageNode,
// where the administrator looking at the stalled worker is already looking.
//
// design-storagenode.md §13.1 is the specification for the event.

package node

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// objectNameField is the field a DaemonSet pod's node affinity is written
// against. k8s.io/api declares no constant for it, and the DaemonSet controller
// writes this literal.
const objectNameField = "metadata.name"

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// reportPodScheduling announces a storage-node pod that cannot be placed on this
// node's worker.
//
// It reports rather than decides: a node whose pod is unplaceable is already
// held by whichever step is waiting on the worker, and failing it here would
// turn a condition that resolves when somebody frees capacity into one that
// needs the object recreated.
//
// An unreadable pod list says nothing, because the alternative is announcing a
// scheduling failure on the strength of not having looked.
func (r *StorageNodeReconciler) reportPodScheduling(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) {
	var pods corev1.PodList
	err := r.List(ctx, &pods,
		client.InNamespace(node.Namespace),
		client.MatchingLabels{
			kube.LabelApp:                kube.AppStorageNode,
			kube.LabelSimplyblockCluster: node.Spec.ClusterRef,
		})
	if err != nil {
		return
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if podWorker(pod) != node.Spec.WorkerNode {
			continue
		}
		message, refused := podSchedulingRefusal(pod)
		if !refused {
			continue
		}
		r.emit(node, corev1.EventTypeWarning, PodSchedulingFailed,
			"the storage-node pod "+pod.Name+" cannot be placed on "+
				node.Spec.WorkerNode+": "+message)
		return
	}
}

// podWorker is the worker a storage-node pod belongs to.
//
// It is not spec.nodeName, because a pod that has not been placed has no node
// name and being unplaced is the case this file exists for. The DaemonSet
// controller writes the worker into the pod's node affinity as a metadata.name
// match field and leaves the binding to the scheduler, so an unplaced pod names
// its worker there and nowhere else.
func podWorker(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	terms := affinity.NodeAffinity.
		RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for _, term := range terms {
		for _, field := range term.MatchFields {
			if field.Key != objectNameField ||
				field.Operator != corev1.NodeSelectorOpIn ||
				len(field.Values) != 1 {
				continue
			}
			return field.Values[0]
		}
	}
	return ""
}

// podSchedulingRefusal reports whether the scheduler has refused this pod, and
// what it said.
//
// Only Unschedulable counts. A gated pod carries the same false condition and is
// one the scheduler has not yet been asked about, so announcing it would raise a
// warning for every pod on its way to running rather than for one that is stuck.
func podSchedulingRefusal(pod *corev1.Pod) (string, bool) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type != corev1.PodScheduled ||
			condition.Status != corev1.ConditionFalse ||
			condition.Reason != corev1.PodReasonUnschedulable {
			continue
		}
		return condition.Message, true
	}
	return "", false
}
