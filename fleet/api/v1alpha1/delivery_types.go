// DeliveryStatus: what the ManifestWork carrying a payload into a member
// reports, shared by every kind in this group that delivers one.
//
// It lives in its own file because it is the one status block three kinds hold
// in common, and because it is the only place a write the member refused can be
// seen from the hub. A refused create writes no object in the member, so there
// is no remote status to read back and nothing a ManagedClusterView could
// fetch, which leaves the ManifestWork's own conditions as the whole report.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeliveryStatus is what the ManifestWork carrying a payload reports, and the
// only place a refused write can be seen from the hub.
type DeliveryStatus struct {
	// ManifestWorkName is the object in the member's namespace on the hub.
	// +optional
	ManifestWorkName string `json:"manifestWorkName,omitempty"`

	// AppliedFingerprint is the fingerprint of what actually reached the member.
	// +optional
	AppliedFingerprint string `json:"appliedFingerprint,omitempty"`

	// Conditions mirror the ManifestWork's Applied, Available, and Degraded,
	// each written against this object's own generation, which the hub issued
	// and can therefore compare.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
