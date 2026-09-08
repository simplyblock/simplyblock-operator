// The node service: the CSI RPCs that run on the node the volume is attached
// to, rather than wherever the controller plugin happens to be.
/*
Copyright (c) Arm Limited and Contributors.

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

package node

import (
	"k8s.io/client-go/kubernetes"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
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
	mounter     *mount.Mounter
	volumeLocks *csicommon.VolumeLocks
	kubeClient  kubernetes.Interface
	manager     *sbkube.Manager
	guardian    *guardian.Guardian
}

// New builds the node service. It performs no I/O and starts nothing: the
// background loops the node plugin runs — the connection monitor and the
// guardian — are started by the driver, next to the operator link, so that
// constructing a service does not launch a daemon.
//
//nolint:unparam // error return kept for constructor symmetry / future use
func New(d *csicommon.CSIDriver, kubeClient kubernetes.Interface, manager *sbkube.Manager) (*Server, error) {
	return &Server{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(d),
		mounter:           mount.New(),
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
