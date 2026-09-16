package csicommon

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

type CSIDriver struct {
	name    string
	nodeID  string
	version string
	cap     []*csi.ControllerServiceCapability
	vc      []*csi.VolumeCapability_AccessMode
	// groupController records whether the driver serves the GroupController
	// service (VolumeGroupSnapshot, design §9). The identity server advertises
	// GROUP_CONTROLLER_SERVICE only when it is set, so a driver built without
	// it (the sanity harness) does not draw the generic group-snapshot tests,
	// which assume arbitrary volumes can be group-snapshotted rather than the
	// persistent, placement-pinned groups this driver requires (§3, §11.6).
	groupController bool
}

// EnableGroupController marks the driver as serving the GroupController service.
func (d *CSIDriver) EnableGroupController() { d.groupController = true }

// GroupControllerEnabled reports whether the GroupController service is served.
func (d *CSIDriver) GroupControllerEnabled() bool { return d.groupController }

// NewCSIDriver creates a driver object. Assumes vendor version is equal to driver version &
// does not support optional driver plugin info manifest field. Refer to CSI spec for more details.
func NewCSIDriver(name, v, nodeID string) *CSIDriver {
	if name == "" {
		klog.Errorf("Driver name missing")
		return nil
	}

	if nodeID == "" {
		klog.Errorf("NodeID missing")
		return nil
	}
	if v == "" {
		klog.Errorf("Version argument missing")
		return nil
	}

	driver := CSIDriver{
		name:    name,
		version: v,
		nodeID:  nodeID,
	}

	return &driver
}

func (d *CSIDriver) ValidateControllerServiceRequest(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}

	for _, cap := range d.cap {
		if c == cap.GetRpc().GetType() {
			return nil
		}
	}
	return status.Error(codes.InvalidArgument, c.String())
}

func (d *CSIDriver) AddControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) {
	csc := make([]*csi.ControllerServiceCapability, 0, len(cl))

	for _, c := range cl {
		klog.Infof("Enabling controller service capability: %v", c.String())
		csc = append(csc, NewControllerServiceCapability(c))
	}

	d.cap = csc
}

func (d *CSIDriver) AddVolumeCapabilityAccessModes(
	vc []csi.VolumeCapability_AccessMode_Mode,
) []*csi.VolumeCapability_AccessMode {
	vca := make([]*csi.VolumeCapability_AccessMode, 0, len(vc))
	for _, c := range vc {
		klog.Infof("Enabling volume access mode: %v", c.String())
		vca = append(vca, NewVolumeCapabilityAccessMode(c))
	}
	d.vc = vca
	return vca
}

func (d *CSIDriver) GetVolumeCapabilityAccessModes() []*csi.VolumeCapability_AccessMode {
	return d.vc
}

func (d *CSIDriver) GetName() string {
	return d.name
}

func (d *CSIDriver) GetNodeID() string {
	return d.nodeID
}
