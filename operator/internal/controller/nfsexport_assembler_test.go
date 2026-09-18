// What the host is told about an export, derived from the record.
//
// The identifiers are the whole of it: the host resolves its device by volume
// UUID and reaches the control plane by cluster and pool, and every one of the
// three comes out of spec.volumeRef. A spec that loses any of them reaches a
// host that cannot attach the namespace, and the failure surfaces as a device
// that is not there rather than as a missing field.

package controller

import (
	"errors"
	"testing"

	"github.com/simplyblock/atlas/export"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	specCluster = "f0bb9077-78c4-4482-9ccf-a5693ce2df78"
	specPool    = "9d016dd4-34d7-42f0-b549-52a5af2f1399"
	specVolume  = "bfc56677-d602-4017-804b-975f3b929e3f"
)

func readyExport() *simplyblockv1alpha2.NFSExport {
	return testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Spec.VolumeRef = "nfs:" + specCluster + ":" + specPool + ":" + specVolume
		e.Status.LVolID = specVolume
		e.Status.AllowedClients = []string{"192.168.10.0/24"}
	})
}

func TestSpecCarriesTheVolumesClusterAndPool(t *testing.T) {
	spec, err := specFor(readyExport())
	if err != nil {
		t.Fatalf("specFor: %v", err)
	}
	if spec.ClusterID != specCluster {
		t.Errorf("cluster = %q, want %q", spec.ClusterID, specCluster)
	}
	if spec.PoolID != specPool {
		t.Errorf("pool = %q, want %q", spec.PoolID, specPool)
	}
	if spec.VolumeUUID != specVolume {
		t.Errorf("volume = %q, want %q", spec.VolumeUUID, specVolume)
	}
}

// A malformed volumeRef is refused rather than passed on half-filled. The host
// would report a device that is not there, which names the wrong problem and
// sends whoever reads it looking at the fabric.
func TestSpecRefusesAVolumeRefItCannotRead(t *testing.T) {
	e := readyExport()
	e.Spec.VolumeRef = "not-a-handle"

	_, err := specFor(e)
	if err == nil {
		t.Fatal("a malformed volumeRef produced a spec")
	}
	if !errors.Is(err, export.ErrInvalidSpec) {
		t.Errorf("err = %v, want it to carry ErrInvalidSpec", err)
	}
}

// The backing volume id is written by provisioning, and until it arrives there
// is nothing to attach. Waiting is right; guessing is not.
func TestSpecWaitsForTheBackingVolumeID(t *testing.T) {
	e := readyExport()
	e.Status.LVolID = ""

	if _, err := specFor(e); err == nil {
		t.Fatal("a spec was built with no backing volume")
	}
}
