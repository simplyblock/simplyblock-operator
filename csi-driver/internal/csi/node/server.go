// The node service: the CSI RPCs that run on the node the volume is attached
// to, rather than wherever the controller plugin happens to be.
package node

import (
	"context"

	"k8s.io/client-go/kubernetes"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/fabric"
	"github.com/simplyblock/csi-driver/internal/guardian"
	sbkube "github.com/simplyblock/csi-driver/internal/kubernetes"
	"github.com/simplyblock/csi-driver/internal/mount"
)

type Server struct {
	*csicommon.DefaultNodeServer
	// mounter performs the node-local half of staging: reading a device,
	// formatting and mounting it, and managing the path it is mounted on. It is
	// injectable so a test can script the probe's answers and observe exactly
	// which commands staging chose to run, which is how the never-format
	// contract is asserted.
	mounter *mount.Mounter
	// stack is the volume stack the node RPCs drive: the host seams a plan is
	// built with, and the runner that walks it. It is injectable for the same
	// reason the mounter is, so a test can assert which verb an RPC chose
	// without a fabric under it.
	stack *stack
	// repairFabric diagnoses a subsystem a bring-up could not get a device out
	// of, and reports whether it tore anything down. It is a field so a test can
	// drive the retry without a kernel, and because the repair reaches sysfs
	// directly rather than through the stack.
	repairFabric func(ctx context.Context, subsystemNQN string, nsID nvme.NamespaceID) bool
	// identifyStaged names the namespace behind a staging path by reading the
	// host, for a volume whose stashed context and stack record both fall short.
	// It is a field for the reason repairFabric is one: it reaches sysfs and the
	// mount table directly, and a test has neither.
	identifyStaged func(ctx context.Context, stagingTargetPath string) (lvol.Connection, error)
	volumeLocks    *csicommon.VolumeLocks
	kubeClient     kubernetes.Interface
	manager        *sbkube.Manager
	guardian       *guardian.Guardian
}

// New builds the node service. It performs no I/O and starts nothing: the
// background loops the node plugin runs, the connection monitor and the
// guardian, are started by the driver, next to the operator link, so that
// constructing a service does not launch a daemon.
//
//nolint:unparam // error return kept for constructor symmetry / future use
func New(d *csicommon.CSIDriver, kubeClient kubernetes.Interface, manager *sbkube.Manager) (*Server, error) {
	mounter := mount.New()
	return &Server{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(d),
		mounter:           mounter,
		stack:             newStack(mounter, stackRecordDir),
		repairFabric:      fabric.RepairAttach,
		identifyStaged:    stagedIdentity(nvme.SysfsConfig{}),
		volumeLocks:       csicommon.NewVolumeLocks(),
		kubeClient:        kubeClient,
		manager:           manager,
	}, nil
}

// AttachGuardian gives the node service the guardian to report a volume's total
// path loss to. It is set after construction because the guardian and the
// service are started by the driver in that order.
func (ns *Server) AttachGuardian(g *guardian.Guardian) {
	ns.guardian = g
}
