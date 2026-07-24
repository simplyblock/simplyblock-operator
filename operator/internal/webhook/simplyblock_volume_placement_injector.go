package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/autoplacement"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:webhook:path=/mutate-v1-pvc-simplyblock-placement,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=persistentvolumeclaims,verbs=create,versions=v1,name=simplyblock-volume-placement-injector.simplyblock.io,admissionReviewVersions=v1
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets,verbs=get;list;watch

// storageNodeLister is the subset of *webapi.Client this webhook depends on, narrowed to an
// interface so callers (tests, or any future substitute) aren't tied to the concrete client.
type storageNodeLister interface {
	GetStorageNodes(ctx context.Context, clusterUUID string) ([]webapi.StorageNodeInfo, error)
}

// primaryNodeSelector is the subset of *autobalancing.StorageNodeSelector this webhook
// depends on, narrowed to an interface for the same reason as storageNodeLister.
type primaryNodeSelector interface {
	SelectBestNode(
		ctx context.Context,
		cfg autoplacement.RebalancingConfig,
		eligible map[string]bool,
		inputs ...autoplacement.StorageNodeSelectorInput,
	) (nodeUUID string, ok bool, err error)
}

// SimplyblockVolumePlacementInjector is a mutating admission webhook that computes the
// least-loaded eligible storage node for a new PVC's primary volume — using the same
// latency-deviation signal the auto-rebalancer (Issue #130) uses — and stamps it onto the
// PVC as the simplyblock.io/host-id annotation, which spdk-csi already reads and forwards
// as host_id on CreateVolume. failurePolicy=ignore, and every skip/error path below allows
// the PVC unmodified, so this can never block volume provisioning: sbcli's own
// weighted-random pick (_get_next_3_nodes) runs as the fallback exactly as it does today.
type SimplyblockVolumePlacementInjector struct {
	Client       client.Client
	APIClient    storageNodeLister
	NodeSelector primaryNodeSelector
}

func (h *SimplyblockVolumePlacementInjector) Handle(
	ctx context.Context,
	req admission.Request,
) admission.Response {
	log := logf.FromContext(ctx).WithValues("pvc", req.Name, "namespace", req.Namespace)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := json.Unmarshal(req.Object.Raw, pvc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// A user-supplied host-id is an explicit manual pin. Normalize it to the
	// canonical selected-storage-node annotation — which the operator's pin
	// controller, drain, and rebalancer recognize, and which the CSI driver reads
	// as the primary host_id source at CreateVolume — and drop the legacy host-id
	// forms. The user's choice wins, so we do not run load-based placement.
	hostID := pvc.Annotations[kube.AnnoHostID]
	if hostID == "" {
		hostID = pvc.Annotations[kube.DeprecatedAnnoHostID]
	}
	if hostID != "" {
		patched := pvc.DeepCopy()
		if patched.Annotations == nil {
			patched.Annotations = make(map[string]string)
		}
		// Do not clobber an existing explicit selected-storage-node pin.
		if patched.Annotations[kube.AnnoSelectedStorageNode] == "" {
			patched.Annotations[kube.AnnoSelectedStorageNode] = hostID
		}
		delete(patched.Annotations, kube.AnnoHostID)
		delete(patched.Annotations, kube.DeprecatedAnnoHostID)
		log.Info("Exchanged legacy host-id for selected-storage-node", "node", hostID)
		return patchResponse(pvc, patched)
	}

	// An explicit selected-storage-node pin is honored as-is; never override it
	// with a load-based pick.
	if pvc.Annotations[kube.AnnoSelectedStorageNode] != "" {
		log.V(1).Info("Skipping: selected-storage-node already set")
		return admission.Allowed("selected-storage-node already set")
	}

	clusterUUID, ok := h.resolveClusterID(ctx, pvc, log)
	if !ok {
		return admission.Allowed("not a simplyblock-provisioned PVC")
	}

	nodeUUID, ok := h.selectPrimaryNode(ctx, clusterUUID, log)
	if !ok {
		return admission.Allowed("no load-based placement decision available")
	}

	// Load-based initial placement is a rebalanceable hint, not a pin, so it is
	// written as the dedicated placement-hint annotation (which the CSI driver
	// consumes at CreateVolume and then clears) rather than selected-storage-node
	// (a permanent pin that would exclude the volume from rebalancing) or the
	// legacy host-id (reserved for backward compatibility with pre-existing PVCs).
	patched := pvc.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = make(map[string]string)
	}
	patched.Annotations[kube.AnnoPlacementHint] = nodeUUID
	log.Info("Selected primary node for new volume", "nodeUUID", nodeUUID, "clusterUUID", clusterUUID)
	return patchResponse(pvc, patched)
}

// patchResponse builds a JSON-patch admission response mutating original into
// patched, or an errored response if either fails to marshal.
func patchResponse(original, patched *corev1.PersistentVolumeClaim) admission.Response {
	originalRaw, err := json.Marshal(original)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	patchedRaw, err := json.Marshal(patched)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(originalRaw, patchedRaw)
}

// resolveClusterID resolves the PVC's StorageClass to a simplyblock cluster UUID. ok=false
// means "not our PVC" (no StorageClass, not our provisioner, no cluster_id) — not an error.
func (h *SimplyblockVolumePlacementInjector) resolveClusterID(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	log logr.Logger,
) (clusterUUID string, ok bool) {
	if ptr.IsEmptyString(pvc.Spec.StorageClassName) {
		return "", false
	}

	sc := &storagev1.StorageClass{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		log.V(1).Info("Skipping: cannot get StorageClass", "storageClass", *pvc.Spec.StorageClassName, "error", err.Error())
		return "", false
	}
	props, err := kube.PropertiesFromStorageClass(sc)
	if err != nil {
		log.V(1).Info("Skipping: not a simplyblock StorageClass", "storageClass", sc.Name, "error", err.Error())
		return "", false
	}
	return props.ClusterID, props.ClusterID != ""
}

// selectPrimaryNode resolves the StorageCluster owning clusterUUID, checks that it has
// opted into latency-driven auto-rebalancing (Issue #130), fetches its storage nodes,
// filters them for placement eligibility, and ranks the survivors by current latency
// deviation. ok=false covers every "no signal available" case (feature not enabled,
// backend/Prometheus unreachable, no eligible node) — all of them fall through to the
// caller allowing the PVC unmodified.
func (h *SimplyblockVolumePlacementInjector) selectPrimaryNode(
	ctx context.Context,
	clusterUUID string,
	log logr.Logger,
) (nodeUUID string, ok bool) {
	var list simplyblockv1alpha1.StorageClusterList
	if err := h.Client.List(ctx, &list); err != nil {
		log.Error(err, "Failed to list StorageClusters")
		return "", false
	}
	var cr *simplyblockv1alpha1.StorageCluster
	for i := range list.Items {
		if list.Items[i].Status.UUID == clusterUUID {
			cr = &list.Items[i]
			break
		}
	}
	if cr == nil {
		log.V(1).Info("Skipping: no matching StorageCluster found", "clusterUUID", clusterUUID)
		return "", false
	}

	spec := ptr.From(cr.Spec.VolumeAutoPlacement, simplyblockv1alpha1.VolumeAutoPlacementSettings{})
	if !ptr.BoolFromOrTrue(spec.Enabled) {
		log.V(1).Info("Skipping: auto-rebalancing disabled", "cluster", cr.Name)
		return "", false
	}

	cfg, err := autoplacement.ResolveAutoPlacementConfig(spec)
	if err != nil {
		log.V(1).Info("Skipping: invalid rebalancing configuration", "cluster", cr.Name, "error", err.Error())
		return "", false
	}

	nodes, err := h.APIClient.GetStorageNodes(ctx, clusterUUID)
	if err != nil {
		log.Error(err, "Cannot list storage nodes; skipping smart placement", "cluster", cr.Name)
		return "", false
	}

	eligible := make(map[string]bool, len(nodes))
	storageNodes := make([]volumemigration.StorageNode, 0, len(nodes))
	for _, n := range nodes {
		storageNodes = append(storageNodes, volumemigration.StorageNode{UUID: n.UUID, ClusterUUID: clusterUUID})
		eligible[n.UUID] = n.Status == "online" && n.Healthy && n.Lvols < n.LvolsMax
	}

	nodeUUID, ok, err = h.NodeSelector.SelectBestNode(ctx, cfg, eligible, autoplacement.StorageNodeSelectorInput{
		Namespace:    cr.Namespace,
		StorageNodes: storageNodes,
	})
	if err != nil {
		log.Error(err, "Cannot select best node; skipping smart placement", "cluster", cr.Name)
		return "", false
	}
	if !ok {
		log.V(1).Info("Skipping: no eligible node found", "cluster", cr.Name)
		return "", false
	}
	return nodeUUID, true
}
