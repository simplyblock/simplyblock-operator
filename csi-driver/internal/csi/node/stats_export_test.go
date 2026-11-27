// The VolumeCondition of a pNFS volume, which follows its NFSExport's MDSReady.
package node

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const (
	testLvolID   = "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
	testHandle   = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90:pool-a:" + testLvolID
	exportsList  = "NFSExportList"
	testExportNS = "pnfs-apps"
)

func exportWithMDSReady(status, message string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.simplyblock.io/v1alpha2",
		"kind":       "NFSExport",
		"metadata":   map[string]any{"name": "nfsexp-" + testLvolID, "namespace": testExportNS},
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "MDSReady", "status": status, "reason": "HealthCheckFailed", "message": message,
		}}},
	}}
}

// pnfsCondition runs NodeGetVolumeStats on a directory taken for an NFS mount,
// with the given NFSExports in the cluster.
func pnfsCondition(t *testing.T, objects ...runtime.Object) *csi.VolumeCondition {
	t.Helper()
	return pnfsConditionAt(t, t.TempDir(), objects...)
}

func pnfsConditionAt(t *testing.T, volumePath string, objects ...runtime.Object) *csi.VolumeCondition {
	t.Helper()
	prev := isNFSPath
	isNFSPath = func(string) bool { return true }
	t.Cleanup(func() { isNFSPath = prev })

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nfsExportGVR: exportsList}, objects...)
	ns := &Server{}
	ns.SetExportReader(dyn)
	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: testHandle, VolumePath: volumePath,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	return resp.GetVolumeCondition()
}

// Regression: a pNFS volume whose export is not served reported Abnormal=false.
func TestAPNFSVolumeWhoseExportIsNotServedIsAbnormal(t *testing.T) {
	cond := pnfsCondition(t, exportWithMDSReady("False", "volume cannot be attached"))

	if !cond.GetAbnormal() || !strings.Contains(cond.GetMessage(), "volume cannot be attached") {
		t.Errorf("condition = %+v, want Abnormal with the export's message", cond)
	}
}

func TestAPNFSVolumeWhoseExportIsServedIsNormal(t *testing.T) {
	if cond := pnfsCondition(t, exportWithMDSReady("True", "")); cond.GetAbnormal() {
		t.Errorf("condition = %+v, want normal", cond)
	}
}

// No export record or no condition yet says nothing about health.
func TestAPNFSVolumeWithNoExportConditionIsNormal(t *testing.T) {
	if cond := pnfsCondition(t); cond.GetAbnormal() {
		t.Errorf("condition = %+v, want normal", cond)
	}
}

// Regression: the export was read only after stat and statfs on the mount,
// which block on a hard mount whose metadata server is down. An unserved
// export is reported without touching the mount; a path that does not exist
// stands in for one that cannot be reached.
func TestAnUnservedPNFSVolumeIsReportedWithoutTouchingItsMount(t *testing.T) {
	unreachable := filepath.Join(t.TempDir(), "unreachable")

	cond := pnfsConditionAt(t, unreachable, exportWithMDSReady("False", "metadata server unreachable"))

	if !cond.GetAbnormal() {
		t.Errorf("condition = %+v, want Abnormal", cond)
	}
}
