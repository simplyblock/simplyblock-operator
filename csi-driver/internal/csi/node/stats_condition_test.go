// The VolumeCondition NodeGetVolumeStats reports, which kubelet turns into a Pod event.
package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func volumeCondition(t *testing.T, req *csi.NodeGetVolumeStatsRequest) *csi.VolumeCondition {
	t.Helper()
	req.VolumeId = "vol"
	resp, err := (&Server{}).NodeGetVolumeStats(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetVolumeCondition() == nil {
		t.Fatal("no VolumeCondition in the response")
	}
	return resp.GetVolumeCondition()
}

func TestNodeGetVolumeStatsHealthyDirectoryReportsNormal(t *testing.T) {
	if cond := volumeCondition(t, &csi.NodeGetVolumeStatsRequest{VolumePath: t.TempDir()}); cond.GetAbnormal() {
		t.Fatalf("want Abnormal=false, got %+v", cond)
	}
}

// Regression: stat on the mount can succeed after the NVMe-oF device is gone.
func TestNodeGetVolumeStatsDisconnectedDeviceReportsAbnormal(t *testing.T) {
	staging, mount := t.TempDir(), t.TempDir()
	missing := filepath.Join(staging, "nvme-disconnected")
	if err := stashVolumeContext(map[string]string{"devicePath": missing}, staging); err != nil {
		t.Fatalf("stash volume context: %v", err)
	}
	cond := volumeCondition(t, &csi.NodeGetVolumeStatsRequest{VolumePath: mount, StagingTargetPath: staging})
	if !cond.GetAbnormal() || !strings.Contains(cond.GetMessage(), missing) {
		t.Fatalf("want Abnormal=true naming %s, got %+v", missing, cond)
	}
}

func TestNodeGetVolumeStatsBlockSizeErrorReportsAbnormal(t *testing.T) {
	target := filepath.Join(t.TempDir(), "block-target")
	if err := os.WriteFile(target, []byte("not a block device"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cond := volumeCondition(t, &csi.NodeGetVolumeStatsRequest{VolumePath: target}); !cond.GetAbnormal() {
		t.Fatalf("want Abnormal=true, got %+v", cond)
	}
}
