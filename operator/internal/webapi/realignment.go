package webapi

import (
	"context"
	"fmt"
	"net/http"
)

// TriggerDataRealignment asks the control plane to re-align the cluster's internal
// data structures to the current volume placement, restoring fault-tolerance (FTT)
// and node-affinity guarantees after one or more volumes have been moved (by the
// auto-rebalancer, a manual VolumeMigration, or a storage node drain and removal).
func (c *Client) TriggerDataRealignment(
	ctx context.Context,
	clusterUUID string,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/rebalance", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("trigger data realignment for cluster %s: %w", clusterUUID, err)
	}
	if statusCode >= 300 {
		return fmt.Errorf("trigger data realignment for cluster %s: status %d: %s", clusterUUID, statusCode, string(body))
	}
	return nil
}
