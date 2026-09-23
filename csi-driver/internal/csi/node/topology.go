// What this node reports about itself, and the topology the controller service
// uses to decide where a volume can be placed.
package node

import (
	"context"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/kube"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (ns *Server) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	topology := ns.buildAccessibleTopology(ctx)

	response := &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.GetNodeID(),
	}

	if len(topology) > 0 {
		response.AccessibleTopology = &csi.Topology{Segments: topology}
	}

	return response, nil
}

func (ns *Server) buildAccessibleTopology(ctx context.Context) map[string]string {
	if ns.kubeClient == nil {
		return nil
	}

	nodeName := ns.Driver.GetNodeID()
	if nodeName == "" {
		return nil
	}

	const maxRetries = 5
	const retryDelay = 5 * time.Second

	node, err := ns.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	for attempt := 2; err != nil && attempt <= maxRetries; attempt++ {
		klog.Warningf("topology discovery: failed to get node %s (attempt %d/%d): %v",
			nodeName, attempt-1, maxRetries, err)
		time.Sleep(retryDelay)
		node, err = ns.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	}
	if err != nil {
		// All retries exhausted. Crash so the pod restarts and retries from a
		// clean state, since registering without topology silently breaks PVC provisioning.
		klog.Fatalf("topology discovery: giving up after %d attempts for node %s — crashing to trigger pod restart: %v",
			maxRetries, nodeName, err)
	}

	segments := make(map[string]string)

	if zone, ok := node.Labels[csicommon.TopologyKeyZoneStable]; ok && zone != "" {
		segments[csicommon.TopologyKeyZoneStable] = zone
	} else if zone, ok := node.Labels[csicommon.TopologyKeyZoneBeta]; ok && zone != "" {
		segments[csicommon.TopologyKeyZoneStable] = zone
	}

	if region, ok := node.Labels[csicommon.TopologyKeyRegionStable]; ok && region != "" {
		segments[csicommon.TopologyKeyRegionStable] = region
	}

	for key, val := range node.Labels {
		if strings.HasPrefix(key, kube.LabelPoolPrefix) && val == kube.LabelPoolAllowed {
			segments[key] = val
		}
		if strings.HasPrefix(key, csicommon.TopologyKeyStorageNodeUUIDPrefix) {
			segments[key] = val
		}
	}

	// Whether this node can run a client-side compressed or deduplicated volume,
	// which is the node half of a pair: the controller service stamps such a
	// volume's PV with accessible topology naming this same key, and the
	// scheduler will only place the pod on a node whose CSINode carries it.
	// Publishing it on only one of the two sides matches nothing, and says
	// nothing while doing so: the PV asks for a key no node advertises, and the
	// pod sits Pending with no failure to report.
	//
	// The value travels as it is rather than being filtered to the capable ones,
	// so that a node whose probe answered no is distinguishable from one whose
	// probe has not answered at all. A node with no label advertises no segment,
	// because inventing one would claim an answer nobody established.
	if capable, ok := node.Labels[kube.LabelVDOCapable]; ok {
		segments[kube.LabelVDOCapable] = capable
	}

	if len(segments) == 0 {
		// No zone/region labels found. Return hostname so the external-provisioner
		// can still build AccessibilityRequirements. Without at least one topology
		// key on the CSINode, WaitForFirstConsumer provisioning fails. The controller
		// falls through to its single-cluster fallback when hostname doesn't match
		// any zone/region map entry.
		return map[string]string{"topology.simplyblock.io/hostname": node.Name}
	}

	return segments
}
