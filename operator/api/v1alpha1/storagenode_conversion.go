// Conversion of StorageNode between this version and the v1alpha2 hub.
//
// design-storagenode.md §15.1 is the delta, and four of its rows need more than
// an assignment.
//
//   - The parent. spec.storageNodeSetRef names a StorageNodeSet the redesign
//     retires; the hub names its StorageCluster and keeps the set's name as
//     spec.nodeSet, a label nothing is fetched by. The cluster is not on the
//     stored object, and a conversion has no client to look one up with, so it is
//     read from the controller owner reference. That is a pure function of the
//     subject, and the upgrade's reparent-storage-nodes step puts the reference
//     there before the storage version moves (design-api-upgrade.md §20), which
//     is the ordering that makes the read answer.
//   - The sizing. spec.config.sizing is required on the hub and has no v1alpha1
//     spelling at all, because the two values lived on the StorageCluster. A node
//     the hub never wrote converts up with none, and the upgrade's
//     stamp-storage-node-sizing step is what fills it from the cluster before
//     anything writes the object again.
//   - The failure domain. It was an index on both the spec and the status and is
//     a label on both here, so the digits are what a mechanical conversion can
//     carry and the meaning that lived outside the API is not. A label that is not
//     a number has nowhere to go on the way down and is stashed whole.
//   - The device summary. status.resources.devices was one string and is two
//     counts. The string is read as total/online rather than as the online/total
//     its own doc comment claimed, because both call sites passed the device
//     count before the online count: a node with three of four devices online
//     stored 4/3. The conversion reads what was written rather than what was
//     documented, since only one of those is in etcd.
//
// Four spec fields travel the other way. ubuntuHost, skipKubeletConfiguration,
// enableCpuTopology, and reservedSystemCPU are declared per node here and reach
// nothing, because their only consumers are environment variables in a DaemonSet
// pod template and a DaemonSet is one object for every node it schedules. They
// move to StorageCluster.spec.storageNodes (§5.1) and stash on the way up under
// storage.simplyblock.io/v1alpha1-<field>, so a node converted back down carries
// what it carried. status.postedAt stashes for the same reason: the persisted
// step replaced it (§3.3).

package v1alpha1

import (
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/atlas/statemachine"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// clusterKind is the kind an owner reference carries when it is the parent the
// hub names, and it is spelled here rather than derived so that the conversion
// package needs no scheme.
const clusterKind = "StorageCluster"

// The annotations holding the five fields this version has and the hub removed.
const (
	annoV1Alpha1NodeUbuntuHost   = "storage.simplyblock.io/v1alpha1-spec.overrides.ubuntuHost"
	annoV1Alpha1NodeSkipKubelet  = "storage.simplyblock.io/v1alpha1-spec.overrides.skipKubeletConfiguration"
	annoV1Alpha1NodeCPUTopology  = "storage.simplyblock.io/v1alpha1-spec.overrides.enableCpuTopology"
	annoV1Alpha1NodeReservedCPUs = "storage.simplyblock.io/v1alpha1-spec.overrides.reservedSystemCPU"
	annoV1Alpha1NodePostedAt     = "storage.simplyblock.io/v1alpha1-status.postedAt"
)

// The annotations holding the hub fields this version cannot express. The two
// SPDK pull policies are among them: this version names both images and never
// said when either is pulled.
const (
	annoNodeCluster    = "storage.simplyblock.io/conversion-spec.clusterRef"
	annoNodeSizing     = "storage.simplyblock.io/conversion-spec.config.sizing"
	annoNodeSpdkPull   = "storage.simplyblock.io/conversion-spec.config.spdkImagePullPolicy"
	annoNodeProxyPull  = "storage.simplyblock.io/conversion-spec.config.spdkProxyImagePullPolicy"
	annoNodeSpecDomain = "storage.simplyblock.io/conversion-spec.config.failureDomain"
	annoNodeStatDomain = "storage.simplyblock.io/conversion-status.failureDomain"
	annoNodeStep       = "storage.simplyblock.io/conversion-status.step"
	annoNodePhase      = "storage.simplyblock.io/conversion-status.phase"
	annoNodeMessage    = "storage.simplyblock.io/conversion-status.message"
	annoNodeObserved   = "storage.simplyblock.io/conversion-status.observedGeneration"
)

// ConvertTo converts this StorageNode to the v1alpha2 hub.
func (src *StorageNode) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageNode)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	overrides := src.Spec.Overrides
	if overrides == nil {
		overrides = &StorageNodeOverrides{}
	}
	stashRemovedBool(&dst.ObjectMeta, annoV1Alpha1NodeUbuntuHost, overrides.UbuntuHost)
	stashRemovedBool(&dst.ObjectMeta, annoV1Alpha1NodeSkipKubelet, overrides.SkipKubeletConfiguration)
	stashRemovedBool(&dst.ObjectMeta, annoV1Alpha1NodeCPUTopology, overrides.EnableCpuTopology)
	stashRemoved(&dst.ObjectMeta, annoV1Alpha1NodeReservedCPUs, overrides.ReservedSystemCPU)
	if err := stash(&dst.ObjectMeta, annoV1Alpha1NodePostedAt, src.Status.PostedAt); err != nil {
		return err
	}

	dst.Spec = v1alpha2.StorageNodeSpec{
		ClusterRef: controllingClusterName(&src.ObjectMeta),
		NodeSet:    src.Spec.StorageNodeSetRef,
		WorkerNode: src.Spec.WorkerNode,
		SocketID:   src.Spec.SocketID,
		NodeIndex:  src.Spec.NodeIndex,
		Slot:       src.Spec.SocketIndex,
		Config: v1alpha2.StorageNodeConfig{
			SpdkImage:        overrides.SpdkImage,
			SpdkProxyImage:   overrides.SpdkProxyImage,
			SpdkSystemMemory: overrides.SpdkSystemMemory,
			PcieAllowList:    overrides.PcieAllowList,
			PcieDenyList:     overrides.PcieDenyList,
			PcieModel:        overrides.PcieModel,
			DriveSizeRange:   overrides.DriveSizeRange,
			DeviceNames:      overrides.DeviceNames,
			FailureDomain:    formatInt32(overrides.FailureDomain),
			Expand:           overrides.Expand,
		},
	}
	if jm := overrides.JournalManagerSpec; jm != nil {
		dst.Spec.Config.JournalManager = &v1alpha2.JournalManagerSpec{
			Count:            jm.Count,
			PercentPerDevice: jm.PercentPerDevice,
		}
	}

	dst.Status = v1alpha2.StorageNodeStatus{
		UUID:          src.Status.UUID,
		Status:        src.Status.Status,
		Health:        src.Status.Health,
		Hostname:      src.Status.Hostname,
		Uptime:        src.Status.Uptime,
		FailureDomain: formatInt32(src.Status.FailureDomain),
		ActiveOpsRef:  src.Status.ActiveOpsRef,
	}
	if lm := src.Status.LatencyMetrics; lm != nil {
		dst.Status.LatencyMetrics = &v1alpha2.NodeLatencyMetrics{
			NodeUUID:           lm.NodeUUID,
			BaselineP50NS:      lm.BaselineP50NS,
			BaselineP99NS:      lm.BaselineP99NS,
			BaselineMeasuredAt: lm.BaselineMeasuredAt,
		}
	}
	if res := src.Status.Resources; res != nil {
		dst.Status.Resources = &v1alpha2.StorageNodeResources{
			CPU:     res.CPU,
			Memory:  res.Memory,
			Volumes: res.Volumes,
			Devices: parseDeviceSummary(res.Devices),
		}
		if cap := res.Capacity; cap != nil {
			dst.Status.Resources.Capacity = &v1alpha2.StorageNodeCapacity{
				TotalBytes: cap.TotalBytes,
				UsedBytes:  cap.UsedBytes,
				SampledAt:  cap.SampledAt,
			}
		}
	}
	if ports := src.Status.Ports; ports != nil {
		dst.Status.Ports = &v1alpha2.StorageNodePorts{
			Management: ports.Management,
			NvmeOf:     ports.NvmeOf,
			Lvol:       ports.Lvol,
			Rpc:        ports.Rpc,
		}
	}

	return restoreNodeHubOnly(&dst.ObjectMeta, dst)
}

// ConvertFrom converts the v1alpha2 hub into this StorageNode.
func (dst *StorageNode) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageNode)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	config := src.Spec.Config
	dst.Spec = StorageNodeSpec{
		StorageNodeSetRef: src.Spec.NodeSet,
		WorkerNode:        src.Spec.WorkerNode,
		SocketID:          src.Spec.SocketID,
		NodeIndex:         src.Spec.NodeIndex,
		SocketIndex:       src.Spec.Slot,
		Overrides: &StorageNodeOverrides{
			SpdkImage:                config.SpdkImage,
			SpdkProxyImage:           config.SpdkProxyImage,
			SpdkSystemMemory:         config.SpdkSystemMemory,
			PcieAllowList:            config.PcieAllowList,
			PcieDenyList:             config.PcieDenyList,
			PcieModel:                config.PcieModel,
			DriveSizeRange:           config.DriveSizeRange,
			DeviceNames:              config.DeviceNames,
			FailureDomain:            parseInt32(config.FailureDomain),
			Expand:                   config.Expand,
			UbuntuHost:               unstashRemovedBool(&dst.ObjectMeta, annoV1Alpha1NodeUbuntuHost),
			SkipKubeletConfiguration: unstashRemovedBool(&dst.ObjectMeta, annoV1Alpha1NodeSkipKubelet),
			EnableCpuTopology:        unstashRemovedBool(&dst.ObjectMeta, annoV1Alpha1NodeCPUTopology),
			ReservedSystemCPU:        unstashRemoved(&dst.ObjectMeta, annoV1Alpha1NodeReservedCPUs),
		},
	}
	if jm := config.JournalManager; jm != nil {
		dst.Spec.Overrides.JournalManagerSpec = &JournalManagerSpec{
			Count:            jm.Count,
			PercentPerDevice: jm.PercentPerDevice,
		}
	}

	dst.Status = StorageNodeStatus{
		UUID:          src.Status.UUID,
		Status:        src.Status.Status,
		Health:        src.Status.Health,
		Hostname:      src.Status.Hostname,
		Uptime:        src.Status.Uptime,
		FailureDomain: parseInt32(src.Status.FailureDomain),
		ActiveOpsRef:  src.Status.ActiveOpsRef,
	}
	if err := unstash(&dst.ObjectMeta, annoV1Alpha1NodePostedAt, &dst.Status.PostedAt); err != nil {
		return err
	}
	if lm := src.Status.LatencyMetrics; lm != nil {
		dst.Status.LatencyMetrics = &NodeLatencyMetrics{
			NodeUUID:           lm.NodeUUID,
			BaselineP50NS:      lm.BaselineP50NS,
			BaselineP99NS:      lm.BaselineP99NS,
			BaselineMeasuredAt: lm.BaselineMeasuredAt,
		}
	}
	if res := src.Status.Resources; res != nil {
		dst.Status.Resources = &StorageNodeResources{
			CPU:     res.CPU,
			Memory:  res.Memory,
			Volumes: res.Volumes,
			Devices: formatDeviceSummary(res.Devices),
		}
		if cap := res.Capacity; cap != nil {
			dst.Status.Resources.Capacity = &StorageNodeCapacity{
				TotalBytes: cap.TotalBytes,
				UsedBytes:  cap.UsedBytes,
				SampledAt:  cap.SampledAt,
			}
		}
	}
	if ports := src.Status.Ports; ports != nil {
		dst.Status.Ports = &StorageNodePorts{
			Management: ports.Management,
			NvmeOf:     ports.NvmeOf,
			Lvol:       ports.Lvol,
			Rpc:        ports.Rpc,
		}
	}

	return stashNodeHubOnly(&dst.ObjectMeta, src)
}

// controllingClusterName reads the parent the hub names off the controller owner
// reference, which is where the upgrade's ownership phase puts it.
//
// A node that has not been reparented yet resolves to the empty string rather
// than to an error. The object is still readable that way, which is what a read
// during an upgrade needs, and the field is Required so the next write of it is
// refused until the reparent has run — which is the ordering the upgrade already
// enforces and a louder failure than a node silently joining no cluster.
func controllingClusterName(meta *metav1.ObjectMeta) string {
	for _, ref := range meta.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && ref.Kind == clusterKind {
			return ref.Name
		}
	}
	return ""
}

// parseDeviceSummary reads this version's device string as the hub's two counts.
//
// The stored order is total/online. Both call sites formatted the device count
// before the online count, against a doc comment claiming online/total, so a node
// with three of four devices online stored 4/3 (§15.1). Reading it the documented
// way would report every degraded node as having more devices online than it has.
//
// A string that is not two numbers is an absent summary rather than zeroes. A
// node the control plane has never reported on and one that genuinely has no
// devices are different answers, and the absent parent is how the hub tells them
// apart.
func parseDeviceSummary(summary string) *v1alpha2.StorageNodeDevices {
	total, online, found := strings.Cut(summary, "/")
	if !found {
		return nil
	}
	totalN, err := strconv.ParseInt(strings.TrimSpace(total), 10, 32)
	if err != nil {
		return nil
	}
	onlineN, err := strconv.ParseInt(strings.TrimSpace(online), 10, 32)
	if err != nil {
		return nil
	}
	return &v1alpha2.StorageNodeDevices{Online: int32(onlineN), Total: int32(totalN)}
}

// formatDeviceSummary writes the two counts back in the order this version's
// readers expect, which is the order parseDeviceSummary reads.
func formatDeviceSummary(devices *v1alpha2.StorageNodeDevices) string {
	if devices == nil {
		return ""
	}
	return strconv.FormatInt(int64(devices.Total), 10) + "/" +
		strconv.FormatInt(int64(devices.Online), 10)
}

// stashNodeHubOnly writes every hub field this version has nowhere to put. A
// field at its zero value writes no annotation, so a node that stated none of
// them is not given metadata it never had.
func stashNodeHubOnly(meta *metav1.ObjectMeta, src *v1alpha2.StorageNode) error {
	for _, field := range []struct {
		key   string
		value any
	}{
		{annoNodeCluster, src.Spec.ClusterRef},
		{annoNodeSizing, src.Spec.Config.Sizing},
		{annoNodeSpdkPull, string(src.Spec.Config.SpdkImagePullPolicy)},
		{annoNodeProxyPull, string(src.Spec.Config.SpdkProxyImagePullPolicy)},
		{annoNodeStep, src.Status.Step},
		{annoNodePhase, string(src.Status.Phase)},
		{annoNodeMessage, src.Status.Message},
		{annoNodeObserved, src.Status.ObservedGeneration},
	} {
		if err := stash(meta, field.key, field.value); err != nil {
			return err
		}
	}

	// A failure domain that is a number survives the narrowing to an index and
	// needs no note. One that is a label — which is what every domain written
	// against the hub is — has nowhere to go, and only that case is recorded.
	stashNonNumericDomain(meta, annoNodeSpecDomain, src.Spec.Config.FailureDomain)
	stashNonNumericDomain(meta, annoNodeStatDomain, src.Status.FailureDomain)
	return nil
}

// stashNonNumericDomain records a failure-domain label this version's int32 field
// cannot hold.
func stashNonNumericDomain(meta *metav1.ObjectMeta, key, domain string) {
	if domain == "" || parseInt32(domain) != nil {
		clear(meta, key)
		return
	}
	stashRemoved(meta, key, domain)
}

// restoreNodeHubOnly reads them back and removes the annotations, so an object
// converted up carries the fields rather than both the fields and the notes about
// them.
func restoreNodeHubOnly(meta *metav1.ObjectMeta, dst *v1alpha2.StorageNode) error {
	// The stash wins over the owner reference, because a cluster the hub stated
	// is what the hub stated, and the reference is the derivation that answers
	// for a node no hub has ever written.
	var cluster string
	if err := unstash(meta, annoNodeCluster, &cluster); err != nil {
		return err
	}
	if cluster != "" {
		dst.Spec.ClusterRef = cluster
	}

	var step statemachine.KubeSnapshot
	if err := unstash(meta, annoNodeStep, &step); err != nil {
		return err
	}
	dst.Status.Step = step

	var phase string
	if err := unstash(meta, annoNodePhase, &phase); err != nil {
		return err
	}
	dst.Status.Phase = v1alpha2.StorageNodePhase(phase)

	for _, field := range []struct {
		key    string
		target any
	}{
		{annoNodeSizing, &dst.Spec.Config.Sizing},
		{annoNodeSpdkPull, &dst.Spec.Config.SpdkImagePullPolicy},
		{annoNodeProxyPull, &dst.Spec.Config.SpdkProxyImagePullPolicy},
		{annoNodeMessage, &dst.Status.Message},
		{annoNodeObserved, &dst.Status.ObservedGeneration},
	} {
		if err := unstash(meta, field.key, field.target); err != nil {
			return err
		}
	}

	if domain := unstashRemoved(meta, annoNodeSpecDomain); domain != "" {
		dst.Spec.Config.FailureDomain = domain
	}
	if domain := unstashRemoved(meta, annoNodeStatDomain); domain != "" {
		dst.Status.FailureDomain = domain
	}
	return nil
}
