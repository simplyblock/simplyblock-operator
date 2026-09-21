// Which stack a volume is, and what it is built out of.
//
// Everything here is a pure function of what the RPC carries: the volume
// context, the volume capability, and what the control plane answered. Nothing
// reaches the host, which is what lets the shape a volume stages as be asserted
// in a unit test rather than on a cluster.
//
// The shapes themselves are atlas-lib's, in volstack/plans. What stays here is
// the selection, because which row a volume is gets read off Kubernetes types
// and the rows do not.

package node

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/plans"

	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/initiator"
	"github.com/simplyblock/csi-driver/internal/mount"
)

// The layer names this build knows. They are the values Layer.Name() returns
// and are stable across releases, because a teardown replays a record an
// earlier version wrote.
const (
	layerFabric            = "fabric"
	layerFilesystem        = "filesystem"
	layerLVMPhysicalVolume = "lvmPhysicalVolume"
	layerLVMVolumeGroup    = "lvmVolumeGroup"
	layerLVMLogicalVolume  = "lvmLogicalVolume"
)

// vdoPoolName is the pool `lvcreate --type vdo` creates alongside the logical
// volume, inside every volume's own group. It is not per volume: uniqueness
// comes from the group, of which there is one per volume. Being the same name
// in every group is what makes it structural, which is why the plan hands it to
// the physical-volume layer as a name to preserve through a clone resolution.
const vdoPoolName = "vdopool"

// stackShape is which row of the plan catalog a volume is. It is named rather
// than decided twice, because a stage reads it off the volume's parameters and
// a teardown reads it off the stack record, and the two have to arrive at the
// same list of layers or the teardown releases the wrong objects.
type stackShape int

const (
	// shapeRawBlock is the fabric alone: a volume the pod opens as a block
	// device, with nothing formatted on it.
	shapeRawBlock stackShape = iota

	// shapePlain is the fabric with a filesystem above it, which is what most
	// volumes are.
	shapePlain

	// shapeLVM is the fabric with the three LVM layers above it and a filesystem
	// on top: the shape a volume carrying client-side compression or
	// deduplication takes.
	shapeLVM

	// shapeLVMRawBlock is shapeLVM without the filesystem, for a volume that
	// asked for client-side compression or deduplication and is opened as a
	// block device.
	shapeLVMRawBlock
)

// planFor is the layer list one of the shapes means, built with the seams the
// node was resolved with.
//
// Raw block mode is a shorter plan rather than a flag inside a stage function,
// which is what keeps a block volume from sharing a code path with a formatting
// one.
func planFor(
	node *plans.Node,
	connection lvol.Connection,
	volume plans.Volume,
	options plans.LogicalVolumeOptions,
	shape stackShape,
) volstack.Plan {
	switch shape {
	case shapeRawBlock:
		return node.RawBlock(connection)
	case shapeLVM:
		return node.LVM(connection, volume, options)
	case shapeLVMRawBlock:
		return node.LVMRawBlock(connection, volume, options)
	case shapePlain:
	}
	return node.Plain(connection, volume)
}

// shapeFor is the row a volume stages as, read off what the RPC carries: the
// capability decides whether there is a filesystem, and the class parameters
// decide whether the LVM layers that provide client-side compression and
// deduplication sit between it and the fabric.
func shapeFor(vc map[string]string, volCap *csi.VolumeCapability) stackShape {
	block := volCap.GetBlock() != nil
	switch {
	case wantsVDO(vc) && block:
		return shapeLVMRawBlock
	case wantsVDO(vc):
		return shapeLVM
	case block:
		return shapeRawBlock
	}
	return shapePlain
}

// wantsVDO reports whether the volume's class asked for client-side compression
// or deduplication. Either one alone needs the whole mechanism, because VDO is
// what provides both.
func wantsVDO(vc map[string]string) bool {
	return boolFromContext(vc[kube.ParamClientCompression]) ||
		boolFromContext(vc[kube.ParamClientDeduplication])
}

// vdoOptions is what the LVM layers of such a volume are built with: a logical
// volume of the type that compresses, deduplicates, or both, inside the pool
// lvcreate creates for it, on a node that advertises the kernel module.
//
// The two parameters are independent, and a volume asking for one and not the
// other gets exactly that: the handler in atlas-lib/lvm passes both switches to
// lvcreate explicitly rather than letting an unset one fall to a default of its
// own.
func vdoOptions(vc map[string]string) plans.LogicalVolumeOptions {
	return plans.LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{
			Compression:   boolFromContext(vc[kube.ParamClientCompression]),
			Deduplication: boolFromContext(vc[kube.ParamClientDeduplication]),
		},
		PoolName:   vdoPoolName,
		Capability: volstack.Capability(kube.LabelVDOCapable),
	}
}

// stackVolume is the volume as the filesystem layer needs it described.
//
// The filesystem is the one the volume was staged with when that was recorded,
// and otherwise the one the capability asks for. Taking the recorded one is
// what lets a volume staged by an earlier, more permissive driver come back up:
// the layer refuses a device carrying a filesystem the plan did not name, and
// on such a volume the class is not what is down there.
func stackVolume(
	stagingPath string,
	vc map[string]string,
	volCap *csi.VolumeCapability,
) plans.Volume {
	fsType := stagedFsType(vc, volCap)
	return plans.Volume{
		UUID:                  deviceLvolID(vc),
		StagingPath:           stagingPath,
		FsType:                fsType,
		MountFlags:            volumeMountFlags(volCap),
		FormatOptions:         mount.FormatOptions(fsType, vc, wantsVDO(vc)),
		ReservedBlocksPercent: vc["tune2fs_reserved_blocks"],
		Encrypted:             boolFromContext(vc[csicommon.ParamEncryption]),
	}
}

// boolFromContext reads a flag the context carries as text, answering false for
// one it does not carry and for one that cannot be read.
//
// False is the safe answer for both: it is the volume whose content the host
// can read, and the guard that reading feeds is the strict one.
func boolFromContext(value string) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return parsed
}

// volumeMountFlags are the flags the volume itself asked for, plus the one the
// access mode implies. What a filesystem requires in order to mount at all is
// the layer's to add, so nothing of that is here.
func volumeMountFlags(volCap *csi.VolumeCapability) []string {
	flags := append([]string{}, volCap.GetMount().GetMountFlags()...)

	switch volCap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		flags = append(flags, "ro")
	case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_UNKNOWN:
	}
	return flags
}

// csiEndpoint is one reachable address as the volume context carries it. The
// per-path tunables are single values on the context, because one set is
// applied to every path.
type csiEndpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// connectionFromContext rebuilds where the volume is published from the context
// stashed beside its staging path.
//
// It is what the teardown path uses, and it deliberately asks the control plane
// nothing: an unstage has to work when the control plane does not, and a
// release needs no credentials because it attaches nothing. What it cannot
// carry is the DHCHAP key material, which is why the attaching paths resolve
// their connection from the control plane instead.
func connectionFromContext(vc map[string]string) lvol.Connection {
	connection := lvol.Connection{
		NQN:  vc["nqn"],
		NSID: uint32(namespaceID(vc)),
		UUID: deviceLvolID(vc),
	}

	var endpoints []csiEndpoint
	if raw := vc["connections"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
			// A context whose endpoints cannot be read still identifies the
			// namespace, and that is all a release needs: the device is found by
			// its identity and detached, never by an address.
			endpoints = nil
		}
	}

	transport := strings.ToLower(vc["targetType"])
	for _, endpoint := range endpoints {
		connection.Endpoints = append(connection.Endpoints, lvol.Endpoint{
			Transport:         transport,
			Address:           endpoint.IP,
			Port:              endpoint.Port,
			NrIOQueues:        intFromContext(vc["nrIoQueues"]),
			ReconnectDelaySec: intFromContext(vc["reconnectDelay"]),
			CtrlLossTMOSec:    ptr.To(initiator.DefaultCtrlLossTmo),
			HostIface:         vc["hostIface"],
		})
	}
	return connection
}

// connectionFromResponses is the control plane's own answer in the shape a plan
// takes it, together with the host identity the connect has to present.
//
// The endpoints keep the order they were returned in, which is the control
// plane's priority order, so the primary path is attached first.
//
// The controller-loss timeout is this node's rather than the control plane's.
// It is a local policy about how long the kernel keeps retrying a path before
// failing I/O on it, and the driver has applied its own since before the stack
// existed.
func connectionFromResponses(
	responses []*controlplane.LvolConnectResp,
	namespaceUUID string,
) (connection lvol.Connection, hostNQN string) {
	if len(responses) == 0 {
		return lvol.Connection{}, ""
	}

	connection = lvol.Connection{
		NQN:  responses[0].Nqn,
		NSID: uint32(responses[0].NSID), //nolint:gosec // a namespace id the control plane reports
		UUID: namespaceUUID,
	}
	for _, response := range responses {
		secret, ctrlSecret, tls, host := connectAuth(response.Connect)
		if host != "" {
			hostNQN = host
		}
		connection.Endpoints = append(connection.Endpoints, lvol.Endpoint{
			Transport:         strings.ToLower(response.TargetType),
			Address:           response.IP,
			Port:              response.Port,
			NrIOQueues:        response.NrIoQueues,
			ReconnectDelaySec: response.ReconnectDelay,
			CtrlLossTMOSec:    ptr.To(initiator.DefaultCtrlLossTmo),
			HostIface:         response.HostIface,
			TLS:               tls,
			DHCHAPSecret:      secret,
			DHCHAPCtrlSecret:  ctrlSecret,
		})
	}
	return connection, hostNQN
}

// connectAuth reads the host identity and the DHCHAP key material out of the
// connect command line the control plane built for this host.
//
// That line is the only channel the driver has for them: the control plane is
// the only party that resolves a host's secret, whether pool-shared or
// per-host, and it bakes the flags into the connect string rather than exposing
// them as fields of their own.
func connectAuth(connect string) (secret, ctrlSecret string, tls bool, hostNQN string) {
	for _, field := range strings.Fields(connect) {
		switch {
		case strings.HasPrefix(field, "--hostnqn="):
			hostNQN = strings.TrimPrefix(field, "--hostnqn=")
		case strings.HasPrefix(field, "--dhchap-secret="):
			secret = strings.TrimPrefix(field, "--dhchap-secret=")
		case strings.HasPrefix(field, "--dhchap-ctrl-secret="):
			ctrlSecret = strings.TrimPrefix(field, "--dhchap-ctrl-secret=")
		case field == "--tls":
			tls = true
		}
	}
	return secret, ctrlSecret, tls, hostNQN
}

// deviceLvolID is the identity the volume's namespace carries on this host,
// which after a failover is the clone's rather than the volume's. It is what a
// device lookup falls back to when the subsystem coordinates no longer find it.
func deviceLvolID(vc map[string]string) string {
	if target := strings.TrimSpace(vc["targetLvolID"]); target != "" {
		return target
	}
	return strings.TrimSpace(vc["uuid"])
}

// namespaceID is the namespace the volume occupies within its subsystem. Zero
// means the context names none, which the fabric layer reads as the
// subsystem's only namespace.
//
// It answers in the type a namespace id is rather than in a machine-width int
// that a caller would have to convert back. An NSID is 32 bits wide by the NVMe
// specification, and on a 32-bit host the round trip through int turns the top
// half of that range negative, which would point a repair at a namespace the
// volume does not own.
func namespaceID(vc map[string]string) nvme.NamespaceID {
	nsID, err := strconv.ParseUint(strings.TrimSpace(vc["nsId"]), 10, 32)
	if err != nil {
		return 0
	}
	return nvme.NamespaceID(nsID)
}

// intFromContext reads a tunable the context carries as text, answering zero
// for one it does not carry, which is what leaves the kernel at its default.
func intFromContext(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return parsed
}

// clusterIDFor is the cluster the volume's control plane calls go to: the one
// the context names when a failover redirected the volume to another, and
// otherwise the one encoded in the subsystem NQN.
func clusterIDFor(vc map[string]string, handle *lvol.Handle) string {
	if clusterID := strings.TrimSpace(vc[csicommon.ParamClusterID]); clusterID != "" {
		return clusterID
	}
	if subsystem, ok := nqn.Parse(vc["nqn"]); ok && subsystem.ClusterID != "" {
		return subsystem.ClusterID
	}
	if handle != nil {
		return handle.ClusterID
	}
	return ""
}

// poolIDFor is the pool the volume's control plane calls go to, which a
// redirected volume carries on its context and every other one takes from its
// handle.
func poolIDFor(vc map[string]string, handle *lvol.Handle) string {
	if poolID := strings.TrimSpace(vc["poolID"]); poolID != "" {
		return poolID
	}
	if handle != nil {
		return handle.PoolRef
	}
	return ""
}

// recordedShapes is every layer list this build stages, against the shape it is.
// A teardown matches a record against it rather than against a condition per
// shape, so that adding a row to the catalog is one entry here and nothing else.
var recordedShapes = []struct {
	layers []string
	shape  stackShape
}{
	{[]string{layerFabric}, shapeRawBlock},
	{[]string{layerFabric, layerFilesystem}, shapePlain},
	{
		[]string{layerFabric, layerLVMPhysicalVolume, layerLVMVolumeGroup, layerLVMLogicalVolume},
		shapeLVMRawBlock,
	},
	{
		[]string{layerFabric, layerLVMPhysicalVolume, layerLVMVolumeGroup, layerLVMLogicalVolume, layerFilesystem},
		shapeLVM,
	},
}

// knownLayers are the layer names the shapes above are made of, which is what
// separates a record this build cannot release from one naming a layer it has
// never heard of.
var knownLayers = map[string]bool{
	layerFabric:            true,
	layerFilesystem:        true,
	layerLVMPhysicalVolume: true,
	layerLVMVolumeGroup:    true,
	layerLVMLogicalVolume:  true,
}

// shapeFromRecord is the plan shape a recorded layer list describes.
//
// A teardown is driven by what was built rather than by what a class says now,
// because a class can be edited or deleted after a volume is provisioned. A
// layer name this build does not know stops the teardown and says which one it
// was, rather than silently skipping an object nobody will release.
func shapeFromRecord(recorded []string) (stackShape, error) {
	for _, candidate := range recordedShapes {
		if slices.Equal(recorded, candidate.layers) {
			return candidate.shape, nil
		}
	}
	for _, layer := range recorded {
		if !knownLayers[layer] {
			return shapePlain, fmt.Errorf(
				"the stack record names the layer %q, which this build does not know how to release; "+
					"releasing the rest would leave that layer's object behind with nothing to remove it",
				layer)
		}
	}
	return shapePlain, fmt.Errorf(
		"the stack record names the layers %v, which is not a shape this build stages",
		recorded)
}
