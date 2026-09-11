package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:webhook:path=/validate-groupsnapshot-storage-k8s-io-v1beta1-volumegroupsnapshot,mutating=false,failurePolicy=fail,sideEffects=None,groups=groupsnapshot.storage.k8s.io,resources=volumegroupsnapshots,verbs=create,versions=v1beta1,name=volumegroupsnapshot-validator.simplyblock.io,admissionReviewVersions=v1

// VolumeGroupSnapshotValidator rejects, at kubectl apply, a VolumeGroupSnapshot
// whose selector does not resolve to exactly one consistency group's current
// membership, so an object that can never be snapshotted is never created
// (design §9.4). It makes two checks with two dispositions:
//
//   - The label check is static and fail-closed: every selected PVC must carry
//     the same non-empty consistency-group label. A selector that spans groups,
//     matches an unlabeled PVC, or matches nothing is rejected outright.
//   - The membership check is backend and fail-open: it resolves the group by
//     its label value and rejects the object unless the selected set equals the
//     group's current membership. When membership cannot be determined (the
//     backend is unreachable, or a selected PVC is not yet bound), it admits and
//     the GroupController backstops it at snapshot time (§9.2).
type VolumeGroupSnapshotValidator struct {
	Client    client.Client
	APIClient *webapi.Client
}

func (v *VolumeGroupSnapshotValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("volumegroupsnapshot", req.Name, "namespace", req.Namespace)

	vgs := &volumegroupsnapshotv1beta1.VolumeGroupSnapshot{}
	if err := json.Unmarshal(req.Object.Raw, vgs); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	pvcs, groupName, denied := v.labelCheck(ctx, req.Namespace, vgs)
	if denied != nil {
		return *denied
	}

	// Membership check (fail-open): compare the selected lvols to the group's
	// current membership.
	clusterUUID, selected, determinable, err := v.selectedLvols(ctx, pvcs)
	if err != nil {
		log.Error(err, "cannot resolve selected volumes; admitting (GroupController backstops)")
		return admission.Allowed("group membership undeterminable; deferring to the GroupController")
	}
	if !determinable {
		return admission.Allowed("a selected PVC is not yet bound; deferring to the GroupController")
	}
	group, err := v.APIClient.GetConsistencyGroupByName(ctx, clusterUUID, groupName)
	if err != nil || group == nil {
		if err != nil {
			log.Error(err, "cannot resolve consistency group; admitting (GroupController backstops)")
		}
		return admission.Allowed("consistency group not resolvable; deferring to the GroupController")
	}
	members, err := v.APIClient.GetConsistencyGroupMembers(ctx, clusterUUID, group.UUID)
	if err != nil {
		log.Error(err, "cannot read group membership; admitting (GroupController backstops)")
		return admission.Allowed("group membership unreadable; deferring to the GroupController")
	}
	if !sameStringSet(selected, members) {
		return admission.Denied(fmt.Sprintf(
			"selector resolves to %d volume(s) but consistency group %q has %d member(s); "+
				"the selector must equal the group's current membership (§9.2)",
			len(selected), groupName, len(members)))
	}
	return admission.Allowed("selector equals the consistency group's membership")
}

// labelCheck resolves the selector to PVCs and enforces the fail-closed label
// rule. It returns the matched PVCs and their shared group name, or a non-nil
// denial response.
func (v *VolumeGroupSnapshotValidator) labelCheck(
	ctx context.Context, namespace string, vgs *volumegroupsnapshotv1beta1.VolumeGroupSnapshot,
) ([]corev1.PersistentVolumeClaim, string, *admission.Response) {
	deny := func(msg string) *admission.Response { r := admission.Denied(msg); return &r }

	if vgs.Spec.Source.Selector == nil {
		return nil, "", deny("VolumeGroupSnapshot has no label selector")
	}
	sel, err := metav1.LabelSelectorAsSelector(vgs.Spec.Source.Selector)
	if err != nil {
		return nil, "", deny(fmt.Sprintf("invalid label selector: %v", err))
	}
	var pvcList corev1.PersistentVolumeClaimList
	if err := v.Client.List(ctx, &pvcList,
		client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		// A read of the API server's own PVCs failing is not a backend blip; with
		// failurePolicy=Fail the API server denies, so surface it.
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
				"PVC %q is not labeled %s; a group snapshot's selector must match only group members",
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

// selectedLvols maps each selected PVC to its backing lvol UUID. determinable is
// false when a PVC is not yet bound (so its lvol is unknown), which makes the
// membership check fail open.
func (v *VolumeGroupSnapshotValidator) selectedLvols(
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
			// A selected PVC that is not a simplyblock CSI volume cannot be a
			// member, so its membership is undeterminable here: fail open.
			return "", nil, false, nil
		}
		clusterUUID = cluster
		lvols = append(lvols, volume)
	}
	return clusterUUID, lvols, true, nil
}

// sameStringSet reports whether a and b hold the same set of values, comparing
// each by its bare last path segment so a prefixed id matches a bare one.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[lastSegment(s)] = true
	}
	for _, s := range b {
		if !seen[lastSegment(s)] {
			return false
		}
	}
	return true
}

func lastSegment(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
}
