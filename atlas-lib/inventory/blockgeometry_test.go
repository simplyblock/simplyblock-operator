// The block geometry the scan reads off a real host: logical block size and the
// atomic write units, from the EKS node captured on 2026-10-27 (i4i.large,
// Amazon Linux 2023, kernel 6.18).
//
// The node carries the three NVMe devices AWS hands a storage node: an EBS gp3
// root volume, the instance store, and an EBS io2 volume attached for the
// capture. All three are formatted at 512 bytes and differ in what they promise
// across a power failure, which is the reading the metadata journal is decided
// on.

package inventory

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/inventory/transcript"
	"github.com/simplyblock/atlas/ptr"
)

func TestTheEKSNodesDevicesReportTheirAtomicity(t *testing.T) {
	root := hostFixture(t, "eks-i4i-large-al2023")
	cfg := syntheticHost(root)
	cfg.DevRoot = devRootOf(root)
	cfg.HostRoot = transcript.HostRootOf(root)

	inv, err := Collect(context.Background(), cfg)
	if err != nil {
		t.Logf("the reading reported: %v", err)
	}
	if inv.HostOS.Distro != "amzn" {
		t.Errorf("host OS = %+v, want Amazon Linux", inv.HostOS)
	}

	for _, tc := range []struct {
		device, model        string
		blockSize            uint32
		atomicMin, atomicMax uint32
	}{
		// EBS: 16 KiB atomic within 16 KiB boundaries, as AWS documents.
		{"nvme0n1", "Amazon Elastic Block Store", 512, 512, 16384},
		{"nvme2n1", "Amazon Elastic Block Store", 512, 512, 16384},
		// The i4i instance store promises one 512-byte block.
		{"nvme1n1", "Amazon EC2 NVMe Instance Storage", 512, 512, 512},
	} {
		t.Run(tc.device, func(t *testing.T) {
			found := false
			for _, c := range inv.Devices {
				if c.Name != tc.device {
					continue
				}
				found = true
				if c.Model != tc.model || c.LogicalBlockSize != tc.blockSize {
					t.Errorf("%s is %q with %d-byte blocks, want %q with %d", tc.device, c.Model, c.LogicalBlockSize, tc.model, tc.blockSize)
				}
				if ptr.From(c.AtomicWriteUnitMinBytes, 0) != tc.atomicMin || ptr.From(c.AtomicWriteUnitMaxBytes, 0) != tc.atomicMax {
					t.Errorf("%s atomic write units = %v..%v, want %d..%d", tc.device,
						c.AtomicWriteUnitMinBytes, c.AtomicWriteUnitMaxBytes, tc.atomicMin, tc.atomicMax)
				}
				if c.AtomicAt(4096) != (tc.atomicMax >= 4096) {
					t.Errorf("%s AtomicAt(4096) = %v", tc.device, c.AtomicAt(4096))
				}
			}
			if !found {
				t.Fatalf("the scan found no %s", tc.device)
			}
		})
	}
}
