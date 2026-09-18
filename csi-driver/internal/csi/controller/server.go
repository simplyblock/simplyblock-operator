// The controller service: the CSI RPCs that run wherever the driver's
// controller plugin runs, rather than on the node holding the volume.
package controller

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/client-go/kubernetes"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

type Server struct {
	*csicommon.DefaultControllerServer
	// The GroupController service (VolumeGroupSnapshot) is implemented in
	// groupsnapshot.go; embedding the unimplemented server satisfies the
	// interface's forward-compat guard for any method not overridden.
	csi.UnimplementedGroupControllerServer
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
