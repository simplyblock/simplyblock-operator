// What the attacher builds before it runs an initiator, which is the part that
// can be tested without a fabric.
//
// The connect itself is the driver's existing NVMe-oF path and is exercised
// where that lives. What is pinned here is the volume context assembled for it,
// because every field in it is one the initiator silently does without: a
// missing hostNQN connects as the wrong host and is refused by a volume with
// allowed_hosts, and a missing pool or cluster makes the device lookup after
// the connect search the wrong place.

package nfsexport

import (
	"context"
	"errors"
	"fmt"
	"testing"

	export "github.com/simplyblock/atlas/nfsexport"

	"github.com/simplyblock/csi-driver/internal/controlplane"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

// testVolume is the backing namespace these tests speak about. One spelling,
// because the point of most of them is that the same identifier reaches the
// initiator unchanged.
const testVolume = "bfc56677-d602-4017-804b-975f3b929e3f"

func TestVolumeContextCarriesEveryIdentifierTheInitiatorNeeds(t *testing.T) {
	spec := export.Spec{
		VolumeUUID: testVolume,
		ClusterID:  "f0bb9077-78c4-4482-9ccf-a5693ce2df78",
		PoolID:     "9d016dd4-34d7-42f0-b549-52a5af2f1399",
		Path:       "/var/lib/simplyblock/exports/team-a-shared-bfc56677",
		FSID:       testVolume,
	}
	const hostNQN = "nqn.2014-08.org.nvmexpress:uuid:9f1c4d0e-2a3b-4c5d-8e6f-7a8b9c0d1e2f"

	// The connection info the control plane returns, which the context is
	// merged onto rather than replaced by.
	info := map[string]string{"targetType": "tcp", "nsId": "1", "nqn": "nqn.2023-02.io.simplyblock:x"}

	vc := volumeContextFor(spec, hostNQN, info)

	for key, want := range map[string]string{
		"uuid":       spec.VolumeUUID,
		"cluster_id": spec.ClusterID,
		"poolID":     spec.PoolID,
		"hostNQN":    hostNQN,
		"targetType": "tcp",
		"nsId":       "1",
	} {
		if vc[key] != want {
			t.Errorf("volume context %s = %q, want %q", key, vc[key], want)
		}
	}
}

// The control plane's answer wins over anything assembled locally: it is the
// authority on where the namespace is served from, and a local value that
// disagreed would connect to a stale target after a failover.
func TestControlPlaneInfoOverridesTheLocalContext(t *testing.T) {
	spec := export.Spec{VolumeUUID: testVolume}
	vc := volumeContextFor(spec, "", map[string]string{"uuid": "the-target-lvol"})

	if vc["uuid"] != "the-target-lvol" {
		t.Errorf("uuid = %q, want the control plane's answer", vc["uuid"])
	}
}

// Regression: 2026-10-09-pnfs-unstage-deleted-volume (run pnfs-1791525621). The
// external provisioner deletes a pNFS volume as soon as its claim goes, while
// kubelet may still be unstaging it on a node. Detach then got "volume not found"
// from the control plane, failed on every retry, and the node's controllers kept
// reconnecting to the removed subsystem. A volume the control plane no longer
// knows is released locally.
func TestDetachReleasesAVolumeTheControlPlaneNoLongerKnows(t *testing.T) {
	spec := export.Spec{VolumeUUID: testVolume, ClusterID: "cluster-1"}
	var released []export.Spec
	a := attacher{
		conn: func(context.Context, export.Spec) (initiator.Initiator, error) {
			return nil, fmt.Errorf("export: connection info for volume %s: %w",
				testVolume, controlplane.ErrVolumeNotFound)
		},
		releaseDeleted: func(_ context.Context, s export.Spec) error {
			released = append(released, s)
			return nil
		},
	}

	if err := a.Detach(context.Background(), spec); err != nil {
		t.Fatalf("Detach = %v, want the deleted volume released locally", err)
	}
	if len(released) != 1 || released[0].VolumeUUID != testVolume || released[0].ClusterID != "cluster-1" {
		t.Errorf("released %v, want [%v]", released, spec)
	}
}

// Any other control-plane failure is not evidence the volume is gone, so it
// stays an error and nothing is torn down on a guess.
func TestDetachKeepsOtherControlPlaneFailuresAnError(t *testing.T) {
	boom := errors.New("connection refused")
	a := attacher{
		conn: func(context.Context, export.Spec) (initiator.Initiator, error) { return nil, boom },
		releaseDeleted: func(context.Context, export.Spec) error {
			t.Error("released a volume the control plane did not report deleted")
			return nil
		},
	}

	if err := a.Detach(context.Background(), export.Spec{VolumeUUID: testVolume}); !errors.Is(err, boom) {
		t.Errorf("Detach = %v, want the control plane's error", err)
	}
}
