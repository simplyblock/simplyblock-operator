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
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
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
	layerFabric     = "fabric"
	layerFilesystem = "filesystem"
)

// planFor is the shape a volume stages as: the fabric alone for raw block, and
// the fabric with a filesystem above it for everything else.
//
// Raw block mode is a shorter plan rather than a flag inside a stage function,
// which is what keeps a block volume from sharing a code path with a formatting
// one.
func planFor(
	node *plans.Node,
	connection lvol.Connection,
	volume plans.Volume,
	volCap *csi.VolumeCapability,
) volstack.Plan {
	if volCap.GetBlock() != nil {
		return node.RawBlock(connection)
	}
	return node.Plain(connection, volume)
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
		FormatOptions:         mount.FormatOptions(fsType, vc),
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

// shapeFromRecord is the plan shape a recorded layer list describes.
//
// A teardown is driven by what was built rather than by what a class says now,
// because a class can be edited or deleted after a volume is provisioned. A
// layer name this build does not know stops the teardown and says which one it
// was, rather than silently skipping an object nobody will release.
func shapeFromRecord(layers []string) (raw bool, err error) {
	switch {
	case len(layers) == 1 && layers[0] == layerFabric:
		return true, nil
	case len(layers) == 2 && layers[0] == layerFabric && layers[1] == layerFilesystem:
		return false, nil
	}
	for _, layer := range layers {
		if layer != layerFabric && layer != layerFilesystem {
			return false, fmt.Errorf(
				"the stack record names the layer %q, which this build does not know how to release; "+
					"releasing the rest would leave that layer's object behind with nothing to remove it",
				layer)
		}
	}
	return false, fmt.Errorf(
		"the stack record names the layers %v, which is not a shape this build stages",
		layers)
}
