// Tests for bucket delivery's pure path derivation and validation: the part
// provable without a live cluster. Deliver and Withdraw themselves call
// stagePNFSVolume/publishPNFSVolume/unstagePNFSVolume, which reach real
// NVMe-oF and sysfs state with no injection seam (the same limitation the
// existing pNFS stage/publish tests already have, not one this file adds),
// so the end-to-end mount behavior is this feature's W-11 acceptance
// criterion that needs a live cluster, not a unit test.

package node

import (
	"strings"
	"testing"
)

func TestBucketTargetPathIsDeterministicAndUnderTheGatewayRoot(t *testing.T) {
	path, err := bucketTargetPath("team-a-reports")
	if err != nil {
		t.Fatalf("bucketTargetPath: %v", err)
	}
	if !strings.HasPrefix(path, s3GatewayMountRoot+"/") {
		t.Errorf("path = %q, want it under %q", path, s3GatewayMountRoot)
	}
	again, err := bucketTargetPath("team-a-reports")
	if err != nil {
		t.Fatalf("bucketTargetPath (second call): %v", err)
	}
	if path != again {
		t.Errorf("bucketTargetPath is not deterministic: %q vs %q", path, again)
	}
}

func TestBucketTargetPathRefusesAPathTraversingName(t *testing.T) {
	for _, name := range []string{"", "../etc", "a/b", `a\b`, "a..b/../../etc"} {
		if _, err := bucketTargetPath(name); err == nil {
			t.Errorf("bucketTargetPath(%q) succeeded, want a refusal", name)
		}
	}
}

func TestBucketStagingPathDoesNotCollideWithTheGatewayRoot(t *testing.T) {
	staging, err := bucketStagingPath("team-a-reports")
	if err != nil {
		t.Fatalf("bucketStagingPath: %v", err)
	}
	target, err := bucketTargetPath("team-a-reports")
	if err != nil {
		t.Fatalf("bucketTargetPath: %v", err)
	}
	if staging == target {
		t.Errorf("staging path equals target path: %q", staging)
	}
	if strings.HasPrefix(staging, s3GatewayMountRoot) {
		t.Errorf("staging path %q sits under the published gateway root; a bind source must not", staging)
	}
}

// 0004-direct-csi-mount-spike-result.md's own finding: a path outside the
// three Bidirectional roots stays invisible to every other pod while still
// looking like it succeeded. validateDeliveryPath is the guard against this
// file's own derivation ever producing one.
func TestValidateDeliveryPathAcceptsEveryBidirectionalRoot(t *testing.T) {
	for _, root := range bidirectionalRoots {
		if err := validateDeliveryPath(root + "/csi.simplyblock.io/s3-gateway-mounts/b"); err != nil {
			t.Errorf("validateDeliveryPath under %s: %v", root, err)
		}
	}
}

func TestValidateDeliveryPathRejectsEverythingElse(t *testing.T) {
	for _, path := range []string{
		"/tmp/s3-gateway-mounts/b",
		"/var/lib/kubelet", // the root itself, not a subdirectory
		"/var/lib/kubeletx/plugins/b",
		"/",
	} {
		if err := validateDeliveryPath(path); err == nil {
			t.Errorf("validateDeliveryPath(%q) succeeded, want a refusal", path)
		}
	}
}

func TestBucketVolumeCapabilityAdvertisesMultiNodeMultiWriter(t *testing.T) {
	capability := bucketVolumeCapability()
	if capability.GetMount() == nil {
		t.Error("capability carries no mount access type")
	}
	if capability.GetAccessMode() == nil || capability.GetAccessMode().GetMode().String() != "MULTI_NODE_MULTI_WRITER" {
		t.Errorf("access mode = %v, want MULTI_NODE_MULTI_WRITER", capability.GetAccessMode())
	}
}
