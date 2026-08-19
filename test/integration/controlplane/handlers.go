// The endpoints the simulator answers.
//
// Every method here implements one of the generated ServerInterface's, and that
// is enforced: ./gen stubs the rest with 501 and fails if a method here matches
// no endpoint in the spec, which is what a rename or removal looks like. So this
// file is the list of what the simulator supports, and adding to it is nothing
// more than writing the method and regenerating.
//
// The parameters arrive decoded — path UUIDs and query params — because the
// generated router does that before dispatching.
package cpsim

import (
	"errors"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

func (s *Server) HealthApiV2MetaHealthGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ReadyApiV2MetaReadyGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ClustersStoragePoolsListApiV2ClustersClusterIdStoragePoolsGet(
	w http.ResponseWriter, _ *http.Request, clusterId openapi_types.UUID,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := []StoragePoolDTO{}
	for _, p := range s.pools {
		if p.ClusterID == clusterId {
			out = append(out, poolDTO(p))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ClustersStoragePoolsDetailApiV2ClustersClusterIdStoragePoolsPoolIdGet(
	w http.ResponseWriter, _ *http.Request, _ openapi_types.UUID, poolId openapi_types.UUID,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.pools[poolId]
	if !ok {
		http.Error(w, "pool not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, poolDTO(p))
}

func (s *Server) ClustersStorageNodesListApiV2ClustersClusterIdStorageNodesGet(
	w http.ResponseWriter, _ *http.Request, clusterId openapi_types.UUID,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := []StorageNodeDTO{}
	for _, n := range s.nodes {
		if n.ClusterID == clusterId {
			out = append(out, nodeDTO(n))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ClustersStorageNodesDetailApiV2ClustersClusterIdStorageNodesStorageNodeIdGet(
	w http.ResponseWriter, _ *http.Request, _ openapi_types.UUID, storageNodeId openapi_types.UUID,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n, ok := s.nodes[storageNodeId]
	if !ok {
		http.Error(w, "storage node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, nodeDTO(n))
}

func (s *Server) ClustersStoragePoolsVolumesListApiV2ClustersClusterIdStoragePoolsPoolIdVolumesGet(
	w http.ResponseWriter, _ *http.Request, _ openapi_types.UUID, poolId openapi_types.UUID,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := []VolumeDTO{}
	for _, v := range s.volumes {
		if v.PoolID == poolId {
			out = append(out, volumeDTO(v))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ClustersStoragePoolsVolumesDetailApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdGet(
	w http.ResponseWriter, _ *http.Request, _, _, volumeId openapi_types.UUID,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	v, ok := s.volumes[volumeId]
	if !ok {
		http.Error(w, "volume not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, volumeDTO(v))
}

// ClustersStoragePoolsVolumesConnectApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdConnectGet
// is the endpoint everything else here exists to serve: how to reach the volume
// over NVMe-oF.
func (s *Server) ClustersStoragePoolsVolumesConnectApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdConnectGet(
	w http.ResponseWriter, _ *http.Request, _, _, volumeId openapi_types.UUID,
	params ClustersStoragePoolsVolumesConnectApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdConnectGetParams,
) {
	s.mu.RLock()
	v, ok := s.volumes[volumeId]
	var entries []NvmeConnectEntry
	var err error
	if ok {
		hostNQN := ""
		if params.HostNqn != nil {
			hostNQN = *params.HostNqn
		}
		entries, err = s.connectEntries(v, hostNQN)
	}
	s.mu.RUnlock()

	if !ok {
		http.Error(w, "volume not found", http.StatusNotFound)
		return
	}

	// An unauthorized host is a 404 whose body is the bare message, not JSON —
	// the control plane's own shape, and a caller may match on the text.
	var he hostError
	if errors.As(err, &he) {
		http.Error(w, string(he), http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
