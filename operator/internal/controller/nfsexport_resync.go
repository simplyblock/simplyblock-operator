// Reassembling a pod-hosted export after its metadata server pod restarted
// (design-pnfs-mds-vm.md §7.5).
//
// A restarted guest is a cold boot: its mounts, exports table, and nfsd
// threads are gone, and only the client-recovery database on its state disk is
// left. Nothing has to ask the guest to find that out. The export records the
// pod instance that assembled it, and a different running instance means the
// export is not being served until it is assembled again. A pod event wakes the
// exports bound to the pod, so this happens when the pod comes back rather
// than at the next health poll.

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// resyncPodHosted reassembles a Ready pod-hosted export whose metadata server
// pod is not the instance that assembled it. handled is false when there is
// nothing to resync, and the caller carries on with its health check.
func (r *NFSExportReconciler) resyncPodHosted(
	ctx context.Context, export *simplyblockv1alpha2.NFSExport,
) (result ctrl.Result, handled bool, err error) {
	name := export.Status.MDSPodName
	if name == "" {
		return ctrl.Result{}, false, nil
	}
	var pod corev1.Pod
	switch err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: name}, &pod); {
	case apierrors.IsNotFound(err):
		// The StatefulSet brings it back, and its event wakes this export.
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, true, nil
	case err != nil:
		return ctrl.Result{}, true, fmt.Errorf("reading metadata server pod %s: %w", name, err)
	}
	if string(pod.UID) == export.Status.AssembledBy {
		return ctrl.Result{}, false, nil
	}

	// The address first, so that a client retrying against the Service
	// reaches the new guest as soon as it serves.
	if ip := pod.Status.PodIP; ip != "" && ip != export.Status.MDSNodeIP {
		if _, err := r.reconcileExportService(ctx, export, ip); err != nil {
			return ctrl.Result{}, true, fmt.Errorf("repointing the Service for %s: %w", export.Name, err)
		}
		if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
			s.MDSNodeIP = ip
		}); err != nil {
			return ctrl.Result{}, true, err
		}
		r.event(export, corev1.EventTypeNormal, "MDSAddressChanged",
			fmt.Sprintf("metadata server pod %s moved to %s", name, ip))
	}

	host := mdsHost(export)
	if r.Assembler == nil || !r.Assembler.HasSession(host) {
		// The restarted guest has not linked yet, which it does once healthy.
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, true, nil
	}
	if err := r.Assembler.CreateExport(ctx, r.exportHost(ctx, export, host), export); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("reassembling export on %s: %w", host, err)
	}
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.AssembledBy = string(pod.UID)
	}); err != nil {
		return ctrl.Result{}, true, err
	}
	r.event(export, corev1.EventTypeNormal, "MDSResynced",
		fmt.Sprintf("metadata server pod %s restarted, export reassembled", name))
	return ctrl.Result{RequeueAfter: nfsExportHealthCheckInterval}, true, nil
}

// assemblingPodUID is the UID of the metadata server pod a pod-hosted export
// is assembled by, or empty for a node-hosted export or a pod that cannot be
// read. Recorded with the assembly, it is what a restart is detected against.
func (r *NFSExportReconciler) assemblingPodUID(ctx context.Context, export *simplyblockv1alpha2.NFSExport) string {
	name := export.Status.MDSPodName
	if name == "" {
		return ""
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: name}, &pod); err != nil {
		return ""
	}
	return string(pod.UID)
}

// exportsBoundToPod maps a metadata server pod to the exports bound to it.
// Exports are few, so they are listed rather than indexed.
func (r *NFSExportReconciler) exportsBoundToPod(ctx context.Context, pod *corev1.Pod) []reconcile.Request {
	if pod.Namespace != r.OperatorNamespace {
		return nil
	}
	var exports simplyblockv1alpha2.NFSExportList
	if err := r.List(ctx, &exports); err != nil {
		log.FromContext(ctx).Error(err, "listing exports for a metadata server pod event", "pod", pod.Name)
		return nil
	}
	var requests []reconcile.Request
	for _, e := range exports.Items {
		if e.Status.MDSPodName == pod.Name {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&e)})
		}
	}
	return requests
}
