// What a volume asking for client-side compression or deduplication stages as.
//
// Everything here is about selection rather than about VDO itself: which row of
// the plan catalog the class parameters name, what the logical-volume layer is
// built with, and what a format on top of a virtualized device is told. The
// mechanism the selection reaches is atlas-lib's, and is tested there.

package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/kube"
)

// lvmRawBlockShape is the LVM row with its filesystem absent, which is what a
// compressed volume the pod opens as a block device stages as.
const lvmRawBlockShape = "fabric → lvmPhysicalVolume → lvmVolumeGroup → lvmLogicalVolume"

// vdoContext is a staged volume's context with the client-side parameters the
// class was written with.
func vdoContext(compression, deduplication string) map[string]string {
	vc := stagedContext()
	vc[kube.ParamClientCompression] = compression
	vc[kube.ParamClientDeduplication] = deduplication
	return vc
}

// A volume whose class asks for client-side compression or deduplication stages
// as the LVM row, not the plain one: the three LVM layers are what put a VDO
// device between the namespace and the filesystem, and a plan without them
// would format the namespace directly and silently drop the feature the class
// asked for.
func TestPlanForSelectsTheLVMShapeForAClientSideVolume(t *testing.T) {
	cases := []struct {
		name                       string
		compression, deduplication string
		capability                 *csi.VolumeCapability
		want                       string
	}{
		{"compression only", "true", "false", mountCapability(), lvmShape},
		{"deduplication only", "false", "true", mountCapability(), lvmShape},
		{"both", "true", "true", mountCapability(), lvmShape},
		{"neither", "false", "false", mountCapability(), plainShape},
		{"unreadable", "yes please", "", mountCapability(), plainShape},
		{"raw block", "true", "false", blockCapability(), lvmRawBlockShape},
		{"raw block without it", "false", "false", blockCapability(), "fabric"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStack(t, newRecordingRunner())
			vc := vdoContext(tc.compression, tc.deduplication)
			volume := stackVolume("/staging", vc, tc.capability)

			plan := planFor(
				s.node("", nil), connectionFromContext(vc), volume, vdoOptions(vc), shapeFor(vc, tc.capability))

			if got := strings.Join(plan.Names(), " → "); got != tc.want {
				t.Errorf("the volume stages as %s, want %s", got, tc.want)
			}
		})
	}
}

// The two parameters are independent switches, and a volume asking for one gets
// exactly that. Collapsing them into a single "VDO on" would turn a class that
// asked to compress into one that also deduplicates, which costs memory and
// throughput the class never asked to spend.
func TestVDOOptionsCarryEachSwitchOnItsOwn(t *testing.T) {
	compressionOnly := vdoOptions(vdoContext("true", "false"))
	if !compressionOnly.Definition.Compression || compressionOnly.Definition.Deduplication {
		t.Errorf("a compression-only class produced %+v", compressionOnly.Definition)
	}

	deduplicationOnly := vdoOptions(vdoContext("false", "true"))
	if deduplicationOnly.Definition.Compression || !deduplicationOnly.Definition.Deduplication {
		t.Errorf("a deduplication-only class produced %+v", deduplicationOnly.Definition)
	}
}

// The pool is named, and the node capability is declared. Without the pool name
// the layer creates a plain linear volume and the feature is silently absent;
// without the capability a volume can be staged on a node whose kernel cannot
// map it, which surfaces as a mount error rather than as a placement decision.
func TestVDOOptionsNameThePoolAndTheCapability(t *testing.T) {
	options := vdoOptions(vdoContext("true", "true"))

	if options.PoolName != vdoPoolName {
		t.Errorf("the pool is %q, want %q", options.PoolName, vdoPoolName)
	}
	if string(options.Capability) != kube.LabelVDOCapable {
		t.Errorf("the capability is %q, want %q", options.Capability, kube.LabelVDOCapable)
	}
}

// A format on top of a VDO device skips mkfs's full-device discard, which VDO's
// block map processes in proportion to the volume's size, and carries none of
// the stripe hints computed for the erasure-coded device underneath: VDO
// relocates blocks, so the filesystem no longer sits on the layout those hints
// describe.
func TestStackVolumeFormatsAVDOVolumeWithoutDiscardOrStripeHints(t *testing.T) {
	vc := vdoContext("true", "false")
	vc["xfs_su"] = "32k"
	vc["xfs_sw"] = "4"

	options := stackVolume("/staging", vc, xfsCapability()).FormatOptions

	if !slices.Contains(options, "-K") {
		t.Errorf("mkfs.xfs was not told to skip the discard: %v", options)
	}
	if slices.Contains(options, "su=32k,sw=4") {
		t.Errorf("the backend's stripe hints were applied to a virtualized device: %v", options)
	}
}

// The same volume without the client-side parameters keeps both, because
// nothing virtualizes its blocks and the discard is near-free on the namespace
// itself.
func TestStackVolumeKeepsTheStripeHintsWithoutVDO(t *testing.T) {
	vc := stagedContext()
	vc["xfs_su"] = "32k"
	vc["xfs_sw"] = "4"

	options := stackVolume("/staging", vc, xfsCapability()).FormatOptions

	if !slices.Contains(options, "su=32k,sw=4") {
		t.Errorf("a plain volume lost the stripe hints its class asked for: %v", options)
	}
	if slices.Contains(options, "-K") {
		t.Errorf("a plain volume skipped a discard nothing asked it to skip: %v", options)
	}
}

// xfsCapability is a mount capability asking for XFS, which is the filesystem
// the stripe hints belong to.
func xfsCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// The teardown takes the pool's name from the record rather than from the
// volume's class, because a class can be edited or deleted after the volume was
// provisioned and a release pointed at a pool by another name finds nothing.
func TestTeardownReadsThePoolNameFromTheRecord(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	writeRecord(t, ns.stack, pvcTestHandle, strings.Split(lvmShape, " → "))

	// The context says the volume wants none of this, which is what a class
	// edited after provisioning leaves behind.
	if _, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", stagedContext()); err != nil {
		t.Fatalf("teardownPlan: %v", err)
	}

	record, err := ns.stack.store.Load(pvcTestHandle)
	if err != nil {
		t.Fatalf("load the record: %v", err)
	}
	options, ok := recordedLVMOptions(record)
	if !ok {
		t.Fatal("the record names no logical volume to take a pool name from")
	}
	if options.PoolName != vdoPoolName {
		t.Errorf("the teardown would release the pool %q, want %q", options.PoolName, vdoPoolName)
	}
}
