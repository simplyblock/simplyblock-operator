// Conversion of ControlPlane between this version and the v1alpha2 hub.
//
// Two properties move (design-property-renames.md §2.4 and §2.5): the top-level
// image regroups under spec.source.local, and the readiness phase Ready becomes
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
// Every value either Enum declares is in a table, so the pass-through covers only
// a hand-edited object or one written by a version that has not shipped.

package v1alpha1

import (
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// controlPlanePhaseToHub maps this version's two readiness phases onto the hub's.
// Both are mapped: neither v1alpha1 spelling is a value v1alpha2's Enum admits,
// so passing one through unchanged produces an object the API server rejects on
// write and the new controller cannot interpret.
var controlPlanePhaseToHub = map[string]string{
	"Initializing": "Installing",
	"Ready":        "Available",
}

// controlPlanePhaseFromHub maps the hub's four phases onto this version's two.
//
// It is written out rather than derived by inverting the table above, because
// the hub says more than this version can hold and the mapping is therefore not
// one to one. Degraded and Unavailable have no v1alpha1 spelling, and each lands
// on the value that tells a v1alpha1 reader the same thing: a Degraded control
// plane answers requests, so it reads as Ready, and an Unavailable one does not,
// so it reads as Initializing.
//
// That makes hub to spoke to hub lossy for those two, which is the direction
// this version cannot help. Spoke to hub to spoke is lossless, and that is the
// trip the API server performs on every read of a stored v1alpha1 object.
var controlPlanePhaseFromHub = map[string]string{
	"Installing":  "Initializing",
	"Available":   "Ready",
	"Degraded":    "Ready",
	"Unavailable": "Initializing",
}

// invertStringMap returns m with its keys and values exchanged. It derives a
// conversion's downward value table from its upward one, so that a renamed value
// means editing one map rather than remembering to edit two. It suits a rename
// and not this file's phases, where the hub holds more values than the spoke and
// the two directions are therefore written out separately.
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
		Local: &v1alpha2.LocalControlPlane{Image: src.Spec.Image},
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

	// A remote control plane has no image, which is what this version's only
	// spec field holds. It converts down to an empty one rather than to an
	// error: the object still has to be readable at v1alpha1, and what a reader
	// there loses is a field that never applied to it.
	dst.Spec.Image = ""
	if managed := src.Spec.Source.Local; managed != nil {
		dst.Spec.Image = managed.Image
	}

	dst.Status.Phase = mapOrPassThrough(controlPlanePhaseFromHub, string(src.Status.Phase))
	dst.Status.Message = src.Status.Message
	dst.Status.LastChecked = src.Status.LastChecked

	return nil
}
