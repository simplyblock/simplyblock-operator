//go:build linux

// The pNFS metadata server's guest agent: it runs inside the guest and
// assembles the exports the runner relays to it, with the same assembler the
// node plugin hosts on a node (design-pnfs-mds-vm.md §6.3).
//
// It listens on the guest's bridge address, which only the runner reaches,
// and serves the export service and gRPC's health service there. The health
// is what the pod's readiness follows: the state disk mounted and nfsd running.
// It also watches nfsd and logs its state to the console (nfsd_watchdog.go).
// It holds no credentials. The namespace's connection arrives with each call,
// resolved by the runner.

package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/klog"
	mountutils "k8s.io/mount-utils"

	atlasexport "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"
	"github.com/simplyblock/csi-driver/internal/mds/agent"
	"github.com/simplyblock/csi-driver/internal/mds/netsetup"
	csimount "github.com/simplyblock/csi-driver/internal/mount"
	"github.com/simplyblock/csi-driver/internal/nfsexport"
)

// healthInterval is how often the guest's health is re-evaluated. The pod's
// readiness probe runs every five seconds, so this keeps the answer fresh for
// each probe.
const healthInterval = 2 * time.Second

func main() {
	listen := flag.String("listen", ":"+strconv.Itoa(netsetup.AgentPort), "Address serving the export and health services")
	stateDir := flag.String("state-dir", nfsexport.NFSStateDir, "Where the state disk is mounted")
	watchdog := flag.Duration("nfsd-watchdog-interval", agent.DefaultNFSDWatchdogInterval,
		"How often nfsd's threads and pool counters are read and judged; 0 turns the watchdog off")
	klog.InitFlags(nil)
	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Exitf("failed to set logtostderr flag: %v", err)
	}
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, *listen, *stateDir, *watchdog); err != nil {
		klog.Errorf("mds-agent: %v", err)
		klog.Flush()
		os.Exit(1)
	}
}

func run(ctx context.Context, listen, stateDir string, watchdog time.Duration) error {
	// No host NQN function: the identity arrives with every call, decided by
	// the operator and resolved by the runner.
	assembler, err := nfsexport.NewAssembler(
		storage.Local(nvme.SysfsConfig{}).DeviceResolver, csimount.New(), nil)
	if err != nil {
		return err
	}
	exports, err := nfsexportrpc.NewServer(nfsexport.WithNFSD(assembler),
		nfsexportrpc.WithExtentReader(&atlasexport.ExtentReader{Root: atlasexport.ExportsRoot}))
	if err != nil {
		return err
	}

	server := grpc.NewServer()
	exports.Register(server)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)

	mounter := mountutils.New("")
	go agent.Report(ctx, healthServer, agent.Health{
		StateDir: stateDir,
		Mounted:  mounter.IsMountPoint,
		NFSD:     nfsexport.CheckNFSDThreads,
	}.Check, healthInterval)
	go agent.RunNFSDWatchdog(ctx, &agent.NFSDWatchdog{Log: agent.KlogLogger{}}, watchdog)

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()
	klog.Infof("mds-agent serving on %s", listen)
	return server.Serve(lis)
}
