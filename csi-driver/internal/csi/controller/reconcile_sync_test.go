package controller

// A recreated claim on a stretched, sync-replicated cluster inherits the volume
// of its name (design-stretched-sync-replication-ramen.md §9.6): it is reused
// whatever site it is led from, and one that is not online is never deleted by
// the create, because on a sync cluster that is also what a volume whose site is
// down looks like.

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

type existingVolumes struct {
	controlplane.ClusterAPI
	volumes []*controlplane.LvolResp
	deleted []string
}

func (e *existingVolumes) ListVolumes(context.Context) ([]*controlplane.LvolResp, error) {
	return e.volumes, nil
}

func (e *existingVolumes) DeleteVolume(_ context.Context, id string) error {
	e.deleted = append(e.deleted, id)
	return nil
}

// Regression: 2026-10-23-sync-recreate-deletes-a-lost-sites-volume: the create
// reconcile deleted a same-named volume that was not online as a failed earlier
// attempt. On a sync cluster a volume whose site is down is not online either,
// and a claim recreated on the surviving site would have deleted it.
func TestASyncRecreateNeverDeletesAVolumeThatIsNotOnline(t *testing.T) {
	api := &existingVolumes{volumes: []*controlplane.LvolResp{{Name: "pvc-1", UUID: "v-1", Status: "offline"}}}
	_, err := reconcileExistingVolume(context.Background(), api, "pvc-1", 0, true)
	if status.Code(err) != codes.Aborted {
		t.Errorf("err = %v, want Aborted holding the create", err)
	}
	if len(api.deleted) != 0 {
		t.Errorf("deleted %v; a sync recreate must not delete the volume", api.deleted)
	}
}

// Off a sync cluster the leftover of a failed attempt is still cleaned up.
func TestANonSyncCreateStillDeletesAFailedLeftover(t *testing.T) {
	api := &existingVolumes{volumes: []*controlplane.LvolResp{{Name: "pvc-1", UUID: "v-1", Status: "offline"}}}
	if _, err := reconcileExistingVolume(context.Background(), api, "pvc-1", 0, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(api.deleted) != 1 {
		t.Errorf("deleted %v; want the leftover deleted", api.deleted)
	}
}

// An online volume of the name is reused on a sync cluster as anywhere.
func TestASyncRecreateReusesTheOnlineVolume(t *testing.T) {
	api := &existingVolumes{volumes: []*controlplane.LvolResp{{Name: "pvc-1", UUID: "v-1", Status: "online"}}}
	id, err := reconcileExistingVolume(context.Background(), api, "pvc-1", 0, true)
	if err != nil || id != "v-1" {
		t.Errorf("reconcile = %q, %v; want v-1 reused", id, err)
	}
}
