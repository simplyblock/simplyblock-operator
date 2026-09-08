// Which cluster a volume is created in, and on which node it is placed.
//
// Every function here is a pure function of the topology the external
// provisioner passed and the parameters the class carries, which is what makes
// placement testable without a cluster.
package controller

import (
	"fmt"
	rand "math/rand/v2"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

type clusterSelection struct {
	clusterID string
	topology  map[string]string
}

func (cs *Server) resolveClusterSelection(req *csi.CreateVolumeRequest) (*clusterSelection, error) {
	params := req.GetParameters()
	if params == nil {
		return nil, fmt.Errorf("missing parameters in CreateVolumeRequest")
	}

	if id, ok := params[csicommon.ParamClusterID]; ok {
		id = strings.TrimSpace(id)
		if id != "" {
			return &clusterSelection{clusterID: id}, nil
		}
	}

	var (
		zoneMap   map[string]string
		regionMap map[string]string
		err       error
	)

	if raw, ok := params[paramZoneClusterMap]; ok {
		zoneMap, err = parseStringMap(raw, paramZoneClusterMap)
		if err != nil {
			return nil, err
		}
	}

	if raw, ok := params[paramRegionClusterMap]; ok {
		regionMap, err = parseStringMap(raw, paramRegionClusterMap)
		if err != nil {
			return nil, err
		}
	}

	if len(zoneMap) == 0 && len(regionMap) == 0 {
		return nil, fmt.Errorf("no %s or %s provided and %s not set",
			paramZoneClusterMap, paramRegionClusterMap, csicommon.ParamClusterID)
	}

	topoReq := req.GetAccessibilityRequirements()

	tryList := func(list []*csi.Topology) *clusterSelection {
		for _, topo := range list {
			if sel := matchTopologyWithZoneMap(topo, zoneMap); sel != nil {
				return sel
			}
			if sel := matchTopologyWithRegionMap(topo, regionMap); sel != nil {
				return sel
			}
		}
		return nil
	}

	if topoReq != nil {
		if sel := tryList(topoReq.GetPreferred()); sel != nil {
			return sel, nil
		}
		if sel := tryList(topoReq.GetRequisite()); sel != nil {
			return sel, nil
		}
		// Topology was provided but contains no zone or region key recognised by
		// the StorageClass map. This means the worker node is missing the required
		// topology labels.
		nodeName := nodeNameFromTopology(topoReq.GetPreferred())
		if nodeName == "" {
			nodeName = nodeNameFromTopology(topoReq.GetRequisite())
		}
		return nil, fmt.Errorf(
			"node %q has no %s or %s topology label but the StorageClass uses %s/%s "+
				"for cluster routing; add the appropriate zone or region label to the node, "+
				"or switch the StorageClass to use %s",
			nodeName, csicommon.TopologyKeyZoneStable, csicommon.TopologyKeyRegionStable,
			paramZoneClusterMap, paramRegionClusterMap, csicommon.ParamClusterID,
		)
	}

	return nil, fmt.Errorf(
		"no topology requirements received; the StorageClass uses %s or %s for cluster "+
			"routing but the node has no topology labels — add %s or %s labels to the node, "+
			"or switch the StorageClass to use %s",
		paramZoneClusterMap, paramRegionClusterMap,
		csicommon.TopologyKeyZoneStable, csicommon.TopologyKeyRegionStable, csicommon.ParamClusterID,
	)
}

// nodeNameFromTopology extracts the node name from the simplyblock hostname
// fallback topology key set by the node plugin when no zone/region labels exist.
func nodeNameFromTopology(topos []*csi.Topology) string {
	for _, topo := range topos {
		if topo == nil {
			continue
		}
		if name, ok := topo.GetSegments()["topology.simplyblock.io/hostname"]; ok && name != "" {
			return name
		}
	}
	return "unknown"
}

// coLocatedHostID extracts the storage-node UUID co-located with the consuming
// Pod's scheduled worker, scoped to clusterID, from CSI topology requirements —
// i.e. a csicommon.TopologyKeyStorageNodeUUIDPrefix-prefixed segment written by the
// simplyblock-operator onto the Kubernetes Node the Pod was scheduled to (see
// StorageNodeSetReconciler.labelWorkerNodes) and advertised via
// nodeserver.buildAccessibleTopology. The segment KEY is
// "<prefix><clusterUUID>.<socketOrdinal>" and the VALUE is the storage-node UUID.
// Preferred segments are checked first, then Requisite, matching
// nodeNameFromTopology's fallback order. When a worker hosts more than one
// storage-node instance (NUMA sockets), one of the matching instances is picked
// uniformly at random, so volumes routed here via Tier 1 spread across every
// co-located instance instead of piling onto one — this is a host-level
// guarantee only, not true socket-level affinity, which isn't resolvable at
// CreateVolume time (kubelet's Topology Manager pins a Pod to a NUMA socket only
// at container start). Returns "" if no segment matches.
func coLocatedHostID(topoReq *csi.TopologyRequirement, clusterID string) string {
	if topoReq == nil {
		return ""
	}
	find := func(topologies []*csi.Topology) string {
		var candidates []string
		for _, topo := range topologies {
			for key, uuid := range topo.GetSegments() {
				if !strings.HasPrefix(key, csicommon.TopologyKeyStorageNodeUUIDPrefix) {
					continue
				}
				if uuid == "" {
					continue
				}
				slot := key[len(csicommon.TopologyKeyStorageNodeUUIDPrefix):]
				sep := strings.LastIndex(slot, ".")
				if sep < 0 {
					continue
				}
				labelCluster, ordinalStr := slot[:sep], slot[sep+1:]
				if labelCluster != clusterID {
					continue
				}
				if _, err := strconv.Atoi(ordinalStr); err != nil {
					continue
				}
				candidates = append(candidates, uuid)
			}
		}
		if len(candidates) == 0 {
			return ""
		}
		return candidates[rand.IntN(len(candidates))]
	}
	if uuid := find(topoReq.GetPreferred()); uuid != "" {
		return uuid
	}
	return find(topoReq.GetRequisite())
}

func matchTopologyWithRegionMap(topo *csi.Topology, regionMap map[string]string) *clusterSelection {
	if topo == nil {
		return nil
	}

	region := regionFromTopology(topo)
	if region == "" {
		return nil
	}

	clusterID, ok := regionMap[region]
	if !ok {
		return nil
	}

	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return nil
	}

	return &clusterSelection{
		clusterID: clusterID,
		topology:  map[string]string{csicommon.TopologyKeyRegionStable: region},
	}
}

func matchTopologyWithZoneMap(topo *csi.Topology, zoneMap map[string]string) *clusterSelection {
	if topo == nil {
		return nil
	}

	zone := zoneFromTopology(topo)
	if zone == "" {
		return nil
	}

	clusterID, ok := zoneMap[zone]
	if !ok {
		return nil
	}

	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return nil
	}

	return &clusterSelection{
		clusterID: clusterID,
		topology:  copyTopologySegments(topo.GetSegments()),
	}
}

func zoneFromTopology(topo *csi.Topology) string {
	if topo == nil {
		return ""
	}
	return zoneFromSegments(topo.GetSegments())
}

func zoneFromSegments(segments map[string]string) string {
	if segments == nil {
		return ""
	}
	if zone, ok := segments[csicommon.TopologyKeyZoneStable]; ok && zone != "" {
		return zone
	}
	if zone, ok := segments[csicommon.TopologyKeyZoneBeta]; ok && zone != "" {
		return zone
	}
	return ""
}

func regionFromSegments(segments map[string]string) string {
	if segments == nil {
		return ""
	}
	if r, ok := segments[csicommon.TopologyKeyRegionStable]; ok && r != "" {
		return r
	}
	return ""
}

func regionFromTopology(topo *csi.Topology) string {
	if topo == nil {
		return ""
	}
	return regionFromSegments(topo.GetSegments())
}

func copyTopologySegments(segments map[string]string) map[string]string {
	if len(segments) == 0 {
		return nil
	}

	copied := make(map[string]string, len(segments))
	for k, v := range segments {
		copied[k] = v
	}
	return copied
}
