// Conversion of ControlPlane between this version and the v1alpha2 hub.
//
// Two properties move (design-property-renames.md §2.4 and §2.5): the top-level
// image regroups under spec.source.managed, and the readiness phase Ready becomes
// Available. Everything else this version declares is carried across unchanged,
// and everything the hub adds — the step, the endpoint, the version, the
// components, the lock, and the observed generation — has no v1alpha1 spelling
// and is dropped on the way down, which is what a spoke that predates a field
// does with it.
//
// The conversion never fails. A phase value in neither table is passed through as
// written, because a conversion webhook is the wrong place to reject an object:
// each version's Enum marker already refuses what that version does not accept,
// and a conversion that errors makes the object unreadable rather than invalid.

package v1alpha1

import (
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// controlPlanePhaseToHub maps this version's readiness phases onto the hub's.
// Only the renamed value appears; anything absent is passed through.
var controlPlanePhaseToHub = map[string]string{
	"Ready": "Available",
}

// controlPlanePhaseFromHub is the inverse of controlPlanePhaseToHub, built from
// it so the two cannot drift into disagreeing about a value.
var controlPlanePhaseFromHub = invertStringMap(controlPlanePhaseToHub)

// invertStringMap returns m with its keys and values exchanged. It is used to
// derive a conversion's downward value table from its upward one, so that adding
// a renamed value means editing one map rather than remembering to edit two.
func invertStringMap(m map[string]string) map[string]string {
	inverted := make(map[string]string, len(m))
	for from, to := range m {
		inverted[to] = from
	}
	return inverted
}

// mapOrPassThrough returns the value table's entry for v, or v itself when the
// table has none. See this file's opening comment for why an unmapped value is
// not an error.
func mapOrPassThrough(table map[string]string, v string) string {
	if mapped, ok := table[v]; ok {
		return mapped
	}
	return v
}

// ConvertTo converts this ControlPlane to the v1alpha2 hub.
func (src *ControlPlane) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.ControlPlane)

	dst.ObjectMeta = src.ObjectMeta

	// Every v1alpha1 ControlPlane is one the chart installed, so the managed
	// member is the one it converts into, and it is set even when the image is
	// empty. The hub requires exactly one member (design-controlplane.md §3.2),
	// and an object arriving upward with neither is one nothing downstream can
	// classify: the reconciler would read it as neither managed nor external and
	// refuse to act on a control plane that is plainly running.
	dst.Spec.Source = v1alpha2.ControlPlaneSource{
		Managed: &v1alpha2.ManagedControlPlane{Image: src.Spec.Image},
	}

	dst.Status.Phase = v1alpha2.ControlPlanePhase(
		mapOrPassThrough(controlPlanePhaseToHub, src.Status.Phase))
	dst.Status.Message = src.Status.Message
	dst.Status.LastChecked = src.Status.LastChecked

	return nil
}

// ConvertFrom converts the v1alpha2 hub into this ControlPlane.
func (dst *ControlPlane) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.ControlPlane)

	dst.ObjectMeta = src.ObjectMeta

	// An external control plane has no image, which is what this version's only
	// spec field holds. It converts down to an empty one rather than to an
	// error: the object still has to be readable at v1alpha1, and what a reader
	// there loses is a field that never applied to it.
	dst.Spec.Image = ""
	if managed := src.Spec.Source.Managed; managed != nil {
		dst.Spec.Image = managed.Image
	}

	dst.Status.Phase = mapOrPassThrough(controlPlanePhaseFromHub, string(src.Status.Phase))
	dst.Status.Message = src.Status.Message
	dst.Status.LastChecked = src.Status.LastChecked

	return nil
}
