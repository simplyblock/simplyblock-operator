// The storage-class parameters the controller service reads, and the parsing
// each one needs. A parameter's name and its meaning stay together.
package controller

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/simplyblock/atlas/kube"
)

// var errVolumeInCreation = status.Error(codes.Internal, "volume in creation")
const (
	annotationNvmfModelID = "simplyblock.io/nvmf-model-id"
	annotationLvolID      = "simplyblock.io/lvol-id"
	annotationPodAffinity = "simplyblock.io/pod-affinity"

	// Deprecated annotation keys, still supported for backward compatibility.
	deprecatedAnnotationNvmfModelID = "simplybk/nvmf-model-id"
	deprecatedAnnotationLvolID      = "simplybk/lvol-id"

	// The four QoS ceilings are not here. Each of them has three live spellings
	// and the operator writes one of them, so the keys and the order they are
	// tried in belong where both components can read them: atlas-lib's
	// kube.QoSParam and kube.QoSAnnotation.

	paramZoneClusterMap     = "zone_cluster_map"
	paramRegionClusterMap   = "region_cluster_map"
	paramDHCHAPNodeSelector = "dhchap_node_selector" // exact DHCHAP allowed-node label key, see kube.PoolNodeLabelKey

)

// dhchapAllowedNodeSegment returns the DHCHAP allowed-node topology key/value
// to pin PersistentVolume.spec.nodeAffinity to, or an empty key and value for
// a plain, ungated volume. It matches the exact key from
// paramDHCHAPNodeSelector rather than a shared prefix, since a node can belong
// to more than one DHCHAP pool and prefix-matching would AND their labels
// together into one nodeAffinity.
//
// Deliberately does not consult req.GetAccessibilityRequirements(): unlike
// the zone/region segments below, which genuinely depend on which node a
// specific volume/pod landed on, this constraint is node-independent ("any
// node currently allowed for this pool") and its value is always the same
// fixed constant, so there's nothing to look up. That matters because
// AccessibilityRequirements is populated by external-provisioner only for
// topology keys already registered in the node's CSINode object, fixed at
// CSI plugin registration time, so a pool's label key would silently be
// missing from it the first time a node is added to that pool, until the
// plugin happens to re-register. Building the segment straight from the
// StorageClass parameter sidesteps that registration-timing gap entirely.
func dhchapAllowedNodeSegment(req *csi.CreateVolumeRequest) (key, val string) {
	key = strings.TrimSpace(req.GetParameters()[paramDHCHAPNodeSelector])
	if key == "" {
		return "", ""
	}
	return key, kube.LabelPoolAllowed
}

// vdoCapableSegment returns the vdo-capable topology key/value to pin
// PersistentVolume.spec.nodeAffinity to, or an empty key and value for a
// volume that requested neither client-side parameter. Either
// client_compression or client_deduplication needs a working VDO stack on the
// node, so either one alone is enough to require the segment.
//
// Built straight from the StorageClass parameters and never from
// req.GetAccessibilityRequirements(), for the same reason
// dhchapAllowedNodeSegment is: unlike DHCHAP's per-pool label, vdo-capable is a
// node self-probed capability the csi-node DaemonSet applies asynchronously
// after it starts, so it is never present in the node's CSINode object at
// plugin-registration time. A generated StorageClass carries no
// allowedTopologies of its own for this reason — see design-issue-277's §5.
func vdoCapableSegment(req *csi.CreateVolumeRequest) (key, val string) {
	params := req.GetParameters()
	compression, _ := strconv.ParseBool(params[kube.ParamClientCompression])
	deduplication, _ := strconv.ParseBool(params[kube.ParamClientDeduplication])
	if !compression && !deduplication {
		return "", ""
	}
	return kube.LabelVDOCapable, "true"
}

func parseStringMap(raw, paramName string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s parameter is empty", paramName)
	}

	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse %s parameter: %w", paramName, err)
	}

	normalized := make(map[string]string, len(parsed))
	for key, value := range parsed {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		normalized[key] = value
	}

	if len(normalized) == 0 {
		return nil, fmt.Errorf("%s parameter did not contain any mappings", paramName)
	}
	return normalized, nil
}
