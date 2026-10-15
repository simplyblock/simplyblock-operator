// The two node-side implementations the driver wires onto a sync-configured
// node Server (design-sync-replication-csi-addons.md §6.1, §6.2): reading this
// node's site from its zone label, and setting a VolumeReplication's site
// annotation. They are kept apart from the stage-time logic in stage_sync.go
// because they touch the Kubernetes API (a node read and a dynamic-client
// patch), and because the driver assembles them with the clients it owns.
package node

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/simplyblock/atlas/kube"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// EnableSyncReplication turns on the scheduling-driven switchover for this node
// (design §6): resolver answers the per-PVC lookups, cachesSynced gates staging
// until the informers behind it have synced, and dyn is the client the VR patch
// uses. The driver calls this only when the cluster carries the VolumeReplication
// CRD; until it does, every volume stages on the ordinary, site-unaware path.
func (ns *Server) EnableSyncReplication(
	resolver *kube.InformerResolver, cachesSynced func() bool, dyn dynamic.Interface,
) {
	ns.syncResolver = resolver
	ns.syncCachesSynced = cachesSynced
	ns.syncNodeSite = ns.readNodeSite
	ns.syncPatchVR = func(ctx context.Context, vr kube.VolumeReplicationRef, site, state string) error {
		return patchVolumeReplicationSite(ctx, dyn, vr, site, state)
	}
}

// readNodeSite returns the replication site of the node this plugin runs on,
// read from its topology zone label: a sync cluster's two sites are its two
// zones (design §6.1, §9.6). The driver wires this onto Server.syncNodeSite.
func (ns *Server) readNodeSite(ctx context.Context) (string, error) {
	name := ns.Driver.GetNodeID()
	if name == "" {
		return "", fmt.Errorf("node name is empty")
	}
	node, err := ns.kubeClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get node %s: %w", name, err)
	}
	if z := node.Labels[csicommon.TopologyKeyZoneStable]; z != "" {
		return z, nil
	}
	if z := node.Labels[csicommon.TopologyKeyZoneBeta]; z != "" {
		return z, nil
	}
	return "", fmt.Errorf("node %s carries no %s zone label", name, csicommon.TopologyKeyZoneStable)
}

// patchVolumeReplicationSite sets a VolumeReplication's SyncSiteAnnotation to a
// site and its replicationState to state (primary or secondary): the write half
// of the switchover the node performs at stage time (design §6.2, §6.4). The
// driver wraps this with its dynamic client into Server.syncPatchVR. The patch is
// a JSON merge, so it touches only the annotation and replicationState and leaves
// the rest of the object, the class included, as the administrator set it. The
// class is immutable, so the site has to ride on this mutable annotation rather
// than on the class. A re-apply of the administrator's spec does not carry the
// annotation, so it does not revert the node's site (design §6.2, co-ownership).
func patchVolumeReplicationSite(
	ctx context.Context, dyn dynamic.Interface, vr kube.VolumeReplicationRef, site, state string,
) error {
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q}},"spec":{"replicationState":%q}}`,
		kube.SyncSiteAnnotation, site, state))
	_, err := dyn.Resource(kube.VolumeReplicationGVR).Namespace(vr.Namespace).
		Patch(ctx, vr.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch VolumeReplication %s/%s to site %s: %w", vr.Namespace, vr.Name, site, err)
	}
	return nil
}
