// The controller service: the CSI RPCs that run wherever the driver's
// controller plugin runs, rather than on the node holding the volume.
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

package controller

import (
	"k8s.io/client-go/kubernetes"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

type Server struct {
	*csicommon.DefaultControllerServer
	volumeLocks *csicommon.VolumeLocks
	// kubeClient reads/patches PVC annotations (host_id resolution, placement-hint
	// cleanup). Built once at construction and reused; nil when no in-cluster
	// config is available (e.g. unit tests), in which case the annotation helpers
	// are no-ops.
	kubeClient kubernetes.Interface
}

//nolint:unparam // error return kept for constructor symmetry / future use
func New(d *csicommon.CSIDriver, kubeClient kubernetes.Interface) (*Server, error) {
	server := Server{
		DefaultControllerServer: csicommon.NewDefaultControllerServer(d),
		volumeLocks:             csicommon.NewVolumeLocks(),
		kubeClient:              kubeClient,
	}
	return &server, nil
}
