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
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csiaddonsidentity "github.com/csi-addons/spec/lib/go/identity"
	csiaddonsreplication "github.com/csi-addons/spec/lib/go/replication"
	csiaddonsvolumegroup "github.com/csi-addons/spec/lib/go/volumegroup"
	"google.golang.org/grpc"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/conntrack"
	"github.com/simplyblock/atlas/conntrack/conntrackrpc"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"
	"github.com/simplyblock/atlas/storage/storagerpc"

	"github.com/simplyblock/csi-driver/internal/config"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/csi/controller"
	csiaddonsidentityserver "github.com/simplyblock/csi-driver/internal/csi/csiaddons/identity"
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
		ns, err = startNodeServer(cd, kubeClient, dynClient)
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
		// The sync write verbs read a volume's active site from its
		// VolumeReplication annotation (design §5.2, §6.3); wire the resolver that
		// reads it, when the cluster carries the CRD.
		enableControllerSyncSite(context.Background(), cs, kubeClient, dynClient)
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
		if err := startLink(ctx, conf); err != nil {
			klog.Fatalf("failed to start the operator link: %s", err)
		}
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
	}

	s := csicommon.NewNonBlockingGRPCServer()
	s.Start(conf.Endpoint, ids, cs, ns, register...)
	s.Wait()
}

// startLink dials the operator as whichever peer this process is.
//
// A node plugin serves its local NVMe state on the link, which is the point of
// linking it, and is identified by the node it runs on. A controller plugin
// links as itself and currently serves nothing. It is registered so the
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
		local := storage.Local(nvme.SysfsConfig{})
		srv, err := storagerpc.NewServer(local)
		if err != nil {
			return fmt.Errorf("node storage: %w", err)
		}
		// The node forgets the connection-tracking entries translated to a dead
		// pNFS metadata server pod when the operator asks. It runs on the host's
		// network, so the table it reaches is the host's.
		flows, err := conntrackrpc.NewServer(conntrack.Forget)
		if err != nil {
			return fmt.Errorf("node flows: %w", err)
		}
		// No export service: exports are assembled in the metadata server's
		// guest, never on a node.
		cfg.ID = link.NodePeer(conf.NodeID)
		cfg.Register = func(r grpc.ServiceRegistrar) {
			srv.Register(r)
			flows.Register(r)
		}
		cfg.Capabilities = append(storagerpc.Capabilities(), conntrackrpc.Capabilities()...)

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
func startNodeServer(
	cd *csicommon.CSIDriver, kubeClient kubernetes.Interface, dynClient dynamic.Interface,
) (*node.Server, error) {
	manager := sbkube.NewManager(kubeClient)
	manager.Start(context.Background())

	ns, err := node.New(cd, kubeClient, manager)
	if err != nil {
		return nil, err
	}

	enableSyncReplication(context.Background(), ns, kubeClient, dynClient)

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

	return ns, nil
}

// enableSyncReplication turns on the node's scheduling-driven switchover when
// the cluster carries the VolumeReplication CRD (design §6). Best-effort: with
// no dynamic client, or on a cluster without the CRD, every volume stages on the
// ordinary, site-unaware path, exactly as before. The informers run for the
// process lifetime; the node's cache-synced gate holds staging of a sync volume
// until they have synced.
func enableSyncReplication(
	ctx context.Context, ns *node.Server, kubeClient kubernetes.Interface, dynClient dynamic.Interface,
) {
	if dynClient == nil || kubeClient == nil {
		return
	}
	if !volumeReplicationCRDPresent(kubeClient.Discovery()) {
		klog.Info("sync replication: VolumeReplication CRD absent; node staging stays site-unaware")
		return
	}
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)
	vr := factory.ForResource(kube.VolumeReplicationGVR).Informer()
	vrClass := factory.ForResource(kube.VolumeReplicationClassGVR).Informer()
	resolver, err := kube.NewSyncResolver(vr, vrClass)
	if err != nil {
		klog.Errorf("sync replication: build resolver: %v; node staging stays site-unaware", err)
		return
	}
	factory.Start(ctx.Done())
	ns.EnableSyncReplication(resolver, func() bool { return vr.HasSynced() && vrClass.HasSynced() }, dynClient)
	klog.Info("sync replication: node staging is site-aware for VolumeReplication-managed volumes")

	// Report whether the caches actually sync, bounded, without blocking startup.
	// The node's own gate holds staging of a sync volume until they do; a wait
	// that never completes is almost always a missing node RBAC for
	// volumereplications, and a loud log turns "all staging hangs" into a cause.
	go func() {
		wctx, cancel := context.WithTimeout(context.Background(), syncCacheSyncTimeout)
		defer cancel()
		if cache.WaitForCacheSync(wctx.Done(), vr.HasSynced, vrClass.HasSynced) {
			klog.Info("sync replication: VolumeReplication caches synced; site-aware staging is ready")
			return
		}
		klog.Errorf("sync replication: VolumeReplication caches did not sync within %s; "+
			"sync-managed volumes fail closed at staging until they do "+
			"(check the node RBAC for volumereplications and volumereplicationclasses)",
			syncCacheSyncTimeout)
	}()
}

// syncCacheSyncTimeout bounds the startup wait that only logs whether the sync
// informers came up; the node's cache-synced gate is the real guard.
const syncCacheSyncTimeout = 2 * time.Minute

// volumeReplicationCRDPresent reports whether the VolumeReplication CRD is served
// by the API server, so the node only runs the sync informers where they resolve.
func volumeReplicationCRDPresent(disc discovery.DiscoveryInterface) bool {
	list, err := disc.ServerResourcesForGroupVersion(kube.VolumeReplicationGVR.GroupVersion().String())
	if err != nil {
		return false
	}
	for _, r := range list.APIResources {
		if r.Name == kube.VolumeReplicationGVR.Resource {
			return true
		}
	}
	return false
}

// enableControllerSyncSite wires the resolver the controller's sync write verbs
// (PromoteVolume, DemoteVolume) use to read a volume's active site from its
// VolumeReplication's SyncSiteAnnotation (design §5.2, §6.3). Best-effort, like
// enableSyncReplication on the node: with no client, or on a cluster without the
// CRD, the seam stays unset and the sync verbs refuse rather than guess a site.
//
// The controller needs a full resolver, not the node's sync-only one: it maps a
// CSI volume handle to its PV, the PV to its claim, and the claim to its
// VolumeReplication, so it watches PVs and PVCs in addition to VolumeReplications.
func enableControllerSyncSite(
	ctx context.Context, cs *controller.Server, kubeClient kubernetes.Interface, dynClient dynamic.Interface,
) {
	if dynClient == nil || kubeClient == nil {
		return
	}
	if !volumeReplicationCRDPresent(kubeClient.Discovery()) {
		klog.Info("sync replication: VolumeReplication CRD absent; controller sync verbs stay site-unaware")
		return
	}
	typed := informers.NewSharedInformerFactory(kubeClient, 0)
	pv := typed.Core().V1().PersistentVolumes().Informer()
	pvc := typed.Core().V1().PersistentVolumeClaims().Informer()
	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)
	vr := dynFactory.ForResource(kube.VolumeReplicationGVR).Informer()
	resolver, err := kube.NewResolver(kube.ResolverConfig{
		PersistentVolumes:      pv,
		PersistentVolumeClaims: pvc,
		VolumeReplications:     vr,
	})
	if err != nil {
		klog.Errorf("sync replication: build controller resolver: %v; sync verbs stay site-unaware", err)
		return
	}
	typed.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	cs.EnableSyncSiteResolution(func(ctx context.Context, h lvol.VolumeHandle) (string, error) {
		ref, err := resolver.VolumeReplicationForVolumeHandle(ctx, h)
		if err != nil {
			return "", err
		}
		return ref.Site, nil
	})
	klog.Info("sync replication: controller sync verbs read the active site from the VolumeReplication annotation")

	// Report whether the caches actually sync, bounded, without blocking startup,
	// for the same reason the node path does: a wait that never completes is almost
	// always a missing controller RBAC, and a loud log names the cause.
	go func() {
		wctx, cancel := context.WithTimeout(context.Background(), syncCacheSyncTimeout)
		defer cancel()
		if cache.WaitForCacheSync(wctx.Done(), pv.HasSynced, pvc.HasSynced, vr.HasSynced) {
			klog.Info("sync replication: controller caches synced; site resolution is ready")
			return
		}
		klog.Errorf("sync replication: controller caches did not sync within %s; "+
			"sync write verbs error until they do "+
			"(check the controller RBAC for persistentvolumes, persistentvolumeclaims, and volumereplications)",
			syncCacheSyncTimeout)
	}()
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

// markBroken is the monitor's hook into the guardian, tolerant of there being
// no guardian: the monitor still reconnects paths without one, it just has
// nowhere to report a volume that lost all of them.
func markBroken(g *guardian.Guardian) func(string) {
	if g == nil {
		return nil
	}
	return g.MarkBrokenLvol
}
