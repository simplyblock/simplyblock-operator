// What a striped volume's stack has to be on the node: the members under the
// LVM layers, in the order the volume was created with, whether the plan is
// built for a stage from the volume context or for a teardown from the record.

package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

const (
	stripedShapeNames    = "members → lvmPhysicalVolume → lvmVolumeGroup → lvmLogicalVolume → filesystem"
	stripedRawBlockNames = "members → lvmPhysicalVolume → lvmVolumeGroup → lvmLogicalVolume"

	memberB = "9f3ecd0e-2c3d-4a4b-8e5f-6a7b8c9d0e1f"
	memberC = "a04fde1f-3d4e-4b5c-9f60-7b8c9d0e1f20"
)

// stripedContext is the context a staged striped volume of three members
// leaves behind.
func stripedContext() map[string]string {
	vc := stagedContext()
	vc[csicommon.CtxStripeWidth] = "3"
	vc[csicommon.CtxStripeMembers] = strings.Join([]string{pvcTestLvol, memberB, memberC}, ",")
	vc[csicommon.CtxStripeChunkBytes] = "131072"
	return vc
}

// memberNQN is the subsystem a member is published from.
func memberNQN(id string) string { return "nqn.2023-05.io.simplyblock:lvol:" + id }

// writeStripedRecord writes the record a striped stage leaves, with the members
// in the order given and the logical volume striped across them.
func writeStripedRecord(t *testing.T, s *stack, handle string, members []string, withFilesystem bool) {
	t.Helper()
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fabrics := make([]volstack.Entry, 0, len(members))
	for _, id := range members {
		fabrics = append(fabrics, volstack.Entry{
			Layer:  layerFabric,
			Params: raw(layers.FabricParams{NQN: memberNQN(id), NSID: 1}),
		})
	}
	plan := []volstack.Entry{
		{Layer: layerMembers, Members: fabrics},
		{Layer: layerLVMPhysicalVolume},
		{Layer: layerLVMVolumeGroup},
		{Layer: layerLVMLogicalVolume, Params: raw(layers.LVMLogicalVolumeParams{
			Stripes: len(members), StripeChunkBytes: 128 << 10,
		})},
	}
	if withFilesystem {
		plan = append(plan, volstack.Entry{Layer: layerFilesystem, Params: raw(layers.FilesystemParams{FsType: "xfs"})})
	}
	record := volstack.Record{Version: volstack.RecordVersion, VolumeHandle: handle, Plan: plan}
	if err := s.store.Write(record); err != nil {
		t.Fatalf("write the stack record: %v", err)
	}
}

// memberNQNs is the subsystem each member of a plan's members layer attaches,
// in member order.
func memberNQNs(t *testing.T, plan volstack.Plan) []string {
	t.Helper()
	composite, ok := plan[0].(volstack.Composite)
	if !ok {
		t.Fatalf("the bottom of the plan is %T, not the members composite", plan[0])
	}
	nqns := make([]string, 0, len(composite.Members()))
	for _, member := range composite.Members() {
		nqns = append(nqns, member.(volstack.Recorder).Params().(layers.FabricParams).NQN)
	}
	return nqns
}

// stripes is the stripe count and chunk the plan's logical volume is created
// with.
func stripes(t *testing.T, plan volstack.Plan) layers.LVMLogicalVolumeParams {
	t.Helper()
	for _, l := range plan {
		recorder, ok := l.(volstack.Recorder)
		if !ok {
			continue
		}
		if p, ok := recorder.Params().(layers.LVMLogicalVolumeParams); ok {
			return p
		}
	}
	t.Fatal("the plan has no logical volume")
	return layers.LVMLogicalVolumeParams{}
}

func TestShapeForAStripedVolume(t *testing.T) {
	if got := shapeFor(stripedContext(), mountCapability()); got != shapeStriped {
		t.Errorf("a striped filesystem volume is shape %v, want striped", got)
	}
	if got := shapeFor(stripedContext(), blockCapability()); got != shapeStripedRawBlock {
		t.Errorf("a striped block volume is shape %v, want striped raw block", got)
	}
	// A width of 1 in the context is no stripe: the ordinary rows are chosen
	// exactly as before.
	vc := stagedContext()
	vc[csicommon.CtxStripeWidth] = "1"
	if got := shapeFor(vc, mountCapability()); got != shapePlain {
		t.Errorf("a context of width 1 is shape %v, want plain", got)
	}
}

func TestStripedPlansAreTheMembersUnderTheLVMLayers(t *testing.T) {
	s, _ := newTestStack(t, newRecordingRunner())
	node := s.node("", nil)
	vc := stripedContext()
	volume := stackVolume("/staging", vc, mountCapability())
	conns := []string{pvcTestLvol, memberB, memberC}
	connections := connectionsFor(conns)

	plan := stripedPlanFor(node, connections, volume, stripeOptions(vc), shapeStriped)
	if got := strings.Join(plan.Names(), " → "); got != stripedShapeNames {
		t.Errorf("a striped filesystem volume stages as %s", got)
	}
	if p := stripes(t, plan); p.Stripes != 3 || p.StripeChunkBytes != 128<<10 {
		t.Errorf("the logical volume is %+v, want 3 stripes of 128KiB", p)
	}
	if got := memberNQNs(t, plan); strings.Join(got, ",") != strings.Join(nqnsOf(conns), ",") {
		t.Errorf("members attach %v, want %v in that order", got, nqnsOf(conns))
	}

	raw := stripedPlanFor(node, connections, volume, stripeOptions(vc), shapeStripedRawBlock)
	if got := strings.Join(raw.Names(), " → "); got != stripedRawBlockNames {
		t.Errorf("a striped block volume stages as %s, and nothing may format it", got)
	}
}

// TestTeardownOfAStripedVolumeReplaysTheRecordedOrder holds a teardown to the
// members as they were brought up, which the record keeps, rather than to the
// order the context happens to list them in now.
func TestTeardownOfAStripedVolumeReplaysTheRecordedOrder(t *testing.T) {
	for _, withFilesystem := range []bool{true, false} {
		ns, _ := newStackedServer(t, newRecordingRunner())
		recorded := []string{pvcTestLvol, memberB, memberC}
		writeStripedRecord(t, ns.stack, pvcTestHandle, recorded, withFilesystem)

		// A context listing them in another order does not reorder the stripe.
		vc := stripedContext()
		vc[csicommon.CtxStripeMembers] = strings.Join([]string{pvcTestLvol, memberC, memberB}, ",")

		plan, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", vc)
		if err != nil {
			t.Fatalf("teardownPlan: %v", err)
		}
		want := stripedRawBlockNames
		if withFilesystem {
			want = stripedShapeNames
		}
		if got := strings.Join(plan.Names(), " → "); got != want {
			t.Errorf("the teardown walks %s, want %s", got, want)
		}
		if got := memberNQNs(t, plan); strings.Join(got, ",") != strings.Join(nqnsOf(recorded), ",") {
			t.Errorf("the teardown releases %v, want the recorded order %v", got, nqnsOf(recorded))
		}
		if p := stripes(t, plan); p.Stripes != 3 {
			t.Errorf("the teardown's logical volume is %+v, want the recorded 3 stripes", p)
		}
	}
}

func TestTeardownOfAStripedVolumeWithoutARecord(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	plan, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", stripedContext())
	if err != nil {
		t.Fatalf("teardownPlan: %v", err)
	}
	if got := strings.Join(plan.Names(), " → "); got != stripedShapeNames {
		t.Errorf("a striped volume with no record is released as %s, not as one member", got)
	}
}

// TestAttachingAStripedVolumeWithoutTheControlPlane is the heal or publish of a
// staged striped volume while the control plane does not answer: the record
// names every member, and that is enough to find namespaces already attached.
// With no record either, there is nothing to attach from, and that is an error
// rather than a stage of the first member alone.
func TestAttachingAStripedVolumeWithoutTheControlPlane(t *testing.T) {
	t.Setenv("SPDKCSI_SECRET", t.TempDir()+"/no-such-secret.json")

	ns, _ := newStackedServer(t, newRecordingRunner())
	_, err := ns.attachPlan(context.Background(), pvcTestHandle, "/staging", stripedContext(), mountCapability())
	if err == nil {
		t.Error("attachPlan built a striped plan with neither the control plane nor a record")
	}

	recorded := []string{pvcTestLvol, memberB, memberC}
	writeStripedRecord(t, ns.stack, pvcTestHandle, recorded, true)
	plan, err := ns.attachPlan(context.Background(), pvcTestHandle, "/staging", stripedContext(), mountCapability())
	if err != nil {
		t.Fatalf("attachPlan: %v", err)
	}
	if got := strings.Join(plan.Names(), " → "); got != stripedShapeNames {
		t.Errorf("a striped volume attaches as %s", got)
	}
	if got := memberNQNs(t, plan); strings.Join(got, ",") != strings.Join(nqnsOf(recorded), ",") {
		t.Errorf("members attach %v, want %v", got, nqnsOf(recorded))
	}
}

func TestAStripedContextListingTheWrongNumberOfMembersIsRefused(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	vc := stripedContext()
	vc[csicommon.CtxStripeMembers] = pvcTestLvol + "," + memberB
	if _, err := ns.attachPlan(context.Background(), pvcTestHandle, "/staging", vc, mountCapability()); err == nil {
		t.Error("attachPlan accepted a width-3 volume whose context lists two members")
	}
}

// fakeMembers is a members layer exposing fixed devices.
type fakeMembers struct{ paths []string }

func (f fakeMembers) Name() string                                     { return layerMembers }
func (f fakeMembers) Members() volstack.Plan                           { return nil }
func (f fakeMembers) Release(context.Context, volstack.Artifact) error { return nil }
func (f fakeMembers) Destroy(context.Context, volstack.Artifact) error { return nil }
func (f fakeMembers) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	_, a, err := f.Observe(ctx, below)
	return a, err
}
func (f fakeMembers) Observe(context.Context, volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	var a volstack.Artifact
	for _, p := range f.paths {
		a.Devices = append(a.Devices, blockdev.Device{Path: p})
	}
	return volstack.StateReady, a, nil
}

// TestTheMembersAreWhatThePathLossWatchSees holds the watch to the members'
// namespaces, which sysfs lists, rather than to the striped device on top, which
// it does not and would report gone on its first look.
func TestTheMembersAreWhatThePathLossWatchSees(t *testing.T) {
	vc := stripedContext()
	members := fakeMembers{paths: []string{"/dev/nvme1n1", "/dev/nvme2n1"}}
	if !markMembersPresent(context.Background(), volstack.Plan{members}, vc) {
		t.Fatal("a striped plan was not recognized")
	}
	if vc[stripeDevicePathsKey] != "/dev/nvme1n1,/dev/nvme2n1" {
		t.Errorf("recorded member devices %q", vc[stripeDevicePathsKey])
	}
	forgetMembers(vc)

	plain := stagedContext()
	s, _ := newTestStack(t, newRecordingRunner())
	plan := planFor(s.node("", nil), connectionFromContext(plain), stackVolume("/staging", plain, mountCapability()),
		vdoOptions(plain), shapePlain)
	if markMembersPresent(context.Background(), plan, plain) {
		t.Error("an ordinary plan was taken for a striped one")
	}
	if _, ok := plain[stripeDevicePathsKey]; ok {
		t.Error("an ordinary volume's context gained member devices")
	}
}

func nqnsOf(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, memberNQN(id))
	}
	return out
}

func connectionsFor(ids []string) []lvol.Connection {
	out := make([]lvol.Connection, 0, len(ids))
	for _, id := range ids {
		out = append(out, lvol.Connection{NQN: memberNQN(id), NSID: 1, UUID: id})
	}
	return out
}

// TestExpandOfAStripedVolumeGrowsTheWholeStack holds a striped expansion to the
// striped plan: the volume group resizes every member's physical volume, the
// logical volume extends across what they gained, and the filesystem grows onto
// it. Growing the first member's stack alone would leave the stripe unchanged.
func TestExpandOfAStripedVolumeGrowsTheWholeStack(t *testing.T) {
	t.Setenv("SPDKCSI_SECRET", t.TempDir()+"/no-such-secret.json")
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	writeStripedRecord(t, ns.stack, pvcTestHandle, []string{pvcTestLvol, memberB, memberC}, true)
	parent := stashedAt(t, stripedContext())

	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
		VolumePath:        filepath.Join(parent, pvcTestHandle),
		VolumeCapability:  mountCapability(),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if got := strings.Join(runner.plans["grow"], " → "); got != stripedShapeNames {
		t.Errorf("the expansion walked %s, want %s", got, stripedShapeNames)
	}
}

// TestTeardownOfAStripedVolumeWithoutItsContextNamesTheGroup is the unstage
// after the stashed context was lost: the record still names the members, and
// the volume group is named after the first member, read off the volume id.
func TestTeardownOfAStripedVolumeWithoutItsContextNamesTheGroup(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	writeStripedRecord(t, ns.stack, pvcTestHandle+":s3", []string{pvcTestLvol, memberB, memberC}, true)

	plan, err := ns.teardownPlan(context.Background(), pvcTestHandle+":s3", "/staging", map[string]string{})
	if err != nil {
		t.Fatalf("teardownPlan: %v", err)
	}
	if got := strings.Join(plan.Names(), " → "); got != stripedShapeNames {
		t.Fatalf("the teardown walks %s, want %s", got, stripedShapeNames)
	}
	for _, l := range plan {
		recorder, ok := l.(volstack.Recorder)
		if !ok {
			continue
		}
		if p, ok := recorder.Params().(layers.LVMLogicalVolumeParams); ok && p.Stripes != 3 {
			t.Errorf("the logical volume is %+v, want 3 stripes", p)
		}
	}
	want := strings.Join(nqnsOf([]string{pvcTestLvol, memberB, memberC}), ",")
	if got := strings.Join(memberNQNs(t, plan), ","); got != want {
		t.Errorf("the teardown releases %s, want %s", got, want)
	}
	volume := teardownVolume(pvcTestHandle+":s3", "/staging", map[string]string{})
	if got := volume.VolumeGroup(); got != "vol-"+pvcTestLvol {
		t.Errorf("the teardown releases volume group %q, want the first member's", got)
	}
}
