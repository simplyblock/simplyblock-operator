// Striped volumes on the node: several namespaces, one per member, attached and
// striped into one block device by LVM (csi-driver/docs/striped-volumes.md).
//
// The shape is atlas-lib's Striped plan, the members composite under the LVM
// layers. What is here is where the members come from: the volume context lists
// them in stripe order, a stage asks the control plane where each is published,
// and a teardown takes them from the stack record, whose members entry holds
// them in the order they were brought up. The order is the stripe: the same
// members striped in another order are a different device, which is why a
// teardown never re-derives it from anything but the record.

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
	"github.com/simplyblock/atlas/volstack/plans"

	"github.com/simplyblock/csi-driver/internal/clusters"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

// watchedDevicePathsKey is where the stashed context keeps the namespace devices
// the path-loss watch was given, comma separated in member order, for the
// unstage that forgets them.
const watchedDevicePathsKey = "watchedDevicePaths"

// stripeDevicePathsKey is where an earlier version kept a striped volume's
// member devices. An unstage still reads it, for a volume staged before the
// upgrade.
const stripeDevicePathsKey = "stripeDevicePaths"

// watchDevice registers a namespace device with the path-loss watch, and
// forgetDevice drops one. Variables so tests can see what the watch is given.
var (
	watchDevice  = initiator.MarkDevicePresent
	forgetDevice = initiator.ForgetDevice
)

// stripeWidth is how many members the volume context says the volume is
// striped across, 1 for an ordinary volume.
func stripeWidth(vc map[string]string) int {
	width, err := strconv.Atoi(strings.TrimSpace(vc[csicommon.CtxStripeWidth]))
	if err != nil || width < 2 {
		return 1
	}
	return width
}

// stripeMembers is the members' logical-volume ids the volume context lists, in
// stripe order.
func stripeMembers(vc map[string]string) []string {
	raw := strings.TrimSpace(vc[csicommon.CtxStripeMembers])
	if raw == "" {
		return nil
	}
	members := strings.Split(raw, ",")
	for i := range members {
		members[i] = strings.TrimSpace(members[i])
	}
	return members
}

// stripeOptions is what the logical volume of a striped volume is: as many
// stripes as members, at the chunk the class chose.
func stripeOptions(vc map[string]string) plans.LogicalVolumeOptions {
	chunk, err := strconv.ParseInt(strings.TrimSpace(vc[csicommon.CtxStripeChunkBytes]), 10, 64)
	if err != nil || chunk < 0 {
		chunk = 0
	}
	return plans.LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{
			Stripes:          stripeWidth(vc),
			StripeChunkBytes: chunk,
		},
	}
}

// stripedPlanFor is planFor for the two striped shapes, which are built from a
// connection per member where every other shape has one.
func stripedPlanFor(
	node *plans.Node,
	connections []lvol.Connection,
	volume plans.Volume,
	options plans.LogicalVolumeOptions,
	shape stackShape,
) volstack.Plan {
	if shape == shapeStripedRawBlock {
		return node.StripedRawBlock(connections, volume, options)
	}
	return node.Striped(connections, volume, options)
}

// isStripedShape reports whether a shape is one of the striped rows.
func isStripedShape(shape stackShape) bool {
	return shape == shapeStriped || shape == shapeStripedRawBlock
}

// stripedAttachPlan is attachPlan for a striped volume.
func (ns *Server) stripedAttachPlan(
	ctx context.Context,
	volumeID, stagingTargetPath string,
	vc map[string]string,
	volCap *csi.VolumeCapability,
	shape stackShape,
) (volstack.Plan, error) {
	connections, hostNQN, err := ns.stripedConnections(ctx, volumeID, vc)
	if err != nil {
		return nil, err
	}
	node := ns.stack.node(hostNQN, ns.priorFormat(volumeID, vc))
	volume := stackVolume(stagingTargetPath, vc, volCap)
	return stripedPlanFor(node, connections, volume, stripeOptions(vc), shape), nil
}

// stripedConnections resolves where every member of a striped volume is
// published, in member order, and the host identity they are published to.
//
// The control plane is asked for each member, for the reason publishedConnection
// asks it for an ordinary volume: it is the only party that resolves a host's
// DHCHAP key material. When it does not answer, the members a stack record holds
// are enough to find namespaces that are already attached, which is what a heal
// or a publish of a staged volume needs.
func (ns *Server) stripedConnections(
	ctx context.Context, volumeID string, vc map[string]string,
) ([]lvol.Connection, string, error) {
	width := stripeWidth(vc)
	members := stripeMembers(vc)
	if len(members) != width {
		return nil, "", fmt.Errorf(
			"volume %s is striped across %d members and its context lists %d (%q)",
			volumeID, width, len(members), vc[csicommon.CtxStripeMembers])
	}

	connections, hostNQN, cpErr := memberConnectionsFromControlPlane(ctx, volumeID, vc, members)
	if cpErr == nil {
		return connections, hostNQN, nil
	}

	record, err := ns.stack.store.Load(volumeID)
	if err != nil {
		return nil, "", fmt.Errorf("volume %s: %w; and no stack record names its members: %v", volumeID, cpErr, err)
	}
	connections, err = recordedMemberConnections(record, members)
	if err != nil {
		return nil, "", fmt.Errorf("volume %s: %w; and its stack record cannot stand in: %v", volumeID, cpErr, err)
	}
	klog.Warningf(
		"volume %s: attaching its %d members from the stack record, because the control plane did not answer: %v",
		volumeID, width, cpErr)
	return connections, vc["hostNQN"], nil
}

// memberConnectionsFromControlPlane asks the control plane where each member is
// published for this host.
//
// The members are published to one host identity, the pool's, so a control
// plane that names a different host for two of them has answered something this
// node cannot attach under one identity, and that is refused.
func memberConnectionsFromControlPlane(
	ctx context.Context, volumeID string, vc map[string]string, members []string,
) ([]lvol.Connection, string, error) {
	handle, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		handle = nil
	}
	client, err := clusters.Client(ctx, clusterIDFor(vc, handle), poolIDFor(vc, handle))
	if err != nil {
		return nil, "", fmt.Errorf("no control-plane client for the volume's cluster: %w", err)
	}

	connections := make([]lvol.Connection, 0, len(members))
	hostNQN := ""
	for i, member := range members {
		responses, err := client.LvolConnections(ctx, member, vc["hostNQN"])
		if err != nil {
			return nil, "", fmt.Errorf("the control plane refused to publish member %d (%s): %w", i, member, err)
		}
		if len(responses) == 0 {
			return nil, "", fmt.Errorf("the control plane published no endpoint for member %d (%s)", i, member)
		}
		connection, memberHost := initiator.ConnectionFrom(responses, member)
		switch {
		case memberHost == "":
		case hostNQN == "":
			hostNQN = memberHost
		case hostNQN != memberHost:
			return nil, "", fmt.Errorf(
				"the control plane publishes member %d (%s) to host %s and an earlier member to %s",
				i, member, memberHost, hostNQN)
		}
		connections = append(connections, connection)
	}
	return connections, hostNQN, nil
}

// recordedMemberConnections rebuilds the members' connections from the stack
// record, in the order the record holds them, which is the order they were
// brought up in. members, the ids the volume context lists, are added to them,
// since a namespace is found by its id when its subsystem coordinates no longer
// find it.
//
// Entry i is paired with the context's member i, which is sound because the
// context lists the members in the order they were created and is never
// rewritten (csicommon.CtxStripeMembers), and the stage that wrote the record
// brought them up in that order. A context naming a different number of members
// than the record cannot be paired, so no id is added, and the record's
// subsystems alone find the namespaces.
func recordedMemberConnections(record volstack.Record, members []string) ([]lvol.Connection, error) {
	var entries []volstack.Entry
	for _, entry := range record.Plan {
		if entry.Layer == layerMembers {
			entries = entry.Members
			break
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("the stack record holds no members")
	}

	connections := make([]lvol.Connection, 0, len(entries))
	for i, entry := range entries {
		if entry.Layer != layerFabric {
			return nil, fmt.Errorf("member %d of the stack record is a %q layer, not a fabric", i, entry.Layer)
		}
		var params layers.FabricParams
		if len(entry.Params) > 0 {
			if err := json.Unmarshal(entry.Params, &params); err != nil {
				return nil, fmt.Errorf("member %d of the stack record cannot be read: %w", i, err)
			}
		}
		connection := lvol.Connection{NQN: params.NQN, NSID: params.NSID}
		if len(members) == len(entries) {
			connection.UUID = members[i]
		}
		if connection.NQN == "" && connection.UUID == "" {
			// A selector that constrains nothing matches every namespace on the
			// node, and releasing on one would detach another volume.
			return nil, fmt.Errorf("member %d of the stack record names no namespace", i)
		}
		connections = append(connections, connection)
	}
	return connections, nil
}

// stripedTeardownPlan is teardownPlan for a striped volume: the shape and the
// members the record holds, or, for a volume with no record, the members the
// context lists, which is a stage that never got as far as its first side
// effect and finds nothing to release.
func (ns *Server) stripedTeardownPlan(
	volumeID, stagingTargetPath string,
	vc map[string]string,
	record volstack.Record,
	shape stackShape,
	options plans.LogicalVolumeOptions,
) (volstack.Plan, error) {
	members := stripeMembers(vc)
	var connections []lvol.Connection
	if len(record.Plan) > 0 {
		var err error
		if connections, err = recordedMemberConnections(record, members); err != nil {
			return nil, fmt.Errorf("volume %s: %w", volumeID, err)
		}
	} else {
		for _, member := range members {
			connections = append(connections, lvol.Connection{UUID: member})
		}
		if len(connections) == 0 {
			return nil, fmt.Errorf("volume %s is striped and neither its context nor a stack record names its members",
				volumeID)
		}
	}
	node := ns.stack.node("", nil)
	return stripedPlanFor(node, connections, teardownVolume(volumeID, stagingTargetPath, vc), options, shape), nil
}

// teardownVolume is the volume a striped teardown releases. Its volume group
// is named after the first member, and a teardown without the stashed context
// still has the volume id, whose handle is the first member's: a group named
// after nothing would be released in its place.
func teardownVolume(volumeID, stagingTargetPath string, vc map[string]string) plans.Volume {
	volume := stackVolume(stagingTargetPath, vc, nil)
	if volume.UUID == "" {
		if handle, err := csicommon.ParseVolumeHandle(volumeID); err == nil {
			volume.UUID = handle.VolumeID
		}
	}
	return volume
}

// namespaceDevices is the namespace device under a plan, read from its bottom
// layer without converging it: every member of a striped plan in member order,
// or the one namespace of any other plan. nil when the layer cannot be read.
func namespaceDevices(ctx context.Context, plan volstack.Plan) []string {
	if len(plan) == 0 {
		return nil
	}
	_, own, err := plan[0].Observe(ctx, volstack.Artifact{})
	if err != nil {
		klog.Warningf("could not read the namespace devices under a stack: %v", err)
		return nil
	}
	paths := make([]string, 0, len(own.Devices))
	for _, d := range own.Devices {
		paths = append(paths, d.Path)
	}
	return paths
}

// watchNamespaces registers the namespace devices under a plan with the
// path-loss watch under the volume's identity, and records them in the context
// for the unstage that forgets them. It reports whether it found any; a caller
// that gets false has nothing better to register than the device on top.
//
// The watch compares what it holds with the namespace devices sysfs lists, so it
// is given the namespaces rather than the device on top. Behind the dmLinear
// indirection, the LVM layers, or a stripe, that is a device-mapper node it
// would report gone on its first look. Any one member of a stripe losing every
// path is the whole volume unable to serve, which is why each is registered
// under the volume's own id.
func watchNamespaces(ctx context.Context, plan volstack.Plan, vc map[string]string) bool {
	paths := namespaceDevices(ctx, plan)
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		watchDevice(path, deviceLvolID(vc))
	}
	vc[watchedDevicePathsKey] = strings.Join(paths, ",")
	return true
}

// forgetNamespaces drops the namespace devices an unstage tore down from the
// path-loss watch. It also reads the key a striped volume staged by an earlier
// version recorded its members under.
func forgetNamespaces(vc map[string]string) {
	for _, key := range []string{watchedDevicePathsKey, stripeDevicePathsKey} {
		for _, path := range strings.Split(vc[key], ",") {
			if path = strings.TrimSpace(path); path != "" {
				forgetDevice(path)
			}
		}
	}
}
