// Package vrgfollow moves a stretched cluster's volumes to the site Ramen makes
// their VolumeReplicationGroup primary on (design-stretched-sync-replication-
// ramen.md §9).
//
// Ramen's sync mode moves an application's placement and its PV and PVC
// objects, and never calls storage: it assumes storage active on both sites at
// once. This cluster leads each volume store from one site, so something has to
// move the leadership when Ramen moves the application. This controller makes
// Ramen's own decision the trigger: when the VRG on this site turns primary,
// the volumes it protects are moved here, before Ramen points the placement at
// this site and the application's pods start. A pod's stage only waits for it.
package vrgfollow

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/link/sitev1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// VRGKind is Ramen's VolumeReplicationGroup.
var VRGKind = schema.GroupVersionKind{Group: "ramendr.openshift.io", Version: "v1alpha1", Kind: "VolumeReplicationGroup"}

const (
	csiDriver = "csi.simplyblock.io"

	// moveRequeueInterval is how soon a move the backend is still running, or
	// has refused, is asked about again.
	moveRequeueInterval = 10 * time.Second
	// restoreRequeueInterval is how soon a primary VRG whose claims Ramen has
	// not restored and bound yet is looked at again.
	restoreRequeueInterval = 15 * time.Second

	// EventSyncMoveRefused is emitted on the VRG when the backend refuses to
	// move one of its volumes here.
	EventSyncMoveRefused = "SyncMoveRefused"
)

// Ramen's replication state and action values on a VRG's spec.
const (
	vrgPrimary        = "primary"
	vrgActionFailover = "Failover"
)

// Mover moves a volume's leadership to a site: the operator's SwitchoverService.
type Mover interface {
	Move(ctx context.Context, req *sitev1.MoveRequest) (*sitev1.MoveResponse, error)
}

// Reconciler follows Ramen's VolumeReplicationGroups.
type Reconciler struct {
	client.Client
	Recorder events.EventRecorder
	Mover    Mover
	// Namespace is the operator's, where the stretched StorageCluster lives.
	Namespace string
}

// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=volumereplicationgroups,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves the volumes of a sync VRG that is primary on this site.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	vrg := &unstructured.Unstructured{}
	vrg.SetGroupVersionKind(VRGKind)
	if err := r.Get(ctx, req.NamespacedName, vrg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	spec, _ := vrg.Object["spec"].(map[string]any)
	// An async VRG's volumes are moved by Ramen's VolumeReplications through
	// csi-addons. A VRG that is not primary here asks for nothing on this site.
	if _, sync := spec["sync"]; !sync {
		return ctrl.Result{}, nil
	}
	if state, _ := spec["replicationState"].(string); state != vrgPrimary {
		return ctrl.Result{}, nil
	}
	site, clusterUUID, err := r.stretchedSite(ctx)
	if err != nil || site == "" {
		return ctrl.Result{}, err
	}

	handles, waiting, err := r.volumes(ctx, vrg, spec, clusterUUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	allowForced := spec["action"] == vrgActionFailover

	// The backend names the volumes blocking a move by their own id, the last
	// segment of a handle.
	own := make(map[string]bool, len(handles))
	for _, h := range handles {
		own[h[strings.LastIndex(h, ":")+1:]] = true
	}

	pending := false
	for _, h := range handles {
		resp, err := r.Mover.Move(ctx, &sitev1.MoveRequest{Volume: h, ToSite: site, AllowForced: allowForced})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("move volume %s to site %s: %w", h, site, err)
		}
		switch resp.GetState() {
		case sitev1.MoveResponse_STATE_MOVED:
		case sitev1.MoveResponse_STATE_REFUSED:
			pending = true
			// A store's volumes are moved one at a time, so a promote is refused
			// while this VRG's other volumes on the store are still active on the
			// old site. They are demoted in this same pass, and the next one
			// promotes the store: ordering, not a problem to report.
			if blockedOnlyBy(resp.GetBlockingVolumes(), own) {
				log.Info("volume move waits on this VRG's other volumes of its store", "volume", h, "to", site)
				continue
			}
			note := fmt.Sprintf("volume %s cannot be moved to site %s: %s", h, site, resp.GetReason())
			if blocking := resp.GetBlockingVolumes(); len(blocking) > 0 {
				note += "; still served on site " + resp.GetLedFrom() + ": " + strings.Join(blocking, ", ")
			}
			r.Recorder.Eventf(vrg, nil, corev1.EventTypeWarning, EventSyncMoveRefused, "Move", note)
		default:
			pending = true
			log.Info("volume move in progress", "volume", h, "to", site)
		}
	}

	switch {
	case pending:
		return ctrl.Result{RequeueAfter: moveRequeueInterval}, nil
	case waiting:
		return ctrl.Result{RequeueAfter: restoreRequeueInterval}, nil
	}
	return ctrl.Result{}, nil
}

// stretchedSite is this operator's site and its backend cluster, or "" when
// no StorageCluster in the namespace is one site of a stretched cluster.
func (r *Reconciler) stretchedSite(ctx context.Context) (site, clusterUUID string, err error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(r.Namespace)); err != nil {
		return "", "", fmt.Errorf("list StorageClusters: %w", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if c.Spec.Stretched != nil && c.Spec.Stretched.Site != "" && c.Status.UUID != "" {
			return c.Spec.Stretched.Site, c.Status.UUID, nil
		}
	}
	return "", "", nil
}

// volumes is the handles of the VRG's claims bound to this cluster's volumes,
// and whether any claim it selects is still to be restored or bound.
func (r *Reconciler) volumes(
	ctx context.Context, vrg *unstructured.Unstructured, spec map[string]any, clusterUUID string,
) ([]string, bool, error) {
	var selector metav1.LabelSelector
	if raw, ok := spec["pvcSelector"].(map[string]any); ok {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &selector); err != nil {
			return nil, false, fmt.Errorf("VRG %s/%s pvcSelector: %w", vrg.GetNamespace(), vrg.GetName(), err)
		}
	}
	sel, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, false, fmt.Errorf("VRG %s/%s pvcSelector: %w", vrg.GetNamespace(), vrg.GetName(), err)
	}
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(vrg.GetNamespace()),
		client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, false, fmt.Errorf("list the claims of VRG %s/%s: %w", vrg.GetNamespace(), vrg.GetName(), err)
	}

	waiting := len(claims.Items) == 0
	var handles []string
	for i := range claims.Items {
		pvc := &claims.Items[i]
		if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
			waiting = true
			continue
		}
		var pv corev1.PersistentVolume
		if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
			return nil, false, fmt.Errorf("PV %s of claim %s/%s: %w", pvc.Spec.VolumeName, pvc.Namespace, pvc.Name, err)
		}
		csi := pv.Spec.CSI
		if csi == nil || csi.Driver != csiDriver || !strings.HasPrefix(csi.VolumeHandle, clusterUUID+":") {
			continue
		}
		handles = append(handles, csi.VolumeHandle)
	}
	return handles, waiting, nil
}

// SetupWithManager watches VRGs, on spec changes only, and the claims they
// select, which Ramen restores after it has made the VRG primary.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	vrg := &unstructured.Unstructured{}
	vrg.SetGroupVersionKind(VRGKind)
	return ctrl.NewControllerManagedBy(mgr).
		Named("vrgfollow").
		For(vrg, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.vrgsOfClaim)).
		Complete(r)
}

// vrgsOfClaim is every VRG in a claim's namespace.
func (r *Reconciler) vrgsOfClaim(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(VRGKind.GroupVersion().WithKind(VRGKind.Kind + "List"))
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].GetNamespace(), Name: list.Items[i].GetName(),
		}})
	}
	return reqs
}

// blockedOnlyBy reports whether a refusal names blocking volumes, all of them
// in own.
func blockedOnlyBy(blocking []string, own map[string]bool) bool {
	if len(blocking) == 0 {
		return false
	}
	for _, v := range blocking {
		if !own[v] {
			return false
		}
	}
	return true
}
