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
func TestAttachShapeFollowsTheFlagOnlyForAFreshStage(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())

	t.Setenv("SPDKCSI_DM_INDIRECTION", "")
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
	if got := ns.attachShape(pvcTestHandle, shapeLVM); got != shapeLVM {
		t.Errorf("an LVM stack has no indirect variant, got %v", got)
	}

	writeRecord(t, ns.stack, pvcTestHandle, []string{"fabric", "filesystem"})
	if got := ns.attachShape(pvcTestHandle, shapePlain); got != shapePlain {
		t.Errorf("flag on, staged without the layer: shape %v, want plain", got)
	}

	t.Setenv("SPDKCSI_DM_INDIRECTION", "")
	writeRecord(t, ns.stack, pvcTestHandle, []string{"fabric", "dmLinear", "filesystem"})
	if got := ns.attachShape(pvcTestHandle, shapePlain); got != shapeIndirectPlain {
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
