// The storage-class parameters the controller service reads, and the parsing
// each one needs. A parameter's name and its meaning stay together.
package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// var errVolumeInCreation = status.Error(codes.Internal, "volume in creation")
const (
	annotationNvmfModelID = "simplyblock.io/nvmf-model-id"
	annotationLvolID      = "simplyblock.io/lvol-id"
	annotationQoSRWIOPS   = "simplyblock.io/qos-rw-iops"
	annotationQoSRWMBps   = "simplyblock.io/qos-rw-mbps"
	annotationQoSRMBps    = "simplyblock.io/qos-r-mbps"
	annotationQoSWMBps    = "simplyblock.io/qos-w-mbps"
	annotationPodAffinity = "simplyblock.io/pod-affinity"

	// Deprecated annotation keys — still supported for backward compatibility.
	deprecatedAnnotationNvmfModelID = "simplybk/nvmf-model-id"
	deprecatedAnnotationLvolID      = "simplybk/lvol-id"
	deprecatedAnnotationQoSRWIOPS   = "simplybk/qos-rw-iops"
	deprecatedAnnotationQoSRWMBps   = "simplybk/qos-rw-mbytes"
	deprecatedAnnotationQoSRMBps    = "simplybk/qos-r-mbytes"
	deprecatedAnnotationQoSWMBps    = "simplybk/qos-w-mbytes"

	paramZoneClusterMap     = "zone_cluster_map"
	paramRegionClusterMap   = "region_cluster_map"
	paramDHCHAPNodeSelector = "dhchap_node_selector" // exact DHCHAP allowed-node label key; see poolNodeLabelKey

)

// dhchapAllowedNodeLabelValue must match the literal value the operator's
// syncNodeLabels writes onto every node in a pool's AllowedNodes
// (simplyblockstoragepool_controller.go) — see dhchapAllowedNodeSegment.
const dhchapAllowedNodeLabelValue = "allowed"

// dhchapAllowedNodeSegment returns the DHCHAP allowed-node topology key/value
// to pin PersistentVolume.spec.nodeAffinity to, or ("", "") for a plain,
// ungated volume. Matches the exact key from paramDHCHAPNodeSelector rather than
// a shared prefix, since a node can belong to more than one DHCHAP pool and
// prefix-matching would AND their labels together into one nodeAffinity.
//
// Deliberately does not consult req.GetAccessibilityRequirements(): unlike
// the zone/region segments below, which genuinely depend on which node a
// specific volume/pod landed on, this constraint is node-independent ("any
// node currently allowed for this pool") and its value is always the same
// fixed constant, so there's nothing to look up. That matters because
// AccessibilityRequirements is populated by external-provisioner only for
// topology keys already registered in the node's CSINode object — fixed at
// CSI plugin registration time — so a pool's label key would silently be
// missing from it the first time a node is added to that pool, until the
// plugin happens to re-register. Building the segment straight from the
// StorageClass parameter sidesteps that registration-timing gap entirely.
func dhchapAllowedNodeSegment(req *csi.CreateVolumeRequest) (key, val string) {
	key = strings.TrimSpace(req.GetParameters()[paramDHCHAPNodeSelector])
	if key == "" {
		return "", ""
	}
	return key, dhchapAllowedNodeLabelValue
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
