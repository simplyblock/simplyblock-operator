package identity

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// advertisesGroupController reports whether GetPluginCapabilities lists the
// GROUP_CONTROLLER_SERVICE plugin capability.
func advertisesGroupController(t *testing.T, d *csicommon.CSIDriver) bool {
	t.Helper()
	resp, err := New(d).GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	for _, c := range resp.GetCapabilities() {
		if c.GetService().GetType() == csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE {
			return true
		}
	}
	return false
}

func TestGroupControllerCapabilityGatedOnEnable(t *testing.T) {
	off := csicommon.NewCSIDriver("test.csi.simplyblock.io", "v0", "node")
	if advertisesGroupController(t, off) {
		t.Error("GROUP_CONTROLLER_SERVICE must not be advertised before EnableGroupController")
	}

	on := csicommon.NewCSIDriver("test.csi.simplyblock.io", "v0", "node")
	on.EnableGroupController()
	if !advertisesGroupController(t, on) {
		t.Error("GROUP_CONTROLLER_SERVICE must be advertised after EnableGroupController")
	}
}
