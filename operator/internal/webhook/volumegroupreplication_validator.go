package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// volumeGroupReplicationGVK and volumeGroupReplicationClassGVK name the
// csi-addons kinds this repository vendors no Go type for (design
// §4.1). Duplicated, not shared, with internal/controller's identical pair:
// each package that needs an unstructured kind's GVK declares its own,
// matching the layering the design's own §4.4/§4.3 keep as two independent
// enforcement points (design-consistency-groups.md §9's "last line of
// defense, not the first").
var (
	volumeGroupReplicationGVK = schema.GroupVersionKind{
		Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeGroupReplication",
	}
	volumeGroupReplicationClassGVK = schema.GroupVersionKind{
		Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeGroupReplicationClass",
	}
)

// +kubebuilder:webhook:path=/validate-replication-storage-openshift-io-v1alpha1-volumegroupreplication,mutating=false,failurePolicy=fail,sideEffects=None,groups=replication.storage.openshift.io,resources=volumegroupreplications,verbs=create,versions=v1alpha1,name=vvolumegroupreplication.simplyblock.io,admissionReviewVersions=v1

// VolumeGroupReplicationValidator rejects, at kubectl apply, a
// VolumeGroupReplication whose selector does not resolve to exactly one
// consistency group's current membership, so VolumeGroupReplicationReconciler
// (design §4.3) never has to reconcile an object that could never fan out
// correctly. It makes the same two checks, with the same two dispositions,
// design-consistency-groups.md §9.4 already established for
// VolumeGroupSnapshot:
//
//   - The label check is static and fail-closed: every selected PVC must carry
//     the same non-empty consistency-group label. A selector that spans groups,
//     matches an unlabeled PVC, or matches nothing is rejected outright.
//   - The membership check is backend and fail-open: it resolves the group by
//     its label value and rejects the object unless the selected set equals the
//     group's current membership. When membership cannot be determined (the
//     backend is unreachable, or a selected PVC is not yet bound), it admits and
//     the reconciler backstops it on its own next reconcile (design §4.3).
type VolumeGroupReplicationValidator struct {
	Client    client.Client
	APIClient *webapi.Client
}

// ownsVolumeGroupReplication reports whether the object is external and
// attributed to this driver's class, the same ownership signal the
// reconciler applies (design §4.3). Every undecidable case admits, because
// with failurePolicy=fail a webhook error would block every
// VolumeGroupReplication in the cluster, foreign drivers included.
func (v *VolumeGroupReplicationValidator) ownsVolumeGroupReplication(
	ctx context.Context, vgr *unstructured.Unstructured,
) (bool, string) {
	external, _, _ := unstructured.NestedBool(vgr.Object, "spec", "external")
	if !external {
		return false, "spec.external is not true; the generic controller-manager reconciles this one"
	}
	className, _, _ := unstructured.NestedString(vgr.Object, "spec", "volumeGroupReplicationClassName")
	if className == "" {
		return false, "no volumeGroupReplicationClassName; not attributable to this driver"
	}
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeGroupReplicationClassGVK)
	if err := v.Client.Get(ctx, client.ObjectKey{Name: className}, class); err != nil {
		return false, fmt.Sprintf("volume group replication class %q not readable; not validating", className)
	}
	provisioner, _, _ := unstructured.NestedString(class.Object, "spec", "provisioner")
	if provisioner != "csi.simplyblock.io" {
		return false, fmt.Sprintf("class %q belongs to provisioner %q; not validating", className, provisioner)
	}
	return true, ""
}

func (v *VolumeGroupReplicationValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("volumegroupreplication", req.Name, "namespace", req.Namespace)

	vgr := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, vgr); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if ours, reason := v.ownsVolumeGroupReplication(ctx, vgr); !ours {
		return admission.Allowed(reason)
	}

	pvcs, groupName, denied := v.labelCheck(ctx, req.Namespace, vgr)
	if denied != nil {
		return *denied
	}

	clusterUUID, selected, determinable, err := v.selectedLvols(ctx, pvcs)
	if err != nil {
		log.Error(err, "cannot resolve selected volumes; admitting (reconciler backstops)")
		return admission.Allowed("group membership undeterminable; deferring to the reconciler")
	}
	if !determinable {
		return admission.Allowed("a selected PVC is not yet bound; deferring to the reconciler")
	}
	group, err := v.APIClient.GetConsistencyGroupByName(ctx, clusterUUID, groupName)
	if err != nil || group == nil {
		if err != nil {
			log.Error(err, "cannot resolve consistency group; admitting (reconciler backstops)")
		}
		return admission.Allowed("consistency group not resolvable; deferring to the reconciler")
	}
	members, err := v.APIClient.GetConsistencyGroupMembers(ctx, clusterUUID, group.UUID)
	if err != nil {
		log.Error(err, "cannot read group membership; admitting (reconciler backstops)")
		return admission.Allowed("group membership unreadable; deferring to the reconciler")
	}
	if !sameStringSet(selected, members) {
		return admission.Denied(fmt.Sprintf(
			"selector resolves to %d volume(s) but consistency group %q has %d member(s); "+
				"the selector must equal the group's current membership",
			len(selected), groupName, len(members)))
	}
	return admission.Allowed("selector equals the consistency group's membership")
}

// labelCheck resolves spec.source.selector to PVCs and enforces the
// fail-closed label rule, the same as VolumeGroupSnapshotValidator.labelCheck.
func (v *VolumeGroupReplicationValidator) labelCheck(
	ctx context.Context, namespace string, vgr *unstructured.Unstructured,
) ([]corev1.PersistentVolumeClaim, string, *admission.Response) {
	deny := func(msg string) *admission.Response { r := admission.Denied(msg); return &r }

	selMap, found, err := unstructured.NestedMap(vgr.Object, "spec", "source", "selector")
	if err != nil || !found {
		return nil, "", deny("VolumeGroupReplication has no spec.source.selector")
	}
	var labelSelector metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selMap, &labelSelector); err != nil {
		return nil, "", deny(fmt.Sprintf("invalid label selector: %v", err))
	}
	sel, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, "", deny(fmt.Sprintf("invalid label selector: %v", err))
	}
	var pvcList corev1.PersistentVolumeClaimList
	if err := v.Client.List(ctx, &pvcList,
		client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, "", deny(fmt.Sprintf("cannot list PVCs for the selector: %v", err))
	}
	if len(pvcList.Items) == 0 {
		return nil, "", deny("selector matches no PersistentVolumeClaim")
	}
	groupName := ""
	for i := range pvcList.Items {
		val := pvcList.Items[i].Labels[consistencyGroupLabel]
		if val == "" {
			return nil, "", deny(fmt.Sprintf(
				"PVC %q is not labeled %s; a group replication's selector must match only group members",
				pvcList.Items[i].Name, consistencyGroupLabel))
		}
		if groupName == "" {
			groupName = val
		} else if val != groupName {
			return nil, "", deny(fmt.Sprintf(
				"selector spans two consistency groups (%q and %q); it must resolve to exactly one",
				groupName, val))
		}
	}
	return pvcList.Items, groupName, nil
}

// selectedLvols maps each selected PVC to its backing lvol UUID, the same as
// VolumeGroupSnapshotValidator.selectedLvols.
func (v *VolumeGroupReplicationValidator) selectedLvols(
	ctx context.Context, pvcs []corev1.PersistentVolumeClaim,
) (clusterUUID string, lvols []string, determinable bool, err error) {
	for i := range pvcs {
		pvName := pvcs[i].Spec.VolumeName
		if pvName == "" {
			return "", nil, false, nil
		}
		cluster, _, volume, ok, err := pvVolumeHandle(ctx, v.Client, pvName)
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			return "", nil, false, nil
		}
		clusterUUID = cluster
		lvols = append(lvols, volume)
	}
	return clusterUUID, lvols, true, nil
}
