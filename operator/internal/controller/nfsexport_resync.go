// Reassembling an export after its metadata server pod restarted
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
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// resyncAfterRestart reassembles a Ready export whose metadata server pod is
// not the instance that assembled it. handled is false when there is nothing to
// resync, and the caller carries on with its health check.
func (r *NFSExportReconciler) resyncAfterRestart(
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
		if err := r.withdrawAddress(ctx, export); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, true, nil
	case err != nil:
		return ctrl.Result{}, true, fmt.Errorf("reading metadata server pod %s: %w", name, err)
	}
	if !pod.DeletionTimestamp.IsZero() {
		// Going away: its address stops taking connections now, not when the
		// replacement has one.
		if err := r.withdrawAddress(ctx, export); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, true, nil
	}
	// The address first, and on every reconcile rather than only after a
	// restart: an export bound while its pod was being replaced records the old
	// pod's address and is then assembled by the new one, so its instance
	// matches while its address does not. A client retrying against the
	// Service reaches the running guest as soon as it serves.
	if err := r.followAddress(ctx, export, &pod); err != nil {
		return ctrl.Result{}, true, err
	}
	if mdsInstance(&pod) == export.Status.AssembledBy {
		return ctrl.Result{}, false, nil
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
		s.AssembledBy = mdsInstance(&pod)
	}); err != nil {
		return ctrl.Result{}, true, err
	}
	r.event(export, corev1.EventTypeNormal, "MDSResynced",
		fmt.Sprintf("metadata server pod %s restarted, export reassembled", name))
	return ctrl.Result{RequeueAfter: nfsExportHealthCheckInterval}, true, nil
}

// followAddress points the export's Service and status at the running pod's
// address when they name another one, and has the nodes forget their flows to
// the old address.
func (r *NFSExportReconciler) followAddress(
	ctx context.Context, export *simplyblockv1alpha2.NFSExport, pod *corev1.Pod,
) error {
	if ip := pod.Status.PodIP; ip != "" && ip != export.Status.MDSNodeIP {
		old := export.Status.MDSNodeIP
		if _, err := r.reconcileExportService(ctx, export, ip); err != nil {
			return fmt.Errorf("repointing the Service for %s: %w", export.Name, err)
		}
		if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
			s.MDSNodeIP = ip
		}); err != nil {
			return err
		}
		r.event(export, corev1.EventTypeNormal, "MDSAddressChanged",
			fmt.Sprintf("metadata server pod %s moved to %s", pod.Name, ip))
		// A connection that reached the old address before it was withdrawn
		// stays translated to it, so the nodes forget it once more now. When
		// the CNI gave the replacement the same address, those flows reach the
		// live pod: nothing more is forgotten, and the withdrawal's request,
		// if it is still waiting, is dropped.
		if old == "" {
			old = r.takeWithdrawn(export)
		}
		if old == ip {
			r.cancelForget(ctx, export, old)
		} else {
			r.forget(ctx, export, old)
		}
	}
	return nil
}

// assemblingInstance is the metadata server instance an export bound to a pod
// is assembled by, or "" when there is no such pod.
func (r *NFSExportReconciler) assemblingInstance(ctx context.Context, export *simplyblockv1alpha2.NFSExport) string {
	name := export.Status.MDSPodName
	if name == "" {
		return ""
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: name}, &pod); err != nil {
		return ""
	}
	return mdsInstance(&pod)
}

// mdsInstance names one boot of a metadata server's guest: the pod's UID and
// its runner container's ID. The UID alone misses the restart that matters
// most. A guest that crashes takes the runner with it, kubelet restarts the
// container inside the same pod, and the UID stays while the guest boots cold
// with no mounts and no exports. The container's ID is new every time it
// starts. A pod with no running runner yet is named by its UID alone, which
// matches no assembled instance, so the export is reassembled once it runs.
func mdsInstance(pod *corev1.Pod) string {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == driver.MDSContainerName && c.ContainerID != "" {
			return string(pod.UID) + "/" + c.ContainerID
		}
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

// withdrawAddress takes the metadata server's address out of the export's
// EndpointSlice and has the nodes forget the connections translated to it
// (design-pnfs-mds-vm.md §8.1). Clients reconnect within a second of the guest
// shutting down, and a connection translated to the dead pod is kept alive in
// conntrack by its own SYN retries, for up to two minutes after the
// replacement serves. With no endpoint, kube-proxy refuses the connection
// instead. An address already withdrawn is left as it is.
func (r *NFSExportReconciler) withdrawAddress(ctx context.Context, export *simplyblockv1alpha2.NFSExport) error {
	old := export.Status.MDSNodeIP
	if old == "" {
		return nil
	}
	if err := r.reconcileExportEndpointSlice(ctx, export, ""); err != nil {
		return fmt.Errorf("withdrawing the address of %s: %w", export.Name, err)
	}
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.MDSNodeIP = ""
	}); err != nil {
		return err
	}
	r.rememberWithdrawn(export, old)
	r.event(export, corev1.EventTypeNormal, "MDSAddressWithdrawn",
		fmt.Sprintf("metadata server pod %s is going away, %s withdrawn until its replacement has an address",
			export.Status.MDSPodName, old))
	r.forget(ctx, export, old)
	return nil
}

// forget has the nodes forget the flows to the export's Service translated to
// the given address. Without the Service's ClusterIP nothing is asked: a
// request naming only the address would also take the flows of any pod that
// inherited it.
func (r *NFSExportReconciler) forget(ctx context.Context, export *simplyblockv1alpha2.NFSExport, ip string) {
	if ip == "" || r.Flows == nil {
		return
	}
	if svc := r.exportServiceIP(ctx, export); svc != "" {
		r.Flows.Forget(svc, ip)
	}
}

func (r *NFSExportReconciler) cancelForget(ctx context.Context, export *simplyblockv1alpha2.NFSExport, ip string) {
	if ip == "" || r.Flows == nil {
		return
	}
	if svc := r.exportServiceIP(ctx, export); svc != "" {
		r.Flows.Cancel(svc, ip)
	}
}

// exportServiceIP is the ClusterIP of the export's Service: the status records
// it, and the Service itself answers when the status does not yet.
func (r *NFSExportReconciler) exportServiceIP(ctx context.Context, export *simplyblockv1alpha2.NFSExport) string {
	if a, err := netip.ParseAddr(export.Status.ServiceAddress); err == nil {
		return a.String()
	}
	var svc corev1.Service
	key := client.ObjectKey{Namespace: export.Namespace, Name: utils.NFSExportServiceName(export.Name)}
	if err := r.Get(ctx, key, &svc); err != nil {
		return ""
	}
	if a, err := netip.ParseAddr(svc.Spec.ClusterIP); err == nil {
		return a.String()
	}
	return ""
}

// rememberWithdrawn keeps the address an export's withdrawal took away, so the
// move to the replacement can have the nodes forget it a second time. It lives
// in memory only: an operator restart in between loses the second flush, and
// the first one has already run.
func (r *NFSExportReconciler) rememberWithdrawn(export *simplyblockv1alpha2.NFSExport, ip string) {
	r.withdrawnMu.Lock()
	defer r.withdrawnMu.Unlock()
	if r.withdrawn == nil {
		r.withdrawn = map[client.ObjectKey]string{}
	}
	r.withdrawn[client.ObjectKeyFromObject(export)] = ip
}

func (r *NFSExportReconciler) takeWithdrawn(export *simplyblockv1alpha2.NFSExport) string {
	r.withdrawnMu.Lock()
	defer r.withdrawnMu.Unlock()
	key := client.ObjectKeyFromObject(export)
	ip := r.withdrawn[key]
	delete(r.withdrawn, key)
	return ip
}
