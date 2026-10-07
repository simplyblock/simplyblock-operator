// Binding an export to the metadata server pod of its storage cluster, when
// the driver runs the metadata server in a pod instead of on labeled nodes
// (design-pnfs-mds-vm.md §7.3).
//
// It lives beside the reconciler rather than in it because it is the one part
// of binding that creates workload objects: the StatefulSet the driver package
// renders, which this reconciler applies on a storage cluster's first export
// and then leaves alone. Everything after the host is chosen is the node-hosted
// path's own bindExport.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	atlaslvol "github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
)

// nfsExportMDSBootRequeue is how often a waiting export looks at a metadata
// server pod that is scheduled but whose guest has not turned healthy. The
// guest boots within its two-minute deadline, so the binding should follow
// within seconds of it.
const nfsExportMDSBootRequeue = 10 * time.Second

// The metadata server's workload, applied on a storage cluster's first export,
// and the driver that configures it.
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;create
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=get;list;watch

// podHostedDriver returns the driver that runs the metadata server in a pod, or
// nil when none in the operator's namespace does. More than one such driver is
// a misconfiguration this does not try to resolve: the first by name wins, so
// the choice is at least stable.
func (r *NFSExportReconciler) podHostedDriver(ctx context.Context) (*simplyblockv1alpha2.SimplyblockDriver, error) {
	if r.OperatorNamespace == "" {
		return nil, nil
	}
	var list simplyblockv1alpha2.SimplyblockDriverList
	if err := r.List(ctx, &list, client.InNamespace(r.OperatorNamespace)); err != nil {
		return nil, fmt.Errorf("listing drivers: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b simplyblockv1alpha2.SimplyblockDriver) int {
		return strings.Compare(a.Name, b.Name)
	})
	for i := range list.Items {
		if list.Items[i].Spec.PNFS.MDS != nil {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// reconcilePendingPodHosted brings up the export's storage cluster's metadata
// server and binds the export to it once its guest is healthy.
func (r *NFSExportReconciler) reconcilePendingPodHosted(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	machine *statemachine.Machine[phase],
	logger interface{ Info(string, ...any) },
	d *simplyblockv1alpha2.SimplyblockDriver,
) (ctrl.Result, error) {
	handle, ok := atlaslvol.ParseHandle(atlaslvol.VolumeHandle(export.Spec.VolumeRef))
	if !ok {
		// Admission validates the pattern, so this is a record nothing can
		// serve rather than one to retry.
		return ctrl.Result{}, r.toDegraded(ctx, export, machine,
			fmt.Sprintf("volumeRef %q names no storage cluster", export.Spec.VolumeRef))
	}

	if err := r.ensureMDS(ctx, d, handle.ClusterID); err != nil {
		r.event(export, corev1.EventTypeWarning, "MDSUnavailable",
			fmt.Sprintf("cannot create the metadata server for storage cluster %s: %v", handle.ClusterID, err))
		return ctrl.Result{}, err
	}

	podName := driver.MDSPodName(d, handle.ClusterID)
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: podName}, &pod)
	switch {
	case apierrors.IsNotFound(err):
		return r.waitForMDS(ctx, export, fmt.Sprintf("waiting for the metadata server pod %s", podName),
			nfsExportMDSBootRequeue)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading metadata server pod %s: %w", podName, err)
	}

	if reason, message, blocked := unschedulable(&pod); blocked {
		// A refusal owes an event, or it looks like a reconcile that never ran.
		r.event(export, corev1.EventTypeWarning, reason,
			fmt.Sprintf("metadata server pod %s cannot be scheduled: %s", podName, message))
		return r.waitForMDS(ctx, export, fmt.Sprintf("metadata server pod %s cannot be scheduled", podName),
			nfsExportNoHostRequeue)
	}
	if !podReady(&pod) || pod.Status.PodIP == "" {
		return r.waitForMDS(ctx, export,
			fmt.Sprintf("waiting for the metadata server guest in %s to turn healthy", podName),
			nfsExportMDSBootRequeue)
	}

	return r.bindExport(ctx, export, machine, logger, mdsBinding{pod: pod.Name, address: pod.Status.PodIP})
}

// ensureMDS creates the storage cluster's metadata server ServiceAccount and
// StatefulSet when they are absent, owned by the driver so that removing it
// removes them. An existing one is left alone: changing the StatefulSet
// restarts the guest, which costs every export of the cluster an outage.
func (r *NFSExportReconciler) ensureMDS(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, clusterID string,
) error {
	sa, sts, err := driver.MDSObjects(d, clusterID)
	if err != nil {
		return err
	}
	for _, obj := range []client.Object{sa, sts} {
		if err := controllerutil.SetControllerReference(d, obj, r.Scheme); err != nil {
			return fmt.Errorf("owning %s by the driver: %w", obj.GetName(), err)
		}
		if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// waitForMDS records why the export is not bound yet and comes back later.
func (r *NFSExportReconciler) waitForMDS(
	ctx context.Context, export *simplyblockv1alpha2.NFSExport, message string, after time.Duration,
) (ctrl.Result, error) {
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.Phase = phasePending
		s.Message = message
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// unschedulable reports why a pod cannot be scheduled, as the event reason the
// design names: its state disk not binding, or, otherwise, no node offering
// KVM.
func unschedulable(pod *corev1.Pod) (reason, message string, blocked bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse ||
			c.Reason != corev1.PodReasonUnschedulable {
			continue
		}
		if strings.Contains(strings.ToLower(c.Message), "persistentvolumeclaim") {
			return "MDSStateUnavailable", c.Message, true
		}
		return "NoKVMCapableNode", c.Message, true
	}
	return "", "", false
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
