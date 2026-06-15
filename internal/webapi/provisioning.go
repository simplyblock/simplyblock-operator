/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// StoragePoolCreateParams is the request body for POST /api/v2/clusters/{id}/storage-pools/.
type StoragePoolCreateParams struct {
	Name string `json:"name"`
}

// VolumeCreateParams is the request body for POST /api/v2/clusters/{id}/storage-pools/{id}/volumes/.
// Only the fields required for benchmark volumes are exposed; all other fields use API defaults.
type VolumeCreateParams struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	HostID string `json:"host_id,omitempty"`
}

// CreatePool creates a new storage pool in the given cluster.
func (c *Client) CreatePool(ctx context.Context, clusterUUID string, params StoragePoolCreateParams) (*StoragePoolInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, params)
	if err != nil {
		return nil, fmt.Errorf("create pool %q: %w", params.Name, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("create pool %q: status %d: %s", params.Name, statusCode, string(body))
	}
	var pool StoragePoolInfo
	if err := json.Unmarshal(body, &pool); err != nil {
		return nil, fmt.Errorf("unmarshal create pool response: %w", err)
	}
	return &pool, nil
}

// CreateVolume creates a new volume in the given storage pool.
// The API returns HTTP 201 with a Location header containing the resource URL;
// the volume UUID is extracted from the trailing path segment of that URL.
func (c *Client) CreateVolume(ctx context.Context, clusterUUID, poolUUID string, params VolumeCreateParams) (*VolumeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/", clusterUUID, poolUUID)
	body, headers, statusCode, err := c.DoWithHeaders(ctx, http.MethodPost, endpoint, params)
	if err != nil {
		return nil, fmt.Errorf("create volume %q: %w", params.Name, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("create volume %q: status %d: %s", params.Name, statusCode, string(body))
	}
	location := headers.Get("Location")
	if location == "" {
		return nil, fmt.Errorf("create volume %q: no Location header in 201 response", params.Name)
	}
	trimmed := strings.TrimRight(location, "/")
	uuid := trimmed[strings.LastIndex(trimmed, "/")+1:]
	if uuid == "" {
		return nil, fmt.Errorf("create volume %q: cannot parse UUID from Location: %s", params.Name, location)
	}
	return &VolumeInfo{UUID: uuid, Name: params.Name}, nil
}
