// The reads of ControlPlane.spec that more than one file needs.
//
// They exist because spec.source is two optional members of which exactly one is
// set, so every read of anything under it is a nil check the API server's CEL
// rule has already made. Doing it once here means a builder reads the image
// rather than the source, and a controller asks whether the control plane is
// managed rather than unpacking a pointer.

package controlplane

import (
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// localImage is the control plane's own image, empty when the object names an
// remote control plane. The workload builders call it rather than reaching
// through the source themselves.
func localImage(cp *simplyblockv1alpha2.ControlPlane) string {
	if local := cp.Spec.Source.Local; local != nil {
		return local.Image
	}
	return ""
}

// foundationDBSpecOf is the sizing block, which is optional inside an optional
// member. A nil return is the API's defaults rather than an error.
func foundationDBSpecOf(cp *simplyblockv1alpha2.ControlPlane) *simplyblockv1alpha2.FoundationDBSpec {
	if local := cp.Spec.Source.Local; local != nil {
		return local.FoundationDB
	}
	return nil
}

// apiReplicas is how many management API instances to run. Two is the default
// and the number the phases assume: a single instance makes Degraded unreachable
// for this component and turns every restart into an outage.
func apiReplicas(local *simplyblockv1alpha2.LocalControlPlane) int32 {
	if local == nil || local.Replicas == nil {
		return 2
	}
	return *local.Replicas
}

// isLocal reports whether the operator installs this control plane. It is the
// branch every path in the reconciler takes first, and the one the operations
// are refused on.
func isLocal(cp *simplyblockv1alpha2.ControlPlane) bool {
	return cp.Spec.Source.Local != nil
}

// isManaged reports whether the control plane already exists somewhere the
// operator does not own. A source with neither member set is neither, which is
// what the reconciler reports rather than guessing at.
func isManaged(cp *simplyblockv1alpha2.ControlPlane) bool {
	return cp.Spec.Source.Managed != nil
}
