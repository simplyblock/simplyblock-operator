package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha1-volumemigration,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=volumemigrations,verbs=create,versions=v1alpha1,name=volumemigration-validator.simplyblock.io,admissionReviewVersions=v1

// VolumeMigrationValidator rejects, at admission, a VolumeMigration whose target
// volume (or a sibling sharing its NVMe subsystem) is a consistency-group
// member. Members are pinned to one logical volume store, so migrating one off
// it would break the group's frozen snapshot; the operator declines the request
// at kubectl apply rather than starting a migration the backend would abort
// partway (design §8.4, §9.5). The backend refusal is the backstop.
//
// The membership check is fail-open: when the control plane cannot be reached to
// determine membership, the object is admitted and the backend refusal catches
// it. The webhook needs `get` on persistentvolumes; it resolves the PV's CSI
// volume handle to the backing lvol and reads its group_id.
type VolumeMigrationValidator struct {
	Client    client.Client
	APIClient *webapi.Client
}

func (v *VolumeMigrationValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("volumemigration", req.Name, "namespace", req.Namespace)

	migration := &simplyblockv1alpha1.VolumeMigration{}
	if err := json.Unmarshal(req.Object.Raw, migration); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	pvName := migration.Spec.PVName
	if pvName == "" {
		return admission.Allowed("no target PV")
	}

	clusterUUID, poolUUID, volumeUUID, ok, err := v.resolveHandle(ctx, pvName)
	if err != nil {
		// Fail-open (§9.5): a PV read failure must not block a migration the
		// backend will itself refuse for a member.
		log.Error(err, "cannot resolve target PV; admitting (backend backstops)", "pv", pvName)
		return admission.Allowed("target PV not resolvable; deferring to the backend")
	}
	if !ok {
		return admission.Allowed("target PV is not a simplyblock CSI volume")
	}

	member, groupID, err := v.memberOfGroup(ctx, clusterUUID, poolUUID, volumeUUID)
	if err != nil {
		log.Error(err, "cannot determine group membership; admitting (backend backstops)")
		return admission.Allowed("consistency-group membership undeterminable; deferring to the backend")
	}
	if member {
		return admission.Denied(fmt.Sprintf(
			"volume %s is a member of consistency group %s and cannot be migrated; "+
				"a group's members are pinned to one logical volume store (§8.4)", volumeUUID, groupID))
	}
	return admission.Allowed("target volume is not a consistency-group member")
}

// resolveHandle reads the PV and splits its CSI volume handle
// ({clusterUUID}:{poolUUID}:{volumeUUID}). ok is false for a non-simplyblock PV.
func (v *VolumeMigrationValidator) resolveHandle(
	ctx context.Context, pvName string,
) (clusterUUID, poolUUID, volumeUUID string, ok bool, err error) {
	clusterUUID, poolUUID, volumeUUID, ok, err = pvVolumeHandle(ctx, v.Client, pvName)
	if err != nil {
		return "", "", "", false, fmt.Errorf("get PV %q: %w", pvName, err)
	}
	return clusterUUID, poolUUID, volumeUUID, ok, nil
}

// memberOfGroup reports whether the volume, or any sibling sharing its NVMe
// subsystem, is a consistency-group member. A migration moves the whole
// subsystem, so a member sibling is as disqualifying as the named volume itself
// (§9.5).
func (v *VolumeMigrationValidator) memberOfGroup(
	ctx context.Context, clusterUUID, poolUUID, volumeUUID string,
) (bool, string, error) {
	vol, err := v.APIClient.GetVolume(ctx, clusterUUID, poolUUID, volumeUUID)
	if err != nil {
		return false, "", err
	}
	if vol == nil {
		return false, "", nil
	}
	if vol.GroupID != "" {
		return true, vol.GroupID, nil
	}
	// Sibling check: every volume sharing the subsystem migrates together.
	if vol.NQN == "" {
		return false, "", nil
	}
	siblings, err := v.APIClient.GetSubsystemVolumes(ctx, clusterUUID, vol.NQN)
	if err != nil {
		return false, "", err
	}
	for i := range siblings {
		if siblings[i].GroupID != "" {
			return true, siblings[i].GroupID, nil
		}
	}
	return false, "", nil
}
