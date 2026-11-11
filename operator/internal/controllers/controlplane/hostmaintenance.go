// The HostMaintenance action: taking a FoundationDB host through a Kubernetes
// node drain.
//
// A cordoned worker that runs FoundationDB processes gets a window. The window
// holds the worker's FoundationDB pods with a PodDisruptionBudget that allows no
// disruption, waits for a drain of the worker to be observed, and then, under
// the control plane's lock, sets FoundationDB's maintenance mode for the
// worker's zone, moves the coordinators the worker holds onto other hosts,
// verifies the move in the cluster file, and deletes the budget so the drain
// proceeds. Afterward it waits for the worker's process groups to report back,
// wherever they come back, and clears the maintenance mode it set.
//
//	Holding ──► EnteringMaintenance ──► MovingCoordinators ──► Releasing ──►
//	AwaitingHost ──► LeavingMaintenance ──► Cleanup
//
// The window never refuses a drain. Every command it issues can fail, and
// every failure before Releasing is reported and stepped over, because a
// budget that outlives its window makes the worker undrainable by anything
// with nothing left saying why. Every terminal path, success or not, takes the
// markers down.
//
// This file carries the dispatch, the three steps that touch only Kubernetes
// objects, and the teardown. The steps that issue FoundationDB commands are in
// hostmaintenance_database.go. design-fdb-host-maintenance.md is the
// specification; §5 for the steps and §6 for the machine.

package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// windowBudgetPrefix names the window's budget. It is not the storage node's
// prefix, so that the two windows a converged worker gets hold and release
// their own objects (§5.10).
const windowBudgetPrefix = "sb-fdb-maintenance-"

// windowBudgetName is the budget of the window for a worker, hashed the way the
// storage node's is so a worker named by a fully qualified domain name still
// yields a label.
func windowBudgetName(worker string) string {
	return windowBudgetPrefix + hash8(worker)
}

// windowBudget is the budget Holding raises: no disruption allowed over the
// pods carrying the window's label for the worker.
func windowBudget(ops *simplyblockv1alpha2.ControlPlaneOps) *policyv1.PodDisruptionBudget {
	worker := ops.Spec.HostMaintenance.WorkerNode
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:            windowBudgetName(worker),
			Namespace:       ops.Namespace,
			Labels:          windowJobLabels(ops),
			OwnerReferences: []metav1.OwnerReference{ownerOf(ops)},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{fdbMaintenanceLabel: worker},
			},
		},
	}
}

// windowHold is the Holding step (§5.2). It records the zone and the process
// groups on the worker, labels the worker's FoundationDB pods, raises the budget
// over them, places the sentinel, and completes once the budget reports those
// pods and a drain of the worker has been observed.
//
// The zone and the process groups are written before anything is created, so
// that a later step acts on what this one saw, and a budget that could not be
// created still leaves the window knowing what it is for.
func (r *ControlPlaneOpsReconciler) windowHold(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	worker := ops.Spec.HostMaintenance.WorkerNode

	if ops.Status.HostMaintenance == nil || ops.Status.HostMaintenance.Zone == "" {
		zone, err := r.zoneOf(ctx, worker)
		if err != nil {
			return false, "", err
		}
		groups, err := r.fdbProcessGroups(ctx, ops.Namespace)
		if err != nil {
			return false, "", err
		}
		var onZone []string
		for _, g := range groups {
			if g.FaultDomain == zone {
				onZone = append(onZone, g.ID)
			}
		}
		err = r.patchWindow(ctx, ops, func(s *simplyblockv1alpha2.HostMaintenanceStatus) {
			s.Zone = zone
			s.ProcessGroups = onZone
		})
		if err != nil {
			return false, "", err
		}
	}

	pods, err := r.fdbPodsOn(ctx, ops.Namespace, worker)
	if err != nil {
		return false, "", err
	}
	for i := range pods {
		if err := r.labelPod(ctx, &pods[i], worker); err != nil {
			return false, "", err
		}
	}

	budget, err := r.ensureBudget(ctx, ops)
	if err != nil {
		return false, "", err
	}
	// The sentinel is one of two drain signals, so a sentinel that cannot be
	// placed costs the window a signal and not its hold: the budget is up, and
	// a neighbor's eviction still completes the wait.
	sentinelNote := ""
	if _, err := ensureJob(ctx, r.Client, sentinelJob(ops, target)); err != nil {
		sentinelNote = fmt.Sprintf("; the sentinel could not be placed (%v)", err)
	}
	if err := r.observeDrain(ctx, ops); err != nil {
		return false, "", err
	}
	if _, err := r.holdingStillOpen(ctx, ops); err != nil {
		return false, "", err
	}

	// The budget has to have been seen by the disruption controller before an
	// eviction is refused by it. A budget just created reports nothing, and a
	// budget that expects fewer pods than are labeled has not caught up.
	reported := budget.Status.ObservedGeneration >= budget.Generation &&
		budget.Status.DisruptionsAllowed == 0 &&
		int(budget.Status.ExpectedPods) >= len(pods)
	if !reported {
		return false, fmt.Sprintf(
			"waiting for the budget over %d FoundationDB pods on %s to report", len(pods), worker), nil
	}
	if ops.Status.HostMaintenance.DrainObservedAt == nil {
		return false, fmt.Sprintf(
			"holding %d FoundationDB pods on %s until a drain of the worker is observed%s",
			len(pods), worker, sentinelNote), nil
	}
	return true, "", nil
}

// windowRelease is the Releasing step (§5.8). It deletes the budget, removes
// the window's label from the pods, takes the sentinel down, and completes once
// the budget is gone. The lock is given back on the way out of this step by the
// controller.
func (r *ControlPlaneOpsReconciler) windowRelease(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (bool, string, error) {
	worker := ops.Spec.HostMaintenance.WorkerNode
	budget := &policyv1.PodDisruptionBudget{}
	key := client.ObjectKey{Namespace: ops.Namespace, Name: windowBudgetName(worker)}
	switch err := r.Get(ctx, key, budget); {
	case apierrors.IsNotFound(err):
		// Gone, which is what the step is for.
	case err != nil:
		return false, "", fmt.Errorf("read the budget of worker %s: %w", worker, err)
	case budget.DeletionTimestamp == nil:
		if err := r.Delete(ctx, budget); err != nil && !apierrors.IsNotFound(err) {
			return false, "", fmt.Errorf("delete the budget of worker %s: %w", worker, err)
		}
		r.emit(ops, corev1.EventTypeNormal, EvictionReleased,
			fmt.Sprintf("the budget over the FoundationDB pods of %s is gone and the drain proceeds", worker))
		if hm := ops.Status.HostMaintenance; hm != nil && hm.DrainObservedAt != nil {
			fdbEvictionHoldSeconds.WithLabelValues(ops.Namespace, string(hm.DrainObservedBy)).
				Observe(time.Since(hm.DrainObservedAt.Time).Seconds())
		}
	}
	if err := r.unlabelPods(ctx, ops.Namespace, worker); err != nil {
		return false, "", err
	}
	if err := r.deleteSentinel(ctx, ops); err != nil {
		return false, "", err
	}

	err := r.Get(ctx, key, budget)
	if apierrors.IsNotFound(err) {
		return true, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("read the budget of worker %s: %w", worker, err)
	}
	return false, fmt.Sprintf("waiting for the budget of %s to be gone", worker), nil
}

// windowCleanup is the last step of a window that succeeded (§5.9): whatever
// marker the release left behind goes.
func (r *ControlPlaneOpsReconciler) windowCleanup(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (bool, string, error) {
	worker := ops.Spec.HostMaintenance.WorkerNode
	if err := r.unlabelPods(ctx, ops.Namespace, worker); err != nil {
		return false, "", err
	}
	if err := r.deleteSentinel(ctx, ops); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// teardownWindow takes a window's markers down from a terminal transition that
// is not Cleanup: a failure, an abort, or a deletion. It is best effort, because
// retrying it forever would leave an operation that can never end, and a
// MaintenanceMarkersLeft event is what says a worker needs a hand. It does
// nothing for any other action.
func (r *ControlPlaneOpsReconciler) teardownWindow(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) {
	if ops.Spec.Action != simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance ||
		ops.Spec.HostMaintenance == nil {
		return
	}
	worker := ops.Spec.HostMaintenance.WorkerNode
	fdbMaintenanceZoneActive.WithLabelValues(ops.Namespace, worker).Set(0)
	var left []string

	budget := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Namespace: ops.Namespace, Name: windowBudgetName(worker),
	}}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = r.Delete(ctx, budget); err == nil || apierrors.IsNotFound(err) {
			err = nil
			break
		}
	}
	if err != nil {
		left = append(left, fmt.Sprintf("the budget %s (%v)", budget.Name, err))
	}
	if err := r.unlabelPods(ctx, ops.Namespace, worker); err != nil {
		left = append(left, fmt.Sprintf("the label on the worker's pods (%v)", err))
	}
	if err := r.deleteSentinel(ctx, ops); err != nil {
		left = append(left, fmt.Sprintf("the sentinel (%v)", err))
	}
	if len(left) > 0 {
		r.emit(ops, corev1.EventTypeWarning, MaintenanceMarkersLeft, fmt.Sprintf(
			"the window for %s ended with markers it could not remove, and the worker will not "+
				"drain until they are: %s", worker, strings.Join(left, "; ")))
	}
}

// zoneOf is the FoundationDB fault domain of a worker, which is its hostname
// label and not its name: the two differ on clouds whose node names are not
// hostnames.
func (r *ControlPlaneOpsReconciler) zoneOf(ctx context.Context, worker string) (string, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: worker}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &terminalStepError{message: fmt.Sprintf(
				"no Node %q to hold: the window names a worker this cluster does not have", worker)}
		}
		return "", fmt.Errorf("read Node %s: %w", worker, err)
	}
	zone := node.Labels[corev1.LabelHostname]
	if zone == "" {
		return "", &terminalStepError{message: fmt.Sprintf(
			"Node %q carries no %s label, which is the fault domain FoundationDB knows the worker by",
			worker, corev1.LabelHostname)}
	}
	return zone, nil
}

// fdbProcessGroups reads status.processGroups of the FoundationDBCluster, in
// the shape the coordinator choice takes.
func (r *ControlPlaneOpsReconciler) fdbProcessGroups(
	ctx context.Context, namespace string,
) ([]fdbProcessGroup, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(fdbClusterGVK)
	key := client.ObjectKey{Namespace: namespace, Name: ComponentFDBCluster}
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read FoundationDBCluster %s: %w", ComponentFDBCluster, err)
	}
	entries, _, err := unstructured.NestedSlice(obj.Object, "status", "processGroups")
	if err != nil {
		return nil, fmt.Errorf("read status.processGroups of %s: %w", ComponentFDBCluster, err)
	}
	groups := make([]fdbProcessGroup, 0, len(entries))
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["processGroupID"].(string)
		class, _ := m["processClass"].(string)
		zone, _ := m["faultDomain"].(string)
		conditions, _ := m["processGroupConditions"].([]any)
		_, removing := m["removalTimestamp"]
		missing := false
		for _, condition := range conditions {
			if cm, ok := condition.(map[string]any); ok && cm["conditionType"] == "MissingProcesses" {
				missing = true
			}
		}
		groups = append(groups, fdbProcessGroup{
			ID: id, Class: class, FaultDomain: zone,
			Healthy: len(conditions) == 0 && !removing,
			Missing: missing,
		})
	}
	return groups, nil
}

// fdbPodsOn lists the FoundationDB pods on a worker.
func (r *ControlPlaneOpsReconciler) fdbPodsOn(
	ctx context.Context, namespace, worker string,
) ([]corev1.Pod, error) {
	var pods corev1.PodList
	err := r.List(ctx, &pods, client.InNamespace(namespace), client.HasLabels{utils.LabelFDBClusterName})
	if err != nil {
		return nil, fmt.Errorf("list the FoundationDB pods: %w", err)
	}
	var on []corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == worker {
			on = append(on, pods.Items[i])
		}
	}
	return on, nil
}

// labelPod puts the window's label on a pod, once.
func (r *ControlPlaneOpsReconciler) labelPod(ctx context.Context, pod *corev1.Pod, worker string) error {
	if pod.Labels[fdbMaintenanceLabel] == worker {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[fdbMaintenanceLabel] = worker
	if err := r.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("label pod %s on worker %s: %w", pod.Name, worker, err)
	}
	return nil
}

// unlabelPods removes the window's label from every pod carrying it for the
// worker. The storage node's label on the same pods is not this window's.
func (r *ControlPlaneOpsReconciler) unlabelPods(ctx context.Context, namespace, worker string) error {
	var pods corev1.PodList
	err := r.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{fdbMaintenanceLabel: worker})
	if err != nil {
		return fmt.Errorf("list the pods held for worker %s: %w", worker, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		patch := client.MergeFrom(pod.DeepCopy())
		delete(pod.Labels, fdbMaintenanceLabel)
		if err := r.Patch(ctx, pod, patch); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("unlabel pod %s on worker %s: %w", pod.Name, worker, err)
		}
	}
	return nil
}

// ensureBudget creates the window's budget when it is not there, emitting
// EvictionHeld once, and returns the budget as it is.
func (r *ControlPlaneOpsReconciler) ensureBudget(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (*policyv1.PodDisruptionBudget, error) {
	worker := ops.Spec.HostMaintenance.WorkerNode
	desired := windowBudget(ops)
	existing := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read the budget of worker %s: %w", worker, err)
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
				return nil, fmt.Errorf("read the budget of worker %s: %w", worker, err)
			}
			return existing, nil
		}
		return nil, fmt.Errorf("create the budget of worker %s: %w", worker, err)
	}
	r.emit(ops, corev1.EventTypeNormal, EvictionHeld,
		fmt.Sprintf("the eviction of the FoundationDB pods of %s is held by budget %s", worker, desired.Name))
	return desired, nil
}

// deleteSentinel takes the sentinel Job and its pod down.
func (r *ControlPlaneOpsReconciler) deleteSentinel(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: ops.Namespace, Name: fdbJobName(ops.Name, fdbCommandSentinel),
	}}
	err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the sentinel of %s: %w", ops.Name, err)
	}
	return nil
}

// patchWindow writes one change to status.hostMaintenance.
func (r *ControlPlaneOpsReconciler) patchWindow(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	mutate func(*simplyblockv1alpha2.HostMaintenanceStatus),
) error {
	base := ops.DeepCopy()
	if ops.Status.HostMaintenance == nil {
		ops.Status.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceStatus{}
	}
	mutate(ops.Status.HostMaintenance)
	if err := r.Status().Patch(ctx, ops, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record the window's status: %w", err)
	}
	return nil
}
