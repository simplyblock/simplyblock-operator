/*
Copyright (c) Arm Limited and Contributors.
Copyright (c) Simplyblock GmbH

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

// Package driver assembles the CSI driver: it builds the services this process
// was asked to serve, starts the background loops a node plugin owns, opens the
// link to the operator, and serves gRPC.
//
// It is the only package that reaches across every layer, which is what an
// assembly package is for. Nothing imports it but main.
package driver

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"
	"github.com/simplyblock/atlas/storage/storagerpc"

	"github.com/simplyblock/csi-driver/internal/config"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/csi/controller"
	"github.com/simplyblock/csi-driver/internal/csi/identity"
	"github.com/simplyblock/csi-driver/internal/csi/node"
	"github.com/simplyblock/csi-driver/internal/csilink"
	"github.com/simplyblock/csi-driver/internal/guardian"
	sbkube "github.com/simplyblock/csi-driver/internal/kubernetes"
	"github.com/simplyblock/csi-driver/internal/reconnect"
)

func Run(conf *config.Config) {
	var (
		cd  *csicommon.CSIDriver
		ids *identity.Server
		cs  *controller.Server
		ns  *node.Server

		controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
			csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
			csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
			csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
			csi.ControllerServiceCapability_RPC_GET_VOLUME,
			// csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
			csi.ControllerServiceCapability_RPC_VOLUME_CONDITION,
		}
		volumeModes = []csi.VolumeCapability_AccessMode_Mode{
			csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		}
	)

	cd = csicommon.NewCSIDriver(conf.DriverName, conf.DriverVersion, conf.NodeID)
	if cd == nil {
		klog.Fatalln("Failed to initialize CSI Driver.")
	}
	if conf.IsControllerServer {
		cd.AddControllerServiceCapabilities(controllerCaps)
		cd.AddVolumeCapabilityAccessModes(volumeModes)
	}

	ids = identity.New(cd)

	// Build one Kubernetes client shared by the node and controller servers
	// (PV/PVC/topology reads, PVC-annotation patches) instead of each constructing
	// its own in-cluster config + clientset. A missing in-cluster config is
	// non-fatal — the features that need it degrade to no-ops.
	var kubeClient kubernetes.Interface
	if k8sConfig, err := rest.InClusterConfig(); err != nil {
		klog.Warningf("no in-cluster config; Kubernetes API features disabled: %v", err)
	} else if clientset, err := kubernetes.NewForConfig(k8sConfig); err != nil {
		klog.Warningf("failed to create kubernetes client; Kubernetes API features disabled: %v", err)
	} else {
		kubeClient = clientset
	}

	if conf.IsNodeServer {
		var err error
		ns, err = startNodeServer(cd, kubeClient)
		if err != nil {
			klog.Fatalf("failed to create node server: %s", err)
		}
	}

	if conf.IsControllerServer {
		var err error
		cs, err = controller.New(cd, kubeClient)
		if err != nil {
			klog.Fatalf("failed to create controller server: %s", err)
		}
	}

	// The link to the operator, when enabled. It is independent of the CSI
	// server: the driver serves kubelet whether or not the operator is
	// reachable, and a dropped link is reconnected rather than reported.
	if conf.LinkEnabled {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := startLink(ctx, conf); err != nil {
			klog.Fatalf("failed to start the operator link: %s", err)
		}
	}

	s := csicommon.NewNonBlockingGRPCServer()
	s.Start(conf.Endpoint, ids, cs, ns)
	s.Wait()
}

// startLink dials the operator as whichever peer this process is.
//
// A node plugin serves its local NVMe state on the link — that is the point of
// linking it — and is identified by the node it runs on. A controller plugin
// links as itself and currently serves nothing; it is registered so the
// operator can see it, and so services can be added without new plumbing.
func startLink(ctx context.Context, conf *config.Config) error {
	cfg := csilink.Config{
		HubAddress:  conf.LinkHubAddress,
		CAFile:      conf.LinkCAFile,
		ServerName:  conf.LinkServerName,
		TokenFile:   conf.LinkTokenFile,
		InstanceUID: conf.PodUID,
	}

	switch {
	case conf.IsNodeServer:
		if conf.NodeID == "" {
			return fmt.Errorf("link needs the node name (--nodeid); the operator verifies it")
		}
		// storage.Local reads this node through sysfs. It must be the local
		// one: serving a remote accessor would make this node a proxy for
		// another, which nothing wants and which doubles every round trip.
		srv, err := storagerpc.NewServer(storage.Local(nvme.SysfsConfig{}))
		if err != nil {
			return fmt.Errorf("node storage: %w", err)
		}
		cfg.ID = link.NodePeer(conf.NodeID)
		cfg.Register = srv.Register
		cfg.Capabilities = storagerpc.Capabilities()

	case conf.IsControllerServer:
		if conf.PodName == "" {
			return fmt.Errorf("link needs the pod name (--pod-name); the operator verifies it")
		}
		cfg.ID = link.ControllerPeer(conf.PodName)

	default:
		return fmt.Errorf("link needs either --node or --controller")
	}

	_, err := csilink.Start(ctx, cfg)
	return err
}

// startNodeServer builds the node service and starts the two background loops
// that belong to a node plugin rather than to a request: the connection
// monitor, which reconnects NVMe-oF paths the control plane still publishes,
// and the guardian, which restarts the pods whose volumes lost every path.
//
// They start here rather than inside the service's constructor because
// constructing a service should not launch a daemon: a test wanting a node
// service should not get a poll loop against a live control plane with it.
//
// One Kubernetes cache manager is shared by both. The monitor reads
// PersistentVolumes every few seconds and the guardian reads them per pod on
// every poll, so a single instance means a single PV watch and a single PVC
// watch. It serves reads from cache once synced and falls back to the API until
// then, so neither consumer needs a fallback of its own.
func startNodeServer(cd *csicommon.CSIDriver, kubeClient kubernetes.Interface) (*node.Server, error) {
	manager := sbkube.NewManager(kubeClient)
	manager.Start(context.Background())

	ns, err := node.New(cd, kubeClient, manager)
	if err != nil {
		return nil, err
	}

	nodeName := cd.GetNodeID()
	podGuardian, err := guardian.Start(context.Background(), guardian.NewDefaultConfig(nodeName), manager)
	if err != nil {
		// A guardian that will not start costs pod restarts on total path loss,
		// which is a degradation rather than a reason to refuse to serve.
		klog.Errorf("failed to start guardian: %v", err)
	} else {
		ns.AttachGuardian(podGuardian)
	}

	go reconnect.MonitorConnection(markBroken(podGuardian), manager, cd.GetName(), nodeName)

	return ns, nil
}

// markBroken is the monitor's hook into the guardian, tolerant of there being
// no guardian: the monitor still reconnects paths without one, it just has
// nowhere to report a volume that lost all of them.
func markBroken(g *guardian.Guardian) func(string) {
	if g == nil {
		return nil
	}
	return g.MarkBrokenLvol
}
