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
	"testing"

	"github.com/simplyblock/atlas/export"
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
