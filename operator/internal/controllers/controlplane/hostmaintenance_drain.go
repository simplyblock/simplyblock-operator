// How a HostMaintenance window sees the drain of its worker, and how its
// Holding deadline follows the drains of the pool.
//
// A drain is a series of eviction requests, and a request the budget refuses
// leaves no record in the API. The window therefore watches two things that a
// drain does leave: the sentinel pod it placed beside the held pods, which no
// budget holds and a drain evicts among the first, and the DisruptionTarget
// condition the eviction API puts on every other pod it evicts from the worker.
// Either is the drain, the first to arrive is recorded, and the record is never
// overwritten.
//
// The Holding deadline is measured from the last drain any window in the
// namespace observed, and from the window's own creation while none has. A
// pool whose update is progressing never expires a window waiting its turn,
// however many hosts the pool has, and a pool in which nothing has been drained
// for the length of the budget is a cordon with no update behind it.
//
// design-fdb-host-maintenance.md §5.3 and §7.2 are the specification.

package controlplane

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// evictionByEvictionAPI is the reason the eviction API writes into the
// DisruptionTarget condition of a pod it evicts.
const evictionByEvictionAPI = "EvictionByEvictionAPI"

// observeDrain records the drain of the worker when one of the two signals
// shows it, and does nothing once one is recorded.
func (r *ControlPlaneOpsReconciler) observeDrain(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) error {
	if ops.Status.HostMaintenance != nil && ops.Status.HostMaintenance.DrainObservedAt != nil {
		return nil
	}
	worker := ops.Spec.HostMaintenance.WorkerNode

	signal := simplyblockv1alpha2.HostMaintenanceDrainSignal("")
	evicted, err := r.sentinelEvicted(ctx, ops)
	if err != nil {
		return err
	}
	switch {
	case evicted:
		signal = simplyblockv1alpha2.HostMaintenanceDrainSignalSentinel
	default:
		neighbor, err := r.neighborEvicted(ctx, ops.Namespace, worker)
		if err != nil {
			return err
		}
		if neighbor != "" {
			signal = simplyblockv1alpha2.HostMaintenanceDrainSignalPodDisruption
		}
	}
	if signal == "" {
		return nil
	}

	now := metav1.Now()
	if err := r.patchWindow(ctx, ops, func(s *simplyblockv1alpha2.HostMaintenanceStatus) {
		s.DrainObservedAt = &now
		s.DrainObservedBy = signal
	}); err != nil {
		return err
	}
	how := "the eviction of the window's sentinel pod"
	if signal == simplyblockv1alpha2.HostMaintenanceDrainSignalPodDisruption {
		how = "the eviction of another pod on the worker"
	}
	r.emit(ops, corev1.EventTypeNormal, DrainObserved,
		fmt.Sprintf("a drain of %s was observed through %s", worker, how))
	return nil
}

// sentinelEvicted reports whether the sentinel has gone the way a drain takes
// it: its pod carries a deletion timestamp, or the Job ran a pod that is no
// longer there. A sentinel that was never created or never ran reports nothing.
func (r *ControlPlaneOpsReconciler) sentinelEvicted(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (bool, error) {
	var job batchv1.Job
	key := client.ObjectKey{Namespace: ops.Namespace, Name: fdbJobName(ops.Name, fdbCommandSentinel)}
	if err := r.Get(ctx, key, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read the sentinel of %s: %w", ops.Name, err)
	}
	if job.Status.Failed > 0 || jobOutcome(&job) == jobFailed {
		return true, nil
	}
	var pods corev1.PodList
	err := r.List(ctx, &pods, client.InNamespace(ops.Namespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name})
	if err != nil {
		return false, fmt.Errorf("list the sentinel's pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			return true, nil
		}
	}
	ran := job.Status.StartTime != nil
	return ran && len(pods.Items) == 0 && job.Status.Active == 0 && job.Status.Succeeded == 0, nil
}

// neighborEvicted is the name of a pod on the worker the eviction API has
// evicted, or "" when none has been.
func (r *ControlPlaneOpsReconciler) neighborEvicted(
	ctx context.Context, namespace, worker string,
) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list the pods of %s: %w", namespace, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != worker {
			continue
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue &&
				c.Reason == evictionByEvictionAPI {
				return pod.Name, nil
			}
		}
	}
	return "", nil
}

// holdingStillOpen is the sliding deadline of §5.3: the latest drain any window
// in the namespace observed, plus the Holding budget. It records that deadline
// on the window when it is later than the one recorded, and reports whether
// the window is still inside it. A namespace in which no drain has been
// observed leaves the window on its own deadline.
func (r *ControlPlaneOpsReconciler) holdingStillOpen(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (bool, error) {
	var list simplyblockv1alpha2.ControlPlaneOpsList
	if err := r.List(ctx, &list, client.InNamespace(ops.Namespace)); err != nil {
		return false, fmt.Errorf("list the operations in %s: %w", ops.Namespace, err)
	}
	var latest *metav1.Time
	holding := 0
	for i := range list.Items {
		w := &list.Items[i]
		if w.Spec.Action != simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance ||
			terminalOps(w.Status.Phase) {
			continue
		}
		if w.Status.Step.State == string(stepHolding) {
			holding++
		}
		if w.Status.HostMaintenance == nil {
			continue
		}
		if at := w.Status.HostMaintenance.DrainObservedAt; at != nil && (latest == nil || at.After(latest.Time)) {
			latest = at
		}
	}
	fdbHoldingWindows.WithLabelValues(ops.Namespace).Set(float64(holding))
	if latest == nil {
		return false, nil
	}
	deadline := metav1.NewTime(latest.Add(holdingDeadline))
	if time.Now().After(deadline.Time) {
		return false, nil
	}
	if ops.Status.Step.Deadline == nil || deadline.After(ops.Status.Step.Deadline.Time) {
		base := ops.DeepCopy()
		ops.Status.Step.Deadline = &deadline
		if err := r.Status().Patch(ctx, ops, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("extend the Holding deadline of %s: %w", ops.Name, err)
		}
	}
	return true, nil
}

// windowsForPod maps a pod to the window of the worker it runs on, when that
// window is Holding: a pod on a held worker is a sentinel or a neighbor whose
// eviction the window is waiting to see.
func (r *ControlPlaneOpsReconciler) windowsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	return r.windowRequests(ctx, pod.Namespace, func(w *simplyblockv1alpha2.ControlPlaneOps) bool {
		return w.Status.Step.State == string(stepHolding) &&
			w.Spec.HostMaintenance.WorkerNode == pod.Spec.NodeName
	})
}

// windowsForClusterFile maps the cluster-file ConfigMap to the windows that are
// verifying a move, so that verification runs when the FoundationDB operator
// rewrites the file rather than on the next requeue.
func (r *ControlPlaneOpsReconciler) windowsForClusterFile(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm.Name != clusterFileConfigMapName {
		return nil
	}
	return r.windowRequests(ctx, cm.Namespace, func(w *simplyblockv1alpha2.ControlPlaneOps) bool {
		return w.Status.Step.State == string(stepMovingCoordinators)
	})
}

// holdingWindowsBeside maps a window that has observed its drain to every
// other window in Holding, which is how the Holding deadline of §5.3 is
// re-armed as soon as a drain is seen rather than on the next requeue.
func (r *ControlPlaneOpsReconciler) holdingWindowsBeside(ctx context.Context, obj client.Object) []reconcile.Request {
	ops, ok := obj.(*simplyblockv1alpha2.ControlPlaneOps)
	if !ok || ops.Spec.Action != simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance ||
		ops.Status.HostMaintenance == nil || ops.Status.HostMaintenance.DrainObservedAt == nil {
		return nil
	}
	return r.windowRequests(ctx, ops.Namespace, func(w *simplyblockv1alpha2.ControlPlaneOps) bool {
		return w.Name != ops.Name && w.Status.Step.State == string(stepHolding)
	})
}

// windowRequests lists the live HostMaintenance windows of a namespace that a
// predicate selects, as reconcile requests.
func (r *ControlPlaneOpsReconciler) windowRequests(
	ctx context.Context, namespace string, selected func(*simplyblockv1alpha2.ControlPlaneOps) bool,
) []reconcile.Request {
	var list simplyblockv1alpha2.ControlPlaneOpsList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		w := &list.Items[i]
		if w.Spec.Action != simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance ||
			w.Spec.HostMaintenance == nil || terminalOps(w.Status.Phase) || !selected(w) {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(w)})
	}
	return requests
}
