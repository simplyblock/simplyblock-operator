// A cluster's workload names as the operator reads and writes them: out of
// status.workloadNames, into the control plane's create body, and back out of
// its answer. The names themselves are derived in atlas-lib/kube, and this file
// only moves them between the three shapes they travel in.

package utils

import (
	atlascp "github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// WorkloadNamesOf is the cluster's recorded workload names, and false when it
// has none recorded yet. A cluster without a record is reached by the constant
// names, which is what the first return carries then.
func WorkloadNamesOf(cluster *simplyblockv1alpha2.StorageCluster) (atlaskube.WorkloadNames, bool) {
	recorded := cluster.Status.WorkloadNames
	if recorded == nil {
		return atlaskube.ConstantWorkloadNames(), false
	}
	return atlaskube.WorkloadNames{
		NodeAPIService:     recorded.NodeAPIService,
		NodeAPITLSSecret:   recorded.NodeAPITLSSecret,
		SpdkProxyService:   recorded.SpdkProxyService,
		SpdkProxyTLSSecret: recorded.SpdkProxyTLSSecret,
		NodeLabelSuffix:    recorded.NodeLabelSuffix,
	}, true
}

// WorkloadNamesStatus is the status record of a set of names.
func WorkloadNamesStatus(names atlaskube.WorkloadNames) *simplyblockv1alpha2.WorkloadNames {
	return &simplyblockv1alpha2.WorkloadNames{
		NodeAPIService:     names.NodeAPIService,
		NodeAPITLSSecret:   names.NodeAPITLSSecret,
		SpdkProxyService:   names.SpdkProxyService,
		SpdkProxyTLSSecret: names.SpdkProxyTLSSecret,
		NodeLabelSuffix:    names.NodeLabelSuffix,
	}
}

// BackendWorkloadNames is the part of the names the control plane records. It
// reaches the storage-node API's Secret never, because the node agent mounts
// that one itself.
func BackendWorkloadNames(names atlaskube.WorkloadNames) atlascp.ClusterWorkloadNames {
	return atlascp.ClusterWorkloadNames{
		NodeAPIService:     names.NodeAPIService,
		SpdkProxyService:   names.SpdkProxyService,
		SpdkProxyTLSSecret: names.SpdkProxyTLSSecret,
		NodeLabelSuffix:    names.NodeLabelSuffix,
	}
}

// BackendHolds reports whether the control plane's record of a cluster carries
// these names. A record without them is read as the constant names, which is
// how the control plane itself reads it, so a control plane that predates the
// record holds the constant names and no others.
func BackendHolds(backend atlascp.ClusterWorkloadNames, names atlaskube.WorkloadNames) bool {
	constant := BackendWorkloadNames(atlaskube.ConstantWorkloadNames())
	if backend.NodeAPIService == "" {
		backend.NodeAPIService = constant.NodeAPIService
	}
	if backend.SpdkProxyService == "" {
		backend.SpdkProxyService = constant.SpdkProxyService
	}
	if backend.SpdkProxyTLSSecret == "" {
		backend.SpdkProxyTLSSecret = constant.SpdkProxyTLSSecret
	}
	return backend == BackendWorkloadNames(names)
}
