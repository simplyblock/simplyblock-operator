// The controller service: the CSI RPCs that run wherever the driver's
// controller plugin runs, rather than on the node holding the volume.
package controller

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/csi-addons/spec/lib/go/replication"
	"k8s.io/client-go/kubernetes"

	"github.com/simplyblock/atlas/lvol"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

type Server struct {
	*csicommon.DefaultControllerServer
	// The GroupController service (VolumeGroupSnapshot) is implemented in
	// groupsnapshot.go; embedding the unimplemented server satisfies the
	// interface's forward-compat guard for any method not overridden.
	csi.UnimplementedGroupControllerServer
	// The csi-addons Replication service (design §5) is implemented in
	// replication.go, for the three Phase 1 verbs; the rest
	// (PromoteVolume, DemoteVolume, ResyncVolume) fall through to this
	// embedded default until Phase 2.
	replication.UnimplementedControllerServer
	// The csi-addons VolumeGroup (GroupController) service (design §14.3) is
	// implemented in volumegroup.go. Its unimplemented base is embedded through
	// a named wrapper (volumeGroupUnimplemented) because the replication base
	// above already occupies the UnimplementedControllerServer embed name.
	volumeGroupUnimplemented
	volumeLocks *csicommon.VolumeLocks
	// kubeClient reads/patches PVC annotations (host_id resolution, placement-hint
	// cleanup). Built once at construction and reused, and nil when no in-cluster
	// config is available (e.g., unit tests), in which case the annotation helpers
	// are no-ops.
	kubeClient kubernetes.Interface
	// exports records the NFSExport a pNFS volume needs. Nil outside a
	// cluster and when the CRD is unavailable, in which case a pNFS claim is
	// refused with a clear message rather than the driver failing to start.
	exports ExportRegistry
	// syncVolumeSite resolves the site a sync volume is served from, read from its
	// VolumeReplication's SyncSiteAnnotation (design §5.2, §6.3). Nil until the
	// driver wires it (the controller needs a VolumeReplication resolver); the
	// sync write verbs error rather than guess a site when it is unset.
	syncVolumeSite func(context.Context, lvol.VolumeHandle) (string, error)
}

// EnableSyncSiteResolution wires the resolver the sync write verbs (PromoteVolume,
// DemoteVolume) use to read a volume's active site from its VolumeReplication's
// SyncSiteAnnotation (design §5.2, §6.3). The driver calls it when the cluster
// carries the VolumeReplication CRD; until then the sync verbs have no site
// source and refuse rather than guess.
func (cs *Server) EnableSyncSiteResolution(resolve func(context.Context, lvol.VolumeHandle) (string, error)) {
	cs.syncVolumeSite = resolve
}

//nolint:unparam // error return kept for constructor symmetry / future use
func New(d *csicommon.CSIDriver, kubeClient kubernetes.Interface, exports ExportRegistry) (*Server, error) {
	server := Server{
		DefaultControllerServer: csicommon.NewDefaultControllerServer(d),
		volumeLocks:             csicommon.NewVolumeLocks(),
		kubeClient:              kubeClient,
		exports:                 exports,
	}
	return &server, nil
}
