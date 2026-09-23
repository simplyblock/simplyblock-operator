package controlplane

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

func TestSizeToInt(t *testing.T) {
	if got, err := sizeToInt(1 << 30); err != nil || got != 1<<30 {
		t.Errorf("sizeToInt(1GiB) = %d, %v; want 1073741824, nil", got, err)
	}
	if _, err := sizeToInt(math.MaxUint64); err == nil {
		t.Error("sizeToInt(MaxUint64) = nil error, want overflow error")
	}
}

// TestClientSubsystemVolumesSpansEveryPool. A migration moves an NVMe-oF
// subsystem rather than one volume inside it, so whoever asks for a migration
// has to know which volumes travel with the one they named. Nothing addresses a
// subsystem's members directly, and the members are not required to share a
// pool: a subsystem is a cluster-level object, so the answer is every pool's
// volumes filtered by the NQN they publish under.
func TestClientSubsystemVolumesSpansEveryPool(t *testing.T) {
	const otherPool = "55555555-5555-5555-5555-555555555555"
	const wanted = "nqn.wanted"

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
			_, _ = w.Write([]byte(`[` +
				`{"id":"` + testPool + `","cluster_id":"` + testCluster + `","name":"pool1",` +
				`"max_size":0,"capacity":{},"max_r_mbytes":0,"max_rw_iops":0,"max_rw_mbytes":0,` +
				`"max_w_mbytes":0,"volume_max_size":0,"status":"active"},` +
				`{"id":"` + otherPool + `","cluster_id":"` + testCluster + `","name":"pool2",` +
				`"max_size":0,"capacity":{},"max_r_mbytes":0,"max_rw_iops":0,"max_rw_mbytes":0,` +
				`"max_w_mbytes":0,"volume_max_size":0,"status":"active"}]`))
		case strings.Contains(r.URL.Path, "/storage-pools/"+testPool+"/volumes/"):
			_, _ = w.Write([]byte(`[{"id":"` + testVolume + `","name":"vol1","pool_name":"pool1",` +
				`"size":100,"ns_id":1,"nqn":"` + wanted + `"},` +
				`{"id":"44444444-4444-4444-4444-444444444444","name":"vol2","pool_name":"pool1",` +
				`"size":200,"ns_id":2,"nqn":"nqn.elsewhere"}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":"66666666-6666-6666-6666-666666666666","name":"vol3",` +
				`"pool_name":"pool2","size":300,"ns_id":3,"nqn":"` + wanted + `"}]`))
		}
	})

	members, err := c.SubsystemVolumes(context.Background(), testCluster, wanted)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want the two publishing under %s: %+v", len(members), wanted, members)
	}
	for _, m := range members {
		if m.NQN != wanted {
			t.Errorf("member %s publishes under %q, which is a different subsystem", m.Name, m.NQN)
		}
	}
	// The handle is what maps a member back to the PersistentVolume fronting
	// it, so a member from another pool has to carry that pool.
	if members[1].ID != lvol.NewVolumeHandle(testCluster, otherPool, "66666666-6666-6666-6666-666666666666") {
		t.Errorf("the member from the second pool has handle %q", members[1].ID)
	}
}

// TestClientSubsystemVolumesRefusesAnEmptyNQN. Every volume in the cluster
// would otherwise be a member of the subsystem named by nothing.
func TestClientSubsystemVolumesRefusesAnEmptyNQN(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the control plane was asked about a subsystem with no name")
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c.SubsystemVolumes(context.Background(), testCluster, ""); err == nil {
		t.Fatal("an empty NQN was accepted")
	}
}
