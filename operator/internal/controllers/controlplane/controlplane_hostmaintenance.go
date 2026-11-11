// How the ControlPlane controller raises a HostMaintenance window, and calls
// it off.
//
// The trigger is a worker being cordoned while it runs FoundationDB pods, which
// the controller sees by watching Kubernetes Node objects. The window is a
// ControlPlaneOps named for the control plane and the worker, so a second pass
// over the same cordon finds the one already raised. A pool-wide cordon raises
// one window per FoundationDB host, and every one of them starts at once.
//
// An uncordon is answered the way the storage node's window answers it. A
// window still Holding has done nothing to the database and is aborted. From
// EnteringMaintenance on it runs on, because something has to clear what it
// set. A window in a terminal phase is deleted, so the next cordon of the same
// worker raises a fresh one. A worker that is replaced rather than uncordoned
// has its Node deleted, and a deleted Node is treated as an uncordon for the
// terminal record.
//
// design-fdb-host-maintenance.md §5.1 and §7.2 are the specification.

package controlplane

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// windowName is the window of a control plane for a worker: the control
// plane's name, `-maintenance-`, and the first eight hex digits of the
// worker's SHA-256, which keeps the name a DNS label under a worker named by a
// fully qualified domain name.
func windowName(cp *simplyblockv1alpha2.ControlPlane, worker string) string {
	return cp.Name + "-maintenance-" + hash8(worker)
}

// reconcileHostMaintenance raises a window for every cordoned worker that runs
// a FoundationDB pod, and answers the uncordon of every worker that has one.
func (r *ControlPlaneReconciler) reconcileHostMaintenance(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) error {
	var pods corev1.PodList
	err := r.List(ctx, &pods, client.InNamespace(cp.Namespace), client.HasLabels{utils.LabelFDBClusterName})
	if err != nil {
		return fmt.Errorf("list the FoundationDB pods: %w", err)
	}
	fdbHosts := map[string]bool{}
	for i := range pods.Items {
		if worker := pods.Items[i].Spec.NodeName; worker != "" {
			fdbHosts[worker] = true
		}
	}

	var list simplyblockv1alpha2.ControlPlaneOpsList
	if err := r.List(ctx, &list, client.InNamespace(cp.Namespace)); err != nil {
		return fmt.Errorf("list the operations in %s: %w", cp.Namespace, err)
	}
	windows := map[string]*simplyblockv1alpha2.ControlPlaneOps{}
	for i := range list.Items {
		w := &list.Items[i]
		if w.Spec.Action == simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance &&
			w.Spec.HostMaintenance != nil && w.Spec.ControlPlaneRef == cp.Name {
			windows[w.Spec.HostMaintenance.WorkerNode] = w
		}
	}

	workers := map[string]bool{}
	for worker := range fdbHosts {
		workers[worker] = true
	}
	for worker := range windows {
		workers[worker] = true
	}

	for worker := range workers {
		var node corev1.Node
		err := r.Get(ctx, client.ObjectKey{Name: worker}, &node)
		switch {
		case apierrors.IsNotFound(err):
			// A replaced worker: its Node is gone, and that is its uncordon.
		case err != nil:
			return fmt.Errorf("read Node %s: %w", worker, err)
		}
		cordoned := err == nil && node.Spec.Unschedulable

		window := windows[worker]
		if cordoned {
			if window == nil && fdbHosts[worker] {
				if err := r.raiseWindow(ctx, cp, worker); err != nil {
					return err
				}
			}
			// A window that exists under a standing cordon is left as it is,
			// whatever its phase: one window per cordon, and a failed one is
			// not retried until the worker is uncordoned.
			continue
		}
		if window == nil {
			continue
		}
		if err := r.releaseWindow(ctx, window); err != nil {
			return err
		}
	}
	return nil
}

// raiseWindow creates the window and says so on the control plane.
func (r *ControlPlaneReconciler) raiseWindow(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane, worker string,
) error {
	window := &simplyblockv1alpha2.ControlPlaneOps{
		ObjectMeta: metav1.ObjectMeta{Name: windowName(cp, worker), Namespace: cp.Namespace},
		Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
			ControlPlaneRef: cp.Name,
			Action:          simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance,
			HostMaintenance: &simplyblockv1alpha2.HostMaintenanceSpec{WorkerNode: worker},
		},
	}
	if err := r.Create(ctx, window); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("raise the maintenance window for %s: %w", worker, err)
	}
	r.emit(cp, corev1.EventTypeNormal, HostMaintenanceRaised, fmt.Sprintf(
		"worker %s is cordoned and runs FoundationDB processes; window %s holds them until a "+
			"drain of the worker has had the coordinators moved off it", worker, window.Name))
	return nil
}

// releaseWindow answers the uncordon of a worker that has a window: a finished
// one is deleted, one still Holding is aborted, and one past Holding runs on.
func (r *ControlPlaneReconciler) releaseWindow(
	ctx context.Context, window *simplyblockv1alpha2.ControlPlaneOps,
) error {
	switch {
	case terminalOps(window.Status.Phase):
		if err := r.Delete(ctx, window); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete the finished window %s: %w", window.Name, err)
		}
	case window.Status.Step.State == string(stepHolding) || window.Status.Step.State == "":
		if window.Spec.Abort {
			return nil
		}
		patch := client.MergeFrom(window.DeepCopy())
		window.Spec.Abort = true
		if err := r.Patch(ctx, window, patch); err != nil {
			return fmt.Errorf("call off the window %s: %w", window.Name, err)
		}
	}
	return nil
}

// nodeRequests maps a Node to the control plane whose namespace holds a
// FoundationDB pod on it or a window for it, so that a cordon, an uncordon, and
// a deletion each reach the raise.
func (r *ControlPlaneReconciler) nodeRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	namespaces := map[string]bool{}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.HasLabels{utils.LabelFDBClusterName}); err == nil {
		for i := range pods.Items {
			if pods.Items[i].Spec.NodeName == node.Name {
				namespaces[pods.Items[i].Namespace] = true
			}
		}
	}
	var windows simplyblockv1alpha2.ControlPlaneOpsList
	if err := r.List(ctx, &windows); err == nil {
		for i := range windows.Items {
			w := &windows.Items[i]
			if w.Spec.Action == simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance &&
				w.Spec.HostMaintenance != nil && w.Spec.HostMaintenance.WorkerNode == node.Name {
				namespaces[w.Namespace] = true
			}
		}
	}

	requests := make([]reconcile.Request, 0, len(namespaces))
	for namespace := range namespaces {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: namespace, Name: SingletonName},
		})
	}
	return requests
}
