// Binding an export to the metadata server pod of its storage cluster
// (design-pnfs-mds-vm.md §7.3).
//
// It lives beside the reconciler rather than in it because it is the one part
// of binding that creates workload objects: the StatefulSet the driver package
// renders, which this reconciler applies on a storage cluster's first export
// and then leaves alone. Everything after the pod is chosen is bindExport's.

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;create
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=get;list;watch
// The attachment to the storage data network, when the driver names one: the
// NetworkAttachmentDefinition, and the nodes whose subnet labels choose its
// host interface.
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;create;update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// mdsDriver returns the driver that configures the metadata server, or nil when
// none in the operator's namespace does. More than one such driver is a
// misconfiguration this does not try to resolve: the first by name wins, so
// the choice is at least stable.
func (r *NFSExportReconciler) mdsDriver(ctx context.Context) (*simplyblockv1alpha2.SimplyblockDriver, error) {
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

// bindMDS brings up the export's storage cluster's metadata server and binds
// the export to it once its guest is healthy.
func (r *NFSExportReconciler) bindMDS(
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

	updated, err := r.ensureMDS(ctx, export, d, handle.ClusterID)
	if updated {
		r.event(export, corev1.EventTypeNormal, "MDSUpdated",
			fmt.Sprintf("updated the metadata server StatefulSet %s to this release's pod template, "+
				"which restarts its guest", driver.MDSStatefulSetName(d, handle.ClusterID)))
	}
	if err != nil {
		var wait *mdsWaitError
		if errors.As(err, &wait) {
			// Not retried hot: nothing changes until somebody acts on the
			// reason, so the export waits and looks again later.
			r.event(export, corev1.EventTypeWarning, wait.reason,
				fmt.Sprintf("cannot bring up the metadata server for storage cluster %s: %v", handle.ClusterID, err))
			return r.waitForMDS(ctx, export, err.Error(), nfsExportNoHostRequeue)
		}
		r.event(export, corev1.EventTypeWarning, "MDSUnavailable",
			fmt.Sprintf("cannot bring up the metadata server for storage cluster %s: %v", handle.ClusterID, err))
		return ctrl.Result{}, err
	}

	podName := driver.MDSPodName(d, handle.ClusterID, 0)
	var pod corev1.Pod
	err = r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: podName}, &pod)
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
// removes them, and reports whether it rewrote an existing StatefulSet.
//
// An existing StatefulSet whose pod template is not the one this release
// builds, by MDSTemplateHash, gets the current template. That restarts the
// guest, and every export of the cluster rides out the restart through the
// guest's grace period. Without it, an upgrade leaves the metadata server on
// the image of the release that created it.
func (r *NFSExportReconciler) ensureMDS(
	ctx context.Context, export *simplyblockv1alpha2.NFSExport, d *simplyblockv1alpha2.SimplyblockDriver,
	clusterID string,
) (updated bool, err error) {
	var existing appsv1.StatefulSet
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: driver.MDSStatefulSetName(d, clusterID)}
	switch err := r.Get(ctx, key, &existing); {
	case err == nil:
		// The name carries a digest of the cluster ID, so another cluster's
		// StatefulSet here is all but impossible, and binding to it would
		// serve this cluster's exports from the other cluster's guest.
		if owner := existing.Labels[driver.MDSClusterLabel]; owner != clusterID {
			return false, &mdsWaitError{reason: "MDSNameCollision", message: fmt.Sprintf(
				"StatefulSet %s belongs to storage cluster %q, not %s", key.Name, owner, clusterID)}
		}
		return r.updateMDS(ctx, export, d, &existing)
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("reading the metadata server StatefulSet %s: %w", key.Name, err)
	}

	// Enforced here as well as at admission, which ignores its own failures.
	if problem := driver.MDSResourcesProblem(d.Spec.PNFS.MDS); problem != "" {
		return false, &mdsWaitError{reason: "MDSResourcesInvalid", message: problem}
	}
	data, err := r.mdsDataAttachment(ctx, export, d)
	if err != nil {
		return false, err
	}
	sa, sts, err := driver.MDSObjects(d, clusterID, data)
	if err != nil {
		return false, err
	}
	for _, obj := range []client.Object{sa, sts} {
		if err := controllerutil.SetControllerReference(d, obj, r.Scheme); err != nil {
			return false, fmt.Errorf("owning %s by the driver: %w", obj.GetName(), err)
		}
		if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("creating %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return false, nil
}

// updateMDS gives an existing metadata server StatefulSet the pod template this
// release builds, when its recorded hash says it has another one. Only the
// template and the hash are written: the selector cannot be changed.
func (r *NFSExportReconciler) updateMDS(
	ctx context.Context, export *simplyblockv1alpha2.NFSExport, d *simplyblockv1alpha2.SimplyblockDriver,
	existing *appsv1.StatefulSet,
) (bool, error) {
	// Enforced on an update as on a create: limits the runner refuses would
	// replace a running guest with one that never boots.
	if problem := driver.MDSResourcesProblem(d.Spec.PNFS.MDS); problem != "" {
		return false, &mdsWaitError{reason: "MDSResourcesInvalid", message: problem}
	}
	data, err := r.mdsDataAttachment(ctx, export, d)
	if err != nil {
		return false, err
	}
	return driver.UpdateMDS(ctx, r.Client, d, existing, data)
}

// mdsDataAttachment is the metadata server's attachment to the storage data
// network, or nil for one on the pod network alone. A data network the
// cluster cannot attach is reported on the export and the metadata server
// runs on the pod network, which works wherever the nodes forward traffic on
// their data interfaces.
func (r *NFSExportReconciler) mdsDataAttachment(
	ctx context.Context, export *simplyblockv1alpha2.NFSExport, d *simplyblockv1alpha2.SimplyblockDriver,
) (*driver.MDSDataAttachment, error) {
	data, err := driver.EnsureMDSDataNetwork(ctx, r.Client, d)
	var fallback *driver.MDSDataNetworkFallback
	if errors.As(err, &fallback) {
		r.event(export, corev1.EventTypeWarning, fallback.Reason, fallback.Message)
		return nil, nil
	}
	return data, err
}

// refreshMDS brings a Ready export's metadata server StatefulSet up to this
// release's pod template. Without it an upgrade reaches the StatefulSet only
// through a Pending export, and on a stable cluster none is. It never creates
// the StatefulSet, which is the Pending path's, and it never moves the export:
// the guest restart that follows is handled by resyncAfterRestart.
//
// Its cost on every Ready reconcile is the cached driver list, one Get, and a
// hash compare.
func (r *NFSExportReconciler) refreshMDS(ctx context.Context, export *simplyblockv1alpha2.NFSExport) error {
	d, err := r.mdsDriver(ctx)
	if err != nil || d == nil {
		return err
	}
	handle, ok := atlaslvol.ParseHandle(atlaslvol.VolumeHandle(export.Spec.VolumeRef))
	if !ok {
		return nil
	}
	var existing appsv1.StatefulSet
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: driver.MDSStatefulSetName(d, handle.ClusterID)}
	switch err := r.Get(ctx, key, &existing); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading the metadata server StatefulSet %s: %w", key.Name, err)
	}
	if existing.Labels[driver.MDSClusterLabel] != handle.ClusterID {
		return nil
	}
	updated, err := r.updateMDS(ctx, export, d, &existing)
	var wait *mdsWaitError
	switch {
	case errors.As(err, &wait):
		// The template cannot be built. The running guest keeps serving.
		r.event(export, corev1.EventTypeWarning, wait.reason,
			fmt.Sprintf("cannot update the metadata server StatefulSet %s: %v", key.Name, err))
		return nil
	case err != nil:
		return err
	case updated:
		r.event(export, corev1.EventTypeNormal, "MDSUpdated",
			fmt.Sprintf("updated the metadata server StatefulSet %s to this release's pod template, "+
				"which restarts its guest", key.Name))
	}
	return nil
}

// mdsWaitError is a reason the metadata server cannot be created that no
// retry fixes: the export waits, with reason as the event's, rather than
// failing hot, since what it takes to proceed is somebody acting on it.
type mdsWaitError struct{ reason, message string }

func (e *mdsWaitError) Error() string { return e.message }

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

// unschedulable reports why a pod cannot be scheduled: no node offering KVM.
func unschedulable(pod *corev1.Pod) (reason, message string, blocked bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
			c.Reason == corev1.PodReasonUnschedulable {
			return "NoKVMCapableNode", c.Message, true
		}
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
