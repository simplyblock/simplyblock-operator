// Whether this operator can act on a HugePagesMode value at all, independent
// of any one cluster's state. See design-preprovisioned-hugepages.md S:6.2.

package platform

import simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"

// HugePagesModeSupported reports whether this operator can act on mode.
// Preprovisioned needs nothing beyond the precheck in internal/hugepages and
// is always available. Exclusive and Shared are not implemented on any
// platform yet, so this returns false for both unconditionally: it does not
// consult ClusterDeploymentConfigSpec.Environment,
// StorageCluster.spec.storageNodes.talos, or .openshift, even though all
// three already exist and are exactly what a future per-platform decision
// would read. Threading an unused platform argument through now would not
// change today's answer for any of them.
func HugePagesModeSupported(mode simplyblockv1alpha2.HugePagesMode) bool {
	return mode == "" || mode == simplyblockv1alpha2.HugePagesModePreprovisioned
}
