// Tests for the snapshot calls.

package controlplane

import (
	"context"
	"net/http"
	"testing"
)

func TestClientVolumeSnapshots(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testSnapshot + `","name":"sbk-1","status":"online","group_id":"","group_seq":0,` +
			`"created_at":"2026-10-09T10:00:00Z","health_check":true,"migrating":false,"size":1,"used_size":1,"lvol":null}]`))
	})

	got, err := c.VolumeSnapshots(context.Background(), testCluster, testPool, testVolume)
	if err != nil || len(got) != 1 || got[0].ID != testSnapshot || got[0].Name != "sbk-1" {
		t.Fatalf("snapshots = %+v, err = %v", got, err)
	}
}
