// The allowed hosts of one volume: which hosts the target lets connect to its
// subsystem. The metadata server's guests connect volumes themselves, each with
// a host NQN of its own, so the operator adds those hosts to every volume that
// restricts them (design-pnfs-mds-standby.md, P0-2).
package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

// HostGrant is what EnsureVolumeHost did.
type HostGrant int

const (
	// HostNotNeeded is a volume that lists no allowed hosts, which the control
	// plane reads as allowing every host. Nothing was changed.
	HostNotNeeded HostGrant = iota + 1
	// HostAlreadyAllowed is a host the volume already lists.
	HostAlreadyAllowed
	// HostAdded is a host the call added to the volume.
	HostAdded
)

// The control plane answers a duplicate add and a removal of an unlisted host
// with a 400 whose message is the only thing that tells them from a failure.
const (
	messageAlreadyAllowed = "already allowed"
	messageNotListed      = "not in the allowed list"
)

// EnsureVolumeHost makes hostNQN an allowed host of the volume, when the volume
// restricts hosts at all.
//
// A volume that lists no allowed hosts allows every host, so adding one would
// not grant anything: the control plane appends it to the list, and the next
// time it re-applies the list to the target the volume admits that host and no
// other. That volume is left alone, and the call says so with HostNotNeeded.
func (c *Client) EnsureVolumeHost(ctx context.Context, h lvol.VolumeHandle, hostNQN string) (HostGrant, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return 0, err
	}
	if hostNQN == "" {
		return 0, fmt.Errorf("allow a host on volume %s: empty host NQN", h)
	}
	what := fmt.Sprintf("allow host %s on volume %s", hostNQN, h)

	detail, err := c.api.ClustersStoragePoolsVolumesDetailApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdGetWithResponse(ctx, cluster, pool, volume, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	d, err := payload(what, detail.JSON200, detail.StatusCode(), detail.Body)
	if err != nil {
		return 0, err
	}
	switch {
	case len(d.AllowedHosts) == 0:
		return HostNotNeeded, nil
	case slices.Contains(d.AllowedHosts, hostNQN):
		return HostAlreadyAllowed, nil
	}

	resp, err := c.api.ClustersStoragePoolsVolumesAddHostApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdHostsPostWithResponse(
		ctx, cluster, pool, volume, cpapi.UnderscoreAddHostParams{HostNqn: hostNQN})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	switch code := resp.StatusCode(); {
	case code == http.StatusCreated || code == http.StatusOK:
		return HostAdded, nil
	case code == http.StatusBadRequest && strings.Contains(string(resp.Body), messageAlreadyAllowed):
		return HostAlreadyAllowed, nil
	default:
		return 0, respError(what, code, resp.Body)
	}
}

// RemoveVolumeHost removes hostNQN from the volume's allowed hosts. A host the
// volume does not list, and a volume that is gone, count as done.
func (c *Client) RemoveVolumeHost(ctx context.Context, h lvol.VolumeHandle, hostNQN string) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	if hostNQN == "" {
		return fmt.Errorf("remove a host from volume %s: empty host NQN", h)
	}
	what := fmt.Sprintf("remove host %s from volume %s", hostNQN, h)
	resp, err := c.api.ClustersStoragePoolsVolumesRemoveHostApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdHostsHostNqnDeleteWithResponse(
		ctx, cluster, pool, volume, hostNQN)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	switch code := resp.StatusCode(); {
	case code == http.StatusOK || code == http.StatusNoContent || code == http.StatusNotFound:
		return nil
	case code == http.StatusBadRequest && strings.Contains(string(resp.Body), messageNotListed):
		return nil
	default:
		return respError(what, code, resp.Body)
	}
}
