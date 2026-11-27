// Tests for what the node plugin advertises.

package node

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// kubelet sends SINGLE_NODE_SINGLE_WRITER for a ReadWriteOncePod claim only to
// a node that advertises SINGLE_NODE_MULTI_WRITER.
func TestTheNodeAdvertisesReadWriteOncePod(t *testing.T) {
	resp, err := (&Server{}).NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER {
			return
		}
	}
	t.Errorf("node capabilities %v lack SINGLE_NODE_MULTI_WRITER", resp.GetCapabilities())
}
