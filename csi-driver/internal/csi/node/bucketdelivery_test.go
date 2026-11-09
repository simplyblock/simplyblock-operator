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

	"github.com/simplyblock/atlas/bucketdelivery"
)

// testMountPrefix is an arbitrary, valid prefix for tests that only need
// *a* prefix, not a particular one.
const testMountPrefix = "s3-system/team-a-store"

func TestBucketTargetPathIsDeterministicAndUnderTheGatewayRoot(t *testing.T) {
	path, err := bucketTargetPath(testMountPrefix, "team-a-reports")
	if err != nil {
		t.Fatalf("bucketTargetPath: %v", err)
	}
	if !strings.HasPrefix(path, bucketdelivery.GatewayMountRoot+"/") {
		t.Errorf("path = %q, want it under %q", path, bucketdelivery.GatewayMountRoot)
	}
	again, err := bucketTargetPath(testMountPrefix, "team-a-reports")
	if err != nil {
		t.Fatalf("bucketTargetPath (second call): %v", err)
	}
	if path != again {
		t.Errorf("bucketTargetPath is not deterministic: %q vs %q", path, again)
	}
}

func TestBucketTargetPathRefusesAPathTraversingName(t *testing.T) {
	for _, name := range []string{"", "../etc", "a/b", `a\b`, "a..b/../../etc"} {
		if _, err := bucketTargetPath(testMountPrefix, name); err == nil {
			t.Errorf("bucketTargetPath(%q, %q) succeeded, want a refusal", testMountPrefix, name)
		}
	}
}

func TestBucketTargetPathRefusesAPathTraversingMountPrefix(t *testing.T) {
	for _, prefix := range []string{"", "/s3-system/store", "../etc", `a\b`, "a/../../etc"} {
		if _, err := bucketTargetPath(prefix, "team-a-reports"); err == nil {
			t.Errorf("bucketTargetPath(%q, ...) succeeded, want a refusal", prefix)
		}
	}
}

// Regression: issues/19-mc-smoke-test-bucket-not-found.md, tasks.md D4 — two
// ObjectStores whose gateway pods land on the same node shared one flat
// directory, so one's posix backend (BucketLinks mode) listed every
// subdirectory under it, including buckets delivered for the other
// ObjectStore entirely. mc test_rb against a fresh "smoke-test" bucket in
// one ObjectStore failed against the other's pre-existing bucket of the same
// name, reproduced twice on independently fresh cluster91 deployments.
func TestBucketTargetPathScopesDifferentMountPrefixesToDifferentPaths(t *testing.T) {
	a, err := bucketTargetPath("s3-system/store-a", "smoke-test")
	if err != nil {
		t.Fatalf("bucketTargetPath for store-a: %v", err)
	}
	b, err := bucketTargetPath("s3-system/store-b", "smoke-test")
	if err != nil {
		t.Fatalf("bucketTargetPath for store-b: %v", err)
	}
	if a == b {
		t.Fatalf("two different ObjectStores' same-named bucket both resolved to %q; "+
			"store-b's gateway would list store-a's bucket as its own", a)
	}
}

func TestBucketStagingPathDoesNotCollideWithTheGatewayRoot(t *testing.T) {
	staging, err := bucketStagingPath("team-a-reports")
	if err != nil {
		t.Fatalf("bucketStagingPath: %v", err)
	}
	target, err := bucketTargetPath(testMountPrefix, "team-a-reports")
	if err != nil {
		t.Fatalf("bucketTargetPath: %v", err)
	}
	if staging == target {
		t.Errorf("staging path equals target path: %q", staging)
	}
	if strings.HasPrefix(staging, bucketdelivery.GatewayMountRoot) {
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
