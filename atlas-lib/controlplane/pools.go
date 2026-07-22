package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/ptr"
)

// StoragePool is a simplyblock storage pool within a cluster.
type StoragePool struct {
	ID           string
	ClusterID    string
	Name         string
	MaxSizeBytes uint64
}

func storagePoolFromDTO(d cpapi.StoragePoolDTO) StoragePool {
	return StoragePool{
		ID:           d.Id.String(),
		ClusterID:    d.ClusterId.String(),
		Name:         d.Name,
		MaxSizeBytes: uint64(d.MaxSize),
	}
}

// ListStoragePools returns every storage pool in a cluster.
func (c *Client) ListStoragePools(ctx context.Context, clusterID string) ([]StoragePool, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersStoragePoolsListApiV2ClustersClusterIdStoragePoolsGetWithResponse(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("list storage pools in %s: %w", clusterID, err)
	}
	if resp.JSON200 == nil {
		return nil, respError("list storage pools in "+clusterID, resp.StatusCode(), resp.Body)
	}
	out := make([]StoragePool, 0, len(*resp.JSON200))
	for _, d := range *resp.JSON200 {
		out = append(out, storagePoolFromDTO(d))
	}
	return out, nil
}

// GetStoragePool returns a single storage pool by id. It wraps
// errs.ErrNotFound when the pool does not exist.
func (c *Client) GetStoragePool(ctx context.Context, clusterID, poolID string) (StoragePool, error) {
	cluster, pool, err := parseIDs(clusterID, poolID)
	if err != nil {
		return StoragePool{}, err
	}
	resp, err := c.api.ClustersStoragePoolsDetailApiV2ClustersClusterIdStoragePoolsPoolIdGetWithResponse(ctx, cluster, pool)
	if err != nil {
		return StoragePool{}, fmt.Errorf("get storage pool %s: %w", poolID, err)
	}
	if resp.JSON200 == nil {
		return StoragePool{}, respError("storage pool "+poolID, resp.StatusCode(), resp.Body)
	}
	return storagePoolFromDTO(*resp.JSON200), nil
}

// StoragePoolByName returns the pool with the given name in a cluster. The v2
// API has no by-name lookup, so this lists and filters; it wraps
// errs.ErrNotFound when no pool matches.
func (c *Client) StoragePoolByName(ctx context.Context, clusterID, name string) (StoragePool, error) {
	pools, err := c.ListStoragePools(ctx, clusterID)
	if err != nil {
		return StoragePool{}, err
	}
	for _, p := range pools {
		if p.Name == name {
			return p, nil
		}
	}
	return StoragePool{}, fmt.Errorf("storage pool %q in cluster %s: %w", name, clusterID, errs.ErrNotFound)
}

// CreateStoragePoolParams are the inputs for creating a storage pool. Only
// Name is required.
type CreateStoragePoolParams struct {
	Name               string
	MaxSizeBytes       uint64 // pool_max; 0 = unlimited
	VolumeMaxSizeBytes uint64 // per-volume cap; 0 = unset

	// QoS limits; 0 means unset.
	MaxRWIOPS   int
	MaxRWMbytes int
	MaxRMbytes  int
	MaxWMbytes  int
}

// CreateStoragePool creates a storage pool and returns it.
func (c *Client) CreateStoragePool(ctx context.Context, clusterID string, params CreateStoragePoolParams) (StoragePool, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return StoragePool{}, err
	}
	body := cpapi.StoragePoolParams{Name: params.Name}
	if params.MaxSizeBytes > 0 {
		body.PoolMax = ptr.To(int(params.MaxSizeBytes))
	}
	if params.VolumeMaxSizeBytes > 0 {
		body.VolumeMaxSize = ptr.To(int(params.VolumeMaxSizeBytes))
	}
	if params.MaxRWIOPS > 0 {
		body.MaxRwIops = ptr.To(params.MaxRWIOPS)
	}
	if params.MaxRWMbytes > 0 {
		body.MaxRwMbytes = ptr.To(params.MaxRWMbytes)
	}
	if params.MaxRMbytes > 0 {
		body.MaxRMbytes = ptr.To(params.MaxRMbytes)
	}
	if params.MaxWMbytes > 0 {
		body.MaxWMbytes = ptr.To(params.MaxWMbytes)
	}

	resp, err := c.api.ClustersStoragePoolsCreateApiV2ClustersClusterIdStoragePoolsPostWithResponse(ctx, cluster, nil, body)
	if err != nil {
		return StoragePool{}, fmt.Errorf("create storage pool %q: %w", params.Name, err)
	}
	// The create endpoint returns the full StoragePoolDTO body (untyped in the
	// spec's response, so decode it here).
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return StoragePool{}, respError("create storage pool "+params.Name, code, resp.Body)
	}
	var d cpapi.StoragePoolDTO
	if err := json.Unmarshal(resp.Body, &d); err != nil {
		return StoragePool{}, fmt.Errorf("create storage pool %q: decode response: %w", params.Name, err)
	}
	return storagePoolFromDTO(d), nil
}
