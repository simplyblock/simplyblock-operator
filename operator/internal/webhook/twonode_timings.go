// Admission warnings for the two-node timings the control plane does not take.
//
// spec.arbitration.holdMs and leaseTtlMs are kept in the schema so that a
// manifest setting them still applies, but the control plane runs every
// two-node cluster at fixed timings and has no field to receive them. Both the
// StorageCluster and the ClusterDeploymentConfig that creates one carry the
// block, so both validators warn from here.

package webhook

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The control plane's fixed timings (sbcli constants.TWO_NODE_HOLD_MS and
// TWO_NODE_LEASE_TTL_MS), named in the warning so the reader learns what the
// cluster actually runs at.
const (
	controlPlaneHoldMs     = 2500
	controlPlaneLeaseTTLMs = 1500
)

// twoNodeTimingWarnings returns one warning for each timing the block sets.
// path is where the block sits in the object, as the reader wrote it.
func twoNodeTimingWarnings(path string, arbitration *simplyblockv1alpha2.ArbitrationSpec) admission.Warnings {
	if arbitration == nil {
		return nil
	}
	var warnings admission.Warnings
	// Reading the deprecated fields is the point: the warning is that they are set.
	if arbitration.HoldMs != nil { //nolint:staticcheck // SA1019: warning that the deprecated field is set
		warnings = append(warnings, fmt.Sprintf(
			"%s.holdMs has no effect and is deprecated: the control plane holds writes for a fixed "+
				"%d ms after a node loses its peer, and the operator does not send the field. Remove it",
			path, controlPlaneHoldMs))
	}
	if arbitration.LeaseTtlMs != nil { //nolint:staticcheck // SA1019: warning that the deprecated field is set
		warnings = append(warnings, fmt.Sprintf(
			"%s.leaseTtlMs has no effect and is deprecated: the control plane grants arbiter leases "+
				"of a fixed %d ms, and the operator does not send the field. Remove it",
			path, controlPlaneLeaseTTLMs))
	}
	return warnings
}
