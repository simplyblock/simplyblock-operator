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
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration/autobalancing"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// clusterIDParam is the StorageClass parameter key holding the target cluster's
	// backend UUID (see upsertStorageClass in simplyblockpool_controller.go).
	clusterIDParam = "cluster_id"
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
		cfg autobalancing.RebalancingConfig,
		eligible map[string]bool,
		inputs ...autobalancing.StorageNodeSelectorInput,
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

	if pvc.Annotations[kube.AnnoHostID] != "" || pvc.Annotations[kube.DeprecatedAnnoHostID] != "" {
		log.V(1).Info("Skipping: host-id already set")
		return admission.Allowed("host-id already set")
	}

	clusterUUID, ok := h.resolveClusterID(ctx, pvc, log)
	if !ok {
		return admission.Allowed("not a simplyblock-provisioned PVC")
	}

	nodeUUID, ok := h.selectPrimaryNode(ctx, clusterUUID, log)
	if !ok {
		return admission.Allowed("no load-based placement decision available")
	}

	patched := pvc.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = make(map[string]string)
	}
	patched.Annotations[kube.AnnoHostID] = nodeUUID

	original, err := json.Marshal(pvc)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	patchedRaw, err := json.Marshal(patched)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	log.Info("Selected primary node for new volume", "nodeUUID", nodeUUID, "clusterUUID", clusterUUID)
	return admission.PatchResponseFromRaw(original, patchedRaw)
}

// resolveClusterID resolves the PVC's StorageClass to a simplyblock cluster UUID. ok=false
// means "not our PVC" (no StorageClass, not our provisioner, no cluster_id) — not an error.
func (h *SimplyblockVolumePlacementInjector) resolveClusterID(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	log logr.Logger,
) (clusterUUID string, ok bool) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "", false
	}

	sc := &storagev1.StorageClass{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		log.V(1).Info("Skipping: cannot get StorageClass", "storageClass", *pvc.Spec.StorageClassName, "error", err.Error())
		return "", false
	}
	if sc.Provisioner != utils.CSIProvisioner {
		return "", false
	}
	clusterUUID = sc.Parameters[clusterIDParam]
	return clusterUUID, clusterUUID != ""
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

	vms := cr.Spec.VolumeMigrationSettings
	if vms == nil || vms.AutoRebalancing == nil {
		log.V(1).Info("Skipping: auto-rebalancing not configured", "cluster", cr.Name)
		return "", false
	}
	spec := vms.AutoRebalancing
	if spec.Enabled != nil && !*spec.Enabled {
		log.V(1).Info("Skipping: auto-rebalancing disabled", "cluster", cr.Name)
		return "", false
	}

	cfg, err := autobalancing.ResolveRebalancingConfig(spec)
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

	nodeUUID, ok, err = h.NodeSelector.SelectBestNode(ctx, cfg, eligible, autobalancing.StorageNodeSelectorInput{
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
