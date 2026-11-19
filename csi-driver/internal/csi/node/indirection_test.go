package node

import (
	"context"
	"strings"
	"testing"
)

// A volume staged behind the dm indirection is torn down through it: the
// record names the layer, and the teardown walks it.
func TestTeardownPlanOfAnIndirectVolumeWalksTheMapping(t *testing.T) {
	for _, layers := range [][]string{
		{"fabric", "dmLinear"},
		{"fabric", "dmLinear", "filesystem"},
	} {
		ns, _ := newStackedServer(t, newRecordingRunner())
		writeRecord(t, ns.stack, pvcTestHandle, layers)
		plan, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", stagedContext())
		if err != nil {
			t.Fatalf("teardownPlan(%v): %v", layers, err)
		}
		if got, want := strings.Join(plan.Names(), " → "), strings.Join(layers, " → "); got != want {
			t.Errorf("the teardown walks %s, want %s", got, want)
		}
	}
}

// A fresh stage follows SPDKCSI_DM_INDIRECTION; a staged volume follows its
// record, so the layer is never inserted under (or pulled from under) a live
// consumer by a later heal.
// indirectHandle is a second volume, so the record of one never answers for
// another.
const indirectHandle = "0c5b4c4e-6f1d-4b8e-9d2a-3a1f2b7c9e10:pool-1:7d1e0f5a-2b3c-4d5e-8f90-a1b2c3d4e5f6"

func TestAttachShapeFollowsTheFlagOnlyForAFreshStage(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())

	t.Setenv("SPDKCSI_DM_INDIRECTION", "false")
	if got := ns.attachShape(pvcTestHandle, shapePlain); got != shapePlain {
		t.Errorf("flag off, no record: shape %v, want plain", got)
	}
	t.Setenv("SPDKCSI_DM_INDIRECTION", "true")
	if got := ns.attachShape(pvcTestHandle, shapePlain); got != shapeIndirectPlain {
		t.Errorf("flag on, no record: shape %v, want indirect plain", got)
	}
	if got := ns.attachShape(pvcTestHandle, shapeRawBlock); got != shapeIndirectRawBlock {
		t.Errorf("flag on, no record: shape %v, want indirect raw block", got)
	}
	if got := ns.attachShape(pvcTestHandle, shapeLVM); got != shapeIndirectLVM {
		t.Errorf("flag on, no record: shape %v, want indirect LVM", got)
	}

	writeRecord(t, ns.stack, indirectHandle, []string{"fabric", "filesystem"})
	if got := ns.attachShape(indirectHandle, shapePlain); got != shapePlain {
		t.Errorf("flag on, staged without the layer: shape %v, want plain", got)
	}

	t.Setenv("SPDKCSI_DM_INDIRECTION", "false")
	writeRecord(t, ns.stack, indirectHandle, []string{"fabric", "dmLinear", "filesystem"})
	if got := ns.attachShape(indirectHandle, shapePlain); got != shapeIndirectPlain {
		t.Errorf("flag off, staged with the layer: shape %v, want indirect plain", got)
	}
}

func TestPlanForBuildsTheIndirectRows(t *testing.T) {
	s, _ := newTestStack(t, newRecordingRunner())
	node := s.node("", nil)
	vc := stagedContext()
	volume := stackVolume("/staging", vc, mountCapability())
	build := func(shape stackShape) string {
		return strings.Join(planFor(node, connectionFromContext(vc), volume, vdoOptions(vc), shape).Names(), " → ")
	}
	got := build(shapeIndirectPlain)
	if got != "fabric → dmLinear → filesystem" {
		t.Errorf("indirect plain = %s", got)
	}
	got = build(shapeIndirectRawBlock)
	if got != "fabric → dmLinear" {
		t.Errorf("indirect raw block = %s", got)
	}
}

// U-02: both VDO rows are built with the indirection under the physical volume.
func TestPlanForBuildsTheIndirectLVMRows(t *testing.T) {
	s, _ := newTestStack(t, newRecordingRunner())
	node := s.node("", nil)
	vc := stagedContext()
	vc["client_compression"] = "true"
	volume := stackVolume("/staging", vc, mountCapability())
	build := func(shape stackShape) string {
		return strings.Join(planFor(node, connectionFromContext(vc), volume, vdoOptions(vc), shape).Names(), " → ")
	}
	if got := build(shapeIndirectLVM); got !=
		"fabric → dmLinear → lvmPhysicalVolume → lvmVolumeGroup → lvmLogicalVolume → filesystem" {
		t.Errorf("indirect LVM = %s", got)
	}
	if got := build(shapeIndirectLVMRawBlock); got !=
		"fabric → dmLinear → lvmPhysicalVolume → lvmVolumeGroup → lvmLogicalVolume" {
		t.Errorf("indirect LVM raw block = %s", got)
	}
}

// U-06: a fresh stage of every shape takes the indirection when the flag is
// unset, which is the default a live subsystem flip needs.
func TestAFreshStageOfEveryShapeIsIndirectByDefault(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	t.Setenv("SPDKCSI_DM_INDIRECTION", "")
	for shape, want := range map[stackShape]stackShape{
		shapeRawBlock:    shapeIndirectRawBlock,
		shapePlain:       shapeIndirectPlain,
		shapeLVM:         shapeIndirectLVM,
		shapeLVMRawBlock: shapeIndirectLVMRawBlock,
	} {
		if got := ns.attachShape(pvcTestHandle, shape); got != want {
			t.Errorf("shape %v: got %v, want %v", shape, got, want)
		}
	}
}

// U-07: the flag set off keeps every fresh stage direct.
func TestTurningTheIndirectionOffStagesEveryShapeDirect(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	for _, off := range []string{"false", "0", "no", "off"} {
		t.Setenv("SPDKCSI_DM_INDIRECTION", off)
		for _, shape := range []stackShape{shapeRawBlock, shapePlain, shapeLVM, shapeLVMRawBlock} {
			if got := ns.attachShape(pvcTestHandle, shape); got != shape {
				t.Errorf("%q, shape %v: got %v", off, shape, got)
			}
		}
	}
}

// A shape that is already indirect stays as it is.
func TestIndirectIsIdempotent(t *testing.T) {
	for _, shape := range []stackShape{
		shapeIndirectRawBlock, shapeIndirectPlain, shapeIndirectLVM, shapeIndirectLVMRawBlock,
	} {
		if got := indirect(shape); got != shape {
			t.Errorf("indirect(%v) = %v", shape, got)
		}
	}
}

// U-09: a VDO volume staged behind the indirection is torn down through it,
// the mapping released after the volume group above it.
func TestTeardownPlanOfAnIndirectVDOVolumeWalksTheMapping(t *testing.T) {
	for _, layers := range [][]string{
		{"fabric", "dmLinear", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume", "filesystem"},
		{"fabric", "dmLinear", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume"},
	} {
		ns, _ := newStackedServer(t, newRecordingRunner())
		writeRecord(t, ns.stack, pvcTestHandle, layers)
		plan, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", stagedContext())
		if err != nil {
			t.Fatalf("teardownPlan(%v): %v", layers, err)
		}
		if got, want := strings.Join(plan.Names(), " → "), strings.Join(layers, " → "); got != want {
			t.Errorf("the teardown walks %s, want %s", got, want)
		}
	}
}
