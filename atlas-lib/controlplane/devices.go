// One physical device of one storage node, and the operations the v2 API offers
// against it.
//
// The v2 API offers five verbs on a device (restart, remove, fail, reset, and
// resize) where design-storagedevice.md §7 asks for seven. What is here is what
// exists: the reads the operations wait on, the restart one action is built
// around, the removal and the failure the next is, and the growth of an lblk
// device the third is. Self-test, detach, and the adopt call
// are the three that are missing, and each is recorded as a TODO beside the
// action it blocks, in the operator's storagedeviceops_types.go, rather than as
// a client method here that would return a 404.
//
// Reset is served and has no caller: it is one of the two exits from a failed
// device, and no action in §6 asks for one.

package controlplane

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// Device is one physical device of a storage node.
//
// It is the identity and the state, not the occupancy: what a device holds
// changes continuously and the DTO's capacity block is a snapshot the stream
// never refreshes, so the current figures are the exporter's gauges and are read
// through atlas-lib/prometheus instead (§7).
type Device struct {
	ID        string
	ClusterID string
	NodeID    string

	// Status is the control plane's own spelling, carried rather than mapped.
	// An operation waits for it to become what the operation asked for, and a
	// vocabulary translated here would be one the wait could not express.
	Status string

	// The hardware identity, which is what a person matches against a slot.
	Model          string
	SerialNumber   string
	PCIeAddress    string
	NVMeController string

	// SizeBytes is what the device is, as the control plane measured it.
	SizeBytes uint64

	// Health is what the device reports about itself. IOError and
	// RetriesExhausted are the two the control plane acts on; HealthCheck is
	// absent where the device does not report one.
	HealthCheck      *bool
	IOError          bool
	RetriesExhausted bool
}

func deviceFromDTO(d cpapi.DeviceDTO) Device {
	return Device{
		ID:               d.Id.String(),
		ClusterID:        d.ClusterId.String(),
		NodeID:           d.StorageNodeId.String(),
		Status:           d.Status,
		Model:            d.Model,
		SerialNumber:     d.SerialNumber,
		PCIeAddress:      d.PcieAddress,
		NVMeController:   d.NvmeController,
		SizeBytes:        uint64(d.Size),
		HealthCheck:      d.HealthCheck,
		IOError:          d.IoError,
		RetriesExhausted: d.RetriesExhausted,
	}
}

// ListDevices returns every device one storage node holds.
func (c *Client) ListDevices(ctx context.Context, clusterID, nodeID string) ([]Device, error) {
	cluster, node, err := parseNodeIDs(clusterID, nodeID)
	if err != nil {
		return nil, err
	}

	resp, err := c.api.ClustersStorageNodesDevicesListApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesGetWithResponse(
		ctx, cluster, node, nil)
	if err != nil {
		return nil, fmt.Errorf("list the devices of storage node %s: %w", nodeID, err)
	}
	ds, err := payload("devices of storage node "+nodeID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}

	out := make([]Device, 0, len(*ds))
	for _, d := range *ds {
		out = append(out, deviceFromDTO(d))
	}
	return out, nil
}

// Device returns one device. It wraps errs.ErrNotFound for a device the control
// plane does not hold, which is how a caller tells a device that is gone from a
// control plane it could not reach.
func (c *Client) Device(ctx context.Context, clusterID, nodeID, deviceID string) (Device, error) {
	cluster, node, device, err := parseDeviceIDs(clusterID, nodeID, deviceID)
	if err != nil {
		return Device{}, err
	}

	resp, err := c.api.ClustersStorageNodesDevicesDetailApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesDeviceIdGetWithResponse(
		ctx, cluster, node, device, nil)
	if err != nil {
		return Device{}, fmt.Errorf("read device %s: %w", deviceID, err)
	}
	d, err := payload("device "+deviceID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return Device{}, err
	}
	return deviceFromDTO(*d), nil
}

// RestartDevice recycles one device in place.
//
// It is the narrowest recycling the control plane offers: the alternative is
// restarting the device's storage node, which takes every other device on it
// along and costs the cluster a node's worth of redundancy for the duration.
//
// The call returns when the control plane has accepted the request rather than
// when the device is back, so the caller waits on the device's own status.
func (c *Client) RestartDevice(ctx context.Context, clusterID, nodeID, deviceID string) error {
	cluster, node, device, err := parseDeviceIDs(clusterID, nodeID, deviceID)
	if err != nil {
		return err
	}

	resp, err := c.api.ClustersStorageNodesDevicesRestartApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesDeviceIdRestartPostWithResponse(
		ctx, cluster, node, device, nil)
	if err != nil {
		return fmt.Errorf("restart device %s: %w", deviceID, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return respError("restart device "+deviceID, code, resp.Body)
	}
	return nil
}

// RemoveDevice takes one device out of the data path, leaving it in its slot.
//
// It is the first of the two calls a deliberate failure is made of: the control
// plane refuses to fail a device that is still serving, so a device is removed
// and then failed. On its own the removal is reversible, because a removed
// device can be restarted back into service.
//
// The removal is never forced. The control plane's own force flag skips the
// refusals it raises for a device in a state it cannot be removed from and for
// a restart already running against it, and each of those is a fact the caller
// needs to see rather than override.
func (c *Client) RemoveDevice(ctx context.Context, clusterID, nodeID, deviceID string) error {
	cluster, node, device, err := parseDeviceIDs(clusterID, nodeID, deviceID)
	if err != nil {
		return err
	}

	resp, err := c.api.ClustersStorageNodesDevicesRemoveApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesDeviceIdRemovePostWithResponse(
		ctx, cluster, node, device, nil)
	if err != nil {
		return fmt.Errorf("remove device %s: %w", deviceID, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return respError("remove device "+deviceID, code, resp.Body)
	}
	return nil
}

// FailDevice declares one removed device untrustworthy, so the cluster rebuilds
// the redundancy it held elsewhere and stops reading from it.
//
// It is the second of the two calls, and the irreversible one: a failed device
// does not return to service, and the two ways out of the state are both
// physical. The control plane refuses the call for a device it does not already
// hold as removed, and for a cluster with a volume migration running on any of
// its nodes.
//
// The call returns when the control plane has recorded the failure rather than
// when the rebuild it starts has finished, so the caller waits on the device's
// own status.
func (c *Client) FailDevice(ctx context.Context, clusterID, nodeID, deviceID string) error {
	cluster, node, device, err := parseDeviceIDs(clusterID, nodeID, deviceID)
	if err != nil {
		return err
	}

	resp, err := c.api.ClustersStorageNodesDevicesFailApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesDeviceIdFailPostWithResponse(
		ctx, cluster, node, device)
	if err != nil {
		return fmt.Errorf("fail device %s: %w", deviceID, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return respError("fail device "+deviceID, code, resp.Body)
	}
	return nil
}

// DeviceGrowth is the outcome of growing one device to the size its block
// device has now.
//
// The device's placement weight keeps the size it had when it was placed, so
// the capacity a growth adds is only fully usable once every data device of the
// cluster has grown by the same factor. Uniform reports whether that holds for
// the cluster after this growth, and Warning says what it means when it does
// not.
type DeviceGrowth struct {
	DeviceID string

	// OldSize and NewSize are the device's size before and after, in bytes.
	// They are equal when the block device had not grown.
	OldSize int64
	NewSize int64

	// PlacementSize is the size the device's placement weight is derived from.
	PlacementSize int64

	// Resized is false when there was nothing to grow.
	Resized bool

	Uniform bool
	Warning string
}

// GrowDevice grows one Linux block device (lblk) online to the size its block
// device now has, for example, a LUN grown on its storage array.
//
// The call is synchronous and idempotent: it returns when the device has taken
// the new size, and a repeat after a growth finds nothing to grow. The control
// plane refuses it with a 409 for a device that is not an online lblk device,
// and for a block device that shrank.
func (c *Client) GrowDevice(ctx context.Context, clusterID, nodeID, deviceID string) (DeviceGrowth, error) {
	cluster, node, device, err := parseDeviceIDs(clusterID, nodeID, deviceID)
	if err != nil {
		return DeviceGrowth{}, err
	}

	resp, err := c.api.ClustersStorageNodesDevicesResizeApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesDeviceIdResizePostWithResponse(
		ctx, cluster, node, device)
	if err != nil {
		return DeviceGrowth{}, fmt.Errorf("grow device %s: %w", deviceID, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 || resp.JSON200 == nil {
		return DeviceGrowth{}, respError("grow device "+deviceID, code, resp.Body)
	}
	return deviceGrowthFromDTO(*resp.JSON200), nil
}

func deviceGrowthFromDTO(d cpapi.DeviceResizeDTO) DeviceGrowth {
	g := DeviceGrowth{
		DeviceID:      d.DeviceId,
		OldSize:       int64(d.OldSize),
		NewSize:       int64(d.NewSize),
		PlacementSize: int64(d.PlacementSize),
		Resized:       d.Resized,
		Uniform:       d.Uniform,
	}
	if d.Warning != nil {
		g.Warning = *d.Warning
	}
	return g
}

// ClusterDeviceGrowth is the outcome of growing every lblk data device of a
// cluster.
type ClusterDeviceGrowth struct {
	// Devices holds one entry per device the call looked at, grown or not.
	Devices []DeviceGrowth

	// Errors maps the id of each device that could not be grown to the reason.
	Errors map[string]string

	// Uniform reports whether every data device has grown by the same factor,
	// so that the device weights keep their relative sizes.
	Uniform bool

	// Rebalance is set when the devices had grown unevenly and a weight update
	// was asked for.
	Rebalance *DeviceRebalance
}

// DeviceRebalance is the weight update and rebalance after an uneven growth.
type DeviceRebalance struct {
	// WeightsUpdated: every node places data by the devices' new sizes.
	WeightsUpdated bool
	// Started: the cluster rebalance that moves data to the new weights was
	// queued.
	Started bool
	// Reason says why either is missing.
	Reason string
}

// GrowClusterDevices grows every online lblk data device of a cluster to the
// size of its block device, one device at a time. A device that fails does not
// stop the others; it is reported in Errors. With rebalance, devices that have
// grown unevenly get weights by their new sizes and the cluster is rebalanced.
func (c *Client) GrowClusterDevices(
	ctx context.Context, clusterID string, rebalance bool,
) (ClusterDeviceGrowth, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return ClusterDeviceGrowth{}, err
	}

	resp, err := c.api.ClustersGrowDevicesApiV2ClustersClusterIdGrowDevicesPostWithResponse(ctx, cluster,
		&cpapi.ClustersGrowDevicesApiV2ClustersClusterIdGrowDevicesPostParams{Rebalance: &rebalance})
	if err != nil {
		return ClusterDeviceGrowth{}, fmt.Errorf("grow devices of cluster %s: %w", clusterID, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 || resp.JSON200 == nil {
		return ClusterDeviceGrowth{}, respError("grow devices of cluster "+clusterID, code, resp.Body)
	}
	out := ClusterDeviceGrowth{Errors: resp.JSON200.Errors, Uniform: resp.JSON200.Uniform}
	if r := resp.JSON200.Rebalance; r != nil {
		out.Rebalance = &DeviceRebalance{WeightsUpdated: r.WeightsUpdated, Started: r.Started, Reason: r.Reason}
	}
	for _, raw := range resp.JSON200.Devices {
		out.Devices = append(out.Devices, deviceGrowthFromMap(raw))
	}
	return out, nil
}

// deviceGrowthFromMap reads one entry of the cluster call's device list, which
// the spec types as a free-form object.
func deviceGrowthFromMap(m map[string]interface{}) DeviceGrowth {
	num := func(k string) int64 {
		if f, ok := m[k].(float64); ok {
			return int64(f)
		}
		return 0
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	flag := func(k string) bool {
		b, _ := m[k].(bool)
		return b
	}
	return DeviceGrowth{
		DeviceID:      str("device_id"),
		OldSize:       num("old_size"),
		NewSize:       num("new_size"),
		PlacementSize: num("placement_size"),
		Resized:       flag("resized"),
		Uniform:       flag("uniform"),
		Warning:       str("warning"),
	}
}

// parseNodeIDs parses a cluster and storage-node identifier.
func parseNodeIDs(clusterID, nodeID string) (cluster, node uuid.UUID, err error) {
	if cluster, err = parseUUID("cluster id", clusterID); err != nil {
		return cluster, node, err
	}
	node, err = parseUUID("storage node id", nodeID)
	return cluster, node, err
}

// parseDeviceIDs parses the three identifiers every device path carries.
func parseDeviceIDs(clusterID, nodeID, deviceID string) (cluster, node, device uuid.UUID, err error) {
	if cluster, node, err = parseNodeIDs(clusterID, nodeID); err != nil {
		return cluster, node, device, err
	}
	device, err = parseUUID("device id", deviceID)
	return cluster, node, device, err
}
