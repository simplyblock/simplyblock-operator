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
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/simplyblock/atlas/kube"
	atlaslvol "github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

// nfsExportMDSBootRequeue is how often a waiting export looks at a metadata
// server pod that is scheduled but whose guest has not turned healthy. The
// guest boots within its two-minute deadline, so the binding should follow
// within seconds of it.
const nfsExportMDSBootRequeue = 10 * time.Second

// The metadata server's workload, applied on a storage cluster's first export,
// and the driver that configures it.
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies;validatingadmissionpolicybindings,verbs=get;create
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;create
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=get;list;watch

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

	if err := r.ensureMDS(ctx, d, handle.ClusterID); err != nil {
		var wait *mdsWaitError
		if errors.As(err, &wait) {
			// Not retried hot: nothing changes until somebody acts on the
			// reason, so the export waits and looks again later.
			r.event(export, corev1.EventTypeWarning, wait.reason,
				fmt.Sprintf("cannot create the metadata server for storage cluster %s: %v", handle.ClusterID, err))
			return r.waitForMDS(ctx, export, err.Error(), nfsExportNoHostRequeue)
		}
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
// That includes its state disk's class, which is chosen only here, before the
// StatefulSet exists, because a claim template cannot be changed afterward.
func (r *NFSExportReconciler) ensureMDS(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, clusterID string,
) error {
	var existing appsv1.StatefulSet
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: driver.MDSStatefulSetName(d, clusterID)}
	switch err := r.Get(ctx, key, &existing); {
	case err == nil:
		// The name carries a digest of the cluster ID, so another cluster's
		// StatefulSet here is all but impossible, and binding to it would
		// serve this cluster's exports from the other cluster's guest.
		if owner := existing.Labels[driver.MDSClusterLabel]; owner != clusterID {
			return &mdsWaitError{reason: "MDSNameCollision", message: fmt.Sprintf(
				"StatefulSet %s belongs to storage cluster %q, not %s", key.Name, owner, clusterID)}
		}
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("reading the metadata server StatefulSet %s: %w", key.Name, err)
	}

	// Enforced here as well as at admission, which ignores its own failures.
	if problem := driver.MDSResourcesProblem(d.Spec.PNFS.MDS); problem != "" {
		return &mdsWaitError{reason: "MDSResourcesInvalid", message: problem}
	}
	stateClass, err := r.mdsStateClass(ctx, d, clusterID)
	if err != nil {
		return err
	}
	sa, sts, err := driver.MDSObjects(d, clusterID, stateClass)
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

// mdsWaitError is a reason the metadata server cannot be created that no
// retry fixes: the export waits, with reason as the event's, rather than
// failing hot, since what it takes to proceed is somebody acting on it.
type mdsWaitError struct{ reason, message string }

func (e *mdsWaitError) Error() string { return e.message }

// stateUnavailable is the mdsWaitError of a state disk class that cannot be
// used.
func stateUnavailable(format string, args ...any) error {
	return &mdsWaitError{reason: "MDSStateUnavailable", message: fmt.Sprintf(format, args...)}
}

// mdsStateClass is the StorageClass of the metadata server's state disk, which
// is always a simplyblock volume. A node-local disk would pin the pod to the
// node it first ran on, and the client-recovery database on it is what lets
// NFS clients reclaim their state when the pod comes back somewhere else.
//
// A class named in spec.pnfs.mds.stateStorageClassName is used when this
// driver provisions it. Otherwise, the storage cluster gets a class of its
// own (driver.MDSStateClass), reserved for the state disk by an admission
// policy: one known class rather than whichever user class happens to exist,
// and none of the caps a user class carries. It is derived from the cluster's
// own class, since the metadata server serves exports of that cluster only
// and keeping its state there adds no failure the exports do not already
// have. Among the cluster's classes one the operator wrote for a pool is
// preferred, and the first by name otherwise, so the choice is stable.
//
// pNFS classes are never chosen: they provision an export, not a block device.
func (r *NFSExportReconciler) mdsStateClass(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, clusterID string,
) (string, error) {
	provisioner := driver.DriverName(d)

	if named := d.Spec.PNFS.MDS.StateStorageClassName; named != nil && *named != "" {
		var sc storagev1.StorageClass
		switch err := r.Get(ctx, client.ObjectKey{Name: *named}, &sc); {
		case apierrors.IsNotFound(err):
			return "", stateUnavailable("the state disk's storage class %q does not exist", *named)
		case err != nil:
			return "", fmt.Errorf("reading storage class %s: %w", *named, err)
		}
		if !isSimplyblockBlockClass(&sc, provisioner) {
			return "", stateUnavailable(
				"the state disk's storage class %q is not a simplyblock block volume class "+
					"(provisioner %q, fstype %q); name a class of %s", *named, sc.Provisioner,
				sc.Parameters[kube.ParamFSType], provisioner)
		}
		return *named, nil
	}

	// The policy goes first, so the class never exists unreserved.
	if err := r.ensureStatePolicy(ctx, d); err != nil {
		return "", err
	}
	name := driver.MDSStateClassName(d, clusterID)
	var existing storagev1.StorageClass
	switch err := r.Get(ctx, client.ObjectKey{Name: name}, &existing); {
	case err == nil:
		return name, nil
	case !apierrors.IsNotFound(err):
		return "", fmt.Errorf("reading storage class %s: %w", name, err)
	}

	source, err := r.clusterBlockClass(ctx, provisioner, clusterID)
	if err != nil {
		return "", err
	}
	if err := r.Create(ctx, driver.MDSStateClass(d, clusterID, source)); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating storage class %s: %w", name, err)
	}
	return name, nil
}

// clusterBlockClass is the simplyblock block class of a storage cluster the
// state disk's class is derived from.
func (r *NFSExportReconciler) clusterBlockClass(
	ctx context.Context, provisioner, clusterID string,
) (*storagev1.StorageClass, error) {
	var classes storagev1.StorageClassList
	if err := r.List(ctx, &classes); err != nil {
		return nil, fmt.Errorf("listing storage classes: %w", err)
	}
	var candidates []*storagev1.StorageClass
	for i := range classes.Items {
		sc := &classes.Items[i]
		if isSimplyblockBlockClass(sc, provisioner) && sc.Parameters[kube.ParamClusterID] == clusterID &&
			!strings.HasSuffix(sc.Name, driver.MDSStateClassSuffix) {
			candidates = append(candidates, sc)
		}
	}
	if len(candidates) == 0 {
		return nil, stateUnavailable(
			"no storage class of %s provisions block volumes on storage cluster %s to derive the "+
				"state disk's class from; create one, or name one in spec.pnfs.mds.stateStorageClassName",
			provisioner, clusterID)
	}
	slices.SortFunc(candidates, func(a, b *storagev1.StorageClass) int {
		if am, bm := pool.IsOperatorManaged(a), pool.IsOperatorManaged(b); am != bm {
			if am {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return candidates[0], nil
}

// ensureStatePolicy creates the admission policy reserving the state disk
// classes, and its binding, when they are absent. Existing ones are left as
// they are: an administrator who changed them meant to.
func (r *NFSExportReconciler) ensureStatePolicy(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) error {
	policy, binding := driver.MDSStatePolicy(d, r.OperatorNamespace)
	for _, obj := range []client.Object{policy, binding} {
		if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// isSimplyblockBlockClass reports whether sc provisions a simplyblock block
// volume: this driver is its provisioner, and it is not a pNFS class.
func isSimplyblockBlockClass(sc *storagev1.StorageClass, provisioner string) bool {
	return sc.Provisioner == provisioner && sc.Parameters[kube.ParamFSType] != kube.FSTypePNFS
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
