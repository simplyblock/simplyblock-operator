// The placement service's questions to a member's control plane, asked through
// atlas-lib's client as that member: whether a pool holds a volume of a name,
// which settles a retried CreateVolume, and how many volumes a consistency
// group has, which keeps a group in one member.

package placement

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ControlPlaneLookup answers MemberLookup from the control plane.
type ControlPlaneLookup struct {
	// Reader reads a member's credential Secret.
	Reader client.Reader

	// Config is how the control plane is reached. Its token is the
	// operator's, used for a member that has no credential of its own.
	Config controlplane.Config
}

// VolumeExists reports whether the member's pool holds a volume of that name.
func (l ControlPlaneLookup) VolumeExists(
	ctx context.Context, member *simplyblockv1alpha2.StorageCluster, poolUUID, name string,
) (bool, error) {
	api, err := l.client(ctx, member)
	if err != nil {
		return false, err
	}
	volumes, err := api.ListVolumes(ctx, member.Status.UUID, poolUUID)
	if err != nil {
		return false, err
	}
	for _, v := range volumes {
		if v.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// GroupMembers is the number of volumes in the member's group of that name.
func (l ControlPlaneLookup) GroupMembers(
	ctx context.Context, member *simplyblockv1alpha2.StorageCluster, group string,
) (int, error) {
	api, err := l.client(ctx, member)
	if err != nil {
		return 0, err
	}
	found, ok, err := api.ConsistencyGroupNamed(ctx, member.Status.UUID, group)
	if err != nil || !ok {
		return 0, err
	}
	return found.MemberCount, nil
}

// client builds a client authenticated as the member: with the credential the
// cluster reconciler recorded for it, which is what reaches a control plane
// another Kubernetes cluster runs, or with the operator's own token.
func (l ControlPlaneLookup) client(
	ctx context.Context, member *simplyblockv1alpha2.StorageCluster,
) (*controlplane.Client, error) {
	cfg := l.Config
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: member.Namespace, Name: "simplyblock-cluster-" + member.Name}
	if err := l.Reader.Get(ctx, key, &secret); err == nil && len(secret.Data["secret"]) > 0 {
		cfg.Token = string(secret.Data["secret"])
	}
	api, err := controlplane.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("build the control-plane client for member %s: %w", member.Name, err)
	}
	return api, nil
}
