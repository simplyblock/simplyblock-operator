//go:build linux

// The pNFS metadata server's guest agent: it runs inside the guest and
// assembles the exports the runner relays to it, with the same assembler the
// node plugin hosts on a node (design-pnfs-mds-vm.md §6.3).
//
// It listens on the guest's bridge address, which only the runner reaches,
// and serves the export service and gRPC's health service there. The health
// is what the pod's readiness follows: the state disk mounted and nfsd running.
// It also watches nfsd and logs its state to the console (nfsd_watchdog.go),
// and keeps every export's namespace on all its paths (paths.go).
// It holds no credentials. The namespace's connection arrives with each call,
// resolved by the runner.

package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/klog"
	mountutils "k8s.io/mount-utils"

	"github.com/simplyblock/atlas/lvol"
	atlasexport "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
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

// healthTimeout bounds one health check. A check blocked on a dead state disk
// or a shut-down export reports the guest unhealthy instead of freezing the
// last answer.
const healthTimeout = 5 * time.Second

// nfsdStuckLimit is how long nfsd may look stuck before the guest is
// unhealthy. A reconnect stalls it for well under this.
const nfsdStuckLimit = 3 * time.Minute

// statfs answers whether the filesystem mounted at path still responds. A
// getattr fails with EIO once XFS has shut down on an I/O error, and a statfs
// may not.
func statfs(path string) error {
	var st unix.Stat_t
	return unix.Stat(path, &st)
}

func main() {
	listen := flag.String("listen", ":"+strconv.Itoa(netsetup.AgentPort), "Address serving the export and health services")
	stateDir := flag.String("state-dir", nfsexport.NFSStateDir, "Where the state disk is mounted")
	watchdog := flag.Duration("nfsd-watchdog-interval", agent.DefaultNFSDWatchdogInterval,
		"How often nfsd's threads and pool counters are read and judged; 0 turns the watchdog off")
	pathInterval := flag.Duration("path-interval", agent.DefaultPathInterval,
		"How often every export's namespace is reconnected over the paths it is missing")
	klog.InitFlags(nil)
	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Exitf("failed to set logtostderr flag: %v", err)
	}
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, *listen, *stateDir, *watchdog, *pathInterval); err != nil {
		klog.Errorf("mds-agent: %v", err)
		klog.Flush()
		os.Exit(1)
	}
}

// reconcilePaths brings one namespace onto every path its connection
// publishes, with the connector and identity the assembler attached it with.
func reconcilePaths(ctx context.Context, conn lvol.Connection, hostNQN string) (nvmeof.PathState, error) {
	var opts []nvmeof.TargetOption
	if hostNQN != "" {
		opts = append(opts, nvmeof.WithHostNQN(hostNQN))
	}
	connector := nvmeof.NewCLIConnector(nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{}))
	return nvmeof.ReconcilePaths(ctx, connector, nil, conn, opts...)
}

const (
	// stateDevice is the state disk QEMU attaches with serial pnfs-state.
	stateDevice = "/dev/disk/by-id/virtio-pnfs-state"
	// gateFile lets the image's nfsd units start.
	gateFile = "/run/mds/active"
	// stateTmpfiles recreates what the state mount hides.
	stateTmpfiles = "/usr/lib/tmpfiles.d/pnfs.conf"

	activateRetryInterval = 2 * time.Second
)

// runCommand runs one command in the guest and returns its combined output.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func run(ctx context.Context, listen, stateDir string, watchdog, pathInterval time.Duration) error {
	// No host NQN function: the identity arrives with every call, decided by
	// the operator and resolved by the runner.
	assembler, err := nfsexport.NewAssembler(
		storage.Local(nvme.SysfsConfig{}).DeviceResolver, csimount.New(), nil)
	if err != nil {
		return err
	}
	keeper := &agent.PathKeeper{Reconcile: reconcilePaths, Log: agent.KlogLogger{}}
	exports, err := nfsexportrpc.NewServer(keeper.Wrap(nfsexport.WithNFSD(assembler)),
		nfsexportrpc.WithExtentReader(&atlasexport.ExtentReader{Root: atlasexport.ExportsRoot}))
	if err != nil {
		return err
	}

	server := grpc.NewServer()
	exports.Register(server)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)

	mounter := mountutils.New("")
	// A guest image built for the warm standby leaves the state disk unmounted
	// and nfsd stopped until it is promoted. Nothing promotes the only metadata
	// server, so it activates itself.
	activation := agent.Activation{
		StateDevice: stateDevice,
		StateDir:    stateDir,
		GateFile:    gateFile,
		Tmpfiles:    stateTmpfiles,
		Mounted:     mounter.IsMountPoint,
		Run:         runCommand,
		Log:         agent.KlogLogger{},
	}
	go activation.ActivateUntilDone(ctx, activateRetryInterval)
	nfsdWatchdog := &agent.NFSDWatchdog{Log: agent.KlogLogger{}}
	go agent.Report(ctx, healthServer, agent.Health{
		StateDir:      stateDir,
		Mounted:       mounter.IsMountPoint,
		NFSD:          nfsexport.CheckNFSDThreads,
		StateWritable: agent.WriteProbe(stateDir),
		Exports:       func() error { return keeper.CheckFilesystems(statfs) },
		Stuck:         nfsdWatchdog.StuckFor,
		StuckLimit:    nfsdStuckLimit,
	}.Check, healthInterval, healthTimeout)
	go agent.RunNFSDWatchdog(ctx, nfsdWatchdog, watchdog)
	go keeper.Run(ctx, pathInterval)

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
