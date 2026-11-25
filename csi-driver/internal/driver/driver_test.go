// Tests for what the driver advertises.

package driver

import (
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// Kubernetes hands a ReadWriteOncePod claim to a driver only when the
// controller and the node both advertise SINGLE_NODE_MULTI_WRITER; it then
// sends SINGLE_NODE_SINGLE_WRITER for RWOP and SINGLE_NODE_MULTI_WRITER for
// RWO, and the driver has to accept both.
func TestTheControllerAdvertisesReadWriteOncePod(t *testing.T) {
	if !slices.Contains(controllerCapabilities, csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER) {
		t.Errorf("controller capabilities %v lack SINGLE_NODE_MULTI_WRITER", controllerCapabilities)
	}
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	} {
		if !slices.Contains(volumeAccessModes, mode) {
			t.Errorf("access modes %v lack %s", volumeAccessModes, mode)
		}
	}
}

// The modes the driver serves stay advertised.
func TestTheServedAccessModesAreAdvertised(t *testing.T) {
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	} {
		if !slices.Contains(volumeAccessModes, mode) {
			t.Errorf("access modes %v lack %s", volumeAccessModes, mode)
		}
	}
}
