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
	csiaddonsidentity "github.com/csi-addons/spec/lib/go/identity"
	csiaddonsreplication "github.com/csi-addons/spec/lib/go/replication"
	csiaddonsvolumegroup "github.com/csi-addons/spec/lib/go/volumegroup"
	"google.golang.org/grpc"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/conntrack"
	"github.com/simplyblock/atlas/conntrack/conntrackrpc"
	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/netcheck"
	"github.com/simplyblock/atlas/netcheck/netcheckrpc"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"
	"github.com/simplyblock/atlas/storage/storagerpc"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/config"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/csi/controller"
	csiaddonsidentityserver "github.com/simplyblock/csi-driver/internal/csi/csiaddons/identity"
	"github.com/simplyblock/csi-driver/internal/csi/fence"
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
		// A ReadWriteMany claim needs this advertised or it cannot bind. It is
		// served by a pNFS export, and CreateVolume refuses it on any other
		// fsType. The other multi-node modes are deliberately absent.
		volumeModes = []csi.VolumeCapability_AccessMode_Mode{
			csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		}
	)

	cd = csicommon.NewCSIDriver(conf.DriverName, conf.DriverVersion, conf.NodeID)
	if cd == nil {
		klog.Fatalln("Failed to initialize CSI Driver.")
	}
	if conf.IsControllerServer {
		cd.AddControllerServiceCapabilities(controllerCaps)
		cd.AddVolumeCapabilityAccessModes(volumeModes)
		// The controller serves the GroupController service (VolumeGroupSnapshot,
		// design §9); advertise it so the csi-snapshotter routes group snapshots here.
		cd.EnableGroupController()
	}

	ids = identity.New(cd)

	// Build one Kubernetes client shared by the node and controller servers
	// (PV/PVC/topology reads, PVC-annotation patches) instead of each constructing
	// its own in-cluster config + clientset. A missing in-cluster config is
	// non-fatal, and the features that need it degrade to no-ops.
	var kubeClient kubernetes.Interface
	var dynClient dynamic.Interface
	var exports controller.ExportRegistry
	if k8sConfig, err := rest.InClusterConfig(); err != nil {
		klog.Warningf("no in-cluster config; Kubernetes API features disabled: %v", err)
	} else if clientset, err := kubernetes.NewForConfig(k8sConfig); err != nil {
		klog.Warningf("failed to create kubernetes client; Kubernetes API features disabled: %v", err)
	} else {
		kubeClient = clientset
		// The consistency-group watcher requests VolumeMigrations (the
		// pre-join live migration) and pNFS provisioning creates its export
		// resource, both without importing the operator's types.
		if d, err := dynamic.NewForConfig(k8sConfig); err != nil {
			klog.Warningf("failed to create dynamic client; pre-join migrations and pNFS volumes disabled: %v", err)
		} else {
			dynClient = d
			exports = controller.NewExportRegistry(d)
		}
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
		cs, err = controller.New(cd, kubeClient, exports)
		if err != nil {
			klog.Fatalf("failed to create controller server: %s", err)
		}
		// The membership label is live for the volume's whole life (design
		// §4.5): a PVC watcher joins a volume when the label is added and
		// detaches it when the label is removed. Degrades to a logged no-op
		// without a Kubernetes client, like the other kube-backed features.
		watcherCtx, watcherCancel := context.WithCancel(context.Background())
		defer watcherCancel()
		controller.StartConsistencyGroupLabelWatcher(watcherCtx, kubeClient, dynClient, conf.DriverName)
	}

	// The link to the operator, when enabled. It is independent of the CSI
	// server: the driver serves kubelet whether or not the operator is
	// reachable, and a dropped link is reconnected rather than reported.
	if conf.LinkEnabled {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		agent, err := startLink(ctx, conf)
		if err != nil {
			klog.Fatalf("failed to start the operator link: %s", err)
		}
		// The controller asks the operator over the link which member of a
		// supercluster a volume goes to (design-superclusters.md §7.2).
		if cs != nil {
			cs.SetPlacer(csilink.Placement{Agent: agent})
		}
	}

	// The site link, on a controller plugin of a stretched cluster's member.
	// Like the operator link it is independent of the CSI server.
	if conf.SiteLinkEnabled && conf.IsControllerServer && kubeClient != nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		csilink.StartSiteLink(ctx, kubeClient, csilink.SiteLinkConfig{
			Address:        conf.SiteLinkAddress,
			AgentNamespace: conf.SiteLinkNamespace,
			TokenFile:      conf.SiteLinkTokenFile,
			ID:             link.ControllerPeer(conf.PodName),
			InstanceUID:    conf.PodUID,
		})
	}

	// The csi-addons Identity and Replication services register alongside the
	// CSI services on the same socket. Identity is always registered (it just
	// answers capability probes); Replication only when this process serves
	// the controller (cs is nil on a node-only process).
	register := []func(*grpc.Server){
		func(gs *grpc.Server) {
			csiaddonsidentity.RegisterIdentityServer(gs, csiaddonsidentityserver.New(conf.DriverName, conf.DriverVersion))
		},
	}
	if cs != nil {
		register = append(register, func(gs *grpc.Server) {
			csiaddonsreplication.RegisterControllerServer(gs, cs)
		})
		// The csi-addons VolumeGroup service (design §14.3): the stock
		// controller-manager dials it to form a backend consistency group before
		// replicating the group as one unit.
		register = append(register, func(gs *grpc.Server) {
			csiaddonsvolumegroup.RegisterControllerServer(gs, cs)
		})
		// The csi-addons NetworkFence service (design-stretched-sync-replication-
		// ramen.md §10.2): Ramen fences a lost site of a stretched cluster
		// through it before failing over.
		fences := fence.New(func(clusterID string) (fence.Backend, error) {
			return clusters.ReplicationClient(context.Background(), clusterID)
		}, soleSyncCluster)
		register = append(register, func(gs *grpc.Server) { fences.Register(gs) })
	}

	s := csicommon.NewNonBlockingGRPCServer()
	s.Start(conf.Endpoint, ids, cs, ns, register...)
	s.Wait()
}

// startLink dials the operator as whichever peer this process is, and returns
// the agent holding the link.
//
// A node plugin serves its local NVMe state on the link, which is the point of
// linking it, and is identified by the node it runs on. A controller plugin
// links as itself and serves nothing: it calls the operator's placement
// service over the link instead.
func startLink(ctx context.Context, conf *config.Config) (*link.Agent, error) {
	cfg := csilink.Config{
		HubAddress:  conf.LinkHubAddress,
		CAFile:      conf.LinkCAFile,
		CertFile:    conf.LinkCertFile,
		KeyFile:     conf.LinkKeyFile,
		ServerName:  conf.LinkServerName,
		TokenFile:   conf.LinkTokenFile,
		InstanceUID: conf.PodUID,
	}

	switch {
	case conf.IsNodeServer:
		if conf.NodeID == "" {
			return nil, fmt.Errorf("link needs the node name (--nodeid); the operator verifies it")
		}
		// storage.Local reads this node through sysfs. It must be the local
		// one: serving a remote accessor would make this node a proxy for
		// another, which nothing wants and which doubles every round trip.
		local := storage.Local(nvme.SysfsConfig{})
		srv, err := storagerpc.NewServer(local)
		if err != nil {
			return nil, fmt.Errorf("node storage: %w", err)
		}
		// The node forgets the connection-tracking entries translated to a dead
		// pNFS metadata server pod when the operator asks. It runs on the host's
		// network, so the table it reaches is the host's.
		flows, err := conntrackrpc.NewServer(conntrack.Forget)
		if err != nil {
			return nil, fmt.Errorf("node flows: %w", err)
		}
		// The node answers the operator's host checks: the addresses of a NIC
		// before a storage pod takes it, and whether a connection from the host
		// to a storage listener succeeds. Both read the host's namespace, which
		// is this process's.
		checks, err := netcheckrpc.NewServer(
			func(sel netcheck.Selector) (netcheck.Report, error) {
				return netcheck.Interface(inventory.Config{}, sel)
			},
			netcheck.Connect,
		)
		if err != nil {
			return nil, fmt.Errorf("node host checks: %w", err)
		}
		// No export service: exports are assembled in the metadata server's
		// guest, never on a node.
		cfg.ID = link.NodePeer(conf.NodeID)
		cfg.Register = func(r grpc.ServiceRegistrar) {
			srv.Register(r)
			flows.Register(r)
			checks.Register(r)
		}
		cfg.Capabilities = append(storagerpc.Capabilities(), conntrackrpc.Capabilities()...)
		cfg.Capabilities = append(cfg.Capabilities, netcheckrpc.Capabilities()...)

	case conf.IsControllerServer:
		if conf.PodName == "" {
			return nil, fmt.Errorf("link needs the pod name (--pod-name); the operator verifies it")
		}
		cfg.ID = link.ControllerPeer(conf.PodName)

	default:
		return nil, fmt.Errorf("link needs either --node or --controller")
	}

	return csilink.Start(ctx, cfg)
}

// startNodeServer builds the node service and starts the background loops that
// belong to a node plugin rather than to a request: the connection monitor,
// which reconnects NVMe-oF paths the control plane still publishes, the
// guardian, which restarts the pods whose volumes lost every path, and the pNFS
// layout primer, which takes the first layout after an MDS restart before a pod
// can.
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
	go node.KeepLayoutsPrimed(context.Background(), cd.GetName())

	go advertiseVDOCapability(kubeClient, nodeName)
	go advertiseKVMCapability(kubeClient, nodeName)
	go advertiseSubnets(kubeClient, nodeName)
	go advertiseHostNQN(kubeClient, nodeName)

	return ns, nil
}

// advertiseVDOCapability runs once in the background, same as the guardian
// above: a failure here degrades to "not yet advertised" rather than
// blocking node plugin startup.
func advertiseVDOCapability(kubeClient kubernetes.Interface, nodeName string) {
	err := node.AdvertiseVDOCapability(context.Background(), kubeClient, nodeName)
	if err != nil {
		klog.Errorf("failed to advertise vdo-capable for node %s: %v", nodeName, err)
	}
}

// advertiseKVMCapability publishes whether this node can host the pNFS metadata
// server's guest, in the background for the same reason as the VDO probe.
func advertiseKVMCapability(kubeClient kubernetes.Interface, nodeName string) {
	err := node.AdvertiseKVMCapability(context.Background(), kubeClient, nodeName)
	if err != nil {
		klog.Errorf("failed to advertise kvm-capable for node %s: %v", nodeName, err)
	}
}

// advertiseSubnets publishes the subnets this node's interfaces are on, which
// place the pNFS metadata server on its data network, in the background for the
// same reason as the VDO probe.
func advertiseSubnets(kubeClient kubernetes.Interface, nodeName string) {
	err := node.AdvertiseSubnets(context.Background(), kubeClient, nodeName)
	if err != nil {
		klog.Errorf("failed to advertise subnets for node %s: %v", nodeName, err)
	}
}

// advertiseHostNQN publishes the kernel's default NVMe host NQN, which the
// control plane revokes on the peer when it fences this node's host on a
// two-node cluster, in the background for the same reason as the VDO probe.
func advertiseHostNQN(kubeClient kubernetes.Interface, nodeName string) {
	err := node.AdvertiseHostNQN(context.Background(), kubeClient, nodeName)
	if err != nil {
		klog.Errorf("failed to advertise the NVMe host NQN for node %s: %v", nodeName, err)
	}
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

// soleSyncCluster is the cluster a fence naming none fences: the one
// sync-replicated cluster in the driver's secret.
func soleSyncCluster() (string, error) {
	ids, err := clusters.List()
	if err != nil {
		return "", err
	}
	var sync []string
	for _, id := range ids {
		if _, ok, err := clusters.SyncSite(id); err == nil && ok {
			sync = append(sync, id)
		}
	}
	if len(sync) != 1 {
		return "", fmt.Errorf(
			"the secret has %d sync-replicated clusters; the fence must name one in parameter clusterID", len(sync))
	}
	return sync[0], nil
}
