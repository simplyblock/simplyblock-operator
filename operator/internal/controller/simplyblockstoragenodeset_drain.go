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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// defaultSystemVolumeFilter is compiled once at package init from the
// well-known default pattern. A MustCompile panics at startup if the constant
// is malformed — intentional fast-fail for a hardcoded value.
var defaultSystemVolumeFilter = regexp.MustCompile(simplyblockv1alpha1.DefaultSystemVolumeFilterRegex)

// Requeue intervals used by the drain state machine.
const (
	drainRequeueImmediate  = 1 * time.Second
	drainRequeueSuspend    = 10 * time.Second
	drainRequeueMigrate    = 15 * time.Second
	drainRequeueMigrateNew = 10 * time.Second
	drainRequeueVerify     = 30 * time.Second
	drainRequeueBlocking   = 60 * time.Second
	drainRequeueValidate   = 30 * time.Second
)

// fetchPoolVolumes fetches all pools and returns (pools, nodeVolumes, err).
// Callers that need both the pool list (e.g. for cleanup) and the node volumes
// should call this once and reuse the returned pools, avoiding a second
// GetStoragePools round-trip within the same reconcile.
func fetchPoolVolumes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	nodeUUID string,
) (pools []webapi.StoragePoolInfo, nodeVols []webapi.VolumeInfo, err error) {
	pools, err = apiClient.GetStoragePools(ctx, clusterUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("listNodeVolumes: %w", err)
	}
	for _, pool := range pools {
		vols, err := apiClient.GetPoolVolumes(ctx, clusterUUID, pool.UUID)
		if err != nil {
			return nil, nil, fmt.Errorf("listNodeVolumes: pool %s: %w", pool.UUID, err)
		}
		for _, v := range vols {
			if v.PrimaryNodeUUID != nodeUUID {
				continue
			}
			// Skip volumes already being deleted — backend deletion is async so
			// the volume may still appear in the list briefly after DELETE 204.
			if v.Status == "in_deletion" {
				continue
			}
			nodeVols = append(nodeVols, v)
		}
	}
	return pools, nodeVols, nil
}

// listNodeVolumes returns volumes on nodeUUID. Use fetchPoolVolumes when the
// pool list is also needed (e.g. drainVerify cleanup) to avoid a double fetch.
func listNodeVolumes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	nodeUUID string,
) ([]webapi.VolumeInfo, error) {
	_, vols, err := fetchPoolVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	return vols, err
}

// matchVolumesToPVs classifies each backend volume into one of three buckets:
//   - pvManaged: volume UUID has a corresponding simplyblock PV, not pinned
//   - pinned: volume UUID has a PV but the PVC has the pinned-volume annotation
//   - unmanaged: volume UUID has no corresponding PV (and is not a system volume)
//
// System volumes (those matching filterRegex by name) are skipped entirely.
// pvNameByVolumeUUID maps volume UUID → PV name for the pvManaged and pinned buckets.
// matchVolumesToPVs classifies each backend volume into pvManaged, pinned, or
// unmanaged buckets. System volumes matching filterRegex are skipped entirely.
//
// Note: if the PVC fetch for a PV-backed volume fails (e.g. API server
// temporarily unavailable), that volume is conservatively placed in the
// unmanaged bucket. This will block drain with an UnmanagedVolumeBlocking
// event until the next reconcile succeeds. It is a transient false-positive,
// not a permanent classification.
// matchVolumesToPVs classifies backend volumes and additionally returns
// pvcFetchFailed=true when at least one PV-backed volume could not be classified
// because its PVC GET failed transiently. Callers in Migrating must requeue on
// pvcFetchFailed to avoid silently skipping volumes that would stall Verifying.
func matchVolumesToPVs(
	ctx context.Context,
	c client.Client,
	volumes []webapi.VolumeInfo,
	sysFilter *regexp.Regexp,
) (pvManaged, pinned, unmanaged []string, pvNameByVolumeUUID map[string]string, pvcFetchFailed bool, err error) {
	log := logf.FromContext(ctx)

	pvNameByVolumeUUID = make(map[string]string)

	// Build a map: volumeUUID → pvName for all simplyblock PVs.
	var pvList corev1.PersistentVolumeList
	if err = c.List(ctx, &pvList); err != nil {
		return nil, nil, nil, nil, false, fmt.Errorf("matchVolumesToPVs: list PVs: %w", err)
	}

	// pvByVolumeUUID maps volume UUID → PV object for simplyblock PVs.
	pvByVolumeUUID := make(map[string]*corev1.PersistentVolume, len(pvList.Items))
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != utils.CSIProvisioner {
			continue
		}
		// VolumeHandle format: clusterUUID:poolUUID:volumeUUID — extract last segment.
		volHandle := pv.Spec.CSI.VolumeHandle
		if volHandle != "" {
			parts := strings.SplitN(volHandle, ":", 3)
			volumeUUID := parts[len(parts)-1]
			if volumeUUID != "" {
				pvByVolumeUUID[volumeUUID] = pv
			}
		}
	}

	for _, vol := range volumes {
		// System volume: skip entirely.
		if sysFilter.MatchString(vol.Name) {
			continue
		}

		pv, isManagedByCSI := pvByVolumeUUID[vol.UUID]
		if !isManagedByCSI {
			unmanaged = append(unmanaged, vol.UUID)
			continue
		}

		// PV exists — check if the PVC has the pinned annotation.
		if pv.Spec.ClaimRef == nil {
			// PV has no claim; treat as PV-managed (not pinned).
			pvManaged = append(pvManaged, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
			continue
		}

		var pvc corev1.PersistentVolumeClaim
		if err := c.Get(ctx, types.NamespacedName{
			Namespace: pv.Spec.ClaimRef.Namespace,
			Name:      pv.Spec.ClaimRef.Name,
		}, &pvc); err != nil {
			log.Error(err, "matchVolumesToPVs: failed to get PVC, treating volume as unmanaged",
				"pvc", pv.Spec.ClaimRef.Name, "namespace", pv.Spec.ClaimRef.Namespace)
			unmanaged = append(unmanaged, vol.UUID)
			pvcFetchFailed = true
			continue
		}

		if kube.IsPinnedVolume(pvc.Annotations) {
			pinned = append(pinned, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
		} else {
			pvManaged = append(pvManaged, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
		}
	}

	return pvManaged, pinned, unmanaged, pvNameByVolumeUUID, pvcFetchFailed, nil
}

// getNodeBackendStatus fetches the current status string of a single storage
// node directly from the backend API.
func getNodeBackendStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("getNodeBackendStatus: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("getNodeBackendStatus: status %d", status)
	}
	var resp utils.NodeStatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("getNodeBackendStatus: unmarshal: %w", err)
	}
	return resp.Status, nil
}

// roundRobinTargetNodes lists all online nodes (excluding the drained node) and
// assigns each PV name a target node UUID using round-robin order. The i-th PV
// in pvNames is assigned to onlineNodes[i % len(onlineNodes)], distributing
// migrations evenly across the cluster without requiring persistent state.
// Returns an error if no online peer node is available.
func roundRobinTargetNodes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	excludeNodeUUID string,
	pvNames []string,
) (map[string]string, error) {
	nodes, err := apiClient.GetStorageNodes(ctx, clusterUUID)
	if err != nil {
		return nil, fmt.Errorf("roundRobinTargetNodes: %w", err)
	}

	var online []string
	for _, n := range nodes {
		if n.UUID != excludeNodeUUID && n.Status == utils.NodeStatusOnline {
			online = append(online, n.UUID)
		}
	}
	if len(online) == 0 {
		return nil, fmt.Errorf("roundRobinTargetNodes: no online node available other than %s", excludeNodeUUID)
	}

	assignment := make(map[string]string, len(pvNames))
	for i, pv := range pvNames {
		assignment[pv] = online[i%len(online)]
	}
	return assignment, nil
}

// drainMigrationName builds a DNS-label-safe name for a VolumeMigration CR.
func drainMigrationName(nodeUUID, pvName string) string {
	prefix := "drain-"
	if len(nodeUUID) >= 8 {
		prefix += nodeUUID[:8] + "-"
	}
	name := prefix + pvName
	name = strings.ToLower(name)
	// Replace invalid chars with '-'.
	var result []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else {
			result = append(result, '-')
		}
	}
	s := strings.Trim(string(result), "-")

	// Guard against name collisions when two PV names share a long common prefix
	// that gets truncated to the same 63-char string. Append a 6-char FNV-32
	// hash of the original pvName before truncating so each PV always maps to a
	// unique CR name regardless of length.
	const maxLen = 63
	if len(s) > maxLen {
		h := fnv32Hash(pvName)
		suffix := fmt.Sprintf("-%06x", h) // 7 chars: '-' + 6 hex digits
		keep := maxLen - len(suffix)
		if keep < 0 {
			keep = 0
		}
		s = s[:keep] + suffix
	}
	return s
}

// fnv32Hash returns a non-cryptographic 32-bit FNV-1a hash of s, used as a
// short disambiguation suffix in drainMigrationName.
func fnv32Hash(s string) uint32 {
	const (
		offset32 uint32 = 2166136261
		prime32  uint32 = 16777619
	)
	h := offset32
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}
